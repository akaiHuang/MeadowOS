"""Architecture/hash guards and optional session setup; not visual acceptance."""
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

import image
from test_foundation import os_session


class WindowResourcesTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.expected = {"desktop/glass/meadow-glass-capture": "desktop/glass/capture.c",
                         "window_manager/libmeadow-hover-chrome.so": "window_manager/meadow_hover_chrome.c"}
        for name in self.expected:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            value = bytearray(64)
            value[:6] = b"\x7fELF\x02\x01"
            struct.pack_into("<H", value, 18, image.MACHINES["arm64"])
            path.write_bytes(value)
            path.chmod(0o755)
        self.receipt = {"schema_version": 1, "arch": "arm64",
                        "files": {name: image.file_record(self.root / name) for name in self.expected},
                        "source_files": {name: image.file_record(image.PACKAGING / name)
                                         for name in self.expected.values()}}
        self.save()

    def tearDown(self):
        self.temporary.cleanup()

    def save(self):
        (self.root / "window-resources.json").write_text(json.dumps(self.receipt))

    def test_matching_resources_accepted(self):
        self.assertEqual(image.window_resources(self.root, "arm64"), self.receipt)

    def add_folder_resource(self):
        name = "folder_materials/libmeadow-folder-materials.so"
        path = self.root / name
        path.parent.mkdir(parents=True)
        path.write_bytes((self.root / next(iter(self.expected))).read_bytes())
        path.chmod(0o755)
        self.receipt["files"][name] = image.file_record(path)
        for source in image.WINDOW_RESOURCE_SOURCES[name]:
            self.receipt["source_files"][source] = image.file_record(image.PACKAGING / source)
        self.save()

    def test_full_folder_resource_sources_accepted(self):
        self.add_folder_resource()
        self.assertEqual(image.window_resources(self.root, "arm64"), self.receipt)

    def test_missing_folder_header_rejected(self):
        self.add_folder_resource()
        del self.receipt["source_files"]["window_manager/meadow_folder_paint.h"]
        self.save()
        with self.assertRaisesRegex(ValueError, "manifest"):
            image.window_resources(self.root, "arm64")

    def test_changed_folder_painter_rejected(self):
        self.add_folder_resource()
        self.receipt["source_files"]["window_manager/meadow_folder_paint.c"]["sha256"] = "old"
        self.save()
        with self.assertRaisesRegex(ValueError, "source differs"):
            image.window_resources(self.root, "arm64")

    def test_other_architecture_rejected(self):
        with self.assertRaisesRegex(ValueError, "manifest"):
            image.window_resources(self.root, "amd64")

    def test_changed_binary_rejected(self):
        with (self.root / next(iter(self.expected))).open("ab") as stream:
            stream.write(b"changed")
        with self.assertRaisesRegex(ValueError, "changed"):
            image.window_resources(self.root, "arm64")

    def test_changed_source_rejected(self):
        self.receipt["source_files"][next(iter(self.expected.values()))]["sha256"] = "old"
        self.save()
        with self.assertRaisesRegex(ValueError, "source differs"):
            image.window_resources(self.root, "arm64")

    def test_manifest_cannot_supply_extra_paths(self):
        self.receipt["files"]["../../elsewhere"] = {}
        self.save()
        with self.assertRaisesRegex(ValueError, "manifest"):
            image.window_resources(self.root, "arm64")

    def test_session_preserves_existing_modules_and_has_explicit_opt_out(self):
        share = self.root / "share/meadow-os"
        for name in (*self.expected, "desktop/glass/bridge.py"):
            path = share / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.touch()
        env = {"GTK_MODULES": "existing-accessibility"}
        status = os_session.configure_window_resources(self.root, env)
        self.assertTrue(status["configured"])
        self.assertFalse(status["visual_verified"])
        self.assertFalse(status["folder_materials"]["configured"])
        self.assertTrue(env["GTK_MODULES"].startswith("existing-accessibility:"))
        before = dict(env)
        os_session.configure_window_resources(self.root, env)
        self.assertEqual(env, before)
        disabled = {"MEADOW_WINDOW_EFFECTS": "0", "GTK_MODULES": "existing"}
        self.assertFalse(os_session.configure_window_resources(self.root, disabled)["configured"])
        self.assertEqual(disabled, {"MEADOW_WINDOW_EFFECTS": "0", "GTK_MODULES": "existing"})

    def test_folder_session_retains_stock_and_custom_extensions(self):
        prefix = self.root / "opt/meadow"
        share = prefix / "share/meadow-os"
        for name in (*image.WINDOW_RESOURCE_SOURCES, "desktop/glass/bridge.py"):
            path = share / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.touch()
        extension = self.root / image.folder_extension_path("arm64")
        extension.parent.mkdir(parents=True)
        extension.touch()
        env = {"GTK_MODULES": "accessibility", "THUNARX_DIRS": "/custom/extensions"}
        with patch.object(os_session.sysconfig, "get_config_var", return_value="aarch64-linux-gnu"):
            status = os_session.configure_window_resources(prefix, env)
            before = dict(env)
            os_session.configure_window_resources(prefix, env)
        self.assertTrue(status["folder_materials"]["configured"])
        self.assertFalse(status["folder_materials"]["visual_verified"])
        self.assertEqual(env, before)
        self.assertEqual(env["THUNARX_DIRS"].split(":"), ["/custom/extensions",
            "/usr/lib/aarch64-linux-gnu/thunarx-3", "/usr/lib/thunarx-3", str(extension.parent)])
        self.assertIn(str(extension), env["GTK_MODULES"].split(":"))
        self.assertNotIn(str(share / image.FOLDER_RESOURCE), env["GTK_MODULES"].split(":"))
        self.assertTrue(env["GTK_MODULES"].startswith("accessibility:"))
        disabled = {"MEADOW_FOLDER_EFFECTS": "0", "THUNARX_DIRS": "/custom/extensions"}
        self.assertFalse(os_session.configure_window_resources(prefix, disabled)["folder_materials"]["configured"])
        self.assertEqual(disabled["THUNARX_DIRS"], "/custom/extensions")
        self.assertNotIn("folder-materials", disabled["GTK_MODULES"])

    def test_session_missing_resources_preserves_environment(self):
        env = {"GTK_MODULES": "existing"}
        self.assertFalse(os_session.configure_window_resources(self.root, env)["configured"])
        self.assertEqual(env, {"GTK_MODULES": "existing"})

    def test_preview_session_requires_both_payloads_and_preserves_explicit_choice(self):
        prefix = self.root / "opt/meadow"
        share = prefix / "share/meadow-os"
        for name in (*image.WINDOW_RESOURCE_SOURCES, "desktop/glass/bridge.py", "desktop/quicklook.py"):
            path = share / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.touch()
        extension = self.root / image.folder_extension_path("arm64")
        extension.parent.mkdir(parents=True)
        extension.touch()
        with patch.object(os_session.sysconfig, "get_config_var", return_value="aarch64-linux-gnu"):
            missing = {}
            os_session.configure_window_resources(prefix, missing)
            self.assertNotIn("MEADOW_QUICKLOOK_HELPER", missing)
            (share / "desktop/file_preview.py").touch()
            env = {"MEADOW_QUICKLOOK": "0", "MEADOW_FILE_THUMBNAILS": "0",
                   "MEADOW_QUICKLOOK_HELPER": "/owned/explicit-preview.py"}
            status = os_session.configure_window_resources(prefix, env)
            self.assertEqual(env["MEADOW_QUICKLOOK_HELPER"], "/owned/explicit-preview.py")
            self.assertFalse(status["folder_materials"]["quicklook"]["configured"])
            self.assertFalse(status["folder_materials"]["quicklook"]["content_thumbnails"])
            defaults = {}
            ready = os_session.configure_window_resources(prefix, defaults)
            self.assertEqual(defaults["MEADOW_QUICKLOOK_HELPER"], str(share / "desktop/quicklook.py"))
            self.assertTrue(ready["folder_materials"]["quicklook"]["configured"])


if __name__ == "__main__":
    unittest.main()
