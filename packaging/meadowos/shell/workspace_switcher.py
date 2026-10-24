"""Compact native desktop tabs; window lifetimes belong to the WM/apps."""
from pathlib import Path

from gi.repository import Gdk, Gio, GLib, Gtk
from workspace_lifecycle import WorkspaceLifecycle


def _glyph(name):
    path = Path(__file__).with_name('assets') / ('workspace-' + name + '-symbolic.svg')
    icon = Gio.FileIcon.new(Gio.File.new_for_path(str(path)))
    widget = Gtk.Image.new_from_gicon(icon, Gtk.IconSize.MENU)
    widget.set_pixel_size(12)
    return widget


class _Segment(Gtk.EventBox):
    def __init__(self, owner, workspace):
        super().__init__()
        self.owner, self.workspace = owner, workspace
        self.hide_timer = None
        self.syncing = False
        self.last_allocated_width = None
        self.set_visible_window(False)
        self.get_style_context().add_class('workspace-segment')
        self.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK | Gdk.EventMask.LEAVE_NOTIFY_MASK)
        row = Gtk.Box(spacing=0)
        self.button = Gtk.ToggleButton(label=f'桌面{workspace.get_number() + 1}')
        self.button.get_style_context().add_class('workspace-tab')
        self.button.set_focus_on_click(False)
        self.button.get_accessible().set_name(self.button.get_label())
        self.button.connect('clicked', self.activate)
        self.button.connect('enter-notify-event', self.enter)
        self.button.connect('leave-notify-event', self.leave)
        self.button.connect('focus-in-event', self.enter)
        self.button.connect('focus-out-event', self.leave)
        row.pack_start(self.button, False, False, 0)
        self.close_button = Gtk.Button()
        self.close_button.get_style_context().add_class('workspace-close')
        self.close_button.set_focus_on_click(False)
        self.close_button.add(_glyph('close'))
        title = f'關閉桌面{workspace.get_number() + 1}'
        self.close_button.get_accessible().set_name(title)
        self.close_button.set_tooltip_text(title + '，將視窗移到相鄰桌面')
        self.close_button.connect('clicked', lambda *_: owner.lifecycle.close_workspace(workspace))
        self.close_button.connect('enter-notify-event', self.enter)
        self.close_button.connect('leave-notify-event', self.leave)
        self.close_button.connect('focus-in-event', self.enter)
        self.close_button.connect('focus-out-event', self.leave)
        self.revealer = Gtk.Revealer()
        self.revealer.set_transition_type(Gtk.RevealerTransitionType.SLIDE_RIGHT)
        self.revealer.set_transition_duration(100)
        self.revealer.add(self.close_button)
        row.pack_start(self.revealer, False, False, 0)
        self.add(row)
        self.connect('enter-notify-event', self.enter)
        self.connect('leave-notify-event', self.leave)
        self.connect('destroy', self.destroyed)
        self.connect('size-allocate', self.allocated)

    def allocated(self, _widget, allocation):
        # Scrolling changes child origins too; only growing the close control
        # needs visibility correction, otherwise manual scroll would snap back.
        changed = allocation.width != self.last_allocated_width
        self.last_allocated_width = allocation.width
        if changed and self.revealer.get_reveal_child():
            self.owner.reveal_segment(self)

    def activate(self, *_args):
        if self.syncing:
            return
        # GTK can emit clicked during set_active; synchronize without a second activation.
        self.set_checked(self.workspace == self.owner.screen.get_active_workspace())
        if not self.owner.lifecycle.busy:
            self.workspace.activate(self.owner.timestamp())

    def set_checked(self, value):
        self.syncing = True
        try:
            self.button.set_active(value)
        finally:
            self.syncing = False

    def enter(self, *_args):
        self.cancel_hide()
        if self.owner.scrolling:
            return False
        if self.owner.screen.get_workspace_count() > 1:
            self.revealer.set_reveal_child(True)
            self.owner.reveal_segment(self)
        return False

    def leave(self, *_args):
        self.cancel_hide()
        self.hide_timer = GLib.timeout_add(120, self.maybe_hide)
        return False

    def cancel_hide(self):
        if self.hide_timer is not None:
            GLib.source_remove(self.hide_timer)
            self.hide_timer = None

    def maybe_hide(self):
        self.hide_timer = None
        focus = self.get_toplevel().get_focus()
        if self.get_toplevel().is_active() and focus is not None and (focus is self or focus.is_ancestor(self)):
            return False
        x, y = self.get_pointer()
        if 0 <= x < self.get_allocated_width() and 0 <= y < self.get_allocated_height():
            return False
        self.revealer.set_reveal_child(False)
        return False

    def destroyed(self, *_args):
        self.cancel_hide()

    def update(self, active, busy, can_close):
        self.set_checked(self.workspace == active)
        self.button.set_sensitive(not busy)
        self.close_button.set_sensitive(can_close and not busy)
        if not can_close:
            self.revealer.set_reveal_child(False)


class WorkspaceSwitcher(Gtk.Box):
    def __init__(self, screen, timestamp, on_error=None):
        super().__init__(spacing=1)
        self.screen, self.timestamp = screen, timestamp
        self.on_error, self.signature, self.tabs = on_error, None, {}
        self.visibility_timer, self.selected_number, self.maximum_width = None, None, 500
        self.need_selected_visible, self.closed = True, False
        self.layout_pending, self.reveal_number = True, None
        self.scrolling, self.scroll_timer, self.keyboard_restore = False, None, False
        self.get_style_context().add_class('shell-workspaces')
        self.set_valign(Gtk.Align.CENTER)
        self.strip = Gtk.Box(spacing=1)
        self.scroll = Gtk.ScrolledWindow()
        # Overlay scrollbar input regions can cover this entire short rail.
        self.scroll.set_overlay_scrolling(False)
        self.scroll.set_policy(Gtk.PolicyType.EXTERNAL, Gtk.PolicyType.NEVER)
        self.scroll.set_propagate_natural_width(True)
        self.scroll.set_min_content_width(0)
        self.scroll.set_max_content_width(500)
        self.scroll.set_size_request(0, -1)
        self.scroll.add(self.strip)
        self.scroll.connect('size-allocate', self.allocated)
        self.scroll.connect('scroll-event', self.user_scroll)
        self.pack_start(self.scroll, True, True, 0)
        self.add_button = Gtk.Button()
        self.add_button.get_style_context().add_class('workspace-add')
        self.add_button.set_focus_on_click(False)
        self.add_button.add(_glyph('add'))
        self.add_button.get_accessible().set_name('新增桌面')
        self.add_button.set_tooltip_text('新增桌面')
        self.add_button.connect('clicked', lambda *_: self.lifecycle.add_workspace())
        self.pack_end(self.add_button, False, False, 0)
        self.lifecycle = WorkspaceLifecycle(screen, timestamp=timestamp, glib=GLib,
            on_busy=self.busy_changed, on_complete=lambda *_: self.refresh(),
            on_error=self.failed)
        self.handlers = [screen.connect(name, lambda *_: self.refresh()) for name in
            ('workspace-created', 'workspace-destroyed', 'active-workspace-changed')]
        self.connect('destroy', self.destroyed)
        self.refresh()

    def busy_changed(self, busy):
        if busy:
            top = self.get_toplevel()
            focus = top.get_focus() if isinstance(top, Gtk.Window) else None
            # Disabling the initiating control clears GTK focus immediately.
            # Remember keyboard ownership before that first busy refresh.
            if isinstance(top, Gtk.Window) and top.is_active() and focus and focus.is_ancestor(self):
                self.keyboard_restore = True
        self.refresh()

    def set_max_width(self, width):
        maximum = max(60, int(width) - 34)
        if maximum != self.maximum_width:
            self.maximum_width = maximum
            self.need_selected_visible = True
            self.layout_pending = True
            self.scroll.set_max_content_width(maximum)
            self.schedule_selected_visible()

    def failed(self, message):
        self.add_button.set_tooltip_text(message)
        if self.on_error:
            self.on_error(message)

    def refresh(self):
        if self.closed:
            return
        workspaces = list(self.screen.get_workspaces())
        signature = tuple(workspaces)
        if signature != self.signature:
            focus = self.get_toplevel().get_focus() if isinstance(self.get_toplevel(), Gtk.Window) else None
            if self.get_mapped() and self.get_toplevel().is_active() and focus and focus.is_ancestor(self.strip):
                self.keyboard_restore = True
            self.layout_pending = True
            self.signature = signature
            for child in self.strip.get_children():
                child.destroy()
            self.tabs = {}
            for workspace in workspaces:
                segment = _Segment(self, workspace)
                self.tabs[workspace.get_number()] = segment
                self.strip.pack_start(segment, False, False, 0)
            self.strip.show_all()
        active = self.screen.get_active_workspace()
        for segment in self.tabs.values():
            segment.update(active, self.lifecycle.busy, len(workspaces) > 1)
        self.add_button.set_sensitive(not self.lifecycle.busy)
        if not self.lifecycle.error:
            self.add_button.set_tooltip_text('新增桌面')
        number = active.get_number() if active else None
        if number != self.selected_number:
            self.selected_number = number
            self.need_selected_visible = True
        self.schedule_selected_visible()
        if self.keyboard_restore and not self.lifecycle.busy:
            self.keyboard_restore = False
            target = self.tabs.get(number)
            (target.button if target else self.add_button).grab_focus()

    def user_scroll(self, *_args):
        # Native wheel/trackpad movement owns the viewport until it settles;
        # in-flight hover animations must not pull it back to an old tab.
        self.scrolling = True
        self.need_selected_visible, self.reveal_number = False, None
        if self.visibility_timer is not None:
            GLib.source_remove(self.visibility_timer)
            self.visibility_timer = None
        if self.scroll_timer is not None:
            GLib.source_remove(self.scroll_timer)
        self.scroll_timer = GLib.timeout_add(160, self.scroll_finished)
        return False

    def scroll_finished(self):
        self.scroll_timer, self.scrolling = None, False
        x, y = self.scroll.get_pointer()
        if 0 <= x < self.scroll.get_allocated_width() and 0 <= y < self.scroll.get_allocated_height():
            for segment in self.tabs.values():
                sx, sy = segment.get_pointer()
                if 0 <= sx < segment.get_allocated_width() and 0 <= sy < segment.get_allocated_height():
                    segment.enter()
                    break
        return False

    def allocated(self, *_args):
        self.layout_pending = False
        self.schedule_selected_visible()

    def reveal_segment(self, segment):
        number = segment.workspace.get_number()
        if not self.scrolling and self.tabs.get(number) is segment:
            self.reveal_number = number
            self.need_selected_visible = True
            self.schedule_selected_visible()

    def schedule_selected_visible(self):
        if not self.closed and not self.scrolling and self.need_selected_visible and self.visibility_timer is None:
            self.visibility_timer = GLib.idle_add(self.ensure_selected_visible)

    def ensure_selected_visible(self):
        self.visibility_timer = None
        number = self.reveal_number if self.reveal_number is not None else self.selected_number
        segment = self.tabs.get(number)
        adjustment = self.scroll.get_hadjustment()
        if not self.layout_pending and segment is not None and self.get_mapped() and adjustment.get_page_size() > 0:
            bounds = segment.get_allocation()
            adjustment.clamp_page(bounds.x, bounds.x + bounds.width)
            self.need_selected_visible = False
            self.reveal_number = None
        return False

    def destroyed(self, *_args):
        self.closed = True
        if self.visibility_timer is not None:
            GLib.source_remove(self.visibility_timer)
            self.visibility_timer = None
        if self.scroll_timer is not None:
            GLib.source_remove(self.scroll_timer)
            self.scroll_timer = None
        self.lifecycle.dispose()
        for handler in self.handlers:
            self.screen.disconnect(handler)
        self.handlers = []
