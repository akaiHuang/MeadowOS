"""Bounded, per-composer undo for plain GTK 3 TextViews.

GTK 3 does not supply TextBuffer undo. Native begin/end-user-action signals
define atomic edits (including paste, cut and replacement); programmatic buffer
replacement is a history boundary. IME preedit is presentation-only and never
enters this history. No draft text is persisted by this helper.
"""
from dataclasses import dataclass


@dataclass(frozen=True)
class TextSnapshot:
    text: str
    insert: int
    bound: int


@dataclass(frozen=True)
class TextEdit:
    before: TextSnapshot
    after: TextSnapshot
    bytes_used: int


class EditHistory:
    """Whole-edit snapshots, capped by steps and combined UTF-8 text bytes."""

    def __init__(self, max_steps=100, max_bytes=2 * 1024 * 1024):
        self.max_steps = max(0, int(max_steps))
        self.max_bytes = max(0, int(max_bytes))
        self.undo_steps = []
        self.redo_steps = []
        self.bytes_used = 0

    def clear(self):
        self.undo_steps.clear()
        self.redo_steps.clear()
        self.bytes_used = 0

    def record(self, before, after):
        if before.text == after.text:
            return
        self.bytes_used -= sum(edit.bytes_used for edit in self.redo_steps)
        self.redo_steps.clear()
        cost = len(before.text.encode("utf-8")) + len(after.text.encode("utf-8"))
        if not self.max_steps or cost > self.max_bytes:
            # Retaining older undo across an unrecorded large edit would apply
            # a stale snapshot and erase newer text. Make it a clean boundary.
            self.clear()
            return
        self.undo_steps.append(TextEdit(before, after, cost))
        self.bytes_used += cost
        while len(self.undo_steps) > self.max_steps or self.bytes_used > self.max_bytes:
            self.bytes_used -= self.undo_steps.pop(0).bytes_used

    def undo(self):
        if not self.undo_steps:
            return None
        edit = self.undo_steps.pop()
        self.redo_steps.append(edit)
        return edit.before

    def redo(self):
        if not self.redo_steps:
            return None
        edit = self.redo_steps.pop()
        self.undo_steps.append(edit)
        return edit.after


class TextEditingHistory:
    """Attach once, after restoring a draft; reset after typed send admission.

    Successful typed submission must call reset even if a newer draft remains
    in the buffer. Voice submission and failed admission must leave it alone.
    This prevents undo from resurrecting already-submitted text.
    """

    def __init__(self, view, max_steps=100, max_bytes=2 * 1024 * 1024,
                 composition_active=None):
        self.view = view
        self.preedit_text = ""
        self.composition_active = composition_active
        self.history = EditHistory(max_steps, max_bytes)
        self.buffer = None
        self.depth = 0
        self.before = None
        self.applying = False
        self.closed = False
        self.buffer_connections = []
        self.view_connections = [
            view.connect("key-press-event", self.handle_key),
            view.connect("preedit-changed", self.preedit_changed),
            view.connect("notify::buffer", self.buffer_replaced),
            view.connect("destroy", self.close),
        ]
        self.buffer_replaced()

    def preedit_changed(self, _view, text):
        self.preedit_text = text or ""

    def is_composing(self):
        """Native IME or the existing host-preedit overlay owns this edit."""
        return bool(self.preedit_text or
                    (self.composition_active and self.composition_active()))

    def snapshot(self):
        start, end = self.buffer.get_bounds()
        return TextSnapshot(self.buffer.get_text(start, end, True),
                            self.buffer.get_iter_at_mark(self.buffer.get_insert()).get_offset(),
                            self.buffer.get_iter_at_mark(self.buffer.get_selection_bound()).get_offset())

    def buffer_replaced(self, *_args):
        if self.closed:
            return
        for signal in self.buffer_connections:
            self.buffer.disconnect(signal)
        self.buffer = self.view.get_buffer()
        self.buffer_connections = [
            self.buffer.connect("begin-user-action", self.begin_action),
            self.buffer.connect("end-user-action", self.end_action),
            self.buffer.connect("changed", self.changed),
        ]
        self.depth = 0
        self.before = None
        self.history.clear()

    def begin_action(self, *_args):
        if self.applying:
            return
        if not self.depth:
            self.before = self.snapshot()
        self.depth += 1

    def end_action(self, *_args):
        if self.applying or not self.depth:
            return
        self.depth -= 1
        if not self.depth:
            self.history.record(self.before, self.snapshot())
            self.before = None

    def changed(self, *_args):
        if not self.applying and not self.depth:
            self.reset()

    def reset(self):
        """Keep current draft and selection, but start a fresh editing history."""
        self.history.clear()
        self.before = self.snapshot() if self.depth else None

    def restore(self, snapshot):
        self.applying = True
        try:
            self.buffer.set_text(snapshot.text)
            length = self.buffer.get_char_count()
            self.buffer.select_range(
                self.buffer.get_iter_at_offset(min(length, max(0, snapshot.insert))),
                self.buffer.get_iter_at_offset(min(length, max(0, snapshot.bound))))
            self.view.scroll_mark_onscreen(self.buffer.get_insert())
        finally:
            self.applying = False

    def step(self, redo=False):
        if (self.closed or self.depth or self.is_composing()
                or not self.view.get_editable() or not self.view.get_sensitive()):
            return False
        snapshot = self.history.redo() if redo else self.history.undo()
        if snapshot is None:
            return False
        self.restore(snapshot)
        return True

    def handle_key(self, _view, event):
        from gi.repository import Gdk
        if self.is_composing():
            return False
        state = event.state
        if not state & Gdk.ModifierType.CONTROL_MASK:
            return False
        if state & (Gdk.ModifierType.MOD1_MASK | Gdk.ModifierType.SUPER_MASK | Gdk.ModifierType.META_MASK):
            return False
        key = Gdk.keyval_to_lower(event.keyval)
        if key not in (Gdk.KEY_z, Gdk.KEY_y):
            return False
        if self.closed or not self.view.get_editable() or not self.view.get_sensitive():
            return False
        self.step(redo=key == Gdk.KEY_y or bool(state & Gdk.ModifierType.SHIFT_MASK))
        return True

    def close(self, *_args):
        if self.closed:
            return
        self.closed = True
        for signal in self.buffer_connections:
            self.buffer.disconnect(signal)
        for signal in self.view_connections:
            self.view.disconnect(signal)
        self.buffer_connections.clear()
        self.view_connections.clear()
        self.history.clear()
        self.before = None
        self.preedit_text = ""
