# syntax=docker/dockerfile:1
# Native MeadowOS development desktop. Build from the clean public Git clone.
FROM ubuntu:26.04 AS native-build
ENV DEBIAN_FRONTEND=noninteractive PYTHONDONTWRITEBYTECODE=1
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git python3 golang-go gcc g++ make pkg-config libc6-dev \
      libgtk-3-dev libwebkit2gtk-4.1-dev libthunarx-3-dev \
      libx11-dev libxcomposite-dev libxrender-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Only the audited, standalone public tree belongs in this build context.
# Git metadata is excluded: source revision is an explicit, validated build input.
COPY . .
RUN cd go && go mod download
ARG SOURCE_COMMIT
ARG MEADOW_BUILD_VERSION=0.1-dev
ARG MEADOW_WINDOW_RESOURCES=1
RUN python3 -c 'import re,sys; assert re.fullmatch(r"[0-9a-f]{40}", sys.argv[1]), "Pass --build-arg SOURCE_COMMIT=$(git rev-parse HEAD)"' "$SOURCE_COMMIT" \
 && task_arch="$(dpkg --print-architecture)" \
 && python3 scripts/meadowos/build.py --arch "$task_arch" \
      --source-commit "$SOURCE_COMMIT" --no-source-dirty \
      --build-version "$MEADOW_BUILD_VERSION" --output /build/native \
 && if [ "$MEADOW_WINDOW_RESOURCES" = 1 ]; then \
      python3 scripts/meadowos/build_window_resources.py \
        --arch "$task_arch" --output /build/window-resources; \
      python3 scripts/meadowos/image.py render --arch "$task_arch" \
        --build-version "$MEADOW_BUILD_VERSION" \
        --desktop-binary /build/native/bin/meadow-desktop \
        --agent-binary /build/native/libexec/meadow-agentd \
        --window-resources /build/window-resources --output /build/bundle; \
    elif [ "$MEADOW_WINDOW_RESOURCES" = 0 ]; then \
      python3 scripts/meadowos/image.py render --arch "$task_arch" \
        --build-version "$MEADOW_BUILD_VERSION" \
        --desktop-binary /build/native/bin/meadow-desktop \
        --agent-binary /build/native/libexec/meadow-agentd \
        --output /build/bundle; \
    else echo 'MEADOW_WINDOW_RESOURCES must be 0 or 1' >&2; exit 2; fi

FROM ubuntu:26.04 AS runtime
ENV DEBIAN_FRONTEND=noninteractive PYTHONDONTWRITEBYTECODE=1 \
    HOME=/home/meadow USER=meadow LOGNAME=meadow \
    LANG=C.UTF-8 LC_ALL=C.UTF-8 \
    MEADOW_PREFIX=/opt/meadow MEADOW_DISPLAY=:121 MEADOW_OS_SESSION=1 \
    MEADOW_VOICE_SPOOL=/home/meadow/.local/state/meadow-os/companion \
    XDG_SESSION_TYPE=x11 XDG_CURRENT_DESKTOP=Meadow:XFCE \
    LIBGL_ALWAYS_SOFTWARE=1 WEBKIT_DISABLE_DMABUF_RENDERER=1
# This is a dedicated container, not the VM provisioning script. Keep Ubuntu's
# command providers; no protected package replacement or host init changes.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash git ca-certificates curl openssh-client \
      python3 python3-venv python3-pip python3-xlib python3-gi python3-cairo \
      python3-gi-cairo python3-pyatspi python3-numpy python3-pil \
      gir1.2-gtk-3.0 gir1.2-atspi-2.0 gir1.2-wnck-3.0 gir1.2-webkit2-4.1 \
      poppler-utils ffmpeg gir1.2-gstreamer-1.0 gir1.2-gst-plugins-base-1.0 \
      gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-gtk3 gstreamer1.0-libav \
      nodejs npm golang-go gcc g++ make dpkg-dev libc6-dev libcrypt-dev pkg-config \
      libgtk-3-dev libthunarx-3-dev libwebkit2gtk-4.1-dev libxcb1 libx11-6 xclip \
      xfwm4 xfce4-settings xfce4-terminal thunar dbus-x11 dbus-user-session \
      at-spi2-core libglib2.0-bin x11-utils x11-xserver-utils xdotool \
      xdg-utils xdg-user-dirs desktop-file-utils libgtk-3-bin fonts-noto-core fonts-noto-cjk \
      libgl1-mesa-dri libglx-mesa0 xvfb x11vnc novnc websockify tini epiphany-browser \
 && rm -rf /var/lib/apt/lists/* \
 && if getent passwd ubuntu >/dev/null; then userdel ubuntu; fi \
 && useradd --uid 1000 --create-home --shell /bin/bash meadow \
 && install -d -o meadow -g meadow -m 0700 /home/meadow/.local/share /home/meadow/.local/state
COPY --from=native-build /build/bundle/payload/ /
COPY --from=native-build /build/bundle/manifest.json /opt/meadow/share/meadow-os/bundle-manifest.json
# Preserve these two paths: noVNC preparation resolves its cursor/assets by
# relative source layout. This clean tooling tree contains no Go model runtime.
COPY scripts/meadowos/ /usr/local/share/meadowos-source/scripts/meadowos/
COPY packaging/meadowos/desktop/icons/Meadow-Cursors/ /usr/local/share/meadowos-source/packaging/meadowos/desktop/icons/Meadow-Cursors/
COPY scripts/meadowos/configure_inception.py /usr/local/share/meadow-os/configure_inception.py
COPY docker/ /usr/local/lib/meadowos/
COPY LICENSE third_party/ /usr/local/share/meadowos-licenses/
COPY go/desktop/frontend/vendor/*LICENSE* go/desktop/frontend/vendor/*NOTICE* /usr/local/share/meadowos-licenses/frontend/
COPY go/desktop/frontend/icons/nearby-lucide-LICENSE.txt /usr/local/share/meadowos-licenses/frontend/
RUN install -m 0755 /usr/local/lib/meadowos/meadowos /usr/local/bin/meadowos \
 && install -m 0755 /usr/local/lib/meadowos/meadowos-healthcheck /usr/local/bin/meadowos-healthcheck \
 && install -m 0755 /usr/local/lib/meadowos/meadowos-run /usr/local/bin/meadowos-run \
 && gtk-update-icon-cache -f -t /usr/share/icons/Meadow \
 && update-desktop-database /usr/share/applications
USER meadow:meadow
WORKDIR /home/meadow
EXPOSE 6080 6081
HEALTHCHECK --interval=15s --timeout=10s --start-period=60s --retries=4 \
  CMD ["/usr/local/bin/meadowos-healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/meadowos"]
