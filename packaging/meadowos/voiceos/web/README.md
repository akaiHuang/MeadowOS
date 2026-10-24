# VoiceOS visual surface

`index.html` is the local, dependency-free visual surface hosted by the native
MeadowOS WebKit view. It can also be served unchanged in a browser for inspection.
It does not record audio or open model connections. Microphone access and agent
work belong to the host; a click only requests a state change.

At rest the surface contains a single procedural crystalline glaze disk on a
charcoal field. Its slowly advected edge light uses the same palette as the disk.
Following the user's four-edge LCD backlight reference, soft independent bands
run along the top, bottom, left and right. Their brightness and breadth drift
independently along each edge, with irregular broad lobes, thin stretches and
dimmer gaps. Light is strongest immediately inside the edge and fades at varying
depths, roughly 3–18% of the surface, rather than tracing a uniform rectangle.
The center stays dark. The earlier diagonal-corner
concept is superseded for the light placement; no water ripples or neon outline
are used.
The edge canvas is capped at 640 pixels per dimension and 18 fps; the existing
orb renderer is capped at 384 pixels, 24 idle / 30 audio-active fps. Hidden views,
page suspension, and reduced motion stop rendering loops. Context loss keeps a
static, palette-tinted fallback instead of an empty screen. Compact mode stops
the edge renderer, makes the page transparent, and centers a 42-pixel disk.

## Host contract

Load `index.html`, register `window.webkit.messageHandlers.meadowVoiceOS`, then
wait for `{type: 'ready', palette}`. The host is authoritative for audio state.

```js
window.MeadowVoiceOS.setState({
  phase: 'idle', // idle | listening | thinking | speaking | error
  energy: 0, // normalized 0..1 observed microphone/playback amplitude
  muted: false,
  compact: false,
  palette: 'meadow', // meadow | cobalt | jade | iron | bronze | frost
});
window.MeadowVoiceOS.getState();
window.MeadowVoiceOS.showPalette();
window.MeadowVoiceOS.hidePalette();
window.MeadowVoiceOS.dispose(); // only when permanently destroying the view
```

Updates are partial. Neither clicking the disk nor rendering an animated shader
changes `phase` to listening. Energy is effective only in host-confirmed listening
or speaking phases. A change of phase clears any old audio level. The shader's
existing 300 ms input timeout lets an interrupted level stream decay naturally.

Native messages:

| Message | Trigger |
| --- | --- |
| `{type: 'ready', palette}` | API and renderers initialized |
| `{type: 'toggle-microphone'}` | Fullscreen disk click / keyboard activation |
| `{type: 'expand'}` | Compact disk click, or palette request while compact |
| `{type: 'collapse'}` | Escape with no palette dialog open |
| `{type: 'palette', palette}` | User selects a palette |

When no WebKit handler exists, an embedded browser view posts the same message
to its parent with `source: 'meadow-voiceos'`. Hosts must check the sender window
and expected origin. A standalone page dispatches a `meadow-voiceos` CustomEvent
but never pretends a microphone is connected.

Palette choices persist under `meadow.voiceos.glaze.v1`; native hosts can override
the initial value using their own preference store after `ready`. Right click,
P, or a touch long press opens the palette dialog. Arrow keys select colors;
Escape closes it first, then collapses VoiceOS on the next press. No controls or
text are visible while the dialog is closed.

## Source provenance

The following project-owned assets were copied byte-for-byte on 2026-10-23 from
`fish-voice-call/web/`, the existing Meadow crystalline-glaze implementation:

| File | SHA-256 |
| --- | --- |
| `vendor/ink-orb.js` | `5836c1b393dba48c0a3ef7a283f7eda75ea86904ae519eb94cbd2b387a811283` |
| `vendor/glaze-material.js` | `fa77270a8700da07e6077e62a9bd7a7cdc241e2b88566af53774d34e9cca00ac` |
| `vendor/glaze-poster.webp` | `fd249345be28c6858427173681b22f00f9df0e5eb55d5b2559d759dcf699a3a6` |

The new edge-light shader, page controller, layout, and palette dialog live beside
those assets. The reference design is
`docs/desktop/verification/meadowos-voiceos-20261023/voiceos-dark-concept.png`.
This surface's visual verification is distinct from native lifecycle, microphone
transport, transcription, agent completion, and audio playback acceptance.


## Native work surfaces (2026-10-24)

The GTK host exposes Work Files, Tasks and Chat in the orb's context menu (P or right-click). These controls remain hidden in the standalone renderer preview; it has no Agent, workspace or desktop window ownership.

- Work Files lists the connected Agent workspace, one directory at a time. Hidden files and symbolic links are not traversed. The grid shows MIME icons; full Finder folder animation/color operations remain in the existing file manager.
- Tasks reads the current session's committed `task-plan` and refreshes on its SSE update. Empty plans stay empty; neither frontend generates todo content or completion status from prose.
- Explicit document opening uses native Quick Look on a private, bounded 32 MiB snapshot from pinned file descriptors. It retains the filename and original modification time. Closing the preview removes its snapshot; source files are unchanged.
- Work windows use shared glass/chrome, support dragging/resizing/pinning, and hide while the central VoiceOS is fullscreen. Collapse restores prior visible windows. A disconnected Agent marks task data as unsynchronized.
- Model-triggered native window presentation, meeting-note/MVP workflows, and physical microphone/playback acceptance remain separate unfinished requirements.
