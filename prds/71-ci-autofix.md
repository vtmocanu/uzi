# PRD #71: Automatic CI-fix for failed pipelines (opt-in, loop-guarded)

**GitLab Issue**: [vtmocanu/uzi#71](https://gitlab.example.com/vtmocanu/uzi/-/issues/71)
**Status**: Draft (reviewed — Opus architecture + fact-check pass, 2026-07-17; **drift review 2026-08-10** — two-agent fact-check/research pass re-verified all extension points live and folded staleness fixes below: migration head, forge-driver fan-out, `notifysvc` spelling, PRD #70/#158 landings)
**Priority**: Medium
**Created**: 2026-07-17
**Extends**: PRD #6 (CI Status Integration & CI-Fix Agent, done) — this is precisely the "auto-spawn fix runs on failure" that PRD #6 put Out of scope ("needs spend controls + notification story first"). Reuses the PRD #19 autopilot detector pattern.
**Depends on**: PRD #6 (manual `ci_fix` run machinery, pipeline watch, verification stamping — all merged) and PRD #19 (autopilot detector + `auto_approve` column — merged). No pending-PRD dependency; all extension points are live.

## Problem

PRD #6 gave uzi a *manual* **Fix CI** button: a failed watched pipeline surfaces a button, a human clicks it, a `ci_fix` run diagnoses and fixes. The button works, but nothing fires it automatically. So an agent's own MR goes red and sits red until a person notices and clicks. MR !66 is the live example that motivated this PRD: `test:web` failed (a stale `AppShell.test.tsx` mock the favicon change did not update), the pipeline went red, and no automation acted on it — the fix machinery existed but was never triggered.

PRD #6 deferred the auto-trigger deliberately, naming the two things it needed first: **spend controls** (auto-fixing burns tokens with nobody watching) and a **notification story** (the user must learn when an unattended fix ran or gave up). This PRD supplies both and turns the trigger on, without weakening any of the four `main`-never-touched guardrail layers.

## Solution Overview

An **opt-in, per-user, loop-guarded** automatic trigger for the existing `ci_fix` run. A new poller post-sync detector (`CIAutoFix`, a sibling of the PRD #19 `Autopilot` detector) runs each tick after the pipeline-status sync, on the freshly-refreshed `pipeline_statuses` cache. For each **agent-owned MR ref** whose newest pipeline is `failed` and whose owning user has opted in and has an Anthropic token, it consults a loop guard and either queues an **auto** `ci_fix` run (`auto_approve=true`) or **halts** (issue comment + notify + record, no run).

The four product decisions (user-locked 2026-07-17):

1. **Trigger scope — agent-owned MR refs only.** Auto-fix fires only on `refs/merge-requests/*` pipelines of agent-authored branches (`agent/issue-N`). `main` and protected branches are **never** auto-touched. This aligns with the primary directive: a fix still lands on the MR branch and a human still merges.
2. **Loop guard — cap + no-progress halt.** Max **2** auto attempts per ref (`CI_AUTOFIX_MAX_ATTEMPTS`), and an early halt when a re-run's *failure signature* is unchanged from the prior attempt (the fix made no progress). On halt: the last attempt is already terminal (`fix_verdict=fix_failed`), the detector posts a comment on the backing issue with the reason, notifies the owner, records the terminal state (with a comment-once latch) in the attempts ledger, and does **not** re-queue. A persistently-red pipeline can never loop.
3. **Toggle — per-user `ci_autofix_enabled`, default OFF.** A new per-user boolean modeled exactly on `judge_enabled` (migration `00061`) / `autopilot_enabled` (`00037`): self-service PUT plus an admin force-toggle (judge parity), default `false`. Opt-in.
4. **Fix authority — code auto, CI-config gated.** The agent diagnoses what is at fault ("we fix whatever should be fixed"). A **code/test** fix pushes automatically. A fix that edits the **CI config itself** (the pipeline definition) must pass the approval gate before pushing. "Usually we fix the code"; CI-config edits are the reviewed exception. Enforced in two layers (below), the load-bearing one being a fail-closed worker push-time guard whose protected-path set is **server-produced and includes the project's actual configured `ci_config_path`**, not just the static defaults.

### Extends, not copies (audit vs PRD #6's deferral)

PRD #6's Decision Log (2026-07-04) deferred auto-spawn for two reasons; this PRD answers each:

| PRD #6 deferral reason | This PRD's answer |
|---|---|
| "burns tokens with nobody watching" | Per-user **opt-in** + a hard **attempt cap** + a **no-progress halt** so it cannot loop. |
| "needs a notification story" | On auto-run **start**, on **halt**, and on a **landed (verified) fix**: an issue comment + an in-app notification (reusing the PRD #46 `notifications` system) + Slack best-effort where wired (`slacksvc`). |
| multica's "ungated autopilot is the audited weakness" | The plan gate still holds for CI-config fixes; code fixes are bounded by the cap + opt-in, not by nothing. Scope is narrowed to agent-owned MR branches, never `main`. |

## Design Decisions

Following the design doc and the Opus review (both in the Decision Log). The hard points:

**(a) Where the failure signature + attempt count live — a dedicated `ci_autofix_attempts` ledger.** Keyed `(repo_id, ref)`, the direct analogue of PRD #19's `autopilot_triggers`: a durable ledger that outlives both the run rows and the evictable pipeline cache. Rejected: deriving the count as `COUNT(ci_fix runs on ref)` (couples the guard to run retention and makes reset/compare a multi-row query); reusing `pipeline_statuses` (a pure forge cache, upserted/evicted every tick — durable guard state must not ride an evictable row, the lesson `autopilot_triggers` exists for).

**(b) The no-progress signature.** `workersvc.FailureSignature(snapshot)` = SHA-256 over a canonical string: the sorted `failed-job-name|stage` list, then per job a **normalized** fingerprint of the last ~20 non-empty log-tail lines. Normalization (one documented function) strips volatile tokens that differ run-to-run without meaning a different failure — ANSI escapes, ISO timestamps, durations, digit runs, hex/pointer addresses, `/builds`|`/tmp` absolute paths → placeholders; lowercase; collapse whitespace — then compares for exact equality. Deliberately **biased toward "same"** (aggressive normalization): a false no-progress halt costs one attempt early (the manual button remains), while under-normalizing wastes the 2nd attempt anyway, so over-matching is the cheaper error and the cap of 2 bounds both.

**(c) Two-layer code-vs-CI-config enforcement (Decision 4)** — because the agent's self-classification is not trustworthy (CI log tails are the most attacker-influenceable text uzi feeds an agent):
- *UX routing (agent-declared, plan time):* `buildCIFixPlanPrompt` gains a `CI_CONFIG_MARKER` sentinel (mirrors the existing `NOT_CODE_MARKER`). When the fix plan edits the pipeline definition, the agent marks the plan. **`runner.gatePlan` (agent-side, M5) parses `CI_CONFIG_MARKER` from the `planMd` it already receives** via a shared `isCIConfigPlan(planMd)` helper; for a CI-config-classified ci_fix plan it does **not** take the `auto_approve` short-circuit — it parks at `awaiting_approval` and sets a `humanApproved` closure variable that `execute()` reads at push time. This deliberately avoids touching `executor.ts`/`RunResult` (which does not carry the plan text, so push-time re-derivation is impossible); the classification travels via the closure, keeping M3 (prompt) and M5 (gate+guard) independently shippable.
- *Security backstop (worker-enforced, push time, load-bearing):* before the push, for a ci_fix run the worker computes `changedFiles` (already done for self-improve) and — **iff the run was auto-approved (`!humanApproved`) AND the diff touches a protected CI-config path** — refuses to push and fails closed (mark failed + issue comment + notify). Two correctness pins: the matcher **reuses the existing dotfile-capable regex path guard** (`self-improve.ts` `GUARD_CRITICAL_PATTERNS` / `flagGuardPaths`), NOT a new glob dependency (the default paths are all leading-dot files that a stock glob matcher misses by default — a fail-open trap); and a `changedFiles === null` (diff failure) result **fails the push closed** (self-improve treats null as informational and still pushes — the security guard must not). A human-approved run is never blocked (a human was in the loop, exactly as in today's manual flow). Rejected: a second post-implementation gate — the plan-time declaration + the push-time fail-closed diff guard together make it unnecessary.

**(d) The protected-path set is server-produced and includes the real pipeline file (Decision 4 hole-close).** GitLab lets a project point its pipeline at an arbitrary file (`ci_config_path`, even in another repo). So the static globs alone are not a true boundary: an agent editing the *configured* pipeline file would slip them. The server therefore **fetches the project's configured `ci_config_path`** (a new forge read) and delivers it, merged with the `CI_AUTOFIX_CONFIG_PATHS` defaults, in the claim's `CIConfigPaths`. The **server owns the boundary** (consistent with every other cap) and the field is produced **before** the worker consumes it (see Milestones — this closes the B1 fail-open). Residual, documented: `include: local:` YAML templates pulled into the pipeline are NOT scanned in MVP (a documented residual, like PRD #6's third-party-secrets residual), because it means parsing YAML the agent may be mid-edit; the configured top-level `ci_config_path` is the common case and is covered.

**(e) The snapshot builder is shared by handler + poller.** Move `snapshotFailedPipeline` + `scrubKnownTokens` out of `handler` into `workersvc` (`workersvc/ci_fix_snapshot.go`) as `BuildFailureSnapshot(...)`, and add `FailureSignature` next to it. (`workersvc` already imports `api/internal/forge`, so there is no new import to add and the high→low layering the earlier draft worried about already exists.) Handler and the poller detector both call it.

### Trigger + guard sequence

```mermaid
sequenceDiagram
  participant Tick as poller syncRepo
  participant PS as forgesvc.SyncPipelines
  participant D as CIAutoFix.detect
  participant Q as store (ci_autofix_attempts)
  participant WS as workersvc.CreateAutoCIFixRun
  participant F as forge (issue comment / snapshot / ci_config_path)

  Tick->>PS: refresh pipeline_statuses (+ stamp fix_verdict; verified → notify + delete ledger)
  Tick->>D: detect(repo, forge)
  D->>Q: ListCIAutofixCandidateRefs (failed + owner opted-in + token, excl. default/protected)
  loop each candidate ref (newest failed pipeline P_new)
    D->>Q: GetCIAutofixAttempt(repo, ref)
    alt P_new.id == last_pipeline_id
      D-->>D: already handled, skip
    else active ci_fix run exists (uq index)
      D->>Q: swallow — record last_pipeline_id ONLY IF <= active run's target id (M-b)
    else count >= cap OR (count>=1 AND signature==last_signature)
      alt not yet halt_notified
        D->>F: issue comment (halt reason + last-run link) + notify
        D->>Q: set halt_notified=true, record last_pipeline_id
      else already notified
        D->>Q: record last_pipeline_id (silent)
      end
    else proceed
      D->>F: fetch ci_config_path; BuildFailureSnapshot(P_new); FailureSignature
      D->>F: issue comment (auto-fix started) + notify
      D->>WS: CreateAutoCIFixRun(owner, repo, ref, snapshot)  %% auto_approve=true, CIConfigPaths
      WS-->>D: run (or ErrActiveFixExists/ErrBranchInUse → swallow)
      D->>Q: upsert {count+1, last_signature, last_pipeline_id=P_new.id}
    end
  end
```

## Data model

Goose numbers are DRAFTS; renumber to the next free numbers above the **live head** at merge time, per the CLAUDE.md convention. Do not trust a number written here — the live head keeps advancing: it was `00066` when this PRD was drafted and `00113` as of 2026-08-10, so read `api/internal/store/migrations/` at merge.

`NNNN_user_ci_autofix_enabled.sql` — mirror `00061`:
```sql
-- +goose Up
ALTER TABLE users ADD COLUMN ci_autofix_enabled BOOLEAN NOT NULL DEFAULT false;
-- +goose Down
ALTER TABLE users DROP COLUMN ci_autofix_enabled;
```

`NNNN_ci_autofix_attempts.sql` — mirror `autopilot_triggers` (`00038`):
```sql
-- +goose Up
CREATE TABLE ci_autofix_attempts (
    repo_id            uuid   NOT NULL REFERENCES repos ON DELETE CASCADE,
    ref                text   NOT NULL,
    attempt_count      int    NOT NULL DEFAULT 0,   -- AUTO attempts only (cap default 2)
    last_signature     text,                        -- failure signature the last attempt targeted
    last_pipeline_id   bigint,                      -- dedup: pipeline already evaluated
    halt_notified      boolean NOT NULL DEFAULT false, -- comment-once latch after the cap/no-progress halt
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, ref)
);
-- +goose Down
DROP TABLE ci_autofix_attempts;
```

New sqlc queries:
- `users.sql`: `SetUserCIAutofixEnabled` (mirror `SetUserJudgeEnabled`).
- `ci_autofix.sql` (new):
  - `ListCIAutofixCandidateRefs` — newest run per branch (DISTINCT ON, like `pipeline_statuses.sql`) JOIN `pipeline_statuses ps ON ps.ref = branch AND ps.status='failed'` JOIN `users u ON u.id = run.user_id` WHERE `u.ci_autofix_enabled AND u.anthropic_token_ciphertext IS NOT NULL AND run.mr_iid IS NOT NULL AND branch <> @default_branch AND branch NOT IN (protected refs)`. Returns `ref, mr_iid, user_id, pipeline_id, pipeline_web_url, sha`. **The `runs` table now carries six kinds (`issue`, `ci_fix`, `chat`, `judge`, `self_improve`, `prompt`), so this query must be run-kind-aware — select only `ci_fix`-eligible agent MR refs and never match a `chat`/`judge`/`self_improve`/`prompt` run sharing the ref space.**
  - `GetCIAutofixAttempt`, `UpsertCIAutofixAttempt` (increment + set signature/pipeline id), `RecordCIAutofixPipeline` (record `last_pipeline_id` only — the swallow/halt paths, capped to the active run's target per M-b), `SetCIAutofixHaltNotified`, `DeleteCIAutofixAttempt` (reset-on-green + eviction cleanup).

## Touchpoints

**Config (new env, `config.go`)** — all land in M2 (server side) so they exist before the M5 consumer:
- `CI_AUTOFIX_MAX_ATTEMPTS` (default 2) — the per-ref auto cap.
- `CI_AUTOFIX_CONFIG_PATHS` (default `.gitlab-ci.yml,.gitlab/**,**/*.gitlab-ci.yml`) — the static CI-config protected-path defaults, **merged server-side with the project's fetched `ci_config_path`** and delivered in the claim. Matched by the reused `self-improve` regex guard (dotfile-aware), never a raw glob.
- No new global on/off setting in MVP: the detector is optional-wired (`SetCIAutoFix`), so *not wiring it* is the instance kill-switch (the autopilot nil pattern), and it can only run where `CI_WATCH_MAX_REFS>0`. A global `app_settings.ci_autofix_enabled` (full judge parity) is deferred — see Decision Log Q4.

**Forge**
- New read `ProjectCIConfigPath(ctx, projectID) (string, error)` on `forge.Forge` (GET project → `ci_config_path`; empty means the default `.gitlab-ci.yml`). Same neutral-type + redaction discipline as existing methods. **The forge layer now has THREE production drivers — `gitlab.go`, `forgejo.go`, `github.go` (github landed 2026-08-08) — plus five test fakes that implement the interface method-by-method: `forgesvc/sync_test.go`, `handler/forge_test.go`, `poller/autopilot_test.go`, `privcheck/checker_test.go`, `seed/seed_test.go` (`handler/ci_fix_snapshot_test.go` inherits via embedding, no edit). Adding this method to the interface is compile-time mandatory in all three drivers + the five fakes** — Forgejo/GitHub may be minimal reads or stubs, only runtime validation stays GitLab-only. **It is a SERVER-side read at claim assembly (Decision (d), server-owned boundary) — do NOT implement it as a PRD #158-style worker forge route (`worker_forge.go`/`apitypes/forge.go`); that is the opposite, agent-facing trust direction.**
- The halt / start / landed comment reuses the existing `CreateIssueNote` on the backing `agent/issue-N` issue (no new note method — every auto candidate has an issue). Documented here so the coder does not add `CreateMergeRequestNote`.

**Claim + agent contract** (the wire field is pinned across sides with a cross-side contract test in M5 — the PRD #6 M1+M2 lenient-fakes lesson):
- `ClaimConfig.CIConfigPaths []string` (`workersvc/claim.go`) — **server-produced in M2**, consumed by the M5 guard. Populated at run-creation with the config defaults + the project's `ci_config_path`.
- `buildCIFixPlanPrompt` (`agent/src/prompt.ts`, M3): a `CI_CONFIG_MARKER` first-line sentinel + a shared `isCIConfigPlan(planMd)` helper; **prompt-only**, no `executor.ts` change.
- `runner.gatePlan` (`agent/src/runner.ts`, M5): for `kind==="ci_fix"` + `isCIConfigPlan`, skip the `autoApprove` short-circuit (park); set the `humanApproved` closure var.
- `runner.ts` pre-push (M5): for `kind==="ci_fix"`, compute `changedFiles`; if `!humanApproved` and (any path matches `CIConfigPaths` **or** `changedFiles === null`) → skip `pushBranch`, report `failed`, emit the reason.

**workersvc**
- `CreateAutoCIFixRun(...)` — parallel to `CreateCIFixRun` but sets `auto_approve=true`; same one-active-fix index + cross-kind branch guards + typed errors (`ErrActiveFixExists`/`ErrBranchInUse`). **It cannot simply mirror the `CreateAutopilotRun` split:** `CreateAutopilotRun` is a thin wrapper over the shared `createRun(..., autoApprove=true, ...)`, whereas `CreateCIFixRun` uses a dedicated `ci_fix.sql` INSERT that does **not** parametrize `auto_approve` (a manual fix defaults to `false`). The auto variant must add an `auto_approve` param (or a new query) that sets `runs.auto_approve=true` explicitly.
- `BuildFailureSnapshot`, `scrubKnownTokens`, `FailureSignature` (moved/added).

**Notifications** (M4 + M6): a `notifysvc` collaborator is wired into `poller/ci_autofix.go` (start + halt) and into the `pipeline_sync` verified-stamp path (landed). Each event enqueues a PRD #46 in-app notification + the issue comment + Slack best-effort.

**DTO + web**
- `handler.go` user DTO: `ci_autofix_enabled bool`.
- `web`: `Settings.tsx` toggle (mirror the judge block), `AdminUsers.tsx` admin force-toggle, `api.ts` `setCIAutofixEnabled`, `mocks/mockApi.ts` + `mocks/data.ts` field.

## Milestones

Phase 1 (parallel, disjoint files) → Phase 2 (depends on Phase 1) → Phase 3 (integration) → Phase 4 (tests/docs).

| M | Title | Area | Depends | Phase |
|---|-------|------|---------|-------|
| M1 | Per-user `ci_autofix_enabled` toggle (vertical slice) | migration, `users.sql`, `handler/ci_autofix_toggle.go`, DTO, routes, web `Settings.tsx`/`AdminUsers.tsx`/`api.ts`/mocks | — | P1 |
| M2 | Server guard plumbing + shared snapshot/signature | `workersvc/ci_fix_snapshot.go` (moved), `handler/ci_fix.go` (call shared), **`ClaimConfig.CIConfigPaths` producer**, `config.go` (`CI_AUTOFIX_*`), forge `ProjectCIConfigPath` (all 3 drivers + 5 fakes) | — | P1 |
| M3 | Agent CI-config classification (prompt-only) | `agent/src/prompt.ts` (`CI_CONFIG_MARKER` + `isCIConfigPlan`) | — | P1 |
| M4 | Attempts ledger + loop guard + `CreateAutoCIFixRun` + reset/notify-on-green | migration, `ci_autofix.sql`, `workersvc/ci_fix.go`, `forgesvc/pipeline_sync.go` (green → `DeleteCIAutofixAttempt` + landed-notify) | M2 | P2 |
| M5 | Worker CI-config guard + gate override + cross-side contract test | `agent/src/runner.ts` (gatePlan parse + fail-closed push guard reusing `flagGuardPaths`), `agent/src/config.ts`, contract test that the server `CIConfigPaths` reaches + drives the guard | M2, M3 | P2 |
| M6 | `CIAutoFix` poller detector + notifications wiring | `poller/ci_autofix.go` (new), `poller/poller.go`, `cmd/api/main.go`, `notifysvc`/`slacksvc` + `CreateIssueNote` on start/halt | M2, M4 | P3 |
| M7 | Tests + docs + spec feed | integration/e2e, `docs/*.md`, `specs/` via spec-keeper | M1–M6 | P4 |

Dependency graph: `M1, M2, M3` parallel → `M4` (needs M2) and `M5` (needs M2 for the `CIConfigPaths` field + M3 for the marker) → `M6` (needs M2, M4; reads M1's flag) → `M7`. The load-bearing guard's **server producer (M2) lands before its consumer (M5)**, and M5 carries the cross-side contract test — closing the B1 fail-open.

Per-milestone acceptance (mechanical):
- **M1**: `PUT /api/me/ci-autofix` flips the flag (session identity only, never body-supplied target); DTO carries `ci_autofix_enabled`; web toggle reflects + persists; default false; admin force-toggle mirrors judge.
- **M2**: `handler.CreateCIFixRun` still produces a byte-identical snapshot (no behavior change); `FailureSignature` stable across volatile-token variation, distinct across different failing jobs; `ProjectCIConfigPath` returns the configured path (and `.gitlab-ci.yml` default when unset); `ClaimConfig.CIConfigPaths` is populated with defaults ∪ configured path.
- **M3**: a plan editing the pipeline file emits `CI_CONFIG_MARKER`; a code-only plan does not; `isCIConfigPlan` parses both.
- **M4**: cap blocks the 3rd auto attempt; `signature==last_signature` blocks the 2nd; a `success` pipeline deletes the ledger row + notifies landed; the swallow does NOT advance `last_pipeline_id` past the active run's target (M-b); `halt_notified` latches one comment; `CreateAutoCIFixRun` sets `auto_approve=true` and hits the same 409s as the manual path.
- **M5**: an auto-approved ci_fix run editing a CI-config path (incl. a `.`-leading path AND the project's configured `ci_config_path`) does NOT push and reports `failed`; `changedFiles===null` also refuses the push; a human-approved run editing CI config DOES push; a code-only auto run pushes. The contract test proves the server's `CIConfigPaths` actually reaches the guard.
- **M6**: a repo with a failed agent-branch pipeline + opted-in owner → one tick creates exactly one auto ci_fix run + a start notification; a 2nd tick on the same pipeline id creates none; an active manual fix on the ref makes the tick a no-op; a halt posts exactly one issue comment (latch).
- **M7**: integration covers race-with-manual, own-push loop, no-progress halt, cap halt, CI-config gate (declared + backstop + configured-path + dotfile + null-diff) paths, and the start/halt/landed notifications; docs + specs updated.

## Success Criteria

- With `ci_autofix_enabled` on, a failed pipeline on an agent-owned MR ref auto-queues exactly one `ci_fix` run within one poll interval, with no human click, and the owner gets a start notification. With the flag off (the default), behavior is identical to today (manual button only).
- `main` and protected branches are never the target of an auto-fix: candidate selection excludes the default branch and protected refs and requires `mr_iid`; the four `main`-untouched guardrail layers are unchanged, proven by re-running the guardrail suite for auto `ci_fix` runs.
- A persistently-failing pipeline stops after at most `CI_AUTOFIX_MAX_ATTEMPTS` auto attempts, and earlier if the failure signature does not change; on stop the owner gets exactly one issue comment + notification and no further auto attempts fire. No infinite loop and no silent stop is reachable, including when the fix's own push re-triggers the watch and when a poll tick lands mid-push (M-b).
- A code fix pushes without a gate; a CI-config fix reaches `awaiting_approval` (declared path) and, if an auto-approved run edits any protected CI-config path — including the project's configured `ci_config_path` and leading-dot files — or the diff cannot be computed, the push is refused and the run fails closed. Proven by test, with the server-produced `CIConfigPaths` covered by a cross-side contract test.
- The signature/attempt ledger is durable (survives run eviction and pipeline-cache churn) and resets only on a green pipeline or ref eviction, never on fresh human breakage.
- Snapshots remain bounded and treated as data, carrying none of uzi's own credentials — the PRD #6 redaction discipline is inherited unchanged by the shared builder.

## Risks / edge cases

- **Race with the manual Fix CI button.** Both insert through the same `uq_runs_one_active_ci_fix` index + `CountActiveRunsWithBranch`. The loser gets `ErrActiveFixExists`/`ErrBranchInUse`; the detector **swallows**, exactly as autopilot swallows an active run. The index is the arbiter; no new locking.
- **The fix's own push re-triggering the watch, and a poll tick landing mid-push (M-b).** The fix push yields a new pipeline id; success deletes the ledger row (no loop), failure is attempt N+1 bounded by the cap + no-progress signature. The swallow path (a tick landing while the fix run is still active) records `last_pipeline_id` **only up to the active run's own target pipeline id** — it must not advance past it, or attempt #2 and its halt comment would be silently skipped once the run terminates. Explicitly tested.
- **Attempt-counter reset.** Only a green pipeline (or ref eviction) resets `attempt_count`; a human pushing new broken code does NOT reset (prevents an infinite auto-loop on repeated human breakage). The manual button is the always-available escape hatch after the cap (it never reads `ci_autofix_attempts`). Ledger eviction happens on the reconcile tick only, when the ref leaves the watch set — so a reused `agent/issue-N` branch does not inherit a stale `count=2` after the old ref ages out.
- **CI-config boundary completeness.** The guard covers the static defaults + the project's configured `ci_config_path`. Residual (documented): `include: local:` templates pulled into the pipeline are not scanned in MVP. The push guard reuses the proven dotfile-capable regex matcher, so the leading-dot defaults are actually caught (not silently missed by a stock glob).
- **Approval-gate path vs the cap.** A CI-config-classified plan parks at `awaiting_approval`; that attempt has consumed a slot. If never approved it times out → `failed`, leaving the branch free and the count incremented — so an ignored CI-config auto-fix still respects the cap.
- **Log-tail truncation / log-borne prompt injection.** Unchanged from PRD #6: signature and agent see the same bounded tail; logs are untrusted data framed as quoted evidence; guardrails do not trust the model; the shared snapshot builder inherits the redaction pass and size caps.
- **Auth/ownership.** The ref's owning user = the newest run's `user_id` on that branch; that user's flag + Anthropic token govern and pay. Never main/protected by construction.
- **Manual fix does not seed the auto no-progress signature.** After a manual fix, the *next* auto attempt cannot early-halt on an unchanged signature (the manual path does not write `ci_autofix_attempts`); the cap still bounds it. Accepted for MVP — wiring the manual path into the ledger is a possible follow-up, not a correctness requirement.

## Out of Scope (deferred)

- Auto-fixing `main` / protected-branch pipelines (Decision 1 — higher blast radius; a later phase, only through a gate).
- Global `app_settings` kill-switch beyond the optional-wiring instance switch (Decision Log Q4).
- Scanning `include: local:` templates for the CI-config guard (Decision Log M-a residual).
- `CreateMergeRequestNote` — comments ride the backing issue note in MVP (Decision Log M-d).
- Webhook-driven trigger (still poll-based, inherited from PRD #6).
- Auto-*merge* of a verified fix (humans merge, unchanged); pipeline retry/cancel from uzi (a forge write needing primary-directive review).
- Cross-forge **runtime** parity (Forgejo/GitHub) — GitLab first; the detector is forge-neutral by construction but validated on GitLab only. Note the new `ProjectCIConfigPath` interface method is nonetheless **compile-time mandatory** on all three drivers + the five test fakes (see Touchpoints); only its live validation is GitLab-only.

## Decision Log

- **2026-07-17 (user)**: create a PRD for automatic CI-fix. Four decisions locked via structured choice: (1) trigger scope = agent-owned MR refs only, never `main`; (2) loop guard = cap (2) + no-progress halt on unchanged failure signature, then mark-failed + comment + notify + no re-queue; (3) toggle = per-user `ci_autofix_enabled`, default OFF, mirroring `judge_enabled`/`autopilot_enabled`; (4) fix authority = code fixes auto, CI-config edits gated by approval. Stated philosophy: "usually we prefer to fix code, not CI itself, but if CI is really at fault we can add a CI fix in the MR — we fix whatever should be fixed." Confirmed uzi/bot can already see CI details (failed jobs + bounded log tails via the PRD #6 `FailureSnapshot`).
- **2026-07-17 (AI, architect design pass)**: chosen architecture = a poller post-sync `CIAutoFix` detector (autopilot sibling); durable `ci_autofix_attempts` ledger; `FailureSignature` normalized-and-biased-toward-same; two-layer code-vs-CI-config enforcement; shared snapshot builder moved into `workersvc`. Full rationale + rejected alternatives in the design doc. No guardrail layer weakened; all changes additive.
- **2026-07-17 (AI defaults for the architect's original 6 open questions)**: Q1 admin toggle = **judge parity** (self + admin force-disable). Q2 the manual Fix CI button does **not** count toward the auto cap. Q3 CI-config boundary = narrow default glob, configurable, **plus the project's configured `ci_config_path`** (strengthened by the review — see M-a below). Q4 global kill-switch = **deferred** (optional wiring + per-user opt-in). Q5 halt = **no distinct "halted" run row**; the prior attempt is already `fix_failed` and the detector's comment + notification + ledger latch carry the "gave up" signal. Q6 notification = **issue comment + in-app + Slack best-effort**.
- **2026-08-10 (drift review — two-agent fact-check + research pass against HEAD `b94f5244`, release 0.26.0)**: architecture and all 14 building-block extension points re-verified **live** (manual `ci_fix` machinery, autopilot/judge patterns, `notifysvc`, `slacksvc`, `runner.gatePlan` + the auto-approve short-circuit, `flagGuardPaths`/`GUARD_CRITICAL_PATTERNS` already reused in `runner.ts`, pre-push `changedFiles`, `CreateIssueNote`, `pipeline_statuses`). Staleness folded into the sections above: (1) migration head **00066 → 00113**; (2) the forge layer gained a **`github.go`** driver (2026-08-08), so `ProjectCIConfigPath` is compile-time mandatory across **three** drivers + **five** explicit fakes; (3) the notification package is **`notifysvc`** (three spots had a typo, now corrected); (4) **PRD #70 (status favicon) has since merged** (MR !66, now `prds/done/70-status-favicon.md`) — the 2026-07-17 "favicon unmerged" wording note is obsolete, though its conclusion (wire the PRD #46 `notifysvc`, not the favicon) stands; (5) **PRD #158 (worker-mediated forge reads) landed 2026-08-10** — `ProjectCIConfigPath` stays a server-side claim-assembly read (Decision (d)), NOT a worker route; (6) `CreateCIFixRun` uses a dedicated no-`auto_approve` query, so the auto variant needs an explicit param; (7) `workersvc` already imports `forge`; (8) the `runs` table now carries six kinds, so `ListCIAutofixCandidateRefs` must be kind-aware; (9) the run-lane executor is now `agent/src/sdk-executor.ts` (M3's prompt consumption at `sdk-executor.ts`), though `gatePlan` remains in `runner.ts` — confirm both file references during M3/M5. The 2026-07-17 entries below are left as the dated record of what was true then (head 00066 and the unmerged favicon were both accurate on that date).
- **2026-07-17 (Opus review pass — architecture + fact-check; the Fable review the user requested could not run, account lacks Fable usage credits, so Opus was substituted with the user's approval)**: **Fact-check** — 10/10 load-bearing code claims Confirmed (auto_approve semantics, ci_fix machinery + indexes, verdict stamping, autopilot ledger/detector, judge toggle pattern, `NOT_CODE_MARKER`, `slacksvc`, `notifications`, migration head 00066, pre-push `changedFiles`); one wording nit fixed (the status favicon is PRD #70/unmerged, so the notification reuse is stated against the PRD #46 `notifications` system directly, not "the favicon already reads"). **Architecture** — loop-guard core, no-permanent-wedge, and the four guardrail layers verified **sound**; two fail-open holes and an under-scoped notification story fixed into this revision: **B1** the guard's server-side `CIConfigPaths` producer + config default moved to M2 (before the M5 consumer) with a cross-side contract test in M5; **B2** the push matcher pinned to the existing dotfile-capable `flagGuardPaths` regex (a stock glob would miss the leading-dot defaults); **M-a** (user gate: **fetch real path + doc includes**) the server now fetches the project's `ci_config_path` into `CIConfigPaths`, with `include: local:` a documented residual; **M-b** the active-run swallow must not advance `last_pipeline_id` past the active run's target (else attempt #2 + its halt notification silently drop); **M-c** classification parsed in `gatePlan` from `planMd` via a `humanApproved` closure, avoiding an `executor.ts`/`RunResult` contract change; **minors** — `changedFiles===null` fails the push closed, a `halt_notified` comment-once latch, protected refs excluded at selection not just push, ledger eviction pinned to the reconcile tick, and the "manual seeds the signature" over-claim dropped. **User gates (2026-07-17)**: M-a = fetch `ci_config_path` + document includes as residual; halt/notify comment target = **reuse `CreateIssueNote` on the backing issue** (no new forge method); notification timing = **on start + halt + landed**.
