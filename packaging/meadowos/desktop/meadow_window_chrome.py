"""Shared chrome for real Meadow GTK windows; no Agent or application lifetime policy.

The existing top-level Gtk.Window remains the only window. Controls and content
share a vertical allocation, so the toolbar never covers the document area.
GTK is loaded only when the chrome is instantiated, keeping status tools usable
on systems without a display.
"""
from __future__ import annotations

from functools import lru_cache
import math
from pathlib import Path

TOOLBAR_HEIGHT = 30
TRIGGER_HEIGHT = 12
CORNER_RADIUS = 16
HIDE_DELAY_MS = 380
TRANSITION_MS = 140


def can_auto_hide(*, sticky=False, focused=False, pointer_inside=False, pressed=False):
    """A pending leave must not dismiss controls that are still being used."""
    return not (sticky or focused or pointer_inside or pressed)


def rounded_input_rows(width, height, radius):
    """Integer input mask matching the visual outside corners in logical pixels."""
    if width <= 0 or height <= 0:
        return []
    radius = min(max(0, int(radius)), width // 2, height // 2)
    if not radius:
        return [(0, 0, width, height)]
    rows = [(0, radius, width, height - 2 * radius)] if height > 2 * radius else []
    for y in range(radius):
        inset = max(0, math.ceil(radius - math.sqrt(max(0, radius * radius - (radius - y - .5) ** 2)) - .5))
        rows.extend(((inset, y, width - 2 * inset, 1),
                     (inset, height - y - 1, width - 2 * inset, 1)))
    return rows


@lru_cache(maxsize=1)
def _native_type():
    import gi
    gi.require_version("Gtk", "3.0")
    gi.require_version("Gdk", "3.0")
    import cairo
    from gi.repository import Gdk, GLib, Gtk

    css = Gtk.CssProvider()
    css.load_from_path(str(Path(__file__).with_name("window-chrome.css")))
    Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), css,
                                             Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 10)

    class HoverChrome(Gtk.Overlay):
        def __init__(self, owner, content, *, title, close=None, minimize=None,
                     maximize=None, focus_content=None, before_move=None, allow_pin=False,
                     geometry_changed=None, title_widget=None, trailing_widget=None, toolbar_height=None):
            super().__init__()
            self.owner, self.content = owner, content
            self.focus_content, self.before_move = focus_content, before_move
            self.geometry_changed, self.reported_geometry = geometry_changed, None
            self.hide_timer = self.fade_timer = None
            self.sticky, self.alive, self.fullscreen = False, True, False
            self.radius = CORNER_RADIUS
            self.mask_size = None
            self.restore_focus = None
            self.close_action = close or owner.close
            self.minimize_action = minimize or owner.iconify
            self.maximize_action = maximize or self.toggle_maximize
            owner.set_decorated(False)
            owner.get_style_context().add_class("meadow-hover-window")
            owner.get_style_context().add_class("meadow-hover-chrome-owned")
            self.get_style_context().add_class("meadow-hover-frame")
            content.get_style_context().add_class("meadow-hover-content")
            self.layout = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=0)
            self.add(self.layout)
            screen = self.screen = owner.get_screen()
            self.visual = screen.get_rgba_visual()
            if self.visual:
                owner.set_visual(self.visual)
            self.composited = bool(self.visual and screen.is_composited())
            self.screen_handler = screen.connect("composited-changed", self.composition_changed)
            self.composition_changed()

            self.trigger = Gtk.EventBox()
            self.trigger.set_visible_window(False)
            self.trigger.set_above_child(True)
            self.trigger.set_halign(Gtk.Align.FILL)
            self.trigger.set_valign(Gtk.Align.START)
            self.trigger.set_size_request(-1, TRIGGER_HEIGHT)
            self.trigger.set_no_show_all(True)
            self.trigger.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK | Gdk.EventMask.LEAVE_NOTIFY_MASK
                                    | Gdk.EventMask.BUTTON_PRESS_MASK | Gdk.EventMask.TOUCH_MASK)
            self.trigger.connect("enter-notify-event", lambda *_: self.reveal())
            self.trigger.connect("leave-notify-event", self.schedule_hide)
            self.trigger.connect("button-press-event", self.trigger_press)
            self.trigger.connect("touch-event", self.trigger_touch)
            self.add_overlay(self.trigger)
            self.set_overlay_pass_through(self.trigger, True)

            self.revealer = Gtk.Revealer()
            self.revealer.set_transition_type(Gtk.RevealerTransitionType.SLIDE_DOWN)
            self.revealer.set_transition_duration(TRANSITION_MS)
            self.revealer.set_halign(Gtk.Align.FILL)
            self.revealer.set_valign(Gtk.Align.START)
            self.revealer.set_no_show_all(True)
            self.bar = Gtk.EventBox()
            self.bar.set_visible_window(True)
            self.bar.get_style_context().add_class("meadow-hover-toolbar")
            self.bar.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK | Gdk.EventMask.LEAVE_NOTIFY_MASK)
            self.bar.connect("enter-notify-event", lambda *_: self.reveal())
            self.bar.connect("leave-notify-event", self.schedule_hide)
            row = Gtk.Box(spacing=0)
            row.set_margin_start(14)
            row.set_margin_end(12)
            row.set_size_request(-1, toolbar_height or TOOLBAR_HEIGHT)
            self.buttons = []
            from window_controls import window_button
            for style, text, callback in (
                ("close", "關閉視窗", self.close_action),
                ("minimize", "最小化視窗", self.minimize_action),
                ("maximize", "最大化／還原視窗", self.maximize_action),
            ):
                button = window_button(style, text, lambda action=callback: self.activate(action))
                row.pack_start(button, False, False, 0)
                self.buttons.append(button)
            self.buttons[2].set_sensitive(owner.get_resizable())

            drag = Gtk.EventBox()
            drag.set_visible_window(False)
            drag.set_above_child(True)
            drag.get_accessible().set_name("拖曳移動視窗；雙擊最大化或還原")
            drag.add_events(Gdk.EventMask.BUTTON_PRESS_MASK)
            drag.connect("button-press-event", self.drag_press)
            caption = title_widget if title_widget is not None else Gtk.Label(label=title)
            caption.get_style_context().add_class("meadow-window-title")
            drag.add(caption)
            row.pack_start(drag, True, True, 0)
            self.pin_button = None
            self.overlay_above = False
            if allow_pin:
                pin = Gtk.ToggleButton()
                self.pin_button = pin
                pin.get_style_context().add_class("meadow-window-pin")
                from gi.repository import Gio
                pin_icon = Gio.FileIcon.new(Gio.File.new_for_path(str(
                    Path(__file__).with_name("assets") / "window-controls" / "pin-symbolic.svg")))
                pin_image = Gtk.Image.new_from_gicon(pin_icon, Gtk.IconSize.MENU)
                pin_image.set_pixel_size(16)
                pin_image.set_halign(Gtk.Align.CENTER)
                pin_image.set_valign(Gtk.Align.CENTER)
                pin.add(pin_image)
                pin.set_focus_on_click(False)
                pin.set_tooltip_text("保持在最上層")
                pin.get_accessible().set_name("保持在最上層")
                pin.connect("toggled", lambda button: self.set_overlay_above(self.overlay_above))
                row.pack_end(pin, False, False, 0)
            if trailing_widget is not None:
                row.pack_end(trailing_widget, False, False, 0)
            self.bar.add(row)
            self.revealer.add(self.bar)
            self.layout.pack_start(self.revealer, False, False, 0)
            self.layout.pack_start(content, True, True, 0)
            self.bar.show_all()
            self.revealer.hide()
            self.bar.set_sensitive(False)


            self.edges = []
            for edge, horizontal, vertical, width, height, cursor in (
                (Gdk.WindowEdge.NORTH, Gtk.Align.FILL, Gtk.Align.START, -1, 4, "n-resize"),
                (Gdk.WindowEdge.SOUTH, Gtk.Align.FILL, Gtk.Align.END, -1, 5, "s-resize"),
                (Gdk.WindowEdge.WEST, Gtk.Align.START, Gtk.Align.FILL, 5, -1, "w-resize"),
                (Gdk.WindowEdge.EAST, Gtk.Align.END, Gtk.Align.FILL, 5, -1, "e-resize"),
                (Gdk.WindowEdge.NORTH_WEST, Gtk.Align.START, Gtk.Align.START, 8, 8, "nw-resize"),
                (Gdk.WindowEdge.NORTH_EAST, Gtk.Align.END, Gtk.Align.START, 8, 8, "ne-resize"),
                (Gdk.WindowEdge.SOUTH_WEST, Gtk.Align.START, Gtk.Align.END, 8, 8, "sw-resize"),
                (Gdk.WindowEdge.SOUTH_EAST, Gtk.Align.END, Gtk.Align.END, 8, 8, "se-resize"),
            ):
                hit = Gtk.EventBox()
                hit.set_visible_window(False)
                hit.set_above_child(True)
                hit.set_halign(horizontal)
                hit.set_valign(vertical)
                hit.set_size_request(width, height)
                hit.set_no_show_all(True)
                hit.add_events(Gdk.EventMask.BUTTON_PRESS_MASK | Gdk.EventMask.ENTER_NOTIFY_MASK | Gdk.EventMask.LEAVE_NOTIFY_MASK)
                hit.connect("button-press-event", lambda _widget, event, selected=edge: self.resize_press(event, selected))
                hit.connect("enter-notify-event", lambda _widget, event, selected=cursor: self.edge_enter(event, selected))
                hit.connect("leave-notify-event", self.edge_leave)
                if vertical == Gtk.Align.START:
                    hit.connect("enter-notify-event", lambda *_: self.reveal())
                self.add_overlay(hit)
                hit.show()
                self.edges.append(hit)
            owner.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK | Gdk.EventMask.LEAVE_NOTIFY_MASK)
            owner.connect("enter-notify-event", lambda *_: self.reveal())
            owner.connect("leave-notify-event", self.schedule_hide)
            self.pointer_timer = GLib.timeout_add(100, self.poll_pointer)
            owner.connect("key-press-event", self.window_key)
            owner.connect("window-state-event", self.window_state)
            owner.connect("unmap", self.unmapped)
            owner.connect("destroy", self.destroyed)
            self.connect("size-allocate", self.allocation_changed)

        def set_overlay_above(self, enabled):
            """Temporary overlay layering never changes the user's pin choice."""
            self.overlay_above = bool(enabled)
            pinned = bool(self.pin_button and self.pin_button.get_active())
            self.owner.set_keep_above(self.overlay_above or pinned)

        def composition_changed(self, *_args):
            if not self.alive:
                return
            self.composited = bool(self.visual and self.owner.get_screen().is_composited())
            style = self.owner.get_style_context()
            (style.add_class if self.composited else style.remove_class)("meadow-hover-composited")
            self.mask_size = None
            self.allocation_changed(self, self.get_allocation())
            self.notify_material_geometry()
            self.queue_draw()

        def notify_material_geometry(self):
            if not self.alive or self.geometry_changed is None:
                return
            geometry = (self.radius if self.composited else 0,
                        self.owner.get_style_context().has_class("meadow-chrome-visible"))
            if geometry != self.reported_geometry:
                self.reported_geometry = geometry
                self.geometry_changed(*geometry)

        def do_draw(self, context):
            allocation = self.get_allocation()
            radius = self.radius if self.composited else 0
            radius = min(radius, allocation.width / 2, allocation.height / 2)
            context.save()
            if radius:
                for x, y, start in ((allocation.width - radius, radius, -math.pi / 2),
                                    (allocation.width - radius, allocation.height - radius, 0),
                                    (radius, allocation.height - radius, math.pi / 2),
                                    (radius, radius, math.pi)):
                    context.arc(x, y, radius, start, start + math.pi / 2)
                context.close_path()
                context.clip()
            result = Gtk.Overlay.do_draw(self, context)
            context.restore()
            return result

        def allocation_changed(self, _widget, allocation):
            if not self.alive:
                return
            drawable = self.owner.get_window()
            if not drawable:
                return
            radius = self.radius if self.composited else 0
            key = (allocation.width, allocation.height, radius)
            if key == self.mask_size:
                return
            self.mask_size = key
            region = cairo.Region()
            for rectangle in rounded_input_rows(*key):
                region.union(cairo.RectangleInt(*rectangle))
            drawable.input_shape_combine_region(region, 0, 0)

        def activate(self, callback):
            self.dismiss(focus_input=True)
            callback()

        def cancel_hide(self):
            if self.hide_timer:
                GLib.source_remove(self.hide_timer)
                self.hide_timer = None

        def reveal(self, *_args, sticky=False, keyboard=False):
            if not self.alive or self.fullscreen:
                return False
            self.cancel_hide()
            if self.fade_timer:
                GLib.source_remove(self.fade_timer)
                self.fade_timer = None
            if not self.revealer.get_reveal_child():
                self.restore_focus = self.owner.get_focus()
            self.sticky = self.sticky or sticky or keyboard
            self.owner.get_style_context().add_class("meadow-chrome-visible")
            self.bar.set_sensitive(True)
            self.revealer.show()
            self.revealer.set_reveal_child(True)

            self.notify_material_geometry()
            if keyboard:
                self.buttons[0].grab_focus()
            return False

        def toggle_controls(self):
            if self.revealer.get_reveal_child() and self.sticky:
                self.dismiss(focus_input=True)
            else:
                self.reveal(sticky=True)

        def controls_focused(self):
            focus = self.owner.get_focus()
            return bool(focus and (focus is self.bar or focus.is_ancestor(self.bar)))

        def pointer_status(self):
            drawable = self.owner.get_window()
            seat = self.owner.get_display().get_default_seat()
            pointer = seat.get_pointer() if seat else None
            if not drawable or not pointer:
                return False, False
            _child, x, y, modifiers = drawable.get_device_position(pointer)
            # get_device_position returns a child, which is None over GTK's
            # windowless body. Query the actual drawable under the pointer so
            # plain content reveals controls and foreign windows still occlude.
            under_pointer, _px, _py = pointer.get_window_at_position()
            allocation = self.get_allocation()
            translated = self.translate_coordinates(self.owner, 0, 0)
            origin_x, origin_y = translated if translated else (0, 0)
            same_window = bool(under_pointer and under_pointer.get_effective_toplevel() == drawable.get_effective_toplevel())
            inside = (same_window and origin_x <= x < origin_x + allocation.width
                      and origin_y <= y < origin_y + allocation.height)
            pressed = bool(modifiers & (Gdk.ModifierType.BUTTON1_MASK | Gdk.ModifierType.BUTTON2_MASK | Gdk.ModifierType.BUTTON3_MASK))
            return inside, pressed

        def poll_pointer(self):
            if not self.alive:
                return False
            if self.owner.get_mapped() and not self.fullscreen:
                inside, _pressed = self.pointer_status()
                if inside:
                    self.reveal()
                elif self.revealer.get_reveal_child() and not self.hide_timer:
                    self.schedule_hide()
            return True

        def schedule_hide(self, _widget=None, event=None):
            if not self.alive:
                return False
            if event is not None and event.detail == Gdk.NotifyType.INFERIOR:
                return False
            self.cancel_hide()
            self.hide_timer = GLib.timeout_add(HIDE_DELAY_MS, self.hide_if_idle)
            return False

        def hide_if_idle(self):
            self.hide_timer = None
            if not self.alive:
                return False
            inside, pressed = self.pointer_status()
            if can_auto_hide(sticky=self.sticky, focused=self.controls_focused(),
                             pointer_inside=inside, pressed=pressed):
                self.dismiss()
            elif not self.sticky:
                # WM move/resize grabs may consume the eventual release event.
                self.hide_timer = GLib.timeout_add(HIDE_DELAY_MS, self.hide_if_idle)
            return False

        def dismiss(self, focus_input=False):
            if not self.alive:
                return
            self.cancel_hide()
            self.sticky = False
            if focus_input:
                if self.focus_content:
                    self.focus_content()
                elif self.restore_focus and self.restore_focus.get_visible():
                    self.restore_focus.grab_focus()
            self.revealer.set_reveal_child(False)
            self.bar.set_sensitive(False)

            if self.fade_timer:
                GLib.source_remove(self.fade_timer)
            self.fade_timer = GLib.timeout_add(TRANSITION_MS + 20, self.finish_hiding)

        def finish_hiding(self):
            self.fade_timer = None
            if not self.alive:
                return False
            if not self.revealer.get_reveal_child():
                self.revealer.hide()
                self.owner.get_style_context().remove_class("meadow-chrome-visible")
                self.notify_material_geometry()
            return False

        def trigger_press(self, _widget, event):
            if event.button == 1:
                self.reveal(sticky=True)
                return True
            return False

        def trigger_touch(self, _widget, event):
            if event.type == Gdk.EventType.TOUCH_BEGIN:
                self.reveal(sticky=True)
                return True
            return False

        def toggle_maximize(self):
            window = self.owner.get_window()
            if not self.owner.get_resizable():
                return
            if window and window.get_state() & Gdk.WindowState.MAXIMIZED:
                self.owner.unmaximize()
            else:
                if self.before_move:
                    self.before_move()
                self.owner.maximize()

        def drag_press(self, _widget, event):
            if event.button != 1:
                return False
            if event.type == Gdk.EventType.DOUBLE_BUTTON_PRESS:
                self.maximize_action()
            elif event.type == Gdk.EventType.BUTTON_PRESS:
                if self.before_move:
                    self.before_move()
                self.owner.begin_move_drag(event.button, int(event.x_root), int(event.y_root), event.time)
                self.schedule_hide()
            return True

        def resize_press(self, event, edge):
            window = self.owner.get_window()
            special = window and window.get_state() & (Gdk.WindowState.MAXIMIZED | Gdk.WindowState.FULLSCREEN)
            if event.button != 1 or event.type != Gdk.EventType.BUTTON_PRESS or special or not self.owner.get_resizable():
                return False
            if self.before_move:
                self.before_move()
            self.owner.begin_resize_drag(edge, event.button, int(event.x_root), int(event.y_root), event.time)
            self.schedule_hide()
            return True

        def edge_enter(self, event, cursor):
            if self.owner.get_resizable() and self.radius:
                event.window.set_cursor(Gdk.Cursor.new_from_name(self.owner.get_display(), cursor))
            return False

        def edge_leave(self, _widget, event):
            event.window.set_cursor(None)
            return self.schedule_hide(_widget, event)

        def window_key(self, _window, event):
            if not self.alive:
                return False
            modifiers = event.state & (Gdk.ModifierType.MOD1_MASK | Gdk.ModifierType.CONTROL_MASK
                                      | Gdk.ModifierType.SHIFT_MASK | Gdk.ModifierType.SUPER_MASK)
            shortcut = (event.keyval == Gdk.KEY_F10 and not modifiers) or (
                event.keyval == Gdk.KEY_space and modifiers == (
                    Gdk.ModifierType.CONTROL_MASK | Gdk.ModifierType.MOD1_MASK))
            if shortcut:
                self.reveal(keyboard=True)
                return not self.fullscreen
            if event.keyval == Gdk.KEY_Escape and self.revealer.get_reveal_child():
                self.dismiss(focus_input=True)
                return True
            return False

        def window_state(self, _window, event):
            if not self.alive:
                return False
            self.fullscreen = bool(event.new_window_state & Gdk.WindowState.FULLSCREEN)
            special = bool(event.new_window_state & (Gdk.WindowState.MAXIMIZED | Gdk.WindowState.FULLSCREEN))
            self.radius = 0 if special else CORNER_RADIUS
            style = self.owner.get_style_context()
            (style.add_class if special else style.remove_class)("meadow-chrome-square")
            self.mask_size = None
            self.allocation_changed(self, self.get_allocation())
            self.notify_material_geometry()
            self.queue_draw()
            if self.fullscreen:
                self.dismiss()
                self.trigger.hide()
            else:
                self.trigger.hide()
            # Returning False from a resize handler does not remove its input
            # window. Unmap every resize strip while maximized/fullscreen so
            # real content at the screen edge remains clickable. show_all()
            # cannot bring them back; only a restored normal state can.
            for edge in self.edges:
                edge.set_visible(not special)
            return False

        def unmapped(self, *_args):
            self.cancel_hide()
            if self.fade_timer:
                GLib.source_remove(self.fade_timer)
                self.fade_timer = None
            self.sticky = False
            self.revealer.set_reveal_child(False)
            self.revealer.hide()
            self.bar.set_sensitive(False)

            self.owner.get_style_context().remove_class("meadow-chrome-visible")
            self.notify_material_geometry()

        def destroyed(self, *_args):
            if not self.alive:
                return
            self.alive = False
            if self.pointer_timer:
                GLib.source_remove(self.pointer_timer)
                self.pointer_timer = None
            try:
                self.unmapped()
            finally:
                handler, self.screen_handler = self.screen_handler, None
                if handler:
                    self.screen.disconnect(handler)

    return HoverChrome


def create_hover_chrome(owner, content, **options):
    return _native_type()(owner, content, **options)
