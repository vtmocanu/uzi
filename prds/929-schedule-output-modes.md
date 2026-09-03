# PRD #929: Schedule output modes: MR idea files vs forge issues for proposal jobs

> Anchors re-verified against main at fcfd8aa on 2026-09-03, after the epic #915 file splits.

**GitHub Issue**: [#929](https://github.com/vtmocanu/uzi/issues/929)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Related**:
- `api/internal/schedtmpl/catalog/*.md` — the shipped default-jobs catalog: frontmatter (`slug`, `name`, `description`, `target`, `cron`, `timezone`, `model`) + body (the baked prompt). `feature-bingo.md` is the reference proposal-shaped job: its prompt writes one idea file under `ideas/` and opens an MR, with an explicit no-op path ("make no change and open no merge request").
- `api/internal/schedsvc/scheduler.go:361` and `:525` — fire-time resolution by `catalog_slug` (prompt for prompt targets, labels+guidance for sweeps). A shipped catalog change reaches every enabled default; this PRD's per-schedule mode setting composes at the same point.
- `api/internal/forge/forge.go:469-472` — `CreateIssue(ctx, projectID, title, description, labels)`: the existing driver primitive issues-mode filing uses.
- Precedent for server-side filing with a marker label: incidental findings (`uzi findings file`) and judge-recommendation filing both assemble a server-templated issue on the user's forge connection with a mandatory marker label. The AGENT has no forge-write tools by design; the api does the writing.
- Issue [#928](https://github.com/vtmocanu/uzi/issues/928) (refactor-scout) is mode-agnostic; it landed (PR #962, commit `24b9447`, `api/internal/schedtmpl/catalog/refactor-scout.md`) and adopts this feature once M4 ships it. Epic #915 is where dedup-by-disposition was established.

## Problem

Proposal-shaped scheduled jobs — feature-bingo and refactor-scout (#928), both shipped — have exactly one delivery mechanism: write an idea file, open an MR. That has three costs:

1. **Triage friction.** An idea worth doing gets read in an MR, then someone manually files an issue anyway. Issues are the native triage surface: promoting a proposal to real work is just adding labels (the existing promote flow).
2. **The disposition STORE is ad-hoc.** File-mode dedup means the prompt tells the worker to read a folder; declined-ness lives in ad-hoc file sections that only work if declined MRs are merged. Forge issues carry disposition natively: open vs closed, plus the closing comment for the reason. (Note the neutral `forge.Issue` has NO close-reason field — `State` + `Title` + `Labels` only — and close-reason is not uniformly available across forges; the decline *reason*, where wanted, comes from `ListIssueComments`.) Be clear what improves: the store. Dedup ENFORCEMENT stays soft in both modes — the digest instructs the agent; nothing mechanically blocks a duplicate `CreateIssue`.
3. **Git noise.** Proposals are meta-content; each one is a commit + MR in history.

File-mode remains right where the output *is* content (docs-hygiene's output is real doc fixes). So the mechanism should be a per-schedule choice, not a migration.

## Solution

A per-schedule **output mode**, `mr` (default, today's behavior) or `issues`, honored for **prompt-target** schedules:

1. **Setting**: a new nullable column on `run_schedules` (`output_mode`, `mr`/`issues`, NULL = catalog/job default), a matching optional `output:` key in catalog frontmatter (default `mr`), exposed on `uzi schedule create/edit --output mr|issues` and the web schedule modal. Sweep/issue targets reject it (422/usage error) — their output is a run on an existing issue, not a proposal.
2. **Issues-mode filing is SERVER-SIDE.** The run produces the proposal (title + body, structured); after the run completes, the api files it via `forge.CreateIssue` on the owner's connection, carrying the scoped marker label **`proposal::<catalog-slug>`** (auto-created on the forge if missing, same advisory guardrail pattern as sweep-label creation). The agent gains NO forge-write tool — same trust model as findings/recommendation filing.
3. **The filed issue is never sweep-eligible at creation.** It carries ONLY `proposal::<slug>` — never `uzi`, never a sweep selector (`Planned`/`bug`/`refactor`), never a bot assignment. A human promotes it by adding labels. This is a tested invariant, not a convention.
4. **Dedup-by-disposition, injected at fire time.** For an issues-mode job, the fire path lists the repo's `proposal::<slug>` issues — open AND closed, with state and title (`ListIssuesOptions.Labels` is AND-filtered, forge.go:421) — and appends a compact digest to the rendered prompt, instructing: do not re-propose an open or closed proposal unless the evidence materially changed (and say what changed). Server-side injection is chosen for DETERMINISM, not capability: the agent does have a `list_issues` tool (agent/src/forge-tools.ts:144 → handler/worker_forge.go:254), but a fire-time digest composes exactly once and does not depend on the agent remembering to call a tool and obey.
5. **Prompt bodies adapt per mode.** The catalog body stays mode-neutral where possible; the fire path appends the mode-specific delivery instruction (mr: "write the idea file and open an MR" / issues: "emit the proposal via the structured channel; do not write files or open an MR"). feature-bingo's shipped default stays `mr` (no behavior change on upgrade); owners flip per schedule.

## Sequencing (BLOCKING constraint for the sweep)

(Satisfied: #918/#919/#920/#921 all merged as of 2026-09-03.)

**Do not start this PRD before #918 and #919 are merged.** #929 shares files with two epic #915 Batch 1 PRDs: `api/internal/handler/schedules.go` (#918 migrated its uuid-parse sites, now `schedules_request.go:314,:399` and `schedules_clone.go:62,:190`, `CreateSchedule` at `schedules.go:49`; M1 adds output_mode validation there) and `web/src/components/ScheduleModal.tsx` + `web/src/pages/Schedules.tsx` (#919 migrates their ApiError sites; M1 adds the mode field). The epic freeze normally enforces this automatically — the Planned sweep only resumes after all refactor PRDs merge — but the constraint stands on its own against any manual early start. The #920/#921 collisions are avoided structurally instead: M2's new code goes in NEW files only (never inside `workersvc/service.go`, never in the `agent/src` files #920 consolidates).

## Milestones

- [ ] **M1 — setting + plumbing.** `run_schedules.output_mode` migration (number assigned at merge per convention), sqlc queries, catalog frontmatter `output:` key parsed in `schedtmpl` (default `mr`, validated against the enum), `uzi schedule create/edit --output` (three-way: omit = inherit job/catalog default), web schedule modal field for prompt-target schedules (plus the matching `output_mode` in `web/src/mocks/mockApi/schedules.ts`'s schedule shape, so mock mode does not silently drift), server-side validation rejecting it on non-prompt targets. Tests: frontmatter parse/validity alongside the existing catalog tests; a scheduler test that a NULL mode resolves to the catalog default and an explicit mode overrides it. `task gate:api` + `gate:web` green.
- [ ] **M2 — the proposal channel + server-side filing.** The near-exact shape ALREADY EXISTS in chat: `workersvc/chat.go:498 CreateProposal(wkr, runID, repoID, title, description, labelsJSON)` → the `issue_proposals` table → `ConfirmProposalForUser` → `forge.CreateIssue` (composite.go:149; `ConfirmProposal` at chat.go:458 only stamps the IID). Reuse that storage shape and worker-facing channel; the delta for issues-mode prompt runs is auto-filing on run completion instead of chat's human-click confirm. **File locations are constrained for freeze-sequencing (see Sequencing below): new files only — e.g. `workersvc/proposal_filing.go` or a schedsvc home — NEVER edits inside `workersvc/service.go` (being split by #921) and no edits to the files #920 consolidates in `agent/src` (prefer zero agent-side change; the channel exists).** The fixed decision: the agent gets no forge-write tool, the api does the filing (D1), with `proposal::<slug>` auto-created (advisory-warn on forge label errors like the sweep-label guardrail). The no-proposal no-op run files nothing. Tests: filing happens with exactly the marker label; the never-sweepable invariant (D3) asserted — the created issue carries no `uzi`, no sweep selector; a live-DB test if the storage path needs one. `task gate:api` green.
- [ ] **M3 — fire-time dedup digest.** For issues-mode prompt schedules, the fire path lists `proposal::<slug>` issues via the existing forge layer (open + closed; state + title only — `forge.Issue` has NO close-reason field and adding one is an interface change this PRD must NOT make: it would fan out to 3 drivers + `forgetest.BaseFake` (the six test fakes embed it and inherit new methods, PRD #922) and lacks cross-forge support) and appends the digest + dedup instruction to the rendered prompt. Note `composeRunDescription` (scheduler.go:828) is a 2-segment `(body, guidance)` join — appending digest + delivery instruction needs a small extension of that seam, not a drop-in. Size-capped digest (newest N when over cap, cap recorded in code). Test: a scheduler test proving the digest lands in the rendered prompt and a closed proposal's title appears in it. `task gate:api` green.
- [ ] **M4 — mode-aware prompt assembly + feature-bingo adoption.** The mode-specific delivery instruction appended at fire time; feature-bingo's body trimmed of mr-only wording where it would contradict issues mode (shipped default stays `output: mr` — no behavior change for existing enablements). Cross-link #928 so refactor-scout ships mode-agnostic. Tests: rendered-prompt assertions for both modes. `task gate:api` green.
- [ ] **M5 — docs + CLI skill.** `docs/scheduling.md` (the output-mode concept, the `proposal::<slug>` label, the promote flow, the never-sweepable guarantee), `docs/cli.md` + the embedded CLI skill (`api/internal/uzicli/skill/SKILL.md`, drift-tested) for the new flag, web modal help text. `check-docs` green; full `task gate` green.

## Success criteria

1. An issues-mode feature-bingo fire produces a forge issue labeled `proposal::feature-bingo` and NOTHING else label-wise; an mr-mode fire behaves byte-for-byte as today.
2. The never-sweepable invariant holds under test: a filed proposal issue cannot be picked by any sweep until a human adds eligibility labels.
3. A second fire does not re-propose an open or declined proposal (dedup digest proven present in the rendered prompt).
4. Existing enablements are unaffected on upgrade (defaults stay `mr`; NULL-mode rows resolve to catalog default).
5. `task gate` green across components; no `.github/workflows/**` in the branch diff (implementation OR validation).

## Decision Log

- **D1 — filing is server-side; the agent never gains a forge-write tool.** The worker/agent trust model deliberately keeps forge writes in the api (findings and judge-recommendation filing already work this way, PAT never in agent hands). An in-run "create issue" tool was rejected: it widens the agent's blast surface for a convenience the completion hook provides anyway.
- **D2 — scoped marker label `proposal::<catalog-slug>`.** Matches the repo's existing `effort::*` scoping; one label answers both "all proposals" (prefix) and "this job's dedup history" (exact). Auto-created with the same advisory (never blocking) guardrail as sweep-label creation.
- **D3 — proposal issues are never sweep-eligible at creation.** No `uzi`, no `Planned`/`bug`/other selector, no bot assignment. An unattended 02:00 sweep must never auto-implement a half-formed idea; promotion is a deliberate human act (this is the issue-triage skill's "the inverse is just as deliberate" rule, made structural). Tested invariant (M2), not prompt text.
- **D4 — dedup context is injected at fire time by the server, for DETERMINISM, not capability.** The agent CAN list labeled issues itself (`list_issues`, agent/src/forge-tools.ts:144 → handler/worker_forge.go:254) — do not be confused by finding that tool. Server-side injection is chosen because a fire-time digest composes exactly once, is testable in the scheduler, and does not depend on the agent remembering to call a tool and obey its output. The fire path already resolves catalog content per schedule; the digest extends `composeRunDescription` (scheduler.go:828, today a 2-segment join) rather than riding it unchanged.
- **D5 — shipped catalog defaults stay `mr`.** Zero behavior change on upgrade; switching a job to issues mode is an owner action per schedule (or a later, deliberate catalog-default change). Which mode refactor-scout (#928) defaults to is decided in #928, not here.
- **D6 — sweep/issue targets reject the setting** rather than ignoring it: a silently-ignored option teaches wrong mental models; a 422 teaches the real one.

## Risks & mitigations

- **Label proliferation on the forge.** One label per proposal-shaped job, auto-created; bounded by catalog size. The advisory guardrail warns rather than fails when a forge refuses label creation.
- **A filed proposal issue accidentally becomes sweep-eligible** (the real hazard). Mitigated by D3 as a tested invariant plus the label set being constructed server-side from constants, never from model output.
- **Digest bloat on repos with many historical proposals.** Size-capped digest (newest N + counts); dedup degrades gracefully to "most recent history" rather than growing the prompt unboundedly.
- **Prompt-mode divergence** (a catalog body contradicting the appended delivery instruction). M4 trims mode-contradicting wording; the rendered-prompt tests pin both modes.
- **Migration numbering collision.** Standard convention: number assigned at merge time above the live head; `check:migration-numbering` gates duplicates.
- **Parallel-sweep collision with epic #915 Batch 1** (the Sequencing section's subject). Enforced twice: the freeze ordering (Planned sweep resumes only post-freeze) and M2's new-files-only constraint. If this PRD is ever started manually before #918/#919 merge, expect merge conflicts in `handler/schedules.go` and the web schedule components — wait instead.
