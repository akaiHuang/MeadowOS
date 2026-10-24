"""Failure receipts must not allow one native region's pixels into another."""
from pathlib import Path
import tempfile
import unittest

from glass.backdrop import Backdrop, BackdropError


class ReaderProtocolTests(unittest.TestCase):
    def reader(self, body):
        directory = tempfile.TemporaryDirectory(prefix="meadow-glass-reader-test-")
        self.addCleanup(directory.cleanup)
        helper = Path(directory.name) / "helper"
        helper.write_text("#!/usr/bin/python3\nimport sys, struct\nsys.stdin.readline()\n" + body)
        helper.chmod(0o700)
        reader = Backdrop(display=":owned-test", helper=str(helper))
        self.addCleanup(reader.close)
        return reader

    def test_invalid_dimensions_permanently_reject_following_bytes(self):
        reader = self.reader("sys.stdout.buffer.write(struct.pack('=4I', 0, 1, 3, 2) + bytes(24))\nsys.stdout.buffer.flush()\n")
        with self.assertRaisesRegex(BackdropError, "dimensions"):
            reader.capture(1, 0, 0, 2, 2)
        self.assertTrue(reader.failed)
        with self.assertRaisesRegex(BackdropError, "unavailable"):
            reader.capture(1, 10, 10, 2, 2)

    def test_partial_frame_cannot_become_next_response(self):
        reader = self.reader("sys.stdout.buffer.write(struct.pack('=4I', 0, 1, 2, 2) + bytes(8))\nsys.stdout.buffer.flush()\n")
        with self.assertRaisesRegex(BackdropError, "stopped"):
            reader.capture(1, 0, 0, 2, 2)
        self.assertTrue(reader.failed)

    def test_recovery_uses_new_helper_and_clean_frame_boundary(self):
        reader = self.reader("from pathlib import Path\nflag = Path(__file__).with_suffix('.seen')\n"
                             "if not flag.exists():\n flag.touch()\n payload = bytes(8)\n"
                             "else:\n payload = bytes([42]) * 16\n"
                             "sys.stdout.buffer.write(struct.pack('=4I', 0, 1, 2, 2) + payload)\n"
                             "sys.stdout.buffer.flush()\n")
        old_pid = reader.process.pid
        with self.assertRaises(BackdropError):
            reader.capture(1, 0, 0, 2, 2)
        self.assertTrue(reader.recover())
        self.assertNotEqual(reader.process.pid, old_pid)
        pixels = reader.capture(1, 10, 10, 2, 2)
        self.assertTrue((pixels == 42).all())
        self.assertEqual(reader.restart_count, 1)

    def test_failed_helpers_have_a_finite_restart_budget(self):
        reader = self.reader("sys.stdout.buffer.write(struct.pack('=4I', 0, 1, 3, 2) + bytes(24))\nsys.stdout.buffer.flush()\n")
        for attempt in range(3):
            with self.assertRaises(BackdropError):
                reader.capture(1, 0, 0, 2, 2)
            self.assertEqual(reader.recover(), attempt < 2)
        self.assertEqual(reader.restart_count, 2)


if __name__ == "__main__":
    unittest.main()
