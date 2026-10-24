"""Validate shipped Xcursor bytes, alpha and lookup aliases without a display."""
import configparser
from pathlib import Path
import struct
import unittest

ROOT = Path(__file__).resolve().parents[1] / "icons" / "Meadow-Cursors"
GROUPS = (
    "e-resize w-resize ew-resize left_side right_side sb_h_double_arrow h_double_arrow col-resize",
    "n-resize s-resize ns-resize top_side bottom_side sb_v_double_arrow v_double_arrow row-resize",
    "nw-resize se-resize nwse-resize top_left_corner bottom_right_corner size_fdiag",
    "ne-resize sw-resize nesw-resize top_right_corner bottom_left_corner size_bdiag",
)
POINTER_ALIASES = 'left_ptr default arrow top_left_arrow'
SIZES = [24, 32, 48, 64, 96, 128]
IMAGE_TYPE = 0xFFFD0002


def parse_xcursor(data):
    if len(data) < 16 or data[:4] != b"Xcur":
        raise ValueError("Invalid Xcursor file header")
    header, version, count = struct.unpack_from("<3I", data, 4)
    if header != 16 or version != 0x10000 or not 1 <= count <= 16 or header + count * 12 > len(data):
        raise ValueError("Invalid Xcursor table")
    images, previous_end = [], header + count * 12
    for i in range(count):
        kind, nominal, position = struct.unpack_from("<3I", data, header + i * 12)
        if kind != IMAGE_TYPE or position != previous_end or position + 36 > len(data):
            raise ValueError("Invalid Xcursor image position")
        length, chunk_type, subtype, chunk_version, width, height, hot_x, hot_y, delay = struct.unpack_from("<9I", data, position)
        if (length, chunk_type, subtype, chunk_version) != (36, IMAGE_TYPE, nominal, 1):
            raise ValueError("Invalid Xcursor image header")
        if width <= 0 or height <= 0 or width > 256 or height > 256:
            raise ValueError("Unbounded Xcursor image")
        if hot_x >= width or hot_y >= height:
            raise ValueError("Xcursor hotspot outside image")
        previous_end = position + length + width * height * 4
        if previous_end > len(data):
            raise ValueError("Truncated Xcursor pixels")
        pixels = struct.unpack_from("<" + str(width * height) + "I", data, position + length)
        images.append((nominal, width, height, hot_x, hot_y, delay, pixels))
    if previous_end != len(data):
        raise ValueError("Unexpected trailing Xcursor bytes")
    return images


class CursorThemeTest(unittest.TestCase):
    def test_real_theme_and_all_lookup_aliases_are_regular_files(self):
        config = configparser.ConfigParser()
        config.read(ROOT / "index.theme")
        self.assertEqual(config["Icon Theme"]["Name"], "Meadow-Cursors")
        self.assertEqual(config["Icon Theme"]["Inherits"], "Adwaita")
        expected = {name for group in (*GROUPS, POINTER_ALIASES) for name in group.split()}
        self.assertEqual({path.name for path in (ROOT / "cursors").iterdir()}, expected)
        distinct = []
        for group in (*GROUPS, POINTER_ALIASES):
            contents = []
            for name in group.split():
                path = ROOT / "cursors" / name
                self.assertTrue(path.is_file())
                self.assertFalse(path.is_symlink())
                contents.append(path.read_bytes())
            self.assertEqual(len(set(contents)), 1)
            distinct.append(contents[0])
        self.assertEqual(len(set(distinct)), 5)

    def test_all_shipped_images_have_valid_size_hotspot_and_glass_alpha(self):
        for path in (ROOT / "cursors").iterdir():
            with self.subTest(cursor=path.name):
                images = parse_xcursor(path.read_bytes())
                self.assertEqual([row[0] for row in images], SIZES)
                for nominal, width, height, hot_x, hot_y, delay, pixels in images:
                    hot = ((round(5*nominal/32), round(3*nominal/32))
                           if path.name in POINTER_ALIASES.split() else (nominal//2, nominal//2))
                    self.assertEqual((width, height, hot_x, hot_y, delay),
                                     (nominal, nominal, *hot, 0))
                    alpha = [pixel >> 24 for pixel in pixels]
                    self.assertIn(0, alpha)
                    self.assertTrue(any(0 < a < 255 for a in alpha))
                    self.assertGreater(sum(a > 0 for a in alpha), nominal)
                    # Xcursor stores premultiplied ARGB, including zero RGB in
                    # transparent pixels; invalid channels cause coloured halos.
                    for pixel, a in zip(pixels, alpha):
                        self.assertLessEqual((pixel >> 16) & 255, a)
                        self.assertLessEqual((pixel >> 8) & 255, a)
                        self.assertLessEqual(pixel & 255, a)

    def test_resize_rasters_have_two_separate_compact_white_edged_shapes(self):
        for group in GROUPS:
            name = group.split()[0]
            for size, width, _height, _x, _y, _delay, pixels in parse_xcursor((ROOT/'cursors'/name).read_bytes()):
                with self.subTest(cursor=name, size=size):
                    solid = {(i%width,i//width) for i,p in enumerate(pixels) if p>>24 >= 128}
                    remaining = set(solid); components = []
                    while remaining:
                        todo = [remaining.pop()]; count = 0
                        while todo:
                            x,y = todo.pop(); count += 1
                            for dx in (-1,0,1):
                                for dy in (-1,0,1):
                                    point = (x+dx,y+dy)
                                    if point in remaining: remaining.remove(point); todo.append(point)
                        components.append(count)
                    self.assertEqual(len(components), 2, components)
                    self.assertLess(pixels[(size//2)*width+size//2]>>24, 128, 'no visible centre shaft')
                    xs,ys=zip(*solid)
                    self.assertLessEqual(max(xs)-min(xs)+1, round(size*.8))
                    self.assertLessEqual(max(ys)-min(ys)+1, round(size*.8))
                    white = [p for p in pixels if p>>24 >= 200 and min((p>>16)&255,(p>>8)&255,p&255) >= .9*(p>>24)]
                    self.assertGreaterEqual(len(white), size//2)

    def test_binary_validator_rejects_corrupt_or_truncated_asset(self):
        original = (ROOT / "cursors" / "e-resize").read_bytes()
        for broken in (b"wrong" + original[5:], original[:-1], original + b"extra"):
            with self.subTest(length=len(broken)):
                with self.assertRaises(ValueError):
                    parse_xcursor(broken)


if __name__ == "__main__":
    unittest.main()
