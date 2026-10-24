#!/usr/bin/env python3
"""Exercise a fresh container's actual native apps and WebKit windows, no model."""
import json
import os
from pathlib import Path
import subprocess
import time


def main():
    from run_in_session import session_environment
    env = session_environment()
    os.environ.update(env)
    status = json.loads(subprocess.check_output(["/usr/local/bin/meadowos-healthcheck"], text=True))
    if status.get("model_ready"):
        raise RuntimeError("Fresh smoke unexpectedly inherited a model configuration")
    home = Path.home()
    profile = home / ".local/share/meadow-os/agent"
    if (profile / "model-connections-v1.json").exists() or (profile / "model-connections-v1.json.credentials").exists():
        raise RuntimeError("Fresh smoke unexpectedly inherited provider state")
    folder = home / "MeadowWork/MeadowOS smoke"
    folder.mkdir(mode=0o700, parents=True, exist_ok=True)
    (folder / "persistence.txt").write_text("MeadowOS named home volume\n")
    output = home / ".local/state/meadow-os/smoke"
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    children, logs, visible, captures = [], [], [], []

    def start(name, command):
        log = (output / (name + ".log")).open("wb")
        logs.append(log)
        child = subprocess.Popen(["/usr/local/bin/meadowos-run", *command], env=env,
                                 stdout=log, stderr=subprocess.STDOUT)
        children.append((name, child))
        return child

    def windows():
        search = subprocess.run(["xdotool", "search", "--onlyvisible", "--name", "."],
                                env=env, text=True, capture_output=True)
        result = []
        for xid in search.stdout.split():
            title = subprocess.run(["xdotool", "getwindowname", xid], env=env,
                                   text=True, capture_output=True)
            if title.returncode == 0:
                result.append({"xid": xid, "title": title.stdout.strip()})
        return result

    def wait_windows(required, seconds=30):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            current = windows()
            if all(any(title in window["title"] for window in current) for title in required):
                return current
            failed = [(name, child.returncode) for name, child in children
                      if child.poll() not in (None, 0)]
            if failed:
                raise RuntimeError(f"Native app exited before mapping: {failed}")
            time.sleep(.25)
        raise RuntimeError(f"Native windows did not map: {required}; visible={windows()}")

    def focus(title):
        current = windows()
        xid = next(window["xid"] for window in current if title in window["title"])
        subprocess.run(["xdotool", "windowactivate", "--sync", xid], env=env, check=True, timeout=5)

    def capture(name):
        # Let the newly focused native surface finish its first compositor frame.
        time.sleep(.5)
        import gi
        gi.require_version("Gdk", "3.0")
        from gi.repository import Gdk
        display = Gdk.Display.get_default()
        if display is None:
            raise RuntimeError("X11 display unavailable")
        display.sync()
        image = Gdk.pixbuf_get_from_window(Gdk.get_default_root_window(), 0, 0, 1920, 1080)
        if image is None:
            raise RuntimeError("Native root capture unavailable")
        image.savev(str(output / name), "png", [], [])
        captures.append(name)

    def agent_pids():
        expected = Path("/opt/meadow/libexec/meadow-agentd").resolve()
        result = []
        for path in Path("/proc").glob("[0-9]*/exe"):
            try:
                if path.resolve() == expected:
                    result.append(int(path.parent.name))
            except (OSError, RuntimeError):
                continue
        return sorted(result)

    result = {"native_x11": True, "geometry": [1920, 1080], "agent_connected": status["agent_connected"],
              "model_configured": False, "model_task": "not_run", "vm_boot_verified": False,
              "persistence_file": str(folder / "persistence.txt"), "sandbox_configuration": "unchanged"}
    try:
        original_agents = agent_pids()
        if len(original_agents) != 1:
            raise RuntimeError(f"Fresh desktop must have one native Agent; found {original_agents}")
        start("files", ["thunar", str(folder)])
        start("terminal", ["xfce4-terminal", "--disable-server", "--title=MeadowOS terminal"])
        start("settings", ["/opt/meadow/bin/meadow-settings"])
        visible = wait_windows(["MeadowOS terminal", "MeadowOS smoke", "Meadow 設定"])
        focus("MeadowOS terminal")
        capture("native-desktop.png")

        page = folder / "browser-smoke.html"
        page.write_text("""<!doctype html><html lang="en"><meta charset="utf-8">
<title>MeadowOS browser smoke</title><style>
body{margin:64px;background:#f3f6f2;color:#153b31;font:22px system-ui}
h1{font-size:52px;letter-spacing:-2px}p{max-width:650px;line-height:1.6}
</style><h1>MeadowOS browser smoke</h1><p>This local page is rendered by GNOME Web.
No model, remote page or API credentials are used for this check.</p></html>""")
        browser_profile = output / "browser-profile"
        browser_profile.mkdir(mode=0o700)
        start("browser", ["epiphany", "--private-instance", "--profile=" + str(browser_profile), page.as_uri()])
        visible = wait_windows(["MeadowOS browser smoke"])
        focus("MeadowOS browser smoke")
        capture("native-browser.png")
        result["browser_window_mapped"] = True

        start("native-desktop", ["/opt/meadow/share/meadow-os/bin/meadow-session", "desktop"])
        visible = wait_windows(["MeadowOS · 開發版"])
        focus("MeadowOS · 開發版")
        capture("native-api-desktop.png")
        current_agents = agent_pids()
        if current_agents != original_agents:
            raise RuntimeError(f"Managed Desktop changed the sole Agent: {original_agents} -> {current_agents}")
        result.update(native_desktop_window_mapped=True, agent_pids=current_agents,
                      status="passed_window_mapping; screenshots_require_visual_review")
    except Exception as error:
        result.update(status="failed", error=str(error))
        try:
            capture("failure-desktop.png")
        except Exception as capture_error:
            result["capture_error"] = str(capture_error)
        raise
    finally:
        result.update(windows=windows(), captures=captures)
        (output / "smoke.json").write_text(json.dumps(result, indent=2) + "\n")
        for _name, child in reversed(children):
            if child.poll() is None:
                child.terminate()
        for _name, child in reversed(children):
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
        for log in logs:
            log.close()


if __name__ == "__main__":
    main()
