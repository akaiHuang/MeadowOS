"""Native overlay exit and explicit companion ownership without a GUI."""
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest

SOURCE = Path(__file__).resolve().parents[1] / "voiceos_surface.py"
spec = importlib.util.spec_from_file_location("voiceos_overlay", SOURCE)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class Window:
    def __init__(self, calls, name="voice", above=False):
        self.calls, self.name, self.visible, self.above = calls, name, True, above
        self.transient, self.destroy_handler = None, None
    def set_accept_focus(self, value): self.calls.append((self.name, "accept", value))
    def set_focus_on_map(self, value): self.calls.append((self.name, "focus-on-map", value))
    def hide(self): self.visible = False; self.calls.append((self.name, "hide"))
    def unfullscreen(self): self.calls.append((self.name, "unfullscreen"))
    def get_visible(self): return self.visible
    def get_mapped(self): return self.visible
    def get_window(self): return self
    def present_with_time(self, timestamp): self.calls.append((self.name, "present", timestamp))
    def focus(self, timestamp): self.calls.append((self.name, "focus", timestamp))
    def get_state(self): return 32 if self.above else 0
    def get_transient_for(self): return self.transient
    def set_transient_for(self, value): self.transient = value; self.calls.append((self.name, "transient", value))
    def set_destroy_with_parent(self, value): self.calls.append((self.name, "destroy-with-parent", value))
    def set_keep_above(self, value): self.above = value; self.calls.append((self.name, "above", value))
    def connect(self, _signal, callback): self.destroy_handler = callback; return 81
    def disconnect(self, handler): self.calls.append((self.name, "disconnect", handler))


class OverlayTests(unittest.TestCase):
    def setUp(self):
        self.calls, self.timers, self.javascript = [], {}, []
        self.surface = module.VoiceOSSurface.__new__(module.VoiceOSSurface)
        self.surface.closed = False
        self.surface.enabled = self.surface.expanded = self.surface.ready = True
        self.surface.escape_generation = 0
        self.surface.escape_timer = None
        self.surface.companions = {}
        self.surface.state = {"muted": False, "energy": .4, "compact": False}
        self.surface.window = Window(self.calls)
        self.surface.Gdk = SimpleNamespace(KEY_Escape=65307,
            WindowState=SimpleNamespace(FULLSCREEN=16, ABOVE=32))
        self.surface.GdkX11 = SimpleNamespace(x11_get_server_time=lambda _window: 123)
        self.surface.GLib = SimpleNamespace(timeout_add=self.timeout, source_remove=self.remove,
            idle_add=lambda callback: self.calls.append(("idle", callback)))
        self.surface.on_mode_change = lambda value: self.calls.append(("mode", value))
        self.surface.on_action = lambda kind, payload: self.calls.append(("action", kind, payload))
        self.surface._render = lambda: self.calls.append(("render",))
        self.surface._render_mode = lambda: self.calls.append(("render-mode",))
        self.surface.view = SimpleNamespace(run_javascript=self.run_javascript,
            grab_focus=lambda: self.calls.append(("view", "focus")),
            run_javascript_finish=lambda result: SimpleNamespace(get_js_value=lambda:
                SimpleNamespace(to_string=lambda: result)))

    def timeout(self, _milliseconds, callback, generation):
        timer = len(self.timers) + 1
        self.timers[timer] = (callback, generation)
        return timer

    def remove(self, timer):
        self.calls.append(("remove-timer", timer))

    def run_javascript(self, _script, _cancel, callback, generation):
        self.javascript.append((callback, generation))

    def escape(self):
        return self.surface._key(None, SimpleNamespace(keyval=65307))

    def test_explicit_exit_hides_whole_overlay_and_emits_once(self):
        self.surface.dismiss()
        self.assertFalse(self.surface.enabled)
        self.assertFalse(self.surface.expanded)
        self.assertFalse(self.surface.window.visible)
        self.assertEqual(self.surface.state, {"muted": True, "energy": 0, "compact": True})
        self.assertEqual(self.calls[-2:], [("mode", False), ("action", "exit", {})])
        self.surface.dismiss()
        self.assertEqual(sum(row == ("action", "exit", {}) for row in self.calls), 1)

    def test_work_handoff_collapses_without_ending_voice(self):
        self.surface.collapse()
        self.assertTrue(self.surface.enabled)
        self.assertFalse(self.surface.expanded)
        self.assertNotIn(("action", "exit", {}), self.calls)

    def test_escape_closes_open_palette_before_exiting(self):
        self.assertTrue(self.escape())
        callback, generation = self.javascript.pop()
        callback(self.surface.view, "palette", generation)
        self.assertTrue(self.surface.enabled)
        self.assertTrue(self.escape())
        callback, generation = self.javascript.pop()
        callback(self.surface.view, "exit", generation)
        self.assertFalse(self.surface.enabled)

    def test_escape_without_loaded_renderer_exits_immediately(self):
        self.surface.ready = False
        self.assertTrue(self.escape())
        self.assertFalse(self.surface.enabled)
        self.assertEqual(self.javascript, [])

    def test_hung_renderer_times_out_to_native_exit(self):
        self.escape()
        callback, generation = self.timers[self.surface.escape_timer]
        self.assertFalse(callback(generation))
        self.assertFalse(self.surface.enabled)

    def test_late_escape_reply_does_not_close_reopened_overlay(self):
        self.escape()
        callback, generation = self.javascript.pop()
        self.surface.dismiss()
        self.surface.enabled = self.surface.expanded = True
        callback(self.surface.view, "exit", generation)
        self.assertTrue(self.surface.enabled)

    def test_js_dispatch_failure_still_exits(self):
        def failed(*_args): raise RuntimeError("web process gone")
        self.surface.view.run_javascript = failed
        self.assertTrue(self.escape())
        self.assertFalse(self.surface.enabled)

    def test_companion_layer_is_explicit_and_restores_previous_pin(self):
        original_parent = object()
        chat = Window(self.calls, "chat", above=True)
        chat.transient = original_parent
        self.assertTrue(self.surface.show_companion(chat))
        self.assertIs(chat.transient, self.surface.window)
        self.assertTrue(chat.above)
        self.surface.release_companion(chat)
        self.assertIs(chat.transient, original_parent)
        self.assertTrue(chat.above)
        self.assertEqual(self.surface.companions, {})

    def test_fullscreen_ack_never_steals_focus_from_visible_companion(self):
        chat = Window(self.calls, "chat")
        self.surface.show_companion(chat)
        self.calls.clear()
        self.surface._window_state(None, SimpleNamespace(changed_mask=16, new_window_state=16))
        self.assertEqual(self.calls, [])

    def test_companion_release_uses_current_user_pin_choice(self):
        for initial in (False, True):
            with self.subTest(initial=initial):
                chat = Window(self.calls, "chat", above=initial)
                pin = {"active": initial}
                self.surface.show_companion(chat, lambda: pin["active"])
                pin["active"] = not initial
                self.surface.release_companion(chat)
                self.assertEqual(chat.above, not initial)

    def test_temporary_layer_is_released_independently_from_user_pin(self):
        chat = Window(self.calls, "chat", above=True)
        chrome = {"pinned": True, "overlay": False}
        lifts = []
        def set_overlay_above(value):
            lifts.append(value)
            chrome["overlay"] = value
            chat.set_keep_above(chrome["pinned"] or chrome["overlay"])
        self.surface.show_companion(chat, lambda: chrome["pinned"], set_overlay_above)
        chrome["pinned"] = False
        chat.set_keep_above(chrome["pinned"] or chrome["overlay"])
        self.assertTrue(chat.above)
        self.surface.release_companion(chat)
        self.assertEqual(lifts, [True, False])
        self.assertFalse(chat.above)

    def test_explicit_return_focuses_overlay_only_after_chat_hides(self):
        chat = Window(self.calls, "chat")
        self.surface.show_companion(chat)
        self.calls.clear()
        self.assertFalse(self.surface.focus_overlay())
        self.assertEqual(self.calls, [])
        chat.hide()
        self.surface.release_companion(chat)
        self.calls.clear()
        self.assertFalse(self.surface.focus_overlay())
        self.assertEqual(self.calls, [("voice", "present", 123),
                                    ("voice", "focus", 123), ("view", "focus")])

    def test_explicit_return_never_focuses_disabled_or_compact_overlay(self):
        for enabled, expanded in ((False, False), (True, False), (False, True)):
            with self.subTest(enabled=enabled, expanded=expanded):
                self.surface.enabled, self.surface.expanded = enabled, expanded
                self.calls.clear()
                self.assertFalse(self.surface.focus_overlay())
                self.assertEqual(self.calls, [])

    def test_exit_releases_only_owned_companion_layer(self):
        chat = Window(self.calls, "chat", above=False)
        self.surface.show_companion(chat)
        self.surface.dismiss()
        self.assertIsNone(chat.transient)
        self.assertFalse(chat.above)
        self.assertEqual(self.surface.companions, {})

    def test_destroyed_companion_drops_ownership(self):
        chat = Window(self.calls, "chat")
        self.surface.show_companion(chat)
        chat.destroy_handler(chat)
        self.assertEqual(self.surface.companions, {})


if __name__ == "__main__":
    unittest.main()
