"""Native Intelbras Allo integration."""

import asyncio
from dataclasses import dataclass, field
from collections.abc import Callable

from homeassistant.const import EVENT_HOMEASSISTANT_STOP
from homeassistant.exceptions import ConfigEntryAuthFailed, ConfigEntryError
from homeassistant.helpers import config_validation as cv

from .const import DOMAIN, PLATFORMS
from .helper import Helper, HelperError

CONFIG_SCHEMA = cv.config_entry_only_config_schema(DOMAIN)


@dataclass
class Runtime:
    helper: Helper
    device: dict
    available: bool = False
    listeners: set[Callable] = field(default_factory=set)
    camera: object = None

    hass: object = None
    entry: object = None
    started: bool = False
    stopping: bool = False
    needs_media_auth: bool = False
    recovery_task: asyncio.Task | None = None
    last_error: str | None = None
    media_lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    def attach(self):
        helper = self.helper
        helper.subscribe(lambda event: self.event(event) if self.helper is helper else None)

    def event(self, event):
        if event.get("event") == "push_error" and event.get("code") in (
            "reauth_required",
            "resume_required",
            "events_failed",
        ):
            self.last_error = event["code"]
            self.available = False
            if self.last_error == "reauth_required" and self.started and not self.stopping:
                self.entry.async_start_reauth(self.hass)
        if event.get("event") == "helper_stopped":
            self.available = False
            if (
                self.started
                and not self.stopping
                and (self.recovery_task is None or self.recovery_task.done())
            ):
                self.recovery_task = asyncio.create_task(self._recover())
        if event.get("event") == "availability":
            self.available = event.get("available") is True
        for listener in tuple(self.listeners):
            listener(event)

    async def _recover(self):
        delay = 1
        while not self.stopping:
            helper = None
            try:
                helper = await Helper.async_start()
                self.helper = helper
                self.attach()
                ready = await helper.async_request(
                    "resume_events",
                    device_id=self.device["id"],
                    state_dir=self.hass.config.path(".storage", DOMAIN, self.entry.entry_id),
                )
                if ready.get("ready") is not True:
                    raise HelperError("events_failed")
                self.needs_media_auth = True
                self.event({"event": "availability", "available": True})
                return
            except HelperError as error:
                if helper is not None:
                    await helper.async_close()
                if error.code not in ("helper_unavailable", "helper_stopped", "timeout"):
                    self.last_error = error.code
                    if error.code in ("reauth_required", "not_authenticated", "login_failed"):
                        self.entry.async_start_reauth(self.hass)
                    return
            except asyncio.CancelledError:
                if helper is not None:
                    await helper.async_close()
                raise
            await asyncio.sleep(delay)
            delay = min(delay * 2, 30)

    async def async_open_media(self, offer):
        async with self.media_lock:
            if self.needs_media_auth:
                prepared = await self.helper.async_request(
                    "prepare_media",
                    email=self.entry.data["email"],
                    password=self.entry.data["password"],
                    device_id=self.device["id"],
                )
                if prepared.get("credentials_ready") is not True:
                    raise HelperError("credentials_failed")
                self.needs_media_auth = False
            return await self.helper.async_request("open_media", offer=offer)

    async def async_stop(self):
        self.stopping = True
        self.started = False
        if self.recovery_task is not None:
            self.recovery_task.cancel()
            await asyncio.gather(self.recovery_task, return_exceptions=True)
        await self.helper.async_close()
        self.event({"event": "availability", "available": False})

    def subscribe(self, listener):
        self.listeners.add(listener)
        return lambda: self.listeners.discard(listener)


async def async_setup(hass, config):
    from .frontend_assets import async_register as async_register_frontend
    from .websocket import async_register

    async_register(hass)
    async_register_frontend(hass)
    return True


async def async_setup_entry(hass, entry):
    helper = None
    try:
        helper = await Helper.async_start()
        result = await helper.async_request(
            "login", email=entry.data["email"], password=entry.data["password"]
        )
        device = next(
            (device for device in result["devices"] if device["id"] == entry.data["device_id"]),
            None,
        )
        if device is None:
            raise HelperError("credentials_failed")
        selected = await helper.async_request("select_device", device_id=device["id"])
        if selected.get("credentials_ready") is not True:
            raise HelperError("credentials_failed")
        runtime = Runtime(helper, device, hass=hass, entry=entry)
        runtime.attach()
        ready = await helper.async_request(
            "start_events",
            state_dir=hass.config.path(".storage", DOMAIN, entry.entry_id),
            **({"reauthenticate": True} if entry.data.get("reauthenticate") is True else {}),
        )
        if ready.get("ready") is not True:
            raise HelperError("events_failed")
        if entry.data.get("reauthenticate") is True:
            hass.config_entries.async_update_entry(
                entry,
                data={key: value for key, value in entry.data.items() if key != "reauthenticate"},
            )
        runtime.available = True
        entry.runtime_data = runtime
        await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
        runtime.started = True

        async def stop(event):
            from .websocket import async_close_entry

            await async_close_entry(hass, entry.entry_id)
            await runtime.async_stop()

        entry.async_on_unload(hass.bus.async_listen_once(EVENT_HOMEASSISTANT_STOP, stop))
    except HelperError as error:
        if helper is not None:
            await helper.async_close()
        if error.code in ("login_failed", "not_authenticated", "reauth_required"):
            raise ConfigEntryAuthFailed("Authentication failed") from None
        raise ConfigEntryError(f"Helper unavailable: {error.code}") from None
    except BaseException:
        if helper is not None:
            await helper.async_close()
        raise
    return True


async def async_unload_entry(hass, entry):
    if not await hass.config_entries.async_unload_platforms(entry, PLATFORMS):
        return False
    from .websocket import async_close_entry

    await async_close_entry(hass, entry.entry_id)
    await entry.runtime_data.async_stop()
    return True
