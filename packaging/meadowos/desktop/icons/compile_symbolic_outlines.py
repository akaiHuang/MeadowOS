#!/usr/bin/env python3
"""Compile original round strokes to GTK-compatible filled vector outlines.

Authoring-only dependencies: fonttools==4.67.0, skia-pathops==0.9.2. Runtime
packaging and generate_icons.py --check require only Python's standard library.
"""
import hashlib
import json
from pathlib import Path

import pathops
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.svgLib.path import SVGPath

from generate_icons import GLYPHS


def compile_outlines():
    glyphs = {}
    for name, body in GLYPHS.items():
        path = pathops.Path()
        # FontTools accepts matrix transforms. This is the exact matrix of the
        # original gear's translate(.5 -1) scale(1 .96), without changing art.
        body = body.replace('transform="translate(.5 -1) scale(1 .96)"',
                            'transform="matrix(1 0 0 .96 .5 -1)"')
        SVGPath.fromstring('<svg xmlns="http://www.w3.org/2000/svg">' + body + '</svg>').draw(path.getPen())
        path.stroke(1.75, pathops.LineCap.ROUND_CAP, pathops.LineJoin.ROUND_JOIN, 4)
        path.convertConicsToQuads(0.02)
        path = pathops.simplify(path)
        pen = SVGPathPen(None, ntos=lambda number: f"{number:.4f}".rstrip("0").rstrip("."))
        path.draw(pen)
        glyphs[name] = pen.getCommands()
    source = json.dumps(GLYPHS, sort_keys=True, separators=(",", ":")).encode()
    result = {
        "format": 1,
        "source_sha256": hashlib.sha256(source).hexdigest(),
        "authoring_tools": {"fonttools": "4.67.0", "skia-pathops": "0.9.2"},
        "stroke_width": 1.75,
        "curve_tolerance": 0.02,
        "glyphs": glyphs,
    }
    target = Path(__file__).with_name("symbolic_outlines.json")
    target.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
    print(f"Compiled {len(glyphs)} original glyphs to {target}")


if __name__ == "__main__":
    compile_outlines()
