#!/usr/bin/env python3
"""Launch an explicit command on this container's existing native session bus."""
import json
import os
from pathlib import Path
import stat
import sys


def session_environment():
    path = Path.home() / ".local/state/meadow-os/desktop-environment.json"
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_size > 16384:
            raise RuntimeError("Native session environment is unavailable")
        session = json.load(stream)
    allowed = {"DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "GDK_BACKEND",
               "XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP", "MEADOW_OS_SESSION"}
    if set(session) != allowed or any(not isinstance(value, str) or "\x00" in value for value in session.values()):
        raise RuntimeError("Native session environment is invalid")
    sys.path.insert(0, "/opt/meadow/share/meadow-os/bin")
    import runtime
    import os_session
    env = runtime.runtime_environment()
    env.update(session)
    os_session.configure_window_resources(Path("/opt/meadow"), env)
    return env


if __name__ == "__main__":
    if len(sys.argv) < 2:
        raise SystemExit("usage: meadowos-run COMMAND [ARG ...]")
    try:
        os.execvpe(sys.argv[1], sys.argv[1:], session_environment())
    except (OSError, ValueError, RuntimeError) as error:
        print(f"Could not join native Meadow session: {error}", file=sys.stderr)
        raise SystemExit(1)
