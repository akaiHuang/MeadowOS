"""Archive packaging verifies actual source bytes without copying Git credentials."""
import copy
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("meadow_archive_image", Path(__file__).with_name("image.py"))
image = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(image)


class ArchiveProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        for name in ("go/source.go", "scripts/meadowos/image.py", "packaging/meadowos/runtime.py"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("original source\n")
        self.repo = mock.patch.object(image, "REPO", self.root)
        self.repo.start()
        files, digest = image.source_inventory()
        self.provenance = {"source": {"commit": "a" * 40, "dirty": False,
            "commit_origin": "explicit_builder_input", "source_files": files,
            "source_inventory_sha256": digest}}

    def tearDown(self):
        self.repo.stop()
        self.temporary.cleanup()

    def no_git(self):
        return mock.patch.object(image.subprocess, "run", return_value=subprocess.CompletedProcess([], 128, "", "no Git"))

    def test_archive_uses_validated_full_commit_and_rehashed_sources(self):
        with self.no_git():
            self.assertEqual(image.verified_source_identity(self.provenance), ("a" * 40, False))

    def test_archive_rejects_missing_or_invalid_source_identity(self):
        for change in ({"commit": "abc123"}, {"dirty": "false"}, {"commit_origin": "invented"}):
            value = copy.deepcopy(self.provenance)
            value["source"].update(change)
            with self.subTest(change=change), self.no_git(), self.assertRaises(ValueError):
                image.verified_source_identity(value)

    def test_archive_rejects_changed_source(self):
        (self.root / "go/source.go").write_text("changed source\n")
        with self.no_git(), self.assertRaisesRegex(ValueError, "source differs"):
            image.verified_source_identity(self.provenance)

    def test_archive_rejects_added_source(self):
        (self.root / "go/added.go").write_text("added source\n")
        with self.no_git(), self.assertRaisesRegex(ValueError, "source differs"):
            image.verified_source_identity(self.provenance)

    def test_archive_rejects_symlink_input(self):
        (self.root / "go/link.go").symlink_to("source.go")
        with self.assertRaisesRegex(ValueError, "symlinks"):
            image.source_inventory()

    def test_git_checkout_must_match_provenance(self):
        with mock.patch.object(image.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "b" * 40 + "\n", "")), \
             mock.patch.object(image.subprocess, "check_output", return_value=""), \
             self.assertRaisesRegex(ValueError, "Git source identity differs"):
            image.verified_source_identity(self.provenance)


if __name__ == "__main__":
    unittest.main()
