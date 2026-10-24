#!/usr/bin/python3
"""First-login Xfce defaults; subsequent logins preserve user customizations."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

PAYLOAD = Path(__file__).resolve().parent
sys.path.insert(0, str(PAYLOAD))
from meadow_state import state_root


def preference_commands(theme: str, query: str = "xfconf-query") -> list[list[str]]:
    # Keep the existing compositor/window manager, shortcuts and user panel layout.
    selected = "Meadow-Light" if theme == "light" else "Meadow-Dark"
    return [
        [query, "-c", "xsettings", "-p", "/Gtk/CursorThemeName", "--create", "-t", "string", "-s", "Meadow-Cursors"],
        [query, "-c", "xsettings", "-p", "/Gtk/CursorThemeSize", "--create", "-t", "int", "-s", "32"],
        [query, "-c", "xsettings", "-p", "/Net/ThemeName", "--create", "-t", "string", "-s", selected],
        [query, "-c", "xsettings", "-p", "/Net/IconThemeName", "--create", "-t", "string", "-s", "Meadow"],
        [query, "-c", "xsettings", "-p", "/Gtk/FontName", "--create", "-t", "string", "-s", "Noto Sans 10"],
        [query, "-c", "xsettings", "-p", "/Gtk/MonospaceFontName", "--create", "-t", "string", "-s", "Monospace 10"],
        [query, "-c", "xsettings", "-p", "/Gtk/DecorationLayout", "--create", "-t", "string", "-s", "close,minimize,maximize:"],
        # These are native application preferences, not a wrapper around an app.
        # Thunar puts its existing location toolbar into its real GtkHeaderBar.
        [query, "-c", "xsettings", "-p", "/Gtk/DialogsUseHeader", "--create", "-t", "bool", "-s", "true"],
        [query, "-c", "thunar", "-p", "/misc-use-csd", "--create", "-t", "bool", "-s", "true"],
        [query, "-c", "thunar", "-p", "/last-menubar-visible", "--create", "-t", "bool", "-s", "false"],
        [query, "-c", "thunar", "-p", "/misc-symbolic-icons-in-sidepane", "--create", "-t", "bool", "-s", "true"],
        [query, "-c", "thunar", "-p", "/misc-symbolic-icons-in-toolbar", "--create", "-t", "bool", "-s", "true"],
        [query, "-c", "thunar", "-p", "/last-statusbar-visible", "--create", "-t", "bool", "-s", "true"],
        [query, "-c", "thunar", "-p", "/last-separator-position", "--create", "-t", "int", "-s", "190"],
        [query, "-c", "thunar", "-p", "/last-location-bar", "--create", "-t", "string", "-s", "ThunarLocationButtons"],
        [query, "-c", "thunar", "-p", "/shortcuts-icon-size", "--create", "-t", "string", "-s", "THUNAR_ICON_SIZE_16"],
        [query, "-c", "thunar", "-p", "/misc-small-toolbar-icons", "--create", "-t", "bool", "-s", "false"],
        [query, "-c", "xfwm4", "-p", "/general/box_move", "--create", "-t", "bool", "-s", "false"],
        [query, "-c", "xfwm4", "-p", "/general/box_resize", "--create", "-t", "bool", "-s", "false"],
        [query, "-c", "xfwm4", "-p", "/general/theme", "--create", "-t", "string", "-s", selected],
        [query, "-c", "xfwm4", "-p", "/general/button_layout", "--create", "-t", "string", "-s", "CHM|"],
        [query, "-c", "xfwm4", "-p", "/general/title_font", "--create", "-t", "string", "-s", "Noto Sans 9"],
    ]


def seed_browser_preferences(config_root: Path | None = None) -> dict:
    """Fresh Chrome/Chromium profiles use the native Meadow WM decoration.

    Existing profiles (including a running browser) are never rewritten. This
    is a first-login appearance default, not a browser fork or a managed policy.
    """
    root = config_root or Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home() / ".config")))
    result = {}
    for browser in ("google-chrome", "chromium"):
        profile = root / browser
        if profile.exists() or profile.is_symlink() or root.is_symlink():
            result[browser] = "preserved"
            continue
        try:
            profile.mkdir(parents=True, mode=0o700)
            default = profile / "Default"
            default.mkdir(mode=0o700)
            with (default / "Preferences").open("x") as stream:
                json.dump({"browser": {"custom_chrome_frame": False}}, stream)
                stream.write("\n")
            (default / "Preferences").chmod(0o600)
            result[browser] = "native_frame_default"
        except FileExistsError:
            result[browser] = "preserved"
    return result


def native_frame_files() -> set[str]:
    frames = {f"{part}-{state}.xpm" for state in ("active", "inactive") for part in
              [*(f"title-{index}" for index in range(1, 6)), "top-left", "top-right", "bottom-left", "bottom-right", "left", "right", "bottom"]}
    buttons = {f"{part}-{state}.xpm" for state in ("active", "inactive", "prelight", "pressed") for part in
               ["close", "hide", "maximize", "menu", "shade", "stick", "maximize-toggled", "shade-toggled", "stick-toggled"]}
    return frames | buttons


def theme_complete(directory: Path) -> bool:
    required = ["index.theme", "gtk-3.0/gtk.css", "gtk-3.0/sidebar-states.css", "xfwm4/themerc", "xfwm4/assets.json",
                *(f"gtk-3.0/assets/window-{name}.svg" for name in ("close", "minimize", "maximize", "inactive")),
                *(f"xfwm4/{name}" for name in native_frame_files())]
    # GTK-only or empty per-user directories must never count as an installed
    # native frame theme; an incomplete user directory may shadow the system one.
    return (not directory.is_symlink() and not (directory / "gtk-3.0").is_symlink()
        and not (directory / "gtk-3.0/assets").is_symlink()
        and not (directory / "xfwm4").is_symlink() and all(
        not (directory / relative).is_symlink() and (directory / relative).is_file()
        and (directory / relative).stat().st_size > 0 for relative in required))


def existing_user_theme(selected: str) -> Path | None:
    candidates = [Path.home() / ".themes" / selected,
                  Path(os.environ.get("XDG_DATA_HOME", str(Path.home() / ".local/share"))) / "themes" / selected]
    first = None
    for directory in candidates:
        if directory.exists() or directory.is_symlink():
            # Xfwm4 searches legacy ~/.themes first. Validate both user locations
            # so neither native nor GTK resolution can shadow a complete theme.
            if not theme_complete(directory):
                raise RuntimeError(f"Theme directory is incomplete; existing user files were preserved: {directory}")
            first = first or directory
    return first


def ensure_theme_available(selected: str) -> Path:
    existing = existing_user_theme(selected)
    if existing:
        return existing
    installed_theme = Path(os.environ.get("XDG_DATA_HOME", str(Path.home() / ".local/share"))) / "themes" / selected
    system_theme = Path("/usr/share/themes") / selected
    if theme_complete(system_theme):
        return system_theme
    source = PAYLOAD / "themes" / selected
    if not theme_complete(source):
        raise RuntimeError(f"Bundled theme is incomplete: {selected}")
    installed_theme.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(source, installed_theme)
    if not theme_complete(installed_theme):
        raise RuntimeError(f"Theme installation is incomplete: {installed_theme}")
    return installed_theme


def meadow_session() -> bool:
    return ("meadow" in os.environ.get("XDG_CURRENT_DESKTOP", "").casefold().split(":")
            or os.environ.get("DESKTOP_SESSION", "").casefold() == "meadow")


def verify_preferences(commands: list[list[str]]) -> None:
    for command in commands:
        result = subprocess.run(command[:5], capture_output=True, text=True, timeout=3)
        expected = command[-1]
        actual = result.stdout.strip()
        if command[7] == "bool":
            actual, expected = actual.casefold(), expected.casefold()
        if result.returncode or actual != expected:
            raise RuntimeError("Xfce could not confirm a saved appearance preference; defaults are incomplete.")


def store_theme(root: Path, theme: str, *, wallpaper_applied: bool, wallpaper_owner: str = "xfdesktop") -> None:
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=".theme-", dir=root)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump({"version": 1, "theme": theme, "appearance_applied": True,
                       "wallpaper_applied": wallpaper_applied, "wallpaper_owner": wallpaper_owner}, stream)
            stream.write("\n")
        os.replace(temporary, root / "theme.json")
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def apply_theme(theme: str, root: Path, *, defaults: bool) -> dict:
    marker = root / "theme.json"
    previous = {}
    if defaults and marker.exists():
        try:
            previous = json.loads(marker.read_text())
            if not isinstance(previous, dict):
                raise ValueError("expected theme object")
        except (OSError, ValueError):
            raise RuntimeError("Theme state is unreadable; appearance was left unchanged.")
        # Old successful markers did not have per-component fields.
        if previous.get("wallpaper_applied", True) or previous.get("wallpaper_owner") == "shell":
            return {"changed": False, "reason": "User defaults were already applied; later customizations are preserved."}
        theme = "light" if previous.get("theme") == "light" else "dark"
    query = shutil.which("xfconf-query")
    if not query or not os.environ.get("DISPLAY") or not os.environ.get("DBUS_SESSION_BUS_ADDRESS"):
        raise RuntimeError("An active Xfce X11 session, D-Bus and xfconf-query are required; no defaults were recorded.")
    selected = "Meadow-Light" if theme == "light" else "Meadow-Dark"
    ensure_theme_available(selected)
    shell_wallpaper = meadow_session()
    if not defaults or not previous.get("appearance_applied"):
        commands = preference_commands(theme, query)
        for argv in commands:
            result = subprocess.run(argv, capture_output=True, text=True, timeout=3)
            if result.returncode:
                raise RuntimeError("Xfce could not apply a theme preference; defaults are incomplete.")
        verify_preferences(commands)
        seed_browser_preferences()
        # Only the actually completed component is recorded. A later retry must
        # not reset a user's theme while waiting for xfdesktop to initialize.
        store_theme(root, theme, wallpaper_applied=False, wallpaper_owner="shell" if shell_wallpaper else "xfdesktop")
    if shell_wallpaper:
        # Meadow Shell observes the native theme and owns its own wallpaper.
        # A successful preference write does not prove the Shell painted it.
        store_theme(root, theme, wallpaper_applied=False, wallpaper_owner="shell")
        return {"changed": True, "theme": theme, "appearance_applied": True,
                "wallpaper_owner": "shell", "wallpaper_applied": False}
    deadline = time.monotonic() + 2.0
    while True:
        listed = subprocess.run([query, "-c", "xfce4-desktop", "-l"], capture_output=True, text=True, timeout=3)
        if listed.returncode:
            raise RuntimeError("Xfce desktop settings are unavailable; wallpaper remains pending and will be retried.")
        backgrounds = [p for p in listed.stdout.splitlines() if p.startswith("/backdrop/") and p.endswith("/last-image")]
        if backgrounds:
            break
        if time.monotonic() >= deadline:
            raise RuntimeError("Xfce has not initialized its desktop backgrounds; wallpaper remains pending and will be retried.")
        time.sleep(0.2)
    wallpaper = PAYLOAD / "assets" / f"wallpaper-{theme}.svg"
    for prop in backgrounds:
        result = subprocess.run([query, "-c", "xfce4-desktop", "-p", prop, "-s", str(wallpaper)], capture_output=True, text=True, timeout=3)
        if result.returncode:
            raise RuntimeError("Xfce could not apply the desktop background; wallpaper remains pending.")
    store_theme(root, theme, wallpaper_applied=True)
    return {"changed": True, "theme": theme, "backgrounds_updated": len(backgrounds)}


def main() -> int:
    parser = argparse.ArgumentParser(description="Apply Meadow Xfce appearance preferences")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--apply-defaults", action="store_true", help="Apply dark defaults once per user; retain later customizations")
    group.add_argument("--set", choices=("dark", "light"), dest="theme", help="Explicit user-requested appearance change")
    group.add_argument("--print-plan", choices=("dark", "light"), help="Read-only command plan")
    args = parser.parse_args()
    try:
        if args.print_plan:
            selected = "Meadow-Light" if args.print_plan == "light" else "Meadow-Dark"
            if not theme_complete(PAYLOAD / "themes" / selected):
                raise RuntimeError(f"Bundled theme is incomplete: {selected}")
            existing_user_theme(selected)
            print(json.dumps(preference_commands(args.print_plan), indent=2))
            return 0
        result = apply_theme(args.theme or "dark", state_root(), defaults=args.apply_defaults)
        print(json.dumps(result))
        return 0
    except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
