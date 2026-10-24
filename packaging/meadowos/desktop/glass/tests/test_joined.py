"""Actual native IPC request path for the continuous Files material."""
import unittest

import numpy as np

from glass.server import parse_request, render_request


class FakeBackdrop:
    failed = False

    def __init__(self, value=0, stripes=False):
        self.value = value
        self.stripes = stripes
        self.calls = []

    def capture(self, xid, x, y, width, height):
        self.calls.append((xid, x, y, width, height))
        pixels = np.full((height, width, 4), self.value, dtype=np.uint8)
        if self.stripes:
            pixels[:, ::2, :3] = 255-self.value
        pixels[..., 3] = 255
        return pixels


class JoinedMaterialTests(unittest.TestCase):
    def test_one_capture_and_no_inner_corner_or_alpha_gap(self):
        backdrop = FakeBackdrop(64)
        output = render_request("0x14 10 20 240 160 0 split:80", backdrop)
        self.assertEqual(backdrop.calls, [(20, -30, -20, 320, 240)])
        self.assertTrue((output[..., 3] == 255).all())
        # No independent rounded pane rim at top/bottom of the inner join.
        self.assertTrue((output[0, 79:81] == output[80, 79:81]).all())
        self.assertTrue((output[-1, 79:81] == output[80, 79:81]).all())

    def test_sidebar_frost_has_a_contrast_floor_over_dark_underlay(self):
        dark = render_request("1 0 0 240 160 0 split:80", FakeBackdrop(0))
        light = render_request("1 0 0 240 160 0 split:80", FakeBackdrop(255))
        difference = light[..., 0].astype(float)-dark[..., 0]
        self.assertGreater(difference[:, 120:].mean(), 0)
        self.assertLess(difference[:, :60].mean(), difference[:, 120:].mean())
        self.assertGreaterEqual(dark[:, :60, :3].min(), 185)

    def test_body_frosts_actual_high_frequency_background(self):
        output = render_request("1 0 0 240 160 0 split:80", FakeBackdrop(0, stripes=True))
        self.assertLess(output[20:-20, 100:-20, 0].std(), 2)
        self.assertGreater(output[20:-20, 100:-20, 0].mean(), 100)

    def test_no_sidebar_and_dark_profile(self):
        light = render_request("1 0 0 240 160 0 split:0", FakeBackdrop(200))
        dark = render_request("1 0 0 240 160 0 split-dark:0", FakeBackdrop(200))
        self.assertLess(dark[..., :3].mean(), light[..., :3].mean())
        self.assertTrue((dark[0, 0] == dark[-1, -1]).all())

    def test_unchanged_underlay_skips_processing_but_moved_content_and_theme_invalidate(self):
        backdrop=FakeBackdrop(64)
        cache={}
        line="1 0 0 240 160 0 split:80"
        self.assertIsNotNone(render_request(line,backdrop,cache=cache))
        self.assertIsNone(render_request(line,backdrop,cache=cache))
        backdrop.value=80
        self.assertIsNotNone(render_request(line,backdrop,cache=cache))
        self.assertIsNotNone(render_request("1 0 0 240 160 0 split-dark:80",backdrop,cache=cache))
        self.assertIsNotNone(render_request("1 0 0 240 160 0 split-dark:120",backdrop,cache=cache))

    def test_full_hd_allowed_and_requests_remain_bounded(self):
        self.assertEqual(parse_request("1 0 0 1920 1080 0 split:190")[3:5], (1920, 1080))
        for line in ("1 0 0 4096 4096 0 split:190", "1 0 0 4097 100 0 split:10",
                     "1 0 0 240 160 0 split:241", "1 0 0 240 160 0 split:-1",
                     "1 0 0 240 160 0 split:bad", "1 0 0 240 160 nan split:10",
                     "1 0 0 240 160 0 unknown", "0 0 0 240 160 0 split:10"):
            with self.subTest(line=line), self.assertRaises(ValueError):
                parse_request(line)


if __name__ == "__main__":
    unittest.main()
