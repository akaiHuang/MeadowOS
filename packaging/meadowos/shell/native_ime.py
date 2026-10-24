"""Ephemeral host-IME presentation for the native chat composer.

This bridge never commits text, reads the draft, or calls an Agent. Committed
characters continue through the existing RFB keyboard path. Only the native
caret geometry is public; preedit is consumed into native memory then unlinked.
"""
from __future__ import annotations

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

IME_TTL = 8
STATE_TTL = 6
MAX_BYTES = 8192
MAX_PREEDIT = 256
IDENTIFIER = re.compile(r"[A-Za-z0-9_.:-]{1,160}\Z")


class IMEError(Exception):
    def __init__(self, code, message, status=400):
        super().__init__(message)
        self.code, self.status = code, status


def _directory(root):
    root = Path(root).expanduser().absolute()
    if root.is_symlink():
        raise ValueError("IME spool must not be a symlink")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if not root.is_dir() or root.stat().st_mode & 0o077:
        raise ValueError("IME spool must be private (0700)")
    path = root / "ime"
    if path.is_symlink():
        raise ValueError("IME directory must not be a symlink")
    path.mkdir(mode=0o700, exist_ok=True)
    if not path.is_dir() or path.stat().st_mode & 0o077:
        raise ValueError("IME directory must be private (0700)")
    return path


def _write(path, value):
    fd, temporary = tempfile.mkstemp(prefix=".ime-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, ensure_ascii=False, allow_nan=False)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def _read(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise ValueError("IME file must be private and regular")
        payload = stream.read(MAX_BYTES + 1)
    if len(payload) > MAX_BYTES:
        raise ValueError("IME file exceeds limit")
    def constant(_value):
        raise ValueError("Nonfinite IME value")
    result = json.loads(payload, parse_constant=constant)
    if not isinstance(result, dict):
        raise ValueError("IME object required")
    return result


def _identifier(value):
    if not isinstance(value, str) or not IDENTIFIER.fullmatch(value):
        raise IMEError("invalid_id", "A bounded IME identifier is required")
    return value


def _number(value):
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)


def _caret(value):
    if not isinstance(value, dict) or set(value) != {"x", "y", "width", "height"}:
        raise ValueError("A caret rectangle is required")
    if not all(_number(value[key]) for key in value):
        raise ValueError("Invalid caret coordinate")
    if not all(abs(value[key]) <= 65536 for key in ("x", "y")):
        raise ValueError("Caret is outside the supported desktop")
    if not all(0 < value[key] <= 4096 for key in ("width", "height")):
        raise ValueError("Invalid caret size")
    return {key: value[key] for key in ("x", "y", "width", "height")}


def _preedit(value):
    active, text, cursor = value.get("active"), value.get("text"), value.get("cursor")
    if (not isinstance(active, bool) or not isinstance(text, str) or len(text) > MAX_PREEDIT
            or isinstance(cursor, bool) or not isinstance(cursor, int) or not 0 <= cursor <= len(text)
            or any(ord(char) < 32 and char != "\n" for char in text)):
        raise IMEError("invalid_preedit", "Invalid bounded IME composition")
    if not active and (text or cursor):
        raise IMEError("invalid_preedit", "An inactive composition must be empty")
    return {"active": active, "text": text, "cursor": cursor}


class IMEApplication:
    """Independent IME ownership on the existing loopback companion listener."""
    def __init__(self, root):
        self.path = _directory(root)
        self.lock = threading.RLock()
        self.owner = None
        self.sequence = -1

    def state(self):
        empty = {"version": 1, "epoch": "", "active": False, "caret": None, "updatedAt": 0}
        try:
            value = _read(self.path / "state.json")
            if (value.get("version") != 1 or not isinstance(value.get("active"), bool)
                    or not isinstance(value.get("epoch"), str) or not IDENTIFIER.fullmatch(value["epoch"])
                    or not _number(value.get("updatedAt"))):
                raise ValueError("Invalid native IME state")
            if not -5 <= time.time() - value["updatedAt"] <= STATE_TTL:
                return empty
            return {"version": 1, "epoch": value["epoch"], "active": value["active"],
                    "caret": _caret(value.get("caret")) if value["active"] else None,
                    "updatedAt": value["updatedAt"]}
        except FileNotFoundError:
            return empty
        except (OSError, ValueError, TypeError) as exc:
            raise IMEError("invalid_state", "Native IME state is unavailable", 503) from exc

    def _require_epoch(self, epoch):
        _identifier(epoch)
        state = self.state()
        if state["active"] is not True or state["epoch"] != epoch:
            raise IMEError("stale_epoch", "Native composer focus has changed", 409)
        return state

    def _expire(self, state):
        if self.owner and (not state["active"] or self.owner[1] != state["epoch"]
                           or self.owner[2] <= time.monotonic()):
            self.owner = None
            self.sequence = -1

    def lease(self, value):
        client, epoch = _identifier(value.get("clientId")), value.get("epoch")
        if not isinstance(value.get("release", False), bool):
            raise IMEError("invalid_release", "IME release must be a boolean")
        with self.lock:
            self._expire(self.state())
            if value.get("release"):
                if self.owner and self.owner[:2] == (client, epoch):
                    self._clear(epoch)
                    self.owner = None
                    self.sequence = -1
                return {"owned": False, "ttlSeconds": IME_TTL}
            self._require_epoch(epoch)
            if self.owner and self.owner[:2] != (client, epoch):
                return {"owned": False, "ttlSeconds": IME_TTL}
            self.owner = (client, epoch, time.monotonic() + IME_TTL)
            return {"owned": True, "ttlSeconds": IME_TTL}

    def _clear(self, epoch):
        _write(self.path / "preedit.json", {"epoch": epoch, "active": False,
               "text": "", "cursor": 0, "receivedAt": time.time()})

    def preedit(self, value, client):
        _identifier(client)
        epoch, sequence = value.get("epoch"), value.get("sequence")
        event = _preedit(value)
        if isinstance(sequence, bool) or not isinstance(sequence, int) or not 0 <= sequence <= 9007199254740991:
            raise IMEError("invalid_sequence", "A bounded increasing IME sequence is required")
        with self.lock:
            state = self._require_epoch(epoch)
            self._expire(state)
            if self.owner is None or self.owner[:2] != (client, epoch):
                raise IMEError("ime_not_owned", "IME ownership has changed or expired", 403)
            if sequence <= self.sequence:
                raise IMEError("stale_sequence", "An older IME update was discarded", 409)
            self._require_epoch(epoch)
            event.update(epoch=epoch, clientId=client, sequence=sequence, receivedAt=time.time())
            _write(self.path / "preedit.json", event)
            self.sequence = sequence
            return {"accepted": True, "sequence": sequence}


class NativeIMETransport:
    def __init__(self, state_dir, on_preedit):
        configured = os.environ.get("MEADOW_IME_SPOOL") or os.environ.get("MEADOW_VOICE_SPOOL")
        self.enabled = bool(configured)
        self.path = _directory(configured or Path(state_dir) / "ime-bridge") if self.enabled else None
        self.on_preedit = on_preedit
        self.closed, self.timer = False, None
        self.before_poll = None
        self.state = {"version": 1, "epoch": uuid.uuid4().hex, "active": False,
                      "caret": None, "updatedAt": 0}
        self.current = {"active": False, "text": "", "cursor": 0}
        self.deadline = 0
        self.publish()

    def start(self):
        if self.enabled and self.timer is None:
            from gi.repository import GLib
            self.timer = GLib.timeout_add(80, self.poll)

    def _clear(self):
        if self.current["active"] or self.current["text"]:
            self.current = {"active": False, "text": "", "cursor": 0}
            self.on_preedit(dict(self.current, epoch=self.state["epoch"]))
        self.deadline = 0

    def invalidate(self):
        self.state["epoch"] = uuid.uuid4().hex
        self._clear()
        self.publish()

    def publish(self, *, active=None, caret=None):
        if not self.enabled or self.closed:
            return
        if active is not None:
            if not isinstance(active, bool):
                raise ValueError("IME active must be a boolean")
            if active != self.state["active"]:
                self.state["epoch"] = uuid.uuid4().hex
                self._clear()
            self.state["active"] = active
        if caret is not None:
            self.state["caret"] = _caret(caret)
        if self.state["active"] and self.state["caret"] is None:
            raise ValueError("An active composer requires caret geometry")
        self.state["updatedAt"] = time.time()
        _write(self.path / "state.json", self.state)

    def poll(self):
        if self.closed or not self.enabled:
            return False
        if self.before_poll is not None:
            self.before_poll()
        if self.state["active"] and time.time() - self.state["updatedAt"] >= 2:
            self.publish()
        consume = self.path / (".consume-" + uuid.uuid4().hex)
        try:
            # Rename first: a concurrent producer can safely write the next
            # preedit.json while this exact snapshot is consumed and removed.
            os.replace(self.path / "preedit.json", consume)
            event = _read(consume)
            age = time.time() - event.get("receivedAt", 0)
            if (event.get("epoch") == self.state["epoch"] and self.state["active"]
                    and -5 <= age <= IME_TTL):
                current = _preedit(event)
                self.deadline = time.monotonic() + max(0, IME_TTL - age)
                if current != self.current:
                    self.current = current
                    self.on_preedit(dict(current, epoch=self.state["epoch"]))
        except (OSError, ValueError, TypeError, IMEError):
            pass
        finally:
            try:
                consume.unlink()
            except FileNotFoundError:
                pass
        if self.deadline and time.monotonic() >= self.deadline:
            self._clear()
        return True

    def close(self):
        if self.closed:
            return
        self.publish(active=False)
        self._clear()
        self.closed = True
        if self.timer is not None:
            from gi.repository import GLib
            GLib.source_remove(self.timer)
            self.timer = None
