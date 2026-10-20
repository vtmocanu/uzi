# PRD #3166: Observable cross-check activity

**Status**: Draft. Child 1 of 3 under umbrella #3165 (Deeper, observable cross-check). No blockers.

Resolved facts below were read at `main` `bf21acdc`.

## Problem

An owner or reviewing session looking at a Plan or Code cross-check cannot tell what the checker looked at. A clean result after 24 seconds may be an adequate review of a small diff or a skim; nothing distinguishes them. The checkers observe their own tool calls but persist none of them:

- The Claude checker consumes tool frames for confinement and, on the code stage only, its `CodeToolBudget`; it posts only the `init` and `result` usage frames (`agent/src/claude-cross-check.ts`, `ClaudeCrossCheck.run`).
- The Codex checker services broker callbacks without emitting activity (`agent/src/codex/cross-check.ts`); `init` and `result` are posted separately.
- The runner persists only what the adapters emit, through `postUsage` and `client.postMessages` (`agent/src/cross-check-runner.ts`), bypassing the batcher's sanitize and redact path (`agent/src/batcher.ts`).

So a checker run's feed with no tool rows proves nothing either way: it neither shows that files were read nor that they were not.

## Outcome

Every Plan and Code checker run, in both families, records a bounded, sanitized account of what it read and searched, with an explicit start marker and final summary. The checker run page and `uzi run logs <checker>` show it; `uzi run get <checker>` shows only the summary. A run from an older worker shows "activity capture unavailable", never "read nothing".

Acceptance examples:

1. A Codex code checker reads three files and one read fails with the helper's oversize code. The checker run page lists the three paths with line ranges and outcome `ok`, the failed path with outcome `oversize`, and a summary "4 reads, 0 searches, 0 dropped, complete".
2. A Claude plan checker runs a search confined to `api/internal/forge`. The feed records the scope path, match count and outcome, and never the query or pattern.
3. A checker attempts a read outside its checkout. The feed records one `denied` event in the "outside checkout" category, without the attempted path.
4. A worker on the previous version runs a check. New web and CLI show "activity capture unavailable (older worker)"; a marker without a summary shows "activity capture incomplete". Old web and CLI tolerate the new events without breaking (they render an unknown status event generically).

## Out of scope

- Any change to what the checker reviews, its brief, its verdict schema or approval rules (PRDs #3167 and #3168).
- Large-file ranged reads (#3073) and Codex search limits.
- Lead run health: activity lands on the checker child run, so the lead's stalled-health warning during a code-check wait stays with #3012.
- Persisting hidden reasoning, raw tool arguments, file contents or full transcripts.

## Modules and seams

- **Activity event family.** A new `status` event family distinct from `tool_use` / `tool_result` (read by run-health loop detection, `api/internal/workersvc/health.go`, and the interrupted-calls advisory, `interrupted_calls.go`) and from `event: "init"` / `"result"` (read by usage accounting, `web/src/lib/runUsage.ts`). Closed, harness-neutral outcome set: `ok`, `denied`, `oversize`, `limit`, `error`, mapped per family (Codex helper codes; Claude `tool_result.is_error` plus guard denies).
  - Read event: checkout-relative path (control and bidi characters stripped, byte-bounded) and line range.
  - Search event: scope path, match count and outcome; never the query or pattern.
  - Out-of-checkout or guard-denied attempts: a category, never the path.
  - A start marker when capture begins and a final summary (counts per kind and outcome, dropped count, `complete` flag). No marker means capture unavailable; a marker without a summary means capture incomplete.
- **Delivery.** Through the worker's existing redaction and sanitization path, not the raw `client.postMessages` call. Preserve the run-message stream's per-run sequence, generation fencing, ownership checks and the existing terminal delivery barrier (`messagesThroughSeq`, `agent/src/terminal-resolve.ts`).
- **Resource bounds** (checker output is model-driven, so untrusted in volume):
  - per-check event cap N (Claude plan checks have no tool budget, only max turns), with excess counted in `dropped`;
  - per-event byte cap for paths;
  - bounded observability overhead: activity is optional telemetry, so no verdict failure, sequence gap or terminal-state failure may be caused solely by it. The PRD does not prescribe a queue implementation.
- **Surfaces.** Web checker run page renders events as plain text through the existing hardened renderer. `uzi run logs <checker>` prints them through `renderer.Plain`. `uzi run get <checker>` prints the summary only. Old web and CLI render an unknown status event as `status: <event>` (`RunEvent.tsx`), so a worker that rolls out first is harmless.

## Testing decisions

- Agent unit tests per family: emitted events for ok, denied, oversize and error reads; search events carry no query; cap reached produces `dropped` and a final summary; path sanitization (control, bidi, overlong).
- A test that telemetry failure (post rejected) does not fail the verdict, adds only bounded overhead, and leaves no sequence gap or broken terminal delivery.
- API/store: the new event family does not trip health loop detection or the interrupted-calls advisory (regression tests on those readers).
- Web and CLI render tests for complete, incomplete and unavailable capture; follow the existing `CodeCrossCheck.tsx` and `run_get.go` tests.

## Milestones

- [ ] **M1: Every checker run records and shows its activity.** Both families, both stages: event family, delivery through redaction, caps, start marker and summary, web, `uzi run logs`, `uzi run get` summary, docs (`docs/cross-check.md`, `docs/cli.md`, then `task docs:sync`), changelog fragment. Blocked by: none. Acceptance: examples 1-4 above; health and interrupted-calls regression tests pass; gates `task gate:agent`, `task gate:api`, `task gate:web`.

## Decision Log

- **Reuse the run-message stream, not a new store.** It already gives sequencing, generation fencing, ownership, retention, replay and the checker-run link. Rejected: a dedicated activity table (more moving parts for the same guarantees).
- **New status event family, not tool_use/tool_result.** Those kinds feed health loop detection and the interrupted-calls advisory; checker reads would produce false looping or interruption signals.
- **Never record search queries or tool arguments.** They can carry repository content or secrets; scope, count and outcome are enough to judge breadth.
- **Telemetry is best-effort.** Observability must not reduce the reliability of the check it observes.
