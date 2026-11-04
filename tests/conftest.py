"""Offline Home Assistant integration fixtures."""

from unittest.mock import AsyncMock

import pytest


@pytest.fixture(autouse=True)
def custom_integrations(enable_custom_integrations):
    yield


@pytest.fixture
def fake_helper(monkeypatch):
    from custom_components.intelbras_allo.helper import Helper

    helper = AsyncMock()
    helper.listeners = set()
    helper.subscribe = lambda listener: (
        helper.listeners.add(listener) or (lambda: helper.listeners.discard(listener))
    )

    async def request(op, **args):
        return {
            "login": {
                "devices": [
                    {"id": "synthetic-a", "name": "Front door", "model": "Allo"},
                    {"id": "synthetic-b", "name": "Back door", "model": "Allo"},
                ]
            },
            "select_device": {"credentials_ready": True},
            "start_events": {"ready": True},
            "open_media": {"session_id": "synthetic-session", "answer": "synthetic-answer"},
            "close_media": {"closed": True},
            "talk_start": {"talk_active": True},
            "talk_stop": {"talk_active": False},
        }[op]

    helper.async_request.side_effect = request
    monkeypatch.setattr(Helper, "async_start", AsyncMock(return_value=helper))
    return helper


@pytest.fixture
def entry(hass):
    from pytest_homeassistant_custom_component.common import MockConfigEntry

    entry = MockConfigEntry(
        domain="intelbras_allo",
        unique_id="test@example.test:synthetic-a",
        data={
            "email": "test@example.test",
            "password": "synthetic-secret",
            "device_id": "synthetic-a",
        },
        title="Front door",
    )
    entry.add_to_hass(hass)
    return entry
