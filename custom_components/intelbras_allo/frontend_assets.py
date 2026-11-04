"""Expose the packaged intercom card and register its Lovelace module."""

import logging
from hashlib import sha256
from pathlib import Path
from urllib.parse import urlsplit

from homeassistant.components.http import StaticPathConfig
from homeassistant.components.lovelace.const import LOVELACE_DATA
from homeassistant.core import callback
from homeassistant.setup import async_when_setup

from .const import DOMAIN

_LOGGER = logging.getLogger(__name__)
_CARD_PATH = "/intelbras_allo/allo-intercom-card.js"
_REGISTERED = f"{DOMAIN}_frontend_registered"


def _card_url():
    content = (Path(__file__).parent / "frontend/allo-intercom-card.js").read_bytes()
    return f"{_CARD_PATH}?v={sha256(content).hexdigest()}"


@callback
def async_register(hass):
    """Register once across config entries and their reloads."""
    if hass.data.get(_REGISTERED):
        return
    hass.data[_REGISTERED] = True

    async def register_static(hass, component):
        await hass.http.async_register_static_paths(
            [
                StaticPathConfig(
                    _CARD_PATH, str(Path(__file__).parent / "frontend/allo-intercom-card.js")
                )
            ]
        )

    async def register_resource(hass, component):
        card_url = await hass.async_add_executor_job(_card_url)
        lovelace = hass.data[LOVELACE_DATA]
        resources = lovelace.resources
        await resources.async_get_info()
        existing = None
        for item in resources.async_items():
            url = urlsplit(item["url"])
            if not url.scheme and not url.netloc and url.path == _CARD_PATH:
                existing = item
                break
        if existing and existing["url"] == card_url:
            return
        if lovelace.resource_mode != "storage":
            _LOGGER.warning(
                "Intelbras Allo card requires a Lovelace YAML resource: add URL %s with type module",
                card_url,
            )
            return
        if existing:
            await resources.async_update_item(existing["id"], {"url": card_url})
        else:
            await resources.async_create_item({"res_type": "module", "url": card_url})

    async_when_setup(hass, "http", register_static)
    async_when_setup(hass, "lovelace", register_resource)
