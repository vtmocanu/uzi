# PRD #1353: Per-milestone live lanes

**Issue**: #1353
**Priority**: Medium
**Status**: Planned
**Builds on**: PRD #1224 (per-milestone declared attribution), PRD #1064 (the `RunActivity` "now" line), PRD #99 + migration `00075` (subagent invocation id: `agent_instance = parent_tool_use_id`).
**Anchors**: refreshed 2026-09-19 against `main` `788514f7`. Line numbers drift; re-grep the named symbol rather than trusting the number.

## Problem

A milestone-structured run shows, per in-progress milestone, the agent the lead **dispatched to implement** it. That attribution is the lead's best-effort declaration through `report_progress`'s optional `milestones_agents` argument (`agent/src/prompt.ts:1456-1460`; parsed in `agent/src/signals.ts`; persisted by `milestoneAgentsParam` in `api/internal/workersvc/milestones.go:225`). It names one agent per milestone — the implementer, e.g. `coder`.

Once that implementer finishes and hands off, the milestone stays in progress while **other lanes work it**: `reviewer`, `auditor`, `tester`, `documenter`, and often several at once (an observed run showed `reviewer ×2`, `tester`, `auditor` all working while `coder` was idle). Two failures follow:

1. **The idle owner reads as active.** Every in-progress strip renders the live-green **pulsing** dot regardless of whether the owner is doing anything (`MilestoneNowStrip` always emits `animate-pulse` — `web/src/pages/RunView.tsx:436`). A strip with no live frame still pulses green, so an idle `coder` looks like it is working.
2. **The agents actually working are invisible on the milestone.** The live "now" signal is a single newest-tool-use frame (`RunActivity`, `api/internal/apitypes/run.go:61`; server-derived by the `LatestToolUseForRuns` query, `DISTINCT ON (run_id)` newest `tool_use`, in `api/internal/store/queries/runtime.sql:589`, folded by `runactivity.Latest`), attached to a milestone strip **only when its agent name byte-matches exactly one declared milestone agent** (the D3 unique-match join: `RunView.tsx:494`; CLI/TUI twin `uniqueMilestoneAgentMatch` in `api/cmd/uzi/tui_detail_rail.go:424`; web twin `uniqueLiveMatchMilestoneId` in `web/src/lib/runBadge.ts:787`). A `reviewer` matches no declared owner, so its activity attaches to nothing. Worse, on the CLI/TUI the per-milestone rows **replace** the global now-line when attribution is present (`milestoneNowRows` returns non-nil, suppressing the fallback `nowRow` at the `run_render.go:172-174` branch; `milestoneNowRows` itself is `api/cmd/uzi/run_render.go:664-709`), so the live reviewer disappears entirely from `uzi run get`.

The effect is identical across the three parity-tested surfaces (web `RunView`, CLI `run_render.go`, TUI `tui_detail_rail.go`), because they share the same `effectiveMilestoneAgents` + unique-match logic, and that agreement is itself pinned by a Go/TS cross-language join fixture (see Testing).

## Goal

Two independent wins, in this order:

1. **Honesty (no new data):** reserve the live styling (the pulsing dot / the `NOW` framing) for a strip that has a real live frame; a declared owner with no live activity renders as a quiet, non-pulsing owner line. This is the "After A" fix and needs none of the derivation below.
2. **Live lanes (new data):** show, per in-progress milestone, **every agent currently working it** — role, task label, tool, age — any role, several at once, additively beside the declared owner.

### Non-goals

- **The board (`/runs`, Dashboard) is out of scope.** `RunsList.tsx` / `Dashboard.tsx` render only the single `current_activity` now-line (`RunsList.tsx:281,351`), which already shows the real live agent honestly; the compact card intentionally shows one lane. B does not add per-milestone lanes there (D9).
- Not changing what counts as a milestone, the freeze mechanism, or completion reporting.
- Not verifying or judging the work — this is visibility only.

## User impact

On the run view (and `uzi run get`, and the TUI crew rail), an in-progress milestone shows its declared owner as a quiet, non-pulsing line, plus one green line per agent currently on it. The owner line no longer implies the owner is active. When a reviewer, auditor and tester are all reviewing a milestone, the user sees all three with their live tool and age, distinguishing two reviewers on the same milestone.

## Technical scope

Two data flows, kept separate:

- **Declared owner** (exists): `milestones_agents` — one `MilestoneAgent{id, agent, agent_label}` per milestone (`api/internal/apitypes/run.go:30`). Keep as-is; it becomes the "owner" line.
- **Live lanes** (new): the set of currently-active subagent lanes on each in-progress milestone, **server-derived and web-consumed** (authoritative on the server like `MilestonesAgents`, never re-derived on the client — the CLI/TUI fetch run messages trimmed via `?payload_max`, so a client re-derivation of the dispatch back-join would be fragile). Each lane carries `{agent, agent_instance, agent_label, tool, detail, at}`.

**The milestone→lane binding uses correlation that already exists — no new frame field, no new column, no migration (D1):**
- A subagent's frames already carry `agent_instance = parent_tool_use_id` (migration `00075`), which equals the `tool_use` id of the lead's dispatch. The dispatch itself is persisted as a `tool_use` frame `{id, name:"Agent", input:{subagent_type, description}}`.
- The only missing link is dispatch→milestone. The lead **writes the milestone id into the dispatch `description`** (an existing, uzi-authored, trim-preserved field it already fills with the task label) as a leading convention token, e.g. `[c2] Review placement/assembly`. Because the description is content the lead controls (not a new key on the SDK-owned `Agent` input schema), it survives into the persisted frame with no schema risk.
- The derivation is therefore a **two-part read**, not the single-frame `RunActivity` shape: (1) the newest in-window frame per live `agent_instance`, back-joined to (2) that instance's original `Agent` dispatch frame (by `payload.id == agent_instance`) to recover its milestone tag and label. Group by `(milestone_id, agent_instance)`.

Surfaces to update in lockstep (parity is the primary risk): web `RunView.tsx` (`MilestoneChecklist` / `MilestoneNowStrip`), CLI `api/cmd/uzi/run_render.go` (`milestoneNowRows`), TUI `api/cmd/uzi/tui_detail_rail.go`. **The CLI and TUI are the same Go package (`api/cmd/uzi`, `package main`) and share the join helpers (`effectiveMilestoneAgents`/`uniqueMilestoneAgentMatch` live in `tui_detail_rail.go`, consumed by `run_render.go`); any new shared live-lane helper goes in a neutral file or the derivation package, and the two renderers are implemented serially, not as concurrent subagents.** Only web is a separately parallelizable module.

**Internet independence (uzi-offline safe):** every fact above was resolved by reading this repo; the feature touches only uzi's own code (web, api, agent, tests). No open-web lookup, no external API, and **no `.github/workflows/**` files** in implementation or validation (the worker PAT lacks `workflow` scope; `.claude/rules/prds.md`). **No goose migration — the binding rides existing fields; a new `run_messages` column is explicitly forbidden** (it would be a hot-path migration with a merge-time numbering hazard).

## Decision Log

- **D1 (binding): a subagent frame is mapped to a milestone via existing correlation, not a new field.** The lead tags the milestone id into the dispatch `description`; the derivation reads it off the `Agent` dispatch frame and correlates live frames by `agent_instance = parent_tool_use_id` (migration `00075`). Rejected alternatives: (a) adding a structured key to the SDK builtin `Agent` tool input — uzi does not own that schema, so a model-emitted extra key may not survive to the persisted frame; (b) extending `milestones_agents` re-declaration — it has no instance key and the lead cannot supply one, so it cannot tell `reviewer ×2` apart (D4), making it a fallback for the **honesty half only**, never the multi-lane goal; (c) temporal "whatever was in progress when the frame fired" — ambiguous because several milestones are in progress at once. An ADR (numbered by this issue) records the description-tag convention as a seam other code must respect.
- **D2 (derivation shape): two-part query, server-authoritative.** Newest in-window frame per live `agent_instance`, back-joined to its dispatch frame by `payload.id`. Not `runactivity.Latest` (single `DISTINCT ON (run_id)` frame that does not even select `agent_instance`). A new exported derivation function; the `RunActivity` package is the structural precedent, not the same query.
- **D3 (recency): a lane counts as live** when its most-recent frame is within the same freshness window `RunActivity` uses. An owner with no live frame is the (non-pulsing) owner line, never a live lane.
- **D4 (dedup): distinguish repeated roles by `agent_instance`, never by name.** `reviewer ×2` is two lanes.
- **D5 (additive, back-compat): a pre-feature run, a run with no live lanes, and a terminal run render exactly as today** (the #1224 / #1064 D8 contract). Branch on "live lanes present" (`len(...) > 0`), never on a nil test.
- **D6 (owner + lanes coexist):** the declared owner line stays (quiet, non-pulsing); live lanes render below it. On the CLI/TUI, `NOW`/live styling is reserved for a live lane and the global now-line is no longer wholesale-suppressed by the mere presence of attribution.
- **D7 (untrusted text, parse AND render):** the milestone tag parsed out of the description is **membership-validated server-side against the run's frozen + in-progress milestone set** (a non-member tag is dropped, never rendered), mirroring `progressParams`. Separately, `agent`, `agent_label`, `tool`, `detail` are model-authored: every renderer folds them through its terminal-safety path (`cellText` on Go, `stripUnsafeChars` on web), and the TUI registers the new lane fields in `d7UntrustedFields` (`api/cmd/uzi/tui_d7_guard_test.go`; `.claude/rules/tui.md`) with the hostile-value render test extended.
- **D8 (DTO shape): `milestones_live`** — a list of `{milestone_id, lanes: [{agent, agent_instance, agent_label, tool, detail, at}]}` on `RunDTO`, server-authoritative, web-consumed. The DTO change is the three-file edit (`.claude/rules/web.md`): the Go struct in `apitypes`, `fixtures/api-contract/run.{zero,full}.json` (re-recorded from the failing contract test, never hand-authored), and `web/src/lib/apiTypes.ts`. The message-frame payload is unchanged (the tag lives inside the existing `description` text), so there is no `run_messages` contract change.
- **D9 (board unchanged)** — restated so a future change does not silently add lanes to the compact card.

## Milestones

M1 is the guaranteed floor and depends on nothing below it; the live-lane work (M2–M7) builds on top and can stall on M2's binding without losing M1's value.

- [ ] **M1 — Honesty fix (D1-independent), all three renderers.** Reserve the pulse / `NOW` / live styling for a strip that has a live frame; demote a declared owner with no live frame to a quiet, non-pulsing line (web `RunView.tsx:436`; CLI `run_render.go` owner-as-tag; TUI `tui_detail_rail.go`), and stop the CLI/TUI from wholesale-suppressing the global now-line (D6). Uses only today's `current_activity` + unique-match — no derivation, no agent changes. Regression pins for **both** defects: the idle-owner pulse (web) and the suppressed global now-row (`uzi run get`, via `countNowRows` / `TestRenderRunDetailUnattributedExactlyOneNowRow`), each failing on current code and passing after. Parity goldens extended in lockstep.
- [ ] **M2 — Milestone→lane binding (D1).** The lead writes the milestone id into the subagent dispatch `description` (agent-side guidance in `agent/src/prompt.ts`; parse in `agent/src/signals.ts` if needed), and the server membership-validates the parsed tag against the frozen + in-progress set (D7). No new column, no migration. Unit tests: valid tag, non-member tag dropped, untagged dispatch (back-compat), malformed description.
- [ ] **M3 — Server derivation + DTO.** The two-part derivation (D2) producing per-milestone live lanes, exposed as `milestones_live` (D8), with the three-file api-contract edit re-recorded and a **new Go/TS cross-language fixture** (`fixtures/milestone-live-lanes/cases.json`, asserted from the derivation's Go test and a TS twin, like `fixtures/run-activity`). Derivation tests: multiple lanes on one milestone, `reviewer ×2` by instance, idle-owner-only (owner shown, no lane), an **out-of-window dispatch frame** (instance live but its dispatch frame is old — the back-join must still resolve), terminal run empty, no-milestones back-compat. `-count=1` applies (cross-module fixture; `.claude/rules/go.md`).
- [ ] **M4 — Web RunView render** (parallelizable). Consume `milestones_live`: owner line (from M1) plus a green live line per lane. Tests: multi-lane, repeated role, back-compat unchanged, sanitization via `stripUnsafeChars` on `agent`/`label`/`tool`.
- [ ] **M5 — CLI `uzi run get` render** (Go package, serialized with M6). Per-milestone lanes in `milestoneNowRows`; owner demoted; global now-line intact. Parity golden `run_render_attribution_test.go` extended.
- [ ] **M6 — TUI crew-rail render** (Go package, serialized with M5). Lanes stacked under the in-progress milestone; register the new lane fields in `d7UntrustedFields` and extend the hostile-value test (D7). Parity goldens `tui_milestone_attribution_test.go` + `milestone_attribution_fixture_test.go` extended.
- [ ] **M7 — Cross-surface parity + contract green.** The Go/TS **join** fixture is extended on BOTH halves together: `api/cmd/uzi/milestone_attribution_fixture_test.go` (Go) **and** `web/src/lib/milestoneAttribution.fixture.test.ts` (TS), sharing `fixtures/milestone-attribution/cases.json`; likewise the new `fixtures/milestone-live-lanes` fixture. `task gate` green across api, web, agent, controller; DTO contract tests pass together.
- [ ] **M8 — Docs + ADR.** ADR for the description-tag binding; update `ARCHITECTURE.md` where the run-view surfaces are described and this PRD's Decision Log; `task docs:sync` if a `docs/*.md` changed.

### Milestone dependency graph (for the implementing lead)

| Phase | Milestones | Depends on | Notes |
|---|---|---|---|
| 1 (floor) | M1 | — | Ships the honesty win alone; no D1, no derivation, no agent change. |
| 2 (foundation, sequential) | M2 → M3 | M1 | Binding, then the two-part derivation + DTO + new fixture. |
| 3 (renderers) | M4 ‖ (M5 → M6) | M3 | **M4 (web) is parallel; M5 and M6 are one Go package sharing helpers + the cross-surface golden — do them serially, not as concurrent subagents.** |
| 4 (gate) | M7 → M8 | M4–M6 | Reconcile both halves of both cross-language fixtures; docs + ADR. |

## Testing strategy

- **Honesty (M1):** two regression pins, each failing on current code — the idle-owner pulse (web) and the wholesale-suppressed global now-row (`uzi run get`) — pinned to their specific defect (the repo's regression rule), not the area.
- **Derivation (M3):** table-driven Go tests over `run_messages`, including the out-of-window dispatch back-join and `reviewer ×2` by `agent_instance`; the new `fixtures/milestone-live-lanes` Go/TS fixture is the single pin both languages assert from.
- **Renderers (M4–M6):** each surface asserts owner-only vs owner+lanes, repeated-role rendering, sanitization of untrusted `agent`/`label`/`tool`, and a back-compat snapshot (byte-identical for the Go goldens; presence/absence for the web render test).
- **Parity (M7):** the cross-language **join** fixture is Go + TS + shared JSON — extend all three, not the Go half alone; `RunView.attribution.test.tsx` is a per-surface render test, not the parity golden. Run the whole `go test ./...` with `-count=1` (cross-module fixtures; `.claude/rules/go.md`).
- **D7:** parse-time membership validation test (non-member tag dropped server-side) plus the TUI `d7UntrustedFields` hostile-value test.

## Risks and mitigations

- **Parity drift** across web/CLI/TUI and across the Go/TS halves of the join fixture — the top risk. Mitigation: extend both halves of both cross-language fixtures together; M7 reconciles; M5/M6 kept in one serialized Go-package step.
- **Binding reliability.** The description-tag rides an existing uzi-authored field, so there is no SDK-schema risk; the residual risk is the lead not always tagging. That degrades gracefully to the M1 honesty baseline + name-match join (owner shown, lane absent), never to a wrong lane, because the tag is membership-validated (D7).
- **A naive lead adding a `run_messages` column** for the binding — explicitly forbidden (D1/scope): hot-path migration + merge-time numbering hazard.
- **`agent_instance` gaps.** It is reliable for subagent frames (= `parent_tool_use_id`, migration `00075`) but nil for orchestrator/infra frames (`actorCell`, `run_render.go`); lanes derive only from subagent frames, and the success criterion is scoped accordingly.
- **DTO contract staleness** — enforced by `gate:api` / `gate:web`; re-record `run.{zero,full}.json` from the failing contract test, never by hand.

## Dependencies

PRD #1224 (declared attribution, shipped), PRD #1064 (`RunActivity`), PRD #99 + migration `00075` (`agent_instance`). All merged; no external dependency.

## Success criteria

- On a run where non-implementer agents are working an in-progress milestone, the run view / `uzi run get` / TUI show those agents as live lanes on that milestone, with tool and age, distinguishing repeated roles by `agent_instance`.
- An idle declared owner no longer renders with the live pulsing dot (shippable from M1 alone).
- Pre-feature, no-lane, and terminal runs render byte-identically on the Go goldens; the web render test shows unchanged presence/absence.
- `task gate` green across all components; both halves of both cross-language fixtures and the DTO contract tests pass together.
