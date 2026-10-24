#!/usr/bin/env python3
"""Small white Meadow pointer and opposed-triangle resize cursors.

The shared vector geometry is rendered at 8x for native Xcursor assets. The
remote client can use those same vectors after identifying the shipped raster;
VNC's one-bit cursor mask otherwise discards the native antialiasing.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path
import struct

from PIL import Image, ImageDraw

ROOT = Path(__file__).with_name("icons") / "Meadow-Cursors"
SIZES = (24, 32, 48, 64, 96, 128)
RESIZE_GROUPS = (
    (0, "e-resize w-resize ew-resize left_side right_side sb_h_double_arrow h_double_arrow col-resize"),
    (math.pi / 2, "n-resize s-resize ns-resize top_side bottom_side sb_v_double_arrow v_double_arrow row-resize"),
    (math.pi / 4, "nw-resize se-resize nwse-resize top_left_corner bottom_right_corner size_fdiag"),
    (-math.pi / 4, "ne-resize sw-resize nesw-resize top_right_corner bottom_left_corner size_bdiag"),
)
POINTER_ALIASES = "left_ptr default arrow top_left_arrow"
TRIANGLES = (((-9, 0), (-3, -4.5), (-3, 4.5)), ((9, 0), (3, 4.5), (3, -4.5)))
POINTER = ((5, 3), (5, 23), (10, 18), (14, 26), (17, 24.5), (13, 17), (21, 17))


def geometry(angle: float | None):
    if angle is None:
        return (POINTER,)
    return tuple(tuple((16 + x * math.cos(angle) - y * math.sin(angle),
                        16 + x * math.sin(angle) + y * math.cos(angle))
                       for x, y in triangle) for triangle in TRIANGLES)


def hotspot(angle: float | None, size: int):
    return (round(5 * size / 32), round(3 * size / 32)) if angle is None else (size // 2, size // 2)


def cursor(angle: float | None, size: int):
    factor = 8
    image = Image.new("RGBA", (size * factor, size * factor))
    draw = ImageDraw.Draw(image)
    scale = size * factor / 32
    for polygon in geometry(angle):
        points = [(x * scale, y * scale) for x, y in polygon]
        # The restrained dark keyline remains visible over bright wallpaper;
        # the two resize shapes have no connecting shaft or extra ornaments.
        draw.polygon(points, fill=(255, 255, 255, 255) if angle is None else (27, 35, 42, 150))
        draw.line(points + points[:1], fill=(23, 32, 40, 210),
                  width=max(1, round(2.5 * scale)), joint="curve")
        draw.line(points + points[:1], fill=(255, 255, 255, 255),
                  width=max(1, round(1.5 * scale)), joint="curve")
    return image.resize((size, size), Image.Resampling.LANCZOS)


def vector(angle: float | None, logical_size: int = 24):
    polygons = []
    for polygon in geometry(angle):
        points = " ".join(f"{x:.5f},{y:.5f}" for x, y in polygon)
        fill = "#ffffff" if angle is None else "#1b232a"
        opacity = "1" if angle is None else ".59"
        polygons.append(f'<polygon points="{points}" fill="{fill}" fill-opacity="{opacity}" stroke="#172028" stroke-opacity=".82" stroke-width="2.5" stroke-linejoin="round"/>')
        polygons.append(f'<polygon points="{points}" fill="none" stroke="#ffffff" stroke-width="1.5" stroke-linejoin="round"/>')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{logical_size}" height="{logical_size}" viewBox="0 0 32 32">'
            + "".join(polygons) + "</svg>")


def pixels(image):
    return [(a << 24) | ((r * a // 255) << 16) | ((g * a // 255) << 8) | (b * a // 255)
            for r, g, b, a in image.getdata()]


def encode(angle: float | None):
    data, toc = [], []
    position = 16 + len(SIZES) * 12
    for size in SIZES:
        values = pixels(cursor(angle, size))
        x, y = hotspot(angle, size)
        chunk = (struct.pack("<9I", 36, 0xfffd0002, size, 1, size, size, x, y, 0)
                 + struct.pack("<" + str(len(values)) + "I", *values))
        toc.append(struct.pack("<3I", 0xfffd0002, size, position))
        data.append(chunk)
        position += len(chunk)
    return b"Xcur" + struct.pack("<3I", 16, 0x10000, len(SIZES)) + b"".join(toc + data)


def remote_manifest():
    """Identify only exact shipped cursor RGB/hotspot data after VNC alpha loss.

    x11vnc normally sends premultiplied RGB and a 1-bit transparency mask.
    Include its explicit -alpharemove variant and the red/blue ordering produced
    by noVNC's requested RGBX format plus its fixed BGRX cursor decoder. No shape
    heuristic or application cursor-name override is used by the client.
    """
    variants = []
    for name, angle in (("pointer", None), ("horizontal", 0), ("vertical", math.pi / 2),
                        ("diagonal-down", math.pi / 4), ("diagonal-up", -math.pi / 4)):
        for size in SIZES:
            image = cursor(angle, size)
            raw = list(image.getdata())
            x, y = hotspot(angle, size)
            for mode in ("premultiplied", "straight"):
                rgb = bytes(channel for r, g, b, a in raw for channel in
                            ((r * a // 255, g * a // 255, b * a // 255) if mode == "premultiplied" else (r, g, b)))
                for channel_order in ("RGB", "BGR"):
                    ordered = rgb if channel_order == "RGB" else bytes(
                        channel for index in range(0, len(rgb), 3)
                        for channel in (rgb[index + 2], rgb[index + 1], rgb[index]))
                    variants.append({"name": name, "width": size, "height": size,
                                     "hotx": x, "hoty": y, "rgb_sha256": hashlib.sha256(ordered).hexdigest(),
                                     "channel_order": channel_order, "alpha_mode": mode,
                                     "vector": vector(angle), "logical_size": 24})
    return {"version": 1, "algorithm": "SHA-256 RGB; exact width, height and hotspot", "cursors": variants}


def assets():
    result = {"index.theme": ("[Icon Theme]\nName=Meadow-Cursors\n"
                             "Comment=Small white pointer and opposed-triangle resize cursors\nInherits=Adwaita\n").encode()}
    for angle, names in RESIZE_GROUPS:
        content = encode(angle)
        for name in names.split():
            result["cursors/" + name] = content
    pointer = encode(None)
    for name in POINTER_ALIASES.split():
        result["cursors/" + name] = pointer
    result["remote-cursors.json"] = (json.dumps(remote_manifest(), sort_keys=True, separators=(",", ":")) + "\n").encode()
    return result


def build(check=False):
    expected = assets()
    if check:
        actual = {str(path.relative_to(ROOT)) for path in ROOT.rglob("*") if path.is_file()}
        mismatch = [name for name, content in expected.items()
                    if name not in actual or (ROOT / name).read_bytes() != content]
        extra = sorted(actual - set(expected))
        if mismatch or extra:
            raise SystemExit(f"Cursor assets differ: changed={mismatch}, extra={extra}")
        print(f"Verified {len(expected) - 2} cursors at {SIZES} and remote vector manifest")
        return
    for name, content in expected.items():
        path = ROOT / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)
    print(f"Wrote {len(expected) - 2} cursors at {SIZES} and remote vector manifest")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    build(parser.parse_args().check)
