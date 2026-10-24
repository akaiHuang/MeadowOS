#!/usr/bin/python3
"""Session-owned decorations for real X11 server-decorated application windows.

xfwm4 retains focus, move/resize, maximize, minimize and lifetime authority.
No client is embedded or relaunched. A system toolbar reserves space above the
existing client and restores its geometry/decorations when the service stops.
"""
from __future__ import annotations
import json
import math
import os
from pathlib import Path
import signal
import sys
import time

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT.parent / 'desktop'))
HEADER = 32
RADIUS = 16


def body_rect(outer, shown, header=HEADER):
    x, y, width, height = outer
    inset = min(header, max(0, height-64)) if shown else 0
    return x, y+inset, width, height-inset


def shape_rows(width, height, top, bottom):
    """Scanline rectangles for the actual client, never an artificial inner frame."""
    result=[]
    radius=min(RADIUS,width//2,height//2)
    for y in range(height):
        edge = y if top and y<radius else height-y-1 if bottom and y>=height-radius else radius
        inset=max(0,math.ceil(radius-math.sqrt(max(0,radius*radius-(radius-edge-.5)**2))-.5)) if edge<radius else 0
        if result and result[-1][0]==inset and result[-1][2]==width-inset*2:
            a,b,c,d=result[-1];result[-1]=(a,b,c,d+1)
        else:result.append((inset,y,width-inset*2,1))
    return result


def motif_decoration(hints, enabled):
    """Change only decoration hints, preserving the client's restrictions."""
    result = (list(hints) + [0] * 5)[:5]
    result[0] |= 2
    result[2] = int(enabled)
    return result


def normal_hint_record(normal):
    # python-xlib nests aspect-ratio DictWrappers inside WM_NORMAL_HINTS.
    return {key: dict(value._data) if hasattr(value, '_data') else int(value)
            for key,value in normal._data.items()}


def validate_normal_hints(record):
    if record is None:return None
    scalar={'flags','min_width','min_height','max_width','max_height','width_inc','height_inc','base_width','base_height','win_gravity'}
    if not isinstance(record,dict) or set(record)!=scalar|{'min_aspect','max_aspect'}:
        raise ValueError('Invalid saved normal hints')
    for key in scalar:
        value=record[key];limit=2**32 if key=='flags' else 2**31
        if type(value) is not int or not (0 if key=='flags' else -2**31)<=value<limit:
            raise ValueError('Invalid saved normal hint value')
    for key in ('min_aspect','max_aspect'):
        value=record[key]
        if not isinstance(value,dict) or set(value)!={'num','denum'} or any(type(n) is not int or not -2**31<=n<2**31 for n in value.values()):
            raise ValueError('Invalid saved aspect ratio')
    return record


def keep_header(inside, held, dragging, active, shown):
    # Another application's drag must not reveal or resize this client.
    return inside or held and (dragging or active and shown)


def gesture_rect(rect, direction, dx, dy, minimum=(160, 96), maximum=(65535, 65535)):
    x,y,w,h=rect
    if direction==8:return x+dx,y+dy,w,h
    left=direction in (0,6,7);right=direction in (2,3,4)
    top=direction in (0,1,2);bottom=direction in (4,5,6)
    nw=max(minimum[0],min(maximum[0],w+(-dx if left else dx if right else 0)))
    nh=max(minimum[1],min(maximum[1],h+(-dy if top else dy if bottom else 0)))
    return x+w-nw if left else x,y+h-nh if top else y,nw,nh


def main():
    import gi
    gi.require_version('Gtk','3.0');gi.require_version('Gdk','3.0');gi.require_version('GdkX11','3.0')
    import cairo
    from gi.repository import Gdk,GdkX11,GLib,Gtk
    from Xlib import X, Xatom, Xutil, display, error
    from Xlib.protocol import event
    from Xlib.ext import shape
    from glass.gtk_surface import GlassSurface
    if not os.environ.get('DISPLAY'):raise RuntimeError('A private X11 session is required')
    Gtk.init(None)
    connection=display.Display()
    root=connection.screen().root
    root.change_attributes(event_mask=X.PropertyChangeMask|X.SubstructureNotifyMask)
    atoms={}
    def atom(name):
        if name not in atoms:atoms[name]=connection.intern_atom(name)
        return atoms[name]
    def values(window,name,kind=X.AnyPropertyType):
        prop=window.get_full_property(atom(name),kind)
        return prop.value if prop else []
    def message(client,name,data):
        root.send_event(event.ClientMessage(window=client,client_type=atom(name),data=(32,list(data)+[0]*(5-len(data)))),event_mask=X.SubstructureRedirectMask|X.SubstructureNotifyMask)
        connection.flush()
    def geometry(client):
        g=client.get_geometry();p=root.translate_coords(client,0,0)
        return p.x,p.y,g.width,g.height
    def set_geometry(client,rect):message(client,'_NET_MOVERESIZE_WINDOW',[10|(15<<8)|(2<<12),*map(int,rect)])
    def decoration(client,enabled):
        hints=values(client,'_MOTIF_WM_HINTS')
        client.change_property(atom('_MOTIF_WM_HINTS'),atom('_MOTIF_WM_HINTS'),32,motif_decoration(hints,enabled));connection.flush()
    def native_rects(window,rects):
        region=cairo.Region([cairo.RectangleInt(*rect) for rect in rects])
        window.shape_combine_region(region,0,0);window.input_shape_combine_region(region,0,0)
    def rounded_client(client,w,h,shown,maximized):
        if maximized:client.shape_mask(shape.SO.Set,shape.SK.Bounding,0,0,X.NONE)
        else:client.shape_rectangles(shape.SO.Set,shape.SK.Bounding,X.YXBanded,0,0,shape_rows(w,h,not shown,True))

    gd=Gdk.Display.get_default();screen=Gdk.Screen.get_default()
    css=Gtk.CssProvider();css.load_from_path(str(ROOT.parent/'desktop/window-chrome.css'))
    Gtk.StyleContext.add_provider_for_screen(screen,css,Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION+12)
    frame_css=Gtk.CssProvider();frame_css.load_from_data(b'''
      .meadow-system-frame, .meadow-system-edge {background: transparent; border:0; box-shadow:none;}
      .meadow-system-frame.maximized .meadow-window-button {min-height:22px;}
      .meadow-system-frame .meadow-hover-toolbar {background:transparent;box-shadow:inset 0 1px alpha(white,.24),inset 0 -1px alpha(@theme_fg_color,.07);border-radius:16px 16px 0 0;}
    ''');Gtk.StyleContext.add_provider_for_screen(screen,frame_css,Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION+15)
    marker=Gtk.Window(type=Gtk.WindowType.POPUP);marker.set_decorated(False);marker.realize()
    marker_xid=marker.get_window().get_xid()
    owner_atom=atom('_MEADOW_FRAME_SERVICE')
    previous=values(root,'_MEADOW_FRAME_SERVICE')
    if len(previous):
        try:
            connection.create_resource_object('window',int(previous[0])).get_attributes()
            raise RuntimeError('A Meadow system frame service already owns this display')
        except error.BadWindow:pass
    root.change_property(owner_atom,Xatom.CARDINAL,32,[marker_xid,os.getpid()]);connection.flush()

    class Managed:
        def __init__(self,client):
            self.client=client;self.xid=client.id;self.shown=False;self.hidden=False;self.closed=False
            client.change_attributes(event_mask=X.PropertyChangeMask|X.StructureNotifyMask)
            self.leave=None;self.pending=None;self.deadline=0;self.last_shape=None;self.maximized=False;self.header_height=HEADER
            self.dragging=False;self.gesture=None;self.pre_max=None;self.fullscreen=False;self.pre_full=None
            self.special_deadline=0;self.layouts={};self.last_stack=None;self.last_title=None;self.last_bar_shape=None
            self.last_record=None
            hints=values(client,'_MOTIF_WM_HINTS')
            saved=values(client,'_MEADOW_FRAME_RESTORE')
            self.original=list(map(int,hints))
            normal=client.get_wm_normal_hints()
            self.original_normal=normal_hint_record(normal) if normal and normal.flags & Xutil.PResizeInc else None
            self.outer=geometry(client)
            ext=values(client,'_NET_FRAME_EXTENTS')
            self.original_extents=list(map(int,ext)) if len(ext)==4 else [0,0,0,0]
            if isinstance(saved,bytes) and saved:
                record=json.loads(saved.decode('utf-8'))
                if (not isinstance(record,dict) or len(record.get('outer',[]))!=4
                        or len(record.get('extents',[]))!=4 or len(record.get('hints',[]))>5
                        or not all(isinstance(n,int) for key in ('outer','extents','hints') for n in record[key])):
                    raise ValueError('Invalid saved system-frame geometry')
                self.original=record['hints'];self.outer=tuple(record['outer']);self.original_extents=record['extents']
                self.original_normal=validate_normal_hints(record.get('normal_hints',self.original_normal))
            elif len(ext)==4:
                x,y,w,h=self.outer;l,r,t,b=self.original_extents;self.outer=(x-l,y-t,w+l+r,h+t+b)
            # A new default app can size itself before the Shell publishes its
            # reserved panel area. Keep the actual outer window on this desktop.
            work=values(root,'_NET_WORKAREA')
            desktop=values(root,'_NET_CURRENT_DESKTOP');index=(int(desktop[0]) if len(desktop) else 0)*4
            if len(work)>=index+4:
                ax,ay,aw,ah=map(int,work[index:index+4]);x,y,w,h=self.outer
                w,h=min(w,aw),min(h,ah)
                self.outer=(min(max(x,ax),ax+aw-w),min(max(y,ay),ay+ah-h),w,h)
            self.checkpoint()
            # Pixel resizing keeps a hover row from accumulating terminal-cell
            # rounding on every show/hide. Keep every min/max/aspect restriction
            # and restore the application's original increments on teardown.
            if self.original_normal:
                pixel=dict(self.original_normal);pixel['flags'] &= ~Xutil.PResizeInc
                client.set_wm_normal_hints(pixel)
            self.applied_normal=pixel if self.original_normal else normal_hint_record(normal) if normal else None
            self.bar=self.popup('meadow-system-frame')
            self.surface=GlassSurface(self.bar,mode='content',fps=2)
            # Paint the one outer frame at the popup bounds. Putting this
            # radius/highlight on the padded controls creates an inset second
            # corner, especially visible above Chrome's own square content.
            toolbar=Gtk.Box(spacing=0);toolbar.get_style_context().add_class('meadow-hover-toolbar')
            box=Gtk.Box(spacing=0);self.box=box;box.set_margin_start(14);box.set_margin_end(12)
            box.set_size_request(-1,HEADER);toolbar.pack_start(box,True,True,0)
            from window_controls import window_button
            for style,label,action in (('close','關閉視窗',self.close),('minimize','最小化視窗',self.minimize),('maximize','最大化／還原視窗',self.maximize)):
                box.pack_start(window_button(style,label,action),False,False,0)
            drag=Gtk.EventBox();drag.set_visible_window(False);drag.add_events(Gdk.EventMask.BUTTON_PRESS_MASK)
            drag.connect('button-press-event',self.drag)
            self.title=Gtk.Label();self.title.set_ellipsize(3);self.title.get_style_context().add_class('meadow-window-title')
            drag.add(self.title);box.pack_start(drag,True,True,10);self.surface.add(toolbar);self.bar.add(self.surface)
            self.edges=[]
            for direction,cursor in ((0,'nw-resize'),(1,'n-resize'),(2,'ne-resize'),(3,'e-resize'),(4,'se-resize'),(5,'s-resize'),(6,'sw-resize'),(7,'w-resize')):
                edge=self.popup('meadow-system-edge');edge.add_events(Gdk.EventMask.BUTTON_PRESS_MASK|Gdk.EventMask.ENTER_NOTIFY_MASK)
                edge.connect('button-press-event',lambda _w,e,d=direction:self.resize(e,d))
                edge.connect('enter-notify-event',lambda w,_e,name=cursor:self.cursor(w,name))
                self.edges.append(edge)
            states=set(map(int,values(client,'_NET_WM_STATE')))
            self.fullscreen=atom('_NET_WM_STATE_FULLSCREEN') in states
            self.maximized=bool({atom('_NET_WM_STATE_MAXIMIZED_HORZ'),atom('_NET_WM_STATE_MAXIMIZED_VERT')} & states)
            if not (self.fullscreen or self.maximized):
                decoration(client,False);self.apply(False)
            client.change_property(atom('_MEADOW_SYSTEM_FRAME'),Xatom.CARDINAL,32,[1,marker_xid,0,HEADER])

        def popup(self,style):
            window=Gtk.Window(type=Gtk.WindowType.POPUP);window.set_decorated(False)
            window.set_accept_focus(False);window.set_focus_on_map(False);window.set_resizable(True)
            visual=screen.get_rgba_visual()
            if visual:window.set_visual(visual)
            window.set_app_paintable(True);window.get_style_context().add_class(style)
            window.set_wmclass('meadow-system-decoration','MeadowSystemDecoration')
            # GTK maps a newly revealed toolbar asynchronously. Restack after
            # MapNotify too, so its top edge cannot cover the resize hit strip.
            window.connect('map-event',self.decoration_mapped)
            window.connect('draw',lambda _w,cr:(cr.set_operator(cairo.OPERATOR_SOURCE),cr.set_source_rgba(0,0,0,0),cr.paint(),cr.set_operator(cairo.OPERATOR_OVER),False)[-1])
            return window

        def decoration_mapped(self,*_):
            nonlocal last_pointer
            self.last_stack=None
            last_pointer=None
            return False

        def cursor(self,widget,name):widget.get_window().set_cursor(Gdk.Cursor.new_from_name(gd,name))
        def activate(self):message(self.client,'_NET_ACTIVE_WINDOW',[2,Gtk.get_current_event_time()])
        def close(self):message(self.client,'_NET_CLOSE_WINDOW',[Gtk.get_current_event_time(),2])
        def minimize(self):message(self.client,'WM_CHANGE_STATE',[3])
        def maximize(self):message(self.client,'_NET_WM_STATE',[2,atom('_NET_WM_STATE_MAXIMIZED_HORZ'),atom('_NET_WM_STATE_MAXIMIZED_VERT'),2])
        def move_resize(self,e,direction):
            if e.button!=1 or self.fullscreen or self.maximized:return False
            self.activate()
            self.dragging=True
            hints=self.client.get_wm_normal_hints()
            minimum=(max(64,int(getattr(hints,'min_width',160))),max(64,int(getattr(hints,'min_height',64)))+(self.header_height if self.shown else 0))
            maximum=(int(getattr(hints,'max_width',65535)) or 65535,(int(getattr(hints,'max_height',65535)) or 65535)+(self.header_height if self.shown else 0))
            self.gesture=(direction,int(e.x_root),int(e.y_root),self.outer,minimum,maximum)
            # Keep GTK's real implicit pointer grab. Geometry is requested from
            # the existing WM; no second client can steal that grab mid-press.
            GLib.timeout_add(16,self.drag_tick)
            return True
        def drag_tick(self):
            if self.closed or not self.gesture:return False
            try:
                pointer=root.query_pointer()
                if not pointer.mask & X.Button1Mask:
                    self.gesture=None;self.dragging=False;return False
                direction,px,py,rect,minimum,maximum=self.gesture
                target=gesture_rect(rect,direction,pointer.root_x-px,pointer.root_y-py,minimum,maximum)
                if target!=self.outer:
                    self.outer=target;set_geometry(self.client,body_rect(target,self.shown,self.header_height))
                    self.pending=None;self.last_shape=None
                    self.sync(pointer.root_x,pointer.root_y,self.xid,pointer.mask,self.xid,())
                return True
            except error.BadWindow:self.gesture=None;return False
        def drag(self,_widget,e):
            if e.type==Gdk.EventType.DOUBLE_BUTTON_PRESS:self.maximize();return True
            return self.move_resize(e,8)
        def resize(self,e,direction):return self.move_resize(e,direction)

        def apply(self,shown):
            self.shown=shown;rect=body_rect(self.outer,shown,self.header_height)
            self.pending=rect;self.deadline=time.monotonic()+.4
            set_geometry(self.client,rect)
            self.client.change_property(atom('_MEADOW_SYSTEM_FRAME'),Xatom.CARDINAL,32,[1,marker_xid,int(shown),self.header_height])

        def checkpoint(self):
            record=json.dumps(dict(outer=self.outer,hints=self.original,extents=self.original_extents,normal_hints=self.original_normal)).encode()
            if record!=self.last_record:
                self.client.change_property(atom('_MEADOW_FRAME_RESTORE'),atom('UTF8_STRING'),8,record)
                self.last_record=record

        def hide(self):
            for window in [self.bar,*self.edges]:
                if window.get_visible():window.hide()
            self.last_stack=None

        def layout(self,window,rect):
            previous=self.layouts.get(window)
            if previous!=rect:
                x,y,w,h=rect;window.move(x,y);window.resize(max(1,w),max(1,h));self.layouts[window]=rect
            if not window.get_visible():window.show_all();self.last_stack=None

        def stack(self,windows):
            # Keep decorations immediately above their owner, below windows
            # which occlude that owner. No global keep-above windows.
            parent=self.client.query_tree().parent
            sibling=parent if parent.id!=root.id else self.client
            for window in windows:
                native=window.get_window()
                if native and window.get_mapped():
                    item=connection.create_resource_object('window',native.get_xid())
                    item.configure(sibling=sibling,stack_mode=X.Above);sibling=item

        def sync(self,px,py,top,buttons,active,stacking):
            now=time.monotonic();states=set(map(int,values(self.client,'_NET_WM_STATE')))
            is_hidden=atom('_NET_WM_STATE_HIDDEN') in states
            maxed=bool({atom('_NET_WM_STATE_MAXIMIZED_HORZ'),atom('_NET_WM_STATE_MAXIMIZED_VERT')} & states)
            full=atom('_NET_WM_STATE_FULLSCREEN') in states
            if not buttons & X.Button1Mask:self.dragging=False
            if full:
                if not self.fullscreen:
                    self.pre_full=(self.outer,self.shown)
                    self.client.shape_mask(shape.SO.Set,shape.SK.Bounding,0,0,X.NONE)
                    self.fullscreen=True;self.last_shape=None
                self.hide()
                return
            if self.fullscreen:
                self.fullscreen=False;self.special_deadline=now+.2
                if not maxed and self.pre_full:
                    self.outer,self.shown=self.pre_full;self.apply(self.shown)
                self.pre_full=None
                return
            if is_hidden or self.client.get_attributes().map_state!=X.IsViewable:
                self.hide();return
            if maxed!=self.maximized:
                # Maximize uses the WM's own reserved title row; never cover
                # client content or fake maximization with an app-sized window.
                if maxed:self.pre_max=(self.outer,self.shown)
                self.maximized=maxed;decoration(self.client,maxed);self.pending=None
                self.last_shape=None;self.special_deadline=now+.2
                self.hide()
                if not maxed:
                    if self.pre_max:self.outer,self.shown=self.pre_max
                    else:
                        x,y,w,h=geometry(self.client);ext=values(self.client,'_NET_FRAME_EXTENTS')
                        l,r,t,b=map(int,ext) if len(ext)==4 else (0,0,0,0)
                        self.outer=(x-l,y-t,w+l+r,h+t+b);self.shown=False
                    self.pre_max=None;self.apply(self.shown)
                return
            if now<self.special_deadline:return
            normal=self.client.get_wm_normal_hints()
            current=normal_hint_record(normal) if normal else None
            if current is not None and (current!=self.applied_normal or current['flags'] & Xutil.PResizeInc):
                self.original_normal=current
                pixel=dict(current);pixel['flags'] &= ~Xutil.PResizeInc
                if pixel!=current:self.client.set_wm_normal_hints(pixel)
                self.applied_normal=pixel;self.checkpoint()
            hints=values(self.client,'_MOTIF_WM_HINTS')
            if len(hints)>=5 and len(self.original)>=3:
                updated=list(map(int,hints));updated[0]=(updated[0]&~2)|(self.original[0]&2);updated[2]=self.original[2]
                if updated!=self.original:self.original=updated;self.checkpoint()
            if not maxed and (len(hints)<3 or not hints[0]&2 or hints[2]):
                # Chrome can publish decoration hints again on a GTK theme
                # change. Reapply the system frame without adding a second row.
                decoration(self.client,False);self.apply(self.shown)
                self.special_deadline=now+.15;return
            rect=geometry(self.client)
            if self.pending and rect!=self.pending:
                # Some terminals also snap their own GTK allocation, even with
                # pixel WM hints. Reserve that small cell remainder above the
                # client instead of shrinking the outer window each hover.
                if self.shown and rect[:3]==self.pending[:3]:
                    inset=self.outer[3]-rect[3]
                    if HEADER<=inset<=HEADER+64 and inset!=self.header_height:
                        self.header_height=inset;self.apply(True);return
                if now<self.deadline:return
            self.pending=None
            x,y,w,h=rect
            ext=values(self.client,'_NET_FRAME_EXTENTS')
            bar_height=self.header_height
            if maxed:
                top_inset=int(ext[2]) if len(ext)==4 else 24
                self.outer=(x,y-top_inset,w,h+top_inset)
                bar_height=top_inset
            elif not self.gesture:
                self.outer=(x,y-(self.header_height if self.shown else 0),w,h+(self.header_height if self.shown else 0))
            ox,oy,ow,oh=self.outer
            own_ids={self.xid}
            own_ids.update(win.get_window().get_xid() for win in [self.bar,*self.edges] if win.get_window())
            inside=top in own_ids and ox-5<=px<ox+ow+5 and oy-5<=py<oy+oh+5
            want=keep_header(inside,bool(buttons & X.Button1Mask),self.dragging,active==self.xid,self.shown)
            if want:self.leave=None
            elif self.leave is None:self.leave=now
            if not maxed and want!=self.shown and (want or now-self.leave>.38):
                self.apply(want);return
            key=(w,h,self.shown,maxed)
            if key!=self.last_shape:
                rounded_client(self.client,w,h,self.shown,maxed);self.last_shape=key
            if not maxed and not self.gesture:self.checkpoint()
            title=values(self.client,'_NET_WM_NAME')
            if isinstance(title,bytes) and title!=self.last_title:
                self.title.set_text(title.decode('utf-8','replace'));self.last_title=title
            if self.shown or maxed:
                style=self.bar.get_style_context()
                if style.has_class('maximized')!=maxed:
                    (style.add_class if maxed else style.remove_class)('maximized')
                if self.box.get_size_request().height!=bar_height:self.box.set_size_request(-1,bar_height)
                self.layout(self.bar,(ox,oy,ow,bar_height))
                bar_shape=(ow,bar_height,maxed)
                if bar_shape!=self.last_bar_shape:
                    native_rects(self.bar.get_window(),shape_rows(ow,bar_height,not maxed,False));self.last_bar_shape=bar_shape
            elif self.bar.get_visible():self.bar.hide();self.last_stack=None
            rects=[(ox,oy,8,8),(ox+8,oy,ow-16,4),(ox+ow-8,oy,8,8),
                   (ox+ow-4,oy+8,4,oh-16),(ox+ow-8,oy+oh-8,8,8),
                   (ox+8,oy+oh-4,ow-16,4),(ox,oy+oh-8,8,8),(ox,oy+8,4,oh-16)]
            for edge,geometry_ in zip(self.edges,rects):
                if maxed:
                    if edge.get_visible():edge.hide();self.last_stack=None
                    continue
                self.layout(edge,geometry_)
            if self.last_stack!=stacking:
                self.stack([self.bar,*self.edges]);self.last_stack=stacking
            connection.flush()

        def restore(self):
            if self.closed:return
            self.closed=True
            try:
                self.client.shape_mask(shape.SO.Set,shape.SK.Bounding,0,0,X.NONE)
                if self.original:self.client.change_property(atom('_MOTIF_WM_HINTS'),atom('_MOTIF_WM_HINTS'),32,self.original)
                else:
                    # xfwm does not re-enable a removed border when Motif hints
                    # merely disappear. Publish the default explicitly first,
                    # let the WM acknowledge its frame, then restore absence.
                    self.client.change_property(atom('_MOTIF_WM_HINTS'),atom('_MOTIF_WM_HINTS'),32,[2,0,1,0,0])
                    connection.flush()
                    deadline=time.monotonic()+.6
                    while any(self.original_extents) and time.monotonic()<deadline:
                        ext=values(self.client,'_NET_FRAME_EXTENTS')
                        if len(ext)==4 and any(ext):break
                        time.sleep(.01)
                    self.client.delete_property(atom('_MOTIF_WM_HINTS'))
                if self.original_normal:self.client.set_wm_normal_hints(self.original_normal)
                if not self.maximized and not self.fullscreen:
                    x,y,w,h=self.outer;l,r,t,b=self.original_extents
                    set_geometry(self.client,(x+l,y+t,max(64,w-l-r),max(64,h-t-b)))
                for name in ('_MEADOW_SYSTEM_FRAME','_MEADOW_FRAME_RESTORE'):self.client.delete_property(atom(name))
            except error.XError:pass
            self.bar.destroy()
            for edge in self.edges:edge.destroy()

    managed={};stopping=False;last_pointer=None
    def stop(*_):
        nonlocal stopping;stopping=True
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    def top_client(pointer):
        window=pointer.child
        for _ in range(6):
            if not window or window==X.NONE:break
            owned={w.get_window().get_xid() for item in managed.values() for w in [item.bar,*item.edges] if w.get_window()}
            if window.id in managed or window.id in owned:return pointer,window.id
            child=window.query_pointer().child
            if not child or child==X.NONE:return pointer,window.id
            window=child
        return pointer,0
    def tick():
        nonlocal last_pointer
        if stopping:Gtk.main_quit();return False
        try:
            dirty=last_pointer is None
            while connection.pending_events():connection.next_event();dirty=True
            pointer=root.query_pointer()
            position=(pointer.root_x,pointer.root_y,pointer.mask,pointer.child.id if pointer.child else 0)
            now=time.monotonic()
            unsettled=any(item.pending or item.gesture or now<item.special_deadline
                          or item.shown and item.leave is not None for item in managed.values())
            if not dirty and position==last_pointer and not unsettled:return True
            last_pointer=position
            ids=set(map(int,values(root,'_NET_CLIENT_LIST')))
            for xid in set(managed)-ids:managed.pop(xid).restore()
            for xid in ids-set(managed):
                window=connection.create_resource_object('window',xid)
                types=values(window,'_NET_WM_WINDOW_TYPE')
                if len(types) and atom('_NET_WM_WINDOW_TYPE_NORMAL') not in types:continue
                if len(values(window,'_MEADOW_CHROME_STATE')):continue
                classes=window.get_wm_class() or ()
                if any(name.casefold().startswith('meadow') for name in classes):continue
                hints=values(window,'_MOTIF_WM_HINTS');ext=values(window,'_NET_FRAME_EXTENTS')
                has_frame=len(ext)==4 and any(ext)
                restoring=len(values(window,'_MEADOW_FRAME_RESTORE'))>0
                if not has_frame and not restoring:continue
                # CSD is application content and cannot be erased by a WM.
                if len(hints)>=3 and hints[0]&2 and not hints[2] and not restoring:continue
                # Preserve custom-shaped clients rather than replacing their
                # own application geometry. Recovered Meadow shapes are ours.
                if not restoring and window.shape_query_extents().bounding_shaped:continue
                try:managed[xid]=Managed(window)
                except (ValueError,KeyError,TypeError,UnicodeError):
                    print('Skipped invalid saved system-frame state',file=sys.stderr,flush=True)
            pointer,top=top_client(pointer);active=values(root,'_NET_ACTIVE_WINDOW')
            stacking=tuple(map(int,values(root,'_NET_CLIENT_LIST_STACKING')))
            for client in list(managed.values()):
                try:client.sync(pointer.root_x,pointer.root_y,top,pointer.mask,int(active[0]) if len(active) else 0,stacking)
                except error.BadWindow:managed.pop(client.xid,None);client.restore()
        except error.XError as exc:print(f'Meadow frame X11 request failed: {type(exc).__name__}',file=sys.stderr,flush=True)
        return True
    GLib.timeout_add(80,tick)
    try:Gtk.main()
    finally:
        for client in list(managed.values()):client.restore()
        root.delete_property(owner_atom);connection.sync();marker.destroy();connection.close()
    return 0

if __name__=='__main__':raise SystemExit(main())
