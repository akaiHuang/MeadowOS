#!/usr/bin/env python3
"""Read-only native desktop health; model readiness is reported separately."""
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import urllib.request


def main():
    home = Path.home()
    state = home / ".local/state/meadow-os"
    receipt = json.loads((state / "lab/processes.json").read_text())
    for name in ("xvfb", "agent", "session", "vnc", "viewer"):
        pid = receipt["processes"][name]
        if not isinstance(pid, int) or pid <= 1:
            raise RuntimeError("Invalid child PID")
        os.kill(pid, 0)
    painted = json.loads((state / "shell-painted.json").read_text())
    if painted.get("painted") is not True or painted.get("display") != os.environ.get("MEADOW_DISPLAY", ":121"):
        raise RuntimeError("Native Shell has not painted this display")
    os.kill(painted["pid"], 0)
    if painted.get("geometry") != [0, 0, 1920, 1080]:
        raise RuntimeError("Native Shell geometry differs from 1920x1080")
    session = json.loads((state / "session.json").read_text())
    if session.get("state") != "shell_running":
        raise RuntimeError("Native session is not running")
    env = dict(os.environ, DISPLAY=os.environ.get("MEADOW_DISPLAY", ":121"))
    subprocess.run(["xprop", "-root", "_NET_SUPPORTING_WM_CHECK"], env=env,
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3)
    with urllib.request.urlopen("http://127.0.0.1:6080/vnc.html", timeout=3) as response:
        if response.status != 200:
            raise RuntimeError("noVNC is unavailable")
        response.read(1024)
    # Traverse the bridge proxy while keeping the same Host and Origin as the
    # user's browser. This proves the companion isn't stranded on loopback.
    request = urllib.request.Request("http://127.0.0.1:6081/voice/status", headers={
        "Host": "127.0.0.1:60531", "Origin": "http://127.0.0.1:60511", "X-Meadow-Voice": "1"})
    with urllib.request.urlopen(request, timeout=3) as response:
        companion = json.load(response)
    sys.path.insert(0, "/opt/meadow/share/meadow-os/shell")
    from agent_client import AgentClient
    client = AgentClient(timeout=3)
    try:
        status = client.connect()
    finally:
        client.close()
    if status.get("connected") is not True:
        raise RuntimeError("Native Agent did not authenticate")
    print(json.dumps({"desktop_ready": True, "agent_connected": status["connected"],
                      "model_ready": bool(status.get("model", {}).get("ready")),
                      "geometry": painted["geometry"], "speech_asr": companion.get("asr"),
                      "speech_tts": companion.get("tts"), "vm_boot_verified": False}))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, KeyError, subprocess.SubprocessError) as error:
        print(f"MeadowOS desktop is not ready: {error}", file=sys.stderr)
        raise SystemExit(1)
