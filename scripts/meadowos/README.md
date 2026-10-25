# MeadowOS build tools

For a local desktop, follow the [Docker setup guide](../../docs/QUICKSTART.md).
These tools are for contributors building native components or packaging a Linux image.

## Build environment

Build inside a Linux container or guest matching the target architecture:
`arm64` or `amd64`. The [Dockerfile](../../Dockerfile) includes the Ubuntu build
dependencies and the complete build sequence.

## Build native components

From the project root, choose the architecture of your Linux builder:

```sh
python3 scripts/meadowos/build.py \
  --arch arm64 --build-version dev --output dist/meadowos/native
```

The build creates the Agent and Code Desktop binaries. To include the window
and folder helpers:

```sh
python3 scripts/meadowos/build_window_resources.py \
  --arch arm64 --output dist/meadowos/window-resources
```

## Create a bundle

```sh
python3 scripts/meadowos/image.py render \
  --arch arm64 --build-version dev \
  --desktop-binary dist/meadowos/native/bin/meadow-desktop \
  --agent-binary dist/meadowos/native/libexec/meadow-agentd \
  --window-resources dist/meadowos/window-resources \
  --output dist/meadowos/bundle
```

This creates a desktop installation bundle. A bootable installer is not included.
For the Docker desktop, Compose handles building and starting the environment.

## Tool reference

| Tool | Purpose |
| --- | --- |
| `build.py` | Compile the native Agent and Code Desktop |
| `build_window_resources.py` | Compile window and folder UI helpers |
| `image.py` | Assemble and install desktop bundles |
| `linux_lab.py` | Start an isolated X11 development desktop |
| `prepare_novnc.py` | Prepare the browser desktop client |
| `configure_inception.py` | Connect Inception using an interactive prompt |

See [architecture](../../docs/ARCHITECTURE.md) and
[development and tests](../../docs/VERIFICATION.md) for the component map and test commands.
