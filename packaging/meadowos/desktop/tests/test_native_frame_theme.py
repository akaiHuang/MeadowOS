"""Native theme asset/installation regressions, not a GUI interaction pass."""
import ast
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import meadow_theme
from native_frame_assets import BUTTONS, PALETTES, ROOT, STATES, TOGGLED, theme_files


def read_xpm(path):
    if not path.read_bytes().startswith(b"/* XPM */\n"):
        raise ValueError("missing standard XPM loader signature")
    rows = [ast.literal_eval(line.rstrip(",")) for line in path.read_text().splitlines() if line.startswith('"')]
    width, height, colors, cpp = map(int, rows[0].split())
    palette = {row[:cpp]: row[cpp:].strip().split()[1] for row in rows[1:1 + colors]}
    raster = rows[1 + colors:]
    if len(palette) != colors or len(raster) != height or any(len(row) != width * cpp for row in raster):
        raise ValueError("invalid XPM dimensions")
    pixels = [[palette[row[x:x + cpp]] for x in range(0, len(row), cpp)] for row in raster]
    return width, height, pixels


class NativeFrameThemeTest(unittest.TestCase):
    def test_both_payloads_have_complete_original_reproducible_assets(self):
        for name in PALETTES:
            with self.subTest(theme=name):
                root = ROOT / "themes" / name
                self.assertTrue(meadow_theme.theme_complete(root))
                actual = {path.name for path in (root / "xfwm4").glob("*.xpm")}
                self.assertEqual(actual, meadow_theme.native_frame_files())
                self.assertEqual(len(actual), 60)
                for relative, expected in theme_files(name).items():
                    self.assertEqual((root / "xfwm4" / relative).read_text(), expected)
                receipt = json.loads((root / "xfwm4/assets.json").read_text())
                self.assertEqual(receipt["external_images"], [])
                for relative, expected in receipt["files"].items():
                    self.assertEqual(hashlib.sha256((root / "xfwm4" / relative).read_bytes()).hexdigest(), expected)

    def test_all_xpm_states_parse_and_preserve_frame_and_button_geometry(self):
        for name in PALETTES:
            root = ROOT / "themes" / name / "xfwm4"
            for path in root.glob("*.xpm"):
                width, height, _pixels = read_xpm(path)
                self.assertGreater(width, 0)
                self.assertGreater(height, 0)
                if path.name.startswith(("title-", "top-left-", "top-right-")):
                    self.assertEqual(height, 24)
            for button in BUTTONS:
                variants = [button, *( [button + "-toggled"] if button in TOGGLED else [])]
                for variant in variants:
                    for state in STATES:
                        width, height, pixels = read_xpm(root / f"{variant}-{state}.xpm")
                        self.assertEqual((width, height), (20, 24))
                        # Xfwm4 uses ShapeBounding from these pixels as the hit area.
                        self.assertTrue(all(color.startswith("#") and len(color) == 7 for row in pixels for color in row))
            for button in ("close", "hide", "maximize"):
                _width, _height, pixels = read_xpm(root / f"{button}-active.xpm")
                baseline = PALETTES[name]["background"]
                dot = [(x, y) for y, row in enumerate(pixels) for x, color in enumerate(row)
                       if 0 < y < 23 and color != baseline]
                self.assertEqual(max(x for x, _ in dot) - min(x for x, _ in dot) + 1, 12)
                self.assertEqual(max(y for _, y in dot) - min(y for _, y in dot) + 1, 12)

    def test_plan_selects_matching_native_theme_and_left_controls(self):
        for color in ("dark", "light"):
            commands = meadow_theme.preference_commands(color)
            by_property = {tuple(command[2:5]): command[-1] for command in commands}
            self.assertEqual(by_property[("xfwm4", "-p", "/general/theme")], "Meadow-" + color.title())
            self.assertEqual(by_property[("xfwm4", "-p", "/general/button_layout")], "CHM|")
            self.assertEqual(by_property[("xfwm4", "-p", "/general/title_font")], "Noto Sans 9")
            self.assertEqual(by_property[("xsettings", "-p", "/Gtk/DecorationLayout")], "close,minimize,maximize:")
            self.assertEqual(by_property[("xsettings", "-p", "/Gtk/DialogsUseHeader")], "true")
            self.assertEqual(by_property[("thunar", "-p", "/misc-use-csd")], "true")
            self.assertEqual(by_property[("thunar", "-p", "/last-menubar-visible")], "false")
            native = [command for command in commands if command[4] in {
                "/Gtk/DialogsUseHeader", "/misc-use-csd", "/last-menubar-visible"}]
            self.assertTrue(all(command[7] == "bool" for command in native))

    def test_fresh_browsers_use_native_frames_and_existing_profile_is_preserved(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)
            self.assertEqual(meadow_theme.seed_browser_preferences(root)["google-chrome"],"native_frame_default")
            preferences=root/"google-chrome/Default/Preferences"
            self.assertFalse(json.loads(preferences.read_text())["browser"]["custom_chrome_frame"])
            preferences.write_text('{"browser":{"custom_chrome_frame":true},"test":"existing user"}')
            before=preferences.read_bytes()
            self.assertEqual(meadow_theme.seed_browser_preferences(root)["google-chrome"],"preserved")
            self.assertEqual(preferences.read_bytes(),before)

    def test_gtk_only_missing_state_and_symlinked_assets_are_incomplete(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "Meadow-Light"
            shutil.copytree(ROOT / "themes/Meadow-Light", root)
            self.assertTrue(meadow_theme.theme_complete(root))
            dot = root / "gtk-3.0/assets/window-close.svg"
            dot_source = dot.read_bytes()
            dot.unlink()
            self.assertFalse(meadow_theme.theme_complete(root))
            dot.write_bytes(dot_source)
            self.assertTrue(meadow_theme.theme_complete(root))
            state = root / "xfwm4/maximize-toggled-pressed.xpm"
            original = state.read_bytes()
            state.unlink()
            self.assertFalse(meadow_theme.theme_complete(root))
            elsewhere = Path(temporary) / "elsewhere.xpm"
            elsewhere.write_bytes(original)
            state.symlink_to(elsewhere)
            self.assertFalse(meadow_theme.theme_complete(root))
            shutil.rmtree(root / "xfwm4")
            self.assertFalse(meadow_theme.theme_complete(root))

    def test_incomplete_existing_user_theme_is_preserved(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            theme = root / "themes/Meadow-Dark"
            theme.mkdir(parents=True)
            sentinel = theme / "custom.txt"
            sentinel.write_text("user content")
            with patch.dict(os.environ, {"HOME": str(root), "XDG_DATA_HOME": str(root)}), patch("meadow_theme.shutil.copytree") as copy:
                with self.assertRaisesRegex(RuntimeError, "existing user files were preserved"):
                    meadow_theme.ensure_theme_available("Meadow-Dark")
            copy.assert_not_called()
            self.assertEqual(sentinel.read_text(), "user content")

    def test_incomplete_legacy_theme_cannot_shadow_complete_xdg_theme(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            legacy = root / ".themes/Meadow-Light"
            (legacy / "xfwm4").mkdir(parents=True)
            sentinel = legacy / "xfwm4/themerc"
            sentinel.write_text("user content")
            shutil.copytree(ROOT / "themes/Meadow-Light", root / "data/themes/Meadow-Light")
            with patch.dict(os.environ, {"HOME": str(root), "XDG_DATA_HOME": str(root / "data")}):
                with self.assertRaisesRegex(RuntimeError, "existing user files were preserved"):
                    meadow_theme.ensure_theme_available("Meadow-Light")
                with self.assertRaisesRegex(RuntimeError, "existing user files were preserved"):
                    meadow_theme.existing_user_theme("Meadow-Light")
            self.assertEqual(sentinel.read_text(), "user content")

    def test_partial_user_copy_does_not_mark_theme_installed(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            original_check = meadow_theme.theme_complete

            def check(path):
                return False if path == Path("/usr/share/themes/Meadow-Dark") else original_check(path)

            def partial_copy(_source, target):
                target.mkdir()
                (target / "index.theme").write_text("incomplete fixture")

            with patch.dict(os.environ, {"HOME": str(root), "XDG_DATA_HOME": str(root)}), \
                 patch("meadow_theme.theme_complete", side_effect=check), \
                 patch("meadow_theme.shutil.copytree", side_effect=partial_copy):
                with self.assertRaisesRegex(RuntimeError, "installation is incomplete"):
                    meadow_theme.ensure_theme_available("Meadow-Dark")
            self.assertFalse((root / "theme.json").exists())


if __name__ == "__main__":
    unittest.main()
