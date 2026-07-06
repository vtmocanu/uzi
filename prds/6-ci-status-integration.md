# PRD #6: CI Status Integration & CI-Fix Agent

**GitLab Issue**: [vtmocanu/uzi#6](https://gitlab.example.com/vtmocanu/uzi/-/issues/6)
**Status**: Draft
**Priority**: Medium
**Created**: 2026-07-04
**Depends on**: PRD #2 (forge integration + poller, done) for the **display half** (M1–M3; it also *reads* the `runs` table from PRD #4 M1, which is already merged — no dependency on PRD #4's pending milestones, so it can run in parallel with PRD #4 M3–M7); PRD #4 **M3 (SDK executor), M4 (plan gate + MR flow), M5 (run view UI)** for the **fix-agent half** (M4–M7 here). The two halves are phased accordingly — see "Phasing & parallel-safety".

## Problem

plan.md line 52: "integration with gitlab to check and display CI status, if it is broken spin up an agent to review what happened and if it can fix it - if the code was bad => uzi verifies it's work". Today uzi shows a kanban of PRD issues and (once PRD #4 lands) live agent runs ending in MRs — but pipelines are invisible. A red pipeline on `main` goes unnoticed until someone opens GitLab; an agent's MR with failing CI looks "done" in uzi while being unmergeable; and fixing broken CI is entirely manual. For a system whose product *is* merged MRs, CI state is the missing half of the feedback loop.

## Solution Overview

Two halves along the dependency boundary:

1. **CI status display (server + web, PRD #2 machinery only)** — the poller tick additionally syncs the latest pipeline per *watched ref* (repo default branch + branches of that repo's agent runs) into a `pipeline_statuses` cache table; the repos list, board header, and issue cards render live status badges linking to the GitLab pipeline. Forge stays the source of truth; uzi caches, never invents.
2. **CI-fix agent (rides PRD #4's run machinery)** — a failed watched pipeline gets a "Fix CI" affordance that queues a run of a new kind `ci_fix`: the claim carries a snapshot of the failed jobs + truncated logs, the lead agent diagnoses (plan gate: root cause + proposed fix, or an explicit "not a code problem" verdict), fixes on a branch, and the **worker** pushes + opens/updates the MR exactly as in PRD #4. **uzi verifies the fix**: the pipeline sync watches the fix branch and stamps the run `verified` when its new pipeline passes (or `fix_failed` when it doesn't) — plan.md's "uzi verifies it's work", mechanically.

### Inspiration check (per plan.md, audited 2026-07-04)

| Concern | bottega does | multica does | dot-agent-deck does | uzi will do |
|---|---|---|---|---|
| CI visibility | Per-task PR `ciStatus` fetched on demand (`prService.getCIStatusWithDetails`) — no board-wide view | **Server-side aggregated `checks_conclusion` per PR head SHA** from GitHub check_suite webhooks (passed/failed/pending + counts) | Nothing | Poll-based per-ref latest-pipeline cache (webhooks deferred — the compose laptop can't receive them; see Risks), surfaced repo-wide + per-card |
| CI-failure reaction | PR-feedback agent addresses review comments; CI failure details feed the PR agent's context | Generic **webhook→autopilot run** dispatcher (any provider event can trigger an agent) — powerful but auto-fires with no human gate | Nothing | Manual "Fix CI" trigger into the existing run queue, **plan-gated like every run** (auto-trigger deferred — cost + primary-directive caution; multica's auto-fire with no gate is the weakness to avoid) |
| Fix verification | Human reads the PR | None | N/A | Pipeline sync closes the loop: fix run stamped `verified`/`fix_failed` from the post-fix pipeline — none of the three has this |

## Technical Design

### Forge interface additions (`api/internal/forge/forge.go`, GitLab driver `gitlab.go` — same neutral-domain-type + redaction discipline as the existing methods)

```go
// LatestPipeline returns the newest *branch* pipeline for a ref, or ErrNoPipeline.
// GitLab: GET /projects/:id/pipelines?ref=<ref>&per_page=1 (default order_by=id desc).
LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error)
// LatestMRPipeline returns the newest pipeline attached to a merge request —
// this is what catches detached MR pipelines (refs/merge-requests/:iid/head)
// and merged-results pipelines, which never appear under the source-branch ref.
// GitLab: GET /projects/:id/merge_requests/:iid/pipelines (newest first).
LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error)
// ListPipelineJobs returns the pipeline's jobs with status/stage/name.
// GitLab: GET /projects/:id/pipelines/:pipeline_id/jobs (scope filter client-side).
ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error)
// JobLogTail returns at most maxBytes from the END of a job's trace (failures
// conclude logs). GitLab: GET /projects/:id/jobs/:job_id/trace — the endpoint
// has NO range/tail parameter, so this is a full download truncated client-side;
// acceptable because it runs only at fix-trigger time, never on the poll tick.
JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error)
```

`Pipeline{ID, Ref, SHA, Status, WebURL, CreatedAt, UpdatedAt}` — `Status` normalized to the GitLab set (`created|waiting_for_resource|preparing|pending|running|success|failed|canceled|skipped|manual|scheduled`); the UI collapses these to five tones (see Web). `Job{ID, Name, Stage, Status, WebURL}`. Repos with no CI configured simply never have a pipeline for the ref — `ErrNoPipeline` maps to "no CI" in the cache, not an error.

**Pipeline-source honesty**: branch-ref filtering only sees *branch* pipelines. Projects configured MR-only (`rules: if $CI_PIPELINE_SOURCE == "merge_request_event"`) run detached pipelines on `refs/merge-requests/:iid/head`, invisible to `LatestPipeline(branch)`. So the sync uses **`LatestMRPipeline` whenever the watched ref belongs to a run with an `mr_iid`** (which is every pushed run branch — PRD #4's worker pushes and opens the MR in the same step) and `LatestPipeline` only for the default branch. Default branches of MR-only projects will honestly show "no CI" — documented, not fabricated.

### Pipeline status sync + persistence (migration drafted as `00040`+ — final number assigned at merge time, next free above the live head, per the CLAUDE.md convention)

```sql
pipeline_statuses (
  id bigserial PK, repo_id uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
  ref text NOT NULL,                -- default branch, or an agent run branch
  pipeline_id bigint NOT NULL, sha text NOT NULL,
  status text NOT NULL, web_url text NOT NULL,
  forge_updated_at timestamptz, synced_at timestamptz NOT NULL,
  UNIQUE(repo_id, ref)              -- latest-per-ref cache, upsert on sync
)
```

**Watched refs per enabled repo** = the repo's `default_branch` (via `LatestPipeline`) + the `runs.branch` of that repo's runs that are non-terminal **or** terminal with an MR and finished within `CI_WATCH_RUN_WINDOW` (default `14d` — long enough to cover review cycles, bounded so dead branches age out), each queried via `LatestMRPipeline(run.mr_iid)` when the run has an MR, else `LatestPipeline(branch)`; capped at `CI_WATCH_MAX_REFS` (default 20, newest first; hitting the cap is logged, not silent). Sync happens on the existing poller tick (`poller.Engine.syncRepo` gains a pipeline step after issue sync — same bounded concurrency, same jitter; no second ticker, no new interval knob; the step is a new `forgesvc` method, `FullSync`/`IncrementalSync` stay issue-only). Cost: ≤ 1 + capped-refs GitLab calls per repo per tick; both list calls are cheap indexed queries GitLab-side (`JobLogTail` is *not* called on the tick). Refs no longer watched are evicted from the cache on the reconcile tick (mirrors the issues-cache eviction discipline). Note the read dependency: watched-ref computation reads the `runs` table — a PRD #4 M1 artifact, already merged, so Phase 1 needs no pending PRD #4 work.

**API surfacing** (read paths only, shapes extend existing DTOs):
- `repoDTO` gains `pipeline: {status, web_url, pipeline_id, synced_at} | null` (default branch) — this enriches both `GET /repos` (enabled-repos list) and the per-connection projects listing the Repos page actually renders (`listProjects`; non-enabled projects have no cache row and get `null`).
- `GET /repos/{id}/board` gains the same at board level, and each card whose issue has runs gains `pipeline` for the **most recent** run's branch (join `runs` → `pipeline_statuses` in `buildBoard`).

### CI-fix runs (schema evolution on PRD #4's `runs`, migration in the `00040`+ range)

```sql
ALTER TABLE runs
  ADD COLUMN kind text NOT NULL DEFAULT 'issue',        -- issue | ci_fix
  ALTER COLUMN issue_iid DROP NOT NULL,                 -- ci_fix on default branch has no issue
  ADD COLUMN pipeline_id bigint, ADD COLUMN pipeline_ref text,
  ADD COLUMN failure_snapshot jsonb,                    -- failed jobs + log tails, frozen at queue time
  ADD COLUMN fix_verdict text,                          -- NULL | verified | fix_failed | not_code
  ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'  AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix' AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL));
-- one active fix per failing ref (partial unique index, mirrors uq_runs_one_active_per_issue):
CREATE UNIQUE INDEX uq_runs_one_active_ci_fix ON runs(repo_id, pipeline_ref)
  WHERE kind = 'ci_fix' AND status NOT IN ('completed','failed','cancelled');
```

The existing one-active-run-per-issue index gets a `kind = 'issue'` predicate in the same migration — **defensive, not load-bearing** (ci_fix rows carry NULL `issue_iid`, and Postgres treats NULLs as distinct in unique indexes, so they could never collide anyway); the essential change is only `DROP NOT NULL`. Existing rows are untouched (`DEFAULT 'issue'` backfills).

**Rejected alternative** (decision log): auto-creating a GitLab issue per CI failure to reuse the issue-run path unchanged. Rejected — it would spam the forge and the board with operational noise, and the PRD-link sanity check would be meaningless for these issues. A nullable `issue_iid` + CHECK is the honest shape.

**Trigger**: `POST /api/repos/{id}/ci-fix-runs` body `{ref}` (routes mount under `/api`, no `/v1` — same correction PRD #5 logged) — validates: pipeline cache shows `failed` for that ref, no active fix for the ref (409, backed by the partial index), **no active run of any kind whose `branch` equals the ref** (an issue run re-fired on `agent/issue-N` while a ci_fix is active there — or vice versa — would collide in the same worktree; the index pair can't express this cross-kind exclusion, so both `CreateRun` and this endpoint check it at trigger time, and git's "branch already checked out" failure remains the loud backstop for the race window), user has a connected worker + Anthropic token (same preconditions as `CreateRun`). At queue time the server snapshots the failure into `failure_snapshot`: pipeline id/sha/web_url + up to `CI_FIX_MAX_JOBS` (default 10) failed jobs, each with `JobLogTail(…, CI_FIX_LOG_TAIL_BYTES)` (default 32 KiB). Snapshot at queue time for the same reason PRD #4 snapshots issues: the pipeline cache row will be overwritten by newer runs of the same ref, and the run must stay self-contained. **Manual trigger only in MVP** — auto-spawn on failure is deferred (see Out of scope): it burns tokens with nobody watching, and multica's ungated autopilot is the audited weakness, not the pattern to copy. The plan gate keeps a human in the loop either way.

**Claim payload** gains `kind` and, for `ci_fix`, `pipeline: {id, ref, sha, web_url, failed_jobs: [{name, stage, web_url, log_tail}]}`. **State report** (worker→server) gains an optional `fix_verdict` field so a `not_code` outcome travels the wire on the `completed` report (`SetRunCompleted` extended accordingly) — both directions pinned before the worker milestone and covered by a cross-side contract test per the M1+M2 lesson (the outbound verdict is exactly the kind of field two lenient fakes would each invent differently).

### Worker: `ci_fix` workflow (extends PRD #4's executor, same guardrails verbatim)

- **Log tails are untrusted data, never instructions** — the exact discipline PRD #4's auditors mandated for issue bodies applies doubly here: CI logs echo arbitrary repo/dependency output. The lead prompt frames the snapshot as quoted evidence.
- **Flow**: worktree on the failing ref → lead agent diagnoses from snapshot + repo (may re-run the failing commands locally via Bash — tests, linters; it cannot touch the forge) → plan gate posts `awaiting_approval` with root cause + proposed fix **or** a `not_code` verdict (infra/flaky/secret/runner problem: run completes with the diagnosis as its result, `fix_verdict = 'not_code'`, no MR — approving a no-op costs nothing, and the diagnosis alone is the value) → on approval, implement ⇄ review loop as in PRD #4 →
  - **ref = default branch**: fix lands on new branch `ci-fix/pipeline-{id}`, worker pushes + opens an MR (description links the failing pipeline URL — there is no issue to link).
  - **ref = an agent run branch** (`agent/issue-N` with an open MR): fix commits land **on that same branch**, worker pushes, the existing MR updates — no second MR (bottega's PR-feedback shape).
- All PRD #4 guardrails hold unchanged: worker-held PAT (agent has no push credential), `PreToolUse` deny-hook, `bypassPermissions` + `disallowedTools`, `settingSources: []`, sparse env, iteration cap, watchdogs. The push targets above are non-protected branches, so no guardrail loosening is needed — a `ci_fix` run attempting `git push origin main` fails identically to an issue run.

### Verification loop ("uzi verifies it's work")

Verification keys on **`runs.branch`** (the fix branch), *not* `pipeline_ref` (the failed ref) — for a default-branch fix these differ (`pipeline_ref='main'`, `branch='ci-fix/pipeline-{id}'`), and keying on `pipeline_ref` would either never stamp or false-stamp from unrelated `main` commits. The fix branch is a watched ref by construction (its run is non-terminal, then recently-terminal-with-MR), observed via `LatestMRPipeline` of the fix run's MR — so detached/merged-results pipelines are caught too. When the sync sees a pipeline there with **id > the snapshot's pipeline id** (i.e. triggered by the fix push; this guard is also what disambiguates the agent-branch case where `branch == pipeline_ref`) reaching terminal status, it stamps: `success` → `fix_verdict = 'verified'`; `failed` → `'fix_failed'`. Stamp target selection (a branch like `agent/issue-N` can host several sequential fix runs over time): the ci_fix run with `branch = ref AND fix_verdict IS NULL AND snapshot pipeline id < observed id`, newest first. Stamping is a cheap column update inside the sync step — no new loop, no worker involvement. `fix_failed` is surfaced, not auto-retried: the user decides whether to fire another fix run (which gets the *new* failure snapshot). Runs whose fix branch stops being watched before a terminal pipeline (window expiry) keep `fix_verdict = NULL`, shown as "unverified" — honest, not fabricated.

### Web UI

1. **Pipeline badge component** — five tones mapping the GitLab status set: `passed` (success), `failed` (failed), `running` (created/waiting/preparing/pending/running/scheduled), `attention` (manual — someone must click in GitLab), `neutral` (canceled/skipped/no CI). Links to `web_url`, shows `synced_at` staleness on hover. Reuses `ui.tsx` `Badge` tones.
2. **Repos page** — badge per row (default branch).
3. **Board** — header badge (default branch); per-card badge where the card's run branch has a status; failed states render the **"Fix CI"** button (precondition-disabled with reason, mirroring "Start run").
4. **Fix-run view** — `/runs/:id` from PRD #4 M5 renders `ci_fix` runs as-is (same message stream, plan panel, stop, follow-up); header shows the failing pipeline link + verdict chip (`verified ✓` / `fix failed ✗` / `not a code problem` / `unverified`).
5. Responsive, like everything since PRD #2.

### Configuration (env)

`CI_WATCH_RUN_WINDOW` (14d), `CI_WATCH_MAX_REFS` (20), `CI_FIX_MAX_JOBS` (10), `CI_FIX_LOG_TAIL_BYTES` (32768). Pipeline sync rides `FORGE_POLL_INTERVAL` — no new interval. `CI_WATCH_MAX_REFS=0` disables pipeline sync entirely (and with it the badges + Fix CI), preserving today's behavior for operators who want it off.

### Phasing & parallel-safety

| Phase | Milestones | Depends on | Files touched | Overlap with in-flight work |
|---|---|---|---|---|
| 1 (start now, parallel with PRD #4 M3–M7 and PRD #5) | M1–M3 | PRD #2 (done) + read-only use of the `runs` table (PRD #4 M1, already merged) | `forge/forge.go`+`gitlab.go`, `poller/`, `forgesvc/`, migration `00040`, `handler/board.go`+`repos`, `web` Repos/Board | **`forge.go` is a three-way merge point**: PRD #4 M5 adds `CreateIssue`, PRD #5 M3 adds three introspection methods (plus an `IsAdmin` field on `VerifyToken`'s `BotIdentity`), this adds four pipeline methods — all pure additions, whoever lands later rebases trivially; flagged in all three PRDs. `Board.tsx`/`board.go` also gain PRD #4 M5 changes — coordinate at merge. |
| 2 (gated) | M4–M7 | PRD #4 M3+M4 (executor, plan gate, MR flow) shipped; M5 for UI reuse | `runs` migration, `workersvc/`, `handler/`, `agent/` executor, web run view | Same files PRD #4 M3–M5 create — **do not start until those merge**. |

## User Journey

1. A teammate merges something that breaks `main`. Within a poll tick the repo row and board header flip to a red `failed` badge. No one has to open GitLab to know.
2. The user clicks the badge's **Fix CI**. A `ci_fix` run queues; their worker claims it; the run view streams the lead agent reading the two failed jobs' log tails and reproducing the test failure locally.
3. The run gates: "Root cause: `TestFoo` broken by commit abc123 (nil guard removed). Fix: restore the guard + regression test." The user approves. The agent fixes, the reviewer subagent passes it, the worker pushes `ci-fix/pipeline-4242` and opens an MR linking the failing pipeline.
4. GitLab runs the MR pipeline; uzi's next ticks show `running` then `passed` on the fix branch, and the run's verdict chip flips to **verified ✓**. The user merges the MR in GitLab; `main`'s badge goes green.
5. A different failure is a runner disk-full. The plan gate says so: verdict `not a code problem`, diagnosis attached, no MR, no tokens wasted on an unfixable fix.
6. An agent's own MR (card in "In Review") shows a red per-card badge — the fix run lands its commits on the same `agent/issue-N` branch, and the existing MR simply updates.

## Milestones

- [x] **M1 — Forge: pipeline read methods**: `LatestPipeline` / `LatestMRPipeline` / `ListPipelineJobs` / `JobLogTail` (+ `Pipeline`/`Job` domain types, `ErrNoPipeline`), GitLab driver with client-side tail-truncation and redaction discipline, driver tests against recorded fixtures incl. no-CI, detached-MR-pipeline, and huge-trace cases.
- [ ] **M2 — Server: pipeline sync + surfacing**: migration `00040` (`pipeline_statuses`); watched-ref computation (default branch + run branches, window + cap); poller-tick integration with eviction on reconcile; `/repos` and board DTO enrichment; unit tests against the fake forge (status transitions, cap, eviction, no-CI).
- [ ] **M3 — Web: status badges**: badge component (5 tones + staleness), Repos rows, board header, per-card badges; responsive; component tests. *Phase 1 complete — user-visible value ships here.*
- [ ] **M4 — Server: ci_fix run type** *(gate: PRD #4 M3+M4 merged)*: runs migration (kind/nullable issue_iid/CHECK/partial indexes); failure snapshot capture; `POST /repos/{id}/ci-fix-runs` with precondition checks incl. the cross-kind same-branch exclusion; claim payload + `fix_verdict` state-report extension, both under a cross-side wire-contract test (per the PRD #4 M1+M2 lesson); verification stamping in the sync step keyed on `runs.branch` (verified/fix_failed, stamp-target selection rule); authz + constraint tests.
- [ ] **M5 — Worker: ci_fix workflow** *(gate: PRD #4 M3+M4 merged)*: kind-aware executor (diagnosis prompt with untrusted-log framing, `not_code` verdict path, default-branch vs run-branch fix targets, same-branch MR update); guardrail tests rerun for `ci_fix` runs (hostile log content attempting to steer a push to main must be denied); dummy credentials throughout (PRD #4 testing-credentials policy applies verbatim).
- [ ] **M6 — Web: Fix CI + verdicts** *(gate: PRD #4 M5 merged)*: Fix CI button with disabled-reasons; fix-run rendering in the run view (pipeline link, verdict chip); verified/fix_failed/not_code surfacing on card + run.
- [ ] **M7 — E2E + docs**: scripted scenario against the fake forge + stub executor (red main → fix run → plan gate → MR → verification stamp; red agent-MR → same-branch update; not_code path; constraint races); auditor pass on log-snapshot handling (prompt-injection framing, size caps, no secret leakage in snapshots — job logs can contain tokens teammates printed: **snapshot redaction check**); `docs/configuration.md` (4 env vars), ARCHITECTURE.md (pipeline sync + verification loop), README demo.

## Success Criteria

- A pipeline failure on any watched ref is visible in uzi within one poll interval, on the repo row, board header, and (for run branches) the card — each badge linking to the GitLab pipeline. `CI_WATCH_MAX_REFS=0` reproduces today's behavior bit-for-bit.
- From a red badge: one click → plan-gated fix run → approved → MR with the fix, and the verdict chip reaches `verified ✓` with zero manual pipeline-watching. For non-code failures the run ends with a useful diagnosis and no MR.
- The primary directive holds for `ci_fix` runs exactly as for issue runs: guardrail suite proves hostile log content cannot push to protected branches or leak credentials.
- Failure snapshots are bounded (jobs × tail bytes) and treated as data: no snapshot content is ever interpolated into system-prompt instructions.
- Wire contract between server and worker for `ci_fix` claims is covered by a cross-side contract test (not two lenient fakes).
- No new secret surface: pipeline/job reads use the existing per-user PAT lifecycle; snapshots and badges contain none of **uzi's own credentials** (PAT, join token, Anthropic token) nor known token patterns like `glpat-*` — redaction tests cover the four new driver methods and the snapshot path. Arbitrary third-party secrets printed by pipelines are the *documented residual risk* (see Risks), not a claim this criterion makes.

## Risks

- **Poll-based staleness**: badges lag up to `FORGE_POLL_INTERVAL` (1m default) + tick spread. Accepted for MVP; webhook ingress (multica's autopilot shape, HMAC-verified) is the later upgrade once uzi runs somewhere webhooks can reach (k8s).
- **API call growth**: watched refs multiply forge calls per tick. Bounded by `CI_WATCH_MAX_REFS` + the run window; the poller's existing concurrency cap and per-call timeout contain the blast; hitting the cap logs which refs were dropped.
- **Log-borne prompt injection**: CI logs are the most attacker-influenceable text uzi will ever feed an agent (dependencies, test output, PR content all echo into them). Mitigations are PRD #4's guardrail layers (which don't trust the model) + data-not-instructions framing + the M7 auditor pass focused exactly here.
- **Secrets in job logs**: teammates' pipelines may print tokens. Snapshots go through the redaction pass and are size-capped, but uzi cannot know every secret shape — the snapshot is stored server-side like any run message, visible only to the run's owner/admins (same authz as PRD #4 messages). Residual risk accepted and documented.
- **Verification false-positives**: a fix MR's pipeline can pass while the *merge result* still fails on main (semantic conflicts) — unless the project runs merged-results pipelines, which `LatestMRPipeline` does pick up. `verified` means "the fix MR's latest pipeline passed", stated as such in the UI; enabling merged-results pipelines is a GitLab-config concern, noted in docs.
- **Schema evolution on live `runs`**: relaxing `issue_iid` NOT NULL + re-scoping the partial unique index touches PRD #4's core table while PRD #4 is in flight. Mitigation: Phase 2 is hard-gated on PRD #4 M3+M4 being merged; the migration is written against the landed schema, not the PRD text.

## Out of scope (deferred)

Auto-spawning fix runs on failure (belongs with plan.md's auto-start-on-label family; needs spend controls + notification story first — manual trigger + plan gate is the MVP-honest version of "spin up an agent"); webhook-driven status (needs reachable ingress); pipeline *retry/cancel* from uzi (a forge write beyond MR creation — primary-directive review needed before any such write); job log full-view in uzi (badges link to GitLab); flaky-test detection/quarantine; MR merge from uzi (humans merge, unchanged); Slack notifications on CI failure (plan.md later-stuff); forgejo driver parity.

## Decision Log

- 2026-07-04 (user): create the CI-status PRD per plan.md line 52 — display pipeline status on cards/repo view + "spin agent to fix CI"; the fix-agent may depend on PRD #4 completion but the PRD must address that dependency; review the PRD with agents after drafting.
- 2026-07-04 (research, submodule audit): bottega fetches per-task PR `ciStatus` on demand and feeds failures to a PR-feedback agent (no board-wide view); multica aggregates `checks_conclusion` per PR head SHA from check_suite webhooks and has a generic webhook→autopilot run dispatcher (ungated auto-fire = the weakness); dot-agent-deck has no CI awareness. uzi takes multica's server-side aggregation idea (poll-based, webhooks deferred), bottega's failure-details-to-agent shape, and adds the verification stamp neither has.
- 2026-07-04 (AI, post-draft review wave — design reviewer + fact-checker, both against the landed code / GitLab docs / vendored inspirations; no blockers): **fact-check gap fixed** — detached MR pipelines run on `refs/merge-requests/:iid/head` and never appear under the source-branch ref (GitLab MR !25504; bites MR-only-pipeline projects hardest), so a fourth forge method `LatestMRPipeline` was added and run-branch status + verification now key on the run's MR, with default-branch-of-MR-only-projects honestly showing "no CI"; job-trace tailing acknowledged as full-download-then-client-truncate (the trace endpoint has no range parameter) and confined to fix-trigger time, never the poll tick. **Design should-fixes applied**: verification stamp keys on `runs.branch` not `pipeline_ref` (they differ for default-branch fixes) with an explicit stamp-target selection rule for branches hosting sequential fix runs; cross-kind same-branch exclusion added at trigger time in both create paths (the two partial unique indexes are disjoint and can't express it; worktree "already checked out" failure is the race backstop); `not_code`/`fix_verdict` outbound wire field pinned into the state report + contract test (the M1+M2 lenient-fakes lesson applies to worker→server too); trigger path corrected to `/api` (no `/v1`, same correction PRD #5 logged); snapshot-redaction success criterion scoped to uzi-known credentials + known patterns, third-party secrets stay documented residual risk. Nits: PRD #5 adds three methods + a `BotIdentity` field (not four methods); Phase 1's read dependency on the merged `runs` table acknowledged; Repos-page badge rides `repoDTO` so both `/repos` and the per-connection projects list get it; `uq_runs_one_active_per_issue` re-scope marked defensive-only (NULL `issue_iid` never collides); per-card badge pinned to the most recent run. All factual claims about GitLab APIs, client-go v2.44.0 capability, the status enum, the inspirations, and repo state were CONFIRMED by the fact-checker.
- 2026-07-04 (AI, defaults chosen while user AFK — **revisit on review**): manual Fix-CI trigger only, auto-spawn deferred (cost with nobody watching + multica's ungated-autopilot lesson; plan gate keeps the human in the loop either way); `ci_fix` as a run *kind* with nullable `issue_iid` + CHECK over auto-creating GitLab issues per failure (forge/board noise, meaningless PRD-link check); pipeline sync rides the existing poller tick, no second loop/interval; watched refs = default branch + windowed run branches, capped; failure snapshot frozen at queue time (self-containment, same reason PRD #4 snapshots issues); verification = passive stamp from the pipeline sync when the fix ref's new pipeline concludes (no worker involvement, no auto-retry); fix on the same branch when the failing ref is an agent MR branch (update the existing MR, don't stack a second one); log tails from the END of traces (failures conclude logs), 32 KiB × 10 jobs caps; migration range `00040+` reserved.
