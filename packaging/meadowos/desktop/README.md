# MeadowOS native desktop entry

This is a real Python/PyGObject GTK3 launcher, not a Web preview or another
chat/Agent implementation. It uses the existing Meadow double-wave asset and
dark/light frontend palette. The original asset is copied without alteration.

## Payload contract

Copy this directory to `/opt/meadow/share/meadow-os/desktop`. Symlink
`bin/meadow-welcome` and `bin/meadow-theme` into `/opt/meadow/bin`, and install
`share/applications/meadow-welcome.desktop` in `/usr/share/applications`.
The theme directories may be installed in `/usr/share/themes`; otherwise an
explicit theme application installs them in the current user's XDG data root.

Requires Python3, python3-gi, gir1.2-gtk-3.0, GTK3's Adwaita resources, SVG
pixbuf support and Noto fonts. Xfce appearance application additionally requires
xfconf-query, DISPLAY and the graphical user's D-Bus session. This entry does
not start or replace Agent services.

Desktop launches through `/opt/meadow/share/meadow-os/bin/meadow-session desktop`
so it shares the Agent profile. Its installed binary must also exist at
`/opt/meadow/bin/meadow-desktop`. The platform payload provides those entries.
Files opens `$HOME/MeadowWork` if present, otherwise Home; browser and system
settings actions are enabled only when their executables are present.

Service status comes from a current `systemctl --user is-active
meadow-agent.service` call; this is not a model-health assertion. URL metadata
comes from `$XDG_STATE_HOME/meadow-os/status.json` (`~/.local/state` default),
with `private_url` null until configured. A configured URL is not shown as a
verified reachable endpoint. Pairing URLs never appear in status CLI output.

The first-login process may call `meadow-theme --apply-defaults` once a real
graphical session exists. Appearance and wallpaper completion are recorded
separately. A missing desktop channel or uninitialized backdrop returns an
error and remains pending; a retry does not reset the already-applied appearance.
A fully successful marker prevents subsequent logins from overriding user
appearance changes. The visible theme buttons are explicit
requests to change appearance. They do not alter the canonical Meadow Desktop
frontend or its quiet task UI. Initial defaults do not alter user panel layout,
shortcuts or compositor configuration.

## Shared native frame themes

`themes/Meadow-Light/xfwm4` and `themes/Meadow-Dark/xfwm4` contain original,
source-generated XPM assets, with a 24px title strip and 3px resize borders.
Each has the complete 60-file xfwm4 state inventory, including active/inactive
frames and normal/hover/pressed/toggled buttons. The visible traffic-light dots
are 12px; every button has an **opaque 20x24 canvas** matching the title surface.
Xfwm4 uses the XPM mask as the button shape, so transparent padding would have
incorrectly reduced the hit area to the dot. State changes preserve geometry.
`assets.json` records asset hashes and generation dimensions; no vendor image
was copied. The generator is `tests/native_frame_assets.py`.

The appearance plan selects the matching GTK and xfwm4 theme. Its `CHM|` layout
places close, minimize and maximize/restore on the left. Native application
constraints remain in force: non-resizable/dialog windows retain only their
allowed controls, and close uses normal WM/application behavior. This change
does not alter focus, shortcuts, the compositor or window content geometry on
hover. The native title strip remains present; global hover auto-hide is not
implemented.

The plan also publishes `xsettings` `/Gtk/DecorationLayout` as
`close,minimize,maximize:` for GTK client-side decorations. Individual apps may
override that setting. GTK HeaderBars and Chrome's custom frame are not xfwm4
decorations; this theme does not promise a uniform height/style for them or
silently change Chrome's frame preference. The green native button retains
xfwm4 maximize/restore semantics, not macOS fullscreen behavior.

Theme availability requires the GTK assets, themerc, provenance and every
native state file. Both legacy `~/.themes` and XDG user locations are checked;
xfwm4 searches the legacy location first. GTK-only, empty or symlinked/incomplete user themes are not
reported as installed; existing user files are preserved with a clear error.
When no complete system theme is present, explicit application can install the
complete bundled theme into the user's XDG data root. `--print-plan` validates
the bundled assets and prints commands without installing or applying them;
Meadow Shell can consume that appearance plan without requiring xfdesktop's
wallpaper channel. Existing successful default markers still preserve user
customizations; upgrading assets alone does not force an appearance reset.

Read-only asset regeneration check:

```sh
python3 packaging/meadowos/desktop/tests/native_frame_assets.py
packaging/meadowos/desktop/bin/meadow-theme --print-plan light
```

The tests parse every XPM, verify hashes, fixed state dimensions, opaque button
canvases, 12px dots, left-control plans, and incomplete installation handling.
These are asset/fixture checks. Actual native Thunar/Terminal title controls,
drag/resize/focus, maximize/restore and dialogs need the separate Linux GUI
acceptance run; they are not proved by the generated assets.

## Focused checks

```sh
python3 -m unittest discover -s packaging/meadowos/desktop/tests -v
packaging/meadowos/desktop/bin/meadow-welcome --status-json
```

Real GTK/X11 render (with dependencies installed):

```sh
dbus-run-session -- xvfb-run -a -s '-screen 0 1280x900x24' \
  packaging/meadowos/desktop/bin/meadow-welcome --theme dark --smoke /tmp/meadow-dark.png
dbus-run-session -- xvfb-run -a -s '-screen 0 1280x900x24' \
  packaging/meadowos/desktop/bin/meadow-welcome --theme light --smoke /tmp/meadow-light.png
```

`--smoke` captures the actual GTK window after rendering; no fixture replaces
service, app or URL checks. `--theme` previews that window without changing the
user's desktop. A container X11 capture is not a booted VM, working Agent chat,
private URL or model acceptance. The status refresh action is read-only.
