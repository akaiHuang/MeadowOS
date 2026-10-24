"""Presentation transport for a local VoiceOS audio companion.

The audio companion never receives Agent credentials. Final recognized text
enters the same native chat through the Shell callback, with ordinary admission
and permissions. The development VM uses a private shared spool because VNC
does not carry microphone or speaker audio.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import stat
import tempfile
import time
import uuid


def _write(path, value):
    fd, temporary = tempfile.mkstemp(prefix=".voice-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, ensure_ascii=False)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class NativeVoiceTransport:
    def __init__(self, state_dir, on_event):
        configured = os.environ.get("MEADOW_VOICE_SPOOL")
        self.enabled = bool(configured)
        self.path = Path(configured) if configured else Path(state_dir) / "voice-bridge"
        self.on_event = on_event
        self.timer = None
        self.closed = False
        self.state = {"version": 1, "revision": 0, "epoch": str(uuid.uuid4()),
                      "active": False, "mode": "compact", "palette": "meadow",
                      "microphoneRequested": False, "speech": None}
        self.seen = set()
        self.transcripts = set()
        if self.enabled:
            if not self.path.is_absolute() or self.path.is_symlink():
                raise ValueError("Voice spool must be an absolute private directory")
            self.path.mkdir(parents=True, exist_ok=True, mode=0o700)
            if self.path.stat().st_mode & 0o077:
                raise ValueError("Voice spool must be private")
            (self.path / "events").mkdir(exist_ok=True, mode=0o700)
            self.publish()

    def start(self):
        if self.enabled and self.timer is None:
            from gi.repository import GLib
            self.timer = GLib.timeout_add(120, self.poll)

    def publish(self, **fields):
        if not self.enabled or self.closed:
            return
        allowed = {"active", "mode", "palette", "microphoneRequested", "speech"}
        changed = {key: value for key, value in fields.items()
                   if key in allowed and self.state.get(key) != value}
        if "active" in changed or changed.get("microphoneRequested") is False:
            self.state["epoch"] = str(uuid.uuid4())
            self.state["speech"] = None
        self.state.update(changed)
        self.state["revision"] += 1
        self.state["updatedAt"] = time.time()
        _write(self.path / "state.json", self.state)

    def toggle_microphone(self):
        if not self.enabled:
            self.on_event({"type": "error", "message": "語音服務尚未連接。"})
            return
        self.publish(microphoneRequested=not self.state["microphoneRequested"])

    def speak(self, text):
        if self.enabled and self.state["active"] and isinstance(text, str) and text.strip():
            self.publish(speech={"id": str(uuid.uuid4()), "text": text})

    def poll(self):
        if self.closed:
            return False
        if self.state["active"] and time.time() - self.state.get("updatedAt", 0) >= 2:
            self.publish()
        for path in sorted((self.path / "events").glob("*.json"))[:64]:
            try:
                info = path.lstat()
                if not stat.S_ISREG(info.st_mode) or info.st_size > 64 * 1024:
                    path.unlink()
                    continue
                event = json.loads(path.read_text())
                event_id = event.get("eventId") or event.get("id")
                path.unlink()
                if (not isinstance(event_id, str) or event_id in self.seen or
                        event.get("epoch") != self.state["epoch"] or not self.state["active"]):
                    continue
                self.seen.add(event_id)
                if len(self.seen) > 2048:
                    self.seen = {event_id}
                if event.get("type") == "transcript":
                    if not self.state["microphoneRequested"]:
                        continue
                    transcript_id = event.get("id")
                    if not isinstance(transcript_id, str) or transcript_id in self.transcripts:
                        continue
                    self.transcripts.add(transcript_id)
                self.on_event(event)
            except (OSError, ValueError, TypeError, AttributeError):
                continue
        return True

    def close(self):
        if self.closed:
            return
        self.publish(active=False, microphoneRequested=False, speech=None)
        self.closed = True
        if self.timer is not None:
            from gi.repository import GLib
            GLib.source_remove(self.timer)
            self.timer = None
