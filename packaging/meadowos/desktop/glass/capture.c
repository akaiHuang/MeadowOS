/* Meadow native backdrop reader. Original implementation; no screen screenshots.
 * Build: cc -O2 -Wall -Wextra capture.c -o meadow-glass-capture -lX11 -lXcomposite -lXrender
 * Input: target-XID region-x region-y width height (relative to client origin).
 * Output: four native uint32 values (status, source-count, width, height), BGRA.
 * This process owns its X error handler; never load it into a GTK process.
 */
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <X11/extensions/Xcomposite.h>
#include <X11/extensions/Xrender.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int xerror;
static int trap(Display *d, XErrorEvent *e) { (void)d; (void)e; xerror = 1; return 0; }
static unsigned component(unsigned long pixel, unsigned long mask) {
    if (!mask) return 0;
    while (!(mask & 1)) { mask >>= 1; pixel >>= 1; }
    return (unsigned)((pixel & mask) * 255 / mask);
}
static Window top_frame(Display *d, Window root, Window window) {
    for (int depth = 0; depth < 32; ++depth) {
        Window tree_root, parent, *children = NULL;
        unsigned count;
        if (!XQueryTree(d, window, &tree_root, &parent, &children, &count)) return None;
        if (children) XFree(children);
        if (parent == root) return window;
        if (!parent || parent == window) return None;
        window = parent;
    }
    return None;
}
static int paint(Display *d, Window root, Window source, int rx, int ry,
                 unsigned width, unsigned height, unsigned char *out) {
    XWindowAttributes a;
    xerror = 0;
    if (!XGetWindowAttributes(d, source, &a) || a.map_state != IsViewable ||
        a.class == InputOnly) return 0;
    int sx, sy; Window child;
    if (!XTranslateCoordinates(d, source, root, 0, 0, &sx, &sy, &child)) return 0;
    sx -= a.border_width; sy -= a.border_width;
    int left = sx > rx ? sx : rx, top = sy > ry ? sy : ry;
    int right = sx + a.width + 2 * a.border_width;
    int bottom = sy + a.height + 2 * a.border_width;
    if (right > rx + (int)width) right = rx + (int)width;
    if (bottom > ry + (int)height) bottom = ry + (int)height;
    if (right <= left || bottom <= top) return 0;
    Pixmap pixmap = XCompositeNameWindowPixmap(d, source);
    XSync(d, False);
    if (xerror || !pixmap) return 0;
    XImage *image = XGetImage(d, pixmap, left - sx, top - sy,
                             right - left, bottom - top, AllPlanes, ZPixmap);
    XSync(d, False);
    if (xerror || !image) { XFreePixmap(d, pixmap); return 0; }
    XRenderPictFormat *format = XRenderFindVisualFormat(d, a.visual);
    unsigned long alpha_mask = (format && format->type == PictTypeDirect)
        ? ((unsigned long)format->direct.alphaMask << format->direct.alpha) : 0;
    /* Some XImage pixmaps omit masks; their window visual remains authoritative. */
    unsigned long red = a.visual->red_mask, green = a.visual->green_mask, blue = a.visual->blue_mask;
    if (image->bits_per_pixel==32 && image->byte_order==LSBFirst &&
        red==0xff0000 && green==0xff00 && blue==0xff &&
        (!alpha_mask || alpha_mask==0xff000000)) {
        for(int y=top;y<bottom;y++) {
            const unsigned char *src=(const unsigned char *)image->data+(size_t)(y-top)*image->bytes_per_line;
            unsigned char *dst=out+((size_t)(y-ry)*width+left-rx)*4;
            for(int x=left;x<right;x++,src+=4,dst+=4) {
                unsigned alpha=alpha_mask?src[3]:255;
                for(int channel=0;channel<3;channel++) {
                    unsigned value=src[channel]+(dst[channel]*(255-alpha)+127)/255;
                    dst[channel]=value>255?255:value;
                }
            }
        }
    } else {
    for (int y = top; y < bottom; ++y) for (int x = left; x < right; ++x) {
        unsigned long p = XGetPixel(image, x - left, y - top);
        unsigned alpha = alpha_mask ? component(p, alpha_mask) : 255;
        unsigned char *dst = out + ((size_t)(y - ry) * width + x - rx) * 4;
        unsigned rgb[3] = {component(p, blue), component(p, green), component(p, red)};
        for (int channel = 0; channel < 3; ++channel) {
            /* XRender ARGB pixmaps contain premultiplied channels. */
            unsigned value = rgb[channel] + (dst[channel] * (255 - alpha) + 127) / 255;
            dst[channel] = value > 255 ? 255 : value;
        }
    }
    }
    XDestroyImage(image); XFreePixmap(d, pixmap);
    return 1;
}
int main(int argc, char **argv) {
    if (argc != 3 || strcmp(argv[1], "--display")) return 2;
    Display *d = XOpenDisplay(argv[2]);
    if (!d) return 3;
    XSetErrorHandler(trap);
    int event, error;
    if (!XCompositeQueryExtension(d, &event, &error)) return 4;
    int screen = DefaultScreen(d);
    Window root = RootWindow(d, screen);
    char selection[64]; snprintf(selection, sizeof selection, "_NET_WM_CM_S%d", screen);
    Atom compositor = XInternAtom(d, selection, False);
    char line[160];
    while (fgets(line, sizeof line, stdin)) {
        unsigned long target; int x, y; unsigned width, height;
        uint32_t header[4] = {1, 0, 0, 0};
        if (sscanf(line, "%lx %d %d %u %u", &target, &x, &y, &width, &height) != 5 ||
            !width || !height || width > 4176 || height > 4176 ||
            (size_t)width * height > 5000000) {
            fwrite(header, sizeof header, 1, stdout); fflush(stdout); continue;
        }
        Window frame = top_frame(d, root, target);
        Window tree_root, parent, *children = NULL, ignored;
        unsigned count = 0; int tx = 0, ty = 0, found = -1;
        if (XGetSelectionOwner(d, compositor) && frame &&
            XTranslateCoordinates(d, target, root, x, y, &tx, &ty, &ignored) &&
            XQueryTree(d, root, &tree_root, &parent, &children, &count)) {
            for (unsigned i = 0; i < count; ++i) if (children[i] == frame) { found = (int)i; break; }
        }
        if (found < 0) {
            if (children) XFree(children);
            fwrite(header, sizeof header, 1, stdout); fflush(stdout); continue;
        }
        unsigned char *pixels = malloc((size_t)width * height * 4);
        if (!pixels) return 5;
        /* Root fallback only; normal Meadow wallpaper is a lower native window. */
        for (size_t i = 0; i < (size_t)width * height; ++i) {
            pixels[i * 4] = 242; pixels[i * 4 + 1] = 239;
            pixels[i * 4 + 2] = 235; pixels[i * 4 + 3] = 255;
        }
        header[0] = 0; header[2] = width; header[3] = height;
        for (int i = 0; i < found; ++i) header[1] += paint(d, root, children[i], tx, ty, width, height, pixels);
        XFree(children);
        fwrite(header, sizeof header, 1, stdout);
        fwrite(pixels, (size_t)width * height * 4, 1, stdout);
        fflush(stdout); free(pixels);
    }
    XCloseDisplay(d); return 0;
}
