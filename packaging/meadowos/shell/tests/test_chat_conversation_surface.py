"""Actual native handlers with widget fixtures; no desktop or model is opened."""
import ast
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import Mock


SOURCE = Path(__file__).resolve().parents[1] / "meadow_shell.py"
spec = importlib.util.spec_from_file_location("chat_surface_source", SOURCE)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
TREE = ast.parse(SOURCE.read_text())


def handlers(class_name, names, namespace):
    source_class = next(node for node in ast.walk(TREE)
                        if isinstance(node, ast.ClassDef) and node.name == class_name)
    nodes = [node for node in source_class.body
             if isinstance(node, ast.FunctionDef) and node.name in names]
    exec(compile(ast.Module(body=nodes, type_ignores=[]), str(SOURCE), "exec"), namespace)
    return type(class_name + "Fixture", (), {name: namespace[name] for name in names})


class Widget:
    def __init__(self, *args, **kwargs):
        self.children, self.text = [], args[0] if args else ""
        self.visible, self.shown = False, 0

    def get_style_context(self):
        return SimpleNamespace(add_class=lambda *_: None)

    def pack_start(self, child, *_args):
        self.children.append(child)

    def get_children(self):
        return list(self.children)

    def remove(self, child):
        self.children.remove(child)

    def show_all(self):
        self.visible = True
        self.shown += 1

    def hide(self):
        self.visible = False

    def get_visible(self):
        return self.visible

    def set_text(self, value):
        self.text = value

    set_selectable = set_max_width_chars = set_tooltip_text = lambda *_: None


class ConversationSurfaceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        namespace = {"Gtk": SimpleNamespace(Box=Widget, Orientation=SimpleNamespace(VERTICAL=1)),
                     "label": Widget, "failed_tool_text": module.failed_tool_text,
                     "FAILED": module.FAILED, "REPLIED": module.REPLIED, "json": module.json,
                     "uuid": module.uuid}
        self.shell = SimpleNamespace(voiceos=None, voice_transport=None,
                                     refresh_work_surfaces=Mock(), refresh_work_tasks=Mock())
        namespace["shell"] = self.shell
        fixture = handlers("Companion", {"on_event", "append_system_notice", "system_notice", "show_service_error"}, namespace)
        self.view = fixture()
        view = self.view
        view.timeline, view.work_records, view.error, view.empty = [Widget() for _ in range(4)]
        view.empty.visible = True
        view.notices = module.PresentationNotices(Path(self.tmp.name) / "notices.json", "chat-A")
        view.assistant_messages = module.AssistantMessages()
        view.assistant_bubbles = {}
        view.assistant_label, view.current_run_id = None, None
        view.assistant_text = view.voice_reply_text = ""
        view.voice_replay_through = 0
        view.turn_had_reply = view.running = False
        view.tool_progress = view.work_notifications = None
        view.follow_bottom, view.mark_unread, view.update_controls = Mock(), Mock(), Mock()
        view.is_active = lambda: True
        view.permission, view.user_input = Mock(), Mock()
        view.requests = Widget()
        self.messages = []
        def append(role, text):
            row = [role, text]
            self.messages.append(row)
            return SimpleNamespace(set_message_text=lambda value: row.__setitem__(1, value))
        view.append_message = append

    def event(self, kind, data, identity=1):
        self.view.on_event({"id": identity, "type": kind, "data": data})

    def test_recovered_tool_failure_stays_in_log_without_polluting_or_scrolling_chat(self):
        self.event("session_update", {"update": {"sessionUpdate": "tool_call_update",
            "status": "failed", "toolCallId": "a", "title": "read_file", "runId": "r"}})
        self.event("session_update", {"update": {"sessionUpdate": "agent_message_chunk",
            "messageId": "r:assistant:2", "content": {"text": "找到規格了。"}}}, 2)
        self.event("turn_completed", {"stopReason": "end_turn"}, 3)
        self.assertEqual(self.messages, [["assistant", "找到規格了。"]])
        self.assertEqual(len(self.view.work_records.children), 1)
        self.assertEqual(self.view.timeline.children, [])
        self.assertEqual(self.view.error.text, "")
        self.view.follow_bottom.assert_not_called()
        self.assertEqual(self.view.notices.rows[0]["kind"], "tool_failed")

    def test_internal_completion_does_not_become_a_reply_but_empty_response_has_actionable_alert(self):
        self.event("turn_completed", {"stopReason": "completed"})
        self.assertEqual(self.messages, [])
        self.assertEqual(self.view.timeline.children, [])
        self.assertEqual(len(self.view.work_records.children), 1)
        self.assertIn("工作紀錄", self.view.error.text)
        self.view.mark_unread.assert_called_once()

    def test_required_user_questions_and_permissions_are_not_hidden_with_tool_records(self):
        question = {"requestId": "q", "questions": [{"question": "要用哪個檔案？"}]}
        permission = {"requestId": "p", "title": "受保護操作"}
        self.event("user_input_request", question)
        self.event("permission_request", permission, 2)
        self.view.user_input.assert_called_once_with(question)
        self.view.permission.assert_called_once_with(permission)
        self.assertEqual(self.messages, [])

    def test_private_thought_is_neither_chat_nor_work_record(self):
        self.event("session_update", {"update": {"sessionUpdate": "agent_thought_chunk",
            "content": {"text": "PRIVATE_THOUGHT"}}})
        self.assertEqual(self.messages, [])
        self.assertEqual(self.view.work_records.children, [])

    def test_restored_technical_history_stays_outside_front_chat_and_is_bounded(self):
        for index in range(45):
            self.view.append_system_notice({"kind": "background_completed", "text": str(index)}, previous=True)
        self.assertEqual(len(self.view.work_records.children), 40)
        self.assertEqual(self.view.timeline.children, [])
        self.assertTrue(self.view.empty.visible)
        self.view.follow_bottom.assert_not_called()

    def test_unsent_voice_keeps_exact_user_text_and_recovery_in_chat(self):
        text = "未送出的語音：\n幫我查這台電腦。"
        self.view.append_system_notice({"kind": "voice_draft", "text": text})
        self.assertEqual(self.view.work_records.children, [])
        self.assertEqual(self.view.timeline.children[0].children[1].text, text)
        self.assertFalse(self.view.empty.visible)
        self.view.follow_bottom.assert_called_once()

    def test_raw_stream_error_is_durable_in_log_and_front_alert_is_actionable(self):
        detail = "SSE read failed: synthetic transport diagnostic"
        self.event("stream_error", {"error": detail})
        self.assertNotIn(detail, self.view.error.text)
        self.assertIn("重新連線", self.view.error.text)
        self.assertEqual(self.view.timeline.children, [])
        self.assertEqual(self.view.work_records.children[0].children[1].text, detail)
        restored = module.PresentationNotices(Path(self.tmp.name) / "notices.json", "chat-A")
        self.assertEqual(restored.rows[0]["text"], detail)


class VoiceChatPresentationTests(unittest.TestCase):
    def setUp(self):
        self.idles = []
        namespace = {"Gtk": SimpleNamespace(get_current_event_time=lambda: 123),
                     "GLib": SimpleNamespace(idle_add=lambda fn, *args: self.idles.append((fn, args))),
                     "Gdk": SimpleNamespace(WindowState=SimpleNamespace(MAXIMIZED=1, ICONIFIED=2)),
                     "bounded_geometry": module.bounded_geometry, "os": module.os,
                     "Wnck": SimpleNamespace(WindowType=SimpleNamespace(NORMAL=1))}
        companion = handlers("Companion", {"show_for_voice", "place_voice_chat", "restore_position",
            "hide_chat", "minimize_chat", "toggle"}, namespace)
        shell_class = handlers("Shell", {"voice_show_chat", "voice_action", "voice_active_window_changed",
            "start_voiceos"}, namespace)
        self.shell = shell_class()
        namespace["shell"] = self.shell
        self.shell.geometry = SimpleNamespace(x=-1920, y=0, width=1920, height=1080)
        self.shell.voiceos = SimpleNamespace(enabled=True, expanded=True, collapse=Mock(), show_companion=Mock(),
            release_companion=Mock(), focus_overlay=Mock(), expand=Mock(),
            window=SimpleNamespace(get_window=lambda: SimpleNamespace(get_xid=lambda: 1)))
        self.shell.voice_transport = SimpleNamespace(publish=Mock())
        self.shell.voice_listening = False
        self.shell.update_chat_button = Mock()
        self.shell.update_windows = Mock()
        self.chat = companion()
        self.shell.chat = self.chat
        self.visible = False
        self.native_state = 0
        self.chat.get_window = lambda: SimpleNamespace(get_state=lambda: self.native_state)
        self.chat.get_visible = lambda: self.visible
        self.chat.show_all = lambda: setattr(self, "visible", True)
        self.chat.hide = lambda: setattr(self, "visible", False)
        self.chat.iconify = Mock()
        self.chat.save_draft = self.chat.remember_normal_geometry = Mock()
        self.chat.geometry_guard = SimpleNamespace(current_area=lambda: (-1920, 34, 1920, 948))
        self.chat.last_size = None
        self.chat.last_position = (200, 200)
        self.chat.unmaximize, self.chat.deiconify = Mock(), Mock()
        self.chat.present_with_time, self.chat.move, self.chat.resize = Mock(), Mock(), Mock()
        self.chat.input = SimpleNamespace(grab_focus=Mock())
        self.chat.chrome = SimpleNamespace(pin_button=SimpleNamespace(get_active=lambda: False),
                                           set_overlay_above=Mock(), dismiss=Mock())
        self.chat.iconified = self.chat.maximized = False

    def drain(self):
        pending, self.idles = self.idles, []
        for callback, args in pending:
            callback(*args)

    def test_repeated_chat_requests_are_visible_top_right_and_do_not_collapse_overlay(self):
        for _ in range(2):
            self.shell.voice_show_chat(force=True)
            self.drain()
        self.assertTrue(self.visible)
        self.assertTrue(self.shell.voiceos.expanded)
        self.shell.voiceos.collapse.assert_not_called()
        self.assertEqual(self.shell.voiceos.show_companion.call_count, 2)
        self.chat.move.assert_called_with(-544, 56)
        self.chat.resize.assert_called_with(520, 550)
        self.assertFalse(self.chat.voice_placement_pending)

    def test_maximized_chat_waits_for_actual_window_state_then_places_inside_screen(self):
        self.chat.last_size = (900, 4000)
        self.native_state = 1
        self.shell.voice_show_chat()
        self.drain()
        self.chat.move.assert_not_called()
        self.assertTrue(self.chat.voice_placement_pending)
        self.native_state = 0
        self.chat.restore_position()
        x, y = self.chat.last_position
        width, height = self.chat.last_size
        self.assertGreaterEqual(x, -1920)
        self.assertLessEqual(x + width, 0)
        self.assertLessEqual(y + height, 982)
        self.assertFalse(self.chat.voice_placement_pending)

    def test_force_show_without_voiceos_is_idempotent(self):
        self.shell.voiceos = None
        self.shell.voice_show_chat(force=True)
        self.shell.voice_show_chat(force=True)
        self.assertTrue(self.visible)
        self.assertEqual(self.chat.present_with_time.call_count, 2)

    def test_explicit_exit_disables_capture_and_pending_speech(self):
        self.shell.voice_listening = True
        self.shell.voice_action("exit", {})
        self.assertFalse(self.shell.voice_listening)
        self.shell.voice_transport.publish.assert_called_once_with(
            active=False, microphoneRequested=False, speech=None)

    def test_orb_retries_stale_mic_intent_and_pauses_only_observed_capture(self):
        self.shell.voice_action("toggle-microphone", {})
        calls = self.shell.voice_transport.publish.call_args_list
        self.assertEqual([call.kwargs for call in calls],
            [{"microphoneRequested": False}, {"microphoneRequested": True}])
        self.shell.voice_transport.publish.reset_mock()
        self.shell.voice_listening = True
        self.shell.voice_action("toggle-microphone", {})
        self.shell.voice_transport.publish.assert_called_once_with(microphoneRequested=False)

    def test_opening_voiceos_requests_host_microphone(self):
        self.shell.start_voiceos()
        self.shell.voiceos.expand.assert_called_once()
        self.shell.voice_transport.publish.assert_called_once_with(active=True, microphoneRequested=True)

    def test_hide_and_minimize_suppress_wm_fallback_then_return_focus_to_overlay(self):
        for action in (self.chat.hide_chat, self.chat.minimize_chat):
            self.shell.voice_focus_owned = True
            self.shell.voiceos.release_companion.side_effect = lambda _: self.assertFalse(self.shell.voice_focus_owned)
            self.visible = True
            action()
            self.assertFalse(self.shell.voice_focus_owned)
            self.drain()
        self.assertEqual(self.shell.voiceos.focus_overlay.call_count, 2)

    def test_window_fallback_and_notification_do_not_collapse_but_external_app_activation_does(self):
        active = SimpleNamespace(get_xid=lambda: 2, get_pid=lambda: -1, get_window_type=lambda: 1)
        self.shell.screen = SimpleNamespace(get_active_window=lambda: active)
        self.shell.voice_focus_owned = False
        self.shell.voice_active_window_changed()
        self.shell.voiceos.collapse.assert_not_called()
        active.get_xid = lambda: 1
        self.shell.voice_active_window_changed()
        self.assertTrue(self.shell.voice_focus_owned)
        active.get_xid = lambda: 2
        active.get_window_type = lambda: 2
        self.shell.voice_active_window_changed()
        self.shell.voiceos.collapse.assert_not_called()
        active.get_window_type = lambda: 1
        self.shell.voice_active_window_changed()
        self.shell.voiceos.collapse.assert_called_once()

    def test_legacy_chat_toggle_during_voice_is_also_presentation_not_collapse(self):
        self.chat.toggle()
        self.drain()
        self.assertTrue(self.visible)
        self.shell.voiceos.collapse.assert_not_called()


class WorkLogTransientTests(unittest.TestCase):
    def test_constructor_makes_log_a_transient_of_chat_without_forcing_its_pin(self):
        class NativeWidget(Widget):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self.transient_for = None
                self.keep_above = False

            def set_transient_for(self, parent):
                self.transient_for = parent

            def set_keep_above(self, value):
                self.keep_above = value

            def add(self, child):
                self.pack_start(child)

            set_default_size = set_skip_taskbar_hint = set_border_width = set_expanded = lambda *_: None
            set_policy = set_propagate_natural_height = connect = lambda *_: None

        chrome = Mock(side_effect=lambda _owner, content, **_kwargs: content)
        namespace = {"Gtk": SimpleNamespace(Window=NativeWidget, Box=NativeWidget,
            ScrolledWindow=NativeWidget, Orientation=SimpleNamespace(VERTICAL=1),
            PolicyType=SimpleNamespace(NEVER=0, AUTOMATIC=1)), "label": NativeWidget,
            "composited_surface": Mock(), "GlassSurface": NativeWidget,
            "create_hover_chrome": chrome, "ChatGeometry": Mock()}
        fixture = handlers("Companion", {"create_work_log"}, namespace)
        chat = fixture()
        chat.keep_above = True  # The native parent may already be user-pinned.
        chat.pending, chat.progress = NativeWidget(), NativeWidget()
        chat.hide_work_log = Mock()
        chat.create_work_log()
        self.assertIs(chat.work_log.transient_for, chat)
        self.assertTrue(chat.keep_above)
        self.assertFalse(chat.work_log.keep_above)
        self.assertFalse(chat.work_log.visible)
        self.assertTrue(chrome.call_args.kwargs["allow_pin"])


class ChromeLayerTests(unittest.TestCase):
    def test_temporary_layer_and_user_pin_are_composed_without_changing_preference(self):
        path = SOURCE.parent.parent / "desktop/meadow_window_chrome.py"
        tree = ast.parse(path.read_text())
        method = next(node for node in ast.walk(tree)
                      if isinstance(node, ast.FunctionDef) and node.name == "set_overlay_above")
        namespace = {}
        exec(compile(ast.Module(body=[method], type_ignores=[]), str(path), "exec"), namespace)
        owner, pin = SimpleNamespace(set_keep_above=Mock()), SimpleNamespace(get_active=Mock(return_value=False))
        view = SimpleNamespace(owner=owner, pin_button=pin, overlay_above=False)
        apply = lambda value: namespace["set_overlay_above"](view, value)
        apply(True)
        owner.set_keep_above.assert_called_with(True)
        # Toggling the explicit pin off during a temporary lift must stay above.
        apply(view.overlay_above)
        owner.set_keep_above.assert_called_with(True)
        pin.get_active.return_value = True
        apply(False)
        owner.set_keep_above.assert_called_with(True)
        pin.get_active.return_value = False
        apply(False)
        owner.set_keep_above.assert_called_with(False)


if __name__ == "__main__":
    unittest.main()
