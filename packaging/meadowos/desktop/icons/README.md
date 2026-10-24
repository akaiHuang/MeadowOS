# Meadow native icon theme

This is original SVG artwork for the native Linux icon-theme lookup. It is not
an HTML icon layer. `Meadow/index.theme` inherits `Adwaita,hicolor` for icons not
owned by Meadow, including the installed Chrome and other third-party brands.

The desktop application canvas is 64 × 64 with a 54 px optical body (40.5 px at
the shell's 48 px size). System tools use the same silver grey squircle and white
line family. Files uses a layered blue folder, Terminal uses a graphite tile,
and Meadow uses the transparent double-wave mark. Navigation artwork is a
single `#61736e` colour on a 24 px canvas with 1.75 px round strokes. The GTK
symbolic loader recolours `-symbolic` names for selected and dark surfaces.
Symbolic assets use actual filled vector outlines, rather than SVG strokes:
GTK's symbolic renderer overrides stroke/fill presentation and otherwise turns
open arrows and hollow monitors into solid shapes.

Core names:

| Purpose | Native icon name |
| --- | --- |
| Applications | `view-app-grid-symbolic` |
| Files | `system-file-manager` |
| Terminal | `utilities-terminal` |
| Meadow | `meadow` |
| Settings | `preferences-system` |
| Appearance control | `preferences-desktop-appearance-symbolic` |
| Display control | `preferences-desktop-display-symbolic` |
| Information control | `help-info-symbolic` |
| Tools control | `tools-symbolic` |

The aliases include every Xfce application icon name observed in the MeadowOS
MVP's native `.desktop` files. The six native theme contexts are applications,
places, actions, MIME types, devices and status. Every normal `folder-*` name
and `user-desktop` retains the layered blue folder artwork for the file grid;
sidebars and toolbars request the separate hollow `-symbolic` variants. A
large standard directory must never become an enlarged sidebar pictogram.

Rebuild deterministically with Python's standard library:

```sh
python3 packaging/meadowos/desktop/icons/generate_icons.py
python3 packaging/meadowos/desktop/icons/generate_icons.py --check
python3 -m unittest discover -s packaging/meadowos/desktop/tests -p test_icon_theme.py
```

The checked-in `symbolic_outlines.json` is generated from the original glyphs
and guarded by their SHA-256. To change their geometry, use an authoring virtual
environment with `fonttools==4.67.0` and `skia-pathops==0.9.2`, then run
`python compile_symbolic_outlines.py` before rebuilding. Those packages are
authoring tools only; neither the native OS nor the packaging check uses them.

Install the `Meadow` directory under `/usr/share/icons/Meadow`, select
`Meadow` as the GTK icon theme, and rebuild the native icon cache with
`gtk-update-icon-cache -f -t /usr/share/icons/Meadow` when that utility is
available. The generator and this README are source tools, not runtime assets.
