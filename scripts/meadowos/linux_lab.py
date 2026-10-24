#!/usr/bin/env python3
"""Run the native Meadow session in a dedicated Linux container, not a VM.

The container must be alone on a dedicated Docker network, with 6080 published
on host loopback only. Shared bridge peers can bypass a loopback host publish.
The VNC server itself listens only on container loopback. This is a local lab,
not the product's authenticated remote-access or systemd boot implementation.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time

from prepare_novnc import prepare


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prefix", type=Path, default=Path("/opt/meadow"))
    parser.add_argument("--display", default=":93")
    args = parser.parse_args()
    if sys.platform != "linux" or not Path("/.dockerenv").exists() or os.geteuid() == 0:
        parser.error("run as the dedicated non-root desktop user inside the Linux lab container")
    if not args.display.startswith(":") or not args.display[1:].isdigit():
        parser.error("a local numbered X display is required")
    if not os.environ.get("DBUS_SESSION_BUS_ADDRESS"):
        parser.error("launch under dbus-run-session so every child shares one graphical session bus")
    if Path(f"/tmp/.X11-unix/X{args.display[1:]}").exists():
        parser.error("display already exists; refusing to replace another session")
    entry = args.prefix / "share/meadow-os/bin/meadow-session"
    if not entry.is_file():
        parser.error("install a Meadow bundle first")
    data = Path(os.environ.get("XDG_DATA_HOME", str(Path.home() / ".local/share"))) / "meadow-os/agent"
    if (data / "run/agent.sock").exists():
        parser.error("Agent socket already exists; do not start a second Agent")
    state = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "meadow-os/lab"
    state.mkdir(parents=True, mode=0o700, exist_ok=True)
    runtime = Path.home() / ".runtime-meadow-lab"
    runtime.mkdir(mode=0o700, exist_ok=True)
    runtime.chmod(0o700)
    env = dict(os.environ, DISPLAY=args.display, GDK_BACKEND="x11",
               XDG_RUNTIME_DIR=str(runtime), XDG_SESSION_TYPE="x11",
               XDG_CURRENT_DESKTOP="Meadow:XFCE")
    processes, logs = [], []
    client_directory = tempfile.TemporaryDirectory(prefix="novnc-", dir=state)
    client = Path(client_directory.name) / "web"
    prepare(Path("/usr/share/novnc"), client)
    stopping = False

    def stop(_sig, _frame):
        nonlocal stopping
        stopping = True

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, stop)

    def start(name, command):
        log = (state / f"{name}.log").open("wb")
        os.chmod(log.name, 0o600)
        logs.append(log)
        process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        processes.append((name, process))
        return process

    def wait_ready(check, seconds=15):
        until = time.monotonic() + seconds
        while time.monotonic() < until and not stopping:
            if check():
                return
            dead = [(name, child.returncode) for name, child in processes if child.poll() is not None]
            if dead:
                raise RuntimeError(f"lab process exited: {dead}; inspect {state}")
            time.sleep(.1)
        raise RuntimeError("lab readiness timed out")

    def vnc_ready():
        try:
            with socket.create_connection(("127.0.0.1", 5900), timeout=.2):
                return True
        except OSError:
            return False

    try:
        start("xvfb", ["Xvfb", args.display, "-screen", "0", "1920x1080x24", "-nolisten", "tcp"])
        wait_ready(lambda: Path(f"/tmp/.X11-unix/X{args.display[1:]}").exists())
        # Explicit test supervision replaces systemd only in this container lab.
        # Production session setup never silently starts this fallback.
        start("agent", [str(entry), "agent"])
        wait_ready(lambda: (data / "run/agent.sock").exists())
        start("session", [str(entry), "os-session"])
        start("vnc", ["x11vnc", "-display", args.display, "-localhost", "-rfbport", "5900",
                      "-forever", "-shared", "-nopw", "-noxdamage", "-nowireframe", "-nowirecopyrect"])
        wait_ready(vnc_ready)
        start("viewer", ["websockify", "--web", str(client), "0.0.0.0:6080", "127.0.0.1:5900"])
        receipt = {"environment": "dedicated Ubuntu container/Xvfb, not VM",
                   "display": args.display, "geometry": [1920, 1080],
                   "systemd_boot_verified": False, "public_remote_access": False,
                   "processes": {name: child.pid for name, child in processes}}
        receipt_path = state / "processes.json"
        receipt_path.write_text(json.dumps(receipt, indent=2) + "\n")
        receipt_path.chmod(0o600)
        print(json.dumps(receipt), flush=True)
        while not stopping:
            for name, child in processes:
                if child.poll() is not None:
                    raise RuntimeError(f"{name} exited {child.returncode}; inspect {state}")
            time.sleep(.5)
        return 0
    finally:
        for _name, child in reversed(processes):
            if child.poll() is None:
                try:
                    os.killpg(child.pid, signal.SIGTERM)
                    child.wait(timeout=5)
                except ProcessLookupError:
                    pass
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=5)
        for log in logs:
            log.close()
        client_directory.cleanup()


if __name__ == "__main__":
    sys.exit(main())
