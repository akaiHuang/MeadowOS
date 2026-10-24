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
  if "${task_compose[@]}" exec -T meadowos /usr/local/bin/meadowos-healthcheck > "$task_artifacts/restart-health.json" 2>/dev/null; then
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
echo "Fresh native desktop and restart smoke passed; evidence: $task_artifacts"
