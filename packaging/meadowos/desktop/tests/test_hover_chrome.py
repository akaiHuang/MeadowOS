"""Native chrome event/geometry policy without starting a display or Agent.

These checks protect event races and input masks. Actual GTK painting, WM grabs,
and material rendering still require a separate native desktop acceptance run.
"""
import ast
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

SOURCE = Path(__file__).resolve().parents[1] / "meadow_window_chrome.py"
spec = importlib.util.spec_from_file_location("meadow_window_chrome", SOURCE)
chrome = importlib.util.module_from_spec(spec)
spec.loader.exec_module(chrome)


class InputMaskTest(unittest.TestCase):
    def pixels(self, width, height, radius):
        pixels = set()
        for x, y, w, h in chrome.rounded_input_rows(width, height, radius):
            self.assertGreater(w, 0)
            self.assertGreater(h, 0)
            self.assertGreaterEqual(x, 0)
            self.assertGreaterEqual(y, 0)
            self.assertLessEqual(x + w, width)
            self.assertLessEqual(y + h, height)
            pixels.update((column, row) for column in range(x, x + w) for row in range(y, y + h))
        return pixels

    def test_rounded_corners_do_not_intercept_the_window_behind(self):
        pixels = self.pixels(360, 420, 16)
        for corner in ((0, 0), (359, 0), (0, 419), (359, 419)):
            self.assertNotIn(corner, pixels)
        # The central top strip stays reachable without a visible handle.
        for y in range(chrome.TRIGGER_HEIGHT):
            self.assertIn((180, y), pixels)
        for edge in ((180, 0), (0, 210), (359, 210), (180, 419)):
            self.assertIn(edge, pixels)

    def test_maximized_mask_preserves_every_edge_pixel(self):
        self.assertEqual(len(self.pixels(80, 40, 0)), 3200)

    def test_tiny_allocations_never_produce_invalid_regions(self):
        for width, height in ((1, 1), (2, 2), (7, 3), (3, 7), (32, 32)):
            self.pixels(width, height, 16)
        self.assertEqual(chrome.rounded_input_rows(0, 200, 16), [])


class ChromeEventTest(unittest.TestCase):
    def setUp(self):
        tree = ast.parse(SOURCE.read_text())
        native = next(node for node in ast.walk(tree) if isinstance(node, ast.ClassDef) and node.name == "HoverChrome")
        selected = {"cancel_hide", "reveal", "controls_focused", "hide_if_idle", "dismiss", "finish_hiding", "unmapped", "pointer_status", "window_key", "window_state", "destroyed", "notify_material_geometry", "schedule_hide"}
        methods = [node for node in native.body if isinstance(node, ast.FunctionDef) and node.name in selected]
        self.callbacks, self.removed = {}, []

        def timeout(_delay, callback):
            timer = len(self.callbacks) + 1
            self.callbacks[timer] = callback
            return timer

        context = {"GLib": SimpleNamespace(timeout_add=timeout, source_remove=self.removed.append),
                   "Gdk": SimpleNamespace(KEY_F10=10, KEY_space=32, KEY_Escape=27,
                       WindowState=SimpleNamespace(MAXIMIZED=2, FULLSCREEN=4),
                       ModifierType=SimpleNamespace(BUTTON1_MASK=256, BUTTON2_MASK=512, BUTTON3_MASK=1024,
                           CONTROL_MASK=4, MOD1_MASK=8, SHIFT_MASK=1, SUPER_MASK=64)),
                   "can_auto_hide": chrome.can_auto_hide, "HIDE_DELAY_MS": chrome.HIDE_DELAY_MS,
                   "TRANSITION_MS": chrome.TRANSITION_MS, "TOOLBAR_HEIGHT": chrome.TOOLBAR_HEIGHT,
                   "CORNER_RADIUS": chrome.CORNER_RADIUS}
        exec(compile(ast.Module(body=methods, type_ignores=[]), str(SOURCE), "exec"), context)
        self.view = SimpleNamespace(pointer_timer=None,hide_timer=None, fade_timer=None, fullscreen=False, sticky=False,
                                    focus_content=Mock(), restore_focus=None, alive=True,
                                    radius=16, composited=True, geometry_changed=None, reported_geometry=None)
        for name in selected:
            setattr(self.view, name, lambda *args, key=name, **kwargs: context[key](self.view, *args, **kwargs))
        self.visible, self.mapped, self.classes, self.pass_through = False, False, set(), True
        style = SimpleNamespace(add_class=self.classes.add, remove_class=self.classes.discard,
                                has_class=lambda name: name in self.classes)
        self.focus = object()
        self.view.owner = Mock(get_style_context=lambda: style, get_focus=lambda: self.focus)
        self.view.bar = Mock()
        self.view.revealer = SimpleNamespace(get_reveal_child=lambda: self.visible,
            set_reveal_child=lambda value: setattr(self, "visible", value),
            show=lambda: setattr(self, "mapped", True), hide=lambda: setattr(self, "mapped", False))
        self.view.buttons = [Mock()]
        self.view.set_overlay_pass_through = lambda _child, value: setattr(self, "pass_through", value)
        self.view.controls_focused = lambda: False
        self.view.pointer_status = lambda: (False, False)
        self.native_pointer_status = lambda: context["pointer_status"](self.view)

    def test_keyboard_reveal_keeps_controls_until_explicit_dismiss(self):
        self.view.reveal(keyboard=True)
        self.assertTrue(self.visible)
        self.assertTrue(self.view.sticky)
        self.view.bar.set_sensitive.assert_called_with(True)
        self.view.buttons[0].grab_focus.assert_called_once()
        self.view.hide_if_idle()
        self.assertTrue(self.visible)
        self.view.dismiss(focus_input=True)
        self.view.focus_content.assert_called_once()
        self.assertFalse(self.visible)
        self.assertFalse(self.visible)

    def test_reenter_during_fade_cancels_stale_unmap_and_preserves_corner_owner(self):
        self.view.reveal()
        self.view.dismiss()
        pending = self.view.fade_timer
        self.view.reveal()
        self.assertIn(pending, self.removed)
        # Even an already-delivered stale callback cannot hide a revealed bar.
        self.callbacks[pending]()
        self.assertTrue(self.mapped)
        self.assertIn("meadow-chrome-visible", self.classes)

    def test_wm_drag_release_does_not_need_a_widget_release_event(self):
        self.view.reveal()
        self.view.pointer_status = lambda: (False, True)
        self.view.hide_if_idle()
        self.assertTrue(self.visible)
        retry = self.view.hide_timer
        self.assertIsNotNone(retry)
        self.view.pointer_status = lambda: (False, False)
        self.callbacks[retry]()
        self.assertFalse(self.visible)

    def test_pointer_or_keyboard_focus_holds_controls(self):
        for inside, focused in ((True, False), (False, True)):
            self.view.reveal()
            self.view.pointer_status = lambda: (inside, False)
            self.view.controls_focused = lambda: focused
            self.view.hide_if_idle()
            self.assertTrue(self.visible)

    def test_unmap_cancels_timers_and_reopen_starts_without_controls(self):
        self.view.reveal()
        self.view.dismiss()
        pending = self.view.fade_timer
        self.view.unmapped()
        self.assertIn(pending, self.removed)
        self.assertFalse(self.visible)
        self.assertFalse(self.mapped)
        self.assertFalse(self.visible)
        self.assertNotIn("meadow-chrome-visible", self.classes)

    def test_hover_never_calls_owner_geometry_or_focus_operations(self):
        self.view.reveal()
        self.view.hide_if_idle()
        self.view.finish_hiding()
        self.view.owner.move.assert_not_called()
        self.view.owner.resize.assert_not_called()
        self.view.owner.maximize.assert_not_called()
        self.view.focus_content.assert_not_called()

    def test_fullscreen_retains_the_application_input_surface(self):
        self.view.fullscreen = True
        self.view.reveal(keyboard=True)
        self.assertFalse(self.visible)
        self.assertFalse(self.visible)
        self.view.buttons[0].grab_focus.assert_not_called()

    def test_occluding_foreign_window_does_not_keep_toolbar_revealed(self):
        actual = SimpleNamespace(get_effective_toplevel=lambda: "different-owner")
        pointer = SimpleNamespace(get_window_at_position=lambda: (actual, 20, 5))
        drawable = SimpleNamespace(get_effective_toplevel=lambda: "owner",
            get_device_position=lambda _pointer: (actual, 20, 5, 256))
        self.view.owner.get_window.return_value = drawable
        self.view.owner.get_display.return_value = SimpleNamespace(
            get_default_seat=lambda: SimpleNamespace(get_pointer=lambda: pointer))
        self.view.get_allocation = lambda: SimpleNamespace(width=360, height=420)
        self.view.translate_coordinates = lambda *_args: (0, 0)
        self.assertEqual(self.native_pointer_status(), (False, True))
        actual.get_effective_toplevel = lambda: "owner"
        self.assertEqual(self.native_pointer_status(), (True, True))
        drawable.get_device_position = lambda _pointer: (None, 20, 5, 0)
        # A windowless body returns no child; the actual owner still reveals.
        self.assertEqual(self.native_pointer_status(), (True, False))
        pointer.get_window_at_position = lambda: (None, 0, 0)
        self.assertEqual(self.native_pointer_status(), (False, False))

    def test_shared_keyboard_shortcut_preserves_other_modified_keys(self):
        for key, modifiers, accepted in ((32, 12, True), (10, 0, True), (10, 8, False),
                                         (32, 4, False), (32, 8, False), (32, 13, False),
                                         (32, 76, False), (10, 4, False)):
            self.view.unmapped()
            result = self.view.window_key(None, SimpleNamespace(keyval=key, state=modifiers))
            self.assertEqual(result, accepted, (key, modifiers))
            self.assertEqual(self.visible, accepted, (key, modifiers))
        self.view.fullscreen = True
        self.view.unmapped()
        self.assertFalse(self.view.window_key(None, SimpleNamespace(keyval=32, state=12)))

    def test_material_corners_change_with_the_visible_corner_owner(self):
        changes = []
        self.view.geometry_changed = lambda radius, revealed: changes.append((radius, revealed))
        self.view.notify_material_geometry()
        self.view.reveal()
        self.view.dismiss()
        self.assertEqual(changes, [(16, False), (16, True)])
        self.view.finish_hiding()
        self.view.radius = 0
        self.view.notify_material_geometry()
        self.assertEqual(changes[-2:], [(16, False), (0, False)])
        self.view.notify_material_geometry()
        self.assertEqual(len(changes), 4)

    def test_destroy_cancels_timers_and_late_callbacks_cannot_revive_them(self):
        self.view.screen, self.view.screen_handler = Mock(), 17
        self.view.reveal()
        self.view.schedule_hide()
        pending_hide = self.view.hide_timer
        self.view.dismiss()
        pending_fade = self.view.fade_timer
        self.view.destroyed()
        self.assertIn(pending_hide, self.removed)
        self.assertIn(pending_fade, self.removed)
        self.view.screen.disconnect.assert_called_once_with(17)
        count = len(self.callbacks)
        for call in (self.view.reveal, self.view.schedule_hide, self.view.dismiss,
                     self.view.hide_if_idle, self.view.finish_hiding, self.view.destroyed):
            call()
        self.assertEqual(len(self.callbacks), count)
        self.assertFalse(self.visible)
        self.view.screen.disconnect.assert_called_once_with(17)

    def test_destroy_disconnects_screen_even_when_widget_cleanup_raises(self):
        self.view.screen, self.view.screen_handler = Mock(), 17
        self.view.unmapped = Mock(side_effect=RuntimeError("already finalized"))
        with self.assertRaises(RuntimeError):
            self.view.destroyed()
        self.view.screen.disconnect.assert_called_once_with(17)
        self.assertIsNone(self.view.screen_handler)

    def test_maximized_edges_are_unmapped_but_toolbar_trigger_remains_available(self):
        self.view.trigger, self.view.edges = Mock(), [Mock() for _ in range(8)]
        self.view.get_allocation, self.view.allocation_changed, self.view.queue_draw = Mock(), Mock(), Mock()
        self.view.window_state(None, SimpleNamespace(new_window_state=2))
        self.view.trigger.hide.assert_called_once()
        self.view.trigger.show.assert_not_called()
        for edge in self.view.edges:
            edge.set_visible.assert_called_with(False)
        self.assertEqual(self.view.radius, 0)
        self.view.window_state(None, SimpleNamespace(new_window_state=0))
        for edge in self.view.edges:
            edge.set_visible.assert_called_with(True)
        self.assertEqual(self.view.radius, 16)

    def test_fullscreen_unmaps_both_trigger_and_resize_strips(self):
        self.view.trigger, self.view.edges = Mock(), [Mock() for _ in range(8)]
        self.view.get_allocation, self.view.allocation_changed, self.view.queue_draw = Mock(), Mock(), Mock()
        self.view.window_state(None, SimpleNamespace(new_window_state=4))
        self.view.trigger.hide.assert_called_once()
        self.view.trigger.show.assert_not_called()
        for edge in self.view.edges:
            edge.set_visible.assert_called_with(False)


if __name__ == "__main__":
    unittest.main()
