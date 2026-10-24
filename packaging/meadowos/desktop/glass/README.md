# Native Meadow glass prototype

This is an original, region-scoped X11 material renderer. Liquid Glass Studio is
the primary optical reference: separate sharp/blurred backdrop textures, rounded
edge normals, refraction, slight dispersion and directional rim light. No source
code or artwork from that project is bundled here.

`capture.c` reads named XComposite pixmaps of mapped root frames below the target
in actual X11 stacking order. It never samples a screen screenshot or the target
window. The existing compositor continues owning the display. The helper neither
redirects windows nor changes compositor/WM configuration. Only intersecting
regions are read, with a 40px optical margin. Wallpaper is just another lower
native surface. The root fallback is neutral; root pixmap wallpapers without a
native desktop window are not reproduced by the current prototype.

`optics.py` blurs the region, displaces backdrop sampling along the signed-distance
normal of the rounded edge, mixes a clearer edge and adds a restrained highlight.
`gtk_surface.py` paints the result before real GTK child widgets, leaving text and
controls sharp. It is a CPU prototype capped at 8 updates/second, not a GPU
compositor implementation or a claim of complete Liquid Glass parity.

## Build and native integration

Runtime dependencies: X11/XComposite/XRender shared libraries, Python 3,
PyGObject/GTK3, PyCairo, NumPy and Pillow. Build dependencies: C compiler plus
`libx11-dev`, `libxcomposite-dev`, `libxrender-dev`. On Ubuntu, Python package names
are `python3-gi`, `python3-cairo`, `python3-numpy`, and `python3-pil`.

Compile once on the target Linux architecture (ARM64 or AMD64):

```sh
python3 glass/build.py --output /installed/desktop/glass/meadow-glass-capture
```

The default helper is a sibling of `backdrop.py`; `MEADOW_GLASS_CAPTURE` can point
to a staged binary. No paint handler compiles or installs anything.

```python
from glass.gtk_surface import GlassSurface

# Before owner realize; the native chrome helper can perform this setup.
owner.set_visual(owner.get_screen().get_rgba_visual())
surface = GlassSurface(owner, mode="sidebar", corner_radius=0)
surface.add(existing_sidebar)
root.pack_start(surface, False, False, 0)
```

The native root and intervening containers must be transparent; paint the main
content body separately. Keep sidebar and list backgrounds transparent. Use
radius0 where native outer chrome already clips the shared window outline.
Destroying the widget closes its owned reader. Missing helper/render failure
uses a usable translucent fallback and exposes `last_error`; fallback is not a
successful blur/refraction receipt. No user model/API operation is involved.

## Foreign GTK adapter protocol

`python3 /installed/desktop/glass/bridge.py --display :DISPLAY` serves optical pixels for a native
adapter. Request: `hex-XID x y width height radius mode\n`. Legacy modes are
`sidebar` and `toolbar`. Native Thunar uses `split:<x>` or `split-dark:<x>`:
`x` is the sidebar's right edge in physical pixels within the full content
allocation; zero means the sidebar is hidden. One backdrop capture supplies
both panes. The left material uses blur18/tint.42 and the content uses
blur28/tint.68. The join has no rounded mask or edge highlight; only the existing
native outer clip rounds the perimeter. Foreground GTK text/icons/selection are
drawn afterward. The result is precomposed opaque BGRA from actual lower-window
pixels, not opacity applied to the entire native widget.

The joined request is bounded to 4096px per dimension / 4,194,304px total.
The helper allows the additional 40px optical margin per side, bounded to
4176px per dimension / 5,000,000px total. The legacy modes keep their original
2048px / 2,000,000px bounds. The C adapter polls at most four times/second with
one request in flight. Requested and completed geometry are separate: size,
scale, split or theme mismatch restores the normal opaque native background
until a matching frame arrives. Capture failure also removes the glass class.

Response: four native uint32 values `(status, source_count, width, height)`, then
premultiplied BGRA bytes only on status0. Adapters must read asynchronously, keep
at most one in-flight request, retain a usable fallback, and dispose only their
own worker. The native GUI must never block on the worker in a draw callback.

Proposed optional metadata property `_MEADOW_GLASS_REGIONS` (CARDINAL):
`[1, count, x,y,width,height,kind,radius, ...]`, where coordinates are client-relative
and kind1 means sidebar, kind2 means toolbar. This property itself has no renderer
consumer. A native draw adapter or compositor integration must actually paint it.
Malformed, timed-out or partial reader replies permanently invalidate that owned
helper and pipe. The GTK/bridge owner may create at most two fresh helpers; it
never consumes a previous region's late pixels. Repeated failures retain the
usable fallback. `GlassSurface.reader_restarts` exposes this bounded recovery.

`GlassSurface.set_corner_radius()` accepts a scalar or `(TL,TR,BR,BL)` tuple;
sidebar `(16,0,0,16)` preserves square inner corners. The Python GTK adapter
converts logical GTK geometry to physical X11 pixels and sets Cairo device scale.
The foreign bridge uses physical client-relative pixels. Its scalar radius
protocol remains unchanged. HiDPI source support is implemented but has not
received native display acceptance in this scale1 probe.

## Evidence and remaining acceptance

An isolated Ubuntu ARM64/Xvfb `:97` native probe passed with one actual lower GTK
window and one target GTK window. Changing target text did not change the backdrop
hash. Changing the lower window did. The edge differed measurably from a matched
zero-refraction render, and foreground text remained sharp in the GTK widget
render. It used the existing xfwm compositor, not the rejected Picom path.
Measured CPU material rendering for 300×380 was approximately48–77ms; no VM GPU
performance or multi-window acceptance is implied. A failed initial probe metric
sampled a top edge while its source varied horizontally; that evidence remains
separate from the corrected side-edge comparison.

Run the fixture only in an owned isolated graphical session:

```sh
PYTHONPATH=/path/to/desktop python3 -m glass.probe --output /owned/evidence
```

It refuses live display`:93`. Native acceptance still needs actual full-window
screenshots, moving/resizing targets, z-order changes, translucent lower windows,
remote viewing, and target VM/graphics performance. Outer compositor shadows are
not captured as source windows. Polling must be replaced or augmented with
damage-driven invalidation before treating many concurrent surfaces as finished.
Lower windows with separate `_NET_WM_WINDOW_OPACITY` or XShape bounding masks are
outside the current fidelity acceptance: the reader composites per-pixel ARGB
and opaque TrueColor pixmaps, but does not apply those additional WM attributes.
