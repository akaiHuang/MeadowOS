"""Isolated original-byte ingress, native staging receipts and HTTP admission."""
import hashlib
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
from urllib.parse import quote
from urllib.request import Request, urlopen
import uuid

SHELL = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SHELL))
import native_attachments as ingress

spec = importlib.util.spec_from_file_location("attachment_voice_service", SHELL.parent / "voiceos" / "voice_service.py")
service = importlib.util.module_from_spec(spec)
spec.loader.exec_module(service)


class AttachmentIngressTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.environment = patch.dict(os.environ, {"MEADOW_ATTACHMENT_SPOOL": str(self.root)})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.calls = []
        def stage(event, payload):
            self.calls.append((event, payload))
            return {"id": "private-cache-item", "path": "/must/not/reach/browser"}
        self.native = ingress.NativeAttachmentTransport(self.root, stage)
        self.native.publish(active=True, rect={"x": 100, "y": 200, "width": 800, "height": 600}, session_id="chat-one")
        self.app = ingress.AttachmentIngress(self.root)
        self.epoch = self.native.state["epoch"]
        self.payload = b"original-byte-fixture"

    def lease(self, client="browser-a", **extra):
        return self.app.lease(dict({"clientId": client, "epoch": self.epoch, "point": {"x": 110, "y": 210}}, **extra))

    def meta(self, **extra):
        return dict({"eventId": uuid.uuid4().hex, "name": "參考圖.png", "mime": "image/png", "size": len(self.payload),
                     "sha256": hashlib.sha256(self.payload).hexdigest(), "epoch": self.epoch, "collectionId": "dropped-group"}, **extra)

    def test_original_bytes_native_receipt_and_private_state(self):
        self.assertTrue(self.lease()["owned"])
        meta = self.meta()
        reply = self.app.upload(self.payload, meta, "browser-a")
        self.assertEqual(reply["status"], "accepted")
        self.assertEqual(self.calls, [])
        self.native._consume_one()
        reply = self.app.receipt(meta["eventId"], "browser-a")
        self.assertEqual(reply["status"], "staged")
        self.assertEqual(reply["attachmentId"], "private-cache-item")
        self.assertEqual(self.calls[0][1], self.payload)
        self.assertEqual(self.calls[0][0]["sessionId"], "chat-one")
        self.assertNotIn("path", reply)
        self.assertNotIn("sessionId", self.app.state())
        self.assertNotIn("clientId", reply)
        self.assertEqual(list((self.app.path / "objects").glob("*.blob")), [])
        self.assertEqual(list((self.app.path / "events").iterdir()), [])
        for item in self.app.path.rglob("*"):
            self.assertFalse(item.stat().st_mode & 0o077, str(item))

    def test_replay_is_idempotent_and_cannot_change_bytes(self):
        self.lease()
        meta = self.meta()
        first = self.app.upload(self.payload, meta, "browser-a")
        second = self.app.upload(self.payload, meta, "browser-a")
        self.assertEqual(first, second)
        self.assertEqual(len(list((self.app.path / "events").glob("*.json"))), 1)
        with self.assertRaises(ingress.AttachmentIngressError) as error:
            self.app.upload(self.payload, dict(meta, name="other.png"), "browser-a")
        self.assertEqual(error.exception.code, "receipt_conflict")

    def test_separate_owner_drop_rectangle_and_stale_epoch(self):
        with self.assertRaises(ingress.AttachmentIngressError):
            self.lease(point={"x": 10, "y": 10})
        self.assertTrue(self.lease()["owned"])
        self.assertFalse(self.lease("browser-b")["owned"])
        with self.assertRaises(ingress.AttachmentIngressError) as error:
            self.app.upload(self.payload, self.meta(), "browser-b")
        self.assertEqual(error.exception.code, "attachment_not_owned")
        self.native.publish(session_id="chat-two")
        with self.assertRaises(ingress.AttachmentIngressError) as error:
            self.app.upload(self.payload, self.meta(), "browser-a")
        self.assertEqual(error.exception.code, "stale_epoch")

    def test_native_chat_switch_rejects_mailbox_and_retains_original(self):
        self.lease()
        meta = self.meta()
        self.app.upload(self.payload, meta, "browser-a")
        self.native.publish(session_id="chat-two")
        self.native._consume_one()
        self.assertEqual(self.calls, [])
        receipt = self.app.receipt(meta["eventId"], "browser-a")
        self.assertEqual(receipt["status"], "failed")
        self.assertEqual(receipt["error"], "stale_epoch")
        self.assertEqual((self.app.path / "objects" / (meta["eventId"] + ".blob")).read_bytes(), self.payload)

    def test_native_cache_failure_is_failed_receipt_and_retains_original(self):
        self.lease()
        meta = self.meta()
        self.app.upload(self.payload, meta, "browser-a")
        self.native.on_attachment = lambda *_: (_ for _ in ()).throw(ValueError("private local path"))
        self.native._consume_one()
        receipt = self.app.receipt(meta["eventId"], "browser-a")
        self.assertEqual(receipt["status"], "failed")
        self.assertNotIn("private local path", json.dumps(receipt))
        self.assertTrue((self.app.path / "objects" / (meta["eventId"] + ".blob")).exists())

    def test_restart_recovers_claim_to_explicit_failure_and_retains_original(self):
        self.lease()
        meta = self.meta()
        self.app.upload(self.payload, meta, "browser-a")
        event = next((self.app.path / "events").glob("*.json"))
        event.rename(event.with_name(".consume-" + uuid.uuid4().hex))
        restarted = ingress.NativeAttachmentTransport(self.root, self.native.on_attachment)
        restarted.publish(active=True, rect={"x": 100, "y": 200, "width": 800, "height": 600}, session_id="chat-one")
        restarted._consume_one()
        self.assertEqual(self.calls, [])
        self.assertEqual(self.app.receipt(meta["eventId"], "browser-a")["status"], "failed")
        self.assertTrue((self.app.path / "objects" / (meta["eventId"] + ".blob")).exists())

    def test_durable_receipts_and_metadata_consume_private_disk_budget(self):
        self.lease()
        with patch.object(ingress, "MAX_TOTAL_BYTES", len(self.payload) + ingress.MAX_JSON_BYTES):
            with self.assertRaises(ingress.AttachmentIngressError) as error:
                self.app.upload(self.payload, self.meta(), "browser-a")
        self.assertEqual(error.exception.code, "spool_full")

    def test_missing_original_produces_explicit_failed_receipt(self):
        self.lease()
        meta = self.meta()
        self.app.upload(self.payload, meta, "browser-a")
        (self.app.path / "objects" / (meta["eventId"] + ".blob")).unlink()
        self.native._consume_one()
        self.assertEqual(self.calls, [])
        self.assertEqual(self.app.receipt(meta["eventId"], "browser-a")["status"], "failed")

    def test_tampered_blob_and_private_symlink_cannot_reach_callback(self):
        self.lease()
        meta = self.meta()
        self.app.upload(self.payload, meta, "browser-a")
        blob = self.app.path / "objects" / (meta["eventId"] + ".blob")
        blob.write_bytes(b"changed")
        self.native._consume_one()
        self.assertEqual(self.calls, [])
        self.assertEqual(self.app.receipt(meta["eventId"], "browser-a")["error"], "byte_mismatch")
        path = self.app.path / "state.json"
        path.chmod(0o644)
        with self.assertRaises(ingress.AttachmentIngressError):
            self.app.state()
        path.unlink()
        path.symlink_to(self.root / "unrelated")
        with self.assertRaises(ingress.AttachmentIngressError):
            self.app.state()

    def test_per_file_byte_bounds_hash_and_basename_are_authoritative(self):
        self.lease()
        cases = [{"size": ingress.MAX_FILE_BYTES + 1}, {"size": True}, {"size": 0},
                 {"name": "/Users/host/image.png"}, {"name": "../image.png"}, {"name": "image\x00.png"},
                 {"mime": "text/plain"}, {"sha256": "0" * 64}, {"eventId": "../../escape"}]
        for change in cases:
            with self.subTest(change=change), self.assertRaises(ingress.AttachmentIngressError):
                self.app.upload(self.payload, self.meta(**change), "browser-a")
        self.assertEqual(list((self.app.path / "objects").iterdir()), [])

    def test_collection_has_no_count_cap_and_only_disk_byte_guard(self):
        self.lease()
        for _ in range(40):
            meta = self.meta()
            self.app.upload(self.payload, meta, "browser-a")
            self.native._consume_one()
        self.assertEqual(len(self.calls), 40)
        with patch.object(ingress, "MAX_TOTAL_BYTES", len(self.payload) - 1):
            with self.assertRaises(ingress.AttachmentIngressError) as error:
                self.app.upload(self.payload, self.meta(), "browser-a")
        self.assertEqual(error.exception.code, "spool_full")

    def test_expired_heartbeat_lease_and_private_receipt(self):
        self.lease()
        meta = self.meta()
        self.app.upload(self.payload, meta, "browser-a")
        with self.assertRaises(ingress.AttachmentIngressError) as error:
            self.app.receipt(meta["eventId"], "browser-b")
        self.assertEqual(error.exception.code, "receipt_not_owned")
        with patch.object(ingress.time, "monotonic", return_value=self.app.owner[2] + .01):
            with self.assertRaises(ingress.AttachmentIngressError):
                self.app.upload(self.payload, self.meta(), "browser-a")
        raw = ingress._json(self.app.path / "state.json")
        raw["updatedAt"] = time.time() - 20
        ingress._write(self.app.path / "state.json", raw)
        self.assertFalse(self.app.state()["active"])
        self.assertIsNone(self.app.state()["rect"])

    def test_http_actual_bytes_to_native_receipt_and_origin_admission(self):
        app = service.VoiceApplication(service.VoiceSpool(self.root), SimpleNamespace(capabilities=lambda: {}))
        server = service.VoiceHTTPServer(("127.0.0.1", 0), app, ["http://127.0.0.1:60511"])
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = f"http://127.0.0.1:{server.server_port}"
        headers = {"Origin": "http://127.0.0.1:60511", "X-Meadow-Attachments": "1", "X-Meadow-Attachments-Client": "browser-a"}
        def call(path, data=None, extra=None, method=None):
            request = Request(url + path, data=data, headers=dict(headers, **(extra or {})), method=method)
            with urlopen(request) as response:
                return response.status, json.load(response) if response.status != 204 else None
        status, native = call("/attachments/state")
        self.assertEqual(status, 200)
        self.assertTrue(native["active"])
        status, owned = call("/attachments/lease", json.dumps({"clientId": "browser-a", "epoch": self.epoch,
                                    "point": {"x": 110, "y": 210}}).encode(), {"Content-Type": "application/json"})
        self.assertTrue(owned["owned"])
        meta = self.meta()
        status, receipt = call("/attachments/upload", self.payload, {"Content-Type": "application/octet-stream",
                                      "X-Meadow-Attachments-Meta": quote(json.dumps(meta))})
        self.assertEqual(status, 202)
        self.assertEqual(receipt["status"], "accepted")
        self.native._consume_one()
        status, receipt = call("/attachments/receipt?eventId=" + meta["eventId"])
        self.assertEqual(receipt["status"], "staged")
        self.assertEqual(self.calls[-1][1], self.payload)
        for extra in ({"Origin": "https://example.test"}, {"X-Meadow-Attachments": "0"}):
            with self.subTest(extra=extra), self.assertRaises(HTTPError) as error:
                call("/attachments/state", extra=extra)
            self.assertEqual(error.exception.code, 403)
        status, _ = call("/attachments/upload", extra={"Access-Control-Request-Method": "POST",
                         "Access-Control-Request-Headers": "content-type,x-meadow-attachments,x-meadow-attachments-client,x-meadow-attachments-meta"}, method="OPTIONS")
        self.assertEqual(status, 204)


if __name__ == "__main__":
    unittest.main()
