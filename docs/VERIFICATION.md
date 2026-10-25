# Development and tests

MeadowOS uses Go for the Agent and Code Desktop, Python/GTK for the Shell, and
native helpers for window and file UI. Start with the [architecture](ARCHITECTURE.md)
for a component map.

## Continuous integration

[GitHub Actions](https://github.com/akaiHuang/MeadowOS/actions) builds and tests
Linux amd64 and arm64. Checks cover desktop startup, Agent connectivity,
restart recovery, file persistence, API setup and conversation behavior.
WebKit app availability is reported separately because it depends on host sandbox support.

## Run offline checks

From the project root:

```sh
PYTHONPATH=scripts/meadowos:docker python3 -m unittest \
  scripts/meadowos/test_foundation.py \
  scripts/meadowos/test_archive_provenance.py \
  docker/test_companion_proxy.py docker/test_smoke_capabilities.py \
  docker/test_container_lifecycle.py \
  scripts/meadowos/test_shell_agent_client.py \
  scripts/meadowos/test_configure_inception.py \
  scripts/meadowos/test_companion_new_chat.py
```

On Linux, run the native Agent integration tests from `go/`:

```sh
CGO_ENABLED=0 go test -tags meadow_vm_lab ./core/runtimecontrol ./core/agentdaemon
```

These tests use a model fixture and do not send paid inference requests.

## Test the desktop

With Docker running and ports 60511 and 60531 available:

```sh
bash docker/smoke.sh
```

The smoke test builds a separate desktop instance, opens native windows,
checks restart recovery and captures screenshots. It removes its test instance
when finished. Results are written to `artifacts/meadowos-smoke/`.

Changes to desktop presentation should include a screenshot. For bug reports,
include your platform, reproduction steps and relevant logs, with personal
information removed.
