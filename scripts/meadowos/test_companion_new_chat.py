"""Exercise actual Companion handlers without GTK, network, or a live Agent."""
import ast
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

SOURCE = Path(__file__).resolve().parents[2] / "packaging/meadowos/shell/meadow_shell.py"
TREE = ast.parse(SOURCE.read_text())
COMPANION = next(node for node in ast.walk(TREE)
                 if isinstance(node, ast.ClassDef) and node.name == "Companion")


def extract_handler(name, namespace):
    node = next(node for node in COMPANION.body
                if isinstance(node, ast.FunctionDef) and node.name == name)
    exec(compile(ast.Module(body=[node], type_ignores=[]), str(SOURCE), "exec"), namespace)
    return namespace[name]


class Widget:
    def __init__(self, text=""):
        self.text, self.sensitive, self.visible = text, None, None
        self.style = SimpleNamespace(add_class=Mock(), remove_class=Mock())

    def set_sensitive(self, value):
        self.sensitive = value

    def set_visible(self, value):
        self.visible = value

    def set_text(self, value):
        self.text = value

    def get_text(self):
        return self.text

    def set_tooltip_text(self, value):
        self.tooltip = value

    def get_style_context(self):
        return self.style


class CompanionNewChatTests(unittest.TestCase):
    def setUp(self):
        self.threads = []
        fixture = self

        class ImmediateThread:
            def __init__(self, *, target, **options):
                self.target = target
                fixture.threads.append(options)

            def start(self):
                self.target()

        self.next_client = Mock()
        self.next_client.last_event_id = 7
        self.next_client.load_messages.return_value = []
        self.next_client.status.return_value = {"connected": True, "model": {"ready": True}}
        self.next_client.image_capability.return_value = False
        self.next_client.events.return_value = []
        self.factory = Mock(return_value=self.next_client)
        self.shell = SimpleNamespace(update_chat_button=Mock())
        namespace = {"AgentClient": self.factory, "AgentError": RuntimeError,
                     "threading": SimpleNamespace(Thread=ImmediateThread), "shell": self.shell}
        method = extract_handler("connect_agent", namespace)
        self.view = SimpleNamespace(sending=False, connecting=False, running=True,
            generation=3, client=Mock(), connected=True, model_ready=True,
            status_label=Widget(), retry=Widget(), new_chat=Widget(),
            attachments=SimpleNamespace(cache=object()), update_controls=Mock(),
            dispatch=Mock(), accept_connection=Mock(), on_event=Mock(), disconnected=Mock())
        self.previous = self.view.client
        self.buffer = Widget("保留尚未送出的草稿\n第二行")
        self.view.input = SimpleNamespace(get_buffer=lambda: self.buffer)
        self.connect = lambda **kwargs: method(self.view, **kwargs)

    def test_new_chat_passes_explicit_flag_and_only_disconnects_old_presentation(self):
        draft = self.buffer.text
        self.connect(new_chat=True)
        self.factory.assert_called_once_with(attachment_cache=self.view.attachments.cache)
        self.next_client.connect.assert_called_once_with()
        self.next_client.open_conversation.assert_called_once_with(new_chat=True)
        self.previous.close.assert_called_once_with()
        self.previous.cancel.assert_not_called()
        self.previous.send.assert_not_called()
        self.next_client.cancel.assert_not_called()
        self.next_client.send.assert_not_called()
        self.assertEqual(self.buffer.text, draft)
        self.assertTrue(self.view.running, "opening a chat must not cancel old Agent work")
        self.assertEqual(self.view.generation, 4)
        self.view.dispatch.assert_called_once()
        self.assertEqual(self.view.dispatch.call_args.args[:2], (4, self.view.accept_connection))
        self.assertEqual(self.view.dispatch.call_args.args[-1], 7)

    def test_reconnect_preserves_default_resume_behavior(self):
        self.connect()
        self.next_client.open_conversation.assert_called_once_with(new_chat=False)
        self.previous.close.assert_called_once_with()
        self.previous.cancel.assert_not_called()

    def test_inflight_submission_or_connection_cannot_be_discarded(self):
        for sending, connecting in [(True, False), (False, True), (True, True)]:
            with self.subTest(sending=sending, connecting=connecting):
                self.setUp()
                self.view.sending, self.view.connecting = sending, connecting
                self.connect(new_chat=True)
                self.factory.assert_not_called()
                self.previous.close.assert_not_called()
                self.previous.cancel.assert_not_called()
                self.assertEqual(self.threads, [])
                self.assertEqual(self.view.generation, 3)
                self.assertEqual(self.buffer.text, "保留尚未送出的草稿\n第二行")

    def test_new_chat_sensitivity_depends_on_pending_client_operations(self):
        method = extract_handler("update_controls", {"shell": self.shell})
        for connecting in [False, True]:
            for sending in [False, True]:
                for running in [False, True]:
                    view = SimpleNamespace(connecting=connecting, sending=sending,
                        running=running, connected=True, model_ready=True,
                        retry=Widget(), new_chat=Widget(), send=Widget(), stop=Widget(),
                        status_label=Widget(), header_mark=Widget(), status_dot=Widget())
                    method(view)
                    self.assertEqual(view.new_chat.sensitive, not connecting and not sending)

    def test_native_menu_label_and_callback_are_wired(self):
        assignments = [node for node in ast.walk(COMPANION) if isinstance(node, ast.Assign)
                       and any(isinstance(target, ast.Attribute) and target.attr == "new_chat"
                               for target in node.targets)]
        self.assertTrue(any(isinstance(node.value, ast.Call)
            and isinstance(node.value.func, ast.Attribute) and node.value.func.attr == "MenuItem"
            and any(item.arg == "label" and isinstance(item.value, ast.Constant)
                    and item.value.value == "新對話" for item in node.value.keywords)
            for node in assignments))
        connections = [node for node in ast.walk(COMPANION) if isinstance(node, ast.Call)
                       and isinstance(node.func, ast.Attribute) and node.func.attr == "connect"
                       and isinstance(node.func.value, ast.Attribute)
                       and node.func.value.attr == "new_chat"]
        self.assertTrue(any(len(node.args) >= 2 and isinstance(node.args[0], ast.Constant)
            and node.args[0].value == "activate" and any(isinstance(child, ast.Call)
                and isinstance(child.func, ast.Attribute) and child.func.attr == "connect_agent"
                and any(keyword.arg == "new_chat" and isinstance(keyword.value, ast.Constant)
                        and keyword.value.value is True for keyword in child.keywords)
                for child in ast.walk(node.args[1])) for node in connections))
        self.assertTrue(any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
            and node.func.attr == "append" and any(isinstance(arg, ast.Attribute)
                and arg.attr == "new_chat" for arg in node.args) for node in ast.walk(COMPANION)))


if __name__ == "__main__":
    unittest.main()
