#!/usr/bin/python3
"""Construct owned native windows and verify asynchronous real backdrop arrival.

This does not exercise UI inputs or assert optical visual acceptance. Run only
on an isolated X display with a WM/compositor, never the user's live desktop.
"""
import json
import subprocess
import time
import gi

gi.require_version("Gtk", "3.0")
gi.require_version("GdkX11", "3.0")
from gi.repository import Gtk, Gdk, GdkX11, GLib

background = Gtk.Window(title="Meadow glass probe lower source")
background.set_decorated(False)
background.set_default_size(1000, 700)
background.move(40, 40)
area = Gtk.DrawingArea()

def paint(widget, context):
    width = widget.get_allocated_width()
    height = widget.get_allocated_height()
    for x in range(0, width, 32):
        context.set_source_rgb(*((.95, .75, .17) if (x//32)%2 else (.12, .24, .65)))
        context.rectangle(x, 0, 32, height)
        context.fill()
    return False

area.connect("draw", paint)
background.add(area)
background.show_all()
window = Gtk.Window(title="Meadow GTK adapter real backdrop probe")
window.set_default_size(640, 440)
window.move(160, 130)
header = Gtk.HeaderBar(title="Native original controls")
header.set_show_close_button(True)
header.set_decoration_layout("close,minimize,maximize:")
window.set_titlebar(header)
body = Gtk.Box()
sidebar = Gtk.PlacesSidebar()
sidebar.set_size_request(190, -1)
body.pack_start(sidebar, False, True, 0)
entry = Gtk.Entry()
entry.set_text("Native foreground draft remains intact")
body.pack_start(entry, True, True, 0)
window.add(body)
window.show_all()
started = time.monotonic()
result = {}

def inspect():
    ready = sidebar.get_style_context().has_class("meadow-native-glass-sidebar")
    if ready or time.monotonic()-started > 10:
        xid = window.get_window().get_xid()
        prop = subprocess.run(["xprop", "-id", hex(xid), "_MEADOW_GLASS_CAPTURE_STATUS", "_MEADOW_GLASS_REGIONS"],
                              capture_output=True, text=True, timeout=2)
        result.update({"ready": ready, "elapsed": time.monotonic()-started,
                       "properties": prop.stdout.strip(), "draft": entry.get_text(),
                       "same_native_content": body.get_parent() is window.get_child().get_child(),
                       "visual_acceptance": "not_run", "display": Gdk.Display.get_default().get_name()})
        print(json.dumps(result, ensure_ascii=False))
        window.destroy()
        background.destroy()
        Gtk.main_quit()
        return False
    return True

GLib.timeout_add(100, inspect)
Gtk.main()
raise SystemExit(0 if result.get("ready") and result.get("same_native_content") else 1)
