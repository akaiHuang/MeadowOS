"""Private host-browser image ingress into the visible native chat draft.

The companion accepts bytes, never a host path. A native heartbeat scopes every
lease and mailbox item to one foreground chat epoch. Accepted means spooled;
only a native cache callback can produce a staged receipt. No Agent is invoked.
"""
from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
import tempfile
import threading
import time
import uuid

MAX_FILE_BYTES = 20 * 1024 * 1024
MAX_TOTAL_BYTES = 1024 * 1024 * 1024
MAX_JSON_BYTES = 8192
STATE_TTL = 6
LEASE_TTL = 12
_IDENTIFIER = re.compile(r"[A-Za-z0-9_.:-]{1,160}\Z")
_EVENT_ID = re.compile(r"[a-f0-9]{32}\Z")
_HASH = re.compile(r"[a-f0-9]{64}\Z")
_MIMES = {"image/png", "image/jpeg", "image/gif", "image/webp"}


class AttachmentIngressError(Exception):
    def __init__(self, code, message, status=400):
        super().__init__(message)
        self.code, self.status = code, status


def _identifier(value):
    if not isinstance(value, str) or not _IDENTIFIER.fullmatch(value):
        raise AttachmentIngressError("invalid_id", "A bounded attachment identifier is required")
    return value


def _event_id(value):
    if not isinstance(value, str) or not _EVENT_ID.fullmatch(value):
        raise AttachmentIngressError("invalid_id", "An attachment receipt identifier is required")
    return value


def _number(value):
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)


def _rect(value):
    if (not isinstance(value, dict) or set(value) != {"x", "y", "width", "height"}
            or not all(_number(value[key]) for key in value)
            or any(abs(value[key]) > 65536 for key in ("x", "y"))
            or any(not 0 < value[key] <= 65536 for key in ("width", "height"))):
        raise ValueError("A bounded native chat rectangle is required")
    return {key: value[key] for key in ("x", "y", "width", "height")}


def _private(path, *, directory=False):
    info = path.lstat()
    if (stat.S_ISLNK(info.st_mode) or info.st_mode & 0o077
            or not (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode))):
        raise ValueError("Attachment bridge files must be private and regular")


def _directory(root):
    root = Path(root).expanduser().absolute()
    if root.is_symlink():
        raise ValueError("Attachment spool must not be a symlink")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    _private(root, directory=True)
    path = root / "attachments"
    path.mkdir(mode=0o700, exist_ok=True)
    _private(path, directory=True)
    for name in ("objects", "events", "receipts"):
        child = path / name
        child.mkdir(mode=0o700, exist_ok=True)
        _private(child, directory=True)
    return path


def _write(path, value):
    fd, temporary = tempfile.mkstemp(prefix=".attachment-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, ensure_ascii=False, allow_nan=False)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def _read(path, limit=MAX_JSON_BYTES):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0))
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise ValueError("Attachment file is not private and regular")
        payload = stream.read(limit + 1)
    if len(payload) > limit:
        raise ValueError("Attachment file exceeds its byte limit")
    return payload


def _json(path):
    def reject(_value):
        raise ValueError("Nonfinite attachment JSON")
    value = json.loads(_read(path), parse_constant=reject)
    if not isinstance(value, dict):
        raise ValueError("Attachment object required")
    return value


def _metadata(value):
    event_id = _event_id(value.get("eventId"))
    name, mime, size, digest = (value.get(key) for key in ("name", "mime", "size", "sha256"))
    if (not isinstance(name, str) or not name or len(name) > 512
            or any(ord(char) < 32 for char in name) or "/" in name or "\\" in name
            or name in (".", "..")):
        raise AttachmentIngressError("invalid_name", "Attachment filename must be a bounded basename")
    if mime not in _MIMES:
        raise AttachmentIngressError("invalid_image", "Use PNG, JPEG, GIF, or WebP images")
    if isinstance(size, bool) or not isinstance(size, int) or not 0 < size <= MAX_FILE_BYTES:
        raise AttachmentIngressError("file_too_large", "Each image must be at most 20 MiB", 413)
    if not isinstance(digest, str) or not _HASH.fullmatch(digest):
        raise AttachmentIngressError("invalid_hash", "An original-byte SHA-256 is required")
    return {"eventId": event_id, "name": name, "mime": mime, "size": size, "sha256": digest}


class AttachmentIngress:
    """Independent lease on the existing origin-restricted companion listener."""
    def __init__(self, root):
        self.path = _directory(root)
        self.lock = threading.RLock()
        self.owner = None
        self.upload_lock = threading.Lock()

    def state(self):
        empty = {"version": 1, "epoch": "", "active": False, "rect": None,
                 "updatedAt": 0, "maxFileBytes": MAX_FILE_BYTES}
        try:
            value = _json(self.path / "state.json")
            if (value.get("version") != 1 or not isinstance(value.get("active"), bool)
                    or not _number(value.get("updatedAt"))):
                raise ValueError("Invalid attachment state")
            _identifier(value.get("epoch"))
            if not -5 <= time.time() - value["updatedAt"] <= STATE_TTL:
                return empty
            # Session identity, local paths, draft text and cache state are private.
            return dict(empty, epoch=value["epoch"], active=value["active"],
                        rect=_rect(value["rect"]) if value["active"] else None,
                        updatedAt=value["updatedAt"])
        except FileNotFoundError:
            return empty
        except (OSError, ValueError, TypeError, AttachmentIngressError) as exc:
            raise AttachmentIngressError("invalid_state", "Native chat attachment ingress is unavailable", 503) from exc

    def _require_epoch(self, epoch):
        _identifier(epoch)
        state = self.state()
        if not state["active"] or state["epoch"] != epoch:
            raise AttachmentIngressError("stale_epoch", "The foreground native chat has changed", 409)
        return state

    def lease(self, value):
        client, epoch = _identifier(value.get("clientId")), value.get("epoch")
        if not isinstance(value.get("release", False), bool):
            raise AttachmentIngressError("invalid_release", "Attachment release must be a boolean")
        with self.lock:
            state = self.state()
            if self.owner and (not state["active"] or self.owner[1] != state["epoch"]
                               or self.owner[2] <= time.monotonic()):
                self.owner = None
            if value.get("release"):
                if self.owner and self.owner[:2] == (client, epoch):
                    self.owner = None
                return {"owned": False, "ttlSeconds": LEASE_TTL}
            state = self._require_epoch(epoch)
            point, rect = value.get("point"), state["rect"]
            if (not isinstance(point, dict) or set(point) != {"x", "y"}
                    or not all(_number(point[key]) for key in point)
                    or not rect["x"] <= point["x"] <= rect["x"] + rect["width"]
                    or not rect["y"] <= point["y"] <= rect["y"] + rect["height"]):
                raise AttachmentIngressError("outside_chat", "Drop images inside the foreground native chat", 409)
            if self.owner and self.owner[:2] != (client, epoch):
                return {"owned": False, "ttlSeconds": LEASE_TTL}
            self.owner = (client, epoch, time.monotonic() + LEASE_TTL)
            return {"owned": True, "ttlSeconds": LEASE_TTL}

    def admit_upload(self, value, client):
        # Header-only validation before HTTP allocates an original-byte body.
        _metadata(value)
        _identifier(value.get("collectionId"))
        _identifier(client)
        epoch = value.get("epoch")
        with self.lock:
            self._require_epoch(epoch)
            if self.owner is None or self.owner[:2] != (client, epoch) or self.owner[2] <= time.monotonic():
                raise AttachmentIngressError("attachment_not_owned", "Attachment browser ownership has expired", 403)

    def upload(self, payload, value, client):
        meta = _metadata(value)
        epoch, collection_id = value.get("epoch"), _identifier(value.get("collectionId"))
        _identifier(client)
        if len(payload) != meta["size"] or hashlib.sha256(payload).hexdigest() != meta["sha256"]:
            raise AttachmentIngressError("byte_mismatch", "Attachment bytes do not match the original receipt")
        with self.lock:
            self.admit_upload(value, client)
            receipt_path = self.path / "receipts" / (meta["eventId"] + ".json")
            if receipt_path.exists():
                previous = self.receipt(meta["eventId"], client)
                if any(previous.get(key) != meta[key] for key in ("name", "mime", "size", "sha256")):
                    raise AttachmentIngressError("receipt_conflict", "Receipt identifier belongs to different bytes", 409)
                return previous
            # No item-count cap: only outstanding private bytes are bounded.
            used = 0
            for entry in self.path.rglob("*"):
                try:
                    info = entry.lstat()
                except FileNotFoundError:  # Native atomically replaces receipts.
                    continue
                if stat.S_ISREG(info.st_mode):
                    used += max(info.st_size, getattr(info, "st_blocks", 0) * 512)
            # Include durable receipts/events and their filesystem allocation;
            # tiny files cannot bypass the private disk budget through metadata.
            if used + len(payload) + MAX_JSON_BYTES * 3 > MAX_TOTAL_BYTES:
                raise AttachmentIngressError("spool_full", "Pending attachment storage is full; let native staging finish", 507)
            native = _json(self.path / "state.json")
            if native.get("epoch") != epoch or not native.get("active"):
                raise AttachmentIngressError("stale_epoch", "The foreground native chat has changed", 409)
            event = dict(meta, epoch=epoch, collectionId=collection_id, clientId=client,
                         sessionId=_identifier(native.get("sessionId")), receivedAt=time.time(), blobId=meta["eventId"])
            blob = self.path / "objects" / (event["blobId"] + ".blob")
            fd = os.open(blob, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            try:
                with os.fdopen(fd, "wb") as stream:
                    stream.write(payload)
                    stream.flush()
                    os.fsync(stream.fileno())
                self._require_epoch(epoch)
                receipt = dict(meta, status="accepted", clientId=client, updatedAt=time.time())
                _write(receipt_path, receipt)
                _write(self.path / "events" / (f"{time.time_ns():020d}-" + event["eventId"] + ".json"), event)
            except Exception:
                blob.unlink(missing_ok=True)
                receipt_path.unlink(missing_ok=True)
                raise
            return self.receipt(meta["eventId"], client)

    def receipt(self, event_id, client):
        _event_id(event_id)
        _identifier(client)
        try:
            value = _json(self.path / "receipts" / (event_id + ".json"))
        except FileNotFoundError as exc:
            raise AttachmentIngressError("receipt_not_found", "Attachment receipt is unavailable", 404) from exc
        except (OSError, ValueError, TypeError) as exc:
            raise AttachmentIngressError("invalid_receipt", "Attachment receipt is unavailable", 503) from exc
        if value.get("clientId") != client:
            raise AttachmentIngressError("receipt_not_owned", "Attachment receipt belongs to another browser", 403)
        return {key: value[key] for key in ("eventId", "name", "mime", "size", "sha256", "status",
                "attachmentId", "error", "message", "updatedAt") if key in value}


class NativeAttachmentTransport:
    """GTK heartbeat plus one private byte-staging worker; no network or Agent."""
    def __init__(self, state_dir, on_attachment):
        configured = os.environ.get("MEADOW_ATTACHMENT_SPOOL") or os.environ.get("MEADOW_VOICE_SPOOL")
        self.enabled = bool(configured)
        self.path = _directory(configured or Path(state_dir) / "attachment-bridge") if self.enabled else None
        self.on_attachment, self.before_poll = on_attachment, None
        self.closed, self.timer = False, None
        self.lock = threading.RLock()
        self.worker = None
        self.state = {"version": 1, "epoch": uuid.uuid4().hex, "active": False,
                      "rect": None, "sessionId": "inactive", "updatedAt": 0}
        self.publish()
        if self.enabled:
            self._recover_claims()

    def _recover_claims(self):
        # A prior Shell can exit after claiming an event. Requeue only its
        # immutable descriptor; the new native epoch rejects it explicitly.
        for claim in (self.path / "events").glob(".consume-*"):
            try:
                event = _json(claim)
                event_id = _event_id(event.get("eventId"))
                destination = self.path / "events" / (f"{time.time_ns():020d}-" + event_id + ".json")
                os.replace(claim, destination)
            except (OSError, ValueError, TypeError, AttachmentIngressError):
                continue  # Corrupt private bytes remain available for diagnosis.

    def start(self):
        if self.enabled and self.timer is None:
            from gi.repository import GLib
            self.timer = GLib.timeout_add(100, self.poll)

    def invalidate(self):
        with self.lock:
            self.state["epoch"] = uuid.uuid4().hex
            self.publish()

    def publish(self, *, active=None, rect=None, session_id=None):
        if not self.enabled or self.closed:
            return
        with self.lock:
            if active is not None and not isinstance(active, bool):
                raise ValueError("Attachment active must be a boolean")
            session_id = self.state["sessionId"] if session_id is None else _identifier(session_id)
            if ((active is not None and active != self.state["active"])
                    or session_id != self.state["sessionId"]):
                self.state["epoch"] = uuid.uuid4().hex
            if active is not None:
                self.state["active"] = active
            if rect is not None:
                self.state["rect"] = _rect(rect)
            self.state["sessionId"] = session_id
            if self.state["active"] and self.state["rect"] is None:
                raise ValueError("An active native chat requires its window rectangle")
            self.state["updatedAt"] = time.time()
            _write(self.path / "state.json", self.state)

    def poll(self):
        if self.closed or not self.enabled:
            return False
        if self.before_poll:
            self.before_poll()
        if time.time() - self.state["updatedAt"] >= 2:
            self.publish()
        if ((self.worker is None or not self.worker.is_alive())
                and next((self.path / "events").glob("*.json"), None) is not None):
            self.worker = threading.Thread(target=self._consume_one, name="meadow-attachment-stage", daemon=True)
            self.worker.start()
        return True

    def _consume_one(self):
        # Scan filenames, then read/hash/cache exactly one original per worker.
        candidate = min((self.path / "events").glob("*.json"), default=None)
        if candidate is None:
            return
        consume = candidate.with_name(".consume-" + uuid.uuid4().hex)
        event, blob = None, None
        try:
            os.replace(candidate, consume)
            event = _json(consume)
            meta = _metadata(event)
            if event.get("blobId") != meta["eventId"]:
                raise ValueError("Invalid confined attachment blob")
            blob = self.path / "objects" / (meta["eventId"] + ".blob")
            with self.lock:
                if (self.closed or not self.state["active"] or event.get("epoch") != self.state["epoch"]
                        or event.get("sessionId") != self.state["sessionId"]):
                    raise AttachmentIngressError("stale_epoch", "The native chat changed before attachment staging", 409)
            payload = _read(blob, MAX_FILE_BYTES)
            if len(payload) != meta["size"] or hashlib.sha256(payload).hexdigest() != meta["sha256"]:
                raise AttachmentIngressError("byte_mismatch", "The original attachment bytes failed verification")
            staged = self.on_attachment(dict(event, spool_path=str(blob)), payload)
            if not isinstance(staged, dict) or not isinstance(staged.get("id"), str):
                raise ValueError("Native attachment cache did not return a staged receipt")
            if any(key in staged and staged[key] != meta[key] for key in ("sha256", "size")):
                raise AttachmentIngressError("cache_byte_mismatch", "Native cache receipt does not match the original bytes")
            receipt = dict(meta, status="staged", clientId=event["clientId"],
                           attachmentId=staged["id"], updatedAt=time.time())
            _write(self.path / "receipts" / (meta["eventId"] + ".json"), receipt)
            blob.unlink(missing_ok=True)  # Canonical original exists only after cache receipt.
        except Exception as exc:
            if event and _EVENT_ID.fullmatch(str(event.get("eventId", ""))):
                receipt = {key: event[key] for key in ("eventId", "name", "mime", "size", "sha256", "clientId") if key in event}
                receipt.update(status="failed", updatedAt=time.time(),
                               error=getattr(exc, "code", "native_staging_failed"),
                               message="Image could not be added to the native draft")
                _write(self.path / "receipts" / (event["eventId"] + ".json"), receipt)
            # Failed originals remain private for diagnosis/retry; never silently
            # claim attachment cache success or delete the only byte source.
        finally:
            consume.unlink(missing_ok=True)

    def close(self):
        if self.closed:
            return
        self.publish(active=False)
        self.closed = True
        if self.timer is not None:
            from gi.repository import GLib
            GLib.source_remove(self.timer)
            self.timer = None
