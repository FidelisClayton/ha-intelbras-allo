"""Native camera ownership cleanup during an unfinished offer."""

import asyncio
from types import SimpleNamespace
from unittest.mock import Mock

from custom_components.intelbras_allo import Runtime
from custom_components.intelbras_allo.camera import AlloCamera


async def test_close_during_offer_closes_result(hass, fake_helper):
    started, release = asyncio.Event(), asyncio.Event()
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "open_media":
            started.set()
            await release.wait()
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    runtime = Runtime(fake_helper, {"id": "synthetic-a", "name": "Front", "model": "Allo"})
    camera = AlloCamera(SimpleNamespace(runtime_data=runtime, unique_id="synthetic-identity"))
    camera.hass = hass
    send = Mock()
    task = asyncio.create_task(
        camera.async_handle_async_webrtc_offer("synthetic-offer", "ha-id", send)
    )
    await started.wait()
    camera.close_webrtc_session("ha-id")
    release.set()
    await task
    assert not camera._sessions
    fake_helper.async_request.assert_awaited_with("close_media", session_id="synthetic-session")
    send.assert_not_called()


async def test_stock_camera_prepares_media_after_recovery(hass, fake_helper):
    original = fake_helper.async_request.side_effect

    async def request(op, **args):
        if op == "prepare_media":
            return {"credentials_ready": True}
        return await original(op, **args)

    fake_helper.async_request.side_effect = request
    entry = SimpleNamespace(
        unique_id="synthetic-identity",
        data={
            "email": "test@example.test",
            "password": "secret",
        },
    )
    runtime = Runtime(
        fake_helper,
        {"id": "synthetic-a", "name": "Front", "model": "Allo"},
        entry=entry,
        needs_media_auth=True,
    )
    entry.runtime_data = runtime
    camera = AlloCamera(entry)
    camera.hass = hass
    send = Mock()
    await camera.async_handle_async_webrtc_offer("offer", "ha-id", send)
    assert [c.args[0] for c in fake_helper.async_request.await_args_list] == [
        "prepare_media",
        "open_media",
    ]
    assert send.call_count == 1
    await camera.async_will_remove_from_hass()
