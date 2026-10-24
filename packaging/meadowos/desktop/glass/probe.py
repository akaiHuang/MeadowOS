"""Owned native rendering probe. It creates only fixtures; no user UI input.

Run as `python3 -m glass.probe --output /owned/evidence` on an isolated display
with its own compositor. Refuses the live Meadow display :93.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import time

import gi
gi.require_version("Gtk", "3.0")
gi.require_version("Gdk", "3.0")
from gi.repository import Gdk, GLib, Gtk
import cairo
import numpy as np
from PIL import Image

from .backdrop import Backdrop
from .gtk_surface import GlassSurface
from .optics import Material, render


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    if os.environ.get("DISPLAY", "").split(".")[0] == ":93":
        raise SystemExit("Rendering probe refuses the live Meadow display")
    args.output.mkdir(parents=True, exist_ok=True)
    state = {"phase": 0, "samples": [], "started": time.monotonic()}
    background = Gtk.Window(title="Meadow glass owned dynamic source")
    background.set_decorated(False)
    background.set_default_size(900, 650)
    background.move(30, 30)
    drawing = Gtk.DrawingArea()
    def draw(_widget, cr):
        cr.set_source_rgb(.12, .22, .3); cr.paint()
        for x in range(-40, 960, 24):
            cr.set_source_rgb(.95 if (x // 24) % 2 else .05, .25 if state["phase"] else .7, .28 if state["phase"] else .95)
            cr.rectangle(x + state["phase"] * 11, 0, 12, 650); cr.fill()
        cr.set_source_rgb(.8, .2 if state["phase"] else .8, .1)
        cr.rectangle(270 + state["phase"] * 100, 0, 100, 650); cr.fill()
        return False
    drawing.connect("draw", draw)
    background.add(drawing)
    background.show_all()
    target = Gtk.Window(title="Meadow native glass probe")
    target.set_decorated(False)
    target.set_visual(target.get_screen().get_rgba_visual())
    target.set_default_size(300, 380)
    target.move(220, 150)
    css = Gtk.CssProvider()
    css.load_from_data(b"window, .meadow-glass-surface { background: transparent; } label { color: #142f38; font-size: 20px; font-weight: 600; }")
    Gtk.StyleContext.add_provider_for_screen(target.get_screen(), css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
    surface = GlassSurface(target, corner_radius=22)
    content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=24)
    content.set_margin_top(42); content.set_margin_start(22); content.set_margin_end(22)
    for text in ("Meadow", "Native backdrop", "Sharp foreground"):
        content.pack_start(Gtk.Label(label=text), False, False, 0)
    surface.add(content); target.add(surface); target.show_all()
    def sample(phase):
        with Backdrop() as provider:
            raw = provider.capture(target.get_window().get_xid(), -40, -40, 380, 460)
            start = time.monotonic()
            material = Material(radius=22, blur=18, tint=.46)
            output = render(raw, 300, 380, material=material)
            flat = render(raw, 300, 380, material=Material(radius=22, blur=18, tint=.46, refraction=0, dispersion=0))
            elapsed = (time.monotonic() - start) * 500
            Image.fromarray(raw[..., [2, 1, 0, 3]]).save(args.output / f"backdrop-{phase}.png")
            Image.fromarray(output[..., [2, 1, 0, 3]]).save(args.output / f"material-{phase}.png")
            edge_diff = np.abs(output.astype(float) - flat.astype(float))[30:-30, 2:20, :3].mean()
            widget_render = cairo.ImageSurface(cairo.FORMAT_ARGB32, 300, 380)
            surface.draw(cairo.Context(widget_render))
            widget_render.write_to_png(str(args.output / f"native-widget-{phase}.png"))
            state["samples"].append({"phase": phase, "hash": hashlib.sha256(raw).hexdigest(),
                                      "sources": provider.source_count, "material_ms": elapsed,
                                      "edge_refraction_difference": float(edge_diff),
                                      "surface_frames": surface.frame_count, "surface_error": surface.last_error})
            if phase == 0:
                request = f"{target.get_window().get_xid():x} 0 0 300 380 22 sidebar\n".encode()
                bridge = subprocess.run([sys.executable, str(Path(__file__).with_name("bridge.py")),
                                         "--display", os.environ["DISPLAY"]], input=request,
                                        capture_output=True, timeout=5, check=True)
                bridge_header = struct.unpack("=4I", bridge.stdout[:16])
                state["bridge"] = {"header": list(bridge_header), "matches_direct_renderer":
                                   bridge_header == (0, provider.source_count, 300, 380)
                                   and bridge.stdout[16:] == output.tobytes()}
        if phase == 0:
            # Changing target pixels must not feed back into its own backdrop.
            content.get_children()[-1].set_text("Changed foreground")
            GLib.timeout_add(1600, lambda: sample(1))
        elif phase == 1:
            state["phase"] = 1; drawing.queue_draw()
            GLib.timeout_add(1600, lambda: sample(2))
        elif phase == 2:
            target.move(260, 180)
            GLib.timeout_add(1600, lambda: sample(3))
        else:
            state["passed"] = (state["samples"][0]["hash"] == state["samples"][1]["hash"]
                               and state["samples"][0]["hash"] != state["samples"][2]["hash"]
                               and state["samples"][2]["hash"] != state["samples"][3]["hash"]
                               and state["bridge"]["matches_direct_renderer"]
                               and all(s["sources"] > 0 and s["edge_refraction_difference"] > .1 and not s["surface_error"] for s in state["samples"]))
            state["elapsed"] = time.monotonic() - state.pop("started")
            (args.output / "receipt.json").write_text(json.dumps(state, indent=2))
            print(json.dumps(state))
            target.destroy(); background.destroy(); Gtk.main_quit()
        return False
    GLib.timeout_add(2000, lambda: sample(0))
    Gtk.main()


if __name__ == "__main__":
    main()
