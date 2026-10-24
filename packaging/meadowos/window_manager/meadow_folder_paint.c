#include "meadow_folder_paint.h"

#include <math.h>
#include <stdint.h>
#include <string.h>

/* The material is rendered in a bounded icon-sized buffer. Blurring this
 * internal back/preview layer is intentional: this does not sample or claim
 * to refract the desktop behind the file manager. */
#define MATERIAL_LIMIT 256
#define PI 3.14159265358979323846

typedef struct {
    double red, green, blue;
    gboolean dark;
} FolderColor;

static double clamp01(double x)
{
    return isfinite(x) ? CLAMP(x, 0.0, 1.0) : 0.0;
}

static void rounded_rect(cairo_t *cr, double x, double y,
                         double width, double height, double radius)
{
    const double k = 0.5522847498307936;
    double r = MIN(radius, MIN(width, height) * .5);
    cairo_new_sub_path(cr);
    cairo_move_to(cr, x + r, y);
    cairo_line_to(cr, x + width - r, y);
    cairo_curve_to(cr, x + width - r + r * k, y,
                   x + width, y + r - r * k, x + width, y + r);
    cairo_line_to(cr, x + width, y + height - r);
    cairo_curve_to(cr, x + width, y + height - r + r * k,
                   x + width - r + r * k, y + height, x + width - r, y + height);
    cairo_line_to(cr, x + r, y + height);
    cairo_curve_to(cr, x + r - r * k, y + height,
                   x, y + height - r + r * k, x, y + height - r);
    cairo_line_to(cr, x, y + r);
    cairo_curve_to(cr, x, y + r - r * k, x + r - r * k, y, x + r, y);
    cairo_close_path(cr);
}

static void back_path(cairo_t *cr)
{
    cairo_new_path(cr);
    cairo_move_to(cr, .10, .255);
    cairo_curve_to(cr, .10, .20, .125, .175, .175, .175);
    cairo_line_to(cr, .34, .175);
    cairo_curve_to(cr, .389, .175, .414, .265, .478, .265);
    cairo_line_to(cr, .825, .265);
    cairo_curve_to(cr, .876, .265, .90, .292, .90, .343);
    cairo_line_to(cr, .90, .815);
    cairo_curve_to(cr, .90, .866, .876, .89, .825, .89);
    cairo_line_to(cr, .175, .89);
    cairo_curve_to(cr, .124, .89, .10, .866, .10, .815);
    cairo_close_path(cr);
}

static void front_path(cairo_t *cr, double progress)
{
    /* Open the cover before the papers finish spreading. The bottom remains
     * the hinge and the original native hit box never changes. */
    double lid = MIN(1.0, progress * 1.35);
    double left = .09 - .005 * lid;
    double right = .91 + .005 * lid;
    double top = .39 + .09 * lid;
    double radius = .075;
    cairo_new_path(cr);
    cairo_move_to(cr, left, top + radius);
    cairo_curve_to(cr, left, top + .025, left + .025, top, left + radius, top);
    cairo_line_to(cr, right - radius, top);
    cairo_curve_to(cr, right - .025, top, right, top + .025,
                   right, top + radius);
    cairo_line_to(cr, right, .91 - radius);
    cairo_curve_to(cr, right, .885, right - .025, .91, right - radius, .91);
    cairo_line_to(cr, left + radius, .91);
    cairo_curve_to(cr, left + .025, .91, left, .885, left, .91 - radius);
    cairo_close_path(cr);
}

static void source_color(cairo_t *cr, const FolderColor *color, double alpha)
{
    cairo_set_source_rgba(cr, color->red, color->green, color->blue, alpha);
}

static void paint_back(cairo_t *cr, const FolderColor *color, int pixels)
{
    cairo_pattern_t *gradient = cairo_pattern_create_linear(0, .18, .3, .89);
    double white = color->dark ? .08 : .57;
    double mix = 1.0 - white;
    cairo_pattern_add_color_stop_rgba(gradient, 0,
        white + color->red * mix, white + color->green * mix,
        white + color->blue * mix, color->dark ? .65 : .58);
    cairo_pattern_add_color_stop_rgba(gradient, 1,
        color->red, color->green, color->blue, color->dark ? .53 : .38);
    back_path(cr);
    cairo_set_source(cr, gradient);
    cairo_fill_preserve(cr);
    cairo_set_source_rgba(cr, 1, 1, 1, color->dark ? .26 : .63);
    cairo_set_line_width(cr, .008);
    cairo_stroke(cr);
    cairo_pattern_destroy(gradient);
    back_path(cr);
    cairo_set_source_rgba(cr, .18, .27, .38, color->dark ? .12 : .19);
    cairo_set_line_width(cr, MAX(.006, .60 / pixels));
    cairo_stroke(cr);
}

static const char *kind_label(const char *kind)
{
    if (g_strcmp0(kind, "image") == 0) return "IMG";
    if (g_strcmp0(kind, "video") == 0) return "VID";
    if (g_strcmp0(kind, "audio") == 0) return "AUD";
    if (g_strcmp0(kind, "pdf") == 0) return "PDF";
    if (g_strcmp0(kind, "document") == 0) return "DOC";
    if (g_strcmp0(kind, "spreadsheet") == 0) return "XLS";
    if (g_strcmp0(kind, "presentation") == 0) return "PPT";
    if (g_strcmp0(kind, "code") == 0) return "{ }";
    if (g_strcmp0(kind, "text") == 0) return "TXT";
    if (g_strcmp0(kind, "archive") == 0) return "ZIP";
    if (g_strcmp0(kind, "folder") == 0) return "DIR";
    return "FILE";
}

static void paint_kind(cairo_t *cr, const char *kind)
{
    if (!kind || !*kind)
        return;
    const char *label = kind_label(kind);
    cairo_save(cr);
    cairo_select_font_face(cr, "sans", CAIRO_FONT_SLANT_NORMAL, CAIRO_FONT_WEIGHT_BOLD);
    cairo_set_font_size(cr, .050);
    cairo_text_extents_t extents;
    cairo_text_extents(cr, label, &extents);
    double width = extents.x_advance + .036;
    rounded_rect(cr, .015, .012, width, .070, .015);
    cairo_set_source_rgba(cr, .98, .99, 1, .92);
    cairo_fill_preserve(cr);
    cairo_set_source_rgba(cr, .20, .30, .42, .15);
    cairo_set_line_width(cr, .004);
    cairo_stroke(cr);
    cairo_set_source_rgba(cr, .18, .28, .40, .96);
    cairo_move_to(cr, .033, .064);
    cairo_show_text(cr, label);
    cairo_restore(cr);
}

static void paint_card(cairo_t *cr, GdkPixbuf *pixbuf, double progress,
                       double position, guint count, const char *kind,
                       gboolean is_thumbnail)
{
    if (!pixbuf || !GDK_IS_PIXBUF(pixbuf))
        return;
    double fan = clamp01((progress - .08) / .92);
    double width = .57 - .07 * fan, height = .62;
    double offset = count > 1 ? position : 0;
    double angle = offset * (2.0 + 14.0 * fan) * PI / 180.0;
    double x = .5 + offset * (.08 + .08 * fan);
    /* A closed folder already shows its real stack. Staggered upper edges
     * expose each actual type/thumbnail; hover only fans that stack farther. */
    double resting_top = count == 1 ? .235 : count == 2 ?
                         .235 + offset * .060 : .230 + offset * .085;
    /* A wider, balanced fan exposes all three actual previews. Rotate around
     * each paper's centre so the outer cards stay inside the icon allocation. */
    double open_top = .055 + fabs(offset) * .065;
    double top = resting_top + (open_top - resting_top) * fan;
    double pw = gdk_pixbuf_get_width(pixbuf);
    double ph = gdk_pixbuf_get_height(pixbuf);
    if (pw <= 0 || ph <= 0)
        return;
    cairo_save(cr);
    cairo_translate(cr, x, top + height * .5);
    cairo_rotate(cr, angle);
    cairo_translate(cr, -width * .5, -height * .5);
    rounded_rect(cr, -.005, .008, width + .01, height, .045);
    cairo_set_source_rgba(cr, .07, .10, .15, .10);
    cairo_fill(cr);
    rounded_rect(cr, 0, 0, width, height, .042);
    cairo_set_source_rgba(cr, .96, .97, .99, .96);
    cairo_fill_preserve(cr);
    cairo_save(cr);
    cairo_clip(cr);
    /* Non-square image thumbnails fill the paper like a photo preview.
     * Square MIME icons keep their full recognizable shape, with no invented
     * document text. Both branches use only the caller's actual child pixels. */
    double inset = .018;
    gboolean image_aspect = is_thumbnail || pw / ph > 1.2 || pw / ph < .8;
    double scale = image_aspect ? MAX(width / pw, height / ph) :
                   MIN((width - inset * 2) / pw, (height - inset * 2) / ph);
    cairo_translate(cr, (width - pw * scale) * .5,
                    image_aspect ? (height - ph * scale) * .5 : inset);
    cairo_scale(cr, scale, scale);
    gdk_cairo_set_source_pixbuf(cr, pixbuf, 0, 0);
    cairo_pattern_set_filter(cairo_get_source(cr), CAIRO_FILTER_BILINEAR);
    cairo_paint(cr);
    cairo_restore(cr);
    rounded_rect(cr, 0, 0, width, height, .042);
    cairo_set_source_rgba(cr, .22, .30, .40, .20);
    cairo_set_line_width(cr, .007);
    cairo_stroke(cr);
    paint_kind(cr, kind);
    cairo_restore(cr);
}

static cairo_surface_t *make_contents(int pixels, const FolderColor *color,
                                      double progress, GdkPixbuf *const previews[3],
                                      guint count, const MeadowFolderContentInfo *info)
{
    cairo_surface_t *surface = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, pixels, pixels);
    if (cairo_surface_status(surface) != CAIRO_STATUS_SUCCESS)
        return surface;
    cairo_t *cr = cairo_create(surface);
    cairo_scale(cr, pixels, pixels);
    paint_back(cr, color, pixels);
    /* Paper extends behind the cover, never below the folder's bottom. */
    cairo_save(cr);
    cairo_rectangle(cr, 0, 0, 1, .89);
    cairo_clip(cr);
    /* Back-to-front order follows the stepped real entries. Each upper edge
     * remains visible at rest; no extra paper is drawn to suggest a fake count. */
    for (guint i = 0; i < count; i++) {
        double position = count == 1 ? 0 : count == 2 ? (i ? .75 : -.75) : (double) i - 1;
        paint_card(cr, previews[i], progress, position, count,
                   info ? info->kinds[i] : NULL,
                   info ? info->is_thumbnail[i] : FALSE);
    }
    cairo_restore(cr);
    cairo_destroy(cr);
    return surface;
}

/* A sliding box pass operates on premultiplied channels, including alpha.
 * Three passes approximate a Gaussian without unbounded sampling or halos. */
static void blur_pass(const unsigned char *source, unsigned char *target,
                      int size, int radius, gboolean horizontal)
{
    int span = radius * 2 + 1;
    for (int line = 0; line < size; line++) {
        unsigned int sum[4] = {0, 0, 0, 0};
        for (int n = -radius; n <= radius; n++) {
            int p = CLAMP(n, 0, size - 1);
            int index = horizontal ? (line * size + p) * 4 : (p * size + line) * 4;
            for (int channel = 0; channel < 4; channel++)
                sum[channel] += source[index + channel];
        }
        for (int p = 0; p < size; p++) {
            int index = horizontal ? (line * size + p) * 4 : (p * size + line) * 4;
            for (int channel = 0; channel < 4; channel++)
                target[index + channel] = (unsigned char) (sum[channel] / span);
            int leaving = CLAMP(p - radius, 0, size - 1);
            int entering = CLAMP(p + radius + 1, 0, size - 1);
            int old_index = horizontal ? (line * size + leaving) * 4 : (leaving * size + line) * 4;
            int new_index = horizontal ? (line * size + entering) * 4 : (entering * size + line) * 4;
            for (int channel = 0; channel < 4; channel++)
                sum[channel] += (unsigned int) source[new_index + channel] - source[old_index + channel];
        }
    }
}

static cairo_surface_t *blur_contents(cairo_surface_t *source, int pixels)
{
    cairo_surface_t *blurred = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, pixels, pixels);
    if (cairo_surface_status(blurred) != CAIRO_STATUS_SUCCESS)
        return blurred;
    int stride = cairo_image_surface_get_stride(blurred);
    unsigned char *data = cairo_image_surface_get_data(blurred);
    unsigned char *temporary = g_try_malloc_n((gsize) pixels * pixels, 4);
    if (!temporary) {
        cairo_surface_destroy(blurred);
        return NULL;
    }
    cairo_surface_flush(source);
    const unsigned char *input = cairo_image_surface_get_data(source);
    int input_stride = cairo_image_surface_get_stride(source);
    /* ARGB32 width is always 4-byte aligned; copying row-wise documents that
     * the blur kernel's tight buffer assumption is explicit. */
    for (int y = 0; y < pixels; y++)
        memcpy(data + y * stride, input + y * input_stride, (gsize) pixels * 4);
    int radius = MAX(1, (int) lround(pixels * .032));
    for (int pass = 0; pass < 3; pass++) {
        blur_pass(data, temporary, pixels, radius, TRUE);
        blur_pass(temporary, data, pixels, radius, FALSE);
    }
    g_free(temporary);
    cairo_surface_mark_dirty(blurred);
    return blurred;
}

static void paint_surface_normalized(cairo_t *cr, cairo_surface_t *surface, int pixels)
{
    cairo_save(cr);
    cairo_scale(cr, 1.0 / pixels, 1.0 / pixels);
    cairo_set_source_surface(cr, surface, 0, 0);
    cairo_pattern_set_filter(cairo_get_source(cr), CAIRO_FILTER_BILINEAR);
    cairo_paint(cr);
    cairo_restore(cr);
}

static void paint_front(cairo_t *cr, cairo_surface_t *frost, int pixels,
                        const FolderColor *color, double progress,
                        gboolean selected, gboolean prelight, double side)
{
    cairo_save(cr);
    front_path(cr, progress);
    cairo_clip(cr);
    if (frost && cairo_surface_status(frost) == CAIRO_STATUS_SUCCESS)
        paint_surface_normalized(cr, frost, pixels);
    /* The cover adds a restrained translucent tint and a milky grazing-light
     * gradient over the genuinely blurred internal content. */
    source_color(cr, color, color->dark ? .37 : .25);
    cairo_paint(cr);
    cairo_pattern_t *coat = cairo_pattern_create_linear(.1, .39, .75, .93);
    double alpha = color->dark ? .14 : .43;
    cairo_pattern_add_color_stop_rgba(coat, 0, 1, 1, 1, alpha + .10);
    cairo_pattern_add_color_stop_rgba(coat, .43, 1, 1, 1, alpha);
    cairo_pattern_add_color_stop_rgba(coat, 1, 1, 1, 1, alpha + .06);
    cairo_set_source(cr, coat);
    cairo_paint(cr);
    cairo_pattern_destroy(coat);
    /* Thin bottom-edge density supplies depth without an opaque slab. */
    cairo_pattern_t *edge = cairo_pattern_create_linear(0, .83, 0, .94);
    cairo_pattern_add_color_stop_rgba(edge, 0, .10, .14, .20, 0);
    cairo_pattern_add_color_stop_rgba(edge, 1, .10, .14, .20, color->dark ? .22 : .085);
    cairo_set_source(cr, edge);
    cairo_paint(cr);
    cairo_pattern_destroy(edge);
    cairo_restore(cr);

    front_path(cr, progress);
    cairo_set_source_rgba(cr, .20, .29, .40, color->dark ? .16 : .18);
    cairo_set_line_width(cr, MAX(.012, 1.0 / side));
    cairo_stroke(cr);
    front_path(cr, progress);
    cairo_pattern_t *rim = cairo_pattern_create_linear(.1, .36, .91, .91);
    cairo_pattern_add_color_stop_rgba(rim, 0, 1, 1, 1, .89);
    cairo_pattern_add_color_stop_rgba(rim, .48, 1, 1, 1, .52);
    cairo_pattern_add_color_stop_rgba(rim, 1, 1, 1, 1, .26);
    cairo_set_source(cr, rim);
    cairo_set_line_width(cr, MAX(.0055, .65 / side));
    cairo_stroke(cr);
    cairo_pattern_destroy(rim);

    if (selected || prelight) {
        front_path(cr, progress);
        source_color(cr, color, selected ? .30 : .12);
        cairo_set_line_width(cr, MAX(.006, .6 / side));
        cairo_stroke(cr);
    }
}

static void paint_count(cairo_t *cr, const MeadowFolderContentInfo *info,
                        const FolderColor *color, double side)
{
    if (!info || !info->ready || side < 48)
        return;
    char label[24];
    g_snprintf(label, sizeof(label), "%u%s", info->item_count,
               info->count_complete ? "" : "+");
    cairo_save(cr);
    cairo_select_font_face(cr, "sans", CAIRO_FONT_SLANT_NORMAL, CAIRO_FONT_WEIGHT_NORMAL);
    double font_size = MAX(.072, 6.0 / side);
    cairo_set_font_size(cr, font_size);
    cairo_text_extents_t extents;
    cairo_text_extents(cr, label, &extents);
    if (extents.x_advance > .56) {
        cairo_set_font_size(cr, font_size * .56 / extents.x_advance);
        cairo_text_extents(cr, label, &extents);
    }
    double width = MAX(.15, extents.x_advance + .075);
    double height = MAX(.13, 9.0 / side);
    rounded_rect(cr, .16, .83 - height, width, height, height * .45);
    cairo_set_source_rgba(cr, 1, 1, 1, color->dark ? .15 : .38);
    cairo_fill(cr);
    cairo_set_source_rgba(cr, color->dark ? .92 : .20,
                         color->dark ? .95 : .29, color->dark ? 1 : .39, .86);
    cairo_move_to(cr, .16 + (width - extents.x_advance) * .5,
                  .83 - height * .5 - (extents.y_bearing + extents.height * .5));
    cairo_show_text(cr, label);
    cairo_restore(cr);
}

void meadow_folder_paint(cairo_t *cr, const GdkRectangle *glyph_box,
                         double progress, const GdkRGBA *tint,
                         GdkPixbuf *const previews[3], guint preview_count,
                         gboolean selected, gboolean prelight,
                         const MeadowFolderContentInfo *info)
{
    if (!cr || !glyph_box || glyph_box->width <= 0 || glyph_box->height <= 0)
        return;
    double side = MIN(glyph_box->width, glyph_box->height);
    if (side < 8)
        return;
    double p = clamp01(progress);
    FolderColor color = {.red = .54, .green = .64, .blue = .73, .dark = FALSE};
    if (tint) {
        color.red = clamp01(tint->red);
        color.green = clamp01(tint->green);
        color.blue = clamp01(tint->blue);
    }
    color.dark = .2126 * color.red + .7152 * color.green + .0722 * color.blue < .32;
    guint count = previews ? MIN(preview_count, 3u) : 0;
    double dx = side, dy = 0;
    cairo_user_to_device_distance(cr, &dx, &dy);
    int pixels = CLAMP((int) ceil(hypot(dx, dy)), 16, MATERIAL_LIMIT);
    cairo_surface_t *contents = make_contents(pixels, &color, p, previews, count, info);
    if (cairo_surface_status(contents) != CAIRO_STATUS_SUCCESS) {
        cairo_surface_destroy(contents);
        return;
    }
    cairo_surface_t *frost = blur_contents(contents, pixels);
    cairo_save(cr);
    cairo_rectangle(cr, glyph_box->x, glyph_box->y, glyph_box->width, glyph_box->height);
    cairo_clip(cr);
    cairo_translate(cr, glyph_box->x + (glyph_box->width - side) * .5,
                    glyph_box->y + (glyph_box->height - side) * .5);
    cairo_scale(cr, side, side);

    /* A feathered contact shadow adds depth without stacked outline rings. */
    cairo_save(cr);
    cairo_translate(cr, .5, .916);
    cairo_scale(cr, .45, .045);
    cairo_pattern_t *shadow = cairo_pattern_create_radial(0, 0, .20, 0, 0, 1);
    cairo_pattern_add_color_stop_rgba(shadow, 0, .07, .11, .18, .14);
    cairo_pattern_add_color_stop_rgba(shadow, 1, .07, .11, .18, 0);
    cairo_set_source(cr, shadow);
    cairo_arc(cr, 0, 0, 1, 0, 2 * PI);
    cairo_fill(cr);
    cairo_pattern_destroy(shadow);
    cairo_restore(cr);
    /* A frosted cover replaces the sharp internal layer in its own shape.
     * Painting both layers there would compound their alpha and turn the
     * material into an opaque-looking skin, especially for empty folders. */
    cairo_save(cr);
    front_path(cr, p);
    cairo_path_t *cover_path = cairo_copy_path(cr);
    cairo_new_path(cr);
    cairo_rectangle(cr, 0, 0, 1, 1);
    cairo_append_path(cr, cover_path);
    cairo_path_destroy(cover_path);
    cairo_set_fill_rule(cr, CAIRO_FILL_RULE_EVEN_ODD);
    cairo_clip(cr);
    paint_surface_normalized(cr, contents, pixels);
    cairo_restore(cr);
    paint_front(cr, frost, pixels, &color, p, selected, prelight, side);
    paint_count(cr, info, &color, side);
    cairo_restore(cr);
    if (frost)
        cairo_surface_destroy(frost);
    cairo_surface_destroy(contents);
}
