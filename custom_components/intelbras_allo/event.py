"""Physical bell presses, never synthesized on startup."""

from homeassistant.components.event import EventDeviceClass, EventEntity

from .entity import AlloEntity


async def async_setup_entry(hass, entry, async_add_entities):
    async_add_entities([AlloDoorbell(entry)])


class AlloDoorbell(AlloEntity, EventEntity):
    _attr_name = "Doorbell"
    _attr_device_class = EventDeviceClass.DOORBELL
    _attr_event_types = ["ring"]

    def __init__(self, entry):
        super().__init__(entry)
        self._attr_unique_id = f"{self._identity}:doorbell"

    def _handle_event(self, event):
        super()._handle_event(event)
        if (
            event.get("event") == "doorbell"
            and event.get("device_id") == self.runtime.device["id"]
            and event.get("channel") == 1
        ):
            self._trigger_event("ring")
            self.async_write_ha_state()
