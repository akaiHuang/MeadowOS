"""Owned socket recovery and actual subprocess cleanup ordering regressions."""
import importlib.util
import os
from pathlib import Path
import select
import socket
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("meadow_container_session", Path(__file__).with_name("container_session.py"))
session = importlib.util.module_from_spec(spec)
spec.loader.exec_module(session)


class DisplayRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="meadow-display-", dir="/tmp")
        self.root = Path(self.temporary.name)
        (self.root / ".X11-unix").mkdir()
        self.socket_path = self.root / ".X11-unix/X121"
        self.lock_path = self.root / ".X121-lock"
        self.proc = self.root / "proc"
        self.proc.mkdir()

    def tearDown(self):
        self.temporary.cleanup()

    def test_stale_owned_display_socket_and_lock_are_removed(self):
        with socket.socket(socket.AF_UNIX) as server:
            server.bind(str(self.socket_path))
        self.lock_path.write_text("999999\n")
        session.remove_stale_display(":121", self.root, self.proc)
        self.assertFalse(self.socket_path.exists())
        self.assertFalse(self.lock_path.exists())

    def test_live_socket_is_never_removed(self):
        with socket.socket(socket.AF_UNIX) as server:
            server.bind(str(self.socket_path))
            server.listen(1)
            self.lock_path.write_text("999999\n")
            with self.assertRaisesRegex(RuntimeError, "Another X server"):
                session.remove_stale_display(":121", self.root, self.proc)
            self.assertTrue(self.socket_path.exists())
            self.assertTrue(self.lock_path.exists())

    def test_live_xserver_lock_is_preserved_before_socket_is_ready(self):
        (self.proc / "123").mkdir()
        (self.proc / "123/cmdline").write_bytes(b"/usr/bin/Xvfb\0:121\0")
        self.lock_path.write_text("123\n")
        with self.assertRaisesRegex(RuntimeError, "live X server"):
            session.remove_stale_display(":121", self.root, self.proc)
        self.assertTrue(self.lock_path.exists())

    def test_symlink_resource_is_rejected_without_touching_target(self):
        target = self.root / "preserved"
        target.write_text("keep\n")
        self.lock_path.symlink_to(target)
        with self.assertRaisesRegex(RuntimeError, "unsafe resources"):
            session.remove_stale_display(":121", self.root, self.proc)
        self.assertEqual(target.read_text(), "keep\n")

    def test_invalid_display_never_selects_another_path(self):
        with self.assertRaises(RuntimeError):
            session.remove_stale_display(":../other", self.root, self.proc)


class OwnedProcessOrderingTests(unittest.TestCase):
    def test_private_bus_survives_until_native_lab_cleanup_finishes(self):
        code = r'''
import pathlib,signal,sys,time
name,path=sys.argv[1],pathlib.Path(sys.argv[2])
def stop(_signal,_frame):
    if name=="lab":
        time.sleep(.15)
        marker="lab_cleanup_done"
    else:
        marker="bus_stopped_after_lab" if path.exists() and "lab_cleanup_done" in path.read_text() else "bus_stopped_too_early"
    with path.open("a") as stream:stream.write(marker+"\n")
    raise SystemExit(0)
signal.signal(signal.SIGTERM,stop)
print("ready",flush=True)
while True:time.sleep(.1)
'''
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / "order.txt"
            children = []
            try:
                for name in ("bus", "lab"):
                    child = subprocess.Popen([sys.executable, "-c", code, name, str(receipt)],
                                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
                    children.append((name, child))
                    self.assertTrue(select.select([child.stdout], [], [], 5)[0])
                    self.assertEqual(child.stdout.readline().strip(), b"ready")
                session.stop_owned_processes(children)
                self.assertEqual(receipt.read_text().splitlines(), ["lab_cleanup_done", "bus_stopped_after_lab"])
                self.assertTrue(all(child.returncode == 0 for _, child in children))
            finally:
                for _, child in children:
                    if child.poll() is None:
                        child.kill()
                        child.wait()
                    child.stdout.close()
                    child.stderr.close()


if __name__ == "__main__":
    unittest.main()
