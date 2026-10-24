#!/usr/bin/env python3
"""Host-local development audio transport for the native MeadowOS session.

This service never invokes an Agent, records a microphone, or downloads a model.
The trusted noVNC browser captures audio after normal microphone permission.
Shell owns state.json and consumes immutable event files in the private spool.
"""
from __future__ import annotations

import argparse
import importlib.util
import io
import json
import math
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import threading
import time
import uuid
import wave
from urllib.parse import parse_qs, unquote, urlsplit
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Share only the IME presentation contract, not voice/Agent state.
_ime_spec = importlib.util.spec_from_file_location(
    "meadow_native_ime", Path(__file__).resolve().parents[1] / "shell" / "native_ime.py")
_ime_module = importlib.util.module_from_spec(_ime_spec)
_ime_spec.loader.exec_module(_ime_module)
IMEApplication, IMEError = _ime_module.IMEApplication, _ime_module.IMEError
_attachment_spec = importlib.util.spec_from_file_location(
    "meadow_native_attachments", Path(__file__).resolve().parents[1] / "shell" / "native_attachments.py")
_attachment_module = importlib.util.module_from_spec(_attachment_spec)
_attachment_spec.loader.exec_module(_attachment_module)
AttachmentIngress, AttachmentIngressError = _attachment_module.AttachmentIngress, _attachment_module.AttachmentIngressError

MAX_AUDIO_SECONDS = 30
MAX_AUDIO_BYTES = 16000 * 2 * MAX_AUDIO_SECONDS + 4096
MAX_JSON_BYTES = 64 * 1024
MAX_TEXT = 12000
MAX_EVENTS = 256
LEASE_SECONDS = 8
STATE_TTL_SECONDS = 15
IDENTIFIER = re.compile(r"[A-Za-z0-9_.:-]{1,160}\Z")


def _reject_json_constant(_value):
    raise ValueError("Nonfinite JSON values are unsupported")


class VoiceError(Exception):
    def __init__(self, code, message, status=400):
        super().__init__(message)
        self.code, self.status = code, status


def decode_wav(payload):
    if not payload or len(payload) > MAX_AUDIO_BYTES:
        raise VoiceError("invalid_audio", "Audio exceeds the 30 second limit")
    try:
        with wave.open(io.BytesIO(payload), "rb") as stream:
            if (stream.getnchannels(), stream.getsampwidth(), stream.getframerate(), stream.getcomptype()) != (1, 2, 16000, "NONE"):
                raise VoiceError("invalid_audio", "Expected mono 16-bit PCM WAV at 16000 Hz")
            count = stream.getnframes()
            if count < 1600 or count > 16000 * MAX_AUDIO_SECONDS:
                raise VoiceError("invalid_audio", "Audio must be between 0.1 and 30 seconds")
            pcm = stream.readframes(count)
            if len(pcm) != count * 2:
                raise VoiceError("invalid_audio", "Audio is truncated")
            return pcm
    except (wave.Error, EOFError) as exc:
        raise VoiceError("invalid_audio", "Could not read PCM WAV") from exc


class VoiceSpool:
    def __init__(self, root):
        self.root = Path(root).expanduser().absolute()
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.root.is_symlink() or not self.root.is_dir() or self.root.stat().st_mode & 0o077:
            raise ValueError("Voice spool must be a private non-symlink directory (0700)")
        self.events = self.root / "events"
        self.events.mkdir(mode=0o700, exist_ok=True)
        if self.events.is_symlink() or not self.events.is_dir() or self.events.stat().st_mode & 0o077:
            raise ValueError("Voice events must be a private non-symlink directory (0700)")
        self.lock = threading.Lock()

    def state(self):
        try:
            fd = os.open(self.root / "state.json", os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            with os.fdopen(fd, "rb") as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
                    raise VoiceError("unsafe_state", "Native voice state is not private", 503)
                data = stream.read(MAX_JSON_BYTES + 1)
            if len(data) > MAX_JSON_BYTES:
                raise ValueError("state too large")
            value = json.loads(data, parse_constant=_reject_json_constant)
            if not isinstance(value, dict) or value.get("version") != 1:
                raise ValueError("invalid state version")
            if not isinstance(value.get("active"), bool) or not isinstance(value.get("epoch"), str):
                raise ValueError("invalid state lifecycle")
            if value.get("active") and not IDENTIFIER.fullmatch(value["epoch"]):
                raise ValueError("invalid active epoch")
            if value.get("mode") not in ("expanded", "compact"):
                raise ValueError("invalid display mode")
            if not isinstance(value.get("palette"), str) or len(value["palette"]) > 40:
                raise ValueError("invalid palette")
            if not isinstance(value.get("microphoneRequested"), bool):
                raise ValueError("invalid microphone state")
            updated = value.get("updatedAt")
            if isinstance(updated, bool) or not isinstance(updated, (float, int)) or not math.isfinite(updated):
                raise ValueError("missing native heartbeat")
            speech = value.get("speech")
            if speech is not None and (not isinstance(speech, dict)
                    or not isinstance(speech.get("id"), str) or not IDENTIFIER.fullmatch(speech["id"])
                    or not isinstance(speech.get("text"), str) or len(speech["text"]) > MAX_TEXT):
                raise ValueError("invalid speech state")
            # Native may store private diagnostics next to state. Only forward
            # the intentionally public presentation contract to the browser.
            public = {key: value.get(key) for key in ("version", "revision", "epoch", "active",
                    "mode", "palette", "microphoneRequested", "speech", "updatedAt")}
            if not -5 <= time.time() - updated <= STATE_TTL_SECONDS:
                public.update(active=False, microphoneRequested=False, speech=None)
            return public
        except FileNotFoundError:
            return {"version": 1, "revision": 0, "epoch": "", "active": False,
                    "mode": "compact", "palette": "ocean", "microphoneRequested": False,
                    "speech": None, "updatedAt": 0}
        except (OSError, ValueError, TypeError) as exc:
            raise VoiceError("invalid_state", "Native voice state is unavailable", 503) from exc

    def require_epoch(self, epoch):
        if not isinstance(epoch, str) or not IDENTIFIER.fullmatch(epoch):
            raise VoiceError("invalid_epoch", "A current voice epoch is required")
        state = self.state()
        if state.get("active") is not True or state.get("epoch") != epoch:
            raise VoiceError("stale_epoch", "Voice activation has ended", 409)
        return state

    def action(self, value):
        epoch = value.get("epoch")
        self.require_epoch(epoch)
        kind = value.get("type")
        result = {"type": kind, "epoch": epoch}
        if kind == "transcript":
            text = value.get("text")
            if not isinstance(text, str) or not text.strip() or len(text) > MAX_TEXT:
                raise VoiceError("invalid_action", "Transcript is empty or too long")
            result.update(text=text, id=_identifier(value.get("id")))
        elif kind == "microphone":
            if value.get("status") not in ("listening", "stopped", "denied", "error"):
                raise VoiceError("invalid_action", "Invalid microphone status")
            result["status"] = value["status"]
        elif kind == "speaking":
            if value.get("status") not in ("started", "finished", "error"):
                raise VoiceError("invalid_action", "Invalid playback status")
            result.update(status=value["status"], id=_identifier(value.get("id")))
        elif kind == "energy":
            energy = value.get("energy")
            if isinstance(energy, bool) or not isinstance(energy, (int, float)) or not 0 <= energy <= 1:
                raise VoiceError("invalid_action", "Energy must be between 0 and 1")
            result["energy"] = energy
        else:
            raise VoiceError("invalid_action", "Unsupported voice action")
        if "message" in value:
            if not isinstance(value["message"], str) or len(value["message"]) > 500:
                raise VoiceError("invalid_action", "Invalid status message")
            result["message"] = value["message"]
        with self.lock:
            if sum(1 for _ in self.events.glob("*.json")) >= MAX_EVENTS:
                raise VoiceError("event_queue_full", "Native voice event queue is full", 429)
            self.require_epoch(epoch)
            result.update(eventId=uuid.uuid4().hex, receivedAt=time.time())
            name = f"{time.time_ns():020d}-{result['eventId']}.json"
            fd, temporary = tempfile.mkstemp(prefix=".event-", dir=self.events)
            try:
                with os.fdopen(fd, "w") as stream:
                    json.dump(result, stream, ensure_ascii=False)
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, self.events / name)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        return {"accepted": True, "eventId": result["eventId"]}


def _identifier(value):
    if not isinstance(value, str) or not IDENTIFIER.fullmatch(value):
        raise VoiceError("invalid_id", "A bounded request identifier is required")
    return value


class LocalSpeechBackend:
    """One local MLX decoder, with macOS system TTS as the development voice."""
    def __init__(self, model_path, voice="Meijia", *, say_path="/usr/bin/say"):
        self.model_path = Path(model_path).expanduser().resolve()
        self.voice, self.say_path = voice, say_path
        self._transcriber = None

    def capabilities(self):
        model = ((self.model_path / "config.json").is_file()
                 and (self.model_path / "weights.safetensors").is_file())
        return {"asr": model and importlib.util.find_spec("mlx_whisper") is not None,
                "tts": bool(shutil.which(self.say_path)),
                "backend": "local-mlx-whisper+macos-say",
                "microphone": "browser-permission-required",
                "maxRecordingSeconds": MAX_AUDIO_SECONDS}

    def transcribe(self, payload):
        pcm = decode_wav(payload)
        if not self.capabilities()["asr"]:
            raise VoiceError("asr_unavailable", "The existing local Whisper model or runtime is unavailable", 503)
        # Enforce a local checkpoint before importing MLX; no Hub fallback.
        if self._transcriber is None:
            from mlx_whisper import transcribe
            self._transcriber = transcribe
        import numpy as np
        samples = np.frombuffer(pcm, dtype="<i2").astype(np.float32) / 32768.0
        if float(np.sqrt(np.mean(samples * samples))) < 0.0001:
            return {"text": "", "language": None}
        result = self._transcriber(samples, path_or_hf_repo=str(self.model_path),
                                   language=None, initial_prompt=None,
                                   condition_on_previous_text=False,
                                   temperature=0.0, no_speech_threshold=0.6,
                                   logprob_threshold=-1.0, verbose=None)
        return {"text": str(result.get("text", "")).strip(), "language": result.get("language")}

    def synthesize(self, text):
        if not self.capabilities()["tts"]:
            raise VoiceError("tts_unavailable", "The local system speech renderer is unavailable", 503)
        with tempfile.TemporaryDirectory(prefix="meadow-voice-") as directory:
            source = Path(directory) / "text.txt"
            output = Path(directory) / "speech.wav"
            source.write_text(text, encoding="utf-8")
            os.chmod(source, 0o600)
            try:
                subprocess.run([self.say_path, "-v", self.voice, "-r", "185", "-o", str(output),
                                "--data-format=LEI16@24000", "-f", str(source)],
                               check=True, stdin=subprocess.DEVNULL,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=90)
            except (subprocess.SubprocessError, OSError) as exc:
                raise VoiceError("tts_failed", "Local speech rendering failed", 503) from exc
            if not output.is_file() or output.stat().st_size > 32 * 1024 * 1024:
                raise VoiceError("tts_failed", "Local speech output is unavailable or too large", 503)
            return output.read_bytes()


class VoiceApplication:
    def __init__(self, spool, backend):
        self.spool, self.backend = spool, backend
        self.ime = IMEApplication(spool.root)
        self.attachments = AttachmentIngress(spool.root)
        self._inference = threading.Lock()
        self._owner_lock = threading.RLock()
        self._owner = None

    def lease(self, value):
        client_id = _identifier(value.get("clientId"))
        epoch = value.get("epoch")
        if not isinstance(value.get("release", False), bool):
            raise VoiceError("invalid_release", "Lease release must be a boolean")
        with self._owner_lock:
            state = self.spool.state()
            self._expire_owner(state)
            if value.get("release"):
                if self._owner and self._owner[:2] == (client_id, epoch):
                    self._owner = None
                return {"owned": False, "ttlSeconds": LEASE_SECONDS}
            self.spool.require_epoch(epoch)
            if self._owner and self._owner[:2] != (client_id, epoch):
                return {"owned": False, "ttlSeconds": LEASE_SECONDS}
            self._owner = (client_id, epoch, time.monotonic() + LEASE_SECONDS)
            return {"owned": True, "ttlSeconds": LEASE_SECONDS}

    def _expire_owner(self, state):
        if self._owner and (state.get("active") is not True or state.get("epoch") != self._owner[1]
                            or self._owner[2] <= time.monotonic()):
            self._owner = None

    def require_owner(self, client_id, epoch):
        _identifier(client_id)
        state = self.spool.require_epoch(epoch)
        with self._owner_lock:
            self._expire_owner(state)
            if self._owner is None or self._owner[:2] != (client_id, epoch):
                raise VoiceError("voice_not_owned", "Another browser owns voice, or the microphone lease expired", 403)
        return state

    def action(self, value, client_id):
        # Lease release/handoff and event admission must be linearized. A
        # transcript waiting behind the spool lock cannot outlive its owner.
        with self._owner_lock:
            state = self.require_owner(client_id, value.get("epoch"))
            if value.get("type") == "transcript" and state.get("microphoneRequested") is not True:
                raise VoiceError("microphone_stopped", "Voice input has stopped", 409)
            return self.spool.action(value)

    def snapshot(self):
        return {"native": self.spool.state(), "capabilities": self.backend.capabilities(),
                "serverTime": time.time()}

    def transcribe(self, payload, epoch, client_id):
        state = self.require_owner(client_id, epoch)
        if state.get("microphoneRequested") is not True:
            raise VoiceError("microphone_stopped", "Voice input has stopped", 409)
        decode_wav(payload)
        result = self._run(epoch, client_id, lambda: dict(self.backend.transcribe(payload), epoch=epoch))
        if self.spool.require_epoch(epoch).get("microphoneRequested") is not True:
            raise VoiceError("microphone_stopped", "Voice input has stopped", 409)
        return result

    def synthesize(self, value, client_id):
        epoch = value.get("epoch")
        self.require_owner(client_id, epoch)
        _identifier(value.get("id"))
        text = value.get("text")
        if not isinstance(text, str) or not text.strip() or len(text) > MAX_TEXT:
            raise VoiceError("invalid_text", "Speech text is empty or too long")
        return self._run(epoch, client_id, lambda: self.backend.synthesize(text))

    def _run(self, epoch, client_id, callback):
        if not self._inference.acquire(blocking=False):
            raise VoiceError("speech_busy", "Local speech is processing another turn", 429)
        try:
            self.require_owner(client_id, epoch)
            result = callback()
            self.require_owner(client_id, epoch)  # MLX decode cannot be interrupted mid-call.
            return result
        finally:
            self._inference.release()


class VoiceHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = False

    def __init__(self, address, application, origins):
        if address[0] != "127.0.0.1":
            raise ValueError("Voice adapter must bind to IPv4 loopback")
        self.application = application
        self.origins = frozenset(origins)
        if not self.origins or any(not re.fullmatch(r"http://127\.0\.0\.1:[0-9]{1,5}", origin) for origin in self.origins):
            raise ValueError("Trusted origins must be explicit loopback HTTP ports")
        super().__init__(address, VoiceHandler)


class VoiceHandler(BaseHTTPRequestHandler):
    server_version = "MeadowVoice/1"

    def log_message(self, *args):
        pass  # Never log transcript, payload, or provider information.

    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def _admit(self, preflight=False):
        for name in ("Host", "Origin", "X-Meadow-Voice", "Content-Length", "Content-Type", "X-Meadow-Voice-Epoch", "X-Meadow-Voice-Client", "X-Meadow-IME", "X-Meadow-IME-Client", "X-Meadow-Attachments", "X-Meadow-Attachments-Client", "X-Meadow-Attachments-Meta", "Transfer-Encoding"):
            if len(self.headers.get_all(name, [])) > 1:
                raise VoiceError("duplicate_header", "Ambiguous voice request headers", 400)
        if self.headers.get("Host") != f"127.0.0.1:{self.server.server_port}":
            raise VoiceError("forbidden_host", "Untrusted voice host", 403)
        if self.headers.get("Origin") not in self.server.origins:
            raise VoiceError("forbidden_origin", "Untrusted voice origin", 403)
        header = ("X-Meadow-Attachments" if self.path.startswith("/attachments/") else
                  "X-Meadow-IME" if self.path.startswith("/ime/") else "X-Meadow-Voice")
        if not preflight and self.headers.get(header) != "1":
            raise VoiceError("missing_client_header", "Companion client header is required", 403)

    def _headers(self, status, content_type, length):
        self.send_response(status)
        if self.headers.get("Origin") in self.server.origins:
            self.send_header("Access-Control-Allow-Origin", self.headers["Origin"])
            self.send_header("Vary", "Origin")
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(length))

    def _json(self, status, value):
        body = json.dumps(value, ensure_ascii=False, allow_nan=False).encode("utf-8")
        self._headers(status, "application/json; charset=utf-8", len(body))
        self.end_headers()
        self.wfile.write(body)

    def _failure(self, error):
        try:
            self._json(error.status, {"error": error.code, "message": str(error)})
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            pass

    def do_OPTIONS(self):
        try:
            self._admit(preflight=True)
            if self.headers.get("Access-Control-Request-Method") not in ("GET", "POST"):
                raise VoiceError("invalid_method", "Unsupported voice method", 405)
            requested = {v.strip().lower() for v in self.headers.get("Access-Control-Request-Headers", "").split(",") if v.strip()}
            if not requested <= {"content-type", "x-meadow-voice", "x-meadow-voice-epoch", "x-meadow-voice-client", "x-meadow-ime", "x-meadow-ime-client", "x-meadow-attachments", "x-meadow-attachments-client", "x-meadow-attachments-meta"}:
                raise VoiceError("invalid_headers", "Unsupported voice headers", 403)
            self._headers(204, "text/plain", 0)
            self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            self.send_header("Access-Control-Allow-Headers", "Content-Type, X-Meadow-Voice, X-Meadow-Voice-Epoch, X-Meadow-Voice-Client, X-Meadow-IME, X-Meadow-IME-Client, X-Meadow-Attachments, X-Meadow-Attachments-Client, X-Meadow-Attachments-Meta")
            self.send_header("Access-Control-Max-Age", "600")
            self.end_headers()
        except (VoiceError, IMEError, AttachmentIngressError) as exc:
            self._failure(exc)

    def do_GET(self):
        try:
            self._admit()
            if self.path == "/attachments/state":
                self._json(200, self.server.application.attachments.state())
            elif urlsplit(self.path).path == "/attachments/receipt":
                try:
                    query = parse_qs(urlsplit(self.path).query, strict_parsing=True)
                except ValueError as exc:
                    raise VoiceError("invalid_receipt", "Attachment receipt query is invalid") from exc
                if set(query) != {"eventId"} or len(query["eventId"]) != 1:
                    raise VoiceError("invalid_receipt", "One attachment receipt is required")
                self._json(200, self.server.application.attachments.receipt(
                    query["eventId"][0], self.headers.get("X-Meadow-Attachments-Client")))
            elif self.path == "/ime/state":
                self._json(200, self.server.application.ime.state())
            elif self.path == "/voice/state":
                self._json(200, self.server.application.snapshot())
            elif self.path == "/voice/status":
                self._json(200, self.server.application.backend.capabilities())
            else:
                raise VoiceError("not_found", "Unknown voice route", 404)
        except (VoiceError, IMEError, AttachmentIngressError) as exc:
            self._failure(exc)

    def _body(self, limit):
        if self.headers.get("Transfer-Encoding"):
            raise VoiceError("invalid_body", "Chunked voice uploads are unsupported")
        try:
            length = int(self.headers.get("Content-Length", ""))
        except ValueError as exc:
            raise VoiceError("invalid_body", "Content-Length is required", 411) from exc
        if length < 1 or length > limit:
            raise VoiceError("body_too_large", "Voice request size is outside the limit", 413)
        body = self.rfile.read(length)
        if len(body) != length:
            raise VoiceError("invalid_body", "Voice upload is truncated")
        return body

    def do_POST(self):
        try:
            self._admit()
            app = self.server.application
            if self.path == "/attachments/upload":
                if self.headers.get("Content-Type", "").split(";", 1)[0] != "application/octet-stream":
                    raise VoiceError("invalid_media_type", "Expected application/octet-stream", 415)
                raw = self.headers.get("X-Meadow-Attachments-Meta", "")
                if not raw or len(raw) > 8192:
                    raise VoiceError("invalid_metadata", "A bounded attachment descriptor is required")
                try:
                    value = json.loads(unquote(raw, errors="strict"), parse_constant=_reject_json_constant)
                    if not isinstance(value, dict):
                        raise ValueError("not an object")
                except (ValueError, UnicodeError) as exc:
                    raise VoiceError("invalid_metadata", "Attachment descriptor is invalid") from exc
                # Verify metadata before allocating the bounded original body.
                client = self.headers.get("X-Meadow-Attachments-Client")
                app.attachments.admit_upload(value, client)
                if not app.attachments.upload_lock.acquire(blocking=False):
                    raise AttachmentIngressError("attachment_busy", "Another original image is being received; retry this file", 429)
                try:
                    self._json(202, app.attachments.upload(self._body(_attachment_module.MAX_FILE_BYTES), value, client))
                finally:
                    app.attachments.upload_lock.release()
            elif self.path == "/attachments/lease":
                if self.headers.get("Content-Type", "").split(";", 1)[0] != "application/json":
                    raise VoiceError("invalid_media_type", "Expected application/json", 415)
                try:
                    value = json.loads(self._body(_attachment_module.MAX_JSON_BYTES), parse_constant=_reject_json_constant)
                    if not isinstance(value, dict):
                        raise ValueError("not an object")
                except (ValueError, UnicodeError) as exc:
                    raise VoiceError("invalid_json", "Expected a finite JSON object") from exc
                if value.get("clientId") != self.headers.get("X-Meadow-Attachments-Client"):
                    raise VoiceError("invalid_client", "Attachment lease client mismatch", 403)
                self._json(200, app.attachments.lease(value))
            elif self.path in ("/ime/lease", "/ime/preedit"):
                if self.headers.get("Content-Type", "").split(";", 1)[0] != "application/json":
                    raise VoiceError("invalid_media_type", "Expected application/json", 415)
                try:
                    value = json.loads(self._body(_ime_module.MAX_BYTES), parse_constant=_reject_json_constant)
                    if not isinstance(value, dict):
                        raise ValueError("not an object")
                except (ValueError, UnicodeError) as exc:
                    raise VoiceError("invalid_json", "Expected a finite JSON object") from exc
                if self.path == "/ime/lease":
                    self._json(200, app.ime.lease(value))
                else:
                    self._json(202, app.ime.preedit(value, self.headers.get("X-Meadow-IME-Client")))
            elif self.path == "/voice/transcribe":
                if self.headers.get("Content-Type", "").split(";", 1)[0] not in ("audio/wav", "audio/wave"):
                    raise VoiceError("invalid_media_type", "Expected audio/wav", 415)
                payload = self._body(MAX_AUDIO_BYTES)
                self._json(200, app.transcribe(payload, self.headers.get("X-Meadow-Voice-Epoch"), self.headers.get("X-Meadow-Voice-Client")))
            elif self.path in ("/voice/action", "/voice/synthesize", "/voice/lease"):
                if self.headers.get("Content-Type", "").split(";", 1)[0] != "application/json":
                    raise VoiceError("invalid_media_type", "Expected application/json", 415)
                try:
                    value = json.loads(self._body(MAX_JSON_BYTES))
                    if not isinstance(value, dict):
                        raise ValueError("not an object")
                except (ValueError, UnicodeError) as exc:
                    raise VoiceError("invalid_json", "Expected a JSON object") from exc
                if self.path == "/voice/action":
                    self._json(202, app.action(value, self.headers.get("X-Meadow-Voice-Client")))
                elif self.path == "/voice/lease":
                    self._json(200, app.lease(value))
                else:
                    audio = app.synthesize(value, self.headers.get("X-Meadow-Voice-Client"))
                    self._headers(200, "audio/wav", len(audio))
                    self.end_headers()
                    self.wfile.write(audio)
            else:
                raise VoiceError("not_found", "Unknown voice route", 404)
        except (VoiceError, IMEError, AttachmentIngressError) as exc:
            self._failure(exc)
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            pass
        except Exception:
            if self.path.startswith("/attachments/"):
                self._failure(VoiceError("attachment_ingress_failed", "Native image ingress failed", 503))
            else:
                self._failure(VoiceError("speech_failed", "Local speech processing failed", 503))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--spool", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--voice", default="Meijia")
    parser.add_argument("--port", type=int, default=60531)
    parser.add_argument("--origin", action="append", default=[])
    args = parser.parse_args()
    backend = LocalSpeechBackend(args.model, args.voice)
    app = VoiceApplication(VoiceSpool(args.spool), backend)
    server = VoiceHTTPServer(("127.0.0.1", args.port), app,
                             args.origin or ["http://127.0.0.1:60511", "http://127.0.0.1:60512"])
    print(json.dumps({"listening": f"http://127.0.0.1:{server.server_port}",
                      "capabilities": backend.capabilities()}), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
