#!/usr/bin/env python3
"""Stage noVNC with Meadow Command mapping and crisp known native cursors.

Vendor files and MPL notices are preserved. The desktop IME adapter maps an
explicit macOS editing allowlist to native Control; other Command chords keep
Super and browser Command+R stays local. Copy/cut/paste use the guest clipboard.
The cursor enhancement recognizes
only shipped Meadow raster RGB/dimensions/hotspots, then uses its source SVG on
the desktop CSS cursor path. Other app cursors and touch fallback stay native.
Unknown keyboard/cursor vendor sources fail before creating an output directory.
"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import shutil

CURSOR_MANIFEST = (Path(__file__).resolve().parents[2] /
                   "packaging/meadowos/desktop/icons/Meadow-Cursors/remote-cursors.json")
IME_SOURCE = Path(__file__).with_name("assets") / "desktop-ime.js"
VOICE_SOURCE = Path(__file__).with_name("assets") / "desktop-voice.js"
ATTACHMENTS_SOURCE = Path(__file__).with_name("assets") / "desktop-attachments.js"


def patch_keyboard(source):
    old = """                case KeyTable.XK_Super_L:
                    keysym = KeyTable.XK_Alt_L;
                    break;
                case KeyTable.XK_Super_R:
                    keysym = KeyTable.XK_Super_L;
                    break;"""
    new = """                // MeadowOS: preserve Command as Super for OS shortcuts.
                case KeyTable.XK_Super_L:
                case KeyTable.XK_Super_R:
                    break;"""
    if source.count(old) != 1:
        raise ValueError("unsupported noVNC keyboard mapping; vendor source preserved")
    source = source.replace(old, new)
    entry = "    _handleKeyDown(e) {"
    if source.count(entry) != 1:
        raise ValueError("unsupported noVNC composition entry; vendor source preserved")
    return source.replace(entry, entry + """
        // Do not suppress native IME preedit or transmit its phonetic keys.
        if (e.isComposing || e.keyCode === 229 || e.key === 'Process') return;
""")


def patch_ui(source):
    marker = "    connectFinished(e) {"
    if source.count(marker) != 1:
        raise ValueError("unsupported noVNC UI connect entry; vendor source preserved")
    return ("import { installDesktopIME } from './meadow-ime.js';\n"
            "import { installDesktopVoice } from './meadow-voice.js';\n"
            "import { installDesktopAttachments } from './desktop-attachments.js';\n" + source).replace(
        marker, marker + "\n        installDesktopIME(UI.rfb, document.getElementById('noVNC_container'));"
        "\n        installDesktopVoice(UI.rfb);"
        "\n        installDesktopAttachments(UI.rfb, document.getElementById('noVNC_container'));")


def cursor_module(manifest):
    if manifest.get("version") != 1 or not isinstance(manifest.get("cursors"), list):
        raise ValueError("unsupported Meadow cursor manifest")
    candidates = {}
    for cursor in manifest["cursors"]:
        key = ",".join(str(cursor[field]) for field in ("width", "height", "hotx", "hoty"))
        entry = {"sha256": cursor["rgb_sha256"], "size": cursor["logical_size"],
                 "uri": "data:image/svg+xml;base64," + base64.b64encode(cursor["vector"].encode()).decode()}
        candidates.setdefault(key, []).append(entry)
    return """// Original Meadow cursor presentation. No application cursor-name heuristics.
const candidates = """ + json.dumps(candidates, separators=(",", ":")) + """;

export async function meadowVectorCursor(rgba, hotx, hoty, w, h) {
    const matches = candidates[[w, h, hotx, hoty].join(',')];
    const subtle = globalThis.crypto && globalThis.crypto.subtle;
    if (!matches || !subtle || rgba.length !== w * h * 4) return null;
    // RichCursor transports a one-bit alpha mask. Match every original RGB
    // byte; its SHA-256 and exact geometry identify the shipped source vector.
    const rgb = new Uint8Array(w * h * 3);
    for (let source = 0, dest = 0; source < rgba.length; source += 4) {
        rgb[dest++] = rgba[source];
        rgb[dest++] = rgba[source + 1];
        rgb[dest++] = rgba[source + 2];
    }
    const digest = new Uint8Array(await subtle.digest('SHA-256', rgb));
    const hash = Array.from(digest, byte => byte.toString(16).padStart(2, '0')).join('');
    const found = matches.find(candidate => candidate.sha256 === hash);
    if (!found) return null;
    return 'url("' + found.uri + '") ' + Math.round(hotx * found.size / w) +
        ' ' + Math.round(hoty * found.size / h) + ', default';
}
"""


def patch_cursor(source):
    imported = "import { supportsCursorURIs, isTouchDevice } from './browser.js';"
    change = "    change(rgba, hotx, hoty, w, h) {"
    clear = "    clear() {"
    assigned = "            this._target.style.cursor = 'url(' + url + ')' + hotx + ' ' + hoty + ', default';"
    if any(source.count(needle) != 1 for needle in (imported, change, clear, assigned)):
        raise ValueError("unsupported noVNC cursor implementation; vendor source preserved")
    source = source.replace(imported, imported + "\nimport { meadowVectorCursor } from './meadow-cursors.js';")
    source = source.replace(change, change + "\n        const meadowSerial = this._meadowCursorSerial = (this._meadowCursorSerial || 0) + 1;")
    source = source.replace(clear, clear + "\n        this._meadowCursorSerial = (this._meadowCursorSerial || 0) + 1;")
    source = source.replace(assigned, assigned + """
            // Use the same vector only after an exact shipped-raster match.
            // Digest work must not replace a later cursor or a cleared target.
            meadowVectorCursor(rgba, hotx, hoty, w, h).then(cursor => {
                if (cursor && this._target && this._meadowCursorSerial === meadowSerial) {
                    this._target.style.cursor = cursor;
                }
            }).catch(() => {}); // Native cursor remains the available fallback.
""")
    return source


def prepare(source, output):
    original = source / "core/input/keyboard.js"
    original_cursor = source / "core/util/cursor.js"
    patched = patch_keyboard(original.read_text())
    patched_cursor = patch_cursor(original_cursor.read_text())
    patched_ui = patch_ui((source / "app/ui.js").read_text())
    manifest_bytes = CURSOR_MANIFEST.read_bytes()
    helper = cursor_module(json.loads(manifest_bytes))
    html = (source / "vnc.html").read_text()
    if html.count('import UI from "./app/ui.js";') != 1:
        raise ValueError("unsupported noVNC HTML entry; vendor source preserved")
    # A browser reload may retain transitive ES-module responses from an older
    # desktop at the same localhost URL. Give the complete module graph a new
    # immutable path whenever its entry or Meadow patches change.
    if output.exists() or output.is_symlink():
        raise ValueError("client output already exists; choose a new owned directory")
    shutil.copytree(source, output)
    target = output / "core/input/keyboard.js"
    target.write_text(patched)
    target_cursor = output / "core/util/cursor.js"
    target_cursor.write_text(patched_cursor)
    (output / "app/ui.js").write_text(patched_ui)
    shutil.copyfile(IME_SOURCE, output / "app/meadow-ime.js")
    shutil.copyfile(IME_SOURCE.with_name("desktop-ime-native.js"), output / "app/desktop-ime-native.js")
    shutil.copyfile(VOICE_SOURCE, output / "app/meadow-voice.js")
    shutil.copyfile(ATTACHMENTS_SOURCE, output / "app/desktop-attachments.js")
    shutil.copyfile(VOICE_SOURCE.with_name("voice-capture-worklet.js"), output / "app/voice-capture-worklet.js")
    (output / "core/util/meadow-cursors.js").write_text(helper)
    graph_hash = hashlib.sha256(html.encode())
    for directory in ("app", "core", "vendor"):
        for path in sorted((output / directory).rglob("*")):
            if path.is_file():
                graph_hash.update(str(path.relative_to(output)).encode() + b"\0")
                graph_hash.update(hashlib.sha256(path.read_bytes()).digest())
    release = graph_hash.hexdigest()[:16]
    module_prefix = "client/" + release
    versioned = output / module_prefix
    versioned.mkdir(parents=True)
    for directory in ("app", "core", "vendor"):
        if (output / directory).is_dir():
            shutil.copytree(output / directory, versioned / directory)
    html = html.replace('"./app/', '"./' + module_prefix + '/app/').replace('"app/', '"' + module_prefix + '/app/')
    (output / "vnc.html").write_text(html)
    receipt = {"source": str(source), "output": str(output), "command_mapping": "Super",
               "source_keyboard_sha256": hashlib.sha256(original.read_bytes()).hexdigest(),
               "keyboard_sha256": hashlib.sha256(target.read_bytes()).hexdigest(),
               "source_cursor_sha256": hashlib.sha256(original_cursor.read_bytes()).hexdigest(),
               "cursor_sha256": hashlib.sha256(target_cursor.read_bytes()).hexdigest(),
               "cursor_manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
               "client_release": release,
               "module_prefix": module_prefix,
               "cursor_presentation": "known Meadow SVGs at 24 CSS px; native fallback otherwise",
               "desktop_ime": "host composition commit once by Unicode code point; vendor touch input retained",
               "desktop_ime_sha256": hashlib.sha256(IME_SOURCE.read_bytes()).hexdigest(),
               "desktop_shortcuts": {"mac_editing": "Command A/C/X/V/Z/Y/F/S/O/P/N/Enter/+/=/-/0 to native Control; Shift preserved",
                                     "mac_other": "OS Super; Command R and Command Shift R stay in host browser",
                                     "windows_linux": "native Control and OS Super retained",
                                     "clipboard": "guest-native C/X/V and Shift Insert; explicit browser input retained; no host clipboard API or automatic sync"},
               "desktop_voice": "explicit host microphone permission; local audio companion at loopback 60531",
               "desktop_attachments": "foreground native chat rectangle; original host File bytes individually bounded at 20 MiB; private native staging receipt",
               "desktop_attachments_sha256": hashlib.sha256(ATTACHMENTS_SOURCE.read_bytes()).hexdigest(),
               "vendor_modified": False}
    (output / "meadow-client.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(prepare(args.source, args.output)))
