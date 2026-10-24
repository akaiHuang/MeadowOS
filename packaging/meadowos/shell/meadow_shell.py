#!/usr/bin/python3
"""Meadow's X11 desktop, native panels and presentation-only companion.

xfwm4 remains the window manager. External apps remain independent processes;
this process neither owns their lifetime nor the background Meadow Agent.
"""
from __future__ import annotations

import argparse
import ctypes
import datetime
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import uuid

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT.parent / "desktop"))


from application_catalog import APPLICATION_PRESENTATIONS, application_presentation, launcher_entries
from chat_geometry import ChatGeometry, bounded_geometry
from chat_markup import markdown_to_pango
from chat_stream import AssistantMessages
from text_editing import TextEditingHistory
from work_notifications import FAILED, REPLIED, ToolProgress, WorkNotifications, wake_snapshot_events


GLASS_APP_ICONS = frozenset(path.stem for path in (ROOT.parent / "desktop/icons/Meadow/scalable/apps").glob("*.svg"))
SHELL_THEME_NAMES = {"Meadow-Light": "light", "Meadow-Dark": "dark"}


def dock_width(monitor_width, preferred_width):
    """The native layout and first-paint receipt must agree on one width."""
    return min(monitor_width - 40, max(400, preferred_width))


def state_directory() -> Path:
    return Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "meadow-os"


def atomic_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=".shell-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(value, stream, ensure_ascii=False)
            stream.write("\n")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def spawn(argv: list[str]) -> None:
    # The supervisor can recover the Shell without terminating these apps.
    subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                     stderr=subprocess.DEVNULL, start_new_session=True)


def failed_tool_text(update: dict) -> str | None:
    """Present only an observed native failure; never infer a task outcome."""
    if update.get("sessionUpdate") != "tool_call_update" or update.get("status") != "failed":
        return None
    title = str(update.get("title") or update.get("toolCallId") or "工具")
    return f"工具未完成：{title}"[:240]


class PresentationNotices:
    """Bounded cache of observed system notices, separate from Agent history."""
    def __init__(self, path: Path, chat_id: str):
        self.path, self.chat_id, self.rows = path, chat_id, []
        self.error = None
        try:
            if path.exists():
                if path.stat().st_size > 256 * 1024:
                    raise ValueError("notice cache is too large")
                saved = json.loads(path.read_text())
                if saved.get("version") == 1 and saved.get("chatId") == chat_id:
                    self.rows = [row for row in saved.get("notices", [])
                                 if isinstance(row, dict) and isinstance(row.get("text"), str)
                                 and isinstance(row.get("runId"), str) and isinstance(row.get("eventId"), int)
                                 and row.get("kind") in {"tool_failed", "empty_reply", "turn_failed", "task_completed",
                                     "connection_failed", "request_failed", "stream_failed", "model_unavailable"}][-40:]
        except (OSError, ValueError, AttributeError, TypeError):
            self.error = "先前的系統通知無法載入。"

    def add(self, event: dict, kind: str, text: str) -> dict | None:
        data = event.get("data") or {}
        update = data.get("update") or {}
        run_id = update.get("runId") or data.get("runId") or ""
        event_id = event.get("id", 0)
        if any((row["runId"], row["eventId"], row["kind"]) == (run_id, event_id, kind) for row in self.rows):
            return None
        row = {"runId": run_id, "eventId": event_id, "kind": kind, "text": text[:1200],
               "observedAt": datetime.datetime.now(datetime.timezone.utc).isoformat()}
        self.rows = (self.rows + [row])[-40:]
        try:
            atomic_json(self.path, {"version": 1, "chatId": self.chat_id, "notices": self.rows})
        except OSError:
            self.error = "系統通知尚未保存到磁碟。"
        return row


def main() -> int:
    parser = argparse.ArgumentParser(description="MeadowOS native X11 desktop Shell")
    parser.add_argument("--theme", choices=("dark", "light"))
    parser.add_argument("--prefix", type=Path, default=Path("/opt/meadow"))
    parser.add_argument("--wallpaper", type=Path)
    parser.add_argument("--show-chat", action="store_true", help="Explicitly open the companion at startup")
    parser.add_argument("--smoke", type=Path, metavar="PNG", help="Capture the real desktop, then quit only this Shell")
    parser.add_argument("--smoke-delay", type=float, default=2)
    args = parser.parse_args()
    try:
        import gi
        gi.require_version("Gtk", "3.0")
        gi.require_version("Gdk", "3.0")
        gi.require_version("GdkX11", "3.0")
        gi.require_version("Wnck", "3.0")
        import cairo
        from gi.repository import Gdk, GdkPixbuf, GdkX11, Gio, GLib, Gtk, Pango, Wnck
        from chat_preedit import ChatPreedit
        from agent_client import AgentClient, AgentError
        from meadow_window_chrome import create_hover_chrome
        from glass.gtk_surface import GlassSurface
    except (ImportError, ValueError) as exc:
        print(f"Meadow Shell needs GTK3, GdkX11, Wnck3, Cairo and its Agent client: {exc}", file=sys.stderr)
        return 2
    ok, _ = Gtk.init_check()
    if not ok or not os.environ.get("DISPLAY"):
        print("Meadow Shell requires its own graphical X11 session.", file=sys.stderr)
        return 2
    display = Gdk.Display.get_default()
    if "X11" not in type(display).__name__:
        print("This Meadow Shell MVP supports X11 only.", file=sys.stderr)
        return 2
    GLib.set_prgname("meadow-shell")
    GLib.set_application_name("MeadowOS")

    def label(text="", css=None, wrap=False):
        widget = Gtk.Label(label=text, xalign=0)
        widget.set_line_wrap(wrap)
        widget.set_line_wrap_mode(Pango.WrapMode.WORD_CHAR)
        if css:
            widget.get_style_context().add_class(css)
        return widget

    def image(name, size=24):
        widget = Gtk.Image.new_from_icon_name(name, Gtk.IconSize.DIALOG)
        widget.set_pixel_size(size)
        return widget

    def mark(size=24, variant="symbol"):
        return image("meadow", size)

    def icon_button(name, title, callback, size=20):
        button = Gtk.Button()
        button.add(image(name, size))
        button.set_tooltip_text(title)
        button.get_accessible().set_name(title)
        button.connect("clicked", callback)
        return button

    def composited_surface(window):
        """Use real alpha only on a composited RGBA display; no blur is implied."""
        screen = window.get_screen()
        visual = screen.get_rgba_visual()
        if visual:
            window.set_visual(visual)

        def changed(*_args):
            style = window.get_style_context()
            if visual and screen.is_composited():
                style.add_class("shell-composited")
            else:
                style.remove_class("shell-composited")
            window.queue_draw()

        screen.connect("composited-changed", changed)
        changed()

    class Struts:
        """Reserve real WM workareas; no simulated window layout."""
        def __init__(self):
            self.lib = ctypes.CDLL("libX11.so.6")
            self.lib.XOpenDisplay.argtypes = [ctypes.c_char_p]
            self.lib.XOpenDisplay.restype = ctypes.c_void_p
            self.lib.XInternAtom.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int]
            self.lib.XInternAtom.restype = ctypes.c_ulong
            self.lib.XChangeProperty.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_ulong,
                ctypes.c_ulong, ctypes.c_int, ctypes.c_int, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_int]
            self.lib.XFlush.argtypes = [ctypes.c_void_p]
            self.lib.XCloseDisplay.argtypes = [ctypes.c_void_p]
            self.connection = self.lib.XOpenDisplay(None)
            if not self.connection:
                raise RuntimeError("Cannot open this X11 display for panel workareas")

        def set(self, window, *, top=0, bottom=0):
            drawable = window.get_window()
            if not drawable:
                return
            xid = drawable.get_xid()
            cardinal = self.lib.XInternAtom(self.connection, b"CARDINAL", 0)
            rect = shell.geometry
            # EWMH distances are measured from the root, not the monitor origin.
            root_height = Gdk.get_default_root_window().get_height()
            values = [0, 0, rect.y + top if top else 0,
                      root_height - rect.y - rect.height + bottom if bottom else 0,
                      0, 0, 0, 0, rect.x, rect.x + rect.width - 1,
                      rect.x, rect.x + rect.width - 1]
            for name, data in ((b"_NET_WM_STRUT_PARTIAL", values), (b"_NET_WM_STRUT", values[:4])):
                array = (ctypes.c_ulong * len(data))(*data)
                self.lib.XChangeProperty(self.connection, xid,
                    self.lib.XInternAtom(self.connection, name, 0), cardinal, 32, 0,
                    ctypes.cast(array, ctypes.POINTER(ctypes.c_ubyte)), len(data))
            self.lib.XFlush(self.connection)

        def close(self):
            if self.connection:
                self.lib.XCloseDisplay(self.connection)
                self.connection = None

    class Surface(Gtk.Window):
        def __init__(self, title, hint, css, focus=False):
            super().__init__(title=title)
            self.set_decorated(False)
            self.set_type_hint(hint)
            self.set_skip_taskbar_hint(True)
            self.set_skip_pager_hint(True)
            self.set_accept_focus(focus)
            self.set_focus_on_map(False)
            self.stick()
            self.get_style_context().add_class("shell-surface")
            self.get_style_context().add_class(css)
            if hint != Gdk.WindowTypeHint.DESKTOP:
                composited_surface(self)

    class Wallpaper(Surface):
        def __init__(self, owner):
            super().__init__("MeadowOS Desktop", Gdk.WindowTypeHint.DESKTOP, "shell-wallpaper")
            self.set_keep_below(True)
            self.area = Gtk.DrawingArea()
            self.area.connect("draw", self.draw)
            self.add(self.area)
            self.reload(owner.theme)

        def reload(self, theme):
            self.picture = self.scaled_picture = self.scaled_size = None
            candidates = [args.wallpaper, ROOT / "assets" / f"wallpaper-{theme}.png", ROOT / "assets/wallpaper.png",
                ROOT.parent / "desktop/assets" / f"wallpaper-{theme}.svg"]
            for path in candidates:
                if path and path.is_file():
                    try:
                        self.picture = GdkPixbuf.Pixbuf.new_from_file(str(path))
                        break
                    except GLib.Error:
                        pass
            self.area.queue_draw()

        def draw(self, _area, context):
            rect = shell.geometry
            if self.picture:
                scale = max(rect.width / self.picture.get_width(), rect.height / self.picture.get_height())
                width, height = round(self.picture.get_width() * scale), round(self.picture.get_height() * scale)
                if self.scaled_size != (width, height):
                    self.scaled_picture = self.picture.scale_simple(width, height, GdkPixbuf.InterpType.BILINEAR)
                    self.scaled_size = (width, height)
                Gdk.cairo_set_source_pixbuf(context, self.scaled_picture, (rect.width - width) / 2, (rect.height - height) / 2)
                context.paint()
            else:
                context.set_source_rgb(*( (0.957, 0.941, 0.906) if shell.theme == "light" else (0.035, 0.067, 0.055)))
                context.paint()
            return False

    class Launcher(Gtk.Window):
        def __init__(self, owner):
            super().__init__(title="MeadowOS · 應用程式")
            self.owner = owner
            self.initially_placed = False
            self.normal_geometry = None
            self.restore_geometry = None
            self.remapped_maximized = False
            self.geometry_timer = None
            self.set_default_size(660, 550)
            self.set_skip_taskbar_hint(True)
            self.set_icon_name("view-app-grid-symbolic")
            self.get_style_context().add_class("shell-launcher")
            self.connect("delete-event", self.hide_launcher)
            self.connect("configure-event", self.configure_geometry)
            self.connect("window-state-event", self.window_state)
            root = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
            root.set_border_width(18)
            self.search = Gtk.SearchEntry(placeholder_text="搜尋應用程式")
            self.search.connect("search-changed", self.filter_apps)
            root.pack_start(self.search, False, False, 0)
            self.sections = []
            content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=22)
            for title in ("應用程式",):
                section = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
                section.set_no_show_all(True)
                heading = label(title, "launcher-section-heading")
                section.pack_start(heading, False, False, 0)
                flow = Gtk.FlowBox()
                flow.set_selection_mode(Gtk.SelectionMode.NONE)
                flow.set_homogeneous(True)
                flow.set_max_children_per_line(6)
                flow.set_min_children_per_line(4)
                flow.set_row_spacing(8)
                flow.set_column_spacing(4)
                flow.set_valign(Gtk.Align.START)
                flow.set_filter_func(lambda child: self.search.get_text().strip().casefold() in child.search_text)
                section.pack_start(flow, False, False, 0)
                content.pack_start(section, False, False, 0)
                heading.show()
                flow.show()
                self.sections.append((section, flow))
            self.flow = self.sections[0][1]
            scroller = Gtk.ScrolledWindow()
            scroller.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            scroller.add(content)
            root.pack_start(scroller, True, True, 0)
            self.glass_content = GlassSurface(self, mode="content", corner_radius=0)
            self.glass_content.add(root)
            self.chrome = create_hover_chrome(self, self.glass_content, title="應用程式",
                close=self.hide_launcher, focus_content=self.search.grab_focus,
                before_move=self.capture_normal_geometry)
            self.add(self.chrome)
            self.refresh()

        def refresh(self):
            for _section, flow in self.sections:
                for child in flow.get_children():
                    flow.remove(child)
            entries = [(app, self.owner.presentation(app)) for app in launcher_entries(self.owner.apps)]
            for app, presentation in sorted(entries, key=lambda entry: (entry[1]["system"], entry[1]["order"], entry[1]["title"].casefold())):
                button = Gtk.Button()
                button.get_accessible().set_name(presentation["title"])
                content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
                app_image = self.owner.app_icon(app, 64)
                content.pack_start(app_image, False, False, 0)
                title = label(presentation["title"], "app-name", True)
                title.set_justify(Gtk.Justification.CENTER)
                title.set_lines(2)
                title.set_ellipsize(Pango.EllipsizeMode.END)
                title.set_max_width_chars(10)
                title.set_halign(Gtk.Align.CENTER)
                content.pack_start(title, False, False, 0)
                button.add(content)
                button.set_tooltip_text(presentation["title"] if presentation["title"] == app.get_display_name() else presentation["title"] + "\n" + app.get_display_name())
                button.connect("clicked", lambda _button, selected=app: self.open_app(selected))
                child = Gtk.FlowBoxChild()
                child.app_name = app.get_display_name().casefold()
                child.search_text = presentation["search"]
                child.add(button)
                self.flow.add(child)
            for _section, flow in self.sections:
                flow.show_all()
            self.filter_apps()

        def filter_apps(self, *_args):
            query = self.search.get_text().strip().casefold()
            for section, flow in self.sections:
                flow.invalidate_filter()
                section.set_visible(any(query in child.search_text for child in flow.get_children()))

        def open_app(self, app):
            shell.launch_app(app)
            self.hide_launcher()

        def hide_launcher(self, *_args):
            self.capture_normal_geometry()
            window = self.get_window()
            self.restore_geometry = self.normal_geometry if window and window.get_state() & Gdk.WindowState.MAXIMIZED else None
            self.remapped_maximized = False
            self.chrome.dismiss(focus_input=True)
            self.hide()
            return True

        def configure_geometry(self, *_args):
            if self.geometry_timer:
                GLib.source_remove(self.geometry_timer)
            self.geometry_timer = GLib.timeout_add(120, self.capture_normal_geometry)
            return False

        def capture_normal_geometry(self):
            self.geometry_timer = None
            window = self.get_window()
            special = window and window.get_state() & (Gdk.WindowState.MAXIMIZED | Gdk.WindowState.ICONIFIED | Gdk.WindowState.FULLSCREEN)
            if self.get_mapped() and not special and self.restore_geometry is None:
                self.normal_geometry = (tuple(self.get_position()), tuple(self.get_size()))
            return False

        def window_state(self, _window, event):
            if self.restore_geometry and self.get_visible():
                if event.new_window_state & Gdk.WindowState.MAXIMIZED:
                    self.remapped_maximized = True
                elif self.remapped_maximized and not event.new_window_state & Gdk.WindowState.ICONIFIED:
                    normal = self.restore_geometry
                    self.restore_geometry = None
                    self.remapped_maximized = False
                    GLib.idle_add(self.restore_normal_geometry, *normal)
            return False

        def restore_normal_geometry(self, position, size):
            window = self.get_window()
            if self.get_visible() and window and not window.get_state() & (Gdk.WindowState.MAXIMIZED | Gdk.WindowState.ICONIFIED):
                self.resize(*size)
                self.move(*position)
                self.normal_geometry = (position, size)
            return False

        def toggle(self):
            window = self.get_window()
            if window and window.get_state() & Gdk.WindowState.ICONIFIED:
                self.deiconify()
                self.present_with_time(Gtk.get_current_event_time())
                self.search.grab_focus()
                return
            if self.get_visible():
                self.hide_launcher()
            else:
                if not self.initially_placed:
                    self.move(shell.geometry.x + 24, shell.geometry.y + 60)
                    self.initially_placed = True
                self.show_all()
                self.present_with_time(Gtk.get_current_event_time())
                self.search.grab_focus()

    class MessageLabel(Gtk.Label):
        """Native wrapped text that hugs short messages and caps long bubbles."""
        def __init__(self, text, limit, *, render_markdown=False):
            self.width_limit = limit
            self.render_markdown = render_markdown
            super().__init__(xalign=0)
            self.set_message_text(text)
            self.set_line_wrap(True)
            self.set_line_wrap_mode(Pango.WrapMode.WORD_CHAR)
            self.set_selectable(True)
            self.get_style_context().add_class("chat-message")

        def set_message_text(self, text):
            self.raw_text = text
            if self.render_markdown:
                self.set_markup(markdown_to_pango(text, trim_trailing=True))
            else:
                self.set_text(text)

        def do_get_preferred_width(self):
            minimum, natural = Gtk.Label.do_get_preferred_width(self)
            limit = getattr(self, "width_limit", 360)
            return min(minimum, limit), min(natural, limit)

        def set_width_limit(self, limit):
            if limit != self.width_limit:
                self.width_limit = limit
                self.queue_resize()

    class Companion(Gtk.Window):
        def __init__(self):
            super().__init__(title="Meadow · AI 工作夥伴")
            self.set_decorated(False)
            self.set_default_size(520, 550)
            self.set_size_request(360, 420)
            self.set_skip_taskbar_hint(True)
            self.get_style_context().add_class("shell-chat")
            self.get_style_context().add_class("chat-variant-c")
            composited_surface(self)
            self.connect("delete-event", self.hide_chat)
            self.client = None
            self.generation = 0
            self.connected = False
            self.connecting = False
            self.model_ready = False
            self.image_support = None
            self.running = False
            self.sending = False
            self.assistant_label = None
            self.assistant_text = ""
            self.assistant_messages = AssistantMessages()
            self.assistant_bubbles = {}
            self.turn_had_reply = False
            self.current_run_id = None
            self.voice_replay_through = 0
            self.voice_reply_text = ""
            self.voice_unsent = []
            try:
                path = state_directory() / "voice-drafts.json"
                if path.is_file() and path.stat().st_size <= 1024 * 1024:
                    saved = json.loads(path.read_text())
                    self.voice_unsent = [item for item in saved.get("items", []) if
                        isinstance(item, dict) and isinstance(item.get("id"), str) and
                        isinstance(item.get("text"), str) and item.get("status") in {"unsubmitted", "uncertain"}]
            except (OSError, ValueError, AttributeError):
                pass
            self.notices = None
            self.work_notifications = None
            self.tool_progress = None
            self.last_position = None
            self.last_size = None
            self.iconified = False
            self.maximized = False
            self.reopen_maximized = False
            self.position_restore_pending = True
            self.voice_placement_pending = False
            self.geometry_timer = None
            self.maximized_hide_geometry = None
            self.remapped_maximized = False
            self.connect("map-event", self.map_chat)
            self.unread = 0
            self.draft_timer = None
            self.message_labels = []
            self.message_reflow_pending = None
            self.following_bottom = True
            self.follow_bottom_pending = None
            self.scrolling_to_bottom = False
            self.status_label = label("Agent 尚未連線", "chat-status")
            root = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
            identity = Gtk.Box(spacing=12)
            identity.get_style_context().add_class("chat-identity-row")
            identity.set_margin_start(20)
            identity.set_margin_end(16)
            identity.set_margin_top(30)
            identity.set_margin_bottom(12)
            identity.pack_start(mark(36, "avatar"), False, False, 0)
            identity_text = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=3)
            identity_text.pack_start(label("Meadow", "chat-name"), False, False, 0)
            status = Gtk.Box(spacing=5)
            self.status_dot = label("●", "chat-status-dot")
            status.pack_start(self.status_dot, False, False, 0)
            status.pack_start(self.status_label, False, False, 0)
            identity_text.pack_start(status, False, False, 0)
            identity.pack_start(identity_text, True, True, 0)
            menu = Gtk.MenuButton()
            menu.get_style_context().add_class("chat-more")
            menu.set_valign(Gtk.Align.CENTER)
            menu.set_tooltip_text("聊天選單")
            menu.get_accessible().set_name("聊天選單")
            menu.add(image("view-more-symbolic", 18))
            # A native popup window shares the OS menu material and keyboard
            # behavior. Its backdrop can be sampled independently of Chat.
            actions = Gtk.Menu()
            actions.get_accessible().set_name("聊天選單")
            self.retry = Gtk.MenuItem(label="重新連線")
            self.retry.get_accessible().set_name("重新連線")
            self.retry.connect("activate", lambda *_: self.connect_agent())
            actions.append(self.retry)
            for title, callback in (("工作檔案", lambda: shell.show_work_files()),
                                    ("待辦事項", lambda: shell.show_work_tasks()),
                                    ("工作紀錄", self.show_work_log)):
                item = Gtk.MenuItem(label=title)
                item.connect("activate", lambda _item, action=callback: action())
                actions.append(item)
            window_menu = Gtk.MenuItem(label="視窗控制（F10）")
            window_menu.connect("activate", lambda *_: self.chrome.toggle_controls())
            actions.append(window_menu)
            actions.show_all()
            menu.set_popup(actions)
            identity.pack_end(menu, False, False, 0)
            # C uses the shared 48px window header; no second identity panel.
            identity.remove(menu)
            identity_text.remove(status)
            status.remove(self.status_label)
            status.remove(self.status_dot)
            identity.destroy()
            self.header_mark = mark(32, "avatar")
            self.header_mark.set_tooltip_text("Meadow · AI 工作夥伴")
            self.timeline = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
            self.timeline.set_border_width(20)
            self.timeline.set_valign(Gtk.Align.END)
            self.timeline.set_vexpand(True)
            self.timeline.get_style_context().add_class("chat-timeline")
            self.timeline.connect("size-allocate", self.reflow_messages)
            self.empty = label("你的 AI 工作夥伴\n連線後可接續同一段工作對話。", "chat-empty", True)
            self.empty.set_no_show_all(True)
            self.empty.show()
            self.empty.set_margin_top(24)
            self.timeline.pack_start(self.empty, False, False, 0)
            self.scroll = Gtk.ScrolledWindow()
            self.scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            self.scroll.set_propagate_natural_height(False)
            self.scroll.add(self.timeline)
            root.pack_start(self.scroll, True, True, 0)
            adjustment = self.scroll.get_vadjustment()
            adjustment.connect("changed", self.timeline_extent_changed)
            adjustment.connect("value-changed", self.timeline_position_changed)
            self.latest = Gtk.Button(label="新回覆 ↓")
            self.latest.get_style_context().add_class("chat-latest")
            self.latest.set_halign(Gtk.Align.CENTER)
            self.latest.set_no_show_all(True)
            self.latest.connect("clicked", lambda *_: self.follow_bottom(force=True))
            root.pack_start(self.latest, False, False, 0)
            self.requests = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
            # Required questions and actionable errors must not become the
            # toplevel's minimum height. Keep the composer outside this bounded
            # region, while retaining every detail for internal scrolling.
            auxiliary = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
            auxiliary.pack_start(self.requests, False, False, 0)
            self.auxiliary_scroll = Gtk.ScrolledWindow()
            self.auxiliary_scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            self.auxiliary_scroll.set_min_content_height(0)
            self.auxiliary_scroll.set_max_content_height(180)
            self.auxiliary_scroll.set_propagate_natural_height(True)
            self.auxiliary_scroll.add(auxiliary)
            root.pack_start(self.auxiliary_scroll, False, False, 0)
            self.progress = Gtk.Expander()
            self.progress.get_style_context().add_class("chat-progress")
            self.progress.set_no_show_all(True)
            self.progress.set_margin_start(18)
            self.progress.set_margin_end(18)
            self.progress.set_tooltip_text("最近 40 筆工具與背景接續紀錄")
            progress_heading = Gtk.Box(spacing=8)
            self.progress_spinner = Gtk.Spinner()
            self.progress_spinner.set_no_show_all(True)
            progress_heading.pack_start(self.progress_spinner, False, False, 0)
            self.progress_summary = label("執行進度", "chat-progress-summary")
            self.progress_summary.set_ellipsize(Pango.EllipsizeMode.END)
            self.progress_summary.set_max_width_chars(38)
            progress_heading.pack_start(self.progress_summary, True, True, 0)
            self.progress.set_label_widget(progress_heading)
            self.progress_rows = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=5)
            progress_scroll = Gtk.ScrolledWindow()
            progress_scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            progress_scroll.set_min_content_height(48)
            progress_scroll.set_max_content_height(150)
            progress_scroll.set_propagate_natural_height(True)
            progress_scroll.add(self.progress_rows)
            self.progress.add(progress_scroll)
            self.pending = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=4)
            self.pending.set_no_show_all(True)
            self.pending.set_border_width(8)
            self.create_work_log()
            self.error = label("", "chat-error", True)
            self.error.set_no_show_all(True)
            self.error.connect("notify::label", lambda widget, _property: widget.set_visible(bool(widget.get_text())))
            self.error.set_margin_start(18)
            self.error.set_margin_end(18)
            self.error.set_margin_top(5)
            self.error.set_margin_bottom(5)
            auxiliary.pack_start(self.error, False, False, 0)
            composer = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
            composer.get_style_context().add_class("chat-composer")
            self.composer = composer
            self.input = Gtk.TextView(wrap_mode=Gtk.WrapMode.WORD_CHAR)
            self.input.set_left_margin(0)
            self.input.set_right_margin(0)
            self.input.set_top_margin(0)
            self.input.set_bottom_margin(0)
            self.input.get_accessible().set_name("訊息")
            self.input.set_tooltip_text("⌘/Ctrl+Enter 送出 · ⌘/Ctrl+Z 復原")
            self.input.connect("key-press-event", self.input_key)
            try:
                saved = json.loads((state_directory() / "shell-draft.json").read_text())
                if isinstance(saved, dict) and isinstance(saved.get("text"), str):
                    self.input.get_buffer().set_text(saved["text"])
            except (OSError, ValueError, TypeError):
                pass
            self.editing = TextEditingHistory(self.input, composition_active=lambda: bool(getattr(getattr(self, "ime_preedit", None), "active", False)))
            self.input.get_buffer().connect("changed", self.draft_changed)
            input_scroll = Gtk.ScrolledWindow()
            input_scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.EXTERNAL)
            input_scroll.set_min_content_height(26)
            input_scroll.set_max_content_height(84)
            input_scroll.set_propagate_natural_height(True)
            input_scroll.add(self.input)
            input_shell = Gtk.Overlay()
            input_shell.get_style_context().add_class("chat-input-shell")
            input_shell.add(input_scroll)
            self.placeholder = label("和 Meadow 說說話…", "chat-placeholder")
            self.placeholder.set_halign(Gtk.Align.START)
            self.placeholder.set_valign(Gtk.Align.CENTER)
            self.placeholder.set_margin_start(0)
            self.placeholder.set_no_show_all(True)
            input_shell.add_overlay(self.placeholder)
            input_shell.set_overlay_pass_through(self.placeholder, True)
            self.update_placeholder()
            from attachment_composer import AttachmentComposer
            self.attachments = AttachmentComposer(state_directory(), self.attachment_changed, self.attachment_error)
            composer.pack_start(self.attachments.widget, False, False, 0)
            composer.pack_start(input_shell, True, True, 0)
            composer_tools = Gtk.Box(spacing=8)
            add_images = icon_button("list-add-symbolic", "加入圖片", self.choose_attachments, 18)
            composer_tools.pack_start(add_images, False, False, 0)
            self.attachment_hint = label("⌘/Ctrl+Enter 送出", "chat-shortcut")
            composer_tools.pack_start(self.attachment_hint, True, True, 0)
            self.ime_preedit = ChatPreedit(self, self.input, self.placeholder, state_directory())
            self.stop = icon_button("media-playback-stop-symbolic", "停止任務", lambda *_: self.request(lambda client: client.cancel()), 16)
            self.stop.get_style_context().add_class("stop")
            self.stop.set_no_show_all(True)
            self.stop.set_sensitive(False)
            self.stop.set_valign(Gtk.Align.CENTER)
            composer_tools.pack_start(self.stop, False, False, 0)
            self.send = icon_button("go-up-symbolic", "送出", self.send_message, 18)
            self.send.get_style_context().add_class("send")
            self.send.set_valign(Gtk.Align.CENTER)
            self.send.set_sensitive(False)
            self.send.set_tooltip_text("送出 · ⌘/Ctrl+Enter")
            composer_tools.pack_start(self.send, False, False, 0)
            composer.pack_start(composer_tools, False, False, 0)
            root.pack_start(composer, False, False, 0)
            self.glass_content = GlassSurface(self, mode="content", corner_radius=0)
            self.glass_content.add(root)
            self.chrome = create_hover_chrome(self, self.glass_content, title="Meadow",
                close=self.hide_chat, minimize=self.minimize_chat,
                maximize=self.toggle_maximize, focus_content=self.input.grab_focus,
                before_move=self.remember_normal_geometry, allow_pin=True,
                title_widget=self.header_mark, trailing_widget=menu, toolbar_height=48)
            self.chrome.buttons[0].set_tooltip_text("收起聊天（工作繼續）")
            self.chrome.buttons[0].get_accessible().set_name("收起聊天（工作繼續）")
            self.add(self.chrome)
            self.set_focus(self.input)
            self.connect("window-state-event", self.window_state)
            self.connect("configure-event", self.configure_geometry)
            self.connect("key-press-event", self.window_key)
            self.geometry_guard = ChatGeometry(self, auxiliary_scroll=self.auxiliary_scroll)
            self.drag_dest_set(Gtk.DestDefaults.ALL,
                [Gtk.TargetEntry.new("text/uri-list", 0, 0)], Gdk.DragAction.COPY)
            self.connect("drag-data-received", self.receive_attachment_drop)
            self.connect("drag-motion", self.attachment_drag_motion)
            self.connect("drag-leave", self.attachment_drag_leave)
            from native_attachments import NativeAttachmentTransport
            self.attachment_transport = NativeAttachmentTransport(state_directory(), self.stage_browser_attachment)
            self.attachment_transport.before_poll = self.publish_attachment_target
            self.attachment_transport.start()
            self.connect("destroy", lambda *_: self.attachment_transport.close())

        def attachment_changed(self):
            if hasattr(self, "attachment_hint"):
                count = len(self.attachments.items)
                self.attachment_hint.set_text(f"{count} 張圖片 · ⌘/Ctrl+Enter 送出" if count else "⌘/Ctrl+Enter 送出")
                if count and getattr(self, "image_support", None) is False:
                    self.error.set_text("目前模型無法辨識圖片。請先移除圖片再傳送文字。")
                elif not count and self.error.get_text().startswith("目前模型無法辨識圖片"):
                    self.error.set_text("")

        def attachment_error(self, error):
            self.error.set_text("圖片尚未加入：" + str(error))

        def choose_attachments(self, *_args):
            dialog = Gtk.FileChooserDialog(title="加入圖片", transient_for=self,
                action=Gtk.FileChooserAction.OPEN)
            dialog.add_buttons("取消", Gtk.ResponseType.CANCEL, "加入", Gtk.ResponseType.ACCEPT)
            dialog.set_select_multiple(True)
            pictures = Gtk.FileFilter()
            pictures.set_name("圖片")
            for mime in ("image/png", "image/jpeg", "image/gif", "image/webp"):
                pictures.add_mime_type(mime)
            dialog.add_filter(pictures)
            if dialog.run() == Gtk.ResponseType.ACCEPT:
                self.attachments.enqueue(dialog.get_filenames())
            dialog.destroy()

        def receive_attachment_drop(self, _widget, context, _x, _y, data, _info, timestamp):
            from gi.repository import Gio
            paths = []
            for uri in data.get_uris() or []:
                path = Gio.File.new_for_uri(uri).get_path()
                if path:
                    paths.append(path)
            if paths:
                self.attachments.enqueue(paths)
            else:
                self.attachment_error("請拖入本機圖片檔案。")
            self.attachment_drag_leave()
            Gtk.drag_finish(context, bool(paths), False, timestamp)

        def attachment_drag_motion(self, *_args):
            self.composer.get_style_context().add_class("attachment-drop-active")
            return False

        def attachment_drag_leave(self, *_args):
            self.composer.get_style_context().remove_class("attachment-drop-active")

        def attachment_session_id(self):
            session = self.client.session if self.client and self.client.session else {}
            return session.get("chatId") or session.get("sessionId") or "inactive"

        def publish_attachment_target(self):
            x, y = self.get_position()
            width, height = self.get_size()
            self.attachment_transport.publish(active=bool(self.get_visible() and self.is_active() and self.connected),
                rect={"x": x, "y": y, "width": width, "height": height},
                session_id=self.attachment_session_id())

        def stage_browser_attachment(self, event, data):
            # Transport verifies its foreground epoch before invoking this worker.
            record = self.attachments.cache.add_bytes(data, event["name"], event["mime"])
            def append():
                if event["sessionId"] == self.attachment_session_id():
                    self.attachments.append(record)
                return False
            GLib.idle_add(append)
            return record

        def create_work_log(self):
            # Observed tool/queue/lifecycle facts remain inspectable without
            # becoming assistant messages or taking space from the composer.
            self.work_log = Gtk.Window(title="Meadow · 工作紀錄")
            self.work_log.set_transient_for(self)
            self.work_log.set_default_size(560, 550)
            self.work_log.set_skip_taskbar_hint(True)
            self.work_log.get_style_context().add_class("shell-chat")
            composited_surface(self.work_log)
            body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
            body.set_border_width(20)
            body.pack_start(label("工作紀錄", "chat-name"), False, False, 0)
            body.pack_start(label("工具進度、排隊訊息與系統紀錄會保留在這裡。", "chat-status", True), False, False, 0)
            body.pack_start(self.pending, False, False, 0)
            self.progress.set_expanded(True)
            body.pack_start(self.progress, False, False, 0)
            self.work_records = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
            body.pack_start(self.work_records, False, False, 0)
            scroll = Gtk.ScrolledWindow()
            scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            scroll.set_propagate_natural_height(False)
            scroll.add(body)
            glass = GlassSurface(self.work_log, mode="content", corner_radius=0)
            glass.add(scroll)
            chrome = create_hover_chrome(self.work_log, glass, title="工作紀錄",
                close=self.hide_work_log, allow_pin=True)
            self.work_log.add(chrome)
            self.work_log.connect("delete-event", self.hide_work_log)
            self.work_log_geometry = ChatGeometry(self.work_log)

        def hide_work_log(self, *_args):
            self.work_log.hide()
            return True

        def show_work_log(self):
            if shell.voiceos and shell.voiceos.expanded:
                shell.voiceos.collapse()
            self.work_log.show_all()
            self.work_log.deiconify()
            self.work_log.present_with_time(Gtk.get_current_event_time())

        def draft(self):
            buffer = self.input.get_buffer()
            return buffer.get_text(buffer.get_start_iter(), buffer.get_end_iter(), True)

        def draft_changed(self, _buffer):
            self.update_placeholder()
            if self.draft_timer:
                GLib.source_remove(self.draft_timer)
            self.draft_timer = GLib.timeout_add(400, self.save_draft)

        def update_placeholder(self):
            self.placeholder.set_visible(self.input.get_buffer().get_char_count() == 0)

        def save_draft(self):
            self.draft_timer = None
            try:
                atomic_json(state_directory() / "shell-draft.json", {"text": self.draft()})
            except OSError:
                self.error.set_text("草稿尚未保存到磁碟。")
            return False

        def hide_chat(self, *_args):
            self.voice_placement_pending = False
            return_to_voice = bool(shell.voiceos and shell.voiceos.enabled and shell.voiceos.expanded)
            if return_to_voice:
                shell.voice_focus_owned = False
            if shell.voiceos:
                shell.voiceos.release_companion(self)
            self.save_draft()
            self.remember_normal_geometry()
            self.reopen_maximized = self.maximized
            self.maximized_hide_geometry = (self.last_position, self.last_size) if self.maximized and self.last_position and self.last_size else None
            self.remapped_maximized = False
            self.position_restore_pending = True
            self.chrome.dismiss(focus_input=True)
            self.hide()
            if return_to_voice:
                GLib.idle_add(shell.voiceos.focus_overlay)
            return True

        def toggle(self):
            if shell.voiceos and shell.voiceos.expanded:
                shell.voice_show_chat(force=True)
                return
            if self.iconified:
                self.deiconify()
                self.present_with_time(Gtk.get_current_event_time())
                self.input.grab_focus()
                return
            if self.get_visible():
                self.hide_chat()
            else:
                self.show_all()
                self.unread = 0
                shell.update_chat_button()
                self.present_with_time(Gtk.get_current_event_time())
                self.input.grab_focus()

        def map_chat(self, *_args):
            # Red hide/show needs explicit placement because xfwm may center
            # a newly mapped window. Native iconify/workspace maps retain the
            # WM's own geometry and must not acquire a stale cached rectangle.
            if self.position_restore_pending:
                GLib.idle_add(self.restore_position)
            return False

        def restore_position(self):
            if self.voice_placement_pending:
                return self.place_voice_chat()
            # Keep the pending flag through the map-to-idle gap. GTK rendering
            # can delay this idle beyond the configure debounce; that earlier
            # configure belongs to the WM's provisional centered placement.
            self.position_restore_pending = False
            if self.reopen_maximized or self.maximized:
                self.maximize()
                self.input.grab_focus()
                return False
            if self.last_position is None:
                rect = shell.geometry
                # Gtk allocations include client-side shadow extents; the
                # logical window size excludes them and matches visible width.
                width, _height = self.get_size()
                self.last_position = (rect.x + max(24, rect.width - width - 24), rect.y + 56)
            if self.last_size is None:
                self.last_size = tuple(self.get_size())
            if self.last_size:
                self.resize(*self.last_size)
            self.move(*self.last_position)
            self.input.grab_focus()
            return False

        def show_for_voice(self):
            # A repeated request presents the same chat. Do not toggle it or
            # collapse the fullscreen voice surface behind it.
            self.voice_placement_pending = self.position_restore_pending = True
            self.reopen_maximized = False
            self.maximized_hide_geometry = None
            self.remapped_maximized = False
            self.unmaximize()
            self.deiconify()
            self.show_all()
            self.unread = 0
            shell.update_chat_button()
            GLib.idle_add(self.place_voice_chat)
            self.present_with_time(Gtk.get_current_event_time())
            self.input.grab_focus()

        def place_voice_chat(self):
            if not self.voice_placement_pending or not self.get_visible():
                return False
            native = self.get_window()
            # Wait for the WM's unmaximize/deiconify receipt, not an arbitrary
            # timer. map/state handlers will retry this pending placement.
            if native and native.get_state() & (Gdk.WindowState.MAXIMIZED | Gdk.WindowState.ICONIFIED):
                return False
            rect = shell.geometry
            size = self.last_size or (520, 550)
            position = (rect.x + rect.width - size[0] - 24, rect.y + 56)
            self.last_position, self.last_size = bounded_geometry(
                position, size, self.geometry_guard.current_area())
            self.resize(*self.last_size)
            self.move(*self.last_position)
            self.voice_placement_pending = self.position_restore_pending = False
            return False

        def remember_normal_geometry(self):
            if self.position_restore_pending:
                return
            window = self.get_window()
            native_special = window and window.get_state() & (Gdk.WindowState.MAXIMIZED | Gdk.WindowState.ICONIFIED | Gdk.WindowState.FULLSCREEN)
            if self.get_mapped() and not self.maximized and not self.iconified and not self.reopen_maximized and not native_special:
                self.last_position = tuple(self.get_position())
                self.last_size = tuple(self.get_size())

        def configure_geometry(self, *_args):
            # Configure/state event ordering is asynchronous. Debounce before
            # reading actual flags, so a maximize-sized configure is never
            # accepted as a normal rectangle before its state event arrives.
            if self.geometry_timer:
                GLib.source_remove(self.geometry_timer)
            self.geometry_timer = GLib.timeout_add(120, self.capture_geometry)
            return False

        def capture_geometry(self):
            self.geometry_timer = None
            self.remember_normal_geometry()
            return False

        def minimize_chat(self, *_args):
            self.voice_placement_pending = False
            return_to_voice = bool(shell.voiceos and shell.voiceos.enabled and shell.voiceos.expanded)
            if return_to_voice:
                shell.voice_focus_owned = False
            if shell.voiceos:
                shell.voiceos.release_companion(self)
            self.remember_normal_geometry()
            self.chrome.dismiss(focus_input=True)
            self.iconify()
            if return_to_voice:
                GLib.idle_add(shell.voiceos.focus_overlay)

        def toggle_maximize(self, *_args):
            if self.maximized:
                self.reopen_maximized = False
                self.unmaximize()
            else:
                self.remember_normal_geometry()
                self.maximize()

        def window_state(self, _window, event):
            was_maximized = self.maximized
            self.maximized = bool(event.new_window_state & Gdk.WindowState.MAXIMIZED)
            self.iconified = bool(event.new_window_state & Gdk.WindowState.ICONIFIED)
            if self.voice_placement_pending and not self.maximized and not self.iconified:
                GLib.idle_add(self.place_voice_chat)
            if self.maximized and self.maximized_hide_geometry and self.get_visible():
                self.remapped_maximized = True
            if was_maximized and not self.maximized and not self.iconified and self.get_visible():
                self.reopen_maximized = False
                # Let xfwm restore its confirmed normal rectangle, including
                # when maximize/move/resize came from a native WM shortcut.
                # Only red hide/remap can destroy that WM receipt. Repair that
                # exact cycle using its frozen normal rectangle after MAX ack.
                if self.remapped_maximized and self.maximized_hide_geometry:
                    normal = self.maximized_hide_geometry
                    self.maximized_hide_geometry = None
                    self.remapped_maximized = False
                    GLib.idle_add(self.restore_remapped_geometry, *normal)
            shell.update_windows()
            return False

        def restore_remapped_geometry(self, position, size):
            if self.get_visible() and not self.maximized and not self.iconified:
                self.last_position, self.last_size = position, size
                self.resize(*size)
                self.move(*position)
            return False

        def window_key(self, _window, event):
            if event.keyval == Gdk.KEY_F10:
                if event.state & Gdk.ModifierType.MOD1_MASK:
                    self.toggle_maximize()
                    return True
                if not event.state & (Gdk.ModifierType.CONTROL_MASK | Gdk.ModifierType.SHIFT_MASK | Gdk.ModifierType.SUPER_MASK):
                    self.chrome.reveal(keyboard=True)
                    return True
            if event.keyval == Gdk.KEY_Escape and self.chrome.revealer.get_reveal_child():
                self.chrome.dismiss(focus_input=True)
                return True
            return False

        def dispatch(self, generation, function, *values):
            def apply():
                if generation == self.generation and not shell.closing:
                    function(*values)
                return False
            GLib.idle_add(apply)

        def connect_agent(self):
            # Do not discard an unresolved submission callback or send twice.
            if self.sending or self.connecting:
                return
            self.connecting = True
            self.generation += 1
            generation = self.generation
            previous, self.client = self.client, None
            self.connected = self.model_ready = False
            self.status_label.set_text("正在連接 Agent…")
            self.retry.set_sensitive(False)
            self.update_controls()
            def worker():
                try:
                    if previous:
                        previous.close()
                    client = AgentClient(attachment_cache=self.attachments.cache)
                    client.connect()
                    client.open_conversation()
                    # SSE can advance client.last_event_id before GTK runs the
                    # queued accept callback. Freeze the snapshot cursor now.
                    replay_after = client.last_event_id
                    messages, status = client.load_messages(), client.status()
                    try:
                        status["presentation_image_support"] = client.image_capability()
                    except AgentError:
                        status["presentation_image_support"] = None
                    self.dispatch(generation, self.accept_connection, client, messages, status, replay_after)
                    for event in client.events():
                        self.dispatch(generation, self.on_event, event)
                except (AgentError, OSError, ValueError) as exc:
                    self.dispatch(generation, self.disconnected, str(exc))
            threading.Thread(target=worker, daemon=True, name="meadow-chat-events").start()

        def accept_connection(self, client, messages, status, replay_after):
            self.connecting = False
            self.client = client
            self.connected = bool(status.get("connected"))
            self.model_ready = bool(status.get("model", {}).get("ready"))
            self.image_support = status.get("presentation_image_support")
            self.attachment_changed()
            self.running = bool((status.get("session") or {}).get("running"))
            for child in self.timeline.get_children():
                if child is not self.empty:
                    self.timeline.remove(child)
            for child in self.work_records.get_children():
                self.work_records.remove(child)
            self.assistant_label = None
            self.assistant_text = ""
            self.assistant_messages = AssistantMessages(messages, replay_after=replay_after)
            self.assistant_bubbles = {}
            self.message_labels = []
            self.following_bottom = True
            self.current_run_id = None
            visible = [message for message in messages if isinstance(message.get("content"), str) and message["content"].strip()]
            self.turn_had_reply = bool(self.running and visible and visible[-1].get("role") == "assistant")
            # Pending native requests are rebuilt from this resume's replay.
            for child in self.requests.get_children():
                self.requests.remove(child)
            self.empty.show()
            for message in messages:
                self.append_message(message.get("role", ""), message.get("content", ""))
            session = status.get("session") or {}
            self.voice_replay_through = session.get("lastEventId", replay_after)
            self.voice_reply_text = ""
            chat_id = session.get("chatId") or session.get("sessionId") or ""
            self.notices = PresentationNotices(state_directory() / "shell-notices.json", chat_id)
            self.work_notifications = WorkNotifications(state_directory() / "shell-push.json",
                session.get("sessionId") or chat_id, atomic_json,
                replay_through=session.get("lastEventId", replay_after))
            self.tool_progress = ToolProgress(state_directory() / "shell-progress.json", chat_id,
                atomic_json, running=self.running, run_id=session.get("runId", ""))
            for notice in self.notices.rows:
                self.append_system_notice(notice, previous=True)
            # Resume owns these delivery facts. Restore visible repair state
            # without a synthetic user turn, speech or a repeated native push.
            for event in wake_snapshot_events(session):
                self.tool_progress.consume(event)
                for notice in self.work_notifications.consume(event, historical=True):
                    if notice.get("display"):
                        self.system_notice({"id": 0, "data": {"runId": notice["key"]}},
                            notice["kind"], notice["text"], failure=notice["kind"] == "turn_failed")
            self.render_progress()
            for item in self.voice_unsent:
                self.present_voice_draft(item)
            self.error.set_text(self.notices.error or "")
            if not self.model_ready:
                self.show_service_error("model_unavailable", str(status.get("model", {}).get("error") or "模型尚未就緒。"),
                    "模型尚未就緒；請在 Meadow Code 設定模型，或開啟「工作紀錄」查看詳細資訊。")
            # Resume may replay only pending requests; hydrate task history from
            # the authoritative snapshot independently, without historical push.
            task_event = {"id": 0, "type": "background_tasks", "data": status.get("tasks") or {}}
            for notice in self.work_notifications.consume(task_event, historical=True):
                if notice.get("display"):
                    self.system_notice({"id": 0, "data": {"runId": notice["key"]}},
                        notice["kind"], notice["text"], failure=notice["kind"] == "turn_failed")
            self.render_queue((status.get("queue") or {}).get("items", []))
            self.follow_bottom(force=True)
            shell.work_connection_changed()
            self.retry.set_sensitive(True)
            self.update_controls()

        def disconnected(self, message):
            self.connecting = False
            was_connected = self.connected
            self.connected = False
            if self.tool_progress:
                self.tool_progress.set_disconnected()
                self.render_progress()
            self.status_label.set_text("連線中斷 · 任務狀態未知" if was_connected else "Agent 尚未連線")
            summary = "連線中斷。可從聊天選單重新連線，或開啟「工作紀錄」查看詳細資訊。"
            self.show_service_error("connection_failed", message, summary)
            self.retry.set_sensitive(True)
            self.update_controls()
            shell.refresh_work_tasks()
            if shell.voiceos and shell.voiceos.enabled:
                shell.voice_event({"type": "error", "message": summary})

        def update_controls(self):
            self.retry.set_sensitive(not self.connecting and not self.sending)
            self.send.set_sensitive(self.connected and self.model_ready and not self.sending)
            self.stop.set_sensitive(self.connected and self.running)
            self.stop.set_visible(self.connected and self.running)
            if self.connected:
                self.status_label.set_text("正在工作" if self.running else ("可以開始對話" if self.model_ready else "Agent 已連線 · 模型尚未就緒"))
            self.header_mark.set_tooltip_text("Meadow · " + self.status_label.get_text())
            style = self.status_dot.get_style_context()
            if self.connected and self.model_ready:
                style.add_class("ready")
            else:
                style.remove_class("ready")
            shell.update_chat_button()

        def append_message(self, role, text):
            if not isinstance(text, str) or not text:
                return None
            self.empty.hide()
            row = Gtk.Box(spacing=10)
            row.get_style_context().add_class("chat-message-row")
            bubble = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
            bubble.get_style_context().add_class("chat-bubble")
            bubble.set_halign(Gtk.Align.END if role == "user" else Gtk.Align.START)
            if role == "assistant":
                bubble.get_style_context().add_class("assistant")
            elif role == "user":
                bubble.get_style_context().add_class("user")
            elif role:
                bubble.pack_start(label(role, "chat-role"), False, False, 0)
            message = MessageLabel(text, self.message_width_limit(role), render_markdown=role == "assistant")
            bubble.pack_start(message, False, False, 0)
            if role == "user":
                row.pack_end(bubble, False, False, 0)
            else:
                row.pack_start(bubble, False, False, 0)
            self.message_labels.append((role, message))
            self.timeline.pack_start(row, False, False, 0)
            row.show_all()
            self.follow_bottom()
            return message

        def message_width_limit(self, role, width=None):
            width = width or self.timeline.get_allocated_width()
            if width <= 1:
                width = self.get_size()[0]
            available = max(180, width - self.timeline.get_border_width() * 2)
            avatar = 0
            return max(100, min(520, int(available * .90) - avatar - 26))

        def reflow_messages(self, _widget, _allocation):
            # GTK may discard a child's resize request while its parent is
            # allocating. Recompute after allocation so both the label and
            # bubble invalidate their cached widths when a window shrinks.
            if self.message_reflow_pending is None:
                self.message_reflow_pending = GLib.idle_add(self.reflow_message_widths)

        def reflow_message_widths(self):
            self.message_reflow_pending = None
            for role, message in self.message_labels:
                message.set_width_limit(self.message_width_limit(role))
            return False

        def timeline_position_changed(self, adjustment):
            # User scrolling changes the follow preference, even if a stream
            # chunk has already queued a follow. Our own scroll is guarded.
            if self.scrolling_to_bottom:
                return
            self.following_bottom = adjustment.get_value() >= adjustment.get_upper() - adjustment.get_page_size() - 24
            if self.following_bottom:
                self.latest.hide()
            elif self.follow_bottom_pending is not None:
                self.latest.show()

        def timeline_extent_changed(self, _adjustment):
            # Wrapped labels can grow over several GTK allocation passes. Keep
            # following each confirmed extent, instead of scrolling once against
            # the old height and then treating the next chunk as reading history.
            if self.following_bottom:
                self.follow_bottom()

        def follow_bottom(self, force=False):
            if force:
                self.following_bottom = True
            if not self.following_bottom:
                self.latest.show()
                return
            self.latest.hide()
            if self.follow_bottom_pending is None:
                self.follow_bottom_pending = GLib.idle_add(self.scroll_to_bottom)

        def scroll_to_bottom(self):
            self.follow_bottom_pending = None
            if not self.following_bottom:
                return False
            adjustment = self.scroll.get_vadjustment()
            self.scrolling_to_bottom = True
            try:
                adjustment.set_value(max(0, adjustment.get_upper() - adjustment.get_page_size()))
            finally:
                self.scrolling_to_bottom = False
            return False

        def append_system_notice(self, notice, previous=False):
            # The only inline system row preserves an unsent user utterance.
            # All execution/turn facts belong to the explicit work log.
            inline = notice.get("kind") == "voice_draft"
            if inline:
                self.empty.hide()
            row = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
            row.get_style_context().add_class("chat-system-notice")
            if notice.get("kind") in {"tool_failed", "turn_failed"}:
                row.get_style_context().add_class("tool-failed")
            title = "未送出的語音" if inline else "先前工作紀錄" if previous else "工作紀錄"
            row.pack_start(label(title, "chat-role"), False, False, 0)
            text = label(notice["text"], "chat-system", True)
            text.set_selectable(True)
            text.set_max_width_chars(42)
            row.pack_start(text, False, False, 0)
            row.set_tooltip_text(notice.get("observedAt", ""))
            target = self.timeline if inline else self.work_records
            target.pack_start(row, False, False, 0)
            if not inline:
                for expired in target.get_children()[:-40]:
                    target.remove(expired)
            row.show_all()
            if inline:
                self.follow_bottom()

        def system_notice(self, event, kind, text, failure=True):
            notice = self.notices.add(event, kind, text) if self.notices else {"kind": kind, "text": text}
            if notice:
                self.append_system_notice(notice)
                # A failed intermediate tool can be recovered by the model.
                # Only an actual incomplete response needs a front-chat alert.
                if failure and kind in {"turn_failed", "empty_reply"}:
                    self.error.set_text("這次回覆未完成。可從聊天選單開啟「工作紀錄」查看詳細資訊。")
                    self.mark_unread()

        def input_key(self, _widget, event):
            modifiers = event.state & Gtk.accelerator_get_default_mod_mask()
            if event.keyval in (Gdk.KEY_Return, Gdk.KEY_KP_Enter) and modifiers == Gdk.ModifierType.CONTROL_MASK:
                if not self.editing.is_composing():
                    self.send_message()
                return True
            return False

        def show_service_error(self, kind, detail, summary, event=None):
            # Local transport observations have their own identity, separate
            # from real run IDs and assistant history. Keep diagnostic details
            # inspectable while the front alert explains the available action.
            event = event or {"id": 0, "data": {"runId": "presentation:" + str(uuid.uuid4())}}
            self.system_notice(event, kind, str(detail), failure=False)
            self.error.set_text(summary)
            self.mark_unread()

        def request(self, action, accepted=None, failed=None):
            client, generation = self.client, self.generation
            if not client or not self.connected:
                return
            def worker():
                try:
                    result = action(client)
                    if accepted:
                        self.dispatch(generation, accepted, result)
                except (AgentError, OSError, ValueError) as exc:
                    self.dispatch(generation, failed or self.request_error, str(exc))
            threading.Thread(target=worker, daemon=True, name="meadow-chat-request").start()

        def request_error(self, error):
            self.sending = False
            summary = "操作結果尚未確認。請先查看「工作紀錄」與排隊訊息，再決定是否重試。"
            self.show_service_error("request_failed", error, summary)
            self.update_controls()
            if shell.voiceos and shell.voiceos.enabled:
                shell.voice_event({"type": "error", "message": summary})

        def send_message(self, *_args):
            return self.submit_text(self.draft(), preserve_draft=False)

        def submit_text(self, text, preserve_draft=True):
            """Same durable queue admission for typed and final recognized text.

            A True result means a request was started, never a model success.
            Voice must not replace a typed draft or synthesize its own history.
            """
            if not preserve_draft and self.attachments.pending:
                self.error.set_text("圖片還在加入，請稍候再送出。")
                return False
            attachments = [] if preserve_draft else list(self.attachments.items)
            if attachments and getattr(self, "image_support", None) is False:
                self.error.set_text("目前模型無法辨識圖片。請先移除圖片再傳送文字。")
                return False
            if not isinstance(text, str) or (not text.strip() and not attachments):
                return False
            voice_draft = None
            if preserve_draft:
                voice_draft = {"id": str(uuid.uuid4()), "text": text, "status": "unsubmitted"}
                self.voice_unsent.append(voice_draft)
                self.save_voice_drafts()
            if not self.send.get_sensitive():
                if voice_draft:
                    self.present_voice_draft(voice_draft)
                return False
            request_id = None
            if attachments:
                try:
                    attachments, request_id = self.attachments.snapshot(text)
                except (OSError, ValueError) as exc:
                    self.error.set_text(str(exc))
                    return False
            if voice_draft:
                # Persist uncertainty before HTTP: a lost receipt may still
                # mean the original text entered the durable queue.
                voice_draft["status"] = "uncertain"
                self.save_voice_drafts()
            self.sending = True
            self.error.set_text("")
            self.update_controls()
            def accepted(_receipt):
                self.sending = False
                if voice_draft and voice_draft in self.voice_unsent:
                    self.voice_unsent.remove(voice_draft)
                    self.save_voice_drafts()
                if not preserve_draft and self.draft() == text:
                    self.input.get_buffer().set_text("")
                    self.save_draft()
                if not preserve_draft:
                    if attachments:
                        self.attachments.admitted({item["id"] for item in attachments})
                    self.editing.reset()
                # The server may broadcast a newer queue before this receipt.
                # Queue and chat history are rendered from native events only.
                self.follow_bottom(force=True)
                self.update_controls()
            def failed(error):
                if voice_draft:
                    voice_draft["status"] = "uncertain"
                    self.save_voice_drafts()
                    self.present_voice_draft(voice_draft)
                self.request_error(error)
            def attachment_progress(item):
                self.dispatch(self.generation, self.attachments.update, item)
            self.request(lambda client: client.send(text, attachments=attachments, request_id=request_id,
                on_attachment=attachment_progress), accepted, failed)
            return True

        def save_voice_drafts(self):
            try:
                atomic_json(state_directory() / "voice-drafts.json", {"version": 1, "items": self.voice_unsent})
            except OSError:
                self.error.set_text("未送出的語音文字尚未保存到磁碟。")

        def present_voice_draft(self, item):
            caption = ("語音送出狀態未確認，請先檢查排隊訊息再重送。" if item["status"] == "uncertain"
                       else "語音文字尚未送出。可複製下方原文，稍後再送出。")
            self.append_system_notice({"kind": "voice_draft", "text": caption + "\n\n" + item["text"]})

        def render_queue(self, items):
            for child in self.pending.get_children():
                self.pending.remove(child)
            queued = [item for item in items if item.get("status") == "pending" and not item.get("backgroundCompletion")]
            if queued:
                self.pending.pack_start(label(f"接著處理 · {len(queued)} 則", "chat-status"), False, False, 0)
                for item in queued[:2]:
                    title = label(item.get("text", ""), "chat-status")
                    title.set_ellipsize(Pango.EllipsizeMode.END)
                    title.set_max_width_chars(40)
                    self.pending.pack_start(title, False, False, 0)
                manage = Gtk.Button(label="在 Meadow Code 管理排隊訊息")
                manage.connect("clicked", lambda *_: shell.open_desktop())
                self.pending.pack_start(manage, False, False, 0)
            self.pending.show_all()
            self.pending.set_visible(bool(queued))

        def render_progress(self):
            if self.tool_progress is None:
                return
            state = self.tool_progress.view()
            self.progress_summary.set_text(state["summary"])
            self.progress.get_accessible().set_name("執行進度 · " + state["summary"])
            for child in self.progress_rows.get_children():
                self.progress_rows.remove(child)
            for item in state["rows"]:
                row = Gtk.Box(spacing=10)
                title = label(("先前 · " if item["previous"] else "") + item["title"], "chat-progress-title")
                title.set_ellipsize(Pango.EllipsizeMode.END)
                title.set_tooltip_text(item["title"])
                title.set_max_width_chars(32)
                row.pack_start(title, True, True, 0)
                status = label(item["statusLabel"], "chat-progress-status")
                if item["status"] in {"failed", "unknown", "blocked"}:
                    status.get_style_context().add_class("chat-progress-unresolved")
                row.pack_end(status, False, False, 0)
                self.progress_rows.pack_start(row, False, False, 0)
            self.progress.get_child().show_all()
            self.progress.get_label_widget().show_all()
            self.progress_spinner.set_visible(state["active"])
            self.progress_spinner.start() if state["active"] else self.progress_spinner.stop()
            self.progress.set_visible(state["visible"])

        def permission(self, data):
            self.replace_request(data.get("requestId"))
            box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
            box.get_style_context().add_class("chat-permission")
            call = data.get("toolCall") or {}
            box.pack_start(label(str(call.get("title") or "需要你的確認"), None, True), False, False, 0)
            details = Gtk.Expander(label="查看操作內容")
            raw = label(json.dumps(call.get("rawInput"), ensure_ascii=False, indent=2), None, True)
            raw.set_selectable(True)
            raw.set_max_width_chars(46)
            details.add(raw)
            box.pack_start(details, False, False, 0)
            choices = Gtk.Box(spacing=6)
            for option in data.get("options", []):
                button = Gtk.Button(label=str(option.get("name") or option.get("optionId")))
                button.connect("clicked", lambda _button, selected=option.get("optionId"): self.request(
                    lambda client: client.respond_permission(data["requestId"], selected), lambda _result: self.dismiss_request(box)))
                choices.pack_start(button, False, False, 0)
            box.pack_start(choices, False, False, 0)
            box.request_id = data.get("requestId")
            self.requests.pack_start(box, False, False, 0)
            self.requests.show_all()
            self.mark_unread()
            shell.voice_show_chat()

        def user_input(self, data):
            self.replace_request(data.get("requestId"))
            box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
            box.get_style_context().add_class("chat-permission")
            entries = []
            for question in data.get("questions", []):
                box.pack_start(label(str(question.get("question", "")), None, True), False, False, 0)
                entry = Gtk.Entry()
                entry.set_placeholder_text("輸入你的回答")
                entries.append(entry)
                options = question.get("options") or []
                if options:
                    box.pack_start(label("／".join(str(option.get("label", "")) for option in options), "chat-status", True), False, False, 0)
                box.pack_start(entry, False, False, 0)
            submit = Gtk.Button(label="回答")
            def submit_answers(_button):
                answers = [entry.get_text() for entry in entries]
                self.request(lambda client: client.respond_user_input(data["requestId"], answers),
                             lambda _result: self.dismiss_request(box))
            submit.connect("clicked", submit_answers)
            box.pack_start(submit, False, False, 0)
            box.request_id = data.get("requestId")
            self.requests.pack_start(box, False, False, 0)
            self.requests.show_all()
            self.mark_unread()
            shell.voice_show_chat()

        def dismiss_request(self, box):
            if box.get_parent() is self.requests:
                self.requests.remove(box)

        def replace_request(self, request_id):
            for child in self.requests.get_children():
                if child.request_id == request_id:
                    self.requests.remove(child)

        def mark_unread(self):
            if not self.get_visible():
                self.unread += 1
                shell.update_chat_button()

        def on_event(self, event):
            kind, data = event.get("type"), event.get("data", {})
            bubble_update = self.assistant_messages.consume(event)
            if self.tool_progress and self.tool_progress.consume(event):
                self.render_progress()
            fresh_voice_event = isinstance(event.get("id"), int) and event["id"] > self.voice_replay_through
            if self.work_notifications:
                observed = self.work_notifications.consume(event,
                    had_reply=self.turn_had_reply, foreground=self.is_active())
                for notice in observed:
                    if notice.get("display") and notice["kind"] != "reply":
                        self.system_notice(event, notice["kind"], notice["text"],
                            failure=notice["kind"] == "turn_failed")
                    if notice["push"]:
                        shell.notify("Meadow 有新回覆" if notice["kind"] == "reply" else
                            "Meadow 工作未完成" if notice["kind"] == "turn_failed" else "Meadow 背景工作已完成",
                            notice["text"])
            if kind == "session_update":
                update = data.get("update", {})
                subtype = update.get("sessionUpdate")
                content = update.get("content") or {}
                text = content.get("text") if isinstance(content, dict) else None
                if subtype == "user_message_chunk" and isinstance(text, str) and not update.get("backgroundCompletion"):
                    self.append_message("user", text)
                elif subtype == "agent_message_chunk" and bubble_update is not None:
                    if fresh_voice_event:
                        self.voice_reply_text += bubble_update.delta
                    self.turn_had_reply = self.turn_had_reply or bool(bubble_update.delta.strip())
                    if bubble_update.create:
                        message = self.append_message("assistant", bubble_update.text)
                        self.assistant_bubbles[bubble_update.key] = message
                    else:
                        message = self.assistant_bubbles[bubble_update.key]
                        message.set_message_text(bubble_update.text)
                        self.follow_bottom()
                    self.assistant_label = message
                    self.assistant_text = bubble_update.text
                elif subtype == "tool_call_update":
                    failure = failed_tool_text(update)
                    if failure:
                        self.system_notice(event, "tool_failed", failure)
            elif kind == "run_state":
                incoming = data.get("runId")
                if data.get("status") == "running" and incoming and incoming != self.current_run_id:
                    if self.current_run_id is not None or not self.running:
                        self.turn_had_reply = False
                    self.current_run_id = incoming
                    self.voice_reply_text = ""
                    self.error.set_text("")
                self.running = data.get("status") in {"running", "cancelling"}
                if shell.voiceos and shell.voiceos.enabled and fresh_voice_event:
                    if self.running:
                        shell.voiceos.set_state(phase="thinking", energy=0)
                self.update_controls()
            elif kind == "turn_completed":
                self.running = False
                if fresh_voice_event and shell.voiceos and shell.voiceos.enabled:
                    failed = data.get("stopReason") in FAILED
                    shell.voiceos.set_state(phase="error" if failed else "idle", energy=0)
                    if failed or data.get("stopReason") == "needs_input":
                        shell.voice_show_chat()
                    if not failed and self.voice_reply_text.strip() and data.get("stopReason") in REPLIED and shell.voice_transport:
                        shell.voice_transport.speak(self.voice_reply_text)
                self.voice_reply_text = ""
                if not self.turn_had_reply and data.get("stopReason") in {"end_turn", "completed"}:
                    self.system_notice(event, "empty_reply", "工作已結束，但尚未收到模型回覆。")
                if self.assistant_label:
                    self.mark_unread()
                self.assistant_label = None
                self.assistant_text = ""
                self.update_controls()
                shell.refresh_work_surfaces()
            elif kind == "task_plan_update":
                shell.refresh_work_tasks()
            elif kind == "queue_changed":
                self.render_queue(data.get("items", []))
            elif kind == "permission_request":
                self.permission(data)
            elif kind == "user_input_request":
                self.user_input(data)
            elif kind in {"permission_cancelled", "user_input_cancelled", "permission_resolved", "user_input_resolved"}:
                for child in self.requests.get_children():
                    if child.request_id == data.get("requestId"):
                        self.requests.remove(child)
            elif kind == "stream_error":
                self.show_service_error("stream_failed",
                    str(data.get("error") or data.get("message") or json.dumps(data, ensure_ascii=False)),
                    "連線中斷，回覆可能尚未完整。可從聊天選單重新連線，或開啟「工作紀錄」查看詳細資訊。", event)

        def close_client(self):
            self.save_draft()
            self.generation += 1
            client = self.client
            if client:
                threading.Thread(target=client.close, daemon=True).start()

    class Shell:
        def __init__(self):
            self.closing = False
            self.notification_window = None
            self.voiceos = None
            self.voice_transport = None
            self.voice_hidden_surfaces = []
            self.voice_focus_owned = False
            self.voice_listening = False
            self.work_surfaces = None
            self.work_binding = None
            self.work_request = None
            self.work_refresh_pending = False
            settings = Gtk.Settings.get_default()
            self.theme = args.theme or SHELL_THEME_NAMES.get(settings.get_property("gtk-theme-name")) or "light"
            self.geometry = (display.get_primary_monitor() or display.get_monitor(0)).get_geometry()
            self.appearance_error = None
            self.apply_appearance_defaults()
            self.configure_panel_shadow()
            settings.set_property("gtk-application-prefer-dark-theme", self.theme == "dark")
            self.css = Gtk.CssProvider()
            self.css.load_from_path(str(ROOT / "assets/shell.css"))
            Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), self.css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
            self.light_css = Gtk.CssProvider()
            self.light_css.load_from_path(str(ROOT / "assets/shell-light.css"))
            if self.theme == "light":
                Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), self.light_css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 1)
            self.surface_css = Gtk.CssProvider()
            self.surface_css.load_from_path(str(ROOT / "assets/shell-surfaces.css"))
            Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), self.surface_css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 2)
            self.apps = sorted([app for app in Gio.AppInfo.get_all() if app.should_show() and isinstance(app, Gio.DesktopAppInfo) and app.get_filename()], key=lambda app: app.get_display_name().casefold())
            Wnck.set_client_type(Wnck.ClientType.PAGER)
            self.screen = Wnck.Screen.get_default()
            self.screen.force_update()
            self.struts = Struts()
            self.chat = None
            self.wallpaper = Wallpaper(self)
            self.top = Surface("MeadowOS System Bar", Gdk.WindowTypeHint.DOCK, "shell-panel", focus=True)
            self.top.set_keep_above(True)
            self.top.set_size_request(-1, 34)
            self.dock = Surface("MeadowOS Dock", Gdk.WindowTypeHint.DOCK, "shell-dock")
            self.dock.set_keep_above(True)
            self.topbox = Gtk.Box(spacing=12)
            self.topbox.set_margin_start(18)
            self.topbox.set_margin_end(18)
            self.top.add(self.topbox)
            self.launcher = None
            applications = Gtk.Button()
            brand = Gtk.Box(spacing=8)
            brand.pack_start(image("meadow", 28), False, False, 0)
            brand.pack_start(label("MeadowOS", "shell-brand"), False, False, 0)
            applications.add(brand)
            applications.set_tooltip_text("開啟應用程式啟動器")
            applications.get_accessible().set_name("應用程式")
            applications.connect("clicked", lambda *_: self.launcher.toggle())
            self.topbox.pack_start(applications, False, False, 0)
            from workspace_switcher import WorkspaceSwitcher
            self.workspaces = WorkspaceSwitcher(self.screen, self.timestamp,
                on_error=lambda message: self.notify("桌面操作未完成", message))
            self.topbox.set_center_widget(self.workspaces)
            self.focus_name = label("")
            self.focus_name.set_ellipsize(Pango.EllipsizeMode.END)
            self.focus_name.set_max_width_chars(30)
            self.focus_name.get_style_context().add_class("shell-focused-app")
            self.topbox.pack_start(self.focus_name, False, False, 0)
            self.statusbox = Gtk.Box(spacing=10)
            self.topbox.pack_end(self.statusbox, False, False, 0)
            self.clock = label("", "shell-clock")
            self.network = icon_button("network-offline-symbolic", "網路狀態尚未取得；開啟系統設定", lambda *_: self.open_settings(), 15)
            self.network.set_no_show_all(True)
            self.chat_button = Gtk.Button()
            self.chat_button.get_accessible().set_name("開啟 Meadow 聊天")
            self.chat_button.connect("clicked", lambda *_: self.chat.toggle())
            chat_identity = Gtk.Box(spacing=5)
            chat_identity.pack_start(image("meadow", 28), False, False, 0)
            self.busy_indicator = label("●", "shell-busy-indicator")
            self.busy_indicator.set_no_show_all(True)
            chat_identity.pack_start(self.busy_indicator, False, False, 0)
            self.chat_button.add(chat_identity)
            self.statusbox.pack_start(self.chat_button, False, False, 0)
            self.statusbox.pack_start(self.network, False, False, 0)
            self.statusbox.pack_start(self.clock, False, False, 0)
            self.dockbox = Gtk.Box(spacing=0)
            self.dockbox.set_border_width(8)
            self.dockbox.set_halign(Gtk.Align.CENTER)
            self.dock.add(self.dockbox)
            self.launcher = Launcher(self)
            self.chat = Companion()
            try:
                from voice_transport import NativeVoiceTransport
                self.voice_transport = NativeVoiceTransport(state_directory(), self.voice_event)
                self.voice_transport.start()
            except (ImportError, OSError, ValueError) as exc:
                self.chat.error.set_text(f"語音服務尚未連接：{exc}")
            self.window_signature = None
            for signal_name in ("window-opened", "window-closed", "active-window-changed", "active-workspace-changed", "workspace-created", "workspace-destroyed"):
                self.screen.connect(signal_name, lambda *_: self.update_windows())
            self.screen.connect("active-window-changed", self.voice_active_window_changed)
            display.connect("monitor-added", lambda *_: self.layout())
            display.connect("monitor-removed", lambda *_: self.layout())
            self.monitor = display.get_primary_monitor() or display.get_monitor(0)
            self.monitor.connect("notify::geometry", lambda *_: self.layout())
            settings.connect("notify::gtk-theme-name", self.appearance_changed)

        def start_voiceos(self, *_args):
            try:
                if self.voiceos is None:
                    from voiceos_surface import VoiceOSSurface
                    self.voiceos = VoiceOSSurface(ROOT.parent / "voiceos/web",
                        state_directory() / "voiceos.json", self.voice_action,
                        self.voice_mode_changed, lambda: self.geometry)
                self.voiceos.expand()
                if self.voice_transport:
                    # Opening VoiceOS requests the host browser's normal mic
                    # permission flow; no additional orb click is required.
                    self.voice_transport.publish(active=True, microphoneRequested=True)
                self.window_signature = None
                self.update_windows()
            except (ImportError, ValueError, RuntimeError, OSError, GLib.Error) as exc:
                self.chat.error.set_text(f"VoiceOS 無法開啟：{exc}")
                self.voice_show_chat(force=True)

        def current_work_binding(self):
            client = self.chat.client
            if not client or not client.session or not self.chat.connected:
                return None
            return (client, self.chat.generation, str(client.workspace), client.session.get("sessionId"))

        def ensure_work_surfaces(self):
            binding = self.current_work_binding()
            if binding is None:
                self.chat.error.set_text("工作區尚未連線；請先連接 Meadow。")
                self.voice_show_chat(force=True)
                return False
            if self.work_binding != binding:
                try:
                    from workbench_surface import create_work_surfaces
                    surfaces = create_work_surfaces(binding[2], lambda: self.geometry,
                        self.present_work_file, self.refresh_work_tasks)
                except (ImportError, OSError, ValueError, RuntimeError, GLib.Error) as exc:
                    self.chat.error.set_text(f"工作介面無法開啟：{exc}")
                    self.voice_show_chat(force=True)
                    return False
                if self.work_surfaces:
                    for surface in self.work_surfaces:
                        if surface in self.voice_hidden_surfaces:
                            self.voice_hidden_surfaces.remove(surface)
                        surface.close_surface()
                self.work_binding = binding
                self.work_surfaces = surfaces
            return True

        def work_connection_changed(self):
            if self.work_surfaces:
                visible = [surface.get_visible() or surface in self.voice_hidden_surfaces
                           for surface in self.work_surfaces]
                if self.ensure_work_surfaces():
                    for surface, was_visible in zip(self.work_surfaces, visible):
                        if was_visible:
                            if self.voiceos and self.voiceos.expanded:
                                self.voice_hidden_surfaces.append(surface)
                            else:
                                surface.present_surface()
                    self.refresh_work_tasks()

        def show_work_files(self):
            if self.ensure_work_surfaces():
                if self.voiceos:
                    self.voiceos.collapse()
                self.work_surfaces[1].present_surface()

        def show_work_tasks(self):
            if self.ensure_work_surfaces():
                if self.voiceos:
                    self.voiceos.collapse()
                self.work_surfaces[0].present_surface()
                self.refresh_work_tasks()

        def refresh_work_tasks(self):
            if self.closing or not self.work_surfaces:
                return
            binding = self.current_work_binding()
            if binding is None or binding != self.work_binding:
                self.work_surfaces[0].set_error("待辦尚未同步；請確認 Meadow 連線。")
                return
            if self.work_request is not None:
                self.work_refresh_pending = True
                return
            token = object()
            self.work_request = token
            target = self.work_surfaces[0]
            def worker():
                try:
                    snapshot, error = binding[0].task_plan(), None
                except (AgentError, OSError, ValueError) as exc:
                    snapshot, error = None, str(exc)
                GLib.idle_add(self.work_tasks_ready, token, binding, target, snapshot, error)
            threading.Thread(target=worker, name="meadow-work-plan", daemon=True).start()

        def work_tasks_ready(self, token, binding, target, snapshot, error):
            if token is not self.work_request:
                return False
            self.work_request = None
            if self.closing:
                return False
            if binding == self.current_work_binding() and binding == self.work_binding:
                if error:
                    target.set_error(error)
                else:
                    target.set_snapshot(snapshot)
            if self.work_refresh_pending:
                self.work_refresh_pending = False
                self.refresh_work_tasks()
            return False

        def refresh_work_surfaces(self):
            if self.work_surfaces:
                self.refresh_work_tasks()
                if self.work_surfaces[1].get_visible():
                    self.work_surfaces[1].refresh()

        def present_work_file(self, path):
            if self.current_work_binding() != self.work_binding:
                raise ValueError("工作區已變更，請重新開啟工作檔案。")
            viewer = args.prefix / "bin/meadow-quicklook"
            if not viewer.is_file():
                raise RuntimeError("檔案預覽尚未安裝。")
            binding, target = self.work_binding, self.work_surfaces[1]
            def worker():
                temporary, snapshot, error, directory = None, None, None, False
                try:
                    from workbench_surface import prepare_workspace_preview, validate_workspace_entry
                    _path, kind = validate_workspace_entry(binding[2], path)
                    directory = kind == "directory"
                    if not directory:
                        temporary, snapshot = prepare_workspace_preview(binding[2], path,
                            state_directory() / "work-previews")
                except (OSError, ValueError) as exc:
                    error = str(exc)
                GLib.idle_add(self.work_preview_ready, binding, target, path,
                    viewer, temporary, snapshot, directory, error)
            threading.Thread(target=worker, name="meadow-work-preview", daemon=True).start()

        def work_preview_ready(self, binding, target, path, viewer, temporary, snapshot, directory, error):
            if self.closing or binding != self.current_work_binding() or binding != self.work_binding:
                if temporary:
                    temporary.cleanup()
                return False
            if error:
                target.set_error(error)
                return False
            if self.voiceos:
                self.voiceos.collapse()
            if directory:
                target.navigate(path)
                target.present_surface()
                return False
            try:
                process = subprocess.Popen([str(viewer), "--path", str(snapshot)],
                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL, start_new_session=True)
            except OSError as exc:
                temporary.cleanup()
                target.set_error(str(exc))
                return False
            def reap():
                try:
                    process.wait()
                finally:
                    temporary.cleanup()
            threading.Thread(target=reap, name="meadow-work-preview-cleanup", daemon=True).start()
            return False

        def voice_mode_changed(self, expanded):
            self.voice_focus_owned = False
            if expanded:
                if not self.voice_hidden_surfaces:
                    self.voice_hidden_surfaces = [window for window in
                        (self.top, self.dock, self.launcher, self.chat, self.chat.work_log,
                         *(self.work_surfaces or ()))
                        if window.get_visible()]
                for window in self.voice_hidden_surfaces:
                    if self.work_surfaces and window in self.work_surfaces:
                        window.hide_surface()
                    else:
                        window.hide()
                if self.notification_window:
                    self.notification_window.hide()
            else:
                for window in self.voice_hidden_surfaces:
                    # Launcher is a transient chooser, not part of returning
                    # to the user's working desktop.
                    if window is not self.launcher:
                        if self.work_surfaces and window in self.work_surfaces:
                            window.present_surface()
                        else:
                            window.show()
                self.voice_hidden_surfaces = []
                if self.notification_window:
                    self.notification_window.show()
            if self.voice_transport:
                self.voice_transport.publish(active=self.voiceos.enabled, mode="expanded" if expanded else "compact",
                                             palette=self.voiceos.palette)

        def voice_active_window_changed(self, *_args):
            if not self.voiceos or not self.voiceos.expanded:
                return
            active = self.screen.get_active_window()
            own = self.voiceos.window.get_window()
            if active and own and active.get_xid() == own.get_xid():
                self.voice_focus_owned = True
            elif (active and self.voice_focus_owned and active.get_pid() != os.getpid()
                  and active.get_window_type() == Wnck.WindowType.NORMAL):
                self.voiceos.collapse()

        def voice_show_chat(self, force=False):
            if not force and (not self.voiceos or not self.voiceos.enabled):
                return
            if self.voiceos and self.voiceos.enabled:
                self.voiceos.show_companion(self.chat,
                    pin_preference=lambda: self.chat.chrome.pin_button.get_active(),
                    overlay_above=self.chat.chrome.set_overlay_above)
                self.chat.show_for_voice()
            else:
                self.chat.show_all()
                self.chat.deiconify()
                self.chat.unread = 0
                self.update_chat_button()
                self.chat.present_with_time(Gtk.get_current_event_time())
                self.chat.input.grab_focus()

        def voice_action(self, kind, payload):
            if kind == "toggle-microphone":
                if self.voice_transport:
                    # Request intent can outlive a host-browser reload or a
                    # denied permission. Retry from observed capture state,
                    # with a fresh epoch, instead of flipping that stale bit.
                    self.voice_transport.publish(microphoneRequested=False)
                    if not self.voice_listening:
                        self.voice_transport.publish(microphoneRequested=True)
                else:
                    self.voice_event({"type": "error", "message": "語音服務尚未連接。"})
            elif kind == "palette" and self.voice_transport:
                self.voice_transport.publish(palette=payload["palette"])
            elif kind == "work-files":
                self.show_work_files()
            elif kind == "work-tasks":
                self.show_work_tasks()
            elif kind == "show-chat":
                self.voice_show_chat(force=True)
            elif kind == "exit":
                self.voice_listening = False
                if self.voice_transport:
                    self.voice_transport.publish(active=False, microphoneRequested=False, speech=None)
            elif kind == "error":
                self.voice_event({"type": "error", **payload})

        def voice_event(self, event):
            if self.closing or not self.voiceos or not self.voiceos.enabled:
                return
            kind = event.get("type")
            if kind == "transcript":
                if not self.chat.submit_text(event.get("text"), preserve_draft=True):
                    self.chat.error.set_text("語音文字尚未送出；請確認 Agent 連線後重試。")
                    self.voice_show_chat()
            elif kind == "energy":
                self.voiceos.set_state(energy=event.get("energy", 0))
            elif kind == "microphone":
                status = event.get("status")
                self.voice_listening = status == "listening"
                if status in {"denied", "error"}:
                    self.voice_event({"type": "error", "message": event.get("message") or "麥克風尚未取得許可。"})
                else:
                    self.voiceos.set_state(muted=not self.voice_listening,
                        phase="thinking" if self.chat.running else "listening" if self.voice_listening else "idle")
            elif kind == "speaking":
                status = event.get("status")
                if status == "error":
                    self.voice_event({"type": "error", "message": event.get("message") or "語音播放未完成。"})
                else:
                    self.voiceos.set_state(phase="speaking" if status == "started" else
                        "thinking" if self.chat.running else "listening" if self.voice_listening else "idle", energy=0)
            elif kind == "error":
                self.voiceos.set_state(phase="error", energy=0)
                self.chat.error.set_text(str(event.get("message") or "語音服務發生錯誤。"))
                self.voice_show_chat()

        def apply_appearance_defaults(self):
            marker = state_directory() / "shell-appearance.json"
            if args.theme is None:
                native = SHELL_THEME_NAMES.get(Gtk.Settings.get_default().get_property("gtk-theme-name"))
                if native:
                    self.theme = native
                    if not marker.exists():
                        try:
                            atomic_json(marker, {"version": 1, "theme": native})
                        except OSError as exc:
                            self.appearance_error = str(exc)
                    return
                if marker.exists():
                    try:
                        saved = json.loads(marker.read_text())
                        if not isinstance(saved, dict) or saved.get("theme") not in {"light", "dark"}:
                            raise ValueError("invalid appearance preference")
                        self.theme = saved["theme"]
                    except (ValueError, OSError):
                        self.appearance_error = "Appearance preference is unreadable; native settings were left unchanged"
                    return
            # Reuse the bundled theme's appearance plan without requiring
            # xfdesktop: this Shell owns its real wallpaper window instead.
            helper = args.prefix / "bin/meadow-theme"
            if not helper.is_file():
                helper = ROOT.parent / "desktop/bin/meadow-theme"
            try:
                planned = subprocess.run([str(helper), "--print-plan", self.theme], capture_output=True, text=True, timeout=3)
                if planned.returncode:
                    raise RuntimeError("Meadow theme appearance plan is unavailable")
                for command in json.loads(planned.stdout):
                    result = subprocess.run(command, capture_output=True, timeout=3)
                    if result.returncode:
                        raise RuntimeError("Xfce settings could not apply the native appearance preference")
                atomic_json(marker, {"version": 1, "theme": self.theme})
            except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as exc:
                self.appearance_error = str(exc)

        def appearance_changed(self, settings, _property):
            theme = SHELL_THEME_NAMES.get(settings.get_property("gtk-theme-name"))
            if theme is None or theme == self.theme:
                return
            screen = Gdk.Screen.get_default()
            if theme == "light":
                Gtk.StyleContext.add_provider_for_screen(screen, self.light_css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 1)
            else:
                Gtk.StyleContext.remove_provider_for_screen(screen, self.light_css)
            self.theme = theme
            settings.set_property("gtk-application-prefer-dark-theme", theme == "dark")
            self.wallpaper.reload(theme)
            try:
                atomic_json(state_directory() / "shell-appearance.json", {"version": 1, "theme": theme})
            except OSError as exc:
                self.appearance_error = f"Appearance changed, but its preference could not be saved: {exc}"

        def layout(self):
            self.geometry = (display.get_primary_monitor() or display.get_monitor(0)).get_geometry()
            rect = self.geometry
            self.wallpaper.move(rect.x, rect.y)
            self.wallpaper.resize(rect.width, rect.height)
            self.wallpaper.area.queue_draw()
            self.top.move(rect.x, rect.y)
            self.top.resize(rect.width, 34)
            self.workspaces.set_max_width(min(560, max(140, rect.width - 520)))
            self.struts.set(self.top, top=34)
            width = dock_width(rect.width, self.dockbox.get_preferred_width()[1])
            self.dock.resize(width, 80)
            self.dock.move(rect.x + (rect.width - width) // 2, rect.y + rect.height - 98)
            self.struts.set(self.dock, bottom=98)
            if self.voiceos and self.voiceos.enabled:
                self.voiceos.reposition()

        def configure_panel_shadow(self):
            # This xfwm4 preference is documented in its installed defaults.
            # Only our DOCK-type system surfaces lose the heavy WM shadow.
            query = shutil.which("xfconf-query")
            if not query:
                return
            try:
                result = subprocess.run([query, "-c", "xfwm4", "-p", "/general/show_dock_shadow", "--create", "-t", "bool", "-s", "false"], capture_output=True, timeout=3)
                if result.returncode:
                    self.appearance_error = "Xfwm dock shadow preference could not be applied"
            except (OSError, subprocess.TimeoutExpired):
                self.appearance_error = "Xfwm dock shadow preference could not be applied"

        def timestamp(self):
            return Gtk.get_current_event_time() or GdkX11.x11_get_server_time(self.top.get_window())

        def windows(self):
            return [window for window in self.screen.get_windows() if not window.is_skip_tasklist() and window.get_window_type() == Wnck.WindowType.NORMAL]

        def activate(self, window):
            if self.voiceos and self.voiceos.expanded:
                self.voiceos.collapse()
            try:
                workspace = window.get_workspace()
                if workspace and workspace != self.screen.get_active_workspace():
                    workspace.activate(self.timestamp())
                if window == self.screen.get_active_window():
                    window.minimize()
                else:
                    window.unminimize(self.timestamp())
                    window.activate(self.timestamp())
            except (TypeError, GLib.Error):
                pass

        def launch_app(self, app):
            if self.voiceos and self.voiceos.expanded:
                self.voiceos.collapse()
            try:
                spawn([shutil.which("gio") or "/usr/bin/gio", "launch", app.get_filename()])
            except OSError as exc:
                self.chat.error.set_text(str(exc))

        def app_for(self, ids, executables=()):
            # Desktop IDs identify the intended app. A shared executable may
            # also expose unrelated actions, such as Thunar's bulk renamer.
            for desktop_id in ids:
                for app in self.apps:
                    if app.get_id() == desktop_id:
                        return app
            for app in self.apps:
                if Path(app.get_executable() or "").name in executables:
                    return app
            return None

        def presentation(self, app):
            return application_presentation(app.get_id(), app.get_display_name(), app.get_categories(), app.get_executable())

        def app_icon(self, app, size=48):
            canonical = self.presentation(app)["icon"]
            if canonical:
                return image(canonical, size)
            gicon = app.get_icon()
            # Theme application assets already include the common glass plate.
            # Do not nest and shrink them again for system desktop entries.
            for name in gicon.get_names() if hasattr(gicon, "get_names") else ():
                if name in GLASS_APP_ICONS:
                    return image(name, size)
            if not gicon:
                return image("meadow-glass-application", size)
            widget = Gtk.Image.new_from_gicon(gicon, Gtk.IconSize.DIALOG)
            widget.set_pixel_size(max(16, round(size * .48)))
            return self.framed_app_icon(widget, size)

        def framed_app_icon(self, widget, size=48):
            plate = Gtk.Overlay()
            plate.add(image("meadow-glass-tile", size))
            widget.set_halign(Gtk.Align.CENTER)
            widget.set_valign(Gtk.Align.CENTER)
            plate.add_overlay(widget)
            plate.set_overlay_pass_through(widget, True)
            return plate

        def window_icon(self, window, size=48):
            identity = (window.get_class_group_name() or "").casefold()
            if identity:
                for app in self.apps:
                    names = ((app.get_startup_wm_class() or "").casefold(),
                             Path(app.get_executable() or "").name.casefold())
                    if identity in names:
                        return self.app_icon(app, size)
            pixbuf = window.get_icon()
            if not pixbuf:
                return image("meadow-glass-application", size)
            glyph_size = max(16, round(size * .48))
            return self.framed_app_icon(Gtk.Image.new_from_pixbuf(
                pixbuf.scale_simple(glyph_size, glyph_size, GdkPixbuf.InterpType.BILINEAR)), size)

        def open_settings(self):
            launcher = args.prefix / "bin/meadow-settings"
            if launcher.is_file():
                try:
                    spawn([str(launcher)])
                except OSError as exc:
                    self.chat.error.set_text(str(exc))
                return
            app = self.app_for(("xfce-settings-manager.desktop", "org.xfce.SettingsManager.desktop"), ("xfce4-settings-manager",))
            if app:
                self.launch_app(app)

        def open_desktop(self):
            launcher = args.prefix / "share/meadow-os/bin/meadow-session"
            if launcher.is_file():
                try:
                    spawn([str(launcher), "desktop"])
                except OSError as exc:
                    self.chat.error.set_text(str(exc))

        def dock_button(self, icon, title, callback, active=False, running=False, enabled=True):
            button = Gtk.Button()
            button.set_sensitive(enabled)
            button.set_tooltip_text(title)
            button.get_accessible().set_name(title)
            content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=4)
            content.set_halign(Gtk.Align.CENTER)
            content.set_valign(Gtk.Align.CENTER)
            icon.set_size_request(48, 48)
            content.pack_start(icon, False, False, 0)
            dot = Gtk.Box()
            dot.set_size_request(4, 4)
            dot.get_style_context().add_class("dock-running-dot")
            dot.set_halign(Gtk.Align.CENTER)
            dot.set_opacity(1 if running else 0)
            content.pack_start(dot, False, False, 0)
            button.add(content)
            if active:
                button.get_style_context().add_class("active")
            button.connect("clicked", callback)
            self.dockbox.pack_start(button, False, False, 0)

        def update_windows(self):
            windows = self.windows()
            active = self.screen.get_active_window()
            workspace = self.screen.get_active_workspace()
            signature = (
                tuple((window.get_xid(), window.get_name(), window.get_class_group_name(), window.is_minimized()) for window in windows),
                active.get_xid() if active else None,
                workspace.get_number() if workspace else None,
                tuple(item.get_name() for item in self.screen.get_workspaces()),
                self.chat.get_visible(),
                self.chat.iconified,
            )
            if signature == self.window_signature:
                return True
            self.window_signature = signature
            for child in self.dockbox.get_children():
                self.dockbox.remove(child)
            self.dock_button(image("meadow-launcher", 48), "應用程式", lambda *_: self.launcher.toggle())
            specifications = [
                ("瀏覽器", "web-browser", ("google-chrome.desktop", "chromium.desktop", "firefox.desktop", "org.gnome.Epiphany.desktop"), ("google-chrome-stable", "google-chrome", "chromium", "firefox", "epiphany"), ("google-chrome", "chromium", "firefox", "epiphany")),
                ("檔案", "system-file-manager", ("thunar.desktop", "Thunar.desktop", "org.xfce.Thunar.desktop"), (), ("thunar",)),
                ("終端機", "utilities-terminal", ("xfce4-terminal.desktop", "org.xfce.Terminal.desktop", "xterm.desktop"), ("xfce4-terminal", "xterm"), ("xfce4-terminal", "xterm")),
            ]
            represented = set()
            for title, fallback, ids, executables, classes in specifications:
                app = self.app_for(ids, executables)
                matches = [window for window in windows if any(name in (window.get_class_group_name() or "").casefold() for name in classes)]
                represented.update(window.get_xid() for window in matches)
                icon = self.app_icon(app, 48) if app else image(fallback, 48)
                self.dock_button(icon, self.presentation(app)["title"] if app else f"{title} 尚未安裝",
                    lambda _button, selected=app, opened=matches: self.activate(opened[0]) if opened else self.launch_app(selected),
                    active in matches, bool(matches), bool(app or matches))
            self.dock_button(image("meadow", 48), "Meadow 聊天", lambda *_: self.chat.toggle(),
                running=self.chat.get_visible() or self.chat.connected and self.chat.running)
            poster = ROOT.parent / "voiceos/web/vendor/glaze-poster.webp"
            try:
                voice_icon = Gtk.Image.new_from_pixbuf(GdkPixbuf.Pixbuf.new_from_file_at_scale(str(poster), 48, 48, True))
            except GLib.Error:
                voice_icon = image("meadow", 48)
            self.dock_button(voice_icon, "VoiceOS · 語音對話", self.start_voiceos,
                running=bool(self.voiceos and self.voiceos.enabled))
            settings_windows = [window for window in windows if any(name in (window.get_class_group_name() or "").casefold()
                for name in ("meadow-settings", "xfce4-settings", "xfce-settings"))]
            represented.update(window.get_xid() for window in settings_windows)
            self.dock_button(image("preferences-system", 48), "系統設定",
                lambda *_: self.open_settings(),
                active in settings_windows, bool(settings_windows))
            for window in windows:
                if window.get_xid() in represented:
                    continue
                app_image = self.window_icon(window, 48)
                self.dock_button(app_image, window.get_name(), lambda _button, selected=window: self.activate(selected), window == active, True)
            self.dockbox.show_all()
            self.workspaces.refresh()
            # A brief actual application identity keeps the system bar quiet;
            # full document titles remain in native windows and Dock tooltips.
            self.focus_name.set_text(active.get_class_group_name() if active and active.get_pid() != os.getpid() else "")
            self.layout()
            return True

        def update_chat_button(self):
            if not self.chat:
                return
            busy = self.chat.connected and self.chat.running
            self.busy_indicator.set_visible(busy)
            self.chat_button.get_accessible().set_name("Meadow 正在工作 · 開啟聊天" if busy else "開啟 Meadow 聊天")
            caption = self.chat.status_label.get_text()
            if self.chat.unread:
                caption += f" · {self.chat.unread} 則未讀"
            self.chat_button.set_tooltip_text("Meadow · " + caption)

        def tick(self):
            self.clock.set_text(datetime.datetime.now().strftime("%m月%d日  %H:%M"))
            return not self.closing

        def network_status(self):
            try:
                proxy = Gio.DBusProxy.new_for_bus_sync(Gio.BusType.SYSTEM, Gio.DBusProxyFlags.DO_NOT_AUTO_START,
                    None, "org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager", "org.freedesktop.NetworkManager", None)
                value = proxy.get_cached_property("State")
                state = value.unpack() if value else None
                def paint():
                    self.network.set_tooltip_text("網路已連線" if state == 70 else "網路尚未連線" if state is not None else "網路狀態尚未取得")
                    self.network.set_visible(state is not None)
                    if state == 70:
                        self.network.set_image(image("network-transmit-receive-symbolic", 16))
                    return False
                GLib.idle_add(paint)
            except GLib.Error:
                pass

        def notify(self, title, text):
            """Native OS presentation; no inference, queue mutation or task replay."""
            if self.closing:
                return
            if self.notification_window:
                self.notification_window.destroy()
            banner = Surface("Meadow 通知", Gdk.WindowTypeHint.NOTIFICATION, "shell-notification")
            self.notification_window = banner
            banner.set_keep_above(True)
            banner.set_resizable(False)
            banner.set_default_size(360, -1)
            row = Gtk.Box(spacing=10)
            row.set_border_width(14)
            row.pack_start(mark(28, "avatar"), False, False, 0)
            button = Gtk.Button()
            button.get_style_context().add_class("notification-body")
            button.get_accessible().set_name("開啟 Meadow 聊天")
            words = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=5)
            heading = label(title, "notification-title")
            body = label(text[:240], "notification-text", True)
            body.set_max_width_chars(30)
            words.pack_start(heading, False, False, 0)
            words.pack_start(body, False, False, 0)
            button.add(words)
            def open_chat(*_args):
                banner.destroy()
                if not self.chat.get_visible() or self.chat.iconified:
                    self.chat.toggle()
                else:
                    self.chat.present_with_time(Gtk.get_current_event_time())
                    self.chat.input.grab_focus()
                self.chat.unread = 0
                self.update_chat_button()
            button.connect("clicked", open_chat)
            row.pack_start(button, True, True, 0)
            close = icon_button("window-close-symbolic", "關閉通知", lambda *_: banner.destroy(), 14)
            close.set_valign(Gtk.Align.START)
            row.pack_start(close, False, False, 0)
            glass = GlassSurface(banner, mode="content", corner_radius=18)
            glass.add(row)
            banner.add(glass)
            def positioned(*_args):
                rect = self.geometry
                width, _height = banner.get_size()
                banner.move(rect.x + rect.width - width - 24, rect.y + 48)
                return False
            banner.connect("map-event", positioned)
            expiry = [None]
            def destroyed(*_args):
                if expiry[0] is not None:
                    GLib.source_remove(expiry[0])
                    expiry[0] = None
                if self.notification_window is banner:
                    self.notification_window = None
            banner.connect("destroy", destroyed)
            banner.show_all()
            def expire():
                expiry[0] = None
                banner.destroy()
                return False
            expiry[0] = GLib.timeout_add_seconds(8, expire)

        def shutdown(self, *_args):
            if self.closing:
                return False
            self.closing = True
            if self.voice_transport:
                self.voice_transport.close()
            if self.voiceos:
                self.voiceos.close()
            if self.work_surfaces:
                for surface in self.work_surfaces:
                    surface.close_surface()
            if self.notification_window:
                self.notification_window.destroy()
            self.chat.close_client()
            for window in (self.chat.work_log, self.chat, self.launcher, self.dock, self.top, self.wallpaper):
                window.destroy()
            self.struts.close()
            Gtk.main_quit()
            return False

        def smoke(self):
            try:
                rect = self.geometry
                pixbuf = Gdk.pixbuf_get_from_window(Gdk.get_default_root_window(), rect.x, rect.y, rect.width, rect.height)
                saved, data = pixbuf.save_to_bufferv("png", [], [])
                if not saved:
                    raise RuntimeError("PNG encoding failed")
                args.smoke.parent.mkdir(parents=True, exist_ok=True)
                args.smoke.write_bytes(data)
                atomic_json(args.smoke.with_suffix(".json"), {
                    "display": os.environ["DISPLAY"], "geometry": [rect.x, rect.y, rect.width, rect.height],
                    "chat_visible": self.chat.get_visible(), "draft_length": len(self.chat.draft()),
                    "agent_connected": self.chat.connected, "model_ready": self.chat.model_ready,
                    "appearance_error": self.appearance_error,
                    "windows": [{"xid": window.get_xid(), "title": window.get_name(), "minimized": window.is_minimized()} for window in self.windows()],
                })
            except (GLib.Error, OSError, RuntimeError) as exc:
                print(f"Meadow Shell smoke failed: {exc}", file=sys.stderr)
                self.smoke_failed = True
            self.shutdown()
            return False

    shell = Shell()
    for window in (shell.wallpaper, shell.top, shell.dock):
        window.show_all()
    shell.update_windows()
    shell.tick()
    # GLX needs an already painted initial desktop on software X11 displays.
    # This is a frame/geometry handshake, never a startup-delay assumption.
    paint_token = os.environ.get("MEADOW_SHELL_PAINT_TOKEN")
    if paint_token:
        painted_surfaces = set()
        paint_handlers = []
        def after_first_paint(_clock, window):
            rect = shell.geometry
            if (not window.get_mapped() or window.get_allocated_width() <= 1
                    or window.get_allocated_height() <= 1):
                return
            width = dock_width(rect.width, shell.dockbox.get_preferred_width()[1])
            target = {
                shell.wallpaper: (rect.x, rect.y, rect.width, rect.height),
                shell.top: (rect.x, rect.y, rect.width, 34),
                shell.dock: (rect.x + (rect.width - width) // 2, rect.y + rect.height - 98, width, 80),
            }[window]
            x, y = window.get_position()
            actual = (x, y, window.get_allocated_width(), window.get_allocated_height())
            if actual != target:
                painted_surfaces.discard(window)
                return
            painted_surfaces.add(window)
            if len(painted_surfaces) != 3:
                return
            for clock, handler in paint_handlers:
                clock.disconnect(handler)
            paint_handlers.clear()
            display.sync()
            try:
                atomic_json(state_directory() / "shell-painted.json", {
                    "token": paint_token, "pid": os.getpid(), "display": os.environ.get("DISPLAY"),
                    "painted": True, "geometry": [rect.x, rect.y, rect.width, rect.height],
                })
            except OSError as exc:
                print(f"Meadow Shell first-paint receipt failed: {exc}", file=sys.stderr)
        for window in (shell.wallpaper, shell.top, shell.dock):
            clock = window.get_frame_clock()
            paint_handlers.append((clock, clock.connect("after-paint", after_first_paint, window)))
            window.queue_draw()
    GLib.timeout_add_seconds(1, shell.tick)
    GLib.timeout_add_seconds(2, shell.update_windows)
    shell.chat.connect_agent()
    threading.Thread(target=shell.network_status, daemon=True, name="meadow-system-status").start()
    if args.show_chat:
        shell.chat.toggle()
    if args.smoke:
        GLib.timeout_add(max(200, int(args.smoke_delay * 1000)), shell.smoke)
    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGTERM, shell.shutdown)
    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGINT, shell.shutdown)
    Gtk.main()
    return 1 if getattr(shell, "smoke_failed", False) else 0


if __name__ == "__main__":
    raise SystemExit(main())
