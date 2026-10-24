#!/usr/bin/python3
"""MeadowOS development session wiring, with no model or task semantics."""
import argparse
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

UNIT = "meadow-agent.service"


def private_dir(path):
    path = Path(path)
    if not path.is_absolute():
        raise ValueError("user paths must be absolute")
    for component in [*reversed(path.parents), path]:
        if component.is_symlink():
            raise ValueError(f"refusing symlink in private path: {component}")
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if not path.is_dir() or path.stat().st_uid != os.geteuid():
        raise ValueError(f"private directory is not owned by current user: {path}")
    path.chmod(0o700)
    return path


def paths(environ=None):
    env = os.environ if environ is None else environ
    home = Path(env.get("HOME", ""))
    if not home.is_absolute():
        raise ValueError("HOME must be absolute")
    data = Path(env.get("XDG_DATA_HOME") or home / ".local/share") / "meadow-os"
    state = Path(env.get("XDG_STATE_HOME") or home / ".local/state") / "meadow-os"
    if not data.is_absolute() or not state.is_absolute():
        raise ValueError("XDG data/state directories must be absolute")
    return data, state, home / "MeadowWork"


def create_json_once(path, value):
    # O_EXCL and O_NOFOLLOW preserve existing identities and reject planted links.
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags, 0o600)
    except FileExistsError:
        if path.is_symlink() or not path.is_file() or path.stat().st_uid != os.geteuid():
            raise ValueError(f"unsafe existing state: {path}")
        if path.stat().st_mode & 0o077:
            raise ValueError(f"private state must have mode 0600: {path}")
        return json.loads(path.read_text())
    with os.fdopen(fd, "w") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    return value


def initialize_state():
    data, state, workspace = paths()
    for directory in [data, state, data / "agent", data / "agent/run", workspace]:
        private_dir(directory)
    identity = create_json_once(data / "device.json", {
        "schema_version": 1,
        "device_id": str(uuid.uuid4()),
        "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "development": True,
    })
    if not isinstance(identity, dict) or not isinstance(identity.get("device_id"), str):
        raise ValueError("invalid existing device identity")
    uuid.UUID(identity["device_id"])
    create_json_once(state / "status.json", {
        "schema_version": 1,
        "development": True,
        "device_id": identity["device_id"],
        "private_url": None,
        "runtime_acceptance": "not_verified",
        "optional_services": {
            "postgresql": "not_enabled", "searxng": "not_enabled",
            "opengcua": "not_verified", "browser": "not_verified",
        },
    })
    return data, state, workspace


def runtime_environment():
    data, _, _ = paths()
    profile = data / "agent"
    env = dict(os.environ)
    for name in ["QWEN_HOME", "MEADOW_AGENT_API_KEY", "MEADOW_CORE_BASE_URL",
                 "MEADOW_MODEL_ID", "MEADOW_AGENT_WORKSPACE", "MEADOW_USE_ROUTER",
                 "MEADOW_HARNESS_MODE", "MEADOW_AGENT_HARNESS", "MEADOW_APPROVAL_MODE", "MEADOW_AGENTD_MANAGED"]:
        env.pop(name, None)
    env.update({
        "MEADOW_HOME": str(profile),
        "MEADOW_DESKTOP_STATE_HOME": str(profile),
        "MEADOW_AGENT_SOCKET": str(profile / "run/agent.sock"),
        "MEADOW_AGENT_TOKEN_FILE": str(profile / "run/agent-api-token"),
        "MEADOW_MODEL_CONNECTIONS_FILE": str(profile / "model-connections-v1.json"),
        "MEADOW_ENABLE_PROMPT_GRANTS": "1",
        "MEADOW_CONVERSATION_PROFILE": "meadow-os",
        "MEADOW_APPROVAL_MODE": "full-access",
        "MEADOW_WORKSPACE": str(paths()[2]),
    })
    return env


def executable(path):
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ValueError(f"missing executable: {path}")


def require_x11():
    if os.environ.get("XDG_SESSION_TYPE", "x11") != "x11" or not os.environ.get("DISPLAY"):
        raise ValueError("an actual X11 graphical session with DISPLAY is required")


def session_init(prefix, no_start, *, apply_theme=True):
    # Identity initialization is useful in staging; it never marks the Agent ready.
    initialize_state()
    if no_start:
        return
    executable(prefix / "bin/meadow-desktop")
    executable(prefix / "libexec/meadow-agentd")
    require_x11()
    if not shutil.which("systemctl"):
        raise ValueError("systemctl is unavailable; no background Agent was started")
    result = subprocess.run(["systemctl", "--user", "cat", UNIT], capture_output=True)
    if result.returncode:
        raise ValueError("Meadow Agent user service is missing or no user service manager is available")
    names = [name for name in ["DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS",
                              "XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP", "XDG_DATA_HOME",
                              "XDG_STATE_HOME", "GTK_MODULES", "XCURSOR_THEME", "XCURSOR_PATH", "THUNARX_DIRS",
                              "MEADOW_GLASS_CAPTURE", "MEADOW_GLASS_BRIDGE_PATH",
                              "MEADOW_QUICKLOOK_HELPER", "MEADOW_OS_SESSION"] if os.environ.get(name)]
    subprocess.run(["systemctl", "--user", "import-environment", *names], check=True)
    subprocess.run(["systemctl", "--user", "enable", "--now", UNIT], check=True)
    # Active is only process state, not model, SDK or product readiness.
    subprocess.run(["systemctl", "--user", "is-active", "--quiet", UNIT], check=True)
    if apply_theme:
        theme = prefix / "bin/meadow-theme"
        executable(theme)
        subprocess.run([str(theme), "--apply-defaults"], check=True)


def first_login(prefix):
    failed = False
    try:
        session_init(prefix, False)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"MeadowOS session setup incomplete: {error}", file=sys.stderr)
        failed = True
    # The actual welcome probes service state and missing configuration itself;
    # show it even when setup failed, rather than leave a blank desktop.
    welcome = prefix / "bin/meadow-welcome"
    executable(welcome)
    subprocess.Popen([str(welcome)], env=runtime_environment(), start_new_session=True)
    return 1 if failed else 0


def run_component(prefix, component):
    if component == "shell":
        script = prefix / "share/meadow-os/shell/meadow_shell.py"
        if script.is_symlink() or not script.is_file():
            raise ValueError(f"missing Meadow Shell entry: {script}")
        require_x11()
        _, _, workspace = initialize_state()
        os.chdir(workspace)
        os.execve("/usr/bin/python3", ["/usr/bin/python3", str(script)], runtime_environment())
        return
    executable(prefix / ("libexec/meadow-agentd" if component == "agent" else "bin/meadow-desktop"))
    _, _, workspace = initialize_state()
    env = runtime_environment()
    if component == "agent":
        command = [str(prefix / "libexec/meadow-agentd"), "--workspace", str(workspace),
                   "--connection-file", env["MEADOW_MODEL_CONNECTIONS_FILE"],
                   "--approval-mode", env["MEADOW_APPROVAL_MODE"]]
    else:
        require_x11()
        env["MEADOW_AGENTD_MANAGED"] = "1"
        command = [str(prefix / "bin/meadow-desktop")]
    os.chdir(workspace)
    os.execve(command[0], command, env)


def doctor(prefix):
    checks = []
    for command in ["bash", "git", "python3", "node", "npm", "go", "xfwm4", "xfsettingsd", "xrandr", "Xorg"]:
        checks.append({"name": command, "available": bool(shutil.which(command)), "required": True})
    for name, relative in [("desktop", "bin/meadow-desktop"), ("agent", "libexec/meadow-agentd")]:
        path = prefix / relative
        checks.append({"name": name, "available": path.is_file() and os.access(path, os.X_OK), "required": True})
    shell = prefix / "share/meadow-os/shell/meadow_shell.py"
    checks.append({"name": "shell_entry", "available": shell.is_file() and not shell.is_symlink(), "required": True})
    active = False
    if shutil.which("systemctl"):
        active = subprocess.run(["systemctl", "--user", "is-active", "--quiet", UNIT],
                                capture_output=True).returncode == 0
    checks.append({"name": "agent_user_service_active", "available": active, "required": True})
    checks.append({"name": "x11_session", "available": bool(os.environ.get("DISPLAY")) and
                   os.environ.get("XDG_SESSION_TYPE", "x11") == "x11", "required": True})
    # These are deliberately pending. No socket, HTTP 200 or active PID proves them.
    print(json.dumps({"schema_version": 1, "checks": checks, "product_ready": False,
                      "pending_acceptance": ["meadow_xsession", "1920x1080_display", "model_connection", "opengcua_control", "browser_control",
                                             "private_url_pairing", "vm_boot_reboot", "update_rollback"],
                      "optional_services": {"postgresql": "not_enabled", "searxng": "not_enabled"}}, indent=2))
    return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prefix", type=Path, default=Path(__file__).resolve().parents[3])
    commands = parser.add_subparsers(dest="command", required=True)
    init = commands.add_parser("init")
    init.add_argument("--no-start", action="store_true", help="initialize private state only; never report ready")
    for name in ["agent", "desktop", "shell", "os-session", "doctor", "first-login"]:
        commands.add_parser(name)
    args = parser.parse_args()
    if not args.prefix.is_absolute():
        parser.error("prefix must be absolute")
    try:
        if args.command == "init":
            session_init(args.prefix, args.no_start)
        elif args.command == "doctor":
            return doctor(args.prefix)
        elif args.command == "first-login":
            return first_login(args.prefix)
        elif args.command == "os-session":
            # A desktop session supervisor only; all task semantics stay in Agent.
            from os_session import run_session
            return run_session(args.prefix)
        else:
            run_component(args.prefix, args.command)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"MeadowOS setup incomplete: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
