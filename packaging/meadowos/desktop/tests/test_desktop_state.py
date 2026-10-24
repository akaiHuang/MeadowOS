import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
import subprocess
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from meadow_state import collect_state, public_state, valid_private_url
from meadow_theme import apply_theme, preference_commands


class DesktopStateTest(unittest.TestCase):
    def test_private_url_accepts_web_only_and_rejects_credentials(self):
        self.assertEqual(valid_private_url("https://desk.example/#pair=secret"), "https://desk.example/#pair=secret")
        self.assertEqual(valid_private_url("http://localhost:18390"), "http://localhost:18390")
        for value in ("javascript:alert(1)", "file:///etc/passwd", "http://public.example", "https://a:b@desk.example", "https://desk.example:wrong", "https://desk.example/\nlaunch"):
            self.assertIsNone(valid_private_url(value))

    def test_active_service_does_not_invent_desktop_or_private_url(self):
        with tempfile.TemporaryDirectory() as temporary, patch("meadow_state.agent_state", return_value="active"):
            state = collect_state(Path(temporary) / "prefix", Path(temporary) / "state")
            self.assertEqual(state["agent_state"], "active")
            self.assertFalse(state["desktop_installed"])
            self.assertFalse(state["desktop_launcher_installed"])
            self.assertIsNone(state["private_url"])

    def test_malformed_metadata_is_visible_and_diagnostic_omits_pairing_url(self):
        with tempfile.TemporaryDirectory() as temporary, patch("meadow_state.agent_state", return_value="inactive"):
            root = Path(temporary)
            (root / "status.json").write_text("not-json")
            self.assertTrue(collect_state(root, root)["metadata_error"])
            (root / "status.json").write_text(json.dumps({"private_url": "https://desk.example/#pair=secret"}))
            diagnostic = public_state(collect_state(root, root))
            self.assertTrue(diagnostic["private_url_configured"])
            self.assertNotIn("secret", json.dumps(diagnostic))

    def test_subsequent_default_application_never_changes_customizations(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "theme.json").write_text('{"theme":"dark","version":1}')
            with patch("meadow_theme.subprocess.run") as run:
                self.assertFalse(apply_theme("dark", root, defaults=True)["changed"])
                run.assert_not_called()

    def test_missing_graphical_session_does_not_mark_defaults_applied(self):
        with tempfile.TemporaryDirectory() as temporary, patch.dict(os.environ, {}, clear=True):
            root = Path(temporary)
            with self.assertRaises(RuntimeError):
                apply_theme("dark", root, defaults=True)
            self.assertFalse((root / "theme.json").exists())

    def test_failed_desktop_query_keeps_wallpaper_pending(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            expected = {(cmd[2], cmd[4]): cmd[-1] for cmd in preference_commands("dark")}
            def result(argv, **_kwargs):
                value = expected.get((argv[2], argv[4]), "") if len(argv) == 5 else ""
                return subprocess.CompletedProcess(argv, 1 if argv[-1] == "-l" else 0, stdout=value, stderr="")
            with patch.dict(os.environ, {"HOME": str(root), "DISPLAY": ":98", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/test", "XDG_DATA_HOME": str(root / "data")}), patch("meadow_theme.shutil.which", return_value="xfconf-query"), patch("meadow_theme.subprocess.run", side_effect=result):
                with self.assertRaisesRegex(RuntimeError, "wallpaper remains pending"):
                    apply_theme("dark", root, defaults=True)
            saved = json.loads((root / "theme.json").read_text())
            self.assertTrue(saved["appearance_applied"])
            self.assertFalse(saved["wallpaper_applied"])

    def test_pending_wallpaper_retry_does_not_reset_user_theme(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "theme.json").write_text(json.dumps({"theme": "dark", "appearance_applied": True, "wallpaper_applied": False}))
            calls = []
            def result(argv, **_kwargs):
                calls.append(argv)
                return subprocess.CompletedProcess(argv, 0, stdout="/backdrop/screen0/monitor0/workspace0/last-image\n" if argv[-1] == "-l" else "", stderr="")
            with patch.dict(os.environ, {"HOME": str(root), "DISPLAY": ":98", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/test", "XDG_DATA_HOME": str(root / "data")}), patch("meadow_theme.shutil.which", return_value="xfconf-query"), patch("meadow_theme.subprocess.run", side_effect=result):
                self.assertTrue(apply_theme("dark", root, defaults=True)["changed"])
            self.assertFalse(any("xsettings" in call or "xfwm4" in call or "thunar" in call for call in calls))
            self.assertTrue(json.loads((root / "theme.json").read_text())["wallpaper_applied"])

    def test_meadow_shell_owns_wallpaper_and_later_defaults_preserve_customizations(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            saved, calls = {}, []
            def result(argv, **_kwargs):
                calls.append(argv)
                self.assertNotIn("xfce4-desktop", argv)
                key = (argv[2], argv[4])
                if "-s" in argv:
                    saved[key] = argv[-1]
                return subprocess.CompletedProcess(argv, 0, stdout=saved.get(key, ""), stderr="")
            env = {"HOME": str(root), "DISPLAY": ":98", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/test",
                   "XDG_DATA_HOME": str(root / "data"), "XDG_CURRENT_DESKTOP": "Meadow:XFCE"}
            with patch.dict(os.environ, env), patch("meadow_theme.shutil.which", return_value="xfconf-query"), patch("meadow_theme.subprocess.run", side_effect=result):
                applied = apply_theme("light", root, defaults=True)
                self.assertTrue(applied["appearance_applied"])
                self.assertEqual(applied["wallpaper_owner"], "shell")
                receipt = json.loads((root / "theme.json").read_text())
                self.assertFalse(receipt["wallpaper_applied"])
                self.assertEqual(receipt["wallpaper_owner"], "shell")
                calls.clear()
                saved[("thunar", "/last-menubar-visible")] = "true"
                self.assertFalse(apply_theme("dark", root, defaults=True)["changed"])
                self.assertEqual(calls, [])
                self.assertEqual(saved[("thunar", "/last-menubar-visible")], "true")

    def test_failed_preference_readback_cannot_record_success(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            env = {"HOME": str(root), "DISPLAY": ":98", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/test",
                   "XDG_DATA_HOME": str(root / "data"), "DESKTOP_SESSION": "meadow"}
            with patch.dict(os.environ, env), patch("meadow_theme.shutil.which", return_value="xfconf-query"), patch("meadow_theme.subprocess.run", return_value=subprocess.CompletedProcess([], 0, stdout="wrong", stderr="")):
                with self.assertRaisesRegex(RuntimeError, "could not confirm"):
                    apply_theme("light", root, defaults=False)
            self.assertFalse((root / "theme.json").exists())


if __name__ == "__main__":
    unittest.main()
