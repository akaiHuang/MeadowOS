"""Desktop presentation and native-theme changes; no display or Agent is started."""
import ast
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[1] / "meadow_shell.py"
spec = importlib.util.spec_from_file_location("meadow_shell_presentation", SOURCE)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
tree = ast.parse(SOURCE.read_text())
shell = next(node for node in ast.walk(tree) if isinstance(node, ast.ClassDef) and node.name == "Shell")
methods = [node for node in shell.body if isinstance(node, ast.FunctionDef) and node.name in {
    "apply_appearance_defaults", "appearance_changed", "open_settings"}]


class PresentationTests(unittest.TestCase):
    def test_compact_dock_layout_and_paint_receipt_share_the_same_width(self):
        self.assertEqual(module.dock_width(1920, 384), 400)
        self.assertEqual(module.dock_width(1920, 720), 720)
        self.assertEqual(module.dock_width(640, 720), 600)
        targets = [node for node in ast.walk(tree) if isinstance(node, ast.FunctionDef)
                   and node.name in {"layout", "after_first_paint"}]
        self.assertEqual(len(targets), 2)
        for target in targets:
            self.assertTrue(any(isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                                and node.func.id == "dock_width" for node in ast.walk(target)))

    def test_files_and_terminal_are_daily_apps_even_with_system_categories(self):
        files = module.application_presentation("Thunar.desktop", "Thunar File Manager", "GTK;System;FileManager;", "thunar")
        terminal = module.application_presentation("org.xfce.Terminal.desktop", "Xfce Terminal", "System;TerminalEmulator;", "xfce4-terminal")
        self.assertEqual((files["title"], files["icon"], files["system"]), ("檔案", "system-file-manager", False))
        self.assertEqual((terminal["title"], terminal["icon"], terminal["system"]), ("終端機", "utilities-terminal", False))

    def test_bulk_rename_stays_a_distinct_searchable_system_tool(self):
        tool = module.application_presentation("Thunar-bulk-rename.desktop", "Bulk Rename", "Utility;", "thunar")
        self.assertEqual(tool["title"], "批次重新命名")
        self.assertTrue(tool["system"])
        for query in ("批次", "bulk rename", "thunar-bulk-rename.desktop"):
            self.assertIn(query, tool["search"])

    def test_chrome_retains_official_icon_and_unknown_app_retains_its_name(self):
        chrome = module.application_presentation("google-chrome.desktop", "Google Chrome", "Network;WebBrowser;", "google-chrome")
        other = module.application_presentation("example.desktop", "Example Project Tool", "Development;", "example")
        self.assertEqual(chrome["icon"], "meadow-browser")
        self.assertEqual(other["title"], "Example Project Tool")
        self.assertIsNone(other["icon"])
        self.assertFalse(other["system"])

    def test_named_system_icons_resolve_to_the_shared_packaged_theme(self):
        theme = SOURCE.parent.parent / "desktop/icons/Meadow"
        names = {path.stem for path in theme.rglob("*.svg")}
        for _title, icon, _system, _order in module.APPLICATION_PRESENTATIONS.values():
            if icon:
                self.assertIn(icon, names)


class AppIconRoutingTests(unittest.TestCase):
    def setUp(self):
        method = next(node for node in shell.body if isinstance(node, ast.FunctionDef) and node.name == "app_icon")
        self.calls = []

        def direct(name, size):
            self.calls.append(("direct", name, size))
            return ("direct", name, size)

        def foreign(gicon, _kind):
            self.calls.append(("foreign", gicon))
            return SimpleNamespace(set_pixel_size=lambda size: self.calls.append(("foreign-size", size)))

        context = {"GLASS_APP_ICONS": module.GLASS_APP_ICONS, "image": direct,
                   "Gtk": SimpleNamespace(IconSize=SimpleNamespace(DIALOG=1),
                                          Image=SimpleNamespace(new_from_gicon=foreign))}
        exec(compile(ast.Module(body=[method], type_ignores=[]), str(SOURCE), "exec"), context)
        self.subject = context["app_icon"]
        self.owner = SimpleNamespace(presentation=lambda _app: {"icon": None},
            framed_app_icon=lambda widget, size: self.calls.append(("frame", size)) or ("frame", widget, size))

    def icon(self, names):
        return SimpleNamespace(get_names=lambda: names)

    def test_packaged_alias_later_in_themed_fallback_list_is_not_shrunk_or_wrapped(self):
        icon = self.icon(["uninstalled-brand", "org.xfce.settings.display"])
        result = self.subject(self.owner, SimpleNamespace(get_icon=lambda: icon), 64)
        self.assertEqual(result, ("direct", "org.xfce.settings.display", 64))
        self.assertEqual(self.calls, [("direct", "org.xfce.settings.display", 64)])

    def test_external_or_file_icon_gets_one_plate_and_bounded_identity_glyph(self):
        for icon in (self.icon(["external-tool"]), object()):
            self.calls.clear()
            result = self.subject(self.owner, SimpleNamespace(get_icon=lambda: icon), 48)
            self.assertEqual(result[0], "frame")
            self.assertEqual(self.calls, [("foreign", icon), ("foreign-size", 23), ("frame", 48)])

    def test_missing_icon_uses_complete_tile_and_known_app_keeps_canonical_identity(self):
        self.assertEqual(self.subject(self.owner, SimpleNamespace(get_icon=lambda: None), 48),
                         ("direct", "meadow-glass-application", 48))
        self.calls.clear()
        self.owner.presentation = lambda _app: {"icon": "system-file-manager"}
        def forbidden():
            raise AssertionError("canonical app must not decode an unrelated fallback")
        self.assertEqual(self.subject(self.owner, SimpleNamespace(get_icon=forbidden), 48),
                         ("direct", "system-file-manager", 48))
        self.assertEqual(self.calls, [("direct", "system-file-manager", 48)])


class WindowIconRoutingTests(unittest.TestCase):
    """Wnck identity matching must preserve packaged app artwork at full size."""
    def setUp(self):
        method = next(node for node in shell.body if isinstance(node, ast.FunctionDef)
                      and node.name == "window_icon")
        self.calls = []
        context = {
            "Path": Path,
            "image": lambda name, size: self.calls.append(("fallback", name, size)) or (name, size),
            "Gtk": SimpleNamespace(Image=SimpleNamespace(
                new_from_pixbuf=lambda pixbuf: self.calls.append(("image", pixbuf)) or pixbuf)),
            "GdkPixbuf": SimpleNamespace(InterpType=SimpleNamespace(BILINEAR="bilinear")),
        }
        exec(compile(ast.Module(body=[method], type_ignores=[]), str(SOURCE), "exec"), context)
        self.subject = context["window_icon"]
        self.owner = SimpleNamespace(apps=[],
            app_icon=lambda app, size: self.calls.append(("app", app, size)) or ("app", app, size),
            framed_app_icon=lambda pixbuf, size: self.calls.append(("frame", size)) or ("frame", size))

    def app(self, startup=None, executable=None):
        return SimpleNamespace(get_startup_wm_class=lambda: startup,
                               get_executable=lambda: executable)

    def recognized(self, identity, app, size):
        def forbidden():
            raise AssertionError("recognized app must not shrink its already-themed window pixbuf")
        self.owner.apps = [self.app(), app]
        window = SimpleNamespace(get_class_group_name=lambda: identity, get_icon=forbidden)
        self.assertEqual(self.subject(self.owner, window, size), ("app", app, size))
        self.assertEqual(self.calls, [("app", app, size)])

    def test_exact_startup_class_is_case_insensitive_and_preserves_full_canvas(self):
        self.recognized("XFCE4-ABOUT", self.app("Xfce4-about", "/usr/bin/other-helper"), 48)

    def test_exact_executable_basename_recognizes_app_without_startup_class(self):
        self.recognized("XFCE4-ABOUT", self.app(None, "/usr/bin/xfce4-about"), 64)

    def test_missing_identity_and_pixbuf_use_one_complete_fallback_tile(self):
        self.owner.apps = [self.app(None, None)]
        for identity in (None, ""):
            self.calls.clear()
            window = SimpleNamespace(get_class_group_name=lambda: identity, get_icon=lambda: None)
            self.assertEqual(self.subject(self.owner, window, 48), ("meadow-glass-application", 48))
            self.assertEqual(self.calls, [("fallback", "meadow-glass-application", 48)])

    def test_unrecognized_or_partial_identity_scales_raw_icon_into_one_plate(self):
        self.owner.apps = [self.app("Xfce4-about", "/usr/bin/xfce4-about")]
        def scale(width, height, interpolation):
            self.calls.append(("scale", width, height, interpolation))
            return "actual-window-pixels"
        for identity in ("xfce4-about-helper", "unregistered-tool"):
            self.calls.clear()
            window = SimpleNamespace(get_class_group_name=lambda: identity,
                get_icon=lambda: SimpleNamespace(scale_simple=scale))
            self.assertEqual(self.subject(self.owner, window, 48), ("frame", 48))
            self.assertEqual(self.calls, [("scale", 23, 23, "bilinear"),
                                         ("image", "actual-window-pixels"), ("frame", 48)])


class NativeThemeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.marker = self.root / "shell-appearance.json"
        self.calls, self.writes = [], []
        self.native = "Meadow-Light"
        self.settings = SimpleNamespace(
            get_property=lambda _name: self.native,
            set_property=lambda name, value: self.writes.append((name, value)))
        self.context = {
            "SHELL_THEME_NAMES": module.SHELL_THEME_NAMES, "ROOT": SOURCE.parent,
            "Gtk": SimpleNamespace(Settings=SimpleNamespace(get_default=lambda: self.settings),
                STYLE_PROVIDER_PRIORITY_APPLICATION=600,
                StyleContext=SimpleNamespace(
                    add_provider_for_screen=lambda *_args: self.calls.append("add-light"),
                    remove_provider_for_screen=lambda *_args: self.calls.append("remove-light"))),
            "Gdk": SimpleNamespace(Screen=SimpleNamespace(get_default=lambda: "screen")),
            "args": SimpleNamespace(theme=None, prefix=self.root),
            "state_directory": lambda: self.root, "atomic_json": module.atomic_json,
            "json": json, "subprocess": SimpleNamespace(run=self.run_plan, TimeoutExpired=TimeoutError),
        }
        exec(compile(ast.Module(body=methods, type_ignores=[]), str(SOURCE), "exec"), self.context)
        self.view = SimpleNamespace(theme="light", appearance_error=None, light_css="provider",
            wallpaper=SimpleNamespace(reload=lambda theme: self.calls.append("wallpaper-" + theme)))

    def run_plan(self, _command, **_kwargs):
        self.calls.append("appearance-plan")
        return SimpleNamespace(returncode=0, stdout="[]")

    def invoke(self, method, *args):
        return self.context[method](self.view, *args)

    def test_existing_native_theme_wins_over_old_shell_marker_without_rewriting_it(self):
        module.atomic_json(self.marker, {"version": 1, "theme": "light"})
        self.native = "Meadow-Dark"
        self.invoke("apply_appearance_defaults")
        self.assertEqual(self.view.theme, "dark")
        self.assertEqual(self.calls, [])
        self.assertEqual(self.writes, [])
        self.assertEqual(json.loads(self.marker.read_text())["theme"], "light")

    def test_fresh_profile_gets_native_default_but_existing_custom_theme_is_preserved(self):
        self.native = "User Custom Theme"
        self.invoke("apply_appearance_defaults")
        self.assertEqual(self.calls, ["appearance-plan"])
        self.assertEqual(json.loads(self.marker.read_text())["theme"], "light")
        self.calls.clear()
        self.invoke("apply_appearance_defaults")
        self.assertEqual(self.calls, [])
        self.assertEqual(self.writes, [])

    def test_adopting_native_theme_marks_profile_without_native_override(self):
        self.native = "Meadow-Dark"
        self.invoke("apply_appearance_defaults")
        self.assertEqual(self.calls, [])
        self.assertEqual(self.writes, [])
        self.assertEqual(json.loads(self.marker.read_text())["theme"], "dark")

    def test_explicit_theme_runs_authorized_plan(self):
        self.context["args"].theme = "light"
        self.native = "Meadow-Dark"
        self.invoke("apply_appearance_defaults")
        self.assertEqual(self.calls, ["appearance-plan"])
        self.assertEqual(json.loads(self.marker.read_text())["theme"], "light")

    def test_notify_switches_css_wallpaper_and_private_preference_without_theme_loop(self):
        self.native = "Meadow-Dark"
        self.invoke("appearance_changed", self.settings, None)
        self.assertEqual(self.calls, ["remove-light", "wallpaper-dark"])
        self.assertEqual(self.writes, [("gtk-application-prefer-dark-theme", True)])
        self.assertEqual(json.loads(self.marker.read_text())["theme"], "dark")
        self.invoke("appearance_changed", self.settings, None)
        self.assertEqual(len(self.calls), 2)
        self.native = "Meadow-Light"
        self.invoke("appearance_changed", self.settings, None)
        self.assertEqual(self.calls[-2:], ["add-light", "wallpaper-light"])
        self.assertEqual(self.writes[-1], ("gtk-application-prefer-dark-theme", False))
        self.native = "Unrelated Theme"
        self.invoke("appearance_changed", self.settings, None)
        self.assertEqual(len(self.calls), 4)


if __name__ == "__main__":
    unittest.main()
