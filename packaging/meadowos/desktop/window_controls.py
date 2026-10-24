"""One vector control asset set for native themes and Python system windows."""
from pathlib import Path
from functools import lru_cache

ASSETS = Path(__file__).with_name('assets') / 'window-controls'

@lru_cache(maxsize=6)
def control_icon(style, hover=False):
    import gi
    gi.require_version('Gio', '2.0')
    from gi.repository import Gio
    if style not in {'close', 'minimize', 'maximize'}:
        raise ValueError('Unknown window control')
    return Gio.FileIcon.new(Gio.File.new_for_path(
        str(ASSETS / f'{style}{"-hover" if hover else ""}.svg')))

def window_button(style, label, callback):
    from gi.repository import Gtk
    button=Gtk.Button();button.set_focus_on_click(False)
    button.set_tooltip_text(label);button.get_accessible().set_name(label)
    button.get_style_context().add_class('meadow-window-button')
    # Keep the SVG as a GIcon. GtkIconHelper resolves it at the widget's current
    # scale factor instead of stretching a cached 12px raster on Retina screens.
    image=Gtk.Image.new_from_gicon(control_icon(style),Gtk.IconSize.MENU)
    image.set_pixel_size(12)
    image.get_style_context().add_class('meadow-control-image')
    image.set_halign(Gtk.Align.CENTER);image.set_valign(Gtk.Align.CENTER)
    button.add(image)
    def changed(widget,*_):
        hover=bool(widget.get_state_flags() & (Gtk.StateFlags.PRELIGHT|Gtk.StateFlags.FOCUSED))
        image.set_from_gicon(control_icon(style,hover),Gtk.IconSize.MENU)
    button.connect('state-flags-changed',changed)
    button.connect('clicked',lambda _button:callback())
    return button
