"""Read only lower-window XComposite pixmaps through an isolated helper process."""
from pathlib import Path
import os
import select
import struct
import subprocess
import time

import numpy as np


class BackdropError(RuntimeError):
    pass


class Backdrop:
    def __init__(self, display=None, helper=None):
        self.display = display or os.environ.get("DISPLAY")
        if not self.display:
            raise BackdropError("Native glass requires an explicit X11 display")
        helper = helper or os.environ.get("MEADOW_GLASS_CAPTURE") or str(Path(__file__).with_name("meadow-glass-capture"))
        self.command = [helper, "--display", self.display]
        self.process = self._start()
        self.source_count = 0
        self.failed = False
        self.restart_count = 0

    def _start(self):
        return subprocess.Popen(self.command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, bufsize=0)

    def recover(self):
        """Retry only with an entirely new pipe/helper, at most twice per owner."""
        if not self.failed:
            return True
        if self.restart_count >= 2:
            return False
        self.close()
        self.restart_count += 1
        self.process = self._start()
        self.failed = False
        self.source_count = 0
        return True

    def _abort(self):
        # A partial/late response cannot be reused as a new frame header.
        self.failed = True
        if self.process.poll() is None:
            self.process.terminate()

    def _read(self, count):
        result = bytearray()
        deadline = time.monotonic() + 2
        while len(result) < count:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not select.select([self.process.stdout], [], [], remaining)[0]:
                self._abort()
                raise BackdropError("Native backdrop reader timed out")
            chunk = os.read(self.process.stdout.fileno(), count - len(result))
            if not chunk:
                self._abort()
                raise BackdropError("Native backdrop reader stopped")
            result.extend(chunk)
        return result

    def capture(self, xid, x, y, width, height):
        if self.process.poll() is not None:
            self.failed = True
        if self.failed:
            raise BackdropError("Native backdrop reader is unavailable")
        try:
            self.process.stdin.write(f"{int(xid):x} {int(x)} {int(y)} {int(width)} {int(height)}\n".encode())
            self.process.stdin.flush()
        except (BrokenPipeError, OSError) as error:
            self._abort()
            raise BackdropError("Native backdrop reader stopped") from error
        status, self.source_count, actual_width, actual_height = struct.unpack("=4I", self._read(16))
        if status:
            raise BackdropError("Native target is not mapped below an active compositor")
        if (actual_width, actual_height) != (width, height):
            self._abort()
            raise BackdropError("Native backdrop reader returned invalid frame dimensions")
        return np.frombuffer(self._read(width * height * 4), dtype=np.uint8).reshape(height, width, 4).copy()

    def close(self):
        if not self.process.stdin.closed:
            try:
                self.process.stdin.close()
            except BrokenPipeError:
                pass
        if self.process.poll() is None:
            try:
                self.process.wait(timeout=.4)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=1)
        self.process.stdout.close()

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()
