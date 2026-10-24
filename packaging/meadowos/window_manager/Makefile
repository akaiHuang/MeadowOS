PKGS = gtk+-3.0 gdk-x11-3.0
CFLAGS ?= -O2
CFLAGS += -std=c11 -fPIC -Wall -Wextra -Werror $(shell pkg-config --cflags $(PKGS))
LDLIBS += $(shell pkg-config --libs $(PKGS)) -lm

all: libmeadow-hover-chrome.so

libmeadow-hover-chrome.so: meadow_hover_chrome.c
	$(CC) $(CFLAGS) -shared -o $@ $< $(LDLIBS)

clean:
	rm -f libmeadow-hover-chrome.so

.PHONY: all clean
