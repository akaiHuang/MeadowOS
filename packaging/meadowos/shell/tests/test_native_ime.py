"""IME transport acceptance: lifecycle, ownership, privacy, and HTTP admission."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.error import HTTPError
from urllib.request import Request, urlopen

SHELL = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SHELL))
import native_ime as ime

spec = importlib.util.spec_from_file_location("ime_voice_service", SHELL.parent / "voiceos" / "voice_service.py")
service = importlib.util.module_from_spec(spec)
spec.loader.exec_module(service)


class NativeIMETest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.calls = []
        self.env = patch.dict(os.environ, {"MEADOW_IME_SPOOL": str(self.root)})
        self.env.start()
        self.addCleanup(self.env.stop)
        self.native = ime.NativeIMETransport(self.root, self.calls.append)
        self.app = ime.IMEApplication(self.root)
        self.native.publish(active=True, caret={"x": 100, "y": 300, "width": 1, "height": 18})
        self.epoch = self.native.state["epoch"]

    def own(self, client="browser-a"):
        return self.app.lease({"clientId": client, "epoch": self.epoch})

    def event(self, sequence=0, text="ㄋㄧˇ", active=True):
        return {"epoch": self.epoch, "sequence": sequence, "text": text,
                "cursor": len(text), "active": active}

    def test_preedit_is_ephemeral_and_does_not_expose_draft_or_preedit(self):
        self.own()
        self.app.preedit(self.event(), "browser-a")
        self.native.poll()
        self.assertEqual(self.calls[-1]["text"], "ㄋㄧˇ")
        self.assertEqual(set(self.app.state()), {"version", "epoch", "active", "caret", "updatedAt"})
        self.assertFalse((self.root / "ime/preedit.json").exists())
        self.assertEqual(list((self.root / "ime").glob(".consume-*")), [])
        self.app.preedit(self.event(1, "", False), "browser-a")
        self.native.poll()
        self.assertEqual(self.calls[-1]["active"], False)
        self.assertEqual(self.calls[-1]["text"], "")

    def test_other_browser_cannot_overwrite_or_clear_owner(self):
        self.assertTrue(self.own()["owned"])
        self.assertFalse(self.own("browser-b")["owned"])
        with self.assertRaises(ime.IMEError) as error:
            self.app.preedit(self.event(), "browser-b")
        self.assertEqual(error.exception.code, "ime_not_owned")
        self.app.lease({"clientId": "browser-b", "epoch": self.epoch, "release": True})
        self.app.preedit(self.event(), "browser-a")
        self.native.poll()
        self.assertEqual(self.calls[-1]["text"], "ㄋㄧˇ")

    def test_release_handoff_rejects_delayed_old_owner(self):
        self.own()
        self.app.preedit(self.event(), "browser-a")
        self.app.lease({"clientId": "browser-a", "epoch": self.epoch, "release": True})
        self.assertTrue(self.own("browser-b")["owned"])
        self.app.preedit(self.event(0, "新的"), "browser-b")
        with self.assertRaises(ime.IMEError):
            self.app.preedit(self.event(999, "舊的"), "browser-a")
        self.native.poll()
        self.assertEqual(self.calls[-1]["text"], "新的")

    def test_old_epoch_mailbox_is_discarded_after_native_focus_change(self):
        self.own()
        self.app.preedit(self.event(), "browser-a")
        self.native.publish(active=False)
        self.native.publish(active=True, caret={"x": 120, "y": 300, "width": 1, "height": 18})
        self.native.poll()
        self.assertEqual(self.calls, [])
        with self.assertRaises(ime.IMEError) as error:
            self.app.preedit(self.event(1), "browser-a")
        self.assertEqual(error.exception.code, "stale_epoch")

    def test_committed_buffer_invalidation_rejects_delayed_composition(self):
        self.own()
        self.app.preedit(self.event(), "browser-a")
        self.native.poll()
        self.native.invalidate()
        self.assertFalse(self.calls[-1]["active"])
        with self.assertRaises(ime.IMEError):
            self.app.preedit(self.event(1), "browser-a")

    def test_stale_out_of_order_update_cannot_undo_latest_text(self):
        self.own()
        self.app.preedit(self.event(2, "完整"), "browser-a")
        for sequence in (0, 1, 2):
            with self.assertRaises(ime.IMEError) as error:
                self.app.preedit(self.event(sequence, "舊"), "browser-a")
            self.assertEqual(error.exception.code, "stale_sequence")
        self.native.poll()
        self.assertEqual(self.calls[-1]["text"], "完整")

    def test_lost_browser_disconnect_expires_visible_text(self):
        self.own()
        self.app.preedit(self.event(), "browser-a")
        self.native.poll()
        with patch.object(ime.time, "monotonic", return_value=self.native.deadline + .01):
            self.native.poll()
        self.assertFalse(self.calls[-1]["active"])

    def test_bounded_preedit_rejects_bool_sequence_and_bad_unicode_cursor(self):
        self.own()
        for change in ({"sequence": True}, {"sequence": -1}, {"sequence": 2**54},
                       {"text": "字" * 257}, {"text": "😀", "cursor": 2},
                       {"text": "\x00", "cursor": 1}, {"cursor": True}, {"active": False}):
            with self.subTest(change=change), self.assertRaises(ime.IMEError):
                self.app.preedit(dict(self.event(), **change), "browser-a")

    def test_fresh_composition_after_old_lease_expiry(self):
        self.own()
        with patch.object(ime.time, "monotonic", return_value=self.app.owner[2] + .01):
            self.assertTrue(self.own("browser-b")["owned"])
            self.app.preedit(self.event(0, "新"), "browser-b")
        self.native.poll()
        self.assertEqual(self.calls[-1]["text"], "新")

    def test_stale_heartbeat_is_inactive_and_private_extra_fields_filtered(self):
        path = self.root / "ime/state.json"
        raw = json.loads(path.read_text())
        raw["draft"] = "must stay private"
        ime._write(path, raw)
        self.assertNotIn("draft", self.app.state())
        raw["updatedAt"] = time.time() - 20
        ime._write(path, raw)
        self.assertFalse(self.app.state()["active"])
        self.assertIsNone(self.app.state()["caret"])

    def test_state_symlink_and_untrusted_permissions_are_rejected(self):
        path = self.root / "ime/state.json"
        path.chmod(0o644)
        with self.assertRaises(ime.IMEError):
            self.app.state()
        path.unlink()
        path.symlink_to(self.root / "unrelated")
        with self.assertRaises(ime.IMEError):
            self.app.state()

    def test_http_uses_separate_ime_header_and_independent_lease(self):
        app = service.VoiceApplication(service.VoiceSpool(self.root), SimpleNamespace(capabilities=lambda: {}))
        server = service.VoiceHTTPServer(("127.0.0.1", 0), app, ["http://127.0.0.1:60511"])
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = f"http://127.0.0.1:{server.server_port}"
        headers = {"Origin": "http://127.0.0.1:60511", "X-Meadow-IME": "1"}
        with urlopen(Request(url + "/ime/state", headers=headers)) as response:
            self.assertTrue(json.load(response)["active"])
        request = Request(url + "/ime/lease", data=json.dumps({"clientId": "browser-a", "epoch": self.epoch}).encode(),
                          headers=dict(headers, **{"Content-Type": "application/json"}))
        with urlopen(request) as response:
            self.assertTrue(json.load(response)["owned"])
        request = Request(url + "/ime/preedit", data=json.dumps(self.event()).encode(), headers=dict(headers,
                          **{"Content-Type": "application/json", "X-Meadow-IME-Client": "browser-a"}))
        with urlopen(request) as response:
            self.assertEqual(response.status, 202)
        self.native.poll()
        self.assertEqual(self.calls[-1]["text"], "ㄋㄧˇ")
        for wrong in ({"Origin": "https://example.test", "X-Meadow-IME": "1"},
                      {"Origin": "http://127.0.0.1:60511", "X-Meadow-Voice": "1"}):
            with self.assertRaises(HTTPError) as error:
                urlopen(Request(url + "/ime/state", headers=wrong))
            self.assertEqual(error.exception.code, 403)


if __name__ == "__main__":
    unittest.main()
