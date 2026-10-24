"""GTK container whose backdrop is rendered separately from sharp child widgets."""
from dataclasses import replace
import math
import queue
import threading
import time

import numpy as np

import cairo
import gi
gi.require_version("Gtk", "3.0")
gi.require_version("Gdk", "3.0")
gi.require_version("GdkX11", "3.0")
from gi.repository import Gdk, GLib, Gtk

from .backdrop import Backdrop
from .optics import Material, render


class GlassSurface(Gtk.EventBox):
    """Pack existing native content as this widget's child; no separate window.

    Parent window must choose its RGBA visual before realize. Its root and the
    ancestors behind this surface must not paint an opaque background. Paint the
    content body separately. The renderer is a bounded CPU prototype, capped at
    8 updates/second; it neither changes nor replaces the system compositor.
    """
    def __init__(self, owner, mode="sidebar", corner_radius=0, fps=4, helper=None):
        super().__init__()
        self.owner = owner
        self.set_visible_window(False)
        self.get_style_context().add_class("meadow-glass-surface")
        # Higher than shared window chrome, scoped to this widget only. Native
        # EventBox's default draw must not cover the captured or fallback pixels.
        self.surface_css = Gtk.CssProvider()
        self.surface_css.load_from_data(b".meadow-glass-surface { background: transparent; border: 0; box-shadow: none; }")
        self.get_style_context().add_provider(self.surface_css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 20)
        # Joined native panels have no inner bevel; HoverChrome owns the outer
        # window clip. The same planar materials are used by native Files.
        blur, tint = {"sidebar": (34, .76), "content": (28, .68)}.get(mode, (8, .10))
        self.material = Material(blur=blur, tint=tint, radius=0,
                                 bevel=0, refraction=0, dispersion=0, highlight=0)
        self.revision = 0
        self.current_request = self.frame_key = None
        self.allocation_key = self.configure_key = None
        self.margin = 40
        self.last_error = None
        self.last_render_ms = None
        self.frame_count = 0
        self.source_count = 0
        self.reader_restarts = 0
        self.surface = None
        self.storage = None
        self.alive = True
        self.requests = queue.Queue(maxsize=1)
        self.results = queue.Queue(maxsize=1)
        self.capture_paused = threading.Event()
        self.publish_lock = threading.Lock()
        self.set_corner_radius(corner_radius)
        self.settings = Gtk.Settings.get_default()
        self.theme_handlers = []
        # GTK_THEME can override Gtk.Settings; style-updated also runs after a
        # settings notification whose newly selected provider is not ready yet.
        self.connect("style-updated", self._theme_changed)
        self._theme_changed()
        if self.settings:
            self.theme_handlers = [self.settings.connect("notify::" + name, self._theme_changed)
                                   for name in ("gtk-theme-name", "gtk-application-prefer-dark-theme")]
        self.owner_state_handler = owner.connect("window-state-event", self._window_state)
        self.owner_configure_handler = owner.connect("configure-event", self._configured)
        self.connect("size-allocate", self._allocated)
        self.connect("unmap", self._unmapped)
        self.helper = helper
        self.display_name = owner.get_display().get_name()
        self.worker = threading.Thread(target=self._worker, name="meadow-native-glass", daemon=True)
        self.worker.start()
        self.timer = GLib.timeout_add(max(1000 // min(max(fps, 1), 12), 1), self._tick)
        self.connect("destroy", self._destroy)

    def _invalidate(self, preserve=False):
        self.revision += 1
        self.current_request = None
        self.next_capture_at = 0
        self.capture_interval = .25
        self.geometry_changed_at = time.monotonic() if preserve else 0
        if not preserve:
            self.frame_key = None
            self.surface = self.storage = None
            self.source_count = 0
        for pending in (self.requests, self.results):
            try:
                pending.get_nowait()
            except queue.Empty:
                pass
        self.queue_draw()

    def set_corner_radius(self, radius):
        if isinstance(radius, (tuple, list)):
            if len(radius) != 4:
                raise ValueError("Corner radii must be TL, TR, BR, BL")
            material = replace(self.material, radius=0,
                               radii=tuple(max(0, float(item)) for item in radius))
        else:
            material = replace(self.material, radius=max(0, float(radius)), radii=None)
        if material != self.material:
            self.material = material
            self._invalidate()

    def _theme_changed(self, *_args):
        if not self.alive:
            return
        found, color = self.get_style_context().lookup_color("meadow_bg")
        if found:
            dark = .2126 * color.red + .7152 * color.green + .0722 * color.blue < .5
        else:
            name = (self.settings.get_property("gtk-theme-name") or "") if self.settings else ""
            dark = "dark" in name.lower() or bool(self.settings and
                self.settings.get_property("gtk-application-prefer-dark-theme"))
        material = replace(self.material, tint_color=(48, 40, 32) if dark else (249, 249, 249))
        if material != self.material:
            self.material = material
            self._invalidate()

    def _allocated(self, _widget, allocation):
        key = (allocation.x, allocation.y, allocation.width, allocation.height,
               self.owner.get_scale_factor())
        if self.alive and key != self.allocation_key:
            self.allocation_key = key
            self._invalidate(preserve=True)

    def _configured(self, _owner, event):
        key = (event.x, event.y, event.width, event.height)
        if self.alive and key != self.configure_key:
            self.configure_key = key
            self._invalidate(preserve=True)
        return False

    def _unmapped(self, *_args):
        if self.alive:
            self._set_capture_paused(True)
            self._invalidate()

    def _request(self, window):
        if not self.get_mapped() or not window:
            return None
        allocation = self.get_allocation()
        coordinates = self.translate_coordinates(self.owner, 0, 0)
        if not coordinates or min(allocation.width, allocation.height) < 2:
            return None
        scale = max(1, self.owner.get_scale_factor())
        # Include the optical margin in the physical-pixel budget. HiDPI and
        # maximized oversized windows use a truthful, geometry-matched fallback.
        width = (allocation.width + self.margin * 2) * scale
        height = (allocation.height + self.margin * 2) * scale
        if max(width, height) > 4096 or width * height > 4_194_304:
            self.last_error = "Native glass area exceeds bounded capture limits"
            return None
        return (self.revision, window.get_xid(), coordinates[0], coordinates[1],
                allocation.width, allocation.height, self.material, scale)

    @staticmethod
    def _latest(pending, value):
        try:
            pending.get_nowait()
        except queue.Empty:
            pass
        try:
            pending.put_nowait(value)
        except queue.Full:
            pass

    def _tick(self):
        if not self.alive:
            return False
        window = self.owner.get_window()
        # GTK may keep mapped true after WM iconification. Hidden surfaces also
        # suspend capture and never publish a completed frame while hidden.
        self._set_capture_paused(not self.get_mapped() or bool(
            window and window.get_state() & Gdk.WindowState.ICONIFIED))
        if self.capture_paused.is_set():
            return True
        request = self._request(window)
        if request != self.current_request:
            # Keep a same-theme frame while the header changes allocation.
            # Its key remains the original geometry, never a false fresh receipt.
            self.current_request = request
            if request is None or (self.frame_key is not None and self.frame_key[6:8] != request[6:8]):
                self.surface = self.storage = None
                self.frame_key = None
                self.source_count = 0
            self.queue_draw()
        try:
            pixels, elapsed, source_count, error, result_key = self.results.get_nowait()
            if request is not None and result_key == request:
                self.last_error = error
                if error:
                    self.surface = self.storage = None
                    self.frame_key = None
                    self.source_count = 0
                    self.queue_draw()
                elif pixels is not None:
                    self.storage = bytearray(pixels.tobytes())
                    height, width = pixels.shape[:2]
                    self.surface = cairo.ImageSurface.create_for_data(
                        self.storage, cairo.FORMAT_ARGB32, width, height, width * 4)
                    self.surface.set_device_scale(request[-1], request[-1])
                    self.frame_key = request
                    self.last_render_ms, self.source_count = elapsed, source_count
                    self.frame_count += 1
                    self.queue_draw()
        except queue.Empty:
            pass
        now = time.monotonic()
        settled = not self.surface or now-getattr(self, "geometry_changed_at", 0) >= .12
        if request is not None and settled and now >= getattr(self, "next_capture_at", 0):
            self.next_capture_at = now+getattr(self, "capture_interval", .25)
            self._latest(self.requests, request)
        return True

    def _set_capture_paused(self, paused):
        if paused == self.capture_paused.is_set():
            return
        if paused:
            self.capture_paused.set()
            # Discard queued capture only; retain the last painted surface and
            # diagnostics. A capture already in flight may finish once.
            try:
                self.requests.get_nowait()
            except queue.Empty:
                pass
        else:
            self.capture_paused.clear()

    def _window_state(self, _owner, event):
        if self.alive:
            self._set_capture_paused(bool(event.new_window_state & Gdk.WindowState.ICONIFIED))
        return False

    def _worker(self):
        provider = None
        startup_failures = 0
        previous_request = previous_pixels = None
        try:
            while True:
                request = self.requests.get()
                if request is None:
                    break
                if not self.alive or self.capture_paused.is_set() or request != self.current_request:
                    continue
                _revision, xid, x, y, width, height, material, scale = request
                x, y, width, height = x * scale, y * scale, width * scale, height * scale
                margin = self.margin * scale
                material = replace(material, radius=material.radius * scale,
                                   radii=tuple(r * scale for r in material.radii) if material.radii else None,
                                   blur=material.blur * scale, bevel=material.bevel * scale,
                                   refraction=material.refraction * scale, dispersion=material.dispersion * scale)
                try:
                    start = time.monotonic()
                    if provider is None:
                        if startup_failures >= 3:
                            raise RuntimeError("Native backdrop reader could not start")
                        try:
                            provider = Backdrop(display=self.display_name, helper=self.helper)
                        except OSError:
                            startup_failures += 1
                            raise
                    if provider.failed:
                        provider.recover()
                    self.reader_restarts = provider.restart_count
                    image = provider.capture(xid, x - margin, y - margin,
                                             width + margin * 2, height + margin * 2)
                    if not self.alive or self.capture_paused.is_set() or request != self.current_request:
                        continue
                    if previous_request == request and previous_pixels is not None and np.array_equal(previous_pixels,image):
                        self.capture_interval = min(1.0, getattr(self, "capture_interval", .25)*2)
                        continue
                    self.capture_interval = .25
                    previous_request,previous_pixels=request,image
                    result = (render(image, width, height, margin, material),
                              (time.monotonic() - start) * 1000, provider.source_count, None, request)
                except (OSError, RuntimeError, ValueError) as error:
                    previous_request=previous_pixels=None
                    result = (None, None, 0, str(error), request)
                with self.publish_lock:
                    if self.alive and not self.capture_paused.is_set() and request == self.current_request:
                        self._latest(self.results, result)
        finally:
            if provider:
                provider.close()

    def _clip(self, context, width, height):
        radii = self.material.radii or (self.material.radius,) * 4
        radii = tuple(min(max(0, r), width / 2, height / 2) for r in radii)
        tl, tr, br, bl = radii
        context.new_path()
        context.move_to(tl, 0)
        context.line_to(width - tr, 0)
        if tr:
            context.arc(width - tr, tr, tr, -math.pi / 2, 0)
        context.line_to(width, height - br)
        if br:
            context.arc(width - br, height - br, br, 0, math.pi / 2)
        context.line_to(bl, height)
        if bl:
            context.arc(bl, height - bl, bl, math.pi / 2, math.pi)
        context.line_to(0, tl)
        if tl:
            context.arc(tl, tl, tl, math.pi, math.pi * 1.5)
        context.close_path()
        context.clip()

    def do_draw(self, context):
        # Drawing only consumes cached pixels; capture/render stay on the worker.
        allocation = self.get_allocation()
        context.save()
        self._clip(context, allocation.width, allocation.height)
        compatible = (self.frame_key is not None
                      and self.frame_key[6] == self.material
                      and self.frame_key[7] == self.owner.get_scale_factor())
        if self.surface and compatible:
            context.save()
            context.scale(allocation.width / self.frame_key[4],
                          allocation.height / self.frame_key[5])
            context.set_source_surface(self.surface, 0, 0)
            context.paint()
            context.restore()
        else:
            # Opaque fallback is deliberate and never reports capture success.
            blue, green, red = self.material.tint_color
            context.set_source_rgb(red / 255, green / 255, blue / 255)
            context.paint()
        result = Gtk.EventBox.do_draw(self, context)
        context.restore()
        return result

    def _destroy(self, *_args):
        # Only the tiny result publication is synchronized, never capture/render.
        with self.publish_lock:
            if not self.alive:
                return
            self.alive = False
        self.capture_paused.set()
        for name in ("owner_state_handler", "owner_configure_handler"):
            handler = getattr(self, name)
            if handler:
                self.owner.disconnect(handler)
                setattr(self, name, None)
        for handler in self.theme_handlers:
            self.settings.disconnect(handler)
        self.theme_handlers.clear()
        if self.timer:
            GLib.source_remove(self.timer)
            self.timer = None
        self.current_request = self.frame_key = None
        self.surface = self.storage = None
        self._latest(self.requests, None)
        try:
            self.results.get_nowait()
        except queue.Empty:
            pass
