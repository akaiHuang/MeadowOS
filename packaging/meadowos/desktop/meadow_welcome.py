#!/usr/bin/python3
"""Native GTK3 launcher, intentionally without a second chat or agent runtime."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import subprocess
import sys

PAYLOAD = Path(__file__).resolve().parent
sys.path.insert(0, str(PAYLOAD))
from meadow_state import collect_state, public_state, state_root


def startup_theme(preview: str | None, native_theme: str | None) -> str:
    """A preview is local; otherwise the live desktop wins over an old receipt."""
    if preview:
        return preview
    native = {"Meadow-Light": "light", "Meadow-Dark": "dark"}.get(native_theme)
    if native:
        return native
    try:
        value = json.loads((state_root() / "theme.json").read_text())
        return "light" if isinstance(value, dict) and value.get("theme") == "light" else "dark"
    except (OSError, ValueError):
        return "dark"


def main() -> int:
    parser = argparse.ArgumentParser(description="MeadowOS native work desktop")
    parser.add_argument("--theme", choices=("dark", "light"), help="Preview this window in a theme without changing desktop preferences")
    parser.add_argument("--prefix", type=Path, default=Path("/opt/meadow"))
    parser.add_argument("--status-json", action="store_true", help="Read actual local status without GTK; private URLs are omitted")
    parser.add_argument("--smoke", type=Path, metavar="PNG", help="Capture the actual GTK window, then exit; does not simulate readiness")
    args = parser.parse_args()
    if args.status_json:
        print(json.dumps(public_state(collect_state(args.prefix)), ensure_ascii=False, indent=2))
        return 0
    try:
        import gi
        gi.require_version("Gtk", "3.0")
        gi.require_version("Gdk", "3.0")
        from gi.repository import Gdk, GLib, Gtk, Pango
        from meadow_window_chrome import create_hover_chrome
        from glass.gtk_surface import GlassSurface
    except (ImportError, ValueError) as exc:
        print(f"MeadowOS requires Python PyGObject and GTK3: {exc}", file=sys.stderr)
        return 2
    ok, _ = Gtk.init_check()
    if not ok:
        print("MeadowOS needs a graphical desktop session (DISPLAY).", file=sys.stderr)
        return 2
    GLib.set_prgname("meadow-welcome")

    class Welcome(Gtk.Window):
        def __init__(self):
            super().__init__(title="MeadowOS")
            self.set_default_size(960, 680)
            self.set_size_request(820, 620)
            self.set_position(Gtk.WindowPosition.CENTER)
            self.set_icon_from_file(str(PAYLOAD / "assets/meadow-wave-mark.svg"))
            self.connect("destroy", Gtk.main_quit)
            self.get_style_context().add_class("meadow-welcome")
            self.css = Gtk.CssProvider()
            self.preview_theme = args.theme is not None
            self.theme = startup_theme(args.theme, Gtk.Settings.get_default().get_property("gtk-theme-name"))
            self.state = {}
            self.root = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL)
            rail = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=20)
            rail.get_style_context().add_class("rail")
            rail.set_size_request(210, -1)
            rail.set_margin_start(24)
            rail.set_margin_end(24)
            rail.set_margin_top(32)
            rail.set_margin_bottom(24)
            self.root.pack_start(rail, False, False, 0)
            mark = Gtk.Image.new_from_file(str(PAYLOAD / "assets/meadow-wave-mark.svg"))
            mark.set_pixel_size(48)
            # Loading a pixbuf fixes the SVG's logical size for an actual GTK widget.
            from gi.repository import GdkPixbuf
            mark.set_from_pixbuf(GdkPixbuf.Pixbuf.new_from_file_at_scale(str(PAYLOAD / "assets/meadow-wave-mark.svg"), 48, 48, True))
            mark.set_halign(Gtk.Align.START)
            rail.pack_start(mark, False, False, 0)
            rail.pack_start(self.label("Meadow", "brand"), False, False, 0)
            rail.pack_start(self.label("你的私人 AI 工作機", "muted"), False, False, 0)
            rail.pack_start(Gtk.Separator(), False, False, 4)
            rail.pack_start(self.label("電腦為工作保留空間。\n你只需要交辦。", "rail-copy", wrap=True), False, False, 0)
            rail.pack_start(Gtk.Box(), True, True, 0)
            rail.pack_start(self.label("外觀", "muted"), False, False, 0)
            switch = Gtk.Box(spacing=8)
            for value, title in (("dark", "深色"), ("light", "淺色")):
                button = Gtk.Button(label=title)
                button.connect("clicked", lambda _button, selected=value: self.choose_theme(selected))
                switch.pack_start(button, True, True, 0)
            rail.pack_start(switch, False, False, 0)
            rail.pack_start(self.label("開發版 · 不需會員登入", "footnote"), False, False, 0)
            self.root.pack_start(Gtk.Separator(orientation=Gtk.Orientation.VERTICAL), False, False, 0)
            content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=18)
            content.set_margin_start(36)
            content.set_margin_end(36)
            content.set_margin_top(28)
            content.set_margin_bottom(22)
            self.root.pack_start(content, True, True, 0)
            content.pack_start(self.label("你的工作桌面", "headline"), False, False, 0)
            content.pack_start(self.label("從 Meadow 交辦工作，或直接使用這台電腦。", "muted", wrap=True), False, False, 0)
            self.desktop_button = Gtk.Button(label="開啟 Meadow Code")
            self.desktop_button.get_style_context().add_class("primary")
            self.desktop_button.set_size_request(-1, 46)
            self.desktop_button.connect("clicked", self.open_desktop)
            content.pack_start(self.desktop_button, False, False, 0)
            self.status_box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
            self.status_box.get_style_context().add_class("status-block")
            content.pack_start(self.status_box, False, False, 0)
            content.pack_start(Gtk.Separator(), False, False, 0)
            content.pack_start(self.label("從其他裝置聊天", "section-title"), False, False, 0)
            url_row = Gtk.Box(spacing=10)
            self.url_label = self.label("", "url", wrap=True)
            self.url_label.set_selectable(True)
            self.url_label.set_can_focus(False)
            self.url_label.set_ellipsize(Pango.EllipsizeMode.MIDDLE)
            self.url_label.set_max_width_chars(44)
            url_row.pack_start(self.url_label, True, True, 0)
            self.copy_button = Gtk.Button.new_from_icon_name("edit-copy-symbolic", Gtk.IconSize.BUTTON)
            self.copy_button.set_tooltip_text("複製私人網址")
            self.copy_button.connect("clicked", self.copy_url)
            url_row.pack_start(self.copy_button, False, False, 0)
            self.web_button = Gtk.Button(label="開啟")
            self.web_button.connect("clicked", self.open_web)
            url_row.pack_start(self.web_button, False, False, 0)
            content.pack_start(url_row, False, False, 0)
            self.url_hint = self.label("", "footnote", wrap=True)
            content.pack_start(self.url_hint, False, False, 0)
            content.pack_start(Gtk.Separator(), False, False, 0)
            tools = Gtk.Box(spacing=10)
            self.tool_buttons = {}
            for action, name, icon in (("files", "檔案", "folder-symbolic"), ("browser", "瀏覽器", "web-browser-symbolic"), ("settings", "系統設定", "preferences-system-symbolic")):
                button = Gtk.Button(label=name)
                button.set_image(Gtk.Image.new_from_icon_name(icon, Gtk.IconSize.BUTTON))
                button.set_always_show_image(True)
                button.set_size_request(-1, 40)
                button.connect("clicked", lambda _button, selected=action: self.open_tool(selected))
                tools.pack_start(button, True, True, 0)
                self.tool_buttons[action] = button
            content.pack_start(tools, False, False, 0)
            content.pack_start(Gtk.Box(), True, True, 0)
            footer = Gtk.Box(spacing=12)
            self.message = self.label("", "footnote", wrap=True)
            footer.pack_start(self.message, True, True, 0)
            refresh = Gtk.Button.new_from_icon_name("view-refresh-symbolic", Gtk.IconSize.BUTTON)
            refresh.set_tooltip_text("重新檢查狀態")
            refresh.connect("clicked", lambda _button: self.refresh())
            footer.pack_start(refresh, False, False, 0)
            content.pack_start(footer, False, False, 0)
            self.apply_theme()
            self.glass = GlassSurface(self, mode="content", corner_radius=0)
            self.glass.add(self.root)
            self.chrome = create_hover_chrome(self, self.glass, title="MeadowOS",
                                              focus_content=self.desktop_button.grab_focus)
            self.add(self.chrome)
            self.refresh()
            self.show_all()

        @staticmethod
        def label(text, css_class, wrap=False):
            label = Gtk.Label(label=text, xalign=0)
            label.get_style_context().add_class(css_class)
            label.set_line_wrap(wrap)
            return label

        def apply_theme(self):
            settings = Gtk.Settings.get_default()
            name = "Meadow-Dark" if self.theme == "dark" else "Meadow-Light"
            settings.set_property("gtk-theme-name", name)
            settings.set_property("gtk-application-prefer-dark-theme", self.theme == "dark")
            self.css.load_from_path(str(PAYLOAD / "themes" / name / "gtk-3.0/gtk.css"))
            Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), self.css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

        def choose_theme(self, theme):
            self.theme = theme
            self.apply_theme()
            if self.preview_theme:
                self.message.set_text("僅預覽此視窗；桌面外觀未變更。")
                return
            try:
                result = subprocess.run([str(PAYLOAD / "bin/meadow-theme"), "--set", theme], capture_output=True, text=True, timeout=5)
                self.message.set_text("已切換桌面外觀。" if result.returncode == 0 else "此視窗已切換；桌面外觀尚未套用，請檢查圖形工作階段。")
            except (OSError, subprocess.TimeoutExpired):
                self.message.set_text("此視窗已切換；桌面外觀尚未套用。")

        def refresh(self):
            self.state = collect_state(args.prefix)
            for child in self.status_box.get_children():
                self.status_box.remove(child)
            activity = self.state["agent_state"]
            service_text = {"active": "Agent 服務運行中", "activating": "Agent 服務啟動中", "inactive": "Agent 服務尚未啟動", "failed": "Agent 服務啟動失敗", "deactivating": "Agent 服務停止中"}.get(activity, "尚無法確認 Agent 服務")
            installed = self.state["desktop_installed"]
            can_launch = installed and self.state["desktop_launcher_installed"]
            desktop_text = "Meadow Code Desktop 已安裝" if can_launch else ("Desktop 啟動入口尚未安裝" if installed else "Meadow Code Desktop 尚未安裝")
            for text, present in ((service_text, activity == "active"), (desktop_text, can_launch)):
                row = Gtk.Box(spacing=9)
                dot = self.label("●", "status-on" if present else "status-off")
                dot.set_tooltip_text("已確認" if present else "尚未完成")
                row.pack_start(dot, False, False, 0)
                row.pack_start(self.label(text, "status-copy"), False, False, 0)
                self.status_box.pack_start(row, False, False, 0)
            self.status_box.show_all()
            self.desktop_button.set_sensitive(can_launch)
            self.desktop_button.set_tooltip_text(None if can_launch else desktop_text)
            url = self.state["private_url"]
            self.url_label.set_text(url or "私人網址尚未設定")
            self.copy_button.set_sensitive(bool(url))
            self.web_button.set_sensitive(bool(url) and bool(self.state["browser"]))
            self.url_hint.set_text("網址已設定；連線與配對狀態請在開啟後確認。" if url else "設定私人網址後，可從其他電腦或手機連到這台工作機。")
            for action, key in (("files", "file_manager"), ("browser", "browser"), ("settings", "settings")):
                enabled = bool(self.state[key])
                self.tool_buttons[action].set_sensitive(enabled)
                self.tool_buttons[action].set_tooltip_text(None if enabled else "這個桌面工具尚未安裝")
            self.message.set_text(self.state["metadata_error"] or "模型 API 與網站登入可在 Meadow 中設定。")

        def launch(self, argv):
            try:
                subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
                self.message.set_text("已送出開啟要求。")
            except OSError:
                self.message.set_text("無法開啟，請重新檢查安裝狀態。")

        def open_desktop(self, _button):
            if self.state.get("desktop_installed") and self.state.get("desktop_launcher_installed"):
                self.launch([self.state["desktop_launcher"], "desktop"])

        def open_tool(self, action):
            if action == "files" and self.state.get("file_manager"):
                workspace = Path(self.state["workspace"])
                self.launch([self.state["file_manager"], str(workspace if workspace.is_dir() else Path.home())])
            elif action == "browser" and self.state.get("browser"):
                self.launch([self.state["browser"]])
            elif action == "settings" and self.state.get("settings"):
                self.launch([self.state["settings"]])

        def copy_url(self, _button):
            if self.state.get("private_url"):
                Gtk.Clipboard.get(Gdk.SELECTION_CLIPBOARD).set_text(self.state["private_url"], -1)
                self.message.set_text("已複製私人網址；只分享給你信任的裝置。")

        def open_web(self, _button):
            if self.state.get("private_url") and self.state.get("browser"):
                self.launch([self.state["browser"], self.state["private_url"]])

    window = Welcome()
    if args.smoke:
        def capture():
            try:
                drawable = window.get_window()
                width, height = drawable.get_width(), drawable.get_height()
                pixbuf = Gdk.pixbuf_get_from_window(drawable, 0, 0, width, height)
                if pixbuf is None:
                    raise RuntimeError("GTK window capture unavailable")
                args.smoke.parent.mkdir(parents=True, exist_ok=True)
                saved, png = pixbuf.save_to_bufferv("png", [], [])
                if not saved:
                    raise RuntimeError("GTK PNG encoding failed")
                args.smoke.write_bytes(png)
                print(json.dumps({"capture": str(args.smoke), "theme": window.theme, "status": public_state(window.state)}, ensure_ascii=False))
                Gtk.main_quit()
            except Exception as exc:
                print(f"GTK smoke capture failed: {exc}", file=sys.stderr)
                Gtk.main_quit()
                window.capture_failed = True
            return False
        GLib.timeout_add(500, capture)
    Gtk.main()
    return 1 if getattr(window, "capture_failed", False) else 0


if __name__ == "__main__":
    raise SystemExit(main())
