"""Draw host-IME preedit inside the existing native GTK chat composer.

No second toplevel, editable field, draft mutation, or submit path is created.
The existing buffer continues to own selection/commit. While composing, its
text is painted with an underlined insertion on one transparent GTK overlay.
"""
import gi
gi.require_version("PangoCairo", "1.0")
from gi.repository import Gdk, Gtk, Pango, PangoCairo
from native_ime import NativeIMETransport


class ChatPreedit:
    def __init__(self, window, input_view, placeholder, state_dir):
        self.window, self.input, self.placeholder = window, input_view, placeholder
        self.active, self.text, self.cursor = False, "", 0
        self.painted_caret = None
        self.closed = False
        self.foreground = None
        self.previous_cursor = True
        self.connections = []
        self.scroll = self.input.get_ancestor(Gtk.ScrolledWindow)
        self.minimum_height = self.scroll.get_min_content_height() if self.scroll else -1
        self.overlay = self.input.get_ancestor(Gtk.Overlay)
        if self.overlay is None:
            raise ValueError("Native preedit requires the existing composer overlay")
        self.paint = Gtk.DrawingArea()
        self.paint.set_no_show_all(True)
        self.paint.set_can_focus(False)
        self.paint.set_halign(Gtk.Align.FILL)
        self.paint.set_valign(Gtk.Align.FILL)
        self.overlay.add_overlay(self.paint)
        self.overlay.set_overlay_pass_through(self.paint, True)
        self.paint.connect("draw", self.draw)
        self.transparent = Gtk.CssProvider()
        self.transparent.load_from_data(b"textview, textview text { color: transparent; caret-color: transparent; }")
        self.transport = NativeIMETransport(state_dir, self.preedit)
        self.transport.before_poll = self.publish_caret
        for obj, signal, callback in (
                (self.input, "focus-in-event", self.changed),
                (self.input, "focus-out-event", self.changed),
                (self.input, "size-allocate", self.changed),
                (self.input.get_buffer(), "mark-set", self.changed),
                (self.input.get_buffer(), "changed", self.buffer_changed),
                (self.window, "configure-event", self.changed),
                (self.window, "notify::is-active", self.changed),
                (self.window, "map-event", self.changed),
                (self.window, "unmap-event", self.changed),
                (self.window, "destroy", self.close)):
            self.connections.append((obj, obj.connect(signal, callback)))
        if self.scroll:
            for adjustment in (self.scroll.get_hadjustment(), self.scroll.get_vadjustment()):
                self.connections.append((adjustment, adjustment.connect("value-changed", self.changed)))
        self.transport.start()
        self.publish_caret()

    @staticmethod
    def _xy(value):
        if value is None:
            raise ValueError("Widget coordinates are unavailable")
        return value[-2], value[-1]

    def changed(self, *_args):
        self.painted_caret = None
        self.publish_caret()
        if self.active:
            self.paint.queue_draw()
        return False

    def buffer_changed(self, *_args):
        # RFB commits arrive through the original GtkTextBuffer. Invalidate
        # before repaint so a delayed old preedit cannot duplicate that commit.
        self.transport.invalidate()
        self.preedit({"active": False})
        self.changed()

    def publish_caret(self):
        if self.closed or not self.transport.enabled:
            return
        focused = (self.window.get_visible() and self.window.get_mapped()
                   and self.window.is_active() and self.input.has_focus()
                   and self.input.get_sensitive() and self.input.get_realized())
        caret = None
        if focused:
            try:
                origin = self._xy(self.input.get_window(Gtk.TextWindowType.WIDGET).get_origin())
                iterator = self.input.get_buffer().get_iter_at_mark(self.input.get_buffer().get_insert())
                rect = self.input.get_iter_location(iterator)
                x, y = self.input.buffer_to_window_coords(Gtk.TextWindowType.WIDGET, rect.x, rect.y)
                if self.active and self.painted_caret:
                    x, y, height = self.painted_caret
                else:
                    height = max(1, rect.height)
                caret = {"x": origin[0] + x, "y": origin[1] + y, "width": 1, "height": height}
            except (AttributeError, TypeError, ValueError):
                focused = False
        if focused != self.transport.state["active"] or caret != self.transport.state.get("caret"):
            self.transport.publish(active=bool(focused), caret=caret)

    def preedit(self, event):
        active = bool(event.get("active")) and bool(event.get("text"))
        if active and not self.active:
            self.foreground = self.input.get_style_context().get_color(Gtk.StateFlags.NORMAL)
            self.previous_cursor = self.input.get_cursor_visible()
            self.input.get_style_context().add_provider(self.transparent, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 100)
            self.input.set_cursor_visible(False)
        elif not active and self.active:
            self.input.get_style_context().remove_provider(self.transparent)
            self.input.set_cursor_visible(self.previous_cursor)
            if self.scroll:
                self.scroll.set_min_content_height(self.minimum_height)
        self.active = active
        self.text = event.get("text", "") if active else ""
        self.cursor = event.get("cursor", 0) if active else 0
        self.painted_caret = None
        self.placeholder.set_visible(not active and self.input.get_buffer().get_char_count() == 0)
        self.paint.set_visible(active)
        self.input.queue_draw()
        self.paint.queue_draw()

    def draw(self, _widget, cr):
        if not self.active or not self.input.get_realized():
            return False
        buffer = self.input.get_buffer()
        start, end = buffer.get_bounds()
        insertion = buffer.get_iter_at_mark(buffer.get_insert())
        selected = buffer.get_selection_bounds()
        left, right = selected[-2:] if selected else (insertion, insertion)
        prefix = buffer.get_text(start, left, True)
        suffix = buffer.get_text(right, end, True)
        layout = self.input.create_pango_layout(prefix + self.text + suffix)
        layout.set_width(max(1, self.input.get_allocated_width() - self.input.get_left_margin()
                             - self.input.get_right_margin()) * Pango.SCALE)
        layout.set_wrap(Pango.WrapMode.WORD_CHAR)
        attrs = Pango.AttrList()
        underline = Pango.attr_underline_new(Pango.Underline.SINGLE)
        underline.start_index = len(prefix.encode("utf-8"))
        underline.end_index = len((prefix + self.text).encode("utf-8"))
        attrs.insert(underline)
        layout.set_attributes(attrs)
        rect = self.input.get_iter_location(start)
        x, y = self.input.buffer_to_window_coords(Gtk.TextWindowType.WIDGET, rect.x, rect.y)
        offset_x, offset_y = self._xy(self.input.translate_coordinates(self.paint, 0, 0))
        caret, _weak = layout.get_cursor_pos(len((prefix + self.text[:self.cursor]).encode("utf-8")))
        height = max(1, caret.height / Pango.SCALE)
        if self.scroll:
            maximum = self.scroll.get_max_content_height()
            desired = layout.get_pixel_size()[1] + self.input.get_top_margin() + self.input.get_bottom_margin()
            bounded = max(self.minimum_height, min(maximum if maximum > 0 else 84, desired))
            if self.scroll.get_min_content_height() != bounded:
                self.scroll.set_min_content_height(bounded)
        # The ephemeral text may wrap beyond the actual buffer's lines. Keep
        # the composing caret visible without changing the buffer or its scroll.
        bottom = y + caret.y / Pango.SCALE + height + self.input.get_bottom_margin()
        y -= max(0, bottom - self.input.get_allocated_height())
        if y + caret.y / Pango.SCALE < 0:
            y = -caret.y / Pango.SCALE
        cr.save()
        cr.rectangle(offset_x, offset_y, self.input.get_allocated_width(), self.input.get_allocated_height())
        cr.clip()
        cr.move_to(offset_x + x, offset_y + y)
        color = self.foreground
        cr.set_source_rgba(color.red, color.green, color.blue, color.alpha)
        PangoCairo.show_layout(cr, layout)
        cx, cy = x + caret.x / Pango.SCALE, y + caret.y / Pango.SCALE
        height = max(1, caret.height / Pango.SCALE)
        cr.set_line_width(1)
        cr.move_to(offset_x + cx, offset_y + cy)
        cr.line_to(offset_x + cx, offset_y + cy + height)
        cr.stroke()
        cr.restore()
        self.painted_caret = (cx, cy, height)
        return False

    def close(self, *_args):
        if self.closed:
            return
        self.transport.close()
        self.preedit({"active": False})
        self.closed = True
        for obj, connection in self.connections:
            obj.disconnect(connection)
        self.connections.clear()
