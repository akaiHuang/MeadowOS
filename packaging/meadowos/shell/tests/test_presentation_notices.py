"""Observed native SSE regressions; no GTK display, provider or Agent is started."""
import ast
import copy
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[1] / 'meadow_shell.py'
spec = importlib.util.spec_from_file_location('meadow_shell', SOURCE)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
klass = next(node for node in ast.walk(ast.parse(SOURCE.read_text())) if isinstance(node, ast.ClassDef) and node.name == 'Companion')
methods = [node for node in klass.body if isinstance(node, ast.FunctionDef) and node.name in {'on_event', 'hide_chat', 'map_chat', 'restore_position', 'remember_normal_geometry', 'capture_geometry', 'restore_remapped_geometry', 'toggle', 'toggle_maximize', 'window_state', 'reflow_messages', 'reflow_message_widths', 'message_width_limit'}]
globals_ = {'failed_tool_text': module.failed_tool_text, 'json': module.json,
            'FAILED': module.FAILED, 'REPLIED': module.REPLIED}
exec(compile(ast.Module(body=methods, type_ignores=[]), str(SOURCE), 'exec'), globals_)

# Actual failed-tool envelope observed in the Linux MVP task. No model reply is fabricated.
FAILURE = {'id': 416, 'type': 'session_update', 'data': {'update': {
    'sessionUpdate': 'tool_call_update', 'status': 'failed',
    'title': 'meadow_browser_visual_check', 'toolCallId': 'call_2',
    'rawOutput': 'Meadow Browser 未啟動，請改用本機檢查',
    'runId': '5e33a5ca-observed-run'}}}


class View:
    on_event = globals_['on_event']
    hide_chat = globals_['hide_chat']
    map_chat = globals_['map_chat']
    restore_position = globals_['restore_position']
    remember_normal_geometry = globals_['remember_normal_geometry']
    toggle = globals_['toggle']
    toggle_maximize = globals_['toggle_maximize']
    window_state = globals_['window_state']
    capture_geometry = globals_['capture_geometry']
    restore_remapped_geometry = globals_['restore_remapped_geometry']
    reflow_messages = globals_['reflow_messages']
    reflow_message_widths = globals_['reflow_message_widths']
    message_width_limit = globals_['message_width_limit']

    def __init__(self, path):
        self.notices = module.PresentationNotices(path, 'exact-chat-id')
        self.turn_had_reply = self.running = False
        self.current_run_id = self.assistant_label = None
        self.assistant_text = ''
        self.assistant_messages = module.AssistantMessages()
        self.assistant_bubbles = {}
        self.voice_reply_text = ''
        self.voice_replay_through = 0
        self.work_notifications = None
        self.tool_progress = None
        self.messages, self.presented = [], []
        self.progress_states = []
        self.error = SimpleNamespace(set_text=lambda text: setattr(self, 'error_text', text))
        self.error_text = ''
        self.maximized = self.iconified = self.reopen_maximized = False
        self.last_size = None
        self.position_restore_pending = True
        self.voice_placement_pending = False
        self.maximized_hide_geometry = None
        self.remapped_maximized = False
        self.chrome = SimpleNamespace(dismiss=lambda **kwargs: None)

    def system_notice(self, event, kind, text):
        row = self.notices.add(event, kind, text)
        if row:
            self.presented.append(row)
            self.error_text = text

    def append_message(self, role, text):
        index = len(self.messages)
        self.messages.append((role, text))
        return SimpleNamespace(set_message_text=lambda text: self.messages.__setitem__(index, (role, text)))

    def update_controls(self):
        pass

    def render_progress(self):
        self.progress_states.append(self.tool_progress.view())

    mark_unread = follow_bottom = update_controls

    def run(self, identity):
        self.on_event({'type': 'run_state', 'data': {'status': 'running', 'runId': identity}})

    def finish(self, identity, event_id=417):
        self.on_event({'id': event_id, 'type': 'turn_completed', 'data': {'runId': identity, 'stopReason': 'end_turn'}})


class ShellView(SimpleNamespace):
    # Workbench refresh is an independent observer of the same terminal events.
    # These notice tests retain their existing delivery/voice assertions.
    def refresh_work_surfaces(self):
        pass

    def refresh_work_tasks(self):
        pass


class PresentationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / 'shell-notices.json'
        self.view = View(self.path)
        globals_['shell'] = ShellView(voiceos=None, voice_transport=None)

    def test_actual_failed_tool_and_empty_terminal_are_system_notices(self):
        self.view.run('5e33a5ca-observed-run')
        self.view.on_event(FAILURE)
        self.view.finish('5e33a5ca-observed-run')
        self.assertEqual(self.view.messages, [])
        self.assertEqual([row['kind'] for row in self.view.presented], ['tool_failed', 'empty_reply'])
        self.assertIn('meadow_browser_visual_check', self.view.presented[0]['text'])
        self.assertNotIn('Meadow Browser 未啟動，請改用本機檢查', self.view.presented[0]['text'])
        restored = module.PresentationNotices(self.path, 'exact-chat-id')
        self.assertEqual(restored.rows, self.view.presented)
        self.assertEqual(module.PresentationNotices(self.path, 'other-chat-id').rows, [])
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)

    def test_replay_deduplicates_exact_event_and_new_reply_has_no_old_error(self):
        self.view.run('5e33a5ca-observed-run')
        for _ in range(2):
            self.view.on_event(FAILURE)
            self.view.finish('5e33a5ca-observed-run')
        self.assertEqual(len(self.view.presented), 2)
        self.view.run('new-run')
        self.assertFalse(self.view.turn_had_reply)
        self.assertEqual(self.view.error_text, '')
        self.view.on_event({'id': 500, 'type': 'session_update', 'data': {'update': {
            'sessionUpdate': 'agent_message_chunk', 'runId': 'new-run',
            'content': {'type': 'text', 'text': 'MeadowOS 聊天連線成功。'}}}})
        self.view.finish('new-run', 501)
        self.assertEqual(len(self.view.presented), 2)
        self.assertEqual(self.view.messages, [('assistant', 'MeadowOS 聊天連線成功。')])

    def test_successful_tool_is_not_presented_as_failure(self):
        event = copy.deepcopy(FAILURE)
        event['data']['update']['status'] = 'completed'
        self.view.on_event(event)
        self.assertEqual(self.view.presented, [])

    def test_real_response_boundaries_render_separate_bubbles_without_fake_messages(self):
        def emit(index, subtype, text=None, identity=None):
            update = {'sessionUpdate': subtype, 'messageId': identity}
            if text is not None:
                update['content'] = {'type': 'text', 'text': text}
            self.view.on_event({'id': index, 'type': 'session_update', 'data': {'update': update}})
        emit(1, 'agent_message_start', identity='ack')
        emit(2, 'agent_message_chunk', '我先', 'ack')
        emit(3, 'agent_message_chunk', '查詢。', 'ack')
        emit(4, 'agent_message_end', identity='ack')
        emit(5, 'tool_call')
        emit(6, 'tool_call_update')
        emit(7, 'agent_thought_chunk', 'PRIVATE')
        emit(8, 'agent_message_chunk', '結果', 'result')
        emit(9, 'agent_message_chunk', '好了。', 'result')
        emit(10, 'agent_message_end', identity='result')
        self.assertEqual(self.view.messages, [('assistant', '我先查詢。'), ('assistant', '結果好了。')])
        self.assertEqual(self.view.presented, [])

    def test_snapshot_message_ids_do_not_duplicate_recorded_bubbles_during_replay(self):
        self.view.messages = [('assistant', '我先查詢。')]
        self.view.assistant_messages = module.AssistantMessages([
            {'role': 'assistant', 'content': '我先查詢。', 'messageId': 'ack'}])
        for index, identity, text in ((1, 'ack', '我先查詢。'), (2, 'result', '結果好了。')):
            self.view.on_event({'id': index, 'type': 'session_update', 'data': {'update': {
                'sessionUpdate': 'agent_message_chunk', 'messageId': identity, 'content': {'text': text}}}})
        self.assertEqual(self.view.messages, [('assistant', '我先查詢。'), ('assistant', '結果好了。')])

    def test_real_tool_events_update_progress_without_synthesizing_chat_messages(self):
        self.view.tool_progress = module.ToolProgress(self.path.with_name('progress.json'), 'chat', module.atomic_json)
        self.view.run('current')
        event = copy.deepcopy(FAILURE)
        event['data']['update'].update(sessionUpdate='tool_call', runId='current', status='in_progress')
        self.view.on_event(event)
        self.assertIn('meadow_browser_visual_check · 執行中', self.view.progress_states[-1]['summary'])
        self.assertEqual(self.view.messages, [])
        event['id'] += 1
        event['data']['update'].update(sessionUpdate='tool_call_update', status='failed')
        self.view.on_event(event)
        self.view.finish('current', 420)
        self.assertEqual(self.view.progress_states[-1]['rows'][0]['statusLabel'], '未完成')
        self.view.run('later')
        self.assertTrue(self.view.progress_states[-1]['rows'][0]['previous'])
        self.assertNotIn('未完成', self.view.progress_states[-1]['summary'])
        self.assertEqual(self.view.messages, [])

    def test_runtime_observation_is_not_a_human_bubble_but_real_answer_is_preserved(self):
        self.view.tool_progress = module.ToolProgress(self.path.with_name('progress.json'), 'chat', module.atomic_json)
        self.view.on_event({'id': 1, 'type': 'background_wake_status', 'data': {
            'actor': 'runtime', 'wakeId': 'wake', 'status': 'running'}})
        self.view.on_event({'id': 2, 'type': 'session_update', 'data': {'update': {
            'sessionUpdate': 'user_message_chunk', 'backgroundCompletion': True,
            'content': {'text': 'private runtime observation'}}}})
        self.assertEqual(self.view.messages, [])
        self.assertIn('執行中', self.view.progress_states[-1]['summary'])
        self.view.on_event({'id': 3, 'type': 'session_update', 'data': {'update': {
            'sessionUpdate': 'agent_message_chunk', 'backgroundCompletion': True,
            'content': {'text': '檔案已完成，這是模型原始回覆。'}}}})
        self.assertEqual(self.view.messages, [('assistant', '檔案已完成，這是模型原始回覆。')])

    def test_voice_speaks_only_fresh_successful_raw_reply(self):
        spoken, phases = [], []
        globals_['shell'] = ShellView(
            voiceos=SimpleNamespace(enabled=True, set_state=lambda **state: phases.append(state)),
            voice_transport=SimpleNamespace(speak=spoken.append), voice_show_chat=lambda: None)
        self.view.voice_replay_through = 450
        for event_id, text in ((400, 'historical reply'), (500, '原始回覆，不改寫。')):
            self.view.on_event({'id': event_id, 'type': 'session_update', 'data': {'update': {
                'sessionUpdate': 'agent_message_chunk', 'content': {'text': text}}}})
        self.view.finish('new-run', 501)
        self.assertEqual(spoken, ['原始回覆，不改寫。'])
        self.assertEqual(self.view.voice_reply_text, '')

    def test_failed_and_cancelled_turns_never_speak_partial_promises(self):
        spoken, shown = [], []
        globals_['shell'] = ShellView(
            voiceos=SimpleNamespace(enabled=True, set_state=lambda **state: None),
            voice_transport=SimpleNamespace(speak=spoken.append), voice_show_chat=lambda: shown.append(True))
        for reason in ('loop_detected', 'cancelled', 'time_budget', 'token_budget', 'token_limit', 'empty', 'audit_inconclusive'):
            self.view.voice_reply_text = '我會幫你完成'
            self.view.on_event({'id': 600, 'type': 'turn_completed', 'data': {'stopReason': reason}})
        self.assertEqual(spoken, [])
        self.assertEqual(shown, [True] * 6)

    def test_notice_cache_is_bounded(self):
        for index in range(45):
            self.view.notices.add({'id': index, 'data': {'runId': 'run'}}, 'tool_failed', '字' * 2000)
        self.assertEqual(len(self.view.notices.rows), 40)
        self.assertTrue(all(len(row['text']) <= 1200 for row in self.view.notices.rows))
        self.assertEqual(len(module.PresentationNotices(self.path, 'exact-chat-id').rows), 40)

    def prepare_placement(self):
        globals_['shell'] = ShellView(geometry=SimpleNamespace(x=0, y=0, width=1920), update_windows=lambda: None, voiceos=None)
        globals_['GLib'] = SimpleNamespace(idle_add=lambda callback, *args: callback(*args))
        self.view.last_position = None
        # Actual native CSS frame: allocation622 includes182px shadow;
        # logical content440 is what should sit24px from the right edge.
        self.view.get_allocated_width = lambda: 622
        self.view.get_size = lambda: (440, 620)
        self.view.move = lambda x, y: setattr(self.view, 'position', (x, y))
        self.view.resize = lambda width, height: setattr(self.view, 'size', (width, height))
        self.view.get_position = lambda: self.view.position
        self.view.get_mapped = lambda: True
        self.view.get_window = lambda: None
        self.view.save_draft = self.view.hide = lambda: None
        self.view.input = SimpleNamespace(grab_focus=lambda: None)

    def test_first_map_excludes_shadow_and_reopen_restores_position(self):
        self.prepare_placement()
        self.view.map_chat()
        self.assertEqual(self.view.position, (1456, 56))
        self.view.hide_chat()
        self.view.position = (714, 159)  # WM centers it during the next map.
        self.view.map_chat()
        self.assertEqual(self.view.position, (1456, 56))

    def test_configure_before_placement_idle_does_not_cache_wm_center(self):
        self.prepare_placement()
        callbacks = []
        globals_['GLib'] = SimpleNamespace(idle_add=lambda callback: callbacks.append(callback))
        self.view.position = (700, 265)
        self.view.map_chat()
        # Real GTK rendering can delay DEFAULT_IDLE longer than the 120ms
        # configure timeout. This rectangle is the WM's provisional placement.
        self.view.capture_geometry()
        self.assertIsNone(self.view.last_position)
        self.assertIsNone(self.view.last_size)
        self.assertTrue(self.view.position_restore_pending)
        callbacks.pop()()
        self.assertEqual(self.view.position, (1456, 56))
        self.assertFalse(self.view.position_restore_pending)
        self.view.position = (400, 200)
        self.view.capture_geometry()
        self.assertEqual(self.view.last_position, (400, 200))

    def test_delayed_reopen_restore_keeps_user_position(self):
        self.prepare_placement()
        self.view.map_chat()
        self.view.position = (300, 200)
        self.view.hide_chat()
        callbacks = []
        globals_['GLib'] = SimpleNamespace(idle_add=lambda callback: callbacks.append(callback))
        self.view.position = (700, 265)
        self.view.map_chat()
        self.view.capture_geometry()
        self.assertEqual(self.view.last_position, (300, 200))
        callbacks.pop()()
        self.assertEqual(self.view.position, (300, 200))
        self.assertFalse(self.view.position_restore_pending)

    def test_user_drag_is_saved_before_hide_and_restored_after_map(self):
        self.prepare_placement()
        self.view.map_chat()
        self.view.position = (300, 400)
        self.view.hide_chat()
        self.view.position = (714, 159)
        self.view.map_chat()
        self.assertEqual(self.view.position, (300, 400))

    def test_iconified_visible_chat_restores_instead_of_hiding(self):
        self.prepare_placement()
        globals_['Gtk'] = SimpleNamespace(get_current_event_time=lambda: 0)
        self.view.iconified = True
        self.view.get_visible = lambda: True
        actions = []
        self.view.deiconify = lambda: actions.append('restore')
        self.view.present_with_time = lambda _time: actions.append('present')
        self.view.hide = lambda: actions.append('hide')
        self.view.toggle()
        self.assertEqual(actions, ['restore', 'present'])

    def test_maximized_hide_preserves_normal_geometry(self):
        self.prepare_placement()
        self.view.map_chat()
        self.view.position = (300, 200)
        self.view.remember_normal_geometry()
        self.view.maximized = True
        self.view.position = (0, 32)
        self.view.get_size = lambda: (1920, 972)
        self.view.hide_chat()
        self.assertEqual(self.view.last_position, (300, 200))
        self.assertEqual(self.view.last_size, (440, 620))
        actions = []
        self.view.maximize = lambda: actions.append('maximize')
        self.view.restore_position()
        self.assertEqual(actions, ['maximize'])
        self.view.maximized = self.view.reopen_maximized = False
        self.view.restore_position()
        self.assertEqual(self.view.position, (300, 200))
        self.assertEqual(self.view.size, (440, 620))

    def test_native_iconify_map_does_not_apply_stale_normal_position(self):
        self.prepare_placement()
        self.view.map_chat()
        self.view.position = (400, 250)  # Native WM move, then Alt+F9 iconify.
        self.view.iconified = True
        self.view.map_chat()
        self.assertEqual(self.view.position, (400, 250))

    def test_native_unmaximize_keeps_wm_confirmed_geometry(self):
        self.prepare_placement()
        globals_['Gdk'] = SimpleNamespace(WindowState=SimpleNamespace(MAXIMIZED=1, ICONIFIED=2))
        self.view.last_position = (1456, 56)  # Older Shell-owned hide receipt.
        self.view.last_size = (440, 620)
        self.view.position = (400, 250)  # WM restores a later Alt+F7/F8 rectangle.
        self.view.maximized = True
        self.view.get_visible = lambda: True
        self.view.window_state(None, SimpleNamespace(new_window_state=0))
        self.assertFalse(self.view.maximized)
        self.assertEqual(self.view.position, (400, 250))

    def test_only_maximized_hide_remap_repairs_lost_wm_normal_rect(self):
        self.prepare_placement()
        self.view.map_chat()
        globals_['Gdk'] = SimpleNamespace(WindowState=SimpleNamespace(MAXIMIZED=1, ICONIFIED=2, FULLSCREEN=4))
        self.view.position = (350, 180)
        self.view.remember_normal_geometry()
        self.view.maximized = True
        self.view.position = (0, 32)
        self.view.get_size = lambda: (1920, 972)
        self.view.hide_chat()
        self.view.get_visible = lambda: True
        self.view.window_state(None, SimpleNamespace(new_window_state=1))  # Remap's actual MAX ack.
        self.assertTrue(self.view.remapped_maximized)
        self.view.window_state(None, SimpleNamespace(new_window_state=0))
        self.assertEqual(self.view.position, (350, 180))
        self.assertEqual(self.view.size, (440, 620))
        self.assertIsNone(self.view.maximized_hide_geometry)

    def test_delayed_configure_reads_native_flags_before_accepting_normal_rect(self):
        self.prepare_placement()
        self.view.map_chat()
        globals_['Gdk'] = SimpleNamespace(WindowState=SimpleNamespace(MAXIMIZED=1, ICONIFIED=2, FULLSCREEN=4))
        self.view.position = (400, 250)
        self.view.capture_geometry()
        self.assertEqual(self.view.last_position, (400, 250))
        self.view.position = (0, 32)
        self.view.get_size = lambda: (1920, 972)
        self.view.get_window = lambda: SimpleNamespace(get_state=lambda: 1)
        self.view.capture_geometry()  # Native MAX exists before the UI state callback.
        self.assertEqual(self.view.last_position, (400, 250))
        self.assertEqual(self.view.last_size, (440, 620))

    def test_message_reflow_waits_until_allocation_finishes_and_reads_latest_width(self):
        # A label resize requested during GTK's parent allocation left the
        # restored bubble at its maximized cached width in the native session.
        callbacks, caps = [], []
        def idle(callback):
            callbacks.append(callback)
            return len(callbacks)
        globals_['GLib'] = SimpleNamespace(idle_add=idle)
        allocated = [1884]
        self.view.timeline = SimpleNamespace(get_allocated_width=lambda: allocated[0], get_border_width=lambda: 18)
        self.view.message_reflow_pending = None
        self.view.message_labels = [('user', SimpleNamespace(set_width_limit=lambda value: caps.append(value)))]
        self.view.reflow_messages(None, SimpleNamespace(width=1884))
        allocated[0] = 484  # Normal size arrives before the queued idle runs.
        self.view.reflow_messages(None, SimpleNamespace(width=484))
        self.assertEqual(caps, [])
        self.assertEqual(len(callbacks), 1)
        self.assertFalse(callbacks[0]())
        self.assertEqual(caps, [332])
        self.assertIsNone(self.view.message_reflow_pending)


if __name__ == '__main__':
    unittest.main()
