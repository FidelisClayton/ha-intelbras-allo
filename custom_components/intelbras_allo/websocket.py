"""Authorized media sessions owned by one HA websocket connection and user."""

from dataclasses import dataclass
import probatio as vol

from homeassistant.auth.permissions.const import POLICY_CONTROL, POLICY_READ
from homeassistant.components import websocket_api
from homeassistant.core import callback
from homeassistant.helpers import entity_registry as er

from .const import DOMAIN
from .helper import HelperError, MAX_LINE

_SESSIONS = f"{DOMAIN}_sessions"


@dataclass
class Session:
    connection: object
    user_id: str
    entity_id: str
    entry_id: str
    helper: object
    unsubscribe: object = None


def _allowed(connection, entity_id):
    return (
        connection.user is not None
        and connection.user.is_active
        and connection.user.permissions.check_entity(entity_id, POLICY_READ)
    )


async def _close(hass, session_id, session):
    hass.data[_SESSIONS].pop(session_id, None)
    session.connection.subscriptions.pop((DOMAIN, session_id), None)
    if session.unsubscribe:
        session.unsubscribe()
    try:
        await session.helper.async_request("close_media", session_id=session_id)
    except HelperError:
        pass


@websocket_api.websocket_command(
    {
        vol.Required("type"): f"{DOMAIN}/open_media",
        vol.Required("entity_id"): str,
        vol.Required("offer"): vol.All(str, vol.Length(min=1, max=MAX_LINE)),
    }
)
@websocket_api.async_response
async def ws_open_media(hass, connection, msg):
    entity_id = msg["entity_id"]
    if not _allowed(connection, entity_id):
        connection.send_error(msg["id"], "unauthorized", "Camera read permission required")
        return
    registry_entry = er.async_get(hass).async_get(entity_id)
    entry = (
        hass.config_entries.async_get_entry(registry_entry.config_entry_id)
        if registry_entry and registry_entry.platform == DOMAIN and entity_id.startswith("camera.")
        else None
    )
    runtime = getattr(entry, "runtime_data", None)
    if (
        runtime is None
        or not runtime.available
        or runtime.camera is None
        or runtime.camera.entity_id != entity_id
    ):
        connection.send_error(msg["id"], "unavailable", "Camera unavailable")
        return
    user_id = connection.user.id
    # Install disconnect cleanup before awaiting the helper, including during negotiation.
    connected = True
    session_id = None

    @callback
    def disconnect():
        nonlocal connected
        connected = False
        if session_id in hass.data[_SESSIONS]:
            hass.async_create_task(_close(hass, session_id, hass.data[_SESSIONS][session_id]))

    connection.subscriptions[msg["id"]] = disconnect
    try:
        result = await runtime.async_open_media(msg["offer"])
    except HelperError as error:
        connection.subscriptions.pop(msg["id"], None)
        if connected:
            connection.send_error(msg["id"], error.code, "Unable to open media")
        return
    connection.subscriptions.pop(msg["id"], None)
    session_id = result["session_id"]
    session = Session(connection, user_id, entity_id, entry.entry_id, runtime.helper)
    hass.data[_SESSIONS][session_id] = session
    if not connected or not _allowed(connection, entity_id) or connection.user.id != user_id:
        await _close(hass, session_id, session)
        if connected:
            connection.send_error(msg["id"], "unauthorized", "Camera read permission required")
        return
    connection.subscriptions[(DOMAIN, session_id)] = disconnect

    @callback
    def media_event(event):
        if event.get("event") == "media_closed" and event.get("session_id") == session_id:
            hass.data[_SESSIONS].pop(session_id, None)
            connection.subscriptions.pop((DOMAIN, session_id), None)
            session.unsubscribe()

    session.unsubscribe = runtime.helper.subscribe(media_event)
    connection.send_result(msg["id"], {"session_id": session_id, "answer": result["answer"]})


@websocket_api.async_response
async def ws_session(hass, connection, msg):
    session_id = msg["session_id"]
    session = hass.data[_SESSIONS].get(session_id)
    if (
        session is None
        or session.connection is not connection
        or connection.user is None
        or session.user_id != connection.user.id
    ):
        connection.send_error(msg["id"], "unauthorized", "Session unavailable")
        return
    op = msg["type"].split("/")[1]
    if op in ("talk_start", "talk_keepalive"):
        if not _allowed(
            connection, session.entity_id
        ) or not connection.user.permissions.check_entity(session.entity_id, POLICY_CONTROL):
            await _close(hass, session_id, session)
            connection.send_error(msg["id"], "unauthorized", "Camera control permission required")
            return
        entry = hass.config_entries.async_get_entry(session.entry_id)
        runtime = getattr(entry, "runtime_data", None)
        if (
            entry is None
            or entry.domain != DOMAIN
            or runtime is None
            or runtime.helper is not session.helper
            or not runtime.available
        ):
            await _close(hass, session_id, session)
            connection.send_error(msg["id"], "unavailable", "Camera unavailable")
            return
    try:
        if op == "close_media":
            await _close(hass, session_id, session)
            result = {"closed": True}
        else:
            result = await session.helper.async_request(op, session_id=session_id)
    except HelperError as error:
        connection.send_error(msg["id"], error.code, "Media command failed")
        return
    connection.send_result(msg["id"], result)


@callback
def async_register(hass):
    hass.data.setdefault(_SESSIONS, {})
    websocket_api.async_register_command(hass, ws_open_media)
    for op in ("close_media", "talk_start", "talk_keepalive", "talk_stop"):
        command = f"{DOMAIN}/{op}"
        schema = websocket_api.messages.BASE_COMMAND_MESSAGE_SCHEMA.extend(
            {vol.Required("type"): command, vol.Required("session_id"): str}
        )
        websocket_api.async_register_command(hass, command, ws_session, schema)


async def async_close_entry(hass, entry_id):
    for session_id, session in list(hass.data.get(_SESSIONS, {}).items()):
        if session.entry_id == entry_id:
            await _close(hass, session_id, session)
