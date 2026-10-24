#!/usr/bin/env python3
"""Rebuild the original Meadow desktop icon theme (stdlib only).

Full-colour app artwork uses a 64 px canvas with a 54 px optical body, which
becomes 40.5 px at the desktop's 48 px size. Symbolic artwork has a 24 px
canvas, 1.75 px round strokes, and a single foreground colour. Browser and
system launchers share the glass plate. Meadow retains its standalone green mark.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent / "Meadow"
INK = "#61736e"
GREEN = "#0bae79"
ICONS: dict[str, str] = {}
SYMBOLIC_GLYPHS: dict[str, str] = {}


def svg(body: str, size: int = 24) -> str:
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{size}" '
            f'height="{size}" viewBox="0 0 {size} {size}">\n{body}\n</svg>\n')


def line(body: str, colour: str = INK) -> str:
    return (f'<g fill="none" stroke="{colour}" stroke-width="1.75" '
            f'stroke-linecap="round" stroke-linejoin="round">{body}</g>')


GLYPHS = {
    "folder": '<path d="M3 7V5.8A1.8 1.8 0 0 1 4.8 4H9l2 2h8.2A1.8 1.8 0 0 1 21 7.8v10.4a1.8 1.8 0 0 1-1.8 1.8H4.8A1.8 1.8 0 0 1 3 18.2V7Zm0 1h18"/>',
    "home": '<path d="m3 10 9-7 9 7M5 9v11h5v-6h4v6h5V9"/>',
    "document": '<path d="M7 3h7l4 4v14H6V3h1Zm7 0v5h4M9 12h6m-6 4h6"/>',
    "download": '<path d="M12 3v12m-4-4 4 4 4-4M5 17v4h14v-4"/>',
    "upload": '<path d="M12 16V4m-4 4 4-4 4 4M5 17v4h14v-4"/>',
    "desktop": '<rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8m-4-4v4M3 14h18"/>',
    "picture": '<rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8" cy="8" r="1.4"/><path d="m3 17 5-5 4 4 4-6 5 7"/>',
    "video": '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M7 3v18M17 3v18M3 8h4m10 0h4M3 16h4m10 0h4"/>',
    "music": '<path d="M10 17V5l10-2v12M10 9l10-2"/><ellipse cx="7" cy="18" rx="3" ry="2.5"/><ellipse cx="17" cy="16" rx="3" ry="2.5"/>',
    "recent": '<circle cx="12" cy="12" r="9"/><path d="M12 6v6l4 2"/>',
    "trash": '<path d="M4 6h16M9 6V3h6v3M6 6l1 15h10l1-15M10 10v7m4-7v7"/>',
    "star": '<path d="m12 3 2.7 5.7 6.3.9-4.5 4.5 1 6.3-5.5-3-5.5 3 1-6.3L3 9.6l6.3-.9Z"/>',
    "network": '<circle cx="12" cy="12" r="9"/><ellipse cx="12" cy="12" rx="4" ry="9"/><path d="M3 12h18M5 6h14M5 18h14"/>',
    "drive": '<path d="M5 4h14l2 12v4H3v-4L5 4ZM3 16h18"/><path d="M16 18h2"/>',
    "usb": '<path d="M12 21V3m-3 3 3-3 3 3M12 15l-5-4V8m5 10 5-4V9"/><circle cx="7" cy="7" r="1.2"/><path d="M16 6h2v3h-2Z"/>',
    "settings": '<path d="m9.5 3-.7 2.7-2.4 1.4-2.7-.8-2.1 3.5 2 1.9v2.7l-2 1.9 2.1 3.5 2.7-.8 2.4 1.4.7 2.7h4l.7-2.7 2.4-1.4 2.7.8 2.1-3.5-2-1.9v-2.7l2-1.9-2.1-3.5-2.7.8-2.4-1.4-.7-2.7Z" transform="translate(.5 -1) scale(1 .96)"/><circle cx="12" cy="12" r="3.5"/>',
    "appearance": '<path d="m9 13 9-10 3 3-10 9-2-2ZM11 15c0 5-4 6-8 6 2-2 1-3 2-5 1-2 3-3 6-1Z"/>',
    "keyboard": '<rect x="2" y="5" width="20" height="14" rx="2"/><path d="M5 9h1m3 0h1m3 0h1m3 0h2M5 12h1m3 0h1m3 0h1m3 0h2M7 16h10"/>',
    "mouse": '<rect x="6" y="2" width="12" height="20" rx="6"/><path d="M6 9h12M12 2v7"/>',
    "accessibility": '<circle cx="12" cy="4" r="2"/><path d="M4 8h16m-8 0v6m0-3-5 10m5-10 5 10"/>',
    "grid": '<rect x="3" y="3" width="7" height="7" rx="1.3"/><rect x="14" y="3" width="7" height="7" rx="1.3"/><rect x="3" y="14" width="7" height="7" rx="1.3"/><rect x="14" y="14" width="7" height="7" rx="1.3"/>',
    "session": '<path d="M12 2v10m-6-7a9 9 0 1 0 12 0"/>',
    "workspaces": '<rect x="2" y="4" width="14" height="12" rx="2"/><path d="M7 20h13a2 2 0 0 0 2-2V9M2 8h14"/>',
    "panel": '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 16h18M7 19h.1M11 19h.1M15 19h.1"/>',
    "window": '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 8h18M7 5.5h.1M10 5.5h.1M13 5.5h.1"/>',
    "sliders": '<path d="M4 6h4m4 0h8M4 12h10m4 0h2M4 18h2m4 0h10"/><circle cx="10" cy="6" r="2"/><circle cx="16" cy="12" r="2"/><circle cx="8" cy="18" r="2"/>',
    "colour": '<path d="m12 3 7 4v10l-7 4-7-4V7l7-4Zm0 0v18M5 7l14 10M19 7 5 17"/>',
    "info": '<circle cx="12" cy="12" r="9"/><path d="M12 11v6m0-10v.1"/>',
    "search": '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 5 5"/>',
    "mail": '<rect x="2" y="5" width="20" height="15" rx="2"/><path d="m3 6 9 8 9-8"/>',
    "terminal": '<path d="m5 6 6 6-6 6M13 18h7"/>',
    "logout": '<path d="M10 4H4v16h6m-2-8h13m-5-5 5 5-5 5"/>',
    "check": '<path d="m5 12 4 4L19 6"/>',
    "close": '<path d="m6 6 12 12M18 6 6 18"/>',
    "plus": '<path d="M12 4v16M4 12h16"/>',
    "minus": '<path d="M4 12h16"/>',
    "back": '<path d="m14 5-7 7 7 7"/>',
    "forward": '<path d="m10 5 7 7-7 7"/>',
    "up": '<path d="m5 14 7-7 7 7"/>',
    "down": '<path d="m5 10 7 7 7-7"/>',
    "refresh": '<path d="M20 8a8.5 8.5 0 1 0 .5 7M20 3v5h-5"/>',
    "list": '<path d="M9 5h12M9 12h12M9 19h12M3 5h.1M3 12h.1M3 19h.1"/>',
    "menu": '<path d="M4 6h16M4 12h16M4 18h16"/>',
    "more": '<circle cx="12" cy="5" r=".7"/><circle cx="12" cy="12" r=".7"/><circle cx="12" cy="19" r=".7"/>',
    "copy": '<path d="M8 6V3h13v14h-3"/><rect x="3" y="7" width="13" height="14" rx="1.5"/>',
    "paste": '<path d="M8 5H5v16h14V5h-3"/><rect x="8" y="2" width="8" height="5" rx="1"/>',
    "cut": '<circle cx="6" cy="17" r="3"/><circle cx="18" cy="17" r="3"/><path d="m8 15 11-12M16 15 5 3"/>',
    "edit": '<path d="m4 16-1 5 5-1L21 7l-4-4L4 16Zm10-10 4 4"/>',
    "save": '<path d="M4 3h13l4 4v14H3V3h1ZM7 3v6h9V3M7 21v-8h10v8"/>',
    "eject": '<path d="m12 5 8 10H4L12 5ZM4 19h16"/>',
    "lock": '<rect x="5" y="10" width="14" height="11" rx="2"/><path d="M8 10V6a4 4 0 0 1 8 0v4M12 14v3"/>',
    "pin": '<path d="m9 3 8 2-1 5 3 4-7 2-4-4 1-9ZM11 15l-6 6"/>',
    "wifi": '<path d="M2 8a16 16 0 0 1 20 0M5 12a11 11 0 0 1 14 0M8 16a6 6 0 0 1 8 0M12 20h.1"/>',
    "sound": '<path d="M3 9h4l5-5v16l-5-5H3V9Zm13-1a6 6 0 0 1 0 8m3-11a10 10 0 0 1 0 14"/>',
    "battery": '<rect x="2" y="6" width="18" height="12" rx="2"/><path d="M23 10v4M5 9v6m3-6v6m3-6v6m3-6v6"/>',
    "chat": '<path d="M21 11a8 8 0 0 1-8 8H7l-4 3v-9a8 8 0 0 1 8-8h2a8 8 0 0 1 8 6ZM7 11h.1M12 11h.1M17 11h.1"/>',
    "stop": '<rect x="5" y="5" width="14" height="14" rx="2"/>',
    "play": '<path d="m7 3 14 9-14 9V3Z"/>',
    "microphone": '<rect x="9" y="2" width="6" height="13" rx="3"/><path d="M5 10v2a7 7 0 0 0 14 0v-2M12 19v3M8 22h8"/>',
    "attach": '<path d="m9 12 6-6a3 3 0 0 1 4 4l-8 8a5 5 0 0 1-7-7l9-9"/>',
    "send": '<path d="M12 21V3m-7 7 7-7 7 7"/>',
}


def register(context: str, names: str, content: str) -> None:
    for name in names.split():
        key = f"scalable/{context}/{name}.svg"
        existing = next((path for path in ICONS if path.casefold() == key.casefold()), None)
        if existing is not None and existing != key:
            raise ValueError(f"Case-folded icon name collision: {existing}, {key}")
        if key in ICONS and ICONS[key] != content:
            raise ValueError(f"Conflicting icon name: {key}")
        ICONS[key] = content


def symbols(context: str, names: str, glyph: str, regular: bool = True) -> None:
    # GTK's symbolic SVG loader replaces fill/stroke presentation. Open stroke
    # paths therefore become solid triangles/discs. Use actual filled outlines
    # with holes, authored from the same 1.75 px round strokes, instead.
    content = svg(f'<path fill="{INK}" fill-rule="nonzero" d="{SYMBOLIC_GLYPHS[glyph]}"/>')
    variants = [name + "-symbolic" for name in names.split()]
    if regular:
        variants.extend(names.split())
    register(context, " ".join(variants), content)


# Tile-only symbols do not alter the compiled symbolic outline source.
APP_TILE_GLYPHS = dict(GLYPHS)
APP_TILE_GLYPHS["meadow"]='<path d="M3 9c2-2.2 4-2.2 6 0s4 2.2 6 0 4-2.2 6 0M3 15c2-2.2 4-2.2 6 0s4 2.2 6 0 4-2.2 6 0"/>'
APP_TILE_GLYPHS["launcher"]=''.join(f'<circle cx="{x}" cy="{y}" r="1.1"/>' for y in (5,12,19) for x in (5,12,19))


def meadow_mark() -> str:
    """Original standalone brand mark, shared by launcher, dock and companion."""
    return svg('<g fill="none" stroke="#0bae79" stroke-width="4.4" stroke-linecap="round"><path d="M12 25c4-4.4 8-4.4 12 0s8 4.4 12 0 8-4.4 12 0M12 37c4-4.4 8-4.4 12 0s8 4.4 12 0 8-4.4 12 0"/></g>', 64)


def brand_assets() -> dict[Path, str]:
    """Compatibility asset paths all resolve to the same brand artwork."""
    desktop = ROOT.parent.parent
    shell = desktop.parent / "shell/assets"
    return {desktop / "assets/meadow-wave-mark.svg": meadow_mark(),
            **{shell / f"meadow-wave-{variant}.svg": meadow_mark()
               for variant in ("symbol", "avatar", "mark")}}


def tile(glyph: str, dark: bool = False, blue: bool = False) -> str:
    # Every application uses the same optical body. Alpha retains the actual
    # host surface; the glyph relief is equally legible on light and dark.
    tint = "#5c9de9" if blue else "#d9edf5"
    face = ".48" if blue else ".22"
    return svg(
        '<defs><linearGradient id="tile" x2=".18" y2="1">'
        '<stop stop-color="#f5fcff" stop-opacity=".34"/>'
        f'<stop offset=".52" stop-color="{tint}" stop-opacity="{face}"/>'
        '<stop offset="1" stop-color="#8fbed5" stop-opacity=".30"/>'
        '</linearGradient><linearGradient id="rim" x2=".8" y2="1">'
        '<stop stop-color="#fff" stop-opacity=".94"/>'
        '<stop offset=".55" stop-color="#e2f5ff" stop-opacity=".24"/>'
        '<stop offset="1" stop-color="#f8fdff" stop-opacity=".76"/>'
        '</linearGradient></defs>'
        '<rect x="5" y="6" width="54" height="54" rx="14" fill="#294a60" opacity=".12"/>'
        '<rect x="5" y="4" width="54" height="54" rx="14" fill="url(#tile)"/>'
        '<rect x="5.7" y="4.7" width="52.6" height="52.6" rx="13.3" fill="none" stroke="url(#rim)" stroke-width="1.4"/>'
        '<rect x="7.2" y="6.2" width="49.6" height="49.6" rx="11.8" fill="none" stroke="#fff" stroke-opacity=".12" stroke-width=".7"/>'
        '<path d="M16 8h31" stroke="#fff" stroke-opacity=".36" stroke-width="1.4" stroke-linecap="round"/>'
        + (f'<g transform="translate(16 15) scale(1.333333)">'
           f'<g fill="none" stroke="#294d63" stroke-opacity=".50" stroke-width="3.1" stroke-linecap="round" stroke-linejoin="round">{APP_TILE_GLYPHS[glyph]}</g>'
           f'<g fill="none" stroke="#f5fdff" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">{APP_TILE_GLYPHS[glyph]}</g></g>' if glyph else ''), 64)


def files_tile() -> str:
    # App identity is a glass stack, never the data-folder outline.
    layers=[]
    shape='M15 22 27 16Q32 13.5 37 16L49 22Q55 25 49 29L37 35Q32 37.5 27 35L15 29Q9 25 15 22Z'
    for shift,alpha in ((13,'.37'),(6.5,'.53'),(0,'.76')):
        layers.append(f'<path d="{shape}" transform="translate(0 {shift})" fill="#e8f7ff" fill-opacity="{alpha}" stroke="#fff" stroke-opacity=".64" stroke-width=".8"/>')
    return tile('',blue=True).replace('</svg>', ''.join(layers)+'</svg>')


def folder() -> str:
    # Resting, empty-folder material from meadow_folder_paint.c. This static
    # fallback transmits the host surface but never samples or blurs it. Only
    # the internal back layer is softened, with no invented content previews.
    back = ('M.10 .255C.10 .20 .125 .175 .175 .175H.34'
            'C.389 .175 .414 .265 .478 .265H.825'
            'C.876 .265 .90 .292 .90 .343V.815'
            'C.90 .866 .876 .89 .825 .89H.175'
            'C.124 .89 .10 .866 .10 .815Z')
    front = ('M.09 .465C.09 .415 .115 .39 .165 .39H.835'
             'C.885 .39 .91 .415 .91 .465V.835'
             'C.91 .885 .885 .91 .835 .91H.165'
             'C.115 .91 .09 .885 .09 .835Z')
    back_layer = (f'<path d="{back}" fill="url(#back)"/>'
                  f'<path d="{back}" fill="none" stroke="#ffffff" stroke-opacity=".63" stroke-width=".008"/>'
                  f'<path d="{back}" fill="none" stroke="#2e4561" stroke-opacity=".19" stroke-width=".009375"/>')
    return svg(
        '<defs>'
        '<linearGradient id="back" gradientUnits="userSpaceOnUse" x1="0" y1=".18" x2=".3" y2=".89">'
        '<stop stop-color="#bfdaf7" stop-opacity=".58"/><stop offset="1" stop-color="#6ba9ed" stop-opacity=".38"/></linearGradient>'
        '<linearGradient id="coat" gradientUnits="userSpaceOnUse" x1=".1" y1=".39" x2=".75" y2=".93">'
        '<stop stop-color="#ffffff" stop-opacity=".53"/><stop offset=".43" stop-color="#ffffff" stop-opacity=".43"/><stop offset="1" stop-color="#ffffff" stop-opacity=".49"/></linearGradient>'
        '<linearGradient id="edge" gradientUnits="userSpaceOnUse" x1="0" y1=".83" x2="0" y2=".94">'
        '<stop stop-color="#1a2433" stop-opacity="0"/><stop offset="1" stop-color="#1a2433" stop-opacity=".085"/></linearGradient>'
        '<linearGradient id="rim" gradientUnits="userSpaceOnUse" x1=".1" y1=".36" x2=".91" y2=".91">'
        '<stop stop-color="#ffffff" stop-opacity=".89"/><stop offset=".48" stop-color="#ffffff" stop-opacity=".52"/><stop offset="1" stop-color="#ffffff" stop-opacity=".26"/></linearGradient>'
        '<radialGradient id="shadow"><stop offset=".2" stop-color="#121c2e" stop-opacity=".14"/><stop offset="1" stop-color="#121c2e" stop-opacity="0"/></radialGradient>'
        '<filter id="frost" filterUnits="userSpaceOnUse" x="0" y="0" width="1" height="1" color-interpolation-filters="sRGB"><feGaussianBlur stdDeviation=".035"/></filter>'
        f'<clipPath id="cover"><path d="{front}"/></clipPath>'
        f'<clipPath id="outside-cover"><path d="M0 0H1V1H0Z{front}" clip-rule="evenodd"/></clipPath>'
        '</defs><g transform="scale(64)">'
        '<ellipse cx=".5" cy=".916" rx=".45" ry=".045" fill="url(#shadow)"/>'
        f'<g clip-path="url(#outside-cover)">{back_layer}</g>'
        f'<g clip-path="url(#cover)"><g filter="url(#frost)">{back_layer}</g>'
        '<path d="M0 0H1V1H0Z" fill="#6ba9ed" fill-opacity=".25"/>'
        '<path d="M0 0H1V1H0Z" fill="url(#coat)"/>'
        '<path d="M0 0H1V1H0Z" fill="url(#edge)"/></g>'
        f'<path d="{front}" fill="none" stroke="#334a66" stroke-opacity=".18" stroke-width=".015625"/>'
        f'<path d="{front}" fill="none" stroke="url(#rim)" stroke-width=".01015625"/>'
        '</g>', 64)


def folder_color(colour: str) -> str:
    # A 12 px swatch at its native 24 px size, with transparent surroundings.
    return svg(f'<circle cx="12" cy="12" r="5.5" fill="{colour}" '
               'stroke="#334a66" stroke-opacity=".25" stroke-width="1"/>')


def document(kind: str) -> str:
    # A file-type mark, never a preview of invented file contents. The shared
    # translucent sheet remains legible over both light and dark host surfaces;
    # distinct original vector glyphs do not depend on an installed text font.
    colours = {"generic": "#829aaf", "text": "#7899b4", "code": "#659f9a",
               "markdown": "#7b9fc6", "word": "#7594ce", "pdf": "#c98390",
               "image": "#83a58b", "sheet": "#75a78d", "slides": "#c7a16c",
               "archive": "#ad9589", "audio": "#ab91bb", "video": "#929cc4",
               "font": "#a08fb5", "executable": "#7b909f"}
    colour = colours[kind]
    content = {
        "generic": '<path d="M26 28h12M26 34h12M26 40h7"/>',
        "text": '<path d="M23 27h18M32 27v17M27 44h10"/>',
        "code": '<path d="m25 29-6 6 6 6m14-12 6 6-6 6M35 27l-6 17"/>',
        "markdown": '<path d="M20 42V29l6 7 6-7v13M40 28v14m-4-4 4 4 4-4"/>',
        "word": '<path d="m21 28 4 16 7-12 7 12 4-16"/>',
        "pdf": '<path d="M20 43V29h4a3.5 3.5 0 0 1 0 7h-4m10 7V29h3a7 7 0 0 1 0 14h-3m11 0V29h6m-6 7h5"/>',
        "image": '<rect x="20" y="27" width="24" height="18" rx="2"/><path d="m21 42 6-7 6 6 5-8 5 9"/><circle cx="27" cy="32" r="1.4"/>',
        "sheet": '<rect x="21" y="26" width="22" height="20" rx="2"/><path d="M21 33h22M21 39h22M28 26v20M36 26v20"/>',
        "slides": '<rect x="20" y="26" width="24" height="17" rx="2"/><path d="M32 43v5M27 48h10m-2-18 6 5-6 5V30Z"/>',
        "archive": '<path d="M29 24h6v5h-6v5h6v5h-6m0 0h6v7h-6v-7Z"/>',
        "audio": '<path d="M29 40V28l13-3v13M29 32l13-3"/><ellipse cx="25" cy="42" rx="4" ry="3"/><ellipse cx="38" cy="40" rx="4" ry="3"/>',
        "video": '<rect x="20" y="26" width="24" height="20" rx="2"/><path d="m29 31 8 5-8 5V31Z"/>',
        "font": '<path d="m21 44 8-17 8 17M25 36h8m8-1v9m0-7c-6-5-8 7-2 7h2"/>',
        "executable": '<rect x="22" y="26" width="20" height="20" rx="3"/><path d="M28 32h8v8h-8ZM26 22v4m6-4v4m6-4v4M26 46v4m6-4v4m6-4v4M18 30h4m-4 6h4m-4 6h4m20-12h4m-4 6h4m-4 6h4"/>',
    }[kind]
    sheet = 'M18 5.5h19.5l13 13V54a5 5 0 0 1-5 5h-27a5 5 0 0 1-5-5V10.5a5 5 0 0 1 5-5Z'
    return svg(
        '<defs><linearGradient id="glass" gradientUnits="userSpaceOnUse" x1="14" y1="6" x2="47" y2="59">'
        '<stop stop-color="#ffffff" stop-opacity=".78"/><stop offset=".5" stop-color="#e5eff8" stop-opacity=".48"/><stop offset="1" stop-color="#cadce9" stop-opacity=".58"/></linearGradient>'
        '<linearGradient id="rim" x2=".7" y2="1"><stop stop-color="#ffffff" stop-opacity=".95"/><stop offset="1" stop-color="#ffffff" stop-opacity=".32"/></linearGradient>'
        '<radialGradient id="shadow"><stop offset=".15" stop-color="#263b50" stop-opacity=".15"/><stop offset="1" stop-color="#263b50" stop-opacity="0"/></radialGradient></defs>'
        '<ellipse cx="32" cy="59" rx="22" ry="3" fill="url(#shadow)"/>'
        f'<path d="{sheet}" fill="url(#glass)" stroke="#3c5870" stroke-opacity=".2" stroke-width="1"/>'
        f'<path d="{sheet}" fill="none" stroke="url(#rim)" stroke-width=".65"/>'
        f'<path d="M37.5 5.5v8a5 5 0 0 0 5 5h8Z" fill="{colour}" fill-opacity=".18"/>'
        '<path d="M37.5 6v7.5a5 5 0 0 0 5 5H50" fill="none" stroke="#ffffff" stroke-opacity=".65" stroke-width=".8"/>'
        f'<g fill="none" stroke="{colour}" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">{content}</g>', 64)


# MIME icon names replace the MIME slash with a hyphen. This is a deliberately
# bounded set of common families; unknown types still inherit Adwaita/hicolor.
# These aliases describe file types only, not thumbnail/decoder capabilities.
DOCUMENT_ALIASES = {
    "generic": "application-octet-stream application-x-generic unknown application-x-empty",
    "text": "text-x-generic text-plain text-x-log text-x-readme",
    "code": "text-x-source text-html text-css text-x-python text-x-script text-x-shellscript application-x-shellscript text-x-javascript application-javascript application-json application-xml text-xml text-x-csrc text-x-chdr text-x-c++src text-x-java text-x-go text-x-rust text-x-yaml application-yaml",
    "markdown": "text-markdown text-x-markdown",
    "word": "x-office-document application-msword application-vnd.openxmlformats-officedocument.wordprocessingml.document application-vnd.oasis.opendocument.text application-vnd.oasis.opendocument.text-template application-rtf text-rtf",
    "pdf": "application-pdf",
    "sheet": "x-office-spreadsheet application-vnd.ms-excel application-vnd.openxmlformats-officedocument.spreadsheetml.sheet application-vnd.oasis.opendocument.spreadsheet application-vnd.oasis.opendocument.spreadsheet-template text-csv text-x-csv text-tab-separated-values",
    "slides": "x-office-presentation application-vnd.ms-powerpoint application-vnd.openxmlformats-officedocument.presentationml.presentation application-vnd.oasis.opendocument.presentation application-vnd.oasis.opendocument.presentation-template",
    "image": "image-x-generic image-png image-jpeg image-gif image-webp image-bmp image-tiff image-avif image-heic image-svg+xml",
    "audio": "audio-x-generic audio-mpeg audio-mp4 audio-ogg audio-flac audio-x-flac audio-wav audio-x-wav audio-aac audio-opus",
    "video": "video-x-generic video-mp4 video-mpeg video-webm video-x-matroska video-quicktime video-x-msvideo video-ogg",
    "archive": "package-x-generic application-zip application-x-tar application-x-compressed-tar application-gzip application-x-gzip application-x-bzip2 application-x-xz application-x-7z-compressed application-vnd.rar application-x-rar",
    "font": "font-x-generic font-ttf font-otf font-woff font-woff2 application-x-font-ttf application-x-font-otf application-font-woff",
    "executable": "application-x-executable application-x-pie-executable application-x-sharedlib application-x-object application-x-ms-dos-executable application-vnd.microsoft.portable-executable",
}


def build() -> dict[str, str]:
    global SYMBOLIC_GLYPHS
    outlines = json.loads(Path(__file__).with_name("symbolic_outlines.json").read_text())
    source = json.dumps(GLYPHS, sort_keys=True, separators=(",", ":")).encode()
    if outlines["source_sha256"] != hashlib.sha256(source).hexdigest():
        raise ValueError("Symbolic outline source changed; run compile_symbolic_outlines.py")
    SYMBOLIC_GLYPHS = outlines["glyphs"]
    ICONS.clear()
    register("apps", "meadow-browser google-chrome google-chrome-stable chromium chromium-browser firefox", tile("network"))
    register("apps", "meadow-glass-application", tile("grid"))
    register("apps", "meadow-glass-tile", tile(""))
    register("apps", "system-file-manager org.xfce.thunar thunar org.xfce.filemanager", files_tile())
    register("places", "folder folder-open inode-directory", folder())
    register("apps", "meadow meadow-os meadow-desktop meadow-shell", meadow_mark())
    register("apps", "meadow-launcher", tile("launcher"))
    register("apps", "utilities-terminal org.xfce.terminal org.xfce.terminalemulator org.xfce.terminal-settings xfce4-terminal terminal", tile("terminal", True))
    app_groups = {
        "settings": "preferences-system org.xfce.settings.manager xfce4-settings-manager systemsettings",
        "appearance": "preferences-desktop-theme preferences-desktop-appearance org.xfce.settings.appearance",
        "desktop": "preferences-desktop-display org.xfce.settings.display display computer video-display",
        "keyboard": "preferences-desktop-keyboard org.xfce.settings.keyboard",
        "mouse": "preferences-desktop-mouse org.xfce.settings.mouse",
        "accessibility": "preferences-desktop-accessibility org.xfce.settings.accessibility",
        "grid": "preferences-desktop-default-applications org.xfce.settings.default-applications",
        "session": "preferences-system-session org.xfce.session xfce4-session",
        "workspaces": "preferences-desktop-workspaces org.xfce.workspaces",
        "panel": "preferences-desktop-panel org.xfce.panel xfce4-panel",
        "picture": "preferences-desktop-wallpaper org.xfce.xfdesktop xfdesktop",
        "window": "preferences-system-windows org.xfce.xfwm4 xfwm4",
        "sliders": "preferences-system-windows-tweaks org.xfce.xfwm4-tweaks org.xfce.settings.editor",
        "colour": "preferences-color org.xfce.settings.color",
        "info": "help-about org.xfce.about xfce4-about",
        "search": "system-search org.xfce.appfinder xfce4-appfinder",
        "mail": "internet-mail org.xfce.mailreader",
        "network": "web-browser org.xfce.webbrowser",
        "logout": "system-log-out xfsm-logout",
    }
    for glyph, names in app_groups.items():
        register("apps", names, tile(glyph))
    places = {
        "home": "user-home folder-home", "desktop": "user-desktop folder-desktop", "recent": "document-open-recent folder-recent",
        "document": "folder-documents folder-templates", "download": "folder-download folder-downloads", "picture": "folder-pictures",
        "music": "folder-music", "video": "folder-videos", "network": "network-workgroup network-server folder-remote",
        "star": "folder-favorites user-bookmarks", "trash": "user-trash user-trash-full", "folder": "folder-publicshare folder-saved-search",
    }
    for glyph, names in places.items():
        symbols("places", names, glyph, regular=False)
        for name in names.split():
            if name.startswith("folder-") or name == "user-desktop":
                register("places", name, folder())
            else:
                register("places", name, svg(f'<path fill="{INK}" fill-rule="nonzero" d="{SYMBOLIC_GLYPHS[glyph]}"/>'))
    symbols("places", "folder folder-open inode-directory", "folder", regular=False)
    devices = {"drive": "drive-harddisk drive-harddisk-system drive-removable-media drive-optical", "desktop": "computer video-display", "usb": "drive-removable-media-usb media-flash", "network": "network-wired"}
    for glyph, names in devices.items():
        symbols("devices", names, glyph)
    actions = {
        "back": "go-previous go-first", "forward": "go-next go-last", "up": "go-up", "down": "go-down", "home": "go-home",
        "refresh": "view-refresh reload", "search": "edit-find system-search", "grid": "view-grid view-icon", "list": "view-list view-list-details view-list-compact",
        "menu": "open-menu application-menu", "more": "view-more", "plus": "list-add zoom-in", "minus": "list-remove zoom-out",
        "copy": "edit-copy", "paste": "edit-paste", "cut": "edit-cut", "edit": "document-edit edit-rename", "save": "document-save document-save-as",
        "trash": "edit-delete", "close": "window-close dialog-cancel process-stop", "check": "dialog-ok emblem-ok", "folder": "document-open folder-new",
        "document": "document-new", "info": "dialog-information help-info info", "settings": "emblem-system preferences-system tools", "appearance": "preferences-desktop-appearance appearance", "eject": "media-eject",
        "lock": "system-lock-screen changes-prevent", "pin": "view-pin", "desktop": "preferences-desktop-display", "download": "download", "upload": "upload",
        "stop": "media-playback-stop", "play": "media-playback-start", "microphone": "audio-input-microphone", "attach": "mail-attachment", "send": "mail-send send",
    }
    for glyph, names in actions.items():
        symbols("actions", names, glyph)
    # The launchpad motif is the nine restrained circles from the approved dock.
    grid = svg('<g fill="#61736e">' + ''.join(f'<circle cx="{x}" cy="{y}" r="1.6"/>' for y in (4, 12, 20) for x in (4, 12, 20)) + '</g>')
    register("actions", "view-app-grid view-app-grid-symbolic", grid)
    for name, colour in {
        "frost": "#c7d3db", "blue": "#6ba9ed", "mint": "#77bfa9",
        "lilac": "#ad91d0", "peach": "#e6ae8d", "rose": "#d895a8", "graphite": "#45505c",
    }.items():
        register("actions", "meadow-folder-color-" + name, folder_color(colour))
    for glyph, names in {"wifi": "network-wireless-signal-excellent network-wireless network-transmit-receive", "sound": "audio-volume-high audio-volume-medium", "battery": "battery-full-charged battery-full", "chat": "user-available", "network": "network-idle network-offline"}.items():
        symbols("status", names, glyph)
    for kind, names in DOCUMENT_ALIASES.items():
        register("mimetypes", names, document(kind))
    register("apps", "application-x-executable", tile("grid"))
    contexts = {"apps": "Applications", "places": "Places", "actions": "Actions", "mimetypes": "MimeTypes", "devices": "Devices", "status": "Status"}
    theme = ["[Icon Theme]", "Name=Meadow", "Comment=Original MeadowOS native desktop icons", "Inherits=Adwaita,hicolor", "Example=system-file-manager", "Directories=" + ",".join("scalable/" + c for c in contexts)]
    for path, context in contexts.items():
        theme.extend(["", f"[scalable/{path}]", "Size=" + ("48" if path in ("apps", "mimetypes") else "24"), "Type=Scalable", "MinSize=8", "MaxSize=512", f"Context={context}"])
    return {"index.theme": "\n".join(theme) + "\n", **ICONS}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="verify generated assets are byte-identical")
    args = parser.parse_args()
    assets = build()
    if args.check:
        # Enumerate names rather than relying on Path.exists(): a case-insensitive
        # development filesystem must not hide a missing Linux filename.
        actual = {str(path.relative_to(ROOT)) for path in ROOT.rglob("*.svg")}
        if (ROOT / "index.theme").is_file():
            actual.add("index.theme")
        mismatch = [name for name, text in assets.items() if name not in actual or (ROOT / name).read_text() != text]
        extra = sorted(actual - set(assets))
        if mismatch or extra:
            raise SystemExit(f"Icon assets differ: changed={mismatch}, extra={extra}")
        brand_mismatch = [str(path) for path, content in brand_assets().items()
                          if not path.is_file() or path.read_text() != content]
        if brand_mismatch:
            raise SystemExit(f"Brand assets differ: {brand_mismatch}")
        print(f"Verified {len(assets) - 1} original SVGs, index.theme and shared brand assets")
        return
    for name, text in assets.items():
        path = ROOT / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
    for path, content in brand_assets().items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
    print(f"Wrote {len(assets) - 1} original SVGs to {ROOT} and shared brand assets")


if __name__ == "__main__":
    main()
