---
name: workspace-repair
description: Locate and repair an existing workspace defect with focused file edits and targeted regression evidence.
---

# Workspace repair

Use this when unfamiliar files, stale edits or repeated checks are slowing a concrete repair. Choose actions from current evidence; no fixed sequence is required.

Locate the actual entrypoint with `glob`, `grep_search` and bounded `read_file`. Read applicable project instructions and the failing operation before changing source. Read the current advertised schemas rather than inventing shell-style parameters for these tools. Keep unrelated user changes intact. For definitions, references or types, use an available symbol/AST/LSP tool when it resolves the question; discover its schema first and fall back to current-file text search when unavailable. A retrieved example describes a reference, not the present workspace's file or symbol state.

An `edit` needs current exact source text. If it reports no match or an ambiguous match, inspect the relevant region, then supply a unique bounded change. Do not resend the same stale replacement or rewrite the entire file to evade the mismatch. A successful write/edit receipt confirms the mutation; reread only when another decision needs the contents.

Run the existing focused test or reproduction that can falsify the proposed fix. Choose its input, observable before/after state and expected result from the requested behavior before running it. Read expected/actual values, not just process exit status. If a check fails, distinguish a source bug from a fixture/environment problem and change only what the evidence supports. Preserve the original behavioral requirement and rerun the failed criterion after repair. Passing a different test does not resolve it. For saved HTML interaction or timing failures, the optional `browser-acceptance` skill covers causal inputs, page semantic operations and controlled state checks.

If `run_shell_command` returns a running process/session identifier, continue it through the advertised `process` tool rather than launching duplicate commands. A running process is not successful completion. Summarize the useful diagnostic; keep large raw logs in a readable file if needed.

Retain passing evidence for unchanged code and reuse already observed source until a change or missing detail requires a bounded reread. Each further repair attempt should test a changed hypothesis or newly observed fact; repeating the same edit, read or failed test without new information calls for another strategy or a concrete blocker. Broaden tests when changed dependencies or new failures justify it. Stop once the requested repair and proportionate checks are complete, or when a budget/capability limit prevents progress; report the unfinished criterion and useful evidence. Optional RAG examples may clarify an interface or supply reusable code; decide whether they fit, preserve the user's intended design and verify the saved result.
