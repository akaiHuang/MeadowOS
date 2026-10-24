"""Observed response boundaries, snapshot replay and public-text preservation."""
import ast
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from chat_stream import AssistantMessages


def event(index, subtype, text=None, identity=None, **extra):
    update = {"sessionUpdate": subtype, **extra}
    if text is not None:
        update["content"] = {"type": "text", "text": text}
    if identity is not None:
        update["messageId"] = identity
    return {"id": index, "type": "session_update", "data": {"update": update}}


class AssistantMessagesTests(unittest.TestCase):
    def test_ack_tools_and_result_are_two_exact_public_messages(self):
        stream = AssistantMessages()
        displayed = {}
        events = [
            event(1, "agent_message_start", identity="run:assistant:1"),
            event(2, "agent_message_chunk", "我先查詢", "run:assistant:1"),
            event(3, "agent_message_chunk", "這台電腦。", "run:assistant:1"),
            event(4, "agent_message_end", identity="run:assistant:1"),
            event(5, "tool_call", title="system_info"),
            event(6, "tool_call_update", status="completed", rawOutput="private tool data"),
            event(7, "agent_message_start", identity="run:assistant:2"),
            event(8, "agent_message_chunk", "查到了：", "run:assistant:2"),
            event(9, "agent_message_chunk", "Ubuntu，ARM64。", "run:assistant:2"),
            event(10, "agent_message_end", identity="run:assistant:2"),
        ]
        creates = 0
        for item in events:
            change = stream.consume(item)
            if change:
                creates += change.create
                displayed[change.key] = change.text
        self.assertEqual(creates, 2)
        self.assertEqual(list(displayed.values()), ["我先查詢這台電腦。", "查到了：Ubuntu，ARM64。"])

    def test_changed_id_separates_without_start_or_tool_events(self):
        stream = AssistantMessages()
        first = stream.consume(event(1, "agent_message_chunk", "一", "a"))
        second = stream.consume(event(2, "agent_message_chunk", "二", "b"))
        self.assertTrue(first.create and second.create)
        self.assertNotEqual(first.key, second.key)
        self.assertEqual(second.text, "二")

    def test_empty_start_end_and_tool_only_responses_never_create_bubbles(self):
        stream = AssistantMessages()
        for index, subtype in enumerate(("agent_message_start", "tool_call", "tool_call_update", "agent_message_end"), 1):
            self.assertIsNone(stream.consume(event(index, subtype, identity="a")))
        self.assertEqual(stream.texts, {})

    def test_foreign_end_cannot_close_current_response(self):
        stream = AssistantMessages()
        stream.consume(event(1, "agent_message_chunk", "first", "a"))
        stream.consume(event(2, "agent_message_end", identity="old"))
        change = stream.consume(event(3, "agent_message_chunk", " second", "a"))
        self.assertFalse(change.create)
        self.assertEqual(change.text, "first second")

    def test_private_thought_and_runtime_observation_are_never_chat_text(self):
        stream = AssistantMessages()
        for index, subtype in enumerate(("agent_thought_chunk", "user_message_chunk", "tool_call_update"), 1):
            self.assertIsNone(stream.consume(event(index, subtype, "PRIVATE", "a", backgroundCompletion=True)))
        self.assertEqual(stream.texts, {})

    def test_snapshot_complete_rows_skip_replay_and_inflight_row_reassembles(self):
        snapshot = [{"role": "user", "content": "查電腦"},
                    {"role": "assistant", "content": "我先查詢。", "messageId": "ack"}]
        stream = AssistantMessages(snapshot, replay_after=10)
        for item in (event(11, "agent_message_start", identity="ack"),
                     event(12, "agent_message_chunk", "我先", "ack"),
                     event(13, "agent_message_chunk", "查詢。", "ack"),
                     event(14, "agent_message_end", identity="ack")):
            self.assertIsNone(stream.consume(item))
        first = stream.consume(event(15, "agent_message_chunk", "規格", "answer"))
        later = stream.consume(event(16, "agent_message_chunk", "在這裡。", "answer"))
        self.assertTrue(first.create)
        self.assertFalse(later.create)
        self.assertEqual(later.text, "規格在這裡。")
        self.assertEqual(len(stream.texts), 1)

    def test_replayed_event_ids_and_sealed_response_never_duplicate_text(self):
        stream = AssistantMessages()
        chunk = event(1, "agent_message_chunk", "Hello", "a")
        first = stream.consume(chunk)
        self.assertTrue(first.create)
        self.assertIsNone(stream.consume(chunk))
        stream.consume(event(2, "agent_message_end", identity="a"))
        self.assertIsNone(stream.consume(event(3, "agent_message_chunk", "late", "a")))
        self.assertEqual(first.text, "Hello")
        self.assertEqual(stream.texts, {})
        self.assertIn('a', stream.closed)

    def test_identical_text_with_distinct_ids_stays_distinct(self):
        stream = AssistantMessages([{"role": "assistant", "content": "好", "messageId": "old"}])
        self.assertTrue(stream.consume(event(1, "agent_message_chunk", "好", "new")).create)

    def test_old_journal_without_ids_is_not_semantically_deduplicated(self):
        stream = AssistantMessages([{"role": "assistant", "content": "好"}])
        self.assertTrue(stream.consume(event(1, "agent_message_chunk", "好")).create)

    def test_legacy_stream_stays_compatible_and_new_run_never_reuses_its_bubble(self):
        stream = AssistantMessages()
        first = stream.consume(event(1, "agent_message_chunk", "a"))
        second = stream.consume(event(2, "agent_message_chunk", "b"))
        self.assertEqual((first.key, second.text, second.create), (second.key, "ab", False))
        stream.consume({"id": 3, "type": "turn_completed", "data": {"stopReason": "end_turn"}})
        third = stream.consume(event(4, "agent_message_chunk", "c"))
        self.assertTrue(third.create)
        self.assertNotEqual(first.key, third.key)
        stream.consume({"id": 5, "type": "run_state", "data": {"status": "running", "runId": "next"}})
        self.assertTrue(stream.consume(event(6, "agent_message_chunk", "d")).create)

    def test_new_reply_after_background_wake_uses_its_real_identity(self):
        stream = AssistantMessages()
        stream.consume(event(1, "agent_message_chunk", "我會稍後回覆。", "first"))
        stream.consume({"id": 2, "type": "turn_completed", "data": {"stopReason": "end_turn"}})
        stream.consume({"id": 3, "type": "background_wake", "data": {"actor": "runtime"}})
        reply = stream.consume(event(4, "agent_message_chunk", "結果好了。", "wake-reply", backgroundCompletion=True))
        self.assertTrue(reply.create)
        self.assertEqual(reply.text, "結果好了。")

    def test_worker_cannot_advance_the_resume_cursor_before_gtk_accepts_it(self):
        source = Path(__file__).resolve().parents[1] / 'meadow_shell.py'
        tree = ast.parse(source.read_text())
        companion = next(node for node in ast.walk(tree) if isinstance(node, ast.ClassDef) and node.name == 'Companion')
        connect = next(node for node in companion.body if isinstance(node, ast.FunctionDef) and node.name == 'connect_agent')
        callbacks, received = [], []
        client = SimpleNamespace(last_event_id=10, connect=lambda: None, open_conversation=lambda **_: None,
                                 load_messages=lambda: [], status=lambda: {}, image_capability=lambda: None)

        def events():
            for index, text in ((11, '規格'), (12, '已讀取。')):
                client.last_event_id = index
                yield event(index, 'agent_message_chunk', text, 'inflight')
        client.events = events
        namespace = {'AgentClient': lambda **_: client, 'AgentError': RuntimeError,
                     'threading': SimpleNamespace(Thread=lambda *, target, **_: SimpleNamespace(start=target))}
        exec(compile(ast.Module(body=[connect], type_ignores=[]), str(source), 'exec'), namespace)
        view = SimpleNamespace(sending=False, connecting=False, generation=0, client=None,
                               status_label=SimpleNamespace(set_text=lambda _: None),
                               retry=SimpleNamespace(set_sensitive=lambda _: None),
                               new_chat=SimpleNamespace(set_sensitive=lambda _: None),
                               attachments=SimpleNamespace(cache=None), update_controls=lambda: None)
        view.dispatch = lambda generation, fn, *args: callbacks.append((fn, args))
        view.disconnected = lambda error: self.fail(error)
        view.accept_connection = lambda client, messages, status, cursor: setattr(view, 'stream', AssistantMessages(messages, replay_after=cursor))
        view.on_event = lambda item: received.append(view.stream.consume(item))
        namespace['connect_agent'](view)
        self.assertEqual(client.last_event_id, 12)
        for fn, args in callbacks:
            fn(*args)
        self.assertEqual([change.text for change in received], ['規格', '規格已讀取。'])


if __name__ == "__main__":
    unittest.main()
