# Native GTK3 window adapter

`libmeadow-hover-chrome.so` is a GTK3 module. It adapts the application's real
window and real HeaderBar, including Thunar's navigation controls; it does not
embed the application in another window or copy its pixels. The first version covers explicit GtkHeaderBar windows; server-side and custom
client-side decorations remain separate integration work.

Load only in the Meadow session after the isolated acceptance gate:

```sh
make -C packaging/meadowos/window_manager
GTK_MODULES=/absolute/path/libmeadow-hover-chrome.so application
```

The module preserves the content widget, focus, scroll position and native close,
minimize and maximize semantics. The header is an overlay, so hovering never
changes content allocation. Pointer entry within 12 logical pixels of the top
reveals it; leaving waits 380 ms. A focused header control or held pointer keeps
it visible. The C adapter currently binds Ctrl+Alt+Space, but xfwm intercepts
that combination in desktop QA; keyboard reveal is not accepted on this path.
Escape leaves controls when the adapter owns keyboard mode.
A touch beginning in the top edge reveals controls. Fullscreen windows suppress
the overlay. Maximized windows have square edges.

The adapter uses the shared `meadow-hover-window`, `meadow-hover-content`,
`meadow-hover-toolbar`, `meadow-chrome-visible`, `meadow-chrome-square` CSS state
names. Meadow-owned windows opt out with `meadow-hover-chrome-owned`. Dialogs,
popup windows, utility windows, undecorated custom windows and non-GTK3 apps are
excluded. Chrome custom tabs and address bars are never cropped or hidden.
GTK3 applications retaining extra menu/tool bars keep those application controls.
The opt-in module chains GtkWindow::show before initial realization; already-
realized windows are intentionally skipped rather than unmapped/recreated.

Rounded clipping is applied to the actual GTK drawing and X11 input/bounding
shape, not by placing a rounded card inside the window. Sidebar discovery tags known native GTK/Thunar side panes. When
`MEADOW_GLASS_BRIDGE_PATH` points to `desktop/glass/bridge.py`, the module starts
a persistent optical worker; `MEADOW_GLASS_CAPTURE` selects its native XComposite
helper. It reads real lower-window pixmaps and paints only the regional sidebar
material before GTK draws the original foreground. Source acquisition and optics
are shared with Meadow-owned windows. No root screenshot or fixed wallpaper is
pasted into the window.

Frames are requested at most four times per second, one in flight per window.
Pipe writes/reads are asynchronous; drawing never waits on the worker. A three
second timeout cancels a stalled request. Failure restores the original opaque
sidebar and retries with exponential backoff (up to 16 seconds). Closing the
window cancels its operations and terminates only its owned worker. HiDPI requests
use physical client-relative coordinates; Cairo paints using the GTK device scale.

`_MEADOW_GLASS_CAPTURE_STATUS` contains `[1, ready, sourceCount, width, height]`.
Ready records a valid regional frame, not visual acceptance. Actual sidebar and
revealed toolbar bounds use `_MEADOW_GLASS_REGIONS` as
`[1, count, x, y, width, height, kind, radius, ...]`; kind 1 is sidebar and 2 is
header. The current consumer paints sidebar only; toolbar optics remain pending.

The isolated native construction suite checks content identity, native controls,
hidden-input pass-through, exclusions and reopen idempotence. The real-backdrop
probe constructs two native windows and checks one lower-window source arrived
without losing the original editable content. Pointer, drag, resize, native
close/maximize and optical appearance still require the real desktop CUA gate.

Build requires `libgtk-3-dev`, `pkg-config` and a C compiler. No additional
runtime dependencies beyond GTK3/GDK X11, GLib, Cairo and Xlib are introduced.

## Thunar lifecycle correction

Thunar 4.20.7 attaches `window::remove` to notebook destruction
(`thunar-window.c:908` and `:1175`). Internal root reparenting must not invoke
that application shutdown handler. The adapter blocks existing `remove` handlers
only during its synchronous remove/reinsert transaction, then restores their
original block counts. GTK's container class closure still performs the real
parent change. A regression matching this exact Thunar lifecycle fails against
the initial adapter and passes after the fix; later genuine window destruction
still invokes the original cleanup handler. Real Thunar rendering and operations
remain an independent CUA acceptance gate.
