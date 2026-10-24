#!/bin/sh
set -eu
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ "$#" -ne 2 ] || [ "$1" != "--output" ]; then
    echo "usage: $0 --output DIRECTORY" >&2
    exit 2
fi
mkdir -p -- "$2"
# An explicit extracted official development-package root supports an isolated
# build without installing packages into a running guest. GTK still uses its
# normal system headers; Thunarx is already a runtime dependency of Thunar.
if [ -n "${MEADOW_FOLDER_BUILD_SYSROOT:-}" ]; then
    thunar_include="$MEADOW_FOLDER_BUILD_SYSROOT/usr/include/thunarx-3"
else
    thunar_include=$(pkg-config --variable=includedir thunarx-3)/thunarx-3
fi
cc -O2 -std=c11 -fPIC -Wall -Wextra -Werror -shared \
    $(pkg-config --cflags gtk+-3.0 gio-2.0 gdk-pixbuf-2.0 gmodule-2.0) \
    -I"$thunar_include" \
    -o "$2/libmeadow-folder-materials.so" \
    "$source_dir/meadow_folder_materials.c" "$source_dir/meadow_folder_paint.c" \
    $(pkg-config --libs gtk+-3.0 gio-2.0 gdk-pixbuf-2.0 gmodule-2.0) \
    -l:libthunarx-3.so.0 -lm
