"""Bridge framing and owned-child cleanup; synthetic pixels, no X server/GUI.

These tests exercise the real bridge subprocess and Backdrop lifecycle. The
small helper is explicitly a protocol fixture, not native visual acceptance.
"""
from pathlib import Path
import json
import os
import select
import signal
import struct
import subprocess
import sys
import tempfile
import time
import unittest


BRIDGE = Path(__file__).resolve().parents[1] / "bridge.py"


@unittest.skipUnless(os.name == "posix", "Popup worker uses POSIX process signals")
class PopupBridgeProtocolTests(unittest.TestCase):
    def start_bridge(self, *, stalled_capture=False):
        owned = tempfile.TemporaryDirectory(prefix="meadow-popup-protocol-")
        self.addCleanup(owned.cleanup)
        root = Path(owned.name)
        helper = root / "synthetic-protocol-helper"
        helper.write_text(
            f"#!{sys.executable}\n"
            "# Synthetic protocol bytes only. This helper never connects to X11.\n"
            "from pathlib import Path\n"
            "import os,signal,struct,sys,time\n"
            "root=Path(__file__).parent\n"
            "def stop(_signal,_frame): raise SystemExit(0)\n"
            "signal.signal(signal.SIGTERM,stop)\n"
            "(root/'helper.pid').write_text(str(os.getpid()))\n"
            "try:\n"
            " for line in sys.stdin:\n"
            "  xid,x,y,w,h=line.split(); w,h=int(w),int(h)\n"
            "  (root/'capture-started').write_text('ready')\n"
            + ("  time.sleep(30)\n" if stalled_capture else "")
            + "  sys.stdout.buffer.write(struct.pack('=4I',0,7,w,h)+bytes((30,80,160,255))*(w*h))\n"
            "  sys.stdout.buffer.flush()\n"
            "finally:\n"
            " (root/'helper-exited').write_text('clean')\n"
        )
        helper.chmod(0o700)
        env = os.environ.copy()
        env.pop("DISPLAY", None)
        env.pop("GTK_MODULES", None)
        process = subprocess.Popen(
            [sys.executable, "-B", str(BRIDGE), "--display", ":owned-protocol-only",
             "--helper", str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, env=env, bufsize=0)
        self.addCleanup(self.stop_owned, process)
        self.wait_for(root / "helper.pid", process)
        return process, root

    @staticmethod
    def stop_owned(process):
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None and not stream.closed:
                stream.close()

    def wait_for(self, path, process):
        deadline = time.monotonic() + 3
        while not path.exists() and time.monotonic() < deadline:
            if process.poll() is not None:
                self.fail(f"Bridge exited before fixture ready: {process.stderr.read()!r}")
            time.sleep(.01)
        self.assertTrue(path.exists(), f"Owned fixture marker missing: {path.name}")

    def read_exact(self, process, count):
        data = bytearray()
        deadline = time.monotonic() + 3
        while len(data) < count:
            timeout = deadline - time.monotonic()
            self.assertGreater(timeout, 0, "Bridge response timed out")
            self.assertTrue(select.select([process.stdout], [], [], timeout)[0])
            chunk = os.read(process.stdout.fileno(), count - len(data))
            self.assertTrue(chunk, "Bridge response ended mid-frame")
            data.extend(chunk)
        return bytes(data)

    def assert_helper_reaped(self, root):
        self.assertEqual((root / "helper-exited").read_text(), "clean")
        helper_pid = int((root / "helper.pid").read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(helper_pid, 0)

    def test_error_header_cannot_shift_following_popup_frame(self):
        process, root = self.start_bridge()
        process.stdin.write(b"malformed request\n1 0 0 48 40 8 popup\n")
        process.stdin.flush()
        self.assertEqual(struct.unpack("=4I", self.read_exact(process, 16)), (1, 0, 0, 0))
        self.assertEqual(struct.unpack("=4I", self.read_exact(process, 16)), (0, 7, 48, 40))
        pixels = self.read_exact(process, 48 * 40 * 4)
        self.assertEqual(len(pixels), 48 * 40 * 4)
        self.assertGreater(pixels[(20 * 48 + 24) * 4 + 3], 0)
        process.stdin.close()
        self.assertEqual(process.wait(timeout=3), 0)
        self.assertEqual(process.stdout.read(), b"")
        self.assertEqual(process.stderr.read(), b"")
        self.assert_helper_reaped(root)

    def test_stdin_eof_reaps_idle_capture_helper(self):
        process, root = self.start_bridge()
        process.stdin.close()
        self.assertEqual(process.wait(timeout=3), 0)
        self.assert_helper_reaped(root)

    def test_sigterm_while_waiting_for_request_reaps_helper(self):
        process, root = self.start_bridge()
        process.send_signal(signal.SIGTERM)
        self.assertEqual(process.wait(timeout=3), 0)
        self.assert_helper_reaped(root)

    def test_sigterm_during_capture_reaps_stalled_helper(self):
        process, root = self.start_bridge(stalled_capture=True)
        process.stdin.write(b"1 0 0 48 40 8 popup-dark\n")
        process.stdin.flush()
        self.wait_for(root / "capture-started", process)
        started = time.monotonic()
        process.send_signal(signal.SIGTERM)
        self.assertEqual(process.wait(timeout=3), 0)
        self.assertLess(time.monotonic() - started, 2)
        self.assert_helper_reaped(root)


if __name__ == "__main__":
    unittest.main()
