"""Exercise media ownership and permission boundaries with real HA registry entries."""

import asyncio
from types import SimpleNamespace
from unittest.mock import Mock

import pytest

from custom_components.intelbras_allo import websocket as ws


@pytest.fixture
async def media(hass, entry, fake_helper):
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    entity_id = entry.runtime_data.camera.entity_id
    permissions = Mock()
    permissions.check_entity.return_value = True
    connection = SimpleNamespace(
        user=SimpleNamespace(id="synthetic-user", is_active=True, permissions=permissions),
        subscriptions={},
        send_result=Mock(),
        send_error=Mock(),
    )
    yield entity_id, connection
    await hass.config_entries.async_unload(entry.entry_id)


async def open_media(hass, connection, entity_id):
    await ws.ws_open_media.__wrapped__(
        hass,
        connection,
        {
            "id": 1,
            "type": "intelbras_allo/open_media",
            "entity_id": entity_id,
            "offer": "synthetic-offer",
        },
    )


async def command(hass, connection, op):
    await ws.ws_session.__wrapped__(
        hass,
        connection,
        {"id": 2, "type": "intelbras_allo/" + op, "session_id": "synthetic-session"},
    )


async def test_permission_denied_before_helper(hass, media, fake_helper):
    entity_id, connection = media
    connection.user.permissions.check_entity.return_value = False
    fake_helper.async_request.reset_mock()
    await open_media(hass, connection, entity_id)
    assert connection.send_error.call_args.args[1] == "unauthorized"
    fake_helper.async_request.assert_not_awaited()


async def test_foreign_entity_rejected(hass, media, fake_helper):
    _, connection = media
    fake_helper.async_request.reset_mock()
    await open_media(hass, connection, "camera.some_other_integration")
    assert connection.send_error.call_args.args[1] == "unavailable"
    fake_helper.async_request.assert_not_awaited()


@pytest.mark.parametrize("op", ["talk_start", "talk_keepalive", "talk_stop", "close_media"])
async def test_other_connection_and_user_rejected(hass, media, fake_helper, op):
    entity_id, connection = media
    await open_media(hass, connection, entity_id)
    assert connection.send_result.call_args.args[1] == {
        "session_id": "synthetic-session",
        "answer": "synthetic-answer",
    }
    other = SimpleNamespace(**vars(connection))
    other.send_error = Mock()
    fake_helper.async_request.reset_mock()
    await command(hass, other, op)
    assert other.send_error.call_args.args[1] == "unauthorized"
    connection.user = SimpleNamespace(**vars(connection.user))
    connection.user.id = "foreign-user"
    await command(hass, connection, op)
    assert connection.send_error.call_args.args[1] == "unauthorized"
    fake_helper.async_request.assert_not_awaited()


async def test_talk_stop_close_and_disconnect(hass, media, fake_helper):
    entity_id, connection = media
    await open_media(hass, connection, entity_id)
    await command(hass, connection, "talk_start")
    assert connection.send_result.call_args.args[1] == {"talk_active": True}
    await command(hass, connection, "talk_stop")
    assert connection.send_result.call_args.args[1] == {"talk_active": False}
    for unsubscribe in list(connection.subscriptions.values()):
        unsubscribe()
    connection.subscriptions.clear()
    await hass.async_block_till_done()
    fake_helper.async_request.assert_awaited_with("close_media", session_id="synthetic-session")
    assert not hass.data[ws._SESSIONS]
    await command(hass, connection, "talk_start")
    assert connection.send_error.call_args.args[1] == "unauthorized"


async def test_disconnect_during_open(hass, media, fake_helper):
    entity_id, connection = media
    started = asyncio.Event()
    release = asyncio.Event()
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "open_media":
            started.set()
            await release.wait()
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    task = asyncio.create_task(open_media(hass, connection, entity_id))
    await started.wait()
    for unsubscribe in list(connection.subscriptions.values()):
        unsubscribe()
    connection.subscriptions.clear()
    release.set()
    await task
    fake_helper.async_request.assert_awaited_with("close_media", session_id="synthetic-session")
    assert not hass.data[ws._SESSIONS]
    connection.send_result.assert_not_called()


async def test_wire_schema_rejects_host_and_device_override(hass, media):
    _, schema = hass.data["websocket_api"]["intelbras_allo/open_media"]
    import probatio

    for private_field in ("host", "device_id", "password"):
        with pytest.raises(probatio.Invalid):
            schema(
                {
                    "id": 1,
                    "type": "intelbras_allo/open_media",
                    "entity_id": "camera.front_door",
                    "offer": "synthetic-offer",
                    private_field: "synthetic-value",
                }
            )


def read_only_permissions(hass):
    from homeassistant.auth.permissions import PolicyPermissions
    from homeassistant.auth.permissions.models import PermissionLookup
    from homeassistant.helpers import device_registry as dr, entity_registry as er

    return PolicyPermissions(
        {"entities": {"all": {"read": True}}},
        PermissionLookup(er.async_get(hass), dr.async_get(hass)),
    )


@pytest.mark.parametrize("op", ["talk_start", "talk_keepalive"])
async def test_read_only_user_can_view_but_cannot_talk(hass, media, fake_helper, op):
    entity_id, connection = media
    connection.user.permissions = read_only_permissions(hass)
    await open_media(hass, connection, entity_id)
    assert connection.send_result.call_args.args[1]["answer"] == "synthetic-answer"
    fake_helper.async_request.reset_mock()
    await command(hass, connection, op)
    assert connection.send_error.call_args.args[1] == "unauthorized"
    assert all(call.args[0] != "talk_start" for call in fake_helper.async_request.await_args_list)
    assert not hass.data[ws._SESSIONS]


@pytest.mark.parametrize("op", ["talk_stop", "close_media"])
@pytest.mark.parametrize("revoked", ["read", "control", "inactive"])
async def test_owner_can_cleanup_after_permissions_revoked(hass, media, fake_helper, op, revoked):
    entity_id, connection = media
    await open_media(hass, connection, entity_id)
    await command(hass, connection, "talk_start")
    if revoked == "read":
        connection.user.permissions.check_entity.return_value = False
    elif revoked == "control":
        connection.user.permissions = read_only_permissions(hass)
    else:
        connection.user.is_active = False
    fake_helper.async_request.reset_mock()
    await command(hass, connection, op)
    fake_helper.async_request.assert_awaited_with(op, session_id="synthetic-session")
    assert connection.send_result.call_args.args[1] == (
        {"closed": True} if op == "close_media" else {"talk_active": False}
    )


@pytest.mark.parametrize("revoked", ["read", "inactive", "entry"])
async def test_revocation_closes_backend_before_rejecting_talk(
    hass, media, fake_helper, entry, monkeypatch, revoked
):
    entity_id, connection = media
    await open_media(hass, connection, entity_id)
    if revoked == "read":
        connection.user.permissions.check_entity.return_value = False
    elif revoked == "inactive":
        connection.user.is_active = False
    else:
        monkeypatch.setattr(hass.config_entries, "async_get_entry", lambda _: None)
    fake_helper.async_request.reset_mock()
    await command(hass, connection, "talk_start")
    fake_helper.async_request.assert_awaited_once_with(
        "close_media", session_id="synthetic-session"
    )
    assert not hass.data[ws._SESSIONS]
    assert connection.send_error.call_args.args[1] == (
        "unavailable" if revoked == "entry" else "unauthorized"
    )
    monkeypatch.undo()


@pytest.mark.parametrize("revoked", ["inactive", "missing", "different", "read"])
async def test_identity_changes_during_open_close_backend(hass, media, fake_helper, revoked):
    entity_id, connection = media
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "open_media":
            if revoked == "inactive":
                connection.user.is_active = False
            elif revoked == "missing":
                connection.user = None
            elif revoked == "read":
                connection.user.permissions.check_entity.return_value = False
            else:
                connection.user.id = "different-user"
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    await open_media(hass, connection, entity_id)
    fake_helper.async_request.assert_awaited_with("close_media", session_id="synthetic-session")
    assert not hass.data[ws._SESSIONS]
    connection.send_result.assert_not_called()
    connection.send_error.assert_called_once_with(
        1, "unauthorized", "Camera read permission required"
    )


async def test_talk_keepalive_renews_owner_session(hass, media, fake_helper):
    entity_id, connection = media
    await open_media(hass, connection, entity_id)
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "talk_keepalive":
            return {"talk_active": True}
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    await command(hass, connection, "talk_keepalive")
    fake_helper.async_request.assert_awaited_with("talk_keepalive", session_id="synthetic-session")
    assert connection.send_result.call_args.args[1] == {"talk_active": True}
    assert "intelbras_allo/talk_keepalive" in hass.data["websocket_api"]
