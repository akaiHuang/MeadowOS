"""Exact voice admission uses the native queue and retains uncertain text."""
import ast
import json
from pathlib import Path
from types import SimpleNamespace, MethodType
import tempfile
import unittest
import uuid

SOURCE = Path(__file__).resolve().parents[1] / "meadow_shell.py"
klass = next(node for node in ast.walk(ast.parse(SOURCE.read_text()))
             if isinstance(node, ast.ClassDef) and node.name == "Companion")
methods = [node for node in klass.body if isinstance(node, ast.FunctionDef)
           and node.name in {"submit_text", "save_voice_drafts", "present_voice_draft"}]


class VoiceSubmissionTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name)
        self.calls, self.notices, self.callbacks = [], [], []
        def write(path, value):
            path.write_text(json.dumps(value))
        context = {"uuid": uuid, "state_directory": lambda: self.path, "atomic_json": write}
        exec(compile(ast.Module(body=methods, type_ignores=[]), str(SOURCE), "exec"), context)
        self.subject = SimpleNamespace(voice_unsent=[], sending=False,
            send=SimpleNamespace(get_sensitive=lambda: True),
            error=SimpleNamespace(set_text=lambda text: self.calls.append(("error", text))),
            update_controls=lambda: None,
            follow_bottom=lambda **kwargs: self.calls.append(("follow_bottom", kwargs)),
            draft=lambda: "a different typed draft",
            input=SimpleNamespace(get_buffer=lambda: SimpleNamespace(set_text=lambda text: self.calls.append(("typed", text)))),
            save_draft=lambda: self.calls.append(("saved_typed",)),
            editing=SimpleNamespace(reset=lambda: self.calls.append(("editing_reset",))),
            request=lambda *callbacks: self.callbacks.append(callbacks),
            request_error=lambda error: self.calls.append(("request_error", error)),
            append_system_notice=self.notices.append)
        for name, function in context.items():
            if name in {method.name for method in methods}:
                setattr(self.subject, name, MethodType(function, self.subject))

    def test_exact_recognized_text_uses_native_send_and_preserves_typed_draft(self):
        text = "請記錄：週五 3 點開會。"
        self.assertTrue(self.subject.submit_text(text))
        action, accepted, failed = self.callbacks[0]
        observed = []
        action(SimpleNamespace(send=observed.append))
        self.assertEqual(observed, [text])
        # The durable pre-request draft records uncertainty before any HTTP.
        self.assertEqual(json.loads((self.path / "voice-drafts.json").read_text())["items"][0]["status"], "uncertain")
        accepted({"queued": True})
        self.assertEqual(self.subject.voice_unsent, [])
        self.assertFalse(any(call[0] == "typed" for call in self.calls))
        self.assertNotIn(("editing_reset",), self.calls)
        self.assertIn(("follow_bottom", {"force": True}), self.calls)

    def test_typed_admission_resets_history_even_if_a_new_draft_was_started(self):
        self.assertTrue(self.subject.submit_text("submitted text", preserve_draft=False))
        self.callbacks[0][1]({"queued": True})
        self.assertEqual(self.calls.count(("editing_reset",)), 1)
        self.assertFalse(any(call[0] == "typed" for call in self.calls))

    def test_typed_admission_clears_matching_draft_and_history(self):
        self.subject.draft = lambda: "submitted text"
        self.subject.submit_text("submitted text", preserve_draft=False)
        self.callbacks[0][1]({"queued": True})
        self.assertIn(("typed", ""), self.calls)
        self.assertEqual(self.calls.count(("editing_reset",)), 1)

    def test_failed_typed_admission_preserves_editing_history(self):
        self.subject.submit_text("submitted text", preserve_draft=False)
        self.callbacks[0][2]("connection closed")
        self.assertNotIn(("editing_reset",), self.calls)

    def test_disconnected_admission_retains_copyable_text_without_auto_retry(self):
        self.subject.send.get_sensitive = lambda: False
        self.assertFalse(self.subject.submit_text("未送出的原文"))
        self.assertEqual(self.callbacks, [])
        self.assertEqual(self.subject.voice_unsent[0]["text"], "未送出的原文")
        self.assertEqual(self.subject.voice_unsent[0]["status"], "unsubmitted")
        self.assertIn("未送出的原文", self.notices[0]["text"])

    def test_unknown_receipt_retains_uncertainty_without_rewriting_or_resubmitting(self):
        self.subject.submit_text("保留這句原文")
        self.callbacks[0][2]("connection closed")
        self.assertEqual(len(self.callbacks), 1)
        self.assertEqual(self.subject.voice_unsent[0]["status"], "uncertain")
        self.assertIn("先檢查排隊訊息", self.notices[0]["text"])
        self.assertIn("保留這句原文", self.notices[0]["text"])


if __name__ == "__main__":
    unittest.main()
