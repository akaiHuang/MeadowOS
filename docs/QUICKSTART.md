# Run the MeadowOS container preview

MeadowOS is an Ubuntu 26.04 native desktop alpha. This path builds the Go Agent,
Go Desktop, GTK Shell and native window helpers from source, then runs the native
Shell preview in Docker with a browser viewer. The core preview and WebKit app
capabilities are verified separately. It supports Linux `amd64` and `arm64`; Docker
Desktop supplies Linux on macOS and Windows. This container recipe is not a
bootable disk or VM installer.

You need Git and Docker with Compose v2. Allow several GB of free disk and memory
for the initial Ubuntu packages and native build. Build on the machine's native
architecture; running another architecture through emulation is not covered by
the native smoke check.

## Build and open

From a clean clone of this repository:

```sh
docker compose build --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)"
docker compose up -d
```

The source commit is required. Git history, Git credentials and `.env` files are
excluded from the build context. The builder records the supplied full commit
and hashes the actual native source bytes before and after compilation. The
renderer rechecks those bytes and both binary identities before packaging.
These development receipts are not a signed release or a reproducible build
claim.

Open [the local MeadowOS desktop](http://127.0.0.1:60511/vnc.html?autoconnect=1&resize=scale).
Use `127.0.0.1` in this URL. The viewer and native text/attachment companion admit
that exact browser origin. The desktop renders at 1920 × 1080 and the viewer
scales it to the browser window.

A fresh home opens the Shell and supports Files, Terminal and settings before
any model is configured. Chat shows that the model needs configuration. WebKit
apps, including the separate Go Desktop UI and GNOME Web, have the container
limitation described below. The image contains no model API key, Meadow login, private
Core runtime or model weights.

Check the actual native session:

```sh
docker compose ps
docker compose exec meadowos meadowos-healthcheck
```

`desktop_ready` and `agent_connected` should be `true`. `model_ready` stays
`false` until a working external API is configured. This check verifies the
Agent's Unix peer and executable fingerprint, painted Shell, X11 window manager,
viewer and companion bridge; it does not call a model.

## Connect your Inception API

In a terminal, run the setup helper and enter your own API key at its hidden
prompt:

```sh
docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
```

The helper sends the key through the authenticated local Agent socket. The Agent
stores the key in a separate private credential file in the user home and applies
the connection. The key is not a Docker build argument or environment variable.
API usage is billed by your provider. No Meadow account is required for this
build.

After first setup, open Chat, choose **聊天選單 ⋮ → 重新連線**, and send a small
request. For a later key/connection change, choose **聊天選單 ⋮ → 新對話** to create
an explicit new conversation while retaining previous work. For example, ask it to create a
text file in `~/MeadowWork` and then open that folder in Files. A configured
connection alone does not establish successful model execution; confirm the
reply and the actual resulting file.

The Agent runs with full access to this dedicated container's desktop, shell and
files. The default Compose file mounts its named home volume and exposes neither
your host home nor the Docker socket.

## Keep your work

The Compose volume `meadow-home` holds `/home/meadow`, including `MeadowWork`,
chat history, settings and user-supplied credentials. Ordinary stop/start and
image rebuilds retain that data:

```sh
docker compose down
docker compose up -d
```

To copy a file out:

```sh
docker compose cp meadowos:/home/meadow/MeadowWork/example.txt ./example.txt
```

To deliberately erase this instance's home and credentials, run
`docker compose down --volumes`. That removes the named home volume; keep any
needed work first.

## Viewer, input and ports

Compose publishes only these host-loopback ports:

| Host | Container | Purpose |
| --- | --- | --- |
| `127.0.0.1:60511` | `6080` | noVNC desktop viewer |
| `127.0.0.1:60531` | `6081` | bounded text composition and original attachment ingress |

VNC itself listens on container loopback. A small route-limited proxy bridges
the published companion port to its existing loopback service, preserving exact
Host/Origin admission. A dedicated Docker network contains only this desktop.
The viewer is a local development transport; it provides no public pairing URL.

Keep both default host ports. If either is occupied, stop the other service or
use a separate Docker host. Changing only the Compose port mappings breaks the
fixed companion origin/endpoint contract.

Host IME composition and attachment transfer use the native Shell's private
spool. Attachments are bounded at 20 MiB per original file. Linux speech
recognition and speech synthesis are unavailable in this image: it includes
neither the private macOS speech runtime nor a speech model. The VoiceOS visual
surface remains present.

## Known WebKit container limitation

The GTK Shell, Files, Terminal, settings and terminal API setup flow do not need
WebKit. GNOME Web and the separate Wails/Go Desktop interface do. On default
Docker hosts, Bubblewrap can be denied the nested namespaces and mounts needed
by WebKit, with `No permissions to create a new namespace` followed by
`Failed to fully launch dbus-proxy`.

In the ARM64 preflight of source `9bcb5305`, the Wails window mapped but its
captured content was black. Its content rendering remains unverified; window
mapping is not rendering success. The separate GNOME Web failure below is a
measured namespace restriction, not an assumed explanation for the black Wails
capture.

That exact observed failure is reported as an **unsupported container capability**,
not a successful browser or Go Desktop rendering check. The native core can still
pass its own checks. Container security and browser sandbox settings remain
unchanged: this recipe does not grant privileges, change host sysctls, disable
sandboxing, or use an unconfined policy. Use a dedicated native Ubuntu graphical
guest for the WebKit interfaces; its boot/runtime acceptance is separate from
this container preview.

The Inception setup helper and GTK Chat provide the core API workflow without
the separate WebKit Desktop UI. `desktop_ready: true` reports the native Shell
core; it does not establish WebKit app support.

## Troubleshooting and development checks

Inspect process logs without dumping the private Agent profile:

```sh
docker compose logs --tail=100 meadowos
docker compose exec meadowos tail -n 80 /home/meadow/.local/state/meadow-os/desktop-lab.log
docker compose exec meadowos tail -n 80 /home/meadow/.local/state/meadow-os/lab/session.log
```

A shared home can have only one owning container. Stop that instance before
starting another container with the same volume. Native process supervision
ends the owned session on container shutdown; this recipe does not test systemd
boot, an always-on host service, VM reboot or update rollback.

Window/folder helpers are built by default. To build the simpler native alpha
composition without those optional helpers:

```sh
docker compose build --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" --build-arg MEADOW_WINDOW_RESOURCES=0
docker compose up -d --force-recreate
```

For CI (with Python3 available on the build host), `bash docker/smoke.sh` builds from the current Git commit, creates a new
Compose project with a fresh home/network, checks the unconfigured Agent,
opens real Files/Terminal/settings windows, probes a local page in GNOME Web
and the managed Go Desktop against the same Agent, captures
actual native states or capability failures, restarts
the container and verifies a persisted file. It removes only its own test project
and volume afterward. Both default viewer ports must be free before this check;
it refuses to replace a running instance. Results are written to `artifacts/meadowos-smoke/` by
default. `acceptance.json` distinguishes `core_passed`, restart persistence and each
WebKit capability status. Only the exact observed namespace/dbus-proxy restriction
is recorded as unsupported; unknown launch failures still fail the smoke.
Window-title checks establish mapping; review the captured pixels before claiming
that WebKit content rendered. Browser sandbox settings remain unchanged.
The smoke sends no model requests and does not establish model quality
or VM boot acceptance.
