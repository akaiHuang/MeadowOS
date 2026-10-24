#!/usr/bin/env python3
"""Offline Unix HTTP/SSE contract tests; NOT model or installed Agent acceptance."""
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import socketserver
import struct
import tempfile
import threading
import unittest
from unittest import mock
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("agent_client", ROOT / "packaging/meadowos/shell/agent_client.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
AgentClient, AgentError = MODULE.AgentClient, MODULE.AgentError
TOKEN = "a" * 43


class UnixServer(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True


class AgentFixture:
    def __init__(self, root):
        self.root, self.calls = root, []
        self.responses, self.streams = {}, []
        self.hold_stream, self.stream_started, self.release_stream = False, threading.Event(), threading.Event()
        self.hold_headers = False
        self.events = []
        self.model = {"ready": True, "status": {"model": {"id": "configured-model"}}}
        self.profile = root / "agent"
        (self.profile / "run").mkdir(parents=True, mode=0o700)
        self.helper = root / "meadow-agentd"
        self.helper.write_bytes(b"offline installed-executable fixture; not an Agent")
        self.helper.chmod(0o700)
        token = self.profile / "run/agent-api-token"
        token.write_text(TOKEN + "\n")
        token.chmod(0o600)
        self.identity = {"ok": True, "pid": os.getpid(), "vm_lab": True, "package_mode": "portable", "state_root_fingerprint": "sha256:" + hashlib.sha256(str(self.profile).encode()).hexdigest(), "binary_fingerprint": "sha256:" + hashlib.sha256(self.helper.read_bytes()).hexdigest()}
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_GET(self):
                self.handle_request()

            def do_POST(self):
                self.handle_request()

            def handle_request(self):
                length = int(self.headers.get("Content-Length", 0))
                body = json.loads(self.rfile.read(length)) if length else None
                fixture.calls.append((self.command, self.path, body, dict(self.headers)))
                if self.headers.get("Authorization") != "Bearer " + TOKEN:
                    self.reply(401, {"error": "unauthorized"})
                    return
                if self.path.endswith("/events"):
                    if fixture.hold_stream:
                        if fixture.hold_headers:
                            fixture.stream_started.set()
                            fixture.release_stream.wait(5)
                            self.close_connection = True
                            return
                        self.send_response(200)
                        self.send_header("Content-Type", "text/event-stream")
                        self.send_header("Connection", "close")
                        self.end_headers()
                        self.wfile.write(b": heartbeat\n\n")
                        self.wfile.flush()
                        fixture.stream_started.set()
                        fixture.release_stream.wait(5)
                        self.close_connection = True
                        return
                    rows = fixture.streams.pop(0) if fixture.streams else fixture.events
                    if isinstance(rows, bytes):
                        data = rows
                    else:
                        data = b": heartbeat\n\n" + b"".join(b"id: " + str(row["id"]).encode() + b"\nevent: " + row["type"].encode() + b"\ndata: " + json.dumps(row, ensure_ascii=False).encode() + b"\n\n" for row in rows)
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Content-Length", str(len(data)))
                    self.end_headers()
                    self.wfile.write(data)
                    return
                override = fixture.responses.get((self.command, self.path))
                if override:
                    self.reply(*override)
                elif self.path == "/transport/identity":
                    self.reply(200, fixture.identity)
                elif self.path == "/agent/model":
                    self.reply(200 if fixture.model["ready"] else 503, fixture.model)
                elif self.path == "/session":
                    self.reply(200, {"sessionId": "live-a", "modelId": "configured-model"})
                elif self.path.startswith("/sessions?cwd="):
                    self.reply(200, {"sessions": []})
                elif self.path.endswith("/queue") and self.command == "GET":
                    self.reply(200, {"sessionId": "live-a", "items": [], "paused": False})
                elif self.path.endswith("/tasks"):
                    self.reply(200, {"sessionId": "live-a", "items": [{"id": "old-loop", "status": "loop_detected"}]})
                elif self.path.endswith("/context"):
                    self.reply(200, {"sessionId": "live-a", "running": True})
                elif self.path.endswith("/harness-controls"):
                    self.reply(200, {"sessionId": "live-a", "running": True, "activeMode": "kernel"})
                elif self.path.endswith("/queue"):
                    self.reply(200, {"item": {"id": "queued-a", "status": "pending", "text": body["prompt"][0]["text"]}, "items": []})
                elif self.path == "/session/resume":
                    self.reply(200, {"sessionId": "live-resumed", "chatId": body["chatId"], "running": True, "messages": [{"role": "user", "content": "original question"}, {"role": "assistant", "content": "stored answer"}], "replayAfterEventId": 12, "lastEventId": 18})
                elif "/user-input/" in self.path:
                    # Match Go serve.handleUserInput's map[string]any contract:
                    # a JSON array is rejected by its decoder, not accepted as
                    # a flexible fixture payload.
                    if not isinstance(body.get("answers"), dict):
                        self.reply(400, {"error": "answers object required"})
                    else:
                        self.reply(200, {"ok": True})
                elif "/permission/" in self.path or self.path.endswith("/cancel"):
                    self.reply(200, None if "/permission/" in self.path else {"ok": True})
                else:
                    self.reply(404, {"error": "unknown route"})

            def reply(self, status, body):
                data = b"" if body is None else json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                if data:
                    self.wfile.write(data)

        self.socket_path = self.profile / "run/agent.sock"
        self.server = UnixServer(str(self.socket_path), Handler)
        self.socket_path.chmod(0o600)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.release_stream.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


class AgentClientTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mos-client-", dir="/tmp")
        self.root = Path(self.temporary.name).resolve()
        self.fixture = AgentFixture(self.root)
        self.clients = []
        self.addCleanup(self.cleanup)

    def cleanup(self):
        for client in self.clients:
            client.close()
        self.fixture.close()
        self.temporary.cleanup()

    def client(self):
        client = AgentClient(workspace=self.root / "work", state_root=self.fixture.profile, helper_path=self.fixture.helper, persistence_path=self.root / "state/conversation.json", timeout=2)
        self.clients.append(client)
        return client

    def connected(self):
        client = self.client()
        client.connect()
        client.open_conversation()
        return client

    def test_authenticated_same_process_identity_and_unconfigured_model_are_distinct(self):
        self.fixture.model = {"ready": False, "error": "configure a model connection"}
        client = self.client()
        status = client.connect()
        self.assertTrue(status["connected"])
        self.assertFalse(status["model"]["ready"])
        self.assertEqual(status["model"]["error"], "configure a model connection")
        self.assertIsNone(status["session"])
        self.assertEqual(self.fixture.calls[0][1], "/transport/identity")
        self.assertTrue(all(call[3]["Authorization"] == "Bearer " + TOKEN for call in self.fixture.calls))

    def test_explicit_new_chat_bypasses_saved_resume_and_keeps_old_agent_work(self):
        first = self.connected()
        first.send("Keep this submitted old conversation and its background work")
        old_preference = json.loads(first.persistence_path.read_text())
        self.assertFalse(old_preference["initialEmpty"])
        old_history = self.root / "agent-history-fixture.json"
        old_history.write_text(json.dumps({"chatId": "live-a", "messages": ["submitted old message"]}))
        history_before = old_history.read_bytes()
        self.fixture.responses[("GET", "/session/live-a/queue")] = (200, {"items": [{"id": "old-background-job", "status": "running"}]})
        self.fixture.responses[("POST", "/session")] = (200, {"sessionId": "explicit-new", "modelId": "mercury-3", "connectionId": "inception-new"})
        second = self.client()
        second.connect()
        boundary = len(self.fixture.calls)
        session = second.open_conversation(new_chat=True)
        self.assertEqual(session["sessionId"], "explicit-new")
        self.assertEqual(session["modelId"], "mercury-3")
        self.assertEqual([(method, path) for method, path, _, _ in self.fixture.calls[boundary:]], [("POST", "/session")])
        saved = json.loads(second.persistence_path.read_text())
        self.assertEqual(saved["chatId"], "explicit-new")
        self.assertTrue(saved["initialEmpty"])
        self.assertNotIn("previousEmptyChatId", saved)
        self.assertEqual(old_history.read_bytes(), history_before)
        self.assertEqual(first._request("GET", "/session/live-a/queue")["items"][0]["id"], "old-background-job")
        self.assertFalse(any(method == "DELETE" or (method == "POST" and path.endswith(("/cancel", "/end")))
                             for method, path, _, _ in self.fixture.calls))

    def test_explicit_new_chat_ignores_invalid_saved_preference_and_validates_arguments(self):
        client = self.client()
        client.connect()
        client.persistence_path.parent.mkdir(mode=0o700, parents=True)
        client.persistence_path.write_text("invalid previous presentation preference")
        client.persistence_path.chmod(0o600)
        with self.assertRaises(AgentError):
            client.open_conversation("existing-chat", new_chat=True)
        self.assertFalse(any(call[1] == "/session" for call in self.fixture.calls))
        self.assertEqual(client.open_conversation(new_chat=True)["sessionId"], "live-a")

    def test_ordinary_send_uses_queue_preserving_exact_text_and_never_implies_completion(self):
        client = self.connected()
        for text in ["write a file", "停止", " /cancel literal text \n"]:
            receipt = client.send(text)
            self.assertEqual(receipt["item"]["status"], "pending")
            call = self.fixture.calls[-1]
            self.assertEqual(call[1], "/session/live-a/queue")
            self.assertEqual(call[2], {"prompt": [{"type": "text", "text": text}], "options": {"mode": "auto"}})
            self.assertTrue(call[3]["X-Meadow-Prompt-Request-ID"])
        self.assertFalse(any(call[1].endswith(("/cancel", "/steer", "/prompt")) for call in self.fixture.calls))

    def test_persistent_passive_resume_history_and_cursor_are_one_snapshot(self):
        first = self.connected()
        first.close()
        client = self.client()
        client.connect()
        session = client.open_conversation()
        self.assertEqual(session["sessionId"], "live-resumed")
        self.assertEqual(client.load_messages()[1], {"role": "assistant", "content": "stored answer"})
        resume = next(call for call in self.fixture.calls if call[1] == "/session/resume")
        self.assertFalse(resume[2]["continueInterrupted"])
        self.assertEqual(resume[2]["chatId"], "live-a")
        self.fixture.events = [{"id": 12, "type": "session_update", "data": {"old": True}}, {"id": 13, "type": "run_state", "data": {"status": "running"}}]
        stream = client.events()
        self.assertEqual(next(stream)["id"], 13)
        with self.assertRaisesRegex(AgentError, "disconnected"):
            next(stream)
        event_call = self.fixture.calls[-1]
        self.assertEqual(event_call[3]["Last-Event-ID"], "12")
        self.assertEqual(client.last_event_id, 13)
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)
        self.assertEqual((self.root / "state/conversation.json").stat().st_mode & 0o777, 0o600)

    def test_native_events_and_error_terminal_reason_preserved_and_eof_is_error(self):
        client = self.connected()
        self.fixture.events = [
            {"id": 1, "type": "session_update", "data": {"update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "原文 delta"}}}},
            {"id": 2, "type": "permission_request", "data": {"requestId": "permission-a", "options": [{"optionId": "reject"}]}},
            {"id": 3, "type": "stream_error", "data": {"message": "provider failure"}},
            {"id": 4, "type": "turn_completed", "data": {"stopReason": "error"}},
        ]
        stream = client.events()
        self.assertEqual([next(stream) for _ in range(4)], self.fixture.events)
        with self.assertRaises(AgentError) as caught:
            next(stream)
        self.assertEqual(caught.exception.code, "event_stream_disconnected")
        self.assertFalse(any("/permission/" in call[1] for call in self.fixture.calls))

    def test_stream_retry_only_replays_events_and_does_not_resend_message(self):
        client = self.connected()
        client.send("one task")
        self.fixture.streams = [[{"id": 1, "type": "run_state", "data": {"status": "running"}}], [{"id": 1, "type": "run_state", "data": {}}, {"id": 2, "type": "turn_completed", "data": {"stopReason": "end_turn"}}]]
        first = client.events()
        next(first)
        with self.assertRaises(AgentError):
            next(first)
        second = client.events()
        self.assertEqual(next(second)["id"], 2)
        second.close()
        self.assertEqual(sum(call[0] == "POST" and call[1].endswith("/queue") for call in self.fixture.calls), 1)
        self.assertEqual(self.fixture.calls[-1][3]["Last-Event-ID"], "1")

    def test_explicit_cancel_permission_and_close_are_separate(self):
        client = self.connected()
        client.respond_permission("permission/a", "approve")
        self.assertEqual(self.fixture.calls[-1][1], "/session/live-a/permission/permission%2Fa")
        self.assertEqual(self.fixture.calls[-1][2], {"outcome": {"outcome": "selected", "optionId": "approve"}})
        client.cancel()
        self.assertTrue(self.fixture.calls[-1][1].endswith("/cancel"))
        before = len(self.fixture.calls)
        client.close()
        self.assertEqual(len(self.fixture.calls), before)
        self.assertTrue(self.fixture.thread.is_alive())

    def test_question_answer_list_matches_go_q_indexed_object_contract(self):
        client = self.connected()
        # This unconverted wire form must actually fail against the fixture's
        # Go-compatible decoder, independently of the convenience method.
        with self.assertRaises(AgentError) as caught:
            client._request("POST", "/session/live-a/user-input/question-a", {"answers": ["one", "two"]})
        self.assertEqual(caught.exception.status, 400)
        answers = ["  保留原文\n換行  ", ["first choice", "second choice"]]
        self.assertEqual(client.respond_user_input("question-a", answers), {"ok": True})
        self.assertEqual(self.fixture.calls[-1][2], {"answers": {"q0": answers[0], "q1": answers[1]}})
        explicit = {"q0": "explicit answer"}
        client.respond_user_input("question-a", explicit)
        self.assertEqual(self.fixture.calls[-1][2], {"answers": explicit})

    def test_close_interrupts_blocked_stream_without_cancelling_background_work(self):
        for hold_headers in (False, True):
            with self.subTest(headers_pending=hold_headers):
                client = self.connected()
                self.fixture.hold_stream = True
                self.fixture.hold_headers = hold_headers
                self.fixture.stream_started.clear()
                self.fixture.release_stream.clear()
                errors = []

                def consume():
                    try:
                        list(client.events())
                    except Exception as exc:
                        errors.append(exc)

                worker = threading.Thread(target=consume, daemon=True)
                worker.start()
                self.assertTrue(self.fixture.stream_started.wait(2))
                client.close()
                worker.join(timeout=2)
                self.assertFalse(worker.is_alive())
                self.assertEqual(errors, [])
                self.fixture.release_stream.set()
                self.assertFalse(any(call[1].endswith(("/cancel", "/end")) for call in self.fixture.calls))
        # The startup error is still visible without an intentional close.
        client = self.connected()
        with mock.patch.object(client, "_open", side_effect=AgentError("real transport error", code="transport_unavailable")):
            with self.assertRaisesRegex(AgentError, "real transport error"):
                next(client.events())
        # A close must not mask an independent identity rejection either.
        def rejected_identity(*args, **kwargs):
            client.close()
            raise AgentError("wrong peer", code="unsafe_peer_identity")
        with mock.patch.object(client, "_open", side_effect=rejected_identity):
            with self.assertRaisesRegex(AgentError, "wrong peer"):
                next(client.events())

    def test_status_uses_native_harness_running_when_context_misreports_queue_as_idle(self):
        client = self.connected()
        self.fixture.responses[("GET", "/session/live-a/context")] = (200, {"running": False, "usedTokens": 15953})
        status = client.status()
        self.assertTrue(status["session"]["running"])
        self.assertTrue(status["harness"]["running"])
        self.assertEqual(status["harness"]["activeMode"], "kernel")
        self.assertFalse(status["context"]["running"])
        self.assertEqual(status["context"]["usedTokens"], 15953)
        self.assertEqual(status["tasks"]["items"][0]["status"], "loop_detected")
        self.assertEqual(status["queue"]["items"], [])
        self.assertTrue(status["model"]["ready"])

    def test_status_does_not_fake_idle_when_native_harness_state_is_unavailable(self):
        client = self.connected()
        client.session["running"] = True
        self.fixture.responses[("GET", "/session/live-a/context")] = (200, {"running": False})
        for code, response in [(503, {"error": "run state unavailable"}), (200, {}), (200, {"running": None}), (200, {"running": 0})]:
            with self.subTest(response=response):
                self.fixture.responses[("GET", "/session/live-a/harness-controls")] = (code, response)
                with self.assertRaises(AgentError):
                    client.status()
                self.assertTrue(client.session["running"])
        # A valid native idle receipt still clears activity, regardless of a
        # pending queue row; only the backend's actual run state is authoritative.
        self.fixture.responses[("GET", "/session/live-a/harness-controls")] = (200, {"running": False})
        self.fixture.responses[("GET", "/session/live-a/queue")] = (200, {"items": [{"id": "pending-work", "status": "pending"}]})
        self.assertFalse(client.status()["session"]["running"])

    def test_unknown_saved_conversation_is_not_silently_replaced(self):
        client = self.connected()
        client.send("a submitted conversation is never an empty initial chat")
        client.close()
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "unknown chat"})
        self.fixture.responses[("GET", "/session/live-a/queue")] = (404, {"error": "unknown session"})
        second = self.client()
        second.connect()
        with self.assertRaises(AgentError) as caught:
            second.open_conversation()
        self.assertEqual(caught.exception.status, 404)
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)

    def test_initial_empty_missing_after_restart_is_replaced_after_history_absence_proof(self):
        first = self.connected()
        state = json.loads(first.persistence_path.read_text())
        self.assertTrue(state["initialEmpty"])
        first.close()
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "unknown chat"})
        self.fixture.responses[("GET", "/session/live-a/queue")] = (404, {"error": "unknown session"})
        self.fixture.responses[("POST", "/session")] = (200, {"sessionId": "fresh-b"})
        second = self.client()
        second.connect()
        session = second.open_conversation()
        self.assertEqual(session["sessionId"], "fresh-b")
        self.assertEqual(session["replacedEmptyChatId"], "live-a")
        saved = json.loads(second.persistence_path.read_text())
        self.assertTrue(saved["initialEmpty"])
        self.assertEqual(saved["previousEmptyChatId"], "live-a")
        self.assertEqual(saved["chatId"], "fresh-b")
        self.assertEqual(second.load_messages(), [])
        self.assertTrue(any(call[1].startswith("/sessions?cwd=") for call in self.fixture.calls))
        self.assertFalse(any(call[0] == "POST" and call[1].endswith("/queue") for call in self.fixture.calls))

    def test_existing_history_prevents_empty_replacement_even_when_resume_returns_404(self):
        first = self.connected()
        first.close()
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "transcript temporarily unavailable"})
        listing_path = "/sessions?cwd=" + quote(str(first.workspace), safe="")
        self.fixture.responses[("GET", listing_path)] = (200, {"sessions": [{"sessionId": "live-a"}]})
        second = self.client()
        second.connect()
        with self.assertRaises(AgentError) as caught:
            second.open_conversation(allow_empty_replacement=True)
        self.assertEqual(caught.exception.status, 404)
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)
        self.assertEqual(json.loads(second.persistence_path.read_text())["chatId"], "live-a")

    def test_onboarding_reconnect_replaces_only_proven_initial_live_empty_chat(self):
        self.fixture.model = {"ready": False, "error": "unconfigured"}
        first = self.connected()
        first.close()
        self.fixture.identity.update({"connection_id": "inception-fixture", "model_id": "mercury-3"})
        self.fixture.model = {"ready": True, "status": {"model": {"id": "mercury-3"}}}
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "unknown chat"})
        self.fixture.responses[("GET", "/session/live-a/context")] = (200, {"running": False, "connectionId": "", "modelId": "unconfigured"})
        self.fixture.responses[("GET", "/session/live-a/harness-controls")] = (200, {"running": False})
        self.fixture.responses[("POST", "/session")] = (200, {"sessionId": "fresh-mercury", "connectionId": "inception-fixture", "modelId": "mercury-3"})
        second = self.client()
        second.connect()
        session = second.open_conversation()
        self.assertEqual(session["sessionId"], "fresh-mercury")
        self.assertEqual(session["modelId"], "mercury-3")
        self.assertEqual(session["replacedEmptyChatId"], "live-a")
        self.assertTrue(json.loads(second.persistence_path.read_text())["initialEmpty"])
        self.assertFalse(any(call[0] == "POST" and call[1].endswith(("/queue", "/cancel", "/end")) for call in self.fixture.calls))

    def test_onboarding_reconnect_does_not_replace_submitted_queued_or_running_chat(self):
        first = self.connected()
        first.close()
        self.fixture.identity.update({"connection_id": "inception-fixture", "model_id": "mercury-3"})
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "unknown chat"})
        self.fixture.responses[("GET", "/session/live-a/context")] = (200, {"running": False, "connectionId": "", "modelId": "unconfigured"})
        self.fixture.responses[("GET", "/session/live-a/harness-controls")] = (200, {"running": False})
        original = json.loads(first.persistence_path.read_text())
        for reason in ("submitted", "queued", "running", "legacy"):
            saved = dict(original)
            self.fixture.responses[("GET", "/session/live-a/queue")] = (200, {"items": []})
            self.fixture.responses[("GET", "/session/live-a/harness-controls")] = (200, {"running": reason == "running"})
            if reason == "submitted":
                saved["initialEmpty"] = False
            if reason == "queued":
                self.fixture.responses[("GET", "/session/live-a/queue")] = (200, {"items": [{"id": "retained-work"}]})
            if reason == "legacy":
                saved["version"] = 1
                saved.pop("initialEmpty")
            first.persistence_path.write_text(json.dumps(saved))
            second = self.client()
            second.connect()
            with self.assertRaises(AgentError):
                second.open_conversation()
            self.assertEqual(json.loads(first.persistence_path.read_text())["chatId"], "live-a")
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)

    def test_unproven_legacy_empty_needs_explicit_replacement_and_keeps_provenance(self):
        first = self.connected()
        legacy = json.loads(first.persistence_path.read_text())
        legacy["version"] = 1
        del legacy["initialEmpty"]
        first.persistence_path.write_text(json.dumps(legacy))
        first.close()
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "unknown chat"})
        self.fixture.responses[("GET", "/session/live-a/queue")] = (404, {"error": "unknown session"})
        second = self.client()
        second.connect()
        with self.assertRaises(AgentError):
            second.open_conversation()
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)
        self.fixture.responses[("POST", "/session")] = (200, {"sessionId": "fresh-from-legacy"})
        session = second.open_conversation(allow_empty_replacement=True)
        self.assertEqual(session["replacedEmptyChatId"], "live-a")
        self.assertEqual(json.loads(second.persistence_path.read_text())["version"], 2)

    def test_failed_or_uncertain_send_is_never_reset_as_initial_empty(self):
        first = self.connected()
        self.fixture.responses[("POST", "/session/live-a/queue")] = (503, {"error": "queue commit uncertain"})
        with self.assertRaises(AgentError):
            first.send("could already have been accepted")
        self.assertFalse(json.loads(first.persistence_path.read_text())["initialEmpty"])
        first.close()
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "unknown chat"})
        self.fixture.responses[("GET", "/session/live-a/queue")] = (404, {"error": "unknown session"})
        second = self.client()
        second.connect()
        with self.assertRaises(AgentError):
            second.open_conversation(allow_empty_replacement=True)
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)

    def test_existing_queue_work_without_transcript_is_preserved_even_during_live_probe_race(self):
        first = self.connected()
        first.close()
        self.fixture.responses[("POST", "/session/resume")] = (404, {"error": "no transcript yet"})
        self.fixture.responses[("GET", "/session/live-a/queue")] = (200, {"items": [{"id": "accepted-work", "status": "pending"}]})
        self.fixture.responses[("GET", "/session/live-a/context")] = (404, {"error": "live session disappeared during probe"})
        second = self.client()
        second.connect()
        with self.assertRaises(AgentError):
            second.open_conversation()
        self.assertEqual(sum(call[1] == "/session" for call in self.fixture.calls), 1)
        del self.fixture.responses[("GET", "/session/live-a/context")]
        session = second.open_conversation()
        self.assertEqual(session["sessionId"], "live-a")
        self.assertTrue(session["running"])
        self.assertFalse(json.loads(second.persistence_path.read_text())["initialEmpty"])

    def test_provider_session_error_and_queue_failure_remain_errors_without_retry(self):
        client = self.client()
        client.connect()
        self.fixture.responses[("POST", "/session")] = (503, {"error": "no configured provider", "code": "connection_unavailable"})
        with self.assertRaises(AgentError) as caught:
            client.open_conversation()
        self.assertEqual(caught.exception.code, "connection_unavailable")
        del self.fixture.responses[("POST", "/session")]
        client.open_conversation()
        self.fixture.responses[("POST", "/session/live-a/queue")] = (503, {"error": "queue failed"})
        with self.assertRaises(AgentError):
            client.send("do not automatically retry")
        self.assertEqual(sum(call[0] == "POST" and call[1].endswith("/queue") for call in self.fixture.calls), 1)

    def test_identity_mismatch_rejected_before_product_routes(self):
        for field, value in [("pid", os.getpid() + 1), ("binary_fingerprint", "sha256:other"), ("state_root_fingerprint", "sha256:other"), ("vm_lab", False), ("package_mode", "public")]:
            with self.subTest(field=field):
                original = self.fixture.identity[field]
                self.fixture.identity[field] = value
                client = self.client()
                with self.assertRaises(AgentError):
                    client.connect()
                self.fixture.identity[field] = original
        self.assertTrue(all(call[1] == "/transport/identity" for call in self.fixture.calls))

    def test_linux_kernel_credentials_require_current_uid_and_real_pid(self):
        connection = mock.Mock()
        with mock.patch.object(MODULE.sys, "platform", "linux"), mock.patch.object(MODULE.socket, "SO_PEERCRED", 17, create=True):
            connection.getsockopt.return_value = struct.pack("iII", os.getpid(), os.geteuid(), os.getegid())
            self.assertEqual(MODULE._peer_pid(connection), os.getpid())
            for pid, uid in [(os.getpid(), os.geteuid() + 1), (1, os.geteuid()), (0, os.geteuid())]:
                connection.getsockopt.return_value = struct.pack("iII", pid, uid, os.getegid())
                with self.assertRaises(AgentError) as caught:
                    MODULE._peer_pid(connection)
                self.assertEqual(caught.exception.code, "unsafe_peer_identity")

    def test_unsafe_or_missing_token_is_not_created_and_no_bearer_is_sent_to_wrong_peer(self):
        token = self.fixture.profile / "run/agent-api-token"
        token.chmod(0o644)
        with self.assertRaises(AgentError):
            self.client().connect()
        token.unlink()
        with self.assertRaises(AgentError):
            self.client().connect()
        self.assertFalse(token.exists())
        token.write_text(TOKEN)
        token.chmod(0o600)
        with mock.patch.object(MODULE, "_peer_pid", side_effect=AgentError("wrong peer UID", code="unsafe_peer_identity")):
            with self.assertRaises(AgentError):
                self.client().connect()
        self.assertEqual(self.fixture.calls, [])

    def test_malformed_or_truncated_sse_is_never_a_terminal_success(self):
        client = self.connected()
        for raw, code in [(b"data: {bad}\n\n", "invalid_event"), (b'data: {"id":1,"type":"turn_completed","data":{}}', "event_stream_disconnected")]:
            with self.subTest(raw=raw):
                self.fixture.streams = [raw]
                with self.assertRaises(AgentError) as caught:
                    next(client.events())
                self.assertEqual(caught.exception.code, code)


if __name__ == "__main__":
    unittest.main()
