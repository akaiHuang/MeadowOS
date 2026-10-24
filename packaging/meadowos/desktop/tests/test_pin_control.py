"""Native pin SVG and toggle contract; owned minimal103 fixture only."""
import os,sys,time,json,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT))

@unittest.skipUnless(os.environ.get('MEADOW_OWNED_CONTROL_FIXTURE')=='1' and os.environ.get('DISPLAY')==':103','requires owned103')
class NativePinControl(unittest.TestCase):
 def test_svg_density_symbolic_colour_geometry_and_toggle(self):
  import gi
  gi.require_version('Gtk','3.0')
  from gi.repository import Gtk,Gdk,GLib
  from meadow_window_chrome import create_hover_chrome
  class Owner(Gtk.Window):
   def __init__(self):super().__init__();self.keep_calls=[]
   def set_keep_above(self,value):
    self.keep_calls.append(bool(value));Gtk.Window.set_keep_above(self,value)
  def settle():
   stop=time.monotonic()+.18
   while time.monotonic()<stop:
    while GLib.MainContext.default().pending():GLib.MainContext.default().iteration(False)
    time.sleep(.004)
  owner=Owner();owner.set_default_size(360,180)
  content=Gtk.Box();content.add(Gtk.Label(label='Owned pin fixture'))
  chrome=create_hover_chrome(owner,content,title='Owned pin',allow_pin=True)
  owner.add(chrome);owner.show_all();chrome.reveal(keyboard=True);settle()
  def walk(widget):
   yield widget
   if isinstance(widget,Gtk.Container):
    for child in widget.get_children():yield from walk(child)
  pins=[w for w in walk(chrome) if w.get_style_context().has_class('meadow-window-pin')]
  self.assertEqual(len(pins),1);pin=pins[0];self.assertIsInstance(pin,Gtk.ToggleButton)
  image=pin.get_child();gicon,_=image.get_gicon()
  self.assertEqual(gicon.get_file().get_path(),str(ROOT/'assets/window-controls/pin-symbolic.svg'))
  self.assertEqual(image.get_storage_type(),Gtk.ImageType.GICON)
  self.assertEqual(image.get_pixel_size(),16)
  settings=Gtk.Settings.get_default();original=settings.get_property('gtk-theme-name');metrics=[]
  try:
   for theme in ['Meadow-Light','Meadow-Dark']:
    settings.set_property('gtk-theme-name',theme);settle()
    for active in [False,True,False]:
     pin.set_active(active);settle()
     scale=image.get_scale_factor();self.assertEqual(scale,int(os.environ['GDK_SCALE']))
     self.assertEqual((image.get_allocated_width(),image.get_allocated_height()),(16,16))
     self.assertEqual(pin.get_allocated_width(),26)
     self.assertGreaterEqual(pin.get_allocated_height(),26)
     x,y=image.translate_coordinates(pin,0,0)
     self.assertAlmostEqual(x+8,pin.get_allocated_width()/2,delta=.5)
     self.assertAlmostEqual(y+8,pin.get_allocated_height()/2,delta=.5)
     info=Gtk.IconTheme.get_default().lookup_by_gicon_for_scale(gicon,16,scale,Gtk.IconLookupFlags.FORCE_SIZE)
     self.assertTrue(info.is_symbolic())
     pixels,was_symbolic=info.load_symbolic_for_context(image.get_style_context())
     self.assertTrue(was_symbolic);self.assertEqual((pixels.get_width(),pixels.get_height()),(16*scale,16*scale))
     state=image.get_state_flags();fg=image.get_style_context().get_color(state)
     raw=pixels.get_pixels();stride=pixels.get_rowstride();channels=pixels.get_n_channels()
     samples=[raw[row*stride+col*channels:row*stride+col*channels+channels] for row in range(pixels.get_height()) for col in range(pixels.get_width())]
     opaque=[p for p in samples if channels==3 or p[3]>=230];self.assertTrue(opaque)
     rgb=[sum(p[i] for p in opaque)/len(opaque)/255 for i in range(3)]
     for a,b in zip(rgb,[fg.red,fg.green,fg.blue]):self.assertAlmostEqual(a,b,delta=.03)
     luminance=.2126*rgb[0]+.7152*rgb[1]+.0722*rgb[2]
     if not active:
      if theme=='Meadow-Dark':self.assertGreater(luminance,.6)
      else:self.assertLess(luminance,.6)
     self.assertEqual(pin.get_active(),active)
     metrics.append({'theme':theme,'active':active,'scale':scale,'logical':[16,16],'pixels':[pixels.get_width(),pixels.get_height()],'button':[pin.get_allocated_width(),pin.get_allocated_height()],'rgb':rgb})
   self.assertEqual(owner.keep_calls,[True,False,True,False])
   self.assertEqual(pin.get_accessible().get_name(),'保持在最上層')
   print('PIN_CONTROL_METRICS '+json.dumps({'cases':metrics,'keep_above_calls':owner.keep_calls,'wm_applied_state':'not claimed: minimal fixture has no WM'}))
  finally:
   owner.destroy();settings.set_property('gtk-theme-name',original);settle()

if __name__=='__main__':unittest.main()
