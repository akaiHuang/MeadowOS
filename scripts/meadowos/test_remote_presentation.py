"""Remote presentation defaults; no browser, VNC, or user-input synthesis."""
import base64
import json
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import unittest

from prepare_novnc import CURSOR_MANIFEST, cursor_module, patch_cursor, patch_keyboard, prepare

VENDOR_MAPPING = """// Copyright noVNC authors - MPL 2.0
    _handleKeyDown(e) {
switch (keysym) {
                case KeyTable.XK_Super_L:
                    keysym = KeyTable.XK_Alt_L;
                    break;
                case KeyTable.XK_Super_R:
                    keysym = KeyTable.XK_Super_L;
                    break;
                case KeyTable.XK_Alt_L:
                    keysym = KeyTable.XK_Mode_switch;
                    break;
}
}
"""
VENDOR_CURSOR = """// Copyright noVNC authors - MPL 2.0
import { supportsCursorURIs, isTouchDevice } from './browser.js';
const useFallback = !supportsCursorURIs || isTouchDevice;
export default class Cursor {
    change(rgba, hotx, hoty, w, h) {
        if ((w === 0) || (h === 0)) { this.clear(); return; }
        const url = 'native';
        if (!useFallback) {
            this._target.style.cursor = 'url(' + url + ')' + hotx + ' ' + hoty + ', default';
        }
    }
    clear() {
        this._target.style.cursor = 'none';
    }
}
"""


def seed_vendor(root):
    root.mkdir(parents=True, exist_ok=True)
    (root / "app").mkdir()
    (root / "app/ui.js").write_text("// noVNC fixture entry\n    connectFinished(e) {\n    }\n")
    (root / "vnc.html").write_text('<script type="module">import UI from "./app/ui.js";</script>')
    (root / "core/input").mkdir(parents=True)
    (root / "core/util").mkdir()
    (root / "core/input/keyboard.js").write_text(VENDOR_MAPPING)
    (root / "core/util/cursor.js").write_text(VENDOR_CURSOR)


def raster_rgba(name="left_side", nominal=32):
    content = (CURSOR_MANIFEST.parent / "cursors" / name).read_bytes()
    header, _, count = struct.unpack_from("<3I", content, 4)
    for index in range(count):
        _, size, offset = struct.unpack_from("<3I", content, header + index * 12)
        if size == nominal:
            _, _, _, _, w, h, x, y, _ = struct.unpack_from("<9I", content, offset)
            values = struct.unpack_from("<" + str(w * h) + "I", content, offset + 36)
            return {"rgba": list(channel for value in values for channel in
                                 ((value >> 16) & 255, (value >> 8) & 255, value & 255, 255 if value >> 24 >= 240 else 0)),
                    "hotx": x, "hoty": y, "w": w, "h": h}
    raise ValueError("missing raster")


class RemotePresentationTest(unittest.TestCase):
    def test_command_is_preserved_and_option_mapping_and_license_are_retained(self):
        patched = patch_keyboard(VENDOR_MAPPING)
        self.assertNotIn("keysym = KeyTable.XK_Alt_L;", patched)
        self.assertIn("case KeyTable.XK_Super_R:", patched)
        self.assertIn("keysym = KeyTable.XK_Mode_switch;", patched)
        self.assertTrue(patched.startswith("// Copyright noVNC authors - MPL 2.0"))

    def test_unknown_version_is_rejected_without_creating_or_changing_output(self):
        for broken in ("keyboard", "cursor"):
            with self.subTest(source=broken), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source = root / "source"
                seed_vendor(source)
                path = source / ("core/input/keyboard.js" if broken == "keyboard" else "core/util/cursor.js")
                path.write_text("unknown vendor source")
                with self.assertRaises(ValueError):
                    prepare(source, root / "output")
                self.assertFalse((root / "output").exists())
                self.assertEqual(path.read_text(), "unknown vendor source")

    def test_vendor_and_prior_staged_clients_are_never_overwritten(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            seed_vendor(root / "vendor")
            original = root / "vendor/core/input/keyboard.js"
            cursor = root / "vendor/core/util/cursor.js"
            receipt = prepare(root / "vendor", root / "client")
            self.assertEqual(original.read_text(), VENDOR_MAPPING)
            self.assertEqual(cursor.read_text(), VENDOR_CURSOR)
            self.assertFalse(receipt["vendor_modified"])
            self.assertNotEqual(receipt["keyboard_sha256"], receipt["source_keyboard_sha256"])
            self.assertNotEqual(receipt["cursor_sha256"], receipt["source_cursor_sha256"])
            self.assertIn("known Meadow SVGs", receipt["cursor_presentation"])
            self.assertIn("Command A/C/X/V", receipt["desktop_shortcuts"]["mac_editing"])
            self.assertIn("guest-native", receipt["desktop_shortcuts"]["clipboard"])
            self.assertEqual(len(receipt["desktop_ime_sha256"]), 64)
            self.assertTrue((root / "client/core/util/meadow-cursors.js").is_file())
            self.assertTrue((root / "client/core/util/cursor.js").read_text().startswith("// Copyright noVNC authors - MPL 2.0"))

    def test_html_loads_the_complete_patched_graph_under_a_release_path(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            seed_vendor(root / "vendor")
            receipt = prepare(root / "vendor", root / "client")
            prefix = receipt["module_prefix"]
            html = (root / "client/vnc.html").read_text()
            self.assertIn(f'import UI from "./{prefix}/app/ui.js";', html)
            self.assertIn("meadowVectorCursor", (root / "client" / prefix / "core/util/cursor.js").read_text())
            self.assertTrue((root / "client" / prefix / "core/util/meadow-cursors.js").is_file())
            # Updating one client patch must invalidate the graph, while an
            # identical staged build can safely reuse its immutable URLs.
            second = prepare(root / "vendor", root / "same")
            self.assertEqual(second["client_release"], receipt["client_release"])
            entry = root / "vendor/vnc.html"
            entry.write_text(entry.read_text() + "<!-- vendor update -->")
            third = prepare(root / "vendor", root / "changed")
            self.assertNotEqual(third["client_release"], receipt["client_release"])
            p = root / "vendor/app/ui.js"
            p.write_text(p.read_text() + "// independent vendor module update")
            fourth = prepare(root / "vendor", root / "module-update")
            self.assertNotEqual(third["client_release"], fourth["client_release"])
            with self.assertRaises(ValueError):
                prepare(root / "vendor", root / "client")

    def test_unknown_cursor_manifest_fails_closed(self):
        with self.assertRaises(ValueError):
            cursor_module({"version": 999, "cursors": []})

    @unittest.skipUnless(shutil.which("node"), "Node.js required for cursor presentation contract")
    def test_exact_shipped_cursor_becomes_vector_and_others_keep_native(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "package.json").write_text('{"type":"module"}')
            (root / "helper.js").write_text(cursor_module(json.loads(CURSOR_MANIFEST.read_text())))
            fixture = raster_rgba()
            (root / "fixture.json").write_text(json.dumps(fixture))
            result = subprocess.run([shutil.which("node"), "--input-type=module", "-e", """
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { webcrypto } from 'node:crypto';
import { meadowVectorCursor } from './helper.js';
if (!globalThis.crypto) globalThis.crypto = webcrypto;
const c = JSON.parse(await readFile('./fixture.json', 'utf8'));
const known = await meadowVectorCursor(c.rgba, c.hotx, c.hoty, c.w, c.h);
assert.match(known, /^url\("data:image\/svg\+xml;base64,/);
assert.match(known, / 12 12, default$/);
// noVNC requests RGBX pixels but its RichCursor decoder swaps BGRX. Recognize
// this exact known transport ordering without recolouring any custom cursor.
const swapped = c.rgba.slice();
for (let i = 0; i < swapped.length; i += 4) {
    [swapped[i], swapped[i + 2]] = [swapped[i + 2], swapped[i]];
}
assert.equal(await meadowVectorCursor(swapped, c.hotx, c.hoty, c.w, c.h), known);
const svg = Buffer.from(known.split('base64,')[1].split('"')[0], 'base64').toString();
assert.match(svg, /width="24" height="24"/);
assert.equal((svg.match(/<polygon/g) || []).length, 4);
const changed = c.rgba.slice(); changed[0] ^= 1;
assert.equal(await meadowVectorCursor(changed, c.hotx, c.hoty, c.w, c.h), null);
assert.equal(await meadowVectorCursor(c.rgba, c.hotx - 1, c.hoty, c.w, c.h), null);
assert.equal(await meadowVectorCursor(c.rgba.slice(1), c.hotx, c.hoty, c.w, c.h), null);
console.log('exact raster, vector dimensions, hotspot and native fallbacks passed');
"""], cwd=root, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    @unittest.skipUnless(shutil.which("node"), "Node.js required for cursor race contract")
    def test_late_vector_match_cannot_replace_a_newer_or_cleared_cursor(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "package.json").write_text('{"type":"module"}')
            (root / "cursor.js").write_text(patch_cursor(VENDOR_CURSOR))
            (root / "browser.js").write_text("export const supportsCursorURIs=true; export const isTouchDevice=false;")
            (root / "meadow-cursors.js").write_text("export const pending=[]; export function meadowVectorCursor() { return new Promise(resolve=>pending.push(resolve)); }")
            result = subprocess.run([shutil.which("node"), "--input-type=module", "-e", """
import assert from 'node:assert/strict';
import Cursor from './cursor.js';
import { pending } from './meadow-cursors.js';
const c = new Cursor(); c._target = {style:{}};
c.change([], 1, 1, 32, 32); c.change([], 2, 2, 32, 32);
pending[0]('old'); await Promise.resolve();
assert.notEqual(c._target.style.cursor, 'old');
pending[1]('current'); await Promise.resolve();
assert.equal(c._target.style.cursor, 'current');
c.change([], 1, 1, 32, 32); c.clear();
pending[2]('cleared'); await Promise.resolve();
assert.equal(c._target.style.cursor, 'none');
console.log('cursor serial and clear guards passed');
"""], cwd=root, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
