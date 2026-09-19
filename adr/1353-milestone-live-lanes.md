# ADR-1353: Milestone live lanes — description-tag binding, liveness rule, server-only DTO

**Status**: Accepted
**Date**: 2026-09-19
**Issue**: [vtmocanu/uzi#1353](https://github.com/vtmocanu/uzi/issues/1353)
**PRD**: [prds/done/1353-milestone-live-lanes.md](../prds/done/1353-milestone-live-lanes.md) — carries the
full Decision Log (D1-D11), the milestone breakdown and the verified code anchors; this ADR
carries only the durable seams a future change must respect and why they are shaped this way.

## Decision (summary)

An in-progress milestone shows, beside its declared owner (`milestones_agents`, PRD #1224), the
set of subagents **currently working it** — `reviewer ×2`, `tester`, `auditor` at once — as
per-milestone **live lanes**. The binding that makes this possible rides correlation that
already exists (`agent_instance = parent_tool_use_id`, migration `00075`) plus one new
convention: the lead tags a subagent dispatch's `description` with a leading `[<id>]` milestone
id. The derived set (`RunDTO.milestones_live`) is server-authoritative and web-consumed, never
re-derived client-side, unlike `current_activity` (which the web does re-derive for its
zero-latency "now" line). No new frame field, no new column, no migration.

## Context

Before this feature, a milestone's only attribution was `milestones_agents` — the lead's
best-effort, one-name-per-milestone declaration through `report_progress`. Once the declared
owner handed off, other lanes (reviewer, auditor, tester) kept working the milestone invisibly,
and the strip kept pulsing green for an owner who had gone idle. Fixing the pulse (M1) needed no
new data; showing the actual crew (M2-M7) needed a way to tell which milestone a live subagent
frame belongs to, without adding a frame field, a `run_messages` column, or a migration (all
explicitly ruled out — a hot-path migration carries a merge-time numbering hazard the PRD's
scope forbids).

## The decisions

### D1 — The dispatch-`description` `[<id>]` tag is a seam other code must respect

A subagent's frames already carry `agent_instance = parent_tool_use_id` (migration `00075`),
equal to the `tool_use` id of the lead's own `Agent`/`Task` dispatch frame. The lead writes the
milestone id as a leading `[<id>]` token into that dispatch's `description` — a field it already
fills with the task label (`agent/src/prompt.ts`), so the tag rides an existing, uzi-authored
field with no SDK schema risk. The derivation (`api/internal/milestonelanes`) reads it back off
the persisted `Agent` dispatch frame (`ParseMilestoneTag`) and back-joins it to every live
subagent frame by `agent_instance == payload.id`. The parsed id is **membership-validated
against the run's in-progress milestone set** before it is trusted (`MilestoneTagBinding`, D7)
— a non-member or malformed tag drops the lane rather than rendering it.

Two alternatives were rejected, and the reasons are durable, not merely historical:

- **A structured key on the SDK-owned `Agent`/`Task` tool input.** uzi does not own that
  schema; a model-emitted extra key has no guarantee of surviving to the persisted frame. The
  `description` field is the one dispatch-input field uzi already authors and controls end to
  end.
- **Extending `milestones_agents`.** It has no per-instance key, so it can never distinguish
  `reviewer ×2` — a fallback only for the honesty half (M1), never the multi-lane goal.

Any future change to the dispatch shape, or to how `report_progress`/`milestones_agents`
correlates to a milestone, must preserve this tag convention or provide an equivalent
membership-validated binding — it is the only thing that makes a live frame attributable to a
specific in-progress milestone at all.

### D2 — The liveness rule is dispatch-completion primary, a freshness window backstop

A lane is dropped from `milestones_live` when **either**:

1. its dispatch has completed — a `tool_result` frame whose `tool_use_id == agent_instance`
   exists (the primary, precise signal: the subagent handed back and is no longer working), or
2. its newest frame is older than `laneFreshWindow` (10 minutes) — a backstop for a subagent
   that crashed or was killed without ever emitting a completion `tool_result`.

**This refines the PRD's D3, which is stated as "the same freshness window `RunActivity`
uses."** `RunActivity` (`api/internal/runactivity`) applies **no** staleness window at all — it
folds whichever `tool_use` frame has the greatest `Seq`, unconditionally. There was no existing
window to reuse, so `laneFreshWindow` is a new, purpose-built constant
(`api/internal/milestonelanes/derive.go`), and the completion signal — not a window — is the
mechanism that actually retires a finished lane promptly. The window exists only to catch the
lane a completion signal will never arrive for. A future change to `RunActivity`'s own staleness
behavior must not be assumed to also apply here; the two rules are independent by construction.

### D3 — `milestones_live` is server-authoritative and web-consumed; no client re-derivation

Unlike `current_activity`, which the web deliberately re-derives client-side from live
WebSocket frames for zero-latency updates (`web/src/lib/runActivity.ts`, a TS twin of
`api/internal/runactivity`, pinned against it by the `fixtures/run-activity` golden), the
two-part back-join `Derive` performs (newest live frame per instance, back-joined to its
dispatch frame, membership-validated, sanitized) is **not** mirrored in TypeScript. The CLI and
TUI fetch run messages trimmed by `?payload_max`, so a client-side re-derivation of the
dispatch back-join would be operating on an incomplete frame set regardless of surface. The web,
CLI and TUI all consume the one server-computed `RunDTO.milestones_live` field instead.

Parity is held by two mechanisms, not a Go/TS cross-language fixture: the DTO-shape
api-contract compile-time check (`api/internal/apitypes/contract_test.go` plus the
`fixtures/api-contract/run.{zero,full}.json` recordings) proves the wire shape agrees between
Go and TS, and `fixtures/milestone-live-lanes/cases.json` — asserted from
`api/internal/milestonelanes/derive_test.go` alone — pins the derivation's own behavior. A
future change to `Derive`'s selection or fold rule does not need a TS-side twin to update, and
adding one would be redundant with the field it is deriving being server-only in the first
place.

### D4 — The tag leaks transiently into the `current_activity` now-line; accepted

`runactivity.FromFrame` folds a dispatch `tool_use` frame (`payload.name == "Agent"`) by setting
`AgentLabel` and `Detail` to `input.description` **verbatim** — tag and all — because that fold
predates this feature and has no reason to know about the `[<id>]` convention. So while a
tagged dispatch frame is the newest frame on a run (before the subagent it names has written
anything), the global now-line's label/detail can read `[c2] Review placement/assembly` rather
than the stripped `Review placement/assembly`. This is:

- **Transient** — it clears the moment the subagent's own first frame supersedes the dispatch
  frame as the newest one.
- **Terminal-safe** — `FromFrame` still runs the value through `sanitize` (strip-and-cap), so
  the leak is cosmetic, never a rendering hazard.
- **One-directional** — the milestone lane display (`milestonelanes.Derive`) strips the tag via
  `MilestoneTagBinding`'s `label` return before it reaches `RunDTO.milestones_live`; only the
  *now-line*, which reads the dispatch frame through the unrelated, unmodified `RunActivity`
  path, shows the raw tag.

Accepted rather than fixed: teaching `RunActivity` about the milestone-tag convention would
couple two independently-owned derivations for a cosmetic, self-clearing artifact.

## Consequences

- A future change to the `Agent`/`Task` dispatch shape must preserve (or knowingly replace) the
  `description`-leading `[<id>]` convention — it is the only binding between a live subagent
  frame and the milestone it is working.
- A future staleness-window change to `RunActivity` does not, and must not be assumed to,
  propagate to `laneFreshWindow` — they are separate constants solving separate problems (D2).
- Adding a client-side TS twin of `milestonelanes.Derive` would be redundant scope: the field is
  server-only by design (D3), and doing so would just be new code to keep in sync with nothing
  gained.
- The `[<id>]` tag's transient appearance in the now-line's label/detail (D4) is a known,
  accepted cosmetic artifact, not a defect to chase; a fix for it is unwarranted unless the
  now-line's own scope changes to need one.
