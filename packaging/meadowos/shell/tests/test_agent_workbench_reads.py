"""GET-only contract checks; no daemon, model or user profile is touched."""
import copy
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import Mock
from urllib.parse import parse_qs, urlsplit


SOURCE = Path(__file__).resolve().parents[1] / "agent_client.py"
SPEC = importlib.util.spec_from_file_location("workbench_agent_client", SOURCE)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
AgentClient, AgentError = MODULE.AgentClient, MODULE.AgentError


class WorkbenchReadTests(unittest.TestCase):
    def setUp(self):
        self.client = AgentClient(workspace="/tmp/工作 files & notes", state_root="/tmp/unused-agent")
        self.client.session = {"sessionId": "live/id ?", "chatId": "saved/chat #"}
        self.plan = {"ok": True, "sessionId": "live/id ?", "plan": {
            "goal": "整理會議", "revision": 2,
            "items": [{"id": "notes", "content": "保存原始決策", "status": "pending",
                       "artifacts": ["Meetings/會議.md"], "evidence_ids": []}]},
            "run": {"running": False, "stopReason": "error"}, "evidence": []}
        self.history = {"files": ["Meetings/會議.md", "images/diagram.svg"],
                        "messages": [{"role": "assistant", "text": "原文"}], "tools": []}

    def error_code(self, method, code):
        with self.assertRaises(AgentError) as raised:
            method()
        self.assertEqual(raised.exception.code, code)

    def test_task_plan_is_one_get_and_preserves_model_fields(self):
        original = copy.deepcopy(self.plan)
        self.client._request = Mock(return_value=self.plan)
        self.assertIs(self.client.task_plan(), self.plan)
        self.client._request.assert_called_once_with("GET", "/session/live%2Fid%20%3F/task-plan")
        self.assertEqual(self.plan, original)

    def test_history_binds_durable_chat_and_literal_workspace(self):
        self.client._request = Mock(return_value=self.history)
        self.assertIs(self.client.workspace_history(), self.history)
        self.client._request.assert_called_once()
        args = self.client._request.call_args.args
        self.assertEqual(args[0], "GET")
        self.assertEqual(len(args), 2)
        parsed = urlsplit(args[1])
        self.assertEqual(parsed.path, "/sessions/saved%2Fchat%20%23/history")
        self.assertEqual(parse_qs(parsed.query), {"cwd": [str(self.client.workspace)]})

    def test_history_falls_back_to_live_id_without_a_chat_id(self):
        self.client.session.pop("chatId")
        self.client._request = Mock(return_value=self.history)
        self.client.workspace_history()
        self.assertTrue(self.client._request.call_args.args[1].startswith("/sessions/live%2Fid%20%3F/history?"))

    def test_no_session_never_sends_or_creates_a_request(self):
        self.client.session = None
        self.client._request = Mock(side_effect=AssertionError("must not request"))
        self.error_code(self.client.task_plan, "no_session")
        self.error_code(self.client.workspace_history, "no_session")
        self.client._request.assert_not_called()

    def test_task_plan_rejects_wrong_session_and_invalid_shape(self):
        invalid = [
            {**self.plan, "sessionId": "another-chat"},
            {**self.plan, "ok": False},
            {**self.plan, "plan": {"items": {}}},
            {**self.plan, "plan": {"items": ["fake plan"]}},
        ]
        for value in invalid:
            with self.subTest(value=value):
                self.client._request = Mock(return_value=value)
                self.error_code(self.client.task_plan, "invalid_response")

    def test_history_rejects_invalid_ledger_without_silently_filtering_it(self):
        invalid = [None, "not an array", [None], [""], ["bad\x00path"]]
        for files in invalid:
            with self.subTest(files=files):
                self.client._request = Mock(return_value={**self.history, "files": files})
                self.error_code(self.client.workspace_history, "invalid_response")
        for key in ("messages", "tools"):
            with self.subTest(key=key):
                self.client._request = Mock(return_value={**self.history, key: None})
                self.error_code(self.client.workspace_history, "invalid_response")

    def test_empty_authoritative_snapshots_are_allowed(self):
        self.client._request = Mock(return_value={**self.plan, "plan": {"items": []}})
        self.assertEqual(self.client.task_plan()["plan"]["items"], [])
        self.client._request = Mock(return_value={"files": [], "messages": [], "tools": []})
        self.assertEqual(self.client.workspace_history()["files"], [])

    def test_http_errors_are_preserved_and_never_retried(self):
        for method in (self.client.task_plan, self.client.workspace_history):
            with self.subTest(method=method.__name__):
                original = AgentError("missing", status=404, code="http_error")
                self.client._request = Mock(side_effect=original)
                with self.assertRaises(AgentError) as raised:
                    method()
                self.assertIs(raised.exception, original)
                self.client._request.assert_called_once()

    def test_inflight_task_plan_cannot_cross_a_session_switch(self):
        def request(*_args):
            self.client.session = {"sessionId": "new-live", "chatId": "new-chat"}
            return self.plan
        self.client._request = Mock(side_effect=request)
        self.error_code(self.client.task_plan, "session_changed")

    def test_inflight_task_plan_cannot_cross_a_workspace_change(self):
        def request(*_args):
            self.client.workspace = Path("/tmp/different-work")
            return self.plan
        self.client._request = Mock(side_effect=request)
        self.error_code(self.client.task_plan, "session_changed")

    def test_inflight_history_cannot_cross_chat_or_workspace_changes(self):
        for change in ("session", "chat", "workspace"):
            with self.subTest(change=change):
                self.setUp()
                def request(*_args):
                    if change == "workspace":
                        self.client.workspace = Path("/tmp/different-work")
                    elif change == "chat":
                        self.client.session["chatId"] = "different-chat"
                    else:
                        self.client.session = None
                    return self.history
                self.client._request = Mock(side_effect=request)
                self.error_code(self.client.workspace_history, "session_changed")


if __name__ == "__main__":
    unittest.main()
