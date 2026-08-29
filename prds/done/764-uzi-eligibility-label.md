# PRD #764 — Single `uzi` label for run-eligibility; make PRD optional and auto-detected

**Issue**: #764
**Priority**: Medium
**Status**: Draft

## Problem

uzi's run-eligibility model tangles several labels, and a new user cannot tell why an issue
"looks queued but never runs." Today an issue starts a run only when it clears **two** gates:

- **Gate A — eligibility**: it carries a label in `run_eligible_labels` (default `PRD,bug`).
- **Gate B — PRD-link**: it links a `prds/*.md` file **OR** carries `PRDLESS` **OR** an admin
  `eligible_label_waives_prd_link` waiver applies.

So the model is "an eligible label AND (a PRD link OR a double-negative escape-hatch label OR an
admin waiver)". That either/or is the single biggest source of the silently-un-sweepable gap the
`issue-triage` skill exists to hunt: an issue with the right selector but no fireable half looks
queued and never fires, and nobody notices. `PRDLESS` reads like a warning rather than a
permission, and the "PRD-vs-PRDLESS" fork is the concept new users most often get wrong.

## Target model (what a new user should have to learn)

Three **orthogonal, one-thing-each** labels, no hidden either/or:

1. **`uzi`** (new, configurable, default `uzi`) — "this issue is uzi's to run." The single
   eligibility gate. Label an issue `uzi` and it is runnable.
2. **`Planned` / `bug`** — sweep **selectors**. A sweep picks a candidate by its selector and
   fires it **only when it also carries `uzi`**.
3. **`autopilot`** — unchanged; the separate opt-in that skips the plan-approval gate.

A linked PRD becomes **optional**: still auto-detected and still implemented when present (with a
visible "PRD" badge), but never required to start a run. The board keeps showing **all** open
issues; `uzi` adds a runnable marker plus a filter facet, so nothing disappears on upgrade.

## Approach: hard cutover, no compatibility shim

uzi currently has exactly one user — us. There are no external self-hosted instances to protect on
upgrade, so this PRD does **not** carry a backward-compatibility shim or a deprecation window.
Instead it is a clean cutover:

- The old eligibility machinery (`PRDLESS`, the `eligible_label_waives_prd_link` waiver, the
  `run_eligible_labels` / `board_extra_labels` sets, and the `PRD`-label special-casing) is
  **removed end to end**, not deprecated-and-left-inert. The whole point is a simpler system; dead
  code that implements the old model defeats that.
- The one-time migration is a **local `gh` loop** that adds `uzi` to our own currently-runnable
  open issues, run at cutover (M5). No product migration tool, no admin endpoint, no CLI verb.
- If uzi ever gains external users, a compat shim can be reintroduced in its own PRD; it is
  explicitly out of scope here (see Decision Log D5).

Because the board still shows every open issue (below), the failure mode of a missed label is soft
and visible — an issue that is no longer runnable until `uzi` is added — never data loss.

## Current implementation (code map — read before implementing)

Verified against the tree at PRD-authoring time by three independent reviewers (only cosmetic line
drift found); re-derive line numbers at implementation time.

- **Config** lives in the `app_settings` table behind `settings.Cache`
  (`api/internal/settings/settings.go`). Relevant keys and defaults:
  `prd_label`=`PRD`, `autopilot_label`=`autopilot`, `prdless_enabled`=`true`,
  `prdless_label`=`PRDLESS`, `run_eligible_labels`=`PRD,bug`, `board_extra_labels`=`bug`,
  `eligible_label_waives_prd_link`=`true`. These keys use the **no-seeded-row** pattern: an absent
  row synthesizes from the Go `Defaults` map (`settings.go:369-378, 433-438`), so adding
  `uzi_label` needs only a `Defaults` entry + `Cache` accessor — **no goose migration** — and it
  auto-applies. `ValidateMerged` (`settings.go:1800-1808`) enforces pairwise distinctness for
  `prd_label`/`autopilot_label`/`prdless_label` and forbids those markers inside the label lists
  (`validateLabelListMerged`, `:1874-1879`); `LabelChanged` (`:1777-1787`) forces a resync only on
  `prd_label`/`autopilot_label`. The session bootstrap **emits** these labels to the SPA at
  `api/internal/handler/auth.go:400-413` (not just reads them at `:351-373`), with the comment that
  "the SPA renders the card/issue-view Start affordance from the eligible set and the waiver" —
  so removing the old keys means unwinding that SPA dependency, not only dropping a field.
- **Eligibility enforcement** is one point: `workersvc.(*Service).createRun`
  (`api/internal/workersvc/service.go:4500`), reached by every create path.
  - Gate A: `isEligibleIssue(issue.Labels, s.runEligibleLabels(ctx))` (`service.go:4623-4624`,
    `isEligibleIssue` at `:4825`) → `ErrNotPRDIssue`. Reads the **cached** `issues.labels` jsonb.
  - Gate B: `if !issue.HasPrdLink && !allowWithoutPRD && !linkWaived { return ErrNoPRDLink }`
    (`service.go:4648-4649`). `allowWithoutPRD` is the PRDLESS bypass computed by the caller
    (`prdlessAllows`, `composite.go:242`); `linkWaived` is the interactive-only waiver
    (`service.go:4647`, via `eligibleByNonPrimary`, `:4843`).
  - Caller matrix: `CreateRun` (interactive Start) `autoApprove=false, allowLinkWaiver=true`
    (`:4419`); `CreateScheduledRun` `false,false` (`:4431`); `CreateAutopilotRun`
    `true,false` (`:4457`); `CreateScheduledAutopilotRun` `true,false` (`:4471`). The waiver is
    reachable **only** on the interactive human path.
  - The two sentinels surface at more than the CLI: `handler/workers.go:1049-1056` (web Start
    messages), `poller/autopilot.go:190,221-224` (posts `noPRDLinkComment` /
    `noEligibleUserComment` **to the forge issue**, text at `:385` telling users to add a
    `prds/*.md` link and re-add the label), `schedsvc/skip_reason.go:18-68` (the
    `no_prd_link`/`not_eligible` enum, a cross-language wire-contract with a TS test),
    `schedsvc/scheduler.go:556-560` (skip logging), and `cmd/server/main.go:1241,1259-1261`
    (server-side CLI error→exit mapping). All of these are worded for the old model.
- **The default-branch guardrail is independent of both gates**: `composite.go:80-101` runs at
  `service.go:4589`, **before** eligibility, and the no-`.github/workflows` push control is a
  PAT-scope/push-time control. Neither depends on a PRD, so removing Gate B has **no**
  main-protection implication (Gate B was a "no spec" nudge, never a security control).
- **PRD detection** is label-independent: `forgesvc.HasPRDLink` (`api/internal/forgesvc/service.go:85-86`,
  regex `prdLinkRe` at `:82`) is computed at sync-write time into `issues.has_prd_link`
  (`api/internal/store/queries/forge.sql:298,306`) and inline on issue creation
  (`api/internal/handler/issues.go:91,102`). Nothing downstream hard-requires a PRD: the agent's
  PRD-update skill is an explicit no-op with no link (`prd-lifecycle/SKILL.md`, `agent/src/prd-link.ts`
  `NO_PRD` fallback), `clampWirePRDDonePath` is nil-safe, and self-improve is a separate
  non-PRD path. A stricter detector, `prdpath.Links` (`api/internal/prdpath/prdpath.go:207`),
  governs which file the agent opens/updates — separate from the gate.
- **Board** already shows **all** open issues: `forgesvc.FullSync` (`service.go:428-462`) does two
  additive fetches — `ListIssues{Labels:[prdLabel]}` state=all, then `ListIssues{State:StateOpened}`
  unfiltered (`extra := withoutLabel(...)`) — and `Handler.GetBoard` (`api/internal/handler/board.go:245`)
  returns every cached issue; non-PRD cards are filtered **client-side** behind a per-browser
  toggle (`board.go:241`). The board-card DTO **already exposes** `has_prd_link`
  (`board.go:57,558,1122`) — the badge reuses it, no new `prd_present` field on the card.
- **Sweeps** are two-stage. Stage 1 selector: `ListSweepCandidateIssues`
  (`api/internal/store/queries/schedules.sql:233`), `state='opened' AND labels @> @labels::jsonb`;
  selector from the schedule catalog (`api/internal/schedtmpl/catalog/planned-sweep.md`, `bug-*`;
  parser at `schedtmpl.go:150,192`). Stage 2 fireability: `createIssueRun`
  (`api/internal/schedsvc/scheduler.go:528`) calls `CreateScheduledRun` /
  `CreateScheduledAutopilotRun` — both `allowLinkWaiver=false`. Because `bug` is in
  `run_eligible_labels` today, a `bug` issue passes Gate A; the selector doubles as an eligibility
  label. **The live-DB (`*LiveDB`) scheduler tests are skipped by `task gate:api`** and only run
  via `./e2e/run-store-it.sh` (see `.claude/rules/go.md`).

## Milestones

- [x] **M1 — `uzi` becomes the single eligibility gate (server).** Add a configurable `uzi_label`
  setting (default `uzi`), with distinctness validation against `autopilot_label` and a rule for
  whether `uzi` may appear in any remaining label list, shipped to the SPA bootstrap
  (`auth.go:400-413`). In `workersvc.createRun`, **Gate A becomes "carries `uzi_label`"** and
  **Gate B is removed** — a run no longer requires a PRD link, PRDLESS, or a waiver. Update the
  server-returned sentinels/wording so a non-`uzi` issue is refused with a clear "add the `uzi`
  label" message on every consumer listed in the code map (`handler/workers.go`,
  `poller/autopilot.go` forge comment, `cmd/server/main.go`). Tests across all four create paths,
  each **calibrated to fail against pre-change code**: a `uzi`-only issue with no PRD link is
  refused pre-change (`ErrNotPRDIssue` on Gate A) and **runs** post-change. *Success*: an issue
  carrying only `uzi` (no PRD, no PRDLESS) starts a run on every path; an issue without `uzi` is
  refused with the new message; the four-path tests fail on the pre-change binary.

- [x] **M2 — PRD optional but still detected, implemented, and signalled.** Confirm `HasPRDLink`
  detection is unchanged and label-independent, so a linked PRD is still found; keep the agent's
  "implement/update the linked `prds/*.md`" behavior when a link is present (guard with a test) and
  confirm a run with no link proceeds cleanly. The board badge **reuses the existing
  `has_prd_link`** DTO field (no new field on the card); add the field to the **runs** DTO only
  where the runs view needs it (assert it is absent pre-change, present post-change — the sole
  falsifiable surface here). *Success*: a `uzi` run with a PRD link opens/updates the PRD exactly
  as today; a `uzi` run without one completes; the runs DTO exposes PRD presence for the UI.

- [x] **M3 — Sweeps: `Planned`/`bug` are pure selectors, gated by `uzi`.** Leave the selector
  query unchanged; fireability inherits M1, so a candidate fires iff it carries `uzi`. `bug` keeps
  its selector role but loses any eligibility role (it is no longer in an eligible-set — that set
  is gone with M1). **Lead tests with the discriminating, fail-pre-change cases**: a `bug`+`uzi`
  issue with no PRD link **fires** (pre-change: refused by Gate B), and a `uzi`-only swept issue
  fires; then a bare `bug` issue (no `uzi`) is a benign `not_eligible` skip that **advances the
  schedule**. The schedule-advance assertion is `*LiveDB` and must run under
  `./e2e/run-store-it.sh` with positive controls (`RUN>0`, zero `--- SKIP`, non-sub-second
  package time — `.claude/rules/go.md`), not `task gate:api`. Accepted decision (D6): a
  `uzi`+selector issue with no PRD auto-runs unattended (sweeps auto-approve). *Success*: the two
  live sweeps (`bug`, `Planned`) fire a candidate only when it also carries `uzi`; the discriminating
  fire-cases fail on the pre-change binary; a bare selector issue skips without erroring the schedule.

- [x] **M4 — Web: `uzi` filter + PRD badge; remove PRDLESS UI.** Board keeps rendering all open
  issues (unchanged). The client toggle becomes `uzi`-only / all (was PRD / all), and each card
  shows a runnable marker = has `uzi`. Add a neutral **"PRD" presence badge** (from `has_prd_link`)
  on the board card **and** the runs view. **Remove** the PRDLESS toggle, the "no PRD link"
  warning, and the PRDLESS badge, and **unwind the SPA's dependency** on the bootstrap
  `run_eligible_labels` / `eligible_label_waives_prd_link` / `prdless_*` keys (they no longer
  exist after M1/M5). Apply the retired-string discipline (`.claude/rules/web.md`): grep each
  removed string across the test tree, **delete unpaired negative assertions and repoint paired
  ones**, and add a **positive** assertion on the new badge (so "the old badges are gone" is not an
  unfalsifiable green). No CLI display change (the CLI already prints the linked PRD path in
  `run get`). *Success*: a user can filter the board to `uzi` issues and see at a glance which runs
  have a PRD; the PRDLESS/no-link UI is gone with no vacuous negative assertions left behind.

- [x] **M5 — Tear out the old model end-to-end, fix messaging, migrate our labels, close out.**
  Delete the now-dead settings and their accessors/validation: `prdless_enabled`, `prdless_label`,
  `eligible_label_waives_prd_link`, `run_eligible_labels`, `board_extra_labels`, and the
  `PRD`-label special-casing (`prd_label`) — PRD-ness is detected purely from the body link, and
  the any-state sync fetch (`FullSync`) keys on `uzi_label` instead of `prd_label`. Remove the dead
  gate helpers (`prdlessAllows`, `eligibleByNonPrimary`, the waiver path) and retire the
  `no_prd_link` skip-reason from the wire enum (keep `not_eligible`), updating its cross-language
  contract test. Migrate our own repo: a committed one-shot (`scripts/…` or a documented `gh`
  loop) that adds `uzi` to currently-runnable open issues, run at cutover. Docs: **delete**
  `docs/prdless.md`; update `docs/scheduling.md` (sweep = selector + `uzi`), `docs/admin-settings.md`
  (new `uzi_label`; removed keys), `docs/cli.md`, `ARCHITECTURE.md` (eligibility + board),
  `specs/ai.md` (decision record), the CHANGELOG, and the in-repo `.claude/skills/issue-triage`
  and `.claude/skills/uzi-watcher` skills so their gating mechanics match. `task gate:api`,
  `gate:web`, and `gate:repo` green (+ store-it where M3 needs it). Move this PRD to `prds/done/`.
  *Success*: no PRDLESS / waiver / eligible-set code, settings, or docs remain; every messaging
  surface describes the `uzi` model; our issues carry `uzi`; gates green.

## Out of scope

- **A backward-compatibility shim / deprecation window.** No external users exist to protect; a
  hard cutover is simpler and avoids the frozen-legacy-set complexity a shim would need (D5). If
  external users ever exist, a shim is a future PRD.
- **`.github/workflows/**` changes** — none are needed, and the branch deliberately touches no
  workflow file so it stays worker-pushable (the worker PAT lacks `workflow` scope; see
  `.claude/rules/prds.md`).
- **A product migration tool** (admin endpoint / CLI verb) — the one-time back-label is a local
  `gh` loop against our own repo (M5).
- **Board column/lane mapping** (forge-label → column via `board.ResolveColumn`) — unrelated to
  eligibility, untouched.
- **`autopilot` semantics** — unchanged; it remains a separate opt-in for skipping approval.

## Success criteria (whole PRD)

1. A user labels one issue `uzi` and it runs — no PRD file, no `PRDLESS`, no waiver knowledge
   required.
2. A linked `prds/*.md` is still detected and implemented when present, and the board and runs
   view show whether a run has a PRD.
3. `Planned`/`bug` sweeps fire a candidate only when it also carries `uzi`; a bare selector issue
   is a benign skip that advances the schedule.
4. No PRDLESS / waiver / eligible-set code, settings, docs, or UI remain; the model is explainable
   as three orthogonal labels: `uzi` (runnable) → `Planned`/`bug` (auto-pick) → `autopilot` (skip
   approval).
5. `task gate:api`, `gate:web`, and `gate:repo` are green, and the M3 schedule-advance assertion
   runs under `./e2e/run-store-it.sh` with positive controls; the new behavior is covered by tests
   that **fail against the pre-change code** (a `uzi`-only issue that pre-change hits
   `ErrNotPRDIssue`; a `bug`+`uzi` no-link issue that pre-change hits `ErrNoPRDLink`).

## Risks

- **R1 — Removing Gate B lets a spec-light issue auto-run under a sweep (no plan gate).**
  *Mitigation*: accepted by design (D6) — already true today for a `PRDLESS`+selector issue;
  non-auto runs keep the plan-approval gate, and the M4 PRD-presence badge makes a spec-light
  auto-run visible for review. No new gate added.
- **R2 — Cutover leaves one of our issues un-migrated.** *Mitigation*: soft, visible failure — the
  issue simply is not runnable until `uzi` is added; the board still shows it; no data loss. The
  M5 `gh` loop is idempotent (`--add-label` is a no-op if present) and re-runnable.
- **R3 — Tearing out PRDLESS/waiver/eligible-set misses a consumer** (the SPA bootstrap dependency,
  a forge-issue comment, the skip-reason enum, the server exit map). *Mitigation*: the code map
  enumerates every consumer the reviewers found; M1 and M5 walk that list, and M4 applies the
  retired-string sweep so no stale wording or vacuous negative assertion survives.
- **R4 — Two PRD detectors disagree about "present".** `forgesvc.HasPRDLink` (the badge source)
  accepts a broader set than `prdpath.Links` (which file the agent opens). *Mitigation*: the badge
  uses the same `has_prd_link` the run used; docs state `prdpath.Links` governs implementation
  targeting; keep them consistent for the common `prds/x.md` case.
- **R5 — `uzi` added directly on the forge then Start clicked immediately hits a cache-lag refusal**
  (Gate A reads cached `issues.labels` by design). *Mitigation*: labeling from uzi's own UI can go
  through the existing forge-first-plus-cache **Promote** path (`handler.PromoteIssue`); labeling
  directly on the forge carries normal sync lag, documented — the same lag any label change has
  today.

## Decision Log

- **D1 — One eligibility label (`uzi`), not a set + waiver + PRD/PRDLESS either/or.** Collapses
  `run_eligible_labels` + `eligible_label_waives_prd_link` + the PRD-link/PRDLESS fork into a
  single opt-in. The removed confusion is the documented cause of "looks queued, never runs" (the
  `issue-triage` skill's Tier-1 gaps).
- **D2 — PRD optional, not removed.** Detection (`has_prd_link`) and implementation stay; only the
  *requirement* goes. A present PRD is still honored and now surfaced as a badge (the user's
  "mark somehow that a PRD was present"), reusing the existing DTO field.
- **D3 — Board shows all; `uzi` is a runnable marker + filter, not a membership gate.** Chosen
  over a `uzi`-gated board because the goal is new-user graspability: an opt-in that empties the
  board on first connect is a worse first-run. This keeps the current all-open board (verified: the
  unfiltered open fetch already captures a `uzi`-only issue) and adds the opt-in where it matters —
  running. Swapping the client toggle from PRD-based to `uzi`-based is a drop-in.
- **D4 — `Planned`/`bug` are pure selectors, gated by `uzi`.** A selector never implies
  runnability; matches the user's "keep planned/bug, as long as they also have `uzi`." Selector =
  candidacy, `uzi` = fire.
- **D5 — Hard cutover, no compat shim — we are the only user.** The shim (run if `uzi` OR
  old-rules) was the largest source of complexity and required a frozen legacy `{PRD,bug}` constant
  decoupled from the mutated default to avoid silently stopping sweeps on upgrade (the P0 all three
  reviewers independently flagged). With no external users, that entire mechanism is unnecessary:
  remove the old model outright and migrate our own labels with a local script. A shim is a future
  PRD if external users ever exist.
- **D6 — Accept unattended spec-light sweep runs.** Sweeps auto-approve, so a `uzi`+selector issue
  with no PRD runs with no plan gate. This already holds today for `PRDLESS`+selector; the badge
  makes it visible; not adding a new gate keeps the model simple.
- **D7 — The `PRD` label loses all special meaning.** PRD-ness is detected purely from the body
  link, so the `prd_label` setting and its sync-fetch/eligibility special-casing are removed; the
  any-state sync fetch keys on `uzi_label` instead. `PRD` may still be used as a plain
  organizational label, but uzi no longer treats it specially.
