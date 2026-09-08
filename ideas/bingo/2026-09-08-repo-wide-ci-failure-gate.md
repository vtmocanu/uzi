# Repo-wide CI failure gate — stop uzi auto-fixing every agent branch for a break that lives on `main`

Line references are against `3f030bbb`.

## Rationale

uzi reacts to a red pipeline in exactly two places, and **both reason one branch at a
time, with no notion of whether the repo's own baseline is green.** It caches the
default branch's pipeline in the same table, on the same tick, a few lines earlier —
and never reads it.

**What it caches.** `SyncPipelines` refreshes the default branch *first*, before any
run branch:

```go
// api/internal/forgesvc/pipeline_sync.go:61-67
// 1. Default branch. Branch pipelines only: an MR-only project's default branch
//    honestly shows "no CI" (documented, not fabricated) ...
if opts.DefaultBranch != "" {
    if s.syncOneRef(ctx, repoID, forgeProjectID, opts.DefaultBranch, pgtype.Int8{}, f) {
```

and the run branches after it (`:70-98`), into the one-row-per-`(repo, ref)` cache
`pipeline_statuses` (`api/internal/store/migrations/00042_pipeline_statuses.sql:13-24`,
`UNIQUE (repo_id, ref)` at `:23`). A query keyed exactly on that row already ships —
`ListDefaultBranchPipelineStatuses` (`api/internal/store/queries/pipeline_statuses.sql:29-38`,
`AND ps.ref = r.default_branch` at `:38`) — but its only production caller is the badge
handler (`api/internal/handler/pipeline_status.go:37`). The poller then runs
`SyncPipelines` (`api/internal/poller/poller.go:411`), `ciAutoFix.detect` (`:438`) and
`mrReviewWatch.detect` (`:450`) in that order, so at detector time the default-branch
row is *normally* at least as fresh as every branch row — but only when step 1's fetch
succeeds. When it does not, `syncOneRef` keeps the existing row *without* upserting
(`pipeline_sync.go:143-149`, `:164-166`; it returns `keep=true` to survive eviction and
retry next tick), so a later run-branch fetch that *does* succeed leaves the baseline
stale relative to the branch. `synced_at` — stamped `now()` only on a successful upsert
(`pipeline_statuses.sql:5`, `:15`) — is the witness that tells "refreshed this tick"
from "preserved from a prior tick" apart, and M1's gate below must read it. The
capability to read the baseline exists; no detector uses it.

**What neither detector does with it.** The autofix candidate query joins only the
*branch's own* pipeline row and mentions the default branch once, to exclude it:

```sql
-- api/internal/store/queries/ci_autofix.sql:76-81
JOIN pipeline_statuses ps
    ON ps.repo_id = @repo_id::uuid AND ps.ref = per_branch.branch
   AND ps.status IN ('failed', 'failure', 'error', 'timed_out', 'startup_failure')
JOIN repos rp ON rp.id = @repo_id::uuid
JOIN users u ON u.id = per_branch.user_id
WHERE per_branch.branch <> rp.default_branch
```

`ListMRReworkCandidates` has the same shape (`api/internal/store/queries/mr_rework.sql:68`,
the same `<> rp.default_branch` exclusion). `detectOne`'s six numbered steps — dedup
(`api/internal/poller/ci_autofix.go:168`), active-run swallow (`:173`), snapshot +
signature (`:205-220`), halt (`:222-225`), proceed (`:261-279`) — never read a
repo-level fact. Exhaustive check at HEAD:

```
$ grep -rn "DefaultBranch\|default_branch" api/internal/poller/*.go | grep -v _test
api/internal/poller/poller.go:408:		if r.DefaultBranch.Valid {
api/internal/poller/poller.go:409:			defaultBranch = r.DefaultBranch.String
api/internal/poller/poller.go:412:			DefaultBranch: defaultBranch,
$ grep -rn "pipelinestatus\." api/internal/poller/*.go | grep -v _test
api/internal/poller/mr_review_watch.go:172:	if !cand.PipelineStatus.Valid || !pipelinestatus.IsSuccess(cand.PipelineStatus.String) {
```

The default branch's *name* reaches the poller only to be handed to the sync; its
*status* is read nowhere in either detector.

**Why that costs real money, and pollutes MRs under review.** When CI is red for a
repo-wide reason, every agent MR branch reddens too, and uzi reads each one as that
branch's own fault. Each becomes an automatic `ci_fix` run that is **auto-approved and
skips the plan gate** (`ARCHITECTURE.md:130`), **on by default for every user** since
PRD #914 (`docs/ci-autofix.md:14-15`, and the SQL's own `u.ci_autofix_enabled IS NOT
FALSE` at `ci_autofix.sql:82`), up to `CI_AUTOFIX_MAX_ATTEMPTS` per branch
(`docs/ci-autofix.md:78`; default 2, `docs/configuration.md:169`). Ten open agent MRs
is up to twenty unattended agent runs, each posting a start comment and a halt comment
on the backing issue (`ci_autofix.go:293`, `:247`) and each pushing a commit onto a
branch a human is mid-review on.

This repo's own release runbook documents the failure class first-hand, and says the
fix is never per-branch:

> "The `vulncheck:{api,controller,web}` steps ... re-evaluate against the LIVE advisory
> DB, so a branch green when cut goes red when a new CVE lands, with no code change,
> and `main` fails the same way. ... If it's repo-wide (new CVE, toolchain currency),
> the fix is a dependency bump, not a PR change: land it on `main` directly"
> (`.claude/skills/uzi-release/SKILL.md:58`)

and it documents the exact damage N independent per-branch fixes cause, because it
already happens with renovate:

> "Back-to-back `--admin` merges of PRs that share a generated lockfile
> (`api/go.mod`+`api/go.sum`, `agent/package-lock.json`) will 3-way-conflict once one
> lands — each is clean against the *current* `main` individually, but merging one
> makes the rest conflict." (`SKILL.md:91`)

Ten agents independently bumping the same dependency on ten branches is that note,
generated automatically.

**The other half is silence.** `mr_rework` is not wasteful here — it is invisible. Its
first Go-side gate (the SQL candidate query has already gated on run/MR state) is the
branch's own green pipeline:

```go
// api/internal/poller/mr_review_watch.go:168-173
// GATE 1 — GREEN HEAD PIPELINE. ...
if !cand.PipelineStatus.Valid || !pipelinestatus.IsSuccess(cand.PipelineStatus.String) {
    return
}
```

A silent no-op by design. So while the baseline is red, review-rework quietly stops for
the entire repo and nothing anywhere says why. And nothing tells the human the baseline
broke at all: the only pipeline notification uzi has ever produced is the
auto-fix-landed one (`api/internal/forgesvc/pipeline_sync.go:193`, `notifyAutofixLanded`
at `:252`); no other notify call exists in that file. The default branch's red badge
renders on the board header (`web/src/pages/Board.tsx:1106`) and waits to be noticed.

**The discriminator already exists.** `workersvc.FailureSignature`
(`api/internal/workersvc/ci_fix_snapshot.go:165`) is a normalized hash over the failed
jobs' sorted `name|stage` plus their last ~20 normalized log-tail lines (doc comment at
`:159-164`), built for exactly this comparison ("the same failure again") and already
computed per candidate at `ci_autofix.go:220`. Computing it once for the default branch
answers the question uzi cannot ask today: *is this branch's failure the repo's
failure?*

## Sketch

### M1 — the gate (the cheapest, highest-value slice; land this alone if nothing else)

- **Candidate query.** `ListCIAutofixCandidateRefs` (`ci_autofix.sql:7-87`) gains a
  `LEFT JOIN pipeline_statuses dbps ON dbps.repo_id = @repo_id::uuid AND dbps.ref =
  rp.default_branch`, selecting `dbps.status`, `dbps.pipeline_id` **and `dbps.synced_at`**
  as three nullable columns. `dbps.synced_at` is the freshness signal GATE 0 reads (below),
  tested absolutely against `now()` rather than against any branch row (the gate says why).
  The join shape — and the `synced_at` select — are the ones
  `ListDefaultBranchPipelineStatuses` already proves (`pipeline_statuses.sql:35`, `:38`).
  **Zero extra forge calls** — the row was written by the same tick's step 1 *when that
  fetch succeeded*; `synced_at` is what proves it did (a failed step-1 fetch preserves a
  stale row, so the freshness gate below must not trust `status` alone). Do **not** add a
  second `IN ('failed','failure',…)` literal:
  select the raw status and classify in Go with `pipelinestatus.IsFailed`
  (`api/internal/pipelinestatus/pipelinestatus.go:51`), so the drift guard
  `api/internal/store/ci_autofix_drift_test.go` (which pins the existing literal list
  to `pipelinestatus.FailedStatuses()`, `:25-31`, for the reason `ci_autofix.sql:69-75`
  records — issue #1005) keeps guarding exactly one copy.
- **GATE 0, coarse, in `detectOne`.** Suppression requires evidence that the
  default-branch baseline was *successfully refreshed on the current tick* — the freshness
  invariant. If the default-branch status is absent, not failed, **or stale**, behave
  exactly as today (fail-open toward current behaviour: fix the branch). *Stale* means the
  baseline was not refreshed on the current tick, tested **absolutely**: `now() -
  dbps.synced_at` exceeds a small freshness window keyed to the poll cadence (under ~two
  poll intervals, with margin for poll jitter). It must be this absolute test, **not** a
  comparison against the candidate branch's own `ps.synced_at`: the branch row is subject
  to the *same* stale-preservation — `syncOneRef` runs for run branches too
  (`pipeline_sync.go:95`) and keeps their stale row on a branch fetch failure — so if both
  fetches fail on one tick the branch anchor is co-stale and a relative test would read the
  baseline as fresh, reopening the very hazard this gate closes. That is precisely the
  case where step 1's fetch failed and `syncOneRef` preserved a stale row
  (`pipeline_sync.go:143-149`, `:164-166`) while the branch's own row refreshed: an
  obsolete red baseline must never suppress a live branch. The already-covered fail-open cases stand — a repo with no CI on
  its default branch, or an MR-only project whose default branch honestly caches nothing
  per `pipeline_sync.go:61-63`, is unaffected.
- **The precise gate, after the snapshot.** When the baseline *is* red, build the
  default branch's snapshot once per repo per *new* default-branch pipeline
  (`workersvc.BuildFailureSnapshot`, `ci_fix_snapshot.go:62` — the same call the manual
  button makes at `api/internal/handler/ci_fix.go:68`), memoized in a
  `ci_autofix_attempts` row for the default-branch ref (the table already has
  `last_signature` + `last_pipeline_id`, `migrations/00117_ci_autofix_attempts.sql:12-21`;
  the sync's eviction keep-set includes the default branch every tick,
  `pipeline_sync.go:66`, and the attempts ledger is reconciled with the same keep-set,
  `:109-114`, so the memo row survives). Suppress the candidate **only when the
  branch's signature equals the baseline's** — the **strict** reading: identical sorted
  `name|stage` job set plus identical normalized log tails, which is the only
  comparison `FailureSignature` supports today. A branch that has its own break *on
  top of* the inherited one hashes differently and is still fixed — which is the
  correct semantics, and the reason a blunt "main is red ⇒ suppress everything" is the
  wrong design.
- **Suppression must be inert, not a halt.** Return before `CreateAutoCIFixRun`
  (`ci_autofix.go:279`), and **do not** stamp `last_pipeline_id` and **do not**
  increment `attempt_count` — so the branch re-evaluates cleanly the tick after the
  baseline goes green, rather than arriving there with its budget already spent. (The
  existing reset-on-green delete at `pipeline_sync.go:180-186` handles the case where
  the branch itself goes green.)
- **Docs.** `docs/ci-autofix.md` "What triggers it" (`:53-72`) and "The loop guard"
  (`:74-91`) gain the new negative case; `docs/mr-review-watcher.md` gains a line
  beside its trigger list saying rework is stalled, not broken, while the baseline is
  red.

### M2 — say it out loud (no token spend)

- **Latch the episode where the observation happens.** `syncOneRef` already performs
  status-conditional side effects (`pipeline_sync.go:180`), so it is the natural home.
  Two nullable columns on `pipeline_statuses` — `red_since timestamptz`,
  `red_notified_at timestamptz` — written through `UpsertPipelineStatus` with a
  `red bool` parameter computed Go-side by `pipelinestatus.IsFailed` (again: no third
  copy of the status list), using the same preserve-across-upsert `CASE` idiom
  `workers.online_since` uses (`api/internal/store/queries/runtime.sql:187`).
- **One notification per red episode**, latched on `red_notified_at` exactly as
  `halt_notified` latches the autofix halt (`ci_autofix.go:230-238`), to the repo's
  connection owner. Kind `default_branch_ci_red`; payload declared beside
  `CIAutofixPayload` (`api/internal/notifysvc/service.go:113-118`), for the reason
  stated there — one package all producers already import. Give it the soft
  `{title, body}` convention so the inbox needs **no** kind switch
  (`web/src/lib/notifications.ts:20-32`), and no `run_id` (repo-level, like
  `early_limit_reset`, `notifysvc/service.go:187`). Body names the count of agent
  branches whose signature matched, i.e. *how many MRs this is holding up*.
- **Web.** No new badge: the default-branch `PipelineBadge` is already on the board
  header (`Board.tsx:1106`) and the Fix CI action already lives in the same page
  (`Board.tsx:643-651`). Add one line of copy next to the badge — "automatic fixes
  paused for N branches" — and a matching marker in `api/cmd/uzi/render.go`.
  `web/src/mocks/mockApi.ts` becomes a second implementation of "when is autofix
  paused", so it needs a **discriminating** fixture, not a snapshot of the demo data:
  one case per branch — baseline red + matching signature (paused), baseline red +
  different signature (not paused), baseline green (not paused), baseline with no
  cached pipeline (not paused, and rendered differently from "green" so the test can
  tell "we don't know" from "it's fine") — plus an assertion that the fixture actually
  contains all four.

### M3 — fix `main` once instead of N times (cuttable, opt-in, default OFF)

When the baseline has been red past a quiet period and no `ci_fix` run is active for
the default-branch ref, auto-queue **one** `ci_fix` run on the default branch. This
needs no new machinery: the manual button already accepts any failed cached ref
(`handler/ci_fix.go:57`, `if !pipelinestatus.IsFailed(ps.Status)`), and a
default-branch fix works on `ci-fix/pipeline-${claim.pipeline.id}`
(`agent/src/run-kind.ts:80`, `agent/src/git.ts:445`), opening an MR — `main` is never
touched and the PRD #66 guardrails (`prds/done/66-guardrail-enforcement.md`) still
refuse if the bot could push to it.

This is a **policy reversal and must be gated as one**: `docs/ci-autofix.md:65`
("`main`, the repo's default branch, and any non-MR ref are never auto-touched") and
the SQL's protected-branch stand-in rationale (`ci_autofix.sql:26-31`) state today's
exclusion deliberately. M3 needs its own admin kill-switch beside `KeyCiAutofixEnabled`
(`api/internal/settings/keys.go:231`), default OFF, read three-state and fail-closed
like `CiAutofixEnabled` (`api/internal/settings/settings_ci_autofix.go:22`), and both
documents amended rather than quietly contradicted.

### Where it lives / what it touches

- `api/internal/store/queries/ci_autofix.sql` — the default-branch LEFT JOIN + three
  columns (`status`, `pipeline_id`, `synced_at`); optionally the same on `mr_rework.sql`
  if M2's explanation is surfaced per candidate
- `api/internal/poller/ci_autofix.go` — GATE 0 + the signature compare; the baseline
  snapshot memo
- `api/internal/forgesvc/pipeline_sync.go` + `api/internal/store/queries/pipeline_statuses.sql`
  — the `red_since` latch (M2)
- `api/internal/store/migrations/` — one migration: `pipeline_statuses.red_since`,
  `.red_notified_at` (M2). Live head at write time is `00202_run_codex_binding.sql`;
  number assigned at the landing merge
- `api/internal/notifysvc/service.go` — the payload; `api/internal/settings/keys.go` —
  M3's kill-switch
- `web/src/pages/Board.tsx`, `web/src/mocks/mockApi.ts` + its discriminating fixture;
  `api/cmd/uzi/render.go`
- `docs/ci-autofix.md`, `docs/mr-review-watcher.md`, `docs/configuration.md`,
  `CHANGELOG.md`

## Caveats

- **Riskiest assumption, validatable in an hour: that a repo-wide failure hashes
  identically on `main` and on a branch.** `FailureSignature` is strict — sorted
  `name|stage` set plus ~20 normalized log-tail lines (`ci_fix_snapshot.go:159-184`),
  bounded by the snapshot's job cap (the `maxJobs` argument, `handler/ci_fix.go:68`).
  On GitHub the branch's and `main`'s runs are different workflow runs of the same
  jobs, so the job identity should match, but path-filtered/skipped jobs can shrink a
  PR pipeline's job set relative to the default branch's, and a log tail that embeds a
  SHA or a run id would not normalize away — under the strict reading a genuinely
  inherited failure may then NOT match. Validate by replaying `FailureSignature` over
  one real red-`main`/red-branch pair per forge **before** committing to the signature
  gate; if it proves too brittle, the honest fallback is to ship M2 (the alert) alone
  and leave suppression out — not to fall back to blunt suppression, and not to loosen
  the comparison ad hoc (a per-job or log-tail-only overlap is a different design and
  needs its own false-positive analysis).
- **Why not blunt suppression.** A false positive here means a genuinely broken agent
  branch goes unfixed while the baseline is red. Mitigations are in the design:
  signature equality, not mere co-redness; the manual Fix CI button is untouched; the
  suppression is inert, so nothing is spent from the branch's budget while it waits.
  Cap the blast radius by making the gate honour the existing `ci_autofix_enabled`
  admin kill-switch.
- **Baseline freshness — fail open on stale.** A failed default-branch fetch does not
  clear the cached row: `syncOneRef` keeps it and retries next tick
  (`pipeline_sync.go:143-149`, `:164-166`), so the baseline is not guaranteed to be from
  the current tick, and a later run-branch fetch can be newer. Gate 0 therefore reads
  `dbps.synced_at` (stamped `now()` only on a successful upsert, `pipeline_statuses.sql:5`,
  `:8`, `:15`) and suppresses **only** when the baseline was refreshed this tick; a
  baseline whose `synced_at` did not advance is treated as absent, and the branch is fixed
  as today. This keeps an obsolete red baseline from suppressing a branch on stale failure
  data and preserves the design's "fail open toward current behaviour" property throughout.
  No schema change: `synced_at` already exists and `ListDefaultBranchPipelineStatuses`
  already selects it (`pipeline_statuses.sql:35`).
- **Poll-based, so the episode boundaries are coarse.** `pipeline_statuses` keeps only
  the latest row per ref, so a red episode that opens and closes between two ticks is
  invisible, and `red_since` is only as precise as the poll cadence. This is PRD #6's
  documented staleness trade, inherited, not introduced.
- **Not every red baseline reddens the branches.** A repo on plain GitLab branch
  pipelines can have a red `main` and green branches; the gate simply never fires
  there, which is correct. The cases it does catch are the evaluated-against-live-state
  ones (`SKILL.md:58`), merge-result/merge-commit pipelines, and branches cut from or
  realigned onto the broken tip.
- **One stale doc, noted so this file does not inherit it.** The same
  `ARCHITECTURE.md:130` sentence quoted above for "auto-approved and skipping the plan
  gate" still calls autofix "opt-in … default off" — false since migration
  `00190_user_ci_autofix_tristate.sql` and `DefaultCiAutofixEnabled = "true"`
  (`settings/keys.go:349`); the default-on fact here is taken from
  `docs/ci-autofix.md:14-15` instead. Filed as an incidental finding for a maintainer;
  not fixed by this idea.
- **Adjacency, checked.** `ideas/bingo/2026-09-01-mr-conflict-watch.md` observes
  *mergeability* per MR; this observes *CI health* per repo. They are orthogonal but
  they collide on presentation: if both land, a card needs one coherent "blocked
  because" line, not two competing badges — settle that in whichever PRD lands second.
  `ideas/bingo/2026-08-25-mr-review-rework-runs.md` shipped as PRD #700/#908/#841 and
  owns `mr_review_watch.go`; this idea only *reads* its GATE 1, and edits it at most to
  explain itself. `ideas/bingo/2026-08-11-run-queue-priority.md` (shipped,
  `prds/done/320-run-queue-priority.md`) and `2026-08-18-worker-pause-quiesce.md` are
  unrelated. Open PRDs checked and not overlapping: #1170 (run-health `slow` →
  near-timeout, a per-run wall clock), #1189/#1190 (per-run budget/pause), #456
  (finalize-time base align, worker-side, one-shot), #377 (unpushable workflow branch),
  #284 (push retry classifier), #1183/#1184 (triage UI), #216/#217 (claim
  placement/credentials). Shipped and checked: `prds/done/71-ci-autofix.md`,
  `914-ci-autofix-default-on.md`, `6-ci-status-integration.md`,
  `700-mr-review-watcher.md`, `908-mr-rework-scheduled-runs.md`,
  `527-mr-merged-state-recording.md`. A sweep of `prds/` (open + done), `ideas/`,
  `adr/`, `docs/` and `specs/` for any baseline-red / default-branch gate on autofix
  found no prior proposal of this mechanism.
- **Explicitly out of scope.** Webhooks (PRD #6's deferred item; this rides the
  existing tick). Pipeline *history* — uzi caches only the latest per ref, so "which
  merged MR broke `main`" needs a history table plus merge timestamps and is a separate
  idea. Per-job or flaky-test attribution. Anything that pushes to `main`: M3 opens an
  MR from `ci-fix/pipeline-N` like the manual button, and the guardrail layers are
  unchanged. Applying the same baseline gate to the `mr_rework` *decision* (its GATE 1
  already suppresses; only the explanation is missing).
