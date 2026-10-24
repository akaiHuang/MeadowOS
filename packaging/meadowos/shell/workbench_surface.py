"""Native, presentation-only work surfaces for the existing VoiceOS session.

Plans are snapshots from Agent; files are the actual workspace. This module
does not generate plans, infer completion, execute files, or create model turns.
GTK imports stay inside the factory so filesystem boundaries can be tested
without a display.
"""
from __future__ import annotations

from contextlib import contextmanager
import mimetypes
import os
from pathlib import Path
import stat
import tempfile
import threading

MAX_ENTRIES = 200
MAX_SCANNED_ENTRIES = 2000
MAX_PLAN_ITEMS = 200
MAX_PREVIEW_BYTES = 32 * 1024 * 1024
STATUS_LABELS = {"pending": "待辦", "in_progress": "進行中",
                 "completed": "已完成", "cancelled": "已取消"}


def _workspace_path(workspace, target):
    root = Path(os.path.abspath(os.fspath(workspace)))
    path = Path(os.path.abspath(os.fspath(target)))
    if "\x00" in str(root) or "\x00" in str(path):
        raise ValueError("無效的工作檔案路徑。")
    try:
        relative = path.relative_to(root)
    except ValueError as exc:
        raise ValueError("只能開啟這個工作資料夾內的項目。") from exc
    return root, path, relative


@contextmanager
def _directory_fd(workspace, directory):
    root, path, relative = _workspace_path(workspace, directory)
    flags = os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(root, flags)
    try:
        for part in relative.parts:
            following = os.open(part, flags, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = following
        yield descriptor, path
    finally:
        os.close(descriptor)


def validate_workspace_entry(workspace, target):
    """Revalidate a clicked local entry; links and special files stay inert."""
    root, path, _relative = _workspace_path(workspace, target)
    if path == root:
        with _directory_fd(root, root):
            return path, "directory"
    with _directory_fd(root, path.parent) as (descriptor, _parent):
        info = os.stat(path.name, dir_fd=descriptor, follow_symlinks=False)
        if stat.S_ISLNK(info.st_mode):
            raise ValueError("捷徑保留在工作資料夾中；此面板不會跟隨符號連結。")
        if stat.S_ISDIR(info.st_mode):
            return path, "directory"
        if stat.S_ISREG(info.st_mode):
            return path, "file"
        raise ValueError("此項目不是一般檔案或資料夾。")


def prepare_workspace_preview(workspace, target, cache_dir):
    """Copy the clicked file from a pinned descriptor into a private snapshot.

    Run in a worker. The returned TemporaryDirectory must remain alive while
    the preview process uses snapshotPath, and be cleaned after that process
    exits. No original path is reopened after validation. A directory/link or
    file changing during the copy fails without returning a preview receipt.
    """
    root, path, _relative = _workspace_path(workspace, target)
    if path == root:
        raise ValueError("請選擇要預覽的檔案。")
    cache = Path(os.path.abspath(os.fspath(cache_dir)))
    cache.mkdir(mode=0o700, parents=True, exist_ok=True)
    cache_info = cache.lstat()
    if (not stat.S_ISDIR(cache_info.st_mode) or cache_info.st_uid != os.geteuid()
            or stat.S_IMODE(cache_info.st_mode) & 0o077):
        raise ValueError("預覽快取必須是目前使用者的私人資料夾。")
    temporary = None
    try:
        with _directory_fd(root, path.parent) as (parent_fd, _parent):
            source_fd = os.open(path.name, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
                                | getattr(os, "O_NONBLOCK", 0), dir_fd=parent_fd)
        with os.fdopen(source_fd, "rb") as source:
            before = os.fstat(source.fileno())
            if not stat.S_ISREG(before.st_mode):
                raise ValueError("此項目不是可預覽的一般檔案。")
            if before.st_size > MAX_PREVIEW_BYTES:
                raise ValueError("檔案超過 32 MiB 的即時預覽上限，尚未開啟預覽。")
            temporary = tempfile.TemporaryDirectory(prefix="meadow-work-preview-", dir=cache)
            snapshot = Path(temporary.name) / path.name
            destination_fd = os.open(snapshot, os.O_WRONLY | os.O_CREAT | os.O_EXCL
                                     | getattr(os, "O_NOFOLLOW", 0), 0o600)
            copied = 0
            with os.fdopen(destination_fd, "wb") as destination:
                while True:
                    chunk = source.read(min(1024 * 1024, MAX_PREVIEW_BYTES + 1 - copied))
                    if not chunk:
                        break
                    copied += len(chunk)
                    if copied > MAX_PREVIEW_BYTES:
                        raise ValueError("檔案超過 32 MiB 的即時預覽上限，尚未開啟預覽。")
                    destination.write(chunk)
                destination.flush()
                after = os.fstat(source.fileno())
                if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (
                        after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) or copied != after.st_size:
                    raise ValueError("檔案正在變更；請等儲存完成後再預覽。")
                # Quick Look presents this private copy. Keep the source's
                # recorded timestamps without reopening or changing the source.
                os.utime(destination.fileno(), ns=(before.st_atime_ns, before.st_mtime_ns))
        return temporary, snapshot
    except Exception:
        if temporary is not None:
            temporary.cleanup()
        raise


def list_workspace_directory(workspace, directory, limit=MAX_ENTRIES):
    """Read one bounded directory off GTK's main thread, without following links.

    At most limit+1 visible entries or 2000 total entries are observed. The bounded page is sorted
    folders first, then case-insensitive filename. Hidden files remain hidden.
    The truncated flag prevents presenting the page as a complete directory.
    """
    if type(limit) is not int or not 1 <= limit <= MAX_ENTRIES:
        raise ValueError("無效的目錄顯示上限。")
    rows, truncated = [], False
    with _directory_fd(workspace, directory) as (descriptor, path):
        with os.scandir(descriptor) as entries:
            for index, entry in enumerate(entries):
                if index >= MAX_SCANNED_ENTRIES:
                    truncated = True
                    break
                if entry.name.startswith("."):
                    continue
                if len(rows) >= limit:
                    truncated = True
                    break
                try:
                    info = entry.stat(follow_symlinks=False)
                    kind = ("directory" if stat.S_ISDIR(info.st_mode) else
                            "file" if stat.S_ISREG(info.st_mode) else
                            "link" if stat.S_ISLNK(info.st_mode) else "unsupported")
                    rows.append({"name": entry.name, "path": str(path / entry.name),
                                 "kind": kind, "size": info.st_size,
                                 "modified": info.st_mtime})
                except OSError:
                    rows.append({"name": entry.name, "path": str(path / entry.name),
                                 "kind": "unavailable", "size": 0, "modified": 0})
    rows.sort(key=lambda row: (row["kind"] != "directory", row["name"].casefold(), row["name"]))
    return {"directory": str(path), "items": rows, "truncated": truncated}


def plan_view(snapshot):
    """Validate the presentation shape without inventing task or run state."""
    if not isinstance(snapshot, dict) or snapshot.get("ok") is not True:
        raise ValueError("尚未取得這段對話的待辦清單。")
    plan = snapshot.get("plan")
    if not isinstance(plan, dict) or not isinstance(plan.get("items"), list):
        raise ValueError("收到的待辦清單格式不完整。")
    rows = []
    for item in plan["items"][:MAX_PLAN_ITEMS]:
        if not isinstance(item, dict) or not isinstance(item.get("content"), str):
            continue
        status = item.get("status") if isinstance(item.get("status"), str) else None
        artifacts = item.get("artifacts", [])
        rows.append({"content": item["content"], "status": status,
                     "statusLabel": STATUS_LABELS.get(status, "狀態未確認"),
                     "artifacts": [value for value in artifacts[:20] if isinstance(value, str) and value]
                     if isinstance(artifacts, list) else []})
    goal = plan.get("goal")
    run = snapshot.get("run") if isinstance(snapshot.get("run"), dict) else {}
    return {"goal": goal if isinstance(goal, str) else "", "items": rows,
            "running": run.get("running") if type(run.get("running")) is bool else None,
            "truncated": len(plan["items"]) > MAX_PLAN_ITEMS,
            "warning": snapshot.get("restoreWarning") if isinstance(snapshot.get("restoreWarning"), str) else ""}


def create_work_surfaces(workspace, geometry, on_open, on_refresh_tasks):
    """Return (TasksWindow, WorkFilesWindow), both initially hidden.

    geometry() returns the active monitor rectangle. on_open(Path) is invoked
    only after an explicit click and worker-side path validation; it decides
    whether to preview or open the actual document. No command is run here.
    All public window methods must be called on the GTK thread.
    """
    import gi
    gi.require_version("Gtk", "3.0")
    gi.require_version("Gdk", "3.0")
    from gi.repository import Gdk, Gio, GLib, Gtk, Pango
    from meadow_window_chrome import create_hover_chrome
    from glass.gtk_surface import GlassSurface

    root_path = Path(os.path.abspath(os.fspath(workspace)))
    css = Gtk.CssProvider()
    css.load_from_data(b"""
      window.meadow-work-surface { background: transparent; }
      .meadow-work-surface .work-body, .meadow-work-surface scrolledwindow,
      .meadow-work-surface viewport, .meadow-work-surface flowbox { background: transparent; }
      .meadow-work-surface .work-title { font-size: 19px; font-weight: 600; }
      .meadow-work-surface .work-muted { color: alpha(@theme_fg_color,.65); font-size: 12px; }
      .meadow-work-surface .work-error { color: @theme_fg_color; }
      .meadow-work-surface .work-item { border-radius: 12px; padding: 10px; background: alpha(@theme_fg_color,.04); }
      .meadow-work-surface .work-file { border-radius: 12px; background: transparent; border: 0; box-shadow: none; padding: 10px; }
      .meadow-work-surface .work-file:hover { background: alpha(@theme_fg_color,.07); }
      .meadow-work-surface .work-file:focus { box-shadow: inset 0 0 0 1px alpha(@theme_selected_bg_color,.7); }
      .meadow-work-surface .work-status { font-size: 11px; color: alpha(@theme_fg_color,.68); }
      .meadow-work-surface .work-artifact { border-radius: 8px; padding: 5px 8px; }
    """)
    Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), css,
                                            Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 3)

    def text(value="", style=None, wrap=False):
        widget = Gtk.Label(label=value, xalign=0)
        widget.set_line_wrap(wrap)
        widget.set_line_wrap_mode(Pango.WrapMode.WORD_CHAR)
        if style:
            widget.get_style_context().add_class(style)
        return widget

    def image(name, size=20):
        widget = Gtk.Image.new_from_icon_name(name, Gtk.IconSize.DIALOG)
        widget.set_pixel_size(size)
        return widget

    def control(icon, title, callback):
        widget = Gtk.Button()
        widget.add(image(icon))
        widget.set_tooltip_text(title)
        widget.get_accessible().set_name(title)
        widget.connect("clicked", callback)
        return widget

    class WorkWindow(Gtk.Window):
        def __init__(self, title, size, minimum, placement):
            super().__init__(title=title)
            self.window = self
            self.alive, self.generation = True, 0
            self.initially_placed, self.last_position = False, None
            self.placement = placement
            self.set_wmclass("meadow-workbench", "MeadowWorkbench")
            self.set_default_size(*size)
            self.set_size_request(*minimum)
            self.get_style_context().add_class("meadow-work-surface")
            visual = self.get_screen().get_rgba_visual()
            if visual:
                self.set_visual(visual)
            self.body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
            self.body.set_border_width(20)
            self.body.get_style_context().add_class("work-body")
            self.header = Gtk.Box(spacing=10)
            self.header.pack_start(self.drag_area(text(title, "work-title")), True, True, 0)
            self.body.pack_start(self.header, False, False, 0)
            self.error = text("", "work-error", True)
            self.error.set_no_show_all(True)
            self.body.pack_start(self.error, False, False, 0)
            glass = GlassSurface(self, mode="content", corner_radius=0)
            glass.add(self.body)
            self.chrome = create_hover_chrome(self, glass, title=title,
                close=self.hide_surface, allow_pin=True)
            self.add(self.chrome)
            self.connect("delete-event", self.hide_surface)
            self.connect("destroy", self.destroyed)
            self.connect("key-press-event", self.key_pressed)

        def drag_area(self, child):
            area = Gtk.EventBox()
            area.set_visible_window(False)
            area.add_events(Gdk.EventMask.BUTTON_PRESS_MASK)
            area.connect("button-press-event", self.drag_press)
            area.add(child)
            return area

        def drag_press(self, _widget, event):
            if event.button == 1 and event.type == Gdk.EventType.BUTTON_PRESS:
                self.begin_move_drag(event.button, int(event.x_root), int(event.y_root), event.time)
                return True
            return False

        def key_pressed(self, _widget, event):
            if event.keyval == Gdk.KEY_Escape:
                return self.hide_surface()
            return False

        def present_surface(self):
            if not self.alive:
                return
            rect = geometry()
            width, height = self.get_size()
            if not self.initially_placed:
                self.initially_placed = True
                width = min(width, max(320, rect.width - 48))
                height = min(height, max(360, rect.height - 130))
                self.resize(width, height)
                x = rect.x + max(24, rect.width - width - 24) if self.placement == "right" else rect.x + (rect.width - width) // 2
                y = rect.y + 92 if self.placement == "right" else rect.y + max(60, (rect.height - height) // 2)
                self.last_position = (x, y)
            self.show_all()
            if self.last_position:
                self.move(*self.last_position)
            self.error.set_visible(bool(self.error.get_text()))
            self.deiconify()
            self.present_with_time(Gtk.get_current_event_time())

        def hide_surface(self, *_args):
            if self.get_visible():
                self.last_position = tuple(self.get_position())
            self.hide()
            return True

        def close_surface(self):
            if self.alive:
                self.destroy()

        def destroyed(self, *_args):
            self.alive = False
            self.generation += 1

        def set_error(self, message):
            if self.alive:
                self.error.set_text(str(message or ""))
                self.error.set_visible(bool(message))

        def activate_path(self, value):
            generation = self.generation
            def worker():
                try:
                    target = Path(value)
                    if not target.is_absolute():
                        target = root_path / target
                    path, kind = validate_workspace_entry(root_path, target)
                    result = (path, kind, None)
                except (OSError, ValueError) as exc:
                    result = (None, None, str(exc))
                GLib.idle_add(self.path_ready, generation, *result)
            threading.Thread(target=worker, name="meadow-work-entry", daemon=True).start()

        def path_ready(self, generation, path, kind, error):
            if not self.alive or generation != self.generation:
                return False
            if error:
                self.set_error(error)
            else:
                try:
                    self.open_validated(path, kind)
                except (OSError, ValueError, RuntimeError) as exc:
                    self.set_error(str(exc))
            return False

        def open_validated(self, path, _kind):
            on_open(path)

    class TasksWindow(WorkWindow):
        def __init__(self):
            super().__init__("待辦事項", (360, 500), (320, 360), "right")
            self.header.pack_end(control("view-refresh-symbolic", "更新待辦事項", lambda *_: on_refresh_tasks()), False, False, 0)
            self.goal = text("", None, True)
            self.goal.set_max_width_chars(34)
            self.body.pack_start(self.goal, False, False, 0)
            scroll = Gtk.ScrolledWindow()
            scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            self.items = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
            scroll.add(self.items)
            self.body.pack_start(scroll, True, True, 0)
            self.caption = text("等待連接這段對話的待辦事項。", "work-muted", True)
            self.body.pack_end(self.drag_area(self.caption), False, False, 0)

        def set_error(self, message):
            super().set_error(message)
            if message and self.alive and hasattr(self, "caption"):
                self.caption.set_text("清單尚未同步；保留最近一次取得的資料。")

        def set_snapshot(self, snapshot):
            if not self.alive:
                return
            try:
                view = plan_view(snapshot)
            except ValueError as exc:
                self.set_error(str(exc))
                return
            self.set_error(view["warning"])
            self.goal.set_text(view["goal"])
            for child in self.items.get_children():
                child.destroy()
            if not view["items"]:
                empty = text("目前沒有待辦事項。\n和 Meadow 討論工作後，已記錄的計畫會顯示在這裡。", "work-muted", True)
                empty.set_margin_top(24)
                self.items.pack_start(self.drag_area(empty), False, False, 0)
            for item in view["items"]:
                row = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=7)
                row.get_style_context().add_class("work-item")
                row.pack_start(text(item["statusLabel"], "work-status"), False, False, 0)
                name = text(item["content"], None, True)
                name.set_max_width_chars(34)
                name.set_selectable(True)
                row.pack_start(name, False, False, 0)
                for artifact in item["artifacts"]:
                    button = Gtk.Button(label=Path(artifact).name or artifact)
                    button.get_child().set_ellipsize(Pango.EllipsizeMode.MIDDLE)
                    button.get_child().set_max_width_chars(28)
                    button.get_style_context().add_class("work-artifact")
                    button.set_tooltip_text(artifact)
                    button.connect("clicked", lambda _button, selected=artifact: self.activate_path(selected))
                    row.pack_start(button, False, False, 0)
                self.items.pack_start(row, False, False, 0)
            self.items.show_all()
            caption = "Meadow 正在工作" if view["running"] is True else "與這段對話同步"
            if view["truncated"]:
                caption += f" · 顯示前 {MAX_PLAN_ITEMS} 項"
            self.caption.set_text(caption)

    class WorkFilesWindow(WorkWindow):
        def __init__(self):
            super().__init__("工作檔案", (820, 600), (480, 400), "center")
            self.directory = root_path
            self.last_request = None
            self.loading = False
            self.header.pack_end(control("view-refresh-symbolic", "更新工作檔案", lambda *_: self.refresh()), False, False, 0)
            self.header.pack_end(control("go-home-symbolic", "回到工作資料夾", lambda *_: self.navigate(root_path)), False, False, 0)
            self.up = control("go-up-symbolic", "上一層資料夾", lambda *_: self.navigate(self.directory.parent))
            self.header.pack_end(self.up, False, False, 0)
            self.breadcrumb = text("工作資料夾", "work-muted")
            self.breadcrumb.set_ellipsize(Pango.EllipsizeMode.MIDDLE)
            self.body.pack_start(self.drag_area(self.breadcrumb), False, False, 0)
            self.scroll = Gtk.ScrolledWindow()
            self.scroll.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
            self.grid = Gtk.FlowBox()
            self.grid.set_selection_mode(Gtk.SelectionMode.NONE)
            self.grid.set_column_spacing(10)
            self.grid.set_row_spacing(12)
            self.grid.set_min_children_per_line(2)
            self.grid.set_max_children_per_line(5)
            self.grid.set_homogeneous(True)
            self.grid.set_valign(Gtk.Align.START)
            self.scroll.add(self.grid)
            self.body.pack_start(self.scroll, True, True, 0)
            self.caption = text("", "work-muted", True)
            self.body.pack_end(self.drag_area(self.caption), False, False, 0)
            self.refresh()

        def present_surface(self):
            super().present_surface()
            self.refresh()

        def navigate(self, directory):
            try:
                _root, path, _relative = _workspace_path(root_path, directory)
            except ValueError as exc:
                self.set_error(str(exc))
                return
            self.directory = path
            self.refresh()

        def refresh(self):
            if not self.alive:
                return
            self.generation += 1
            generation, directory = self.generation, self.directory
            self.last_request = (generation, directory)
            self.caption.set_text("正在讀取工作檔案…")
            self.up.set_sensitive(directory != root_path)
            self.breadcrumb.set_text("工作資料夾" if directory == root_path else str(directory.relative_to(root_path)))
            # One worker drains the latest request. Rapid refresh/navigation
            # cannot create unbounded filesystem threads or install stale rows.
            if self.loading:
                return
            self.loading = True
            threading.Thread(target=self.read_directories, name="meadow-work-files", daemon=True).start()

        def read_directories(self):
            request = self.last_request
            try:
                result, error = list_workspace_directory(root_path, request[1]), None
            except (OSError, ValueError) as exc:
                result, error = None, str(exc)
            GLib.idle_add(self.directory_ready, request[0], result, error)

        def directory_ready(self, generation, result, error):
            self.loading = False
            if not self.alive:
                return False
            if generation != self.generation:
                self.loading = True
                threading.Thread(target=self.read_directories, name="meadow-work-files", daemon=True).start()
                return False
            for child in self.grid.get_children():
                child.destroy()
            self.set_error(error)
            if error:
                self.caption.set_text("無法讀取這個資料夾；檔案未被更動。")
                return False
            rows = result["items"]
            for entry in rows:
                button = Gtk.Button()
                button.get_style_context().add_class("work-file")
                button.set_size_request(126, 116)
                column = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=7)
                if entry["kind"] == "directory":
                    icon = image("folder", 56)
                else:
                    mime = mimetypes.guess_type(entry["name"])[0] or "application/octet-stream"
                    icon = Gtk.Image.new_from_gicon(Gio.content_type_get_icon(mime), Gtk.IconSize.DIALOG)
                    icon.set_pixel_size(48)
                column.pack_start(icon, False, False, 0)
                name = text(entry["name"], None, True)
                name.set_xalign(.5)
                name.set_justify(Gtk.Justification.CENTER)
                name.set_max_width_chars(17)
                name.set_lines(2)
                name.set_ellipsize(Pango.EllipsizeMode.END)
                column.pack_start(name, False, False, 0)
                button.add(column)
                button.set_tooltip_text(entry["name"])
                usable = entry["kind"] in {"file", "directory"}
                button.set_sensitive(usable)
                if not usable:
                    button.set_tooltip_text(entry["name"] + " · 此面板不會開啟捷徑或特殊檔案")
                button.connect("clicked", lambda _button, selected=entry["path"]: self.activate_path(selected))
                self.grid.add(button)
            self.grid.show_all()
            if not rows and result["truncated"]:
                self.caption.set_text("此資料夾項目較多；已達單次讀取上限，尚未顯示所有檔案。")
            elif not rows:
                self.caption.set_text("這個資料夾還沒有檔案。Meadow 儲存的文件與圖片會出現在這裡。")
            elif result["truncated"]:
                self.caption.set_text(f"顯示前 {len(rows)} 個項目；此資料夾還有更多檔案。")
            else:
                self.caption.set_text(f"{len(rows)} 個項目")
            return False

        def open_validated(self, path, kind):
            if kind == "directory":
                self.navigate(path)
            else:
                on_open(path)

    return TasksWindow(), WorkFilesWindow()
