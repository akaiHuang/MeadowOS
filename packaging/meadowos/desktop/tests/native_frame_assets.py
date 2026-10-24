"""Original Meadow xfwm4 pixel assets. --write regenerates; default only checks.

No third-party image is copied. Keep every button pixel opaque: xfwm4 uses the
XPM mask as its input shape, so transparency would shrink the usable hit area.
"""
import argparse
import hashlib
import json
from pathlib import Path
import string

ROOT = Path(__file__).resolve().parents[1]
STATES = ("active", "inactive", "prelight", "pressed")
BUTTONS = ("close", "hide", "maximize", "menu", "shade", "stick")
TOGGLED = ("maximize", "shade", "stick")
PALETTES = {
    "Meadow-Light": {"background": "#edf1f4", "inactive": "#f1f3f5", "rule": "#d7dee4",
                     "text": "#2c3842", "muted": "#84909a", "idle_dot": "#bac2c9"},
    "Meadow-Dark": {"background": "#242d35", "inactive": "#232b32", "rule": "#3a4650",
                    "text": "#e8eef3", "muted": "#96a4af", "idle_dot": "#657480"},
}
DOTS = {"close": "#ec6a5f", "hide": "#e7b94b", "maximize": "#68b56f",
        "menu": "#9aa99d", "shade": "#9aa99d", "stick": "#9aa99d"}


def rgb(value):
    return tuple(int(value[index:index + 2], 16) for index in (1, 3, 5))


def blend(first, second, ratio):
    return tuple(round(a * (1 - ratio) + b * ratio) for a, b in zip(first, second))


def xpm(pixels):
    colors = sorted({pixel for row in pixels for pixel in row})
    alphabet = string.ascii_letters + string.digits
    codes = {color: alphabet[index // len(alphabet)] + alphabet[index % len(alphabet)]
             for index, color in enumerate(colors)}
    rows = [f'{len(pixels[0])} {len(pixels)} {len(colors)} 2']
    rows += [f'{codes[color]} c #{color[0]:02x}{color[1]:02x}{color[2]:02x}' for color in colors]
    rows += [''.join(codes[pixel] for pixel in row) for row in pixels]
    return ('/* XPM */\n/* Original Meadow native frame; fixed opaque geometry. */\n'
            'static char * meadow_frame[] = {\n' +
            ',\n'.join('"' + row + '"' for row in rows) + '\n};\n')


def glyph(name, toggled):
    points = set()
    if name == "close":
        points = {(x, y) for x in range(7, 13) for y in range(9, 15) if x - 7 == y - 9 or x - 7 == 14 - y}
    elif name == "hide":
        points = {(x, 12) for x in range(7, 13)}
    elif name == "maximize":
        if toggled:
            points = {(x, y) for x in range(7, 12) for y in range(10, 15) if x in (7, 11) or y in (10, 14)}
            points |= {(x, 8) for x in range(9, 14)} | {(13, y) for y in range(8, 13)}
        else:
            points = {(x, 12) for x in range(7, 13)} | {(10, y) for y in range(9, 15)}
    elif name == "menu":
        points = {(x, y) for x in range(7, 13) for y in (9, 12, 15)}
    elif name == "shade":
        points = {(x, (14 if toggled else 9)) for x in range(7, 13)}
        points |= {(x, y) for x in range(8, 12) for y in range(10, 14) if abs(x - 9.5) + abs(y - 11.5) < 2.1}
    elif name == "stick":
        points = {(x, y) for x in range(7, 13) for y in range(9, 15) if abs(x - 9.5) + abs(y - 11.5) < 3.1}
        if not toggled:
            points = {(x, y) for x, y in points if abs(x - 9.5) + abs(y - 11.5) > 1.1}
    return points


def button(palette, name, state, toggled=False):
    background = rgb(palette["inactive"] if state == "inactive" else palette["background"])
    rule = rgb(palette["rule"])
    pixels = [[rule if y in (0, 23) else background for _ in range(20)] for y in range(24)]
    fill = rgb(palette["idle_dot"] if state == "inactive" else DOTS[name])
    if state == "pressed":
        fill = blend(fill, (0, 0, 0), .18)
    rim = blend(fill, (0, 0, 0), .17)
    for y in range(6, 18):
        for x in range(4, 16):
            samples = [(x + (sx + .5) / 4 - 10) ** 2 + (y + (sy + .5) / 4 - 12) ** 2
                       for sy in range(4) for sx in range(4)]
            coverage = sum(value <= 36 for value in samples) / 16
            inner = sum(value <= 25 for value in samples) / 16
            color = blend(background, rim, coverage)
            pixels[y][x] = blend(color, fill, inner)
    if state in ("prelight", "pressed") or name not in ("close", "hide", "maximize"):
        ink = blend(fill, (0, 0, 0), .68)
        for x, y in glyph(name, toggled):
            pixels[y][x] = ink
    return xpm(pixels)


def frame(palette, part, state):
    background = rgb(palette["inactive"] if state == "inactive" else palette["background"])
    rule = rgb(palette["rule"])
    if part.startswith("title-"):
        width, height = 4, 24
    elif part.startswith("top-"):
        width, height = 6, 24
    elif part.startswith("bottom-"):
        width, height = 6, 3
    elif part == "bottom":
        width, height = 4, 3
    else:
        width, height = 3, 8
    pixels = [[background for _ in range(width)] for _ in range(height)]
    for y in range(height):
        for x in range(width):
            if ((height == 24 and y in (0, 23)) or
                (part.startswith("bottom") and y == height - 1) or
                ("left" in part and x == 0) or ("right" in part and x == width - 1)):
                pixels[y][x] = rule
    return xpm(pixels)


def theme_files(name):
    palette = PALETTES[name]
    files = {}
    for state in ("active", "inactive"):
        for part in [*(f"title-{index}" for index in range(1, 6)), "top-left", "top-right",
                     "bottom-left", "bottom-right", "left", "right", "bottom"]:
            files[f"{part}-{state}.xpm"] = frame(palette, part, state)
    for name_button in BUTTONS:
        for state in STATES:
            files[f"{name_button}-{state}.xpm"] = button(palette, name_button, state)
            if name_button in TOGGLED:
                files[f"{name_button}-toggled-{state}.xpm"] = button(palette, name_button, state, True)
    files["themerc"] = (f'active_text_color={palette["text"]}\ninactive_text_color={palette["muted"]}\n'
        'button_offset=2\nbutton_spacing=0\nfull_width_title=true\nframe_border_top=0\n'
        'maximized_offset=6\nshow_app_icon=false\nshadow_delta_height=0\nshadow_delta_width=0\n'
        'shadow_delta_x=0\nshadow_delta_y=-3\nshadow_opacity=35\ntitle_horizontal_offset=4\n'
        'title_vertical_offset_active=0\ntitle_vertical_offset_inactive=0\n'
        'title_shadow_active=false\ntitle_shadow_inactive=false\n')
    provenance = {"schema_version": 1, "theme": name, "origin": "original Meadow procedural pixel assets",
                  "generator": "desktop/tests/native_frame_assets.py", "external_images": [],
                  "title_height": 24, "button_canvas": [20, 24], "button_dot_diameter": 12,
                  "resize_border_width": 3, "button_canvas_opaque": True,
                  "files": {path: hashlib.sha256(value.encode()).hexdigest() for path, value in sorted(files.items())}}
    files["assets.json"] = json.dumps(provenance, indent=2) + '\n'
    return files


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="store_true")
    args = parser.parse_args()
    errors = []
    for name in PALETTES:
        root = ROOT / "themes" / name / "xfwm4"
        if args.write:
            root.mkdir(parents=True, exist_ok=True)
        for path, value in theme_files(name).items():
            target = root / path
            if args.write:
                target.write_text(value)
            elif not target.is_file() or target.read_text() != value:
                errors.append(str(target))
    if errors:
        raise SystemExit('Native frame assets differ: ' + ', '.join(errors))
    print('Two original native frame themes: 60 XPMs each, fixed 24px title, opaque 20x24 button canvases.')


if __name__ == "__main__":
    main()
