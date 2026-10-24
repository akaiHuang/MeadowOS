#!/bin/sh
set -eu
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ "$#" -ne 2 ] || [ "$1" != "--output" ]; then
    echo "usage: $0 --output DIRECTORY" >&2
    exit 2
fi
mkdir -p -- "$2"
cc -O2 -std=c11 -fPIC -Wall -Wextra -Werror -shared \
    $(pkg-config --cflags gtk+-3.0 gdk-x11-3.0) \
    -o "$2/libmeadow-hover-chrome.so" "$source_dir/meadow_hover_chrome.c" \
    $(pkg-config --libs gtk+-3.0 gdk-x11-3.0) -lm
