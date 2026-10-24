# Native Linux desktop adapter

`python3 desktop_control.py` consumes one UTF-8 JSON object on stdin and prints
one JSON reply. It is a short-lived native adapter, not a daemon or model loop.
The caller supplies a trusted `DISPLAY` and session bus environment; requests
cannot select a display or bus. Missing/unreachable context fails without
searching X sockets. The caller must resolve and authorize local target/output
paths before invoking it.

Common fields: `action`, optional observed `window_id`. Successful replies have
`ok`, `action`, `display`, `desktop` identity and observed data. Failure replies
have `ok=false`, `error.code/message` and exit status 1. An action dispatch or
application process is not proof that the user's goal was achieved.

Window `geometry` and `move_resize` describe the outer frame bounds, including
window-manager decorations, in selected-display pixel coordinates. The X11
fallback converts `_NET_FRAME_EXTENTS` so it agrees with libwnck. Accessibility
bounds and pointer coordinates are also display-absolute; they are not scaled
browser/noVNC CSS coordinates.

| action | input | observed output |
| --- | --- | --- |
| `status`, `snapshot`, `windows` | none | screen dimensions, native window IDs, PIDs, titles, classes, geometry/state and capability flags |
| `applications` | none | installed desktop IDs plus supported native aliases |
| `launch` | `application`, optional `target` | spawned PID when known, newly observed current-display windows, full window inventory; `window_observed` may be false |
| `open` | approved absolute local `target` or HTTP(S) URL, optional `application` | native launch receipt |
| `focus`, `close`, `minimize` | observed `window_id` | before/after state and observation flag |
| `maximize` | `window_id`, optional boolean `enabled` | before/after state |
| `move_resize` | `window_id`, optional integers `x,y,width,height` | observed geometry |
| `accessibility` | optional `window_id` (defaults active), `max_nodes` 1–400, `max_depth` 1–16 | scoped AT-SPI node IDs, names, roles, bounds, state, text and actions |
| `accessibility_action` | observed `node_id`, `action_name` or `action_index` | actual native return plus before/after node state |
| `set_text` | observed editable `node_id`, `text` | replaced text state; this does not implicitly click another widget |
| `click` | screen-absolute `x,y`, optional button 1–3, count 1–3, optional `window_id` | input dispatched and current windows |
| `move_pointer` | screen-absolute `x,y`, optional `window_id` | hover/native pointer movement |
| `drag` | screen-absolute `x,y,to_x,to_y`, optional button 1–3 and `window_id` | bounded native press/move/release |
| `scroll` | integer `delta_x,delta_y` -20–20, optional `window_id` | native wheel events at selected window's visible centre; positive means down/right |
| `key` | native keysym combination `key`, optional `window_id` | e.g. `ctrl+l`, `Return`, `Alt_L+F4`; native input receipt |
| `type_text` | Unicode `text`, optional `window_id` | native clipboard paste to active/focused window, then window observation |
| `screenshot` | approved new absolute PNG `output_path` with existing parent | atomic no-overwrite creation, path, native dimensions, byte count, SHA-256, selected display |

Native aliases are `finder`/`files`, `chrome`/`browser`, `terminal` and `settings`.
Applications discovered through `applications` can also launch by desktop ID.
Chrome uses a private profile per trusted DesktopSession/display. Files/Thunar
uses an app-lifetime private D-Bus session because its ordinary shared-service
forwarding can ignore a new DISPLAY. Its AT-SPI address is explicitly obtained
from the original selected session bus so observation stays on that desktop.
No existing app or foreign desktop service is stopped to launch a new window.
The owned Chromium child enables its native accessibility bridge through
`ACCESSIBILITY_ENABLED=1` and the complete renderer accessibility mode; global
screen-reader preferences are not changed.
No model-provided shell commands are accepted. Accessibility traversal is bound
to a window present on the selected display, its process ID and matching top
level. Node IDs end with a fingerprint of the observed name, role, native action
names and stable accessible ID when provided. Mutations reject a reused tree
index whose control identity changed. Password text is omitted. Accessibility
node IDs must be refreshed when the UI changes.

For diagnostics only, `operation` is accepted as an alias of `action`.
