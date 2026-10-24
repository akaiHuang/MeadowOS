"""Shared vector assets and owned GTK control geometry; no existing GUI input."""
import importlib.util
import json
import os
from pathlib import Path
import time
import unittest
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("meadow_control_test_subject", ROOT / "window_controls.py")
controls = importlib.util.module_from_spec(spec)
spec.loader.exec_module(controls)
STYLES = ("close", "minimize", "maximize")


class ControlAssets(unittest.TestCase):
    def test_native_themes_use_the_exact_common_vector_bytes(self):
        for style in STYLES:
            for suffix in ("", "-hover"):
                asset = controls.ASSETS / f"{style}{suffix}.svg"
                for theme in ("Meadow-Light", "Meadow-Dark"):
                    native = ROOT / "themes" / theme / "gtk-3.0/assets" / f"window-{style}{suffix}.svg"
                    with self.subTest(asset=asset.name, theme=theme):
                        self.assertEqual(native.read_bytes(), asset.read_bytes())

    def test_vector_canvas_and_dot_are_centred_without_font_glyphs(self):
        for style in STYLES:
            for suffix in ("", "-hover"):
                tree = ET.parse(controls.ASSETS / f"{style}{suffix}.svg")
                svg = tree.getroot()
                self.assertEqual(svg.attrib["viewBox"], "0 0 12 12")
                self.assertEqual((svg.attrib["width"], svg.attrib["height"]), ("12", "12"))
                circle = svg.find("{http://www.w3.org/2000/svg}circle")
                self.assertEqual((float(circle.attrib["cx"]), float(circle.attrib["cy"])), (6, 6))
                self.assertLessEqual(float(circle.attrib["r"]), 6)
                self.assertEqual(len(svg.findall(".//{http://www.w3.org/2000/svg}text")), 0)
                self.assertEqual(len(svg.findall("{http://www.w3.org/2000/svg}path")), int(bool(suffix)))


@unittest.skipUnless(os.environ.get("MEADOW_OWNED_CONTROL_FIXTURE") == "1" and
                     os.environ.get("DISPLAY") == ":103", "requires owned test display :103")
class NativeControlGeometry(unittest.TestCase):
    def test_real_images_remain_centred_and_switch_vectors_without_resizing(self):
        import gi
        gi.require_version("Gtk", "3.0")
        from gi.repository import Gtk, Gdk, GLib
        provider = Gtk.CssProvider()
        provider.load_from_path(str(ROOT / "window-chrome.css"))
        screen = Gdk.Screen.get_default()
        Gtk.StyleContext.add_provider_for_screen(screen, provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 12)
        settings = Gtk.Settings.get_default()
        old_theme = settings.get_property("gtk-theme-name")
        window = Gtk.Window()
        window.get_style_context().add_class("meadow-hover-chrome-owned")
        window.move(100, 100)
        row = Gtk.Box(spacing=0)
        row.set_margin_start(14)
        row.set_size_request(240, 38)
        row.get_style_context().add_class("meadow-hover-toolbar")
        window.add(row)
        calls = []
        rendered = []
        buttons = [controls.window_button(style, style, lambda key=style: calls.append(key)) for style in STYLES]
        for button in buttons:
            row.pack_start(button, False, False, 0)
        window.show_all()

        def settle():
            deadline = time.monotonic() + .08
            while time.monotonic() < deadline:
                while GLib.MainContext.default().pending():
                    GLib.MainContext.default().iteration(False)
                time.sleep(.005)

        try:
            for theme in ("Meadow-Light", "Meadow-Dark"):
                settings.set_property("gtk-theme-name", theme)
                settle()
                positions = [button.translate_coordinates(window, 0, 0) for button in buttons]
                widths = [button.get_allocated_width() for button in buttons]
                self.assertEqual(widths, [18, 18, 18])
                self.assertEqual([p[0] for p in positions], [14, 32, 50])
                for style, button in zip(STYLES, buttons):
                    image = button.get_child()
                    self.assertIsInstance(image, Gtk.Image)
                    self.assertEqual(image.get_halign(), Gtk.Align.CENTER)
                    self.assertEqual(image.get_valign(), Gtk.Align.CENTER)
                    geometry = (button.get_allocated_width(), button.get_allocated_height())
                    for flag, hover in ((Gtk.StateFlags.NORMAL, False), (Gtk.StateFlags.PRELIGHT, True),
                                        (Gtk.StateFlags.FOCUSED, True), (Gtk.StateFlags.NORMAL, False)):
                        button.unset_state_flags(Gtk.StateFlags.PRELIGHT | Gtk.StateFlags.FOCUSED)
                        if flag:
                            button.set_state_flags(flag, False)
                        settle()
                        self.assertEqual(image.get_storage_type(), Gtk.ImageType.GICON)
                        gicon, icon_size = image.get_gicon()
                        self.assertTrue(gicon.equal(controls.control_icon(style, hover)))
                        self.assertEqual(icon_size, Gtk.IconSize.MENU)
                        self.assertEqual(image.get_pixel_size(), 12)
                        scale = image.get_scale_factor()
                        self.assertEqual(scale, int(os.environ.get("GDK_SCALE", "1")))
                        self.assertEqual((image.get_allocated_width(), image.get_allocated_height()), (12, 12))
                        info = Gtk.IconTheme.get_default().lookup_by_gicon_for_scale(
                            gicon, 12, scale, Gtk.IconLookupFlags.FORCE_SIZE)
                        pixels = info.load_icon()
                        self.assertEqual((pixels.get_width(), pixels.get_height()), (12*scale, 12*scale))
                        rendered.append({"theme":theme,"style":style,"hover":hover,"scale":scale,"logical":[image.get_allocated_width(),image.get_allocated_height()],"pixels":[pixels.get_width(),pixels.get_height()]})
                        self.assertEqual((button.get_allocated_width(), button.get_allocated_height()), geometry)
                        ix, iy = image.translate_coordinates(button, 0, 0)
                        self.assertAlmostEqual(ix + image.get_allocated_width() / 2, geometry[0] / 2, delta=.5)
                        self.assertAlmostEqual(iy + image.get_allocated_height() / 2, geometry[1] / 2, delta=.5)
                    button.emit("clicked")
            self.assertEqual(calls, list(STYLES) * 2)
            print("RETINA_CONTROL_METRICS " + json.dumps({"rendered":rendered,"actions":calls,"button_widths":widths,"button_x":[p[0] for p in positions]}))
        finally:
            window.destroy()
            settings.set_property("gtk-theme-name", old_theme)
            Gtk.StyleContext.remove_provider_for_screen(screen, provider)
            settle()
