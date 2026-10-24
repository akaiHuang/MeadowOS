/* Offscreen geometry regression; no fake document pixels or GUI input. */
#include "../meadow_folder_paint.c"
#include <stdio.h>

int main(void) {
    cairo_surface_t *surface=cairo_image_surface_create(CAIRO_FORMAT_ARGB32,128,128);
    cairo_t *cr=cairo_create(surface);
    const double progress[]={0,.5,1};
    unsigned checks=0;
    for(unsigned p=0;p<3;p++) {
        front_path(cr,progress[p]);
        double expected=.39+.09*MIN(1.0,progress[p]*1.35);
        for(unsigned i=0;i<7;i++) {
            double x=.20+.10*i;
            if(cairo_in_fill(cr,x,expected-.005) || !cairo_in_fill(cr,x,expected+.005)) {
                fprintf(stderr,"front not flat at progress %.2f x %.2f\n",progress[p],x);
                return 1;
            }
            checks+=2;
        }
        if(!cairo_in_fill(cr,.5,.89) || cairo_in_fill(cr,.5,.92))return 2;
        checks+=2;
    }
    back_path(cr);
    if(!cairo_in_fill(cr,.25,.20) || cairo_in_fill(cr,.70,.20))return 3;
    checks+=2;
    if(cairo_status(cr)!=CAIRO_STATUS_SUCCESS)return 4;
    cairo_destroy(cr);cairo_surface_destroy(surface);
    printf("PASS %u flat-front/hinge/rear-left-tab geometry assertions\n",checks);
    return 0;
}
