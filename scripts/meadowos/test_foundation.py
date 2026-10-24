"""Guard and session-wiring regressions. Fixtures are not native/VM acceptance."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import struct
import subprocess
import shutil
import tempfile
import unittest
import sys
import signal
from types import SimpleNamespace
from unittest.mock import patch

import image
import build

runtime_spec = importlib.util.spec_from_file_location("meadow_runtime", image.PACKAGING / "runtime.py")
runtime = importlib.util.module_from_spec(runtime_spec)
runtime_spec.loader.exec_module(runtime)
# The installed session modules are siblings in share/meadow-os/bin.
session_spec = importlib.util.spec_from_file_location("meadow_os_session", image.PACKAGING / "os_session.py")
os_session = importlib.util.module_from_spec(session_spec)
with patch.dict(sys.modules, {"runtime": runtime}):
    session_spec.loader.exec_module(os_session)


class FoundationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()

    def tearDown(self):
        self.temp.cleanup()

    def isolated_session_bus(self):
        # Supervisor fixtures own fake process objects and do not run a real
        # session bus. Linux runners may provide this optional activation
        # utility even when macOS does not; keep that host discovery out of
        # lifecycle tests while preserving every other command lookup.
        host_which = runtime.shutil.which
        return patch.object(runtime.shutil, "which", side_effect=lambda command, *args, **kwargs:
                            None if command == "dbus-update-activation-environment"
                            else host_which(command, *args, **kwargs))

    def elf(self, arch="arm64"):
        path = self.root / "fixture-elf"
        value = bytearray(64)
        value[:6] = b"\x7fELF\x02\x01"
        struct.pack_into("<H", value, 18, image.MACHINES[arch])
        path.write_bytes(value)
        path.chmod(0o755)
        return path

    def test_wrong_target_rejected_without_creating_payload(self):
        (self.root / "etc").mkdir()
        for os_id, version in [("debian", "13"), ("ubuntu", "24.04")]:
            (self.root / "etc/os-release").write_text(f'ID={os_id}\nVERSION_ID="{version}"\n')
            with self.assertRaisesRegex(ValueError, "Ubuntu 26.04"):
                image.target_os(self.root)
        self.assertFalse((self.root / "opt").exists())

    def test_cpu_mismatch_and_non_elf_fail(self):
        path = self.elf()
        image.validate_elf(path, "arm64")
        with self.assertRaisesRegex(ValueError, "architecture"):
            image.validate_elf(path, "amd64")
        path.write_text("#!/bin/sh\nexit 0\n")
        with self.assertRaisesRegex(ValueError, "ELF"):
            image.validate_elf(path, "arm64")

    def test_missing_binary_does_not_create_render_output(self):
        output = self.root / "new-bundle"
        args = argparse.Namespace(build_version="test1", output=output, arch="arm64",
                                  desktop_binary=self.root / "missing", agent_binary=self.root / "missing2")
        with self.assertRaisesRegex(ValueError, "missing regular"):
            image.render(args)
        self.assertFalse(output.exists())

    def test_live_root_aliases_canonicalize(self):
        self.assertEqual(image.normalize_root(Path("//")), Path("/"))
        self.assertEqual(image.normalize_root(self.root / ".." / self.root.name), self.root)
        # Path aliases must reach the same live-root gate, never a staging branch.
        self.assertEqual(image.normalize_root(Path("/") / "usr" / ".."), Path("/"))

    def test_planted_install_parent_is_rejected(self):
        outside = self.root / "elsewhere"
        outside.mkdir()
        (self.root / "opt").symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlink"):
            image.safe_target(self.root, "opt/meadow/bin/meadow-desktop")
        self.assertEqual(list(outside.iterdir()), [])

    def test_manifest_path_escape_is_rejected(self):
        bundle = self.root / "bundle"
        bundle.mkdir()
        (bundle / "manifest.json").write_text(json.dumps({
            "product": "meadow-os-foundation", "development_only": True, "arch": "arm64",
            "files": {"../../escaped": {"sha256": "bad"}},
        }))
        with self.assertRaisesRegex(ValueError, "manifest path"):
            image.verify_bundle(bundle)

    def test_identity_is_private_stable_and_does_not_create_model_credentials(self):
        with patch.dict(os.environ, {"HOME": str(self.root)}, clear=True):
            data, state, _ = runtime.initialize_state()
            first = (data / "device.json").read_bytes()
            runtime.initialize_state()
            self.assertEqual(first, (data / "device.json").read_bytes())
            self.assertEqual((data / "device.json").stat().st_mode & 0o777, 0o600)
            status = json.loads((state / "status.json").read_text())
            self.assertIsNone(status["private_url"])
            self.assertEqual(status["runtime_acceptance"], "not_verified")
            self.assertFalse((data / "agent/model-connections-v1.json").exists())
            self.assertFalse((data / "agent/run/agent-api-token").exists())

    def test_standard_places_seed_real_folders_once_and_preserve_user_bookmarks(self):
        binaries = self.root / "bin"
        binaries.mkdir()
        (binaries / "xdg-user-dir").touch()
        home = self.root / "home"
        work = home / "MeadowWork"
        documents = home / "My Documents"
        work.mkdir(parents=True)
        documents.mkdir()
        env = {"HOME": str(home)}
        def query(argv, **kwargs):
            return SimpleNamespace(returncode=0, stdout=str(documents if len(argv) > 1 else home) + "\n")
        with patch.object(os_session.subprocess, "run", side_effect=query):
            os_session.setup_user_places(str(binaries / "xdg-user-dirs-update"), env)
            bookmarks = home / ".config/gtk-3.0/bookmarks"
            self.assertIn(documents.as_uri(), bookmarks.read_text())
            self.assertIn(work.as_uri(), bookmarks.read_text())
            bookmarks.write_text("file:///custom/user-place My own place\n")
            os_session.setup_user_places(str(binaries / "xdg-user-dirs-update"), env)
            self.assertEqual(bookmarks.read_text(), "file:///custom/user-place My own place\n")

    def test_standard_places_never_follow_an_existing_bookmark_symlink(self):
        binaries = self.root / "bin"
        binaries.mkdir()
        (binaries / "xdg-user-dir").touch()
        config = self.root / "config/gtk-3.0"
        config.mkdir(parents=True)
        destination = self.root / "existing"
        destination.write_text("preserved")
        (config / "bookmarks").symlink_to(destination)
        with patch.object(os_session.subprocess, "run") as run:
            os_session.setup_user_places(str(binaries / "xdg-user-dirs-update"),
                                        {"HOME": str(self.root), "XDG_CONFIG_HOME": str(config.parent)})
        run.assert_called_once()
        self.assertEqual(destination.read_text(), "preserved")

    def test_session_uses_one_profile_and_clears_foreign_authority(self):
        with patch.dict(os.environ, {"HOME": str(self.root), "MEADOW_AGENT_API_KEY": "fixture-only",
                                    "MEADOW_AGENT_HARNESS": "legacy", "MEADOW_AGENTD_MANAGED": "foreign",
                                    "MEADOW_CONVERSATION_PROFILE": "inherited-foreign-profile"}, clear=True):
            env = runtime.runtime_environment()
            profile = self.root / ".local/share/meadow-os/agent"
            self.assertEqual(env["MEADOW_HOME"], str(profile))
            self.assertEqual(env["MEADOW_HOME"], env["MEADOW_DESKTOP_STATE_HOME"])
            self.assertEqual(env["MEADOW_AGENT_SOCKET"], str(profile / "run/agent.sock"))
            self.assertEqual(env["MEADOW_MODEL_CONNECTIONS_FILE"], str(profile / "model-connections-v1.json"))
            self.assertEqual(env["MEADOW_ENABLE_PROMPT_GRANTS"], "1")
            self.assertEqual(env["MEADOW_CONVERSATION_PROFILE"], "meadow-os")
            self.assertEqual(env["MEADOW_WORKSPACE"], str(self.root / "MeadowWork"))
            for name in ["MEADOW_AGENT_API_KEY", "MEADOW_AGENT_HARNESS", "MEADOW_AGENTD_MANAGED"]:
                self.assertNotIn(name, env)

    def test_missing_executable_never_starts_fallback_agent(self):
        with patch.dict(os.environ, {"HOME": str(self.root)}, clear=True), patch("subprocess.Popen") as spawn:
            with self.assertRaisesRegex(ValueError, "missing executable"):
                runtime.run_component(self.root / "missing-prefix", "agent")
            spawn.assert_not_called()
            self.assertFalse((self.root / ".local").exists())

    def test_first_login_shows_welcome_after_service_setup_failure(self):
        prefix = self.root / "opt/meadow"
        (prefix / "bin").mkdir(parents=True)
        welcome = prefix / "bin/meadow-welcome"
        welcome.write_text("fixture")
        welcome.chmod(0o755)
        with patch.dict(os.environ, {"HOME": str(self.root)}, clear=True), \
             patch.object(runtime, "session_init", side_effect=ValueError("fixture missing service")), \
             patch("subprocess.Popen") as spawn:
            self.assertEqual(runtime.first_login(prefix), 1)
            spawn.assert_called_once()
            self.assertEqual(spawn.call_args.args[0], [str(welcome)])

    def test_missing_shell_never_starts_other_ui_or_agent(self):
        with patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":fixture", "XDG_SESSION_TYPE": "x11"}, clear=True), \
             patch("subprocess.Popen") as spawn, patch("os.execve") as execute:
            with self.assertRaisesRegex(ValueError, "missing Meadow Shell"):
                runtime.run_component(self.root / "missing-prefix", "shell")
            spawn.assert_not_called()
            execute.assert_not_called()
            self.assertFalse((self.root / ".local").exists())

    def test_shell_exec_uses_shared_runtime_profile(self):
        prefix = self.root / "opt/meadow"
        script = prefix / "share/meadow-os/shell/meadow_shell.py"
        script.parent.mkdir(parents=True)
        script.write_text("# fixture only")
        with patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":fixture"}, clear=True), \
             patch("os.execve") as execute, patch("os.chdir"):
            runtime.run_component(prefix, "shell")
            executable, argv, env = execute.call_args.args
            self.assertEqual(executable, "/usr/bin/python3")
            self.assertEqual(argv, [executable, str(script)])
            self.assertEqual(env["MEADOW_HOME"], str(self.root / ".local/share/meadow-os/agent"))
            self.assertEqual(env["MEADOW_HOME"], env["MEADOW_DESKTOP_STATE_HOME"])
            self.assertNotIn("MEADOW_AGENTD_MANAGED", env)

    def test_display_does_not_claim_unsupported_mode_or_change_multiple_outputs(self):
        for text, actual in [
            ("Screen 0: minimum 8 x 8, current 1280 x 900, maximum 32767 x 32767\nVirtual-1 connected primary 1280x900+0+0\n   1280x900 60.0*\n", [1280, 900]),
            # The screen framebuffer alone is not proof of the output mode.
            ("Screen 0: minimum 8 x 8, current 1920 x 1080, maximum 32767 x 32767\nVirtual-1 connected primary 1280x900+0+0\n   1280x900 60.0*\n", [1920, 1080]),
            ("Screen 0: minimum 8 x 8, current 1920 x 1080, maximum 32767 x 32767\nVirtual-1 connected primary 960x1080+0+0\nVirtual-2 connected 960x1080+960+0\n", [1920, 1080]),
        ]:
            with self.subTest(text=text), patch("subprocess.run", return_value=SimpleNamespace(stdout=text)) as run:
                result = os_session.configure_display("fixture-xrandr")
                self.assertEqual(result["actual"], actual)
                self.assertFalse(result["applied"])
                self.assertTrue(result["error"])
                self.assertEqual(run.call_count, 1)

    def test_display_verifies_actual_result_after_selecting_advertised_mode(self):
        before = "Screen 0: minimum 8 x 8, current 1280 x 900, maximum 32767 x 32767\nVirtual-1 connected primary 1280x900+0+0\n\tTransform: 1.000000 0.000000 0.000000\n\t           0.000000 1.000000 0.000000\n\t           0.000000 0.000000 1.000000\n   1280x900 60.0*\n   1920x1080 60.0\n"
        for size, expected in [("1920 x 1080", True), ("1280 x 900", False)]:
            after = before.replace("current 1280 x 900", "current " + size)
            if expected:
                after = after.replace("connected primary 1280x900", "connected primary 1920x1080")
            with self.subTest(size=size), patch("subprocess.run", side_effect=[SimpleNamespace(stdout=before), SimpleNamespace(), SimpleNamespace(stdout=after)]) as run:
                result = os_session.configure_display("fixture-xrandr")
                self.assertEqual(result["applied"], expected)
                self.assertEqual(run.call_args_list[1].args[0],
                                 ["fixture-xrandr", "--output", "Virtual-1", "--mode", "1920x1080", "--pos", "0x0", "--scale", "1x1", "--fb", "1920x1080"])

    def test_display_rejects_unverified_transform_even_when_frame_and_mode_match(self):
        text = "Screen 0: minimum 8 x 8, current 1920 x 1080, maximum 32767 x 32767\nVirtual-1 connected primary 1920x1080+0+0\n   1920x1080 60.0*\n"
        with patch("subprocess.run", side_effect=[SimpleNamespace(stdout=text), SimpleNamespace(), SimpleNamespace(stdout=text)]):
            result = os_session.configure_display("fixture-xrandr")
        self.assertFalse(result["applied"])
        self.assertIn("identity transform", result["error"])

    def test_shell_crash_recovery_is_bounded_and_retains_single_wm(self):
        state = self.root / "state"
        state.mkdir(mode=0o700)
        commands = {name: name for name in ["xfwm4", "xfsettingsd", "xprop", "xrandr", "xfce4-terminal"]}
        signals, started = {}, []

        def capture_handler(sig, handler):
            old = signals.get(sig, signal.SIG_DFL)
            signals[sig] = handler
            return old

        def spawn(argv, **kwargs):
            started.append(argv)
            crashed = argv[0] == "/usr/bin/python3"
            process = SimpleNamespace(poll=lambda: 1 if crashed else None, returncode=1 if crashed else None)
            if argv[0] == "xfce4-terminal":
                signals[signal.SIGTERM](signal.SIGTERM, None)
            return process

        with self.isolated_session_bus(), \
             patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":fixture"}, clear=True), \
             patch.object(os_session, "configure_display", return_value={"applied": False}), \
             patch.object(os_session, "wm_owner", return_value="0x123"), \
             patch.object(os_session, "agent_service_state", return_value=None), \
             patch.object(runtime, "session_init", side_effect=ValueError("fixture no service manager")), \
             patch.object(os_session.signal, "signal", side_effect=capture_handler), \
             patch.object(os_session.time, "sleep"), \
             patch.object(os_session, "stop_owned"), \
             patch("subprocess.Popen", side_effect=spawn):
            self.assertEqual(os_session.supervise(self.root / "prefix", state, commands), 0)
        self.assertEqual(sum(argv[0] == "xfwm4" for argv in started), 1)
        self.assertEqual(sum(argv[0] == "/usr/bin/python3" for argv in started), os_session.SHELL_RESTART_LIMIT + 1)
        self.assertEqual(sum(argv[0] == "xfce4-terminal" for argv in started), 1)
        self.assertFalse(any("meadow-agentd" in str(argv) for argv in started))
        status = json.loads((state / "session.json").read_text())
        self.assertEqual(status["shell_restarts"], os_session.SHELL_RESTART_LIMIT)
        self.assertIn("fixture no service manager", status["agent_service_setup_error"])
        self.assertFalse(status["product_ready"])

    def test_session_never_stops_preexisting_agent_or_unknown_service_owner(self):
        commands = {name: name for name in ["xfwm4", "xfsettingsd", "xprop", "xrandr", "xfce4-terminal"]}
        for previous_state in ["active", "activating", None, "inactive"]:
            with self.subTest(previous_state=previous_state):
                state = self.root / (previous_state or "unknown")
                state.mkdir(mode=0o700)
                signals = {}

                def capture_handler(sig, handler):
                    old = signals.get(sig, signal.SIG_DFL)
                    signals[sig] = handler
                    return old

                def spawn(argv, **kwargs):
                    if argv[0] == "/usr/bin/python3":
                        signals[signal.SIGTERM](signal.SIGTERM, None)
                    return SimpleNamespace(poll=lambda: None)

                with self.isolated_session_bus(), \
                     patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":fixture"}, clear=True), \
                     patch.object(os_session, "configure_display", return_value={"applied": False}), \
                     patch.object(os_session, "wm_owner", return_value="0x123"), \
                     patch.object(os_session, "agent_service_state", return_value=previous_state), \
                     patch.object(runtime, "session_init"), \
                     patch.object(os_session.signal, "signal", side_effect=capture_handler), \
                     patch.object(os_session, "stop_owned"), \
                     patch("subprocess.Popen", side_effect=spawn), \
                     patch("subprocess.run", return_value=SimpleNamespace(returncode=0)) as run:
                    self.assertEqual(os_session.supervise(self.root / "prefix", state, commands), 0)
                if previous_state == "inactive":
                    run.assert_called_once()
                    self.assertEqual(run.call_args.args[0], ["systemctl", "--user", "stop", runtime.UNIT])
                else:
                    run.assert_not_called()

    def test_default_keeps_xfwm_composition_even_when_picom_is_installed(self):
        prefix, state = self.root / "prefix", self.root / "state"
        state.mkdir()
        config = prefix / "share/meadow-os/desktop/picom.conf"
        config.parent.mkdir(parents=True)
        config.write_text('backend = "glx";')
        commands = {name: name for name in ["xfwm4", "xfsettingsd", "xprop", "xrandr", "xfce4-terminal", "picom"]}
        signals, started = {}, []
        def capture_handler(sig, handler):
            old = signals.get(sig, signal.SIG_DFL)
            signals[sig] = handler
            return old
        def spawn(argv, **kwargs):
            started.append(argv)
            if argv[0] == "/usr/bin/python3":
                signals[signal.SIGTERM](signal.SIGTERM, None)
            return SimpleNamespace(pid=100 + len(started), poll=lambda: None)
        with self.isolated_session_bus(), \
             patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":97"}, clear=True), \
             patch.object(os_session, "configure_display", return_value={}), \
             patch.object(os_session, "wm_owner", return_value="0x123"), \
             patch.object(os_session, "agent_service_state", return_value="active"), \
             patch.object(runtime, "session_init"), \
             patch.object(os_session.signal, "signal", side_effect=capture_handler), \
             patch.object(os_session, "stop_owned"), \
             patch("subprocess.Popen", side_effect=spawn), \
             patch.object(os_session, "wait_for_shell_paint") as wait, \
             patch.object(os_session, "start_blur_compositor") as blur:
            self.assertEqual(os_session.supervise(prefix, state, commands), 0)
        wait.assert_not_called()
        blur.assert_not_called()
        self.assertEqual(started[0], ["xfwm4", "--compositor=on"])
        status = json.loads((state / "session.json").read_text())
        self.assertEqual(status["compositor"], "xfwm4")
        self.assertEqual(status["compositor_requested"], "xfwm4")
        self.assertFalse(status["native_backdrop_blur"])
        self.assertFalse(status["experimental_glass"])

    def test_supervisor_launches_blur_only_after_current_shell_paint(self):
        prefix = self.root / "prefix"
        config = prefix / "share/meadow-os/desktop/picom.conf"
        config.parent.mkdir(parents=True)
        config.write_text('backend = "glx";')
        commands = {name: name for name in ["xfwm4", "xfsettingsd", "xprop", "xrandr", "xfce4-terminal", "picom"]}
        for painted in (True, False):
            with self.subTest(painted=painted):
                state = self.root / str(painted)
                state.mkdir()
                signals, started = {}, []
                def capture_handler(sig, handler):
                    old = signals.get(sig, signal.SIG_DFL)
                    signals[sig] = handler
                    return old
                def spawn(argv, **kwargs):
                    started.append(argv)
                    return SimpleNamespace(pid=100 + len(started), poll=lambda: None)
                def await_paint(process, path, token, display, stopping):
                    self.assertEqual(started[-1][0], "/usr/bin/python3")
                    self.assertEqual(display, ":97")
                    self.assertTrue(token)
                    if not painted:
                        signals[signal.SIGTERM](signal.SIGTERM, None)
                    return painted
                def start_blur(*_args):
                    signals[signal.SIGTERM](signal.SIGTERM, None)
                    return None
                with self.isolated_session_bus(), \
                     patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":97", "MEADOW_EXPERIMENTAL_GLASS": "1"}, clear=True), \
                     patch.object(os_session, "configure_display", return_value={}), \
                     patch.object(os_session, "wm_owner", return_value="0x123"), \
                     patch.object(os_session, "agent_service_state", return_value="active"), \
                     patch.object(runtime, "session_init"), \
                     patch.object(os_session.signal, "signal", side_effect=capture_handler), \
                     patch.object(os_session, "stop_owned"), \
                     patch("subprocess.Popen", side_effect=spawn), \
                     patch.object(os_session, "wait_for_shell_paint", side_effect=await_paint), \
                     patch.object(os_session, "start_blur_compositor", side_effect=start_blur) as blur:
                    self.assertEqual(os_session.supervise(prefix, state, commands), 0)
                self.assertEqual(blur.call_count, int(painted))
                status = json.loads((state / "session.json").read_text())
                self.assertEqual(status["shell_painted_before_compositor"], painted)
                if not painted:
                    self.assertFalse(status["native_backdrop_blur"])

    def test_shell_recovery_rebuilds_only_owned_compositor_after_new_paint(self):
        prefix, state = self.root / "prefix", self.root / "state"
        state.mkdir()
        config = prefix / "share/meadow-os/desktop/picom.conf"
        config.parent.mkdir(parents=True)
        config.write_text('backend = "glx";')
        commands = {name: name for name in ["xfwm4", "xfsettingsd", "xprop", "xrandr", "xfce4-terminal", "picom"]}
        signals, events, tokens = {}, [], []
        shell_count = [0]
        def capture_handler(sig, handler):
            old = signals.get(sig, signal.SIG_DFL)
            signals[sig] = handler
            return old
        def spawn(argv, **kwargs):
            events.append(argv[0])
            if argv[0] == "/usr/bin/python3":
                shell_count[0] += 1
                return SimpleNamespace(pid=100 + shell_count[0], poll=lambda: 1, returncode=1)
            return SimpleNamespace(pid=50, poll=lambda: None)
        def await_paint(_process, _path, token, *_args):
            tokens.append(token)
            events.append("paint")
            return True
        def start_blur(*_args):
            events.append("picom")
            if len(tokens) == 2:
                signals[signal.SIGTERM](signal.SIGTERM, None)
            return SimpleNamespace(pid=200, poll=lambda: None)
        def stop(process):
            events.append("stop:" + str(process.pid))
        with self.isolated_session_bus(), \
             patch.dict(os.environ, {"HOME": str(self.root), "DISPLAY": ":97", "MEADOW_EXPERIMENTAL_GLASS": "1"}, clear=True), \
             patch.object(os_session, "configure_display", return_value={}), \
             patch.object(os_session, "wm_owner", return_value="0x123"), \
             patch.object(os_session, "agent_service_state", return_value="active"), \
             patch.object(runtime, "session_init"), \
             patch.object(os_session.signal, "signal", side_effect=capture_handler), \
             patch.object(os_session, "stop_owned", side_effect=stop), \
             patch.object(os_session.time, "sleep"), \
             patch("subprocess.Popen", side_effect=spawn), \
             patch.object(os_session, "wait_for_shell_paint", side_effect=await_paint), \
             patch.object(os_session, "start_blur_compositor", side_effect=start_blur):
            self.assertEqual(os_session.supervise(prefix, state, commands), 0)
        self.assertEqual(events[:9], ["xfwm4", "xfsettingsd", "/usr/bin/python3", "paint", "picom",
                                     "stop:200", "/usr/bin/python3", "paint", "picom"])
        self.assertEqual(len(set(tokens)), 2)

    def test_first_paint_receipt_requires_current_launch_pid_and_display(self):
        ready = self.root / "painted.json"
        process = SimpleNamespace(pid=4321, poll=lambda: None)
        valid = {"token": "fresh", "pid": 4321, "display": ":97", "painted": True}
        for change in ({"token": "stale"}, {"pid": 1234}, {"display": ":93"}, {"painted": False}):
            ready.write_text(json.dumps({**valid, **change}))
            with patch.object(os_session.time, "monotonic", side_effect=[0, 100]):
                self.assertFalse(os_session.wait_for_shell_paint(process, ready, "fresh", ":97"))
        ready.write_text(json.dumps(valid))
        self.assertTrue(os_session.wait_for_shell_paint(process, ready, "fresh", ":97"))

    def test_first_paint_wait_stops_on_shell_exit_or_session_shutdown(self):
        path = self.root / "missing-receipt.json"
        exited = SimpleNamespace(pid=4321, poll=lambda: 1)
        alive = SimpleNamespace(pid=4321, poll=lambda: None)
        self.assertFalse(os_session.wait_for_shell_paint(exited, path, "fresh", ":97"))
        self.assertFalse(os_session.wait_for_shell_paint(alive, path, "fresh", ":97", lambda: True))
        with patch.object(os_session.time, "monotonic", side_effect=[0, 100]):
            self.assertFalse(os_session.wait_for_shell_paint(alive, path, "fresh", ":97"))

    def test_blur_compositor_acquires_selection_without_replacing_a_window_manager(self):
        process = SimpleNamespace(pid=4321, poll=lambda: None)
        status = {"native_backdrop_blur": False}
        with patch.object(os_session, "compositor_owner", side_effect=[None, 0xCAFE]), \
             patch.object(os_session, "stop_owned") as stop:
            from unittest.mock import Mock
            start = Mock(return_value=process)
            actual = os_session.start_blur_compositor(start, "picom", Path("/packaged/picom.conf"), status)
        self.assertIs(actual, process)
        start.assert_called_once_with(["picom", "--config", "/packaged/picom.conf"])
        stop.assert_not_called()
        self.assertTrue(status["native_backdrop_blur"])
        self.assertEqual(status["compositor_owner"], "0xcafe")
        self.assertEqual(status["compositor_backend"], "glx")

    def test_blur_compositor_does_not_replace_external_selection_owner(self):
        from unittest.mock import Mock
        start, status = Mock(), {}
        with patch.object(os_session, "compositor_owner", return_value=0xFEED), \
             patch.object(os_session, "stop_owned") as stop:
            actual = os_session.start_blur_compositor(start, "picom", Path("/config"), status)
        self.assertIsNone(actual)
        start.assert_not_called()
        stop.assert_called_once_with(None)
        self.assertFalse(status["native_backdrop_blur"])
        self.assertIn("not replaced", status["compositor_error"])

    def test_blur_startup_failure_cleans_only_the_owned_process(self):
        from unittest.mock import Mock
        process = SimpleNamespace(pid=4321, poll=lambda: 1)
        status = {}
        with patch.object(os_session, "compositor_owner", return_value=None), \
             patch.object(os_session, "stop_owned") as stop:
            actual = os_session.start_blur_compositor(Mock(return_value=process), "picom", Path("/config"), status)
        self.assertIsNone(actual)
        stop.assert_called_once_with(process)
        self.assertEqual(status["compositor"], "none")
        self.assertFalse(status["native_backdrop_blur"])

    def test_compositor_crash_degrades_without_restarting_shell_wm_or_agent(self):
        process = SimpleNamespace(poll=lambda: 1, returncode=1)
        status = {"compositor": "picom", "compositor_pid": 4321,
                  "compositor_owner": "0xcafe", "native_backdrop_blur": True,
                  "state": "shell_running", "shell_restarts": 0}
        with patch("subprocess.Popen") as start, patch.object(os_session, "stop_owned") as stop:
            result, changed = os_session.observe_compositor(process, status)
        self.assertIsNone(result)
        self.assertTrue(changed)
        self.assertFalse(status["native_backdrop_blur"])
        self.assertNotIn("compositor_pid", status)
        self.assertEqual(status["state"], "shell_running")
        self.assertEqual(status["shell_restarts"], 0)
        start.assert_not_called()
        stop.assert_not_called()

    def test_compositor_acquisition_timeout_cannot_claim_blur(self):
        from unittest.mock import Mock
        process = SimpleNamespace(pid=4321, poll=lambda: None)
        status = {}
        with patch.object(os_session, "compositor_owner", return_value=None), \
             patch.object(os_session.time, "monotonic", side_effect=[0, 100]), \
             patch.object(os_session, "stop_owned") as stop:
            actual = os_session.start_blur_compositor(Mock(return_value=process), "picom", Path("/config"), status)
        self.assertIsNone(actual)
        stop.assert_called_once_with(process)
        self.assertFalse(status["native_backdrop_blur"])
        self.assertIn("selection", status["compositor_error"])

    def test_repeated_install_refuses_existing_payload_before_overwrite(self):
        (self.root / "opt/meadow").mkdir(parents=True)
        sentinel = self.root / "opt/meadow/keep"
        sentinel.write_text("original")
        with self.assertRaisesRegex(ValueError, "already exists"):
            image.validate_destination(self.root, {"files": {}})
        self.assertEqual(sentinel.read_text(), "original")

    def test_apt_plan_only_allows_expected_provider_and_metapackage_removals(self):
        plan = self.root / "apt-plan.txt"
        script = image.REPO / "scripts/meadowos/provision.sh"
        command = ["bash", "--noprofile", "--norc", "-c",
                   'source "$1"; meadow_validate_apt_plan "$2"', "_", str(script), str(plan)]
        expected = "Remv build-essential [12]\nRemv coreutils-from-uutils [1]\nInst coreutils-from-gnu (1)\n"
        plan.write_text(expected)
        subprocess.run(command, check=True, capture_output=True, text=True)
        for unexpected in ["coreutils", "gnu-coreutils", "dpkg", "gcc", "sudo-common"]:
            with self.subTest(package=unexpected):
                plan.write_text(expected + f"Remv {unexpected} [1]\n")
                result = subprocess.run(command, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Refusing unexpected apt removal", result.stderr)

    def test_empty_apt_plan_does_not_authorize_protected_package_removal(self):
        plan = self.root / "apt-plan.txt"
        plan.write_text("")
        script = image.REPO / "scripts/meadowos/provision.sh"
        result = subprocess.run(["bash", "--noprofile", "--norc", "-c",
                                 'source "$1"; meadow_validate_apt_plan "$2"', "_", str(script), str(plan)],
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unrecognized or empty", result.stderr)

    @unittest.skipUnless(shutil.which("go"), "requires actual Go toolchain for metadata regression")
    def test_real_go_build_retains_identity_even_with_inherited_trimpath(self):
        # A real compiled/executed fixture catches toolchain metadata behavior;
        # mocked `go version` output would have missed the original packaging bug.
        module = self.root / "identity-module"
        probe = module / "cmd/probe"
        probe.mkdir(parents=True)
        (module / "go.mod").write_text("module github.com/hey-meadow/meadow-code/go/core/buildinfo\n\ngo 1.25.0\n")
        fields = {"DesktopPackageMode": "portable", "DesktopVariant": "vm-lab",
                  "DesktopBuild": "metadata-regression", "Commit": "a" * 40,
                  "BuildDate": "2026-10-21T00:00:00Z"}
        (module / "identity.go").write_text("package buildinfo\n" +
            "\n".join(f'var {name} = "unset"' for name in fields) + "\n")
        (probe / "main.go").write_text('package main\nimport ("encoding/json"; "os"; '
            'info "github.com/hey-meadow/meadow-code/go/core/buildinfo")\n'
            'func main() { json.NewEncoder(os.Stdout).Encode(map[string]string{' +
            ",".join(f'"{name}": info.{name}' for name in fields) + '}) }\n')
        pkg = "github.com/hey-meadow/meadow-code/go/core/buildinfo."
        ldflags = " ".join(f"-X {pkg}{name}={value}" for name, value in fields.items())
        binary = module / "probe"
        env = dict(os.environ, GOFLAGS="-trimpath", GOWORK="off")
        subprocess.run(build.native_build_command("./cmd/probe", binary, ldflags),
                       cwd=module, env=env, check=True, capture_output=True, text=True)
        actual = json.loads(subprocess.check_output([str(binary)], text=True))
        self.assertEqual(actual, fields)
        self.assertEqual(image.go_identity(binary), fields)


if __name__ == "__main__":
    unittest.main()
