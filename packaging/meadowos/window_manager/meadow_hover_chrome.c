/* MeadowOS GTK3 native window chrome adapter. SPDX-License-Identifier: Apache-2.0 */
#include <gtk/gtk.h>
#include <gtk/gtkx.h>
#include <gdk/gdkx.h>
#include <math.h>
#include <signal.h>
#include <string.h>

#define TOP_EDGE 12
#define CORNER_RADIUS 16
#define HIDE_DELAY_US (380 * 1000)
#define OWNER_CLASS "meadow-hover-chrome-owned"
#define STATE_KEY "meadow-hover-chrome-state"
#define GLASS_MAX_DIMENSION 4096
#define GLASS_MAX_PIXELS ((gsize)4194304)

typedef struct {
    GtkOverlay parent;
} MeadowOverlay;
typedef struct { GtkOverlayClass parent; } MeadowOverlayClass;
G_DEFINE_TYPE(MeadowOverlay, meadow_overlay, GTK_TYPE_OVERLAY)

typedef struct {
    gint x, y, width, height, scale, split;
    gint root_x,root_y;
    gboolean dark;
} GlassGeometry;

static void rounded_path(cairo_t *cr, double width, double height, double radius) {
    radius = MIN(radius, MIN(width, height) / 2.0);
    cairo_new_sub_path(cr);
    cairo_arc(cr, width-radius, radius, radius, -G_PI/2, 0);
    cairo_arc(cr, width-radius, height-radius, radius, 0, G_PI/2);
    cairo_arc(cr, radius, height-radius, radius, G_PI/2, G_PI);
    cairo_arc(cr, radius, radius, radius, G_PI, 3*G_PI/2);
    cairo_close_path(cr);
}

static gboolean meadow_overlay_draw(GtkWidget *widget, cairo_t *cr) {
    GtkWidget *window = gtk_widget_get_toplevel(widget);
    gboolean square = gtk_style_context_has_class(gtk_widget_get_style_context(window),
                                                  "meadow-chrome-square");
    cairo_save(cr);
    rounded_path(cr, gtk_widget_get_allocated_width(widget),
                 gtk_widget_get_allocated_height(widget), square ? 0 : CORNER_RADIUS);
    cairo_clip(cr);
    /* Thunar leaves blank layout/preview cells unpainted. The window's root is
     * transparent only for outer rounded corners; ordinary content must have
     * one coherent opaque theme base. Regional glass draws over this base,
     * before GTK draws its original foreground. */
    GtkStyleContext *style=gtk_widget_get_style_context(widget);
    GdkRGBA base;
    if (!gtk_style_context_lookup_color(style,"meadow_bg",&base) &&
        !gtk_style_context_lookup_color(style,"theme_bg_color",&base)) {
        GdkRGBA foreground;
        gtk_style_context_get_color(style,gtk_style_context_get_state(style),&foreground);
        gboolean dark=foreground.red*.2126+foreground.green*.7152+foreground.blue*.0722 > .6;
        gdk_rgba_parse(&base,dark ? "#202830" : "#f6f8fa");
    }
    base.alpha=1.0;
    gdk_cairo_set_source_rgba(cr,&base);
    cairo_paint(cr);
    gboolean result = GTK_WIDGET_CLASS(meadow_overlay_parent_class)->draw(widget, cr);
    cairo_restore(cr);
    return result;
}
static void meadow_overlay_class_init(MeadowOverlayClass *klass) {
    GTK_WIDGET_CLASS(klass)->draw = meadow_overlay_draw;
}
static void meadow_overlay_init(MeadowOverlay *self) { (void)self; }

typedef struct {
    GtkWindow *window;
    GtkWidget *overlay;
    GtkWidget *content;
    GtkWidget *header;
    GtkWidget *revealer;
    GtkWidget *layout;
    GtkGesture *move_gesture;
    gint shape_width, shape_height, shape_radius;
    gint64 geometry_changed;
    GlassGeometry observed;
    GtkWidget *resize_edges[4];
    gboolean visible;
    gboolean keyboard;
    gboolean fullscreen;
    gboolean iconified;
    gboolean square;
    gboolean destroyed;
    gint refs;
    GtkWidget *sidebar;
    GtkWidget *files_header_left;
    gboolean joined_surface;
    gboolean dialog_chrome;
    gboolean popup_surface,popup_tooltip,dialog_surface;
    gint popup_radius,popup_root_x,popup_root_y;
    GSubprocess *glass_process;
    GCancellable *glass_cancel;
    cairo_surface_t *glass_frame;
    guint32 glass_header[4];
    guchar *glass_pixels;
    gchar *glass_request;
    gboolean glass_inflight;
    gint64 glass_last_request;
    GlassGeometry glass_requested;
    GlassGeometry glass_completed;
    guint glass_source_count;
    guint glass_failures;
    guint glass_idle_frames;
    gint64 leave_time;
} Chrome;

static GList *windows;
static guint poll_source;
static GtkCssProvider *css;
static void retire_bridge(GSubprocess *process);

static gboolean has_class(GtkWidget *widget, const char *name) {
    return gtk_style_context_has_class(gtk_widget_get_style_context(widget), name);
}
static void add_class(GtkWidget *widget, const char *name) {
    gtk_style_context_add_class(gtk_widget_get_style_context(widget), name);
}
/* The Settings hub launches real native panels with this role. Exact legacy
 * executable identities also cover panels opened by their vendor shortcuts;
 * document applications never inherit a blanket transparent-view policy. */
static gboolean settings_process(void) {
    if(!g_strcmp0(g_getenv("MEADOW_UI_ROLE"),"settings"))return TRUE;
    const gchar *name=g_get_prgname();
    const gchar *tools[]={"xfce4-appearance-settings","xfce4-display-settings",
        "xfce4-keyboard-settings","xfce4-mouse-settings","xfce4-accessibility-settings",
        "xfce4-settings-editor","xfce4-session-settings","xfwm4-settings",
        "xfwm4-tweaks-settings","xfwm4-workspace-settings","xfce4-notifyd-config","xfce4-power-manager-settings",
        "xfce4-mime-settings","xfce4-color-settings",NULL};
    for(guint i=0;tools[i];i++)if(!g_strcmp0(name,tools[i]))return TRUE;
    return FALSE;
}
static void tag_settings_surface(GtkWidget *widget) {
    if(GTK_IS_WINDOW(widget) && settings_process())add_class(widget,"meadow-system-settings");
}
static void state_class(GtkWidget *widget, const char *name, gboolean active) {
    GtkStyleContext *context = gtk_widget_get_style_context(widget);
    if (active) gtk_style_context_add_class(context, name);
    else gtk_style_context_remove_class(context, name);
}

/* Shape both rendering and input. Scaling is handled by GDK. */
static void update_shape(Chrome *chrome) {
    GdkWindow *window = gtk_widget_get_window(GTK_WIDGET(chrome->window));
    if (!window) return;
    if (chrome->square) {
        chrome->shape_width=chrome->shape_height=chrome->shape_radius=0;
        gdk_window_shape_combine_region(window, NULL, 0, 0);
        gdk_window_input_shape_combine_region(window, NULL, 0, 0);
        return;
    }
    int width = gtk_widget_get_allocated_width(GTK_WIDGET(chrome->window));
    int height = gtk_widget_get_allocated_height(GTK_WIDGET(chrome->window));
    int radius = MIN(CORNER_RADIUS, MIN(width, height) / 2);
    if (chrome->shape_width==width && chrome->shape_height==height && chrome->shape_radius==radius) return;
    chrome->shape_width=width;chrome->shape_height=height;chrome->shape_radius=radius;
    cairo_region_t *region = cairo_region_create();
    for (int y=0; y<height; y++) {
        int inset=0;
        if (y < radius || y >= height-radius) {
            double dy = y < radius ? radius-y-0.5 : y-(height-radius)+0.5;
            inset = (int)ceil(radius-sqrt(MAX(0.0, radius*radius-dy*dy)));
        }
        cairo_rectangle_int_t row = {inset, y, MAX(0, width-2*inset), 1};
        cairo_region_union_rectangle(region, &row);
    }
    gdk_window_shape_combine_region(window, region, 0, 0);
    gdk_window_input_shape_combine_region(window, region, 0, 0);
    cairo_region_destroy(region);
}

typedef struct { Chrome *chrome; GArray *values; guint count; } RegionScan;
static void append_region(RegionScan *scan, GtkWidget *widget, gulong kind) {
    if (!gtk_widget_get_visible(widget)) return;
    gint x,y;
    if (!gtk_widget_translate_coordinates(widget, GTK_WIDGET(scan->chrome->window),0,0,&x,&y)) return;
    gint width=gtk_widget_get_allocated_width(widget), height=gtk_widget_get_allocated_height(widget);
    if (width <= 1 || height <= 1 || x < 0 || y < 0) return;
    gulong region[]={x,y,width,height,kind,CORNER_RADIUS};
    g_array_append_vals(scan->values,region,G_N_ELEMENTS(region));
    scan->count++;
}
static void scan_regions(GtkWidget *widget, gpointer data) {
    RegionScan *scan=data;
    if (has_class(widget,"meadow-glass-sidebar")) { append_region(scan,widget,1); return; }
    if (GTK_IS_CONTAINER(widget)) gtk_container_forall(GTK_CONTAINER(widget),scan_regions,data);
}
static void publish_regions(Chrome *chrome, GdkWindow *window) {
    GArray *values=g_array_new(FALSE,FALSE,sizeof(gulong));
    gulong prefix[]={1,0};
    g_array_append_vals(values,prefix,2);
    RegionScan scan={chrome,values,0};
    scan_regions(chrome->content,&scan);
    if (chrome->visible && chrome->header) append_region(&scan,chrome->header,2);
    g_array_index(values,gulong,1)=scan.count;
    gdk_property_change(window,gdk_atom_intern_static_string("_MEADOW_GLASS_REGIONS"),
                        gdk_atom_intern_static_string("CARDINAL"),32,
                        GDK_PROP_MODE_REPLACE,(const guchar *)values->data,values->len);
    g_array_free(values,TRUE);
}

static void publish_state(Chrome *chrome) {
    GdkWindow *window = gtk_widget_get_window(GTK_WIDGET(chrome->window));
    if (!window || !GDK_IS_X11_WINDOW(window)) return;
    /* CARDINALs: version, reveal state, actual toolbar height, outer radius.
     * This is state metadata, not a claim of compositor acceptance. */
    gulong values[] = {1, chrome->visible,
        (gulong)(chrome->visible && chrome->header ? gtk_widget_get_allocated_height(chrome->header) : 0),
        (gulong)(chrome->square ? 0 : CORNER_RADIUS)};
    gdk_property_change(window, gdk_atom_intern_static_string("_MEADOW_CHROME_STATE"),
                        gdk_atom_intern_static_string("CARDINAL"), 32,
                        GDK_PROP_MODE_REPLACE, (const guchar *)values, 4);
    publish_regions(chrome,window);
}

static void set_visible(Chrome *chrome, gboolean visible) {
    if(!chrome->header)return;
    visible = visible && !chrome->fullscreen && !chrome->iconified;
    chrome->leave_time = 0;
    if (chrome->visible == visible) return;
    chrome->visible = visible;
    state_class(GTK_WIDGET(chrome->window), "meadow-chrome-visible", visible);
    if(chrome->dialog_chrome) {
        /* GtkRevealer collapse clears GtkWindow defaults in its descendants.
         * Dialog actions must stay mapped/realized and retain native Enter
         * behavior even before the first hover; only their paint is hidden. */
        gtk_widget_set_opacity(chrome->revealer,visible?1.0:0.0);
    } else {
        gtk_revealer_set_reveal_child(GTK_REVEALER(chrome->revealer), visible);
        gtk_widget_set_sensitive(chrome->header, visible);
    }
    /* Header and body are siblings in a vertical layout, never overlays. */
    publish_state(chrome);
}

/* One async frame per native window. All IPC happens outside GTK's draw path. */
static Chrome *chrome_ref(Chrome *chrome) { chrome->refs++; return chrome; }
static void chrome_unref(Chrome *chrome) {
    if (--chrome->refs) return;
    g_clear_object(&chrome->glass_process);
    g_clear_object(&chrome->glass_cancel);
    if (chrome->glass_frame) cairo_surface_destroy(chrome->glass_frame);
    g_free(chrome->glass_pixels);
    g_free(chrome->glass_request);
    g_free(chrome);
}
static GtkWidget *glass_target(Chrome *chrome) {
    return chrome->joined_surface || chrome->popup_surface ? chrome->content : chrome->sidebar;
}
static gboolean glass_geometry(Chrome *chrome, GlassGeometry *geometry) {
    memset(geometry,0,sizeof(*geometry));
    GtkWidget *target=glass_target(chrome);
    if (!target || !gtk_widget_translate_coordinates(target,GTK_WIDGET(chrome->window),
                                                     0,0,&geometry->x,&geometry->y)) return FALSE;
    geometry->scale=gtk_widget_get_scale_factor(GTK_WIDGET(chrome->window));
    geometry->width=gtk_widget_get_allocated_width(target)*geometry->scale;
    geometry->height=gtk_widget_get_allocated_height(target)*geometry->scale;
    geometry->x*=geometry->scale;
    geometry->y*=geometry->scale;
    if (geometry->width < 2 || geometry->height < 2 || geometry->width > GLASS_MAX_DIMENSION ||
        geometry->height > GLASS_MAX_DIMENSION ||
        (gsize)geometry->width*geometry->height > GLASS_MAX_PIXELS) return FALSE;
    GdkWindow *native=gtk_widget_get_window(GTK_WIDGET(chrome->window));
    if(native)gdk_window_get_origin(native,&geometry->root_x,&geometry->root_y);
    if(chrome->popup_surface) {
        if(!chrome->dialog_surface && (geometry->width>2048 || geometry->height>2048 ||
            (gsize)geometry->width*geometry->height>2000000))return FALSE;
        geometry->root_x=chrome->popup_root_x;geometry->root_y=chrome->popup_root_y;
    }
    geometry->split=0;
    if (chrome->joined_surface && chrome->sidebar && gtk_widget_get_mapped(chrome->sidebar)) {
        gint side_x,side_y;
        if (gtk_widget_translate_coordinates(chrome->sidebar,target,0,0,&side_x,&side_y))
            geometry->split=CLAMP((side_x+gtk_widget_get_allocated_width(chrome->sidebar))*geometry->scale,
                                 0,geometry->width);
    }
    GdkRGBA base;
    GtkStyleContext *style=gtk_widget_get_style_context(target);
    if (!gtk_style_context_lookup_color(style,"meadow_bg",&base) &&
        !gtk_style_context_lookup_color(style,"theme_bg_color",&base)) gdk_rgba_parse(&base,"#f6f8fa");
    geometry->dark=base.red*.2126+base.green*.7152+base.blue*.0722 < .5;
    return TRUE;
}
static gboolean glass_frame_matches(Chrome *chrome) {
    GlassGeometry current;
    if (!chrome->glass_frame || chrome->destroyed || !glass_geometry(chrome,&current)) return FALSE;
    const GlassGeometry *frame=&chrome->glass_completed;
    return current.x == frame->x && current.y == frame->y && current.width == frame->width &&
           current.height == frame->height && current.scale == frame->scale &&
           current.split == frame->split && current.dark == frame->dark &&
           (!chrome->popup_surface || (current.root_x==frame->root_x && current.root_y==frame->root_y));
}
/* Retain a valid same-theme material through header/resize allocation. Menus
 * still require exact geometry; stale frames are never reused across themes. */
static gboolean glass_frame_usable(Chrome *chrome) {
    if(glass_frame_matches(chrome))return TRUE;
    if(chrome->popup_surface && !chrome->dialog_surface)return FALSE;
    GlassGeometry now;
    return chrome->glass_frame && !chrome->destroyed && glass_geometry(chrome,&now) &&
           now.dark==chrome->glass_completed.dark && now.scale==chrome->glass_completed.scale;
}
static void glass_status(Chrome *chrome, gboolean ready);
static void sync_glass_style(Chrome *chrome) {
    GtkWidget *target=glass_target(chrome);
    if (!target || chrome->destroyed) return;
    const char *name=chrome->dialog_surface ? "meadow-native-glass-dialog" :
        chrome->popup_surface ? "meadow-native-glass-popup" :
        chrome->joined_surface ? "meadow-native-glass-content" : "meadow-native-glass-sidebar";
    gboolean ready=glass_frame_usable(chrome);
    if(chrome->popup_surface)state_class(GTK_WIDGET(chrome->window),chrome->dialog_surface ?
        "meadow-native-glass-dialog-window" : "meadow-native-glass-popup-window",ready);
    if (has_class(target,name) != ready) {
        state_class(target,name,ready);
        gtk_widget_queue_draw(target);
        glass_status(chrome,ready);
    }
}
static void glass_status(Chrome *chrome, gboolean ready) {
    if (chrome->destroyed) return;
    GdkWindow *window=gtk_widget_get_window(GTK_WIDGET(chrome->window));
    if (!window) return;
    gulong values[]={1,ready,chrome->glass_source_count,
                    chrome->glass_completed.width,chrome->glass_completed.height};
    gdk_property_change(window,gdk_atom_intern_static_string("_MEADOW_GLASS_CAPTURE_STATUS"),
        gdk_atom_intern_static_string("CARDINAL"),32,GDK_PROP_MODE_REPLACE,
        (const guchar *)values,G_N_ELEMENTS(values));
    if(chrome->popup_surface) {
        gulong popup[]={1,ready,chrome->popup_radius,chrome->popup_tooltip,
            chrome->glass_source_count,chrome->glass_completed.width,chrome->glass_completed.height};
        gdk_property_change(window,gdk_atom_intern_static_string(chrome->dialog_surface ?
            "_MEADOW_DIALOG_GLASS_STATE" : "_MEADOW_POPUP_GLASS_STATE"),
            gdk_atom_intern_static_string("CARDINAL"),32,GDK_PROP_MODE_REPLACE,
            (const guchar *)popup,G_N_ELEMENTS(popup));
    }
}
static void glass_finish(Chrome *chrome, gboolean success, gboolean broken_pipe) {
    chrome->glass_inflight=FALSE;
    g_clear_pointer(&chrome->glass_request,g_free);
    g_clear_pointer(&chrome->glass_pixels,g_free);
    if (!success) {
        if (chrome->glass_frame) { cairo_surface_destroy(chrome->glass_frame); chrome->glass_frame=NULL; }
        chrome->glass_source_count=0;
        chrome->glass_failures=MIN(chrome->glass_failures+1,5);
        chrome->glass_last_request=g_get_monotonic_time()+((gint64)1 << (chrome->glass_failures-1))*1000000-250000;
    }
    if (success) {chrome->glass_failures=0;chrome->glass_idle_frames=0;}
    if (broken_pipe && chrome->glass_process) {
        if(chrome->popup_surface)retire_bridge(g_steal_pointer(&chrome->glass_process));
        else {g_subprocess_force_exit(chrome->glass_process);g_clear_object(&chrome->glass_process);}
        g_clear_object(&chrome->glass_cancel);
    }
    if (!chrome->destroyed) {
        GtkWidget *target=glass_target(chrome);
        sync_glass_style(chrome);
        if (target) gtk_widget_queue_draw(target);
        glass_status(chrome,glass_frame_matches(chrome));
    }
    chrome_unref(chrome);
}
static void glass_body_done(GObject *source, GAsyncResult *result, gpointer data) {
    Chrome *chrome=data;
    gsize count=0;
    GError *error=NULL;
    gboolean ok=g_input_stream_read_all_finish(G_INPUT_STREAM(source),result,&count,&error);
    g_clear_error(&error);
    const GlassGeometry *geometry=&chrome->glass_requested;
    gsize size=(gsize)geometry->width*geometry->height*4;
    if (!ok || count != size || chrome->destroyed) { glass_finish(chrome,FALSE,TRUE); return; }
    if (chrome->glass_frame) cairo_surface_destroy(chrome->glass_frame);
    chrome->glass_frame=cairo_image_surface_create(CAIRO_FORMAT_ARGB32,geometry->width,geometry->height);
    if (cairo_surface_status(chrome->glass_frame) != CAIRO_STATUS_SUCCESS) { glass_finish(chrome,FALSE,FALSE); return; }
    guint8 *dst=cairo_image_surface_get_data(chrome->glass_frame);
    int stride=cairo_image_surface_get_stride(chrome->glass_frame);
    for (int y=0;y<geometry->height;y++)
        memcpy(dst+(gsize)y*stride,chrome->glass_pixels+(gsize)y*geometry->width*4,(gsize)geometry->width*4);
    cairo_surface_mark_dirty(chrome->glass_frame);
    cairo_surface_set_device_scale(chrome->glass_frame,geometry->scale,geometry->scale);
    chrome->glass_completed=*geometry;
    chrome->glass_source_count=chrome->glass_header[1];
    glass_finish(chrome,TRUE,FALSE);
}
static void glass_header_done(GObject *source, GAsyncResult *result, gpointer data) {
    Chrome *chrome=data;
    gsize count=0;
    GError *error=NULL;
    gboolean ok=g_input_stream_read_all_finish(G_INPUT_STREAM(source),result,&count,&error);
    g_clear_error(&error);
    if (!ok || count != sizeof(chrome->glass_header) || chrome->destroyed) {
        glass_finish(chrome,FALSE,TRUE); return;
    }
    if(ok && count==sizeof(chrome->glass_header) && chrome->glass_header[0]==2 &&
       chrome->glass_frame && chrome->glass_header[2]==(guint)chrome->glass_requested.width &&
       chrome->glass_header[3]==(guint)chrome->glass_requested.height) {
        /* Unchanged underlay: no pixel transfer, image allocation or redraw. */
        chrome->glass_completed=chrome->glass_requested;
        chrome->glass_idle_frames=MIN(chrome->glass_idle_frames+1,2);
        chrome->glass_inflight=FALSE;
        g_clear_pointer(&chrome->glass_request,g_free);
        chrome->glass_failures=0;
        chrome_unref(chrome);return;
    }
    /* A capture failure can leave the worker's Backdrop permanently failed.
     * Reset that owned worker so the next backed-off request can recover. */
    if (chrome->glass_header[0] != 0) { glass_finish(chrome,FALSE,TRUE); return; }
    const GlassGeometry *geometry=&chrome->glass_requested;
    if (chrome->glass_header[2] != (guint)geometry->width ||
        chrome->glass_header[3] != (guint)geometry->height) {
        glass_finish(chrome,FALSE,TRUE); return;
    }
    gsize size=(gsize)geometry->width*geometry->height*4;
    chrome->glass_pixels=g_malloc(size);
    g_input_stream_read_all_async(G_INPUT_STREAM(source),chrome->glass_pixels,size,
        G_PRIORITY_DEFAULT,chrome->glass_cancel,glass_body_done,chrome);
}
static void glass_written(GObject *source, GAsyncResult *result, gpointer data) {
    Chrome *chrome=data;
    gsize count=0;
    GError *error=NULL;
    gboolean ok=g_output_stream_write_all_finish(G_OUTPUT_STREAM(source),result,&count,&error);
    g_clear_error(&error);
    if (!ok || count != strlen(chrome->glass_request) || chrome->destroyed) {
        glass_finish(chrome,FALSE,TRUE); return;
    }
    g_input_stream_read_all_async(g_subprocess_get_stdout_pipe(chrome->glass_process),
        chrome->glass_header,sizeof(chrome->glass_header),G_PRIORITY_DEFAULT,
        chrome->glass_cancel,glass_header_done,chrome);
}
static void request_glass(Chrome *chrome, gint64 now) {
    /* Dialog hover chrome shares its original content's per-map material.
     * Do not create a second backdrop worker for the hover wrapper. */
    if(chrome->dialog_chrome)return;
    sync_glass_style(chrome);
    if (chrome->glass_inflight && now-chrome->glass_last_request > 3000000) {
        g_cancellable_cancel(chrome->glass_cancel);
        if (chrome->glass_process && !chrome->popup_surface) g_subprocess_force_exit(chrome->glass_process);
    }
    GtkWidget *target=glass_target(chrome);
    if (!target || chrome->glass_inflight || now-chrome->glass_last_request < (250000 << chrome->glass_idle_frames) ||
        !gtk_widget_get_mapped(target)) return;
    const char *bridge=g_getenv("MEADOW_GLASS_BRIDGE_PATH");
    if (!bridge || !*bridge) return;
    GdkWindow *window=gtk_widget_get_window(GTK_WIDGET(chrome->window));
    if (!window || !GDK_IS_X11_WINDOW(window) || chrome->iconified ||
        (gdk_window_get_state(window) & GDK_WINDOW_STATE_ICONIFIED)) return;
    GlassGeometry geometry;
    if (!glass_geometry(chrome,&geometry)) return;
    gboolean changed=memcmp(&geometry,&chrome->observed,sizeof(geometry))!=0;
    if(changed) {chrome->observed=geometry;chrome->geometry_changed=now;}
    /* During a WM grab reuse the last material; file/text painting stays live.
     * Publish one exact backdrop when movement settles, rather than compete
     * with every resize allocation for CPU and X server bandwidth. */
    GdkSeat *seat=gdk_display_get_default_seat(gdk_window_get_display(window));
    GdkModifierType buttons=0;
    if(seat)gdk_window_get_device_position(window,gdk_seat_get_pointer(seat),NULL,NULL,&buttons);
    if(!chrome->popup_surface && chrome->glass_frame &&
       ((buttons&GDK_BUTTON1_MASK) || now-chrome->geometry_changed<120000))return;
    /* Files has one captured surface: the lighter sidebar ends at a straight
     * join, then the denser document material continues to the outside edge.
     * Only the existing whole-window clip rounds the outer perimeter. */
    chrome->glass_last_request=now;
    if (!chrome->glass_process) {
        GError *error=NULL;
        GSubprocessLauncher *launcher=g_subprocess_launcher_new(G_SUBPROCESS_FLAGS_STDIN_PIPE|G_SUBPROCESS_FLAGS_STDOUT_PIPE|G_SUBPROCESS_FLAGS_STDERR_SILENCE);
        g_subprocess_launcher_setenv(launcher,"MEADOW_GLASS_DEDUPLICATE","1",TRUE);
        chrome->glass_process=g_subprocess_launcher_spawn(launcher,&error,"python3",bridge,"--display",gdk_display_get_name(gdk_window_get_display(window)),NULL);
        g_object_unref(launcher);
        if (!chrome->glass_process) { g_clear_error(&error); return; }
    }
    if (!chrome->glass_cancel) chrome->glass_cancel=g_cancellable_new();
    chrome->glass_requested=geometry;
    chrome->glass_inflight=TRUE;
    if(chrome->popup_surface)
        chrome->glass_request=g_strdup_printf("0x%lx %d %d %d %d %d %s%s\n",
            gdk_x11_window_get_xid(window),geometry.x,geometry.y,geometry.width,geometry.height,
            chrome->popup_radius*geometry.scale,chrome->dialog_surface?"dialog":
            chrome->popup_tooltip?"tooltip":"popup",geometry.dark?"-dark":"");
    else if (chrome->joined_surface)
        chrome->glass_request=g_strdup_printf("0x%lx %d %d %d %d 0 %s:%d\n",
            gdk_x11_window_get_xid(window),geometry.x,geometry.y,geometry.width,geometry.height,
            geometry.dark ? "split-dark" : "split",geometry.split);
    else
        chrome->glass_request=g_strdup_printf("0x%lx %d %d %d %d %d sidebar\n",
            gdk_x11_window_get_xid(window),geometry.x,geometry.y,geometry.width,geometry.height,
            CORNER_RADIUS*geometry.scale);
    g_output_stream_write_all_async(g_subprocess_get_stdin_pipe(chrome->glass_process),
        chrome->glass_request,strlen(chrome->glass_request),G_PRIORITY_DEFAULT,
        chrome->glass_cancel,glass_written,chrome_ref(chrome));
}
static gboolean draw_glass(GtkWidget *widget, cairo_t *cr, gpointer data) {
    Chrome *chrome=data;
    if (!glass_frame_usable(chrome)) return FALSE;
    cairo_save(cr);
    cairo_rectangle(cr,0,0,gtk_widget_get_allocated_width(widget),gtk_widget_get_allocated_height(widget));
    cairo_clip(cr);
    cairo_scale(cr,(double)gtk_widget_get_allocated_width(widget)*chrome->glass_completed.scale/chrome->glass_completed.width,
                   (double)gtk_widget_get_allocated_height(widget)*chrome->glass_completed.scale/chrome->glass_completed.height);
    cairo_set_source_surface(cr,chrome->glass_frame,0,0);
    cairo_paint(cr);
    cairo_restore(cr);
    return FALSE; /* GTK still draws the real foreground controls. */
}

/* Popups and dialogs keep their original GTK trees and action handlers. Their
 * separately owned content material reuses the async frame pipeline, without
 * replacing menu grabs, dialog responses or native header/titlebar controls. */
#define POPUP_KEY "meadow-popup-material-state"
#define POPUP_LIMIT 8
typedef struct {
    GtkWidget *window;
    Chrome *capture;
    guint timer;
    gboolean tooltip,combo,dialog,destroyed;
    gint radius;
} PopupMaterial;
typedef struct {GSubprocess *process;guint timeout;} RetiredBridge;
static guint active_popups;

static GtkWidget *popup_content(GtkWidget *window) {
    Chrome *chrome=g_object_get_data(G_OBJECT(window),STATE_KEY);
    return chrome && chrome->dialog_chrome ? chrome->content : gtk_bin_get_child(GTK_BIN(window));
}

static gboolean retire_timeout(gpointer data) {
    RetiredBridge *retired=data;retired->timeout=0;
    g_subprocess_force_exit(retired->process);return G_SOURCE_REMOVE;
}
static void retire_done(GObject *source,GAsyncResult *result,gpointer data) {
    RetiredBridge *retired=data;
    g_subprocess_wait_finish(G_SUBPROCESS(source),result,NULL);
    if(retired->timeout)g_source_remove(retired->timeout);
    g_object_unref(retired->process);g_free(retired);
}
static void retire_bridge(GSubprocess *process) {
    if(!process)return;
    RetiredBridge *retired=g_new0(RetiredBridge,1);retired->process=process;
    g_subprocess_send_signal(process,SIGTERM);
    retired->timeout=g_timeout_add(4000,retire_timeout,retired);
    g_subprocess_wait_async(process,NULL,retire_done,retired);
}
static gboolean popup_kind(GtkWidget *widget,gboolean *tooltip,gboolean *combo,gboolean *dialog) {
    if(!GTK_IS_WINDOW(widget))return FALSE;
    GtkWidget *child=popup_content(widget);if(!child)return FALSE;
    /* XEmbed panels have no inner window frame. Their material samples below
     * the host's top-level frame, so neither host nor panel captures itself. */
    if(GTK_IS_PLUG(widget) && settings_process()){*dialog=TRUE;return TRUE;}
    GdkWindowTypeHint hint=gtk_window_get_type_hint(GTK_WINDOW(widget));
    if(gtk_window_get_window_type(GTK_WINDOW(widget))==GTK_WINDOW_TOPLEVEL) {
        *dialog=GTK_IS_DIALOG(widget) || (hint==GDK_WINDOW_TYPE_HINT_DIALOG &&
            gtk_window_get_transient_for(GTK_WINDOW(widget))!=NULL);
        return *dialog;
    }
    *tooltip=hint==GDK_WINDOW_TYPE_HINT_TOOLTIP || g_str_equal(G_OBJECT_TYPE_NAME(widget),"GtkTooltipWindow");
    *combo=hint==GDK_WINDOW_TYPE_HINT_COMBO;
    return *tooltip || *combo || GTK_IS_MENU(child) || hint==GDK_WINDOW_TYPE_HINT_POPUP_MENU ||
        hint==GDK_WINDOW_TYPE_HINT_DROPDOWN_MENU || hint==GDK_WINDOW_TYPE_HINT_MENU;
}
static void popup_shape(PopupMaterial *popup) {
    /* Dialogs retain their native titlebar/decorations and complete geometry. */
    if(popup->dialog)return;
    GtkWidget *child=popup_content(popup->window);
    GdkWindow *window=gtk_widget_get_window(popup->window);gint x=0,y=0;
    if(!child || !window || !gtk_widget_translate_coordinates(child,popup->window,0,0,&x,&y))return;
    gint width=gtk_widget_get_allocated_width(child),height=gtk_widget_get_allocated_height(child);
    if(width<2 || height<2)return;
    double radius=MIN(popup->radius,MIN(width,height)/2.0);
    cairo_region_t *region=cairo_region_create();
    for(gint row=0;row<height;row++) {
        gint inset=0;
        if(row<radius || row>=height-radius) {
            double dy=row<radius?radius-row-.5:row-(height-radius)+.5;
            inset=(gint)ceil(radius-sqrt(MAX(0.0,radius*radius-dy*dy)));
        }
        cairo_rectangle_int_t rect={x+inset,y+row,MAX(0,width-2*inset),1};
        cairo_region_union_rectangle(region,&rect);
    }
    gdk_window_shape_combine_region(window,region,0,0);
    /* Preserve GTK's existing input shape, especially tooltip pass-through. */
    cairo_region_destroy(region);
}
static void popup_position(PopupMaterial *popup) {
    if(!popup->capture)return;
    GdkWindow *window=gtk_widget_get_window(popup->window);
    if(window)gdk_window_get_origin(window,&popup->capture->popup_root_x,&popup->capture->popup_root_y);
}
static void popup_stop(PopupMaterial *popup) {
    if(popup->timer){g_source_remove(popup->timer);popup->timer=0;}
    Chrome *capture=g_steal_pointer(&popup->capture);
    state_class(popup->window,popup->dialog?"meadow-native-glass-dialog-window":
        "meadow-native-glass-popup-window",FALSE);
    GtkWidget *child=capture ? capture->content : popup_content(popup->window);
    if(child)state_class(child,popup->dialog?"meadow-native-glass-dialog":
        "meadow-native-glass-popup",FALSE);
    if(!capture)return;
    glass_status(capture,FALSE);
    active_popups--;capture->destroyed=TRUE;
    if(capture->content) {
        g_signal_handlers_disconnect_by_data(capture->content,capture);
        g_object_remove_weak_pointer(G_OBJECT(capture->content),(gpointer *)&capture->content);
        capture->content=NULL;
    }
    if(capture->glass_cancel)g_cancellable_cancel(capture->glass_cancel);
    retire_bridge(g_steal_pointer(&capture->glass_process));
    chrome_unref(capture);
}
static gboolean popup_tick(gpointer data) {
    PopupMaterial *popup=data;
    GdkWindow *window=gtk_widget_get_window(popup->window);
    if(popup->destroyed || !gtk_widget_get_mapped(popup->window) ||
        (window && (gdk_window_get_state(window)&GDK_WINDOW_STATE_ICONIFIED))) {
        popup->timer=0;popup_stop(popup);return G_SOURCE_REMOVE;
    }
    if(popup->capture)request_glass(popup->capture,g_get_monotonic_time());
    return G_SOURCE_CONTINUE;
}
static void popup_start(GtkWidget *widget,gpointer data) {
    PopupMaterial *popup=data;
    if(popup->destroyed || popup->capture || !gtk_widget_get_mapped(widget) || active_popups>=POPUP_LIMIT)return;
    GtkWidget *child=popup_content(widget);
    GdkWindow *window=gtk_widget_get_window(widget);
    if(!child || !window || !GDK_IS_X11_WINDOW(window))return;
    if(gdk_window_get_state(window)&GDK_WINDOW_STATE_ICONIFIED)return;
    /* popup_kind already admitted only native dialogs, embedded settings or
     * real menu/combo/tooltip hints. A NORMAL-only recheck would reject every
     * menu and tooltip here before it could receive its material. */
    GdkVisual *rgba=gdk_screen_get_rgba_visual(gtk_widget_get_screen(widget));
    /* Dialog content is a captured opaque material inside the native frame;
     * it does not need an alpha-capable outer window or a forced re-realize. */
    if(!popup->dialog && (!rgba || gtk_widget_get_visual(widget)!=rgba))return;
    Chrome *capture=g_new0(Chrome,1);capture->refs=1;capture->window=GTK_WINDOW(widget);
    capture->content=child;capture->popup_surface=TRUE;capture->popup_tooltip=popup->tooltip;
    capture->dialog_surface=popup->dialog;
    capture->popup_radius=popup->radius;popup->capture=capture;active_popups++;
    glass_status(capture,FALSE);
    g_object_add_weak_pointer(G_OBJECT(child),(gpointer *)&capture->content);
    g_signal_connect(child,"draw",G_CALLBACK(draw_glass),capture);
    popup_position(popup);popup_shape(popup);
    popup->timer=g_timeout_add(80,popup_tick,popup);
    request_glass(capture,g_get_monotonic_time());
}
static void popup_unmap(GtkWidget *widget,gpointer data) {(void)widget;popup_stop(data);}
static void popup_destroy(GtkWidget *widget,gpointer data) {
    (void)widget;PopupMaterial *popup=data;popup->destroyed=TRUE;popup_stop(popup);
}
static void popup_free(gpointer data) {g_free(data);}
static gboolean popup_configure(GtkWidget *widget,GdkEventConfigure *event,gpointer data) {
    (void)widget;(void)event;PopupMaterial *popup=data;
    popup_position(popup);popup_shape(popup);
    if(popup->capture)sync_glass_style(popup->capture);
    return FALSE;
}
static void popup_size(GtkWidget *widget,GtkAllocation *allocation,gpointer data) {
    (void)widget;(void)allocation;PopupMaterial *popup=data;popup_shape(popup);
    if(popup->capture)sync_glass_style(popup->capture);
}
static gboolean popup_window_state(GtkWidget *widget,GdkEventWindowState *event,gpointer data) {
    PopupMaterial *popup=data;
    if(event->new_window_state&GDK_WINDOW_STATE_ICONIFIED)popup_stop(popup);
    else if(gtk_widget_get_mapped(widget))popup_start(widget,popup);
    return FALSE;
}
static void adapt_popup(GtkWidget *widget) {
    gboolean tooltip=FALSE,combo=FALSE,dialog=FALSE;
    if(!popup_kind(widget,&tooltip,&combo,&dialog) || g_object_get_data(G_OBJECT(widget),POPUP_KEY) ||
        has_class(widget,OWNER_CLASS) || g_strcmp0(g_getenv("MEADOW_WINDOW_EFFECTS"),"0")==0)return;
    GtkWidget *child=popup_content(widget);
    /* The Settings host supplies its common 24px inset. Some vendor panels
     * retain a GtkContainer border from their old standalone dialog (WM:6px),
     * exposing an opaque GtkPlug rim outside the captured content. */
    if(GTK_IS_PLUG(widget) && settings_process() && GTK_IS_CONTAINER(child))
        gtk_container_set_border_width(GTK_CONTAINER(child),0);
    PopupMaterial *popup=g_new0(PopupMaterial,1);popup->window=widget;
    popup->tooltip=tooltip;popup->combo=combo;popup->dialog=dialog;popup->radius=dialog?0:tooltip?8:12;
    g_object_set_data_full(G_OBJECT(widget),POPUP_KEY,popup,popup_free);
    add_class(widget,dialog?"meadow-dialog-surface":"meadow-popup-surface");
    add_class(child,dialog?"meadow-dialog-content":"meadow-popup-content");
    if(!dialog)add_class(widget,tooltip?"meadow-popup-tooltip":combo?"meadow-popup-combo":"meadow-popup-menu");
    if(!dialog && !gtk_widget_get_realized(widget)) {
        GdkVisual *rgba=gdk_screen_get_rgba_visual(gtk_widget_get_screen(widget));
        if(rgba)gtk_widget_set_visual(widget,rgba);
    }
    gtk_style_context_add_provider_for_screen(gtk_widget_get_screen(widget),GTK_STYLE_PROVIDER(css),
                                              GTK_STYLE_PROVIDER_PRIORITY_APPLICATION+1);
    g_signal_connect_after(widget,"map",G_CALLBACK(popup_start),popup);
    g_signal_connect(widget,"unmap",G_CALLBACK(popup_unmap),popup);
    g_signal_connect(widget,"destroy",G_CALLBACK(popup_destroy),popup);
    g_signal_connect(widget,"configure-event",G_CALLBACK(popup_configure),popup);
    g_signal_connect(widget,"window-state-event",G_CALLBACK(popup_window_state),popup);
    g_signal_connect_after(widget,"size-allocate",G_CALLBACK(popup_size),popup);
    if(gtk_widget_get_mapped(widget))popup_start(widget,popup);
}

static gboolean popup_show_hook(GSignalInvocationHint *hint,guint count,const GValue *values,gpointer data) {
    (void)hint;(void)data;
    if(!count || g_strcmp0(g_getenv("MEADOW_WINDOW_EFFECTS"),"0")==0)return TRUE;
    GtkWidget *widget=g_value_get_object(&values[0]);
    if(GTK_IS_POPOVER(widget))add_class(widget,"meadow-popup-popover");
    else if(GTK_IS_WINDOW(widget)){tag_settings_surface(widget);adapt_popup(widget);}
    return TRUE;
}

static gboolean in_header(Chrome *chrome, GtkWidget *widget) {
    return chrome->header && widget && (widget == chrome->header ||
                      gtk_widget_is_ancestor(widget, chrome->header));
}

static gboolean poll_pointer(gpointer unused) {
    (void)unused;
    GdkDisplay *display = gdk_display_get_default();
    GdkSeat *seat = gdk_display_get_default_seat(display);
    GdkDevice *pointer = gdk_seat_get_pointer(seat);
    gint px, py;
    GdkScreen *screen;
    gdk_device_get_position(pointer, &screen, &px, &py);
    GdkWindow *under = gdk_device_get_window_at_position(pointer, NULL, NULL);
    GdkWindow *top = under ? gdk_window_get_toplevel(under) : NULL;
    gint64 now = g_get_monotonic_time();
    for (GList *item=windows; item; item=item->next) {
        Chrome *chrome = item->data;
        if (!chrome->header || !gtk_widget_get_mapped(GTK_WIDGET(chrome->window))) continue;
        GdkWindow *window = gtk_widget_get_window(GTK_WIDGET(chrome->window));
        request_glass(chrome,now);
        gint wx, wy;
        gdk_window_get_origin(window, &wx, &wy);
        int width = gtk_widget_get_allocated_width(GTK_WIDGET(chrome->window));
        gboolean over = top == window && screen == gtk_widget_get_screen(GTK_WIDGET(chrome->window))
                         && px >= wx && px < wx+width;
        int height=gtk_widget_get_allocated_height(GTK_WIDGET(chrome->window));
        gboolean inside = over && py >= wy && py < wy+height;
        GdkModifierType mask = 0;
        gdk_window_get_device_position(window, pointer, NULL, NULL, &mask);
        gboolean pressed = (mask & (GDK_BUTTON1_MASK|GDK_BUTTON2_MASK|GDK_BUTTON3_MASK)) != 0;
        gboolean focused = in_header(chrome, gtk_window_get_focus(chrome->window));
        if (inside || (chrome->visible && (pressed || focused || chrome->keyboard))) {
            set_visible(chrome, TRUE);
        } else if (chrome->visible) {
            if (!chrome->leave_time) chrome->leave_time=now;
            else if (now-chrome->leave_time >= HIDE_DELAY_US) set_visible(chrome, FALSE);
        }
    }
    return G_SOURCE_CONTINUE;
}

static gboolean on_key(GtkWidget *widget, GdkEventKey *event, gpointer data) {
    (void)widget;
    Chrome *chrome=data;
    if(!chrome->header)return FALSE;
    GdkModifierType modifiers = event->state & gtk_accelerator_get_default_mod_mask();
    if(chrome->joined_surface && (event->state&(GDK_SUPER_MASK|GDK_META_MASK|GDK_MOD4_MASK|GDK_CONTROL_MASK)) &&
       !(event->state&GDK_MOD1_MASK)) {
        const char *action=NULL;
        if(event->keyval==GDK_KEY_plus || event->keyval==GDK_KEY_equal || event->keyval==GDK_KEY_KP_Add)action="zoom-in";
        if(event->keyval==GDK_KEY_minus || event->keyval==GDK_KEY_KP_Subtract)action="zoom-out";
        if(action) {gboolean handled=FALSE;g_signal_emit_by_name(chrome->window,action,&handled);return handled;}
    }
    if (event->keyval == GDK_KEY_space && modifiers == (GDK_CONTROL_MASK|GDK_MOD1_MASK)) {
        chrome->keyboard=TRUE;
        set_visible(chrome, TRUE);
        gtk_widget_child_focus(chrome->header, GTK_DIR_TAB_FORWARD);
        return TRUE;
    }
    if (event->keyval == GDK_KEY_Escape && chrome->keyboard) {
        chrome->keyboard=FALSE;
        gtk_widget_child_focus(chrome->content, GTK_DIR_TAB_FORWARD);
        set_visible(chrome, FALSE);
        return !chrome->dialog_chrome;
    }
    return FALSE;
}

static gboolean on_touch(GtkWidget *widget, GdkEventTouch *event, gpointer data) {
    (void)widget;
    Chrome *chrome=data;
    if (event->type != GDK_TOUCH_BEGIN) return FALSE;
    chrome->keyboard=FALSE;
    gint x, y;
    gdk_window_get_origin(gtk_widget_get_window(GTK_WIDGET(chrome->window)), &x, &y);
    if (event->y_root-y >= 0 && event->y_root-y < TOP_EDGE) {
        chrome->keyboard=TRUE; /* persists until an outside pointer or Escape */
        set_visible(chrome, TRUE);
        return TRUE;
    }
    return FALSE;
}

static gboolean on_button(GtkWidget *widget, GdkEventButton *event, gpointer data) {
    (void)widget;
    Chrome *chrome=data;
    if(!chrome->header)return FALSE;
    gint x,y;
    gdk_window_get_origin(gtk_widget_get_window(GTK_WIDGET(chrome->window)), &x,&y);
    if (event->y_root-y >= gtk_widget_get_allocated_height(chrome->header)) chrome->keyboard=FALSE;
    return FALSE;
}

static gboolean on_header_button(GtkWidget *widget, GdkEventButton *event, gpointer data) {
    (void)widget;
    Chrome *chrome=data;
    /* Children retain their own presses. This handler runs only for header background. */
    if (event->button != 1) return FALSE;
    if (event->type == GDK_2BUTTON_PRESS) {
        if(!gtk_window_get_resizable(chrome->window))return TRUE;
        if (gtk_window_is_maximized(chrome->window)) gtk_window_unmaximize(chrome->window);
        else gtk_window_maximize(chrome->window);
    } else if (event->type == GDK_BUTTON_PRESS) {
        gtk_window_begin_move_drag(chrome->window, event->button,
                                   (gint)event->x_root, (gint)event->y_root, event->time);
    }
    return TRUE;
}

static gboolean on_resize_press(GtkWidget *widget, GdkEventButton *event, gpointer data) {
    (void)widget;
    Chrome *chrome=data;
    if (event->button != 1 || event->type != GDK_BUTTON_PRESS ||
        !gtk_window_get_resizable(chrome->window) || chrome->square) return FALSE;
    gint wx,wy;
    gdk_window_get_origin(gtk_widget_get_window(GTK_WIDGET(chrome->window)),&wx,&wy);
    gint x=(gint)event->x_root-wx, y=(gint)event->y_root-wy;
    gint width=gtk_widget_get_allocated_width(GTK_WIDGET(chrome->window));
    gint height=gtk_widget_get_allocated_height(GTK_WIDGET(chrome->window));
    gboolean west=x < CORNER_RADIUS, east=x >= width-CORNER_RADIUS;
    gboolean north=y < CORNER_RADIUS, south=y >= height-CORNER_RADIUS;
    GdkWindowEdge edge;
    if (north) edge=west ? GDK_WINDOW_EDGE_NORTH_WEST : east ? GDK_WINDOW_EDGE_NORTH_EAST : GDK_WINDOW_EDGE_NORTH;
    else if (south) edge=west ? GDK_WINDOW_EDGE_SOUTH_WEST : east ? GDK_WINDOW_EDGE_SOUTH_EAST : GDK_WINDOW_EDGE_SOUTH;
    else edge=west ? GDK_WINDOW_EDGE_WEST : GDK_WINDOW_EDGE_EAST;
    gtk_window_begin_resize_drag(chrome->window,edge,event->button,
                                 (gint)event->x_root,(gint)event->y_root,event->time);
    return TRUE;
}
static gboolean on_resize_cursor(GtkWidget *widget,GdkEventCrossing *event,gpointer data) {
    Chrome *chrome=data;
    if(!event->window)return FALSE;
    if(event->type==GDK_LEAVE_NOTIFY) {gdk_window_set_cursor(event->window,NULL);return FALSE;}
    gint wx,wy;gdk_window_get_origin(gtk_widget_get_window(GTK_WIDGET(chrome->window)),&wx,&wy);
    gint x=(gint)event->x_root-wx,y=(gint)event->y_root-wy;
    gint w=gtk_widget_get_allocated_width(GTK_WIDGET(chrome->window)),h=gtk_widget_get_allocated_height(GTK_WIDGET(chrome->window));
    gboolean west=x<CORNER_RADIUS,east=x>=w-CORNER_RADIUS,north=y<CORNER_RADIUS,south=y>=h-CORNER_RADIUS;
    const char *name=north?(west?"nw-resize":east?"ne-resize":"n-resize"):
        south?(west?"sw-resize":east?"se-resize":"s-resize"):west?"w-resize":"e-resize";
    GdkCursor *cursor=gdk_cursor_new_from_name(gtk_widget_get_display(widget),name);
    gdk_window_set_cursor(event->window,cursor);g_clear_object(&cursor);return FALSE;
}
static void update_resize_edges(Chrome *chrome) {
    gboolean enabled=!chrome->square && gtk_window_get_resizable(chrome->window);
    for (guint i=0;i<G_N_ELEMENTS(chrome->resize_edges);i++)
        if (chrome->resize_edges[i]) gtk_widget_set_visible(chrome->resize_edges[i],enabled);
}
static void on_resizable(GObject *object, GParamSpec *spec, gpointer data) {
    (void)object; (void)spec;
    update_resize_edges(data);
}
static void add_resize_edges(Chrome *chrome) {
    const GtkAlign horizontal[]={GTK_ALIGN_FILL,GTK_ALIGN_FILL,GTK_ALIGN_START,GTK_ALIGN_END};
    const GtkAlign vertical[]={GTK_ALIGN_START,GTK_ALIGN_END,GTK_ALIGN_FILL,GTK_ALIGN_FILL};
    for (guint i=0;i<4;i++) {
        GtkWidget *edge=gtk_event_box_new();
        chrome->resize_edges[i]=edge;
        g_object_add_weak_pointer(G_OBJECT(edge),(gpointer *)&chrome->resize_edges[i]);
        gtk_widget_set_no_show_all(edge,TRUE);
        add_class(edge,"meadow-resize-edge");
        gtk_event_box_set_visible_window(GTK_EVENT_BOX(edge),FALSE);
        gtk_event_box_set_above_child(GTK_EVENT_BOX(edge),TRUE);
        gtk_widget_set_halign(edge,horizontal[i]);
        gtk_widget_set_valign(edge,vertical[i]);
        gtk_widget_set_size_request(edge,i < 2 ? -1 : 4,i < 2 ? 4 : -1);
        gtk_widget_add_events(edge,GDK_BUTTON_PRESS_MASK|GDK_ENTER_NOTIFY_MASK|GDK_LEAVE_NOTIFY_MASK);
        g_signal_connect(edge,"enter-notify-event",G_CALLBACK(on_resize_cursor),chrome);
        g_signal_connect(edge,"leave-notify-event",G_CALLBACK(on_resize_cursor),chrome);
        g_signal_connect(edge,"button-press-event",G_CALLBACK(on_resize_press),chrome);
        gtk_overlay_add_overlay(GTK_OVERLAY(chrome->overlay),edge);
    }
    update_resize_edges(chrome);
}

static gboolean on_state(GtkWidget *widget, GdkEventWindowState *event, gpointer data) {
    (void)widget;
    Chrome *chrome=data;
    chrome->fullscreen = (event->new_window_state&GDK_WINDOW_STATE_FULLSCREEN) != 0;
    chrome->iconified = (event->new_window_state&GDK_WINDOW_STATE_ICONIFIED) != 0;
    chrome->square = (event->new_window_state&(GDK_WINDOW_STATE_FULLSCREEN|GDK_WINDOW_STATE_MAXIMIZED)) != 0;
    state_class(GTK_WIDGET(chrome->window), "meadow-chrome-square", chrome->square);
    if (chrome->fullscreen || chrome->iconified) set_visible(chrome,FALSE);
    update_resize_edges(chrome);
    update_shape(chrome);
    publish_state(chrome);
    return FALSE;
}
static void on_size(GtkWidget *widget, GtkAllocation *allocation, gpointer data) {
    (void)widget; (void)allocation;
    ((Chrome *)data)->glass_idle_frames=0;
    update_shape(data);
    publish_state(data);
}
static void on_focus(GtkWindow *window, GtkWidget *widget, gpointer data) {
    (void)window;
    Chrome *chrome=data;
    if (!in_header(chrome, widget)) chrome->keyboard=FALSE;
    else if(chrome->dialog_chrome) {chrome->keyboard=TRUE;set_visible(chrome,TRUE);}
}
static void on_destroy(GtkWidget *widget, gpointer data) {
    Chrome *chrome=data;
    chrome->destroyed=TRUE;
    /* Destroyed widgets can remain referenced. No later signal may retain
     * this released state, nor a weak pointer write back into its storage. */
    g_signal_handlers_disconnect_by_data(widget,chrome);
    g_object_set_data(G_OBJECT(widget),STATE_KEY,NULL);
    windows=g_list_remove(windows,chrome);
    if(chrome->header) {
        g_signal_handlers_disconnect_by_data(chrome->header,chrome);
        g_object_remove_weak_pointer(G_OBJECT(chrome->header),(gpointer *)&chrome->header);
        chrome->header=NULL;
    }
    for(guint i=0;i<G_N_ELEMENTS(chrome->resize_edges);i++)if(chrome->resize_edges[i]) {
        g_signal_handlers_disconnect_by_data(chrome->resize_edges[i],chrome);
        g_object_remove_weak_pointer(G_OBJECT(chrome->resize_edges[i]),(gpointer *)&chrome->resize_edges[i]);
        chrome->resize_edges[i]=NULL;
    }
    if (chrome->sidebar) {
        g_signal_handlers_disconnect_by_data(chrome->sidebar,chrome);
        g_object_remove_weak_pointer(G_OBJECT(chrome->sidebar),(gpointer *)&chrome->sidebar);
        chrome->sidebar=NULL;
    }
    if (chrome->content) {
        g_signal_handlers_disconnect_by_data(chrome->content,chrome);
        g_object_remove_weak_pointer(G_OBJECT(chrome->content),(gpointer *)&chrome->content);
        chrome->content=NULL;
    }
    g_clear_object(&chrome->move_gesture);
    if (chrome->glass_cancel) g_cancellable_cancel(chrome->glass_cancel);
    if (chrome->glass_process) g_subprocess_force_exit(chrome->glass_process);
    chrome_unref(chrome);
    if (!windows && poll_source) { g_source_remove(poll_source); poll_source=0; }
}

static void tag_sidebar(GtkWidget *widget, gpointer data) {
    Chrome *chrome=data;
    const char *type=G_OBJECT_TYPE_NAME(widget);
    if (!g_strcmp0(type,"ThunarShortcutsPane") || !g_strcmp0(type,"ThunarTreePane") ||
        GTK_IS_PLACES_SIDEBAR(widget)) {
        add_class(widget,"meadow-glass-sidebar");
        if (!chrome->sidebar) {
            chrome->sidebar=widget;
            g_object_add_weak_pointer(G_OBJECT(widget),(gpointer *)&chrome->sidebar);
            if (!chrome->joined_surface) g_signal_connect(widget,"draw",G_CALLBACK(draw_glass),chrome);
        }
    }
    if (GTK_IS_CONTAINER(widget)) gtk_container_forall(GTK_CONTAINER(widget),tag_sidebar,data);
}

/* Reuse Thunar's original widgets and actions. No file model or fake path. */
static GtkWidget *find_widget(GtkWidget *root,const char *type,const char *id) {
    if((type && !g_strcmp0(G_OBJECT_TYPE_NAME(root),type)) ||
       (id && !g_strcmp0(g_object_get_data(G_OBJECT(root),"id"),id)))return root;
    if(!GTK_IS_CONTAINER(root))return NULL;
    GList *children=gtk_container_get_children(GTK_CONTAINER(root));GtkWidget *found=NULL;
    for(GList *item=children;item && !found;item=item->next)found=find_widget(item->data,type,id);
    g_list_free(children);return found;
}
static void zoom_scale_changed(GtkRange *range,gpointer data) {
    gint value=(gint)round(gtk_range_get_value(range));
    g_object_set(data,"zoom-level",value,NULL);
}
static void zoom_property_changed(GObject *window,GParamSpec *spec,gpointer data) {
    (void)spec;gint value=0;g_object_get(window,"zoom-level",&value,NULL);
    g_signal_handlers_block_by_func(data,zoom_scale_changed,window);
    gtk_range_set_value(GTK_RANGE(data),value);
    g_signal_handlers_unblock_by_func(data,zoom_scale_changed,window);
}
static void sync_files_header(GtkWidget *sidebar,GtkAllocation *allocation,gpointer data) {
    Chrome *chrome=data;
    if(chrome->destroyed || !chrome->files_header_left || !chrome->content)return;
    gint x=0,y=0;
    if(!gtk_widget_translate_coordinates(sidebar,chrome->content,0,0,&x,&y))return;
    gint width=MAX(100,x+allocation->width),old=-1;
    gtk_widget_get_size_request(chrome->files_header_left,&old,NULL);
    if(old!=width)gtk_widget_set_size_request(chrome->files_header_left,width,-1);
}
static void split_files_header(Chrome *chrome) {
    GtkWidget *left=chrome->header;
    GList *children=gtk_container_get_children(GTK_CONTAINER(left));
    GtkWidget *right=gtk_box_new(GTK_ORIENTATION_HORIZONTAL,8);
    if(g_getenv("MEADOW_WINDOW_DIAGNOSTICS"))g_printerr("Meadow split header %s\n",G_OBJECT_TYPE_NAME(left));
    guint count=0;
    for(GList *item=children;item;item=item->next)if(GTK_IS_TOOLBAR(item->data)) {
        GtkWidget *toolbar=item->data;
        if(g_getenv("MEADOW_WINDOW_DIAGNOSTICS"))g_printerr("Meadow split toolbar %s parent %s\n",G_OBJECT_TYPE_NAME(toolbar),G_OBJECT_TYPE_NAME(gtk_widget_get_parent(toolbar)));
        g_object_ref(toolbar);
        if(gtk_header_bar_get_custom_title(GTK_HEADER_BAR(left))==toolbar)
            gtk_header_bar_set_custom_title(GTK_HEADER_BAR(left),NULL);
        else gtk_container_remove(GTK_CONTAINER(gtk_widget_get_parent(toolbar)),toolbar);
        if(g_getenv("MEADOW_WINDOW_DIAGNOSTICS"))g_printerr("Meadow detached toolbar parent %p\n",(void*)gtk_widget_get_parent(toolbar));
        gtk_box_pack_start(GTK_BOX(right),toolbar,TRUE,TRUE,0);g_object_unref(toolbar);count++;
    }
    g_list_free(children);
    if(!count) {gtk_widget_destroy(right);return;}
    GtkWidget *row=gtk_box_new(GTK_ORIENTATION_HORIZONTAL,0);
    add_class(row,"meadow-hover-toolbar");add_class(row,"meadow-files-split-toolbar");
    add_class(left,"meadow-files-sidebar-header");add_class(right,"meadow-files-main-toolbar");
    gtk_header_bar_set_title(GTK_HEADER_BAR(left),"");
    gtk_widget_set_hexpand(left,FALSE);gtk_widget_set_size_request(left,190,-1);
    g_object_ref(left);gtk_container_remove(GTK_CONTAINER(gtk_widget_get_parent(left)),left);
    if(g_getenv("MEADOW_WINDOW_DIAGNOSTICS"))g_printerr("Meadow detached left parent %p, toolbars %u\n",(void*)gtk_widget_get_parent(left),count);
    gtk_box_pack_start(GTK_BOX(row),left,FALSE,FALSE,0);g_object_unref(left);
    gtk_box_pack_start(GTK_BOX(row),right,TRUE,TRUE,0);gtk_container_add(GTK_CONTAINER(chrome->revealer),row);
    g_object_remove_weak_pointer(G_OBJECT(left),(gpointer *)&chrome->header);
    chrome->header=row;chrome->files_header_left=left;
    g_object_add_weak_pointer(G_OBJECT(row),(gpointer *)&chrome->header);
    if(chrome->sidebar)g_signal_connect(chrome->sidebar,"size-allocate",G_CALLBACK(sync_files_header),chrome);
    /* Preserve Thunar's hidden toolbar actions and mode switcher. */
    gtk_widget_show(right);gtk_widget_show(row);
}
static void prepare_files(Chrome *chrome) {
    GtkWidget *slot=find_widget(chrome->header,NULL,"location-bar");
    GtkWidget *status=find_widget(chrome->content,"ThunarStatusbar",NULL);
    if(!slot || !GTK_IS_BIN(slot) || !status)return;
    GtkWidget *location=gtk_bin_get_child(GTK_BIN(slot));
    GtkWidget *event_box=gtk_widget_get_parent(status);
    GtkWidget *view_box=gtk_widget_get_parent(event_box);
    if(!location || !GTK_IS_GRID(view_box) || !GTK_IS_BIN(event_box))return;
    gint row=0;gtk_container_child_get(GTK_CONTAINER(view_box),event_box,"top-attach",&row,NULL);
    gtk_grid_insert_row(GTK_GRID(view_box),row);
    GtkWidget *path=gtk_box_new(GTK_ORIENTATION_HORIZONTAL,8);
    add_class(path,"meadow-files-path");
    g_object_ref(location);gtk_container_remove(GTK_CONTAINER(slot),location);
    gtk_widget_set_margin_start(location,0);gtk_widget_set_margin_end(location,0);
    gtk_box_pack_start(GTK_BOX(path),location,TRUE,TRUE,0);g_object_unref(location);
    gtk_grid_attach(GTK_GRID(view_box),path,0,row,1,1);
    gtk_widget_show(path);gtk_widget_show(location);
    gtk_widget_hide(slot);gtk_widget_set_no_show_all(slot,TRUE);
    const char *views[]={"view-as-icons","view-as-detailed-list","view-as-compact-list"};
    for(guint i=0;i<G_N_ELEMENTS(views);i++) {
        GtkWidget *button=find_widget(chrome->header,NULL,views[i]);
        if(button) {add_class(button,"meadow-files-view-button");gtk_widget_show(button);}
    }
    GtkWidget *switcher=find_widget(chrome->header,NULL,"view-switcher");
    if(switcher) {gtk_widget_hide(switcher);gtk_widget_set_no_show_all(switcher,TRUE);}
    /* A real zoom-level binding, not an image scale or copied folder asset. */
    GParamSpec *zoom=g_object_class_find_property(G_OBJECT_GET_CLASS(chrome->window),"zoom-level");
    if(zoom && G_IS_PARAM_SPEC_ENUM(zoom)) {
        GEnumClass *values=G_ENUM_CLASS(g_type_class_ref(G_PARAM_SPEC_VALUE_TYPE(zoom)));
        GtkWidget *scale=gtk_scale_new_with_range(GTK_ORIENTATION_HORIZONTAL,values->minimum,values->maximum,1);
        g_type_class_unref(values);
        gtk_scale_set_draw_value(GTK_SCALE(scale),FALSE);gtk_widget_set_size_request(scale,112,-1);
        gtk_widget_set_tooltip_text(scale,"資料夾與檔案大小（⌘ + / −）");
        gtk_widget_get_accessible(scale);
        atk_object_set_name(gtk_widget_get_accessible(scale),"檔案圖示大小");
        GtkWidget *row_box=gtk_box_new(GTK_ORIENTATION_HORIZONTAL,12);
        add_class(row_box,"meadow-files-status");
        g_object_ref(status);gtk_container_remove(GTK_CONTAINER(event_box),status);
        gtk_box_pack_start(GTK_BOX(row_box),status,TRUE,TRUE,0);g_object_unref(status);
        gtk_box_pack_end(GTK_BOX(row_box),scale,FALSE,FALSE,0);
        gtk_container_add(GTK_CONTAINER(event_box),row_box);gtk_widget_show_all(row_box);
        g_signal_connect_object(scale,"value-changed",G_CALLBACK(zoom_scale_changed),chrome->window,0);
        g_signal_connect_object(chrome->window,"notify::zoom-level",G_CALLBACK(zoom_property_changed),scale,0);
        zoom_property_changed(G_OBJECT(chrome->window),NULL,scale);
    }
    add_class(chrome->header,"meadow-files-toolbar");
    gtk_widget_set_hexpand(chrome->header,TRUE);
    split_files_header(chrome);
}
/* Pick the actual hit widget in owner coordinates. Buttons, entries, documents,
 * rows and splitters retain their own gestures; sidebar blank space may move. */
static GtkWidget *hit_widget(GtkWidget *root,GtkWidget *owner,gint x,gint y) {
    if(!gtk_widget_get_visible(root) || !gtk_widget_get_child_visible(root))return NULL;
    gint rx,ry;if(!gtk_widget_translate_coordinates(root,owner,0,0,&rx,&ry))return NULL;
    if(x<rx || y<ry || x>=rx+gtk_widget_get_allocated_width(root) || y>=ry+gtk_widget_get_allocated_height(root))return NULL;
    if(GTK_IS_CONTAINER(root)) {
        GList *children=gtk_container_get_children(GTK_CONTAINER(root));
        for(GList *item=g_list_last(children);item;item=item->prev) {
            GtkWidget *hit=hit_widget(item->data,owner,x,y);
            if(hit) {g_list_free(children);return hit;}
        }
        g_list_free(children);
    }
    return root;
}
static void long_move(GtkGestureLongPress *gesture,gdouble x,gdouble y,gpointer data) {
    Chrome *chrome=data;GtkWidget *owner=GTK_WIDGET(chrome->window);
    GtkWidget *hit=hit_widget(chrome->layout,owner,(gint)x,(gint)y);
    for(GtkWidget *node=hit;node && node!=owner;node=gtk_widget_get_parent(node)) {
        const char *type=G_OBJECT_TYPE_NAME(node);
        if(GTK_IS_BUTTON(node) || GTK_IS_ENTRY(node) || GTK_IS_TEXT_VIEW(node) || GTK_IS_RANGE(node) ||
           (GTK_IS_PANED(node) && hit==node) || GTK_IS_MENU_SHELL(node) ||
           !g_strcmp0(type,"ExoIconView") || g_str_has_prefix(type,"ThunarStandardView") ||
           !g_strcmp0(type,"ThunarIconView") || !g_strcmp0(type,"ThunarDetailsView") ||
           !g_strcmp0(type,"ThunarCompactView"))return;
        if(GTK_IS_TREE_VIEW(node)) {
            gboolean sidebar=chrome->sidebar && (node==chrome->sidebar || gtk_widget_is_ancestor(node,chrome->sidebar));
            gint nx,ny;if(!sidebar || !gtk_widget_translate_coordinates(owner,node,(gint)x,(gint)y,&nx,&ny))return;
            GtkTreePath *path=NULL;
            if(gtk_tree_view_get_path_at_pos(GTK_TREE_VIEW(node),nx,ny,&path,NULL,NULL,NULL)) {
                gtk_tree_path_free(path);return;
            }
        }
    }
    if(!hit)return;
    const GdkEvent *event=gtk_gesture_get_last_event(GTK_GESTURE(gesture),gtk_gesture_single_get_current_sequence(GTK_GESTURE_SINGLE(gesture)));
    if(!event)return;
    gdouble rx,ry;gdk_event_get_root_coords(event,&rx,&ry);
    gtk_gesture_set_state(GTK_GESTURE(gesture),GTK_EVENT_SEQUENCE_CLAIMED);
    gtk_window_begin_move_drag(chrome->window,1,(gint)rx,(gint)ry,gdk_event_get_time(event));
}

static void prepare_native_header(GtkWidget *widget) {
    if(!GTK_IS_WINDOW(widget) || GTK_IS_PLUG(widget) || gtk_widget_in_destruction(widget) || gtk_widget_get_realized(widget) ||
        gtk_window_get_window_type(GTK_WINDOW(widget))!=GTK_WINDOW_TOPLEVEL ||
        !gtk_window_get_decorated(GTK_WINDOW(widget)) || has_class(widget,OWNER_CLASS) ||
        g_object_get_data(G_OBJECT(widget),STATE_KEY) ||
        g_strcmp0(g_getenv("MEADOW_WINDOW_EFFECTS"),"0")==0)return;
    GdkVisual *rgba=gdk_screen_get_rgba_visual(gtk_widget_get_screen(widget));
    if(rgba)gtk_widget_set_visual(widget,rgba);
    if(gtk_window_get_titlebar(GTK_WINDOW(widget)))return;
    GtkWidget *header=gtk_header_bar_new();
    /* GtkWindow copies the installed titlebar title back to itself. Preserve
     * the application's existing title before installing the native control. */
    gtk_header_bar_set_title(GTK_HEADER_BAR(header),gtk_window_get_title(GTK_WINDOW(widget)));
    gtk_header_bar_set_has_subtitle(GTK_HEADER_BAR(header),FALSE);
    gtk_header_bar_set_show_close_button(GTK_HEADER_BAR(header),TRUE);
    gtk_header_bar_set_decoration_layout(GTK_HEADER_BAR(header),GTK_IS_DIALOG(widget)?"close:":"close,minimize,maximize:");
    add_class(header,"meadow-dialog-native-header");
    gtk_widget_show(header);
    gtk_window_set_titlebar(GTK_WINDOW(widget),header);
}

static void align_native_controls(GtkWidget *widget,gpointer unused) {
    (void)unused;
    if(GTK_IS_BUTTON(widget) && has_class(widget,"titlebutton")) {
        GtkWidget *group=gtk_widget_get_parent(widget);
        if(GTK_IS_BOX(group)) {
            if(gtk_box_get_spacing(GTK_BOX(group))!=0)gtk_box_set_spacing(GTK_BOX(group),0);
            /* GtkHeaderBar's internal box starts one logical pixel before its
             * CSS content edge; align it with Meadow's other native frames. */
            if(gtk_widget_get_margin_start(group)!=1)gtk_widget_set_margin_start(group,1);
        }
        gtk_widget_set_margin_start(widget,0);gtk_widget_set_margin_end(widget,0);
    }
    if(GTK_IS_CONTAINER(widget)) {
        /* GtkHeaderBar's window controls are internal children, omitted by
         * get_children(). They can also be rebuilt after a theme change. */
        gtk_container_forall(GTK_CONTAINER(widget),align_native_controls,NULL);
    }
}
/* Use Exo's content margin rather than GtkWidget margins: scrolling and the
 * joined sidebar boundary keep their native allocations. Newly selected views
 * receive the same spacing on map, without polling or repeated invalidation. */
static void align_sidebar_rows(GtkWidget *widget,gpointer data) {
    (void)data;
    /* A one-pixel inert cell supplies the shared row minimum. GTK ignores
     * zero-width cells during row sizing. Real text
     * renderers keep their natural height, including larger fonts and CJK. */
    gint row_height=0;
    gtk_style_context_get(gtk_widget_get_style_context(widget),GTK_STATE_FLAG_NORMAL,
                          "min-height",&row_height,NULL);
    if(row_height<=0)return;
    GtkTreeViewColumn *column=gtk_tree_view_get_column(GTK_TREE_VIEW(widget),0);
    if(!column)return;
    GtkCellRenderer *spacer=g_object_get_data(G_OBJECT(column),"meadow-sidebar-row-minimum");
    if(!spacer) {
        spacer=gtk_cell_renderer_text_new();
        g_object_set(spacer,"text","","width",1,"height",row_height,"xpad",0,"ypad",0,NULL);
        gtk_tree_view_column_pack_end(column,spacer,FALSE);
        g_object_set_data(G_OBJECT(column),"meadow-sidebar-row-minimum",spacer);
    } else {
        gint current=0;g_object_get(spacer,"height",&current,NULL);
        if(current!=row_height)g_object_set(spacer,"height",row_height,NULL);
    }
    if(!g_object_get_data(G_OBJECT(widget),"meadow-sidebar-style-hook")) {
        g_object_set_data(G_OBJECT(widget),"meadow-sidebar-style-hook",GINT_TO_POINTER(1));
        g_signal_connect(widget,"style-updated",G_CALLBACK(align_sidebar_rows),NULL);
    }
}
static void inset_files_view(GtkWidget *widget) {
    const gchar *type=G_OBJECT_TYPE_NAME(widget);
    gboolean icon=!g_strcmp0(type,"ExoIconView");
    if(!icon && !GTK_IS_TREE_VIEW(widget))return;
    gboolean files=FALSE,details=FALSE,sidebar=FALSE;
    for(GtkWidget *parent=gtk_widget_get_parent(widget);parent;parent=gtk_widget_get_parent(parent)) {
        const gchar *name=G_OBJECT_TYPE_NAME(parent);
        if(!g_strcmp0(name,"ThunarDetailsView"))details=TRUE;
        if(!g_strcmp0(name,"ThunarShortcutsPane") || !g_strcmp0(name,"ThunarTreePane"))sidebar=TRUE;
        if(!g_strcmp0(name,"ThunarWindow")){files=TRUE;break;}
    }
    if(!files)return;
    if(icon) {
        GParamSpec *spec=g_object_class_find_property(G_OBJECT_GET_CLASS(widget),"margin");
        if(spec && spec->owner_type==G_OBJECT_TYPE(widget) && G_IS_PARAM_SPEC_INT(spec)) {
            gint margin=0;g_object_get(widget,"margin",&margin,NULL);
            if(margin!=24)g_object_set(widget,"margin",24,NULL);
        }
    } else if(details)add_class(widget,"meadow-files-details");
    else if(sidebar)align_sidebar_rows(widget,NULL);
}
static void inset_files_descendants(GtkWidget *widget,gpointer data) {
    (void)data;inset_files_view(widget);
    if(GTK_IS_CONTAINER(widget))
        gtk_container_foreach(GTK_CONTAINER(widget),inset_files_descendants,NULL);
}
static gboolean settings_plug_adapt_idle(gpointer data) {
    GtkWidget *plug=data;
    g_object_set_data(G_OBJECT(plug),"meadow-settings-plug-pending",NULL);
    if(!gtk_widget_in_destruction(plug))adapt_popup(plug);
    return G_SOURCE_REMOVE;
}
static gboolean files_view_map_hook(GSignalInvocationHint *hint,guint count,
                                   const GValue *values,gpointer data) {
    (void)hint;(void)data;
    if(count && GTK_IS_WIDGET(g_value_get_object(values))) {
        GtkWidget *widget=GTK_WIDGET(g_value_get_object(values));
        inset_files_view(widget);
        /* Xfce creates and shows its empty GtkPlug before adding the panel.
         * A descendant's first map supplies the original content that was not
         * present during GtkPlug::show. Adapt once, without polling its tree. */
        if(settings_process()) {
            GtkWidget *top=gtk_widget_get_toplevel(widget);
            if(GTK_IS_PLUG(top)) {
                tag_settings_surface(top);
                /* A visible vendor panel maps synchronously inside
                 * gtk_widget_set_parent, before GtkBin stores its child.
                 * Defer once until that native ownership transaction ends. */
                if(!g_object_get_data(G_OBJECT(top),POPUP_KEY) &&
                   !g_object_get_data(G_OBJECT(top),"meadow-settings-plug-pending")) {
                    g_object_set_data(G_OBJECT(top),"meadow-settings-plug-pending",GINT_TO_POINTER(1));
                    g_idle_add_full(G_PRIORITY_DEFAULT_IDLE,settings_plug_adapt_idle,
                                    g_object_ref(top),g_object_unref);
                }
            }
        }
    }
    return TRUE;
}
static gboolean adapt_window(gpointer data) {
    GtkWindow *window=GTK_WINDOW(data);
    GtkWidget *widget=GTK_WIDGET(window);
    gboolean dialog=GTK_IS_DIALOG(widget);
    if (GTK_IS_PLUG(widget) || gtk_widget_in_destruction(widget) || gtk_widget_get_realized(widget) || g_object_get_data(G_OBJECT(window),STATE_KEY) ||
        g_strcmp0(g_getenv("MEADOW_WINDOW_EFFECTS"),"0")==0 ||
        (!dialog && g_object_get_data(G_OBJECT(window),POPUP_KEY)) ||
        has_class(widget,OWNER_CLASS) || has_class(widget,"meadow-hover-window") ||
        gtk_window_get_window_type(window) != GTK_WINDOW_TOPLEVEL ||
        (!dialog && (gtk_window_get_type_hint(window) != GDK_WINDOW_TYPE_HINT_NORMAL ||
                     gtk_window_get_modal(window) || gtk_window_get_transient_for(window))) ||
        !gtk_window_get_decorated(window)) return G_SOURCE_REMOVE;
    GtkWidget *content=gtk_bin_get_child(GTK_BIN(window));
    GtkWidget *header=gtk_window_get_titlebar(window);
    /* Supported GTK3 clients share native controls; non-GTK clients use the WM. */
    if (!content || !header || (!dialog && !GTK_IS_HEADER_BAR(header))) return G_SOURCE_REMOVE;
    Chrome *chrome=g_new0(Chrome,1);
    chrome->refs=1; chrome->window=window; chrome->content=content; chrome->header=header;
    chrome->joined_surface=!g_strcmp0(G_OBJECT_TYPE_NAME(window),"ThunarWindow");
    chrome->dialog_chrome=dialog;
    g_object_add_weak_pointer(G_OBJECT(content),(gpointer *)&chrome->content);
    g_object_add_weak_pointer(G_OBJECT(header),(gpointer *)&chrome->header);
    chrome->overlay=g_object_new(meadow_overlay_get_type(),NULL);
    chrome->revealer=gtk_revealer_new();
    chrome->layout=gtk_box_new(GTK_ORIENTATION_VERTICAL,0);
    g_object_set_data(G_OBJECT(window),STATE_KEY,chrome);
    g_object_ref(content); g_object_ref(header);
    GtkWidget *saved_default=dialog?gtk_window_get_default_widget(window):NULL;
    GtkWidget *saved_focus=dialog?gtk_window_get_focus(window):NULL;
    if(saved_default)g_object_ref(saved_default);
    if(saved_focus)g_object_ref(saved_focus);
    /* Thunar 4.20 destroys its notebooks from the window's `remove` signal.
     * A presentation-only reparent must not run application teardown. Block
     * connected handlers only for this synchronous transaction; GtkContainer's
     * class closure still performs the real remove, and later teardown remains
     * unchanged. A pre-existing handler block count is preserved. */
    guint remove_signal=g_signal_lookup("remove",GTK_TYPE_CONTAINER);
    g_signal_handlers_block_matched(window,G_SIGNAL_MATCH_ID,remove_signal,0,NULL,NULL,NULL);
    gtk_container_remove(GTK_CONTAINER(window),content);
    gtk_window_set_titlebar(window,NULL);
    gtk_window_set_decorated(window,FALSE);
    gtk_container_add(GTK_CONTAINER(window),chrome->overlay);
    gtk_container_add(GTK_CONTAINER(chrome->overlay),chrome->layout);
    gtk_box_pack_start(GTK_BOX(chrome->layout),chrome->revealer,FALSE,FALSE,0);
    gtk_box_pack_start(GTK_BOX(chrome->layout),content,TRUE,TRUE,0);
    g_signal_handlers_unblock_matched(window,G_SIGNAL_MATCH_ID,remove_signal,0,NULL,NULL,NULL);
    gtk_container_add(GTK_CONTAINER(chrome->revealer),header);
    if(GTK_IS_HEADER_BAR(header) && !chrome->joined_surface)
        g_object_bind_property(window,"title",header,"title",G_BINDING_SYNC_CREATE|G_BINDING_BIDIRECTIONAL);
    gtk_widget_set_halign(chrome->revealer,GTK_ALIGN_FILL);
    gtk_widget_set_valign(chrome->revealer,GTK_ALIGN_START);
    gtk_revealer_set_transition_type(GTK_REVEALER(chrome->revealer),GTK_REVEALER_TRANSITION_TYPE_SLIDE_DOWN);
    gtk_revealer_set_transition_duration(GTK_REVEALER(chrome->revealer),140);
    gtk_revealer_set_reveal_child(GTK_REVEALER(chrome->revealer),dialog);
    if(dialog)gtk_widget_set_opacity(chrome->revealer,0.0);
    /* The only overlay children are native resize hit strips. */
    /* Unparenting clears these GtkWindow assignments despite retaining the
     * original action widgets. Restore only surviving descendants. */
    if(saved_default && !gtk_widget_in_destruction(saved_default) &&
        gtk_widget_is_ancestor(saved_default,widget) && gtk_widget_get_can_default(saved_default))
        gtk_window_set_default(window,saved_default);
    if(saved_focus && !gtk_widget_in_destruction(saved_focus) &&
        gtk_widget_is_ancestor(saved_focus,widget) && gtk_widget_get_can_focus(saved_focus))
        gtk_window_set_focus(window,saved_focus);
    g_clear_object(&saved_default);g_clear_object(&saved_focus);
    add_class(widget,"meadow-hover-window");
    add_class(content,"meadow-hover-content");
    add_class(header,"meadow-hover-toolbar");
    add_class(header,"titlebar");
    align_native_controls(header,NULL);
    g_signal_connect_after(header,"map",G_CALLBACK(align_native_controls),NULL);
    g_signal_connect_after(header,"style-updated",G_CALLBACK(align_native_controls),NULL);
    add_class(chrome->overlay,"meadow-hover-surface");
    if(!dialog)tag_sidebar(content,chrome);
    if(chrome->joined_surface) {
        prepare_files(chrome);
        inset_files_descendants(content,NULL);
    }
    if(header!=chrome->header) {
        g_object_ref(chrome->header);g_object_unref(header);header=chrome->header;
    }
    if (chrome->joined_surface) g_signal_connect(content,"draw",G_CALLBACK(draw_glass),chrome);
    gtk_style_context_add_provider_for_screen(gtk_widget_get_screen(widget),GTK_STYLE_PROVIDER(css),
                                              GTK_STYLE_PROVIDER_PRIORITY_APPLICATION+1);
    gtk_widget_add_events(widget,GDK_TOUCH_MASK|GDK_BUTTON_PRESS_MASK);
    gtk_widget_add_events(header,GDK_BUTTON_PRESS_MASK);
    g_signal_connect(widget,"key-press-event",G_CALLBACK(on_key),chrome);
    g_signal_connect(widget,"touch-event",G_CALLBACK(on_touch),chrome);
    g_signal_connect(widget,"button-press-event",G_CALLBACK(on_button),chrome);
    g_signal_connect(header,"button-press-event",G_CALLBACK(on_header_button),chrome);
    g_signal_connect(widget,"window-state-event",G_CALLBACK(on_state),chrome);
    g_signal_connect(window,"notify::resizable",G_CALLBACK(on_resizable),chrome);
    g_signal_connect(widget,"size-allocate",G_CALLBACK(on_size),chrome);
    g_signal_connect(window,"set-focus",G_CALLBACK(on_focus),chrome);
    g_signal_connect(widget,"destroy",G_CALLBACK(on_destroy),chrome);
    add_resize_edges(chrome);
    if(chrome->joined_surface) {
        chrome->move_gesture=gtk_gesture_long_press_new(widget);
        gtk_gesture_single_set_button(GTK_GESTURE_SINGLE(chrome->move_gesture),1);
        gtk_event_controller_set_propagation_phase(GTK_EVENT_CONTROLLER(chrome->move_gesture),GTK_PHASE_CAPTURE);
        g_signal_connect(chrome->move_gesture,"pressed",G_CALLBACK(long_move),chrome);
    }
    /* Never recursively show application content. Thunar deliberately hides
     * its menu/status/preview widgets according to saved user preferences. */
    gtk_widget_show(chrome->overlay);
    gtk_widget_show(chrome->layout);
    gtk_widget_show(chrome->revealer);
    gtk_widget_show(header);
    if(!dialog)gtk_widget_set_sensitive(header,FALSE);

    if(dialog)on_focus(window,gtk_window_get_focus(window),chrome);
    g_object_unref(header); g_object_unref(content);
    windows=g_list_prepend(windows,chrome);
    if (!poll_source) poll_source=g_timeout_add(40,poll_pointer,NULL);
    update_shape(chrome); publish_state(chrome);
    return G_SOURCE_REMOVE;
}

/* GTK's global show signal hook runs after GtkWindow::show has realized the
 * window. Adapt before realization by chaining this process-local class vfunc.
 * GtkWindow subclasses that override show must chain GtkWindow::show, as GTK
 * requires. Already-realized windows are deliberately left untouched. */
static void (*original_window_show)(GtkWidget *);
static void (*original_window_realize)(GtkWidget *);
static void window_realize(GtkWidget *widget) {
    tag_settings_surface(widget);
    if(GTK_IS_DIALOG(widget)) {prepare_native_header(widget);adapt_window(widget);}
    adapt_popup(widget);
    original_window_realize(widget);
}
static void window_show(GtkWidget *widget) {
    tag_settings_surface(widget);
    prepare_native_header(widget);
    adapt_window(widget);
    adapt_popup(widget);
    original_window_show(widget);
}

G_MODULE_EXPORT void gtk_module_init(gint *argc, gchar ***argv) {
    (void)argc; (void)argv;
    static gboolean initialized=FALSE;
    if (initialized) return;
    initialized=TRUE;
    css=gtk_css_provider_new();
    gtk_css_provider_load_from_data(css,
        ".meadow-hover-window { background-color: transparent; border-radius: 16px; }"
        ".meadow-hover-window decoration { background: transparent; border: 0; box-shadow: none; }"
        ".meadow-native-glass-sidebar, .meadow-native-glass-sidebar scrolledwindow,"
        ".meadow-native-glass-sidebar viewport, .meadow-native-glass-sidebar treeview.view {"
        " background-color: transparent; background-image: none; }"
        /* Clear only Files' content backing after a complete capture arrives.
         * Selected native cells and popup menus retain their theme materials. */
        ".meadow-native-glass-content, .meadow-native-glass-content box,"
        ".meadow-native-glass-content paned, .meadow-native-glass-content notebook,"
        ".meadow-native-glass-content notebook > stack,"
        ".meadow-native-glass-content scrolledwindow, .meadow-native-glass-content viewport,"
        ".meadow-native-glass-content .sidebar,"
        ".meadow-native-glass-content .view:not(:selected):not(:hover):not(:drop(active)) {"
        " background-color: transparent; background-image: none; }"
        /* The shared capture already supplies the only pane boundary. GTK's
         * wide Paned handle otherwise paints a pale band with inherited edge
         * shadows over it. Keep its native allocation and drag target, but
         * remove those paints, including the adjacent scrolled-frame rims. */
        ".meadow-native-glass-content paned > separator {"
        " background-color: transparent; background-image: none;"
        " border-color: transparent; box-shadow: none; }"
        ".meadow-native-glass-content .meadow-glass-sidebar,"
        ".meadow-native-glass-content .sidebar,"
        ".meadow-native-glass-content scrolledwindow,"
        ".meadow-native-glass-content viewport,"
        ".meadow-native-glass-content notebook,"
        ".meadow-native-glass-content notebook > stack {"
        " border-color: transparent; box-shadow: none; }"
        ".meadow-native-glass-content, .meadow-native-glass-content .meadow-glass-sidebar {"
        " border-radius: 0; }"
        /* These classes exist only while an exact popup frame is available.
         * Native rows/checks/arrows and selected states retain theme paint. */
        "window.meadow-native-glass-popup-window, tooltip.meadow-native-glass-popup-window,"
        ".meadow-native-glass-popup { background-color: transparent; background-image: none; }"
        "window.meadow-native-glass-popup-window decoration {"
        " background-color: transparent; background-image: none; box-shadow: none; }"
        /* Only the original dialog content backing is cleared. Headerbars,
         * response buttons, entries and document views keep native paint. */
        ".meadow-native-glass-dialog { background-color: transparent; background-image: none; }",
        -1,NULL);
    GtkWidgetClass *window_class = g_type_class_ref(GTK_TYPE_WINDOW);
    original_window_show = window_class->show;
    window_class->show = window_show;
    original_window_realize=window_class->realize;
    window_class->realize=window_realize;
    guint show_signal=g_signal_lookup("show",GTK_TYPE_WIDGET);
    if(show_signal)g_signal_add_emission_hook(show_signal,0,popup_show_hook,NULL,NULL);
    guint map_signal=g_signal_lookup("map",GTK_TYPE_WIDGET);
    if(map_signal)g_signal_add_emission_hook(map_signal,0,files_view_map_hook,NULL,NULL);
    /* Keep the class pinned for the lifetime of this GTK module. */
}
