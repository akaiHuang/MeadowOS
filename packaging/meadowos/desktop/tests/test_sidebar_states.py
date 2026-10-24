"""Shared sidebar semantics; native cases require the owned :103 fixture."""
import ast
import importlib.util
import os
from pathlib import Path
import shutil
import tempfile
import unittest

BASE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('meadow_sidebar_styles', BASE/'sidebar_styles.py')
styles = importlib.util.module_from_spec(spec); spec.loader.exec_module(styles)


class SidebarPackagingTests(unittest.TestCase):
    def test_both_themes_end_with_same_generated_state_contract(self):
        for theme in ('Meadow-Light', 'Meadow-Dark'):
            directory = BASE/'themes'/theme/'gtk-3.0'
            self.assertEqual((directory/'sidebar-states.css').read_text(), styles.stylesheet())
            self.assertTrue((directory/'gtk.css').read_text().rstrip().endswith(
                '@import url("sidebar-states.css");'))

    def test_theme_without_state_contract_is_incomplete(self):
        spec = importlib.util.spec_from_file_location('sidebar_theme_complete', BASE/'meadow_theme.py')
        theme = importlib.util.module_from_spec(spec); spec.loader.exec_module(theme)
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary)/'theme'
            shutil.copytree(BASE/'themes/Meadow-Light', target)
            self.assertTrue(theme.theme_complete(target))
            (target/'gtk-3.0/sidebar-states.css').unlink()
            self.assertFalse(theme.theme_complete(target))


@unittest.skipUnless(os.environ.get('MEADOW_OWNED_SIDEBAR_FIXTURE') == '1' and
                     os.environ.get('DISPLAY') == ':103', 'requires owned :103 fixture')
class NativeSidebarStateTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import gi
        gi.require_version('Gtk', '3.0')
        from gi.repository import Gtk, Gdk
        Gtk.init([]); cls.Gtk, cls.Gdk = Gtk, Gdk
        cls.providers = {}
        for name in ('Meadow-Light', 'Meadow-Dark'):
            p = Gtk.CssProvider(); errors = []
            p.connect('parsing-error', lambda _p, _s, e: errors.append(str(e)))
            p.load_from_path(str(BASE/'themes'/name/'gtk-3.0/gtk.css'))
            if errors: raise AssertionError(errors)
            cls.providers[name] = p
        inline = next(n.value for n in ast.walk(ast.parse((BASE/'meadow_settings.py').read_text()))
                      if isinstance(n, ast.Constant) and isinstance(n.value, bytes)
                      and b'.settings-sidebar' in n.value)
        cls.application = Gtk.CssProvider(); cls.application.load_from_data(inline)
        cls.paths = {
            'settings': [('window', ['background', 'meadow-settings']), ('box', ['settings-sidebar']),
                         ('list', ['sidebar']), ('row', ['activatable', 'settings-nav'])],
            'chooser': [('dialog', ['background']), ('filechooser', []), ('box', []), ('paned', []),
                        ('placessidebar', ['sidebar']), ('viewport', []), ('list', []),
                        ('row', ['activatable', 'sidebar-row'])],
            'generic-list': [('window', ['background']), ('box', []), ('list', []),
                             ('row', ['activatable'])],
            'thunar-cell': [('window', ['background', 'thunar']),
                            ('scrolledwindow', ['sidebar', 'meadow-glass-sidebar']),
                            ('treeview', ['view', 'cell'])],
            'thunar-focus': [('window', ['background', 'thunar']),
                             ('scrolledwindow', ['sidebar', 'meadow-glass-sidebar']),
                             ('treeview', ['view'])],
        }
        cls.content_paths = {
            'chooser-outer': cls.paths['chooser'][:5],
            'content-tree': [('window', ['background']), ('treeview', ['view', 'cell'])],
            'content-icons': [('window', ['background']), ('iconview', ['view'])],
            'thunar-content': [('window', ['background', 'thunar']), ('iconview', ['view'])],
        }

    def context(self, theme, family, flags):
        Gtk = self.Gtk
        path = Gtk.WidgetPath.new()
        nodes = self.paths[family] if family in self.paths else self.content_paths[family]
        for name, classes in nodes:
            index = path.append_type(Gtk.Widget); path.iter_set_object_name(index, name)
            for value in classes: path.iter_add_class(index, value)
        ctx = Gtk.StyleContext.new(); ctx.set_screen(self.Gdk.Screen.get_default()); ctx.set_path(path)
        # Local providers preserve theme/app precedence, above the fixture's
        # ambient default theme. Nothing is installed into another process.
        ctx.add_provider(self.providers[theme], Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
        if family == 'settings':
            ctx.add_provider(self.application, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION+1)
        ctx.set_state(flags)
        return ctx

    def rgba(self, value):
        return tuple(round(getattr(value, k), 4) for k in ('red','green','blue','alpha'))

    def test_selected_states_share_material_foreground_and_radius(self):
        flags = self.Gtk.StateFlags
        states = [flags.SELECTED | extra for extra in (flags.NORMAL, flags.PRELIGHT,
                  flags.FOCUSED, flags.ACTIVE, flags.BACKDROP, flags.BACKDROP|flags.PRELIGHT)]
        for theme in self.providers:
            for state in states:
                values = []
                for family in self.paths:
                    with self.subTest(theme=theme, family=family, state=int(state)):
                        ctx = self.context(theme, family, state)
                        bg = self.rgba(ctx.get_background_color(state)); fg = self.rgba(ctx.get_color(state))
                        self.assertAlmostEqual(bg[3], .9, places=3)
                        self.assertEqual(ctx.get_property('border-radius', state), 8)
                        if family in ('settings', 'chooser', 'generic-list'):
                            self.assertEqual(ctx.get_property('min-height', state), 32)
                            padding = ctx.get_padding(state)
                            self.assertEqual((padding.top, padding.right, padding.bottom, padding.left),
                                             (0, 12, 0, 12))
                        values.append((bg, fg))
                self.assertEqual(len(set(values)), 1, (theme, int(state), values))

    def test_hover_is_gentle_and_disabled_is_unpainted(self):
        f = self.Gtk.StateFlags
        for theme in self.providers:
            for family in self.paths:
                for state, alpha in ((f.PRELIGHT, .055), (f.INSENSITIVE|f.SELECTED, 0)):
                    with self.subTest(theme=theme, family=family, state=int(state)):
                        ctx = self.context(theme, family, state)
                        self.assertAlmostEqual(ctx.get_background_color(state).alpha, alpha, places=3)

    def test_keyboard_focus_is_solid_inset_and_backdrop_hides_only_ring(self):
        f = self.Gtk.StateFlags
        for theme in self.providers:
            for family in self.paths:
                for state, width in ((f.NORMAL, 0), (f.FOCUSED, 1),
                                     (f.SELECTED|f.FOCUSED, 1), (f.SELECTED|f.FOCUSED|f.BACKDROP, 0)):
                    with self.subTest(theme=theme, family=family, state=int(state)):
                        ctx = self.context(theme, family, state)
                        self.assertEqual(ctx.get_property('outline-width', state), width)
                        if width:
                            self.assertEqual(ctx.get_property('outline-style', state), self.Gtk.BorderStyle.SOLID)
                            self.assertEqual(ctx.get_property('outline-offset', state), -2)
                            # GTK3's deprecated shorthand query reports the
                            # border radius here. The ring's visual rounding
                            # is verified by the native pixel/UI acceptance.

    def test_chooser_outer_inset_and_content_selection_radius(self):
        f = self.Gtk.StateFlags
        for theme in self.providers:
            with self.subTest(theme=theme, family='chooser-outer'):
                ctx = self.context(theme, 'chooser-outer', f.NORMAL)
                p = ctx.get_padding(f.NORMAL)
                self.assertEqual((p.top, p.right, p.bottom, p.left), (12, 12, 12, 12))
            for family in ('content-tree', 'content-icons', 'thunar-content'):
                for state in (f.SELECTED, f.SELECTED|f.PRELIGHT,
                              f.SELECTED|f.FOCUSED, f.SELECTED|f.BACKDROP):
                    with self.subTest(theme=theme, family=family, state=int(state)):
                        ctx = self.context(theme, family, state)
                        self.assertEqual(ctx.get_property('border-radius', state), 8)


if __name__ == '__main__':
    unittest.main()
