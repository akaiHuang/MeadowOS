#!/usr/bin/env python3
"""Own a fresh container desktop; model planning remains in native Agent."""
import errno
import fcntl
import json
import os
from pathlib import Path
import signal
import socket
import stat
import subprocess
import sys
import time


def private_directory(path):
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise RuntimeError(f"Container state must be an owned private directory: {path}")


def remove_stale_socket(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid():
        raise RuntimeError("Existing Agent socket is unsafe")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as probe:
        probe.settimeout(1)
        try:
            probe.connect(str(path))
        except OSError as error:
            if error.errno not in (errno.ECONNREFUSED, errno.ENOENT):
                raise RuntimeError("Existing Agent socket could not be checked") from error
        else:
            raise RuntimeError("Another Agent already owns this home; stop that container first")
    if path.exists():
        path.unlink()



def seed_terminal_defaults(home):
    """First-run appearance only; retain every existing user terminal profile."""
    directory = home / ".config/xfce4/terminal"
    directory.mkdir(parents=True, mode=0o700, exist_ok=True)
    target = directory / "terminalrc"
    try:
        fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    except FileExistsError:
        return
    with os.fdopen(fd, "w") as stream:
        stream.write("""[Configuration]
MiscMenubarDefault=FALSE
MiscToolbarDefault=FALSE
MiscBordersDefault=TRUE
BackgroundMode=TERMINAL_BACKGROUND_TRANSPARENT
BackgroundDarkness=0.88
ColorUseTheme=FALSE
ColorBackground=#1f2c24
ColorForeground=#e6eee7
FontUseSystem=FALSE
FontName=Monospace 12
""")


def main():
    if os.geteuid() == 0 or not Path("/.dockerenv").exists():
        raise RuntimeError("Run as the dedicated Meadow user inside its Docker container")
    if not os.environ.get("DBUS_SESSION_BUS_ADDRESS"):
        raise RuntimeError("A private D-Bus session is required")
    home = Path.home()
    seed_terminal_defaults(home)
    state = home / ".local/state/meadow-os"
    data = home / ".local/share/meadow-os/agent"
    for path in (home / ".local/state", home / ".local/share", state, data, data / "run"):
        private_directory(path)
    lock_path = state / "container.lock"
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    lock = os.fdopen(fd, "r+")
    if os.fstat(fd).st_uid != os.geteuid() or os.fstat(fd).st_mode & 0o077:
        raise RuntimeError("Unsafe container lock")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as error:
        raise RuntimeError("Another container already owns this Meadow home") from error
    remove_stale_socket(data / "run/agent.sock")
    spool = Path(os.environ["MEADOW_VOICE_SPOOL"])
    private_directory(spool)
    display = os.environ.get("MEADOW_DISPLAY", ":121")
    if not display.startswith(":") or not display[1:].isdigit():
        raise RuntimeError("MEADOW_DISPLAY must be a local numbered X display")
    environment_path = state / "desktop-environment.json"
    environment_fd = os.open(environment_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(environment_fd, "w") as stream:
        json.dump({"DISPLAY": display, "DBUS_SESSION_BUS_ADDRESS": os.environ["DBUS_SESSION_BUS_ADDRESS"],
                   "XDG_RUNTIME_DIR": str(home / ".runtime-meadow-lab"), "GDK_BACKEND": "x11",
                   "XDG_SESSION_TYPE": "x11", "XDG_CURRENT_DESKTOP": "Meadow:XFCE",
                   "MEADOW_OS_SESSION": "1"}, stream)
        stream.write("\n")
    processes, logs = [], []
    stopping = False

    def stop(_signal, _frame):
        nonlocal stopping
        stopping = True

    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, stop)

    def start(name, command):
        log_path = state / (name + ".log")
        log_fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        log = os.fdopen(log_fd, "wb")
        logs.append(log)
        child = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        processes.append((name, child))

    try:
        # This Linux image deliberately contains no speech model. IME and
        # original attachment ingress share the native spool without API keys.
        start("companion", ["/usr/bin/python3", "/opt/meadow/share/meadow-os/voiceos/voice_service.py",
             "--spool", str(spool), "--model", "/opt/meadow/no-speech-model",
             "--origin", "http://127.0.0.1:60511", "--port", "60531"])
        start("companion-proxy", ["/usr/bin/python3", "/usr/local/lib/meadowos/companion_proxy.py"])
        start("desktop-lab", ["/usr/bin/python3", "/usr/local/share/meadowos-source/scripts/meadowos/linux_lab.py",
             "--display", display])
        print(json.dumps({"desktop": "http://127.0.0.1:60511/vnc.html?autoconnect=1&resize=scale",
                          "home_persistent": True, "model_configuration": "required for chat",
                          "linux_speech": "unavailable"}), flush=True)
        while not stopping:
            for name, child in processes:
                if child.poll() is not None:
                    raise RuntimeError(f"{name} exited {child.returncode}; inspect {state / (name + '.log')}")
            time.sleep(.25)
        return 0
    finally:
        for _name, child in reversed(processes):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=35)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=5)
        for log in logs:
            log.close()
        lock.close()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, KeyError) as error:
        print(f"MeadowOS container could not start: {error}", file=sys.stderr)
        raise SystemExit(1)
