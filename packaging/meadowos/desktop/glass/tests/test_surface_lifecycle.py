"""Exercise glass lifecycle and freshness without GTK or an X server."""
import ast
from dataclasses import replace
import math
from pathlib import Path
import queue
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, call

SOURCE = Path(__file__).resolve().parents[1] / "gtk_surface.py"
OPTICS = SOURCE.with_name("optics.py")


class SurfaceLifecycleTest(unittest.TestCase):
    def setUp(self):
        # Execute the actual widget methods against controlled GTK-shaped state.
        # No Gtk initialization, display, capture helper or subprocess is started.
        from dataclasses import dataclass
        material_node = next(node for node in ast.parse(OPTICS.read_text()).body
                             if isinstance(node, ast.ClassDef) and node.name == "Material")
        material_context = {"dataclass": dataclass}
        exec(compile(ast.Module(body=[material_node], type_ignores=[]), str(OPTICS), "exec"), material_context)
        self.Material = material_context["Material"]
        surface_node = next(node for node in ast.parse(SOURCE.read_text()).body if isinstance(node, ast.ClassDef))
        methods = [node for node in surface_node.body if isinstance(node, ast.FunctionDef) and node.name != "__init__"]
        for node in methods:
            node.decorator_list = []
        self.provider = Mock(failed=False, restart_count=0, source_count=2)
        self.image_surface = Mock()
        self.pixels = SimpleNamespace(shape=(570, 190, 4), tobytes=lambda: b"pixels")
        self.context = {"queue": queue, "replace": replace, "time": time, "math": math,
            "Gdk": SimpleNamespace(WindowState=SimpleNamespace(ICONIFIED=1)),
            "GLib": SimpleNamespace(source_remove=Mock()),
            "Gtk": SimpleNamespace(EventBox=SimpleNamespace(do_draw=Mock(return_value=False))),
            "cairo": SimpleNamespace(FORMAT_ARGB32=0, ImageSurface=SimpleNamespace(
                create_for_data=Mock(return_value=self.image_surface))),
            "Backdrop": Mock(return_value=self.provider), "render": Mock(return_value=self.pixels)}
        exec(compile(ast.Module(body=methods, type_ignores=[]), str(SOURCE), "exec"), self.context)
        self.native_state, self.mapped, self.scale = 0, True, 1
        self.allocation = SimpleNamespace(x=0, y=0, width=190, height=570)
        self.native_window = SimpleNamespace(get_state=lambda: self.native_state, get_xid=lambda: 42)
        self.preferences = {"gtk-theme-name": "Meadow-Light", "gtk-application-prefer-dark-theme": False}
        self.resolved_color = None
        self.style = SimpleNamespace(lookup_color=lambda _name: (self.resolved_color is not None, self.resolved_color))
        settings = Mock()
        settings.get_property.side_effect = self.preferences.get
        self.surface = SimpleNamespace(alive=True, capture_paused=threading.Event(), publish_lock=threading.Lock(),
            owner=SimpleNamespace(get_window=lambda: self.native_window, get_scale_factor=lambda: self.scale,
                                  disconnect=Mock()),
            owner_state_handler=7, owner_configure_handler=9, timer=8, settings=settings, theme_handlers=[11, 12],
            display_name=":isolated-test", helper=None, margin=40, revision=0,
            current_request=None, frame_key=None, allocation_key=None, configure_key=None,
            requests=queue.Queue(maxsize=1), results=queue.Queue(maxsize=1),
            get_mapped=lambda: self.mapped, get_allocation=lambda: self.allocation,
            get_style_context=lambda: self.style,
            translate_coordinates=lambda *_: (0, 0),
            material=self.Material(blur=18, tint=.42, radius=0, bevel=0, refraction=0, dispersion=0, highlight=0),
            frame_count=12, source_count=2, last_error=None, last_render_ms=63,
            surface=object(), storage=object(), queue_draw=Mock())
        for node in methods:
            name = node.name
            if name == "_latest":
                setattr(self.surface, name, self.context[name])
            else:
                setattr(self.surface, name, lambda *args, key=name, **kwargs: self.context[key](self.surface, *args, **kwargs))
        request = self.surface._request(self.native_window)
        self.surface.current_request = self.surface.frame_key = request

    def test_iconified_but_gtk_mapped_does_not_capture_or_repaint(self):
        self.native_state = 1
        before = (self.surface.material, self.surface.surface, self.surface.storage)
        self.surface.requests.put_nowait("queued-before-iconify")
        self.surface.results.put_nowait((None, None, 0, "pending-result", self.surface.current_request))
        for _ in range(20):
            self.assertTrue(self.surface._tick())
        self.assertTrue(self.surface.capture_paused.is_set())
        self.assertTrue(self.surface.requests.empty())
        self.assertEqual(self.surface.results.qsize(), 1)
        self.surface.queue_draw.assert_not_called()
        self.assertEqual((self.surface.frame_count, self.surface.source_count), (12, 2))
        self.assertEqual((self.surface.material, self.surface.surface, self.surface.storage), before)

    def test_native_restore_resumes_capture_with_original_geometry_and_material(self):
        self.native_state = 1
        self.surface._tick()
        before = (self.surface.material, self.surface.surface, self.surface.storage)
        request = self.surface.current_request
        self.native_state = 0
        self.surface._window_state(None, SimpleNamespace(new_window_state=0))
        self.assertTrue(self.surface._tick())
        self.assertFalse(self.surface.capture_paused.is_set())
        self.assertEqual(self.surface.requests.get_nowait(), request)
        self.assertEqual((self.surface.material, self.surface.surface, self.surface.storage), before)
        self.assertEqual(self.surface.frame_count, 12)

    def test_native_iconify_event_drops_queue_before_next_timer(self):
        self.surface.requests.put_nowait("queued")
        self.surface._window_state(None, SimpleNamespace(new_window_state=1))
        self.assertTrue(self.surface.requests.empty())
        self.assertTrue(self.surface.capture_paused.is_set())

    def test_worker_rechecks_pause_before_creating_capture_helper(self):
        self.surface.capture_paused.set()
        self.surface.requests = queue.Queue()
        self.surface.requests.put_nowait(self.surface.current_request)
        self.surface.requests.put_nowait(None)
        self.surface._worker()
        self.context["Backdrop"].assert_not_called()
        self.context["render"].assert_not_called()

    def test_destroy_disconnects_all_external_signals_and_is_idempotent(self):
        self.surface.requests.put_nowait("queued")
        self.surface._destroy()
        self.surface._destroy()
        self.assertFalse(self.surface.alive)
        self.surface.owner.disconnect.assert_has_calls([call(7), call(9)])
        self.surface.settings.disconnect.assert_has_calls([call(11), call(12)])
        self.context["GLib"].source_remove.assert_called_once_with(8)
        self.assertIsNone(self.surface.requests.get_nowait())
        self.assertIsNone(self.surface.surface)
        self.surface._window_state(None, SimpleNamespace(new_window_state=0))
        self.assertTrue(self.surface.capture_paused.is_set())
        self.assertFalse(self.surface._tick())

    def test_matching_result_publishes_sharp_surface(self):
        request = self.surface.current_request
        self.surface.results.put_nowait((self.pixels, 2, 3, None, request))
        self.surface._tick()
        self.assertIs(self.surface.surface, self.image_surface)
        self.assertEqual(self.surface.frame_key, request)
        self.assertEqual((self.surface.frame_count, self.surface.source_count), (13, 3))
        self.image_surface.set_device_scale.assert_called_once_with(1, 1)

    def test_stale_error_cannot_erase_current_valid_surface(self):
        before = self.surface.surface
        old_key = (-1,) + self.surface.current_request[1:]
        self.surface.results.put_nowait((None, None, 0, "old capture failed", old_key))
        self.surface._tick()
        self.assertIs(self.surface.surface, before)
        self.assertIsNone(self.surface.last_error)

    def test_resize_retains_material_until_exact_frame_without_accepting_stale_result(self):
        old_key = self.surface.current_request
        old_surface = self.surface.surface
        self.allocation.width = 300
        self.surface._allocated(None, self.allocation)
        self.assertIs(self.surface.surface, old_surface)
        self.surface.results.put_nowait((self.pixels, 2, 3, None, old_key))
        self.surface._tick()
        self.assertIs(self.surface.surface, old_surface)
        self.assertEqual(self.surface.current_request[4:6], (300, 570))
        self.assertEqual(self.surface.frame_count, 12)

    def test_move_and_theme_change_invalidate_even_without_resize(self):
        old = self.surface.current_request
        self.surface._configured(None, SimpleNamespace(x=80, y=90, width=760, height=570))
        self.surface._tick()
        self.assertNotEqual(self.surface.current_request, old)
        old = self.surface.current_request
        self.preferences["gtk-theme-name"] = "Meadow-Dark"
        self.surface._theme_changed()
        self.assertIsNone(self.surface.surface)
        self.assertEqual(self.surface.material.tint_color, (48, 40, 32))
        self.surface._tick()
        self.assertNotEqual(self.surface.current_request, old)
        self.preferences["gtk-theme-name"] = "Meadow-Light"
        self.surface._theme_changed()
        self.assertEqual(self.surface.material.tint_color, (249, 249, 249))

    def test_resolved_dark_style_overrides_stale_light_settings(self):
        # GTK_THEME=Meadow-Dark can leave Gtk.Settings reporting Meadow-Light.
        self.resolved_color = SimpleNamespace(red=32 / 255, green=40 / 255, blue=48 / 255)
        old_key = self.surface.current_request
        self.surface._theme_changed()
        self.assertEqual(self.preferences["gtk-theme-name"], "Meadow-Light")
        self.assertFalse(self.preferences["gtk-application-prefer-dark-theme"])
        self.assertEqual(self.surface.material.tint_color, (48, 40, 32))
        self.assertIsNone(self.surface.surface)
        self.surface._tick()
        self.assertNotEqual(self.surface.current_request, old_key)

    def test_style_update_after_settings_notify_uses_newly_resolved_provider(self):
        self.preferences["gtk-theme-name"] = "Meadow-Dark"
        self.resolved_color = SimpleNamespace(red=.96, green=.97, blue=.98)
        self.surface._theme_changed()  # settings notify, before CSS resolution
        self.assertEqual(self.surface.material.tint_color, (249, 249, 249))
        self.resolved_color = SimpleNamespace(red=32 / 255, green=40 / 255, blue=48 / 255)
        self.surface._theme_changed(self.surface)  # later style-updated
        self.assertEqual(self.surface.material.tint_color, (48, 40, 32))
        self.resolved_color = SimpleNamespace(red=.96, green=.97, blue=.98)
        self.surface._theme_changed(self.surface)
        self.assertEqual(self.surface.material.tint_color, (249, 249, 249))

    def test_scale_change_discards_old_pixels_and_exceeds_budget_safely(self):
        self.scale = 2
        self.surface._tick()
        self.assertIsNone(self.surface.surface)
        self.assertEqual(self.surface.current_request[-1], 2)
        self.allocation.width, self.allocation.height = 1600, 1200
        self.surface._tick()
        self.assertIsNone(self.surface.current_request)
        self.assertIn("bounded capture", self.surface.last_error)

    def test_latest_queue_replaces_stale_capture(self):
        self.surface.requests.put_nowait("stale")
        self.surface._tick()
        self.assertEqual(self.surface.requests.qsize(), 1)
        self.assertEqual(self.surface.requests.get_nowait(), self.surface.current_request)

    def test_unmap_suspends_and_remap_queues_fresh_capture(self):
        self.mapped = False
        self.surface._unmapped()
        self.surface._tick()
        self.assertTrue(self.surface.requests.empty())
        self.assertIsNone(self.surface.surface)
        self.assertTrue(self.surface.capture_paused.is_set())
        self.mapped = True
        self.surface._tick()
        self.assertFalse(self.surface.capture_paused.is_set())
        self.assertEqual(self.surface.requests.qsize(), 1)

    def test_inflight_capture_after_destroy_neither_renders_nor_publishes(self):
        self.surface.requests = queue.Queue()
        self.surface.requests.put_nowait(self.surface.current_request)
        self.provider.capture.side_effect = lambda *_: self.surface._destroy()
        self.surface._worker()
        self.context["render"].assert_not_called()
        self.assertTrue(self.surface.results.empty())
        self.provider.close.assert_called_once()

    def test_missing_helper_startup_retries_are_bounded(self):
        self.context["Backdrop"].side_effect = OSError("missing helper")
        self.surface.requests = queue.Queue()
        for _ in range(20):
            self.surface.requests.put_nowait(self.surface.current_request)
        self.surface.requests.put_nowait(None)
        self.surface._worker()
        self.assertEqual(self.context["Backdrop"].call_count, 3)
        self.context["render"].assert_not_called()
        self.assertEqual(self.surface.results.qsize(), 1)

    def test_draw_uses_matching_geometry_and_dark_fallback_without_capture(self):
        self.surface.material = replace(self.surface.material, tint_color=(48, 40, 32), radii=(16, 0, 0, 16))
        context = Mock()
        self.surface.do_draw(context)
        context.set_source_surface.assert_not_called()
        context.set_source_rgb.assert_called_once_with(32 / 255, 40 / 255, 48 / 255)
        self.assertEqual(context.arc.call_count, 2)
        context.clip.assert_called_once()
        context.save.assert_called_once()
        context.restore.assert_called_once()
        self.context["Backdrop"].assert_not_called()
        self.context["render"].assert_not_called()


if __name__ == "__main__":
    unittest.main()
