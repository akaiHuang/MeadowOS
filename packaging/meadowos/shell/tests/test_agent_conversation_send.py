"""Offline conversation inbox admission contract; no live Agent or drafts."""
import ast
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import uuid

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from agent_client import AgentClient, AgentError


class ConversationSendTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='meadow-send-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name).resolve()
        self.client = AgentClient(workspace=root / 'workspace', state_root=root / 'agent',
                                  persistence_path=root / 'state/conversation.json')
        self.client.session = {'sessionId': 'live-chat', 'chatId': 'saved-chat'}
        self.client._initial_empty = True
        self.client._request = Mock(return_value={})

    def test_explicit_logical_send_id_and_original_text_reach_queue_unchanged(self):
        identity = str(uuid.uuid4())
        text = '  停止只是原文，不是控制指令。\nPlease keep chatting.  '
        receipt = {'requestId': identity, 'messageId': 'message-1', 'disposition': 'accepted',
                   'delivery': 'conversation', 'duplicate': False, 'items': []}
        self.client._request.return_value = receipt
        self.assertIs(self.client.send(text, request_id=identity), receipt)
        self.client._request.assert_called_once_with('POST', '/session/live-chat/queue',
            {'prompt': [{'type': 'text', 'text': text}], 'options': {'mode': 'auto'}},
            headers={'X-Meadow-Prompt-Request-ID': identity})
        self.assertFalse(json.loads(self.client.persistence_path.read_text())['initialEmpty'])

    def test_explicit_same_message_identity_is_retained_for_deliberate_recovery(self):
        identity = str(uuid.uuid4())
        first = {'requestId': identity, 'messageId': 'same-message', 'disposition': 'accepted',
                 'delivery': 'conversation', 'duplicate': False, 'items': []}
        duplicate = {**first, 'duplicate': True}
        self.client._request.side_effect = [first, duplicate]
        self.assertIs(self.client.send('original', request_id=identity), first)
        self.assertIs(self.client.send('original', request_id=identity), duplicate)
        calls = self.client._request.call_args_list
        self.assertEqual(calls[0], calls[1])

    def test_default_sends_generate_one_distinct_uuid_each(self):
        identities = [uuid.uuid4(), uuid.uuid4()]
        with patch('agent_client.uuid.uuid4', side_effect=identities) as generate:
            self.client.send('same text')
            self.client.send('same text')
        self.assertEqual(generate.call_count, 2)
        self.assertEqual([call.kwargs['headers']['X-Meadow-Prompt-Request-ID']
                          for call in self.client._request.call_args_list], list(map(str, identities)))

    def test_unknown_admission_is_not_retried_and_is_not_reclassified_empty(self):
        self.client._request.side_effect = AgentError('unknown receipt', code='response_interrupted')
        with self.assertRaises(AgentError) as caught:
            self.client.send('exact draft', request_id=str(uuid.uuid4()))
        self.assertEqual(caught.exception.code, 'response_interrupted')
        self.assertEqual(self.client._request.call_count, 1)
        self.assertFalse(json.loads(self.client.persistence_path.read_text())['initialEmpty'])

    def test_invalid_identity_has_no_network_or_persistence_side_effect(self):
        for identity in ('', 'not-a-uuid', '\r\nAuthorization: injected', 1):
            with self.subTest(identity=identity), self.assertRaises(AgentError) as caught:
                self.client.send('text', request_id=identity)
            self.assertEqual(caught.exception.code, 'invalid_request_id')
        self.client._request.assert_not_called()
        self.assertFalse(self.client.persistence_path.exists())

    def test_legacy_receipt_is_still_returned_without_becoming_an_answer(self):
        legacy = {'item': {'id': 'old-queue', 'status': 'pending'}, 'items': []}
        self.client._request.return_value = legacy
        self.assertIs(self.client.send('question'), legacy)


class ConversationReceiptUITests(unittest.TestCase):
    """Run actual Shell methods with fake widgets and recorded HTTP receipts."""
    def setUp(self):
        source = ROOT / 'meadow_shell.py'
        tree = ast.parse(source.read_text())
        companion = next(node for node in ast.walk(tree) if isinstance(node, ast.ClassDef) and node.name == 'Companion')
        names = {'submit_text', 'update_controls'}
        methods = [node for node in companion.body if isinstance(node, ast.FunctionDef) and node.name in names]
        namespace = {'uuid': uuid, 'shell': SimpleNamespace(update_chat_button=lambda: None)}
        exec(compile(ast.Module(body=methods, type_ignores=[]), str(source), 'exec'), namespace)
        subject = type('CompanionReceiptHarness', (), {name: namespace[name] for name in names})
        self.view = subject()
        self.callbacks, self.messages, self.draft_text = [], [], 'question'
        self.send_enabled = False
        self.view.send = SimpleNamespace(get_sensitive=lambda: self.send_enabled,
            set_sensitive=lambda value: setattr(self, 'send_enabled', value))
        self.view.stop = SimpleNamespace(set_sensitive=lambda _: None, set_visible=lambda _: None)
        self.view.retry = SimpleNamespace(set_sensitive=lambda _: None)
        self.status_text = ''
        self.view.status_label = SimpleNamespace(
            set_text=lambda text: setattr(self, 'status_text', text),
            get_text=lambda: self.status_text)
        self.view.header_mark = SimpleNamespace(set_tooltip_text=lambda _: None)
        self.view.attachments = SimpleNamespace(pending=0, items=[],
            snapshot=lambda text: ([], str(uuid.uuid4())), admitted=lambda _: None)
        self.view.status_dot = SimpleNamespace(get_style_context=lambda: SimpleNamespace(add_class=lambda _: None, remove_class=lambda _: None))
        self.view.connected = self.view.model_ready = self.view.running = True
        self.view.connecting = self.view.sending = False
        self.view.error = SimpleNamespace(set_text=lambda _: None)
        self.view.draft = lambda: self.draft_text
        self.view.input = SimpleNamespace(get_buffer=lambda: SimpleNamespace(set_text=lambda text: setattr(self, 'draft_text', text)))
        self.view.editing = SimpleNamespace(reset=lambda: None)
        self.view.save_draft = self.view.follow_bottom = lambda **_: None
        self.view.append_message = lambda *message: self.messages.append(message)
        self.view.request = lambda *callbacks: self.callbacks.append(callbacks)
        self.view.voice_unsent = []
        self.view.update_controls()

    def test_conversation_receipt_keeps_busy_chat_open_without_local_user_echo(self):
        self.assertTrue(self.send_enabled)
        self.view.submit_text('question', preserve_draft=False)
        self.assertFalse(self.send_enabled)  # Only while HTTP admission is pending.
        self.callbacks[0][1]({'requestId': str(uuid.uuid4()), 'messageId': 'message-1',
                             'disposition': 'accepted', 'delivery': 'conversation',
                             'duplicate': False, 'items': []})
        self.assertEqual(self.draft_text, '')
        self.assertTrue(self.view.running)
        self.assertTrue(self.send_enabled)
        self.assertEqual(self.messages, [])  # The server's user echo owns that bubble.
        self.view.submit_text('another question', preserve_draft=False)
        self.assertEqual(len(self.callbacks), 2)

    def test_duplicate_receipt_never_adds_a_local_bubble_or_clears_a_new_draft(self):
        self.view.submit_text('question', preserve_draft=False)
        self.draft_text = 'next draft typed during admission'
        self.callbacks[0][1]({'requestId': str(uuid.uuid4()), 'messageId': 'message-1',
                             'disposition': 'accepted', 'delivery': 'conversation',
                             'duplicate': True, 'items': []})
        self.assertEqual(self.draft_text, 'next draft typed during admission')
        self.assertEqual(self.messages, [])
        self.assertTrue(self.send_enabled)


if __name__ == '__main__':
    unittest.main()
