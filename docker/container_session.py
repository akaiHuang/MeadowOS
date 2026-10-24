#!/usr/bin/env python3
"""Own a fresh container desktop; model planning remains in native Agent."""
import errno
import fcntl
import json
import select
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




def remove_stale_display(display, temporary_root=Path("/tmp"), proc_root=Path("/proc")):
    """Recover only this owned display's dead socket/lock inside the container."""
    if not display.startswith(":") or not display[1:].isascii() or not display[1:].isdigit():
        raise RuntimeError("A local numbered display is required for cleanup")
    socket_path = temporary_root / ".X11-unix" / ("X" + display[1:])
    lock_path = temporary_root / (".X" + display[1:] + "-lock")
    if temporary_root.is_symlink() or socket_path.parent.is_symlink():
        raise RuntimeError("Display cleanup refuses symlinked resource directories")
    candidates = []
    for path, kind in ((socket_path, stat.S_ISSOCK), (lock_path, stat.S_ISREG)):
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        if not kind(info.st_mode) or info.st_uid != os.geteuid():
            raise RuntimeError("Display cleanup refuses foreign or unsafe resources")
        if path == lock_path and info.st_size > 32:
            raise RuntimeError("Existing X display lock is too large")
        candidates.append((path, info.st_ino))
    if not candidates:
        return
    # X11 can listen on both filesystem and abstract Unix sockets.
    addresses = [str(socket_path)]
    if sys.platform.startswith("linux"):
        addresses.append("\0" + str(socket_path))
    for address in addresses:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as probe:
            probe.settimeout(1)
            try:
                probe.connect(address)
            except OSError as error:
                if error.errno not in (errno.ECONNREFUSED, errno.ENOENT):
                    raise RuntimeError("Existing X11 display could not be checked") from error
            else:
                raise RuntimeError("Another X server owns this display; refusing cleanup")
    if lock_path.exists():
        raw = lock_path.read_text().strip()
        if not raw.isascii() or not raw.isdigit():
            raise RuntimeError("Existing X display lock is invalid")
        command = proc_root / raw / "cmdline"
        try:
            args = command.read_bytes().split(b"\0")
        except FileNotFoundError:
            args = []
        if args and any(b"Xvfb" in arg or b"Xorg" in arg for arg in args[:1]) and display.encode() in args:
            raise RuntimeError("A live X server owns this display lock")
    for path, inode in candidates:
        current = path.lstat()
        if current.st_ino != inode or current.st_uid != os.geteuid():
            raise RuntimeError("Display resource changed during cleanup")
        path.unlink()


def stop_owned_processes(processes):
    # The bus is started first, so it remains available until desktop cleanup ends.
    for _name, child in reversed(processes):
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=35)
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait(timeout=5)


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
    remove_stale_display(display)
    processes, logs = [], []
    stopping = False

    def stop(_signal, _frame):
        nonlocal stopping
        stopping = True

    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, stop)

    def open_log(name):
        log_path = state / (name + ".log")
        log_fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        log = os.fdopen(log_fd, "wb")
        logs.append(log)
        return log

    def start(name, command):
        log = open_log(name)
        child = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        processes.append((name, child))
        return child

    try:
        # Own the bus directly: a dbus-run-session parent can exit on SIGTERM
        # before the child supervisor finishes Xvfb/socket cleanup on restart.
        read_fd, write_fd = os.pipe()
        with os.fdopen(read_fd, "rb") as bus_address:
            try:
                bus = subprocess.Popen(["dbus-daemon", "--session", "--nofork", f"--print-address={write_fd}"],
                                       pass_fds=(write_fd,), stdout=open_log("session-bus"),
                                       stderr=subprocess.STDOUT, start_new_session=True)
                processes.append(("session-bus", bus))
            finally:
                os.close(write_fd)
            if not select.select([bus_address], [], [], 10)[0]:
                raise RuntimeError("Private session bus did not publish its address")
            address = bus_address.readline(16384).decode().strip()
            if not address.startswith("unix:") or "\x00" in address or bus.poll() is not None:
                raise RuntimeError("Private session bus failed to start")
        os.environ["DBUS_SESSION_BUS_ADDRESS"] = address
        environment_path = state / "desktop-environment.json"
        environment_fd = os.open(environment_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(environment_fd, "w") as stream:
            json.dump({"DISPLAY": display, "DBUS_SESSION_BUS_ADDRESS": os.environ["DBUS_SESSION_BUS_ADDRESS"],
                       "XDG_RUNTIME_DIR": str(home / ".runtime-meadow-lab"), "GDK_BACKEND": "x11",
                       "XDG_SESSION_TYPE": "x11", "XDG_CURRENT_DESKTOP": "Meadow:XFCE",
                       "MEADOW_OS_SESSION": "1"}, stream)
            stream.write("\n")
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
        stop_owned_processes(processes)
        for log in logs:
            log.close()
        lock.close()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, KeyError) as error:
        print(f"MeadowOS container could not start: {error}", file=sys.stderr)
        raise SystemExit(1)
