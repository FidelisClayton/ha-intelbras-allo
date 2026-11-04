"""Standard Home Assistant account and device selection forms."""

import probatio as vol

from homeassistant import config_entries
from homeassistant.core import callback
from homeassistant.helpers import selector

from .const import DOMAIN
from .helper import Helper, HelperError


def credentials(email=None):
    return vol.Schema(
        {
            vol.Required("email", **({"default": email} if email else {})): selector.TextSelector(
                selector.TextSelectorConfig(type=selector.TextSelectorType.EMAIL)
            ),
            vol.Required("password"): selector.TextSelector(
                selector.TextSelectorConfig(type=selector.TextSelectorType.PASSWORD)
            ),
        }
    )


class AlloConfigFlow(config_entries.ConfigFlow, domain=DOMAIN):
    VERSION = 1

    def __init__(self):
        self._helper = None
        self._data = {}
        self._devices = {}
        self._reauth = False

    async def _close(self):
        if self._helper is not None:
            helper, self._helper = self._helper, None
            await helper.async_close()

    @callback
    def async_remove(self):
        self._data = {}
        if self._helper is not None:
            self.hass.async_create_task(self._close())

    async def async_step_user(self, user_input=None):
        return await self._credentials("user", user_input)

    async def async_step_reauth(self, entry_data):
        self._reauth = True
        return await self.async_step_reauth_confirm()

    async def async_step_reauth_confirm(self, user_input=None):
        return await self._credentials("reauth_confirm", user_input)

    async def _credentials(self, step, user_input):
        errors = {}
        entry = self._get_reauth_entry() if self._reauth else None
        email = entry.data["email"] if entry else None
        if user_input is not None:
            canonical = user_input["email"].strip().casefold()
            if not canonical or "@" not in canonical or not user_input["password"]:
                errors["base"] = "invalid_auth"
            elif entry and canonical != entry.data["email"]:
                errors["base"] = "account_mismatch"
            else:
                await self._close()
                try:
                    self._helper = await Helper.async_start()
                    result = await self._helper.async_request(
                        "login", email=canonical, password=user_input["password"]
                    )
                    self._devices = {device["id"]: device for device in result["devices"]}
                    self._data = {"email": canonical, "password": user_input["password"]}
                    if not self._devices:
                        await self._close()
                        return self.async_abort(reason="no_devices")
                    if entry:
                        if entry.data["device_id"] not in self._devices:
                            await self._close()
                            return self.async_abort(reason="device_missing")
                        return await self.async_step_device({"device_id": entry.data["device_id"]})
                    return await self.async_step_device()
                except HelperError as error:
                    await self._close()
                    self._data = {}
                    errors["base"] = (
                        "invalid_auth" if error.code == "login_failed" else "cannot_connect"
                    )
        return self.async_show_form(step_id=step, data_schema=credentials(email), errors=errors)

    async def async_step_device(self, user_input=None):
        errors = {}
        if user_input is not None:
            device_id = user_input["device_id"]
            if device_id not in self._devices:
                errors["base"] = "device_missing"
            else:
                try:
                    selected = await self._helper.async_request(
                        "select_device", device_id=device_id
                    )
                    if selected.get("credentials_ready") is not True:
                        raise HelperError("credentials_failed")
                except HelperError:
                    await self._close()
                    self._data = {}
                    return self.async_abort(reason="cannot_connect")
                await self._close()
                data, self._data = {**self._data, "device_id": device_id}, {}
                await self.async_set_unique_id(f"{data['email']}:{device_id}")
                if self._reauth:
                    self._abort_if_unique_id_mismatch()
                    data["reauthenticate"] = True
                    return self.async_update_reload_and_abort(self._get_reauth_entry(), data=data)
                self._abort_if_unique_id_configured()
                return self.async_create_entry(title=self._devices[device_id]["name"], data=data)
        return self.async_show_form(
            step_id="device",
            errors=errors,
            data_schema=vol.Schema(
                {
                    vol.Required("device_id"): selector.SelectSelector(
                        selector.SelectSelectorConfig(
                            options=[
                                selector.SelectOptionDict(value=key, label=device["name"])
                                for key, device in self._devices.items()
                            ],
                            mode=selector.SelectSelectorMode.DROPDOWN,
                        )
                    )
                }
            ),
        )
