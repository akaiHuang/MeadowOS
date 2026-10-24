#!/usr/bin/python3
"""Unified native Meadow Settings with real XEmbed settings panels."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import subprocess
import sys
import threading

PAYLOAD = Path(__file__).resolve().parent
sys.path.insert(0, str(PAYLOAD))
from meadow_state import state_root

from application_catalog import (available_settings_tools, installed_applications,
                                 SETTINGS_TOOLS, settings_route)


def query_preference(channel: str, prop: str) -> str | None:
    executable = shutil.which("xfconf-query")
    if not executable:
        return None
    try:
        result = subprocess.run([executable, "-c", channel, "-p", prop], capture_output=True, text=True, timeout=2)
        return result.stdout.strip() if result.returncode == 0 else None
    except (OSError, subprocess.TimeoutExpired):
        return None


def appearance_snapshot() -> dict:
    native = query_preference("xsettings", "/Net/ThemeName")
    frame = query_preference("xfwm4", "/general/theme")
    icon_theme = query_preference("xsettings", "/Net/IconThemeName")
    receipt = None
    try:
        path = state_root() / "theme.json"
        if path.stat().st_size <= 65536:
            value = json.loads(path.read_text())
            if isinstance(value, dict):
                receipt = {key: value.get(key) for key in ("theme", "appearance_applied", "wallpaper_applied", "wallpaper_owner")}
    except (OSError, ValueError):
        pass
    selected = {"Meadow-Light": "light", "Meadow-Dark": "dark"}.get(native)
    return {"native_theme": native, "frame_theme": frame, "icon_theme": icon_theme,
            "theme": selected if frame == native else None, "receipt": receipt}


def parse_xrandr(text: str) -> dict:
    current = re.search(r"\bcurrent\s+(\d+)\s+x\s+(\d+)", text)
    maximum = re.search(r"\bmaximum\s+(\d+)\s+x\s+(\d+)", text)
    outputs, active = [], None
    lines = text.splitlines()
    for index, line in enumerate(lines):
        match = re.match(r"^(\S+) connected(?: (primary))?(?:\s|$)", line)
        if match:
            geometry = re.search(r"\b(\d+)x(\d+)([+-]\d+)([+-]\d+)", line)
            active = {"name": match[1], "primary": bool(match[2]),
                      "size": [int(geometry[1]), int(geometry[2])] if geometry else None,
                      "position": [int(geometry[3]), int(geometry[4])] if geometry else None,
                      "scale": None, "transform": None, "modes": []}
            outputs.append(active)
        elif line and not line[0].isspace():
            active = None
        elif active and (mode := re.match(r"^\s+(\d+x\d+)\s", line)):
            active["modes"].append(mode[1])
        elif active and line.strip().startswith("Transform:"):
            numbers = re.findall(r"[-+]?\d+(?:\.\d+)?", " ".join([line.split(":", 1)[1], *lines[index + 1:index + 3]]))
            if len(numbers) == 9:
                matrix = list(map(float, numbers))
                active["transform"] = matrix
                if (matrix[1] == matrix[3] == matrix[6] == matrix[7] == 0 and matrix[8] == 1
                        and matrix[0] > 0 and matrix[4] > 0):
                    active["scale"] = [matrix[0], matrix[4]]
    return {"size": list(map(int, current.groups())) if current else None,
            "maximum": list(map(int, maximum.groups())) if maximum else None, "outputs": outputs}


def virtual_display_server(display: str | None = None, proc: Path = Path("/proc")) -> bool:
    """Identify the actual local Xvfb server, not a guessed output/vendor name."""
    display = os.environ.get("DISPLAY", "") if display is None else display
    match = re.fullmatch(r"(?:unix)?:([0-9]+)(?:\.[0-9]+)?", display)
    if not match:
        return False
    wanted = ":" + str(int(match[1]))
    try:
        processes = proc.iterdir()
        for entry in processes:
            if not entry.name.isdecimal():
                continue
            try:
                if (entry / "comm").read_text().strip() != "Xvfb":
                    continue
                argv = (entry / "cmdline").read_bytes().split(b"\0")
                if len(argv) > 1 and Path(os.fsdecode(argv[0])).name == "Xvfb" and wanted.encode() in argv[1:]:
                    return True
            except (OSError, ValueError):
                continue
    except OSError:
        pass
    return False


VIRTUAL_SCALES = (1.0, 1.25, 1.5, 1.75, 2.0)


def virtual_scale_value(display: dict) -> float | None:
    maximum, size = display.get("maximum"), display.get("size")
    if not maximum or not size or min(size) <= 0:
        return None
    for scale in VIRTUAL_SCALES:
        if size == [round(side / scale) for side in maximum]:
            return scale
    return None


def apply_virtual_scale(scale: float) -> dict:
    """Scale the remote logical desktop; Xvfb cannot transform a CRTC.

    The viewer fits the logical framebuffer to its window. At 1.5x a native
    1920x1080 desktop becomes 1280x720, without a failing GPU transform. The
    synthetic output is detached while scaled; this is not a disabled desktop.
    All further changes use framebuffer size, independently of output state.
    """
    if type(scale) not in (int, float) or scale not in VIRTUAL_SCALES:
        return {"confirmed": False, "error": "請選擇支援的顯示比例。"}
    if not virtual_display_server():
        return {"confirmed": False, "error": "這個顯示器不是可復原的虛擬顯示器。"}
    before = display_snapshot()
    outputs, maximum = before["outputs"], before.get("maximum")
    if before["error"] or len(outputs) != 1 or not maximum:
        return {"confirmed": False, "error": "無法確認虛擬顯示器，未變更設定。"}
    output = outputs[0]
    mode = "x".join(map(str, maximum))
    if mode not in output.get("modes", []):
        return {"confirmed": False, "error": "顯示器未提供原始解析度，未變更設定。"}
    executable = shutil.which("xrandr")
    if not executable:
        return {"confirmed": False, "error": "目前無法使用顯示器設定工具。"}
    size = [round(side / scale) for side in maximum]
    frame = "x".join(map(str, size))
    command = ([executable, "--output", output["name"], "--mode", mode, "--pos", "0x0", "--fb", mode]
               if scale == 1 else [executable, "--output", output["name"], "--off", "--fb", frame])
    try:
        result = subprocess.run(command,
                                capture_output=True, text=True, timeout=5)
        after = display_snapshot()
    except (OSError, subprocess.TimeoutExpired):
        result = subprocess.CompletedProcess(command, -1)
        after = display_snapshot()
    active = next((item for item in after["outputs"] if item["name"] == output["name"]), {})
    confirmed = (result.returncode == 0 and not after["error"] and after["size"] == size
                 and (scale != 1 or (active.get("size") == maximum and active.get("position") == [0, 0]
                                     and active.get("scale") == [1, 1])))
    # A failed setting must return to the last usable logical size, not leave
    # the output detached at a partially changed framebuffer size.
    rollback = None
    if not confirmed:
        previous = virtual_scale_value(before) or 1.0
        previous_size = before.get("size")
        if previous is not None and previous_size:
            recovery = ([executable, "--output", output["name"], "--mode", mode, "--pos", "0x0", "--fb", mode]
                        if previous == 1 else [executable, "--output", output["name"], "--off", "--fb", "x".join(map(str, previous_size))])
            try:
                reverted = subprocess.run(recovery, capture_output=True, text=True, timeout=5)
                rollback = reverted.returncode == 0 and display_snapshot().get("size") == previous_size
            except (OSError, subprocess.TimeoutExpired):
                rollback = False
    return {"confirmed": confirmed, "display": after, "scale": scale, "rollback_confirmed": rollback,
            "error": None if confirmed else "顯示比例未完成套用，已嘗試恢復先前尺寸。"}


def restore_virtual_display() -> dict:
    return apply_virtual_scale(1.0)


def display_snapshot() -> dict:
    executable = shutil.which("xrandr")
    if not executable:
        return {"size": None, "outputs": [], "error": "目前無法讀取顯示器資訊。"}
    try:
        result = subprocess.run([executable, "--current", "--verbose"], capture_output=True, text=True, timeout=3)
        if result.returncode:
            raise OSError("xrandr query failed")
        return parse_xrandr(result.stdout) | {"error": None}
    except (OSError, subprocess.TimeoutExpired):
        return {"size": None, "outputs": [], "error": "目前無法讀取顯示器資訊。"}


def read_os_release(path: Path = Path("/etc/os-release")) -> dict:
    values = {}
    try:
        for line in path.read_text().splitlines():
            if "=" not in line or line.startswith("#"):
                continue
            key, value = line.split("=", 1)
            parsed = shlex.split(value)
            if len(parsed) == 1:
                values[key] = parsed[0]
    except (OSError, ValueError):
        pass
    return values


def memory_snapshot(meminfo: Path = Path("/proc/meminfo"), cgroup: Path = Path("/sys/fs/cgroup")) -> dict:
    total = available = None
    scope = "system"
    try:
        values = {key: int(value) * 1024 for key, value in
                  re.findall(r"^(MemTotal|MemAvailable):\s+(\d+)\s+kB", meminfo.read_text(), re.MULTILINE)}
        total, available = values.get("MemTotal"), values.get("MemAvailable")
    except (OSError, ValueError):
        pass
    try:
        limit = int((cgroup / "memory.max").read_text().strip())
        used = int((cgroup / "memory.current").read_text().strip())
        if limit > 0 and (total is None or limit < total):
            total, available, scope = limit, max(0, limit - used), "environment"
    except (OSError, ValueError):
        pass
    return {"total": total, "available": available, "scope": scope}


def system_snapshot() -> dict:
    try:
        cpu_count = len(os.sched_getaffinity(0))
    except (AttributeError, OSError):
        cpu_count = os.cpu_count()
    cpu_limit = None
    try:
        quota, period = Path("/sys/fs/cgroup/cpu.max").read_text().split()
        if quota != "max" and int(period) > 0:
            cpu_limit = int(quota) / int(period)
    except (OSError, ValueError):
        pass
    try:
        usage = shutil.disk_usage(Path.home())
        disk = {"total": usage.total, "free": usage.free}
    except OSError:
        disk = {"total": None, "free": None}
    return {"os": read_os_release().get("PRETTY_NAME"), "kernel": platform.release(),
            "architecture": platform.machine(), "cpu_count": cpu_count,
            "cpu_limit": cpu_limit, "memory": memory_snapshot(), "home_disk": disk}


def available_tools() -> list[dict]:
    return available_settings_tools()


def launch_tool(tool_id: str, socket_id: int | None = None):
    if tool_id == "display" and virtual_display_server():
        raise ValueError("虛擬顯示器請使用 Meadow 顯示器設定。")
    tool = next((item for item in available_tools() if item["id"] == tool_id), None)
    if not tool:
        raise ValueError("這項設定工具目前無法使用。")
    argv = list(tool["argv"])
    if socket_id is not None:
        if not tool["embed"] or not isinstance(socket_id, int) or socket_id <= 0:
            raise ValueError("這項設定不支援在系統設定內開啟。")
        argv.append(f"--socket-id={socket_id}")
    environment = os.environ.copy()
    environment["MEADOW_UI_ROLE"] = "settings"
    return subprocess.Popen(argv, env=environment, stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                            start_new_session=True)


def theme_helper(prefix: Path) -> Path:
    installed = prefix / "bin/meadow-theme"
    return installed if installed.is_file() and os.access(installed, os.X_OK) else PAYLOAD / "bin/meadow-theme"


def apply_appearance(theme: str, prefix: Path) -> dict:
    if theme not in {"light", "dark"}:
        raise ValueError("無效的外觀選項。")
    try:
        # The helper writes and confirms every native component. A loaded VM
        # can exceed 20s even while each bounded preference request succeeds.
        result = subprocess.run([str(theme_helper(prefix)), "--set", theme], capture_output=True, text=True, timeout=90)
        error = (result.stderr.strip() or f"外觀設定工具未完成（代碼 {result.returncode}）。") if result.returncode else None
        returncode = result.returncode
    except (OSError, subprocess.TimeoutExpired) as exc:
        error = str(exc) or "目前無法執行外觀設定工具。"
        returncode = None
    state = appearance_snapshot()
    receipt = state.get("receipt") or {}
    confirmed = (state["theme"] == theme and receipt.get("theme") == theme
                 and receipt.get("appearance_applied") is True)
    return {"appearance": state, "confirmed": confirmed, "error": error, "returncode": returncode}


def bytes_label(value: int | None) -> str:
    if value is None:
        return "無法讀取"
    for suffix in ("B", "KiB", "MiB", "GiB", "TiB", "PiB"):
        if value < 1024 or suffix == "PiB":
            return f"{value:.1f} {suffix}"
        value /= 1024
    raise AssertionError("unreachable")


def snapshot() -> dict:
    try:
        applications, inventory_error = installed_applications(), None
    except (ImportError, ValueError, RuntimeError) as exc:
        applications, inventory_error = [], str(exc)
    return {"appearance": appearance_snapshot(), "display": display_snapshot(),
            "system": system_snapshot(), "tools": available_tools(),
            "applications": applications, "inventory_error": inventory_error}


def settings_application_id(display: str) -> str:
    # Multiple development desktops may share a session bus. A display owns one
    # Settings window, independently of the launcher path or image revision.
    canonical = re.sub(r"\.[0-9]+$", "", display)
    digest = hashlib.sha256(canonical.encode()).hexdigest()[:16]
    return "ai.meadow.Settings.d" + digest


def main() -> int:
    parser = argparse.ArgumentParser(description="Meadow 原生系統設定")
    parser.add_argument("--prefix", type=Path, default=Path("/opt/meadow"))
    parser.add_argument("--status-json", action="store_true", help="Read actual settings without starting GTK")
    parser.add_argument("--page", choices=("general", "appearance", "display", "keyboard", "mouse", "accessibility", "windows", "workspaces", "startup", "filetypes", "color", "applications", "advanced"), default="general")
    args = parser.parse_args()
    if args.status_json:
        print(json.dumps(snapshot(), ensure_ascii=False, indent=2))
        return 0
    try:
        import gi
        gi.require_version("Gtk", "3.0")
        gi.require_version("Gdk", "3.0")
        from gi.repository import Gdk, Gio, GLib, Gtk, Pango
        from meadow_window_chrome import create_hover_chrome
        from glass.gtk_surface import GlassSurface
    except (ImportError, ValueError) as exc:
        print(f"Meadow Settings requires GTK3/PyGObject: {exc}", file=sys.stderr)
        return 2
    GLib.set_prgname("meadow-settings")
    ok, _ = Gtk.init_check()
    if not ok:
        print("Meadow Settings requires a graphical desktop session.", file=sys.stderr)
        return 2

    application = Gio.Application(application_id=settings_application_id(os.environ.get("DISPLAY", "")),
                                  flags=Gio.ApplicationFlags.FLAGS_NONE)
    window = None
    requested_page = [args.page]

    def present_settings(_action, parameter):
        page = parameter.get_string() if parameter is not None else ""
        if window is None:
            if page:
                requested_page[0] = page
            return
        if page in window.navigation_rows:
            window.navigation.select_row(window.navigation_rows[page])
        window.show()
        window.deiconify()
        window.present_with_time(Gtk.get_current_event_time())

    show_action = Gio.SimpleAction.new("show-page", GLib.VariantType.new("s"))
    show_action.connect("activate", present_settings)
    application.add_action(show_action)
    try:
        application.register(None)
    except GLib.Error as exc:
        print(f"Meadow Settings cannot register its desktop instance: {exc}", file=sys.stderr)
        return 2
    if application.get_is_remote():
        # A normal Dock click preserves the current page. An explicit --page
        # request still navigates the existing window to the requested panel.
        explicit_page = any(arg == "--page" or arg.startswith("--page=") for arg in sys.argv[1:])
        application.activate_action("show-page", GLib.Variant("s", args.page if explicit_page else ""))
        application.get_dbus_connection().flush_sync(None)
        return 0
    application.hold()

    class Settings(Gtk.Window):
        def __init__(self):
            super().__init__(title="Meadow 設定")
            self.virtual_display = virtual_display_server()
            self.set_wmclass("meadow-settings", "meadow-settings")
            self.set_default_size(1020, 740)
            self.set_size_request(720, 420) if self.virtual_display else self.set_size_request(880, 640)
            self.set_position(Gtk.WindowPosition.CENTER)
            self.set_icon_name("preferences-system")
            self.get_style_context().add_class("meadow-settings")
            self.connect("destroy", self.closed)
            self.alive, self.busy, self.updating = True, False, False
            self.read_generation = 0
            self.data, self.values, self.navigation_rows = {}, {}, {}
            self.panel_process = self.panel_socket = self.panel_tool = None
            self.panel_generation = 0
            self.panel_retry = False
            self.display_previous_scale = None
            self.display_inflight_previous = None
            self.display_revert_timer = None
            self.display_seconds = 0
            self.display_busy = False
            if self.virtual_display:
                Gdk.Screen.get_default().connect("size-changed", lambda *_: GLib.idle_add(self.fit_virtual_display))
            self.css = Gtk.CssProvider()
            self.css.load_from_data(b"""
                .meadow-settings, .meadow-settings:backdrop { color: @theme_fg_color; }
                .meadow-settings label { font-size: 14px; }
                .meadow-settings .settings-title { font-size: 22px; font-weight: 600; }
                .meadow-settings .settings-secondary, .meadow-settings .settings-secondary:backdrop { font-size: 12px; opacity: .78; }
                .meadow-settings .settings-section { font-weight: 600; }
                .meadow-settings .settings-sidebar, .meadow-settings .settings-sidebar list { background: transparent; border: 0; }
                .meadow-settings .settings-body-surface { background: transparent; }
                .meadow-settings .settings-tool { padding: 10px 12px; border: 0; border-radius: 10px; background: transparent; box-shadow: none; }
                .meadow-settings .settings-tool:hover { background: alpha(@theme_fg_color,.045); }
                .meadow-settings .settings-feedback { font-size: 12px; }
                .meadow-settings .settings-native-socket { background: transparent; }
            """)
            Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), self.css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
            root = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL)
            sidebar = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
            sidebar.get_style_context().add_class("settings-sidebar")
            sidebar.set_size_request(214, -1)
            self.navigation_search = Gtk.SearchEntry(placeholder_text="搜尋設定")
            self.navigation_search.set_margin_start(12)
            self.navigation_search.set_margin_end(12)
            self.navigation_search.set_margin_top(16)
            self.navigation_search.connect("search-changed", self.filter_navigation)
            sidebar.pack_start(self.navigation_search, False, False, 0)
            self.navigation = Gtk.ListBox(selection_mode=Gtk.SelectionMode.SINGLE)
            self.navigation.get_style_context().add_class("sidebar")
            self.navigation.set_margin_start(12)
            self.navigation.set_margin_end(12)
            self.navigation.set_margin_bottom(12)
            nav_scroll = Gtk.ScrolledWindow()
            nav_scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            nav_scroll.add(self.navigation)
            sidebar.pack_start(nav_scroll, True, True, 0)
            self.glass_sidebar = GlassSurface(self, mode="sidebar", corner_radius=0)
            self.glass_sidebar.add(sidebar)
            root.pack_start(self.glass_sidebar, False, False, 0)
            body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=14)
            body.get_style_context().add_class("settings-body")
            for side, size in (("start", 24), ("end", 24), ("top", 24), ("bottom", 16)):
                getattr(body, "set_margin_" + side)(size)
            self.glass_content = GlassSurface(self, mode="content", corner_radius=0)
            self.glass_content.get_style_context().add_class("settings-body-surface")
            self.glass_content.add(body)
            root.pack_start(self.glass_content, True, True, 0)
            self.stack = Gtk.Stack(transition_type=Gtk.StackTransitionType.NONE)
            if self.virtual_display:
                self.stack.set_homogeneous(False)
            self.stack.set_vexpand(True)
            body.pack_start(self.stack, True, True, 0)
            footer = Gtk.Box(spacing=8)
            self.feedback = self.text("", "settings-feedback", wrap=True)
            footer.pack_start(self.feedback, True, True, 0)
            refresh = Gtk.Button.new_from_icon_name("view-refresh-symbolic", Gtk.IconSize.BUTTON)
            refresh.set_tooltip_text("重新讀取設定")
            refresh.get_accessible().set_name("重新讀取設定")
            refresh.connect("clicked", lambda *_: self.refresh())
            footer.pack_end(refresh, False, False, 0)
            body.pack_end(footer, False, False, 0)
            navigation = (
                ("general", "一般", "preferences-system-symbolic"),
                ("appearance", "外觀", "preferences-desktop-appearance-symbolic"),
                ("display", "顯示器", "preferences-desktop-display-symbolic"),
                ("windows", "視窗", "preferences-system-windows-symbolic"),
                ("workspaces", "工作區", "preferences-desktop-workspaces-symbolic"),
                ("keyboard", "鍵盤", "input-keyboard-symbolic"),
                ("mouse", "滑鼠與觸控板", "input-mouse-symbolic"),
                ("accessibility", "輔助使用", "preferences-desktop-accessibility-symbolic"),
                ("startup", "登入項目", "preferences-system-session-symbolic"),
                ("filetypes", "預設應用程式", "preferences-system-symbolic"),
                ("color", "色彩描述檔", "preferences-desktop-display-symbolic"),
                ("applications", "應用程式", "view-app-grid-symbolic"),
                ("advanced", "進階設定", "preferences-system-symbolic"),
            )
            self.native_pages = {key for key, *_ in navigation} - {"general", "appearance", "applications", "advanced"}
            if self.virtual_display:
                self.native_pages.discard("display")
            for key, title, icon in navigation:
                row = Gtk.ListBoxRow()
                row.key, row.search_text = key, (title + " " + key).casefold()
                row.get_style_context().add_class("settings-nav")
                line = Gtk.Box(spacing=10)
                line.pack_start(self.image(icon, 16), False, False, 0)
                line.pack_start(self.text(title), True, True, 0)
                row.add(line)
                self.navigation.add(row)
                self.navigation_rows[key] = row
                if key not in self.native_pages:
                    page = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
                    page.pack_start(self.text(title, "settings-title"), False, False, 0)
                    if key == "display":
                        scroll = Gtk.ScrolledWindow()
                        scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
                        scroll.add(page)
                        self.stack.add_named(scroll, key)
                    else:
                        self.stack.add_named(page, key)
                    getattr(self, "build_" + key)(page)
            self.panel_page = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=14)
            self.panel_title = self.text("", "settings-title")
            self.panel_page.pack_start(self.panel_title, False, False, 0)
            self.panel_description = self.text("", "settings-secondary", True)
            self.panel_page.pack_start(self.panel_description, False, False, 0)
            self.panel_host = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
            self.panel_host.set_vexpand(True)
            self.panel_page.pack_start(self.panel_host, True, True, 0)
            self.stack.add_named(self.panel_page, "native-panel")
            self.navigation.connect("row-selected", lambda _list, row: self.show_page(row.key) if row else None)
            self.chrome = create_hover_chrome(self, root, title="設定", focus_content=self.navigation.grab_focus)
            self.add(self.chrome)
            self.show_all()
            self.navigation.select_row(self.navigation_rows[args.page])
            self.refresh()
            if self.virtual_display:
                GLib.idle_add(self.fit_virtual_display)

        def fit_virtual_display(self):
            if not self.alive:
                return False
            screen = Gdk.Screen.get_default()
            maximum_w, maximum_h = max(720, screen.get_width() - 48), max(420, screen.get_height() - 80)
            width, height = self.get_size()
            width, height = min(width, maximum_w), min(height, maximum_h)
            self.resize(width, height)
            x, y = self.get_position()
            self.move(max(24, min(x, screen.get_width() - width - 24)),
                      max(40, min(y, screen.get_height() - height - 40)))
            return False

        def filter_navigation(self, *_args):
            query = self.navigation_search.get_text().strip().casefold()
            for row in self.navigation_rows.values():
                row.set_visible(query in row.search_text)

        def show_page(self, key):
            if key in self.native_pages:
                self.open_tool(key)
            else:
                if key == "display":
                    self.stop_panel()
                self.stack.set_visible_child_name(key)
                self.feedback.set_text("")
                if key == "display":
                    self.refresh()

        @staticmethod
        def text(value, css=None, wrap=False):
            label = Gtk.Label(label=value, xalign=0)
            if css:
                label.get_style_context().add_class(css)
            label.set_line_wrap(wrap)
            return label

        @staticmethod
        def image(name, size):
            theme = Gtk.IconTheme.get_default()
            selected = name if theme.has_icon(name) else "preferences-system-symbolic"
            image = Gtk.Image.new_from_icon_name(selected, Gtk.IconSize.BUTTON)
            image.set_pixel_size(size)
            return image

        def value(self, page, key, title):
            row = Gtk.Box(spacing=16)
            caption = self.text(title, "settings-secondary")
            caption.set_size_request(106, -1)
            row.pack_start(caption, False, False, 0)
            value = self.text("正在讀取…", wrap=True)
            value.set_selectable(True)
            row.pack_start(value, True, True, 0)
            self.values[key] = value
            page.pack_start(row, False, False, 0)

        def build_appearance(self, page):
            page.pack_start(self.text("選擇適合你的桌面明暗。", "settings-secondary"), False, False, 0)
            choices = Gtk.Box(spacing=8)
            self.theme_buttons = {}
            for key, title in (("light", "淺色"), ("dark", "深色")):
                button = Gtk.ToggleButton(label=title)
                button.set_size_request(110, 36)
                button.connect("clicked", lambda _button, selected=key: self.choose_theme(selected))
                choices.pack_start(button, False, False, 0)
                self.theme_buttons[key] = button
            page.pack_start(choices, False, False, 0)
            page.pack_start(Gtk.Separator(), False, False, 4)
            self.value(page, "appearance", "目前外觀")
            self.value(page, "icons", "圖示")
            self.value(page, "background", "桌面背景")
            button = Gtk.Button(label="字型與進階外觀…")
            button.set_halign(Gtk.Align.START)
            button.connect("clicked", lambda *_: self.open_tool("appearance"))
            page.pack_start(button, False, False, 4)

        def build_general(self, page):
            page.pack_start(self.text("管理這部電腦與原生系統偏好。", "settings-secondary", True), False, False, 0)
            for key, title in (("os", "作業系統"), ("kernel", "核心版本"), ("architecture", "處理器架構"),
                               ("cpu", "處理器"), ("memory", "記憶體"), ("disk", "主目錄磁碟"), ("desktop", "桌面解析度")):
                self.value(page, key, title)
            page.pack_start(Gtk.Separator(), False, False, 4)
            page.pack_start(self.text("鍵盤、滑鼠、輔助使用與其他偏好，皆可從左側選擇。", "settings-secondary", True), False, False, 0)

        def build_display(self, page):
            page.pack_start(self.text("遠端桌面", "settings-section"), False, False, 0)
            choices = Gtk.Box(spacing=12)
            choices.pack_start(self.text("Scale 顯示比例"), True, True, 0)
            scale_control = Gtk.Box(spacing=8)
            self.display_scale_value = self.text("1×")
            self.display_scale_value.set_width_chars(5)
            self.display_scale_value.set_xalign(1)
            self.display_scale_value.get_accessible().set_name("目前顯示比例")
            scale_control.pack_start(self.display_scale_value, False, False, 4)
            self.display_minus = Gtk.Button(label="−")
            self.display_plus = Gtk.Button(label="＋")
            for button, delta, title in ((self.display_minus, -.25, "縮小顯示比例"),
                                         (self.display_plus, .25, "放大顯示比例")):
                button.set_size_request(36, 34)
                button.get_accessible().set_name(title)
                button.set_tooltip_text(title + " 0.25×")
                button.connect("clicked", lambda _button, step=delta: self.choose_display_scale(step))
                scale_control.pack_start(button, False, False, 0)
            choices.pack_end(scale_control, False, False, 0)
            page.pack_start(choices, False, False, 0)
            self.display_confirmation = Gtk.Box(spacing=12)
            self.display_confirmation.set_no_show_all(True)
            self.display_countdown = self.text("", "settings-secondary", True)
            self.display_confirmation.pack_start(self.display_countdown, True, True, 0)
            keep = Gtk.Button(label="保留變更")
            keep.connect("clicked", lambda *_: self.keep_display_scale())
            self.display_confirmation.pack_start(keep, False, False, 0)
            revert = Gtk.Button(label="還原")
            revert.connect("clicked", lambda *_: self.revert_display_scale())
            self.display_confirmation.pack_start(revert, False, False, 0)
            page.pack_start(self.display_confirmation, False, False, 0)
            self.value(page, "virtual-desktop", "目前解析度")
            self.value(page, "virtual-output", "顯示狀態")
            page.pack_start(Gtk.Separator(), False, False, 4)
            page.pack_start(self.text("調整虛擬桌面的邏輯尺寸，連線畫面會自動配合視窗。1.5× 會放大介面，保留較少的工作空間。",
                                      "settings-secondary", True), False, False, 0)
            page.pack_start(self.text("如果畫面縮小、跳掉或無法操作，可恢復顯示器原始尺寸。",
                                      "settings-secondary", True), False, False, 0)
            self.display_restore = Gtk.Button(label="恢復原始顯示尺寸")
            self.display_restore.set_halign(Gtk.Align.START)
            self.display_restore.connect("clicked", self.restore_display)
            page.pack_start(self.display_restore, False, False, 4)

        def restore_display(self, *_args):
            self.keep_display_scale()
            self.run_display_scale(1.0, confirm=False)

        def choose_display_scale(self, delta):
            if self.display_busy:
                return
            current = virtual_scale_value(display_snapshot()) or 1.0
            selected = round(current + delta, 2)
            if selected not in VIRTUAL_SCALES:
                return
            previous = self.display_previous_scale
            self.keep_display_scale()
            self.run_display_scale(selected, confirm=True, previous=previous)

        def run_display_scale(self, scale, *, confirm, previous=None):
            if self.display_busy:
                return
            self.display_busy = True
            if previous is None:
                previous = virtual_scale_value(display_snapshot()) or 1.0
            self.display_inflight_previous = previous if confirm else None
            self.display_restore.set_sensitive(False)
            self.display_minus.set_sensitive(False)
            self.display_plus.set_sensitive(False)
            self.feedback.set_text("正在調整顯示比例…")
            def worker():
                result = apply_virtual_scale(scale)
                if not self.alive:
                    if confirm and previous is not None:
                        apply_virtual_scale(previous)
                    return
                GLib.idle_add(self.display_restored, result, previous if confirm else None)
            # Normal window close must let an in-flight display mutation finish
            # and observe alive=False, then restore before this process exits.
            threading.Thread(target=worker, daemon=False, name="meadow-display-restore").start()

        def display_restored(self, result, previous):
            self.display_inflight_previous = None
            if not self.alive:
                if result["confirmed"] and previous is not None:
                    apply_virtual_scale(previous)
                return False
            self.display_busy = False
            self.display_restore.set_sensitive(True)
            self.feedback.set_text("顯示比例已套用。" if result["confirmed"] else result["error"])
            if result["confirmed"] and previous is not None and previous != result["scale"]:
                self.display_previous_scale = previous
                self.display_seconds = 15
                self.display_countdown.set_text("15 秒後自動還原；請確認是否保留。")
                self.display_confirmation.set_no_show_all(False)
                self.display_confirmation.show_all()
                self.display_confirmation.set_no_show_all(True)
                self.display_revert_timer = GLib.timeout_add_seconds(1, self.display_countdown_tick)
            self.refresh()
            return False

        def keep_display_scale(self):
            if self.display_revert_timer:
                GLib.source_remove(self.display_revert_timer)
            self.display_revert_timer = None
            self.display_previous_scale = None
            self.display_confirmation.hide()

        def revert_display_scale(self):
            previous = self.display_previous_scale
            self.keep_display_scale()
            if previous is not None:
                self.run_display_scale(previous, confirm=False)

        def display_countdown_tick(self):
            self.display_seconds -= 1
            if self.display_seconds <= 0:
                self.display_revert_timer = None
                self.revert_display_scale()
                return False
            self.display_countdown.set_text(f"{self.display_seconds} 秒後自動還原；請確認是否保留。")
            return True

        def build_advanced(self, page):
            page.pack_start(self.text("集中管理原生系統工具與進階選項。", "settings-secondary", True), False, False, 0)
            scroll = Gtk.ScrolledWindow()
            scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            self.tools_box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
            scroll.add(self.tools_box)
            page.pack_start(scroll, True, True, 0)

        def build_applications(self, page):
            self.application_count = self.text("正在讀取已安裝項目…", "settings-secondary", True)
            page.pack_start(self.application_count, False, False, 0)
            self.application_search = Gtk.SearchEntry(placeholder_text="搜尋名稱、系統工具或 desktop ID")
            self.application_search.connect("search-changed", lambda *_: self.render_applications())
            page.pack_start(self.application_search, False, False, 0)
            scroll = Gtk.ScrolledWindow()
            scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            self.applications_box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=4)
            scroll.add(self.applications_box)
            page.pack_start(scroll, True, True, 0)

        def render_applications(self):
            for child in self.applications_box.get_children():
                self.applications_box.remove(child)
            records = self.data.get("applications", [])
            ordinary = sum(item["launcher_visible"] and not item["settings_page"] for item in records)
            centralized = sum(bool(item["settings_page"]) for item in records)
            self.application_count.set_text(f"{len(records)} 個已安裝項目 · {ordinary} 個應用程式 · {centralized} 個設定與系統工具（含系統輔助項目）")
            query = self.application_search.get_text().strip().casefold()
            for item in sorted(records, key=lambda item: (bool(item["settings_page"]), item["title"].casefold())):
                if query not in " ".join(str(item.get(key) or "") for key in ("title", "name", "desktop_id", "categories")).casefold():
                    continue
                row = Gtk.Box(spacing=12)
                row.set_margin_top(6)
                row.set_margin_bottom(6)
                row.pack_start(self.image(item["icon"] or "preferences-system-symbolic", 28), False, False, 0)
                words = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=3)
                words.pack_start(self.text(item["title"]), False, False, 0)
                details = item["desktop_id"] + (" · 系統輔助項目" if item["nodisplay"] else "")
                subtitle = self.text(details, "settings-secondary")
                subtitle.set_ellipsize(Pango.EllipsizeMode.END)
                subtitle.set_tooltip_text((item["commandline"] or "") + "\n" + item["categories"])
                words.pack_start(subtitle, False, False, 0)
                row.pack_start(words, True, True, 0)
                destination = item["settings_page"]
                button = Gtk.Button(label="設定" if destination else "開啟")
                button.set_valign(Gtk.Align.CENTER)
                button.set_sensitive(not item["hidden"] and not item["nodisplay"])
                button.connect("clicked", lambda _button, selected=item: self.open_application(selected))
                row.pack_end(button, False, False, 0)
                self.applications_box.pack_start(row, False, False, 0)
            if self.data.get("inventory_error"):
                self.applications_box.pack_start(self.text("目前無法完整讀取應用程式清單。", "settings-secondary", True), False, False, 0)
            self.applications_box.show_all()

        def open_application(self, item):
            page = item["settings_page"]
            # Known embeddable panels use their real contents in this window.
            executable = Path(item["executable"] or "").name
            tool = next((tool for tool in available_tools() if Path(tool["executable"]).name == executable), None) if page else None
            if tool:
                self.open_tool(tool["id"])
            elif page and item["desktop_id"].casefold() in {"meadow-settings.desktop", "meadow-welcome.desktop", "xfce-settings-manager.desktop", "xfce4-settings-manager.desktop", "org.xfce.settingsmanager.desktop", "xfce4-about.desktop"}:
                self.navigation.select_row(self.navigation_rows[page])
            else:
                try:
                    app = Gio.DesktopAppInfo.new_from_filename(item["filename"])
                    if not app:
                        raise ValueError("missing installed desktop entry")
                    context = Gio.AppLaunchContext()
                    if page:
                        context.setenv("MEADOW_UI_ROLE", "settings")
                    app.launch([], context)
                    self.feedback.set_text("已送出開啟要求。")
                except (GLib.Error, OSError, ValueError):
                    self.feedback.set_text("目前無法開啟這個已安裝項目。")

        def closed(self, *_args):
            self.alive = False
            previous = (self.display_previous_scale if self.display_previous_scale is not None
                        else self.display_inflight_previous)
            if self.virtual_display and previous is not None:
                self.keep_display_scale()
                apply_virtual_scale(previous)
            self.stop_panel()
            Gtk.main_quit()

        def refresh(self):
            self.read_generation += 1
            generation = self.read_generation
            def worker():
                data = snapshot()
                GLib.idle_add(self.update, data, generation)
            threading.Thread(target=worker, daemon=True, name="meadow-settings-read").start()

        def update(self, data, generation):
            if not self.alive or generation != self.read_generation:
                return False
            self.data, self.updating = data, True
            appearance = data["appearance"]
            selected = appearance["theme"]
            native_settings = Gtk.Settings.get_default()
            effective_theme = native_settings.get_property("gtk-theme-name")
            if effective_theme != appearance["native_theme"]:
                selected = None
            titles = {"light": "淺色", "dark": "深色"}
            self.values["appearance"].set_text(titles.get(selected, "其他或尚未同步的外觀"))
            effective_icons = native_settings.get_property("gtk-icon-theme-name")
            icons = effective_icons or "無法讀取"
            if appearance["icon_theme"] and effective_icons != appearance["icon_theme"]:
                icons += "（偏好尚未同步）"
            self.values["icons"].set_text(icons)
            receipt = appearance.get("receipt") or {}
            self.values["background"].set_text("由 Meadow 桌面管理" if receipt.get("wallpaper_owner") == "shell" else "使用目前桌面背景")
            for theme, button in self.theme_buttons.items():
                button.set_active(selected == theme)
                button.set_sensitive(not self.busy)
            self.updating = False
            display = data["display"]
            self.values["desktop"].set_text(" × ".join(map(str, display["size"])) if display["size"] else "無法讀取")
            if self.virtual_display:
                self.values["virtual-desktop"].set_text(" × ".join(map(str, display["size"])) if display["size"] else "無法讀取")
                scale = virtual_scale_value(display)
                self.values["virtual-output"].set_text(f"{scale:g}× 邏輯桌面" if scale else "非標準尺寸，可恢復原始顯示尺寸")
                if scale is not None:
                    self.display_scale_value.set_text(f"{scale:g}×")
                self.display_minus.set_sensitive(not self.display_busy and scale is not None and scale > VIRTUAL_SCALES[0])
                self.display_plus.set_sensitive(not self.display_busy and scale is not None and scale < VIRTUAL_SCALES[-1])
            system = data["system"]
            for key in ("os", "kernel", "architecture"):
                self.values[key].set_text(system[key] or "無法讀取")
            count, limit = system["cpu_count"], system["cpu_limit"]
            self.values["cpu"].set_text(f"{limit:g} 個核心（目前環境限制）" if limit and count and limit < count else (f"{count} 個可見核心" if count else "無法讀取"))
            memory, disk = system["memory"], system["home_disk"]
            scope = "（目前環境）" if memory["scope"] == "environment" else ""
            self.values["memory"].set_text(f"{bytes_label(memory['available'])} 可用／{bytes_label(memory['total'])}{scope}" if memory["total"] is not None else "無法讀取")
            self.values["disk"].set_text(f"{bytes_label(disk['free'])} 可用／{bytes_label(disk['total'])}" if disk["total"] is not None else "無法讀取")
            tools = data["tools"]
            for child in self.tools_box.get_children():
                self.tools_box.remove(child)
            for tool in tools:
                if tool["id"] not in {"editor", "window-tweaks", "appearance", "files", "terminal", "notifications"}:
                    continue
                button = Gtk.Button()
                button.get_style_context().add_class("settings-tool")
                line = Gtk.Box(spacing=12)
                line.pack_start(self.image(tool["icon"], 20), False, False, 0)
                words = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
                words.pack_start(self.text(tool["title"]), False, False, 0)
                words.pack_start(self.text(tool["description"], "settings-secondary"), False, False, 0)
                line.pack_start(words, True, True, 0)
                line.pack_end(self.image("go-next-symbolic", 14), False, False, 0)
                button.add(line)
                button.connect("clicked", lambda _button, key=tool["id"]: self.open_tool(key))
                self.tools_box.pack_start(button, False, False, 0)
            if not tools:
                self.tools_box.pack_start(self.text("目前沒有可用的進階設定工具。", "settings-secondary", True), False, False, 0)
            # Every additional installed Settings entry keeps an accessible
            # launch route here, including third-party panels without XEmbed.
            known_commands = {Path(tool["executable"]).name for tool in tools}
            for item in data.get("applications", []):
                if item["settings_page"] != "advanced" or Path(item["executable"] or "").name in known_commands:
                    continue
                button = Gtk.Button()
                button.get_style_context().add_class("settings-tool")
                line = Gtk.Box(spacing=12)
                line.pack_start(self.image(item["icon"] or "preferences-system-symbolic", 20), False, False, 0)
                words = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
                words.pack_start(self.text(item["title"]), False, False, 0)
                description = {"panel-preferences.desktop": "面板的項目、排列與顯示方式",
                               "picom.desktop": "桌面合成與視窗效果",
                               "x11vnc.desktop": "遠端桌面連線與共享"}.get(item["desktop_id"].casefold(), "管理此工具的系統偏好")
                words.pack_start(self.text(description, "settings-secondary"), False, False, 0)
                line.pack_start(words, True, True, 0)
                line.pack_end(self.image("go-next-symbolic", 14), False, False, 0)
                button.add(line)
                button.connect("clicked", lambda _button, selected=item: self.open_application(selected))
                self.tools_box.pack_start(button, False, False, 0)
            self.tools_box.show_all()
            self.render_applications()
            return False

        def panel_receipt(self, status):
            try:
                path = state_root() / "settings-panel.json"
                path.parent.mkdir(parents=True, exist_ok=True)
                temporary = path.with_suffix(".tmp")
                temporary.write_text(json.dumps({"tool": self.panel_tool, "status": status,
                    "pid": self.panel_process.pid if self.panel_process else None,
                    "socket_id": self.panel_socket.get_id() if self.panel_socket and self.panel_socket.get_realized() else None,
                    "plug_attached": bool(self.panel_socket and self.panel_socket.get_plug_window())}, ensure_ascii=False))
                temporary.replace(path)
            except (OSError, RuntimeError):
                pass

        def stop_panel(self):
            self.panel_generation += 1
            process, self.panel_process = self.panel_process, None
            if process and process.poll() is None:
                # Only the child launched for our socket belongs to this window.
                try:
                    process.terminate()
                except ProcessLookupError:
                    pass
                threading.Thread(target=process.wait, daemon=True, name="meadow-settings-panel-reap").start()
            self.panel_socket = None
            for child in self.panel_host.get_children():
                self.panel_host.remove(child)
                child.destroy()

        def open_tool(self, tool_id):
            if tool_id == "display" and self.virtual_display:
                self.navigation.select_row(self.navigation_rows["display"])
                self.show_page("display")
                return
            tool = next((tool for tool in available_tools() if tool["id"] == tool_id), None)
            if not tool:
                self.feedback.set_text("這項設定工具目前尚未安裝。")
                return
            if not tool["embed"]:
                try:
                    launch_tool(tool_id)
                    self.feedback.set_text("已開啟原生應用程式偏好設定。")
                except (OSError, ValueError):
                    self.feedback.set_text("目前無法開啟這項設定工具。")
                return
            self.stack.set_visible_child_name("native-panel")
            if self.panel_tool == tool_id and self.panel_process and self.panel_process.poll() is None and not self.panel_retry:
                return
            self.stop_panel()
            self.panel_tool = tool_id
            self.panel_retry = False
            self.panel_title.set_text(tool["title"])
            self.panel_description.set_text(tool["description"])
            self.panel_socket = Gtk.Socket()
            self.panel_socket.get_style_context().add_class("settings-native-socket")
            self.panel_socket.set_hexpand(True)
            self.panel_socket.set_vexpand(True)
            self.panel_socket.connect("plug-added", lambda *_: self.panel_connected())
            self.panel_socket.connect("plug-removed", lambda *_: self.panel_removed())
            self.panel_host.pack_start(self.panel_socket, True, True, 0)
            self.panel_host.show_all()
            self.panel_socket.realize()
            self.feedback.set_text("正在載入設定…")
            try:
                self.panel_process = launch_tool(tool_id, int(self.panel_socket.get_id()))
                self.panel_receipt("starting")
                generation = self.panel_generation
                GLib.timeout_add(300, self.check_panel, generation, 0)
            except (OSError, ValueError, RuntimeError):
                self.feedback.set_text("目前無法載入這項原生設定。請重新選擇後再試。")
                self.panel_receipt("failed")

        def panel_connected(self):
            self.panel_retry = False
            self.feedback.set_text("")
            self.panel_receipt("embedded")

        def panel_removed(self):
            if self.alive:
                self.panel_retry = True
                self.feedback.set_text("設定面板已關閉；重新選擇即可開啟。")
                self.panel_receipt("closed")
            return True

        def check_panel(self, generation, attempt):
            if not self.alive or generation != self.panel_generation or not self.panel_process:
                return False
            if self.panel_process.poll() is not None:
                self.feedback.set_text("原生設定面板未完成載入，請重新選擇後再試。")
                self.panel_receipt("exited")
                return False
            if self.panel_socket and self.panel_socket.get_plug_window():
                return False
            if attempt >= 33:
                self.panel_retry = True
                self.feedback.set_text("原生設定載入時間較長；可重新選擇這個項目重試。")
                self.panel_receipt("timeout")
                return False
            GLib.timeout_add(300, self.check_panel, generation, attempt + 1)
            return False

        def choose_theme(self, theme):
            if self.updating or self.busy:
                return
            self.busy = True
            for button in self.theme_buttons.values():
                button.set_sensitive(False)
            self.feedback.set_text("正在套用外觀…")
            def worker():
                result = apply_appearance(theme, args.prefix)
                GLib.idle_add(self.theme_finished, theme, result)
            threading.Thread(target=worker, daemon=True, name="meadow-settings-appearance").start()

        def theme_finished(self, theme, result):
            if not self.alive:
                return False
            self.busy = False
            actual = Gtk.Settings.get_default().get_property("gtk-theme-name")
            expected = "Meadow-Light" if theme == "light" else "Meadow-Dark"
            if result["error"]:
                self.feedback.set_text("外觀未完整套用，請稍後再試。")
            elif result["confirmed"] and actual == expected:
                self.feedback.set_text("外觀已套用。")
            elif result["confirmed"]:
                self.feedback.set_text("外觀偏好已儲存，部分介面尚未同步。")
            else:
                self.feedback.set_text("無法確認外觀已套用，請稍後再試。")
            self.feedback.set_tooltip_text(result["error"])
            self.refresh()
            return False

    window = Settings()
    if requested_page[0] in window.navigation_rows and requested_page[0] != args.page:
        window.navigation.select_row(window.navigation_rows[requested_page[0]])
    try:
        Gtk.main()
    finally:
        application.release()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
