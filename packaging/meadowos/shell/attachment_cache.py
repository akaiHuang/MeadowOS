"""Private, byte-budgeted originals for the native Shell composer.

The cache has no attachment count limit. Callers page through ``list`` and
upload one cached original at a time. A submitted original is immutable and is
retained across Shell restarts; removing a composer chip never removes a
submitted task's source. Paths in this module are local presentation state and
must not be sent as model-readable image evidence.
"""
from __future__ import annotations

from contextlib import contextmanager
import errno
import fcntl
import hashlib
import heapq
import io
import json
import os
from pathlib import Path
import re
import stat
import struct
import tempfile
import threading
import time
import uuid
import zlib

MAX_FILE_BYTES = 20 * 1024 * 1024
MAX_TOTAL_BYTES = 1024 * 1024 * 1024
_ID = re.compile(r"[0-9]{20}-[a-f0-9]{32}\Z")
_MAX_METADATA = 32 * 1024


class AttachmentCacheError(Exception):
    def __init__(self, message, *, code="attachment_cache_error"):
        super().__init__(message)
        self.code = code


def _safe_absolute(value):
    path = Path(value)
    if not path.is_absolute() or "\x00" in str(path):
        raise AttachmentCacheError("Attachment path must be absolute", code="invalid_path")
    return Path(os.path.normpath(path))


def _no_symlink_components(path):
    for component in [*reversed(path.parents), path]:
        try:
            info = component.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode):
            raise AttachmentCacheError("Attachment path contains a symlink", code="unsafe_path")


def _open_source(path):
    """Pin each directory descriptor so parent swaps cannot escape checks."""
    directory = os.open(path.anchor, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        for component in path.parts[1:-1]:
            next_directory = os.open(component, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
                                     | getattr(os, "O_NOFOLLOW", 0), dir_fd=directory)
            os.close(directory)
            directory = next_directory
        return os.open(path.name, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
                       | getattr(os, "O_NONBLOCK", 0), dir_fd=directory)
    finally:
        os.close(directory)


def _image_type(data):
    """Validate complete containers without trusting filename extensions.

    Pillow, when installed, additionally verifies the decoder's format. The
    built-in checks remain available in the minimal packaged Python runtime.
    They inspect lengths, terminators and PNG CRCs without allocating pixels.
    """
    mime, width, height = "", 0, 0
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        cursor, seen_data, ended = 8, False, False
        while cursor + 12 <= len(data):
            size = struct.unpack_from(">I", data, cursor)[0]
            end = cursor + 12 + size
            if end > len(data):
                break
            kind = data[cursor + 4:cursor + 8]
            payload = data[cursor + 8:cursor + 8 + size]
            checksum = struct.unpack_from(">I", data, cursor + 8 + size)[0]
            if zlib.crc32(kind + payload) & 0xffffffff != checksum:
                break
            if cursor == 8:
                if kind != b"IHDR" or size != 13:
                    break
                width, height = struct.unpack_from(">II", payload)
            if kind == b"IDAT":
                seen_data = True
            if kind == b"IEND":
                ended = size == 0 and end == len(data)
                break
            cursor = end
        if ended and seen_data:
            mime = "image/png"
    elif data.startswith(b"\xff\xd8") and data.endswith(b"\xff\xd9"):
        cursor = 2
        while cursor + 4 <= len(data):
            if data[cursor] != 0xff:
                break
            while cursor < len(data) and data[cursor] == 0xff:
                cursor += 1
            if cursor >= len(data):
                break
            marker, cursor = data[cursor], cursor + 1
            if marker in (0x01, *range(0xd0, 0xda)):
                continue
            if cursor + 2 > len(data):
                break
            size = struct.unpack_from(">H", data, cursor)[0]
            if size < 2 or cursor + size > len(data):
                break
            if marker in (0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf) and size >= 8:
                height, width = struct.unpack_from(">HH", data, cursor + 3)
            if marker == 0xda:
                if width and height:
                    mime = "image/jpeg"
                break
            cursor += size
    elif data[:6] in (b"GIF87a", b"GIF89a") and len(data) >= 14:
        width, height = struct.unpack_from("<HH", data, 6)
        cursor = 13 + (3 * (2 << (data[10] & 7)) if data[10] & 0x80 else 0)
        seen_image, ended = False, False
        while cursor < len(data):
            marker, cursor = data[cursor], cursor + 1
            if marker == 0x3b:
                ended = cursor == len(data)
                break
            if marker == 0x21:
                cursor += 1  # Extension label precedes length-prefixed blocks.
            elif marker == 0x2c:
                if cursor + 9 > len(data):
                    break
                packed = data[cursor + 8]
                cursor += 9 + (3 * (2 << (packed & 7)) if packed & 0x80 else 0)
                cursor += 1  # LZW minimum code size.
                seen_image = True
            else:
                break
            while cursor < len(data):
                size, cursor = data[cursor], cursor + 1
                if not size:
                    break
                cursor += size
            if cursor > len(data):
                break
        if ended and seen_image:
            mime = "image/gif"
    elif len(data) >= 20 and data.startswith(b"RIFF") and data[8:12] == b"WEBP":
        total = struct.unpack_from("<I", data, 4)[0] + 8
        cursor, seen_image = 12, False
        while cursor + 8 <= len(data):
            kind, size = data[cursor:cursor + 4], struct.unpack_from("<I", data, cursor + 4)[0]
            payload = data[cursor + 8:cursor + 8 + size]
            if len(payload) != size:
                break
            if kind == b"VP8 " and len(payload) >= 10 and payload[3:6] == b"\x9d\x01\x2a":
                width, height = (v & 0x3fff for v in struct.unpack_from("<HH", payload, 6))
                seen_image = True
            elif kind == b"VP8L" and len(payload) >= 5 and payload[0] == 0x2f:
                bits = struct.unpack_from("<I", payload, 1)[0]
                width, height, seen_image = (bits & 0x3fff) + 1, ((bits >> 14) & 0x3fff) + 1, True
            elif kind == b"VP8X" and len(payload) == 10:
                width = int.from_bytes(payload[4:7], "little") + 1
                height = int.from_bytes(payload[7:10], "little") + 1
            elif kind == b"ANMF" and len(payload) >= 16:
                seen_image = True
            cursor += 8 + size + (size & 1)
        if seen_image and cursor == total == len(data):
            mime = "image/webp"
    if not mime or width <= 0 or height <= 0:
        raise AttachmentCacheError("File is not a complete supported PNG, JPEG, GIF, or WebP image", code="invalid_image")
    try:
        from PIL import Image
    except ImportError:
        pass
    else:
        try:
            with Image.open(io.BytesIO(data)) as image:
                if Image.MIME.get(image.format) != mime:
                    raise ValueError("image format mismatch")
                image.verify()
        except Exception as exc:
            raise AttachmentCacheError("Image decoder rejected this file", code="invalid_image") from exc
    return mime, width, height


class AttachmentCache:
    def __init__(self, root, *, max_file_bytes=MAX_FILE_BYTES, max_total_bytes=MAX_TOTAL_BYTES):
        self.root = _safe_absolute(root)
        if max_file_bytes <= 0 or max_total_bytes <= 0:
            raise ValueError("Attachment byte budgets must be positive")
        self.max_file_bytes, self.max_total_bytes = max_file_bytes, max_total_bytes
        self._thread_lock = threading.RLock()
        _no_symlink_components(self.root)
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        info = self.root.stat()
        if info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise AttachmentCacheError("Attachment cache must be user-private", code="unsafe_cache")
        self.objects = self.root / "objects"
        self.objects.mkdir(mode=0o700, exist_ok=True)
        self._check_private(self.objects, directory=True)
        self._lock_path = self.root / ".lock"
        with self._locked():
            # Recovery discards only writes that never obtained a manifest.
            # Every submitted original still has its durable sidecar.
            for entry in os.scandir(self.objects):
                if entry.name.startswith(".attachment-") or (entry.name.endswith(".image")
                        and _ID.fullmatch(entry.name[:-6])
                        and not (self.objects / (entry.name[:-6] + ".json")).exists()):
                    self._check_private(Path(entry.path))
                    os.unlink(entry.path)

    @staticmethod
    def _check_private(path, *, directory=False):
        info = path.lstat()
        correct_type = stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)
        if not correct_type or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise AttachmentCacheError("Attachment cache entry is unsafe", code="unsafe_cache")
        return info

    @contextmanager
    def _locked(self):
        with self._thread_lock:
            self._check_private(self.root, directory=True)
            self._check_private(self.objects, directory=True)
            fd = os.open(self._lock_path, os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600)
            try:
                self._check_private(self._lock_path)
                fcntl.flock(fd, fcntl.LOCK_EX)
                yield
            finally:
                os.close(fd)

    def _paths(self, identity):
        if not isinstance(identity, str) or not _ID.fullmatch(identity):
            raise AttachmentCacheError("Attachment ID is invalid", code="invalid_attachment_id")
        return self.objects / (identity + ".image"), self.objects / (identity + ".json")

    def _read(self, identity):
        original, metadata = self._paths(identity)
        try:
            self._check_private(metadata)
            with metadata.open("rb") as source:
                raw = source.read(_MAX_METADATA + 1)
            record = json.loads(raw)
            info = self._check_private(original)
            if (len(raw) > _MAX_METADATA or not isinstance(record, dict)
                    or record.get("id") != identity or record.get("path") != str(original)
                    or record.get("size") != info.st_size or not isinstance(record.get("sha256"), str)):
                raise ValueError("invalid metadata")
            return record
        except FileNotFoundError as exc:
            raise AttachmentCacheError("Attachment is unavailable", code="attachment_missing") from exc
        except (ValueError, UnicodeError) as exc:
            raise AttachmentCacheError("Attachment metadata is invalid", code="invalid_metadata") from exc

    def _write(self, path, data):
        fd, temporary = tempfile.mkstemp(prefix=".attachment-", dir=self.objects)
        try:
            with os.fdopen(fd, "wb") as target:
                target.write(data)
                target.flush()
                os.fsync(target.fileno())
            os.replace(temporary, path)
            directory = os.open(self.objects, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    @staticmethod
    def _encode(record):
        return json.dumps(record, ensure_ascii=False, separators=(",", ":")).encode("utf-8")

    def _usage(self):
        used = 0
        for entry in os.scandir(self.objects):
            if entry.name.endswith((".image", ".json")):
                info = self._check_private(Path(entry.path))
                used += info.st_size
        return used

    def add(self, path):
        source_path = _safe_absolute(path)
        _no_symlink_components(source_path)
        try:
            fd = _open_source(source_path)
            with os.fdopen(fd, "rb") as source:
                before = os.fstat(source.fileno())
                if not stat.S_ISREG(before.st_mode):
                    raise AttachmentCacheError("Attachment must be a regular file", code="invalid_file")
                if before.st_size <= 0 or before.st_size > self.max_file_bytes:
                    raise AttachmentCacheError(f"Image exceeds the {self.max_file_bytes}-byte per-file budget", code="file_too_large")
                data = source.read(self.max_file_bytes + 1)
                after = os.fstat(source.fileno())
                if len(data) != before.st_size or after.st_size != before.st_size or after.st_mtime_ns != before.st_mtime_ns:
                    raise AttachmentCacheError("Attachment changed while being staged", code="source_changed")
            return self.add_bytes(data, source_path.name)
        except OSError as exc:
            code = "cache_full" if exc.errno in (errno.ENOSPC, errno.EDQUOT) else "attachment_io_error"
            raise AttachmentCacheError("Cannot stage attachment: " + str(exc.strerror or "filesystem error"), code=code) from exc

    def add_bytes(self, data, name, mime=None):
        """Stage one already-received bridge file without resolving a host path."""
        if not isinstance(data, (bytes, bytearray, memoryview)):
            raise AttachmentCacheError("Attachment data must contain bytes", code="invalid_file")
        if len(data) <= 0 or len(data) > self.max_file_bytes:
            raise AttachmentCacheError(f"Image exceeds the {self.max_file_bytes}-byte per-file budget", code="file_too_large")
        if (not isinstance(name, str) or not name or len(name.encode("utf-8")) > 1024
                or name in (".", "..") or any(char in name for char in ("/", "\\", "\x00"))
                or any(ord(char) < 32 for char in name)):
            raise AttachmentCacheError("Attachment filename must be a simple display name", code="invalid_name")
        data = bytes(data)
        actual_mime, width, height = _image_type(data)
        if mime is not None and mime != actual_mime:
            raise AttachmentCacheError("Attachment MIME does not match its image bytes", code="mime_mismatch")
        identity = f"{time.time_ns():020d}-{uuid.uuid4().hex}"
        original, metadata = self._paths(identity)
        record = {"id": identity, "name": name, "path": str(original),
                  "mime": actual_mime, "size": len(data), "sha256": hashlib.sha256(data).hexdigest(),
                  "width": width, "height": height, "state": "staged"}
        encoded = self._encode(record)
        try:
            with self._locked():
                if self._usage() + len(data) + len(encoded) > self.max_total_bytes:
                    raise AttachmentCacheError(f"Attachment cache reached its {self.max_total_bytes}-byte disk budget; remove unused drafts or choose a larger budget", code="cache_full")
                self._write(original, data)
                try:
                    self._write(metadata, encoded)
                except Exception:
                    original.unlink(missing_ok=True)
                    raise
            return record
        except OSError as exc:
            code = "cache_full" if exc.errno in (errno.ENOSPC, errno.EDQUOT) else "attachment_io_error"
            raise AttachmentCacheError("Cannot stage attachment: " + str(exc.strerror or "filesystem error"), code=code) from exc

    def get(self, identity):
        with self._locked():
            record = self._read(identity)
            fd = _open_source(Path(record["path"]))
            with os.fdopen(fd, "rb") as source:
                digest, count = hashlib.sha256(), 0
                for block in iter(lambda: source.read(1024 * 1024), b""):
                    count += len(block)
                    if count > self.max_file_bytes:
                        raise AttachmentCacheError("Cached original exceeded its byte budget", code="source_changed")
                    digest.update(block)
            if count != record["size"] or digest.hexdigest() != record["sha256"]:
                raise AttachmentCacheError("Cached original no longer matches its source hash", code="source_changed")
            return record

    def list(self, *, cursor=None, limit=100):
        if cursor is not None:
            self._paths(cursor)
        if type(limit) is not int or not 1 <= limit <= 1000:
            raise AttachmentCacheError("Page size must be between 1 and 1000", code="invalid_page")
        with self._locked():
            # Only page-size identifiers enter memory, even for large caches.
            identities = heapq.nsmallest(limit + 1, (entry.name[:-5] for entry in os.scandir(self.objects)
                if entry.name.endswith(".json") and _ID.fullmatch(entry.name[:-5])
                and (cursor is None or entry.name[:-5] > cursor)))
            items = [self._read(identity) for identity in identities[:limit]]
            return {"items": items, "next_cursor": identities[limit - 1] if len(identities) > limit else None}

    def remove(self, identity):
        with self._locked():
            try:
                record = self._read(identity)
            except AttachmentCacheError as exc:
                if exc.code == "attachment_missing":
                    return False
                raise
            if record["state"] == "submitted":
                raise AttachmentCacheError("Submitted originals are retained with their task", code="attachment_submitted")
            original, metadata = self._paths(identity)
            metadata.unlink()
            original.unlink()
            return True

    def _update(self, identity, changes):
        with self._locked():
            record = self._read(identity)
            if record["state"] == "submitted":
                raise AttachmentCacheError("Submitted originals are immutable", code="attachment_submitted")
            record.update(changes)
            metadata = self._paths(identity)[1]
            encoded = self._encode(record)
            if self._usage() - metadata.stat().st_size + len(encoded) > self.max_total_bytes:
                raise AttachmentCacheError("Attachment cache reached its byte budget", code="cache_full")
            self._write(metadata, encoded)
            return record

    def mark_uploaded(self, identity, reference):
        def valid(value):
            return isinstance(value, str) and value and len(value) <= 512 and not any(ord(char) < 32 for char in value)
        if isinstance(reference, dict):
            if set(reference) != {"collection_id", "image_id", "session_id"} or not all(valid(value) for value in reference.values()):
                raise AttachmentCacheError("Agent attachment receipt is invalid", code="invalid_reference")
            reference = dict(reference)
        elif not valid(reference):
            raise AttachmentCacheError("Agent attachment reference is invalid", code="invalid_reference")
        return self._update(identity, {"state": "uploaded", "ref": reference, "error": None})

    def mark_submitted(self, identity):
        with self._locked():
            record = self._read(identity)
            if record["state"] == "submitted":
                return record
            if record["state"] != "uploaded" or not record.get("ref"):
                raise AttachmentCacheError("Upload attachment before submitting it", code="attachment_not_uploaded")
            record["state"] = "submitted"
            metadata = self._paths(identity)[1]
            encoded = self._encode(record)
            if self._usage() - metadata.stat().st_size + len(encoded) > self.max_total_bytes:
                raise AttachmentCacheError("Attachment cache reached its byte budget", code="cache_full")
            self._write(metadata, encoded)
            return record

    def set_failed(self, identity, message, *, code="upload_failed"):
        return self._update(identity, {"state": "failed", "error": {"code": str(code)[:128], "message": str(message)[:2048]}})
