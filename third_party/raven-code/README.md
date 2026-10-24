# Raven-Code source and attribution

This directory records the upstream source used for Meadow's native Raven-Code
execution profile. It is not a second model gateway, an installed Python Raven
Host, an EverOS service, or an Evolver deployment.

- Upstream: <https://github.com/EverMind-AI/Raven>
- Reviewed revision: `3632e6040c7038a60ec418ce39ccae185c72c19f`
- Native integration explanation:
  [execution architecture](../../docs/desktop/RAVEN_EXECUTION_ARCHITECTURE_20261006.md)
- `SOURCE_MANIFEST.json`: exact reviewed paths, SHA256, byte counts, pinned source
  URLs and the use of each source. A reference hash is provenance, not evidence
  that every referenced module was copied or implemented.

The upstream coding prompts carried under `go/core/agent/raven_prompts/` retain
their upstream bytes. Rendering the `{{DISCIPLINE}}` sentinel and adding the
Meadow tool-schema/permission adapter are separate native operations. Native Go
implementations adapt execution mechanisms to Meadow's Core, authorization,
runtime budgets, event stream and persistence; they do not vendor the Python
runtime. Their behavioral coverage is documented and tested separately.

`raven_prompts/CURATOR.md` is the exact 681-byte UTF-8 string constant returned by
`CuratorSegmentBuilder._system_prompt` in the pinned Python source, extracted by
AST reading without importing or running that source. Its extraction location
and separate asset hash are recorded under `extracted_assets` in the manifest.
This internal prompt belongs to the private context curation loop, which is
distinct from a transcript head-summary prompt and from Evolver's harness design.

`LICENSE`, `NOTICES.md` and `LICENSES/` are retained verbatim from the reviewed
upstream revision. Raven is Apache-2.0; the upstream notices include its nanobot
MIT ancestry and other components. Retaining these notices does not claim that
Meadow imports the listed UI, service or external runtime components.

The `completion_gates.py` source is recorded solely to prevent attribution
confusion: the reviewed Raven-Code product does not call that helper in its
production path. The CodeParticipant also does not run an independent critic.
Neither helper existence nor a four-interface wrapper establishes an ability
improvement; model quality must be measured separately with fixed controls.
