#!/usr/bin/python3
"""One bounded JSON request against the host-selected MeadowOS desktop.

This adapter never discovers displays. The trusted Agent supplies DISPLAY and
the session bus; model arguments cannot select a display or a different bus.
Execution receipts contain observed window/accessibility state, not goal claims.
"""
from __future__ import annotations

import hashlib
import json
import os
import ctypes
import ctypes.util
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlparse

MAX_REQUEST = 256 * 1024
MAX_TEXT = 16 * 1024
MAX_AX_BYTES = 180 * 1024
READ_ACTIONS = {"status", "snapshot", "windows", "applications", "accessibility", "screenshot"}
WINDOW_ACTIONS = {"focus", "close", "minimize", "maximize", "move_resize"}


class DesktopError(Exception):
    def __init__(self, code, message):
        self.code, self.message = code, message
        super().__init__(message)


def integer(value, name, low, high):
    if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
        raise DesktopError("invalid_argument", f"{name} must be an integer from {low} to {high}")
    return value


def text_arg(request, name, *, required=False, maximum=MAX_TEXT):
    value = request.get(name, "")
    if not isinstance(value, str) or "\x00" in value or len(value.encode()) > maximum:
        raise DesktopError("invalid_argument", f"{name} must be bounded text without NUL")
    if required and not value:
        raise DesktopError("invalid_argument", f"{name} is required")
    return value


def window_number(value):
    try:
        if isinstance(value, bool) or not isinstance(value, (str, int)):
            raise ValueError()
        number = int(value, 0) if isinstance(value, str) else int(value)
        if number <= 0 or number > 0xFFFFFFFF:
            raise ValueError()
        return number
    except (ValueError, TypeError, OverflowError):
        raise DesktopError("invalid_argument", "window_id must be an observed positive X11 window id")


def canonical_display(value):
    return re.sub(r"\.[0-9]+$", "", value)


def node_fingerprint(name, role, actions, accessible_id=""):
    identity = [name, role, [action["name"] for action in actions], accessible_id]
    return hashlib.sha256(json.dumps(identity, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()[:24]


class NativeWindow:
    """Observed X11 client fallback when libwnck has not loaded WM properties."""
    def __init__(self, desktop, data):
        self.desktop, self.data = desktop, data

    def get_xid(self): return int(self.data["window_id"], 16)
    def get_pid(self): return self.data["pid"]
    def get_name(self): return self.data["title"]
    def get_class_group_name(self): return self.data["wm_class"]
    def get_class_instance_name(self): return self.data["instance"]
    def get_geometry(self): return tuple(self.data["geometry"][key] for key in ("x", "y", "width", "height"))
    def get_workspace(self): return None
    def is_active(self):
        return any(row["window_id"] == self.data["window_id"] and row["active"] for row in self.desktop.native_rows())
    def is_minimized(self): return self.data["minimized"]
    def is_maximized(self): return self.data["maximized"]
    def unminimize(self, _time): self.desktop.xdotool("windowmap", self.get_xid())
    def activate(self, _time): self.desktop.xdotool("windowactivate", self.get_xid())
    def close(self, timestamp): self.desktop.send_window_event(self.get_xid(), "_NET_CLOSE_WINDOW", [timestamp, 2, 0, 0, 0])
    def minimize(self): self.desktop.xdotool("windowminimize", self.get_xid())
    def maximize(self): self._maximize(1)
    def unmaximize(self): self._maximize(0)
    def _maximize(self, enabled):
        self.desktop.send_window_event(self.get_xid(), "_NET_WM_STATE", [enabled, "_NET_WM_STATE_MAXIMIZED_VERT", "_NET_WM_STATE_MAXIMIZED_HORZ", 2, 0])
    def set_geometry(self, _gravity, _mask, x, y, width, height):
        extents = self.data.get("frame_extents", [0, 0, 0, 0])
        client_width = max(1, width - extents[0] - extents[1])
        client_height = max(1, height - extents[2] - extents[3])
        self.desktop.send_window_event(self.get_xid(), "_NET_MOVERESIZE_WINDOW", [(15 << 8) | (2 << 12), x, y, client_width, client_height])


class Desktop:
    def __init__(self):
        self.selected = os.environ.get("DISPLAY", "").strip()
        if not self.selected:
            raise DesktopError("desktop_unavailable", "The Agent has no selected DISPLAY; no other desktop was searched")
        import gi
        gi.require_version("Gtk", "3.0")
        gi.require_version("Gdk", "3.0")
        gi.require_version("Wnck", "3.0")
        gi.require_version("Gio", "2.0")
        from gi.repository import Gtk, Gdk, Gio, GLib, Wnck
        self.Gtk, self.Gdk, self.Gio, self.GLib, self.Wnck = Gtk, Gdk, Gio, GLib, Wnck
        initialized = Gtk.init_check([])
        if not (initialized[0] if isinstance(initialized, tuple) else initialized):
            raise DesktopError("desktop_unavailable", f"Cannot connect to selected desktop {self.selected}")
        self.display = Gdk.Display.get_default()
        if self.display is None or canonical_display(self.display.get_name()) != canonical_display(self.selected):
            raise DesktopError("desktop_identity_mismatch", "Toolkit display does not match the host-selected display")
        Wnck.set_client_type(Wnck.ClientType.PAGER)
        self.screen = Wnck.Screen.get_default()
        if self.screen is None:
            raise DesktopError("desktop_unavailable", "The selected desktop has no X11 window inventory")
        self.screen.force_update()
        self.root = Gdk.get_default_root_window()
        self.desktop = {"display": self.display.get_name(),
                        "session_id": os.environ.get("MEADOW_DESKTOP_SESSION_ID", "")}

    def pump(self, duration=.12):
        until = time.monotonic() + duration
        while True:
            while self.Gtk.events_pending():
                self.Gtk.main_iteration_do(False)
            self.screen.force_update()
            if time.monotonic() >= until:
                return
            time.sleep(.015)

    def timestamp(self):
        try:
            import gi
            gi.require_version("GdkX11", "3.0")
            from gi.repository import GdkX11
            return GdkX11.x11_get_server_time(self.root)
        except (ImportError, ValueError, AttributeError):
            return self.Gdk.CURRENT_TIME

    def row(self, window):
        x, y, width, height = window.get_geometry()
        return {"window_id": hex(window.get_xid()), "pid": window.get_pid(),
                "title": (window.get_name() or "")[:500], "wm_class": (window.get_class_group_name() or "")[:200],
                "instance": (window.get_class_instance_name() or "")[:200], "display": self.desktop["display"],
                "geometry": {"x": x, "y": y, "width": width, "height": height},
                "active": window.is_active(), "minimized": window.is_minimized(),
                "maximized": window.is_maximized(), "workspace": window.get_workspace().get_number() if window.get_workspace() else None}

    def windows(self):
        self.screen.force_update()
        # A newly started WM may publish _NET_CLIENT_LIST before its stacking
        # property. Those managed windows are still real desktop observations.
        native = self.screen.get_windows_stacked() or self.screen.get_windows()
        return [self.row(w) for w in native][:100] if native else self.native_rows()

    def xprop(self, *arguments):
        result = subprocess.run(["xprop", *map(str, arguments)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=3, check=False)
        return result.stdout.decode(errors="replace")

    def native_rows(self):
        roots = self.xprop("-root", "_NET_CLIENT_LIST_STACKING", "_NET_CLIENT_LIST", "_NET_ACTIVE_WINDOW")
        properties = {line.split("(", 1)[0]: line for line in roots.splitlines()}
        clients = properties.get("_NET_CLIENT_LIST_STACKING", "")
        if "0x" not in clients:
            clients = properties.get("_NET_CLIENT_LIST", "")
        active = re.search(r"0x[0-9a-fA-F]+", properties.get("_NET_ACTIVE_WINDOW", ""))
        active_id = int(active.group(), 16) if active else 0
        rows = []
        deadline = time.monotonic() + 4
        for xid in re.findall(r"0x[0-9a-fA-F]+", clients)[:100]:
            if time.monotonic() > deadline:
                break
            if int(xid, 16) == 0:
                continue
            props = self.xprop("-id", xid, "_NET_WM_NAME", "WM_NAME", "WM_CLASS", "_NET_WM_PID", "_NET_WM_STATE", "WM_STATE", "_NET_FRAME_EXTENTS")
            fields = {line.split("(", 1)[0]: line for line in props.splitlines()}
            quoted = lambda key: re.findall(r'"((?:[^"\\]|\\.)*)"', fields.get(key, ""))
            titles = quoted("_NET_WM_NAME") or quoted("WM_NAME")
            classes = quoted("WM_CLASS")
            pid = re.search(r"=\s*([0-9]+)", fields.get("_NET_WM_PID", ""))
            info = subprocess.run(["xwininfo", "-id", xid, "-stats"], stdin=subprocess.DEVNULL,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=3, check=False).stdout.decode(errors="replace")
            numbers = []
            for label in ["Absolute upper-left X", "Absolute upper-left Y", "Width", "Height"]:
                match = re.search(re.escape(label) + r":\s*(-?[0-9]+)", info)
                numbers.append(int(match.group(1)) if match else None)
            if any(value is None for value in numbers):
                continue
            extent_text = fields.get("_NET_FRAME_EXTENTS", "").partition("=")[2]
            extents = [int(value) for value in re.findall(r"[0-9]+", extent_text)]
            extents = extents if len(extents) == 4 else [0, 0, 0, 0]
            frame = [numbers[0] - extents[0], numbers[1] - extents[2],
                     numbers[2] + extents[0] + extents[1], numbers[3] + extents[2] + extents[3]]
            rows.append({"window_id": hex(int(xid, 16)), "pid": int(pid.group(1)) if pid else 0,
                         "title": titles[0].replace('\\"', '"').replace("\\\\", "\\")[:500] if titles else "",
                         "instance": classes[0][:200] if classes else "", "wm_class": classes[1][:200] if len(classes) > 1 else "",
                         "display": self.desktop["display"], "geometry": dict(zip(("x", "y", "width", "height"), frame)),
                         "frame_extents": extents,
                         "active": int(xid, 16) == active_id, "minimized": "_NET_WM_STATE_HIDDEN" in props or "Iconic" in props,
                         "maximized": "_NET_WM_STATE_MAXIMIZED_VERT" in props and "_NET_WM_STATE_MAXIMIZED_HORZ" in props,
                         "workspace": None})
        return rows

    def send_window_event(self, xid, event_name, values):
        class MessageData(ctypes.Union):
            _fields_ = [("b", ctypes.c_char * 20), ("s", ctypes.c_short * 10), ("l", ctypes.c_long * 5)]
        class ClientMessage(ctypes.Structure):
            _fields_ = [("type", ctypes.c_int), ("serial", ctypes.c_ulong), ("send_event", ctypes.c_int),
                        ("display", ctypes.c_void_p), ("window", ctypes.c_ulong), ("message_type", ctypes.c_ulong),
                        ("format", ctypes.c_int), ("data", MessageData)]
        class XEvent(ctypes.Union):
            _fields_ = [("client", ClientMessage), ("padding", ctypes.c_long * 24)]
        library = ctypes.CDLL(ctypes.util.find_library("X11") or "libX11.so.6")
        library.XOpenDisplay.argtypes, library.XOpenDisplay.restype = [ctypes.c_char_p], ctypes.c_void_p
        library.XDefaultRootWindow.argtypes, library.XDefaultRootWindow.restype = [ctypes.c_void_p], ctypes.c_ulong
        library.XInternAtom.argtypes, library.XInternAtom.restype = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int], ctypes.c_ulong
        library.XSendEvent.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_int, ctypes.c_long, ctypes.c_void_p]
        library.XFlush.argtypes = library.XCloseDisplay.argtypes = [ctypes.c_void_p]
        connection = library.XOpenDisplay(self.selected.encode())
        if not connection:
            raise DesktopError("desktop_unavailable", "Selected X11 display disconnected")
        try:
            native_event = XEvent()
            event = native_event.client
            event.type, event.send_event, event.display, event.window, event.format = 33, 1, connection, xid, 32
            event.message_type = library.XInternAtom(connection, event_name.encode(), 0)
            for index, value in enumerate(values):
                event.data.l[index] = library.XInternAtom(connection, value.encode(), 0) if isinstance(value, str) else value
            if not library.XSendEvent(connection, library.XDefaultRootWindow(connection), 0, (1 << 19) | (1 << 20), ctypes.byref(native_event)):
                raise DesktopError("window_action_failed", "X11 did not accept the selected-window event")
            library.XFlush(connection)
        finally:
            library.XCloseDisplay(connection)

    def target(self, request, *, active=False):
        if request.get("window_id") is None and active:
            window = self.screen.get_active_window()
            if window is None:
                active_row = next((row for row in self.native_rows() if row["active"]), None)
                if active_row is None:
                    raise DesktopError("window_not_found", "No active window exists on the selected desktop")
                return NativeWindow(self, active_row)
            return window
        wanted = window_number(request.get("window_id"))
        self.screen.force_update()
        for window in self.screen.get_windows():
            if window.get_xid() == wanted:
                return window
        for row in self.native_rows():
            if int(row["window_id"], 16) == wanted:
                return NativeWindow(self, row)
        raise DesktopError("window_not_found", "window_id is not present on the host-selected desktop; refresh snapshot")

    def snapshot(self):
        geometry = self.root.get_geometry()
        return {"screen": {"width": geometry.width, "height": geometry.height},
                "windows": self.windows(),
                "capabilities": {"windows": True, "launch": True, "screenshot": True,
                                 "input": bool(shutil.which("xdotool")),
                                 "accessibility": self.atspi_available()}}

    def atspi_available(self):
        try:
            import gi
            gi.require_version("Atspi", "2.0")
            from gi.repository import Atspi
            return bool(os.environ.get("DBUS_SESSION_BUS_ADDRESS")) and Atspi is not None
        except (ImportError, ValueError):
            return False

    def applications(self):
        rows = []
        for app in self.Gio.AppInfo.get_all():
            if not isinstance(app, self.Gio.DesktopAppInfo) or not app.get_filename():
                continue
            rows.append({"application": app.get_id(), "name": app.get_display_name(),
                         "executable": app.get_executable(), "visible": app.should_show()})
        return {"applications": sorted(rows, key=lambda row: row["application"].casefold())[:400],
                "aliases": ["finder", "files", "browser", "chrome", "terminal", "settings"]}

    def launch(self, request):
        application = text_arg(request, "application", required=True, maximum=300)
        target = text_arg(request, "target", maximum=8192)
        before = {row["window_id"] for row in self.windows()}
        aliases = {
            "finder": (("thunar",), ["--display=" + self.selected, "--window"]),
            "files": (("thunar",), ["--display=" + self.selected, "--window"]),
            "chrome": (("google-chrome", "chromium", "chromium-browser"), ["--force-renderer-accessibility", "--new-window"]),
            "browser": (("google-chrome", "chromium", "chromium-browser", "firefox"), []),
            "terminal": (("xfce4-terminal", "xterm"), []),
            "settings": (("meadow-settings",), []),
            "thunar.desktop": (("thunar",), ["--display=" + self.selected, "--window"]),
            "org.xfce.Thunar.desktop": (("thunar",), ["--display=" + self.selected, "--window"]),
            "org.xfce.thunar.desktop": (("thunar",), ["--display=" + self.selected, "--window"]),
            # These ids are also listed by applications. They must use the same
            # display-owned profile contract as the short aliases, rather than
            # handing off to an unrelated existing Chrome process.
            "google-chrome.desktop": (("google-chrome",), ["--force-renderer-accessibility", "--new-window"]),
            "google-chrome-stable.desktop": (("google-chrome-stable", "google-chrome"), ["--force-renderer-accessibility", "--new-window"]),
            "chromium.desktop": (("chromium", "chromium-browser"), ["--force-renderer-accessibility", "--new-window"]),
            "chromium-browser.desktop": (("chromium-browser", "chromium"), ["--force-renderer-accessibility", "--new-window"]),
        }
        process = None
        launch_pids = []
        expected_class = ""
        if application in aliases:
            choices, arguments = aliases[application]
            executable = next((shutil.which(choice) for choice in choices if shutil.which(choice)), None)
            if executable is None:
                raise DesktopError("application_unavailable", f"No installed executable for {application}")
            if application == "browser" and "chrom" in Path(executable).name:
                arguments = ["--force-renderer-accessibility", "--new-window"]
            if "chrom" in Path(executable).name:
                identity = os.environ.get("MEADOW_DESKTOP_SESSION_ID", "") + "\x00" + canonical_display(self.selected)
                profile_key = hashlib.sha256(identity.encode()).hexdigest()[:24]
                data_home = Path(os.environ.get("XDG_DATA_HOME") or (Path.home() / ".local/share"))
                profile = data_home / "meadow-os/browser-profiles" / profile_key
                profile.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                profile.mkdir(mode=0o700, exist_ok=True)
                stat = profile.lstat()
                if profile.is_symlink() or not profile.is_dir() or stat.st_uid != os.geteuid() or stat.st_mode & 0o077:
                    raise DesktopError("browser_profile_unavailable", "The selected desktop's browser profile must be an owned private directory")
                arguments = ["--force-renderer-accessibility=complete" if item == "--force-renderer-accessibility" else item for item in arguments]
                arguments = [*arguments, "--no-first-run", "--no-default-browser-check", "--user-data-dir=" + str(profile)]
                expected_class = "chrome" if "chrome" in Path(executable).name else "chromium"
            if application == "terminal" and Path(executable).name == "xfce4-terminal":
                arguments = ["--disable-server"]
            if not expected_class:
                expected_class = {"finder": "thunar", "files": "thunar", "settings": "meadow-settings"}.get(application, Path(executable).name)
            argv = [executable, *arguments]
            if target:
                argv.append(target)
            child_env = None
            if "chrom" in Path(executable).name:
                # Chromium's native ATK bridge checks this before the global
                # screen-reader setting. Enable only this owned browser child.
                child_env = dict(os.environ)
                child_env["ACCESSIBILITY_ENABLED"] = "1"
            if Path(executable).name == "thunar":
                # Thunar's shared GApplication service can ignore --display on
                # remote CLI forwarding. Give this app its own bus lifetime,
                # while keeping accessibility on the selected desktop's bus.
                bus_launcher = shutil.which("dbus-run-session")
                if bus_launcher is None:
                    raise DesktopError("application_unavailable", "dbus-run-session is required to bind Files to the selected desktop")
                try:
                    connection = self.Gio.bus_get_sync(self.Gio.BusType.SESSION, None)
                    reply = connection.call_sync("org.a11y.Bus", "/org/a11y/bus", "org.a11y.Bus", "GetAddress", None,
                                                 self.GLib.VariantType.new("(s)"), self.Gio.DBusCallFlags.NONE, 2000, None)
                    accessibility_bus = reply.unpack()[0]
                except self.GLib.Error as exc:
                    raise DesktopError("accessibility_unavailable", "Cannot resolve the selected desktop's accessibility bus: " + str(exc)[:300])
                child_env = dict(os.environ)
                child_env["AT_SPI_BUS_ADDRESS"] = accessibility_bus
                argv = [bus_launcher, "--", *argv]
            process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                       stderr=subprocess.DEVNULL, start_new_session=True, env=child_env)
            launch_pids.append(process.pid)
        else:
            if not re.fullmatch(r"[A-Za-z0-9_.+-]+\.desktop", application):
                raise DesktopError("invalid_argument", "application must be an observed desktop id or supported alias")
            app = self.Gio.DesktopAppInfo.new(application)
            if app is None:
                raise DesktopError("application_unavailable", "Desktop application is not installed")
            context = self.display.get_app_launch_context()
            context.setenv("DISPLAY", self.selected)
            uris = [target if urlparse(target).scheme in ("http", "https", "file") else Path(target).as_uri()] if target else []
            expected_class = app.get_startup_wm_class() or Path(app.get_executable() or "").name
            flags = self.GLib.SpawnFlags.STDOUT_TO_DEV_NULL | self.GLib.SpawnFlags.STDERR_TO_DEV_NULL
            app.launch_uris_as_manager(uris, context, flags, None, None,
                                      lambda _app, pid, _data: launch_pids.append(pid), None)
        end = time.monotonic() + 6
        changed = []
        while time.monotonic() < end:
            self.pump(.10)
            changed = [row for row in self.windows() if row["window_id"] not in before and
                       (row["pid"] in launch_pids or (expected_class and expected_class.casefold() in
                        (row["wm_class"] + " " + row["instance"]).casefold()))]
            if changed:
                break
            if process is not None and process.poll() not in (None, 0):
                raise DesktopError("application_launch_failed", f"Application process exited with status {process.returncode}; no selected-desktop window was observed")
        return {"launch": {"application": application, "target": target, "spawned_pid": process.pid if process else None,
                           "launch_pids": launch_pids,
                           "new_windows_observed": changed,
                           "window_observed": bool(changed),
                           "goal_verified": False}, "windows": self.windows()}

    def open_target(self, request):
        target = text_arg(request, "target", required=True, maximum=8192)
        application = text_arg(request, "application", maximum=300)
        if application:
            return self.launch(request)
        scheme = urlparse(target).scheme
        if scheme in ("http", "https"):
            return self.launch({**request, "application": "browser"})
        if scheme and scheme != "file":
            raise DesktopError("invalid_argument", "Open supports a local approved path or HTTP(S) URL")
        path = Path(target)
        if not path.is_absolute() or not path.exists():
            raise DesktopError("invalid_argument", "Local target must be an existing absolute approved path")
        if path.is_dir():
            return self.launch({**request, "application": "files"})
        if path.suffix.lower() in (".html", ".htm", ".pdf", ".svg", ".png", ".jpg", ".jpeg", ".webp"):
            return self.launch({**request, "application": "browser", "target": path.as_uri()})
        content_type, _ = self.Gio.content_type_guess(str(path), None)
        app = self.Gio.AppInfo.get_default_for_type(content_type, False)
        if app is None or not app.get_id():
            raise DesktopError("application_unavailable", "No installed default application for this target")
        return self.launch({**request, "application": app.get_id()})

    def focus(self, window):
        workspace = window.get_workspace()
        if workspace is not None:
            workspace.activate(self.timestamp())
        window.unminimize(self.timestamp())
        window.activate(self.timestamp())
        self.pump(.18)

    def window_action(self, action, request):
        window = self.target(request)
        before = self.row(window)
        xid = window.get_xid()
        if action == "focus":
            self.focus(window)
        elif action == "close":
            window.close(self.timestamp())
        elif action == "minimize":
            window.minimize()
        elif action == "maximize":
            enabled = request.get("enabled", True)
            if not isinstance(enabled, bool):
                raise DesktopError("invalid_argument", "enabled must be a boolean")
            window.maximize() if enabled else window.unmaximize()
        elif action == "move_resize":
            geometry = before["geometry"]
            x = integer(request.get("x", geometry["x"]), "x", -32768, 32767)
            y = integer(request.get("y", geometry["y"]), "y", -32768, 32767)
            width = integer(request.get("width", geometry["width"]), "width", 64, 32767)
            height = integer(request.get("height", geometry["height"]), "height", 48, 32767)
            window.unmaximize()
            mask = self.Wnck.WindowMoveResizeMask.X | self.Wnck.WindowMoveResizeMask.Y | self.Wnck.WindowMoveResizeMask.WIDTH | self.Wnck.WindowMoveResizeMask.HEIGHT
            window.set_geometry(self.Wnck.WindowGravity.CURRENT, mask, x, y, width, height)
        self.pump(.25)
        after = next((row for row in self.windows() if int(row["window_id"], 16) == xid), None)
        confirmed = ((after is None) if action == "close" else bool(after))
        if after is not None:
            if action == "focus":
                confirmed = after["active"]
            elif action == "minimize":
                confirmed = after["minimized"]
            elif action == "maximize":
                confirmed = after["maximized"] == request.get("enabled", True)
            elif action == "move_resize":
                confirmed = all(after["geometry"][key] == request[key] for key in ("x", "y", "width", "height") if key in request)
        return {"action_receipt": {"before": before, "after": after, "observed": confirmed}, "windows": self.windows()}

    def accessibility_root(self, window):
        if not self.atspi_available():
            raise DesktopError("accessibility_unavailable", "AT-SPI or the selected session bus is unavailable")
        from gi.repository import Atspi
        expected = self.row(window)
        for app in Atspi.get_desktop(0):
            try:
                if app.get_process_id() != expected["pid"]:
                    continue
                candidates = []
                for index in range(app.get_child_count()):
                    child = app.get_child_at_index(index)
                    rect = child.get_component_iface().get_extents(Atspi.CoordType.SCREEN)
                    geometry = expected["geometry"]
                    overlap = (rect.x < geometry["x"] + geometry["width"] and rect.x + rect.width > geometry["x"]
                               and rect.y < geometry["y"] + geometry["height"] and rect.y + rect.height > geometry["y"])
                    if overlap and rect.width > 0 and rect.height > 0:
                        candidates.append((child.get_name() == expected["title"], index, child))
                if candidates:
                    exact = [candidate for candidate in candidates if candidate[0]]
                    chosen = exact if exact else candidates
                    if len(chosen) != 1:
                        raise DesktopError("accessibility_ambiguous", "More than one AT-SPI surface matches this window; use screenshot/native input instead of selecting an arbitrary surface")
                    _, index, child = chosen[0]
                    return Atspi, app, child, [index]
            except (self.GLib.Error, AttributeError, TypeError):
                continue
        raise DesktopError("accessibility_unavailable", "Selected window has no matching AT-SPI surface; use snapshot/screenshot and native input")

    def node_row(self, Atspi, node, window, path):
        name = node.get_name() or ""
        role = node.get_role_name() or ""
        states = node.get_state_set()
        protected = node.get_role() == Atspi.Role.PASSWORD_TEXT
        row = {"name": name[:500], "role": role, "focused": states.contains(Atspi.StateType.FOCUSED),
               "enabled": states.contains(Atspi.StateType.ENABLED), "showing": states.contains(Atspi.StateType.SHOWING)}
        try:
            rectangle = node.get_component_iface().get_extents(Atspi.CoordType.SCREEN)
            row["bounds"] = {"x": rectangle.x, "y": rectangle.y, "width": rectangle.width, "height": rectangle.height}
        except (self.GLib.Error, AttributeError):
            pass
        try:
            action = node.get_action_iface()
            row["actions"] = [{"index": index, "name": action.get_action_name(index)} for index in range(min(action.get_n_actions(), 12))]
        except (self.GLib.Error, AttributeError):
            pass
        if not protected:
            try:
                interface = node.get_text_iface()
                count = Atspi.Text.get_character_count(interface)
                if count:
                    # get_text on Accessible itself is a deprecated interface
                    # getter; explicit Text dispatch avoids that GI collision.
                    row["text"] = Atspi.Text.get_text(interface, 0, min(count, 2000))
                    row["text_truncated"] = count > 2000
            except (self.GLib.Error, AttributeError, TypeError):
                pass
        else:
            row["protected"] = True
        accessible_id = ""
        try:
            getter = getattr(node, "get_accessible_id", None)
            accessible_id = getter() or "" if getter is not None else ""
        except (self.GLib.Error, AttributeError):
            pass
        fingerprint = node_fingerprint(name, role, row.get("actions", []), accessible_id)
        row["node_id"] = "/".join([hex(window.get_xid()), str(window.get_pid()), *map(str, path), "@" + fingerprint])
        return row

    def accessibility(self, request):
        window = self.target(request, active=True)
        Atspi, _app, root, path = self.accessibility_root(window)
        limit = integer(request.get("max_nodes", 200), "max_nodes", 1, 400)
        depth_limit = integer(request.get("max_depth", 12), "max_depth", 1, 16)
        rows, queue = [], [(root, path, 0)]
        encoded_bytes, budget_exhausted = 0, False
        deadline = time.monotonic() + 4
        while queue and len(rows) < limit and time.monotonic() < deadline:
            node, node_path, depth = queue.pop(0)
            try:
                row = self.node_row(Atspi, node, window, node_path)
                if row["showing"] or depth == 0:
                    row["depth"] = depth
                    row_bytes = len(json.dumps(row, ensure_ascii=False, separators=(",", ":")).encode())
                    if encoded_bytes + row_bytes > MAX_AX_BYTES:
                        budget_exhausted = True
                        break
                    encoded_bytes += row_bytes
                    rows.append(row)
                if depth < depth_limit:
                    for index in range(min(node.get_child_count(), 200)):
                        queue.append((node.get_child_at_index(index), [*node_path, index], depth + 1))
            except (self.GLib.Error, AttributeError, TypeError):
                continue
        return {"window": self.row(window), "nodes": rows, "truncated": bool(queue) or budget_exhausted,
                "encoded_node_bytes": encoded_bytes, "node_byte_limit": MAX_AX_BYTES}

    def resolve_node(self, request):
        node_id = text_arg(request, "node_id", required=True, maximum=500)
        parts = node_id.split("/")
        if len(parts) < 4 or len(parts) > 21:
            raise DesktopError("invalid_argument", "node_id must come from a fresh accessibility observation")
        if request.get("window_id") is not None and window_number(request["window_id"]) != window_number(parts[0]):
            raise DesktopError("stale_node", "Accessibility node belongs to a different selected window; refresh observation")
        window = self.target({"window_id": parts[0]})
        if str(window.get_pid()) != parts[1]:
            raise DesktopError("stale_node", "Window process changed; refresh accessibility snapshot")
        Atspi, app, _root, root_path = self.accessibility_root(window)
        try:
            if not re.fullmatch(r"@[a-f0-9]{24}", parts[-1]):
                raise ValueError()
            path = [int(part) for part in parts[2:-1]]
            if path[0] != root_path[0] or any(index < 0 or index > 100000 for index in path):
                raise ValueError()
            node = app
            for index in path:
                node = node.get_child_at_index(index)
                if node is None:
                    raise ValueError()
            observed = self.node_row(Atspi, node, window, path)
            if observed["node_id"].rsplit("/", 1)[-1] != parts[-1]:
                raise DesktopError("stale_node", "The accessibility control at this path changed identity; refresh observation")
            return Atspi, window, node, path
        except (ValueError, self.GLib.Error, TypeError):
            raise DesktopError("stale_node", "Accessibility node changed; refresh accessibility snapshot")

    def accessibility_action(self, action, request):
        Atspi, window, node, path = self.resolve_node(request)
        before = self.node_row(Atspi, node, window, path)
        if action == "set_text":
            text = text_arg(request, "text", maximum=MAX_TEXT)
            interface = node.get_editable_text_iface()
            if interface is None:
                raise DesktopError("node_not_editable", "Selected accessibility node does not support editable text")
            performed = interface.set_text_contents(text)
        else:
            interface = node.get_action_iface()
            if interface is None:
                raise DesktopError("action_unavailable", "Selected accessibility node exposes no actions")
            name = text_arg(request, "action_name", maximum=200)
            index = request.get("action_index")
            if name:
                index = next((i for i in range(interface.get_n_actions()) if interface.get_action_name(i) == name), None)
                if index is None:
                    raise DesktopError("action_unavailable", "Action name was not present on this node; refresh observation")
            if index is None:
                index = 0
            integer(index, "action_index", 0, max(0, interface.get_n_actions() - 1))
            performed = interface.do_action(index)
        self.pump(.2)
        try:
            after = self.node_row(Atspi, node, window, path)
        except (self.GLib.Error, AttributeError, TypeError):
            after = None
        if not performed:
            raise DesktopError("accessibility_action_failed", "The native accessibility interface did not perform the requested action")
        return {"action_receipt": {"performed": True, "before": before, "after": after}, "windows": self.windows()}

    def xdotool(self, *arguments):
        executable = shutil.which("xdotool")
        if executable is None:
            raise DesktopError("input_unavailable", "xdotool is not installed on the selected desktop")
        result = subprocess.run([executable, *map(str, arguments)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, check=False)
        if result.returncode:
            raise DesktopError("input_failed", result.stderr.decode(errors="replace")[-1000:] or "Native input failed")

    def input_action(self, action, request):
        window = self.target(request, active=True)
        self.focus(window)
        if not window.is_active():
            raise DesktopError("focus_unavailable", "The selected window did not become active; no input was sent")
        before = self.row(window)
        paste_key = None
        pointer = None
        if action in ("click", "move_pointer", "drag"):
            geometry = self.root.get_geometry()
            x = integer(request.get("x"), "x", 0, geometry.width - 1)
            y = integer(request.get("y"), "y", 0, geometry.height - 1)
            pointer = {"x": x, "y": y}
            self.xdotool("mousemove", x, y)
            if action == "click":
                button = integer(request.get("button", 1), "button", 1, 3)
                count = integer(request.get("count", 1), "count", 1, 3)
                self.xdotool("click", "--repeat", count, "--delay", 100, button)
            elif action == "drag":
                to_x = integer(request.get("to_x"), "to_x", 0, geometry.width - 1)
                to_y = integer(request.get("to_y"), "to_y", 0, geometry.height - 1)
                button = integer(request.get("button", 1), "button", 1, 3)
                self.xdotool("mousedown", button)
                try:
                    self.xdotool("mousemove", to_x, to_y)
                    self.pump(.15)
                finally:
                    self.xdotool("mouseup", button)
        elif action == "scroll":
            delta_x = integer(request.get("delta_x", 0), "delta_x", -20, 20)
            delta_y = integer(request.get("delta_y", 0), "delta_y", -20, 20)
            screen = self.root.get_geometry()
            frame = before["geometry"]
            left, top = max(0, frame["x"]), max(0, frame["y"])
            right, bottom = min(screen.width, frame["x"] + frame["width"]), min(screen.height, frame["y"] + frame["height"])
            if left >= right or top >= bottom:
                raise DesktopError("input_unavailable", "Selected window has no visible screen area to receive scroll input")
            x, y = (left + right) // 2, (top + bottom) // 2
            pointer = {"x": x, "y": y}
            self.xdotool("mousemove", x, y)
            for delta, negative, positive in ((delta_y, 4, 5), (delta_x, 6, 7)):
                if delta:
                    self.xdotool("click", "--repeat", abs(delta), "--delay", 25, negative if delta < 0 else positive)
        elif action == "key":
            key = text_arg(request, "key", required=True, maximum=128)
            if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_+.-]*", key):
                raise DesktopError("invalid_argument", "key must be a native keysym combination such as ctrl+l or Return")
            self.xdotool("key", "--clearmodifiers", key)
        else:
            text = text_arg(request, "text", maximum=MAX_TEXT)
            window_class = (before["wm_class"] + " " + before["instance"]).casefold()
            terminal = any(name in window_class for name in ("xfce4-terminal", "gnome-terminal", "mate-terminal", "tilix", "konsole", "terminator"))
            xterm = any(name in window_class.split() for name in ("xterm", "uxterm"))
            paste_key = "ctrl+shift+v" if terminal else "shift+Insert" if xterm else "ctrl+v"
            clipboard = self.Gtk.Clipboard.get(self.Gdk.SELECTION_PRIMARY if xterm else self.Gdk.SELECTION_CLIPBOARD)
            clipboard.set_text(text, -1)
            self.pump(.05)
            self.xdotool("key", "--clearmodifiers", paste_key)
            self.pump(.35)
        self.pump(.15)
        return {"input": {"kind": action, "dispatched": True, "window_before": before,
                          "paste_key": paste_key,
                          "pointer": pointer,
                          "text_characters": len(request.get("text", "")) if action == "type_text" else None},
                "windows": self.windows()}

    def screenshot(self, request):
        raw_path = text_arg(request, "output_path", required=True, maximum=8192)
        path = Path(raw_path)
        if not path.is_absolute() or path.suffix.lower() != ".png" or not path.parent.is_dir():
            raise DesktopError("invalid_argument", "output_path must be an absolute approved PNG path with an existing parent")
        if path.is_symlink():
            raise DesktopError("invalid_argument", "Screenshot destination cannot be a symlink")
        geometry = self.root.get_geometry()
        pixels = self.Gdk.pixbuf_get_from_window(self.root, 0, 0, geometry.width, geometry.height)
        if pixels is None:
            raise DesktopError("screenshot_unavailable", "Selected desktop pixels are unavailable")
        descriptor, temporary = tempfile.mkstemp(prefix=".meadow-screenshot-", suffix=".png", dir=path.parent)
        os.close(descriptor)
        temporary_path = Path(temporary)
        try:
            pixels.savev(str(temporary_path), "png", [], [])
            data = temporary_path.read_bytes()
            try:
                os.link(temporary_path, path)
            except FileExistsError:
                raise DesktopError("output_exists", "Screenshot destination already exists; choose a new output path")
        finally:
            temporary_path.unlink(missing_ok=True)
        return {"screenshot": {"path": str(path), "width": pixels.get_width(), "height": pixels.get_height(),
                               "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest(),
                               "display": self.desktop["display"]}, "windows": self.windows()}

    def execute(self, request):
        action = request.get("action", request.get("operation", "status"))
        if not isinstance(action, str):
            raise DesktopError("invalid_argument", "action must be a string")
        if action in ("status", "snapshot", "windows"):
            result = self.snapshot()
        elif action == "applications":
            result = self.applications()
        elif action == "launch":
            result = self.launch(request)
        elif action == "open":
            result = self.open_target(request)
        elif action in WINDOW_ACTIONS:
            result = self.window_action(action, request)
        elif action == "accessibility":
            result = self.accessibility(request)
        elif action in ("accessibility_action", "set_text"):
            result = self.accessibility_action(action, request)
        elif action in ("click", "move_pointer", "drag", "scroll", "key", "type_text"):
            result = self.input_action(action, request)
        elif action == "screenshot":
            result = self.screenshot(request)
        else:
            raise DesktopError("invalid_argument", "Unknown desktop action")
        return {"ok": True, "action": action, "display": self.desktop["display"], "desktop": self.desktop, **result}


def main():
    request, action = {}, "status"
    try:
        raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
        if len(raw) > MAX_REQUEST:
            raise DesktopError("invalid_argument", "Desktop request exceeds size limit")
        request = json.loads(raw or b"{}")
        if not isinstance(request, dict):
            raise DesktopError("invalid_argument", "Desktop request must be a JSON object")
        action = request.get("action", request.get("operation", "status"))
        result = Desktop().execute(request)
    except DesktopError as exc:
        result = {"ok": False, "action": action, "display": os.environ.get("DISPLAY", ""),
                  "error": {"code": exc.code, "message": exc.message}}
    except (ImportError, ValueError, OSError, subprocess.SubprocessError) as exc:
        result = {"ok": False, "action": action, "display": os.environ.get("DISPLAY", ""),
                  "error": {"code": "desktop_unavailable", "message": str(exc)[:1200]}}
    except Exception as exc:
        result = {"ok": False, "action": action, "display": os.environ.get("DISPLAY", ""),
                  "error": {"code": "desktop_operation_failed", "message": str(exc)[:1200]}}
    print(json.dumps(result, ensure_ascii=False, separators=(",", ":")), flush=True)
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
