"""Display-free frame contracts; real pointer/WM acceptance stays in native QA."""
import importlib.util
import json
from copy import deepcopy
from pathlib import Path
import unittest

try:
    from Xlib import Xutil
    from Xlib.protocol.rq import DictWrapper
except ImportError:
    DictWrapper = None

SOURCE = Path(__file__).resolve().parents[1] / "meadow_frame_service.py"
spec = importlib.util.spec_from_file_location("meadow_frame_geometry_test_subject", SOURCE)
frame = importlib.util.module_from_spec(spec)
spec.loader.exec_module(frame)


class FrameGeometryTest(unittest.TestCase):
    @staticmethod
    def normal_checkpoint():
        return {"flags": 16 | 32 | 64 | 128 | 256 | 512,
                "min_width": 80, "min_height": 40, "max_width": 1920, "max_height": 1080,
                "width_inc": 9, "height_inc": 18, "base_width": 4, "base_height": 6,
                "win_gravity": 1, "min_aspect": {"num": 4, "denum": 3},
                "max_aspect": {"num": 16, "denum": 9}}

    def test_eight_directions_move_only_requested_edges(self):
        original = (100, 200, 400, 300)
        expected = {
            0: (130, 220, 370, 280), 1: (100, 220, 400, 280),
            2: (100, 220, 430, 280), 3: (100, 200, 430, 300),
            4: (100, 200, 430, 320), 5: (100, 200, 400, 320),
            6: (130, 200, 370, 320), 7: (130, 200, 370, 300),
        }
        for direction, result in expected.items():
            with self.subTest(direction=direction):
                self.assertEqual(frame.gesture_rect(original, direction, 30, 20), result)

    def test_limits_preserve_opposite_edge_in_all_directions(self):
        original = (100, 200, 400, 300)
        for direction in range(8):
            left = direction in (0, 6, 7)
            right = direction in (2, 3, 4)
            top = direction in (0, 1, 2)
            bottom = direction in (4, 5, 6)
            for delta, expected_width, expected_height in ((-10000, 240, 180), (10000, 640, 480)):
                # Delta describes expansion; invert it for a moving left/top edge.
                dx = -delta if left else delta
                dy = -delta if top else delta
                with self.subTest(direction=direction, expansion=delta):
                    x, y, w, h = frame.gesture_rect(original, direction, dx, dy, (240, 180), (640, 480))
                    self.assertEqual(w, expected_width if left or right else 400)
                    self.assertEqual(h, expected_height if top or bottom else 300)
                    self.assertEqual(x + w if left else x, 500 if left else 100)
                    self.assertEqual(y + h if top else y, 500 if top else 200)

    def test_moving_preserves_size_even_when_resize_bounds_differ(self):
        self.assertEqual(frame.gesture_rect((100, 200, 400, 300), 8, -125, 37, (500, 500), (600, 600)),
                         (-25, 237, 400, 300))

    def test_pointer_movement_is_relative_to_original_not_cumulative(self):
        original = (100, 200, 400, 300)
        self.assertEqual(frame.gesture_rect(original, 3, 10, 0), (100, 200, 410, 300))
        self.assertEqual(frame.gesture_rect(original, 3, 20, 0), (100, 200, 420, 300))
        self.assertEqual(frame.gesture_rect(original, 3, 0, 0), original)

    def test_header_retention_is_owned_and_does_not_reveal_background(self):
        cases = [
            # inside, held, dragging, active, shown, expected
            (True, False, False, False, False, True),
            (False, True, False, False, False, False),
            (False, True, False, False, True, False),
            (False, True, False, True, False, False),
            (False, True, False, True, True, True),
            (False, True, True, False, True, True),
            (False, False, True, True, True, False),
        ]
        for *args, expected in cases:
            with self.subTest(args=args):
                self.assertIs(bool(frame.keep_header(*args)), expected)

    def test_decoration_changes_preserve_client_motif_restrictions(self):
        hints = [1 | 4 | 8, 0x25, 7, 3, 0xAB]
        for enabled in (False, True):
            with self.subTest(enabled=enabled):
                self.assertEqual(frame.motif_decoration(hints, enabled),
                                 [1 | 2 | 4 | 8, 0x25, int(enabled), 3, 0xAB])
                self.assertEqual(hints, [1 | 4 | 8, 0x25, 7, 3, 0xAB])

    def test_absent_motif_hints_only_create_decoration_flag(self):
        self.assertEqual(frame.motif_decoration([], False), [2, 0, 0, 0, 0])
        self.assertEqual(frame.motif_decoration([], True), [2, 0, 1, 0, 0])

    @unittest.skipUnless(DictWrapper is not None, "python-xlib normal-hints protocol types required")
    def test_normal_hints_nested_aspects_survive_json_checkpoint(self):
        flags = Xutil.PMinSize | Xutil.PMaxSize | Xutil.PResizeInc | Xutil.PAspect | Xutil.PBaseSize | Xutil.PWinGravity
        original = DictWrapper({"flags": flags, "min_width": 80, "min_height": 40,
                                "max_width": 1920, "max_height": 1080,
                                "width_inc": 9, "height_inc": 18,
                                "min_aspect": DictWrapper({"num": 4, "denum": 3}),
                                "max_aspect": DictWrapper({"num": 16, "denum": 9}),
                                "base_width": 4, "base_height": 6, "win_gravity": 1})
        record = frame.normal_hint_record(original)
        self.assertEqual(json.loads(json.dumps(record)), record)
        self.assertEqual(record["flags"], flags)
        self.assertEqual(record["min_aspect"], {"num": 4, "denum": 3})
        self.assertEqual(record["max_aspect"], {"num": 16, "denum": 9})
        self.assertEqual((record["width_inc"], record["height_inc"]), (9, 18))
        self.assertEqual((record["min_width"], record["max_width"], record["base_height"], record["win_gravity"]), (80, 1920, 6, 1))
        # The saved original must remain available for cleanup even if active
        # pixel sizing clears resize increments or alters an aspect copy.
        record["flags"] &= ~Xutil.PResizeInc
        record["min_aspect"]["num"] = 99
        self.assertEqual(original.flags, flags)
        self.assertEqual(original.min_aspect.num, 4)

    @unittest.skipUnless(DictWrapper is not None, "python-xlib normal-hints protocol types required")
    def test_normal_hints_record_preserves_unrelated_and_zero_flags(self):
        for flags in (0, Xutil.USPosition | Xutil.USSize | Xutil.PPosition | Xutil.PSize):
            with self.subTest(flags=flags):
                original = DictWrapper({"flags": flags, "x": -8, "y": 40, "width": 800, "height": 600})
                self.assertEqual(frame.normal_hint_record(original),
                                 {"flags": flags, "x": -8, "y": 40, "width": 800, "height": 600})

    def test_saved_normal_hints_accept_none_and_valid_json_without_mutation(self):
        self.assertIsNone(frame.validate_normal_hints(None))
        record = self.normal_checkpoint()
        before = deepcopy(record)
        self.assertEqual(frame.validate_normal_hints(json.loads(json.dumps(record))), before)
        self.assertEqual(record, before)
        limits = deepcopy(record)
        limits.update(flags=2**32-1, min_width=-2**31, max_width=2**31-1)
        limits["min_aspect"] = {"num": -2**31, "denum": 2**31-1}
        self.assertEqual(frame.validate_normal_hints(limits), limits)

    def test_saved_normal_hints_reject_invalid_container_or_field_set(self):
        valid = self.normal_checkpoint()
        missing = deepcopy(valid)
        del missing["width_inc"]
        unknown = dict(valid, unexpected=1)
        for malformed in ([], "hints", 42, {}, missing, unknown):
            with self.subTest(record=malformed):
                with self.assertRaises(ValueError):
                    frame.validate_normal_hints(malformed)

    def test_saved_normal_hints_reject_values_that_cannot_pack_safely(self):
        for field, values in (("flags", (-1, 2**32, True, "64", 64.0)),
                              ("min_width", (-2**31-1, 2**31, True, None, "80", 80.0))):
            for value in values:
                with self.subTest(field=field, value=value):
                    malformed = self.normal_checkpoint()
                    malformed[field] = value
                    with self.assertRaises(ValueError):
                        frame.validate_normal_hints(malformed)

    def test_saved_normal_hints_reject_malformed_nested_aspect(self):
        for value in (None, [], {"num": 4}, {"num": 4, "denum": 3, "extra": 0},
                      {"num": True, "denum": 3}, {"num": 4, "denum": 2**31},
                      {"num": -2**31-1, "denum": 3}, {"num": "4", "denum": 3}):
            for field in ("min_aspect", "max_aspect"):
                with self.subTest(field=field, value=value):
                    malformed = self.normal_checkpoint()
                    malformed[field] = value
                    with self.assertRaises(ValueError):
                        frame.validate_normal_hints(malformed)


if __name__ == "__main__":
    unittest.main()
