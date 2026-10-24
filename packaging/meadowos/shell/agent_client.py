"""Presentation-only client for the existing MeadowOS Go Agent.

All methods block: GTK callers must use workers and dispatch events onto GLib.
`send` commits to the Agent's durable queue, including when idle; a queue receipt
is never a model answer. `events` yields the native SSE envelopes unchanged.
There is no planner, provider implementation, automatic approval or task replay.
"""
from __future__ import annotations

import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import socket
import stat
import struct
import sys
import tempfile
import threading
import uuid
from urllib.parse import quote, quote_plus

try:
    from attachment_cache import AttachmentCache, AttachmentCacheError
except ModuleNotFoundError as exc:
    if exc.name != "attachment_cache":
        raise
    # Existing diagnostic/native loaders import this file directly without
    # adding its directory to sys.path. Resolve only its packaged sibling.
    import importlib.util
    _cache_spec = importlib.util.spec_from_file_location("meadow_shell_attachment_cache", Path(__file__).with_name("attachment_cache.py"))
    _cache_module = importlib.util.module_from_spec(_cache_spec)
    _cache_spec.loader.exec_module(_cache_module)
    AttachmentCache, AttachmentCacheError = _cache_module.AttachmentCache, _cache_module.AttachmentCacheError

MAX_BODY = 16 * 1024 * 1024
MAX_FRAME = 2 * 1024 * 1024


class AgentError(Exception):
    def __init__(self, message, *, status=None, code="agent_error", payload=None):
        super().__init__(message)
        self.status, self.code, self.payload = status, code, payload


def _absolute(value, label):
    path = Path(value)
    if not path.is_absolute() or "\x00" in str(path):
        raise AgentError(f"{label} must be an absolute path", code="invalid_path")
    return Path(os.path.normpath(path))


def _private_read(path, limit):
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
                raise AgentError("Agent private file ownership or permissions are unsafe", code="unsafe_private_file")
            data = stream.read(limit + 1)
            if len(data) > limit:
                raise AgentError("Agent private file is too large", code="invalid_private_file")
            return data
    except OSError as exc:
        raise AgentError("Cannot read existing Agent private state", code="private_file_unavailable") from exc


def _peer_pid(connection):
    if sys.platform.startswith("linux"):
        pid, uid, _ = struct.unpack("iII", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("iII")))
    elif sys.platform == "darwin":  # Offline transport tests also run on macOS.
        credentials = connection.getsockopt(0, 1, 76)  # LOCAL_PEERCRED / xucred
        version, uid = struct.unpack_from("II", credentials)
        if version != 0:
            raise AgentError("Unsupported Unix peer credentials", code="unsafe_peer_identity")
        pid = struct.unpack("i", connection.getsockopt(0, 2, struct.calcsize("i")))[0]  # LOCAL_PEERPID
    else:
        raise AgentError("Unix peer identity is unsupported on this platform", code="peer_identity_unsupported")
    if uid != os.geteuid() or pid <= 1:
        raise AgentError("Agent socket peer identity is unsafe", code="unsafe_peer_identity")
    return pid


class _UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, client, timeout):
        super().__init__("meadow-agent.local", timeout=timeout)
        self.client, self.peer_pid, self.transport_socket = client, None, None

    def connect(self):
        if self.client._closed.is_set():
            raise AgentError("Agent client is closed", code="client_closed")
        info = self.client.socket_path.lstat()
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise AgentError("Agent socket ownership or permissions are unsafe", code="unsafe_socket")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        # HTTPConnection may detach self.sock for a close-delimited SSE reply;
        # keep the actual socket so close() can interrupt its blocked body read.
        self.transport_socket = self.sock
        if self.client._closed.is_set():
            raise AgentError("Agent client is closed", code="client_closed")
        self.sock.settimeout(self.timeout)
        self.sock.connect(str(self.client.socket_path))
        self.peer_pid = _peer_pid(self.sock)
        expected = self.client.identity
        if expected and self.peer_pid != expected["pid"]:
            raise AgentError("Agent process changed; reconnect before continuing", code="agent_identity_changed")


class AgentClient:
    def __init__(self, workspace=None, state_root=None, *, helper_path=None, persistence_path=None, timeout=15, attachment_cache=None):
        home = _absolute(os.environ.get("HOME") or str(Path.home()), "HOME")
        data = _absolute(os.environ.get("XDG_DATA_HOME") or home / ".local/share", "XDG_DATA_HOME")
        state = _absolute(os.environ.get("XDG_STATE_HOME") or home / ".local/state", "XDG_STATE_HOME")
        self.state_root = _absolute(state_root or data / "meadow-os/agent", "Agent state root")
        self.workspace = _absolute(workspace or home / "MeadowWork", "workspace")
        self.socket_path = self.state_root / "run/agent.sock"
        self.token_path = self.state_root / "run/agent-api-token"
        # Installed at <prefix>/share/meadow-os/shell/agent_client.py. Bind to
        # that payload's helper, preserving relocatable Linux installations.
        prefix = Path(__file__).resolve().parents[3]
        self.helper_path = _absolute(helper_path or prefix / "libexec/meadow-agentd", "Agent executable")
        self.persistence_path = _absolute(persistence_path or state / "meadow-os/shell-conversation.json", "Shell conversation state")
        self.timeout, self.identity, self.session = timeout, None, None
        self._attachment_cache = attachment_cache
        self.last_event_id = 0
        self._initial_empty, self._previous_empty_chat_id = False, None
        self._messages, self._connections = [], set()
        self._lock = threading.RLock()
        self._closed = threading.Event()

    def _token(self):
        try:
            token = _private_read(self.token_path, 4096).decode("ascii").strip()
            if not re.fullmatch(r"[A-Za-z0-9_-]+", token) or len(base64.urlsafe_b64decode(token + "=" * (-len(token) % 4))) < 32:
                raise ValueError("invalid token")
            return token
        except (ValueError, UnicodeError) as exc:
            raise AgentError("Existing Agent credential is invalid", code="invalid_credential") from exc

    def _open(self, method, path, body=None, headers=None, *, stream=False, raw_body=None):
        if self._closed.is_set():
            raise AgentError("Agent client is closed", code="client_closed")
        if path != "/transport/identity" and self.identity is None:
            raise AgentError("Connect to the Agent first", code="not_connected")
        token = self._token()
        connection = _UnixHTTPConnection(self, 45 if stream else self.timeout)
        with self._lock:
            if self._closed.is_set():
                raise AgentError("Agent client is closed", code="client_closed")
            self._connections.add(connection)
        try:
            # Explicit connect validates this exact socket peer BEFORE bearer transmission.
            connection.connect()
            payload = raw_body if raw_body is not None else (None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8"))
            request_headers = {"Authorization": "Bearer " + token, "Accept": "text/event-stream" if stream else "application/json"}
            if payload is not None and raw_body is None:
                request_headers["Content-Type"] = "application/json"
            request_headers.update(headers or {})
            connection.request(method, path, payload, request_headers)
            return connection, connection.getresponse()
        except Exception as exc:
            self._release(connection)
            if isinstance(exc, AgentError):
                raise
            raise AgentError("Cannot reach the existing Meadow Agent service", code="transport_unavailable") from exc

    def _release(self, connection):
        with self._lock:
            self._connections.discard(connection)
        connection.close()

    @staticmethod
    def _decode(response, *, allow_empty=False):
        raw = response.read(MAX_BODY + 1)
        if len(raw) > MAX_BODY:
            raise AgentError("Agent response exceeded the client limit", code="response_too_large")
        if not raw and allow_empty and 200 <= response.status < 300:
            return {}
        try:
            value = json.loads(raw)
            if not isinstance(value, dict):
                raise ValueError("object required")
            return value
        except (ValueError, UnicodeError) as exc:
            raise AgentError("Agent returned invalid JSON", status=response.status, code="invalid_response") from exc

    def _request(self, method, path, body=None, headers=None, *, allow_unready=False, allow_empty=False):
        connection, response = self._open(method, path, body, headers)
        try:
            value = self._decode(response, allow_empty=allow_empty)
            if not 200 <= response.status < 300 and not (allow_unready and response.status == 503 and value.get("ready") is False):
                raise AgentError(value.get("error") or value.get("message") or f"Agent HTTP {response.status}", status=response.status, code=value.get("code", "http_error"), payload=value)
            if path == "/transport/identity":
                self._validate_identity(value, connection.peer_pid)
            return value
        except (OSError, http.client.HTTPException) as exc:
            raise AgentError("Agent response was interrupted; request outcome is unknown", code="response_interrupted") from exc
        finally:
            self._release(connection)

    def _validate_identity(self, identity, peer_pid):
        if identity.get("pid") != peer_pid or identity.get("ok") is not True or identity.get("package_mode") != "portable" or identity.get("vm_lab") is not True:
            raise AgentError("Agent is not the expected MeadowOS development service", code="identity_mismatch")
        wanted_profile = "sha256:" + hashlib.sha256(str(self.state_root).encode()).hexdigest()
        if identity.get("state_root_fingerprint") != wanted_profile:
            raise AgentError("Agent belongs to another state profile", code="identity_mismatch")
        try:
            info = self.helper_path.lstat()
            if not stat.S_ISREG(info.st_mode) or not info.st_mode & 0o111:
                raise OSError("invalid helper")
            digest = hashlib.sha256()
            with self.helper_path.open("rb") as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    digest.update(chunk)
        except OSError as exc:
            raise AgentError("Cannot verify the installed Agent executable", code="helper_unavailable") from exc
        if identity.get("binary_fingerprint") != "sha256:" + digest.hexdigest():
            raise AgentError("Running Agent differs from the installed executable", code="identity_mismatch")
        self.identity = identity

    def connect(self):
        self._request("GET", "/transport/identity")
        return self.status()

    def status(self):
        model = self._request("GET", "/agent/model", allow_unready=True)
        queue = self._request("GET", self._session_path("queue")) if self.session else None
        context = self._request("GET", self._session_path("context")) if self.session else None
        harness = self._request("GET", self._session_path("harness-controls")) if self.session else None
        tasks = self._request("GET", self._session_path("tasks")) if self.session else None
        if harness is not None:
            # context.running tracks foreground admission only. The native
            # harness view also owns promoted queue/recovery runs; do not infer
            # activity from queue items or overwrite it with context's false.
            if type(harness.get("running")) is not bool:
                raise AgentError("Agent returned no authoritative run state", code="invalid_response")
            self.session["running"] = harness["running"]
        return {"connected": True, "identity": self.identity, "model": model, "session": self.session, "queue": queue, "context": context, "harness": harness, "tasks": tasks}

    def _session_path(self, suffix):
        if not self.session:
            raise AgentError("Open a conversation first", code="no_session")
        return "/session/" + quote(self.session["sessionId"], safe="") + "/" + suffix

    def task_plan(self):
        """Read the selected session's committed, model-owned plan unchanged."""
        path = self._session_path("task-plan")
        session_id = self.session["sessionId"]
        workspace = str(self.workspace)
        value = self._request("GET", path)
        if (not self.session or self.session.get("sessionId") != session_id
                or str(self.workspace) != workspace):
            raise AgentError("Conversation changed while reading the task plan", code="session_changed")
        plan = value.get("plan")
        if (value.get("ok") is not True or value.get("sessionId") != session_id
                or not isinstance(plan, dict) or not isinstance(plan.get("items"), list)
                or any(not isinstance(item, dict) for item in plan["items"])):
            raise AgentError("Agent returned an invalid task plan snapshot", code="invalid_response")
        return value

    def workspace_history(self):
        """Read this chat's durable history and successful-write file ledger.

        The Agent owns the ledger; these paths are not inferred from model prose
        and do not attest that a file still exists. Opening remains a separate,
        path-confined native action. A missing stored chat stays an HTTP error.
        """
        if not self.session:
            raise AgentError("Open a conversation first", code="no_session")
        session_id = self.session["sessionId"]
        chat_id = self.session.get("chatId") or session_id
        workspace = str(self.workspace)
        path = "/sessions/" + quote(chat_id, safe="") + "/history?cwd=" + quote(workspace, safe="")
        value = self._request("GET", path)
        if (not self.session or self.session.get("sessionId") != session_id
                or (self.session.get("chatId") or self.session["sessionId"]) != chat_id
                or str(self.workspace) != workspace):
            raise AgentError("Conversation changed while reading workspace history", code="session_changed")
        if (not isinstance(value.get("files"), list)
                or any(not isinstance(path, str) or not path or "\x00" in path for path in value["files"])
                or not isinstance(value.get("messages"), list)
                or not isinstance(value.get("tools"), list)):
            raise AgentError("Agent returned an invalid workspace history", code="invalid_response")
        return value

    def _saved(self):
        if not self.persistence_path.exists():
            return None
        try:
            saved = json.loads(_private_read(self.persistence_path, 16384))
            if not isinstance(saved, dict) or saved.get("workspace") != str(self.workspace) or saved.get("stateRoot") != str(self.state_root):
                raise ValueError("state profile mismatch")
            if not isinstance(saved.get("chatId"), str) or not saved["chatId"]:
                raise ValueError("missing chat id")
            if saved.get("version") not in (1, 2) or not isinstance(saved.get("sessionId"), str) or not saved["sessionId"]:
                raise ValueError("invalid conversation identity")
            if saved["version"] == 2 and type(saved.get("initialEmpty")) is not bool:
                raise ValueError("missing initial empty provenance")
            return saved
        except (ValueError, UnicodeError) as exc:
            raise AgentError("Cannot restore Shell conversation state", code="invalid_conversation_state") from exc

    def _persist(self):
        directory = self.persistence_path.parent
        for component in [*reversed(directory.parents), directory]:
            if component.is_symlink():
                raise AgentError("Shell state path contains a symlink", code="unsafe_private_file")
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        info = directory.stat()
        if info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise AgentError("Shell state directory must be user-private", code="unsafe_private_file")
        value = {"version": 2, "workspace": str(self.workspace), "stateRoot": str(self.state_root), "chatId": self.session.get("chatId") or self.session["sessionId"], "sessionId": self.session["sessionId"], "initialEmpty": self._initial_empty}
        if self._previous_empty_chat_id:
            value["previousEmptyChatId"] = self._previous_empty_chat_id
        fd, temporary = tempfile.mkstemp(prefix=".conversation-", dir=directory)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as stream:
                json.dump(value, stream)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.persistence_path)
            directory_fd = os.open(directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def open_conversation(self, chat_id=None, *, allow_empty_replacement=False, new_chat=False):
        """Restore passively, or explicitly open another independent chat.

        Legacy v1 has no submission provenance. Its replacement requires an
        explicit caller decision after independently verifying that it is empty.
        A v2 submitted chat or a chat in the Agent's history is never replaced.
        new_chat changes this presentation's active preference only. Existing
        Agent sessions, history, queued work and jobs are never cancelled.
        """
        if new_chat and chat_id is not None:
            raise AgentError("Choose a new chat or an existing chat identity", code="invalid_conversation_request")
        saved = self._saved() if chat_id is None and not new_chat else None
        wanted = chat_id or (saved and saved["chatId"])
        initial_empty = False
        self._previous_empty_chat_id = saved.get("previousEmptyChatId") if saved else None
        if wanted:
            try:
                session = self._request("POST", "/session/resume", {"cwd": str(self.workspace), "chatId": wanted, "continueInterrupted": False, "approvalChannel": "interactive"})
            except AgentError as exc:
                # A just-created empty chat has no transcript yet, but its live
                # session may still exist. Probe it without creating/replaying work.
                if exc.status != 404 or not saved:
                    raise
                listing = self._request("GET", "/sessions?cwd=" + quote(str(self.workspace), safe=""))
                rows = listing.get("sessions")
                if not isinstance(rows, list) or any(not isinstance(row, dict) or not isinstance(row.get("sessionId"), str) for row in rows):
                    raise AgentError("Agent returned an invalid conversation listing", code="invalid_response")
                if any(row["sessionId"] == wanted for row in rows):
                    raise exc  # A real transcript exists; never hide a resume failure.
                self.session = {"sessionId": saved["sessionId"], "chatId": wanted}
                queue = None
                try:
                    queue = self._request("GET", self._session_path("queue"))
                    context = self._request("GET", self._session_path("context"))
                except AgentError as live_error:
                    self.session = None
                    if live_error.status != 404:
                        raise
                    eligible = saved.get("initialEmpty") is True or (saved["version"] == 1 and allow_empty_replacement)
                    if not eligible or (queue is not None and queue.get("items")):
                        raise exc
                    session = self._request("POST", "/session", {"cwd": str(self.workspace), "sessionScope": "thread", "approvalChannel": "interactive"})
                    session["replacedEmptyChatId"] = wanted
                    self._previous_empty_chat_id, initial_empty = wanted, True
                else:
                    initial_empty = saved.get("initialEmpty") is True and not queue.get("items") and context.get("running") is False
                    # A first unconfigured chat can remain alive after API
                    # onboarding. Read-only observations remain valid on that
                    # old binding, but sending to it must remain prohibited.
                    # Replace only a proven unsubmitted initial chat, with no
                    # durable history, queue entries, or native harness work.
                    current = self.identity or {}
                    stale = any(isinstance(context.get(field), str)
                                and isinstance(current.get(identity_field), str)
                                and context[field] != current[identity_field]
                                for field, identity_field in (("connectionId", "connection_id"), ("modelId", "model_id")))
                    if stale:
                        if (saved.get("version") != 2 or saved.get("initialEmpty") is not True
                                or queue.get("items") != [] or context.get("running") is not False):
                            raise AgentError("原對話保留原來的模型連線；請從聊天選單選擇「新對話」", status=409, code="session_connection_mismatch")
                        harness = self._request("GET", self._session_path("harness-controls"))
                        if harness.get("running") is not False:
                            raise AgentError("Existing chat still has native work; retry after it finishes", status=409, code="session_connection_mismatch")
                        session = self._request("POST", "/session", {"cwd": str(self.workspace), "sessionScope": "thread", "approvalChannel": "interactive"})
                        session["replacedEmptyChatId"] = wanted
                        self._previous_empty_chat_id, initial_empty = wanted, True
                    else:
                        session = {**self.session, "messages": [], "replayAfterEventId": 0, "running": context["running"]}
        else:
            session = self._request("POST", "/session", {"cwd": str(self.workspace), "sessionScope": "thread", "approvalChannel": "interactive"})
            initial_empty = True
        if not isinstance(session.get("sessionId"), str) or not session["sessionId"]:
            raise AgentError("Agent returned no session identity", code="invalid_response")
        self.session = session
        self._initial_empty = initial_empty
        self._messages = session.get("messages") or []
        self.last_event_id = session.get("replayAfterEventId", 0)
        self._persist()
        return session

    def load_messages(self):
        """Resume's authoritative DisplayMessage snapshot, paired with its SSE cursor."""
        if not self.session:
            raise AgentError("Open a conversation first", code="no_session")
        return list(self._messages)

    @property
    def attachment_cache(self):
        if self._attachment_cache is None:
            directory = self.persistence_path.parent
            for component in [*reversed(directory.parents), directory]:
                if component.is_symlink():
                    raise AgentError("Shell state path contains a symlink", code="unsafe_private_file")
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            info = directory.stat()
            if info.st_uid != os.geteuid() or info.st_mode & 0o077:
                raise AgentError("Shell state directory must be user-private", code="unsafe_private_file")
            self._attachment_cache = AttachmentCache(self.persistence_path.parent / "image-attachments")
        return self._attachment_cache

    def image_capability(self):
        """Read current catalog metadata; unknown is never permission to view pixels."""
        connection, response = self._open("GET", "/model/list")
        try:
            if response.status != 200:
                return None
            payload = response.read(1024 * 1024 + 1)
            if len(payload) > 1024 * 1024:
                return None
            catalog = json.loads(payload)
            if not isinstance(catalog, list):
                return None
            active = [item for item in catalog if isinstance(item, dict) and item.get("active") is True]
            if len(active) == 1 and isinstance(active[0].get("multimodal"), bool):
                return active[0]["multimodal"]
            return None
        except (ValueError, OSError, http.client.HTTPException):
            return None
        finally:
            response.close()
            self._release(connection)

    def add_attachment(self, path):
        """Stage an immutable original; no network, model or provider action."""
        try:
            return self.attachment_cache.add(path)
        except AttachmentCacheError as exc:
            raise AgentError(str(exc), code=exc.code) from exc

    def stage_attachment_bytes(self, data, name, mime=None):
        """Stage verified native bridge bytes without accepting a host path."""
        try:
            return self.attachment_cache.add_bytes(data, name, mime)
        except AttachmentCacheError as exc:
            raise AgentError(str(exc), code=exc.code) from exc

    def remove_attachment(self, attachment):
        identity = attachment.get("id") if isinstance(attachment, dict) else attachment
        try:
            return self.attachment_cache.remove(identity)
        except AttachmentCacheError as exc:
            raise AgentError(str(exc), code=exc.code) from exc

    def attachment_images(self, collection_id, *, cursor="", limit=100):
        """Read a bounded page of retained source metadata and progress."""
        try:
            collection_id = str(uuid.UUID(collection_id))
        except (ValueError, TypeError, AttributeError) as exc:
            raise AgentError("Invalid attachment collection", code="invalid_attachment_collection") from exc
        return self._request("GET", self._session_path("attachment-collections/" + collection_id)
            + "?cursor=" + quote(str(cursor), safe="") + "&limit=" + str(int(limit)))

    def _upload_attachment(self, item, collection_id, session_id):
        image_id = str(uuid.UUID(hex=item["id"].rsplit("-", 1)[-1]))
        reference = {"collection_id": collection_id, "image_id": image_id, "session_id": session_id}
        if item.get("state") in {"uploaded", "submitted"} and item.get("ref") == reference:
            return item
        if not self.session or self.session["sessionId"] != session_id:
            raise AgentError("Conversation changed while uploading attachments", code="session_changed")
        # Cache get() checks the original SHA. Pin the same private regular file
        # descriptor for the streaming request; never serialize originals to JSON.
        path = Path(item["path"])
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        connection = None
        try:
            with os.fdopen(fd, "rb") as source:
                info = os.fstat(source.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_size != item["size"] or info.st_mode & 0o077:
                    raise AgentError("Cached attachment changed before upload", code="source_changed")
                headers = {"Content-Type": item["mime"], "Content-Length": str(item["size"]),
                    "X-Meadow-Attachment-SHA256": item["sha256"],
                    "X-Meadow-Attachment-Name": quote_plus(item["name"])}
                connection, response = self._open("PUT", self._session_path(
                    "attachment-collections/" + collection_id + "/images/" + image_id),
                    headers=headers, raw_body=source)
                value = self._decode(response)
                if not 200 <= response.status < 300:
                    raise AgentError(value.get("error") or "Image upload rejected", status=response.status,
                        code=value.get("code", "attachment_upload_failed"), payload=value)
                if (value.get("id") != image_id or value.get("collection_id") != collection_id
                        or value.get("sha256") != item["sha256"] or value.get("size") != item["size"]):
                    raise AgentError("Image receipt does not match the original", code="invalid_attachment_receipt")
        finally:
            if connection is not None:
                self._release(connection)
        if item.get("state") == "submitted":
            return {**item, "ref": reference}
        return self.attachment_cache.mark_uploaded(item["id"], reference)

    def send(self, text, *, attachments=None, request_id=None, on_attachment=None):
        """Admit one logical message, preserving an explicit delivery identity.

        A caller retaining an uncertain submission can keep its UUID for a
        deliberate retry; this method never retries a failed or unknown send.
        Without an explicit identity, one UUID is generated for this invocation.
        Conversation and legacy queue receipts are returned unchanged.
        """
        attachments = attachments or []
        if not isinstance(text, str) or (not text.strip() and not attachments):
            raise AgentError("Message must contain text or images", code="empty_message")
        if request_id is None:
            request_id = str(uuid.uuid4())
        else:
            try:
                if not isinstance(request_id, str):
                    raise ValueError("request identity must be a UUID string")
                request_id = str(uuid.UUID(request_id))
            except ValueError as exc:
                raise AgentError("Message request identity must be a UUID", code="invalid_request_id") from exc
        prompt = [{"type": "text", "text": text}] if text else []
        staged = []
        if attachments:
            # Validate every requested cache identity before creating a remote
            # collection. Item count is unrestricted; payloads cross one at a time.
            try:
                staged = [self.attachment_cache.get(item.get("id") if isinstance(item, dict) else item)
                          for item in attachments]
            except AttachmentCacheError as exc:
                raise AgentError(str(exc), code=exc.code) from exc
            if len({item['id'] for item in staged}) != len(staged):
                raise AgentError("An attachment identity occurs more than once", code="duplicate_attachment")
            session_id = self.session["sessionId"] if self.session else None
            collection_path = self._session_path("attachment-collections")
            self._request("POST", collection_path, {"collection_id": request_id})
            failures = []
            uploaded = []
            for item in staged:
                if on_attachment:
                    on_attachment({**item, "state": "uploading"})
                try:
                    result = self._upload_attachment(item, request_id, session_id)
                    uploaded.append(result)
                    if on_attachment:
                        on_attachment(result)
                except (AgentError, AttachmentCacheError, OSError, http.client.HTTPException) as exc:
                    failure = {**item, "state": "failed", "error": {"code": getattr(exc, "code", "attachment_upload_failed"), "message": str(exc)}}
                    if item.get("state") != "submitted":
                        try:
                            self.attachment_cache.set_failed(item["id"], str(exc), code=failure["error"]["code"])
                        except AttachmentCacheError:
                            pass
                    failures.append(failure)
                    if on_attachment:
                        on_attachment(failure)
            if failures:
                raise AgentError("Some images could not be uploaded; originals and individual progress are retained",
                    code="attachment_upload_incomplete", payload={"collection_id": request_id, "failures": failures, "uploaded": uploaded})
            if not self.session or self.session["sessionId"] != session_id:
                raise AgentError("Conversation changed while uploading attachments", code="session_changed")
            manifest = hashlib.sha256()
            for item in staged:
                image_id = str(uuid.UUID(hex=item["id"].rsplit("-", 1)[-1]))
                manifest.update((image_id + "\x00" + item["sha256"] + "\n").encode("ascii"))
            prompt.append({"type": "attachment_collection", "collection_id": request_id,
                "expected_count": len(staged), "manifest_sha256": manifest.hexdigest()})
            # An uncertain queue response may still mean admission. Retain all
            # originals before sending so a lost receipt cannot delete task data.
            for item in staged:
                try:
                    self.attachment_cache.mark_submitted(item["id"])
                except AttachmentCacheError as exc:
                    raise AgentError(str(exc), code=exc.code) from exc
        # No local keyword commands, task classifiers or busy/idle race. The
        # existing Agent queue owns promotion, execution, cancellation and history.
        path = self._session_path("queue")
        # Commit this BEFORE HTTP: even a lost receipt can mean work was queued.
        # Such a conversation must never be reset as an empty initial shell.
        self._initial_empty = False
        self._persist()
        return self._request("POST", path,
            {"prompt": prompt, "options": {"mode": "auto"}},
            headers={"X-Meadow-Prompt-Request-ID": request_id})

    def cancel(self):
        """Only call for an explicit human Stop action; close/hide never invokes it."""
        return self._request("POST", self._session_path("cancel"), {})

    def respond_permission(self, request_id, option_id):
        if option_id not in {"approve", "reject", "approve_always"}:
            raise AgentError("Unsupported permission response", code="invalid_permission")
        return self._request("POST", self._session_path("permission/" + quote(request_id, safe="")), {"outcome": {"outcome": "selected", "optionId": option_id}}, allow_empty=True)

    def respond_user_input(self, request_id, answers):
        # Go handleUserInput decodes map[string]any and reads q0, q1, ...;
        # GTK collects ordered per-question answers. Preserve each value intact.
        if isinstance(answers, list):
            answers = {f"q{index}": answer for index, answer in enumerate(answers)}
        elif not isinstance(answers, dict):
            raise AgentError("Question answers must be a list or object", code="invalid_user_input")
        return self._request("POST", self._session_path("user-input/" + quote(request_id, safe="")), {"answers": answers})

    def events(self):
        """Native envelopes only. EOF/timeouts are disconnects, never completion.

        Reinvoke after a transport error to replay after last_event_id. Opening a
        conversation resets that cursor to the backend resume snapshot boundary.
        """
        path = self._session_path("events")
        connection, response = None, None
        try:
            try:
                connection, response = self._open("GET", path, headers={"Last-Event-ID": str(self.last_event_id)}, stream=True)
            except AgentError as exc:
                # close() may interrupt getresponse() before headers exist.
                # Suppress only intentional transport shutdown, never credential,
                # peer identity, HTTP status, or protocol validation failures.
                if self._closed.is_set() and exc.code in {"client_closed", "transport_unavailable"}:
                    return
                raise
            if response.status != 200:
                value = self._decode(response)
                raise AgentError(value.get("error", "Agent event stream rejected"), status=response.status, code=value.get("code", "http_error"))
            if response.getheader("Content-Type", "").split(";", 1)[0].strip() != "text/event-stream":
                raise AgentError("Agent did not return an event stream", code="invalid_event_stream")
            frame, size = [], 0
            while not self._closed.is_set():
                line = response.readline(MAX_FRAME + 1)
                if self._closed.is_set():
                    return
                if not line:
                    raise AgentError("Agent event stream disconnected; task state is unknown", code="event_stream_disconnected")
                size += len(line)
                if size > MAX_FRAME:
                    raise AgentError("Agent event frame exceeded the client limit", code="event_too_large")
                if line in (b"\n", b"\r\n"):
                    if frame:
                        try:
                            event = json.loads(b"\n".join(frame))
                            if not isinstance(event, dict) or not isinstance(event.get("type"), str) or not isinstance(event.get("data"), dict) or type(event.get("id")) is not int:
                                raise ValueError("invalid envelope")
                        except (ValueError, UnicodeError) as exc:
                            raise AgentError("Agent returned an invalid event", code="invalid_event") from exc
                        if event["id"] > self.last_event_id:
                            self.last_event_id = event["id"]
                            yield event
                    frame, size = [], 0
                elif line.startswith(b"data:"):
                    frame.append(line[5:].lstrip(b" ").rstrip(b"\r\n"))
        except (OSError, http.client.HTTPException) as exc:
            if not self._closed.is_set():
                raise AgentError("Agent event stream interrupted; task state is unknown", code="event_stream_disconnected") from exc
        finally:
            if response is not None:
                response.close()
            if connection is not None:
                self._release(connection)

    def close(self):
        """Disconnect presentation only. Never stop the service, cancel or unload."""
        self._closed.set()
        with self._lock:
            connections = list(self._connections)
        for connection in connections:
            if connection.transport_socket:
                try:
                    connection.transport_socket.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            self._release(connection)
