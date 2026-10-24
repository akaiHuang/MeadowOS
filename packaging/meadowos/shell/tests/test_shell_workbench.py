"""Run actual Shell lifecycle methods with scheduled workers and fake widgets.

No GTK, daemon, model, document or user workspace is opened by these tests.
"""
import ast
import importlib.util
import json
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "meadow_shell.py"
TREE = ast.parse(SOURCE.read_text())
SHELL = next(node for node in ast.walk(TREE) if isinstance(node, ast.ClassDef) and node.name == "Shell")
NAMES = {"current_work_binding", "ensure_work_surfaces", "work_connection_changed",
         "show_work_files", "show_work_tasks", "refresh_work_tasks", "work_tasks_ready",
         "refresh_work_surfaces", "work_preview_ready", "voice_mode_changed", "voice_action"}
METHODS = [node for node in SHELL.body if isinstance(node, ast.FunctionDef) and node.name in NAMES]


class AgentError(Exception):
    pass


class Surface:
    def __init__(self, visible=False):
        self.visible = self.children_visible = visible
        self.closed = False
        self.errors, self.snapshots, self.presentations, self.refreshes = [], [], 0, 0
        self.position = (20, 30)
        self.last_position = self.position
        self.navigations = []

    def get_visible(self):
        return self.visible

    def hide(self):
        self.visible = False

    def hide_surface(self):
        self.last_position = self.position
        self.hide()

    def show(self):
        if self.closed:
            raise AssertionError("show reached a destroyed surface")
        self.visible = True

    def show_all(self):
        self.show()
        self.children_visible = True

    def present_surface(self):
        self.show_all()
        self.position = self.last_position
        self.presentations += 1

    def close_surface(self):
        self.closed, self.visible = True, False

    def set_error(self, text):
        self.errors.append(text)

    def set_snapshot(self, snapshot):
        if self.closed:
            raise AssertionError("snapshot reached a destroyed surface")
        self.snapshots.append(snapshot)

    def refresh(self):
        self.refreshes += 1

    def navigate(self, path):
        self.navigations.append(path)


class Scheduler:
    def __init__(self):
        self.workers, self.callbacks = [], []

    def thread(self, *, target, **_kwargs):
        return SimpleNamespace(start=lambda: self.workers.append(target))

    def idle_add(self, callback, *args):
        self.callbacks.append((callback, args))
        return len(self.callbacks)

    def finish_worker(self):
        self.workers.pop(0)()

    def apply_callback(self):
        callback, args = self.callbacks.pop(0)
        return callback(*args)


class ShellWorkbenchTests(unittest.TestCase):
    def setUp(self):
        self.scheduler = Scheduler()
        self.process = SimpleNamespace(wait=Mock())
        self.popen = Mock(return_value=self.process)
        namespace = {"AgentError": AgentError,
                     "GLib": SimpleNamespace(idle_add=self.scheduler.idle_add, Error=type("GLibError", (Exception,), {})),
                     "threading": SimpleNamespace(Thread=self.scheduler.thread),
                     "subprocess": SimpleNamespace(Popen=self.popen, DEVNULL=-3)}
        exec(compile(ast.Module(body=METHODS, type_ignores=[]), str(SOURCE), "exec"), namespace)
        harness = type("ShellHarness", (), {name: namespace[name] for name in NAMES})
        self.shell = harness()
        self.client = SimpleNamespace(workspace=Path("/tmp/work-A"),
                                      session={"sessionId": "session-A"}, task_plan=Mock())
        self.chat = Surface(True)
        self.chat.work_log = Surface(False)
        self.chat.client, self.chat.generation, self.chat.connected = self.client, 1, True
        self.chat.error = SimpleNamespace(set_text=lambda text: self.chat.errors.append(text))
        self.shell.chat = self.chat
        self.shell.closing = False
        self.shell.geometry = SimpleNamespace(x=0, y=0, width=1920, height=1080)
        self.shell.work_binding = self.shell.current_work_binding()
        self.tasks, self.files = Surface(True), Surface(False)
        self.shell.work_surfaces = (self.tasks, self.files)
        self.shell.work_request, self.shell.work_refresh_pending = None, False
        self.shell.voiceos = SimpleNamespace(enabled=True, expanded=False, palette="jade", collapse=Mock())
        self.shell.voice_listening = False
        self.shell.voice_transport = SimpleNamespace(publish=Mock(), toggle_microphone=Mock())
        self.shell.top, self.shell.dock, self.shell.launcher = Surface(True), Surface(True), Surface(False)
        self.shell.notification_window = None
        self.shell.voice_hidden_surfaces = []
        self.shell.voice_show_chat = Mock()
        self.shell.voice_event = Mock()
        self.shell.present_work_file = Mock()
        self.new_surfaces = []

        def factory(*_args):
            surfaces = (Surface(), Surface())
            self.new_surfaces.append(surfaces)
            return surfaces
        self.factory = Mock(side_effect=factory)
        self.addCleanup(patch.stopall)
        patch.dict(sys.modules, {"workbench_surface": SimpleNamespace(create_work_surfaces=self.factory)}).start()

    def new_connection(self):
        self.chat.client = SimpleNamespace(workspace=Path("/tmp/work-B"),
            session={"sessionId": "session-B"}, task_plan=Mock(return_value={"revision": 2}))
        self.chat.generation += 1

    def test_one_worker_coalesces_updates_and_ui_only_changes_on_idle(self):
        self.client.task_plan.side_effect = [{"revision": 1}, {"revision": 2}]
        for _ in range(4):
            self.shell.refresh_work_tasks()
        self.assertEqual(len(self.scheduler.workers), 1)
        self.scheduler.finish_worker()
        self.assertEqual(self.tasks.snapshots, [])
        self.scheduler.apply_callback()
        self.assertEqual(self.tasks.snapshots, [{"revision": 1}])
        self.assertEqual(len(self.scheduler.workers), 1)
        self.scheduler.finish_worker()
        self.scheduler.apply_callback()
        self.assertEqual(self.tasks.snapshots, [{"revision": 1}, {"revision": 2}])
        self.assertEqual(self.client.task_plan.call_count, 2)
        self.assertIsNone(self.shell.work_request)
        self.assertFalse(self.shell.work_refresh_pending)

    def test_reconnect_discards_old_callback_and_fetches_new_session(self):
        self.client.task_plan.return_value = {"revision": "old"}
        self.shell.refresh_work_tasks()
        self.new_connection()
        self.shell.work_connection_changed()
        new_tasks = self.shell.work_surfaces[0]
        self.assertTrue(self.tasks.closed)
        self.scheduler.finish_worker()
        self.scheduler.apply_callback()
        self.assertEqual(self.tasks.snapshots, [])
        self.assertEqual(new_tasks.snapshots, [])
        self.scheduler.finish_worker()
        self.scheduler.apply_callback()
        self.assertEqual(new_tasks.snapshots, [{"revision": 2}])

    def test_stale_token_cannot_clear_or_update_a_new_request(self):
        current = self.shell.work_request = object()
        self.shell.work_tasks_ready(object(), self.shell.work_binding, self.tasks, {"bad": True}, None)
        self.assertIs(self.shell.work_request, current)
        self.assertEqual(self.tasks.snapshots, [])

    def test_shutdown_ignores_inflight_worker_result(self):
        self.client.task_plan.return_value = {"revision": 1}
        self.shell.refresh_work_tasks()
        self.shell.closing = True
        self.tasks.close_surface()
        self.scheduler.finish_worker()
        self.scheduler.apply_callback()
        self.assertEqual(self.tasks.snapshots, [])
        self.assertIsNone(self.shell.work_request)

    def test_read_failure_surfaces_error_and_retains_prior_snapshot(self):
        previous = {"revision": 1}
        self.tasks.snapshots.append(previous)
        self.client.task_plan.side_effect = AgentError("connection interrupted")
        self.shell.refresh_work_tasks()
        self.scheduler.finish_worker()
        self.scheduler.apply_callback()
        self.assertEqual(self.tasks.snapshots, [previous])
        self.assertEqual(self.tasks.errors, ["connection interrupted"])

    def test_disconnected_refresh_is_explicitly_unknown_without_get(self):
        self.chat.connected = False
        self.shell.refresh_work_tasks()
        self.assertTrue(self.tasks.errors)
        self.assertEqual(self.scheduler.workers, [])

    def test_fullscreen_restores_only_previously_visible_surfaces(self):
        self.tasks.position = (460, 250)
        self.shell.voice_mode_changed(True)
        self.assertFalse(self.tasks.visible)
        self.assertFalse(self.chat.visible)
        self.tasks.position = (999, 999)
        self.shell.voice_mode_changed(False)
        self.assertTrue(self.tasks.visible)
        self.assertTrue(self.tasks.children_visible)
        self.assertTrue(self.chat.visible)
        self.assertFalse(self.files.visible)
        self.assertFalse(self.shell.launcher.visible)
        self.assertEqual(self.tasks.position, (460, 250))

    def test_reconnect_during_fullscreen_restores_populated_fresh_windows(self):
        self.files.present_surface()
        self.shell.voiceos.expanded = True
        self.shell.voice_mode_changed(True)
        self.new_connection()
        self.shell.work_connection_changed()
        tasks, files = self.shell.work_surfaces
        self.assertFalse(tasks.visible)
        self.assertFalse(files.visible)
        self.assertNotIn(self.tasks, self.shell.voice_hidden_surfaces)
        self.shell.voiceos.expanded = False
        self.shell.voice_mode_changed(False)
        self.assertTrue(tasks.visible and tasks.children_visible)
        self.assertTrue(files.visible and files.children_visible)

    def test_factory_failure_does_not_commit_binding_or_poison_retry(self):
        self.new_connection()
        self.factory.side_effect = RuntimeError("work surface unavailable")
        self.assertFalse(self.shell.ensure_work_surfaces())
        self.assertNotEqual(self.shell.work_binding, self.shell.current_work_binding())
        self.assertTrue(self.chat.errors)
        fresh = (Surface(), Surface())
        self.factory.side_effect = None
        self.factory.return_value = fresh
        self.assertTrue(self.shell.ensure_work_surfaces())
        self.assertEqual(self.shell.work_surfaces, fresh)

    def test_explicit_actions_only_open_requested_surface(self):
        self.shell.show_work_tasks = Mock()
        self.shell.show_work_files = Mock()
        self.shell.voice_action("work-tasks", {"arbitrary": "ignored"})
        self.shell.voice_action("work-files", {})
        self.shell.voice_action("show-chat", {})
        self.shell.voice_action("run-command", {"command": "never"})
        self.shell.show_work_tasks.assert_called_once_with()
        self.shell.show_work_files.assert_called_once_with()
        self.shell.voice_show_chat.assert_called_once_with(force=True)
        self.shell.voice_transport.toggle_microphone.assert_not_called()
        self.assertEqual(self.scheduler.workers, [])

    def test_late_preview_callback_cleans_snapshot_without_launching(self):
        binding = self.shell.work_binding
        temporary = SimpleNamespace(cleanup=Mock())
        self.new_connection()
        self.shell.work_preview_ready(binding, self.files, Path("/tmp/work-A/source.html"),
            Path("/opt/meadow/bin/meadow-quicklook"), temporary,
            Path("/private/snapshot.html"), False, None)
        temporary.cleanup.assert_called_once_with()
        self.popen.assert_not_called()
        self.shell.voiceos.collapse.assert_not_called()

    def test_preview_only_passes_private_snapshot_to_quicklook_and_reaps(self):
        temporary = SimpleNamespace(cleanup=Mock())
        self.shell.work_preview_ready(self.shell.work_binding, self.files,
            Path("/tmp/work-A/run-me.sh"), Path("/opt/meadow/bin/meadow-quicklook"),
            temporary, Path("/private/snapshot.sh"), False, None)
        self.assertEqual(self.popen.call_args.args[0],
            ["/opt/meadow/bin/meadow-quicklook", "--path", "/private/snapshot.sh"])
        self.assertNotIn("shell", self.popen.call_args.kwargs)
        temporary.cleanup.assert_not_called()
        self.scheduler.finish_worker()
        self.process.wait.assert_called_once_with()
        temporary.cleanup.assert_called_once_with()

    def test_preview_launch_failure_cleans_snapshot_and_presents_error(self):
        self.popen.side_effect = OSError("viewer unavailable")
        temporary = SimpleNamespace(cleanup=Mock())
        self.shell.work_preview_ready(self.shell.work_binding, self.files,
            Path("/tmp/work-A/note.md"), Path("/opt/meadow/bin/meadow-quicklook"),
            temporary, Path("/private/snapshot.md"), False, None)
        temporary.cleanup.assert_called_once_with()
        self.assertEqual(self.files.errors, ["viewer unavailable"])
        self.assertEqual(self.scheduler.workers, [])

    def test_directory_preview_navigates_existing_files_without_a_process(self):
        path = Path("/tmp/work-A/notes")
        self.shell.work_preview_ready(self.shell.work_binding, self.files, path,
            Path("/opt/meadow/bin/meadow-quicklook"), None, None, True, None)
        self.assertEqual(self.files.navigations, [path])
        self.assertTrue(self.files.visible)
        self.popen.assert_not_called()


class NativeActionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location("workbench_voiceos_surface", ROOT / "voiceos_surface.py")
        cls.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.module)

    def message(self, surface, payload):
        value = SimpleNamespace(is_string=lambda: True, to_string=lambda: json.dumps(payload))
        surface._message(None, SimpleNamespace(get_js_value=lambda: value))

    def test_native_bridge_accepts_only_known_surface_actions_and_drops_operands(self):
        surface = self.module.VoiceOSSurface.__new__(self.module.VoiceOSSurface)
        surface.closed = False
        surface.on_action = Mock()
        for kind in ("work-files", "work-tasks", "show-chat"):
            self.message(surface, {"type": kind, "path": "/tmp/not-an-opening-request"})
        self.assertEqual(surface.on_action.call_args_list,
            [unittest.mock.call("work-files", {}), unittest.mock.call("work-tasks", {}),
             unittest.mock.call("show-chat", {})])
        self.message(surface, {"type": "open-file", "path": "/tmp/no"})
        self.assertEqual(surface.on_action.call_count, 3)
        surface.closed = True
        self.message(surface, {"type": "work-files"})
        self.assertEqual(surface.on_action.call_count, 3)


if __name__ == "__main__":
    unittest.main()
