#!/usr/bin/env python3
"""Exercise a fresh container's actual native apps and WebKit windows, no model."""
import json
import os
from pathlib import Path
import subprocess
import time


def known_namespace_restriction(log):
    # Admit only the exact restriction observed in the native Linux CI.
    # Other crashes, blank UI, timeouts and launch failures remain failures.
    return ("bwrap: No permissions to create a new namespace" in log
            and "Failed to fully launch dbus-proxy: Child process exited with code 1" in log)


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
    showcase = home / "MeadowWork/Welcome-to-MeadowOS"
    for name in ("Notes", "Documents", "Design"):
        (showcase / name).mkdir(mode=0o700, parents=True, exist_ok=True)
    (showcase / "README.txt").write_text("Welcome to MeadowOS.\nThese are local demonstration files for the native desktop smoke.\nNo model reply or credential is present.\n")
    (showcase / "Notes/Welcome.txt").write_text("Welcome to MeadowOS.\nThis is a real local example document.\n")
    (showcase / "Documents/Project-notes.txt").write_text("Project notes\nDraft ideas and documents belong in this folder.\n")
    (showcase / "Design/Palette.txt").write_text("Meadow green / warm white / soft graphite\n")
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

    def wait_windows(required, seconds=30, watched=None):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            current = windows()
            if all(any(title in window["title"] for window in current) for title in required):
                return current
            failed = [(name, child.returncode) for name, child in (watched or children)
                      if child.poll() not in (None, 0)]
            if failed:
                raise RuntimeError(f"Native app exited before mapping: {failed}")
            time.sleep(.25)
        raise RuntimeError(f"Native windows did not map: {required}; visible={windows()}")

    def window_id(title):
        return next(window["xid"] for window in windows() if title in window["title"])

    def focus(title):
        xid = window_id(title)
        subprocess.run(["xdotool", "windowactivate", "--sync", xid], env=env, check=True, timeout=5)
        return xid

    def arrange(title, x, y, width, height):
        xid = focus(title)
        subprocess.run(["xdotool", "windowsize", "--sync", xid, str(width), str(height)], env=env, check=True, timeout=5)
        subprocess.run(["xdotool", "windowmove", "--sync", xid, str(x), str(y)], env=env, check=True, timeout=5)
        return xid

    def minimize(title):
        xid = window_id(title)
        subprocess.run(["xdotool", "windowminimize", "--sync", xid], env=env, check=True, timeout=5)
        return xid

    def restore(xid):
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

    def probe_webkit(name, command, title, geometry, screenshot, *, settle_seconds=0):
        child = start(name, command)
        try:
            wait_windows([title], watched=[(name, child)])
            if settle_seconds:
                time.sleep(settle_seconds)
            arrange(title, *geometry)
            capture(screenshot)
            if child.poll() not in (None, 0):
                raise RuntimeError(f"{name} exited after mapping: {child.returncode}")
            log = (output / (name + ".log")).read_text(errors="replace")
            if known_namespace_restriction(log):
                raise RuntimeError(f"{name} reported the known namespace restriction")
            minimize(title)
            return {"status": "mapped_requires_visual_review", "window_mapped": True,
                    "rendered_content_verified": False, "screenshot": screenshot}
        except (RuntimeError, StopIteration, subprocess.SubprocessError) as error:
            log = (output / (name + ".log")).read_text(errors="replace")
            if not known_namespace_restriction(log):
                raise
            capture(name + "-unsupported.png")
            return {"status": "unsupported_container_namespace", "window_mapped": False,
                    "reason": "bwrap namespace/dbus-proxy denied by unchanged container policy",
                    "log": name + ".log"}

    sandbox_runtime = {}
    for name, filename in {"apparmor": "/proc/self/attr/current",
                           "unprivileged_userns_clone": "/proc/sys/kernel/unprivileged_userns_clone",
                           "apparmor_restrict_unprivileged_userns": "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"}.items():
        try:
            sandbox_runtime[name] = Path(filename).read_text()[:4096].strip()
        except OSError:
            sandbox_runtime[name] = "unavailable"
    sandbox_runtime["process_filters"] = [line for line in Path("/proc/self/status").read_text().splitlines()
                                           if line.startswith(("NoNewPrivs:", "Seccomp:"))]
    sandbox_runtime["proc_mount_targets"] = [line.split()[4] for line in Path("/proc/self/mountinfo").read_text().splitlines()
                                              if len(line.split()) > 4 and line.split()[4].startswith("/proc")]
    (output / "sandbox-runtime.json").write_text(json.dumps(sandbox_runtime, indent=2) + "\n")

    result = {"native_x11": True, "geometry": [1920, 1080], "agent_connected": status["agent_connected"],
              "model_configured": False, "model_task": "not_run", "vm_boot_verified": False,
              "persistence_file": str(folder / "persistence.txt"), "sandbox_configuration": "unchanged"}
    try:
        original_agents = agent_pids()
        if len(original_agents) != 1:
            raise RuntimeError(f"Fresh desktop must have one native Agent; found {original_agents}")
        start("files", ["thunar", str(showcase)])
        start("terminal", ["xfce4-terminal", "--disable-server", "--title=MeadowOS terminal"])
        start("settings", ["/opt/meadow/bin/meadow-settings"])
        visible = wait_windows(["MeadowOS terminal", "Welcome-to-MeadowOS", "Meadow 設定"])
        arrange("Welcome-to-MeadowOS", 100, 95, 1060, 690)
        arrange("MeadowOS terminal", 760, 640, 940, 285)
        settings_xid = minimize("Meadow 設定")
        focus("MeadowOS terminal")
        capture("native-desktop.png")
        result["desktop_windows"] = windows()
        files_xid = minimize("Welcome-to-MeadowOS")
        terminal_xid = minimize("MeadowOS terminal")
        restore(settings_xid)
        arrange("Meadow 設定", 450, 120, 1020, 760)
        capture("native-settings.png")
        minimize("Meadow 設定")

        # Optional public screenshot of the existing empty GTK Chat. The only
        # action is its real accessible topbar button; no text or model request.
        try:
            subprocess.run(["/usr/local/bin/meadowos-run", "python3", "/usr/local/lib/meadowos/open_chat.py"],
                           env=env, check=True, timeout=12, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            wait_windows(["Meadow · AI 工作夥伴"], seconds=8)
            arrange("Meadow · AI 工作夥伴", 670, 110, 580, 780)
            capture("native-chat.png")
            minimize("Meadow · AI 工作夥伴")
            result["chat_capture"] = "existing GTK Chat; empty and unconfigured; no model request"
        except (RuntimeError, StopIteration, subprocess.SubprocessError) as error:
            result["chat_capture"] = "not_captured; bounded accessibility discovery unavailable"

        page = folder / "browser-smoke.html"
        page.write_text("""<!doctype html><html lang="en"><meta charset="utf-8">
<title>MeadowOS browser smoke</title><style>
body{margin:64px;background:#f3f6f2;color:#153b31;font:22px system-ui}
h1{font-size:52px;letter-spacing:-2px}p{max-width:650px;line-height:1.6}
</style><h1>MeadowOS browser smoke</h1><p>This local page is rendered by GNOME Web.
No model, remote page or API credentials are used for this check.</p></html>""")
        # These are real launch probes. Only the recorded namespace restriction
        # is an unsupported container capability; unknown failures still fail.
        capabilities = {}
        result["capabilities"] = capabilities
        capabilities["wails_desktop"] = probe_webkit("native-desktop",
            ["/opt/meadow/share/meadow-os/bin/meadow-session", "desktop"],
            "MeadowOS · 開發版", (370, 115, 1180, 780), "native-api-desktop.png", settle_seconds=4)
        current_agents = agent_pids()
        if current_agents != original_agents:
            raise RuntimeError(f"Managed Desktop changed the sole Agent: {original_agents} -> {current_agents}")
        browser_profile = output / "browser-profile"
        browser_profile.mkdir(mode=0o700)
        capabilities["browser"] = probe_webkit("browser",
            ["epiphany", "--profile=" + str(browser_profile), page.as_uri()],
            "MeadowOS browser smoke", (290, 110, 1280, 800), "native-browser.png")
        final_health = json.loads(subprocess.check_output(["/usr/local/bin/meadowos-healthcheck"], text=True))
        if not final_health.get("desktop_ready") or not final_health.get("agent_connected"):
            raise RuntimeError("Native core became unhealthy during WebKit capability probes")
        if agent_pids() != original_agents:
            raise RuntimeError("WebKit probes changed the sole native Agent")
        result.update(core_passed=True, capabilities=capabilities, agent_pids=current_agents,
                      status="core_passed; WebKit_capabilities_recorded_separately",
                      screenshots_require_visual_review=True)
    except Exception as error:
        result.update(core_passed=False, status="failed", error=str(error))
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
