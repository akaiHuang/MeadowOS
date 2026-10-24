#!/usr/bin/python3
"""Native, read-only Meadow file preview and isolated thumbnail worker."""
from __future__ import annotations

import argparse
import datetime
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading

PAYLOAD = Path(__file__).resolve().parent
sys.path.insert(0, str(PAYLOAD))
MAX_RESULT_BYTES = 2 * 1024 * 1024
TYPES = {"image": "圖片", "pdf": "PDF 文件", "text": "文字文件", "code": "程式碼",
         "document": "文書文件", "spreadsheet": "試算表", "presentation": "簡報",
         "archive": "壓縮檔", "audio": "音訊", "video": "影片", "font": "字型",
         "folder": "資料夾", "executable": "應用程式", "unknown": "檔案"}
ICONS = {"image": "image-x-generic", "pdf": "application-pdf", "text": "text-x-generic",
         "code": "text-x-script", "document": "application-msword",
         "spreadsheet": "application-vnd.ms-excel", "presentation": "application-vnd.ms-powerpoint",
         "archive": "package-x-generic", "audio": "audio-x-generic", "video": "video-x-generic",
         "font": "font-x-generic", "folder": "folder", "executable": "application-x-executable",
         "unknown": "application-octet-stream"}


def size_text(size):
    value = float(size or 0)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if value < 1024 or unit == "TB":
            return f"{value:.0f} {unit}" if unit == "B" else f"{value:.1f} {unit}"
        value /= 1024


def stop_owned_worker(process):
    if process is not None and process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass


def native_main(path, parent_xid):
    import gi
    gi.require_version("Gtk", "3.0")
    gi.require_version("Gdk", "3.0")
    from gi.repository import Gdk, GdkPixbuf, GLib, Gtk, Pango
    from meadow_window_chrome import create_hover_chrome
    from glass.gtk_surface import GlassSurface

    GLib.set_prgname("meadow-quicklook")
    ok, _ = Gtk.init_check()
    if not ok:
        print("Meadow 預覽需要圖形桌面。", file=sys.stderr)
        return 2

    class Preview(Gtk.Window):
        def __init__(self):
            super().__init__(title=path.name + " — Meadow 預覽")
            self.set_wmclass("meadow-quicklook", "MeadowQuickLook")
            self.set_default_size(980, 720)
            self.set_size_request(620, 440)
            self.set_position(Gtk.WindowPosition.CENTER)
            self.set_icon_name("document-open")
            self.get_style_context().add_class("meadow-quicklook")
            visual = self.get_screen().get_rgba_visual()
            if visual:
                self.set_visual(visual)
            self.alive, self.generation, self.page, self.page_count = True, 0, 0, 1
            self.worker, self.result, self.pixbuf = None, {}, None
            self.player = self.media_bus = self.media_timer = None
            self.media_state, self.media_duration, self.updating_seek = "stopped", 0, False
            self.connect("destroy", self.closed)
            self.connect("key-press-event", self.key_pressed)
            self.connect("realize", self.parent_ready)
            css = Gtk.CssProvider()
            css.load_from_data(b"""
                .meadow-quicklook { background: transparent; }
                .meadow-quicklook .preview-info { background: transparent; padding: 28px 18px 18px; }
                .meadow-quicklook .preview-name { font-size: 16px; font-weight: 600; }
                .meadow-quicklook .preview-muted { color: alpha(@theme_fg_color,.60); font-size: 12px; }
                .meadow-quicklook .preview-body { background: transparent; }
                .meadow-quicklook .preview-controls { padding: 8px 12px; background: transparent; }
                .meadow-quicklook .preview-controls button { min-height: 24px; padding: 2px 12px; border-radius: 8px; }
                .meadow-quicklook textview, .meadow-quicklook textview text { background-color: transparent; }
                .meadow-quicklook treeview { background-color: alpha(@theme_bg_color,.55); }
            """)
            Gtk.StyleContext.add_provider_for_screen(self.get_screen(), css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
            self.css = css
            root = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL)
            info = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
            info.set_size_request(190, -1)
            info.get_style_context().add_class("preview-info")
            self.type_icon = Gtk.Image.new_from_icon_name("text-x-generic", Gtk.IconSize.DIALOG)
            self.type_icon.set_pixel_size(64)
            self.type_icon.set_halign(Gtk.Align.START)
            info.pack_start(self.type_icon, False, False, 0)
            self.name = Gtk.Label(label=path.name, xalign=0)
            self.name.set_line_wrap(True)
            self.name.set_line_wrap_mode(Pango.WrapMode.WORD_CHAR)
            self.name.set_max_width_chars(18)
            self.name.get_style_context().add_class("preview-name")
            info.pack_start(self.name, False, False, 0)
            self.details = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
            info.pack_start(self.details, False, False, 0)
            hint = Gtk.Label(label="空白鍵／Esc 關閉", xalign=0)
            hint.get_style_context().add_class("preview-muted")
            info.pack_end(hint, False, False, 0)
            self.glass_sidebar = GlassSurface(self, mode="sidebar", corner_radius=0)
            self.glass_sidebar.add(info)
            root.pack_start(self.glass_sidebar, False, False, 0)
            body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
            body.get_style_context().add_class("preview-body")
            self.content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
            self.content.set_hexpand(True)
            self.content.set_vexpand(True)
            body.pack_start(self.content, True, True, 0)
            controls = Gtk.Box(spacing=8)
            controls.get_style_context().add_class("preview-controls")
            self.fidelity = Gtk.Label(xalign=0)
            self.fidelity.get_style_context().add_class("preview-muted")
            controls.pack_start(self.fidelity, True, True, 0)
            self.previous = Gtk.Button(label="上一頁")
            self.previous.connect("clicked", lambda *_: self.change_page(-1))
            controls.pack_start(self.previous, False, False, 0)
            self.page_label = Gtk.Label()
            controls.pack_start(self.page_label, False, False, 4)
            self.next = Gtk.Button(label="下一頁")
            self.next.connect("clicked", lambda *_: self.change_page(1))
            controls.pack_start(self.next, False, False, 0)
            body.pack_end(controls, False, False, 0)
            self.glass_content = GlassSurface(self, mode="content", corner_radius=0)
            self.glass_content.add(body)
            root.pack_start(self.glass_content, True, True, 0)
            self.chrome = create_hover_chrome(self, root, title=path.name,
                                              focus_content=self.content.grab_focus)
            self.add(self.chrome)
            self.show_all()
            self.request_page(0)

        def parent_ready(self, *_args):
            if not parent_xid:
                return
            try:
                gi.require_version("GdkX11", "3.0")
                from gi.repository import GdkX11
                parent = GdkX11.X11Window.foreign_new_for_display(self.get_display(), parent_xid)
                if parent:
                    self.get_window().set_transient_for(parent)
            except (ImportError, ValueError, TypeError):
                pass

        def key_pressed(self, _widget, event):
            modifiers = Gdk.ModifierType.CONTROL_MASK | Gdk.ModifierType.MOD1_MASK | Gdk.ModifierType.SUPER_MASK | Gdk.ModifierType.SHIFT_MASK
            if event.keyval == Gdk.KEY_Escape or (event.keyval == Gdk.KEY_space and not event.state & modifiers):
                self.destroy()
                return True
            if not event.state & modifiers and event.keyval in (Gdk.KEY_Left, Gdk.KEY_Page_Up):
                self.change_page(-1)
                return self.page_count > 1
            if not event.state & modifiers and event.keyval in (Gdk.KEY_Right, Gdk.KEY_Page_Down):
                self.change_page(1)
                return self.page_count > 1
            return False

        def clear_content(self):
            self.stop_media()
            for child in self.content.get_children():
                child.destroy()
            self.pixbuf = None

        def request_page(self, page):
            self.generation += 1
            generation = self.generation
            stop_owned_worker(self.worker)
            self.clear_content()
            self.page = page
            self.previous.set_sensitive(False)
            self.next.set_sensitive(False)
            spinner = Gtk.Spinner()
            spinner.set_halign(Gtk.Align.CENTER)
            spinner.set_valign(Gtk.Align.CENTER)
            spinner.set_vexpand(True)
            self.content.pack_start(spinner, True, True, 0)
            spinner.start()
            self.content.show_all()
            env = dict(os.environ)
            env.pop("GTK_MODULES", None)
            try:
                process = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "--inspect", "--path", str(path), "--page", str(page)],
                                           stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                           start_new_session=True, env=env)
            except OSError:
                self.result_ready(generation, None, {"kind": "error", "title": path.name,
                                  "text": "目前無法啟動此檔案的預覽。", "metadata": {}})
                return
            self.worker = process

            def wait_for_result():
                try:
                    output, _errors = process.communicate(timeout=12)
                    if process.returncode or len(output) > MAX_RESULT_BYTES:
                        raise ValueError("preview unavailable")
                    result = json.loads(output)
                    if not isinstance(result, dict):
                        raise ValueError("invalid preview")
                except (OSError, ValueError, subprocess.TimeoutExpired):
                    stop_owned_worker(process)
                    try:
                        process.communicate(timeout=.5)
                    except subprocess.TimeoutExpired:
                        try:
                            os.killpg(process.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        process.communicate()
                    result = {"kind": "error", "title": path.name, "text": "目前無法讀取此檔案的預覽。", "metadata": {}}
                GLib.idle_add(self.result_ready, generation, process, result)

            threading.Thread(target=wait_for_result, name="meadow-preview-reader", daemon=False).start()

        def result_ready(self, generation, process, result):
            if not self.alive or generation != self.generation:
                return False
            if self.worker is process:
                self.worker = None
            self.clear_content()
            self.result = result
            self.page = max(0, int(result.get("page", 0)))
            self.page_count = max(1, int(result.get("page_count", 1)))
            category = result.get("metadata", {}).get("category", "unknown")
            if category not in TYPES:
                from file_preview import category_for
                category = category_for(path, result.get("mime"))
            self.type_icon.set_from_icon_name(ICONS.get(category, ICONS["unknown"]), Gtk.IconSize.DIALOG)
            self.type_icon.set_pixel_size(64)
            self.update_details(result, category)
            kind = result.get("kind", "unsupported")
            if kind == "media":
                self.render_media(result, category)
            elif kind == "image" and result.get("image_path"):
                self.render_image(result["image_path"])
            elif kind in ("table", "folder") and result.get("columns"):
                self.render_table(result)
            else:
                self.render_text(result.get("text") or "此檔案目前提供資訊預覽。", category == "code")
            self.previous.set_visible(self.page_count > 1)
            self.next.set_visible(self.page_count > 1)
            self.page_label.set_visible(self.page_count > 1)
            self.previous.set_sensitive(self.page > 0)
            self.next.set_sensitive(self.page+1 < self.page_count)
            self.page_label.set_text(f"{self.page+1} / {self.page_count}")
            self.fidelity.set_text("文字／表格預覽" if category in ("document", "spreadsheet", "presentation")
                                   and kind in ("text", "table") else "")
            self.content.show_all()
            self.write_diagnostics()
            return False

        def update_details(self, result, category):
            for child in self.details.get_children():
                child.destroy()
            metadata = result.get("metadata", {})
            rows = [("類型", TYPES.get(category, "檔案")), ("大小", size_text(result.get("size")))]
            try:
                rows.append(("修改時間", datetime.datetime.fromtimestamp(path.stat().st_mtime).strftime("%Y/%m/%d %H:%M")))
            except OSError:
                pass
            if metadata.get("width") and metadata.get("height"):
                rows.append(("尺寸", f"{metadata['width']} × {metadata['height']}"))
            if metadata.get("duration") is not None:
                try:
                    seconds = max(0, int(float(metadata["duration"])))
                    rows.append(("長度", f"{seconds//60}:{seconds%60:02d}"))
                except (TypeError, ValueError, OverflowError):
                    pass
            for key, label in (("dimensions", "尺寸"), ("sheet_name", "工作表"), ("format", "格式")):
                if metadata.get(key) is not None:
                    rows.append((label, str(metadata[key])[:160]))
            if self.page_count > 1:
                rows.append(("頁數", str(self.page_count)))
            for label, value in rows[:7]:
                box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=3)
                caption = Gtk.Label(label=label, xalign=0)
                caption.get_style_context().add_class("preview-muted")
                data = Gtk.Label(label=value, xalign=0)
                data.set_line_wrap(True)
                data.set_max_width_chars(20)
                data.set_line_wrap_mode(Pango.WrapMode.WORD_CHAR)
                box.pack_start(caption, False, False, 0)
                box.pack_start(data, False, False, 0)
                self.details.pack_start(box, False, False, 0)
            self.details.show_all()

        def render_text(self, text, monospace=False):
            view = Gtk.TextView()
            view.set_editable(False)
            view.set_cursor_visible(False)
            view.set_monospace(monospace)
            view.set_wrap_mode(Gtk.WrapMode.WORD_CHAR)
            view.set_left_margin(28)
            view.set_right_margin(28)
            view.set_top_margin(28)
            view.set_bottom_margin(28)
            view.get_buffer().set_text(str(text)[:1024*1024])
            scroll = Gtk.ScrolledWindow()
            scroll.add(view)
            self.content.pack_start(scroll, True, True, 0)

        def render_table(self, result):
            columns = [str(x) for x in result.get("columns", [])][:32]
            model = Gtk.ListStore(*([str]*len(columns)))
            for values in result.get("rows", [])[:500]:
                model.append([str(values[i])[:4096] if i < len(values) else "" for i in range(len(columns))])
            view = Gtk.TreeView(model=model)
            view.set_headers_visible(True)
            for index, title in enumerate(columns):
                renderer = Gtk.CellRendererText()
                renderer.set_property("ellipsize", Pango.EllipsizeMode.END)
                column = Gtk.TreeViewColumn(title, renderer, text=index)
                column.set_resizable(True)
                column.set_min_width(100)
                column.set_max_width(360)
                view.append_column(column)
            scroll = Gtk.ScrolledWindow()
            scroll.add(view)
            self.content.pack_start(scroll, True, True, 0)

        def render_image(self, filename):
            try:
                self.pixbuf = GdkPixbuf.Pixbuf.new_from_file_at_scale(str(filename), 1600, 1200, True)
            except GLib.Error:
                self.render_text("目前無法顯示這張圖片。")
                return
            area = Gtk.DrawingArea()
            area.set_hexpand(True)
            area.set_vexpand(True)

            def draw(widget, cr):
                width, height = widget.get_allocated_width(), widget.get_allocated_height()
                pixbuf = self.pixbuf
                if pixbuf is None:
                    return False
                scale = min(max(1, width-40)/pixbuf.get_width(), max(1, height-40)/pixbuf.get_height(), 1)
                cr.save()
                cr.translate((width-pixbuf.get_width()*scale)/2, (height-pixbuf.get_height()*scale)/2)
                cr.scale(scale, scale)
                Gdk.cairo_set_source_pixbuf(cr, pixbuf, 0, 0)
                cr.paint()
                cr.restore()
                return False

            area.connect("draw", draw)
            self.content.pack_start(area, True, True, 0)

        def render_media(self, result, category):
            try:
                gi.require_version("Gst", "1.0")
                from gi.repository import Gst
                Gst.init(None)
                self.Gst = Gst
                player = Gst.ElementFactory.make("playbin", None)
                if player is None:
                    raise RuntimeError("missing playback")
                has_video = category == "video" or any(stream.get("codec_type") == "video"
                    for stream in result.get("metadata", {}).get("streams", []) if isinstance(stream, dict))
                if has_video:
                    sink = Gst.ElementFactory.make("gtksink", None)
                    if sink is None:
                        raise RuntimeError("missing native video sink")
                    player.set_property("video-sink", sink)
                    self.content.pack_start(sink.get_property("widget"), True, True, 0)
                elif result.get("image_path"):
                    self.render_image(result["image_path"])
                else:
                    image = Gtk.Image.new_from_icon_name("audio-x-generic", Gtk.IconSize.DIALOG)
                    image.set_pixel_size(128)
                    self.content.pack_start(image, True, True, 0)
                if not has_video:
                    video_sink = Gst.ElementFactory.make("fakesink", None)
                    if video_sink is None:
                        raise RuntimeError("missing inert video sink")
                    player.set_property("video-sink", video_sink)
                # An isolated lab can explicitly request a silent sink. Product
                # sessions leave this unset and use the real native audio sink.
                if os.environ.get("MEADOW_PREVIEW_AUDIO_SINK") == "fakesink":
                    audio_sink = Gst.ElementFactory.make("fakesink", None)
                    if audio_sink is None:
                        raise RuntimeError("missing silent audio sink")
                    audio_sink.set_property("sync", True)
                    player.set_property("audio-sink", audio_sink)
                from gi.repository import Gio
                player.set_property("uri", Gio.File.new_for_path(str(path)).get_uri())
                self.player = player
                bus = player.get_bus()
                bus.add_signal_watch()
                self.media_bus = bus
                self.media_handler = bus.connect("message", self.media_message)
                controls = Gtk.Box(spacing=10)
                controls.set_border_width(14)
                self.play_button = Gtk.Button(label="播放")
                self.play_button.connect("clicked", self.toggle_play)
                controls.pack_start(self.play_button, False, False, 0)
                self.seek = Gtk.Scale.new_with_range(Gtk.Orientation.HORIZONTAL, 0, 1, .001)
                self.seek.set_draw_value(False)
                self.seek.connect("change-value", self.seek_to)
                controls.pack_start(self.seek, True, True, 0)
                self.elapsed = Gtk.Label(label="0:00")
                controls.pack_start(self.elapsed, False, False, 0)
                self.content.pack_end(controls, False, False, 0)
                self.media_state = "paused"
                player.set_state(Gst.State.PAUSED)
                self.media_timer = GLib.timeout_add(200, self.update_media)
            except (ImportError, ValueError, RuntimeError, TypeError):
                self.stop_media()
                for child in self.content.get_children():
                    child.destroy()
                if result.get("image_path"):
                    self.render_image(result["image_path"])
                else:
                    self.render_text("此檔案目前提供媒體資訊預覽。")

        def toggle_play(self, *_args):
            if self.player is None:
                return
            playing = self.media_state != "playing"
            self.player.set_state(self.Gst.State.PLAYING if playing else self.Gst.State.PAUSED)
            self.media_state = "playing" if playing else "paused"
            self.play_button.set_label("暫停" if playing else "播放")
            self.write_diagnostics()

        def seek_to(self, _range, _scroll, value):
            if self.player and self.media_duration and not self.updating_seek:
                self.player.seek_simple(self.Gst.Format.TIME, self.Gst.SeekFlags.FLUSH | self.Gst.SeekFlags.KEY_UNIT,
                                        int(max(0, min(1, value))*self.media_duration))
            return False

        def update_media(self):
            if not self.alive or self.player is None:
                return False
            ok, duration = self.player.query_duration(self.Gst.Format.TIME)
            if ok and duration > 0:
                self.media_duration = duration
            ok, position = self.player.query_position(self.Gst.Format.TIME)
            if ok and position >= 0:
                seconds = position//self.Gst.SECOND
                self.elapsed.set_text(f"{seconds//60}:{seconds%60:02d}")
                if self.media_duration:
                    self.updating_seek = True
                    self.seek.set_value(position/self.media_duration)
                    self.updating_seek = False
            self.write_diagnostics()
            return True

        def media_message(self, _bus, message):
            if message.type == self.Gst.MessageType.EOS:
                self.player.set_state(self.Gst.State.PAUSED)
                self.media_state = "ended"
                self.play_button.set_label("重播")
                self.player.seek_simple(self.Gst.Format.TIME, self.Gst.SeekFlags.FLUSH, 0)
            elif message.type == self.Gst.MessageType.ERROR:
                self.player.set_state(self.Gst.State.NULL)
                self.media_state = "error"
                self.play_button.set_sensitive(False)
                self.elapsed.set_text("目前無法播放")
            self.write_diagnostics()

        def stop_media(self):
            if self.media_timer:
                GLib.source_remove(self.media_timer)
                self.media_timer = None
            if self.player:
                self.player.set_state(self.Gst.State.NULL)
                self.player = None
            if self.media_bus:
                self.media_bus.disconnect(self.media_handler)
                self.media_bus.remove_signal_watch()
                self.media_bus = None
            self.media_state, self.media_duration = "stopped", 0

        def change_page(self, offset):
            page = self.page+offset
            if 0 <= page < self.page_count:
                self.request_page(page)

        def write_diagnostics(self):
            target = os.environ.get("MEADOW_QUICKLOOK_DIAGNOSTICS")
            if not target:
                return
            try:
                record = {"pid": os.getpid(), "file": str(path), "page": self.page,
                          "page_count": self.page_count, "kind": self.result.get("kind"),
                          "metadata": self.result.get("metadata", {}), "media_state": self.media_state}
                if self.player:
                    ok, value = self.player.query_position(self.Gst.Format.TIME)
                    record["media_position_ns"] = value if ok else None
                destination = Path(target)
                temporary = destination.with_name(destination.name+f".{os.getpid()}.tmp")
                temporary.write_text(json.dumps(record, ensure_ascii=False)+"\n")
                temporary.replace(destination)
            except OSError:
                pass

        def closed(self, *_args):
            if not self.alive:
                return
            self.alive = False
            self.generation += 1
            stop_owned_worker(self.worker)
            self.worker = None
            self.stop_media()
            Gtk.main_quit()

    window = Preview()
    signal.signal(signal.SIGTERM, lambda *_: GLib.idle_add(window.destroy))
    Gtk.main()
    return 0


def main():
    parser = argparse.ArgumentParser(description="Meadow 檔案預覽")
    parser.add_argument("--path", type=Path, required=True)
    parser.add_argument("--parent-xid", type=lambda value: int(value, 0), default=0)
    parser.add_argument("--page", type=int, default=0)
    parser.add_argument("--thumbnail", action="store_true")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--size", type=int, default=256)
    parser.add_argument("--inspect", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not args.path.is_absolute():
        parser.error("需要絕對檔案路徑")
    if args.thumbnail or args.inspect:
        from file_preview import load_preview, make_thumbnail
        if args.thumbnail:
            if args.output is None or not args.output.is_absolute() or not 48 <= args.size <= 256:
                parser.error("縮圖需要完整輸出路徑與有效尺寸")
            return 0 if make_thumbnail(args.path, args.output, args.size) else 1
        result = load_preview(args.path, page=args.page)
        output = json.dumps(result, ensure_ascii=False)
        if len(output.encode()) > MAX_RESULT_BYTES:
            return 1
        print(output)
        return 0
    return native_main(args.path, args.parent_xid)


if __name__ == "__main__":
    raise SystemExit(main())
