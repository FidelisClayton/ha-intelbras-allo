"""Native full-SDP WebRTC camera using the pinned HA camera API."""

from homeassistant.components.camera import Camera, CameraEntityFeature, WebRTCAnswer, WebRTCError
from homeassistant.core import callback

from .entity import AlloEntity
from .helper import HelperError


async def async_setup_entry(hass, entry, async_add_entities):
    camera = AlloCamera(entry)
    entry.runtime_data.camera = camera
    async_add_entities([camera])


class AlloCamera(AlloEntity, Camera):
    _attr_name = "Camera"
    _attr_supported_features = CameraEntityFeature.STREAM

    def __init__(self, entry):
        Camera.__init__(self)
        AlloEntity.__init__(self, entry)
        self._attr_unique_id = f"{self._identity}:camera"
        self._sessions = {}

    async def async_camera_image(self, width=None, height=None):
        return None

    async def async_handle_async_webrtc_offer(self, offer_sdp, session_id, send_message):
        self._sessions[session_id] = None
        try:
            result = await self.runtime.async_open_media(offer_sdp)
        except HelperError as error:
            self._sessions.pop(session_id, None)
            send_message(WebRTCError(error.code, "Unable to open media"))
            return
        if session_id not in self._sessions:
            await self._close(result["session_id"])
            return
        self._sessions[session_id] = result["session_id"]
        send_message(WebRTCAnswer(result["answer"]))

    async def async_on_webrtc_candidate(self, session_id, candidate):
        # The helper accepts complete SDP; clients gather ICE before sending their offer.
        return

    @callback
    def close_webrtc_session(self, session_id):
        if helper_id := self._sessions.pop(session_id, None):
            self.hass.async_create_task(self._close(helper_id))

    async def _close(self, helper_id):
        try:
            await self.runtime.helper.async_request("close_media", session_id=helper_id)
        except HelperError:
            pass

    async def async_will_remove_from_hass(self):
        for helper_id in list(self._sessions.values()):
            if helper_id is not None:
                await self._close(helper_id)
        self._sessions.clear()
