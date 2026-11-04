"""Exercise setup forms, real HA entities and lifecycle without network access."""

import asyncio
from datetime import timedelta
from unittest.mock import AsyncMock, patch
from homeassistant.util import dt as dt_util
from pytest_homeassistant_custom_component.common import async_fire_time_changed

from homeassistant import config_entries
from homeassistant.data_entry_flow import FlowResultType
import pytest

from custom_components.intelbras_allo import async_setup_entry
from custom_components.intelbras_allo.helper import HelperError


async def test_config_devices_and_duplicate(hass, fake_helper, entry):
    result = await hass.config_entries.flow.async_init("intelbras_allo", context={"source": "user"})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {"email": " TEST@example.test ", "password": "synthetic-secret"}
    )
    assert result["step_id"] == "device"
    assert len(result["data_schema"].schema) == 1
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {"device_id": "synthetic-a"}
    )
    assert result["type"] == FlowResultType.ABORT
    assert result["reason"] == "already_configured"
    fake_helper.async_close.assert_awaited()


async def test_create_entry(hass, fake_helper):
    with patch("custom_components.intelbras_allo.async_setup_entry", return_value=True):
        result = await hass.config_entries.flow.async_init(
            "intelbras_allo", context={"source": "user"}
        )
        result = await hass.config_entries.flow.async_configure(
            result["flow_id"], {"email": "TEST@example.test", "password": "synthetic-secret"}
        )
        result = await hass.config_entries.flow.async_configure(
            result["flow_id"], {"device_id": "synthetic-b"}
        )
        assert result["type"] == FlowResultType.CREATE_ENTRY
        assert result["data"] == {
            "email": "test@example.test",
            "password": "synthetic-secret",
            "device_id": "synthetic-b",
        }
        assert result["result"].unique_id == "test@example.test:synthetic-b"
        await hass.async_block_till_done()
    fake_helper.async_close.assert_awaited()


async def test_wrong_login_and_abort_cleanup(hass, fake_helper):
    fake_helper.async_request.side_effect = HelperError("login_failed")
    result = await hass.config_entries.flow.async_init("intelbras_allo", context={"source": "user"})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {"email": "test@example.test", "password": "wrong"}
    )
    assert result["errors"] == {"base": "invalid_auth"}
    fake_helper.async_close.assert_awaited()
    hass.config_entries.flow.async_abort(result["flow_id"])


async def test_cancel_device_step_closes_helper(hass, fake_helper):
    result = await hass.config_entries.flow.async_init("intelbras_allo", context={"source": "user"})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {"email": "test@example.test", "password": "synthetic-secret"}
    )
    hass.config_entries.flow.async_abort(result["flow_id"])
    await hass.async_block_till_done()
    fake_helper.async_close.assert_awaited()


async def test_reauth_cannot_change_account(hass, fake_helper, entry):
    result = await hass.config_entries.flow.async_init(
        "intelbras_allo",
        context={"source": config_entries.SOURCE_REAUTH, "entry_id": entry.entry_id},
        data=entry.data,
    )
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {"email": "other@example.test", "password": "synthetic-secret"}
    )
    assert result["errors"] == {"base": "account_mismatch"}
    fake_helper.async_request.assert_not_awaited()
    hass.config_entries.flow.async_abort(result["flow_id"])


async def test_setup_failure_does_not_retry(hass, entry, fake_helper):
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "start_events":
            raise HelperError("unknown_op")
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    from homeassistant.exceptions import ConfigEntryError

    with pytest.raises(ConfigEntryError, match="unknown_op"):
        await async_setup_entry(hass, entry)
    fake_helper.async_close.assert_awaited()
    assert not await hass.config_entries.async_setup(entry.entry_id)
    assert entry.state == config_entries.ConfigEntryState.SETUP_ERROR
    calls = list(fake_helper.async_request.await_args_list)
    async_fire_time_changed(hass, dt_util.utcnow() + timedelta(minutes=10))
    await hass.async_block_till_done()
    assert fake_helper.async_request.await_args_list == calls


async def test_entities_selected_bell_and_unload(hass, entry, fake_helper):
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    state = hass.states.get("event.front_door_doorbell")
    assert state is not None
    assert state.state == "unknown"
    for listener in tuple(fake_helper.listeners):
        listener({"event": "doorbell", "device_id": "synthetic-b", "channel": 1})
    assert hass.states.get(state.entity_id).state == "unknown"
    for listener in tuple(fake_helper.listeners):
        listener({"event": "doorbell", "device_id": "synthetic-a", "channel": 1})
    assert hass.states.get(state.entity_id).attributes["event_type"] == "ring"
    for listener in tuple(fake_helper.listeners):
        listener({"event": "availability", "available": False})
    assert hass.states.get(state.entity_id).state == "unavailable"
    assert await hass.config_entries.async_unload(entry.entry_id)
    fake_helper.async_close.assert_awaited()


async def test_ha_shutdown_stops_child(hass, entry, fake_helper):
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    fake_helper.async_close.reset_mock()
    await hass.async_stop()
    fake_helper.async_close.assert_awaited()


async def test_reauth_preserves_selected_device(hass, entry, fake_helper):
    with patch("homeassistant.config_entries.ConfigEntries.async_reload", return_value=True):
        result = await hass.config_entries.flow.async_init(
            "intelbras_allo",
            context={"source": config_entries.SOURCE_REAUTH, "entry_id": entry.entry_id},
            data=entry.data,
        )
        result = await hass.config_entries.flow.async_configure(
            result["flow_id"], {"email": "test@example.test", "password": "replacement-secret"}
        )
        assert result["type"] == FlowResultType.ABORT
        assert result["reason"] == "reauth_successful"
        assert entry.data["device_id"] == "synthetic-a"
        assert entry.data["password"] == "replacement-secret"
        assert entry.data["reauthenticate"] is True
        assert entry.unique_id == "test@example.test:synthetic-a"
        await hass.async_block_till_done()


async def test_diagnostics_allowlist_when_not_loaded(hass, entry):
    from custom_components.intelbras_allo.diagnostics import async_get_config_entry_diagnostics

    result = await async_get_config_entry_diagnostics(hass, entry)
    assert result == {"available": False, "platform": "linux-amd64", "integration_version": "0.1.0"}


async def test_reauth_flag_forwarded_and_cleared_only_when_ready(hass, entry, fake_helper):
    hass.config_entries.async_update_entry(entry, data={**entry.data, "reauthenticate": True})
    assert await hass.config_entries.async_setup(entry.entry_id)
    fake_helper.async_request.assert_any_await(
        "start_events",
        state_dir=hass.config.path(".storage", "intelbras_allo", entry.entry_id),
        reauthenticate=True,
    )
    assert "reauthenticate" not in entry.data
    await hass.config_entries.async_unload(entry.entry_id)


async def test_child_recovery_resumes_and_user_open_refreshes_media(hass, entry, fake_helper):
    assert await hass.config_entries.async_setup(entry.entry_id)
    runtime = entry.runtime_data
    old_listener = next(iter(fake_helper.listeners))
    replacement = AsyncMock()
    replacement.subscribe = lambda listener: None
    replacement.async_request.side_effect = lambda op, **args: {
        "resume_events": {"ready": True},
        "prepare_media": {"credentials_ready": True},
        "open_media": {"session_id": "new", "answer": "new-answer"},
    }[op]
    with patch("custom_components.intelbras_allo.Helper.async_start", return_value=replacement):
        old_listener({"event": "helper_stopped"})
        assert not runtime.available
        await runtime.recovery_task
    assert runtime.available
    replacement.async_request.assert_awaited_once_with(
        "resume_events",
        device_id="synthetic-a",
        state_dir=hass.config.path(".storage", "intelbras_allo", entry.entry_id),
    )
    old_listener({"event": "availability", "available": False})
    assert runtime.available
    assert await runtime.async_open_media("offer") == {"session_id": "new", "answer": "new-answer"}
    assert [call.args[0] for call in replacement.async_request.await_args_list] == [
        "resume_events",
        "prepare_media",
        "open_media",
    ]
    replacement.async_request.assert_any_await(
        "prepare_media",
        email="test@example.test",
        password="synthetic-secret",
        device_id="synthetic-a",
    )
    await runtime.async_open_media("offer")
    assert sum(c.args[0] == "prepare_media" for c in replacement.async_request.await_args_list) == 1
    await hass.config_entries.async_unload(entry.entry_id)


@pytest.mark.parametrize("stop", ["unload", "shutdown"])
async def test_unload_during_recovery_backoff_stops_respawn(hass, entry, fake_helper, stop):
    assert await hass.config_entries.async_setup(entry.entry_id)
    runtime = entry.runtime_data
    with patch(
        "custom_components.intelbras_allo.Helper.async_start",
        side_effect=HelperError("helper_unavailable"),
    ) as start:
        next(iter(fake_helper.listeners))({"event": "helper_stopped"})
        await asyncio.sleep(0)
        assert start.call_count == 1
        task = runtime.recovery_task
        if stop == "unload":
            assert await hass.config_entries.async_unload(entry.entry_id)
        else:
            await hass.async_stop()
        assert task.done()
        assert not runtime.available
        assert start.call_count == 1


@pytest.mark.parametrize("code", ["reauth_required", "resume_required", "events_failed"])
async def test_terminal_push_error_requests_owner_without_login(hass, entry, fake_helper, code):
    assert await hass.config_entries.async_setup(entry.entry_id)
    fake_helper.async_request.reset_mock()
    with patch.object(entry, "async_start_reauth") as reauth:
        for listener in tuple(fake_helper.listeners):
            listener({"event": "push_error", "code": code})
        assert reauth.call_count == (1 if code == "reauth_required" else 0)
    assert not entry.runtime_data.available
    assert entry.runtime_data.last_error == code
    fake_helper.async_request.assert_not_awaited()
    await hass.config_entries.async_unload(entry.entry_id)


@pytest.mark.parametrize("code", ["reauth_required", "resume_required", "events_failed"])
async def test_terminal_resume_error_stops_recovery(hass, entry, fake_helper, code):
    assert await hass.config_entries.async_setup(entry.entry_id)
    runtime = entry.runtime_data
    replacement = AsyncMock()
    replacement.subscribe = lambda listener: None
    replacement.async_request.side_effect = HelperError(code)
    with (
        patch(
            "custom_components.intelbras_allo.Helper.async_start", return_value=replacement
        ) as start,
        patch.object(entry, "async_start_reauth") as reauth,
    ):
        next(iter(fake_helper.listeners))({"event": "helper_stopped"})
        await runtime.recovery_task
        assert start.call_count == 1
        assert reauth.call_count == (1 if code == "reauth_required" else 0)
    assert not runtime.available
    replacement.async_close.assert_awaited()
    await hass.config_entries.async_unload(entry.entry_id)


async def test_reauth_required_is_auth_failed_and_keeps_confirmation(hass, entry, fake_helper):
    from homeassistant.exceptions import ConfigEntryAuthFailed

    hass.config_entries.async_update_entry(entry, data={**entry.data, "reauthenticate": True})
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "start_events":
            raise HelperError("reauth_required")
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    with pytest.raises(ConfigEntryAuthFailed):
        await async_setup_entry(hass, entry)
    assert entry.data["reauthenticate"] is True
    fake_helper.async_close.assert_awaited()


async def test_transient_child_recovery_backoff_never_authenticates(hass, entry, fake_helper):
    assert await hass.config_entries.async_setup(entry.entry_id)
    runtime = entry.runtime_data
    replacement = AsyncMock()
    replacement.subscribe = lambda listener: None
    replacement.async_request.return_value = {"ready": True}
    with (
        patch(
            "custom_components.intelbras_allo.Helper.async_start",
            side_effect=[
                HelperError("helper_unavailable"),
                HelperError("helper_unavailable"),
                replacement,
            ],
        ),
        patch("custom_components.intelbras_allo.asyncio.sleep", new_callable=AsyncMock) as sleep,
    ):
        next(iter(fake_helper.listeners))({"event": "helper_stopped"})
        await runtime.recovery_task
    assert [c.args[0] for c in sleep.await_args_list] == [1, 2]
    replacement.async_request.assert_awaited_once_with(
        "resume_events",
        device_id="synthetic-a",
        state_dir=hass.config.path(".storage", "intelbras_allo", entry.entry_id),
    )
    assert runtime.available
    await hass.config_entries.async_unload(entry.entry_id)


async def test_explicit_media_auth_failure_does_not_open_or_retry(hass, entry, fake_helper):
    assert await hass.config_entries.async_setup(entry.entry_id)
    runtime = entry.runtime_data
    runtime.needs_media_auth = True
    fake_helper.async_request.reset_mock()
    fake_helper.async_request.side_effect = HelperError("login_failed")
    with pytest.raises(HelperError, match="login_failed"):
        await runtime.async_open_media("offer")
    fake_helper.async_request.assert_awaited_once_with(
        "prepare_media",
        email="test@example.test",
        password="synthetic-secret",
        device_id="synthetic-a",
    )
    assert runtime.needs_media_auth
    await hass.config_entries.async_unload(entry.entry_id)
