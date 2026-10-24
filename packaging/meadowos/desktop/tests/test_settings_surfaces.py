"""Native settings CSS contracts; only execute on the owned :103 fixture."""
import ctypes
import os
from pathlib import Path
import unittest

BASE = Path(__file__).resolve().parents[1]
UI = BASE.parents[2] / '.qwen/e2e-tests/meadowos-system-ui-20261023/native-ui'


@unittest.skipUnless(os.environ.get('MEADOW_OWNED_SETTINGS_FIXTURE') == '1' and
                     os.environ.get('DISPLAY') == ':103', 'requires owned :103 fixture')
class NativeSettingsSurfaces(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import gi
        gi.require_version('Gtk', '3.0')
        from gi.repository import Gtk, Gdk
        cls.Gtk, cls.Gdk = Gtk, Gdk
        Gtk.init([])
        cls.providers = {}
        for name in ('Meadow-Light', 'Meadow-Dark'):
            provider = Gtk.CssProvider(); errors = []
            provider.connect('parsing-error', lambda _p, _s, error: errors.append(str(error)))
            provider.load_from_path(str(BASE/'themes'/name/'gtk-3.0/gtk.css'))
            if errors: raise AssertionError(errors)
            cls.providers[name] = provider

    def context(self, theme, tail, *, ready=True, selected=False, root='window'):
        Gtk = self.Gtk
        nodes = [(root, ['background', 'meadow-system-settings']),
                 ('box', ['meadow-native-glass-dialog'] if ready else [])] + tail
        path = Gtk.WidgetPath.new()
        for name, classes in nodes:
            i = path.append_type(Gtk.Widget); path.iter_set_object_name(i, name)
            for value in classes: path.iter_add_class(i, value)
        ctx = Gtk.StyleContext.new(); ctx.set_screen(self.Gdk.Screen.get_default()); ctx.set_path(path)
        ctx.add_provider(self.providers[theme], Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
        ctx.set_state(Gtk.StateFlags.SELECTED if selected else Gtk.StateFlags.NORMAL)
        return ctx

    def test_capture_clears_inner_surfaces_only_after_ready(self):
        f = self.Gtk.StateFlags
        tails = [[(name, [])] for name in ('box', 'grid', 'paned', 'notebook', 'scrolledwindow', 'viewport', 'frame')]
        tails += [[('notebook', []), ('stack', [])], [('treeview', ['view'])],
                  [('treeview', ['view']), ('treeview', ['view', 'cell'])]]
        for theme in self.providers:
            for root in ('window', 'plug'):
                for tail in tails:
                    with self.subTest(theme=theme, root=root, tail=tail):
                        ctx = self.context(theme, tail, root=root)
                        self.assertEqual(ctx.get_background_color(f.NORMAL).alpha, 0)
                fallback = self.context(theme, [('treeview', ['view'])], ready=False, root=root)
                self.assertGreater(fallback.get_background_color(f.NORMAL).alpha, .2)
                # Pointer prelight belongs to the whole viewport too. It must
                # not restore an opaque backing behind individual rows.
                hovered = self.context(theme, [('treeview', ['view'])], root=root)
                hovered.set_state(f.PRELIGHT)
                self.assertEqual(hovered.get_background_color(f.PRELIGHT).alpha, 0)

    def test_selected_cells_and_controls_keep_their_paint(self):
        f = self.Gtk.StateFlags
        for theme in self.providers:
            for root in ('window', 'plug'):
                # GTK paints selection on the transient .cell context, not the
                # entire TreeView root; the latter now always stays clear.
                for tail in ([('treeview', ['view']), ('treeview', ['view', 'cell'])],):
                    with self.subTest(theme=theme, root=root, tail=tail):
                        ctx = self.context(theme, tail, selected=True, root=root)
                        self.assertGreater(ctx.get_background_color(f.SELECTED).alpha, .2)
                        self.assertEqual(ctx.get_property('border-radius', f.SELECTED), 8)
                        ctx.set_state(f.PRELIGHT)
                        self.assertGreater(ctx.get_background_color(f.PRELIGHT).alpha, .2)
                for node in ('entry', 'button'):
                    with self.subTest(theme=theme, root=root, control=node):
                        ctx = self.context(theme, [(node, [])], root=root)
                        self.assertGreater(ctx.get_background_color(f.NORMAL).alpha, .2)

    def test_notebook_stack_and_tabs_use_common_geometry(self):
        f = self.Gtk.StateFlags
        for theme in self.providers:
            ctx = self.context(theme, [('notebook', []), ('stack', [])])
            p = ctx.get_padding(f.NORMAL)
            self.assertEqual((p.top,p.right,p.bottom,p.left), (12,12,12,12))
            ctx = self.context(theme, [('notebook', []), ('header', ['top']), ('tabs', []), ('tab', [])])
            p = ctx.get_padding(f.NORMAL)
            self.assertEqual((p.top,p.right,p.bottom,p.left), (4,12,4,12))
            self.assertEqual(ctx.get_property('min-height', f.NORMAL), 28)
            self.assertEqual(ctx.get_property('border-radius', f.NORMAL), 8)

    def test_actual_wm_notebook_reparented_as_plug_root(self):
        # The vendor moves plug-child itself into GtkPlug. Its own CSS stack
        # is not a descendant notebook; the prior enclosing-box fixture missed
        # this real hierarchy. Construct the installed UI, without vendor main.
        Gtk, f = self.Gtk, self.Gtk.StateFlags
        ctypes.CDLL('libxfce4ui-2.so.0').xfce_titled_dialog_get_type()
        screen = self.Gdk.Screen.get_default()
        for theme, provider in self.providers.items():
            Gtk.StyleContext.add_provider_for_screen(screen, provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
            builder = Gtk.Builder(); builder.add_from_file(str(UI/'window-manager.ui'))
            original = builder.get_object('main-dialog')
            notebook = builder.get_object('plug-child')
            plug = Gtk.Plug.new(0)
            try:
                plug.get_style_context().add_class('meadow-system-settings')
                notebook.reparent(plug)
                self.assertIs(plug.get_child(), notebook)
                self.assertIsInstance(notebook, Gtk.Notebook)

                def subnode(widget, nodes):
                    # GtkNotebook's internal stack is a CSS node, not a widget.
                    # Extend the actual reparented widget's native path.
                    path = widget.get_style_context().get_path().copy()
                    # get_path() may retain the pre-add_class snapshot for
                    # this widget until a child context is resolved. Carry
                    # the actual current style classes into the copied node.
                    for cls in widget.get_style_context().list_classes():
                        path.iter_add_class(path.length()-1, cls)
                    for name in nodes:
                        index = path.append_type(Gtk.Widget)
                        path.iter_set_object_name(index, name)
                    ctx = Gtk.StyleContext.new(); ctx.set_screen(screen); ctx.set_path(path)
                    ctx.add_provider(provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
                    return ctx

                before = subnode(notebook, ['stack'])
                self.assertGreater(before.get_background_color(f.NORMAL).alpha, .2)
                notebook.get_style_context().add_class('meadow-native-glass-dialog')
                stack = subnode(notebook, ['stack'])
                for state in (f.NORMAL, f.PRELIGHT, f.BACKDROP):
                    with self.subTest(theme=theme, part='root-notebook-stack', state=state):
                        stack.set_state(state)
                        self.assertEqual(stack.get_background_color(state).alpha, 0)
                p = stack.get_padding(f.NORMAL)
                self.assertEqual((p.top, p.right, p.bottom, p.left), (12, 12, 12, 12))
                header = subnode(builder.get_object('theme_name_treeview'), ['header', 'button'])
                self.assertEqual(header.get_property('min-height', f.NORMAL), 28)
                p = header.get_padding(f.NORMAL)
                self.assertEqual((p.top, p.right, p.bottom, p.left), (4, 8, 4, 8))
                for state in (f.NORMAL, f.PRELIGHT, f.BACKDROP):
                    with self.subTest(theme=theme, part='tree-header', state=state):
                        header.set_state(state)
                        bg = header.get_background_color(state)
                        fg = header.get_color(state)
                        self.assertGreater(bg.alpha, .2)
                        luminance = .2126*fg.red + .7152*fg.green + .0722*fg.blue
                        if theme == 'Meadow-Dark': self.assertGreater(luminance, .6)
                        else: self.assertLess(luminance, .6)
            finally:
                plug.destroy(); original.destroy()
                Gtk.StyleContext.remove_provider_for_screen(screen, provider)

    def test_installed_accessibility_and_wm_unmapped_widget_interiors(self):
        # Register the class used by the installed packages' GtkBuilder data.
        # This constructs widgets only: no application main, signal handlers,
        # Xfconf calls, mapping or user input is involved.
        Gtk = self.Gtk
        ctypes.CDLL('libxfce4ui-2.so.0').xfce_titled_dialog_get_type()
        screen = self.Gdk.Screen.get_default()
        for theme, provider in self.providers.items():
            Gtk.StyleContext.add_provider_for_screen(screen, provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
            try:
                for name, root_id in (('accessibility', 'dialog'), ('window-manager', 'main-dialog')):
                    builder = Gtk.Builder(); builder.add_from_file(str(UI/(name+'.ui')))
                    root = builder.get_object(root_id)
                    try:
                        root.get_style_context().add_class('meadow-system-settings')
                        root.get_content_area().get_style_context().add_class('meadow-native-glass-dialog')
                        containers = []; labels = []
                        def walk(widget):
                            if isinstance(widget, (Gtk.Frame, Gtk.Grid, Gtk.Viewport, Gtk.ScrolledWindow, Gtk.TreeView)):
                                containers.append(widget)
                            if isinstance(widget, Gtk.Label): labels.append(widget)
                            if isinstance(widget, Gtk.Container):
                                for child in widget.get_children(): walk(child)
                        walk(root.get_content_area())
                        self.assertGreater(len(containers), 4)
                        self.assertGreater(len(labels), 10)
                        for widget in containers:
                            with self.subTest(theme=theme, app=name, widget=widget.get_name()):
                                ctx = widget.get_style_context()
                                self.assertEqual(ctx.get_background_color(Gtk.StateFlags.NORMAL).alpha, 0)
                        for label in labels:
                            colour = label.get_style_context().get_color(Gtk.StateFlags.NORMAL)
                            luminance = .2126*colour.red + .7152*colour.green + .0722*colour.blue
                            if theme == 'Meadow-Dark': self.assertGreater(luminance, .6)
                            else: self.assertLess(luminance, .6)
                    finally:
                        root.destroy()
            finally:
                Gtk.StyleContext.remove_provider_for_screen(screen, provider)


if __name__ == '__main__':
    unittest.main()
