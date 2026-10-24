"""Packaging and lookup contracts for the original native Meadow icon theme."""
import configparser
import importlib.util
from pathlib import Path
import unittest
import xml.etree.ElementTree as ET

BASE = Path(__file__).resolve().parents[1] / "icons"
THEME = BASE / "Meadow"
spec = importlib.util.spec_from_file_location("generate_meadow_icons", BASE / "generate_icons.py")
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)


class IconThemeTests(unittest.TestCase):
    def test_build_is_repeatable_without_mutating_symbolic_sources(self):
        original = dict(generator.GLYPHS)
        first = dict(generator.build())
        self.assertEqual(generator.GLYPHS, original)
        self.assertEqual(dict(generator.build()), first)

    def test_rebuild_matches_all_packaged_assets(self):
        for name, content in generator.build().items():
            with self.subTest(name=name):
                self.assertEqual((THEME / name).read_text(), content)

    def test_linux_filenames_are_exact_and_have_no_casefold_collisions(self):
        expected = set(generator.build())
        actual = {str(path.relative_to(THEME)) for path in THEME.rglob("*") if path.is_file()}
        self.assertEqual(actual, expected)
        self.assertEqual(len(expected), len({name.casefold() for name in expected}))
        self.assertEqual(len(actual), len({name.casefold() for name in actual}))
        self.assertIn("scalable/apps/terminal.svg", actual)
        self.assertIn("scalable/apps/thunar.svg", actual)

    def test_generator_rejects_casefold_collision(self):
        generator.build()
        with self.assertRaisesRegex(ValueError, "Case-folded icon name collision"):
            generator.register("apps", "TERMINAL", generator.ICONS["scalable/apps/terminal.svg"])

    def test_theme_search_paths_and_fallback(self):
        theme = configparser.ConfigParser()
        theme.read(THEME / "index.theme")
        self.assertEqual(theme["Icon Theme"]["Inherits"], "Adwaita,hicolor")
        for directory in theme["Icon Theme"]["Directories"].split(","):
            self.assertTrue((THEME / directory).is_dir())
            self.assertEqual(theme[directory]["Type"], "Scalable")

    def test_every_shipped_xfce_application_has_native_artwork(self):
        names = ["org.xfce.thunar", "org.xfce.filemanager", "org.xfce.terminal", "org.xfce.terminalemulator", "org.xfce.terminal-settings", "org.xfce.settings.manager", "org.xfce.settings.appearance", "org.xfce.settings.display", "org.xfce.settings.keyboard", "org.xfce.settings.mouse", "org.xfce.settings.accessibility", "org.xfce.settings.default-applications", "org.xfce.settings.color", "org.xfce.settings.editor", "org.xfce.session", "org.xfce.panel", "org.xfce.xfdesktop", "org.xfce.xfwm4", "org.xfce.xfwm4-tweaks", "org.xfce.workspaces", "org.xfce.about", "org.xfce.appfinder", "org.xfce.mailreader", "org.xfce.webbrowser", "xfsm-logout"]
        for name in names:
            self.assertTrue((THEME / "scalable/apps" / (name + ".svg")).is_file(), name)

    def test_symbolic_navigation_is_single_colour_and_has_no_canvas(self):
        for file in THEME.glob("scalable/*/*-symbolic.svg"):
            root = ET.parse(file).getroot()
            colours = {value for node in root.iter() for key, value in node.attrib.items() if key in ("fill", "stroke") and value != "none"}
            self.assertEqual(colours, {"#61736e"}, str(file))
            self.assertNotIn("-symbolic-symbolic", file.stem)

    def test_symbolic_shapes_are_real_filled_outlines_for_gtk(self):
        for file in THEME.glob("scalable/*/*-symbolic.svg"):
            root = ET.parse(file).getroot()
            for node in root.iter():
                self.assertNotIn("stroke", node.attrib, str(file))
                self.assertNotEqual(node.attrib.get("fill"), "none", str(file))

    def test_standard_folders_use_glass_artwork_and_separate_sidebar_symbols(self):
        places = THEME / "scalable/places"
        blue_folder = (places / "folder.svg").read_text()
        for name in ("folder-open", "inode-directory", "folder-home", "folder-documents", "folder-download", "folder-downloads", "folder-pictures", "folder-music", "folder-videos", "folder-publicshare", "folder-templates", "folder-desktop", "folder-favorites", "folder-recent", "folder-remote", "folder-saved-search", "user-desktop"):
            with self.subTest(name=name):
                self.assertEqual((places / (name + ".svg")).read_text(), blue_folder)
                self.assertNotEqual((places / (name + "-symbolic.svg")).read_text(), blue_folder)
        for name in ("system-file-manager", "org.xfce.thunar", "thunar", "org.xfce.filemanager"):
            application = (THEME / "scalable/apps" / (name + ".svg")).read_text()
            self.assertEqual(application, generator.files_tile())
            self.assertNotEqual(application, blue_folder)

    def test_nonbrand_applications_have_one_common_translucent_optical_plate(self):
        ns = {"svg": "http://www.w3.org/2000/svg"}
        for file in (THEME / "scalable/apps").glob("*.svg"):
            if file.stem in {'meadow', 'meadow-os', 'meadow-desktop', 'meadow-shell'}:
                continue
            with self.subTest(icon=file.name):
                root = ET.parse(file).getroot()
                self.assertEqual(root.attrib["viewBox"], "0 0 64 64")
                faces = root.findall("svg:rect[@fill='url(#tile)']", ns)
                self.assertEqual(len(faces), 1)
                self.assertEqual(tuple(faces[0].attrib[key] for key in ("x", "y", "width", "height")),
                                 ("5", "4", "54", "54"))
                stops = root.findall(".//svg:linearGradient[@id='tile']/svg:stop", ns)
                self.assertGreaterEqual(len(stops), 2)
                self.assertTrue(all(0 < float(stop.attrib["stop-opacity"]) < 1 for stop in stops))

    def test_meadow_brand_aliases_are_one_standalone_green_wave_without_tile(self):
        ns = {"svg": "http://www.w3.org/2000/svg"}
        expected = generator.meadow_mark()
        for name in ('meadow', 'meadow-os', 'meadow-desktop', 'meadow-shell'):
            self.assertEqual((THEME/'scalable/apps'/(name+'.svg')).read_text(), expected)
        for path, content in generator.brand_assets().items():
            self.assertEqual(content, expected)
            self.assertEqual(path.read_text(), expected)
        root = ET.fromstring(expected)
        self.assertEqual(root.attrib['viewBox'], '0 0 64 64')
        self.assertFalse(root.findall('.//svg:rect', ns))
        self.assertFalse(root.findall('.//svg:linearGradient', ns))
        group = root.find('svg:g', ns)
        self.assertEqual(group.attrib['stroke'], '#0bae79')
        self.assertEqual(group.attrib['fill'], 'none')
        self.assertEqual(group.attrib['stroke-linecap'], 'round')
        paths = root.findall('.//svg:path', ns)
        self.assertEqual(len(paths), 1)
        self.assertEqual(paths[0].attrib['d'].count('M'), 2)

    def test_static_folder_material_has_translucent_layers_and_no_fabricated_contents(self):
        root = ET.parse(THEME / "scalable/places/folder.svg").getroot()
        ns = {"svg": "http://www.w3.org/2000/svg"}
        gradients = {node.attrib["id"]: node for node in root.findall("svg:defs/*", ns)
                     if node.tag.endswith("Gradient")}
        painted = [node for child in root if child.tag != "{http://www.w3.org/2000/svg}defs"
                   for node in child.iter()]
        for node in painted:
            if node.tag.split("}")[-1] not in ("path", "ellipse"):
                continue
            fill = node.attrib.get("fill", "#000000")
            if fill.startswith("url(#"):
                stops = gradients[fill[5:-1]].findall("svg:stop", ns)
                self.assertTrue(stops)
                self.assertLess(max(float(stop.attrib.get("stop-opacity", 1)) for stop in stops), 1)
            elif fill != "none":
                self.assertEqual(fill, "#6ba9ed")
                self.assertLess(float(node.attrib.get("fill-opacity", 1)), 1)
        # Soften only the folder's own back layer, never an external image or
        # invented document. Keep the sharp back outside the translucent cover.
        self.assertIsNotNone(root.find(".//svg:clipPath[@id='outside-cover']", ns))
        self.assertIsNotNone(root.find(".//svg:filter[@id='frost']/svg:feGaussianBlur", ns))
        self.assertFalse(root.findall(".//svg:rect", ns))

    def test_folder_colour_menu_swatches_are_small_circles_with_transparent_surroundings(self):
        palette = {"frost": "#c7d3db", "blue": "#6ba9ed", "mint": "#77bfa9",
                   "lilac": "#ad91d0", "peach": "#e6ae8d", "rose": "#d895a8", "graphite": "#45505c"}
        for name, colour in palette.items():
            with self.subTest(name=name):
                root = ET.parse(THEME / "scalable/actions" / ("meadow-folder-color-" + name + ".svg")).getroot()
                self.assertEqual(root.attrib["viewBox"], "0 0 24 24")
                self.assertEqual(len(root), 1)
                circle = root[0]
                self.assertEqual(circle.tag, "{http://www.w3.org/2000/svg}circle")
                self.assertEqual(circle.attrib["fill"], colour)
                self.assertEqual(float(circle.attrib["r"]) * 2 + float(circle.attrib["stroke-width"]), 12)
                self.assertEqual((circle.attrib["cx"], circle.attrib["cy"]), ("12", "12"))

    def test_common_mime_families_have_explicit_native_icon_aliases(self):
        # Product coverage examples are independent of the generator's alias
        # table: dropping a common supported name must fail this contract.
        examples = {
            "generic": ("application-octet-stream", "application-x-generic", "unknown", "application-x-empty"),
            "text": ("text-plain", "text-x-generic"),
            "code": ("application-json", "text-x-python", "text-x-shellscript", "text-html"),
            "markdown": ("text-markdown", "text-x-markdown"),
            "word": ("application-msword", "application-vnd.openxmlformats-officedocument.wordprocessingml.document", "application-vnd.oasis.opendocument.text"),
            "pdf": ("application-pdf",),
            "sheet": ("text-csv", "text-tab-separated-values", "application-vnd.ms-excel", "application-vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application-vnd.oasis.opendocument.spreadsheet"),
            "slides": ("application-vnd.ms-powerpoint", "application-vnd.openxmlformats-officedocument.presentationml.presentation", "application-vnd.oasis.opendocument.presentation"),
            "image": ("image-x-generic", "image-png", "image-jpeg", "image-webp", "image-svg+xml"),
            "audio": ("audio-x-generic", "audio-mpeg", "audio-flac", "audio-ogg", "audio-x-wav"),
            "video": ("video-x-generic", "video-mp4", "video-webm", "video-x-matroska"),
            "archive": ("package-x-generic", "application-zip", "application-x-tar", "application-gzip", "application-x-7z-compressed", "application-vnd.rar"),
            "font": ("font-x-generic", "font-ttf", "font-otf", "font-woff", "font-woff2"),
            "executable": ("application-x-executable", "application-x-pie-executable", "application-x-sharedlib", "application-vnd.microsoft.portable-executable"),
        }
        for kind, names in examples.items():
            for name in names:
                with self.subTest(kind=kind, name=name):
                    self.assertEqual((THEME / "scalable/mimetypes" / (name + ".svg")).read_text(), generator.document(kind))
        app_icon = (THEME / "scalable/apps/application-x-executable.svg").read_text()
        self.assertEqual(app_icon, generator.tile("grid"))
        self.assertNotEqual(app_icon, generator.document("executable"))

    def test_file_families_have_distinct_font_independent_type_glyphs(self):
        artwork = [generator.document(kind) for kind in generator.DOCUMENT_ALIASES]
        self.assertEqual(len(artwork), 14)
        self.assertEqual(len(set(artwork)), 14)
        ns = {"svg": "http://www.w3.org/2000/svg"}
        glyphs = []
        for content in artwork:
            root = ET.fromstring(content)
            self.assertFalse(root.findall(".//svg:text", ns))
            self.assertFalse(root.findall(".//svg:image", ns))
            glyph = root.find("svg:g", ns)
            self.assertIsNotNone(glyph)
            glyphs.append(tuple((node.tag, tuple(node.attrib.items())) for node in glyph))
        self.assertEqual(len(set(glyphs)), 14)

    def test_file_type_artwork_uses_a_shared_translucent_glass_sheet(self):
        ns = {"svg": "http://www.w3.org/2000/svg"}
        bodies = []
        for kind in generator.DOCUMENT_ALIASES:
            root = ET.fromstring(generator.document(kind))
            glass = root.find(".//svg:linearGradient[@id='glass']", ns)
            self.assertIsNotNone(glass)
            stops = glass.findall("svg:stop", ns)
            self.assertTrue(all(0 < float(stop.attrib.get("stop-opacity", 1)) < 1 for stop in stops))
            bodies.append(root.find("svg:path[@fill='url(#glass)']", ns).attrib["d"])
        self.assertEqual(len(set(bodies)), 1)

    def test_approved_glass_browser_icons_and_no_external_svg_references(self):
        blocked = ("libreoffice", "python", "apple")
        browser = (THEME / "scalable/apps/meadow-browser.svg").read_text()
        for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "firefox"):
            self.assertEqual((THEME / "scalable/apps" / (name+".svg")).read_text(), browser)
        for file in THEME.rglob("*.svg"):
            if file.parent.name == "apps":
                self.assertFalse(any(brand in file.stem.lower() for brand in blocked), str(file))
            root = ET.parse(file).getroot()
            self.assertEqual(root.tag, "{http://www.w3.org/2000/svg}svg")
            for node in root.iter():
                self.assertNotIn(node.tag.split("}")[-1], ("image", "script", "foreignObject"))
                self.assertFalse(any(key.endswith("href") for key in node.attrib))


if __name__ == "__main__":
    unittest.main()
