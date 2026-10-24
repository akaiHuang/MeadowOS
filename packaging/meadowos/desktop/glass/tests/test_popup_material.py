"""Optical and request boundaries for sharp native popup foregrounds."""
import unittest

import numpy as np

from glass.server import parse_request, render_request


class SolidBackdrop:
    failed = False

    def __init__(self, value):
        self.value = value
        self.requests = []

    def capture(self, *request):
        self.requests.append(request)
        _, _, _, width, height = request
        return np.full((height, width, 4), self.value, dtype=np.uint8)


class PopupMaterialTests(unittest.TestCase):
    def test_native_popup_and_tooltip_modes_are_bounded(self):
        for mode in ("popup", "popup-dark", "tooltip", "tooltip-dark"):
            with self.subTest(mode=mode):
                result = parse_request(f"123 -4 7 240 180 12 {mode}")
                self.assertEqual(result, (0x123, -4, 7, 240, 180, 12, mode, None))
        for request in ("123 0 0 2049 20 12 popup", "123 0 0 2048 2048 12 popup",
                        "123 0 0 240 180 nan popup", "123 0 0 240 180 12 popup-other"):
            with self.subTest(request=request), self.assertRaises(ValueError):
                parse_request(request)

    def test_popup_edges_are_rounded_and_premultiplied(self):
        result = render_request("123 8 9 240 180 12 popup", SolidBackdrop(255))
        self.assertEqual(result.shape, (180, 240, 4))
        self.assertEqual(result[0, 0, 3], 0)
        self.assertEqual(result[90, 120, 3], 255)
        self.assertTrue(np.all(result[..., :3] <= result[..., 3, None]))

    def test_tint_keeps_native_text_legible_over_extreme_underlays(self):
        # Light fg #293731 is never placed on an unmodified black underlay;
        # dark fg similarly receives a darkened white underlay. This asserts
        # meaningful optical output, independent of CSS selectors or C state.
        for kind in ("popup", "tooltip"):
            light = render_request(f"123 0 0 240 180 12 {kind}", SolidBackdrop(0))
            dark = render_request(f"123 0 0 240 180 12 {kind}-dark", SolidBackdrop(255))
            self.assertGreaterEqual(int(light[90, 120, :3].min()), 160)
            self.assertLessEqual(int(dark[90, 120, :3].max()), 95)

    def test_owner_and_optical_margin_are_preserved(self):
        backdrop = SolidBackdrop(127)
        render_request("ab12 8 9 240 180 12 popup", backdrop)
        self.assertEqual(backdrop.requests, [(0xab12, -32, -31, 320, 260)])

    def test_dialog_content_retains_native_frame_and_has_no_internal_rim(self):
        for mode in ("dialog", "dialog-dark"):
            with self.subTest(mode=mode):
                self.assertEqual(parse_request(f"123 0 34 2560 1200 0 {mode}")[3:5], (2560, 1200))
                result = render_request(f"123 0 34 240 180 0 {mode}", SolidBackdrop(127))
                self.assertTrue(np.all(result[..., 3] == 255))
                self.assertTrue(np.all(result[0, 0] == result[90, 120]))
        with self.assertRaises(ValueError):
            parse_request("123 0 0 4096 2048 0 dialog")


if __name__ == "__main__":
    unittest.main()
