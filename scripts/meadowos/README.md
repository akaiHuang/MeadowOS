# MeadowOS Ubuntu development foundation

This builds and installs the **current native Go Desktop and Agent** into one
Ubuntu 26.04 / Xfce / X11 development guest. It does not install historical
Omarchy binaries, start another planner, contact a model, expose a URL, or modify
an existing host application. `render` creates an installation bundle, **not a
bootable VM disk, installer ISO, signed release or download-and-open launcher**.

Supported recipe targets are `arm64` and `amd64`. Each native GTK build must run
inside a Linux container/VM of its target architecture; recipe support is not
runtime acceptance for both architectures. Do not run `provision.sh` on a host
computer or on the existing macOS VM.

## Build and render

Build prerequisites inside the dedicated Ubuntu 26.04 builder:

```sh
apt-get update
apt-get install -y --no-install-recommends python3 git golang-go build-essential \
  pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
```

The active integration container mounts this checkout at `/src`. If its `.git`
points outside the mount, provide the real host commit and dirty status explicitly:

```sh
task_source_commit="$(git rev-parse HEAD)"
docker exec meadowos-foundation-20261021 python3 /src/scripts/meadowos/build.py \
  --arch arm64 --build-version 20261021.foundation1 \
  --source-commit "$task_source_commit" --source-dirty \
  --output /src/dist/meadowos/20261021.foundation1/native

python3 scripts/meadowos/image.py render \
  --arch arm64 --build-version 20261021.foundation1 \
  --desktop-binary dist/meadowos/20261021.foundation1/native/bin/meadow-desktop \
  --agent-binary dist/meadowos/20261021.foundation1/native/libexec/meadow-agentd \
  --output dist/meadowos/20261021.foundation1/bundle
```

Use `--no-source-dirty` only for an actually clean source tree. If Git is visible,
the builder checks the supplied commit against HEAD and reads the real status.
It records SHA-256 of the native/frontend/packaging source bytes before and after
the two builds and rejects concurrent source changes. The builder passes the
same `gtk3,meadow_vm_lab` tags and immutable portable/VM-Lab build identity to both
binaries. The renderer checks ELF CPU architecture, Go metadata, matching build
identities and exact binary hashes from `build-provenance.json`.

The development builder explicitly uses `-trimpath=false`, because Go omits
`-ldflags` from its embedded build settings when path trimming is enabled. Those
settings are required to inspect the immutable build identity before packaging.
This choice can retain build paths in development binaries; it is not a claim of
path-independent/reproducible public release artifacts. Provenance and strict
identity checks remain enabled.

For `amd64`, use an amd64 Ubuntu builder and replace all `--arch arm64` arguments
with `--arch amd64`. Do not reuse an ARM binary in an amd64 bundle.

The bundle includes a file/hash/mode manifest and source/build provenance. These
development receipts establish what was packaged; they are not a public signing
or authenticity chain. The included `cloud-init.yaml` is only a credential-free
bootstrap policy snippet and does not fetch or run unpinned payloads. A future
image builder must pin the official base image digest and apt repository snapshot
as well; the current recipe records installed package versions, not byte-for-byte
reproducible apt resolution.

### Optional native window resources

Build the three native window/folder helpers inside the target-architecture Linux builder; this
does not rebuild or restart Agent. In addition to the prerequisites above, the
builder needs `libx11-dev`, `libxcomposite-dev`, `libxrender-dev` and `libthunarx-3-dev`:

```sh
python3 scripts/meadowos/build_window_resources.py \
  --arch arm64 --output dist/meadowos/20261021.foundation1/window-resources
python3 scripts/meadowos/image.py render \
  --arch arm64 --build-version 20261021.foundation1 \
  --desktop-binary dist/meadowos/20261021.foundation1/native/bin/meadow-desktop \
  --agent-binary dist/meadowos/20261021.foundation1/native/libexec/meadow-agentd \
  --window-resources dist/meadowos/20261021.foundation1/window-resources \
  --output dist/meadowos/20261021.foundation1/bundle-with-window-resources
```

Use a fresh output directory and the matching architecture throughout. The
resource builder checks the Linux/dpkg target, C source hashes before/after
compilation and ELF architecture. Its `window-resources.json` records the hashes
of `desktop/glass/meadow-glass-capture`,
`window_manager/libmeadow-hover-chrome.so` and
`folder_materials/libmeadow-folder-materials.so`. The folder receipt includes
both implementation files and the painter header. Legacy two-helper receipts
remain supported. Rendering accepts only these known
paths and verifies their ELF architecture, binary hashes and source hashes
against the current checkout before copying them into the bundle.

When the packaged helpers and `desktop/glass/bridge.py` are present, `os_session`
appends the GTK adapter to `GTK_MODULES`, preserving existing modules, and sets
`MEADOW_GLASS_CAPTURE` / `MEADOW_GLASS_BRIDGE_PATH` to the installed paths. Set
`MEADOW_WINDOW_EFFECTS=0` before starting a new session to skip this automatic
configuration; it does not unload modules from already running applications.
Missing resources leave the existing environment unchanged. This is a regional
native XComposite **CPU glass prototype** for Meadow-owned windows and supported
GTK3 HeaderBars, separate from the rejected Picom experiment. Availability and
compile receipts are not visual acceptance (`visual_verified` remains false),
VM boot verification, GPU performance acceptance or a finished system-wide effect.

When the optional folder module is packaged, the bundle installs an identical
copy into `/usr/lib/<architecture-triplet>/thunarx-3/`. This is necessary on
Ubuntu builds that disable custom `THUNARX_DIRS`. The session preloads that
same installed file through `GTK_MODULES` (not a second copy), preserving
existing modules and stock Thunar extension paths. The share directory copy
is retained for its build receipt; bundle verification checks the installed
extension against that receipt. `MEADOW_FOLDER_EFFECTS=0` skips this addition
and the provider itself honors this flag when Thunar discovers it.
The module replaces directory glyphs in Thunar's native icon grid and adds an
official Thunarx color submenu. Native selection, activation, file actions,
labels and drag/drop remain owned by Thunar. Hover opens a preview, not the
directory: up to three real children fan upward. Empty folders stay empty.
The front cover blurs actual preview imagery with Cairo; this is internal
folder frost, not a WebGL refraction implementation or desktop backdrop capture.
Colors use a directory xattr where supported, with an inode-checked XDG fallback.
Directory reads are local, asynchronous and bounded; list/tree glyphs and
symlinks retain the normal renderer. The adapter is version-sensitive and
native UI acceptance must be repeated when changing Thunar versions.

## Install into the dedicated guest

Copy the complete bundle to the Ubuntu development guest. From its directory:

```sh
sudo ./provision.sh --arch arm64 --dedicated-dev-image
```

This validates the Ubuntu version, actual dpkg architecture, payload hashes,
existing user home/shell and destination conflicts **before apt or payload
installation**. It installs the declared package set, explicitly selects GNU
coreutils and classic sudo, checks Python/Node/Go minimum versions, installs the
native payload and records `installed-packages.tsv` / `provision-receipt.json`.
It creates a local `meadow` user with no shared password or SSH credential and a
development-only LightDM Meadow-session autologin. It does not grant passwordless sudo or
enable public remote access. Existing `/opt/meadow` is rejected: live upgrades
need separate idle admission and rollback implementation.

Ubuntu 26.04's `build-essential` metapackage depends directly on its protected
uutils selector. The recipe instead lists its compiler/header packages explicitly
and replaces that metapackage/selector in the **same** GNU-provider installation
transaction. It first simulates the exact request and rejects any removal outside
`build-essential`, `coreutils-from-uutils`, and `sudo-rs`; only then permits the
protected provider replacement. It verifies GNU command ownership and classic
sudo afterward. It never independently removes the working command provider.
To inspect the same package transaction without apt updates or installation:

```sh
sudo ./provision.sh --arch arm64 --dedicated-dev-image --simulate-packages
```

For a non-live staging check, first create a canonical temporary root with
`etc/os-release` copied from the **target Ubuntu guest**, then run:

```sh
python3 scripts/meadowos/image.py install \
  --bundle dist/meadowos/20261021.foundation1/bundle \
  --root /absolute/canonical/staging-root --arch arm64
```

Staging does not run apt, create users, enable units, start a display or execute
the native binaries. It rejects other OS versions, changed/missing files,
architecture mismatch, symlinked destinations and overwrites. Live-root aliases
such as `/tmp/..` and `//` canonicalize before the live-install guard.

## Installed contract

- `/opt/meadow/bin/meadow-desktop`: native Wails Linux Desktop.
- `/opt/meadow/libexec/meadow-agentd`: sole Agent authority.
- `/opt/meadow/share/meadow-os/desktop`: welcome, exact Meadow assets and themes.
- `/opt/meadow/share/meadow-os/shell`: native GTK desktop Shell and chat client.
- `/opt/meadow/share/meadow-os/bin/meadow-session agent|desktop|shell`: shared profile
  environment and native process exec; no background fallback daemon.
- `/opt/meadow/share/meadow-os/bin/meadow-os-session`: the Meadow X11 session supervisor.
- `/usr/share/xsessions/meadow.desktop`: LightDM's selected `meadow` session.
- `/usr/lib/systemd/user/meadow-agent.service`: one user Agent unit.

The default session starts `xfwm4 --compositor=on`, `xfsettingsd` and Meadow
Shell. Its stable alpha surfaces provide translucency, not backdrop blur.
`native_backdrop_blur` is false in the default status and bundle manifest.
The session refuses to replace another WM/compositor. It does not start the
ordinary Xfce panel/desktop, restore saved sessions, or autostart Welcome/Code
Desktop. The session is Linux/non-root only; a missing session bus is created
with `dbus-run-session`. Shell and ordinary applications are real X11 windows.

Picom GLX remains an explicitly rejected experimental option,
`MEADOW_EXPERIMENTAL_GLASS=1`. XRender had GTK redraw holes; GLX can blur actual
backdrops but intermittently shows blank preconstructed hidden windows on first
map. A token/PID/display/final-geometry first-paint handshake fixes the initial
desktop and ordered Shell recovery, but did not resolve that first-map failure.
Therefore this path is not the default or an accepted native-blur feature.
It owns only its compositor and retains WM/Agent/ordinary apps during Shell
recovery. Missing Picom/config uses xfwm4 at startup; a failed running Picom
retains the session with opaque GTK surfaces. VM GPU behavior and performance
remain separate checks. See the reference-fidelity verification document for
actual visible default behavior and retained experiment failures.

The guest's advertised 1920x1080 mode is selected when available. An unsupported
mode or multiple displays are left unchanged and recorded with actual geometry
in `$XDG_STATE_HOME/meadow-os/session.json`; neither case passes the fixed-canvas
acceptance gate. No custom GPU modeline or host display setting is installed.

Startup creates one private durable device UUID, imports the graphical session
environment into the user service manager, and enables/starts the sole Agent
unit. If the manager/service is unavailable, Shell still opens and the failure
is recorded, without spawning another Agent. Closing chat preserves Agent.
Unexpected Shell exits get at most three restarts; repeated failure opens an
actual recovery terminal while retaining the window manager and Agent. Session
shutdown cleans up only this supervisor's system children and stops an Agent
unit only if it was inactive/failed before this session started it. A pre-existing
or unknown-owner Agent is retained. Ordinary app shutdown/unsaved-document
negotiation is not an MVP feature.
Closing the VM or ending its graphical user session is not a 24/7 work guarantee.
The older `meadow-first-login`/Welcome command remains available for diagnostics,
but is not the Meadow session entrypoint.

Both processes use `$XDG_DATA_HOME/meadow-os/agent` (default
`~/.local/share/meadow-os/agent`) for `MEADOW_HOME` and
`MEADOW_DESKTOP_STATE_HOME`, its private `run/agent.sock` and token/catalog paths,
`MEADOW_ENABLE_PROMPT_GRANTS=1`, and `$HOME/MeadowWork` as workspace. Only Desktop
receives `MEADOW_AGENTD_MANAGED=1`; it authenticates and attaches to that existing
Agent rather than spawning or terminating a second one.

Private OS state is `$XDG_STATE_HOME/meadow-os/status.json` (default
`~/.local/state/meadow-os/status.json`). It starts with `private_url: null` and
`runtime_acceptance: not_verified`. Device identity is a random UUID, **not an
owner credential**. No model API key, tunnel credential, owner URL or login cookie
is generated or copied into this payload. Inject private model configuration
through a later explicit per-instance flow.

PostgreSQL, SearXNG, a fixed browser runtime, OpenGCUA Linux control and private URL
provisioning remain explicitly pending in this foundation. The package set
includes X11/AT-SPI support without disabling accessibility. The doctor prints
actual dependency and service checks, exits nonzero for this incomplete product
and never labels an active unit or an HTTP success as complete product readiness:

```sh
/opt/meadow/share/meadow-os/bin/meadow-doctor
```

## Focused checks

```sh
python3 -m unittest discover -s scripts/meadowos -p 'test_*.py' -v
python3 -m unittest discover -s packaging/meadowos/desktop/tests -p 'test_*.py' -v
python3 -m unittest discover -s packaging/meadowos/shell/tests -p 'test_*.py' -v
bash -n scripts/meadowos/provision.sh
```

These guard/session tests use isolated temporary roots and explicit fixtures.
They cover wrong target/CPU, absent native binaries/Shell, root aliases, planted paths,
manifest traversal, stable private identity, foreign-profile cleanup, no fallback
Agent, truthful display-mode results, bounded Shell crash recovery, diagnostic
welcome-on-failure and refusal to overwrite an installation. A real tiny
Go executable regression also verifies its executed linker values and embedded
packager identity even when `GOFLAGS=-trimpath` is inherited. They do not
claim Linux native UI, model, SDK, browser, VM boot/reboot or update acceptance.

## Local Linux desktop lab

The current native Shell MVP uses the separate Ubuntu26.04 ARM64 container
`meadowos-mvp-20261022` and its new non-root user `meadowmvp`. Its sole-member
dedicated bridge is `meadowos-mvp-20261022-private`, not Docker's default bridge.
Do not run these commands against the older foundation container or a VM.
The lab has the reviewed session/Shell development overlay in `/opt/meadow` and
the unchanged foundation3 native binaries; the checkout is mounted at `/src`.
This recipe starts an already prepared lab, not a package installer or VM image
builder. [MVP status and acceptance boundaries](../../docs/desktop/MEADOWOS_MVP_20261022.md)

Inspect the existing lab's network membership and actual loopback-only port:

```sh
task_lab=meadowos-mvp-20261022
docker inspect "$task_lab" --format '{{json .NetworkSettings.Networks}}'
docker network inspect meadowos-mvp-20261022-private --format '{{json .Containers}}'
docker port "$task_lab" 6080/tcp
```

The port output must be `127.0.0.1:<assigned-port>`; this run was `63582`.
Use the actual value to open
`http://127.0.0.1:<assigned-port>/vnc.html?autoconnect=1&resize=scale`.
Viewer scaling preserves the guest's 1920x1080 design surface. x11vnc listens on
container loopback; websockify exposes 6080 within this isolated container
network. Another member on that bridge could reach it directly, so loopback host
publishing alone is not the isolation boundary. This is local development
viewing, not the product's authenticated private-URL/pairing implementation.

Launch only when no previous lab runner/display/Agent remains:

```sh
docker exec -d --user meadowmvp -e HOME=/home/meadowmvp "$task_lab" sh -c \
  'exec dbus-run-session -- python3 /src/scripts/meadowos/linux_lab.py > /home/meadowmvp/lab-launch.log 2>&1'
```

`linux_lab.py` starts Xvfb `:93`, one explicit test-supervised Agent, Meadow
Xsession, x11vnc and websockify under one graphical D-Bus session. It refuses an
existing display/socket instead of replacing another session. The production
session continues to use its systemd user unit; the container's unavailable
systemd manager remains an explicit unverified gate. No model call is sent by
launching this runner.

The remote presentation path explicitly disables x11vnc wireframe and wire
copy replacement so held move/resize operations show real native content.
`prepare_novnc.py` stages a private copy of the installed noVNC client, preserving
its licenses and vendor files. The desktop text adapter maps Mac Command's
explicit editing/app allowlist (A/C/X/V/Z/Y/F/S/O/P/N, Enter and zoom keys) to
native Control, retaining Shift for redo. Command R stays in the host browser;
other Command combinations retain X11 Super for OS shortcuts. Windows/Linux
Control and OS Super keep their native meanings. Copy, cut and normal paste
use the guest clipboard consistently; host clipboard API access and automatic
cross-host synchronization are not enabled. An explicit browser Paste action
can still enter the existing textarea/input path. Browser Copy/Cut menu events
on the active remote input route to that same guest clipboard without reading
host clipboard data; active host composition retains its browser edit behavior.
Target applications remain
responsible for implementing their Control actions, including undo and find.
Desktop browsers also present the shipped Meadow pointer and resize
cursors as their original 24 CSS px SVGs after an exact raster RGB, dimensions
and hotspot match. This avoids RichCursor's one-bit alpha edges and keeps high
DPI displays sharp. Custom app cursors and touch fallback retain the native
noVNC path. An unrecognized keyboard or cursor implementation fails before
staging an approximate patch. Use a fresh viewer origin after changing the
client: an existing browser module graph can retain earlier modules.

Read the child-PID receipt and logs without dumping model configuration:

```sh
docker exec --user meadowmvp -e HOME=/home/meadowmvp "$task_lab" \
  cat /home/meadowmvp/.local/state/meadow-os/lab/processes.json
docker exec --user meadowmvp -e HOME=/home/meadowmvp "$task_lab" \
  tail -n 80 /home/meadowmvp/lab-launch.log
docker exec --user meadowmvp -e HOME=/home/meadowmvp "$task_lab" \
  cat /home/meadowmvp/.local/state/meadow-os/session.json
```

For a normal lab restart, signal the actual verified runner parent, wait for its
cleanup, then restart only this container. Do not hard-code historical PIDs,
use `pkill`, or delete an Agent socket to make a second runner start:

```sh
docker exec -i --user meadowmvp -e HOME=/home/meadowmvp "$task_lab" python3 - <<'PY'
import json, os, pathlib, signal, time
receipt = json.loads((pathlib.Path.home() / '.local/state/meadow-os/lab/processes.json').read_text())
xvfb = int(receipt['processes']['xvfb'])
status = pathlib.Path(f'/proc/{xvfb}/status').read_text()
parent = int(next(line.split()[1] for line in status.splitlines() if line.startswith('PPid:')))
proc = pathlib.Path(f'/proc/{parent}')
argv = (proc / 'cmdline').read_bytes().split(b'\0')
if proc.stat().st_uid != os.geteuid() or b'/src/scripts/meadowos/linux_lab.py' not in argv:
    raise SystemExit('Runner identity did not verify; inspect the dedicated lab before stopping anything.')
def birth():
    return (proc / 'stat').read_text().rsplit(')', 1)[1].split()[19]
original_birth = birth()
os.kill(parent, signal.SIGTERM)
deadline = time.monotonic() + 45
while time.monotonic() < deadline:
    try:
        if birth() != original_birth:
            break
    except FileNotFoundError:
        break
    time.sleep(.1)
else:
    raise SystemExit('Runner cleanup is still pending; inspect its logs before restarting.')
print('The verified lab runner exited; private profile and workspace were retained.')
PY
docker restart "$task_lab"
```

Then repeat the launch command and `docker port` readback. The container's
private profile and workspace persist through this restart. Updating a reviewed
source overlay is a separate development step; restarting does not create a new
packaged build receipt. A successful restart command does not by itself verify
task/session recovery, systemd boot or VM reboot acceptance.
