# PRD #19: Admin Settings (app_settings) + Autopilot Label

**GitLab Issue**: [#19](https://gitlab.example.com/vtmocanu/uzi/-/issues/19)
**Status**: Draft
**Priority**: Medium
**Created**: 2026-07-05

**Depends on**: PRD #2 (forge layer + poller, done), PRD #4 (run lifecycle + claim payload, done). Independent of PRDs #16/#17/#18 (no shared tables), but reserves its own migration range to avoid goose collisions.

## Problem

1. **No instance-settings infrastructure.** Server config is env-vars only (`api/internal/config`); the admin surface is user management only (`api/internal/handler/admin.go`). plan.md already queues up several instance settings with nowhere to live: registration domain allowlist (line 57), enable/disable registration (line 75), the self-improvement schedule toggle (line 68).
2. **The PRD label is hardcoded twice.** `const PRDLabel = "PRD"` (`api/internal/forgesvc/service.go:24`) drives both sync paths and issue creation (`api/internal/handler/issues.go:59`), and the string `"PRD"` is hardcoded again in `web/src/pages/Board.tsx:637`. An instance that wants a different convention has to fork.
3. **Every run needs two uzi visits.** Start run, then approve the plan. For issues a user trusts the factory to handle end to end, they want to add one label in GitLab and come back to a finished MR (plan.md line 70 sketches exactly this) — watching progress via the existing label-based board sync ("In Progress" appears in GitLab), never opening uzi at all.

## Solution Overview

1. **`app_settings`**: a DB-backed key/value settings table, admin-only API (`GET/PUT /api/admin/settings`), and an admin Settings page. First two keys: `prd_label` (default `"PRD"`) and `autopilot_label` (default `"autopilot"`). Values are read through a small cached accessor in the API process; future settings (registration policy, self-improvement toggle) slot in without new plumbing.
2. **Configurable PRD label**: `forgesvc` and issue creation read `prd_label` from settings; the web app receives it via the existing session/bootstrap response (no new endpoint) so `Board.tsx` stops hardcoding `"PRD"`. Changing the label triggers a full resync per connected repo; issues carrying only the old label drop off boards (forge is source of truth — correct and documented).
3. **Autopilot**: when the poller sees the autopilot label appear on a PRD-labeled issue, it resolves *which human added it* (GitLab resource label events API), maps that human to a uzi user, and creates a run for that user with `auto_approve = true`. The worker still runs the planning turn and records the plan, then auto-approves instead of blocking on `awaiting_approval`. On run failure, uzi comments on the GitLab issue so a GitLab-only user isn't left waiting forever. Attribution + consent:
   - Each user may store their **human forge username** on their forge connection (new optional field; uzi only knows the bot identity from the PAT today, `api/internal/forge/gitlab.go:41`).
   - Each user has an **autopilot opt-in** (default off). No mapping or opt-out → no run.
   - Resolution order: label adder → issue author (user, 2026-07-05); first one that maps to an opted-in uzi user with the repo connected wins. No match → no run + one explanatory issue comment (never silent).

## Design Decisions

1. **Settings infra is the deliverable, labels are the first tenants** (user, 2026-07-05). Build `app_settings` generic (key/value TEXT, typed accessors in Go) but ship only the two label keys; registration/self-improvement settings arrive with their own PRDs. Avoids both a one-off `prd_label` column and a speculative settings framework.
2. **Autopilot keeps the planning turn and auto-approves it** rather than skipping planning. The plan stays in the run history as the audit record of what the agent intended; the only thing removed is the human block. Implementation is a claim-payload flag (`ClaimConfig.auto_approve`), worker-side — the gate stays worker-enforced, consistent with the existing architecture (`ctx.gatePlan`, `agent/src/sdk-executor.ts:220`).
3. **Attribution: label adder first, issue author fallback** (user, 2026-07-05 — supersedes the connection-owner-only proposal). Requires the resource-label-events fetch on autopilot detection. Both identities are human GitLab accounts, hence the stored human-username mapping; bot PAT identity cannot resolve them.
4. **Consent is per-user, not per-repo.** A third party adding the autopilot label to an issue you authored must not be able to spend your Anthropic tokens: the mapped user must have autopilot explicitly enabled (default off). A per-repo toggle adds surface without adding consent — the per-user switch plus "repo must be connected by that user" already bounds it.
5. **Trigger on label transition, once per application — state in a dedicated table, not the issues cache** (review finding B1). The `issues` table is a cache the sync is allowed to evict and rewrite (`DeleteIssuesNotIn` on FullSync), so "never re-comment / never re-trigger" guarantees cannot live there. A dedicated `autopilot_triggers` table keyed `(repo_id, issue_iid)` records the last handled label-event id and last comment posted; it survives cache eviction. A run starts only on absent→present transition with no active run for the issue (the existing `uq_runs_one_active_per_issue` index is the structural backstop). Failed runs do not auto-retry — remove and re-add the label to retry (a deliberate human action, the natural GitLab gesture). Removing the label mid-run does not cancel, and re-adding it while a run is active is swallowed (documented) — no queued re-runs in v1.
6. **Outcomes are surfaced in GitLab through one hook.** Forge-first visibility is the whole point of autopilot: on terminal failure/timeout the API posts an issue comment (run link + short reason), and on success a comment with the MR link. A run reaches terminal-failed via two mutually exclusive paths (worker `SetState('failed')` or the sweeper), so the comment is posted from a single "run entered terminal state" hook on the run-lifecycle notifier — not duplicated in both call sites (multica does the same via a terminal-event listener). Ordering: record the handled state in `autopilot_triggers` first, then post the comment — a crash between the two loses one comment rather than ever double-commenting. Requires `CreateIssueNote` on the `Forge` interface (exists in client-go, behind the redactor as usual).
7. **Autopilot trades the pre-execution human review for the MR review — accepted by design** (review finding M1). Issue title/description are untrusted input; today an injected issue faces two human checkpoints (start click + plan approval) before tokens are spent or the agent acts. Autopilot removes both: an injected run executes unattended and spends the mapped user's Anthropic tokens. The *repo* guardrails are genuinely unchanged (Developer role + protected `main`, worker-held PAT, deny-hook, `settingSources: []` — blast radius stays "an MR you must review"), and the consent gates (per-user opt-in, repo connected by that user, members-only labeling on GitLab) bound who can trigger it. The residual risk — unattended token spend and an unreviewed plan acting on injected instructions — is the explicit price of hands-off mode; users accept it per account via the opt-in, whose UI copy says exactly this.
8. **Label validation, no forge-side creation.** Both settings validate non-empty, ≤ 64 chars, no comma (GitLab label-list separator), and **`prd_label != autopilot_label`** (equal values would auto-run every PRD issue); changing them never creates labels on the forge — users create labels in GitLab themselves, as today (docs cover it). Autopilot only sees PRD-labeled issues (the sync filter): an autopilot label without the PRD label is invisible to uzi — no run, no comment — and the docs must say so.

## Technical Design

### 1. app_settings (api + web)

- Migration (reserve `00036`–`00039`; ledger: `00021` live head, `00022` #17, `00023`–`00028` #18, `00030+` #5, `00040+` #6, `00050+` #16): `app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_by UUID NULL REFERENCES users(id) ON DELETE SET NULL, updated_at TIMESTAMPTZ)`. Seed rows for `prd_label='PRD'`, `autopilot_label='autopilot'`.
- Go accessor in a new `api/internal/settings` package: read-through cache with short TTL (poller + handlers read every cycle; a settings PUT invalidates). Defaults compiled in — a missing row never breaks boot. The cache is per-process: correct for the single-`api` compose stack; a second replica would lag a PUT by up to the TTL (noted for the future k8s deployment).
- The same range covers the autopilot tables/columns: `autopilot_triggers` (Decision 5), `forge_connections.human_username`, `users.autopilot_enabled`, `runs.auto_approve`.
- Handlers: `GET /api/admin/settings`, `PUT /api/admin/settings` (admin-gated like `ListUsers`/`PatchUser`), validation per Decision 8.
- Web: admin-only Settings section (route + nav gated the same way existing admin UI is); the session/bootstrap payload gains `prd_label` + `autopilot_label` for Board and issue-creation UI.

### 2. Configurable prd_label (api + web)

- `forgesvc.PRDLabel` const → `settings.PRDLabel(ctx)` at the three use sites (both sync paths `service.go:145,174`, issue creation `issues.go:59`); the const remains only as the compiled-in default.
- **Forced resync mechanism (review finding B2 — none exists today)**: the poller decides FullSync from an in-memory `pollCount % reconcileEvery` (`poller.go:68,152`) with state private to the Engine goroutine, so the settings handler cannot reach it. Add a `ForceReconcile()` signal (non-blocking channel) on `poller.Engine` that resets the per-repo state so the next cycle full-syncs every enabled repo. The PUT returns after signalling — it does not block on the sync itself (which needs each connection's decrypted PAT and belongs in the poller). Document the board effect (old-label issues drop off after the resync completes, not instantly).
- `Board.tsx:637` uses the bootstrap-delivered labels instead of `"PRD"`, excluding **both** `prd_label` and `autopilot_label` from column suggestions, with the compiled-in defaults as fallback until bootstrap resolves.

### 3. Autopilot mapping + consent (api + web)

- `forge_connections` gains `human_username TEXT NULL` (the user's own GitLab account, not the bot), unique per forge host; on save, best-effort verify the username exists on the forge (squatting someone else's username before they connect is otherwise a mild identity-DoS — verified-or-warned, noted in docs). `users` gains `autopilot_enabled BOOLEAN NOT NULL DEFAULT false`. Both editable on the existing Forge/Settings pages with copy explaining what autopilot does and that the pre-execution plan review is skipped (Decision 7).
- `Forge` interface additions: `ListIssueLabelEvents(project, issueIID)` (resource label events — who added which label when; client-go v2.44 has it) and `CreateIssueNote(project, issueIID, body)`. GitLab driver only, as usual; errors through the PAT-scrubbing redactor.

### 4. Autopilot trigger (api: poller + runs)

- **Detection runs only in the poller, after sync** (review finding B3): the sync methods return `(maxUpdated, error)` only, so the poller re-queries the *cache* post-sync for issues carrying `autopilot_label` (new store query) and joins against `autopilot_triggers`. Detection must NOT live in shared `forgesvc` sync code — `CreateIssue` and manual board refresh go through the same service and must never spawn runs.
- Per detected issue: no active run AND stored last-handled label-event id predates the current application → resolve adder via `ListIssueLabelEvents`; map adder→uzi user by `human_username`; fallback issue author; check `autopilot_enabled` + repo connected by that user + Anthropic token present; create the run exactly as the manual start path does (same state machine) but with `auto_approve = true`; record the label-event id in `autopilot_triggers`.
- No resolvable/consenting user → record the event id first, then post one explanatory issue comment (record-then-comment, Decision 6 — never re-comments on later cycles).
- `runs.auto_approve BOOLEAN NOT NULL DEFAULT false`; surfaced in the run UI ("autopilot" badge). Delivered **top-level in `ClaimPayload`** (next to `Status`/`Branch`), read from the `runs` row — not in `ClaimConfig`, which is documented as worker-enforced caps and comes from instance params, not the run. Invariant: a requeued/resumed autopilot run re-delivers `auto_approve = true`, or an unattended resume would hang at the gate forever.

### 5. Autopilot execution (agent + api)

- Worker: when the claim carries `auto_approve`, the run **never enters `awaiting_approval`** (no state flicker, no column-automation churn): the plan is still emitted as the `kind:"plan"` run_message for the audit trail, and `gatePlan` resolves immediately with an approve verdict — auto-approve is a verdict source at the existing gate (`sdk-executor.ts:221`), not a bypass around it.
- Terminal comments (failure reason, or success + MR link) post from the single run-lifecycle terminal hook (Decision 6) in the API — the worker may be dead by then; both the worker `SetState` path and the sweeper path funnel through it.

### 6. Docs + specs

- `docs/`: admin-settings page (audience: user/admin) + autopilot section: label workflow, mapping field, opt-in, retry gesture (re-add label), what failure comments look like.
- `specs/ai.md`: settings infra, label configurability, autopilot design. `specs/human.md` addition (user approval required): autopilot user story.

## Milestones

- [ ] **M1 — app_settings infra**: migration, settings package with cached accessors, admin GET/PUT API, admin Settings UI with the two label fields validated.
- [ ] **M2 — prd_label configurable end-to-end**: forgesvc + issue creation read from settings, bootstrap delivers labels to web, Board uses it, label change triggers full resync.
- [ ] **M3 — Mapping + consent surface**: `human_username` on forge connections, `autopilot_enabled` user toggle, UI copy; Forge interface gains label events + issue notes (GitLab driver + redactor tests).
- [ ] **M4 — Autopilot trigger in poller**: `autopilot_triggers` table, post-sync cache query, transition detection with label-event dedup, adder→author resolution, consent checks, run creation with `auto_approve`, no-match issue comment (record-then-comment).
- [ ] **M5 — Auto-approve in worker + terminal comments**: claim delivers the flag top-level (incl. on resume), gate resolves immediately without entering `awaiting_approval`, plan message still recorded; single terminal-state hook posts failure-reason and success-MR comments; "autopilot" badge in run UI.
- [ ] **M6 — Tests, docs, specs**: e2e: labeled issue → run starts unattended → MR created (stub forge asserts the full loop); failure path comments; docs + specs updated, `specs/human.md` change approved by user.

Phase note: M1→M2 sequential; M3 parallel to M1–M2; M4→M5 after M1+M3. M6 spans all.

## Success Criteria

- Admin changes `prd_label`; board reflects the new label set after resync; no code fork needed.
- Adding `autopilot` + `PRD` to an issue in GitLab, as a user with a mapped username and autopilot enabled, produces — with zero uzi interaction — an "In Progress" label move, a recorded plan in run history (the run never shows `awaiting_approval`), an MR, and a success comment linking it.
- Adding the label as an unmapped/opted-out user produces one explanatory issue comment and no run, and never re-comments on subsequent poll cycles — including after a FullSync evicts and re-inserts the cached issue.
- A failed autopilot run posts one issue comment with the failure reason and a run link, whether it failed via the worker or the sweeper; re-adding the label retries; a resumed/requeued autopilot run never blocks at the plan gate.
- Removing the autopilot label mid-run changes nothing; re-adding it while a run is active is swallowed (both documented).
- Setting `prd_label` and `autopilot_label` to the same value is rejected.
- Plan approval for non-autopilot runs is unchanged; all four guardrail layers unchanged.

## Risks

- **Label-events API cost**: one extra forge call per autopilot-labeled issue per poll cycle until handled; bounded by the event-id dedup. If it proves chatty, cache negatively.
- **Attribution mismatch**: `human_username` is self-declared; a user could enter someone else's username (identity squat blocks that person from mapping later). Bounded: it only *grants* running work under one's own token/PAT, and save-time best-effort forge verification plus per-host uniqueness narrow it. Accepted for v1; note in docs.
- **Unattended injected runs**: Decision 7 — autopilot removes the pre-execution human review of untrusted issue text; MR review is the remaining human gate. Explicit per-user opt-in with honest UI copy.
- **Silent skew between prd_label change and cached issues**: mitigated by the mandatory `ForceReconcile` on change; the settings PUT returns after signalling the poller, and the board converges on the next cycle.
- **Two uzi users mapped to the same GitLab username**: enforce uniqueness on `human_username` per forge host at write time.
- **Auto-approved bad plans**: the cost of hands-off mode; guardrails cap the blast radius at "an MR you have to review anyway" — the MR review stays the human gate.
