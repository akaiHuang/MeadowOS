/* Native Thunar folder materials. SPDX-License-Identifier: Apache-2.0 */
#define _DEFAULT_SOURCE 1
#include <gtk/gtk.h>
#include <gdk/gdkx.h>
#include <thunarx/thunarx.h>
#include <gmodule.h>
#include <glib/gstdio.h>
#include <sys/stat.h>
#include <sys/xattr.h>
#include <fcntl.h>
#include <unistd.h>
#include <signal.h>
#include <errno.h>
#include <math.h>
#include <string.h>
#include "meadow_folder_paint.h"

#define CACHE_LIMIT 128
#define WORK_LIMIT 3
#define XATTR_NAME "user.meadow.folder-color"
#define PREVIEW_LIMIT 3
#define FOLDER_SCAN_LIMIT 1024

typedef struct { const char *name, *label, *hex; } Palette;
static const Palette palette[] = {
    {"frost", "霧白", "#c7d3db"}, {"blue", "天空藍", "#6ba9ed"},
    {"mint", "薄荷綠", "#77bfa9"}, {"lilac", "丁香紫", "#ad91d0"},
    {"peach", "蜜桃橘", "#e6ae8d"}, {"rose", "玫瑰粉", "#d895a8"},
    {"graphite", "石墨灰", "#45505c"}
};

typedef struct {
    gint refs;
    gchar *key, *uri, *path;
    gchar *color, *pending_color;
    gchar *entry_stamp;
    guint generation;
    guint content_generation;
    gboolean metadata_ready, loading, previews_ready, show_hidden;
    gboolean requested_preview, requested_hidden;
    gboolean loading_preview;
    GCancellable *cancel;
    guint count;
    guint eligible_count;
    gboolean content_ready, count_complete;
    GdkPixbuf *previews[PREVIEW_LIMIT];
    gchar *preview_names[PREVIEW_LIMIT];
    const gchar *preview_kinds[PREVIEW_LIMIT];
    gboolean preview_is_thumbnail[PREVIEW_LIMIT];
    GFileMonitor *monitor;
    gint directory_fd;
    gboolean monitor_prepared;
    guint content_debounce;
    GWeakRef view, file_object;
    GdkRectangle cell;
    double progress, target;
    gint64 last_tick, last_seen;
} Folder;

typedef struct {
    Folder *folder;
    guint generation;
    guint content_generation;
    gboolean preview, show_hidden;
    gboolean prepare_monitor, pinned_directory;
    gint directory_fd;
    gchar *thumbnail_helper;
    GCancellable *thumbnail_cancel,*parent_cancel;
    guint thumbnail_timeout;
    gulong parent_cancel_handler;
    gchar *fallback, *write_color;
} Work;

typedef struct {
    gboolean stat_ok, saved, cancelled;
    guint64 device, inode;
    gchar *color, *write_error;
    guint count;
    guint eligible_count;
    gboolean content_ready, count_complete;
    gboolean directory_prepared;
    gboolean directory_stale;
    gint directory_fd;
    GdkPixbuf *previews[PREVIEW_LIMIT];
    GIcon *icons[PREVIEW_LIMIT];
    gchar *names[PREVIEW_LIMIT];
    const gchar *preview_kinds[PREVIEW_LIMIT];
    gboolean preview_is_thumbnail[PREVIEW_LIMIT];
} Result;

static GHashTable *folders, *cut_uris;
static guint active_work, animation_source, install_source;
static gboolean initialized, renderer_hooked;
static GMutex store_mutex;
static gint64 last_diagnostic;
static void (*original_render)(GtkCellRenderer *, cairo_t *, GtkWidget *,
                               const GdkRectangle *, const GdkRectangle *,
                               GtkCellRendererState);
static void pump_work(void);
static void ensure_quicklook(GtkWidget *widget);
static void stop_folder_monitor(Folder *folder);
static void ensure_folder_view(GtkWidget *view);
static gboolean folder_visible(Folder *folder);

static const Palette *color_by_name(const char *name) {
    for (guint i=0; i<G_N_ELEMENTS(palette); i++)
        if (g_strcmp0(name,palette[i].name)==0) return &palette[i];
    return NULL;
}
static const Palette *effective_color(const char *name) {
    const Palette *color=color_by_name(name);
    return color ? color : color_by_name("blue");
}

/* Quick Look belongs to the native file selection. Hover state never chooses
 * the path, and a text editor or a modified Space keeps its normal behavior. */
typedef struct {
    gint refs;
    GWeakRef window;
    GSubprocess *process;
    gboolean destroyed, closing;
    gint64 last_space;
} QuickLook;
static QuickLook *quicklook_ref(QuickLook *state) { state->refs++; return state; }
static void quicklook_unref(gpointer data) {
    QuickLook *state=data; if(--state->refs)return;
    g_weak_ref_clear(&state->window); g_clear_object(&state->process); g_free(state);
}
static gboolean bare_space(guint keyval, GdkModifierType state) {
    const GdkModifierType modifiers=gtk_accelerator_get_default_mod_mask() |
        GDK_SHIFT_MASK | GDK_CONTROL_MASK | GDK_MOD1_MASK | GDK_MOD4_MASK |
        GDK_SUPER_MASK | GDK_HYPER_MASK | GDK_META_MASK;
    return (keyval==GDK_KEY_space || keyval==GDK_KEY_KP_Space) &&
           !(state & modifiers);
}
static GtkWidget *standard_view_for_focus(GtkWidget *focus) {
    if(!focus || GTK_IS_EDITABLE(focus) || GTK_IS_TEXT_VIEW(focus) || GTK_IS_CELL_EDITABLE(focus))return NULL;
    if(g_strcmp0(G_OBJECT_TYPE_NAME(focus),"ExoIconView")!=0 && !GTK_IS_TREE_VIEW(focus))return NULL;
    GType standard=g_type_from_name("ThunarStandardView"); if(!standard)return NULL;
    for(GtkWidget *parent=gtk_widget_get_parent(focus);parent;parent=gtk_widget_get_parent(parent))
        if(g_type_is_a(G_OBJECT_TYPE(parent),standard))return parent;
    return NULL;
}
static gchar *path_from_model(GtkTreeModel *model,GList *paths) {
    gchar *path=NULL; gint file_column=-1;
    if(model)for(gint i=0;i<gtk_tree_model_get_n_columns(model);i++) {
        GType type=gtk_tree_model_get_column_type(model,i);
        if(g_type_is_a(type,THUNARX_TYPE_FILE_INFO)){file_column=i;break;}
    }
    if(model && file_column>=0 && paths) {
        GtkTreeIter iter;
        if(gtk_tree_model_get_iter(model,&iter,paths->data)) {
            GObject *file=NULL; gtk_tree_model_get(model,&iter,file_column,&file,-1);
            if(file && THUNARX_IS_FILE_INFO(file)) {
                gchar *uri=thunarx_file_info_get_uri(THUNARX_FILE_INFO(file));
                path=g_filename_from_uri(uri,NULL,NULL); g_free(uri);
            }
            g_clear_object(&file);
        }
    }
    return path;
}
static gchar *selected_local_path(GtkWidget *focus) {
    GtkTreeModel *model=NULL; GList *paths=NULL;
    if(GTK_IS_TREE_VIEW(focus)) {
        paths=gtk_tree_selection_get_selected_rows(gtk_tree_view_get_selection(GTK_TREE_VIEW(focus)),&model);
        if(model)g_object_ref(model);
    } else {
        typedef GList *(*GetSelected)(gpointer);
        static GetSelected get_selected; static gboolean resolved;
        if(!resolved) {
            GModule *module=g_module_open(NULL,G_MODULE_BIND_LAZY); resolved=TRUE;
            if(module) { g_module_symbol(module,"exo_icon_view_get_selected_items",(gpointer *)&get_selected); g_module_close(module); }
        }
        if(!get_selected)return NULL;
        paths=get_selected(focus); g_object_get(focus,"model",&model,NULL);
    }
    gchar *path=path_from_model(model,paths);
    g_list_free_full(paths,(GDestroyNotify)gtk_tree_path_free); g_clear_object(&model); return path;
}
static void quicklook_error(QuickLook *state, const char *message) {
    GtkWidget *window=g_weak_ref_get(&state->window);
    if(window && !state->destroyed) {
        GtkWidget *dialog=gtk_message_dialog_new(GTK_WINDOW(window),GTK_DIALOG_DESTROY_WITH_PARENT,
            GTK_MESSAGE_ERROR,GTK_BUTTONS_CLOSE,"無法開啟快速預覽");
        gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(dialog),"%s",message);
        g_signal_connect_swapped(dialog,"response",G_CALLBACK(gtk_widget_destroy),dialog); gtk_widget_show(dialog);
    }
    g_clear_object(&window);
}
static void quicklook_done(GObject *source, GAsyncResult *result, gpointer data) {
    QuickLook *state=data; GError *error=NULL;
    gboolean waited=g_subprocess_wait_finish(G_SUBPROCESS(source),result,&error);
    if(!state->closing && !state->destroyed && (!waited || !g_subprocess_get_successful(G_SUBPROCESS(source))))
        quicklook_error(state,error?error->message:"預覽程式未能讀取這個檔案。");
    if(state->process==G_SUBPROCESS(source))g_clear_object(&state->process);
    state->closing=FALSE; g_clear_error(&error); quicklook_unref(state);
}
static void quicklook_close(QuickLook *state) {
    if(state->process && !state->closing) { state->closing=TRUE; g_subprocess_send_signal(state->process,SIGTERM); }
}
static void quicklook_destroyed(GtkWidget *window, gpointer data) {
    (void)window; QuickLook *state=data; state->destroyed=TRUE; quicklook_close(state);
}
static gboolean quicklook_key(GtkWidget *window, GdkEventKey *event, gpointer data) {
    QuickLook *state=data;
    if(g_strcmp0(g_getenv("MEADOW_QUICKLOOK"),"0")==0 ||
        !bare_space(event->keyval,event->state) || state->destroyed)return FALSE;
    GtkWidget *focus=gtk_window_get_focus(GTK_WINDOW(window));
    if(!standard_view_for_focus(focus) || gtk_grab_get_current())return FALSE;
    const char *helper=g_getenv("MEADOW_QUICKLOOK_HELPER");
    if(!helper || !*helper)return FALSE;
    if(g_get_monotonic_time()-state->last_space<250000)return TRUE;
    if(state->process) { state->last_space=g_get_monotonic_time(); quicklook_close(state); return TRUE; }
    gchar *path=selected_local_path(focus); if(!path)return FALSE;
    state->last_space=g_get_monotonic_time();
    GdkWindow *native=gtk_widget_get_window(window);
    gchar *xid=g_strdup_printf("%lu",native && GDK_IS_X11_WINDOW(native)?GDK_WINDOW_XID(native):0UL);
    const gchar *argv[]={"python3",helper,"--path",path,"--parent-xid",xid,NULL}; GError *error=NULL;
    state->process=g_subprocess_newv(argv,G_SUBPROCESS_FLAGS_STDOUT_SILENCE,&error);
    if(state->process)g_subprocess_wait_async(state->process,NULL,quicklook_done,quicklook_ref(state));
    else quicklook_error(state,error?error->message:"找不到預覽程式。");
    g_clear_error(&error); g_free(path); g_free(xid);
    /* Once accepted, never fall through to Exo's activate-cursor-item binding. */
    return TRUE;
}
static void ensure_quicklook(GtkWidget *widget) {
    GtkWidget *window=gtk_widget_get_toplevel(widget);
    if(!GTK_IS_WINDOW(window) || g_strcmp0(G_OBJECT_TYPE_NAME(window),"ThunarWindow")!=0 ||
        g_object_get_data(G_OBJECT(window),"meadow-quicklook-state"))return;
    QuickLook *state=g_new0(QuickLook,1); state->refs=1; g_weak_ref_init(&state->window,window);
    g_object_set_data_full(G_OBJECT(window),"meadow-quicklook-state",state,quicklook_unref);
    g_signal_connect(window,"key-press-event",G_CALLBACK(quicklook_key),state);
    g_signal_connect(window,"destroy",G_CALLBACK(quicklook_destroyed),state);
}
static gboolean quicklook_show_hook(GSignalInvocationHint *hint, guint count,
                                    const GValue *values, gpointer data) {
    (void)hint; (void)data;
    if(count && GTK_IS_WIDGET(g_value_get_object(&values[0])))ensure_quicklook(g_value_get_object(&values[0]));
    return TRUE;
}

/* Sharp content thumbnails are produced outside GTK's render thread. A miss,
 * unsupported file or failed decoder retains Thunar's native category icon. */
typedef struct {
    gint refs;
    gchar *key,*uri,*path,*stamp;
    guint generation;
    gboolean loading,requested,failed;
    GWeakRef view,file_object;
    GdkRectangle cell;
    GdkPixbuf *image;
    GCancellable *cancel;
    gint64 last_seen;
} FileThumb;
typedef struct {
    FileThumb *thumb;
    guint generation,timeout;
    GCancellable *cancel;
    gchar *helper;
} ThumbWork;
static GHashTable *file_thumbs;
static guint thumb_workers;
static void pump_thumbnails(void);

static gboolean thumbnail_extension(const char *path) {
    static const char *extensions[]={"png","jpg","jpeg","webp","gif","bmp","tif","tiff","avif","heic","svg",
        "pdf","txt","md","markdown","csv","log","json","yaml","yml","toml","ini","xml","html","htm",
        "css","js","jsx","ts","tsx","py","go","rs","c","cc","cpp","h","hpp","java","rb","php","sql",
        "docx","xlsx","pptx","odt","ods","odp","zip","mp3","wav","flac","ogg","m4a","aac","mp4","mov","webm","mkv"};
    const char *base=strrchr(path,'/'); base=base?base+1:path;
    const char *dot=strrchr(base,'.');
    if(!dot)return g_ascii_strcasecmp(base,"README")==0 || g_ascii_strcasecmp(base,"LICENSE")==0;
    for(guint i=0;i<G_N_ELEMENTS(extensions);i++)if(g_ascii_strcasecmp(dot+1,extensions[i])==0)return TRUE;
    return FALSE;
}
static FileThumb *thumb_ref(FileThumb *thumb) { thumb->refs++;return thumb; }
static void thumb_unref(gpointer data) {
    FileThumb *thumb=data;if(--thumb->refs)return;
    g_free(thumb->key);g_free(thumb->uri);g_free(thumb->path);g_free(thumb->stamp);
    g_weak_ref_clear(&thumb->view);g_weak_ref_clear(&thumb->file_object);
    g_clear_object(&thumb->image);g_clear_object(&thumb->cancel);g_free(thumb);
}
static void queue_thumb(FileThumb *thumb) {
    GtkWidget *view=g_weak_ref_get(&thumb->view);
    if(view && gtk_widget_get_mapped(view)) {
        gint x=thumb->cell.x,y=thumb->cell.y;
        if(GTK_IS_SCROLLABLE(view)) {
            GtkAdjustment *h=gtk_scrollable_get_hadjustment(GTK_SCROLLABLE(view));
            GtkAdjustment *v=gtk_scrollable_get_vadjustment(GTK_SCROLLABLE(view));
            if(h)x-=gtk_adjustment_get_value(h);
            if(v)y-=gtk_adjustment_get_value(v);
        }
        gtk_widget_queue_draw_area(view,x,y,thumb->cell.width,thumb->cell.height);
    }
    g_clear_object(&view);
}
static void cancel_view_thumbnails(GtkWidget *view) {
    if(!file_thumbs)return;
    GHashTableIter iter;gpointer value;g_hash_table_iter_init(&iter,file_thumbs);
    while(g_hash_table_iter_next(&iter,NULL,&value)) {
        FileThumb *thumb=value;GtkWidget *owner=g_weak_ref_get(&thumb->view);
        if(owner==view) {thumb->requested=FALSE;thumb->generation++;if(thumb->cancel)g_cancellable_cancel(thumb->cancel);}
        g_clear_object(&owner);
    }
}
static void thumbnail_view_unmapped(GtkWidget *view,gpointer data) {(void)data;cancel_view_thumbnails(view);}
static void thumbnail_directory_changed(GObject *object,GParamSpec *spec,gpointer data) {
    (void)object;(void)spec;cancel_view_thumbnails(GTK_WIDGET(data));
}
static void ensure_thumbnail_view(GtkWidget *view) {
    if(g_object_get_data(G_OBJECT(view),"meadow-thumbnail-view"))return;
    g_object_set_data(G_OBJECT(view),"meadow-thumbnail-view",GINT_TO_POINTER(1));
    g_signal_connect(view,"unmap",G_CALLBACK(thumbnail_view_unmapped),NULL);
    GtkWidget *standard=standard_view_for_focus(view);
    if(standard)g_signal_connect_object(standard,"notify::current-directory",
        G_CALLBACK(thumbnail_directory_changed),view,0);
}
static gchar *thumbnail_stamp(GObject *file,GFileInfo *info) {
    return g_strdup_printf("%p:%"G_GUINT64_FORMAT":%"G_GUINT64_FORMAT":%u:%u:%"G_GUINT64_FORMAT":%"G_GUINT64_FORMAT":%u",(void *)file,
        info?(guint64)g_file_info_get_size(info):0,
        info?g_file_info_get_attribute_uint64(info,G_FILE_ATTRIBUTE_TIME_MODIFIED):0,
        info?g_file_info_get_attribute_uint32(info,G_FILE_ATTRIBUTE_TIME_MODIFIED_USEC):0,
        info?g_file_info_get_attribute_uint32(info,G_FILE_ATTRIBUTE_UNIX_DEVICE):0,
        info?g_file_info_get_attribute_uint64(info,G_FILE_ATTRIBUTE_UNIX_INODE):0,
        info?g_file_info_get_attribute_uint64(info,G_FILE_ATTRIBUTE_TIME_CHANGED):0,
        info?g_file_info_get_attribute_uint32(info,G_FILE_ATTRIBUTE_TIME_CHANGED_USEC):0);
}
static FileThumb *get_thumbnail(GtkWidget *view,GObject *file,const gchar *uri,
                                const gchar *path,GFileInfo *info) {
    if(!file_thumbs)file_thumbs=g_hash_table_new_full(g_str_hash,g_str_equal,NULL,thumb_unref);
    gchar *key=g_strdup_printf("%p:%s",(void *)view,uri);FileThumb *thumb=g_hash_table_lookup(file_thumbs,key);
    if(!thumb && g_hash_table_size(file_thumbs)>=CACHE_LIMIT) {
        GHashTableIter iter;gpointer value;FileThumb *oldest=NULL;g_hash_table_iter_init(&iter,file_thumbs);
        while(g_hash_table_iter_next(&iter,NULL,&value)) {
            FileThumb *candidate=value;
            if(!candidate->loading && (!oldest || candidate->last_seen<oldest->last_seen))oldest=candidate;
        }
        if(oldest)g_hash_table_remove(file_thumbs,oldest->key);
    }
    if(!thumb && g_hash_table_size(file_thumbs)<CACHE_LIMIT) {
        thumb=g_new0(FileThumb,1);thumb->refs=1;thumb->key=g_strdup(key);
        thumb->uri=g_strdup(uri);thumb->path=g_strdup(path);
        g_weak_ref_init(&thumb->view,view);g_weak_ref_init(&thumb->file_object,file);
        g_hash_table_insert(file_thumbs,thumb->key,thumb);
    }
    g_free(key);if(!thumb)return NULL;
    gchar *stamp=thumbnail_stamp(file,info);
    GObject *bound=g_weak_ref_get(&thumb->file_object);gboolean replaced=bound!=file;g_clear_object(&bound);
    g_weak_ref_set(&thumb->view,view);
    if(replaced || g_strcmp0(stamp,thumb->stamp)!=0) {
        g_free(thumb->stamp);thumb->stamp=stamp;thumb->generation++;
        g_clear_object(&thumb->image);thumb->failed=FALSE;
        if(thumb->cancel)g_cancellable_cancel(thumb->cancel);
        g_weak_ref_set(&thumb->file_object,file);
    } else g_free(stamp);
    thumb->requested=TRUE;thumb->last_seen=g_get_monotonic_time();return thumb;
}
static gboolean same_identity(const struct stat *a,const struct stat *b) {
    return a->st_dev==b->st_dev && a->st_ino==b->st_ino && a->st_size==b->st_size &&
           a->st_mtim.tv_sec==b->st_mtim.tv_sec && a->st_mtim.tv_nsec==b->st_mtim.tv_nsec &&
           a->st_ctim.tv_sec==b->st_ctim.tv_sec && a->st_ctim.tv_nsec==b->st_ctim.tv_nsec;
}
static gchar *thumbnail_cache_path(const gchar *uri,const struct stat *st) {
    gchar *identity=g_strdup_printf("%s\n%llu:%llu:%lld:%lld:%ld:%lld:%ld",uri,
        (unsigned long long)st->st_dev,(unsigned long long)st->st_ino,(long long)st->st_size,
        (long long)st->st_mtim.tv_sec,st->st_mtim.tv_nsec,(long long)st->st_ctim.tv_sec,st->st_ctim.tv_nsec);
    gchar *hash=g_compute_checksum_for_string(G_CHECKSUM_SHA256,identity,-1);
    gchar *name=g_strconcat(hash,".png",NULL);
    gchar *path=g_build_filename(g_get_user_cache_dir(),"meadow","file-previews",name,NULL);
    g_free(identity);g_free(hash);g_free(name);return path;
}
static GdkPixbuf *read_cached_thumbnail(const char *path) {
    struct stat st;gint width=0,height=0;
    gint fd=open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC|O_NONBLOCK);if(fd<0)return NULL;
    GdkPixbuf *image=NULL;gchar *pinned=g_strdup_printf("/proc/self/fd/%d",fd);
    if(fstat(fd,&st)==0 && S_ISREG(st.st_mode) && st.st_size<=2*1024*1024) {
        GdkPixbufFormat *format=gdk_pixbuf_get_file_info(pinned,&width,&height);
        if(format && width>=1 && height>=1 && width<=256 && height<=256) {
            gchar *name=gdk_pixbuf_format_get_name(format);
            if(g_strcmp0(name,"png")==0)image=gdk_pixbuf_new_from_file(pinned,NULL);
            g_free(name);
        }
    }
    g_free(pinned);close(fd);return image;
}
static gboolean thumbnail_timeout(gpointer data) {
    ThumbWork *work=data;work->timeout=0;g_cancellable_cancel(work->cancel);return G_SOURCE_REMOVE;
}
static GdkPixbuf *read_file_thumbnail(const gchar *uri,const gchar *path,const gchar *helper,
                                      GCancellable *cancel) {
    GdkPixbuf *image=NULL;struct stat before,after;gboolean generated=FALSE;
    if(g_cancellable_is_cancelled(cancel) || lstat(path,&before)!=0 || !S_ISREG(before.st_mode))return NULL;
    gchar *cache=thumbnail_cache_path(uri,&before);
    image=read_cached_thumbnail(cache);
    if(!image && helper && *helper && !g_cancellable_is_cancelled(cancel)) {
        gchar *directory=g_path_get_dirname(cache);
        if(g_mkdir_with_parents(directory,0700)==0) {
            const gchar *argv[]={"python3",helper,"--thumbnail","--path",path,
                                 "--output",cache,"--size","256",NULL};
            GSubprocess *process=g_subprocess_newv(argv,
                G_SUBPROCESS_FLAGS_STDOUT_SILENCE|G_SUBPROCESS_FLAGS_STDERR_SILENCE,NULL);
            if(process) {
                gboolean waited=g_subprocess_wait(process,cancel,NULL);
                if(!waited) {g_subprocess_force_exit(process);g_subprocess_wait(process,NULL,NULL);}
                else if(g_subprocess_get_successful(process)) {generated=TRUE;image=read_cached_thumbnail(cache);}
                g_object_unref(process);
            }
        }
        g_free(directory);
    }
    gboolean source_changed=lstat(path,&after)!=0 || !same_identity(&before,&after);
    if(g_cancellable_is_cancelled(cancel) || source_changed)g_clear_object(&image);
    /* A changed child must not leave newly generated bytes under its old
     * identity key, even though this caller already discards the result. */
    if(source_changed && generated)g_unlink(cache);
    g_free(cache);
    return image;
}
static void thumbnail_worker(GTask *task,gpointer source,gpointer data,GCancellable *cancel) {
    (void)source;ThumbWork *work=data;
    GdkPixbuf *image=read_file_thumbnail(work->thumb->uri,work->thumb->path,work->helper,cancel);
    g_task_return_pointer(task,image,image?g_object_unref:NULL);
}
static void thumbnail_work_free(gpointer data) {
    ThumbWork *work=data;if(work->timeout)g_source_remove(work->timeout);
    thumb_unref(work->thumb);g_clear_object(&work->cancel);g_free(work->helper);g_free(work);
}
static void thumbnail_done(GObject *source,GAsyncResult *result,gpointer data) {
    (void)source;(void)data;GTask *task=G_TASK(result);ThumbWork *work=g_task_get_task_data(task);
    FileThumb *thumb=work->thumb;GdkPixbuf *image=g_task_propagate_pointer(task,NULL);
    if(work->timeout){g_source_remove(work->timeout);work->timeout=0;}
    thumb->loading=FALSE;thumb_workers--;g_clear_object(&thumb->cancel);
    if(work->generation==thumb->generation) {
        g_clear_object(&thumb->image);thumb->image=image;image=NULL;
        thumb->failed=thumb->image==NULL;queue_thumb(thumb);
    }
    g_clear_object(&image);pump_thumbnails();
}
static void pump_thumbnails(void) {
    const char *helper=g_getenv("MEADOW_QUICKLOOK_HELPER");
    if(!helper || !*helper || !file_thumbs || g_strcmp0(g_getenv("MEADOW_FILE_THUMBNAILS"),"0")==0)return;
    GHashTableIter iter;gpointer value;g_hash_table_iter_init(&iter,file_thumbs);
    while(thumb_workers<2 && g_hash_table_iter_next(&iter,NULL,&value)) {
        FileThumb *thumb=value;if(!thumb->requested || thumb->loading || thumb->failed || thumb->image)continue;
        GtkWidget *view=g_weak_ref_get(&thumb->view);GObject *file=g_weak_ref_get(&thumb->file_object);
        gboolean visible=view && file && gtk_widget_get_mapped(view);
        g_clear_object(&view);g_clear_object(&file);if(!visible)continue;
        ThumbWork *work=g_new0(ThumbWork,1);work->thumb=thumb_ref(thumb);work->generation=thumb->generation;
        work->helper=g_strdup(helper);work->cancel=g_cancellable_new();thumb->cancel=g_object_ref(work->cancel);
        GTask *task=g_task_new(NULL,work->cancel,thumbnail_done,NULL);g_task_set_check_cancellable(task,FALSE);
        g_task_set_task_data(task,work,thumbnail_work_free);
        work->timeout=g_timeout_add(8000,thumbnail_timeout,work);thumb->loading=TRUE;thumb_workers++;
        g_task_run_in_thread(task,thumbnail_worker);g_object_unref(task);
    }
}

static Folder *folder_ref(Folder *folder) { folder->refs++; return folder; }
static void folder_unref(gpointer data) {
    Folder *folder=data;
    if (--folder->refs) return;
    stop_folder_monitor(folder);
    for (guint i=0; i<PREVIEW_LIMIT; i++) {
        g_clear_object(&folder->previews[i]);
        g_free(folder->preview_names[i]);
    }
    g_weak_ref_clear(&folder->view); g_weak_ref_clear(&folder->file_object);
    g_clear_object(&folder->cancel); g_free(folder->pending_color);
    g_free(folder->key); g_free(folder->uri); g_free(folder->path); g_free(folder->color);
    g_free(folder->entry_stamp);g_free(folder);
}
static void result_free(gpointer data) {
    Result *result=data;
    if(result->directory_prepared && result->directory_fd>=0)close(result->directory_fd);
    for (guint i=0; i<PREVIEW_LIMIT; i++) {
        g_clear_object(&result->previews[i]); g_clear_object(&result->icons[i]);
        g_free(result->names[i]);
    }
    g_free(result->color); g_free(result->write_error); g_free(result);
}
static void work_free(gpointer data) {
    Work *work=data;
    if(work->thumbnail_timeout)g_source_remove(work->thumbnail_timeout);
    if(work->parent_cancel_handler)g_cancellable_disconnect(work->parent_cancel,work->parent_cancel_handler);
    g_clear_object(&work->thumbnail_cancel);g_clear_object(&work->parent_cancel);g_free(work->thumbnail_helper);
    if(work->pinned_directory)close(work->directory_fd);
    folder_unref(work->folder);
    g_free(work->fallback); g_free(work->write_color); g_free(work);
}
static void queue_folder(Folder *folder) {
    GtkWidget *view=g_weak_ref_get(&folder->view);
    if (view) {
        if (gtk_widget_get_mapped(view)) {
            gint x=folder->cell.x,y=folder->cell.y;
            if(GTK_IS_SCROLLABLE(view)) {
                GtkAdjustment *h=gtk_scrollable_get_hadjustment(GTK_SCROLLABLE(view));
                GtkAdjustment *v=gtk_scrollable_get_vadjustment(GTK_SCROLLABLE(view));
                if(h)x-=gtk_adjustment_get_value(h);
                if(v)y-=gtk_adjustment_get_value(v);
            }
            gtk_widget_queue_draw_area(view,x,y,folder->cell.width,folder->cell.height);
        }
        g_object_unref(view);
    }
}
static void queue_all(void) {
    GHashTableIter iter; gpointer value;
    if (!folders) return;
    g_hash_table_iter_init(&iter,folders);
    while (g_hash_table_iter_next(&iter,NULL,&value)) queue_folder(value);
}

static gboolean folder_visible(Folder *folder) {
    GtkWidget *view=g_weak_ref_get(&folder->view);gboolean visible=FALSE;
    if(view && gtk_widget_get_mapped(view)) {
        GdkWindow *window=gtk_widget_get_window(gtk_widget_get_toplevel(view));
        if(window && (gdk_window_get_state(window)&GDK_WINDOW_STATE_ICONIFIED)) {
            g_object_unref(view);return FALSE;
        }
        double x=0,y=0;
        if(GTK_IS_SCROLLABLE(view)) {
            GtkAdjustment *h=gtk_scrollable_get_hadjustment(GTK_SCROLLABLE(view));
            GtkAdjustment *v=gtk_scrollable_get_vadjustment(GTK_SCROLLABLE(view));
            if(h)x=gtk_adjustment_get_value(h);
            if(v)y=gtk_adjustment_get_value(v);
        }
        visible=folder->cell.width>0 && folder->cell.height>0 &&
            folder->cell.x+folder->cell.width>x && folder->cell.y+folder->cell.height>y &&
            folder->cell.x<x+gtk_widget_get_allocated_width(view) &&
            folder->cell.y<y+gtk_widget_get_allocated_height(view);
    }
    g_clear_object(&view);return visible;
}
static void stop_folder_monitor(Folder *folder) {
    if(folder->content_debounce){g_source_remove(folder->content_debounce);folder->content_debounce=0;}
    if(folder->monitor) {
        g_signal_handlers_disconnect_by_data(folder->monitor,folder);
        g_file_monitor_cancel(folder->monitor);g_clear_object(&folder->monitor);
    }
    if(folder->monitor_prepared && folder->directory_fd>=0)close(folder->directory_fd);
    folder->directory_fd=-1;folder->monitor_prepared=FALSE;
}
static void release_folder_content(Folder *folder) {
    folder->requested_preview=FALSE;folder->content_generation++;
    folder->previews_ready=FALSE;folder->content_ready=FALSE;
    if(folder->loading_preview && folder->cancel)g_cancellable_cancel(folder->cancel);
    stop_folder_monitor(folder);
}
static gboolean content_debounced(gpointer data) {
    Folder *folder=data;folder->content_debounce=0;
    if(folder->requested_preview && folder_visible(folder))pump_work();
    else release_folder_content(folder);
    return G_SOURCE_REMOVE;
}
static void invalidate_folder_content(Folder *folder) {
    folder->content_generation++;folder->previews_ready=FALSE;folder->content_ready=FALSE;
    if(folder->loading_preview && folder->cancel)g_cancellable_cancel(folder->cancel);
    if(folder->content_debounce)g_source_remove(folder->content_debounce);
    folder->content_debounce=g_timeout_add(140,content_debounced,folder);
    queue_folder(folder);
}
static void folder_content_changed(GFileMonitor *monitor,GFile *file,GFile *other,
                                   GFileMonitorEvent event,gpointer data) {
    (void)monitor;(void)file;(void)other;(void)event;
    Folder *folder=data;
    if(folder->requested_preview && folder_visible(folder))invalidate_folder_content(folder);
    else release_folder_content(folder);
}
static void attach_folder_monitor(Folder *folder,Result *result) {
    stop_folder_monitor(folder);
    folder->monitor_prepared=TRUE;folder->directory_fd=result->directory_fd;
    result->directory_prepared=FALSE;result->directory_fd=-1;
    if(folder->directory_fd<0)return;
    gchar *path=g_strdup_printf("/proc/self/fd/%d",folder->directory_fd);
    GFile *file=g_file_new_for_path(path);
    folder->monitor=g_file_monitor_directory(file,G_FILE_MONITOR_WATCH_MOVES,NULL,NULL);
    if(folder->monitor)g_signal_connect(folder->monitor,"changed",G_CALLBACK(folder_content_changed),folder);
    g_object_unref(file);g_free(path);
}
static void cancel_view_folders(GtkWidget *view,gboolean all) {
    if(!folders)return;
    GHashTableIter iter;gpointer value;g_hash_table_iter_init(&iter,folders);
    while(g_hash_table_iter_next(&iter,NULL,&value)) {
        Folder *folder=value;GtkWidget *owner=g_weak_ref_get(&folder->view);
        if(owner==view && (all || !folder_visible(folder)))release_folder_content(folder);
        g_clear_object(&owner);
    }
}
static void folder_view_unmapped(GtkWidget *view,gpointer data) {(void)data;cancel_view_folders(view,TRUE);}
static gboolean folder_view_sweep(gpointer data) {
    GtkWidget *view=data;g_object_set_data(G_OBJECT(view),"meadow-folder-sweep",NULL);
    cancel_view_folders(view,FALSE);return G_SOURCE_REMOVE;
}
static void schedule_folder_sweep(GtkWidget *view) {
    if(g_object_get_data(G_OBJECT(view),"meadow-folder-sweep"))return;
    guint source=g_idle_add_full(G_PRIORITY_DEFAULT_IDLE,folder_view_sweep,g_object_ref(view),g_object_unref);
    g_object_set_data(G_OBJECT(view),"meadow-folder-sweep",GUINT_TO_POINTER(source));
}
static void folder_scroll_changed(GtkAdjustment *adjustment,gpointer data) {(void)adjustment;schedule_folder_sweep(GTK_WIDGET(data));}
static void folder_size_changed(GtkWidget *view,GdkRectangle *allocation,gpointer data) {
    (void)allocation;(void)data;schedule_folder_sweep(view);
}
static void folder_location_changed(GObject *object,GParamSpec *spec,gpointer data) {
    (void)object;(void)spec;cancel_view_folders(GTK_WIDGET(data),TRUE);
}
static gboolean folder_window_state(GtkWidget *window,GdkEventWindowState *event,gpointer data) {
    (void)window;
    if(event->new_window_state&GDK_WINDOW_STATE_ICONIFIED)cancel_view_folders(GTK_WIDGET(data),TRUE);
    return FALSE;
}
static void ensure_folder_view(GtkWidget *view) {
    if(g_object_get_data(G_OBJECT(view),"meadow-folder-view"))return;
    g_object_set_data(G_OBJECT(view),"meadow-folder-view",GINT_TO_POINTER(1));
    g_signal_connect(view,"unmap",G_CALLBACK(folder_view_unmapped),NULL);
    g_signal_connect(view,"destroy",G_CALLBACK(folder_view_unmapped),NULL);
    g_signal_connect(view,"size-allocate",G_CALLBACK(folder_size_changed),NULL);
    GtkWidget *window=gtk_widget_get_toplevel(view);
    if(GTK_IS_WINDOW(window))g_signal_connect_object(window,"window-state-event",G_CALLBACK(folder_window_state),view,0);
    if(GTK_IS_SCROLLABLE(view)) {
        GtkAdjustment *h=gtk_scrollable_get_hadjustment(GTK_SCROLLABLE(view));
        GtkAdjustment *v=gtk_scrollable_get_vadjustment(GTK_SCROLLABLE(view));
        if(h)g_signal_connect_object(h,"value-changed",G_CALLBACK(folder_scroll_changed),view,0);
        if(v)g_signal_connect_object(v,"value-changed",G_CALLBACK(folder_scroll_changed),view,0);
    }
    GtkWidget *standard=standard_view_for_focus(view);
    if(standard) {
        g_signal_connect_object(standard,"notify::current-directory",G_CALLBACK(folder_location_changed),view,0);
        g_signal_connect_object(standard,"notify::show-hidden",G_CALLBACK(folder_location_changed),view,0);
    }
}

/* Per-URI fallback also records the inode. Reusing a deleted path must not
 * transfer somebody else's color. The normal xattr follows filesystem rename. */
static gchar *store_path(void) {
    return g_build_filename(g_get_user_config_dir(),"meadow","folder-colors.ini",NULL);
}
static GKeyFile *store_load(void) {
    GKeyFile *keyfile=g_key_file_new();
    gchar *path=store_path(); GStatBuf statbuf;
    if (g_stat(path,&statbuf)==0 && statbuf.st_size <= 1024*1024)
        g_key_file_load_from_file(keyfile,path,G_KEY_FILE_NONE,NULL);
    g_free(path); return keyfile;
}
static gchar *store_key(const gchar *uri) {
    return g_compute_checksum_for_string(G_CHECKSUM_SHA256,uri,-1);
}
static gchar *store_get(const gchar *uri) {
    g_mutex_lock(&store_mutex);
    GKeyFile *keyfile=store_load(); gchar *key=store_key(uri);
    gchar *value=g_key_file_get_string(keyfile,"folders",key,NULL);
    g_free(key); g_key_file_unref(keyfile); g_mutex_unlock(&store_mutex); return value;
}
static gboolean store_set(const gchar *uri, const gchar *color, guint64 device,
                          guint64 inode, GError **error) {
    g_mutex_lock(&store_mutex);
    GKeyFile *keyfile=store_load(); gchar *key=store_key(uri);
    if (color) {
        gchar *value=g_strdup_printf("%s:%"G_GUINT64_FORMAT":%"G_GUINT64_FORMAT,color,device,inode);
        g_key_file_set_string(keyfile,"folders",key,value); g_free(value);
    } else g_key_file_remove_key(keyfile,"folders",key,NULL);
    gchar *path=store_path(), *dir=g_path_get_dirname(path); gsize length;
    gchar *contents=g_key_file_to_data(keyfile,&length,NULL);
    gboolean ok=g_mkdir_with_parents(dir,0700)==0;
    if (!ok) g_set_error(error,G_FILE_ERROR,g_file_error_from_errno(errno),"%s",g_strerror(errno));
    else ok=g_file_set_contents_full(path,contents,length,
              G_FILE_SET_CONTENTS_CONSISTENT|G_FILE_SET_CONTENTS_DURABLE,0600,error);
    g_free(contents); g_free(path); g_free(dir); g_free(key); g_key_file_unref(keyfile);
    g_mutex_unlock(&store_mutex); return ok;
}
static gchar *fallback_color(const gchar *encoded, guint64 device, guint64 inode) {
    if (!encoded) return NULL;
    gchar **parts=g_strsplit(encoded,":",4), *color=NULL;
    if (g_strv_length(parts)==3 && color_by_name(parts[0]) &&
        g_ascii_strtoull(parts[1],NULL,10)==device && g_ascii_strtoull(parts[2],NULL,10)==inode)
        color=g_strdup(parts[0]);
    g_strfreev(parts); return color;
}

static gint compare_info(gconstpointer a, gconstpointer b) {
    return g_strcmp0(g_file_info_get_name(*(GFileInfo * const *)a),
                    g_file_info_get_name(*(GFileInfo * const *)b));
}
static GdkPixbuf *bounded_image(const gchar *path) {
    struct stat st; int width=0,height=0;
    if(!path)return NULL;
    int fd=open(path,O_RDONLY|O_NOFOLLOW|O_CLOEXEC|O_NONBLOCK);
    if(fd<0)return NULL;
    GdkPixbuf *image=NULL;gchar *pinned=g_strdup_printf("/proc/self/fd/%d",fd);
    if(fstat(fd,&st)==0 && S_ISREG(st.st_mode) && st.st_size<=8*1024*1024 &&
        gdk_pixbuf_get_file_info(pinned,&width,&height) && width>0 && height>0 &&
        (guint64)width*(guint64)height<=16*1024*1024)
        image=gdk_pixbuf_new_from_file_at_scale(pinned,96,112,TRUE,NULL);
    g_free(pinned);close(fd);return image;
}

static const gchar *preview_kind(GFileInfo *info) {
    if(g_file_info_get_is_symlink(info))return "generic";
    if(g_file_info_get_file_type(info)==G_FILE_TYPE_DIRECTORY)return "folder";
    if(g_file_info_get_file_type(info)!=G_FILE_TYPE_REGULAR)return "generic";
    const gchar *mime=g_file_info_get_content_type(info),*name=g_file_info_get_name(info);
    const gchar *dot=name?strrchr(name,'.'):NULL;
    if(mime && g_str_has_prefix(mime,"image/"))return "image";
    if(mime && g_str_has_prefix(mime,"video/"))return "video";
    if(mime && g_str_has_prefix(mime,"audio/"))return "audio";
    if(mime && g_str_equal(mime,"application/pdf"))return "pdf";
    if(mime && (strstr(mime,"spreadsheet") || strstr(mime,"excel")))return "spreadsheet";
    if(mime && (strstr(mime,"presentation") || strstr(mime,"powerpoint")))return "presentation";
    if(mime && (strstr(mime,"wordprocessing") || strstr(mime,"msword") || strstr(mime,"opendocument.text")))return "document";
    if(mime && (strstr(mime,"zip") || strstr(mime,"tar") || strstr(mime,"compressed") || strstr(mime,"archive")))return "archive";
    if(dot) {
        static const gchar *code[]={".py",".js",".jsx",".ts",".tsx",".go",".rs",".c",".h",".cc",".cpp",".hpp",".java",".rb",".php",".css",".html",".sh",".sql",".json",".yaml",".yml",".toml",".xml"};
        for(guint i=0;i<G_N_ELEMENTS(code);i++)if(g_ascii_strcasecmp(dot,code[i])==0)return "code";
        if(g_ascii_strcasecmp(dot,".csv")==0)return "spreadsheet";
    }
    return mime && g_str_has_prefix(mime,"text/") ? "text" : "generic";
}

/* Visible native directories are scanned once, then refreshed by their monitor.
 * Counting is separate from representative selection: a bounded/failed scan
 * never turns its lower bound into an exact total. No target links are read. */
static void read_previews(Work *work, Result *result, GCancellable *cancel) {
    if(g_cancellable_is_cancelled(cancel))return;
    int directory_fd=work->pinned_directory?dup(work->directory_fd):
        open(work->folder->path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if(directory_fd<0)return;
    gchar *directory_path=g_strdup_printf("/proc/self/fd/%d",directory_fd);
    GFile *file=g_file_new_for_path(directory_path);
    GFileEnumerator *enumerator=g_file_enumerate_children(file,
        "standard::name,standard::display-name,standard::type,standard::is-hidden,standard::is-backup,"
        "standard::is-symlink,standard::content-type,standard::icon,thumbnail::path,thumbnail::is-valid",
        G_FILE_QUERY_INFO_NOFOLLOW_SYMLINKS,cancel,NULL);
    if (!enumerator) { g_object_unref(file); g_free(directory_path); close(directory_fd); return; }
    GPtrArray *items=g_ptr_array_new_with_free_func(g_object_unref);
    guint scanned=0;
    for (; scanned<FOLDER_SCAN_LIMIT && !g_cancellable_is_cancelled(cancel); scanned++) {
        GError *error=NULL;GFileInfo *info=g_file_enumerator_next_file(enumerator,cancel,&error);
        if(!info) {
            result->count_complete=error==NULL;result->content_ready=result->count_complete;
            g_clear_error(&error);break;
        }
        result->content_ready=TRUE;
        if (!work->show_hidden && (g_file_info_get_is_hidden(info)||g_file_info_get_is_backup(info)))
            g_object_unref(info);
        else {result->eligible_count++;g_ptr_array_add(items,info);}
    }
    g_file_enumerator_close(enumerator,cancel,NULL); g_object_unref(enumerator);
    g_ptr_array_sort(items,compare_info);
    guint selected[PREVIEW_LIMIT]={0},selected_count=0;
    for(guint pass=0;pass<2 && selected_count<PREVIEW_LIMIT;pass++)for(guint i=0;i<items->len && selected_count<PREVIEW_LIMIT;i++) {
        const gchar *kind=preview_kind(g_ptr_array_index(items,i));gboolean used=FALSE,same_kind=FALSE;
        for(guint j=0;j<selected_count;j++) {
            used|=selected[j]==i;same_kind|=g_str_equal(kind,preview_kind(g_ptr_array_index(items,selected[j])));
        }
        if(!used && (pass || !same_kind))selected[selected_count++]=i;
    }
    for (guint i=0; i<selected_count && !g_cancellable_is_cancelled(cancel); i++) {
        GFileInfo *info=g_ptr_array_index(items,selected[i]);
        GIcon *icon=g_file_info_get_icon(info);
        result->icons[i]=icon ? g_object_ref(icon) : g_themed_icon_new("text-x-generic");
        result->names[i]=g_strdup(g_file_info_get_display_name(info));
        result->preview_kinds[i]=preview_kind(info);
        gboolean regular=g_file_info_get_file_type(info)==G_FILE_TYPE_REGULAR && !g_file_info_get_is_symlink(info);
        const gchar *thumbnail=g_file_info_get_attribute_byte_string(info,G_FILE_ATTRIBUTE_THUMBNAIL_PATH);
        if(regular && g_file_info_get_attribute_boolean(info,G_FILE_ATTRIBUTE_THUMBNAIL_IS_VALID))
            result->previews[i]=bounded_image(thumbnail);
        if (regular && !result->previews[i] && g_str_equal(result->preview_kinds[i],"image")) {
            gchar *path=g_build_filename(directory_path,g_file_info_get_name(info),NULL);
            result->previews[i]=bounded_image(path); g_free(path);
        }
        if(!result->previews[i] && work->thumbnail_helper &&
            !g_str_equal(result->preview_kinds[i],"image") &&
            regular && thumbnail_extension(g_file_info_get_name(info)) &&
            !g_cancellable_is_cancelled(work->thumbnail_cancel)) {
            /* The decoder is a separate process, so /proc/self would identify
             * its own descriptors. Keep our directory FD alive and address it
             * explicitly; the actual child name retains its file format. */
            gchar *source_path=g_strdup_printf("/proc/%ld/fd/%d/%s",(long)getpid(),directory_fd,g_file_info_get_name(info));
            GFile *parent=g_file_new_for_uri(work->folder->uri);
            GFile *child=g_file_get_child(parent,g_file_info_get_name(info));
            gchar *uri=g_file_get_uri(child);
            result->previews[i]=read_file_thumbnail(uri,source_path,work->thumbnail_helper,work->thumbnail_cancel);
            g_free(uri);g_object_unref(child);g_object_unref(parent);g_free(source_path);
        }
        result->preview_is_thumbnail[i]=result->previews[i]!=NULL;
        result->count++;
    }
    if(g_cancellable_is_cancelled(cancel)) {result->content_ready=FALSE;result->count_complete=FALSE;}
    g_ptr_array_unref(items); g_object_unref(file); g_free(directory_path); close(directory_fd);
}

static gboolean pinned_directory_current(const gchar *path,gint fd) {
    struct stat current,pinned;
    return lstat(path,&current)==0 && S_ISDIR(current.st_mode) && fstat(fd,&pinned)==0 &&
        current.st_dev==pinned.st_dev && current.st_ino==pinned.st_ino;
}

static void worker(GTask *task, gpointer source, gpointer task_data, GCancellable *cancel) {
    (void)source;
    Work *work=task_data; Result *result=g_new0(Result,1); struct stat st;
    result->stat_ok=lstat(work->folder->path,&st)==0 && (S_ISDIR(st.st_mode)||S_ISLNK(st.st_mode));
    if (result->stat_ok) {
        result->device=st.st_dev; result->inode=st.st_ino;
        if (work->write_color) {
            gboolean xattr_written=lsetxattr(work->folder->path,XATTR_NAME,work->write_color,
                                              strlen(work->write_color),0)==0;
            GError *error=NULL;
            result->saved=xattr_written || store_set(work->folder->uri,work->write_color,
                                                    result->device,result->inode,&error);
            if (xattr_written) store_set(work->folder->uri,NULL,0,0,NULL);
            if (!result->saved) result->write_error=g_strdup(error?error->message:g_strerror(errno));
            g_clear_error(&error);
            result->color=g_strdup(work->write_color);
        } else {
            gchar value[40]={0}; ssize_t size=lgetxattr(work->folder->path,XATTR_NAME,value,sizeof(value)-1);
            if (size>0 && color_by_name(value)) result->color=g_strdup(value);
            if (!result->color) {
                gchar *fallback=store_get(work->folder->uri);
                result->color=fallback_color(fallback,result->device,result->inode); g_free(fallback);
            }
        }
        if (work->preview && S_ISDIR(st.st_mode)) {
            if(work->prepare_monitor) {
                result->directory_prepared=TRUE;
                result->directory_fd=open(work->folder->path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
                struct stat pinned;
                if(result->directory_fd>=0 && (fstat(result->directory_fd,&pinned)!=0 ||
                    pinned.st_dev!=st.st_dev || pinned.st_ino!=st.st_ino)) {
                    close(result->directory_fd);result->directory_fd=-1;
                }
            } else {
                result->directory_stale=work->pinned_directory &&
                    !pinned_directory_current(work->folder->path,work->directory_fd);
                if(!result->directory_stale)read_previews(work,result,cancel);
                if(work->pinned_directory && !pinned_directory_current(work->folder->path,work->directory_fd))
                    result->directory_stale=TRUE;
            }
        }
    } else if (work->write_color) result->write_error=g_strdup("資料夾已移動或無法讀取");
    result->cancelled=g_cancellable_is_cancelled(cancel);
    g_task_return_pointer(task,result,result_free);
}

static void show_write_error(Folder *folder, const char *message) {
    GtkWidget *view=g_weak_ref_get(&folder->view);
    GtkWidget *window=view ? gtk_widget_get_toplevel(view) : NULL;
    GtkWidget *dialog=gtk_message_dialog_new(GTK_IS_WINDOW(window)?GTK_WINDOW(window):NULL,
        GTK_DIALOG_DESTROY_WITH_PARENT,GTK_MESSAGE_ERROR,GTK_BUTTONS_CLOSE,"無法儲存資料夾顏色");
    gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(dialog),"%s",message);
    g_signal_connect_swapped(dialog,"response",G_CALLBACK(gtk_widget_destroy),dialog);
    gtk_widget_show(dialog); if (view) g_object_unref(view);
}

static void work_done(GObject *source, GAsyncResult *async, gpointer user_data) {
    (void)source; (void)user_data;
    GTask *task=G_TASK(async); Work *work=g_task_get_task_data(task); Folder *folder=work->folder;
    Result *result=g_task_propagate_pointer(task,NULL);
    folder->loading=FALSE; folder->loading_preview=FALSE;
    g_clear_object(&folder->cancel); active_work--;
    if (result && !result->cancelled && work->generation==folder->generation) {
        if (work->write_color) {
            if (!result->saved) {
                show_write_error(folder,result->write_error?result->write_error:"無法讀取資料夾");
            }
            GHashTableIter iter; gpointer value; g_hash_table_iter_init(&iter,folders);
            while(g_hash_table_iter_next(&iter,NULL,&value)) {
                Folder *instance=value; if(g_strcmp0(instance->uri,folder->uri)!=0)continue;
                instance->generation++; g_free(instance->color);
                instance->color=result->saved?g_strdup(work->write_color):NULL;
                instance->metadata_ready=result->saved; queue_folder(instance);
            }
        } else {
            g_free(folder->color); folder->color=g_strdup(result->color);
            folder->metadata_ready=TRUE;
        }
        if (work->preview && work->content_generation==folder->content_generation) {
            if(result->directory_stale) {
                folder->content_generation++;folder->previews_ready=FALSE;folder->content_ready=FALSE;
                stop_folder_monitor(folder);
            } else if(work->prepare_monitor && result->directory_prepared && folder_visible(folder)) {
                attach_folder_monitor(folder,result);
            } else {
            for (guint i=0; i<PREVIEW_LIMIT; i++) {
                g_clear_object(&folder->previews[i]); g_clear_pointer(&folder->preview_names[i],g_free);
                folder->preview_kinds[i]=NULL;folder->preview_is_thumbnail[i]=FALSE;
            }
            folder->count=result->count;
            folder->eligible_count=result->eligible_count;folder->count_complete=result->count_complete;
            folder->content_ready=result->content_ready;
            for (guint i=0; i<result->count; i++) {
                if (result->previews[i]) folder->previews[i]=g_object_ref(result->previews[i]);
                else {
                    GtkIconInfo *icon=gtk_icon_theme_lookup_by_gicon(gtk_icon_theme_get_default(),result->icons[i],64,0);
                    if (icon) { folder->previews[i]=gtk_icon_info_load_icon(icon,NULL); g_object_unref(icon); }
                }
                folder->preview_names[i]=g_strdup(result->names[i]);
                folder->preview_kinds[i]=result->preview_kinds[i];
                folder->preview_is_thumbnail[i]=result->preview_is_thumbnail[i];
            }
            folder->previews_ready=TRUE; folder->show_hidden=work->show_hidden;
            }
        }
        queue_folder(folder);
    }
    if (result) result_free(result);
    pump_work();
}

static void cancel_folder_thumbnails(GCancellable *parent,gpointer data) {
    (void)parent;g_cancellable_cancel(G_CANCELLABLE(data));
}
static gboolean folder_thumbnail_deadline(gpointer data) {
    Work *work=data;work->thumbnail_timeout=0;g_cancellable_cancel(work->thumbnail_cancel);
    return G_SOURCE_REMOVE;
}
static void start_work(Folder *folder, gboolean preview, const char *write_color) {
    Work *work=g_new0(Work,1); work->folder=folder_ref(folder);
    work->generation=folder->generation; work->preview=preview;
    work->content_generation=folder->content_generation;
    work->prepare_monitor=preview && !folder->monitor_prepared;
    if(preview && folder->monitor_prepared && folder->directory_fd>=0) {
        work->directory_fd=dup(folder->directory_fd);work->pinned_directory=work->directory_fd>=0;
    }
    work->show_hidden=folder->requested_hidden; work->write_color=g_strdup(write_color);
    GCancellable *cancel=g_cancellable_new();
    const gchar *helper=g_getenv("MEADOW_QUICKLOOK_HELPER");
    if(preview && !work->prepare_monitor && helper && *helper &&
        g_strcmp0(g_getenv("MEADOW_FILE_THUMBNAILS"),"0")!=0) {
        work->thumbnail_helper=g_strdup(helper);work->thumbnail_cancel=g_cancellable_new();
        work->parent_cancel=g_object_ref(cancel);
        work->parent_cancel_handler=g_cancellable_connect(cancel,G_CALLBACK(cancel_folder_thumbnails),
            g_object_ref(work->thumbnail_cancel),g_object_unref);
        work->thumbnail_timeout=g_timeout_add(8000,folder_thumbnail_deadline,work);
    }
    GTask *task=g_task_new(NULL,cancel,work_done,NULL);
    g_task_set_check_cancellable(task,FALSE);
    g_task_set_task_data(task,work,work_free);
    folder->loading=TRUE; folder->loading_preview=preview;
    folder->cancel=g_object_ref(cancel); active_work++;
    g_task_run_in_thread(task,worker); g_object_unref(task); g_object_unref(cancel);
}
static void pump_work(void) {
    if (!folders || active_work>=WORK_LIMIT) return;
    GHashTableIter iter; gpointer value;
    g_hash_table_iter_init(&iter,folders);
    while (active_work<WORK_LIMIT && g_hash_table_iter_next(&iter,NULL,&value)) {
        Folder *folder=value;
        if (folder->loading) continue;
        if (folder->pending_color) {
            gchar *color=g_steal_pointer(&folder->pending_color);
            start_work(folder,FALSE,color); g_free(color); continue;
        }
        gboolean preview=folder->requested_preview && !folder->content_debounce && folder_visible(folder) &&
            (!folder->previews_ready || folder->show_hidden!=folder->requested_hidden);
        if (!folder->metadata_ready || preview) start_work(folder,preview,NULL);
    }
}

static gboolean can_evict(Folder *folder) {
    return !folder->loading && !folder->pending_color &&
           (folder->target==0 || folder->last_seen<g_get_monotonic_time()-2000000);
}
static Folder *find_folder(const gchar *uri) {
    GHashTableIter iter; gpointer value; g_hash_table_iter_init(&iter,folders);
    while(g_hash_table_iter_next(&iter,NULL,&value)) {
        Folder *folder=value; if(g_strcmp0(folder->uri,uri)==0)return folder;
    }
    return NULL;
}
static Folder *get_folder(const gchar *uri, const gchar *path, GtkWidget *view) {
    /* Metadata persistence is by URI, interaction state is by view + URI.
     * Another window painting this folder cannot steal its hover/cancel state. */
    gchar *key=g_strdup_printf("%p:%s",(void *)view,uri);
    Folder *folder=g_hash_table_lookup(folders,key);
    if (!folder && g_hash_table_size(folders)>=CACHE_LIMIT) {
        GHashTableIter iter; gpointer value; Folder *oldest=NULL;
        g_hash_table_iter_init(&iter,folders);
        while (g_hash_table_iter_next(&iter,NULL,&value)) {
            Folder *item=value;
            if (can_evict(item) && (!oldest || item->last_seen<oldest->last_seen)) oldest=item;
        }
        if (oldest) g_hash_table_remove(folders,oldest->key);
    }
    if (!folder && g_hash_table_size(folders)<CACHE_LIMIT) {
        folder=g_new0(Folder,1); folder->refs=1;
        folder->directory_fd=-1;
        folder->key=g_strdup(key); folder->uri=g_strdup(uri); folder->path=g_strdup(path);
        g_weak_ref_init(&folder->view,view); g_weak_ref_init(&folder->file_object,NULL);
        g_hash_table_insert(folders,folder->key,folder);
    }
    g_free(key);
    if (folder) folder->last_seen=g_get_monotonic_time();
    return folder;
}

static void bind_file_identity(Folder *folder, GObject *file) {
    GObject *previous=g_weak_ref_get(&folder->file_object);
    if(previous!=file) {
        /* Thunar creates a new File object after delete/recreate at the same
         * URI. Never reuse the previous object's cached color or previews. */
        folder->generation++; folder->metadata_ready=FALSE; folder->previews_ready=FALSE;
        folder->content_generation++;folder->content_ready=FALSE;stop_folder_monitor(folder);
        g_clear_pointer(&folder->entry_stamp,g_free);
        g_clear_pointer(&folder->color,g_free); folder->count=0;
        for(guint i=0;i<PREVIEW_LIMIT;i++) {
            g_clear_object(&folder->previews[i]); g_clear_pointer(&folder->preview_names[i],g_free);
        }
        if(folder->cancel)g_cancellable_cancel(folder->cancel);
        g_weak_ref_set(&folder->file_object,file);
    }
    g_clear_object(&previous);
}
static void bind_folder_metadata(Folder *folder,GObject *file,GFileInfo *info) {
    gchar *stamp=thumbnail_stamp(file,info);
    if(folder->entry_stamp && g_strcmp0(folder->entry_stamp,stamp)!=0) {
        folder->metadata_ready=FALSE;stop_folder_monitor(folder);invalidate_folder_content(folder);
    }
    g_free(folder->entry_stamp);folder->entry_stamp=stamp;
}

static gboolean animation_tick(gpointer data) {
    (void)data; gboolean moving=FALSE; gint64 now=g_get_monotonic_time();
    GHashTableIter iter; gpointer value; g_hash_table_iter_init(&iter,folders);
    while (g_hash_table_iter_next(&iter,NULL,&value)) {
        Folder *folder=value;
        if (folder->progress==folder->target) continue;
        GtkWidget *view=g_weak_ref_get(&folder->view);
        if (!view || !gtk_widget_get_mapped(view)) { folder->progress=folder->target=0; if(view)g_object_unref(view); continue; }
        double duration=folder->target>folder->progress ? 360000.0 : 260000.0;
        double step=MIN(1.0,(now-folder->last_tick)/duration); folder->last_tick=now;
        folder->progress=folder->target>folder->progress ? MIN(folder->target,folder->progress+step) : MAX(folder->target,folder->progress-step);
        g_object_unref(view); queue_folder(folder);
        moving|=folder->progress!=folder->target;
    }
    if (!moving) { animation_source=0; return G_SOURCE_REMOVE; }
    return G_SOURCE_CONTINUE;
}
static void set_target(Folder *folder, GtkWidget *view, gboolean hovered) {
    double target=hovered ? 1.0 : 0.0; gboolean animations=TRUE;
    g_object_get(gtk_widget_get_settings(view),"gtk-enable-animations",&animations,NULL);
    if (folder->target!=target) { folder->target=target; folder->last_tick=g_get_monotonic_time(); }
    if (!animations) { folder->progress=target; return; }
    if (folder->progress!=target && !animation_source) animation_source=g_timeout_add(16,animation_tick,NULL);
}

static gboolean view_show_hidden(GtkWidget *widget) {
    for (GtkWidget *parent=widget; parent; parent=gtk_widget_get_parent(parent)) {
        GParamSpec *prop=g_object_class_find_property(G_OBJECT_GET_CLASS(parent),"show-hidden");
        if (prop && G_PARAM_SPEC_VALUE_TYPE(prop)==G_TYPE_BOOLEAN && (prop->flags&G_PARAM_READABLE)) {
            gboolean hidden=FALSE; g_object_get(parent,"show-hidden",&hidden,NULL); return hidden;
        }
    }
    return FALSE;
}

typedef struct {
    gboolean symlink, hidden, cut, drop, disabled;
} FolderVisualState;

static double folder_opacity(const FolderVisualState *state) {
    double opacity=state->cut ? .5 : state->hidden ? .75 : 1.0;
    return state->disabled ? MIN(opacity,.38) : opacity;
}
static gboolean preview_allowed(const FolderVisualState *state, gboolean hovered) {
    (void)hovered;
    return !state->symlink;
}
static void badge_position(const GdkRectangle *box, guint position, double size,
                            double *x, double *y) {
    *x=box->x+(position==0 || position==3 ? box->width-size-1 : 1);
    *y=box->y+(position<2 ? box->height-size-1 : MAX(1.0,box->height*.28));
}
static void paint_link_badge(cairo_t *cr, const GdkRectangle *box, guint position) {
    double size=CLAMP(box->width*.25,10,25),x,y;
    badge_position(box,position,size,&x,&y);
    cairo_save(cr); cairo_translate(cr,x,y); cairo_scale(cr,size/20.0,size/20.0);
    cairo_arc(cr,10,10,8.6,0,2*G_PI);
    cairo_set_source_rgba(cr,1,1,1,.94); cairo_fill_preserve(cr);
    cairo_set_source_rgba(cr,.16,.24,.32,.28); cairo_set_line_width(cr,.8); cairo_stroke(cr);
    /* The familiar shortcut arrow remains crisp above the frosted body. */
    cairo_set_source_rgba(cr,.12,.24,.36,.95); cairo_set_line_width(cr,1.7);
    cairo_set_line_cap(cr,CAIRO_LINE_CAP_ROUND); cairo_set_line_join(cr,CAIRO_LINE_JOIN_ROUND);
    cairo_move_to(cr,5.8,13.8); cairo_line_to(cr,5.8,9.3);
    cairo_curve_to(cr,5.8,7.6,7.1,6.8,8.7,6.8); cairo_line_to(cr,14,6.8);
    cairo_move_to(cr,11,3.8); cairo_line_to(cr,14,6.8); cairo_line_to(cr,11,9.8); cairo_stroke(cr);
    cairo_restore(cr);
}
static void paint_theme_badge(cairo_t *cr, GtkWidget *widget, const GdkRectangle *box,
                              const char *name, guint position) {
    gint size=CLAMP(box->width/4,10,28),scale=gtk_widget_get_scale_factor(widget);
    GdkPixbuf *icon=gtk_icon_theme_load_icon_for_scale(gtk_icon_theme_get_for_screen(gtk_widget_get_screen(widget)),
        name,size,scale,GTK_ICON_LOOKUP_FORCE_SIZE,NULL);
    if(!icon)return;
    double x,y; badge_position(box,position,size,&x,&y);
    cairo_save(cr); cairo_translate(cr,x,y); cairo_scale(cr,1.0/scale,1.0/scale);
    gdk_cairo_set_source_pixbuf(cr,icon,0,0); cairo_paint(cr); cairo_restore(cr); g_object_unref(icon);
}
static void paint_folder_badges(cairo_t *cr, GtkWidget *widget, const GdkRectangle *box,
                                GFileInfo *info, gboolean symlink, gboolean emblems) {
    guint position=0;
    /* Keep the link's identity visible even when optional custom emblems are
     * disabled. This is an affordance, not part of the folder material. */
    if(symlink)paint_link_badge(cr,box,position++);
    if(!emblems || !info)return;
    gboolean unreadable=(g_file_info_has_attribute(info,G_FILE_ATTRIBUTE_ACCESS_CAN_READ) &&
        !g_file_info_get_attribute_boolean(info,G_FILE_ATTRIBUTE_ACCESS_CAN_READ)) ||
        (g_file_info_has_attribute(info,G_FILE_ATTRIBUTE_ACCESS_CAN_EXECUTE) &&
        !g_file_info_get_attribute_boolean(info,G_FILE_ATTRIBUTE_ACCESS_CAN_EXECUTE));
    if(unreadable)paint_theme_badge(cr,widget,box,"emblem-noread",position++);
    else if(g_file_info_has_attribute(info,G_FILE_ATTRIBUTE_ACCESS_CAN_WRITE) &&
            !g_file_info_get_attribute_boolean(info,G_FILE_ATTRIBUTE_ACCESS_CAN_WRITE) &&
            g_file_info_get_attribute_uint32(info,G_FILE_ATTRIBUTE_UNIX_UID)==geteuid())
        paint_theme_badge(cr,widget,box,"emblem-nowrite",position++);
    if(position<4 && g_file_info_get_attribute_boolean(info,"unix::is-mountpoint"))
        paint_theme_badge(cr,widget,box,"drive-harddisk",position++);
    gchar **names=g_file_info_get_attribute_stringv(info,"metadata::emblems");
    if(names)for(guint i=0;names[i] && position<4;i++)
        if(*names[i])paint_theme_badge(cr,widget,box,names[i],position++);
}
static void paint_drop_ring(cairo_t *cr, const GdkRectangle *box) {
    double x=box->x+1.5,y=box->y+1.5,w=box->width-3,h=box->height-3,r=MIN(9.0,w*.12);
    cairo_new_sub_path(cr); cairo_arc(cr,x+w-r,y+r,r,-G_PI/2,0);
    cairo_arc(cr,x+w-r,y+h-r,r,0,G_PI/2); cairo_arc(cr,x+r,y+h-r,r,G_PI/2,G_PI);
    cairo_arc(cr,x+r,y+r,r,G_PI,3*G_PI/2); cairo_close_path(cr);
    cairo_set_source_rgba(cr,.12,.49,.94,.9); cairo_set_line_width(cr,1.8); cairo_stroke(cr);
}

static gboolean try_file_thumbnail(GtkCellRenderer *renderer,cairo_t *cr,GtkWidget *widget,
                                    GObject *file,const GdkRectangle *cell,GtkCellRendererState flags,
                                    gboolean sensitive,gboolean emblems,gboolean drop) {
    if(g_strcmp0(G_OBJECT_TYPE_NAME(widget),"ExoIconView")!=0 || !g_getenv("MEADOW_QUICKLOOK_HELPER") ||
        g_strcmp0(g_getenv("MEADOW_FILE_THUMBNAILS"),"0")==0)return FALSE;
    gint size=0,xpad=0,ypad=0;g_object_get(renderer,"size",&size,NULL);
    gtk_cell_renderer_get_padding(renderer,&xpad,&ypad);
    size=MIN(size,MIN(cell->width-2*xpad,cell->height-2*ypad));if(size<48)return FALSE;
    gchar *uri=thunarx_file_info_get_uri(THUNARX_FILE_INFO(file)),*path=g_filename_from_uri(uri,NULL,NULL);
    GFileInfo *info=thunarx_file_info_get_file_info(THUNARX_FILE_INFO(file));
    gboolean painted=FALSE;
    if(!path || !thumbnail_extension(path) || (info && g_file_info_get_is_symlink(info)))goto done;
    ensure_thumbnail_view(widget);
    FileThumb *thumb=get_thumbnail(widget,file,uri,path,info);if(!thumb)goto done;
    thumb->cell=*cell;pump_thumbnails();if(!thumb->image)goto done;
    GdkRectangle box={cell->x+(cell->width-size)/2,cell->y+(cell->height-size)/2,size,size};
    FolderVisualState state={.cut=cut_uris && g_hash_table_contains(cut_uris,uri),
        .hidden=info && (g_file_info_get_is_hidden(info)||g_file_info_get_is_backup(info)),
        .drop=drop,.disabled=!sensitive || !gtk_widget_is_sensitive(widget) || (flags&GTK_CELL_RENDERER_INSENSITIVE)};
    double width=gdk_pixbuf_get_width(thumb->image),height=gdk_pixbuf_get_height(thumb->image);
    double scale=MIN((size-4)/width,(size-4)/height),w=width*scale,h=height*scale;
    double x=box.x+(size-w)/2,y=box.y+(size-h)/2;
    cairo_save(cr);cairo_rectangle(cr,box.x,box.y,box.width,box.height);cairo_clip(cr);
    cairo_push_group(cr);cairo_set_source_rgba(cr,.1,.17,.23,.12);
    cairo_rectangle(cr,x-.5,y+1,w+1,h+1);cairo_fill(cr);
    cairo_save(cr);cairo_translate(cr,x,y);cairo_scale(cr,scale,scale);
    gdk_cairo_set_source_pixbuf(cr,thumb->image,0,0);cairo_paint(cr);cairo_restore(cr);
    cairo_rectangle(cr,x-.5,y-.5,w+1,h+1);cairo_set_line_width(cr,1);
    if(flags&GTK_CELL_RENDERER_SELECTED)cairo_set_source_rgba(cr,.14,.47,.91,.78);
    else cairo_set_source_rgba(cr,.17,.23,.29,.2);
    cairo_stroke(cr);cairo_pop_group_to_source(cr);cairo_paint_with_alpha(cr,folder_opacity(&state));
    paint_folder_badges(cr,widget,&box,info,FALSE,emblems);if(drop)paint_drop_ring(cr,&box);
    cairo_restore(cr);painted=TRUE;
done:
    g_clear_object(&info);g_free(uri);g_free(path);return painted;
}

static void publish_diagnostic(GtkWidget *widget, Folder *folder, const GdkRectangle *box) {
    if (!g_getenv("MEADOW_FOLDER_DIAGNOSTICS") || g_get_monotonic_time()-last_diagnostic<100000) return;
    GdkWindow *window=gtk_widget_get_window(gtk_widget_get_toplevel(widget)); if(!window)return;
    const Palette *selected=effective_color(folder->color);
    guint color=selected-palette;
    gulong values[]={1,renderer_hooked,(gulong)(folder->progress*1000),folder->count,color,active_work,
                     box->x,box->y,box->width,box->height,folder->eligible_count,
                     folder->count_complete,folder->content_ready,folder->monitor!=NULL};
    gdk_property_change(window,gdk_atom_intern_static_string("_MEADOW_FOLDER_STATE"),
        gdk_atom_intern_static_string("CARDINAL"),32,GDK_PROP_MODE_REPLACE,(guchar*)values,G_N_ELEMENTS(values));
    gdk_property_change(window,gdk_atom_intern_static_string("_MEADOW_FOLDER_URI"),
        gdk_atom_intern_static_string("UTF8_STRING"),8,GDK_PROP_MODE_REPLACE,(const guchar*)folder->uri,strlen(folder->uri));
    last_diagnostic=g_get_monotonic_time();
}

static void folder_render(GtkCellRenderer *renderer, cairo_t *cr, GtkWidget *widget,
                          const GdkRectangle *background, const GdkRectangle *cell,
                          GtkCellRendererState flags) {
    GObject *object=NULL, *drop_file=NULL; gchar *uri=NULL, *path=NULL;
    GFileInfo *info=NULL; Folder *folder=NULL; gboolean sensitive=TRUE,emblems=TRUE;
    ensure_quicklook(widget);
    if (g_strcmp0(G_OBJECT_TYPE_NAME(widget),"ExoIconView")!=0) goto original;
    g_object_get(renderer,"file",&object,"drop-file",&drop_file,"sensitive",&sensitive,"emblems",&emblems,NULL);
    if (!object || !THUNARX_IS_FILE_INFO(object))goto original;
    if(!thunarx_file_info_is_directory(THUNARX_FILE_INFO(object))) {
        if(try_file_thumbnail(renderer,cr,widget,object,cell,flags,sensitive,emblems,drop_file==object))goto done;
        goto original;
    }
    uri=thunarx_file_info_get_uri(THUNARX_FILE_INFO(object));
    path=g_filename_from_uri(uri,NULL,NULL);
    info=thunarx_file_info_get_file_info(THUNARX_FILE_INFO(object));
    FolderVisualState state={
        .symlink=info && g_file_info_get_is_symlink(info),
        .hidden=info && (g_file_info_get_is_hidden(info)||g_file_info_get_is_backup(info)),
        .cut=cut_uris && g_hash_table_contains(cut_uris,uri), .drop=drop_file==object,
        .disabled=!sensitive || !gtk_widget_is_sensitive(widget) || (flags&GTK_CELL_RENDERER_INSENSITIVE)
    };
    folder=get_folder(uri,path,widget);
    gboolean hovered=(flags&GTK_CELL_RENDERER_PRELIT)!=0;
    if(folder) {
        bind_file_identity(folder,object); g_weak_ref_set(&folder->view,widget); folder->cell=*cell;
        bind_folder_metadata(folder,object,info);
        ensure_folder_view(widget);
        set_target(folder,widget,(hovered||state.drop)&&!state.disabled);
        folder->requested_preview=path && preview_allowed(&state,hovered) && folder_visible(folder);
        if(!path)folder->metadata_ready=TRUE;
        if(folder->requested_preview)folder->requested_hidden=view_show_hidden(widget);
        else if(folder->monitor_prepared || folder->loading_preview)release_folder_content(folder);
        pump_work();
    }
    gint size=0,xpad=0,ypad=0;
    g_object_get(renderer,"size",&size,NULL); gtk_cell_renderer_get_padding(renderer,&xpad,&ypad);
    size=MIN(size,MIN(cell->width-2*xpad,cell->height-2*ypad));
    if (size<8) goto original;
    GdkRectangle box={cell->x+(cell->width-size)/2,cell->y+(cell->height-size)/2,size,size};
    const Palette *color=effective_color(folder?folder->color:NULL);
    GdkRGBA tint; gdk_rgba_parse(&tint,color->hex);
    double progress=folder?folder->progress*folder->progress*(3-2*folder->progress):0;
    cairo_save(cr); cairo_rectangle(cr,box.x,box.y,box.width,box.height); cairo_clip(cr);
    cairo_push_group(cr);
    MeadowFolderContentInfo contents={.ready=folder && folder->content_ready && !state.symlink,
        .item_count=folder?folder->eligible_count:0,.count_complete=folder && folder->count_complete};
    if(folder)for(guint i=0;i<PREVIEW_LIMIT;i++) {
        contents.kinds[i]=folder->preview_kinds[i];contents.is_thumbnail[i]=folder->preview_is_thumbnail[i];
    }
    meadow_folder_paint(cr,&box,progress,&tint,folder?folder->previews:NULL,
                        folder && !state.symlink?folder->count:0,
                        (flags&GTK_CELL_RENDERER_SELECTED)!=0,hovered||state.drop,&contents);
    cairo_pop_group_to_source(cr); cairo_paint_with_alpha(cr,folder_opacity(&state));
    paint_folder_badges(cr,widget,&box,info,state.symlink,emblems);
    if(state.drop)paint_drop_ring(cr,&box);
    cairo_restore(cr);
    if(hovered && folder)publish_diagnostic(widget,folder,&box);
    goto done;
original:
    original_render(renderer,cr,widget,background,cell,flags);
done:
    g_clear_object(&info); g_clear_object(&object); g_clear_object(&drop_file); g_free(uri); g_free(path);
}

static void clipboard_received(GtkClipboard *clipboard, GtkSelectionData *selection, gpointer data) {
    (void)clipboard; (void)data;
    g_hash_table_remove_all(cut_uris);
    gint length=gtk_selection_data_get_length(selection);
    if (length>0 && length<=1024*1024) {
        gchar *text=g_strndup((const char *)gtk_selection_data_get_data(selection),length);
        gchar **lines=g_strsplit(text,"\n",-1);
        if (g_strcmp0(g_strstrip(lines[0]),"cut")==0)
            for (guint i=1; lines[i]; i++) if (*g_strstrip(lines[i])) g_hash_table_add(cut_uris,g_strdup(lines[i]));
        g_strfreev(lines); g_free(text);
    }
    queue_all();
}
static void clipboard_changed(GtkClipboard *clipboard, GdkEventOwnerChange *event, gpointer data) {
    (void)event; (void)data;
    gtk_clipboard_request_contents(clipboard,gdk_atom_intern_static_string("x-special/gnome-copied-files"),clipboard_received,NULL);
}
static gboolean install_renderer(gpointer data) {
    (void)data;
    GType type=g_type_from_name("ThunarIconRenderer");
    if (!type) return G_SOURCE_CONTINUE;
    GtkCellRendererClass *klass=g_type_class_ref(type);
    if (g_object_class_find_property(G_OBJECT_CLASS(klass),"file") &&
        g_object_class_find_property(G_OBJECT_CLASS(klass),"size")) {
        original_render=klass->render; klass->render=folder_render; renderer_hooked=TRUE;
    }
    g_type_class_unref(klass); install_source=0;
    GtkClipboard *clipboard=gtk_clipboard_get(GDK_SELECTION_CLIPBOARD);
    g_signal_connect(clipboard,"owner-change",G_CALLBACK(clipboard_changed),NULL);
    clipboard_changed(clipboard,NULL,NULL);
    return G_SOURCE_REMOVE;
}
static void initialize(void) {
    if (initialized) return;
    initialized=TRUE;
    folders=g_hash_table_new_full(g_str_hash,g_str_equal,NULL,folder_unref);
    cut_uris=g_hash_table_new_full(g_str_hash,g_str_equal,g_free,NULL);
    guint show_signal=g_signal_lookup("show",GTK_TYPE_WIDGET);
    if(show_signal)g_signal_add_emission_hook(show_signal,0,quicklook_show_hook,NULL,NULL);
    GList *windows=gtk_window_list_toplevels();
    for(GList *item=windows;item;item=item->next)ensure_quicklook(item->data);
    g_list_free(windows);
    install_source=g_timeout_add(40,install_renderer,NULL);
}
static gboolean effects_enabled(void) {
    return g_strcmp0(g_getenv("MEADOW_FOLDER_EFFECTS"),"0")!=0 &&
           g_strcmp0(g_getenv("MEADOW_WINDOW_EFFECTS"),"0")!=0;
}

typedef struct { GObject parent; } MeadowFolderProvider;
typedef struct { GObjectClass parent; } MeadowFolderProviderClass;
static void provider_interface_init(ThunarxMenuProviderIface *iface);
G_DEFINE_TYPE_WITH_CODE(MeadowFolderProvider,meadow_folder_provider,G_TYPE_OBJECT,
    G_IMPLEMENT_INTERFACE(THUNARX_TYPE_MENU_PROVIDER,provider_interface_init))
static void meadow_folder_provider_class_init(MeadowFolderProviderClass *klass) { (void)klass; }
static void meadow_folder_provider_init(MeadowFolderProvider *self) { (void)self; initialize(); }

typedef struct { GPtrArray *uris; const char *color; GWeakRef window; } ColorAction;
static void color_action_free(gpointer data, GClosure *closure) {
    (void)closure; ColorAction *action=data; g_ptr_array_unref(action->uris); g_weak_ref_clear(&action->window); g_free(action);
}
static gboolean can_accept_colors(GPtrArray *uris) {
    guint required=0,available=CACHE_LIMIT-g_hash_table_size(folders);
    GHashTableIter iter; gpointer value;
    g_hash_table_iter_init(&iter,folders);
    while(g_hash_table_iter_next(&iter,NULL,&value)) {
        Folder *folder=value;
        if(can_evict(folder) && !g_ptr_array_find_with_equal_func(uris,folder->uri,(GEqualFunc)g_str_equal,NULL))available++;
    }
    for(guint i=0;i<uris->len;i++) if(!find_folder(g_ptr_array_index(uris,i)))required++;
    return uris->len<=CACHE_LIMIT && required<=available;
}
static void choose_color(ThunarxMenuItem *item, gpointer data) {
    (void)item; ColorAction *action=data;
    GHashTableIter iter; gpointer value;
    if(!can_accept_colors(action->uris)) {
        GtkWidget *window=g_weak_ref_get(&action->window);
        GtkWidget *dialog=gtk_message_dialog_new(GTK_IS_WINDOW(window)?GTK_WINDOW(window):NULL,
            GTK_DIALOG_DESTROY_WITH_PARENT,GTK_MESSAGE_INFO,GTK_BUTTONS_CLOSE,
            "請減少選取的資料夾後再試一次");
        gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(dialog),
            "每次最多可調整 %d 個資料夾。若仍在儲存，請稍候再試；本次尚未變更任何顏色。",CACHE_LIMIT);
        g_signal_connect_swapped(dialog,"response",G_CALLBACK(gtk_widget_destroy),dialog);
        gtk_widget_show(dialog); if(window)g_object_unref(window); return;
    }
    for (guint i=0;i<action->uris->len;i++) {
        const gchar *uri=g_ptr_array_index(action->uris,i); gchar *path=g_filename_from_uri(uri,NULL,NULL);
        if (!path) continue;
        Folder *folder=find_folder(uri); if(!folder)folder=get_folder(uri,path,NULL);
        g_free(path); if(!folder)continue;
        /* The requested color supersedes older metadata/preview completions. */
        g_hash_table_iter_init(&iter,folders);
        while(g_hash_table_iter_next(&iter,NULL,&value)) {
            Folder *instance=value; if(g_strcmp0(instance->uri,uri)!=0)continue;
            instance->generation++; g_free(instance->color); instance->color=g_strdup(action->color);
            instance->metadata_ready=TRUE; queue_folder(instance);
        }
        g_free(folder->pending_color); folder->pending_color=g_strdup(action->color);
        queue_folder(folder);
    }
    pump_work();
}
static GList *file_menu(ThunarxMenuProvider *provider, GtkWidget *window, GList *files) {
    (void)provider;
    GPtrArray *uris=g_ptr_array_new_with_free_func(g_free);
    for(GList *item=files;item;item=item->next) {
        if (!THUNARX_IS_FILE_INFO(item->data) || !thunarx_file_info_is_directory(item->data)) continue;
        gchar *uri=thunarx_file_info_get_uri(item->data), *path=g_filename_from_uri(uri,NULL,NULL);
        if(path)g_ptr_array_add(uris,uri); else g_free(uri); g_free(path);
    }
    if (!uris->len) { g_ptr_array_unref(uris); return NULL; }
    /* GTK3 otherwise suppresses the color swatches on image menu items.
     * This setting belongs to this Files process; do not change Xsettings or
     * another application's preferences just to expose the native palette. */
    if (GTK_IS_WIDGET(window))
        g_object_set(gtk_widget_get_settings(window),"gtk-menu-images",TRUE,NULL);
    ThunarxMenuItem *root=thunarx_menu_item_new("MeadowFolder::colors","資料夾顏色","選擇資料夾玻璃顏色",NULL);
    ThunarxMenu *menu=thunarx_menu_new(); thunarx_menu_item_set_menu(root,menu);
    const Palette *common=NULL; gboolean known=TRUE;
    for(guint i=0;i<uris->len;i++) {
        Folder *folder=find_folder(g_ptr_array_index(uris,i));
        if(!folder || !folder->metadata_ready){known=FALSE;break;}
        const Palette *color=effective_color(folder->color);
        if(i==0)common=color; else if(common!=color){known=FALSE;break;}
    }
    for(guint i=0;i<G_N_ELEMENTS(palette);i++) {
        gchar *name=g_strdup_printf("MeadowFolder::%s",palette[i].name);
        gchar *label=g_strdup_printf("%s%s",known && common==&palette[i]?"✓  ":"",palette[i].label);
        gchar *icon=g_strdup_printf("meadow-folder-color-%s",palette[i].name);
        ThunarxMenuItem *child=thunarx_menu_item_new(name,label,NULL,icon);
        g_free(name); g_free(label); g_free(icon);
        ColorAction *action=g_new0(ColorAction,1); action->uris=g_ptr_array_ref(uris);
        action->color=palette[i].name; g_weak_ref_init(&action->window,window);
        g_signal_connect_data(child,"activate",G_CALLBACK(choose_color),action,color_action_free,0);
        thunarx_menu_append_item(menu,child); g_object_unref(child);
    }
    g_object_unref(menu); g_ptr_array_unref(uris); return g_list_append(NULL,root);
}
static void provider_interface_init(ThunarxMenuProviderIface *iface) { iface->get_file_menu_items=file_menu; }

G_MODULE_EXPORT void gtk_module_init(gint *argc, gchar ***argv) {
    (void)argc; (void)argv;
    /* This module is inherited by other GTK apps; leave them entirely alone. */
    const gchar *program=g_get_prgname();
    if(effects_enabled() && program && (g_strcmp0(program,"thunar")==0 || g_strcmp0(program,"Thunar")==0)) initialize();
}
G_MODULE_EXPORT void thunar_extension_initialize(ThunarxProviderPlugin *plugin) {
    if(!effects_enabled())return;
    thunarx_provider_plugin_set_resident(plugin,TRUE);
    meadow_folder_provider_get_type(); initialize();
}
G_MODULE_EXPORT void thunar_extension_shutdown(void) { /* Resident render vfunc owns process lifetime. */ }
G_MODULE_EXPORT void thunar_extension_list_types(const GType **types, gint *count) {
    if(!effects_enabled()){*types=NULL;*count=0;return;}
    static GType provided[1]; provided[0]=meadow_folder_provider_get_type(); *types=provided; *count=1;
}
