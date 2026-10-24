"""Read-only state and launch contracts for the native MeadowOS entry."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess
from urllib.parse import urlsplit

AGENT_UNIT = "meadow-agent.service"


def state_root() -> Path:
    return Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "meadow-os"


def valid_private_url(value: object) -> str | None:
    """Only permit web addresses; never turn a local metadata file into code."""
    if not isinstance(value, str) or len(value) > 4096 or any(c.isspace() for c in value):
        return None
    try:
        parsed = urlsplit(value)
        if parsed.username or parsed.password or not parsed.hostname:
            return None
        if parsed.scheme == "https" or (
            parsed.scheme == "http" and parsed.hostname in {"localhost", "127.0.0.1", "::1"}
        ):
            _ = parsed.port  # Validate malformed ports as well.
            return value
    except ValueError:
        pass
    return None


def read_metadata(root: Path) -> tuple[dict, str | None]:
    path = root / "status.json"
    try:
        if path.stat().st_size > 65536:
            return {}, "狀態檔案過大，無法讀取。"
        value = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(value, dict):
            raise ValueError("expected object")
        return value, None
    except FileNotFoundError:
        return {}, None
    except (OSError, ValueError):
        return {}, "無法讀取系統狀態，請重新檢查。"


def executable(path: Path) -> bool:
    return path.is_file() and os.access(path, os.X_OK)


def agent_state() -> str:
    """Service activity is a service signal, not model or Agent health proof."""
    try:
        result = subprocess.run(
            ["systemctl", "--user", "is-active", AGENT_UNIT],
            capture_output=True, text=True, timeout=2, check=False,
        )
        value = result.stdout.strip()
        if value in {"active", "activating", "inactive", "failed", "deactivating"}:
            return value
    except (OSError, subprocess.TimeoutExpired):
        pass
    return "unknown"


def collect_state(prefix: Path = Path("/opt/meadow"), root: Path | None = None) -> dict:
    root = root or state_root()
    metadata, metadata_error = read_metadata(root)
    desktop = prefix / "bin/meadow-desktop"
    session = prefix / "share/meadow-os/bin/meadow-session"
    private_url = valid_private_url(metadata.get("private_url"))
    if metadata.get("private_url") and not private_url:
        metadata_error = "私人網址格式無效，請檢查設定。"
    return {
        "agent_state": agent_state(),
        "desktop_installed": executable(desktop),
        "desktop_launcher_installed": executable(session),
        "desktop_path": str(desktop),
        "desktop_launcher": str(session),
        "private_url": private_url,
        "metadata_error": metadata_error,
        "workspace": str(Path.home() / "MeadowWork"),
        "browser": next((p for name in ("google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "firefox") if (p := shutil.which(name))), None),
        "file_manager": shutil.which("thunar"),
        "settings": shutil.which("xfce4-settings-manager"),
    }


def public_state(state: dict) -> dict:
    """CLI diagnostics expose configuration presence, never a pairing URL/token."""
    return {k: v for k, v in state.items() if k != "private_url"} | {
        "private_url_configured": bool(state.get("private_url"))
    }
