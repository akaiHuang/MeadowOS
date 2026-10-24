"""Read-only data and real-action contracts; this is not native GUI acceptance."""
import contextlib
import copy
import ast
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import meadow_settings as settings


class SettingsDataTest(unittest.TestCase):
    def test_single_instance_identity_is_shared_by_launchers_but_not_other_desktops(self):
        self.assertEqual(settings.settings_application_id(":110"), settings.settings_application_id(":110.0"))
        self.assertNotEqual(settings.settings_application_id(":110"), settings.settings_application_id(":111"))
        self.assertNotEqual(settings.settings_application_id(":110"), settings.settings_application_id("remote:110"))
    def test_virtual_backend_matches_actual_local_server_not_output_name(self):
        with tempfile.TemporaryDirectory() as temporary:
            proc = Path(temporary)
            server = proc / "40"
            server.mkdir()
            (server / "comm").write_text("Xvfb\n")
            (server / "cmdline").write_bytes(b"\0".join([b"/usr/bin/Xvfb", b":110", b"-screen", b"0", b"1920x1080x24", b""]))
            self.assertTrue(settings.virtual_display_server(":110.0", proc))
            self.assertTrue(settings.virtual_display_server("unix:110", proc))
            self.assertFalse(settings.virtual_display_server(":11", proc))
            self.assertFalse(settings.virtual_display_server("remote:110", proc))
            (server / "comm").write_text("Xorg\n")
            self.assertFalse(settings.virtual_display_server(":110", proc))

    def test_virtual_display_recovery_reenables_advertised_mode_and_verifies_readback(self):
        before = settings.parse_xrandr("""Screen 0: minimum 1 x 1, current 1536 x 864, maximum 1920 x 1080
screen connected (normal)
    Transform: 1 0 0
               0 1 0
               0 0 1
  1920x1080 (0x3a) 0.000MHz
""") | {"error": None}
        after = copy.deepcopy(before)
        after["size"] = after["outputs"][0]["size"] = [1920, 1080]
        after["outputs"][0]["position"] = [0, 0]
        with patch.object(settings, "virtual_display_server", return_value=True), patch.object(settings, "display_snapshot", side_effect=[before, after]), patch("meadow_settings.shutil.which", return_value="/usr/bin/xrandr"), patch("meadow_settings.subprocess.run", return_value=subprocess.CompletedProcess([], 0)) as run:
            self.assertTrue(settings.restore_virtual_display()["confirmed"])
            self.assertEqual(run.call_args.args[0], ["/usr/bin/xrandr", "--output", "screen", "--mode", "1920x1080", "--pos", "0x0", "--fb", "1920x1080"])
            self.assertNotIn("shell", run.call_args.kwargs)
        # A successful command cannot mask an output that stayed disabled.
        with patch.object(settings, "virtual_display_server", return_value=True), patch.object(settings, "display_snapshot", side_effect=[before, before, before]), patch("meadow_settings.shutil.which", return_value="/usr/bin/xrandr"), patch("meadow_settings.subprocess.run", return_value=subprocess.CompletedProcess([], 0)):
            self.assertFalse(settings.restore_virtual_display()["confirmed"])

    def test_scale_can_change_repeatedly_while_synthetic_output_is_detached(self):
        original = {"size": [1920, 1080], "maximum": [1920, 1080], "error": None,
                    "outputs": [{"name": "screen", "size": [1920, 1080], "position": [0, 0], "scale": [1, 1], "modes": ["1920x1080"]}]}
        current = copy.deepcopy(original)
        commands = []
        def run(argv, **_kwargs):
            commands.append(argv)
            current["size"] = [int(part) for part in argv[-1].split("x")]
            current["outputs"][0]["size"] = None if "--off" in argv else [1920, 1080]
            return subprocess.CompletedProcess(argv, 0)
        with patch.object(settings, "virtual_display_server", return_value=True), patch.object(settings, "display_snapshot", side_effect=lambda: copy.deepcopy(current)), patch("meadow_settings.shutil.which", return_value="xrandr"), patch("meadow_settings.subprocess.run", side_effect=run):
            for scale in (1.5, 1, 1.25, 1.75, 2, 1.5, 1):
                self.assertTrue(settings.apply_virtual_scale(scale)["confirmed"])
                self.assertEqual(settings.virtual_scale_value(current), scale)
            self.assertEqual(commands[0], ["xrandr", "--output", "screen", "--off", "--fb", "1280x720"])
            self.assertEqual(commands[-1], ["xrandr", "--output", "screen", "--mode", "1920x1080", "--pos", "0x0", "--fb", "1920x1080"])
            self.assertTrue(all("--scale" not in command for command in commands))

    def test_failed_scale_rolls_back_previous_logical_frame(self):
        before = {"size": [1536, 864], "maximum": [1920, 1080], "error": None,
                  "outputs": [{"name": "screen", "size": None, "modes": ["1920x1080"]}]}
        broken = before | {"size": [1000, 700]}
        with patch.object(settings, "virtual_display_server", return_value=True), patch.object(settings, "display_snapshot", side_effect=[before, broken, before]), patch("meadow_settings.shutil.which", return_value="xrandr"), patch("meadow_settings.subprocess.run", side_effect=[subprocess.CompletedProcess([], 1), subprocess.CompletedProcess([], 0)]) as run:
            result = settings.apply_virtual_scale(1.5)
            self.assertFalse(result["confirmed"])
            self.assertTrue(result["rollback_confirmed"])
            self.assertEqual(run.call_args.args[0][-3:], ["--off", "--fb", "1536x864"])
        with patch("meadow_settings.subprocess.run") as run:
            for invalid in (True, 0, 3, "1.5", float("nan")):
                self.assertFalse(settings.apply_virtual_scale(invalid)["confirmed"])
            run.assert_not_called()

    def test_virtual_display_cannot_launch_raw_scale_panel(self):
        with patch.object(settings, "virtual_display_server", return_value=True), patch("meadow_settings.subprocess.Popen") as run:
            with self.assertRaises(ValueError):
                settings.launch_tool("display", 123)
            run.assert_not_called()

    def test_virtual_recovery_refuses_unknown_backend_multiple_outputs_or_unadvertised_mode(self):
        single = {"size": [1536, 864], "maximum": [1920, 1080], "error": None,
                  "outputs": [{"name": "screen", "size": None, "modes": []}]}
        with patch.object(settings, "virtual_display_server", return_value=False), patch("meadow_settings.subprocess.run") as run:
            self.assertFalse(settings.restore_virtual_display()["confirmed"])
            run.assert_not_called()
        for state in (single, single | {"outputs": single["outputs"] * 2}):
            with patch.object(settings, "virtual_display_server", return_value=True), patch.object(settings, "display_snapshot", return_value=state), patch("meadow_settings.subprocess.run") as run:
                self.assertFalse(settings.restore_virtual_display()["confirmed"])
                run.assert_not_called()

    def test_display_does_not_confuse_available_modes_with_active_geometry(self):
        text = """Screen 0: minimum 1 x 1, current 1920 x 1080, maximum 16384 x 16384
screen connected primary 1920x1080+0+0 (normal)
    Transform: 1.000000 0.000000 0.000000
               0.000000 1.000000 0.000000
               0.000000 0.000000 1.000000
   1920x1080 60.00*+
HDMI-1 connected (normal)
   3840x2160 60.00
"""
        data = settings.parse_xrandr(text)
        self.assertEqual(data["size"], [1920, 1080])
        self.assertEqual(data["outputs"][0]["scale"], [1, 1])
        self.assertIsNone(data["outputs"][1]["size"])
        self.assertIsNone(data["outputs"][1]["scale"])

    def test_rotation_is_not_reported_as_a_guessed_scaling_percentage(self):
        data = settings.parse_xrandr("""screen connected 1080x1920+0+0
    Transform: 0 -1 1080
               1 0 0
               0 0 1
""")
        self.assertIsNone(data["outputs"][0]["scale"])
        self.assertEqual(data["outputs"][0]["transform"], [0, -1, 1080, 1, 0, 0, 0, 0, 1])

    def test_missing_display_tool_has_no_sample_resolution(self):
        with patch("meadow_settings.shutil.which", return_value=None):
            data = settings.display_snapshot()
        self.assertIsNone(data["size"])
        self.assertTrue(data["error"])

    def test_stale_theme_receipt_does_not_override_native_state(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "theme.json").write_text(json.dumps({"theme": "dark", "appearance_applied": True}))
            values = {"/Net/ThemeName": "Meadow-Light", "/general/theme": "Meadow-Dark", "/Net/IconThemeName": "Meadow"}
            with patch("meadow_settings.state_root", return_value=root), patch("meadow_settings.query_preference", side_effect=lambda _channel, key: values[key]):
                data = settings.appearance_snapshot()
            self.assertIsNone(data["theme"])
            self.assertEqual(data["native_theme"], "Meadow-Light")
            self.assertEqual(data["receipt"]["theme"], "dark")

    def test_theme_failure_is_preserved_even_when_native_writes_succeeded(self):
        state = {"theme": "light", "receipt": {"theme": "light", "appearance_applied": True}}
        failed = subprocess.CompletedProcess([], 1, stdout="", stderr="wallpaper pending")
        with patch("meadow_settings.subprocess.run", return_value=failed), patch("meadow_settings.appearance_snapshot", return_value=state):
            result = settings.apply_appearance("light", Path("/missing"))
        self.assertTrue(result["confirmed"])
        self.assertEqual(result["error"], "wallpaper pending")
        failed.stderr = ""
        with patch("meadow_settings.subprocess.run", return_value=failed), patch("meadow_settings.appearance_snapshot", return_value=state):
            result = settings.apply_appearance("light", Path("/missing"))
        self.assertEqual(result["returncode"], 1)
        self.assertTrue(result["error"])
        state["receipt"]["theme"] = "dark"
        with patch("meadow_settings.subprocess.run", return_value=failed), patch("meadow_settings.appearance_snapshot", return_value=state):
            self.assertFalse(settings.apply_appearance("light", Path("/missing"))["confirmed"])

    def test_only_available_constant_tools_can_launch_without_a_shell(self):
        with patch("meadow_settings.shutil.which", side_effect=lambda name: "/tool path/display" if name == "xfce4-display-settings" else None), patch("meadow_settings.subprocess.Popen") as launch:
            self.assertEqual([tool["id"] for tool in settings.available_tools()], ["display"])
            with self.assertRaises(ValueError):
                settings.launch_tool("display; echo injected")
            launch.assert_not_called()
            settings.launch_tool("display")
            self.assertEqual(launch.call_args.args[0], ["/tool path/display"])
            self.assertNotIn("shell", launch.call_args.kwargs)
            self.assertTrue(launch.call_args.kwargs["start_new_session"])

    def test_system_data_handles_unavailable_proc_and_environment_memory_limit(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.assertEqual(settings.memory_snapshot(root / "missing", root)["total"], None)
            (root / "meminfo").write_text("MemTotal: 8192 kB\nMemAvailable: 4096 kB\n")
            (root / "memory.max").write_text("1048576")
            (root / "memory.current").write_text("262144")
            data = settings.memory_snapshot(root / "meminfo", root)
            self.assertEqual(data, {"total": 1048576, "available": 786432, "scope": "environment"})
            (root / "os-release").write_text('PRETTY_NAME="Test OS $(no execution)"\n')
            self.assertEqual(settings.read_os_release(root / "os-release")["PRETTY_NAME"], "Test OS $(no execution)")

    def test_status_json_does_not_import_gtk_or_start_an_app(self):
        output = io.StringIO()
        with patch.object(sys, "argv", ["meadow-settings", "--status-json"]), patch.dict(sys.modules, {"gi": None}), patch("meadow_settings.snapshot", return_value={"actual": True}), patch("meadow_settings.subprocess.Popen") as launch, contextlib.redirect_stdout(output):
            self.assertEqual(settings.main(), 0)
        self.assertEqual(json.loads(output.getvalue()), {"actual": True})
        launch.assert_not_called()


class DisplayConfirmationTest(unittest.TestCase):
    """Execute production confirmation callbacks with inert widget doubles."""
    def setUp(self):
        tree = ast.parse(Path(settings.__file__).read_text())
        native = next(node for node in ast.walk(tree) if isinstance(node, ast.ClassDef) and node.name == "Settings")
        names = {"choose_display_scale", "display_restored", "keep_display_scale", "revert_display_scale", "display_countdown_tick", "closed", "fit_virtual_display"}
        methods = [node for node in native.body if isinstance(node, ast.FunctionDef) and node.name in names]
        module = ast.Module(body=[ast.ClassDef(name="Subject", bases=[], keywords=[], body=methods, decorator_list=[])], type_ignores=[])
        self.glib, self.gtk, self.gdk, self.apply = Mock(), Mock(), Mock(), Mock()
        self.snapshot = Mock()
        namespace = {"GLib": self.glib, "Gtk": self.gtk, "Gdk": self.gdk, "apply_virtual_scale": self.apply,
                     "display_snapshot": self.snapshot, "virtual_scale_value": settings.virtual_scale_value,
                     "VIRTUAL_SCALES": settings.VIRTUAL_SCALES}
        exec(compile(ast.fix_missing_locations(module), str(settings.__file__), "exec"), namespace)
        self.subject = namespace["Subject"]()
        for name in ("display_restore", "display_apply", "feedback", "display_countdown", "display_confirmation", "refresh", "stop_panel", "run_display_scale", "get_size", "get_position", "resize", "move"):
            setattr(self.subject, name, Mock())
        self.subject.alive = self.subject.virtual_display = True
        self.subject.display_previous_scale = None
        self.subject.display_inflight_previous = None
        self.subject.display_revert_timer = None
        self.subject.display_seconds = 0
        self.subject.display_busy = False

    def test_timeout_restores_previous_multiplier_and_leaves_controls_usable(self):
        subject = self.subject
        subject.display_restored({"confirmed": True, "scale": 1.5}, 1.25)
        subject.display_confirmation.show_all.assert_called_once()
        self.assertEqual([call.args for call in subject.display_confirmation.set_no_show_all.call_args_list], [(False,), (True,)])
        for _ in range(14):
            self.assertTrue(subject.display_countdown_tick())
            subject.run_display_scale.assert_not_called()
        self.assertFalse(subject.display_countdown_tick())
        subject.run_display_scale.assert_called_once_with(1.25, confirm=False)
        self.assertIsNone(subject.display_previous_scale)

    def test_keep_preserves_scale_and_cancels_only_its_timer(self):
        subject = self.subject
        subject.display_restored({"confirmed": True, "scale": 1.5}, 1.0)
        timer = subject.display_revert_timer
        subject.keep_display_scale()
        self.glib.source_remove.assert_called_once_with(timer)
        self.assertIsNone(subject.display_previous_scale)
        subject.run_display_scale.assert_not_called()
        self.apply.assert_not_called()

    def test_closing_pending_or_inflight_change_restores_previous_scale(self):
        for pending, inflight in ((1.25, None), (None, 1.5)):
            self.subject.alive = True
            self.subject.display_previous_scale = pending
            self.subject.display_inflight_previous = inflight
            self.apply.reset_mock()
            self.subject.closed()
            self.assertFalse(self.subject.alive)
            self.apply.assert_called_once_with(pending or inflight)

    def test_scaled_settings_window_stays_inside_logical_desktop(self):
        screen = self.gdk.Screen.get_default.return_value
        screen.get_width.return_value, screen.get_height.return_value = 960, 540
        self.subject.get_size.return_value = (1020, 740)
        self.subject.get_position.return_value = (450, 300)
        self.subject.fit_virtual_display()
        self.subject.resize.assert_called_once_with(912, 460)
        self.subject.move.assert_called_once_with(24, 40)

    def test_plus_minus_apply_directly_and_repeated_steps_keep_original_revert_target(self):
        self.snapshot.return_value = {"size": [1536, 864], "maximum": [1920, 1080]}
        self.subject.display_previous_scale = 1.0
        self.subject.choose_display_scale(.25)
        self.subject.run_display_scale.assert_called_once_with(1.5, confirm=True, previous=1.0)
        self.subject.run_display_scale.reset_mock()
        self.subject.choose_display_scale(-.25)
        self.subject.run_display_scale.assert_called_once_with(1.0, confirm=True, previous=None)
        self.subject.run_display_scale.reset_mock()
        self.snapshot.return_value = {"size": [1920, 1080], "maximum": [1920, 1080]}
        self.subject.choose_display_scale(-.25)
        self.subject.run_display_scale.assert_not_called()
        self.subject.display_busy = True
        self.subject.choose_display_scale(.25)
        self.subject.run_display_scale.assert_not_called()


if __name__ == "__main__":
    unittest.main()
