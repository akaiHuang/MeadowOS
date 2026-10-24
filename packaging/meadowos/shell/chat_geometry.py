"""Keep the native companion inside its current monitor's usable desktop.

Window limits are independent of transcript length. GTK may otherwise enlarge
a toplevel to satisfy a permission card or a restored, now off-screen size.
"""
from __future__ import annotations


def usable_area(geometry, workarea, *, top=0, bottom=0):
    """Intersect the WM workarea with this monitor and our reserved panels."""
    gx, gy, gw, gh = geometry
    wx, wy, ww, wh = workarea
    left, right = max(gx, wx), min(gx + gw, wx + ww)
    upper, lower = max(gy + top, wy), min(gy + gh - bottom, wy + wh)
    # A transient/empty WM workarea must not create negative size hints.
    if right <= left or lower <= upper:
        return gx, gy, max(1, gw), max(1, gh)
    return left, upper, right - left, lower - upper


def bounded_geometry(position, size, area, *, margin=8):
    """Preserve the user's rectangle where it fits, including negative origins."""
    ax, ay, aw, ah = area
    inset_x, inset_y = min(margin, max(0, (aw - 1) // 2)), min(margin, max(0, (ah - 1) // 2))
    width = min(max(1, size[0]), max(1, aw - 2 * inset_x))
    height = min(max(1, size[1]), max(1, ah - 2 * inset_y))
    x = min(max(position[0], ax + inset_x), ax + aw - inset_x - width)
    y = min(max(position[1], ay + inset_y), ay + ah - inset_y - height)
    return (x, y), (width, height)


class ChatGeometry:
    """Event-driven GTK size hints and repair; no polling or content mutations."""
    def __init__(self, window, *, auxiliary_scroll=None):
        from gi.repository import Gdk, GLib
        self.Gdk, self.GLib = Gdk, GLib
        self.window, self.auxiliary_scroll = window, auxiliary_scroll
        self.display = window.get_display()
        self.pending = None
        self.limits = None
        self.applied_size = None
        self.repair_position = True
        self.alive = True
        self.monitor_handlers = []
        self.handlers = []
        self.xdisplay = None
        self.workarea_watch = None
        for signal in ("realize", "map-event"):
            self.handlers.append((window, window.connect(signal, self.request_fit)))
        for signal in ("configure-event", "window-state-event"):
            self.handlers.append((window, window.connect(signal, self.configure)))
        self.handlers.append((window, window.connect("destroy", self.destroy)))
        for signal in ("monitor-added", "monitor-removed"):
            self.handlers.append((self.display, self.display.connect(signal, self.monitors_changed)))
        screen = window.get_screen()
        self.handlers.append((screen, screen.connect("size-changed", self.request_fit)))
        self.monitors_changed()
        self.watch_workarea()
        self.cancel_pending()
        self.apply()

    @staticmethod
    def rectangle(rect):
        return rect.x, rect.y, rect.width, rect.height

    def current_area(self):
        drawable = self.window.get_window()
        monitor = self.display.get_monitor_at_window(drawable) if drawable else None
        primary = self.display.get_primary_monitor() or self.display.get_monitor(0)
        monitor = monitor or primary
        # These match Shell.layout's actual top-bar and Dock struts. Include
        # them before the WM publishes its workarea during startup/re-layout.
        top, bottom = (34, 98) if monitor == primary else (0, 0)
        return usable_area(self.rectangle(monitor.get_geometry()),
                           self.rectangle(monitor.get_workarea()), top=top, bottom=bottom)

    def monitors_changed(self, *_args):
        for monitor, handler in self.monitor_handlers:
            monitor.disconnect(handler)
        self.monitor_handlers = []
        for index in range(self.display.get_n_monitors()):
            monitor = self.display.get_monitor(index)
            for signal in ("notify::geometry", "notify::workarea"):
                self.monitor_handlers.append((monitor, monitor.connect(signal, self.request_fit)))
        self.request_fit()

    def watch_workarea(self):
        # GDK's X11 get_workarea() updates on root properties without reliably
        # notifying GdkMonitor. Use a separate read-only X connection so this
        # subscription cannot replace GTK's own root event mask.
        from Xlib import X, display
        self.xdisplay = display.Display(self.display.get_name())
        self.workarea_atoms = {self.xdisplay.intern_atom(name) for name in (
            "_NET_WORKAREA", "_NET_CURRENT_DESKTOP")}
        self.xdisplay.screen().root.change_attributes(event_mask=X.PropertyChangeMask)
        self.xdisplay.flush()
        self.workarea_watch = self.GLib.io_add_watch(self.xdisplay.fileno(),
            self.GLib.IOCondition.IN | self.GLib.IOCondition.HUP | self.GLib.IOCondition.ERR,
            self.workarea_event)

    def workarea_event(self, _channel, condition):
        if condition & (self.GLib.IOCondition.HUP | self.GLib.IOCondition.ERR):
            self.workarea_watch = None
            return False
        from Xlib import X
        changed = False
        while self.xdisplay.pending_events():
            event = self.xdisplay.next_event()
            if event.type == X.PropertyNotify and event.atom in self.workarea_atoms:
                changed = True
        if changed:
            self.request_fit()
        return True

    def cancel_pending(self):
        if self.pending is not None:
            self.GLib.source_remove(self.pending)
            self.pending = None

    def request_fit(self, *_args):
        self.repair_position = True
        self.cancel_pending()
        if self.alive:
            self.pending = self.GLib.idle_add(self.apply)
        return False

    def configure(self, *_args):
        # The WM's configure can arrive before its maximized state event.
        # Wait for that transaction, rather than immediately undoing maximize.
        self.cancel_pending()
        if self.alive:
            self.pending = self.GLib.timeout_add(120, self.apply)
        return False

    def apply(self):
        self.pending = None
        if not self.alive:
            return False
        area = self.current_area()
        _, cap = bounded_geometry(area[:2], area[2:], area)
        minimum = (min(360, cap[0]), min(420, cap[1]))
        # The WM must be free to maximize to its complete workarea. The inset
        # is for normal windows only; making it the MAX_SIZE hint disables
        # native maximize in some WMs.
        drawable = self.window.get_window()
        monitor = self.display.get_monitor_at_window(drawable) if drawable else None
        monitor = monitor or self.display.get_primary_monitor() or self.display.get_monitor(0)
        workarea = monitor.get_workarea()
        maximum = (workarea.width, workarea.height)
        if (minimum, maximum, cap) != self.limits:
            self.limits = (minimum, maximum, cap)
            hints = self.Gdk.Geometry()
            hints.min_width, hints.min_height = minimum
            hints.max_width, hints.max_height = maximum
            self.window.set_size_request(*minimum)
            self.window.set_geometry_hints(None, hints,
                self.Gdk.WindowHints.MIN_SIZE | self.Gdk.WindowHints.MAX_SIZE)
            if self.auxiliary_scroll is not None:
                self.auxiliary_scroll.set_max_content_height(max(1, min(220, cap[1] // 3)))
        position, original_size = tuple(self.window.get_position()), tuple(self.window.get_size())
        position, size = bounded_geometry(position, original_size, area)
        repair_position = self.repair_position or original_size != self.applied_size
        self.repair_position = False
        special = drawable and drawable.get_state() & (
            self.Gdk.WindowState.MAXIMIZED | self.Gdk.WindowState.FULLSCREEN | self.Gdk.WindowState.ICONIFIED)
        if not special:
            if size != tuple(self.window.get_size()):
                self.window.resize(*size)
            if not self.window.get_realized():
                self.window.set_default_size(*size)
            elif repair_position and self.window.get_mapped() and position != tuple(self.window.get_position()):
                self.window.move(*position)
        self.applied_size = original_size if special else size
        return False

    def destroy(self, *_args):
        self.alive = False
        self.cancel_pending()
        if self.workarea_watch is not None:
            self.GLib.source_remove(self.workarea_watch)
            self.workarea_watch = None
        if self.xdisplay is not None:
            self.xdisplay.close()
            self.xdisplay = None
        for source, handler in self.monitor_handlers + self.handlers:
            source.disconnect(handler)
        self.monitor_handlers = self.handlers = []
