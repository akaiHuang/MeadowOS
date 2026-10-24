"""Owned native Exo fixtures for adapter type gating; actual Thunar is CUA QA.

Synthetic owner type names select the adapter branch, while ExoIconView,
GtkTreeView, GtkScrolledWindow and their layouts are the installed GTK widgets.
No existing display, application, system input or user preference is touched.
"""
import ctypes
import os
import time
import unittest


@unittest.skipUnless(os.environ.get("MEADOW_OWNED_INSET_FIXTURE") == "1" and
                     os.environ.get("DISPLAY") == ":103", "requires owned display :103")
class FilesInsets(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import gi
        gi.require_version("Gtk", "3.0")
        from gi.repository import Gtk, GObject, GLib
        cls.Gtk, cls.GObject, cls.GLib = Gtk, GObject, GLib
        library = ctypes.CDLL("libexo-2.so.0")
        library.exo_icon_view_get_type.restype = ctypes.c_size_t
        library.exo_icon_view_get_type()
        cls.exo_type = GObject.type_from_name("ExoIconView")
        class ThunarWindow(Gtk.Window):
            __gtype_name__ = "ThunarWindow"
        class ThunarDetailsView(Gtk.ScrolledWindow):
            __gtype_name__ = "ThunarDetailsView"
        cls.window_type, cls.details_type = ThunarWindow, ThunarDetailsView

    def setUp(self):
        self.windows = []

    def settle(self):
        deadline = time.monotonic() + .08
        while time.monotonic() < deadline:
            while self.GLib.MainContext.default().pending():
                self.GLib.MainContext.default().iteration(False)
            time.sleep(.005)

    def tearDown(self):
        for window in self.windows:
            window.destroy()
        self.settle()

    def owner(self, scoped=True):
        window = self.window_type() if scoped else self.Gtk.Window()
        self.windows.append(window)
        window.set_default_size(420, 300)
        header = self.Gtk.HeaderBar(title="Owned inset fixture")
        window.set_titlebar(header)
        content = self.Gtk.Box()
        window.add(content)
        return window, content

    def exo(self, margin=6):
        return self.GObject.new(self.exo_type, margin=margin)

    def test_initial_and_replacement_views_get_inner_margin_without_outer_margins(self):
        window, box = self.owner()
        first = self.exo()
        changes = []
        first.connect("notify::margin", lambda *_: changes.append(first.get_property("margin")))
        box.pack_start(first, True, True, 0)
        window.show_all(); self.settle()
        self.assertEqual(first.get_property("margin"), 24)
        self.assertEqual([first.get_property(k) for k in
                          ("margin-start", "margin-end", "margin-top", "margin-bottom")], [0]*4)
        self.assertEqual(changes, [24])
        # A later view replacement mimics Compact's constructor margin3.
        box.remove(first); first.destroy()
        second = self.exo(3)
        box.pack_start(second, True, True, 0)
        second.show(); self.settle()
        self.assertEqual(second.get_property("margin"), 24)
        writes = []
        second.connect("notify::margin", lambda *_: writes.append(True))
        second.hide(); second.show(); self.settle()
        self.assertEqual(writes, [], "remap must not stop editing by rewriting unchanged margin")

    def test_foreign_exo_view_keeps_its_application_margin(self):
        window, box = self.owner(False)
        view = self.exo(9)
        box.pack_start(view, True, True, 0)
        window.show_all(); self.settle()
        self.assertEqual(view.get_property("margin"), 9)

    def test_only_details_tree_below_thunar_owner_gets_details_style(self):
        for scoped in (True, False):
            with self.subTest(scoped=scoped):
                window, box = self.owner(scoped)
                details = self.details_type()
                tree = self.Gtk.TreeView()
                details.add(tree); box.pack_start(details, True, True, 0)
                sidebar = self.Gtk.TreeView(); box.pack_start(sidebar, True, True, 0)
                window.show_all(); self.settle()
                self.assertEqual(tree.get_style_context().has_class("meadow-files-details"), scoped)
                self.assertFalse(sidebar.get_style_context().has_class("meadow-files-details"))
