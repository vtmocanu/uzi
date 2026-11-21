# ADR-4007: a run's terminal work is preserved under its own forge ref namespace, separate from held work and checkpoint retention

**Status**: Accepted (slice A implemented API-side and dark: no shipped worker advertises the capability, so no ref is created, no gate fires and the sweep lists no rows; slice B is partially built and dark: the worker producer exists behind a compile-time-off gate and runs nowhere in production, slices C to E and the retirement switch are deferred, see below)
**Date**: 2026-11-13
**Issue**: [vtmocanu/uzi#4007](https://github.com/vtmocanu/uzi/issues/4007)
**Refs**: #4007, #2545, #1810, #1867, #2507, #1296, #4488, #4512

## Context

A run's final work can be lost in two ways that custody does not cover.
[ADR-2545](2545-failed-run-held-publication.md) publishes held work to
`refs/uzi-held/<run-id>/<generation>`, but only while a custody hold is open,
and it retires that ref on a retention clock tied to hold release.
[ADR-1810](1810-checkpoint-retention-follows-custody.md) keeps the checkpoint
ref while custody is open and deletes it afterwards. Once custody is gone, a
finished run's last state survives only as long as the PR or branch the owner
happens to keep.

Slice A of #4007 builds the API half of a separate answer: a per-run terminal
ref whose lifetime follows the run's issue or merge request, not its custody.
It is built dark. Every claim below describes what slice A built, not what runs
in production.

## Decision

### A separate namespace and separate tables

A terminal ref is `refs/uzi-terminal/<run-id>`, one per run. It is not a held
publication and not a checkpoint or recovery ref. Two tables carry it. Neither
references `run_held_publications` or `checkpoint_retention`; a contribution
authorized by an open guarded custody hold names that hold (`hold_id`, an
`ON DELETE RESTRICT` foreign key). The other custody interactions are an open
custody hold of the run blocking the retire decision, and the scoped
completed-release gate (below):

- `run_terminal_preservations`: one row per run. States are `bound`,
  `publishing`, `live`, `retiring`, `deleted`, `abandoned` and
  `not_published`. A nullable `live_run_id` is an `ON DELETE RESTRICT` pointer
  to the run, set while the row is `publishing`, `live` or `retiring`, so a run
  delete cannot silently orphan a remote ref. Triggers pin the state machine.
  `retiring` is irrevocable: it may move only to `deleted` or `abandoned`.
- `run_terminal_contributions`: one row per (run, generation), with a
  disposition of `preserved`, `closed_issue_abandoned` or
  `closed_pr_abandoned`.

The reason CHECK on the completed-publication hold is widened (two follow-on
migrations) to admit the two gate reasons below. Migration numbers in the
branch are drafts and are renumbered at landing.

### Typed pushbroker entry points

`api/internal/pushbroker/terminal.go` adds typed create, update, list and
delete for `refs/uzi-terminal/` only. Updates are compare-and-swap
fast-forwards and are never forced; the delete is a CAS on the recorded tip.
The shared `validManagedRef` and the generic delete are unchanged, so no other
code path can delete a terminal ref, and the terminal entry points cannot touch
another namespace.

### Declared coverage, not proof of completeness

Before a push, the broker verifies the worker's declared coverage: every
declared root is reachable from the tip, each declared tree matches, and a
connectivity walk of everything reachable from the tip (the pack plus the
fetched bases) succeeds. A contribution reaching `published`
means only that. It is not proof that the declared inventory is the run's whole
inventory: omission detection is slice B and is not built.

### The worker route and the send-winner rule

`POST /api/worker/runs/{id}/terminal-preservation`
(`api/internal/handler/worker_terminal_preservation.go`, service in
`api/internal/workersvc/terminal_preservation.go`) is the only producer.

- **Authority** is either the current flight or an open guarded hold.
- **Lifetime** is classified once, by the same function the sweep uses: an
  issue-bound run consults only its issue; an issue-less run with a merge
  request consults only the MR; otherwise it is `no_pr`.
- **Send-winner rule**: at most one network send per generation. A marker is
  committed before the push (tx1); only the request that sets it sends. A losing request
  never sends, and a request whose commit is ambiguous never sends; a retry
  reconciles (lists the ref) when the marker is durable, and otherwise may win
  afresh and send once, so the bound stays one send per generation. The outcome
  is recorded in tx2 on a context detached from the request deadline. No forge
  call runs inside a transaction. The shape follows ADR-2545 step A.
- If the issue or MR is proven closed, the route records an abandoned
  disposition and pushes nothing.
- Refusals are typed (`apitypes.TerminalReason*`).
- The route is negotiated by worker capability
  `recovery_terminal_preservation_v1`. Without it the route refuses
  `unsupported` and writes nothing.

### The terminal_preservation sweep and the retire ordering

`SweepTerminalPreservations` (`api/internal/workersvc/terminal_sweep.go`) is
always registered and starts with one bounded, indexed due-row query; an empty
result returns before any credential resolution, forge call or write. Lanes:

- **Reconcile** (live): lists the ref and never sends. A ref at the
  contribution's tip records published; a ref at the prior tip or absent stays
  unknown under backoff; a foreign tip refuses the contribution and abandons the
  preservation without touching the ref.
- **Stale-bound** (live): a bound row with no invoked contribution, after a
  grace, becomes `not_published`. A row whose run no longer exists (repository
  removal cascades runs) is moved under the preservation row lock alone.
- **Retire**: implemented and tested, production-disabled. Its ordering is:
  1. Forge observation of the issue or MR, with the observation clock stamped
     from the database before the forge read.
  2. tx-R1 under the run lock: protections are re-checked, and an observation
     older than 15 s on the database clock is discarded.
  3. Commit the irrevocable `retiring`.
  4. CAS delete with no transaction open.
  5. Record `deleted` (ref absent) or `abandoned` (foreign tip), or stay
     `retiring` with backoff when the outcome is uncertain.

  A `no_pr` run (no issue, no merge request) has no closure signal; it becomes
  eligible (`no_pr_expired`) once the observation is at least 30 days after the
  ref's first verified publication. `first_published_at` is set once, so a later
  update never renews the clock.

  Every decision is bound to the association it observed: the observation
  records the run's repo, issue and merge request, and tx-R1 (like the route's
  tx1) re-reads the locked run and discards the decision if any of them changed.
  A run that gained an issue or MR is classified by it at the next observation.

Lock order wherever several are taken: **consumer gate** (always the first lock
of its transaction; admissions take it shared with a bounded wait, retirement
only try-takes it exclusively, under server-side timeouts set before it, and
never waits for it; see "T3" below); then, on the run-creation path, the
per-user secret-mutation lock (shared), the model-provider row share, the
closure locks (run-branch, pinned-override share, judge target) and the
post-insert credential and alias fences; then worker, run, hold, preservation,
contribution.

### T3: consumer admissions serialize with retirement (#4488)

tx-R1 reads the protections (`GetTerminalRetireBlocker`,
`HasCrossRunLiveConsumer`) under the source run's `FOR NO KEY UPDATE NOWAIT`
lock, at READ COMMITTED. Nothing a consumer admission does conflicts with that
lock: FK dependents take only `FOR KEY SHARE`, and same-issue and MR/branch
consumers share no row with the source. A consumer committed between the
protection read and the `live -> retiring` commit was therefore missed. T3
closes that check-then-act race with one advisory lock per repository,
`TerminalConsumerGateLockClass` (`0x757A7463`, "uztc"; objid = fnv32a of the
repository UUID, `store.TerminalConsumerGateObjID`).

- **Admissions** take it **shared** as the first statement after an explicit
  `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`, before any other lock of
  the transaction, with a bounded wait (`lock_timeout` 6 s, restored after the
  gate so every later wait is unchanged). Shared holders never conflict, so
  admissions never serialize with each other. The helper refuses a non-
  transaction connection and any isolation other than READ COMMITTED.
- **Retirement** (only the decide path, `terminalRetireDecide`, through
  `terminalLockRunForRetire`) sets server-side bounds first
  (`transaction_timeout`, `statement_timeout`,
  `idle_in_transaction_session_timeout`), then **try**-takes the gate
  exclusively in its own statement, then takes the existing NOWAIT run lock and
  the preservation row. A failed try retires nothing and records
  `retire kept: consumer_gate_busy`. Its rechecks run as separate statements
  after the try, so each sees every admission that held the gate. The bound,
  reconcile, unknown, foreign and finish lanes do not take it: after
  `retiring` the decision is already irrevocable. Any future owner escape that
  writes `live -> retiring` must go through the same function.
- **Waiter priority.** PostgreSQL makes a new lock request conflict with queued
  waiters as well as holders, so while an admission is queued every later
  try-exclusive fails. It waits behind at most one exclusive holder; durable
  commit I/O is not bounded by the pre-commit budgets below.
- **Retirement hold bound.** The gate is a transaction-scoped lock, so a
  stalled or killed tx-R1 can only be freed by the server. On PostgreSQL 17 and
  later tx-R1 sets `transaction_timeout = 4s` (with `statement_timeout = 2500ms`
  and `idle_in_transaction_session_timeout = 2s`). Earlier versions (the chart
  pins 16) have no `transaction_timeout`: they set `statement_timeout = 150ms`
  and `idle_in_transaction_session_timeout = 150ms`, and the preservation row
  `lock_timeout` drops to 120ms (below the statement timeout, so contention
  stays busy). tx-R1 makes at most 14 round trips after the gate (8 statements,
  6 of them parameterised and possibly preceded by a Prepare on a cold
  connection), giving a pre-commit budget of 150ms + 14 x (150ms + 150ms) = 4.35s.
  Invariant (asserted by a unit test over the production values): the
  pre-commit budget must stay below the admissions' 6s gate wait. This does not
  bound durable COMMIT: PostgreSQL disables the transaction timeout before
  durability and releases locks after the WAL flush
  ([source](https://github.com/postgres/postgres/blob/REL_17_STABLE/src/backend/access/transam/xact.c)).
  A stalled commit may therefore make admissions busy; testing and any stronger
  protocol are deferred before enablement. Marker durability remains required.
  Enabling retirement on
  PostgreSQL 16 also needs the protection query to fit those per-statement
  limits at real volume (indexes such as `cross_checks(checker_run_id)` and
  `runs(resume_of_run_id)`, part of the slice C/D protection-query work).
- **Why not a source-row lock.** A row lock on the target deadlocked against an
  FK cascade in #3257 (`api/internal/store/queries/judge.sql`, the
  `LockRunForJudge` note); the gate is an advisory lock no cascade takes.

**Audit (the plan checkpoint, at 6ac884bd7).** Every `INSERT INTO runs` in
`api/internal/store/queries/*.sql` (15 inserts) and every post-insert write of
an association column (only `SetRunCompleted` and `ReconcileRunMR`; `repo_id`
is never updated) was classified:

| Family | Class |
|---|---|
| Issue runs (manual, scheduled, autopilot, chat start), self-improve, continued-issue tasks | gated; same-issue consumers (`issue_iid` / `continued_issue_iid`) |
| Then-fix and review children, MR rework | gated; keyed on the target run's repository, revalidated after the gate (a cross-repo relationship is refused) |
| CI fix | gated; matches `mr_run_active` by `pipeline_ref` |
| Judge (auto, rerun, manual) | gated on the target's repository; a repo-less target takes no gate |
| `ReconcileRunMR` | gated in its own transaction: it is status-agnostic and can give a nonterminal ci_fix (inserted with `mr_iid` NULL) an `mr_iid` that matches another source's `mr_run_active` |
| Plan and code cross-checks (incl. the failure record) | already serialized: admission locks the nonterminal lead `FOR UPDATE`, which conflicts with the NOWAIT lock, and a nonterminal lead keeps on `run_nonterminal` |
| Active snapshot | already serialized: `LockFrozenWorkerSnapshotRuns` takes `FOR UPDATE OF r` before it can add a `claim_live` row |
| `SetRunCompleted` on the source | already serialized: own-row lock vs NOWAIT, plus the association re-check |
| `SetRunCompleted` on a consumer, prompt and CI repair, claim, resume, promoter | non-consumers: they set no association column, or touch only already-counted nonterminal runs |
| Chat continuation, chat and job runs | non-consumers: repo-less (`runs_kind_shape`), and preservations carry `repo_id NOT NULL` |

Recorded misses, not widened here: a source linked only by
`continued_issue_iid` gets no same-issue check (it runs only when the source
has an `issue_iid`), and a ci_fix with `ci_fix_mr_iid` set but `mr_iid` NULL is
matched only by branch.

**Retirement-first contract.** No admission or association path reads
`refs/uzi-terminal/` (the namespace is read only by publish, pushbroker,
verification and retention code). A matching protection is therefore a
conservative keep, never proof that the consumer needs the ref. When retirement
commits first, the admission waits on the gate for one bounded hold and then
proceeds unchanged; the preservation stays `retiring` and is never reopened (the
00448 guard allows only `deleted` or `abandoned` next); an ordinary fresh run
is never blocked; and truthful evidence is kept: `ReconcileRunMR` keeps its
status-agnostic SQL and its unchanged row-lock wait, so a late MR or branch
report on a `retiring` or `deleted` source still records.

**Post-commit detachment of the worker terminal report.** After the state
transaction commits, the gated `ReconcileRunMR`, the authoritative re-read that
gates the hook block, and the fire-once judge, task-review and then-fix
dispatches run on `context.WithoutCancel` of the request context, with **no
aggregate or per-hook deadline**. Those hooks warn-and-swallow and never retry,
and the judge target lock wait has no timeout of its own, so any new overall deadline
could drop an otherwise successful fire-once admission even with retirement
disabled. The only new wait is the gate's, bounded as above; at most two gated
writers run per report (`ReconcileRunMR` plus one hook: judge kinds and task
kinds are disjoint, and review and then-fix are mutually exclusive).

**Caller semantics.** Gate contention (timeout or cancellation) is
`ErrTerminalConsumerGateBusy`: HTTP admissions answer 503
with `Retry-After: 1`, the pollers and the scheduler leave the trigger
unrecorded (CI autofix never maps it to the errors that record a pipeline), and
a schedule that has started nothing in that fire is not advanced. A recurring
sweep fire that already started runs records the busy candidate as a visible
`consumer_gate_busy` skip, stops and advances (holding it would re-fire the
whole sweep); it is considered again at the next fire. A scheduled one-time
sweep that already started runs is not advanced: it retries its remaining slots
from the durable per-fire ledger (T3a below), without replay or cap overshoot.

Delivery scope was narrowed for #4488: preserving the original cap and avoiding
replay across partial-fire retries needs durable per-fire candidate progress, so
that fire was first delivered as record, stop and advance for every sweep. The
durable progress was delivered by #4512 (T3a), which restored the one-time
retry; recurring sweeps keep the narrowed behaviour. The active-run/open-MR
guards are not per-fire idempotency after a run ends; the ledger is. Unattempted
candidates after the busy one are not claimed as individually skipped. The final
partial-outcome write uses a cancellation-detached five-second context, so the
canceled gate wait does not itself discard that outcome. Run-now stays a
distinct manual fire with no progress recorded, and never advances.

**Residual limits.** A fire-once hook or `ReconcileRunMR` still drops its work
on a genuine database fault, as every other database error there already does;
a durable retry is not built. A busy repository can defer retirement
indefinitely; that is safe, because the preservation is kept.
Detachment has a cost: before T3 a worker disconnect (including its client-side HTTP timeout)
cancelled the post-commit waits; now a stuck row-lock or judge-lock holder keeps
the report's handler goroutine and pool connection (and, inside a gated write,
the shared gate, so retirement keeps `consumer_gate_busy` for that repository)
until the holder ends. The gate wait itself stays bounded; the downstream waits
are unbounded exactly as long as their holder is. A fire-once hook whose gate
wait times out, including during a stalled durable commit, logs and drops its
admission, as every other database error there does.

### T3a: durable per-fire progress for one-time sweeps (#4512)

This supersedes the narrowed partial-fire paragraph under "Caller semantics" in
T3 above, which recorded the busy candidate and advanced a one-time sweep
instead of retrying it (narrowed for #4488, delivered here by #4512).

**Problem.** A scheduled one-time sweep that started some runs and then met a
busy consumer gate recorded a `consumer_gate_busy` skip and advanced to
`fired`, so the remaining slots were lost. Holding it instead would replay the
whole sweep: no record links a run to its fire. Scheduled issue runs carry no
`schedule_id`; `HasActiveRunForIssue` stops guarding once a run ends and the
open-MR dedup once there is no open MR; and `last_fire` is a diagnostic summary that `AdvanceSchedule` and
`RecordScheduleHeldFire` overwrite. Run creation and the schedule advance also
commit separately, so a crash between them leaves the row due. T3a adds a
durable record of each fire's admissions with these properties:

- **Atomic.** The progress row commits in the same PostgreSQL transaction as
  the run it records. No reader sees a run without its row, or a row without its
  run; any error or cancellation rolls back the run, the fire header and the
  progress row together.
- **Durable across a process crash.** This rests on PostgreSQL commit
  durability at the default `synchronous_commit=on`. No filesystem or
  power-loss claim is made.
- **Recoverable.** A fresh scheduler or API instance re-reads the row's
  `fire_epoch` and the ledger; nothing else is needed. A commit whose
  acknowledgement is lost leaves both rows, and the retry reads them.

No transaction is held across forge HTTP: the issue fetch, candidate listing and
label I/O stay outside the create transaction, as before.

**Occurrence identity.** `run_schedules.fire_epoch bigint NOT NULL DEFAULT 0`
names the occurrence. It is bumped by a `BEFORE UPDATE` trigger, not by
application code, so an older binary's re-arm still invalidates the old
occurrence. The trigger condition is exactly:

```sql
WHEN ((OLD.timing = 'once' OR NEW.timing = 'once') AND (
       (OLD.status IS DISTINCT FROM 'active' AND NEW.status = 'active')
    OR NEW.run_at  IS DISTINCT FROM OLD.run_at
    OR NEW.timing  IS DISTINCT FROM OLD.timing
    OR NEW.target  IS DISTINCT FROM OLD.target
    OR NEW.repo_id IS DISTINCT FROM OLD.repo_id))
```

and the function body only sets `NEW.fire_epoch := OLD.fire_epoch + 1`. A
recurring-to-recurring update never meets the outer guard, so a pure recurring
row's epoch never moves; a conversion into or out of `once` bumps. INSERTs do
not fire it, so a new row starts at epoch 0.

**Progress records** (two additive migrations, each opening with
`SET LOCAL lock_timeout = '5s'`, following the 00438/00439 split):

- The first adds the column, the epoch function and the trigger. Its Down drops
  the trigger, the function, then the column.
- The second adds `schedule_once_fires`, one header per
  `(schedule_id, fire_epoch)` (cascading from `run_schedules`), holding the
  fire's original effective cap: `effective_cap int NULL CHECK (>= 0)`, NULL
  meaning unlimited. It also adds `schedule_once_fire_runs`, an append-only
  ledger with one row per admitted issue, keyed
  `(schedule_id, fire_epoch, issue_iid)` under an explicitly named primary key
  and cascading from the header.
- `run_uuid uuid NOT NULL` is the admitted run's id, written in the same
  transaction. It deliberately has no foreign key, so deleting the run row
  cannot null or cascade it: the run identity, the spent slot, the issue title
  and the web URL (both `NOT NULL`) survive. A CHECK forbids the nil UUID, so a
  summary rebuilt from the ledger never carries a zero `Started.RunID`.
- A `BEFORE UPDATE` trigger raises on any change to `schedule_id`, `fire_epoch`,
  `repo_id`, `issue_iid` or `run_uuid`, and no UPDATE query on the table exists.
- The ledger stores an issue number, so it carries
  `repo_id NOT NULL REFERENCES repos ON DELETE CASCADE` (indexed), gets the
  00474 `awaiting_first_enable` guard trigger, and joins the
  `RepoStoresIssueNumber` EXISTS list. This is the same rule as every other
  table that stores an issue number for a repository: without it a repository
  still awaiting its first enable could acquire issue numbers through the
  ledger, which the issue-tracker link guard assumes cannot happen. The `runs` guard already fires first on the same
  transaction's `CreateRun`, so the ledger guard is defence in depth and keeps
  `RepoStoresIssueNumber` in step with the trigger list.
- The second migration owns both triggers and the immutability function. Its
  Down drops the triggers, then the function, then the tables.

**Writers of `run_schedules`** (from a repo-wide search for UPDATE, INSERT and
DELETE; exhaustive for direct SQL):

| Writer | Epoch | Why |
|---|---|---|
| `CreateRunSchedule`, `CreateDefaultSchedule` (also the self-improve migration) | starts at 0 | INSERT; the trigger is UPDATE-only |
| `UpdateRunSchedule` (config PATCH) | bumps on revival or a `run_at`, timing, target or repo change | the only writer that revives a fired or error once row (`status = 'active'`) |
| `ResetDefaultSchedule` | none | writes `status = 'active'`, but default rows are always `recurring`, so the outer guard is false |
| `SetRunScheduleEnabled` (pause/resume) | none | enabled-only, on any status |
| `ResumeRecurringSchedule` | none | recurring only |
| `CoalesceScheduleSiblingGroup`, `ClearSingletonSiblingGroup` | none | sibling-group bookkeeping |
| `AdvanceSchedule` (recurring arm; becomes the guarded advance below) | none | `run_at` unchanged; `active` stays `active` |
| once advance (becomes `AdvanceOnceScheduleGuarded`) | none | active to fired, `run_at` unchanged |
| `SetRunScheduleStatus` | would bump on a once revival | no caller remains once `park` moves to `ParkScheduleGuarded`; its only callers today are `park`'s, inside `advance` |
| `ParkScheduleGuarded` (new) | none | active to error, `run_at` unchanged |
| `RecordScheduleHeldFire`, `RecordOnceFirePartial` (new) | none | `last_fire` (and, for the held write, `recent_fires`) only |
| `DeleteRunSchedule` | n/a | cascades the header and ledger |
| FK side effects: secret delete `SET NULL` (00233), repo delete cascade (00103) | none / n/a | `credential_override_secret_id` is not in the predicate; a repo delete removes the row |

The no-bump arms of a config edit (one that keeps `run_at`) are reachable
through PATCH only while the row is not yet due, because a one-time PATCH must
carry a future `run_at` (below). Such an epoch has no ledger rows yet, since only
a scheduled fire of a due row writes them. For a due, partially admitted fire
they are reachable only by direct SQL.

**Admission: lock order inside the create transaction.** The scheduler passes an
optional `OnceFireProgress` (schedule, user, repo, epoch, issue, title, URL,
effective cap) to `CreateScheduledRun` / `CreateScheduledAutopilotRun`. Nil is
today's behaviour and is what RunNow, recurring sweeps and the issue target
pass. Non-nil progress without a transaction beginner, or on a store that is not
`*store.Queries`, fails closed (`errOnceFireRequiresTx`, like
`errCIRepairRequiresTx`). The order is:

1. The consumer gate, shared, first (T3).
2. The D11 resolve reads.
3. `LockSecretMutationShared`, when the creation is lock-eligible or pool-selected (unchanged).
4. `LockRunBranch` (advisory).
5. The pinned secret `FOR SHARE` (`lockPinnedOverrideEnabled`).
6. **New:** the repo row `FOR SHARE` (`LockRepoForCIWatch` or an equivalent
   share query).
7. **New:** `LockRunScheduleForUser`, `FOR UPDATE`, owner-scoped. A missing row
   is `ErrOnceFireStale`.
8. **New:** recheck the locked row: `timing = 'once'`, `target = 'sweep'`,
   `status = 'active'`, `fire_epoch` equal to the claimed epoch; otherwise
   `ErrOnceFireStale`. Identity binding: the progress user, repo and issue are
   compared with the actual create arguments before any lock is taken, and the
   locked row's `repo_id` must agree; any mismatch is `ErrOnceFireIdentity`
   before anything is inserted. The locked row's `user_id` agrees by
   construction, because step 7 locks only the owner's row, so a schedule whose
   owner changed reports as stale.
9. **New:** insert the header `ON CONFLICT DO NOTHING` with the passed cap, then
   read the stored cap, the admitted count and whether this issue is already
   recorded. Already recorded is `ErrOnceFireCandidateRecorded`; a non-NULL
   stored cap with count at or above it is `ErrOnceFireCapSpent` (the NULL check
   is explicit in Go). These are fresh READ COMMITTED statements under the row
   lock, so they see every committed admission.
10. `CreateRun`.
11. **New:** insert the ledger row with `run_uuid` set to the id `CreateRun`
    returned. A 23505 on the ledger's primary key maps to
    `ErrOnceFireCandidateRecorded`, checked before the generic
    unique-violation arm that yields `ErrActiveRunExists`.
12. The Codex binding freeze.
13. Commit.

The schedule row lock serializes concurrent admissions for one fire.

**Summary writes.** Every once-sweep summary (the partial write, the held write
and the completing advance) goes through one helper, `WriteOnceFireSummary`. It
opens a short transaction with no forge I/O, sets `lock_timeout = '5s'` (a stuck
admission ends the wait with a 55P03 or, as the helper's detached context is also
5 seconds, with that context's deadline; both are treated as transient), takes the same
`LockRunScheduleForUser FOR UPDATE` row lock as step 7, rechecks the epoch and
`status = 'active'`, re-reads the ledger under that lock, composes Started from
the ledger rows plus the caller's skips and capacity fields, and writes either
`last_fire` only for `RecordOnceFirePartial`, the epoch-guarded
`RecordScheduleHeldFire` when there is no progress yet (which also prepends to
`recent_fires` when `matched > 0`, including the credential-disabled hold with
`matched = 1`, preserving PRD #1732), or the guarded advance
`AdvanceOnceScheduleGuarded` (`AND fire_epoch = @fire_epoch AND
status = 'active'`). An admission either committed before the summary's read
and is included, or waits for the summary to commit; a delayed writer cannot
overwrite a summary that lists more starts than it saw, because Started is never
taken from the caller's earlier read. A stale epoch or status writes nothing and
is logged as a benign stale fire. Outside the helper:

- The recurring arm of `advance` uses `AdvanceScheduleGuarded`
  (`AND fire_epoch = @fire_epoch`). A pure recurring row's epoch never moves, so
  its cadence is unchanged; a stale recurring fire can no longer overwrite a row
  converted to once mid-fire.
- `park` uses `ParkScheduleGuarded` for every timing (`AND fire_epoch AND
  status = 'active'`). Zero rows is a benign stale fire and suppresses the
  owner notification.
- `RecordScheduleHeldFire` outside the helper remains only for one-time issue
  targets, with the added epoch guard.

**Lock-order proof.** Admission locks, in order: advisory gate, advisory secret
mutation (shared, when eligible or pool-selected), advisory run branch, pinned
secret row (share), repo row (share), schedule row (exclusive), then its own new
rows. Each concurrent writer takes the locks it shares with admission in
admission's order (a subsequence), holds only locks admission never needs, or
holds a single row:

- **Repo delete** (`DeleteRepoForUser`, `queries/forge.sql`). It locks the
  repository's existing runs, deletes the repo row, then cascades to its
  schedules, header and ledger rows. Admission takes the repo `FOR SHARE`
  before the schedule `FOR UPDATE`, the order `LockRepoForCIWatch`'s header in
  `queries/ci_watch.sql` records ("pinned secret, repo, schedule, episode,
  run"). If admission holds the share lock, the delete waits at the repo row
  before any cascade. If the delete holds the repo row first, admission waits at
  step 6 and finds the repo gone. Admission locks no existing run row, and its
  later inserts take only `KEY SHARE` on the repo it already share-locked.
  Taking the schedule first would cycle: admission holding the schedule and
  needing the repo key-share for its insert, the delete holding the repo and
  cascading into the schedule.
- **Secret delete and disable** (`withSecretLock`, `handler/secrets.go`,
  `LockSecretMutation` exclusive, then the credential row). A lock-eligible
  admission holds the shared mutation lock, so the two exclude each other
  outright. Otherwise a pinned admission holds the secret row `FOR SHARE`
  (step 5, `lockPinnedOverrideEnabled` in `workersvc/credential_disabled.go`),
  which conflicts with the delete's and the disable's row writes, so the writer
  waits; if the writer is first, admission waits at step 5 before it holds the
  schedule. An unpinned schedule row carries no secret id, so the delete's
  `SET NULL` never reaches it. The admission's own FK checks
  (`credential_override_secret_id` at step 10 for a pinned run,
  `codex_secret_id` at the step-12 freeze) take `KEY SHARE` on the secret;
  against a concurrent delete of that secret they wait and then fail with
  nothing written, because the delete holds nothing the admission already
  holds. The disable writes no `run_schedules` row.
- **Schedule config edits.** A PATCH that involves ci_watch (including a
  conversion from or to a once sweep) runs in one transaction whose first
  statement is the schedule row lock (`lockScheduleForConfig`,
  `handler/schedules_ci.go`); after it come only plain reads (repo ownership,
  episode history, credential validation) and the update's FK checks, which
  take `KEY SHARE` on the repo or secret only when those columns change, and
  `KEY SHARE` is compatible with admission's `FOR SHARE` locks. Admission
  waiting at step 7 therefore holds nothing the edit needs. A conversion bumps
  the epoch, so the waiting admission then fails step 8 as stale. Every other
  PATCH is a single autocommit `UpdateRunSchedule`.
- **CI repair** (`CreateCIRepairRun`, `workersvc/ci_repair.go`) takes the same
  pinned secret, repo, schedule order (`recheckCIWatchConfig`), and its schedule
  is a ci_watch row, never an admitting once sweep.
- **Held, partial and advance writes.** `WriteOnceFireSummary` takes only the
  schedule row; its UPDATE changes no FK column (so no repo or secret
  key-share) and the ledger has no FK to `runs` (so run deletion takes nothing
  on it). The recurring guarded advance and the guarded park are single
  autocommit statements on one row.
- **`ClaimDueSchedules`** is `FOR UPDATE SKIP LOCKED` and autocommits: it never
  waits, and a row under admission is simply not claimed that tick.

**Fire semantics (scheduled tick, one-time sweep only).**

- `process` calls the fire with `scheduled = true`; RunNow passes `false` and
  keeps today's path exactly (no ledger read, no fold, no partial write, its
  503 mapping). RunNow stays a distinct fire.
- The ledger is read first, for the claimed epoch. With rows, the fire is a
  continuation and uses the stored cap, not a recomputed one. A continuation
  whose prior starts already reach the cap skips capacity and the candidate loop
  and goes straight to the guarded advance. Otherwise the `capacity_room_needed`
  admission check is skipped (the fire was already admitted), today's room bounds
  only the new starts, and a room of 0 is a quiet hold (`errOnceFireHeld`): no
  transient warning, no advance, a partial summary that records the observed
  capacity, and a retry next tick. The summary's dedupe ignores the volatile
  in-flight and room numbers, so a long hold writes once and its stored numbers
  are those of the first held tick (the "Last run" view may show a stale
  in-flight count). With no rows, the cap and room-needed check are unchanged.
- A continuation's scan limit is the stored cap plus the backfill headroom,
  whatever today's room. Recorded candidates are skipped before the active-run
  check and the forge fetch and add no skip; prior starts are folded into
  Started from the ledger's stored title, URL and `run_uuid` (so an issue that
  has since left the selector still counts); the cap break counts prior plus new
  starts. `Matched` stays `len(Started)+len(Skips)`.
- The admission sentinels: `ErrOnceFireStale` stops the fire without
  advancing; `ErrOnceFireCandidateRecorded` and `ErrOnceFireCapSpent` mean the
  slot is already spent and add no skip, and `ErrOnceFireCapSpent` takes the cap
  break; `ErrOnceFireIdentity` is a bug guard, logged and treated as transient,
  with nothing created.
- Gate contention after partial progress now returns the error, so the fire is
  not advanced and retries next tick, the same as contention with nothing
  started. A best-effort partial summary is written first. The partial summary
  is `last_fire` only and never appended to `recent_fires`; it is skipped when
  equal to the stored one; its failure never blocks the retry. The row staying
  `active` and due is the visible retrying state.
- A scheduled one-time sweep whose context is cancelled mid-fire (shutdown,
  rollout) returns the context error, checked before each candidate and after
  the loop, so the row stays due and is not advanced; the next tick resumes from
  the ledger. It logs the transient warning and writes no partial summary, so
  `last_fire` shows this attempt's starts only after the next write. A
  cancellation inside the consumer-gate wait instead surfaces as gate
  contention and takes that arm, including its best-effort partial summary.
- The mid-fire credential-disabled and stored-override arms judge this attempt's
  starts: with none, they hold or return as with no starts at all; otherwise
  they record the skip, stop and advance as today.
- Before the advance, Started is rebuilt from the ledger under the summary
  lock, so a lost-ack start that surfaced as `fetch_failed` appears exactly once,
  and the skips of recorded issues are dropped so `Matched` stays exact. If that
  read fails, the fire does not advance.
- Once-sweep summary writes use a detached 5-second context inside
  `WriteOnceFireSummary`; `advance`'s existing 5-second detach becomes
  recurring-only. `errOnceFireHeld` has its own branch in `process`, after the
  credential-hold check and before `advance`. A held write's dedupe compares
  against the row read under the lock, a failed ledger read skips the held
  write, and after re-enable the fire resumes with only its remaining slots.

**Accepted boundaries.**

- A one-time PATCH must carry a future `run_at` (`validateScheduleConfig`'s
  once arm, `handler/schedules_request.go`), and a pending partial fire is
  already due, so any owner edit of it moves `run_at` and starts a new
  occurrence with the full cap. That is the owner's explicit new fire, and it
  may start a finished candidate again. A partial fire parked at `error` is
  revived only by such an edit, with the same result.
- The mid-fire credential-disabled and stored-override arms still record the
  skip and advance once this attempt has started runs, dropping the remaining
  slots. That is today's behaviour, kept to bound this change to gate
  contention; the ledger would allow holding instead, which a later change may
  adopt.
- `process`'s owner-pause and credential pre-checks still run first, so even a
  cap-spent continuation waits for those holds to clear before it advances.
  Nothing is lost.
- A summary may name a run that has since been deleted; the last-fire view
  degrades to "Current state unavailable" (`CurrentStartedRun`,
  `web/src/components/LastRun.tsx`). This is already true of today's `last_fire`
  and `recent_fires` summaries.

**Rollback limitation.** The trigger keeps occurrence invalidation correct when
an older binary writes the row. It does not make an older scheduler read or
respect the ledger. While an old scheduler binary runs it ignores the ledger: it
can exceed the cap when resuming a partial one-time fire and advance normally,
with no transient error involved. After roll-forward, the ledger is
authoritative only for the admissions the new binary recorded; runs an old
binary started in the same epoch have no ledger rows, so that epoch's cap and
per-issue dedup can be exceeded and a finished candidate restarted. Running the
second migration's Down destroys the ledger and every recorded `run_uuid`; a
later Up recreates empty tables.

**Retention.** Header and ledger rows of earlier epochs persist until the
schedule is deleted. Deleting the repository also cascades its ledger rows
through `repo_id`; this is an exception to retention until schedule deletion.
Otherwise there is at most one header and min(cap, matching open issues)
ledger rows per admitted epoch (with no cap, the matching open issues), and one
epoch per owner re-arm. No sweeper is added.

**Not claimed.** There is no e2e coverage of the scheduled retry, the guarded
advance and park, or the serialized summaries: every e2e schedule phase fires
through `schedule run-now`, which this change leaves alone. The evidence is the
PostgreSQL 17 and unit suites. No power-loss durability is claimed. There is no
cap or per-issue dedup protection for runs an old scheduler binary started,
whether during its run or after roll-forward, and none after a Down of the
second migration.

### Reopen semantics (narrowed)

A reopen that the forge already reflects before the observation keeps the ref.
A reopen after the observation is not promised recovery: the forge has no
compare-and-set on issue or MR state, so an issue reopened between the forge
answer and the `retiring` commit is not seen, and the decision is final once
committed. The interval is bounded by the 15 s maximum observation age.

### The retire lane is production-disabled, and why

`terminalRetirementEnabled` is a constant `false`, not a setting. While off, a
due `live` or `retiring` row is only pushed out (one scheduling write; no forge
call, no lifetime read, no delete), so every terminal ref is retained in this
delivery. Enabling it needs work that is not built: a complete protection
list and indexes for the protection query (slices C and D). The
consumer-admission fence is built (T3 above): admissions always take its shared
lock, which contends with nothing until the retire lane runs. A constant keeps
the irrevocable delete unreachable by configuration error until those exist. Tests enable the lane through an unexported hook only.

### Gate scoping: the completed-release gate

`verifyCompletedPublication` (`api/internal/workersvc/completed_publication.go`)
gains one scoped precondition before the `ReleaseCompletedPublicationHold` write. Only the branch-only completion release is
gated, and only when a terminal contribution exists for exactly the completing
generation:

| Contribution state | Outcome |
|---|---|
| `published`, `recorded` | release proceeds |
| `prepared`, `invoked`, `publish_unknown` | refused `terminal_preservation_pending` |
| `refused` | refused `terminal_preservation_refused` |
| any other state | fails closed as `ancestry_unknown` |
| no row | behavior unchanged |

The owner's discard is never gated. A contribution for another generation does
not gate this release.

### Failure model

Covered: API process crash, lost connection, timeout and lost ACK. This relies
on PostgreSQL defaults (fsync on, synchronous_commit on). There is no storage or
power-loss claim beyond that, and remote durability is the forge's.

| Crash point | Recovery |
|---|---|
| Before tx1 | No row, no send; a retry starts afresh |
| tx1 commit ambiguous | The request does not send; a retry reconciles if the marker is durable, otherwise it may win afresh and send once |
| After tx1, before tx2 | Reconcile |
| tx2 ambiguous | Reconcile |
| After the observation, before tx-R1 | No state change; a fresh observation next time |
| After tx-R1 | `retiring`; only the delete step repeats |
| Delete done, before tx-R2 | `retiring`, then list, then deleted or CAS retry or abandoned; never live |

### Removal guards

Repo and forge-connection removal return a separate 409 while pointer-bearing
terminal refs exist (`api/internal/handler/forge.go`), naming up to five refs
and the total. A race that reaches the RESTRICT FK maps to the same 409.

## Consequences

- **Dark delivery.** No shipped worker advertises the capability (there is no
  server-side switch: the capability is worker-reported), so no row is created,
  no ref is pushed, the gate never fires and the sweep lists zero rows. The
  schema, broker, route, sweep and gate are exercised by tests, not by
  production traffic.
- **Every terminal ref is retained** once the caller exists, until retirement
  is enabled. Retained refs block repo and connection removal.
- **Accepted limit: no owner escape for a removal-blocking ref.** Publication
  itself creates the blocking live pointer, so an owner escape (with CLI) is a
  prerequisite before any slice enables the publishing caller, not only before
  reclaim.
- **Accepted limit: a reopen after the observation is not recovered.** See the
  reopen semantics above.
- **Accepted limit: `published` is declared coverage only.**
- **Independence from custody** means neither `SettleRetainedCheckpoint`, the
  checkpoint supersession path nor the held sweep touches this namespace, and
  `validManagedRef` does not admit it.
- Finished rows are not purged yet.

## Deferred (tracked on #4007; none of this is shipped)

- **B**: worker capture and publication (the caller of the route), omission
  detection, and teaching the agent decoders the two new completion reasons.
  B is partially built and production-dark (see "Slice B (partially built,
  production-dark)" below); the remainder is deferred. B must not advertise `recovery_terminal_preservation_v1` before D's owner
  escape for removal-blocking refs lands.
- **C**: reclaim and custody.
- **C/D**: retirement enablement: a complete protection list (the
  consumer-admission fence is built, T3 #4488), the indexes for the
  protection query, and then flipping the constant.
- **D**: activation, and an owner escape for removal-blocking refs, with CLI.
- **E**: fleet rollout.
- **Purge** of finished rows.

### Slice B (partially built, production-dark)

Slice B is the worker half: the producer that calls the route. It is built
partially and is production-dark: the code below exists and is tested with
fakes and real Git, but it is behind a compile-time-off gate, so none of it
runs in production and nothing here is shipped behaviour. This section
supersedes the Deferred "B" bullet for the parts it scopes in. Passive decoding
of the two new completion reasons is the one part that is not gated (see
"Protocol decoding").

- **Gate.** One compile-time constant, `TERMINAL_PRESERVATION_ENABLED = false`
  in `agent/src/terminal-preservation.ts`, is the first check of every new
  entry point, before any inventory, quiescence, Git, private ref, record,
  HTTP or completion-head work. Only a constructor-only test option enables
  it; `main` never passes it. No config, env, chart, server feature or agent
  input reaches it, and the worker does not advertise
  `recovery_terminal_preservation_v1`. `TERMINAL_ORIGIN_ADMISSION_ENABLED`
  is untouched.
- **Narrowed scope.** Only `branch_source` runs (issue, mr_rework and
  self_improve without an egress profile) are preserved. Every other kind
  gets an output-contract label and is kept unchanged. A label is not the
  contract analysis: the report-ACK and no-push proofs for the kept kinds,
  and the terminal sends with no in-lane seat (permanent failure,
  rate-limited, cancel, iteration cancel, owner cancel), are deferred to the
  named follow-up "#4007 slice B2-contracts".
- **Kept, not proved.** A retained predecessor (writer exclusion is #4001's),
  a present non-current attempt clone, a held replay root not reachable from
  the frozen source, a clone-local ref that is not an ancestor of HEAD, a
  stash, a linked worktree, and incomplete private refs after a crash. B
  deletes nothing; removal of incomplete private refs is slice C's.
- **Still deferred.** The T4 shared excluded-origin fence, D's owner escape
  with CLI (including clearing a `localBlock`), one-worker activation and
  startup disable (including upgrade-time pending journals), retirement
  enablement (the T3 consumer gate is built, #4488),
  reclaim and custody (C, including G12), fleet rollout (E), hosted
  acceptance, the named follow-up "#4007 slice B2-contracts" (the kept labels'
  output-contract analysis and the G7 callers) and the #4001 predecessor
  exclusion.

#### Source universe and the feasibility gate (M3)

`agent/src/terminal-inventory.ts` composes, read-only, the run's complete
local source universe: every repo bare (`listRunBares`, strict), the clone
inventory, the attempt ledger, journal, stopped-marker and protected
attribution (`readTerminalAttribution`), owed roots, and the run's
authenticated recovery records. It refuses (keep) on any unknown read,
contradiction, duplicate, record or bundle outside the universe, foreign bare,
cap or deadline, any present attempt clone other than the current flight's
own, any retained predecessor, and any clone-local ref (`origin/*` included)
that is not an ancestor of the clone HEAD, a stash or a linked worktree.
Tracking state and reachability from bare refs are never evidence.

The clone is agent-writable, so the clone-local ref ancestry read runs in the
hardened proof-read view: grafts (`GIT_GRAFT_FILE=/dev/null`), replace objects,
the commit-graph and the multi-pack-index are disabled, and any `rev-list`
stderr refuses. A forged `info/grafts` file or commit-graph cannot fake
ancestry. A ledger attempt of the run that is not retired or abandoned and
whose clone is gone refuses (`ledger_attempt_clone_missing`); a retired or
abandoned attempt may be gone and contributes only owed roots and its bare. The
same attempt ID under two branch keys of one bare, a journal attempt ID that
differs from the ledger attempt at the same clone path, and a duplicated
abandoned manifest entry all refuse.

Record binding. A record binds to the proved source (the current present
attempt) when it is on the same branch and its `bareDir` equals the current
bare's directory name. A record without a `bareDir` binds only when exactly one
bare exists (it then cannot belong to another bare) and the branch matches. A
record not bound to the current source is accepted only when its `sourceSha`
is an explicit root of the universe (the head or an owed root) and it matches
another ledger attempt of the run by branch and by the bare that attempt lives
in; otherwise it is `record_outside_universe`. Only records bound to the
current source contribute generations. The abandoned-entry duplicate check
keys on class and source, so one source may carry both an `ignored` and a
`cold_dirty` entry.

The inventory and the attribution are read under separate bare-lock
acquisitions, so the universe is two snapshots, not one. The later in-boundary
recheck at freeze and before invocation (M5/M6) re-reads both.

Known limits:

- The attempt ledger carries no generation. A source's generation is parsed
  from its attempt ID, and bound records report their own generations; a bound
  record whose generation differs from the current attempt's is reported, not
  refused.
- Records carry no attempt ID or clone path. A record binds by branch, bare
  and (for an absent attempt) an explicit root, as above.
- The existing inventory reports any other run's attribution in the shared
  bare as a foreign owner. The universe refuses on that, so in a bare shared
  with other runs it is unproved. This is over-conservative (keep) and must be
  revisited before any activation; it is not narrowed in slice B.

Feasibility gate. The test "F-gate: an inventory-guarded single-attempt issue
flight is proved end to end (clean tree; an untracked file does not change the
universe)" in
`agent/test/terminal-inventory.test.ts` runs real Git, a real `GitCache` and a
real `RecoveryCoordinator`.

- Fixture evidence: one bare found by `listRunBares`; one live ledger attempt
  and its journal binding; one owed root; one authenticated inventory-guarded
  recovery record bound to the attempt (generation 1); one proved source with
  the clone HEAD and `dev:ino`; roots `current_head` and `owed`; label
  `branch_source`; the omission check accepts the matching manifest and
  refuses it with a root omitted.
- Inferred, not yet evidenced: that the read happens at the joined boundary
  before the terminal report, the capture limits and dirty capture (the
  universe does not read dirty content), and egress binding (the claim carries
  no egress marker, so it is a caller-supplied input).

#### Protocol decoding

The agent decodes `terminal_preservation_pending` and
`terminal_preservation_refused` as completion-release refusals at three sites,
and the strict route client (`publishTerminalPreservation` in
`agent/src/client.ts`) decodes the route's responses. Decoding is passive: it
needs no gate and starts no work. Retryable refusal reasons are unknown, not
final; only `published` at the tip or `recorded` is a final outcome.

#### Capture (`GitCache.captureTerminalSnapshot`)

Capture runs bounded pre-staging refusals before any object is written. A
credential-path denylist is defence in depth only; the control is the secret
scan over the published range. Dirty work is committed through a private index
(the clone's `.git/index`, HEAD, branch and other refs are untouched), and the
staged tree is validated before the commit. Capture refuses a symlinked
objects directory, a `commondir`, and foreign alternates (only a bare-pointing
alternate is admitted). The snapshot's parent must be on
`refs/uzi-runner/<branch>` in the bare. The result is fetched into the bare
through a staging ref (`refs/uzi-incoming/<uuid>`) and then pinned at a
private ref `refs/uzi-terminal-local/<run>/g<gen>` (in the clone while
transient, in the bare once private), and a post-fetch reachable-object check
runs. `buildTerminalAggregate` builds the aggregate over the captured roots.

The clone's real index is agent-writable, so it never decides the verdict or
stages anything. After the real-index enumeration and per-file checks, and
before any `add`, a private index is built from HEAD (`read-tree` writes no
object) and `status` runs on it with the same pins. Its changed-path set must
equal the enumeration's, else the capture refuses `index_mismatch` (a forged
stat cache or entry oid hiding an edit, even a credential or over-limit file,
therefore refuses before its content is hashed into the object store). A clean
tree is empty on both and returns with no `add` and no `write-tree`; a dirty
tree stages into the same private index. A file can still change between its
`lstat`, the preflight and the `add`; the staged-set, mode and size checks and
the tree-diff backstop bound that, and the secret scan is the content control.
The pins include `core.excludesFile=/dev/null`, so a global excludes file
cannot hide untracked work.

Cleanup is bounded. The staging-ref delete and the private-index removal share
one allowance: the earlier of 2 s (`TERMINAL_CAPTURE_CLEANUP_MS`), the lane's
`cleanupBound.deadlineAt` (when the lane has one; absent means no caller
deadline) and the boundary's soft deadline, cancelled by
`cleanupBound.signal` or the boundary signal. It is never bound to the
capture's own signal or deadline, which have usually lapsed. Anything not
confirmed removed is reported as `remnant` on the result: the staging ref as
`retained` (confirmed present at the commit, no delete launched) or `unknown`
(no read was possible, the ref was seen at a different oid, reported with both
the observed and expected oid, or a launched delete was cancelled or failed
unconfirmed), and the private index path when the index file's own removal did
not run or was unconfirmed (a failed lock-file removal alone is only logged). Nothing retries after the call returns. An error the boundary
marks `unsafe_cleanup` propagates from every internal handler and both public
catches instead of becoming a refusal, after the remaining cleanup steps ran.

#### Pinned preparation

`prepareFrozenHeldPinned` (see ADR-2545 amendment) prepares over a frozen
range: it takes an opt-in `perCommitCi` check (`changedCiPathsPerCommit`,
under a cumulative budget) and returns the scan digest and transport digest
(`terminalScanDigest`, `terminalTransportDigest`). The range is persisted with
its presence preserved, and retries never re-resolve it.

#### Operation records and the ordered engine

Operation records (`agent/src/terminal-operation.ts`) live under
`<dataDir>/terminal-preservation/<runId>/`, outside the recovery run directory.
They are MAC-bound (the recovery key over a domain-separated MAC input), merge monotonically, append
invocations only, grow a `localBlock`, and are immutable once closed. Every
write is validated by the decoder before it lands, so a record stays decodable.
One operation owner at a time, and the journal lock is never held across the
network. Before any send, the engine scans all the run's records for a
secret latch. A `generation_bound` or `prior_tip_mismatch` response appends a `conflict`
invocation and adds the `server_conflict` local block. The secret latch is written
before the flagged commits are. Rediscovery runs on the boot recovery pass
(gated), never sends and never deletes: a
`capturing` or `captured` record found on restart becomes `incomplete`, with
its private refs retained. Its journal-lock waits are bounded by the remaining 30 s
rediscovery budget and the caller's signal; a waiter that gives up forfeits its turn and
never acquires later, while a write that already holds the lock completes. The invocation
ledger admits an `invoked` entry only while room remains for it and its outcome (256
entries), so a record left at 255 entries by a crash-dangling `invoked` stays prepared and
is never resent (accepted limit).

#### Callers

All callers sit behind the gate. The in-lane send and the head decision run in
`finishCommittedPublish`, and `journalTerminalReport` runs in place. The
reap-body seat is bounded by its permit, with a proportional reserve (the
lane's cap is 120 s, the reserve is 30 s or a quarter of the permit, whichever
is smaller, and a window under 5 s starts nothing). The completion head is
attached only for a `published` or `recorded` manifest root. A held V2
replay/resume runs inside `inventorySourceBoundary` before any held FINAL; a
non-final outcome keeps the hold and no hold is reopened.
When the claim names no default branch, the lane resolves it inside its own window and signal; a lookup that ends after the lane window lapsed or its signal aborted keeps the output (`default_branch_unresolved`) rather than assuming `main`. The lookup is a terminal-only strict reader (`GitCache.terminalDefaultBranch`; the legacy `defaultBranchName`, which reads any failed git read as an absent ref, is unchanged for its other callers) with three results: a resolved name, proven absence, or unknown. Any read that fails, is killed, times out or overruns its output, a ref git reports as broken or unreadable (a failed read or a `for-each-ref` warning), a dangling `origin/HEAD` (legacy falls through to `main`), a dangling candidate symbolic ref, and a detached HEAD with no common branch are all unknown, and unknown (or a throw) keeps the output as `default_branch_unresolved` even when time remains; a lookup that ends outside the window keeps through the lane's window check above. Because git itself reads some broken storage as a missing ref without any error, the reader also checks the loose ref paths of `origin/HEAD` and every candidate: a symlink, an empty directory or other non-regular entry, an unreadable path, a non-directory ancestor directory (other than the `refs/heads` stub file of a repository that stores its refs in a `reftable/` directory; a live symlinked ancestor also keeps), an `origin/HEAD` that is not a symbolic ref (loose or packed), or a loose candidate file git neither listed nor read as symbolic is unknown too (the legacy lookup reads these as missing). A non-empty directory at a candidate path is a ref namespace and proves only that the candidate has no loose file. Only proven absence falls back to `main`: no `origin/HEAD` symref, a symbolic unborn HEAD, no symbolic candidate, no listed candidate, and every read an ordinary exit; the default branch's tip read in prepare runs under the same window and a read cut short by it keeps the output as a prepare `deadline` before the held-pack preparation starts.

#### Failure model and what is not claimed

The failure model is process crash only. No storage or power-loss durability
is claimed.

#### Named gaps and kept classes (not shipped)

- **G5, G7 (excluded sends).** Permanent failure, `rate_limited`, cancel,
  iteration cancel and owner cancel have no in-lane seat and are not
  preserved. Deferred to "#4007 slice B2-contracts".
- **G8.** The capture-scope allowlist is not defined.
- **G9.** A present non-current attempt clone is kept, not proved.
- **G10.** Held replay roots not reachable from the frozen source are kept.
- **G11.** Clone-local refs that are not ancestors of HEAD, stashes and linked
  worktrees are kept.
- **G12.** Incomplete private refs are retained until slice C removes them.
- **Foreign-owner over-conservatism.** See the known limits under the
  feasibility gate.
- **Unreferenced objects.** A malicious pack can leave unreferenced objects in
  the bare. The shared fetch-back class is pre-existing in `fetchAgentBranch`
  and is tracked separately.
- **Untracked directories.** Replay refuses clones with untracked directories,
  because dirty extents use plain status.
- **Dirty snapshot not in the manifest.** The dirty snapshot is in the
  published tip but is not declared in the manifest.
- **Workers without attempt paths.** A non-Docker worker without attempt paths
  skips with `source_unknown`, and would lose `completion_final_head` once the
  gate is activated.
- **Egress profile.** The worker does not see an egress-profile marker on the
  claim, so the lane passes `egressBound: false`; the exclusion holds only
  because egress-profile runs go to the isolated lane.
- **Reap-body window.** At most about 22.5 s (the 30 s boundary deadline less a
  quarter-of-remaining reserve), for Claude and Codex alike, after the reap and
  quiesce time already spent.
- **(a1)/(a2) without held capture.** These fall back to the 120 s cap and are
  not bounded by the shutdown deadline.

### Review corrections: preparation and a proven local no-send

Terminal preparation checks the existing 1 MiB discovery limit before fetching
bases and before an invoke marker can be committed. This preflight does not
freeze remote refs: a later advertisement change can still leave a send or
read-back ambiguous, which continues to reconcile without another send.

If the request that committed the marker then suppresses both send functions,
its definitive local no-send is recorded as `refused` with `send_suppressed`.
A create settles to `not_published` and clears its live pointer. An update stays
`live` at the prior tip and keeps its first-publication time. A crash before
recording this evidence, an ambiguous database write or any actual interrupted
send retains the existing reconcile-only behavior.
