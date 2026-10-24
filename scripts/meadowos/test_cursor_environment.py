"""Relocated cursor discovery and explicit session environment contracts."""
from pathlib import Path
import tempfile
import unittest

from test_foundation import os_session


class CursorEnvironmentTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.prefix = self.root / "opt/meadow"
        self.icons = self.root / "usr/share/icons"

    def tearDown(self):
        self.temporary.cleanup()

    def resources(self):
        share = self.prefix / "share/meadow-os"
        for name in ("window_manager/libmeadow-hover-chrome.so", "desktop/glass/meadow-glass-capture", "desktop/glass/bridge.py"):
            path = share / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.touch()

    def cursor_theme(self):
        path = self.icons / "Meadow-Cursors/index.theme"
        path.parent.mkdir(parents=True)
        path.write_text("[Icon Theme]\nName=Meadow-Cursors\n")

    def test_missing_or_disabled_optional_resources_do_not_add_cursor_environment(self):
        self.cursor_theme()
        for overrides in ({}, {"MEADOW_WINDOW_EFFECTS": "0"}):
            with self.subTest(overrides=overrides):
                env = {"HOME": "/owned/home", **overrides}
                before = dict(env)
                self.assertFalse(os_session.configure_window_resources(self.prefix, env)["configured"])
                self.assertEqual(env, before)
        self.resources()
        env = {"HOME": "/owned/home", "MEADOW_WINDOW_EFFECTS": "0"}
        before = dict(env)
        self.assertFalse(os_session.configure_window_resources(self.prefix, env)["configured"])
        self.assertEqual(env, before)

    def test_relocated_resources_add_search_path_only_with_installed_cursor_theme(self):
        self.resources()
        env = {"HOME": "/owned/home"}
        self.assertTrue(os_session.configure_window_resources(self.prefix, env)["configured"])
        self.assertNotIn("XCURSOR_PATH", env)
        self.cursor_theme()
        self.assertTrue(os_session.configure_window_resources(self.prefix, env)["configured"])
        self.assertEqual(env["XCURSOR_THEME"], "Meadow-Cursors")
        self.assertEqual(env["XCURSOR_PATH"].split(":"),
                         ["/owned/home/.icons", str(self.icons), "/usr/local/share/icons", "/usr/share/icons", "/usr/share/pixmaps"])
        again = dict(env)
        os_session.configure_window_resources(self.prefix, env)
        self.assertEqual(env, again)

    def test_explicit_cursor_path_and_theme_are_retained(self):
        self.resources()
        self.cursor_theme()
        env = {"HOME": "/owned/home", "XCURSOR_PATH": "/owned/cursors:/other/cursors", "XCURSOR_THEME": "UserCursor"}
        self.assertTrue(os_session.configure_window_resources(self.prefix, env)["configured"])
        self.assertEqual(env["XCURSOR_PATH"], "/owned/cursors:/other/cursors")
        self.assertEqual(env["XCURSOR_THEME"], "UserCursor")


if __name__ == "__main__":
    unittest.main()
