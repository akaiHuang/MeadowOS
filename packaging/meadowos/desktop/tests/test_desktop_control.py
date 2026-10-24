import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock


PATH = Path(__file__).resolve().parents[1] / "desktop_control.py"
SPEC = importlib.util.spec_from_file_location("desktop_control", PATH)
control = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(control)


class DesktopProtocolTest(unittest.TestCase):
    def test_missing_display_does_not_discover_other_displays(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(control.DesktopError) as result:
                control.Desktop()
        self.assertEqual(result.exception.code, "desktop_unavailable")
        self.assertIn("no other desktop was searched", result.exception.message)

    def test_window_ids_reject_invalid_or_boolean_values(self):
        self.assertEqual(control.window_number("0x1200007"), 0x1200007)
        for value in [True, False, None, 1.2, "--display=:98", "-1", 0, 0x100000000]:
            with self.subTest(value=value), self.assertRaises(control.DesktopError):
                control.window_number(value)

    def test_text_and_integer_fields_are_bounded(self):
        self.assertEqual(control.text_arg({"text": "中文"}, "text"), "中文")
        for value in ["a\x00b", 20, "x" * 8]:
            with self.subTest(value=value), self.assertRaises(control.DesktopError):
                control.text_arg({"text": value}, "text", maximum=7)
        with self.assertRaises(control.DesktopError):
            control.integer(True, "button", 1, 5)

    def test_protocol_returns_structured_failure_without_importing_toolkits(self):
        request = mock.Mock()
        request.buffer = io.BytesIO(b'{"action":"snapshot"}')
        output = io.StringIO()
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(sys, "stdin", request), contextlib.redirect_stdout(output):
            exit_code = control.main()
        value = json.loads(output.getvalue())
        self.assertEqual(exit_code, 1)
        self.assertFalse(value["ok"])
        self.assertEqual(value["error"]["code"], "desktop_unavailable")
        self.assertEqual(value["display"], "")

    def test_accessibility_node_must_belong_to_the_selected_window(self):
        desktop = object.__new__(control.Desktop)
        with self.assertRaises(control.DesktopError) as result:
            desktop.resolve_node({"window_id": "0x1200007", "node_id": "0x1300007/123/0/1/@1234567890abcdef12345678"})
        self.assertEqual(result.exception.code, "stale_node")

    def test_node_fingerprint_changes_when_a_control_at_the_same_path_changes(self):
        original = control.node_fingerprint("Save", "push button", [{"name": "click"}], "save-button")
        for name, role, actions, accessible_id in [
            ("Delete", "push button", [{"name": "click"}], "save-button"),
            ("Save", "entry", [{"name": "click"}], "save-button"),
            ("Save", "push button", [{"name": "activate"}], "save-button"),
            ("Save", "push button", [{"name": "click"}], "delete-button"),
        ]:
            self.assertNotEqual(original, control.node_fingerprint(name, role, actions, accessible_id))


if __name__ == "__main__":
    unittest.main()
