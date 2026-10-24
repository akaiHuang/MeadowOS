"""Focus belongs to an explicit fullscreen activation, never the compact orb."""
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest

SOURCE = Path(__file__).resolve().parents[1] / "voiceos_surface.py"
spec = importlib.util.spec_from_file_location("voiceos_focus", SOURCE)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class VoiceFocusTests(unittest.TestCase):
    def setUp(self):
        self.calls = []
        self.view = module.VoiceOSSurface.__new__(module.VoiceOSSurface)
        self.view.closed = self.view.enabled = self.view.expanded = self.view.terminated = False
        self.view.focus_timestamp = 0
        self.view.escape_generation = 0
        self.view.escape_timer = None
        self.view.companions = {}
        self.mapped = False
        self.native = SimpleNamespace(focus=lambda stamp: self.calls.append(("x-focus", stamp)))
        self.view.window = SimpleNamespace(
            set_accept_focus=lambda value: self.calls.append(("accept", value)),
            set_focus_on_map=lambda value: self.calls.append(("focus-on-map", value)),
            realize=lambda: None,
            get_window=lambda: self.native,
            get_mapped=lambda: self.mapped,
            present_with_time=lambda stamp: self.calls.append(("present", stamp)),
            unfullscreen=lambda: self.calls.append(("unfullscreen",)),
            destroy=lambda: None)
        self.view.view = SimpleNamespace(grab_focus=lambda: self.calls.append(("web-focus",)),
                                         stop_loading=lambda: None)
        self.view.Gtk = SimpleNamespace(get_current_event_time=lambda: 12345)
        self.server_time = 54321
        self.view.GdkX11 = SimpleNamespace(x11_get_server_time=self.timestamp)
        self.view.Gdk = SimpleNamespace(WindowState=SimpleNamespace(FULLSCREEN=16))
        self.view.GLib = SimpleNamespace(idle_add=lambda callback: None)
        self.view.on_mode_change = lambda value: self.calls.append(("mode", value))
        self.view._render_mode = lambda: None

    def focus_calls(self):
        return [row for row in self.calls if row[0] in {"present", "x-focus", "web-focus"}]

    def timestamp(self, _window):
        self.calls.append(("server-time", self.server_time))
        return self.server_time

    def test_initial_unmapped_activation_claims_x_and_web_focus_after_map(self):
        self.view.expand()
        self.assertIn(("focus-on-map", True), self.calls)
        self.assertEqual(self.focus_calls(), [])
        self.mapped = True
        self.view._mapped()
        self.assertEqual(self.focus_calls(), [("present", 54321), ("x-focus", 54321), ("web-focus",)])

    def test_fullscreen_ack_survives_wm_fallback_after_active_work_is_hidden(self):
        self.view.expand()
        self.mapped = True
        # Hiding the active work window causes Xfwm to focus Files after the
        # click. X11 rejects any older focus request, even when GTK is mapped.
        last_focus_change = 23456
        accepted = []
        self.native.focus = lambda stamp: accepted.append(stamp) if stamp >= last_focus_change else None
        self.server_time = 34567
        self.view._window_state(None, SimpleNamespace(changed_mask=16, new_window_state=16))
        self.assertEqual(self.view.focus_timestamp, 12345)
        self.assertEqual(accepted, [34567])
        self.assertIn(("present", 34567), self.calls)

    def test_compact_ignores_late_map_and_fullscreen_ack(self):
        self.view.expand()
        self.view.collapse()
        self.mapped = True
        self.calls.clear()
        self.view._mapped()
        self.view._window_state(None, SimpleNamespace(changed_mask=16, new_window_state=16))
        self.view._focus_expanded()
        self.assertEqual(self.focus_calls(), [])
        self.assertFalse(any(row[0] == "server-time" for row in self.calls))
        self.assertFalse(self.view.expanded)

    def test_close_and_unrelated_window_state_never_request_focus(self):
        self.mapped = True
        self.view.expand()
        self.calls.clear()
        self.view._window_state(None, SimpleNamespace(changed_mask=32, new_window_state=16))
        self.assertEqual(self.focus_calls(), [])
        self.view.close()
        self.view._mapped()
        self.assertEqual(self.focus_calls(), [])
        self.assertFalse(any(row[0] == "server-time" for row in self.calls))

    def test_expansion_without_input_event_uses_actual_x_server_time(self):
        self.view.Gtk.get_current_event_time = lambda: 0
        self.mapped = True
        self.view.expand()
        self.assertIn(("x-focus", 54321), self.calls)

    def test_stale_compact_placement_cannot_shrink_reopened_or_closed_window(self):
        self.view.enabled = self.view.expanded = True
        self.assertFalse(self.view._place_compact())
        self.view.expanded = False
        self.view.closed = True
        self.assertFalse(self.view._place_compact())


if __name__ == "__main__":
    unittest.main()
