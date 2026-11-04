"""Register the packaged card without changing user dashboard resources."""

from hashlib import sha256
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from homeassistant.components.lovelace.const import LOVELACE_DATA
from homeassistant.components.lovelace.resources import (
    ResourceStorageCollection,
    ResourceYAMLCollection,
)
from homeassistant.const import EVENT_COMPONENT_LOADED

from custom_components.intelbras_allo import frontend_assets as assets


@pytest.fixture
def card_url():
    content = (Path(assets.__file__).parent / "frontend/allo-intercom-card.js").read_bytes()
    return f"/intelbras_allo/allo-intercom-card.js?v={sha256(content).hexdigest()}"


@pytest.fixture
async def resources(hass):
    collection = ResourceStorageCollection(
        hass, SimpleNamespace(async_load=AsyncMock(return_value={}))
    )
    hass.data[LOVELACE_DATA] = SimpleNamespace(resource_mode="storage", resources=collection)
    hass.config.components.add("lovelace")
    yield collection
    await collection.store.async_save({"items": collection.async_items()})


async def test_card_resource_registered_once(hass, resources, card_url):
    assets.async_register(hass)
    assets.async_register(hass)
    await hass.async_block_till_done()
    items = resources.async_items()
    assert len(items) == 1
    assert items[0]["url"] == card_url
    assert items[0]["type"] == "module"


async def test_existing_resource_updated_preserving_other_data(hass, resources, card_url):
    existing = await resources.async_create_item(
        {"res_type": "js", "url": "/intelbras_allo/allo-intercom-card.js?v=0.1.0"}
    )
    existing["custom_data"] = "preserve me"
    expected = {**existing, "url": card_url}
    assets.async_register(hass)
    await hass.async_block_till_done()
    assert resources.async_items() == [expected]


async def test_unrelated_and_foreign_resources_preserved(hass, resources, card_url):
    urls = [
        "/local/other.js",
        "https://example.com/intelbras_allo/allo-intercom-card.js?v=0.1.0",
        "//example.com/intelbras_allo/allo-intercom-card.js?v=0.1.0",
    ]
    existing = [
        await resources.async_create_item({"res_type": "module", "url": url}) for url in urls
    ]
    assets.async_register(hass)
    await hass.async_block_till_done()
    assert resources.async_items()[:-1] == existing
    assert resources.async_items()[-1]["url"] == card_url


async def test_unchanged_card_does_not_update_resource(hass, resources, card_url, monkeypatch):
    existing = await resources.async_create_item({"res_type": "module", "url": card_url})
    update = AsyncMock(wraps=resources.async_update_item)
    monkeypatch.setattr(resources, "async_update_item", update)
    assets.async_register(hass)
    await hass.async_block_till_done()
    assert resources.async_items() == [existing]
    update.assert_not_awaited()


async def test_changed_bytes_change_url_without_version_change(
    hass, resources, tmp_path, monkeypatch
):
    frontend = tmp_path / "frontend"
    frontend.mkdir()
    script = frontend / "allo-intercom-card.js"
    monkeypatch.setattr(assets, "__file__", str(tmp_path / "frontend_assets.py"))
    script.write_bytes(b"first card")
    assets.async_register(hass)
    await hass.async_block_till_done()
    first = dict(resources.async_items()[0])
    assert first["url"].endswith(sha256(b"first card").hexdigest())

    script.write_bytes(b"updated card")
    hass.data.pop(assets._REGISTERED)
    assets.async_register(hass)
    await hass.async_block_till_done()
    updated_url = f"/intelbras_allo/allo-intercom-card.js?v={sha256(b'updated card').hexdigest()}"
    assert resources.async_items() == [{**first, "url": updated_url}]


async def test_yaml_resources_warn_with_manual_action(hass, caplog, card_url):
    collection = ResourceYAMLCollection([])
    hass.data[LOVELACE_DATA] = SimpleNamespace(resource_mode="yaml", resources=collection)
    hass.config.components.add("lovelace")
    assets.async_register(hass)
    await hass.async_block_till_done()
    assert collection.async_items() == []
    assert card_url in caplog.text
    assert "module" in caplog.text
    assert "YAML" in caplog.text


async def test_http_registered_after_component_setup_once(hass):
    hass.http = SimpleNamespace(async_register_static_paths=AsyncMock())
    assets.async_register(hass)
    assets.async_register(hass)
    assert not hass.http.async_register_static_paths.called
    hass.bus.async_fire(EVENT_COMPONENT_LOADED, {"component": "http"})
    await hass.async_block_till_done()
    configs = hass.http.async_register_static_paths.call_args.args[0]
    assert len(configs) == 1
    assert configs[0].url_path == "/intelbras_allo/allo-intercom-card.js"
    assert Path(configs[0].path).is_file()
    assert Path(configs[0].path).name == "allo-intercom-card.js"
    hass.http.async_register_static_paths.assert_awaited_once()


async def test_static_card_can_be_fetched(hass, hass_client, card_url):
    from homeassistant.setup import async_setup_component

    assert await async_setup_component(hass, "http", {})
    assets.async_register(hass)
    await hass.async_block_till_done()
    client = await hass_client()
    response = await client.get(card_url)
    assert response.status == 200
    assert "allo-intercom-card" in await response.text()
    assert response.content_type in ("application/javascript", "text/javascript")


@pytest.mark.parametrize("current", [False, True])
async def test_yaml_existing_resource_never_written(hass, caplog, card_url, current):
    url = card_url if current else "/intelbras_allo/allo-intercom-card.js?v=0.1.0"
    collection = ResourceYAMLCollection([{"url": url, "type": "module"}])
    hass.data[LOVELACE_DATA] = SimpleNamespace(resource_mode="yaml", resources=collection)
    hass.config.components.add("lovelace")
    assets.async_register(hass)
    await hass.async_block_till_done()
    assert [item["url"] for item in collection.async_items()] == [url]
    if current:
        assert "requires a Lovelace YAML resource" not in caplog.text
    else:
        assert card_url in caplog.text
