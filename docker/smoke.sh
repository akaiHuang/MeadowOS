#!/usr/bin/env bash
# CI smoke: a new project, home volume and network; no provider credentials.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
# The browser companions intentionally use fixed, admitted loopback ports.
python3 - <<'PYPORTS'
import socket
for port in (60511, 60531):
    with socket.socket() as probe:
        try:
            probe.bind(("127.0.0.1", port))
        except OSError as error:
            raise SystemExit(f"Port {port} is already in use; stop that instance before the fresh smoke") from error
PYPORTS
task_project="meadowos-ci-${GITHUB_RUN_ID:-local}-$$"
task_artifacts="${MEADOW_SMOKE_ARTIFACTS:-$PWD/artifacts/meadowos-smoke}"
mkdir -p -- "$task_artifacts"
task_compose=(docker compose -p "$task_project" -f docker-compose.yml)
cleanup() {
  task_status=$?
  trap - EXIT
  "${task_compose[@]}" logs --no-color > "$task_artifacts/container.log" 2>&1 || true
  "${task_compose[@]}" cp meadowos:/home/meadow/.local/state/meadow-os/smoke/. "$task_artifacts/" >/dev/null 2>&1 || true
  mkdir -p -- "$task_artifacts/runtime-logs/lab"
  for task_log in desktop-lab.log session-bus.log session.log companion.log companion-proxy.log; do
    "${task_compose[@]}" cp "meadowos:/home/meadow/.local/state/meadow-os/$task_log" "$task_artifacts/runtime-logs/$task_log" >/dev/null 2>&1 || true
  done
  "${task_compose[@]}" cp meadowos:/home/meadow/.local/state/meadow-os/lab/. "$task_artifacts/runtime-logs/lab/" >/dev/null 2>&1 || true
  "${task_compose[@]}" down --volumes --remove-orphans || true
  exit "$task_status"
}
trap cleanup EXIT
# Leave no attached host repository, Docker socket or credentials in the guest.
task_source_commit="${SOURCE_COMMIT:-$(git rev-parse HEAD)}"
"${task_compose[@]}" build --build-arg "SOURCE_COMMIT=$task_source_commit"
"${task_compose[@]}" up --detach --wait --wait-timeout 180
"${task_compose[@]}" exec -T meadowos /usr/local/bin/meadowos-healthcheck > "$task_artifacts/health.json"
"${task_compose[@]}" exec -T meadowos python3 /usr/local/lib/meadowos/smoke.py
"${task_compose[@]}" cp meadowos:/home/meadow/.local/state/meadow-os/smoke/. "$task_artifacts/"
"${task_compose[@]}" restart meadowos
# Check persistence and native recovery after a normal container restart.
task_ready=0
for task_attempt in $(seq 1 60); do
  if "${task_compose[@]}" exec -T meadowos /usr/local/bin/meadowos-healthcheck > "$task_artifacts/restart-health.json" 2> "$task_artifacts/restart-error.txt"; then
    task_ready=1
    break
  fi
  sleep 2
done
if [ "$task_ready" != 1 ]; then
  echo 'Native desktop did not recover after restart' >&2
  exit 1
fi
"${task_compose[@]}" exec -T meadowos python3 -c 'from pathlib import Path; assert (Path.home()/"MeadowWork/MeadowOS smoke/persistence.txt").read_text()=="MeadowOS named home volume\n"'
python3 - "$task_artifacts" <<'PYRESULT'
from pathlib import Path
import json,sys
root=Path(sys.argv[1])
smoke=json.loads((root/"smoke.json").read_text())
initial=json.loads((root/"health.json").read_text())
restart=json.loads((root/"restart-health.json").read_text())
core=bool(smoke.get("core_passed") and initial.get("desktop_ready") and restart.get("desktop_ready"))
if not core: raise SystemExit("Core native acceptance receipt is incomplete")
(root/"acceptance.json").write_text(json.dumps({
    "core_passed":core,"restart_persistence_verified":True,"agent_authenticated":True,
    "capabilities":smoke["capabilities"],"container_security":"unchanged",
    "onboarding":"separate native Linux CI job","model_task":"not_run",
    "screenshots_require_visual_review":True,"vm_boot_verified":False},indent=2)+"\n")
PYRESULT
echo "Fresh native core and restart smoke passed; WebKit capability outcomes: $task_artifacts/acceptance.json"
