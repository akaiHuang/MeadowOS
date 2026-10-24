"""Offline private original caching and unbounded collection paging contract."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import io
import json
import os
from pathlib import Path
import struct
import sys
import tempfile
import unittest
from unittest.mock import patch
import zlib

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from attachment_cache import AttachmentCache, AttachmentCacheError, _image_type


def png_bytes():
    def chunk(kind, payload):
        return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", zlib.crc32(kind + payload) & 0xffffffff)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(b"\x00\xff\x00\x00")) + chunk(b"IEND", b""))


class AttachmentCacheTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="meadow-attachments-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.source = self.root / "original.weird-extension"
        self.original = png_bytes()
        self.source.write_bytes(self.original)
        self.cache = AttachmentCache(self.root / "cache")

    def test_original_is_byte_exact_and_owned_private(self):
        item = self.cache.add(self.source)
        self.assertEqual(item["mime"], "image/png")
        self.assertEqual(item["name"], self.source.name)
        self.assertEqual(item["sha256"], hashlib.sha256(self.original).hexdigest())
        self.assertEqual((item["width"], item["height"]), (1, 1))
        self.assertEqual(Path(item["path"]).read_bytes(), self.original)
        self.source.write_bytes(b"source later replaced")
        self.assertEqual(Path(item["path"]).read_bytes(), self.original)
        self.assertEqual(self.cache.get(item["id"]), item)
        for path in (self.cache.root, self.cache.objects, Path(item["path"]), self.cache.objects / (item["id"] + ".json")):
            self.assertEqual(path.stat().st_mode & 0o077, 0)
            self.assertEqual(path.stat().st_uid, os.geteuid())

    def test_more_than_four_or_sixteen_images_and_bounded_disk_pagination(self):
        added = [self.cache.add(self.source) for _ in range(40)]
        found, cursor = [], None
        while True:
            page = self.cache.list(cursor=cursor, limit=7)
            self.assertLessEqual(len(page["items"]), 7)
            found.extend(page["items"])
            cursor = page["next_cursor"]
            if cursor is None:
                break
        self.assertEqual([item["id"] for item in found], [item["id"] for item in added])
        self.assertEqual(len({item["id"] for item in found}), 40)

    def test_collection_failure_is_byte_budget_and_existing_originals_survive(self):
        cache = AttachmentCache(self.root / "small", max_total_bytes=1000)
        first = cache.add(self.source)
        with self.assertRaises(AttachmentCacheError) as caught:
            while True:
                cache.add(self.source)
        self.assertEqual(caught.exception.code, "cache_full")
        self.assertEqual(Path(cache.get(first["id"])["path"]).read_bytes(), self.original)
        self.assertEqual(len(list(cache.objects.glob(".attachment-*"))), 0)

    def test_per_file_byte_budget(self):
        cache = AttachmentCache(self.root / "tiny", max_file_bytes=len(self.original) - 1)
        with self.assertRaises(AttachmentCacheError) as caught:
            cache.add(self.source)
        self.assertEqual(caught.exception.code, "file_too_large")
        self.assertEqual(cache.list()["items"], [])

    def test_byte_ingress_preserves_exact_original_and_validates_mime(self):
        item = self.cache.add_bytes(bytearray(self.original), "browser image.png", "image/png")
        self.assertEqual(item["name"], "browser image.png")
        self.assertEqual(Path(item["path"]).read_bytes(), self.original)
        self.assertEqual(item["sha256"], hashlib.sha256(self.original).hexdigest())
        with self.assertRaises(AttachmentCacheError) as caught:
            self.cache.add_bytes(self.original, "mismatch.jpg", "image/jpeg")
        self.assertEqual(caught.exception.code, "mime_mismatch")
        for name in ("../../secret", "/etc/passwd", "C:\\secret", "line\nbreak", "", ".."):
            with self.subTest(name=name), self.assertRaises(AttachmentCacheError) as caught:
                self.cache.add_bytes(self.original, name)
            self.assertEqual(caught.exception.code, "invalid_name")
        for data in (b"", b"x" * (self.cache.max_file_bytes + 1), "text"):
            with self.subTest(data_type=type(data).__name__), self.assertRaises(AttachmentCacheError):
                self.cache.add_bytes(data, "image.png")

    def test_upload_receipt_dictionary_survives_restart_and_is_copied(self):
        item = self.cache.add_bytes(self.original, "image.png")
        receipt = {"collection_id": "opaque-collection", "image_id": "opaque-image", "session_id": "session-1"}
        uploaded = self.cache.mark_uploaded(item["id"], receipt)
        receipt["session_id"] = "caller changed later"
        self.assertEqual(uploaded["ref"]["session_id"], "session-1")
        restarted = AttachmentCache(self.cache.root)
        self.assertEqual(restarted.get(item["id"])["ref"], uploaded["ref"])
        self.assertEqual(restarted.mark_submitted(item["id"])["state"], "submitted")
        for invalid in ({}, {"collection_id": "one"}, {**uploaded["ref"], "unexpected": "extra"},
                        {**uploaded["ref"], "image_id": "\r\nunsafe"}):
            fresh = self.cache.add(self.source)
            with self.subTest(invalid=invalid), self.assertRaises(AttachmentCacheError) as caught:
                self.cache.mark_uploaded(fresh["id"], invalid)
            self.assertEqual(caught.exception.code, "invalid_reference")

    def test_corrupt_truncated_html_and_trailing_payloads_are_rejected(self):
        variants = [b"<html>not an image</html>", self.original[:-1], self.original + b"hidden payload",
                    self.original[:45] + b"broken" + self.original[51:]]
        for data in variants:
            with self.subTest(data=data[:20]):
                self.source.write_bytes(data)
                with self.assertRaises(AttachmentCacheError) as caught:
                    self.cache.add(self.source)
                self.assertEqual(caught.exception.code, "invalid_image")

    def test_symlinks_and_special_sources_are_rejected_without_blocking(self):
        alias = self.root / "alias.png"
        alias.symlink_to(self.source)
        fifo = self.root / "fifo.png"
        os.mkfifo(fifo)
        for path in (alias, fifo, self.root):
            with self.subTest(path=path), self.assertRaises(AttachmentCacheError):
                self.cache.add(path)
        self.assertEqual(self.cache.list()["items"], [])

    def test_symlinked_source_parent_is_rejected(self):
        folder = self.root / "folder"
        folder.mkdir()
        (folder / "image.png").write_bytes(self.original)
        alias = self.root / "alias-folder"
        alias.symlink_to(folder, target_is_directory=True)
        with self.assertRaises(AttachmentCacheError) as caught:
            self.cache.add(alias / "image.png")
        self.assertEqual(caught.exception.code, "unsafe_path")

    def test_cache_root_must_be_private_and_not_symlinked(self):
        public = self.root / "public"
        public.mkdir(mode=0o755)
        with self.assertRaises(AttachmentCacheError) as caught:
            AttachmentCache(public)
        self.assertEqual(caught.exception.code, "unsafe_cache")
        alias = self.root / "linked-cache"
        alias.symlink_to(self.cache.root)
        with self.assertRaises(AttachmentCacheError) as caught:
            AttachmentCache(alias)
        self.assertEqual(caught.exception.code, "unsafe_path")

    def test_uploaded_draft_removal_and_submitted_original_retention(self):
        draft = self.cache.add(self.source)
        uploaded = self.cache.mark_uploaded(draft["id"], "image_ref_opaque")
        self.assertEqual(uploaded["ref"], "image_ref_opaque")
        self.assertTrue(self.cache.remove(draft["id"]))
        self.assertFalse(Path(draft["path"]).exists())
        self.assertFalse(self.cache.remove(draft["id"]))
        source = self.cache.add(self.source)
        self.cache.mark_uploaded(source["id"], "image_ref_durable")
        submitted = self.cache.mark_submitted(source["id"])
        restarted = AttachmentCache(self.cache.root)
        self.assertEqual(restarted.get(source["id"]), submitted)
        self.assertEqual(restarted.mark_submitted(source["id"]), submitted)
        with self.assertRaises(AttachmentCacheError) as caught:
            restarted.remove(source["id"])
        self.assertEqual(caught.exception.code, "attachment_submitted")
        self.assertEqual(Path(source["path"]).read_bytes(), self.original)

    def test_failed_image_progress_is_individual_and_retryable(self):
        first, second = self.cache.add(self.source), self.cache.add(self.source)
        failed = self.cache.set_failed(first["id"], "Original is retained; transport unavailable")
        self.assertEqual(failed["state"], "failed")
        self.assertEqual(failed["error"]["code"], "upload_failed")
        self.assertEqual(self.cache.get(second["id"])["state"], "staged")
        recovered = self.cache.mark_uploaded(first["id"], "opaque-recovered")
        self.assertEqual(recovered["state"], "uploaded")
        self.assertIsNone(recovered["error"])

    def test_submission_requires_actual_upload_reference(self):
        item = self.cache.add(self.source)
        with self.assertRaises(AttachmentCacheError) as caught:
            self.cache.mark_submitted(item["id"])
        self.assertEqual(caught.exception.code, "attachment_not_uploaded")
        for reference in ("", "\r\nBearer token", "x" * 513, None):
            with self.subTest(reference=reference), self.assertRaises(AttachmentCacheError):
                self.cache.mark_uploaded(item["id"], reference)

    def test_invalid_identity_and_page_arguments_have_no_side_effects(self):
        for identity in ("../escape", "", None):
            with self.subTest(identity=identity), self.assertRaises(AttachmentCacheError):
                self.cache.get(identity)
        for limit in (0, -1, 1001, True):
            with self.subTest(limit=limit), self.assertRaises(AttachmentCacheError):
                self.cache.list(limit=limit)

    def test_tampered_metadata_and_original_are_rejected(self):
        item = self.cache.add(self.source)
        metadata = self.cache.objects / (item["id"] + ".json")
        tampered = {**item, "path": str(self.source)}
        metadata.write_text(json.dumps(tampered))
        with self.assertRaises(AttachmentCacheError) as caught:
            self.cache.get(item["id"])
        self.assertEqual(caught.exception.code, "invalid_metadata")
        metadata.write_text(json.dumps(item))
        Path(item["path"]).chmod(0o644)
        with self.assertRaises(AttachmentCacheError) as caught:
            self.cache.get(item["id"])
        self.assertEqual(caught.exception.code, "unsafe_cache")

    def test_same_size_original_replacement_cannot_reuse_source_hash(self):
        item = self.cache.add(self.source)
        Path(item["path"]).write_bytes(bytes(len(self.original)))
        with self.assertRaises(AttachmentCacheError) as caught:
            self.cache.get(item["id"])
        self.assertEqual(caught.exception.code, "source_changed")

    def test_restart_removes_uncommitted_files_and_keeps_manifested_sources(self):
        item = self.cache.add(self.source)
        orphan = self.cache.objects / ("00000000000000000001-" + "a" * 32 + ".image")
        orphan.write_bytes(b"unfinished original")
        orphan.chmod(0o600)
        temporary = self.cache.objects / ".attachment-interrupted-write"
        temporary.write_bytes(b"partial write")
        temporary.chmod(0o600)
        restarted = AttachmentCache(self.cache.root)
        self.assertFalse(orphan.exists())
        self.assertFalse(temporary.exists())
        self.assertEqual(restarted.get(item["id"]), item)

    def test_atomic_failure_keeps_no_orphan_original(self):
        actual_write = self.cache._write
        def fail_metadata(path, data):
            if path.suffix == ".json":
                raise OSError(28, "No space left on device")
            return actual_write(path, data)
        with patch.object(self.cache, "_write", side_effect=fail_metadata):
            with self.assertRaises(AttachmentCacheError) as caught:
                self.cache.add(self.source)
        self.assertEqual(caught.exception.code, "cache_full")
        self.assertEqual(list(self.cache.objects.iterdir()), [])

    def test_concurrent_cache_instances_share_disk_budget(self):
        root = self.root / "shared"
        caches = [AttachmentCache(root, max_total_bytes=2200) for _ in range(8)]
        def stage(cache):
            try:
                return cache.add(self.source)
            except AttachmentCacheError as exc:
                self.assertEqual(exc.code, "cache_full")
        with ThreadPoolExecutor(max_workers=8) as pool:
            records = [value for value in pool.map(stage, caches) if value]
        self.assertGreater(len(records), 0)
        self.assertLess(caches[0]._usage(), 2201)
        self.assertEqual(len(caches[0].list()["items"]), len(records))

    def test_minimal_python_png_container_validation(self):
        # The packaged runtime need not include Pillow for PNG originals.
        original_import = __import__
        def without_pillow(name, *args, **kwargs):
            if name == "PIL":
                raise ImportError("minimal runtime")
            return original_import(name, *args, **kwargs)
        with patch("builtins.__import__", side_effect=without_pillow):
            self.assertEqual(_image_type(self.original), ("image/png", 1, 1))

    def test_decoder_supported_jpeg_gif_webp_containers(self):
        try:
            from PIL import Image, features
        except ImportError:
            self.skipTest("Pillow fixtures unavailable in minimal Python")
        for format, mime in (("JPEG", "image/jpeg"), ("GIF", "image/gif"), ("WEBP", "image/webp")):
            if format == "WEBP" and not features.check("webp"):
                continue
            with self.subTest(format=format):
                data = io.BytesIO()
                Image.new("RGB", (2, 3), "red").save(data, format=format)
                self.assertEqual(_image_type(data.getvalue()), (mime, 2, 3))
                original_import = __import__
                def without_pillow(name, *args, **kwargs):
                    if name == "PIL":
                        raise ImportError("minimal runtime")
                    return original_import(name, *args, **kwargs)
                with patch("builtins.__import__", side_effect=without_pillow):
                    self.assertEqual(_image_type(data.getvalue()), (mime, 2, 3))


if __name__ == "__main__":
    unittest.main()
