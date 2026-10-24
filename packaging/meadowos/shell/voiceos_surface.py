"""Native VoiceOS presentation, backed by one persistent local WebGL view.

This window owns no model session, microphone, or application process. Actions
return to the Shell, which keeps the same Agent and permission flow as Chat.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile
from urllib.parse import unquote, urlparse

PALETTES = frozenset({"meadow", "cobalt", "jade", "iron", "bronze", "frost"})
PHASES = frozenset({"idle", "listening", "thinking", "speaking", "error"})


class VoiceOSSurface:
    COMPACT_SIZE = 74

    def __init__(self, asset_dir, state_path, on_action, on_mode_change, geometry):
        import gi
        gi.require_version("Gtk", "3.0")
        gi.require_version("Gdk", "3.0")
        gi.require_version("GdkX11", "3.0")
        gi.require_version("WebKit2", "4.1")
        from gi.repository import Gdk, GdkX11, Gio, GLib, Gtk, WebKit2

        self.Gdk, self.GLib, self.Gtk, self.WebKit2 = Gdk, GLib, Gtk, WebKit2
        self.GdkX11 = GdkX11
        self.focus_timestamp = 0
        self.escape_generation = 0
        self.escape_timer = None
        self.companions = {}
        self.asset_dir = Path(asset_dir).resolve()
        self.entry = self.asset_dir / "index.html"
        if not self.entry.is_file():
            raise RuntimeError("VoiceOS 的介面資產尚未安裝。")
        self.state_path = Path(state_path)
        self.on_action, self.on_mode_change = on_action, on_mode_change
        self.geometry = geometry
        self.expanded = self.enabled = self.ready = self.closed = self.terminated = False
        self.state = {"phase": "idle", "energy": 0, "palette": "meadow", "compact": False, "muted": True}
        try:
            saved = json.loads(self.state_path.read_text())
            if saved.get("palette") in PALETTES:
                self.state["palette"] = saved["palette"]
        except (OSError, ValueError, AttributeError):
            pass

        self.window = Gtk.Window(title="Meadow VoiceOS")
        self.window.set_wmclass("meadow-voiceos", "MeadowVoiceOS")
        self.window.set_decorated(False)
        self.window.set_skip_taskbar_hint(True)
        self.window.set_skip_pager_hint(True)
        self.window.set_keep_above(True)
        self.window.set_app_paintable(True)
        self.window.set_focus_on_map(False)
        self.window.stick()
        visual = self.window.get_screen().get_rgba_visual()
        if visual:
            self.window.set_visual(visual)
        self.window.connect("delete-event", lambda *_: self._dismiss())
        self.window.connect("key-press-event", self._key)
        self.window.connect("window-state-event", self._window_state)
        self.window.connect("map-event", self._mapped)
        self.window.connect("draw", self._clear)
        css = Gtk.CssProvider()
        css.load_from_data(b"""
          window.voiceos-native { background: transparent; border: 0; box-shadow: none; padding: 0; }
          .voiceos-native button.voiceos-exit {
            color: #f1eee8; background: rgba(26,25,23,.72); background-image: none;
            border: 1px solid rgba(241,238,232,.22); border-radius: 20px;
            box-shadow: none; text-shadow: none; min-width: 30px; min-height: 30px;
            padding: 5px 10px; font-size: 12px;
          }
          .voiceos-native button.voiceos-exit:hover { background: rgba(60,57,52,.92); }
          .voiceos-native button.voiceos-exit:focus { border-color: rgba(241,238,232,.8); }
          .voiceos-native button.voiceos-exit.compact {
            min-width: 18px; min-height: 18px; padding: 2px; border-radius: 12px;
          }
        """)
        style = self.window.get_style_context()
        style.add_class("voiceos-native")
        style.add_provider(css, Gtk.STYLE_PROVIDER_PRIORITY_USER + 10)
        Gtk.StyleContext.add_provider_for_screen(self.window.get_screen(), css,
                                                 Gtk.STYLE_PROVIDER_PRIORITY_USER + 10)
        self.exit_css = css

        manager = WebKit2.UserContentManager()
        manager.register_script_message_handler("meadowVoiceOS")
        manager.connect("script-message-received::meadowVoiceOS", self._message)
        context = WebKit2.WebContext.new_ephemeral()
        self.view = WebKit2.WebView(web_context=context, user_content_manager=manager)
        self.view.set_background_color(Gdk.RGBA(0, 0, 0, 0))
        settings = self.view.get_settings()
        settings.set_enable_webgl(True)
        settings.set_enable_developer_extras(False)
        settings.set_allow_file_access_from_file_urls(True)
        settings.set_allow_universal_access_from_file_urls(False)
        settings.set_enable_media_stream(False)
        self.view.connect("decide-policy", self._navigation)
        self.view.connect("permission-request", self._permission)
        self.view.connect("web-process-terminated", self._terminated)
        self.overlay = Gtk.Overlay()
        self.overlay.add(self.view)
        self.exit_button = Gtk.Button()
        self.exit_button.get_style_context().add_class("voiceos-exit")
        self.exit_button.set_halign(Gtk.Align.START)
        self.exit_button.set_valign(Gtk.Align.START)
        self.exit_button.set_tooltip_text("離開 VoiceOS · Esc")
        self.exit_button.get_accessible().set_name("離開 VoiceOS")
        self.exit_button.connect("clicked", lambda *_: self.dismiss())
        exit_content = Gtk.Box(spacing=6)
        icon_file = Path(__file__).with_name("assets") / "voiceos-exit-symbolic.svg"
        icon = Gtk.Image.new_from_gicon(Gio.FileIcon.new(Gio.File.new_for_path(str(icon_file))), Gtk.IconSize.MENU)
        icon.set_pixel_size(16)
        exit_content.pack_start(icon, False, False, 0)
        self.exit_label = Gtk.Label(label="離開")
        self.exit_label.set_no_show_all(True)
        exit_content.pack_start(self.exit_label, False, False, 0)
        self.exit_button.add(exit_content)
        self.overlay.add_overlay(self.exit_button)
        self.overlay.set_overlay_pass_through(self.exit_button, False)
        self.window.add(self.overlay)
        self.view.load_uri(self.entry.as_uri())

    @property
    def palette(self):
        return self.state["palette"]

    def _clear(self, _window, cr):
        import cairo
        cr.set_operator(cairo.OPERATOR_SOURCE)
        cr.set_source_rgba(0, 0, 0, 1 if self.expanded else 0)
        cr.paint()
        cr.set_operator(cairo.OPERATOR_OVER)
        return False

    def _permission(self, _view, request):
        # Capture is explicitly owned by the host bridge, never a second
        # microphone hidden inside the container's WebKit process.
        request.deny()
        return True

    def _navigation(self, _view, decision, kind):
        if kind not in (self.WebKit2.PolicyDecisionType.NAVIGATION_ACTION,
                        self.WebKit2.PolicyDecisionType.NEW_WINDOW_ACTION):
            return False
        uri = decision.get_request().get_uri()
        parsed = urlparse(uri)
        if kind == self.WebKit2.PolicyDecisionType.NAVIGATION_ACTION and parsed.scheme == "file" and Path(unquote(parsed.path)).resolve() == self.entry:
            return False
        decision.ignore()
        return True

    def _terminated(self, _view, _reason):
        self.ready = False
        self.terminated = True
        self.collapse()
        self.on_action("error", {"message": "VoiceOS 畫面已停止，請重新開啟語音介面。"})

    def _message(self, _manager, result):
        if self.closed:
            return
        try:
            value = result.get_js_value()
            payload = json.loads(value.to_string() if value.is_string() else value.to_json(0))
        except (ValueError, TypeError, AttributeError):
            return
        if not isinstance(payload, dict):
            return
        kind = payload.get("type")
        if kind == "ready":
            self.ready = True
            self._render()
        elif kind in {"collapse", "exit"}:
            # Escape in the current renderer sends collapse. Only purposeful
            # work handoff calls collapse(); a user's exit ends the overlay.
            self.dismiss()
        elif kind == "expand":
            self.expand()
        elif kind == "palette" and payload.get("palette") in PALETTES:
            self.set_state(palette=payload["palette"])
            self._save_palette()
            self.on_action(kind, {"palette": self.palette})
        elif kind in {"toggle-microphone", "work-files", "work-tasks", "show-chat"}:
            self.on_action(kind, {})

    def _save_palette(self):
        temporary = None
        try:
            self.state_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            fd, temporary = tempfile.mkstemp(prefix=".voiceos-", dir=self.state_path.parent)
            with os.fdopen(fd, "w") as stream:
                json.dump({"version": 1, "palette": self.palette}, stream)
            os.replace(temporary, self.state_path)
        except OSError as exc:
            self.on_action("error", {"message": f"VoiceOS 色彩尚未保存：{exc}"})
        finally:
            if temporary and os.path.exists(temporary):
                os.unlink(temporary)

    def _render(self):
        if self.ready and not self.closed:
            script = "window.MeadowVoiceOS && window.MeadowVoiceOS.setState(" + json.dumps(self.state) + ");"
            self.view.run_javascript(script, None, None, None)

    def set_state(self, **values):
        if values.get("phase") in PHASES:
            self.state["phase"] = values["phase"]
        if values.get("palette") in PALETTES:
            self.state["palette"] = values["palette"]
        if isinstance(values.get("energy"), (int, float)):
            self.state["energy"] = max(0, min(1, values["energy"]))
        if "muted" in values:
            self.state["muted"] = bool(values["muted"])
        self.state["compact"] = not self.expanded
        self._render()

    def expand(self):
        if self.closed:
            return
        self._cancel_escape()
        self.release_companions()
        if self.terminated:
            self.terminated = False
            self.view.load_uri(self.entry.as_uri())
        self.enabled = self.expanded = True
        self.window.set_accept_focus(True)
        self.window.set_focus_on_map(True)
        self.window.realize()
        # Record the user activation. Focus is committed with a fresh server
        # timestamp after mapping: hiding an active work surface can make the
        # WM focus Files between this input event and its fullscreen reply.
        self.focus_timestamp = (self.Gtk.get_current_event_time() or
            self.GdkX11.x11_get_server_time(self.window.get_window()))
        self.on_mode_change(True)
        self._render_mode()
        self._focus_expanded()

    def collapse(self):
        if self.closed or not self.enabled:
            return
        self._cancel_escape()
        self.release_companions()
        self.expanded = False
        self.window.set_accept_focus(False)
        self.window.set_focus_on_map(False)
        self.window.unfullscreen()
        self._render_mode()
        self.on_mode_change(False)

    def dismiss(self):
        """Explicit exit: hide the whole overlay, including its compact orb."""
        if self.closed or not self.enabled:
            return
        self._cancel_escape()
        self.release_companions()
        self.enabled = self.expanded = False
        self.state.update(muted=True, energy=0, compact=True)
        self.window.set_accept_focus(False)
        self.window.set_focus_on_map(False)
        self.window.hide()
        self.window.unfullscreen()
        self._render()
        self.on_mode_change(False)
        self.on_action("exit", {})

    def show_companion(self, window, pin_preference=None, overlay_above=None):
        """Place an owned chat above the overlay without changing focus.

        Shell owns the chat's placement/presentation. This relationship uses
        only the two explicit windows, never global active-window heuristics.
        pin_preference reads the chrome's current user choice at release, so
        changing that choice while temporarily lifted is not discarded.
        overlay_above composes that choice with the temporary lift while the
        overlay is visible, including when the user turns the pin off.
        """
        if self.closed or not self.enabled or not self.expanded:
            return False
        if window not in self.companions:
            native = window.get_window()
            above = bool(native and native.get_state() & self.Gdk.WindowState.ABOVE)
            handler = window.connect("destroy", lambda destroyed: self.companions.pop(destroyed, None))
            self.companions[window] = (window.get_transient_for(), above, handler,
                                       pin_preference, overlay_above)
        window.set_transient_for(self.window)
        window.set_destroy_with_parent(False)
        if overlay_above is not None:
            overlay_above(True)
        else:
            window.set_keep_above(True)
        return True

    def release_companion(self, window):
        previous = self.companions.pop(window, None)
        if previous is not None:
            window.disconnect(previous[2])
            window.set_transient_for(previous[0])
            if previous[4] is not None:
                previous[4](False)
                return
            above = previous[1]
            if previous[3] is not None:
                try:
                    above = bool(previous[3]())
                except (RuntimeError, AttributeError):
                    # A chrome destroyed before its owner has no live choice.
                    pass
            window.set_keep_above(above)

    def release_companions(self):
        for window in list(self.companions):
            self.release_companion(window)

    def _sync_exit_controls(self):
        compact = not self.expanded
        context = self.exit_button.get_style_context()
        (context.add_class if compact else context.remove_class)("compact")
        self.exit_button.set_margin_start(2 if compact else 20)
        self.exit_button.set_margin_top(2 if compact else 20)
        self.exit_label.set_visible(not compact)

    def _render_mode(self):
        self.state["compact"] = not self.expanded
        rect = self.geometry()
        if self.expanded:
            self.window.set_size_request(1, 1)
            self.window.move(rect.x, rect.y)
            self.window.resize(rect.width, rect.height)
            self.window.show_all()
            self.window.fullscreen()
        else:
            self._place_compact()
            self.window.show_all()
        self.window.queue_draw()
        self._sync_exit_controls()
        self._render()

    def _place_compact(self):
        if self.closed or not self.enabled or self.expanded:
            return False
        rect = self.geometry()
        size = self.COMPACT_SIZE
        self.window.set_size_request(size, size)
        self.window.resize(size, size)
        self.window.move(rect.x + rect.width - size - 14, rect.y + 30)
        return False

    def _window_state(self, _window, event):
        if self.enabled and self.expanded and event.changed_mask & self.Gdk.WindowState.FULLSCREEN and event.new_window_state & self.Gdk.WindowState.FULLSCREEN:
            self._focus_expanded()
        if self.enabled and not self.expanded and not event.new_window_state & self.Gdk.WindowState.FULLSCREEN:
            # Xfwm processes fullscreen removal asynchronously; the size sent
            # before its acknowledgement can otherwise remain screen-sized.
            self.GLib.idle_add(self._place_compact)
        return False

    def _mapped(self, *_args):
        self._focus_expanded()
        return False

    def focus_overlay(self):
        """Return from an explicitly hidden companion; safe for GLib.idle_add."""
        return self._focus_expanded()

    def _focus_expanded(self):
        if self.closed or not self.enabled or not self.expanded or not self.window.get_mapped():
            return False
        if any(window.get_visible() for window in self.companions):
            return False
        native = self.window.get_window()
        if native is not None:
            # Only fullscreen activation or an explicit return from owned chat.
            # Never run it from layout/paint or while the compact orb is shown.
            # XSetInputFocus ignores a timestamp older than its latest focus
            # change. The WM's asynchronous fallback after hiding a work window
            # can therefore invalidate the original click timestamp.
            committed_at = self.GdkX11.x11_get_server_time(native)
            self.window.present_with_time(committed_at)
            native.focus(committed_at)
            self.view.grab_focus()
        return False

    def _key(self, _window, event):
        if event.keyval != self.Gdk.KEY_Escape:
            return False
        if not self.ready:
            self.dismiss()
            return True
        self._cancel_escape()
        generation = self.escape_generation
        # Consume the native key before WebKit's document handler. The first
        # Escape dismisses an open palette; the next exits. A hung Web process
        # cannot prevent leaving the native window.
        script = """(() => {
          const surface = window.MeadowVoiceOS;
          if (surface && surface.getState().paletteOpen) {
            surface.hidePalette(); return 'palette';
          }
          return 'exit';
        })();"""
        self.escape_timer = self.GLib.timeout_add(250, self._escape_timeout, generation)
        try:
            self.view.run_javascript(script, None, self._escape_result, generation)
        except Exception:
            self._cancel_escape()
            self.dismiss()
        return True

    def _cancel_escape(self):
        self.escape_generation += 1
        if self.escape_timer is not None:
            self.GLib.source_remove(self.escape_timer)
            self.escape_timer = None

    def _escape_timeout(self, generation):
        if generation == self.escape_generation:
            self.escape_timer = None
            self.dismiss()
        return False

    def _escape_result(self, view, result, generation):
        if generation != self.escape_generation or self.closed or not self.enabled:
            return
        try:
            decision = view.run_javascript_finish(result).get_js_value().to_string()
        except Exception:
            decision = "exit"
        self._cancel_escape()
        if decision != "palette":
            self.dismiss()

    def _dismiss(self):
        self.dismiss()
        return True

    def reposition(self):
        if self.enabled:
            self._render_mode()

    def close(self):
        if self.closed:
            return
        self._cancel_escape()
        self.release_companions()
        self.closed = True
        self.enabled = self.expanded = False
        self.view.stop_loading()
        self.window.destroy()
