"""Common device identity and push availability."""

from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.entity import Entity

from .const import DOMAIN


class AlloEntity(Entity):
    _attr_has_entity_name = True
    _attr_should_poll = False

    def __init__(self, entry):
        self.runtime = entry.runtime_data
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, entry.unique_id)},
            manufacturer="Intelbras",
            model=self.runtime.device["model"],
            name=self.runtime.device["name"],
        )
        self._identity = entry.unique_id

    @property
    def available(self):
        return self.runtime.available

    async def async_added_to_hass(self):
        self.async_on_remove(self.runtime.subscribe(self._handle_event))

    def _handle_event(self, event):
        if event.get("event") == "availability":
            self.async_write_ha_state()
