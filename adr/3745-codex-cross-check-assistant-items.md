# ADR-3745: Codex cross-check assistant item lifecycle

**Status**: Accepted (decided contract); implementation and verification pending
**Date**: 2026-11-01
**Issue**: [vtmocanu/uzi#3745](https://github.com/vtmocanu/uzi/issues/3745)
**Extends**: [ADR-2149](2149-cross-check.md) and [ADR-1106](1106-codex-harness.md).

## Context

The [maintainer's probe report](https://github.com/vtmocanu/uzi/issues/3745#issuecomment-6555238674)
records seven serial ephemeral synthetic code-review turns with `codex-cli 0.164.0`,
requesting `gpt-6.1-sol` at `high` effort, using the production native-disabled
configuration and Read/Search definitions. Turns 1–6 emitted a sole
`final_answer` assistant item. Turn 7 emitted distinct assistant items in this order:

1. Item A started with phase `commentary`, streamed, then completed.
2. Item B started with phase `final_answer`, streamed, then completed.
3. A successful terminal event matched the bound thread and turn.

No callbacks occurred. Retained evidence consists of ordering, method, identity,
type, phase and status metadata; no text, raw frames or secrets were retained.
The macOS probe version matches `CODEX_VERSION=0.164.0` in
[the package lock](../agent/codex/codex-package.lock) (line 21). This establishes
neither Linux artifact equality nor confinement, and does not establish the
cause of all historical incidents. One occurrence in seven probes is not a
failure-rate estimate.

At this decision, [CodexCrossCheck.run](../agent/src/codex/cross-check.ts)
uses turn-global assistant identity, streamed text and completion state. Its
line 466 guard rejects any assistant delta after the first assistant completion.
Thus A's completion followed by B's delta explains the rejection mechanism,
without requiring malformed verdict text or a tool callback.

## Decision

### Private per-item lifecycle

Both Plan and Code stages of `CodexCrossCheck.run` must use a private ledger
keyed by stable, nonempty assistant item identity. Track starts, deltas and
completions per item, with at most one incomplete assistant item. Explicit
starts are optional for legacy events; a delta or completion may establish an
item without a start. Reject duplicate starts, duplicate completions, overlapping
incomplete items, and a delta for the same item after its completion. A later
distinct item is permitted after a non-verdict item completes, subject to the
selection rules below.

Track whether any delta was received independently of its text length. If an
item streamed, its completed text must equal its accumulated stream, including
when the stream consists of empty deltas. Completion-only text remains valid
subject to the same identity, phase, size and terminal rules.

### Phase and verdict selection

An item's explicit phase may be learned from its start, completion or delta
when present. Missing or null metadata must not erase a known explicit phase.
Reject conflicting explicit phases, unknown explicit phases and nonstring
explicit phases. `commentary` and `partial_answer` are non-verdict phases;
their text must not enter verdict parsing.

Require exactly one completed `final_answer` candidate. Legacy absent/null
phase is permitted only for a sole assistant item. Reject multiple candidates,
a missing final answer, or another assistant item after final output. Do not
select by last-message-wins or parse commentary as a fallback.

Select the candidate only after a successful terminal event matching the bound
thread and turn, with no unfinished assistant item. Foreign thread/turn
assistant events or terminals, incomplete or failed terminals, and missing
terminals refuse the check. Then apply the existing strict stage validator to
the selected item's complete text: `validateVerdict` for Plan and
`validateCodeFindings` for Code. Item tracking does not relax either schema or
change either stage's semantics.

### Bounds and preserved safeguards

The existing stage text cap is an aggregate UTF-8 byte budget across assistant
items, including non-verdict and completion-only text. Charge deltas and
unstreamed completions; matching streamed/completed text is not charged twice.
The budget must not reset between items. The ledger also remains bounded by
the lifetime transport byte/frame caps.

Diagnostics remain static and bounded, without model text or raw frames.
Preserve confinement and fileop readiness, native-disabled configuration,
tool/callback budgets, deadlines, cancellation, authentication, cleanup and
usage accounting. This decision adds no retries or review passes and changes
neither Claude behavior nor workflow, publication or approval policy.

## Verification boundary

M1 records this contract before any behavior change. Implementation and
verification are pending M2; this ADR does not claim the behavior is built.
M2 must replay the exact rejection mechanism synthetically: completed streamed
commentary A, distinct streamed/completed final answer B, then a successful
bound terminal. Use synthetic text because the probe retained metadata only.
Record red-before/green-after regression evidence in M2, alongside coverage of
the lifecycle, phase, terminal and aggregate-budget refusal boundaries above.
No such regression has been executed as part of M1, and no live model call is
needed for this verification.
