"""Optical pixel worker for native GTK adapters outside Python.

Each request line: hex-XID x y width height radius mode. Coordinates are client
relative; mode is sidebar, toolbar, popup[-dark], tooltip[-dark], dialog[-dark], split:<x>
or split-dark:<x>. Split is the
physical sidebar right edge in a joined Files content surface. Response is 4 native uint32 (status,
source-count,width,height), followed by premultiplied BGRA only on status=0.
Use a GLib IO watch/nonblocking pipe in UI adapters, never wait in a draw handler.
"""
import argparse
import math
import os
import signal
import struct
import sys

import numpy as np

from .backdrop import Backdrop
from .optics import Material, render, render_joined


def parse_request(line):
    xid, x, y, width, height, radius, mode = line.split()
    xid, x, y, width, height = int(xid, 16), int(x), int(y), int(width), int(height)
    radius = float(radius)
    joined = mode.startswith(("split:", "split-dark:"))
    content = mode in ("dialog", "dialog-dark")
    split = int(mode.split(":", 1)[1]) if joined else None
    if (min(width, height) < 2 or max(width, height) > (4096 if joined or content else 2048)
            or width * height > (4_194_304 if joined or content else 2_000_000)
            or max(abs(x), abs(y)) > 65535 or not 0 < xid <= 0xffffffff
            or not math.isfinite(radius)
            or (joined and not 0 <= split <= width)
            or (not joined and mode not in ("sidebar", "toolbar", "popup", "popup-dark",
                                            "tooltip", "tooltip-dark", "dialog", "dialog-dark"))):
        raise ValueError("Invalid native glass region")
    return xid, x, y, width, height, max(0, min(radius, 80)), mode, split


def render_request(line, backdrop, margin=40, cache=None):
    xid, x, y, width, height, radius, mode, split = parse_request(line)
    if backdrop.failed:
        backdrop.recover()
    pixels = backdrop.capture(xid, x-margin, y-margin, width+2*margin, height+2*margin)
    if cache is not None:
        key=(xid,x,y,width,height,radius,mode,margin)
        if cache.get("key")==key and np.array_equal(cache["pixels"],pixels):
            return None
        cache.update(key=key,pixels=pixels.copy())
    if split is not None:
        return render_joined(pixels, width, height, split, margin, mode.startswith("split-dark:"))
    if mode.startswith("dialog"):
        material = Material(radius=0, blur=28, tint=.78 if mode.endswith("-dark") else .68,
                            bevel=0, refraction=0, dispersion=0, highlight=0,
                            tint_color=(48, 40, 32) if mode.endswith("-dark") else (249, 249, 249))
    elif mode.startswith("popup"):
        material = Material(radius=radius, blur=18, tint=.78 if mode.endswith("-dark") else .66, bevel=8,
                            refraction=5, dispersion=.20, highlight=.14,
                            tint_color=(48, 40, 32) if mode.endswith("-dark") else (249, 249, 249))
    elif mode.startswith("tooltip"):
        material = Material(radius=radius, blur=12, tint=.78 if mode.endswith("-dark") else .66, bevel=6,
                            refraction=3, dispersion=.12, highlight=.12,
                            tint_color=(48, 40, 32) if mode.endswith("-dark") else (249, 249, 249))
    else:
        material = Material(radius=radius, blur=34 if mode == "sidebar" else 8,
                            tint=.76 if mode == "sidebar" else .10)
    return render(pixels, width, height, margin, material)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--display", required=True)
    parser.add_argument("--helper")
    args = parser.parse_args()
    # Popups are short-lived. Termination must unwind Backdrop's context so its
    # owned X11 helper is closed and reaped, including while waiting for input.
    def terminate(_signal, _frame):
        raise SystemExit(0)

    previous = signal.signal(signal.SIGTERM, terminate)
    try:
        with Backdrop(display=args.display, helper=args.helper) as backdrop:
            cache = {} if os.environ.get("MEADOW_GLASS_DEDUPLICATE")=="1" else None
            for line in sys.stdin:
                try:
                    output = render_request(line, backdrop, cache=cache)
                    if output is None:
                        _,_,_,width,height,_,_,_=parse_request(line)
                        sys.stdout.buffer.write(struct.pack("=4I",2,backdrop.source_count,width,height))
                        sys.stdout.buffer.flush()
                        continue
                    height, width = output.shape[:2]
                    sys.stdout.buffer.write(struct.pack("=4I", 0, backdrop.source_count, width, height))
                    sys.stdout.buffer.write(output.tobytes())
                except (OSError, RuntimeError, ValueError):
                    if cache is not None:
                        cache.clear()
                    sys.stdout.buffer.write(struct.pack("=4I", 1, 0, 0, 0))
                sys.stdout.buffer.flush()
    finally:
        signal.signal(signal.SIGTERM, previous)


if __name__ == "__main__":
    main()
