"""Private subprocess behavior, using synthetic pipe peers only."""

import asyncio
from pathlib import Path
import stat
import threading
from unittest.mock import AsyncMock, patch

import pytest

from custom_components.intelbras_allo.helper import Helper, HelperError, MAX_LINE


class Process:
    def __init__(self):
        self.stdout = asyncio.StreamReader(limit=MAX_LINE + 1)
        self.stdin = self
        self.returncode = None
        self.requests = []
        self.stopped = False

    def write(self, value):
        self.requests.append(value)

    async def drain(self):
        pass

    def terminate(self):
        self.stopped = True
        self.returncode = 0
        self.stdout.feed_eof()

    def kill(self):
        self.terminate()

    async def wait(self):
        return self.returncode


@pytest.fixture(autouse=True)
def packaged_binary(tmp_path, monkeypatch):
    executable = tmp_path / "bin" / "allo-ha-linux-amd64"
    executable.parent.mkdir()
    executable.write_bytes(b"synthetic helper")
    executable.chmod(0o644)
    monkeypatch.setattr(
        "custom_components.intelbras_allo.helper.__file__", str(tmp_path / "helper.py")
    )
    return executable


@pytest.fixture
async def helper():
    process = Process()
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=process)) as spawn:
        result = await Helper.async_start()
        yield result, process, spawn
        await result.async_close()


async def test_private_pipes_and_events(helper, caplog):
    client, process, spawn = helper
    events = []
    client.subscribe(events.append)
    request = asyncio.create_task(client.async_request("login", email="private", password="secret"))
    await asyncio.sleep(0)
    process.stdout.feed_data(b'{"event":"availability","available":true}\n')
    process.stdout.feed_data(b'{"id":1,"result":{"devices":[]}}\n')
    assert await request == {"devices": []}
    assert events == [{"event": "availability", "available": True}]
    assert len(spawn.call_args.args) == 1
    assert spawn.call_args.kwargs["stderr"] == asyncio.subprocess.DEVNULL
    assert spawn.call_args.kwargs["stdin"] == asyncio.subprocess.PIPE
    assert spawn.call_args.kwargs["stdout"] == asyncio.subprocess.PIPE
    assert "secret" not in caplog.text


async def test_eof_cancels_inflight(helper):
    client, process, _ = helper
    request = asyncio.create_task(client.async_request("login", password="secret"))
    await asyncio.sleep(0)
    process.stdout.feed_eof()
    with pytest.raises(HelperError, match="helper_stopped"):
        await request


async def test_timeout_stops_child(helper):
    client, process, _ = helper
    client.timeout = 0.01
    with pytest.raises(HelperError, match="timeout"):
        await client.async_request("login", password="secret")
    assert process.stopped


async def test_oversize_output_stops_child(helper):
    client, process, _ = helper
    request = asyncio.create_task(client.async_request("login"))
    await asyncio.sleep(0)
    process.stdout.feed_data(b"x" * (MAX_LINE + 2) + b"\n")
    with pytest.raises(HelperError):
        await request
    await asyncio.sleep(0)
    assert process.stopped


async def test_remote_error_is_allowlisted(helper, caplog):
    client, process, _ = helper
    request = asyncio.create_task(client.async_request("login", password="private-secret"))
    await asyncio.sleep(0)
    process.stdout.feed_data(b'{"id":1,"error":{"code":"private-secret"}}\n')
    with pytest.raises(HelperError, match="^helper_error$") as error:
        await request
    assert "private-secret" not in str(error.value)
    assert "private-secret" not in caplog.text


async def test_unsupported_platform_never_spawns(monkeypatch):
    monkeypatch.setattr("platform.machine", lambda: "aarch64")
    spawn = AsyncMock()
    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)
    with pytest.raises(HelperError, match="unsupported_platform"):
        await Helper.async_start()
    spawn.assert_not_awaited()


async def test_extracted_binary_restores_execute_off_loop(packaged_binary, monkeypatch):
    original = Path.chmod

    def chmod(path, mode):
        assert threading.current_thread() is not threading.main_thread()
        original(path, mode)

    monkeypatch.setattr(Path, "chmod", chmod)
    process = Process()
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=process)):
        client = await Helper.async_start()
        assert stat.S_IMODE(packaged_binary.stat().st_mode) == 0o744
        await client.async_close()


async def test_readonly_binary_reports_unavailable(packaged_binary, monkeypatch):
    def readonly(*args):
        raise PermissionError("private filesystem details")

    monkeypatch.setattr(Path, "chmod", readonly)
    spawn = AsyncMock()
    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)
    with pytest.raises(HelperError, match="^helper_unavailable$"):
        await Helper.async_start()
    spawn.assert_not_awaited()


async def test_binary_symlink_is_not_executed(packaged_binary, tmp_path, monkeypatch):
    target = tmp_path / "unreviewed"
    target.write_bytes(b"synthetic")
    packaged_binary.unlink()
    packaged_binary.symlink_to(target)
    spawn = AsyncMock()
    monkeypatch.setattr(asyncio, "create_subprocess_exec", spawn)
    with pytest.raises(HelperError, match="^helper_unavailable$"):
        await Helper.async_start()
    spawn.assert_not_awaited()


@pytest.mark.parametrize(
    ("op", "seconds"), [("start_events", 95), ("resume_events", 95), ("login", 45)]
)
async def test_request_waits_for_operation_budget(helper, op, seconds):
    client, process, _ = helper
    with patch("asyncio.timeout", wraps=asyncio.timeout) as timeout:
        request = asyncio.create_task(client.async_request(op))
        await asyncio.sleep(0)
        process.stdout.feed_data(b'{"id":1,"result":{"ready":true}}\n')
        assert await request == {"ready": True}
        assert timeout.call_args.args == (seconds,)


async def test_child_exit_emits_stopped_after_reaping_once(helper):
    client, process, _ = helper
    events = []
    client.subscribe(lambda event: events.append((event, process.stopped)))
    process.stdout.feed_eof()
    await client._reader
    assert events[-1] == ({"event": "helper_stopped"}, True)
    assert sum(event.get("event") == "helper_stopped" for event, _ in events) == 1
