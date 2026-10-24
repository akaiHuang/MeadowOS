# MeadowOS native UI ownership

- `icons/generate_icons.py`: canonical app/file symbols and the unframed green Meadow brand mark. Do not hand-edit generated theme SVG variants.
- `generate_cursors.py`: native pointer/resize artwork, Xcursor images, aliases and logical sizes. `scripts/meadowos/prepare_novnc.py` packages the same artwork for recognized remote cursors, preserving unknown vendor cursors.
- `sidebar_styles.py`: one shared native component-state contract, emitted into both self-contained GTK themes; includes navigation selections, keyboard focus, and native Settings panel/tab materials. `image.py render` rejects drift.
- `themes/Meadow-{Light,Dark}/gtk-3.0/gtk.css`: light/dark semantic colors and shared GTK widget primitives. Use `@meadow_fg`, `@meadow_muted`, `@meadow_selected` etc. rather than literal foreground colors in apps.
- `meadow_window_chrome.py`, `window-chrome.css`, `assets/window-controls/`: shared native controls and original application handlers.
- `../window_manager/meadow_hover_chrome.c`: native GTK3 material/chrome adapter. The explicit settings role or exact supported vendor executable identifies native settings content; ordinary document app views are not cleared globally.
- System Settings and Launcher share the application/settings catalog. The inventory retains desktop metadata, native panel route and visibility rationale.

A real supported Xfce panel is embedded using XEmbed in the Settings host; there is no inner titlebar or simulated preference form. Native panel actions still belong to the installed tool. A valid backdrop frame clears panel backing fills; capture failure restores readable theme paint.

GTK theme assets do not rewrite Chrome web content or third-party custom toolkits. The native frame service owns their OS frame; application-rendered internals require the application's theme API. Keep those acceptance claims separate.
