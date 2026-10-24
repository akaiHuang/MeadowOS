"""Persistent image draft, with paged thumbnails and serialized disk staging."""
from __future__ import annotations
import json
from pathlib import Path
import queue
import threading
import uuid
from attachment_cache import AttachmentCache, AttachmentCacheError


class AttachmentComposer:
    PAGE_SIZE = 12

    def __init__(self, state_root, changed, failed):
        from gi.repository import Gtk, GLib
        self.Gtk, self.GLib = Gtk, GLib
        self.changed, self.failed = changed, failed
        self.path = Path(state_root) / 'chat-attachment-draft.json'
        self.cache = AttachmentCache(Path(state_root) / 'attachment-cache')
        self.items, self.page, self.pending = [], 0, 0
        self.request_id, self.attempt_ids, self.attempt_text = None, [], None
        try:
            saved = json.loads(self.path.read_text())
            self.items = [self.cache.get(identity) for identity in saved.get('ids', [])]
            self.request_id = saved.get('request_id')
            self.attempt_ids = saved.get('attempt_ids', [])
            self.attempt_text = saved.get('attempt_text')
            if self.request_id:
                uuid.UUID(self.request_id)
        except (OSError, ValueError, TypeError, AttachmentCacheError):
            self.items, self.request_id, self.attempt_ids = [], None, []
        self.widget = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6)
        self.widget.get_style_context().add_class('chat-attachments')
        self.widget.set_no_show_all(True)
        self.summary = Gtk.Label(xalign=0)
        self.summary.get_style_context().add_class('chat-shortcut')
        self.widget.pack_start(self.summary, False, False, 0)
        self.cards = Gtk.Box(spacing=6)
        scroll = Gtk.ScrolledWindow()
        scroll.set_policy(Gtk.PolicyType.AUTOMATIC, Gtk.PolicyType.NEVER)
        scroll.set_min_content_height(64)
        scroll.add(self.cards)
        self.widget.pack_start(scroll, False, False, 0)
        self.paging = Gtk.Box(spacing=8)
        self.previous = Gtk.Button(label='‹')
        self.next = Gtk.Button(label='›')
        self.previous.connect('clicked', lambda *_: self.turn_page(-1))
        self.next.connect('clicked', lambda *_: self.turn_page(1))
        self.paging.pack_start(self.previous, False, False, 0)
        self.paging.pack_end(self.next, False, False, 0)
        self.widget.pack_start(self.paging, False, False, 0)
        self.jobs = queue.Queue()
        threading.Thread(target=self._worker, daemon=True, name='meadow-attachment-staging').start()
        self.render()

    def save(self):
        import os, tempfile
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd, temporary = tempfile.mkstemp(prefix='.draft-', dir=self.path.parent)
        try:
            with os.fdopen(fd, 'w') as stream:
                json.dump({'ids': [item['id'] for item in self.items], 'request_id': self.request_id,
                           'attempt_ids': self.attempt_ids, 'attempt_text': self.attempt_text}, stream)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.path)
        finally:
            if os.path.exists(temporary): os.unlink(temporary)

    def enqueue(self, paths):
        for path in paths:
            self.pending += 1
            self.jobs.put(path)
        self.render()

    def _worker(self):
        while True:
            path = self.jobs.get()
            try:
                record = self.cache.add(path)
            except (OSError, ValueError, AttachmentCacheError) as exc:
                self.GLib.idle_add(self._staged, None, str(exc))
            else:
                self.GLib.idle_add(self._staged, record, None)
            finally:
                self.jobs.task_done()

    def _staged(self, record, error):
        self.pending = max(0, self.pending - 1)
        if record: self.append(record)
        else: self.failed(error)
        self.render()
        return False

    def append(self, record):
        if not any(item['id'] == record['id'] for item in self.items):
            self.items.append(record)
            self.page = (len(self.items) - 1) // self.PAGE_SIZE
            self.save()
            self.render()
            self.changed()
        return False

    def remove(self, identity):
        # Removing a chip releases a draft reference. A queued task keeps its original.
        self.items = [item for item in self.items if item['id'] != identity]
        self.save()
        self.render()
        self.changed()

    def update(self, item):
        self.items = [item if current['id'] == item['id'] else current for current in self.items]
        self.render()
        return False

    def turn_page(self, delta):
        self.page += delta
        self.render()

    def snapshot(self, text=""):
        if self.pending:
            raise ValueError('圖片還在加入，請稍候再送出。')
        ids = [item['id'] for item in self.items]
        # Preserve the request identity after an uncertain receipt; never silently duplicate a task.
        if self.request_id and (self.attempt_ids != ids or self.attempt_text != text):
            raise ValueError('上次送出結果尚未確認，請先重新連線確認排隊訊息。')
        if not self.request_id:
            self.request_id = str(uuid.uuid4())
            self.attempt_ids = ids
            self.attempt_text = text
            self.save()
        return list(self.items), self.request_id

    def admitted(self, ids):
        self.items = [item for item in self.items if item['id'] not in ids]
        self.request_id, self.attempt_ids, self.attempt_text = None, [], None
        self.save()
        self.render()
        self.changed()

    def render(self):
        from gi.repository import GdkPixbuf, Pango
        for child in self.cards.get_children(): self.cards.remove(child)
        count = len(self.items)
        self.page = max(0, min(self.page, max(0, (count - 1) // self.PAGE_SIZE)))
        first = self.page * self.PAGE_SIZE
        for item in self.items[first:first + self.PAGE_SIZE]:
            card = self.Gtk.Box(orientation=self.Gtk.Orientation.VERTICAL, spacing=2)
            card.get_style_context().add_class('chat-attachment-card')
            overlay = self.Gtk.Overlay()
            preview = self.Gtk.Image.new_from_icon_name('image-x-generic', self.Gtk.IconSize.DIALOG)
            try:
                _fmt, width, height = GdkPixbuf.Pixbuf.get_file_info(item['path'])
                if width > 0 and height > 0 and width * height <= 64 * 1024 * 1024:
                    preview.set_from_pixbuf(GdkPixbuf.Pixbuf.new_from_file_at_scale(item['path'], 62, 49, True))
            except Exception:
                pass
            preview.set_size_request(62, 49)
            overlay.add(preview)
            remove = self.Gtk.Button(label='×')
            remove.get_style_context().add_class('attachment-remove')
            remove.set_halign(self.Gtk.Align.END)
            remove.set_valign(self.Gtk.Align.START)
            remove.set_tooltip_text('移除 ' + item['name'])
            remove.get_accessible().set_name('移除 ' + item['name'])
            remove.connect('clicked', lambda _button, identity=item['id']: self.remove(identity))
            overlay.add_overlay(remove)
            card.pack_start(overlay, False, False, 0)
            title = self.Gtk.Label(label=item['name'])
            title.set_ellipsize(Pango.EllipsizeMode.MIDDLE)
            title.set_max_width_chars(9)
            title.get_style_context().add_class('chat-shortcut')
            card.pack_start(title, False, False, 0)
            card.set_tooltip_text(item['name'] + (' · 等待上傳' if item.get('state') == 'staged' else ' · ' + item.get('state', '')))
            self.cards.pack_start(card, False, False, 0)
        suffix = f' · 正在加入 {self.pending} 張' if self.pending else ''
        self.summary.set_text(f'{count} 張圖片{suffix} · 分批處理')
        self.widget.set_visible(bool(count or self.pending))
        if count or self.pending:
            self.widget.set_no_show_all(False)
            self.widget.show_all()
            self.widget.set_no_show_all(True)
        self.paging.set_visible(count > self.PAGE_SIZE)
        self.previous.set_sensitive(self.page > 0)
        self.next.set_sensitive(first + self.PAGE_SIZE < count)
