# Alpha release verification

Release acceptance uses fresh GitHub Linux runners and a new Docker home volume.
It never reuses the maintainer's desktop, API connection or conversations.

Source under test: `d4477c88ba6ac8c593b3fa671bf31f6a822db0b8`.
[Native CI run](https://github.com/akaiHuang/MeadowOS/actions/runs/40124109231).

## Separate evidence layers

| Check | What it establishes |
| --- | --- |
| Secret scanner | Gitleaks 8.30.1 scans all fresh public Git history with redacted output; source is also scanned before publication |
| Native Linux API integration | Real authenticated Unix listener, configuration lease, credential store, immutable conversation bindings, queue and harness; the model is an in-process synthetic fixture |
| 93 offline contracts | Build provenance, session transport, hidden-input setup, reconnect/new-chat behavior process cleanup/socket recovery and narrowly classified WebKit failures |
| Native desktop smoke | Source-built Agent, GTK Shell, Files, Terminal and Settings in a fresh 1920 × 1080 X11 desktop; exactly one native Agent |
| Restart check | Native desktop recovers and a real work file survives a normal container restart |
| WebKit probes | Independent launches of Wails Desktop and GNOME Web; a recognized namespace restriction is recorded as unavailable, never browser success |
| Screenshot review | Actual captured pixels inspected for rendered native UI and absence of credentials/private content |

The desktop jobs cover Linux `amd64` and `arm64`. Each artifact contains health,
smoke and restart receipts. `acceptance.json` separates `core_passed` from
individual WebKit capability outcomes. Other launch failures still fail CI.
The container security policy is unchanged during probes.

## Observed release results

The linked run passed on both `amd64` and `arm64`, including the real desktop
restart and work-file persistence check. Native API integration and all 93
offline tests passed on Linux.

The ARM64 Wails capture was manually reviewed after its first-paint wait: the
welcome panel, text and controls rendered. This establishes initial content
rendering only. GNOME Web remained unavailable with the exact namespace denial;
it is not counted as a working browser. The published screenshots show GTK
Shell/Files/Terminal, Chat and Settings from that clean ARM64 run.

## Boundaries

No real Mercury inference or paid provider task is part of this acceptance.
The setup helper's official model-list check, the local fixture integration and
a real model completing a user's task are separate claims.

These checks do not establish a bootable VM, finished installer, universal
application styling, GPU performance, Linux speech or Wayland support.
See [known runtime limitations](QUICKSTART.md#known-webkit-container-limitation).
