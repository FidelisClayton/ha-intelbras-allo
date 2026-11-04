"""Bounded private stdio transport for the packaged Go helper."""

import asyncio
from collections.abc import Callable
import json
from pathlib import Path
import platform
import stat

MAX_LINE = 2 * 1024 * 1024
_CODES = frozenset(
    {
        "invalid_request",
        "unknown_op",
        "not_authenticated",
        "busy",
        "invalid_session",
        "login_failed",
        "device_list_failed",
        "credentials_failed",
        "credentials_required",
        "media_failed",
        "invalid_offer",
        "talk_failed",
        "events_failed",
        "discovery_failed",
        "reauth_required",
        "resume_required",
        "talk_inactive",
    }
)


def _prepare_executable(path):
    # HACS uses ZipFile.extractall, which does not restore Unix execute bits.
    mode = path.lstat().st_mode
    if not stat.S_ISREG(mode):
        raise OSError("Helper must be a regular file")
    if not mode & stat.S_IXUSR:
        path.chmod(stat.S_IMODE(mode) | stat.S_IXUSR)


class HelperError(Exception):
    """Expose only fixed error identifiers, never remote text."""

    def __init__(self, code):
        self.code = code
        super().__init__(code)


class Helper:
    """One child per account/device entry, with no shell or credential arguments."""

    timeout = 45

    def __init__(self, process):
        self._process = process
        self._pending = {}
        self._listeners = set()
        self._sequence = 0
        self._write_lock = asyncio.Lock()
        self._closed = False
        self._reader = asyncio.create_task(self._read())

    @classmethod
    async def async_start(cls):
        if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
            raise HelperError("unsupported_platform")
        executable = Path(__file__).parent / "bin" / "allo-ha-linux-amd64"
        try:
            await asyncio.to_thread(_prepare_executable, executable)
            process = await asyncio.create_subprocess_exec(
                str(executable),
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.DEVNULL,
                limit=MAX_LINE + 1,
            )
        except OSError:
            raise HelperError("helper_unavailable") from None
        return cls(process)

    def subscribe(self, listener: Callable):
        self._listeners.add(listener)
        return lambda: self._listeners.discard(listener)

    def _emit(self, event):
        for listener in tuple(self._listeners):
            listener(event)

    async def async_request(self, op, **args):
        if self._closed or self._reader.done():
            raise HelperError("helper_stopped")
        self._sequence += 1
        request_id = self._sequence
        payload = json.dumps({"id": request_id, "op": op, **args}).encode()
        if len(payload) > MAX_LINE:
            raise HelperError("invalid_request")
        future = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        try:
            async with asyncio.timeout(
                95 if op in ("start_events", "resume_events") else self.timeout
            ):
                async with self._write_lock:
                    self._process.stdin.write(payload + b"\n")
                    await self._process.stdin.drain()
                return await future
        except TimeoutError:
            await self.async_close()
            raise HelperError("timeout") from None
        except (BrokenPipeError, ConnectionError):
            await self.async_close()
            raise HelperError("helper_stopped") from None
        except asyncio.CancelledError:
            await self.async_close()
            raise
        finally:
            self._pending.pop(request_id, None)

    async def _read(self):
        try:
            while line := await self._process.stdout.readline():
                if len(line.rstrip(b"\n")) > MAX_LINE or not line.endswith(b"\n"):
                    break
                message = json.loads(line)
                if not isinstance(message, dict):
                    break
                if "event" in message:
                    self._emit(message)
                    continue
                request_id = message.get("id")
                if type(request_id) is not int:
                    break
                if future := self._pending.get(request_id):
                    if future.done():
                        continue
                    if isinstance(message.get("result"), dict):
                        future.set_result(message["result"])
                    else:
                        error = message.get("error")
                        code = error.get("code") if isinstance(error, dict) else None
                        future.set_exception(
                            HelperError(
                                code if isinstance(code, str) and code in _CODES else "helper_error"
                            )
                        )
        except (ValueError, UnicodeError):
            pass
        finally:
            for future in self._pending.values():
                if not future.done():
                    future.set_exception(HelperError("helper_stopped"))
            self._emit({"event": "availability", "available": False})
            await self._stop_process()
            self._emit({"event": "helper_stopped"})

    async def _stop_process(self):
        if self._process.returncode is None:
            try:
                self._process.terminate()
            except ProcessLookupError:
                return
            try:
                async with asyncio.timeout(5):
                    await self._process.wait()
            except TimeoutError:
                self._process.kill()
                async with asyncio.timeout(5):
                    await self._process.wait()

    async def async_close(self):
        self._closed = True
        await self._stop_process()
        if self._reader is not asyncio.current_task():
            self._reader.cancel()
            await asyncio.gather(self._reader, return_exceptions=True)
        self._listeners.clear()
