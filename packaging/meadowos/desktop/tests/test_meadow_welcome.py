"""Welcome theme/status regressions without starting GTK or changing a profile."""
import ast
import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import meadow_welcome as welcome


class WelcomeThemeTest(unittest.TestCase):
    def test_live_native_theme_wins_over_stale_saved_theme(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "theme.json").write_text('{"theme":"dark"}')
            with patch.object(welcome, "state_root", return_value=root):
                self.assertEqual(welcome.startup_theme(None, "Meadow-Light"), "light")
            self.assertEqual(json.loads((root / "theme.json").read_text()), {"theme": "dark"})

    def test_explicit_preview_does_not_read_or_change_saved_preferences(self):
        with patch.object(welcome, "state_root") as state:
            self.assertEqual(welcome.startup_theme("dark", "Meadow-Light"), "dark")
            self.assertEqual(welcome.startup_theme("light", "Meadow-Dark"), "light")
        state.assert_not_called()

    def test_saved_fallback_handles_missing_and_malformed_state(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with patch.object(welcome, "state_root", return_value=root):
                self.assertEqual(welcome.startup_theme(None, "Adwaita"), "dark")
                for data in ('{"theme":"light"}', '[]', 'not-json'):
                    (root / "theme.json").write_text(data)
                    expected = "light" if data.startswith('{') else "dark"
                    self.assertEqual(welcome.startup_theme(None, "Adwaita"), expected)

    def methods(self):
        # Exercise actual window methods with process-local GtkSettings doubles;
        # native rendering/input remains the shared chrome's separate QA gate.
        source = Path(welcome.__file__)
        tree = ast.parse(source.read_text())
        native = next(node for node in ast.walk(tree) if isinstance(node, ast.ClassDef) and node.name == "Welcome")
        selected = [node for node in native.body if isinstance(node, ast.FunctionDef)
                    and node.name in {"apply_theme", "choose_theme"}]
        settings, style = Mock(), Mock()
        context = {"Gtk": SimpleNamespace(Settings=SimpleNamespace(get_default=lambda: settings),
                                          StyleContext=style, STYLE_PROVIDER_PRIORITY_APPLICATION=600),
                   "Gdk": SimpleNamespace(Screen=SimpleNamespace(get_default=lambda: "screen")),
                   "PAYLOAD": welcome.PAYLOAD, "subprocess": subprocess}
        exec(compile(ast.Module(body=selected, type_ignores=[]), str(source), "exec"), context)
        return context, settings

    def test_local_theme_uses_the_meadow_native_theme_for_controls_and_css(self):
        methods, settings = self.methods()
        for selected, name, dark in (("light", "Meadow-Light", False), ("dark", "Meadow-Dark", True)):
            view = SimpleNamespace(theme=selected, css=Mock())
            methods["apply_theme"](view)
            settings.set_property.assert_any_call("gtk-theme-name", name)
            settings.set_property.assert_any_call("gtk-application-prefer-dark-theme", dark)
            view.css.load_from_path.assert_called_once_with(str(welcome.PAYLOAD / "themes" / name / "gtk-3.0/gtk.css"))

    def test_preview_theme_buttons_never_write_the_desktop_profile(self):
        methods, _ = self.methods()
        view = SimpleNamespace(preview_theme=True, theme="dark", apply_theme=Mock(), message=Mock())
        with patch.object(subprocess, "run") as run:
            methods["choose_theme"](view, "light")
        self.assertEqual(view.theme, "light")
        view.apply_theme.assert_called_once()
        run.assert_not_called()

    def test_normal_theme_choice_retains_the_explicit_desktop_action(self):
        methods, _ = self.methods()
        view = SimpleNamespace(preview_theme=False, theme="dark", apply_theme=Mock(), message=Mock())
        with patch.object(subprocess, "run", return_value=subprocess.CompletedProcess([], 0)) as run:
            methods["choose_theme"](view, "light")
        self.assertEqual(run.call_args.args[0], [str(welcome.PAYLOAD / "bin/meadow-theme"), "--set", "light"])
        self.assertNotIn("shell", run.call_args.kwargs)

    def test_status_json_remains_display_free_and_omits_pairing_url(self):
        output = io.StringIO()
        actual = {"agent_state": "inactive", "private_url": "https://desk.example/#pair=private"}
        with patch.object(sys, "argv", ["meadow-welcome", "--status-json"]), \
                patch.dict(sys.modules, {"gi": None, "glass.gtk_surface": None}), \
                patch.object(welcome, "collect_state", return_value=actual), \
                patch.object(subprocess, "Popen") as launch, contextlib.redirect_stdout(output):
            self.assertEqual(welcome.main(), 0)
        self.assertEqual(json.loads(output.getvalue()), {"agent_state": "inactive", "private_url_configured": True})
        self.assertNotIn("pair=private", output.getvalue())
        launch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
