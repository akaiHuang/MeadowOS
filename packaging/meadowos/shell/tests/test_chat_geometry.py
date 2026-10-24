"""Chat bounds retain user placement while keeping the composer on-screen."""
import sys
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from chat_geometry import bounded_geometry, usable_area


class ChatGeometryTests(unittest.TestCase):
    def test_small_monitor_respects_both_panels_before_wm_struts_arrive(self):
        monitor = (0, 0, 640, 480)
        area = usable_area(monitor, monitor, top=34, bottom=98)
        position, size = bounded_geometry((96, 56), (520, 550), area)
        self.assertEqual(area, (0, 34, 640, 348))
        self.assertEqual(size, (520, 332))
        self.assertGreaterEqual(position[1], 34)
        self.assertLessEqual(position[1] + size[1], 382)

    def test_wm_reservation_is_intersected_not_subtracted_twice(self):
        self.assertEqual(
            usable_area((0, 0, 1920, 1080), (0, 34, 1920, 948), top=34, bottom=98),
            (0, 34, 1920, 948))

    def test_normal_drag_and_resize_are_preserved_when_inside_workarea(self):
        desired = ((105, 120), (460, 530))
        self.assertEqual(bounded_geometry(*desired, (0, 34, 1280, 948)), desired)

    def test_stale_saved_geometry_is_recovered_after_resolution_change(self):
        position, size = bounded_geometry((1400, 900), (520, 1600), (0, 34, 800, 468))
        self.assertEqual(size, (520, 452))
        self.assertEqual(position, (272, 42))

    def test_negative_monitor_origin_is_not_clamped_to_primary(self):
        area = usable_area((-1280, 0, 1280, 720), (-1280, 0, 3200, 1080))
        position, size = bounded_geometry((-100, 400), (520, 550), area)
        self.assertEqual(position, (-528, 162))
        self.assertEqual(size, (520, 550))
        self.assertEqual(area, (-1280, 0, 1280, 720))

    def test_tiny_bounds_never_produce_negative_size_or_position_outside_area(self):
        for width, height in ((1, 1), (10, 10), (32, 18), (320, 240)):
            area = (-width, -height, width, height)
            (x, y), (w, h) = bounded_geometry((100, 100), (520, 550), area)
            self.assertGreaterEqual(w, 1)
            self.assertGreaterEqual(h, 1)
            self.assertGreaterEqual(x, area[0])
            self.assertGreaterEqual(y, area[1])
            self.assertLessEqual(x + w, area[0] + width)
            self.assertLessEqual(y + h, area[1] + height)


if __name__ == '__main__':
    unittest.main()
