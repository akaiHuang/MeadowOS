/* Headless filesystem/metadata probe; never opens or drives a GUI. */
#define _DEFAULT_SOURCE 1
#include <unistd.h>
#include <fcntl.h>
#include "../meadow_folder_materials.c"

static gchar *fixture;
static gchar *directory(const char *name) {
    gchar *path=g_build_filename(fixture,name,NULL);
    g_assert_cmpint(g_mkdir_with_parents(path,0700),==,0); return path;
}
static void file_at(const gchar *directory_path,const char *name) {
    gchar *path=g_build_filename(directory_path,name,NULL);
    g_assert_true(g_file_set_contents(path,"real fixture\n",-1,NULL)); g_free(path);
}
static Result *run_worker(Folder *folder, const char *color) {
    GCancellable *cancel=g_cancellable_new(); GTask *task=g_task_new(NULL,cancel,NULL,NULL);
    Work work={.folder=folder,.write_color=(char *)color};
    worker(task,NULL,&work,cancel);
    Result *result=g_task_propagate_pointer(task,NULL);
    g_object_unref(task); g_object_unref(cancel); return result;
}
static void colors_validated(void) {
    g_assert_nonnull(color_by_name("frost")); g_assert_nonnull(color_by_name("graphite"));
    g_assert_null(color_by_name("#bad")); g_assert_null(color_by_name("../escape"));
    g_assert_null(color_by_name(NULL));
    g_assert_cmpstr(effective_color(NULL)->name,==,"blue");
    g_assert_cmpstr(effective_color("unknown")->name,==,"blue");
    g_assert_cmpstr(effective_color("frost")->name,==,"frost");
    g_assert_cmpstr(effective_color("lilac")->name,==,"lilac");
}
static void fallback_identity(void) {
    g_assert_true(store_set("file:///fixture","mint",34,56,NULL));
    gchar *encoded=store_get("file:///fixture"); gchar *value=fallback_color(encoded,34,56);
    g_assert_cmpstr(value,==,"mint"); g_free(value);
    g_assert_null(fallback_color(encoded,34,57)); g_assert_null(fallback_color(encoded,35,56));
    g_free(encoded); g_assert_true(store_set("file:///fixture",NULL,0,0,NULL));
    g_assert_null(store_get("file:///fixture"));
    g_assert_null(fallback_color("rose:1:2:extra",1,2));
}
static void xattr_survives_rename(void) {
    gchar *path=directory("color-before"), *renamed=g_build_filename(fixture,"color-after",NULL);
    Folder folder={.path=path}; folder.uri=g_filename_to_uri(path,NULL,NULL);
    Result *written=run_worker(&folder,"lilac"); g_assert_true(written->saved); result_free(written);
    gchar value[40]={0}; g_assert_cmpint(lgetxattr(path,XATTR_NAME,value,sizeof(value)-1),>,0);
    g_assert_cmpstr(value,==,"lilac");
    g_assert_cmpint(g_rename(path,renamed),==,0);
    g_free(folder.uri); folder.path=renamed; folder.uri=g_filename_to_uri(renamed,NULL,NULL);
    Result *read=run_worker(&folder,NULL); g_assert_cmpstr(read->color,==,"lilac"); result_free(read);
    g_free(folder.uri); g_free(path); g_free(renamed);
}
static Result *preview(const gchar *path,gboolean hidden,gboolean cancelled) {
    Folder folder={.path=(gchar *)path}; Work work={.folder=&folder,.show_hidden=hidden};
    GCancellable *cancel=g_cancellable_new(); if(cancelled)g_cancellable_cancel(cancel);
    Result *result=g_new0(Result,1); read_previews(&work,result,cancel); g_object_unref(cancel); return result;
}
static void empty_has_no_cards(void) {
    gchar *path=directory("empty"); Result *result=preview(path,FALSE,FALSE);
    g_assert_cmpuint(result->count,==,0); g_assert_cmpuint(result->eligible_count,==,0);
    g_assert_true(result->content_ready); g_assert_true(result->count_complete);
    result_free(result); g_free(path);
}
static void previews_are_real_and_bounded(void) {
    gchar *path=directory("content");
    file_at(path,"d.txt"); file_at(path,"b.txt"); file_at(path,"a.txt"); file_at(path,"c.txt");
    Result *result=preview(path,FALSE,FALSE); g_assert_cmpuint(result->count,==,3);
    g_assert_cmpuint(result->eligible_count,==,4); g_assert_true(result->count_complete);
    g_assert_cmpstr(result->names[0],==,"a.txt"); g_assert_cmpstr(result->names[2],==,"c.txt");
    result_free(result); g_free(path);
}
static void hidden_respects_view(void) {
    gchar *path=directory("hidden"); file_at(path,".hidden.txt"); file_at(path,"visible.txt"); file_at(path,"backup.txt~");
    Result *result=preview(path,FALSE,FALSE); g_assert_cmpuint(result->count,==,1);
    g_assert_cmpstr(result->names[0],==,"visible.txt");
    g_assert_cmpuint(result->eligible_count,==,1); g_assert_true(result->count_complete); result_free(result);
    result=preview(path,TRUE,FALSE); g_assert_cmpuint(result->count,==,3);
    g_assert_cmpuint(result->eligible_count,==,3); g_assert_true(result->content_ready);
    g_assert_cmpstr(result->names[0],==,".hidden.txt"); result_free(result); g_free(path);
    path=directory("hidden-only"); file_at(path,".hidden.txt"); file_at(path,"backup.txt~");
    result=preview(path,FALSE,FALSE); g_assert_cmpuint(result->eligible_count,==,0);
    g_assert_true(result->content_ready); g_assert_true(result->count_complete); result_free(result); g_free(path);
}
static void shortcut_has_no_target_preview_or_color_write(void) {
    gchar *target=directory("shortcut-target"), *link=g_build_filename(fixture,"shortcut",NULL);
    file_at(target,"actual.txt"); g_assert_cmpint(symlink(target,link),==,0);
    Result *result=preview(link,FALSE,FALSE); g_assert_cmpuint(result->count,==,0);
    g_assert_false(result->content_ready); g_assert_false(result->count_complete); result_free(result);
    Folder folder={.path=link}; folder.uri=g_filename_to_uri(link,NULL,NULL);
    result=run_worker(&folder,"rose"); g_assert_true(result->saved); result_free(result);
    result=run_worker(&folder,NULL); g_assert_cmpstr(result->color,==,"rose"); result_free(result);
    gchar value[40]={0}; g_assert_cmpint(lgetxattr(target,XATTR_NAME,value,sizeof(value)-1),==,-1);
    g_assert_true(errno==ENODATA || errno==ENOTSUP);
    gchar *file=g_build_filename(target,"actual.txt",NULL), *contents=NULL;
    g_assert_true(g_file_get_contents(file,&contents,NULL,NULL)); g_assert_cmpstr(contents,==,"real fixture\n");
    g_free(contents); g_free(file); g_free(folder.uri); g_free(target); g_free(link);
}
static void folder_states_preserve_affordances(void) {
    FolderVisualState normal={0}; g_assert_cmpfloat(folder_opacity(&normal),==,1.0);
    g_assert_true(preview_allowed(&normal,TRUE)); g_assert_true(preview_allowed(&normal,FALSE));
    FolderVisualState link={.symlink=TRUE}; g_assert_false(preview_allowed(&link,TRUE));
    FolderVisualState cut={.cut=TRUE}; g_assert_cmpfloat(folder_opacity(&cut),==,.5);
    FolderVisualState hidden={.hidden=TRUE}; g_assert_cmpfloat(folder_opacity(&hidden),==,.75);
    FolderVisualState disabled={.disabled=TRUE}; g_assert_cmpfloat(folder_opacity(&disabled),<,.5);
    g_assert_true(preview_allowed(&disabled,FALSE));
    FolderVisualState drop={.drop=TRUE}; g_assert_true(preview_allowed(&drop,FALSE));
}
static void actual_counts_are_not_card_counts(void) {
    const guint counts[]={1,2,5,200,1100};
    for(guint case_index=0;case_index<G_N_ELEMENTS(counts);case_index++) {
        guint count=counts[case_index]; gchar *name=g_strdup_printf("actual-count-%u",count);
        gchar *path=directory(name); g_free(name);
        for(guint i=0;i<count;i++) {
            name=g_strdup_printf("actual-%04u.txt",i); file_at(path,name); g_free(name);
        }
        Result *result=preview(path,FALSE,FALSE);
        g_assert_true(result->content_ready);
        g_assert_cmpuint(result->count,==,MIN(count,3));
        g_assert_cmpuint(result->eligible_count,==,MIN(count,1024));
        g_assert_cmpint(result->count_complete,==,count<=1024);
        result_free(result); g_free(path);
    }
}
static void representatives_cover_real_kinds(void) {
    gchar *path=directory("representative-kinds");
    file_at(path,"00-a.txt"); file_at(path,"01-b.txt"); file_at(path,"02-c.txt");
    gchar *image_path=g_build_filename(path,"10-image.png",NULL);
    GdkPixbuf *pixels=gdk_pixbuf_new(GDK_COLORSPACE_RGB,FALSE,8,32,32);
    gdk_pixbuf_fill(pixels,0x44bb66ff);
    g_assert_true(gdk_pixbuf_save(pixels,image_path,"png",NULL,NULL));
    g_object_unref(pixels); g_free(image_path);
    gchar *subdir=g_build_filename(path,"20-subfolder",NULL);
    g_assert_cmpint(g_mkdir(subdir,0700),==,0); g_free(subdir);
    Result *result=preview(path,FALSE,FALSE);
    g_assert_cmpuint(result->eligible_count,==,5); g_assert_cmpuint(result->count,==,3);
    g_assert_true(result->content_ready); g_assert_true(result->count_complete);
    g_assert_cmpstr(result->names[0],==,"00-a.txt");
    g_assert_cmpstr(result->names[1],==,"10-image.png");
    g_assert_cmpstr(result->names[2],==,"20-subfolder");
    g_assert_cmpstr(result->preview_kinds[0],==,"text");
    g_assert_cmpstr(result->preview_kinds[1],==,"image");
    g_assert_cmpstr(result->preview_kinds[2],==,"folder");
    g_assert_false(result->preview_is_thumbnail[0]);
    g_assert_true(result->preview_is_thumbnail[1]);
    g_assert_false(result->preview_is_thumbnail[2]);
    g_assert_nonnull(result->previews[1]);
    result_free(result); g_free(path);
}
static void unreadable_is_unknown_not_empty(void) {
    if(geteuid()==0) { g_test_skip("EACCES fixture requires an unprivileged test owner"); return; }
    gchar *path=directory("unreadable"); file_at(path,"actual.txt");
    g_assert_cmpint(g_chmod(path,0000),==,0);
    Result *result=preview(path,FALSE,FALSE);
    g_assert_false(result->content_ready); g_assert_false(result->count_complete);
    g_assert_cmpuint(result->count,==,0); result_free(result);
    g_assert_cmpint(g_chmod(path,0700),==,0); g_free(path);
}
static void content_invalidation_preserves_color_and_releases_resources(void) {
    Folder folder={.generation=11,.content_generation=17,.metadata_ready=TRUE,
        .previews_ready=TRUE,.content_ready=TRUE,.requested_preview=TRUE,
        .directory_fd=-1,.color=g_strdup("lilac"),.cancel=g_cancellable_new()};
    g_weak_ref_init(&folder.view,NULL);
    invalidate_folder_content(&folder);
    g_assert_cmpuint(folder.generation,==,11);
    g_assert_cmpuint(folder.content_generation,==,18);
    g_assert_true(folder.metadata_ready); g_assert_cmpstr(folder.color,==,"lilac");
    g_assert_false(folder.previews_ready); g_assert_false(folder.content_ready);
    g_assert_false(g_cancellable_is_cancelled(folder.cancel));
    guint prior=folder.content_debounce; g_assert_cmpuint(prior,>,0);
    invalidate_folder_content(&folder);
    g_assert_cmpuint(folder.content_generation,==,19);
    g_assert_null(g_main_context_find_source_by_id(NULL,prior));
    g_assert_nonnull(g_main_context_find_source_by_id(NULL,folder.content_debounce));
    folder.loading_preview=TRUE;
    invalidate_folder_content(&folder);
    g_assert_true(g_cancellable_is_cancelled(folder.cancel));
    g_assert_cmpuint(folder.generation,==,11);
    gchar *path=directory("monitor-cleanup");
    folder.directory_fd=open(path,O_RDONLY|O_DIRECTORY|O_CLOEXEC);
    g_assert_cmpint(folder.directory_fd,>=,0); gint pinned=folder.directory_fd;
    folder.monitor_prepared=TRUE;
    GFile *file=g_file_new_for_path(path);
    folder.monitor=g_file_monitor_directory(file,G_FILE_MONITOR_NONE,NULL,NULL);
    g_assert_nonnull(folder.monitor);
    GFileMonitor *monitor=g_object_ref(folder.monitor);
    release_folder_content(&folder);
    g_assert_false(folder.requested_preview); g_assert_false(folder.monitor_prepared);
    g_assert_cmpuint(folder.content_debounce,==,0); g_assert_null(folder.monitor);
    g_assert_cmpint(folder.directory_fd,==,-1);
    g_assert_true(g_file_monitor_is_cancelled(monitor));
    errno=0; g_assert_cmpint(fcntl(pinned,F_GETFD),==,-1); g_assert_cmpint(errno,==,EBADF);
    g_assert_cmpuint(folder.generation,==,11); g_assert_true(folder.metadata_ready);
    g_object_unref(monitor); g_object_unref(file); g_object_unref(folder.cancel);
    g_weak_ref_clear(&folder.view); g_free(folder.color); g_free(path);
}
static void pinned_directory_rejects_replaced_path(void) {
    gchar *path=directory("pin-original"),*moved=g_build_filename(fixture,"pin-moved",NULL);
    file_at(path,"old.txt");
    int old_fd=open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    g_assert_cmpint(old_fd,>=,0); g_assert_true(pinned_directory_current(path,old_fd));
    g_assert_cmpint(g_rename(path,moved),==,0);
    g_assert_cmpint(g_mkdir(path,0700),==,0);
    g_assert_false(pinned_directory_current(path,old_fd));
    g_assert_true(pinned_directory_current(moved,old_fd));
    int new_fd=open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    g_assert_cmpint(new_fd,>=,0); g_assert_true(pinned_directory_current(path,new_fd));
    g_assert_cmpint(g_rmdir(path),==,0);
    g_assert_cmpint(symlink(moved,path),==,0);
    g_assert_false(pinned_directory_current(path,old_fd));
    g_assert_false(pinned_directory_current(path,new_fd));
    close(new_fd); close(old_fd); g_free(moved); g_free(path);
}
static gchar *preview_pixels_digest(GdkPixbuf *image) {
    GChecksum *sum=g_checksum_new(G_CHECKSUM_SHA256);
    gint stride=gdk_pixbuf_get_rowstride(image);
    gsize row_bytes=(gsize)gdk_pixbuf_get_width(image)*gdk_pixbuf_get_n_channels(image);
    const guchar *pixels=gdk_pixbuf_read_pixels(image);
    for(gint y=0;y<gdk_pixbuf_get_height(image);y++)g_checksum_update(sum,pixels+y*stride,row_bytes);
    gchar *digest=g_strdup(g_checksum_get_string(sum));g_checksum_free(sum);return digest;
}
static Result *actual_content_preview(const gchar *path,const gchar *helper,gboolean exhausted) {
    Folder folder={.path=(gchar *)path};folder.uri=g_filename_to_uri(path,NULL,NULL);
    GCancellable *cancel=g_cancellable_new(),*thumbnail_cancel=g_cancellable_new();
    if(exhausted)g_cancellable_cancel(thumbnail_cancel);
    Work work={.folder=&folder,.thumbnail_helper=(gchar *)helper,.thumbnail_cancel=thumbnail_cancel};
    Result *result=g_new0(Result,1);read_previews(&work,result,cancel);
    g_assert_false(g_cancellable_is_cancelled(cancel));
    g_object_unref(thumbnail_cancel);g_object_unref(cancel);g_free(folder.uri);return result;
}
static void real_document_contents_refresh_after_edit(void) {
    const gchar *helper=g_getenv("MEADOW_PROBE_REAL_HELPER"),*pdf=g_getenv("MEADOW_PROBE_CONTENT_PDF");
    if(!helper || !pdf) {g_test_skip("Set real helper and synthetic PDF paths for actual decoder integration");return;}
    g_assert_true(g_file_test(helper,G_FILE_TEST_IS_REGULAR));
    gchar *path=directory("actual-document-contents"),*source=NULL;gsize length=0;
    g_assert_true(g_file_get_contents(pdf,&source,&length,NULL));
    g_assert_cmpuint(length,>,100);g_assert_cmpuint(length,<,1024*1024);
    g_assert_true(g_str_has_prefix(source,"%PDF-"));
    gchar *pdf_copy=g_build_filename(path,"00-real.pdf",NULL);
    g_assert_true(g_file_set_contents(pdf_copy,source,length,NULL));g_free(source);
    gchar *text=g_build_filename(path,"01-real.txt",NULL);
    g_assert_true(g_file_set_contents(text,"SYNTHETIC QA CONTENT ALPHA\nFirst actual document version\n",-1,NULL));
    struct stat before,after;g_assert_cmpint(lstat(text,&before),==,0);
    gchar *uri=g_filename_to_uri(text,NULL,NULL),*cache_before=thumbnail_cache_path(uri,&before);
    Result *first=actual_content_preview(path,helper,FALSE);
    g_assert_true(first->content_ready);g_assert_cmpuint(first->eligible_count,==,2);
    g_assert_cmpuint(first->count,==,2);g_assert_true(first->count_complete);
    g_assert_cmpstr(first->preview_kinds[0],==,"pdf");g_assert_cmpstr(first->preview_kinds[1],==,"text");
    g_assert_true(first->preview_is_thumbnail[0]);g_assert_true(first->preview_is_thumbnail[1]);
    g_assert_nonnull(first->previews[0]);g_assert_nonnull(first->previews[1]);
    g_assert_true(g_file_test(cache_before,G_FILE_TEST_IS_REGULAR));
    gchar *pdf_digest=preview_pixels_digest(first->previews[0]);
    gchar *text_digest=preview_pixels_digest(first->previews[1]);
    gchar *proof=g_build_filename(fixture,"actual-pdf-preview.png",NULL);
    g_assert_true(gdk_pixbuf_save(first->previews[0],proof,"png",NULL,NULL));g_free(proof);
    proof=g_build_filename(fixture,"actual-text-before.png",NULL);
    g_assert_true(gdk_pixbuf_save(first->previews[1],proof,"png",NULL,NULL));g_free(proof);
    result_free(first);
    const gchar *updated="SYNTHETIC QA CONTENT BETA\nCHANGED DOCUMENT CONTENT\nFresh text must replace the prior pixels.\n";
    int fd=open(text,O_WRONLY|O_TRUNC|O_NOFOLLOW|O_CLOEXEC);g_assert_cmpint(fd,>=,0);
    g_assert_cmpint(write(fd,updated,strlen(updated)),==,(gssize)strlen(updated));
    g_assert_cmpint(fsync(fd),==,0);close(fd);g_assert_cmpint(lstat(text,&after),==,0);
    g_assert_cmpuint(before.st_ino,==,after.st_ino);
    gchar *cache_after=thumbnail_cache_path(uri,&after);g_assert_cmpstr(cache_before,!=,cache_after);
    Result *second=actual_content_preview(path,helper,FALSE);
    g_assert_cmpuint(second->eligible_count,==,2);g_assert_cmpuint(second->count,==,2);
    g_assert_true(second->preview_is_thumbnail[0]);g_assert_true(second->preview_is_thumbnail[1]);
    gchar *new_pdf_digest=preview_pixels_digest(second->previews[0]);
    gchar *new_text_digest=preview_pixels_digest(second->previews[1]);
    g_assert_cmpstr(pdf_digest,==,new_pdf_digest);g_assert_cmpstr(text_digest,!=,new_text_digest);
    g_assert_true(g_file_test(cache_after,G_FILE_TEST_IS_REGULAR));
    proof=g_build_filename(fixture,"actual-text-after.png",NULL);
    g_assert_true(gdk_pixbuf_save(second->previews[1],proof,"png",NULL,NULL));g_free(proof);
    g_test_message("Actual PDF/text decoder artifacts: %s; text inode retained and cache/pixels refreshed",fixture);
    result_free(second);
    Result *exhausted=actual_content_preview(path,helper,TRUE);
    g_assert_true(exhausted->content_ready);g_assert_true(exhausted->count_complete);
    g_assert_cmpuint(exhausted->eligible_count,==,2);g_assert_cmpuint(exhausted->count,==,2);
    g_assert_false(exhausted->preview_is_thumbnail[0]);g_assert_false(exhausted->preview_is_thumbnail[1]);
    g_assert_nonnull(exhausted->icons[0]);g_assert_nonnull(exhausted->icons[1]);result_free(exhausted);
    g_free(pdf_digest);g_free(text_digest);g_free(new_pdf_digest);g_free(new_text_digest);
    g_free(cache_before);g_free(cache_after);g_free(uri);g_free(text);g_free(pdf_copy);g_free(path);
}
static void link_badge_stays_inside_glyph(void) {
    cairo_surface_t *surface=cairo_image_surface_create(CAIRO_FORMAT_ARGB32,128,128);
    cairo_t *cr=cairo_create(surface); GdkRectangle box={16,16,96,96}; paint_link_badge(cr,&box,0);
    cairo_surface_flush(surface); guint32 *pixels=(guint32 *)cairo_image_surface_get_data(surface);
    guint filled=0;
    for(guint y=0;y<128;y++)for(guint x=0;x<128;x++) {
        guint alpha=pixels[y*128+x]>>24;
        if(x<16 || y<16 || x>=112 || y>=112)g_assert_cmpuint(alpha,==,0);
        else if(alpha)filled++;
    }
    g_assert_cmpuint(filled,>,50); cairo_destroy(cr); cairo_surface_destroy(surface);
}
static void symlinks_are_not_followed(void) {
    gchar *path=directory("links"), *outside=directory("outside"); file_at(outside,"secret.txt");
    gchar *link=g_build_filename(path,"external",NULL); g_assert_cmpint(symlink(outside,link),==,0); g_free(link);
    link=g_build_filename(path,"loop",NULL); g_assert_cmpint(symlink(path,link),==,0); g_free(link);
    file_at(path,"visible.txt"); Result *result=preview(path,FALSE,FALSE);
    g_assert_true(result->content_ready);g_assert_true(result->count_complete);
    g_assert_cmpuint(result->eligible_count,==,3);g_assert_cmpuint(result->count,==,3);
    g_assert_cmpstr(result->names[0],==,"external");g_assert_cmpstr(result->names[1],==,"visible.txt");
    g_assert_cmpstr(result->names[2],==,"loop");
    g_assert_cmpstr(result->preview_kinds[0],==,"generic");g_assert_cmpstr(result->preview_kinds[2],==,"generic");
    g_assert_false(result->preview_is_thumbnail[0]);g_assert_false(result->preview_is_thumbnail[2]);
    result_free(result);g_free(path);
    path=directory("visible-link-only");link=g_build_filename(path,"linked.png",NULL);
    g_assert_cmpint(symlink(outside,link),==,0);g_free(link);
    result=preview(path,FALSE,FALSE);
    g_assert_true(result->content_ready);g_assert_true(result->count_complete);
    g_assert_cmpuint(result->eligible_count,==,1);g_assert_cmpuint(result->count,==,1);
    g_assert_cmpstr(result->preview_kinds[0],==,"generic");g_assert_false(result->preview_is_thumbnail[0]);
    g_assert_nonnull(result->icons[0]);result_free(result);
    gchar *fifo=g_build_filename(path,"pipe.pdf",NULL);g_assert_cmpint(mkfifo(fifo,0600),==,0);g_free(fifo);
    const gchar *helper=g_getenv("MEADOW_PROBE_REAL_HELPER");
    result=helper ? actual_content_preview(path,helper,FALSE) : preview(path,FALSE,FALSE);
    g_assert_true(result->content_ready);g_assert_true(result->count_complete);
    g_assert_cmpuint(result->eligible_count,==,2);g_assert_cmpuint(result->count,==,2);
    g_assert_cmpstr(result->preview_kinds[0],==,"generic");g_assert_cmpstr(result->preview_kinds[1],==,"generic");
    g_assert_false(result->preview_is_thumbnail[0]);g_assert_false(result->preview_is_thumbnail[1]);
    result_free(result);g_free(path);g_free(outside);
}
static void cancellation_is_empty(void) {
    gchar *path=directory("cancel"); file_at(path,"visible.txt");
    Result *result=preview(path,FALSE,TRUE); g_assert_cmpuint(result->count,==,0);
    g_assert_false(result->content_ready); g_assert_false(result->count_complete);
    result_free(result); g_free(path);
}
static void decode_is_bounded(void) {
    gchar *path=g_build_filename(fixture,"oversized.png",NULL);
    int fd=g_open(path,O_CREAT|O_WRONLY,0600); g_assert_cmpint(fd,>=,0);
    g_assert_cmpint(ftruncate(fd,9*1024*1024),==,0); close(fd);
    g_assert_null(bounded_image(path)); g_free(path);
    path=g_build_filename(fixture,"actual.png",NULL);
    GdkPixbuf *source=gdk_pixbuf_new(GDK_COLORSPACE_RGB,FALSE,8,32,32); gdk_pixbuf_fill(source,0x44cc88ff);
    g_assert_true(gdk_pixbuf_save(source,path,"png",NULL,NULL)); g_object_unref(source);
    GdkPixbuf *loaded=bounded_image(path); g_assert_nonnull(loaded); g_object_unref(loaded); g_free(path);
}
static void cache_is_per_view(void) {
    folders=g_hash_table_new_full(g_str_hash,g_str_equal,NULL,folder_unref);
    /* GWeakRef needs GObjects; these opaque identities exercise the cache key
     * without initializing GTK or opening a display. */
    GObject *first=g_object_new(G_TYPE_OBJECT,NULL), *second=g_object_new(G_TYPE_OBJECT,NULL);
    Folder *a=get_folder("file:///same","/same",(GtkWidget *)first);
    Folder *b=get_folder("file:///same","/same",(GtkWidget *)second);
    g_assert_true(a!=b); a->target=1.0; b->target=0.0;
    g_assert_cmpfloat(a->target,==,1.0);
    g_assert_true(get_folder("file:///same","/same",(GtkWidget *)first)==a);
    g_object_unref(first); g_object_unref(second); g_hash_table_unref(folders); folders=NULL;
}
static void replacement_drops_cached_identity(void) {
    Folder folder={0}; g_weak_ref_init(&folder.file_object,NULL);
    GObject *first=g_object_new(G_TYPE_OBJECT,NULL), *replacement=g_object_new(G_TYPE_OBJECT,NULL);
    bind_file_identity(&folder,first); folder.metadata_ready=TRUE;
    folder.previews_ready=TRUE; folder.count=2; folder.color=g_strdup("rose");
    guint generation=folder.generation;
    bind_file_identity(&folder,first); g_assert_true(folder.metadata_ready);
    bind_file_identity(&folder,replacement); g_assert_false(folder.metadata_ready);
    g_assert_false(folder.previews_ready); g_assert_cmpuint(folder.count,==,0);
    g_assert_null(folder.color); g_assert_cmpuint(folder.generation,>,generation);
    g_weak_ref_clear(&folder.file_object); g_object_unref(first); g_object_unref(replacement);
}
static void bulk_preflight_is_all_or_nothing(void) {
    folders=g_hash_table_new_full(g_str_hash,g_str_equal,NULL,folder_unref);
    GPtrArray *uris=g_ptr_array_new_with_free_func(g_free);
    for(guint i=0;i<129;i++)g_ptr_array_add(uris,g_strdup_printf("file:///bulk/%u",i));
    g_assert_false(can_accept_colors(uris)); g_assert_cmpuint(g_hash_table_size(folders),==,0);
    g_ptr_array_remove_index(uris,128); g_assert_true(can_accept_colors(uris));
    Folder *pending=get_folder("file:///busy","/busy",NULL); pending->pending_color=g_strdup("rose");
    g_assert_false(can_accept_colors(uris)); g_assert_cmpuint(g_hash_table_size(folders),==,1);
    g_ptr_array_unref(uris); g_hash_table_unref(folders); folders=NULL;
}
static void disabled_provider_exposes_no_types(void) {
    const GType *types=NULL; gint count=-1;
    g_setenv("MEADOW_FOLDER_EFFECTS","0",TRUE); thunar_extension_list_types(&types,&count);
    g_assert_cmpint(count,==,0); g_assert_null(types); g_unsetenv("MEADOW_FOLDER_EFFECTS");
    g_setenv("MEADOW_WINDOW_EFFECTS","0",TRUE); thunar_extension_list_types(&types,&count);
    g_assert_cmpint(count,==,0); g_assert_null(types); g_unsetenv("MEADOW_WINDOW_EFFECTS");
}

typedef struct {GObject parent;gchar *uri;} ProbeFile;
typedef struct {GObjectClass parent;} ProbeFileClass;
static guint probe_files_freed;
static gchar *probe_file_uri(ThunarxFileInfo *file){return g_strdup(((ProbeFile *)file)->uri);}
static void probe_file_info_init(ThunarxFileInfoIface *iface){iface->get_uri=probe_file_uri;}
G_DEFINE_TYPE_WITH_CODE(ProbeFile,probe_file,G_TYPE_OBJECT,
    G_IMPLEMENT_INTERFACE(THUNARX_TYPE_FILE_INFO,probe_file_info_init))
static void probe_file_finalize(GObject *object){g_free(((ProbeFile *)object)->uri);probe_files_freed++;G_OBJECT_CLASS(probe_file_parent_class)->finalize(object);}
static void probe_file_class_init(ProbeFileClass *klass){G_OBJECT_CLASS(klass)->finalize=probe_file_finalize;}
static void probe_file_init(ProbeFile *file){(void)file;}
static void selection_uses_actual_model_paths(void) {
    ProbeFile *first=g_object_new(probe_file_get_type(),NULL),*second=g_object_new(probe_file_get_type(),NULL);
    first->uri=g_strdup("file:///tmp/First%20file.txt");second->uri=g_strdup("file:///tmp/Selected%20file.pdf");
    GtkListStore *model=gtk_list_store_new(3,G_TYPE_STRING,G_TYPE_INT,probe_file_get_type());GtkTreeIter row;
    gtk_list_store_append(model,&row);gtk_list_store_set(model,&row,0,"first",2,first,-1);
    gtk_list_store_append(model,&row);gtk_list_store_set(model,&row,0,"actual selection",2,second,-1);
    GList *paths=g_list_append(NULL,gtk_tree_path_new_from_indices(1,-1));
    gchar *path=path_from_model(GTK_TREE_MODEL(model),paths);
    g_assert_cmpstr(path,==,"/tmp/Selected file.pdf");g_free(path);
    g_assert_null(path_from_model(GTK_TREE_MODEL(model),NULL));
    g_free(second->uri);second->uri=g_strdup("sftp://remote/selected.pdf");
    g_assert_null(path_from_model(GTK_TREE_MODEL(model),paths));
    g_list_free_full(paths,(GDestroyNotify)gtk_tree_path_free);g_object_unref(model);
    g_object_unref(first);g_object_unref(second);g_assert_cmpuint(probe_files_freed,==,2);
}
static void space_respects_modifiers(void) {
    g_assert_true(bare_space(GDK_KEY_space,0));g_assert_true(bare_space(GDK_KEY_space,GDK_LOCK_MASK));
    g_assert_false(bare_space(GDK_KEY_space,GDK_SHIFT_MASK));g_assert_false(bare_space(GDK_KEY_space,GDK_CONTROL_MASK));
    g_assert_false(bare_space(GDK_KEY_space,GDK_MOD1_MASK));g_assert_false(bare_space(GDK_KEY_space,GDK_SUPER_MASK));
    g_assert_false(bare_space(GDK_KEY_space,GDK_MOD4_MASK));g_assert_false(bare_space(GDK_KEY_space,GDK_HYPER_MASK));
    g_assert_false(bare_space(GDK_KEY_space,GDK_META_MASK));g_assert_false(bare_space(GDK_KEY_Return,0));
    g_assert_null(standard_view_for_focus(NULL));
}
static void thumbnail_formats_are_bounded(void) {
    g_assert_true(thumbnail_extension("/tmp/Real FILE.PDF"));g_assert_true(thumbnail_extension("/tmp/source.py"));
    g_assert_true(thumbnail_extension("/tmp/README"));g_assert_false(thumbnail_extension("/tmp/installer.exe"));
    g_assert_false(thumbnail_extension("/tmp/unknown.bin"));g_assert_false(thumbnail_extension("/tmp/fake.pdf/extensionless"));
    gchar *path=g_build_filename(fixture,"thumbnail-too-large.png",NULL);
    GdkPixbuf *large=gdk_pixbuf_new(GDK_COLORSPACE_RGB,FALSE,8,257,10);gdk_pixbuf_fill(large,0x22aa44ff);
    g_assert_true(gdk_pixbuf_save(large,path,"png",NULL,NULL));g_object_unref(large);
    g_assert_null(read_cached_thumbnail(path));g_free(path);
}
static void thumbnail_identity_changes_cache_key(void) {
    struct stat a={0},b={0};a.st_dev=1;a.st_ino=2;a.st_size=3;a.st_mtim.tv_sec=4;a.st_mtim.tv_nsec=5;b=a;
    gchar *first=thumbnail_cache_path("file:///tmp/test.pdf",&a),*same=thumbnail_cache_path("file:///tmp/test.pdf",&b);
    g_assert_cmpstr(first,==,same);g_assert_true(same_identity(&a,&b));g_free(same);
    b.st_ino++;gchar *replaced=thumbnail_cache_path("file:///tmp/test.pdf",&b);
    g_assert_cmpstr(first,!=,replaced);g_assert_false(same_identity(&a,&b));g_free(replaced);
    b=a;b.st_mtim.tv_nsec++;gchar *modified=thumbnail_cache_path("file:///tmp/test.pdf",&b);
    g_assert_cmpstr(first,!=,modified);g_assert_false(same_identity(&a,&b));g_free(modified);
    b=a;b.st_size++;g_assert_false(same_identity(&a,&b));
    b=a;b.st_ctim.tv_nsec++;g_assert_false(same_identity(&a,&b));
    gchar *changed=thumbnail_cache_path("file:///tmp/test.pdf",&b);g_assert_cmpstr(first,!=,changed);
    g_free(changed);g_free(first);
    GObject *file=g_object_new(G_TYPE_OBJECT,NULL);GFileInfo *info=g_file_info_new();g_file_info_set_size(info,0);
    gchar *initial=thumbnail_stamp(file,info);
    g_file_info_set_attribute_uint64(info,G_FILE_ATTRIBUTE_UNIX_INODE,42);
    gchar *inode=thumbnail_stamp(file,info);g_assert_cmpstr(initial,!=,inode);
    g_file_info_set_attribute_uint64(info,G_FILE_ATTRIBUTE_TIME_CHANGED,99);
    gchar *ctime=thumbnail_stamp(file,info);g_assert_cmpstr(inode,!=,ctime);
    g_free(initial);g_free(inode);g_free(ctime);g_object_unref(file);g_object_unref(info);
}
typedef struct {GMainLoop *loop;GdkPixbuf *image;} ThumbnailProbe;
static void thumbnail_probe_done(GObject *source,GAsyncResult *result,gpointer data) {
    (void)source;ThumbnailProbe *probe=data;probe->image=g_task_propagate_pointer(G_TASK(result),NULL);g_main_loop_quit(probe->loop);
}
static GdkPixbuf *run_thumbnail_probe(const char *source,const char *helper,guint deadline_ms) {
    FileThumb thumb={.path=(gchar *)source};thumb.uri=g_filename_to_uri(source,NULL,NULL);
    ThumbWork work={.thumb=&thumb,.helper=(gchar *)helper,.cancel=g_cancellable_new()};
    ThumbnailProbe probe={.loop=g_main_loop_new(NULL,FALSE)};
    GTask *task=g_task_new(NULL,work.cancel,thumbnail_probe_done,&probe);g_task_set_check_cancellable(task,FALSE);
    g_task_set_task_data(task,&work,NULL);work.timeout=g_timeout_add(deadline_ms,thumbnail_timeout,&work);
    g_task_run_in_thread(task,thumbnail_worker);g_main_loop_run(probe.loop);
    if(work.timeout)g_source_remove(work.timeout);
    g_main_loop_unref(probe.loop);g_object_unref(task);g_object_unref(work.cancel);g_free(thumb.uri);return probe.image;
}
static void thumbnail_worker_handles_success_failure_timeout(void) {
    gchar *dir=directory("thumbnail-worker"),*image=g_build_filename(dir,"real.png",NULL);
    GdkPixbuf *pixels=gdk_pixbuf_new(GDK_COLORSPACE_RGB,FALSE,8,64,80);gdk_pixbuf_fill(pixels,0x5b72dfff);
    g_assert_true(gdk_pixbuf_save(pixels,image,"png",NULL,NULL));g_object_unref(pixels);
    gchar *helper=g_build_filename(dir,"fixture-helper.py",NULL);
    gchar *script=g_strdup_printf("import argparse,shutil,time,sys\np=argparse.ArgumentParser()\np.add_argument('--thumbnail',action='store_true')\np.add_argument('--path')\np.add_argument('--output')\np.add_argument('--size')\na=p.parse_args()\nif 'timeout' in a.path: time.sleep(10)\nif 'failure' in a.path: sys.exit(1)\nshutil.copyfile(%c%s%c,a.output)\n",'\'',image,'\'');
    g_assert_true(g_file_set_contents(helper,script,-1,NULL));g_free(script);
    file_at(dir,"success.txt");gchar *source=g_build_filename(dir,"success.txt",NULL);
    GdkPixbuf *result=run_thumbnail_probe(source,helper,2000);g_assert_nonnull(result);
    g_assert_cmpint(gdk_pixbuf_get_width(result),==,64);g_object_unref(result);g_free(source);
    file_at(dir,"failure.txt");source=g_build_filename(dir,"failure.txt",NULL);
    g_assert_null(run_thumbnail_probe(source,helper,2000));g_free(source);
    file_at(dir,"timeout.txt");source=g_build_filename(dir,"timeout.txt",NULL);gint64 start=g_get_monotonic_time();
    g_assert_null(run_thumbnail_probe(source,helper,50));g_assert_cmpint(g_get_monotonic_time()-start,<,1500000);
    g_free(source);g_free(helper);g_free(image);g_free(dir);
}
int main(int argc,char **argv) {
    fixture=g_dir_make_tmp("meadow-folder-unit-XXXXXX",NULL); g_assert_nonnull(fixture);
    g_setenv("XDG_CONFIG_HOME",fixture,TRUE);g_setenv("XDG_CACHE_HOME",fixture,TRUE);g_test_init(&argc,&argv,NULL);
    g_test_add_func("/folder/disabled-provider",disabled_provider_exposes_no_types);
    g_test_add_func("/folder/palette-validation",colors_validated);
    g_test_add_func("/folder/fallback-inode-identity",fallback_identity);
    g_test_add_func("/folder/xattr-rename",xattr_survives_rename);
    g_test_add_func("/folder/empty-no-cards",empty_has_no_cards);
    g_test_add_func("/folder/three-real-previews",previews_are_real_and_bounded);
    g_test_add_func("/folder/hidden-view-policy",hidden_respects_view);
    g_test_add_func("/folder/no-symlink-traversal",symlinks_are_not_followed);
    g_test_add_func("/folder/shortcut-preserves-target",shortcut_has_no_target_preview_or_color_write);
    g_test_add_func("/folder/status-affordances",folder_states_preserve_affordances);
    g_test_add_func("/folder/actual-counts-and-scan-boundary",actual_counts_are_not_card_counts);
    g_test_add_func("/folder/representative-real-kinds",representatives_cover_real_kinds);
    g_test_add_func("/folder/unreadable-not-empty",unreadable_is_unknown_not_empty);
    g_test_add_func("/folder/content-invalidation-lifecycle",content_invalidation_preserves_color_and_releases_resources);
    g_test_add_func("/folder/pinned-directory-replacement",pinned_directory_rejects_replaced_path);
    g_test_add_func("/folder/actual-document-content-refresh",real_document_contents_refresh_after_edit);
    g_test_add_func("/folder/link-badge-bounds",link_badge_stays_inside_glyph);
    g_test_add_func("/folder/cancelled-preview",cancellation_is_empty);
    g_test_add_func("/folder/bounded-image-decode",decode_is_bounded);
    g_test_add_func("/folder/per-view-hover-isolation",cache_is_per_view);
    g_test_add_func("/folder/replacement-drops-cached-color",replacement_drops_cached_identity);
    g_test_add_func("/folder/bulk-color-preflight",bulk_preflight_is_all_or_nothing);
    g_test_add_func("/quicklook/actual-model-selection",selection_uses_actual_model_paths);
    g_test_add_func("/quicklook/unmodified-space-only",space_respects_modifiers);
    g_test_add_func("/thumbnail/safe-formats-and-size",thumbnail_formats_are_bounded);
    g_test_add_func("/thumbnail/cache-identity",thumbnail_identity_changes_cache_key);
    g_test_add_func("/thumbnail/worker-success-failure-timeout",thumbnail_worker_handles_success_failure_timeout);
    int status=g_test_run(); g_print("Owned test artifacts: %s\n",fixture); g_free(fixture); return status;
}
