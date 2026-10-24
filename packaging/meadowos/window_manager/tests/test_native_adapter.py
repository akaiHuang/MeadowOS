#!/usr/bin/python3
"""GTK fixture construction tests; pointer/keyboard acceptance is separate CUA QA.

Run on an isolated display with GTK_MODULES pointing at the built adapter.
No fixture opens a provider, launches applications, or modifies a session.
"""
import unittest
import os
import json
from pathlib import Path
import tempfile
import time
import cairo
import gi

gi.require_version("Gtk", "3.0")
from gi.repository import Gtk, Gdk, GLib

owned_windows = []


def descendants(widget):
    result = []
    if isinstance(widget, Gtk.Container):
        widget.forall(lambda child: result.append(child))
    for child in result:
        yield child
        yield from descendants(child)


def fixture(*, owned=False, dialog=False, custom=False, resizable=True, with_sidebar=False):
    window = Gtk.Window(type=Gtk.WindowType.TOPLEVEL)
    owned_windows.append(window)
    window.set_title("Meadow adapter construction fixture")
    window.set_default_size(480, 320)
    window.set_resizable(resizable)
    if owned:
        window.get_style_context().add_class("meadow-hover-chrome-owned")
    if dialog:
        window.set_modal(True)
    if custom:
        window.set_decorated(False)
    header = Gtk.HeaderBar(title="Native fixture")
    header.set_show_close_button(True)
    header.set_decoration_layout("close,minimize,maximize:")
    window.set_titlebar(header)
    body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
    if with_sidebar:
        body.pack_start(Gtk.PlacesSidebar(), True, True, 0)
    entry = Gtk.Entry()
    entry.set_text("Preserved original draft")
    body.pack_start(entry, False, False, 0)
    window.add(body)
    window.show_all()
    while GLib.MainContext.default().pending():
        GLib.MainContext.default().iteration(False)
    return window, header, body, entry


class NativeAdapter(unittest.TestCase):
    def tearDown(self):
        for window in owned_windows:
            window.destroy()
        owned_windows.clear()
        while GLib.MainContext.default().pending():
            GLib.MainContext.default().iteration(False)

    def test_preserves_real_content_and_native_controls(self):
        window, header, body, entry = fixture()
        self.assertEqual(type(window.get_child()).__name__, "MeadowOverlay")
        self.assertIs(body.get_parent(), window.get_child().get_child())
        self.assertIs(header.get_toplevel(), window)
        self.assertEqual(entry.get_text(), "Preserved original draft")
        controls = {x.get_accessible().get_name() for x in descendants(header)
                    if isinstance(x, Gtk.Button)}
        self.assertTrue({"Close", "Minimize", "Maximize"}.issubset(controls))
        self.assertEqual(window.get_allocated_width(), body.get_allocated_width())
        self.assertEqual(window.get_allocated_height(), body.get_allocated_height())

    def test_initial_toolbar_is_hidden_and_input_passes_through(self):
        window, header, _, _ = fixture()
        revealer = header.get_parent()
        self.assertIsInstance(revealer, Gtk.Revealer)
        self.assertFalse(revealer.get_reveal_child())
        self.assertFalse(header.get_sensitive())
        self.assertIs(revealer.get_parent(), body_layout := window.get_child().get_child())
        self.assertIsInstance(body_layout, Gtk.Box)
        self.assertFalse(window.get_style_context().has_class("meadow-chrome-visible"))

    def test_custom_title_toolbar_moves_without_losing_actions_or_hidden_items(self):
        # Thunar uses custom_title rather than pack_start. Exercise GTK's real
        # ownership API, including its internal parent, on an owned fixture.
        window, header, _, entry = fixture()
        toolbar = Gtk.Toolbar()
        action = Gtk.ToolButton(label="Original action")
        hidden = Gtk.ToolButton(label="Application-hidden action")
        toolbar.insert(action, -1)
        toolbar.insert(hidden, -1)
        header.set_custom_title(toolbar)
        toolbar.show()
        action.show()
        hidden.hide()
        self.assertIs(header.get_custom_title(), toolbar)
        self.assertIsNotNone(toolbar.get_parent())
        invoked = []
        action.connect("clicked", lambda _widget: invoked.append(True))
        # get_children() exposes custom_title, while its actual parent can be
        # an internal GtkBox. Clear the property before assigning a new parent.
        self.assertIn(toolbar, header.get_children())
        header.set_custom_title(None)
        self.assertIsNone(toolbar.get_parent())
        right = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        right.pack_start(toolbar, True, True, 0)
        header.pack_end(right)
        right.show()  # show_all would incorrectly reveal application-hidden UI.
        self.assertIs(toolbar.get_parent(), right)
        self.assertTrue(action.get_visible())
        self.assertFalse(hidden.get_visible())
        action.emit("clicked")
        self.assertEqual(invoked, [True])
        self.assertEqual(entry.get_text(), "Preserved original draft")
        names = {widget.get_accessible().get_name() for widget in descendants(header)
                 if isinstance(widget, Gtk.Button)}
        self.assertTrue({"Close", "Minimize", "Maximize"}.issubset(names))

    def test_revealed_header_allocates_above_original_body(self):
        window,header,body,_=fixture()
        header.get_parent().set_reveal_child(True)
        deadline=time.monotonic()+1
        while time.monotonic()<deadline:
            while GLib.MainContext.default().pending():
                GLib.MainContext.default().iteration(False)
            time.sleep(.01)
        hx,hy=header.translate_coordinates(window,0,0)
        bx,by=body.translate_coordinates(window,0,0)
        self.assertGreater(header.get_allocated_height(),20)
        self.assertGreaterEqual(by,hy+header.get_allocated_height())
        self.assertEqual(bx,hx)
        self.assertEqual(window.get_allocated_height(),by+body.get_allocated_height())
        self.assertEqual(len([x for x in descendants(window) if isinstance(x,Gtk.Revealer)]),1)

    def test_internal_titlebuttons_keep_18px_centres_after_real_theme_change(self):
        # This is an owned fixture on its test-only display. GtkSettings is
        # process-local here; no xfconf, user preferences or live app is touched.
        self.assertNotIn("GTK_THEME", os.environ,
                         "A pinned GTK_THEME would invalidate theme-change coverage")
        settings = Gtk.Settings.get_default()
        original_theme = settings.get_property("gtk-theme-name")
        window, header, body, entry = fixture()
        # Dispatch only into this owned fixture's native keyboard reveal hook,
        # so sensitivity/visible classes match a real revealed toolbar. This
        # is not a system input injection and cannot target another window.
        event = Gdk.Event.new(Gdk.EventType.KEY_PRESS)
        event.key.window = window.get_window()
        event.key.keyval = Gdk.KEY_space
        event.key.state = Gdk.ModifierType.CONTROL_MASK | Gdk.ModifierType.MOD1_MASK
        self.assertTrue(window.emit("key-press-event", event))
        self.assertTrue(header.get_sensitive())
        updates = []
        header.connect("style-updated", lambda _widget: updates.append(True))
        measurements = []
        def settle():
            header.get_parent().set_reveal_child(True)
            deadline = time.monotonic() + .45
            while time.monotonic() < deadline:
                while GLib.MainContext.default().pending():
                    GLib.MainContext.default().iteration(False)
                time.sleep(.01)
        try:
            for theme in ("Meadow-Light", "Meadow-Dark", "Meadow-Light"):
                settings.set_property("gtk-theme-name", theme)
                settle()
                buttons = {widget.get_accessible().get_name(): widget
                           for widget in descendants(header)
                           if isinstance(widget, Gtk.Button) and
                           widget.get_style_context().has_class("titlebutton")}
                controls = [buttons[name] for name in ("Close", "Minimize", "Maximize")]
                # These native GtkHeaderBar controls are reached via forall;
                # the original get_children traversal misses their internal box.
                self.assertTrue(all(button.get_parent() not in header.get_children()
                                    for button in controls))
                positions = [button.translate_coordinates(window, 0, 0) for button in controls]
                widths = [button.get_allocated_width() for button in controls]
                centres = [position[0] + width / 2 for position, width in zip(positions, widths)]
                hx, hy = header.translate_coordinates(window, 0, 0)
                style = header.get_style_context()
                padding = style.get_padding(style.get_state())
                border = style.get_border(style.get_state())
                measurements.append({"theme": theme, "button_x": [p[0] for p in positions],
                                     "widths": widths, "centres": centres,
                                     "left_inset": positions[0][0] - hx,
                                     "header_origin": [hx, hy], "padding_left": padding.left,
                                     "border_left": border.left,
                                     "header_style_path": style.get_path().to_string()})
                with self.subTest(theme=theme):
                    self.assertTrue(all(button.get_mapped() for button in controls))
                    self.assertEqual(widths, [18, 18, 18])
                    self.assertEqual([centres[1] - centres[0], centres[2] - centres[1]], [18, 18])
                    self.assertEqual([button.get_parent().get_spacing() for button in controls], [0, 0, 0])
                    self.assertEqual(positions[0][0] - hx, 14)
                    self.assertGreaterEqual(body.translate_coordinates(window, 0, 0)[1],
                                            hy + header.get_allocated_height())
                    self.assertEqual(entry.get_text(), "Preserved original draft")
            self.assertGreaterEqual(len(updates), 2)
            if output := os.environ.get("MEADOW_NATIVE_FIXTURE_METRICS"):
                Path(output).write_text(json.dumps({"scope": "owned native fixture, not live UI",
                                                   "measurements": measurements,
                                                   "style_updated_count": len(updates)}, indent=2) + "\n")
        finally:
            settings.set_property("gtk-theme-name", original_theme)

    def test_meadow_owned_window_unchanged(self):
        window, header, body, _ = fixture(owned=True)
        self.assertIs(window.get_child(), body)
        self.assertIs(window.get_titlebar(), header)

    def test_modal_window_unchanged(self):
        window, header, body, _ = fixture(dialog=True)
        self.assertIs(window.get_child(), body)
        self.assertIs(window.get_titlebar(), header)

    def test_custom_undecorated_window_unchanged(self):
        window, header, body, _ = fixture(custom=True)
        self.assertIs(window.get_child(), body)
        self.assertIs(window.get_titlebar(), header)

    def test_nonresizable_edges_remain_hidden_after_show_all(self):
        window, _, _, _ = fixture(resizable=False)
        edges = [x for x in descendants(window)
                 if x.get_style_context().has_class("meadow-resize-edge")]
        self.assertEqual(len(edges), 4)
        self.assertTrue(all(not x.get_visible() for x in edges))
        window.show_all()
        self.assertTrue(all(not x.get_visible() for x in edges))
        self.assertTrue(all(x.get_no_show_all() for x in edges))

    def test_empty_body_has_opaque_theme_base_and_children_paint_above_it(self):
        window, _, body, _ = fixture()
        provider = Gtk.CssProvider()
        provider.load_from_data(b"@define-color meadow_bg #13579b;")
        screen = window.get_screen()
        Gtk.StyleContext.add_provider_for_screen(screen, provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION+100)
        root = window.get_child()
        def render():
            surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, root.get_allocated_width(), root.get_allocated_height())
            root.draw(cairo.Context(surface))
            surface.flush()
            return surface
        def pixel(surface, x, y):
            offset = y*surface.get_stride()+x*4
            return bytes(surface.get_data()[offset:offset+4])
        try:
            image = render()
            self.assertEqual(pixel(image, 80, 260), bytes([155,87,19,255]))
            self.assertEqual(pixel(image, 0, 0)[3], 0)
            # Regional material and native child painting must stay ABOVE base.
            def child_paint(widget, context):
                context.set_source_rgba(.2,.4,.6,1)
                context.rectangle(60,220,80,60)
                context.fill()
                return False
            handler = body.connect("draw", child_paint)
            image = render()
            self.assertEqual(pixel(image, 80, 260), bytes([153,102,51,255]))
            body.disconnect(handler)
        finally:
            Gtk.StyleContext.remove_provider_for_screen(screen, provider)

    def test_app_hidden_menu_status_preview_and_header_children_stay_hidden(self):
        window = Gtk.Window()
        owned_windows.append(window)
        header = Gtk.HeaderBar(title="Native visibility fixture")
        header.set_show_close_button(True)
        hidden_header = Gtk.Button(label="App-hidden header action")
        header.pack_end(hidden_header)
        header.show()
        window.set_titlebar(header)
        body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        body.show()
        hidden = [Gtk.MenuBar(), Gtk.Statusbar(), Gtk.Box()]
        for child in hidden:
            body.pack_start(child, False, False, 0)
            child.hide()
        visible = Gtk.Entry(text="Visible directory content")
        visible.show()
        body.pack_start(visible, True, True, 0)
        window.add(body)
        # Native applications show the already configured hierarchy; they do
        # not ask to reveal controls hidden by their own preferences.
        window.show()
        while GLib.MainContext.default().pending():
            GLib.MainContext.default().iteration(False)
        self.assertEqual(type(window.get_child()).__name__, "MeadowOverlay")
        self.assertTrue(body.get_visible())
        self.assertTrue(visible.get_visible())
        self.assertTrue(all(not child.get_visible() for child in hidden))
        self.assertFalse(hidden_header.get_visible())
        self.assertIs(visible.get_parent(), body)

    def test_reparent_preserves_thunar_remove_cleanup_contract(self):
        # Thunar 4.20.7 connects window::remove to notebook destruction. Match
        # that real lifecycle contract instead of testing a generic GtkWindow.
        window = Gtk.Window()
        owned_windows.append(window)
        header = Gtk.HeaderBar(title="Thunar lifecycle regression")
        header.set_show_close_button(True)
        window.set_titlebar(header)
        body = Gtk.Box()
        notebook = Gtk.Notebook()
        notebook.append_page(Gtk.Entry(text="Native directory tab"), Gtk.Label(label="Documents"))
        body.pack_start(notebook, True, True, 0)
        window.add(body)
        cleanup_calls = []
        def cleanup(owner, child):
            cleanup_calls.append(child)
            notebook.destroy()
        window.connect("remove", cleanup)
        window.show_all()
        while GLib.MainContext.default().pending():
            GLib.MainContext.default().iteration(False)
        self.assertEqual(cleanup_calls, [])
        self.assertEqual(notebook.get_n_pages(), 1)
        self.assertIs(notebook.get_parent(), body)
        self.assertEqual(notebook.get_nth_page(0).get_text(), "Native directory tab")
        # The transaction must not disconnect/disable real application cleanup.
        window.destroy()
        self.assertTrue(cleanup_calls)

    def test_failed_capture_worker_is_replaced_and_recovers(self):
        # Fault-injection IPC test only, not an optical/source-capture test.
        old_bridge = os.environ.get("MEADOW_GLASS_BRIDGE_PATH")
        with tempfile.TemporaryDirectory(prefix="meadow-gtk-worker-test-") as tmp:
            count_path = Path(tmp) / "starts"
            worker = Path(tmp) / "worker.py"
            worker.write_text("import pathlib,struct,sys\n"
                              f"p=pathlib.Path({str(count_path)!r})\n"
                              "n=int(p.read_text())+1 if p.exists() else 1\n"
                              "p.write_text(str(n))\n"
                              "for line in sys.stdin:\n"
                              " parts=line.split(); w,h=int(parts[3]),int(parts[4])\n"
                              " if n==1: sys.stdout.buffer.write(struct.pack('=4I',1,0,0,0))\n"
                              " else:\n"
                              "  sys.stdout.buffer.write(struct.pack('=4I',0,1,w,h))\n"
                              "  sys.stdout.buffer.write(bytes([240,240,240,255])*(w*h))\n"
                              " sys.stdout.buffer.flush()\n")
            os.environ["MEADOW_GLASS_BRIDGE_PATH"] = str(worker)
            try:
                window, _, body, entry = fixture(with_sidebar=True)
                sidebar = next(x for x in descendants(body) if isinstance(x, Gtk.PlacesSidebar))
                deadline = time.monotonic()+8
                while time.monotonic() < deadline:
                    while GLib.MainContext.default().pending():
                        GLib.MainContext.default().iteration(False)
                    if sidebar.get_style_context().has_class("meadow-native-glass-sidebar"):
                        break
                    time.sleep(.01)
                self.assertTrue(sidebar.get_style_context().has_class("meadow-native-glass-sidebar"))
                self.assertGreaterEqual(int(count_path.read_text()), 2)
                self.assertEqual(entry.get_text(), "Preserved original draft")
                window.destroy()
            finally:
                if old_bridge is None:
                    os.environ.pop("MEADOW_GLASS_BRIDGE_PATH", None)
                else:
                    os.environ["MEADOW_GLASS_BRIDGE_PATH"] = old_bridge

    def test_iconified_state_stops_new_capture_and_restore_resumes(self):
        # Inject the native window-state event into this owned fixture. This
        # verifies dispatch gating, not the independent WM/CUA minimize gate.
        old_bridge = os.environ.get("MEADOW_GLASS_BRIDGE_PATH")
        with tempfile.TemporaryDirectory(prefix="meadow-gtk-iconified-test-") as tmp:
            count_path = Path(tmp) / "requests"
            worker = Path(tmp) / "worker.py"
            worker.write_text("import pathlib,struct,sys\n"
                              f"p=pathlib.Path({str(count_path)!r})\n"
                              "for line in sys.stdin:\n"
                              " n=int(p.read_text())+1 if p.exists() else 1\n"
                              " p.write_text(str(n))\n"
                              " a=line.split();w,h=int(a[3]),int(a[4])\n"
                              " sys.stdout.buffer.write(struct.pack('=4I',0,1,w,h))\n"
                              " sys.stdout.buffer.write(bytes([240,240,240,255])*(w*h))\n"
                              " sys.stdout.buffer.flush()\n")
            os.environ["MEADOW_GLASS_BRIDGE_PATH"] = str(worker)
            def spin(seconds):
                deadline = time.monotonic()+seconds
                while time.monotonic() < deadline:
                    while GLib.MainContext.default().pending():
                        GLib.MainContext.default().iteration(False)
                    time.sleep(.01)
            try:
                window, _, _, _ = fixture(with_sidebar=True)
                spin(.9)
                self.assertTrue(count_path.exists())
                event = Gdk.Event.new(Gdk.EventType.WINDOW_STATE)
                event.window_state.window = window.get_window()
                event.window_state.changed_mask = Gdk.WindowState.ICONIFIED
                event.window_state.new_window_state = Gdk.WindowState.ICONIFIED
                window.emit("window-state-event", event)
                spin(.4)  # allow the one already-dispatched request to finish
                stopped = int(count_path.read_text())
                spin(.7)
                self.assertEqual(int(count_path.read_text()), stopped)
                event.window_state.new_window_state = Gdk.WindowState(0)
                window.emit("window-state-event", event)
                spin(.7)
                self.assertGreater(int(count_path.read_text()), stopped)
                window.destroy()
            finally:
                if old_bridge is None:
                    os.environ.pop("MEADOW_GLASS_BRIDGE_PATH", None)
                else:
                    os.environ["MEADOW_GLASS_BRIDGE_PATH"] = old_bridge

    def test_show_again_keeps_single_native_overlay(self):
        window, header, body, _ = fixture()
        root = window.get_child()
        window.hide()
        window.show_all()
        self.assertIs(window.get_child(), root)
        self.assertIs(body.get_parent(), root.get_child())
        self.assertIs(header.get_toplevel(), window)
        self.assertEqual(sum(type(x).__name__ == "MeadowOverlay" for x in descendants(window)), 1)


if __name__ == "__main__":
    unittest.main()
