#!/usr/bin/python3
"""The Meadow X11 session lifecycle. No model, scheduler, or task planning."""
import ctypes
import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import sysconfig
import time
import uuid

import runtime

DESKTOP_SIZE = (1920, 1080)
SHELL_RESTART_LIMIT = 3
COMPOSITOR_START_TIMEOUT = 5
SHELL_PAINT_TIMEOUT = 12


def configure_window_resources(prefix, env):
    """Configure packaged GTK adapters; availability is not visual acceptance."""
    share = prefix / "share/meadow-os"
    # Xsettings is the single live appearance authority. A pinned GTK_THEME
    # makes a dark foreground update disagree with the actual native provider.
    if env.get("GTK_THEME", "").split(":", 1)[0] in ("Meadow-Light", "Meadow-Dark"):
        env.pop("GTK_THEME", None)
    module = share / "window_manager/libmeadow-hover-chrome.so"
    capture = share / "desktop/glass/meadow-glass-capture"
    bridge = share / "desktop/glass/bridge.py"
    if env.get("MEADOW_WINDOW_EFFECTS", "1") == "0":
        return {"configured": False, "reason": "disabled", "visual_verified": False}
    if any(path.is_symlink() or not path.is_file() for path in (module, capture, bridge)):
        return {"configured": False, "reason": "packaged native resources unavailable", "visual_verified": False}
    env.setdefault("XCURSOR_THEME", "Meadow-Cursors")
    # libXcursor does not search staged XDG_DATA_DIRS. Keep an installed or
    # relocated image's own cursors discoverable without changing user paths.
    icons = prefix.parent.parent / "usr/share/icons"
    if (icons / "Meadow-Cursors/index.theme").is_file():
        env.setdefault("XCURSOR_PATH", ":".join((str(Path(env.get("HOME", str(Path.home()))) / ".icons"),
                                               str(icons), "/usr/local/share/icons", "/usr/share/icons", "/usr/share/pixmaps")))
    modules = [value for value in env.get("GTK_MODULES", "").split(":") if value]
    if str(module) not in modules:
        modules.append(str(module))
    env.update({"GTK_MODULES": ":".join(modules), "MEADOW_GLASS_CAPTURE": str(capture),
                "MEADOW_GLASS_BRIDGE_PATH": str(bridge)})
    multiarch = sysconfig.get_config_var("MULTIARCH")
    # Use the very same installed file for GTK preload and Thunarx discovery.
    # Loading duplicate copies would register the provider GType twice.
    folder_module = prefix.parent.parent / "usr/lib" / (multiarch or "") / "thunarx-3/libmeadow-folder-materials.so"
    folder_status = {"configured": False, "visual_verified": False}
    if env.get("MEADOW_FOLDER_EFFECTS", "1") == "0":
        folder_status["reason"] = "disabled"
    elif folder_module.is_symlink() or not folder_module.is_file():
        folder_status["reason"] = "optional folder module unavailable"
    else:
        if str(folder_module) not in modules:
            modules.append(str(folder_module))
        # Setting THUNARX_DIRS replaces Thunar's defaults. Retain the stock
        # extension locations as well as any explicitly configured locations.
        extension_dirs = [value for value in env.get("THUNARX_DIRS", "").split(":") if value]
        defaults = ([f"/usr/lib/{multiarch}/thunarx-3"] if multiarch else []) + ["/usr/lib/thunarx-3"]
        for directory in (*defaults, str(folder_module.parent)):
            if directory not in extension_dirs:
                extension_dirs.append(directory)
        env.update({"GTK_MODULES": ":".join(modules), "THUNARX_DIRS": ":".join(extension_dirs)})
        folder_status.update({"configured": True, "scope": "Thunar GTK3 icon grid",
                              "material": "native Cairo internal-preview frost"})
        preview = share / "desktop/quicklook.py"
        decoder = share / "desktop/file_preview.py"
        if all(path.is_file() and not path.is_symlink() for path in (preview, decoder)):
            env.setdefault("MEADOW_QUICKLOOK_HELPER", str(preview))
            folder_status["quicklook"] = {"configured": env.get("MEADOW_QUICKLOOK", "1") != "0",
                                          "content_thumbnails": env.get("MEADOW_FILE_THUMBNAILS", "1") != "0",
                                          "visual_verified": False}
    return {"configured": True, "scope": "Meadow-owned windows and supported GTK3 HeaderBars",
            "material": "regional native XComposite CPU prototype", "visual_verified": False,
            "folder_materials": folder_status}


def parse_display(text):
    match = re.search(r"\bcurrent\s+(\d+)\s+x\s+(\d+)", text)
    outputs = []
    current = None
    lines = text.splitlines()
    for index, line in enumerate(lines):
        output = re.match(r"^(\S+) connected(?: (primary))?(?:\s|$)", line)
        if output:
            geometry = re.search(r"\b(\d+)x(\d+)([+-]\d+)([+-]\d+)", line)
            current = {"name": output[1], "primary": bool(output[2]), "modes": [],
                       "geometry": [int(geometry[1]), int(geometry[2])] if geometry else None,
                       "position": [int(geometry[3]), int(geometry[4])] if geometry else None,
                       "transform": None}
            outputs.append(current)
        elif line and not line[0].isspace():
            current = None
        elif current and (mode := re.match(r"^\s+(\d+x\d+)\s", line)):
            current["modes"].append(mode[1])
        elif current and line.strip().startswith("Transform:"):
            matrix = " ".join([line.split(":", 1)[1], *lines[index + 1:index + 3]])
            values = re.findall(r"[-+]?\d+(?:\.\d+)?", matrix)
            if len(values) == 9:
                current["transform"] = [float(value) for value in values]
    return {"size": [int(match[1]), int(match[2])] if match else None, "outputs": outputs}


def configure_display(xrandr):
    """Select an advertised mode only; never invent an unverified GPU modeline."""
    result = {"requested": list(DESKTOP_SIZE), "actual": None, "applied": False, "error": None}
    try:
        query = subprocess.run([xrandr, "--query", "--verbose"], capture_output=True, text=True, check=True, timeout=10)
        display = parse_display(query.stdout)
        result["actual"] = display["size"]
        if len(display["outputs"]) != 1:
            raise ValueError("MVP requires one connected guest display; resolution was left unchanged")
        output = display["outputs"][0]
        result["output"] = output
        identity = [1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0]

        def exact_geometry():
            return (display["size"] == list(DESKTOP_SIZE) and output["geometry"] == list(DESKTOP_SIZE)
                    and output["position"] == [0, 0] and output["transform"] == identity)

        if "1920x1080" not in output["modes"]:
            raise ValueError("guest display does not advertise 1920x1080; actual geometry is retained")
        if not exact_geometry():
            subprocess.run([xrandr, "--output", output["name"], "--mode", "1920x1080", "--pos", "0x0",
                            "--scale", "1x1", "--fb", "1920x1080"],
                           capture_output=True, text=True, check=True, timeout=10)
            query = subprocess.run([xrandr, "--query", "--verbose"], capture_output=True, text=True, check=True, timeout=10)
            display = parse_display(query.stdout)
            result["actual"] = display["size"]
            if len(display["outputs"]) != 1:
                raise ValueError("guest output topology changed during mode selection")
            output = display["outputs"][0]
            result["output"] = output
        if not exact_geometry():
            raise ValueError("guest output mode, origin and identity transform did not verify as 1920x1080")
        result["applied"] = True
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        result["error"] = str(error)
    return result


def agent_service_state():
    if not runtime.shutil.which("systemctl"):
        return None
    result = subprocess.run(["systemctl", "--user", "show", runtime.UNIT, "--property=ActiveState", "--value"],
                            capture_output=True, text=True, timeout=5)
    return result.stdout.strip() if result.returncode == 0 else None


def wm_owner(xprop):
    result = subprocess.run([xprop, "-root", "_NET_SUPPORTING_WM_CHECK"], capture_output=True, text=True, timeout=5)
    match = re.search(r"window id # (0x[0-9a-fA-F]+)", result.stdout)
    if not match or int(match[1], 16) == 0:
        return None
    # Root properties can outlive a crashed WM. A live supporting window matters.
    support = subprocess.run([xprop, "-id", match[1], "_NET_SUPPORTING_WM_CHECK"],
                             capture_output=True, text=True, timeout=5)
    return match[1] if support.returncode == 0 and match[1] in support.stdout else None


def compositor_owner():
    """Read the real X11 compositor selection, not a stale root property."""
    lib = ctypes.CDLL("libX11.so.6")
    lib.XOpenDisplay.argtypes = [ctypes.c_char_p]
    lib.XOpenDisplay.restype = ctypes.c_void_p
    lib.XDefaultScreen.argtypes = [ctypes.c_void_p]
    lib.XDefaultScreen.restype = ctypes.c_int
    lib.XInternAtom.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int]
    lib.XInternAtom.restype = ctypes.c_ulong
    lib.XGetSelectionOwner.argtypes = [ctypes.c_void_p, ctypes.c_ulong]
    lib.XGetSelectionOwner.restype = ctypes.c_ulong
    lib.XCloseDisplay.argtypes = [ctypes.c_void_p]
    display = lib.XOpenDisplay(None)
    if not display:
        raise ValueError("Cannot inspect the current X11 compositor")
    try:
        name = f"_NET_WM_CM_S{lib.XDefaultScreen(display)}".encode()
        atom = lib.XInternAtom(display, name, 0)
        return lib.XGetSelectionOwner(display, atom) or None
    finally:
        lib.XCloseDisplay(display)


def start_blur_compositor(start, executable, config, status):
    """Own one compositor. Failure retains WM/Agent and lets GTK use opaque surfaces."""
    process = None
    try:
        if compositor_owner():
            raise ValueError("another compositor already owns this display; it was not replaced")
        process = start([executable, "--config", str(config)])
        deadline = time.monotonic() + COMPOSITOR_START_TIMEOUT
        while True:
            if process.poll() is not None:
                raise ValueError("picom exited before acquiring the display")
            owner = compositor_owner()
            if owner:
                status.pop("compositor_error", None)
                status.update({"compositor": "picom", "compositor_backend": "glx",
                               "compositor_pid": process.pid, "compositor_owner": hex(owner),
                               "native_backdrop_blur": True})
                return process
            if time.monotonic() >= deadline:
                raise ValueError("picom did not acquire the compositor selection")
            time.sleep(0.1)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        stop_owned(process)
        status.update({"compositor": "none", "native_backdrop_blur": False,
                       "compositor_error": str(error)})
        return None


def wait_for_shell_paint(process, path, token, display, stopping=lambda: False):
    """Only a current, painted Shell can establish the initial GLX backdrop."""
    deadline = time.monotonic() + SHELL_PAINT_TIMEOUT
    while not stopping() and process.poll() is None:
        try:
            if not path.is_symlink() and path.is_file() and path.stat().st_size <= 4096:
                ready = json.loads(path.read_text())
                if (ready.get("token") == token and ready.get("pid") == process.pid
                        and ready.get("display") == display and ready.get("painted") is True):
                    return True
        except (OSError, ValueError, AttributeError):
            pass
        if time.monotonic() >= deadline:
            break
        time.sleep(0.05)
    return False


def observe_compositor(process, status):
    """A crashed compositor must not restart the WM, Shell or Agent."""
    if process is not None and process.poll() is not None:
        status.update({"compositor": "none", "native_backdrop_blur": False,
                       "compositor_error": f"picom exited ({process.returncode}); opaque surfaces remain available"})
        status.pop("compositor_pid", None)
        status.pop("compositor_owner", None)
        return None, True
    return process, False


def write_status(path, value):
    value["updated_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    temporary = path.with_name(f".{path.name}.{uuid.uuid4().hex}")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def stop_owned(process):
    if process is None or process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=3)
    except ProcessLookupError:
        pass
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=3)


def setup_user_places(updater, env):
    """Create standard real directories, then seed bookmarks only on first use."""
    subprocess.run([updater], env=env, capture_output=True, timeout=10, check=True)
    lookup = Path(updater).with_name("xdg-user-dir")
    if not lookup.is_file():
        return
    config = Path(env.get("XDG_CONFIG_HOME", str(Path(env["HOME"]) / ".config")))
    bookmarks = config / "gtk-3.0/bookmarks"
    if bookmarks.exists() or bookmarks.is_symlink():
        return
    places = []
    workspace = Path(env["HOME"]) / "MeadowWork"
    if workspace.is_dir():
        places.append((workspace, "工作檔案"))
    for key, label in (("DOCUMENTS", "文件"), ("DOWNLOAD", "下載"), ("PICTURES", "圖片"),
                       ("VIDEOS", "影片"), ("MUSIC", "音樂")):
        result = subprocess.run([str(lookup), key], env=env, capture_output=True, text=True, timeout=3)
        folder = Path(result.stdout.strip())
        if result.returncode == 0 and folder.is_absolute() and folder != Path(env["HOME"]) and folder.is_dir():
            places.append((folder, label))
    if not places:
        return
    bookmarks.parent.mkdir(parents=True, exist_ok=True)
    try:
        fd = os.open(bookmarks, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    except FileExistsError:
        return
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        stream.writelines(f"{folder.as_uri()} {label}\n" for folder, label in places)


def run_session(prefix):
    if sys.platform != "linux" or os.geteuid() == 0:
        raise ValueError("Meadow Xsession requires a non-root Linux desktop user")
    runtime.require_x11()
    shell = prefix / "share/meadow-os/shell/meadow_shell.py"
    if shell.is_symlink() or not shell.is_file():
        raise ValueError(f"missing Meadow Shell entry: {shell}")
    commands = {}
    for command in ["xfwm4", "xfsettingsd", "xprop", "xrandr", "xfce4-terminal"]:
        path = runtime.shutil.which(command)
        if not path:
            raise ValueError(f"missing session dependency: {command}")
        commands[command] = path
    if not os.environ.get("DBUS_SESSION_BUS_ADDRESS"):
        bus = runtime.shutil.which("dbus-run-session")
        if not bus:
            raise ValueError("session D-Bus is missing and dbus-run-session is unavailable")
        script = prefix / "share/meadow-os/bin/runtime.py"
        os.execve(bus, [bus, "--", "/usr/bin/python3", str(script), "--prefix", str(prefix), "os-session"], dict(os.environ))
        return 1
    if wm_owner(commands["xprop"]):
        raise ValueError("an existing window manager owns this display; Meadow will not replace it")
    if compositor_owner():
        raise ValueError("an existing compositor owns this display; Meadow will not replace it")
    if path := runtime.shutil.which("picom"):
        commands["picom"] = path
    if path := runtime.shutil.which("xdg-user-dirs-update"):
        commands["xdg-user-dirs-update"] = path
    _, state, _ = runtime.initialize_state()
    lock_path = state / "session.lock"
    fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(fd, "r+") as lock:
        if lock_path.stat().st_uid != os.geteuid() or lock_path.stat().st_mode & 0o077:
            raise ValueError("unsafe Meadow session lock")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError("another Meadow session already owns this user profile")
        return supervise(prefix, state, commands)


def supervise(prefix, state, commands):
    env = runtime.runtime_environment()
    env.update({"XDG_CURRENT_DESKTOP": "Meadow:XFCE", "XDG_SESSION_DESKTOP": "meadow",
                "DESKTOP_SESSION": "meadow", "XDG_SESSION_TYPE": "x11",
                "GDK_SCALE": "1", "GDK_DPI_SCALE": "1", "MEADOW_OS_SESSION": "1"})
    window_materials = configure_window_resources(prefix, env)
    # The session bus may predate the graphical environment (display manager).
    # Give activated native applications the same presentation environment.
    activation_names = [name for name in ("HOME", "DISPLAY", "XAUTHORITY", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
        "XDG_STATE_HOME", "XDG_CURRENT_DESKTOP", "GTK_MODULES", "XCURSOR_THEME", "XCURSOR_PATH", "THUNARX_DIRS",
        "MEADOW_GLASS_CAPTURE", "MEADOW_GLASS_BRIDGE_PATH", "MEADOW_QUICKLOOK_HELPER") if env.get(name)]
    if activation := runtime.shutil.which("dbus-update-activation-environment"):
        subprocess.run([activation, *activation_names], env=env, check=True, timeout=5)

    # Register real standard places on a fresh account. The freedesktop utility
    # preserves existing user-dir preferences and never populates sample files.
    directory_setup = commands.get("xdg-user-dirs-update")
    if directory_setup:
        setup_user_places(directory_setup, env)
    status_path = state / "session.json"
    compositor_config = prefix / "share/meadow-os/desktop/picom.conf"
    experimental_glass = env.get("MEADOW_EXPERIMENTAL_GLASS") == "1"
    use_picom = (experimental_glass and "picom" in commands and compositor_config.is_file()
                 and not compositor_config.is_symlink())
    status = {"schema_version": 1, "session_id": str(uuid.uuid4()), "state": "starting",
              "window_manager": "xfwm4", "compositor": "none" if use_picom else "xfwm4",
              "compositor_requested": "picom" if experimental_glass else "xfwm4", "native_backdrop_blur": False,
              "experimental_glass": experimental_glass,
              "window_materials": window_materials,
              "blur_scope": "meadow-shell transparent surfaces",
              "display": configure_display(commands["xrandr"]), "shell_restarts": 0,
              "agent_service_setup_error": None, "product_ready": False}
    if experimental_glass and not use_picom:
        status["compositor_error"] = "experimental picom or its regular packaged config is unavailable; xfwm4 alpha fallback"
    write_status(status_path, status)
    processes = []
    stopping = False

    def request_stop(_signal, _frame):
        nonlocal stopping
        stopping = True

    handlers = {sig: signal.signal(sig, request_stop) for sig in [signal.SIGTERM, signal.SIGINT]}
    log_fd = os.open(state / "session.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0), 0o600)
    agent_started = False
    with os.fdopen(log_fd, "a") as log:
        def start(argv):
            process = subprocess.Popen(argv, env=env, stdout=log, stderr=log, start_new_session=True)
            processes.append(process)
            return process

        try:
            wm = start([commands["xfwm4"], "--compositor=off" if use_picom else "--compositor=on"])
            deadline = time.monotonic() + 10
            while not wm_owner(commands["xprop"]):
                if stopping or wm.poll() is not None or time.monotonic() >= deadline:
                    raise ValueError("xfwm4 did not acquire the display; no replacement WM was started")
                time.sleep(0.1)
            compositor = None
            start([commands["xfsettingsd"], "--no-daemon"])
            try:
                # Import the actual session env without leaking the caller's profile.
                previous_agent_state = agent_service_state()
                status["agent_service_previous_state"] = previous_agent_state
                with_environment = dict(os.environ)
                os.environ.update(env)
                try:
                    runtime.session_init(prefix, False, apply_theme=False)
                    # An attached pre-existing unit is not owned by this session.
                    # Unknown/transitioning states also never grant stop authority.
                    agent_started = previous_agent_state in ("inactive", "failed")
                finally:
                    os.environ.clear()
                    os.environ.update(with_environment)
            except (OSError, ValueError, subprocess.SubprocessError) as error:
                status["agent_service_setup_error"] = str(error)
                print(f"Meadow Agent service is unavailable: {error}", file=log, flush=True)
            argv = ["/usr/bin/python3", str(prefix / "share/meadow-os/bin/runtime.py"), "--prefix", str(prefix), "shell"]
            def start_shell():
                nonlocal compositor
                env["MEADOW_SHELL_PAINT_TOKEN"] = uuid.uuid4().hex
                child = start(argv)
                if use_picom:
                    painted = wait_for_shell_paint(child, state / "shell-painted.json",
                                                  env["MEADOW_SHELL_PAINT_TOKEN"], env.get("DISPLAY"),
                                                  lambda: stopping or wm.poll() is not None)
                    status["shell_painted_before_compositor"] = painted
                    if painted and not stopping and wm.poll() is None:
                        compositor = start_blur_compositor(start, commands["picom"], compositor_config, status)
                    else:
                        status["compositor_error"] = "current Shell did not finish its first paint; opaque rendering retained"
                return child

            frame_script = prefix / "share/meadow-os/window_manager/meadow_frame_service.py"
            frame_enabled = window_materials.get("configured", False) and frame_script.is_file() and not frame_script.is_symlink()
            frame_server = start(["/usr/bin/python3", str(frame_script)]) if frame_enabled else None
            frame_restarts = 0
            status["system_frame_service"] = bool(frame_server)
            child = start_shell()
            status["state"] = "shell_running"
            write_status(status_path, status)
            while not stopping:
                if wm.poll() is not None:
                    raise ValueError("Meadow window manager exited")
                if frame_server is not None and frame_server.poll() is not None and frame_restarts < 3:
                    frame_restarts += 1
                    frame_server = start(["/usr/bin/python3", str(frame_script)])
                    status["frame_service_restarts"] = frame_restarts
                    write_status(status_path, status)
                elif frame_server is not None and frame_server.poll() is not None and status["system_frame_service"]:
                    status["system_frame_service"] = False
                    status["frame_service_error"] = "System frame service stopped after bounded recovery"
                    write_status(status_path, status)
                compositor, changed = observe_compositor(compositor, status)
                if changed:
                    print(status["compositor_error"], file=log, flush=True)
                    write_status(status_path, status)
                if child is not None and child.poll() is not None:
                    status["last_shell_exit_code"] = child.returncode
                    if status["shell_restarts"] < SHELL_RESTART_LIMIT:
                        status["shell_restarts"] += 1
                        # The initial backdrop was destroyed with the Shell.
                        # Recreate only our compositor after the new Shell paints;
                        # keep WM, Agent and ordinary applications untouched.
                        if compositor is not None:
                            stop_owned(compositor)
                            compositor = None
                            status.update({"compositor": "none", "native_backdrop_blur": False})
                            status.pop("compositor_pid", None)
                            status.pop("compositor_owner", None)
                        status["state"] = "shell_restarting"
                        write_status(status_path, status)
                        time.sleep(1)
                        if stopping:
                            break
                        child = start_shell()
                        status["state"] = "shell_running"
                    else:
                        # Keep WM, ordinary applications and systemd Agent alive.
                        start([commands["xfce4-terminal"], "--disable-server", "--title=MeadowOS recovery"])
                        child = None
                        status["state"] = "shell_failed_recovery_terminal"
                        print("Meadow Shell failed repeatedly. Recovery terminal opened; Agent and other apps remain.", file=log, flush=True)
                    write_status(status_path, status)
                time.sleep(0.1)
            status["state"] = "stopping"
            write_status(status_path, status)
            return 0
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            status.update({"state": "failed", "error": str(error)})
            write_status(status_path, status)
            raise
        finally:
            # Only children created by this supervisor; never pkill users' apps.
            for process in reversed(processes):
                stop_owned(process)
            if agent_started:
                try:
                    subprocess.run(["systemctl", "--user", "stop", runtime.UNIT],
                                   capture_output=True, timeout=10, check=True)
                except (OSError, subprocess.SubprocessError) as error:
                    status["agent_service_stop_error"] = str(error)
                    write_status(status_path, status)
            for sig, handler in handlers.items():
                signal.signal(sig, handler)
