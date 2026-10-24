#!/usr/bin/python3
"""Shared installed-application presentation and System Settings destinations.

Desktop metadata controls presentation only. Original desktop launchers and native
settings processes retain their actions and permissions.
"""
from __future__ import annotations

import json
from pathlib import Path
import shutil

# command arguments are constant, never copied from a search or user string.
# --socket-id support was checked against the installed Xfce 4.20 tool help.
SETTINGS_TOOLS = (
    ("appearance", "進階外觀", "字型與原生介面的詳細選項", "preferences-desktop-appearance-symbolic", ("xfce4-appearance-settings",), True, "appearance"),
    ("display", "顯示器", "排列顯示器與調整解析度", "preferences-desktop-display-symbolic", ("xfce4-display-settings",), True, "display"),
    ("keyboard", "鍵盤", "鍵盤配置、輸入與快捷鍵", "input-keyboard-symbolic", ("xfce4-keyboard-settings",), True, "keyboard"),
    ("mouse", "滑鼠與觸控板", "指標速度、按鈕與捲動", "input-mouse-symbolic", ("xfce4-mouse-settings",), True, "mouse"),
    ("accessibility", "輔助使用", "鍵盤、滑鼠與輔助技術", "preferences-desktop-accessibility-symbolic", ("xfce4-accessibility-settings",), True, "accessibility"),
    ("windows", "視窗", "視窗外觀、焦點與鍵盤操作", "preferences-system-windows-symbolic", ("xfwm4-settings",), True, "windows"),
    ("window-tweaks", "視窗進階選項", "焦點、切換與工作區行為", "preferences-system-windows-symbolic", ("xfwm4-tweaks-settings",), True, "advanced"),
    ("workspaces", "工作區", "虛擬桌面的數量與名稱", "preferences-desktop-workspaces-symbolic", ("xfwm4-workspace-settings",), True, "workspaces"),
    ("startup", "登入項目", "工作階段與登入時開啟的項目", "preferences-system-session-symbolic", ("xfce4-session-settings",), True, "startup"),
    ("filetypes", "預設應用程式", "開啟檔案與連結的預設程式", "preferences-system-symbolic", ("xfce4-mime-settings",), True, "filetypes"),
    ("color", "色彩描述檔", "顯示器與裝置的色彩管理", "preferences-desktop-display-symbolic", ("xfce4-color-settings",), True, "color"),
    ("editor", "進階設定編輯器", "檢視及修改原生系統偏好", "preferences-system-symbolic", ("xfce4-settings-editor",), True, "advanced"),
    ("notifications", "通知", "應用程式通知偏好", "preferences-system-notifications-symbolic", ("xfce4-notifyd-config",), False, "advanced"),
    ("files", "檔案偏好設定", "檔案管理的顯示與操作選項", "system-file-manager", ("thunar-settings",), False, "applications"),
    ("terminal", "終端機偏好設定", "字型、配色與終端機操作", "utilities-terminal", ("xfce4-terminal", "--preferences"), False, "applications"),
)

# A single destination registry is shared by Launcher, Settings and inventory.
SETTINGS_DESTINATIONS = {
    "meadow-settings.desktop": "general",
    "meadow-welcome.desktop": "general",
    "xfce-settings-manager.desktop": "general",
    "xfce4-settings-manager.desktop": "general",
    "org.xfce.settingsmanager.desktop": "general",
    "xfce4-about.desktop": "general",
    "xfce-ui-settings.desktop": "appearance",
    "xfce-backdrop-settings.desktop": "appearance",
    "xfce-display-settings.desktop": "display",
    "xfce-keyboard-settings.desktop": "keyboard",
    "xfce-mouse-settings.desktop": "mouse",
    "xfce4-accessibility-settings.desktop": "accessibility",
    "xfce-wm-settings.desktop": "windows",
    "xfce-wmtweaks-settings.desktop": "advanced",
    "xfce-workspaces-settings.desktop": "workspaces",
    "xfce-session-settings.desktop": "startup",
    "xfce4-mime-settings.desktop": "filetypes",
    "xfce4-file-manager.desktop": "filetypes",
    "xfce4-terminal-emulator.desktop": "filetypes",
    "xfce4-web-browser.desktop": "filetypes",
    "xfce4-mail-reader.desktop": "filetypes",
    "xfce4-color-settings.desktop": "color",
    "xfce4-settings-editor.desktop": "advanced",
    "thunar-settings.desktop": "applications",
    "xfce4-terminal-settings.desktop": "applications",
    "org.xfce.terminal-settings.desktop": "applications",
    "panel-preferences.desktop": "advanced",
    "picom.desktop": "advanced",
    "x11vnc.desktop": "advanced",
    "xfce4-appfinder.desktop": "applications",
    "org.xfce.appfinder.desktop": "applications",
    "xfce4-run.desktop": "applications",
    "xfce4-session-logout.desktop": "startup",
}

APPLICATION_PRESENTATIONS = {
    "google-chrome.desktop": ("Google Chrome", "meadow-browser", False, 0),
    "google-chrome-stable.desktop": ("Google Chrome", "meadow-browser", False, 0),
    "chromium.desktop": ("Chromium", "meadow-browser", False, 0),
    "chromium-browser.desktop": ("Chromium", "meadow-browser", False, 0),
    "firefox.desktop": ("Firefox", "meadow-browser", False, 0),
    "thunar.desktop": ("檔案", "system-file-manager", False, 1),
    "org.xfce.thunar.desktop": ("檔案", "system-file-manager", False, 1),
    "xfce4-terminal.desktop": ("終端機", "utilities-terminal", False, 2),
    "org.xfce.terminal.desktop": ("終端機", "utilities-terminal", False, 2),
    "xterm.desktop": ("終端機", "utilities-terminal", False, 2),
    "meadow-desktop.desktop": ("Meadow Code", "meadow-desktop", False, 3),
    "meadow-settings.desktop": ("系統設定", "preferences-system", True, 4),
    "meadow-welcome.desktop": ("關於 MeadowOS", "meadow", True, 5),
    "xfce-settings-manager.desktop": ("原生設定管理員", "preferences-system", True, 6),
    "xfce4-settings-manager.desktop": ("原生設定管理員", "preferences-system", True, 6),
    "org.xfce.settingsmanager.desktop": ("原生設定管理員", "preferences-system", True, 6),
    "thunar-settings.desktop": ("檔案偏好設定", "system-file-manager", True, 7),
    "thunar-bulk-rename.desktop": ("批次重新命名", "system-file-manager", True, 8),
    "xfce4-appfinder.desktop": ("應用程式搜尋", "meadow-launcher", True, 9),
    "org.xfce.appfinder.desktop": ("應用程式搜尋", "meadow-launcher", True, 9),
    "xfce4-terminal-settings.desktop": ("終端機偏好設定", "utilities-terminal", True, 10),
    "org.xfce.terminal-settings.desktop": ("終端機偏好設定", "utilities-terminal", True, 10),
    "picom.desktop": ("視窗合成器", "preferences-system", True, 11),
    "x11vnc.desktop": ("遠端桌面", "computer", True, 12),
}


def settings_route(desktop_id: str | None, categories: str = "") -> str | None:
    known = SETTINGS_DESTINATIONS.get((desktop_id or "").casefold())
    return known or ("advanced" if "Settings" in (categories or "").split(";") else None)


def application_presentation(desktop_id, display_name, categories="", executable=""):
    desktop_id = desktop_id or ""
    known = APPLICATION_PRESENTATIONS.get(desktop_id.casefold())
    if known:
        title, icon, system, order = known
    else:
        title, icon, order = display_name, None, 100
        system = bool(settings_route(desktop_id, categories))
    page = settings_route(desktop_id, categories)
    return {"title": title, "icon": icon, "system": system, "order": order,
            "settings_page": page,
            "launcher_visible": page is None or desktop_id.casefold() == "meadow-settings.desktop",
            "search": " ".join((title, display_name, desktop_id, executable or "")).casefold()}


def launcher_entries(apps):
    """Keep real utilities; consolidate only destinations represented in Settings."""
    result = []
    for app in apps:
        presentation = application_presentation(app.get_id(), app.get_display_name(), app.get_categories(), app.get_executable())
        if presentation["launcher_visible"]:
            result.append(app)
    return result


def available_settings_tools() -> list[dict]:
    return [{"id": key, "title": title, "description": description, "icon": icon,
             "executable": path, "argv": [path, *command[1:]], "embed": embed, "page": page}
            for key, title, description, icon, command, embed, page in SETTINGS_TOOLS
            if (path := shutil.which(command[0]))]


def installed_applications() -> list[dict]:
    # GIO enumeration includes NoDisplay/OnlyShowIn entries. Never change the
    # session environment to force them into Launcher; inventory is independent.
    import gi
    gi.require_version("Gio", "2.0")
    from gi.repository import Gio
    records = []
    for app in Gio.AppInfo.get_all():
        if not isinstance(app, Gio.DesktopAppInfo) or not app.get_filename():
            continue
        presentation = application_presentation(app.get_id(), app.get_display_name(), app.get_categories(), app.get_executable())
        icon = app.get_icon()
        records.append({"desktop_id": app.get_id(), "name": app.get_display_name(),
                        "title": presentation["title"], "filename": app.get_filename(),
                        "commandline": app.get_commandline(), "executable": app.get_executable(),
                        "categories": app.get_categories() or "", "icon": icon.to_string() if icon else None,
                        "visible": app.should_show(), "hidden": app.get_is_hidden(), "nodisplay": app.get_nodisplay(),
                        "settings_page": presentation["settings_page"],
                        "launcher_visible": app.should_show() and presentation["launcher_visible"]})
    return sorted(records, key=lambda item: item["desktop_id"].casefold())


if __name__ == "__main__":
    print(json.dumps(installed_applications(), ensure_ascii=False, indent=2))
