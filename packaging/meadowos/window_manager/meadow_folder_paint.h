#ifndef MEADOW_FOLDER_PAINT_H
#define MEADOW_FOLDER_PAINT_H

#include <gtk/gtk.h>

G_BEGIN_DECLS

/* Only successful directory reads set ready. An incomplete count is a lower
 * bound, never an exact total; unavailable/loading content must not display 0.
 * Kinds and thumbnail flags describe the corresponding actual preview entry. */
typedef struct {
    gboolean ready;
    guint item_count;
    gboolean count_complete;
    const char *kinds[3];
    gboolean is_thumbnail[3];
} MeadowFolderContentInfo;

/* Draw only inside glyph_box. The caller owns every pixbuf and the animation.
 * preview_count is clamped to three; each non-NULL pixbuf must represent an
 * actual child entry. Optional content metadata supplies honest count/type
 * labels. No file access, timers or navigation occur here.
 * Tint RGB selects the material accent; its alpha is not used. */
void meadow_folder_paint(cairo_t *cr,
                         const GdkRectangle *glyph_box,
                         double progress,
                         const GdkRGBA *tint,
                         GdkPixbuf *const previews[3],
                         guint preview_count,
                         gboolean selected,
                         gboolean prelight,
                         const MeadowFolderContentInfo *contents);

G_END_DECLS

#endif
