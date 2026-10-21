---
title: Recovering unpublished work
order: 113
audience: user
---

# Recovering unpublished work

Sometimes a run commits real work but fails at the very last step: a
workflow or secret preflight check, a rejected push, or exhausted base
alignment against a moving default branch. Before this failure can happen,
uzi captures the run's **original committed history** — exactly what the
agent committed, not a diff or a rebased copy — into a durable, encrypted,
owner-only Git bundle stored in the database. This is your recovery path
even after the worker and its disk are gone.

uzi captures at other boundaries too, not only at final failure: when a run
is parked (a usage limit or a recovery wait) or gracefully stopped while it
holds committed work, uzi archives that exact work before its source can be
torn down. For guarded generations, release normally requires final acknowledgment
of a covering, prerequisite-free archive or a verified empty inventory.
Eligible completed issue, MR-rework and self-improvement runs can instead
release their exact completing-generation hold after the API verifies the
published final commit against the run's branch and MR (see below). Legacy generations can release on proof that their
recorded work is published. If the boundary cannot verify and resolve the
source, it keeps the source held for you to decide (see below). A hard
crash with no shutdown window — a killed node, an out-of-memory kill — is
outside this guarantee.

An eligible run that ends `failed` or `cancelled` first tries a different route:
uzi publishes its held work to a run-scoped ref on your forge,
`refs/uzi-held/<run-id>/<generation>`,
and releases custody against that ref. The archive above is then the fallback
when publishing is not possible. See
[Publishing terminal held work](#publishing-terminal-held-work).

This is a safety net for a run that already produced real commits, not a
guarantee against every possible failure. It does **not** cover uncommitted
files (except the work-in-progress commit a terminal run's held ref carries), the SDK transcript, the worker's HOME directory, or a worker that
died before it reached this protected boundary.

For operators investigating checkpoint acknowledgement failures, see
[Checkpoint publish diagnostics](./checkpoint-publish-diagnostics.md) for the
HTTP evidence boundary and a deployment comparison procedure. An unknown
acknowledgement does not prove that origin rejected the update.

## Memory-pressure holds

With the [proactive memory guard](./worker-setup.md#proactive-memory-guard)
enabled, allowance exhaustion, uncertain drainage or unconfirmed outcome
reporting invokes capture and attempts to park the affected run at `recovery_wait` with cause
`worker_memory_pressure`. An accepted park is **owner-held**, with no automatic
retry time. An unconfirmed capture or park retains local custody. It is separate
from recovery after an actual OOM kills a worker.

1. Check the run's intervention outcome and current recovery archive status.
   A historical **Available** report may be outdated: an archive can expire or
   be discarded. Verify availability by exporting it. Neither that report nor
   an export proves that the latest work was captured, that the command drained,
   or that the memory hold's custody may be released.
2. Resolve the memory/custody problem, then use owner **Resume** to queue a
   fresh memory episode. The previous episode's persisted maximum and spent
   allowance survive ordinary redispatch; Resume renews the memory episode
   subject to remaining run budget, custody, claim and cross-check lifecycle
   fences. Unknown drain evidence remains retained across Resume.
3. Use **Cancel** to end the run instead. Cancel is terminal; it does not
   prove retained work has been archived or authorize deleting uncertain
   local source.

Credential changes, Codex account changes, budget extension, approval,
follow-up and pause do not release a held memory episode. Extending a timed
run can supply budget for a subsequent owner Resume, but does not itself
queue the run. An available archive does not automatically release this
hold's retained custody. After discarded/released precedence, an OPEN hold at
`recovery_wait` for either `worker_memory_pressure` or
`worker_requeue_exhausted` reports `needs_action` when its latest capture failed,
otherwise `source_only`, before available unguarded archives or capture progress.
Other custody attention decisions retain their existing ordering.

The worker attempts preservation using its existing recovery path. Failed
or uncertain capture keeps local source custody; there is no archive or
latest-work guarantee, and loss of that worker's retained storage can lose
uncaptured work. Confirmed drain, a resolved outcome and fresh usage below
the quiet threshold for the configured dwell are required before another
intervention/retry. Transport retries do not provide an exactly-once
delivery guarantee.

Source boundaries: [captured-context reconciliation](../agent/src/runner.ts),
[memory reservation and outcomes](../api/internal/workersvc/memory.go),
[owner Resume](../api/internal/store/queries/memory.sql) and
[custody release](../api/internal/store/queries/recovery.sql). Automatic and
credential-driven promotion exclude this cause in the
[runtime queries](../api/internal/store/queries/runtime.sql).

## Completion publication holds

An interlocked issue run can hold before push when a non-closing delivery
contains a commit message that closes its source issue, or when its current
completion projection cannot be confirmed. The feed names up to five
offending SHAs and a remaining count, or reports unreadable messages without
a SHA. Unknown projection uses fixed copy without commit content.

1. For unpublished commits on a declared or owner-partial run without Stop,
   use `uzi run decide <id> --continue --guidance "<text>"`. Name the SHAs
   and ask the lead to reword its own unpublished commits. The worker does
   not rewrite history or force-push.
2. For published history or a Stop directive, cancel and manually deliver
   the preserved work through the recovery paths on this page. Stop resumes
   into finalization without another lead turn. Partial and accept decisions
   cannot repair commit history.
3. Check the available recovery evidence before replacing the worker.
   Pre-push holds skip remote checkpoint publication. If bounded capture
   exhausts, a degraded hold can record an empty head and retain the clone
   and session without a verified recovery copy. Latest work may exist only
   on that worker; an older archive does not prove it contains that work.

An accepted or ambiguous hold does not produce a failed terminal report;
a definitive hold refusal can fail. An uncertain completion-attempt report
still proceeds to the hold; stale claim ownership stops the flight while
retaining its source. See [Completion holds](run-completion-hold.md) for scope
decisions, plain-resume refusal and version limits.

## Credential-switch holds

When you switch the token of a run that still carries retained recovery work
([Anthropic tokens](anthropic-token.md#where-you-can-pick-it)), the worker
first proves the run's own processes stopped before it reports the switch:
a guarded command settle, a kill of the agent's process tree, a reap of
processes attributed to the run's HOME, and a quiescence check, all within a
bounded allowance. If that is proven, the switch releases as usual. If it is
not, the run is held for you. The hold is `paused` and keeps the run's work
and custody on that worker:

- **`credential_switch_settling`**: the worker is still stopping or joining its
  own operations. **Resume** and `uzi run set-token` are refused with a 409
  until the hold settles; **Cancel** always works. The CLI shows `credential
  switch settling`, the web shows **stopping processes: token switch**.
- **`credential_switch_unsettled`**: the worker has acknowledged and the run
  is held for you. The CLI shows `credential switch unsettled`, the web shows
  **held: token switch**. **Resume** requeues the run on the token you chose
  (the same worker is preferred); `uzi run set-token` saves a token and
  resumes. **Cancel** ends the run. The feed message and diagnostics name any
  survivor class (`unreadable`, `worker_child`, `restarted`, `kill_failed`).

Settling lifts only on the worker's acknowledgement (fenced on the worker,
run, claim generation and registration nonce), the worker's heartbeat going
stale, the worker's removal, or the worker re-registering. Elapsed time never
lifts it. Resuming after a lift does not skip any check: the next claim still
runs its fail-closed predecessor quiescence proof, and a survivor that stays
unverified still ends the run `worker_residue_blocked` with custody kept.

This does not cover every switch on a retained run. In three cases the worker
stops non-terminal as before, without a hold: the API lacks the
`credential_switch_hold` protocol feature, the hold report is refused or stays
unconfirmed after one retry and an ownership read, or the retained flight's
ownership reads `awaiting_*`. The next claim's proof stays fail-closed, so the
worst case is the previous behaviour. In a narrow race, a refused `set-token`
can still save the token you chose. Design rationale:
[ADR-1247](../adr/1247-per-run-token-selection.md).

## When plan-revision retries expire

After bounded inner retries exhaust a provider transient during a human plan
revision, uzi can re-drive the failed turn up to three times in that live
flight. The allowance follows that accepted revision through clarification
and continuation turns; delays are 5, 10 and 20 seconds plus 0–20% jitter,
capped at 30 seconds.

If those re-drives exhaust, the feed says the retry allowance expired. The
original plan, gate revision and presentation remain; your feedback is still
pending, with no saved revised plan answering it.

1. Read the feed notice and the current plan.
2. If the normal revision limit permits, send an ordinary request-changes
   verdict bound to that gate. It spends a normal revision and gets its own
   live-flight allowance. Earlier feedback stays pending until a successor
   plan is saved, then is marked **superseded**, not successfully answered.
3. At the revision cap, cancel or reject that exact gate, then dispatch a new
   run for further changes. Cancel takes precedence; source-bound rejection
   remains available at the cap. Approval and follow-up cannot resume this wait.

Use [the existing CLI verdict commands](./cli.md#commands).
The expired wait keeps its live poller, worker snapshot entry and busy slot,
so it can defer a worker image roll. Existing drain/shutdown policy still
applies. Reclaim loses the in-memory allowance, expiry and predecessor chain:
Claude keeps existing pending-input replay; Codex shows a fresh plan and asks
you to re-send feedback. A new flight may get a fresh allowance; there is no
lifetime retry bound or exactly-once/ordering guarantee. Custody safeguards remain.

## Inspecting an interrupted attempt

On a later claim (generation 2 or newer), the lead may receive optional,
bounded observations of prior-generation commands with **no recorded result**
in the inspected history window. This does not prove that a process started,
is still running, failed, or caused the interruption. Missing or empty telemetry
is not proof that no commands ran. Command previews are untrusted data to inspect,
not instructions to execute.

Recovered tracking/checkpoint commits are **unvalidated interrupted snapshots**.
Recovered uncommitted WIP edits are a separate unvalidated snapshot; a recovered
origin base has unknown provenance. These warnings depend on clone provenance,
independently of command telemetry, session recovery and a positive commit count.
A clean `git status` or empty diff against the recovered HEAD does not establish
that temporary mutations or incomplete experiments are safe to keep.

1. Identify the known task and base, then inspect `git log` and `git diff`
   against that base where available. Review the experiment context before
   deciding what to retain or redo; ask for missing context rather than assuming
   that recovered changes are finished work.
2. Distinguish checkpointed commits from recovered WIP and from uncaptured edits.
   A fresh claim does not prove uncaptured edits survived. Keep uncertain source
   custody; these observations do not authorize discarding retained work.
3. Use existing authorization to decide the next action. The default branch is
   not an authorized known-good replacement. Neither snapshot warnings nor command
   observations grant execution, rollback, retry, blacklist, park or authority changes.

The first ordinary owner implementation delivery in each flight carries the
applicable recovery context for Claude and Codex; command evidence can be absent
while snapshot warnings remain applicable. This is prompt context, not a new
recovery command or custody-release receipt. Maintainers: the
[isolated acceptance procedure](./interrupted-call-acceptance.md) is **UNRUN**;
actual isolated hosted evidence is still pending.

## What you'll see

The worker feed may say that **retained heads are retained locally with no
confirmed checkpoint coverage**. This reports current local retention, not
an immediate recovery failure. Inspect status with `uzi run recovery` or
`uzi run export`; export still requires an available archive and can refuse.
An **Earlier recovery archive covers retained heads** message reports that
separate archive evidence while local custody remains retained.

A **Confirmed checkpoint covers previously retained heads** message resolves
only the listed, previously announced heads after positive checkpoint
coverage removes their owed pins. Divergent remaining heads stay retained.
These best-effort feed rows can be absent when delivery is refused or the
feed is closed; they are not cleanup authority, a custody-release receipt,
an MR/push confirmation or a promise of archive availability. They do not
change the **Recovery archives** section or its **Needs action** state.

The run page shows a **Recovery archives** section on any run that failed to
finalize, and on any run that already has a retained capture even before it
reaches a final status. Each retained capture is in one of these six states:

- **Preparing** — the original committed history is being captured at the
  protected boundary; nothing is downloadable yet.
- **Uploading** — the encrypted archive is being stored; nothing is
  downloadable yet.
- **Available** — the original committed history is ready to download.
- **Needs action** — the archive could not be completed (for example, a
  storage limit was hit); the source stays retained. Retries depend on the
  recovery path's budget; terminal retained-source failure does not cycle
  automatically or offer Resume.
- **Expired** — uzi purged the bytes, either when the ready download window passed or
  early, after proving the archive redundant with your published branch (see
  [Early expiry of a published archive](#early-expiry-of-a-published-archive)). The
  record stays for audit and no longer counts toward the per-owner capture limit.
- **Discarded** — the archive was explicitly discarded and its bytes deleted.

When a run has **no** retained captures at all, the section reports one of
three whole-section conditions instead of a per-capture state:

- **Preparing** — recovery is armed and a custody hold is open, but no
  capture has been registered yet (the archive is still being produced).
- **Unsupported / legacy** — the run predates durable recovery (an old
  worker or server never captured anything for it).
- **Unavailable source** — recovery was armed but nothing needed capturing,
  or the source could not be preserved.

### Failed runs that need landing

A run can finish every milestone and pass every gate, and still end up
`failed` — because the worker could not publish its branch. That happens
for a handful of reasons: a workflow-scoped file the bot can't push, a
base-alignment conflict against a moving default branch, a push blocked by
secret-scanning, or a rewritten published history. In every one of those
cases the committed work itself is usually still intact, and uzi labels the
run **needs landing** instead of a plain failure, so it's obvious a human
can land it by hand. It still counts as a failure in your run stats — the
factory didn't publish anything — the label just tells you the work is not
lost.

Where the work actually is depends on what got captured for that run: a
preserved diff, shown inline on the run page (or via `uzi run get <run-id>
--field preserved_patch`), or a recovery archive you can download — see
[Downloading an archive](#downloading-an-archive) below, or `uzi run export
<run-id> --output <path>`. When a run needs landing but neither a preserved
diff nor an available archive exists yet, uzi reports it as
**unrecoverable** instead: the same underlying cause, but nothing left to
hand a human.

A third place to look on a terminal run is its **held ref**: when uzi could
publish the run's work to your forge, the run page shows a **Held work** panel
and `uzi run held <run-id>` lists the ref. See
[Publishing terminal held work](#publishing-terminal-held-work).

Every download surface warns that the original may contain secrets: review
it before publishing anywhere, and if it exposed a real credential, revoke
and rotate it and remove it from the affected history — deleting the
archive alone does not undo that exposure.

## Retained-source failure and local proof

A claim discovers retained source before forge refresh or disk preflight
and recovers it within that claim. Its source-bound durable budget reserves at most **3 total
iterations**, blocked and nonblocked together. Each positively verified claim gets
one **five-minute deadline window**, starting at its first reservation. The first
window is fenced by the acquired server claim and charged before ownership probes.
Renewing an existing episode requires a fresh ownership proof; a lost claim leaves
that predecessor episode unchanged. A transient renewal-proof failure also leaves
the episode untouched and stops the flight without a terminal report; ordinary
bounded missing-flight requeue governs the next attempt. First-window and
same-window ownership outages still spend the charged recovery budget. Retries
and process recreation within that claim preserve the window. A later verified
claim renews only the deadline, retaining the cumulative count, source attribution
and exponential retry cap of 16 times the base delay. A legacy episode without a
claim fence binds once to a positively verified claim; its old claim identity is
never inferred. Already-blocked episodes retain their original reason and never
renew. Reset of the reservation count requires a trusted, successfully settled
real model turn. Recovery adopts work on a fresh attempt path and model
session even on an unwired worker; ordinary unwired keys that never entered
retained recovery keep same-path continuity.

Permanent blockers or exhaustion end the run **failed** with custody kept,
without automatic reclaim or failed-run Resume. The reason names the
blocker: missing source, invalid clock, preservation/adoption failure,
missing or unverifiable needed prerequisites, or a fallback still oversized.
`worker_residue_blocked` identifies an actual quiescence failure, not an
archive or prerequisite failure. Transient capture, forge and disk failures
spend the same reservation budget. These bounds are separate from healthy
provider/empty-turn parks and the owner worker-recovery-exhaustion hold.

A verified thin bundle within the cap, with its needed prerequisites locally
available and verified, permits local adoption and model execution even
when remote publication is unknown. Nonempty prerequisites alone do not
block it. Local verification reads reachable history and checks object hashes.
Decoded history above the worker's configured cap (default **8 GiB**, or
8589934592 bytes) fails closed with work and custody retained, even when the
thin archive is below 64 MiB. Verification covers the full reachable history,
including prerequisite history, and refuses missing or corrupt objects.
`uzi run recovery` and the worker log show the recorded blocker. Local proof does not establish independent recovery
or remote durability and does not release custody: predecessor sources, pins, journals
and descriptors remain until the existing verified final disposition or
explicit discard. The guarded prerequisite-free archive/empty-inventory
release conditions below remain unchanged.

The configured budget counts delivered object contents cumulatively within a
recovery operation. Successfully verified objects are cached per bare identity
within that operation; only uncached objects are hashed and charged. Nested
operations share the cache and delivered-byte counter. Every check still walks
the complete closure and checks object presence, type and size. The operation-wide
cache entry bound leaves overflow objects uncached and fully verified. A fresh
operation starts empty. Reuse assumes immutable worker-owned object storage;
corruption, replaced loose objects or pack files, or an added alternate object
store can escape later hashing within unchanged directory identities and matching
cached type/size metadata. The user explicitly accepted this additional risk on
2026-10-14. Object contents stream through a pipe
chunk and bounded header, without content buffers proportional to the cap.
Metadata listings retain the existing 64 MiB output cap (`GIT_MAX_BUFFER`);
object arrays remain bounded by that listing. The delivered-byte cap is **not a
RAM limit** and does **not** bound Git-internal delta decompression memory. The
deadline limits duration, and the shared worker cgroup does not isolate this
verifier from sibling runs. The resulting residual resource-exhaustion risk
remains deferred scope, as authorized by the human review on 2026-10-09 (#2512).

Operators can set `UZI_RECOVERY_DECODED_LIMIT_BYTES` at worker startup; see
[Recovery decoded-history limit](./configuration.md#recovery-decoded-history-limit).
The blocker shows exact cap bytes, plus GiB for caps of at least 1 GiB. A
previously blocked journal remains blocked after restart or a cap increase:
its reason reports previous decoded verification exhaustion, the current cap,
and that the prior cap was not recorded. This changes neither the 64 MiB
encoded archive cap nor archive storage quotas. Per-operation object
deduplication does not exclude any reachable history from the walk. Cancellation and existing deadlines still apply.

A healthy run with an unknown publish outcome continues in the same claim
generation, retaining its tip, owed roots and publication time gate.
Confirmed durability requires positive proof. External restarts require a
genuinely fenced successor; loss of local storage can still lose work.

For a failed source-only run, an operator must recover from retained worker
storage. `uzi run export` still requires a manifest-bound available archive;
there is no new source download API or failed-run Resume. Downgrade during
pending recovery is unsupported because older workers may drop recovery
fields. See [bounded retained-source recovery](./run-recovery-wait.md#bounded-retained-source-recovery).

## Downloading an archive

From the run page, click **Export archive** once a capture shows
**Available**. From the CLI:

```sh
uzi run export <run-id> --output ./recovered.bundle
```

- **Owner-only.** You can export only your own runs; an admin viewing
  someone else's run is refused. The download talks to the API directly, so
  it works even if the worker that ran the job no longer exists.
- **Explicit capture choice.** A run can retain more than one archive. With
  exactly one `available` capture, export uses it; with more than one, it
  refuses to guess and lists every capture's id and state so you can pick
  with `--capture <id>`.
- **Byte-verified and atomic.** The byte count and checksum are verified against
  the server's manifest before the file is written, and the destination is
  never overwritten (an existing file or symlink there is refused).
- **No partial files.** An interrupted, corrupt, or expired download exits
  nonzero and leaves nothing at the destination.

See [the CLI reference](cli.md#recovering-unpublished-work-uzi-run-export)
for the full flag and exit-code contract.

## Importing the bundle

A downloaded archive is a real Git bundle with a dedicated
`refs/heads/recovered-source` ref, importable into a clean clone of the forge
repo:

```sh
git clone <your-forge-repo-url> recovered-repo
cd recovered-repo
git bundle verify ../recovered.bundle
git fetch ../recovered.bundle refs/heads/recovered-source:refs/heads/recovered-source
git checkout recovered-source
```

Run verification inside the destination clone **before importing or discarding
retained source**. Every required prerequisite commit must already exist in that
clone. The download's byte count and checksum verify the file against its
manifest; they do not prove that those external commits are available. If
verification fails because dependencies are missing (for example after a forge
history rewrite), keep custody and recover the missing history from retained
source before importing. Do not discard the source on the strength of a checksum.

Inventory-guarded capture tries a self-contained bundle first. Only a size-cap
refusal permits a thin fallback using the exact worker-bare cached default-branch
tip and **all** its merge bases with the captured source. If a merge base equals
the source H, it excludes H's verified parents instead so H remains in the
bundle; a root H has no useful boundary and keeps the self-contained/oversized
outcome. This needs no forge PAT
and makes no fresh public-availability guarantee. The prerequisite list records
the dependencies actually written in the bundle header. Legacy capture using a
fresh verified forge tip is unchanged; restart and quarantine capture without the
explicit guarded fallback option stays self-contained. The thin fallback obeys
the same byte cap as self-contained capture. Historical oversized
captures are not automatically reproduced. Original commits retain identical
commit ids, parent relationships, trees, and binary contents.

For an inventory-guarded capture, `recovered-source` can be a **synthetic
aggregate**, with the current source tree and the retained divergent heads
in its ancestry. It is not necessarily an original agent commit, and its
tree does not combine the contents of those divergent heads. Inspect the
ancestor history and **every original root**, not just the aggregate tree, then
check out each exact original SHA you need:

```sh
git log --graph --oneline recovered-source
git show <original-sha>
git checkout -b recovered-original <original-sha>
```

The worker's retained-head feed notices and recovery journal identify the
original roots; feed labels may abbreviate SHAs, so resolve them against the
imported history. The server receipt identifies the selected archive and
its `coverage_digest`; it does not provide a new server-side root list or
prove Git ancestry. Preserve the downloaded bundle while reviewing which
original head to land.

## Limits

These are the shipped defaults. Every one below except the last is an
operator-configurable environment variable on the API:

| Limit | Default | Meaning |
|---|---|---|
| Max bundle size | 64 MiB | A larger archive is refused, not truncated. Guarded capture may fall back to a capped thin bundle; if capture still fails, the source stays retained. Historical oversized captures are not automatically reproduced. |
| Ready payload per owner | 1 GiB | Your available archive bytes plus preparing/uploading reservations. |
| Instance byte quota | 4 GiB | Available archive bytes plus preparing/uploading reservations across the deployment; a refusal retains the affected source. |
| Ready-artifact retention | 7 days | Normal ready-download window. Earlier non-final and legacy captures count from readiness; a selected final guarded archive is protected while its local worker exists, then gets this window renewed on physical worker deletion. A completed run's archive can expire earlier, only after uzi proves its commits are already on the published branch. |
| Automatic upload-retry window | 24 hours | How long uzi keeps retrying a stalled upload before it needs your attention. |
| Captures per claim | 16 | Distinct capture attempts one worker claim can accumulate. |
| Retained captures per owner | 256 | Captures on file that are neither discarded nor expired (preparing, uploading, available, needs action; any other state also counts). Expired tombstones stay for audit but free their slot. |
| Admission-counted recovery holds per owner | 8 by default; the admin setting `custody_hold_limit` (1 to 1000) | Open holds count except at most one per run backing a healthy current claim without an owner decision. Total open custody stays protected. At the admission limit, uzi pauses admitting **new** runs subject to custody admission until the counted pressure falls. Hold-free Plan and Code checker children (`kind = 'cross_check'`, both harness families) are exempt; judges and jobs gain no exemption. A requeued run with 1 to limit-1 of its own owner-scoped open holds remains exempt; at the limit it loses that exemption. This admission gate is not a strict ceiling: concurrent claims and stale heartbeats can raise pressure past the limit. Admins change the limit in [Admin settings](./admin-settings.md#custody-hold-limit); it is not an environment variable. See [ADR-2445](../adr/2445-custody-admission-accounting.md). |

A byte refusal keeps the source retained. The archive's **Needs action**
reason can read `storage quota exceeded`; current workers preserve a typed
API quota refusal as `storage_quota_exceeded` and retry with backoff.
An untyped HTTP 507 remains a generic transient failure.
`uzi run recovery` joins hold and capture identities but omits capture
reason in both human and JSON output; read the web archive or owner archive
API for that reason. A new capture can also be refused at a capture-count
ceiling before a capture row exists.

For exact limits, byte accounting, custody counts and a read-only
reproduction, see [Recovery storage diagnosis](./recovery-storage-diagnosis.md).
Admins can use [Recovery storage health](./admin-health.md#recovery-storage)
to find current persisted quota-refused captures. This warning does not
show historical refusals or measure physical disk capacity.

### Shared stored-file budget

Recovery archives and [job files](./jobs.md#job-files) live in the same database, so they share one ceiling: `UZI_STORED_FILES_BUDGET_BYTES` (4 GiB by default, the same figure as the instance byte quota above). Both stores count against it: recovery-archive bytes (ready archives, plus the declared size of an upload still in flight) **plus** job-file bytes (every non-expired job file, reservations included). It is a hard combined limit, and **recovery wins**:

- When a recovery upload would not fit the budget, uzi first reclaims job files to make room, in this order: files already past their expiry, then the oldest `available` job files (those of finished jobs). It **never** reclaims a file attached to a job that is still running, a file whose upload is in flight, or an unattached upload still inside its one-hour window. A reclaimed job file becomes expired: its bytes are deleted and a later download is a 410.
- Admission itself deletes nothing: it only checks that enough reclaimable job-file bytes exist. The reclaim runs once the upload's size and checksum have verified, and only for the bytes that upload actually delivered, so a worker that declares a size and never sends the bytes cannot expire anyone's files.
- Shared-budget admission refuses if reclaimable job bytes cannot cover the declared excess. After size/checksum verification, bind sizes reclaim from committed bytes plus the verified archive size, excluding reservations; it then checks committed bytes after reclaim plus reserved job bytes plus the verified size. That later check can also refuse, rolling back reclaim and the upload. Job files do not reclaim recovery archives; a job-file upload over the budget is refused 507 `storage_quota_exceeded`.

The owner and instance recovery quotas in the table above still apply on top of the shared budget. If you raise the budget or either store's quota, size the database volume for the budget plus your ordinary data ([Configuration](./configuration.md#job-files-and-product-skill-sets-prd-1909)). The design is recorded in [ADR-1909](../adr/1909-stored-file-budget.md).

## Custody: why a worker won't disappear

While a run's committed work is unpublished and not yet durably captured
(or already captured but not yet resolved), the worker — and its disk —
that holds it is kept around rather than torn down, even if it would
otherwise be idle or recycled. Such a worker shows a **retaining work**
badge in the worker list. Deleting that worker is refused until the work is
recovered or explicitly discarded, and the refusal names how many holds are
blocking the delete and the exact commands to clear them (`uzi run recovery`
to list, then `uzi run export` or `uzi run discard`). `uzi worker rm` never
force-discards held work for you. Removing a repo that still has a run
retaining custody this way is refused the same way, naming how many runs are
retaining unpublished work.

Each retained claim keeps its own hold, keyed to the exact claim that
produced the work, so a later run on the same worker never drops an older
claim's only copy.

## Where the work is kept

Inventory-capable workers pin each divergent unpublished head locally at
`refs/uzi-owed/<run-id>/<sha>` before replacing its tracking head. These
pins retain the original SHAs without descendant compaction. Feed notices
report **local retention with no confirmed checkpoint coverage** separately
from remote confirmation: a local pin does not survive loss of that worker's disk.
Only containment in a broker-confirmed or claim-confirmed remote head can
clear a candidate through publication reconciliation; a local tracking ref
or checkpoint bridge is not that evidence.

**Retention is capped at 64 heads per run.** A run that keeps rewriting its
branch without a confirmed checkpoint cannot grow its pins (which block
garbage collection on the worker disk) without bound. At 64 retained heads the
worker keeps every existing pin and the run's working clone, refuses to
retain or supersede another head, and stops the run through the normal
preservation-failure path: the run reports failed with
"tracking preservation refused: the retained owed-head limit (64) was
reached", even when the checkpoint that hit the limit was a best-effort one.
Retaining a head that is already pinned costs nothing, and a confirmed
checkpoint releases every pin it covers, freeing capacity. Recover the
retained heads with `uzi run recovery` or `uzi run export`. The feed lists a
retained head once: a later checkpoint or a resume on the same worker
announces only heads it has not announced, and archive notices are tracked
separately from head notices.

### When a run is stopped by a stale or fenced claim

A worker stops a flight when its claim has gone stale (`stale_claim`) or when an
input receipt fenced it. In each case the worker keeps the clone's unfetched commits and its dirty
and untracked files instead of tearing the clone down. It stops the flight's own
processes first, then makes no further report, publish or tracking write through
the stopped claim, and records the source as retained:

- **Attempt clone (Docker-wired worker):** the attempt ledger entry is marked
  `reclaimed` and the plain recovery journal is cleared.
- **Canonical clone:** a `uzi-stopped` marker is written, bound by digest to the
  exact plain journal, and that journal is cleared. The key's attempt mode is
  persisted first, so the worker never seeds that canonical path again. The marker,
  not the journal, now attributes and protects the kept clone. If the clear never
  landed (a crash after the marker write), the next discovery finishes it.

A stale or fenced stop keeps the clone even when it is clean and its HEAD is
already anchored. The worker still checks coverage, and logs the verdict as a
diagnostic only ("coverage verified (diagnostic only)"), because the check is a
snapshot: a writer that writes and exits between the status read and the final
process scan is invisible to it, and releasing the clone would delete that
writer's bytes. The clone waits for explicit operator discard. A clone counts as
covered when its HEAD is anchored (a tracking tip this run and generation owns,
an owed pin of this run, or a commit already on the worker bare's default branch,
for a flight that never committed past its seed base) and every
local ref of the clone, in any namespace, points at a commit reachable from
that HEAD or from a ref of the worker's bare repository, with no stash entry. A
commit that only the clone holds (on another local branch, under a tag or under
`refs/remotes/`) or a stash entry makes it not covered. Gitignored files are not covered by any capture, so they do not
make a clone that is otherwise anchored not covered. Accepted residuals: commits reachable
only from the reflog (for example after an agent's `git reset --hard`) are not
counted as source, and a bare ref that justified coverage can later move (a
forge force-push, pin settlement).

Coverage is also refused (`not_covered`) in these cases, which matter where coverage
still gates release (terminal exits, below); on parks it is diagnostic:

- **Populated submodule.** A `.git/modules` store, or a gitlink path that holds
  anything but nothing or an empty directory, is never covered, even with
  `ignore=all` set in `.gitmodules` or config. The status read runs with
  `--ignore-submodules=none`, and the parent proof says nothing about a
  submodule's own files, commits or refs.
- **Late writer.** If the final process proof had to kill a writer, the earlier
  byte checks do not cover what it wrote. The residual is a writer that writes and
  exits between the status read and that proof. Stopped and park exits keep the
  clone regardless of coverage. Completed, failed and cancelled terminal exits
  retain their existing covered release; that residual remains open under
  [#2852](https://github.com/vtmocanu/uzi/issues/2852).
- **Docker teardown failed.** The proof scans processes only, so it is skipped
  unless the Docker half was `not_wired` or `docker_unconfirmed`.

The coverage proof and the inventory's status reads on a kept clone take no
optional index locks, so they do not rewrite the clone's index.

A later claim of the issue (a newer generation of the same run, or a different
run) starts on a fresh path on every worker and leaves the retained clone
untouched. On a Docker-wired (attempt-path) worker that is a fresh attempt. On a
worker with no Docker host, persisting the attempt mode switches that issue's key
to fresh attempt paths: it no longer fails with `worker_residue_blocked`. The
claim re-reads the attempt mode right before seeding and retries once on a fresh
path if the mode landed late. Consequences:

- That key stays in attempt mode on that worker permanently, so later claims start
  a fresh model session; the work carries over through the tracking ref and
  checkpoints, not the old session.
- Every stale or fenced stop keeps its clone until an operator discards it, even
  a clean one the bare already holds. Attempt-ledger `reclaimed` entries stay
  until discard, so disk use grows per stop. On a worker with no Docker host,
  every stopped canonical clone (even one stopped while planning, clean at its
  seed base) persists the key's attempt mode. Stopped markers count toward the
  per-run cap of 64; past it the record is `unknown` and the clone stays under
  its plain journal.
- A dirty kept clone keeps the run's hold open, so inspect routes it to
  `owner_hold_discard`.
- The run's final inventory counts marker-kept paths. A missing one makes that
  run's inventory unknown until the source is discarded.

The source is kept in every fail-closed case, but what a later run sees differs:

- **`worker_residue_blocked`** (message naming the worker-local inspect command),
  only when marker evidence names the path or the run: the marker evidence is
  malformed or over its cap, or the journal digest bound to a marker no longer
  matches. Separately, the fresh seed's existing quiescence check finding
  unattributed processes in the key's footprint is refused the same way.
- **Nothing recorded.** If persisting the attempt mode or the marker fails, no
  marker exists. The clone stays
  under its plain journal and routes as before: the same run's next claim enters
  ordinary retained recovery, a different run goes through the existing
  foreign-journal handling (terminal-orphan handling), and inspect lists it as a
  marker-less plain-journal leftover.
- **Journal no longer names the source.** If, when the stop is recorded, the
  journal is absent or already names another source, nothing is written and that
  journal keeps its own routing. Inspect then does not list the kept clone (a
  narrow race). If the journal was absent, the next canonical claim on that key
  frees the path as before; the journal is written before any model runs, so that
  clone holds only its fresh seed.
- **Corrupt attempt mode or failed heal.** A malformed attempt-mode value, or a
  failed write while discovery completes a detachment, fails the run closed with
  "Retained recovery blocked: corrupt journal or unsafe local recovery storage"
  (the default failure origin, not `worker_residue_blocked`).
- **Residue quarantine latched.** The attempt mode and marker are still written
  and the source is reported retained, but the journal is kept. A quarantined
  worker claims nothing until its container restarts; the next discovery then
  completes the detachment.

Recovery handler exits decide once, when the handler exits. A capture taken
earlier, remembered or degraded, never counts as proof. Which record a kept clone
leaves, and whether coverage can release it, depends on the exit:

- **Exits that stop the flight** (a stopped confirm, another generation, a stale
  park report) record the source as retained, as above, and keep it even when
  covered; the verdict is diagnostic only.
- **Park release paths** (recovery, provider outage, credential-switch, disk and
  live non-terminal final parks) keep the exact clone and its existing recovery
  journal even when covered, with no `uzi-stopped` marker or `reclaimed` entry.
  Coverage is diagnostic when checked; shutdown still skips the check. A
  successful direct credential switch also keeps the clone for recapture.
- **Terminal exits** (`completed`, `failed`, `cancelled`) retain their existing
  covered release at the handler's exit. Otherwise the clone and plain journal
  stay; a kept terminal clone still follows existing terminal-orphan handling.
- An owner-authorized cancel of a non-credential run keeps its normal cleanup; a credential-deferred captured cancel is revalidated at cleanup, and a retained result keeps the clone.

On the same worker, the next claim follows retained-source recovery
(`recoverRetainedClaim`) to recapture late bytes onto a fresh attempt path and
model session. Failed or unknown recapture keeps the source. The old parked
source also stays after successful recapture and completion: recapture or a new
claim alone does not authorize deletion. Existing verified-successor retirement
applies to its exact successor where authorized; the covering FINAL path retires
that successor and reanchors the journal to retained predecessors. It does not
delete the old parked predecessor. Otherwise use explicit operator discard.
Stopped and fenced sources still require operator discard.

Keeping the old parked source costs storage, including after resume. On an
unwired worker retained recovery also starts a fresh path and model session,
instead of ordinary same-path continuity; Docker-wired resumes already start
fresh sessions. Capacity pressure does not authorize deletion. This is a partial
fix for #2852: terminal/Docker writer exclusion and safe reclaim remain open.
After recapture, parked predecessors can become abandoned attempts; repeated
disk parks can compound retained storage (#2013/#225). The retention-only,
non-closing #1858 change disables unsafe automatic retention reclaim on every
worker, including abandoned attempts and crash-window live orphans, shared
own-key residue from canonical and attempt entries, and orphaned skills.
There is no automatic reclaim escape from that accumulation.

Retained bytes, identity metadata and the safety inventory have no retention
bound. Existing per-seed process/teardown scope over non-live paths and ledger
compaction work grow with inventory; no cap skips safety checks. Compaction keeps
identities for present paths. The unchanged disk governor does not establish
writer exclusion or guarantee protection against node eviction. Active-clone
measurements are snapshots,
not marginal retention cost, a growth bound or a complete historical deletion rate.

To find these sources, run an aggregate inspect (see
[Retained sources with no open hold](#retained-sources-with-no-open-hold-worker-local-inspect-and-discard)):
it lists protected and episode-journaled sources, stopped-source markers and
attempt-ledger entries. An attempt clone (Docker-wired workers) kept by a park or
terminal exit is listed through its ledger entry. A canonical clone kept under its
plain journal (no marker) is also listed, with marker state `none`, journal state
`plain`, and can be discarded with the same digest confirmation only when the
server reports the run terminal (`completed`, `failed` or `cancelled`; see the
disposition notes below). The same run's resume on that worker still recaptures it,
and the next run's existing terminal-orphan handling still moves it to retention as
before. Park timing, report count and order, and retries are unchanged.

This is **local retention only**. It is not a published checkpoint, an archive or
server custody, and nothing about it appears in the feed or in `uzi run recovery`
as a hold:

- A restart of the same worker pod keeps the retained clone.
- Replacing the pod loses an `emptyDir` clone on the Docker tier, even though the
  bare repository's ledger entry or marker survive on the data volume. Inspect
  then reports the source as missing, never as empty.
- Tearing down the PVC loses both.

A successor's release of the old hold is not proof that the old clone's dirty
bytes were preserved; only a published checkpoint, an archive or the retained
clone itself holds them. A retained source is removed only through the
worker-local inspect and discard commands in
[Retained sources with no open hold](#retained-sources-with-no-open-hold-worker-local-inspect-and-discard).

### Guarded inventory and final custody transfer

A claim is inventory-guarded when it has `inventory_guarded: true` and the
API supports `recovery_inventory_v1`. An earlier **Available** archive or
completion alone keeps its hold **open**. The completed-publication exception
below requires a separate API proof and receipt for the exact generation. At a verified source/process
boundary, the worker freezes the unresolved inventory and constructs a
recovery-only aggregate covering the retained roots by ancestry, with one
current source tree. This aggregate never becomes the task branch or a
checkpoint, and introduces no new publication path.

For the archive path, custody transfers after the API acknowledges the final
disposition: either the exact selected available, prerequisite-free archive (capture id,
source SHA and `coverage_digest`) or a verified empty inventory with settlement
evidence. A thin archive is downloadable but does not authorize archive-backed
FINAL or source cleanup, even when it covers every original root. Without a
completed-publication receipt, holds, pins and clones remain retained until
independently verified empty-inventory settlement or explicit owner discard.
Thin-archive availability alone does not retire a pod; pod, PVC and hold costs
continue while custody is open.
The owner-readable receipt remains visible through `uzi run recovery`, and
the selected available archive remains downloadable through `uzi run export`
after the hold closes, including after worker deletion. This receipt is an
identity acknowledgment; the worker verifies ancestry locally, not the server.
Closed custody does not authorize new archive reservations or uploads, and a
deleted worker's credentials receive 401 from worker authentication.

After an authenticated final acknowledgment covers the inventory, the worker
can retire that generation's local recovery, coverage and owed-head pins,
bundles and recovery journals. It checks ref deletion before removing bundles,
then removes source journals and the covering acknowledgment journal last.
If an earlier step fails, the acknowledgment stays for bounded retries on
restart or while the worker is live; completed steps can safely be repeated.
Removal of the now-empty directory is best-effort and never recursive.
Unknown, unreadable, malformed, foreign or uncovered inventory keeps local
custody. Shared owed-head pins, sibling generations and lightweight context
metadata still referenced by tracking or publication records remain intact.
Archive coverage is not evidence that the work was published to your forge.

The server keeps the durable archive disposition receipt and selected archive
after local cleanup; this archive path adds no new local receipt or tombstone.
The completed-publication path below stores its receipt in an existing
authenticated recovery record. Execution
or worker quarantine keeps the local evidence. Rejected credentials stop
network recovery work, but bounded local cleanup of an already acknowledged
inventory can still retry when execution and quarantine guards permit it.
Boot discovery does not recreate journals for closed or discarded holds.

Retiring a queued completion or finalization report is a separate decision.
An authenticated report for generation G can retire after real execution ends
when fresh ownership is terminal (`completed`, `failed` or `cancelled`) at
exactly G, and an authenticated exact-G reservation/upload journal identifies
a fresh, healthy **Available** server archive with a matching bound manifest.
The worker must also prove complete current source coverage and strictly clean
actual clone contents; archive byte verification alone does not establish
source completeness. Dirty, untracked or reset-soft WIP retains the report;
a clean, covered WIP commit can qualify. A thin capture can support this
**report-only** decision, without authorizing FINAL, custody release, source
or pin deletion, or clone cleanup. Busy execution, queued duplicates, changed
report identity, missing old-contract evidence, uncertain reads, input overflow
or an expired proof budget retain the report. Proof is bounded to one second
and any shorter caller budget; restart requires fresh proof. Once live execution
has ended and the report retires, the next accepted heartbeat omits its
terminal-pending active-run row. See
[ADR-2652](../adr/2652-report-only-retirement.md) for the protected boundary,
cancellation and rollout limits.

Outside that archive-backed exception, without a covering acknowledgment or a persisted
exact-generation completed-publication receipt,
the worker needs fresh proof of terminal ownership and a complete, nonempty
custody decision for that exact generation,
with no open sibling holds on that worker. Pending guarded inventory requires
every exact hold to be **discarded**; a mixture of released and discarded
holds is insufficient. With absent recovery journals, released or discarded
exact holds can authorize report retirement, but absence alone cannot. A
positively verified legacy generation follows its existing report policy.
Missing ownership, transferred ownership or unavailable evidence keeps the
report. A `stale_claim` response supersedes the report; it does not settle or
delete recovery inventory. Exact discard authorizes report retirement alone:
physical clone or recovery-source retirement still requires the existing
quiescence and retention rules. Guarded recovery journals require a covering
final acknowledgment or the distinct completed-publication cleanup authority. See [ADR-2417](../adr/2417-guarded-local-retention.md)
for the exact checks; the [bad-MAC terminal-record cleanup](#when-a-terminal-record-fails-authentication-after-restart)
below remains a distinct policy.

A guarded generation that parks (forge unreachable) or fails before its
repository was ever cloned adopted nothing, yet its hold stays open until a
final disposition. The worker closes that empty hold itself: after reporting
the park or failure it sends a settled `forge_no_output` release for exactly
that generation, with the empty-inventory digest, and retries a few times if
the API is not ready. It does this only when it can prove nothing was adopted
locally (no journal record for the generation, no retained pin or clone of
the run); on any doubt the hold stays open. The API accepts that release for
a parked run only for a `forge_unreachable` park and only for the settled
`forge_no_output` class. A parked run that adopted source keeps its hold.

A dirty or unverified source, or an unproven process boundary, retains the
clone and hold, reports **Needs action**, and uses bounded retries. Boot
recovery cannot automatically capture dirty source and has no forge PAT.
An **Available** source archive with final custody acknowledgment still
pending is not safe terminal release or permission to reclaim the source.

The **selected final archive** is protected from automatic expiry while its
local worker row exists. Physical worker deletion renews its configured
normal ready-retention window (7 days by default) and clears the server's
local-replica marker (a published-redundancy early expiry also clears it);
protection is not perpetual. Local acknowledgment
cleanup leaves that marker and the archive's expiry unchanged. Earlier
non-final and legacy ready captures keep their existing TTL, including thin
captures. Custody itself does not expire when a download expires. Size limits,
quotas, owner-only access and explicit discard behavior are unchanged. The exception to the retention
rules above is the proven early expiry of a completed run's archive, described
below.

Operational rollout: deploy the API's prerequisite-release guard before deploying
the worker producer that can emit guarded thin archives.

### Automatic release after verified completion

A completed **issue**, **MR-rework** (`mr_rework`) or **self-improvement**
(`self_improve`) run can release its inventory-guarded hold without preparing
another archive. Both worker and API must support
`recovery_completed_publication_v1`. After completion is committed, the API
checks that the run's recorded MR names the expected branch in its own
repository, that the branch head matches the MR head, and that this head
contains or equals the fixed final commit reported at completion. A matching
copy in that repository qualifies; this is not a branch-protection or non-fork
guarantee. Later user deletion or rewriting of the published copy is outside
this proof.

The release applies to the **exact completing generation**. It does not
release older or successor holds or backfill older completed runs, including
earlier self-improvement runs without a stamped completion identity. Failed,
cancelled and parked runs, and `ci_fix`, `prompt` and `task`, keep their
existing rules for this completion proof; a failed run's separate route is
[held-work publication](#publishing-terminal-held-work). Healthy
completion release needs no heartbeat prerequisite.

A successful receipt needs no capture, WIP commit, frozen inventory or
clean-source proof. Existing available, thin or needs-action captures do not
block it. Retained archives keep their existing retention policy unless uzi
proves them redundant, as described in the next subsection.
Unavailable forge evidence, missing or mismatched identities, inconclusive
ancestry or identity drift keeps custody open with a bounded reason.
Replaying the exact original completion returns the stored receipt without
depending on current forge or mutable run state.

#### Early expiry of a published archive

After a completed run's custody is released, its worker can ask the API to
expire the run's guarded archive early. The API does not trust the worker: it
checks through the forge, against the current head of the run's published
branch, that every root, the archive's captured head and every prerequisite
the server recorded for it is already an ancestor of that head. For an archive
of parked uncommitted work, the published branch must carry a commit with the
same tree as the archive plus every parent of the captured head; the worker
only offers the run's delivered final commit as that commit. The claim is
bound to the exact hold, generation, coverage digest and source commit, and
needs a released, completed hold stamped with the completion identity and MR.

When the proof succeeds the archive becomes **Expired** with reason
`published_redundant` and its bytes are deleted at once, instead of at the end
of the 7-day window. When any part is missing, mismatched or inconclusive (the
branch is gone, its head differs, the forge cannot answer), the archive simply
keeps its normal retention. A refusal caused by forge answers, including timeouts and inconclusive
answers, is not retried for an hour. The worker sends these claims from its
completion cleanup and its covering final-acknowledgement cleanup, once
per archive in practice, and a missed or failed claim never blocks cleanup; it
only leaves the normal window in place. Failed, cancelled and parked runs (the API refuses a run that did not
complete as `not_completed`), and archives without a complete proof, are unchanged, and an explicit discard
behaves as before.

**Durability risk you accept.** The proof is made against the published branch
as it is at that moment. After the archive expires early, uzi no longer
protects those commits: if the published branch (or the merge request's source
branch) is later deleted or force-rewritten, the commits may be lost. The
forge's own merge request refs may still hold them, but that is not
guaranteed. This is the same after-publication risk as the automatic custody
release above.

**Quota effect.** Early expiry returns the archive's bytes to your ready
payload and the instance byte quota, and, because an expired archive no longer
counts toward the per-owner capture limit (#2744), it also frees that slot. The
database may reuse the freed space, but its file size does not shrink at once.

When its existing outbox can journal the terminal report, the worker saves
the original completion before archive preparation. A valid receipt skips that preparation and must be persisted and read back in an
existing authenticated generation recovery record before protected report
retirement. Without a valid receipt, including a refusal, unknown ACK or a
mixed-version or excluded-kind response, the existing capture path remains
the fallback. If the API released custody but its ACK was lost, a later
capture refusal grants no cleanup authority and does not fail or reopen the
completed run; replaying its original completion recovers the receipt.

**Custody release and physical cleanup are separate.** Local cleanup still
requires process quiescence, credential isolation, exact owned paths and
source attribution under execution, adoption and repository locks. It may
discard eligible dirty or untracked leftovers of that completing source.
Sibling generations, shared pins, referenced metadata, foreign or unknown
inventory and quarantine remain protected. The receipt does not prove that
the worker's disk has already been cleaned.

`uzi run recovery <run-id>` shows the receipt's fixed final SHA, observed
branch head, branch and MR, separately from archive availability and physical
retirement. Open holds show a bounded completed-publication refusal reason.
JSON exposes additive `completed_publication_receipt` and
`completed_publication_reason` fields on hold records; older responses can
omit them. See [ADR-1296](../adr/1296-durable-run-recovery.md) and
[ADR-2417](../adr/2417-guarded-local-retention.md) for the identity and cleanup
boundaries.

### Published checkpoint refs

A finished run that already published a checkpoint to your forge keeps that
ref, `refs/uzi-checkpoints/<branch>`, for as long as any of its custody
holds is still open — a failed or cancelled run, but also a completed run
that still has an open hold from an older generation of the same run. It's
not deleted the moment the run ends, so the published copy stays reachable
even if the worker and its disk are gone. Once every hold on the run is
released or discarded, uzi removes the ref.

If you start a new run on the same branch while the old ref is still held,
uzi normally moves the old tip out of the way rather than blocking your
new run: it creates `refs/uzi-recovery/<run-id>` pointing at the old run's
tip, frees `refs/uzi-checkpoints/<branch>` for the new run, and keeps the
old ref around under its own name until that run's holds are resolved. A
stuck supersession — for example, the branch is not actually at the tip
its record names — is the exception: it can refuse your new run's
checkpoints until the old run's holds all release, rather than blocking
just the one publish that hit it.

`uzi run recovery <run-id>` shows which ref uzi's record names for a run's
checkpoint, its tip commit, and its retention state — so you usually know
whether to fetch the branch ref or the recovery ref. It can lag briefly
during an in-progress supersession, or leave a stray ref untracked in a
narrow window described in
[ADR-1810](../adr/1810-checkpoint-retention-follows-custody.md#consequences);
when in doubt, check both refs on your forge. Fetch either directly from
your forge into a local branch; a bare fetch only sets `FETCH_HEAD`, which
the next fetch overwrites, so the commit could become unreachable once uzi
deletes the ref:

```sh
git fetch origin refs/uzi-checkpoints/<branch>:refs/heads/recovered/<run-id>
git fetch origin refs/uzi-recovery/<run-id>:refs/heads/recovered/<run-id>
```

The ref disappears once the run's last hold is released (automatically, by
settlement) or discarded (`uzi run discard`) — the same custody lifecycle
that governs the recovery archives above.

## Automatic discard after the recorded PR merges

For a completed run at generation G, uzi can automatically discard earlier OPEN
custody holds (generation < G) after freshly verifying that the run's own recorded
PR/MR is merged. The server rechecks the exact owner, repository, connection,
project, forge, base URL, MR, run and G under locks. Open, closed-unmerged,
unknown, unreadable, wrong-IID or drifting evidence leaves custody retained.
Historical cached-merged and issue-less runs can qualify, but cached state alone
is insufficient. This policy includes prior recovery aggregates that are not
ancestors of the merged work; it does not prove their work was published.

The discarded holds leave the existing custody count and decision hotlist.
Preparing, uploading and needs-action captures are discarded with partial-byte
cleanup. **Available archives survive** with their prerequisites, expiry policy
and export eligibility. The audit records `merged_predecessor_discard`, not
ancestry proof, archive FINAL coverage or a publication receipt.

Hold discard grants no new worker source-deletion authority. Existing retention
guards and the [worker-local operator procedure](#retained-sources-with-no-open-hold-worker-local-inspect-and-discard)
remain; clearing custody has existing owner-discard lifecycle consequences and
adds no worker/PVC fence or retention guarantee. Physical local-source and
available-archive disposal previously promised by Option D remains **deferred**
to #2823 and #1373. This is partial, nonclosing #2506; see
[ADR-2506](../adr/2506-merged-predecessor-discard.md) for polling and rollout limits.

## Automatic settlement of an older held generation

The ancestry settlement below applies to legacy, unguarded holds. Outside the
[merged-PR server-discard exception](#automatic-discard-after-the-recorded-pr-merges), guarded
holds use the [final inventory disposition](#guarded-inventory-and-final-custody-transfer)
above; the completed-publication exception settles only the completing generation,
not an older adopted generation. Publishing one adopted head does not settle
that older generation's divergent inventory.

When a run is resumed on the same worker (after a rate-limit park, a
recovery restart, or a similar restart), the new generation may adopt an
older generation's committed work as its starting point. If that older
generation still has an open custody hold and the resumed run goes on to
publish, uzi can release the older hold automatically once it can *prove*
the older work is actually contained in what got published — you don't have
to notice and discard it by hand.

The worker never decides this itself. It reports the candidate commits — the
older generation's recorded source, the commit it adopted, and the head it
pushed — and the server checks them against what it has on record (an earlier
recovery capture of that hold, or the completed run's permitted head, when
either exists). The server then asks your forge whether each candidate is an
ancestor of the branch's current published head, using the forge's own
comparison API. Only a positive, unambiguous answer releases the hold. A
rate-limited, erroring, or inconclusive answer, or a run whose state changed
underneath the check, keeps the hold open and the worker retries later with
backoff (up to a bounded number of attempts); a definite "not contained", a
candidate that contradicts the server's record, or a branch that no longer
exists stops automatic settlement for that hold. Before reporting anything,
the worker also checks locally that the older generation's recorded source is
contained in the commit the resume adopted. When it isn't (for example, a
resume that restored uncommitted parked work from a `wip(park)` checkpoint),
that hold gets no settlement attempt from this resume, and a later resume
settles it only if the commit it adopts contains that source; otherwise it
stays open for `uzi run export` or `uzi run discard`. A hold that settlement can't clear behaves
exactly like any other open hold: it still shows up under **Recovery
archives**, and you can still resolve it yourself with `uzi run export` or
`uzi run discard`.

The resumed run doesn't have to finish first. While it is still running on the
same worker, each checkpoint the server confirms it published (or, for a task
run, its pushed branch) lets the worker ask for the same proof early. The
server checks the candidates against the checkpoint or branch it derives for that run,
not a ref the worker names, and releases the older hold only if the run is
still live on that worker at that generation. A run that was cancelled,
reclaimed, or moved in the meantime keeps the hold. An older generation taken
by a *different* worker is never settled this way; it waits for you or for the
completed-run check above.

## Reviewing and resolving held work

Once the **admission-counted holds per owner** limit (the *Admission-counted
recovery holds* limit above: default 8, the admin setting `custody_hold_limit`)
is reached, uzi pauses admitting **new** runs subject to
custody admission for you. Hold-free Plan and Code checker children
(`kind = 'cross_check'`) in both harness families are exempt; judges and jobs
gain no exemption. Checker claims still require eligible owner identity,
current parent/generation/round, deadline, protocol, vault, credentials,
checker slots and workers (see [Cross-check](./cross-check.md)).
A run that was interrupted and requeued while it has 1 to limit-1 of its own open
holds keeps resuming; once that one run alone holds the limit, it loses the exemption.
When that happens, the dashboard shows a full-width alert beneath the page
heading with your safety-slot use, how many held sources need a decision, how
many runs are blocked, and a **Review held work** button; if you connected
Slack, you also get one coalesced direct message per blocked episode (not one
per run) carrying your open-hold and blocked-run counts, a link to the surface,
and the exact discard command. The per-worker **retaining work** pills stay as
row context.

The **Workers** settings page is where you resolve them: the holds that need a
decision (and MAC-rejected terminal records) are grouped by worker with the run
it came from, its claim generation, and a state that separates them from normal
active protection. Each row offers the right action for its state:

- **Export archive** — download a ready archive (same bundle as `uzi run
  export`). Downloading does not itself release custody. Legacy holds can
  release automatically when an archive becomes ready; guarded holds await
  a verified final disposition even if an archive is available; prerequisites
  prevent archive-backed final custody release. Eligible completed runs can
  instead release through the separate completed-publication proof below.
  An Export-only row is not proof that guarded custody has closed.
- **Discard held work** — for a hold whose source may be the only copy (no
  ready archive can restore it), permanently release custody so the worker
  and its disk can be torn down. The confirmation names the run, worker, and
  claim generation, and warns that this can destroy the work for good.

On the run page, the **Recovery archives** section offers **Export archive**
and, once you have a copy, **Delete archive** — archive-artifact cleanup that
is deliberately distinct from discarding a held source. Deleting an archive
while its hold is still open does not resolve custody; left untouched, a
non-final or legacy archive expires after its retention window. A selected
final guarded archive follows the worker-deletion retention rule above.

At or over the limit, the surface also shows an **Admission information**
section so you can see what is consuming capacity without needing a decision.
**Counting toward the limit** lists the non-decision holds the server confirms
it counts, each with its run link, claim generation, worker, and the reason it
counts (for example, no live claim backs it). These rows are read-only: this is
visibility only, and adds no new way to release or discard custody and changes
no accounting. **Admission membership not reported** lists holds from an older
server that does not report membership; uzi never infers it, so an unreported
hold is unknown rather than uncounted.

From the CLI, list a run's holds and captures with `uzi run recovery
<run-id>`, then discard one exact held source:

```sh
uzi run recovery <run-id>
uzi run discard <run-id> --hold <hold-id> --yes
```

`uzi run discard` targets one exact hold, prompts for confirmation
interactively, and requires `--yes` when there is no terminal. It never
touches a ready archive or any other hold. See
[the CLI reference](cli.md#recovering-unpublished-work-uzi-run-export) for the
`uzi run export` flag and exit-code contract.

### When a terminal record fails authentication after restart

Recovery shows the fixed diagnostic:

> terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody

A MAC-rejected terminal record supplies no trustworthy outcome: it cannot
authorize outcome replay, completion, a terminal lease renewal, or an extra
finalize-resume allowance. Ordinary requeue policy still applies; if that
policy reaches exhaustion with no recorded recovery evidence or unresolved
custody, its failure origin stays `worker_lost`, with the
rejection prose above. This is an unauthenticated record, distinct from a
trusted unsent outcome protected against a second execution.

The open hold for that originating worker and exact claim generation G
stays in custody. An OPEN hold for `recovery_wait` /
`worker_requeue_exhausted` reports `source_only`, or `needs_action` if its latest
capture failed, before archive readiness or capture progress is considered.
The same narrow OPEN-hold exception applies to `worker_memory_pressure` at
`recovery_wait`; this worker-loss example does not make a rejected record
memory-intervention evidence. Discarded/released precedence remains unchanged. An
available archive remains exportable; a latest preparing/uploading capture is not
yet downloadable, and an earlier archive may omit latest worker-local work. These
capture facts do not settle the owner decision or implicitly release custody.
Other terminal holds without a verified available capture report `source_only`,
rather than `active_protected`. An open inventory-guarded hold also reports `source_only` while an
archive is downloadable: coverage may be incomplete, or the archive may require
external commits even when it covers every root. Availability does not settle
custody. Recorded evidence or uncertainty at exhaustion holds the
run for owner Resume; the MAC rejection grants no extra allowance. Absence of
recorded evidence does not prove absence of unrecorded worker work:

> no recovery archive; custody of worker `<name>`'s local source is retained (export unavailable; it may be the only copy)

A rejected record cannot supply a source head or make an archive exportable.
Export is offered when a capture is available, including a thin capture; a
verified pin must first produce an available archive. Export verifies bytes only;
verify Git dependencies in the destination clone before import or source discard.

The worker negotiates `terminal_rejection_report` through registration's
`protocol_features`. Without support it sends neither the diagnostic POST
nor the custody GET, retains local bytes, and logs the unsupported state
once per unsupported episode. A strict-decode rollback clears the shared
feature set, disabling these requests until support is negotiated again.

Hold discard changes database custody; it does not repair the worker journal.
The worker periodically checks custody with a fresh GET scoped to immutable
owner, originating worker, run, and generation provenance. Cleanup requires
a complete, nonempty exact-generation hold set, all released or discarded,
and no open sibling-generation holds on that worker. It also requires local
quiescence under admission and run locks: no admitted or active execution.
Together these checks permit deletion of only the exact observed bad-MAC
terminal files, rechecked for unchanged device, inode, hash, and MAC failure.
A diagnostic POST acknowledgment, missing hold, error, or elapsed timer
does not authorize deletion. This cleanup does not repair other journals
or delete source clones.

### When retained work blocks a new run on the same branch

A new run can be refused because the worker's recovery journal still names a
clone belonging to another run, with a message such as
`refusing to replace a retained clone owned by another run`.
Orphan reclaim checks that older owner before freeing the new run's path;
a terminal status alone does not make the older work disposable.

1. Use the diagnostic's `owner_id` to find the owning run's claim holds in
   **Workers** settings or `uzi run recovery <run-id>`. Check the originating
   worker and generation for each hold: a run that finished on another worker
   can still have an older hold here. The events contain no `hold_id`; use
   the existing recovery view to identify it. A `source_only` hold without an
   archive may be the only copy, including dirty or untracked files.
2. Confirm the owner is terminal and belongs to the same repository and branch
   lineage. The worker derives that branch from the owner's run identity;
   for merge-request rework (`mr_rework`), it uses `pipeline_ref` even when
   the stored branch is null. This comparison is not forge ancestry proof.
3. Rerun with a worker containing this fix so its existing verification can
   quarantine canonical residue, retain a proven attempt clone in place, or
   confirm the source is already absent. The path must match the owner's
   canonical clone or an attempt recorded for that owner in the worker ledger.
   Attempt-enabled workers also require their scoped capture quiescence check
   to pass; this does not promise process quiescence on unwired workers.
4. If reclaim still refuses, retain the source and custody hold and investigate
   the named guard below. Do not manually delete the clone, edit its journal,
   or discard the hold as a repair. Hold discard changes database custody;
   it does not repair the worker journal. Known commits pushed to the forge
   do not prove dirty or untracked bytes disposable during orphan reclaim.

This reclaim does not request custody release. The server-proof automatic
settlement described above governs eligible legacy holds. Guarded source
retirement requires final inventory acknowledgment or the separate exact-generation
completed-publication authority; orphan reclaim itself supplies neither.

#### Reading orphan-reclaim diagnostics

These worker events identify the failed guard or completed disposition:

| Event | Level | Meaning |
|---|---|---|
| `orphan_reclaim_refused` | warn | Reclaim stopped at `stage` for `reason`; keep the held work and investigate that guard. |
| `orphan_retirement_failed` | warn | Canonical retirement failed at an inner stage, with `canonical_retirement_failure`; inspect its safe `errno`. |
| `orphan_reclaim_succeeded` | info | `stage: complete`; `reason` is exactly `quarantined`, `retained-in-place`, or `source-already-absent`. |

| Refusal stage | Reasons and investigation |
|---|---|
| `classification` | `http` (check `http_status`), `transport_unknown` (check connectivity), `nonterminal_owner` (owner is not terminal), or `repo_mismatch` (different repository). |
| `identity` | `malformed_identity`: owner identity cannot be derived, including an unknown run kind; reclaim fails closed. |
| `branch` | `branch_mismatch`: owner-derived branch does not match the blocked branch. |
| `path` | `path_error` or `path_mismatch`: owner path verification failed or ownership was not proven. |
| `quiescence` | `quiescence_blocked` or `quiescence_error`: the scoped capture check blocked or failed. |
| `attempt_ledger` / `attempt_journal` | `attempt_release_failure`: recording retained attempt state or clearing its journal failed. |
| `retirement` | `canonical_retirement_failure`, or `attempt_release_failure` when attempt validation falls through to this generic stage. |

Inner retirement stages are `journal_read`, `journal_validation`, `containment`,
`holding_parent`, `rename`, `retained_copy`, `intra_device_rename`, and
`journal_clear`. The worker validates the old owner/path pair under the bare
repository lock and does not clear a successor's journal. For canonical
retirement, success describes retained quarantine or confirmed source absence
established under that lock,
not merely a retirement call returning. A completed quarantine stays retained
if a later journal clear fails; reclaimed attempt bytes stay in place and are
excluded from the former retention sweep. Since #1858, automatic retention
deletion is disabled for disposable-kind attempts, shared own-key residue and
orphaned skills on every worker. A refusal after a move or copy need not restore
the original layout.

The new events carry `event`, `stage`, `reason`, `claimant_id`, `owner_id`,
`repo_id`, `path_shape`, `path_fingerprint`, and `errno`. IDs are UUID strings
or `invalid`; path shape is `canonical`, `attempt`, or `unknown`. The fingerprint
is a 64-hex SHA-256 digest of the path, with 64 zeroes if hashing is unavailable.
`errno` is allowlisted or `unknown`; optional `http_status` is an integer from
100 through 599 supplied for a classification `RequestError`, without its
response body. Logging and hashing are best effort; an event may be absent.

These new events omit raw branches, journals, paths, status/kind values, error
messages, stacks, HTTP bodies, and credentials. They do not rewrite historical
logs or apply to ordinary terminal discard cleanup. A recurrence supplies safe
IDs, a path digest, stage, and available HTTP status or errno for investigation;
it does not establish the cause of the historical refusal in
[issue #1848](https://github.com/vtmocanu/uzi/issues/1848): the supplied report
lacked the deployed worker revision, classification request status, and
underlying retirement error.

### Retained sources with no open hold (worker-local inspect and discard)

A retained source can outlive its server hold: the hold was released or discarded,
but a worker-local record (attempt ledger, recovery journal, marker) still names the
clone, so it keeps blocking reclaim or occupying disk. The server views above cannot
show or discard it. The worker exposes a small operator channel for exactly this case:
a read-only **inspect** and a guarded **discard**, run on the worker itself.

Automatic retention reclaim is disabled on every worker (Refs #1858,
retention-only and non-closing). Attributed attempts and their paired skills may
use this existing retained-source discard route, subject to all its checks.
Shared residue (`.uzi-residue-*`) and orphaned skills have **no supported generic
reclaim route**. API custody discard alone is not local writer-exclusion proof.
Do not fall back to deleting a source or volume without independent preservation
of protected work and writer exclusion.

The former 3-attempt / 5-residue caps and any-orphan trigger are diagnostic
thresholds only. Known excess (>3 attempts, >5 residue, >0 orphaned skills) can
log `runner clone retention: automatic reclaim disabled`, with a sanitized,
truncated key and observed lower-bound counts/completeness. Observation validates
the parent and is limited to 4096 direct siblings; it does no recursive byte walk.
Overflow or read failure gives partial/unknown evidence. Protection predicates
fail closed; protected-or-unreadable custody makes attempt classification partial
without a second probe. Warning rate is one per hour per full canonical key in a
128-key LRU; restart or eviction may warn again. These are diagnostic bounds,
not storage, identity-metadata or safety-inventory bounds.

No process scan, daemon listing (including repeated clean listings), time, age,
count or disk pressure authorizes retention deletion. Existing canonical
`beforeFree` / `freeCanonicalClonePath` and quarantine are unchanged and retain
the **process-proof / daemon pending-create residual**: a create already accepted
by the daemon can complete after process proof and clean listings. #1860 is the
separate daemon-mediation follow-up; #2852's terminal/Docker writer-exclusion
remainder is not fixed. Current unwired status is not durable proof that a path
was never exposed to a daemon. No new proxy, marker, ledger, API, schema,
configuration flag or infrastructure is added.

- **Socket.** The worker listens on `<dataDir>/ops/retained-source.sock` (a
  worker-owned `0700` directory holding a `0600` socket). There is no TCP listener. If
  the channel cannot be set up the worker logs it and keeps running; the channel is
  then unavailable, not the worker.
- **Invocation.** Run the helper as the `worker` user, with a scrubbed environment and
  the image-owned node binary. `/usr/bin/env -i` plus that binary is why a planted `NODE_OPTIONS`,
  `NODE_PATH` or tsx cache cannot influence it. **Never run the client as root:** the
  client file is owned by the worker uid (the image runs `chown -R worker:worker /app`),
  so running it as root would execute a worker-writable script with root privileges.
  Plain `docker exec` runs as root on compose, and `kubectl exec` has no user flag, so
  under the uid split (the container itself runs as root) you must drop to the worker uid
  first:

  ```sh
  # Docker Compose
  docker exec -u worker <agent container> \
    /usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs \
    inspect --run <run-id> --generation <n|none> --attempt <attempt-id|canonical>

  # Kubernetes, single-UID workers (the default, workers.uidSplit.enabled: false):
  # the container already runs as the worker uid
  kubectl exec -c worker <pod> -- \
    /usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs \
    inspect --run <run-id> --generation <n|none> --attempt <attempt-id|canonical>

  # Kubernetes, split-UID workers: drop to the worker uid (10001) first;
  # /bin/setpriv is the util-linux binary in the image
  kubectl exec -c worker <pod> -- \
    /bin/setpriv --reuid=10001 --regid=10001 --init-groups \
    --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
    /usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs \
    inspect --run <run-id> --generation <n|none> --attempt <attempt-id|canonical>
  ```

  Every binary in these commands is named by an absolute, root-owned path because the
  image `PATH` includes runner-writable directories (`/opt/uzi-toolchain/bin` and the nix
  profile `bin`) ahead of `/bin`; a bare `env` or `setpriv` could run a runner-planted
  program as root or as the worker uid.

  Under `/usr/bin/env -i` the client cannot read `UZI_DATA_DIR` and defaults to `/data`; for a
  non-default data directory pass `--socket <path>` naming the socket. As defense in
  depth, the run's Bash guardrail denies commands that name the installed client or the
  socket path from inside a run; the uid split, not that guardrail, is the real boundary.

**Inspect** is read-only and content-free. It reports the generation (or `none`), the
attempt or `canonical`, the ledger, journal and marker state, a descriptor digest,
whether the source is present, and its size in bytes. A size that hit the scan budget is
reported as partial or unknown, never as `0`. Server hold state comes from the worker
API (this worker's open holds); an empty list means the hold is absent or released,
never something inferred from local files. When a source is selected
(`--run`/`--generation`/`--attempt`) it names one disposition; an aggregate inspect (no
selector) only lists sources, without holds or a disposition. Aggregate output
is capped at 64 descriptors (`RETAINED_MAX_DESCRIPTORS`); excess descriptors make
it partial. Use selector-scoped inspection for a specific run even when aggregate
output is partial; that inspection still has its existing time/read budgets.
Aggregate output is not a complete inventory or disk-growth accounting, and
unknown measurement does not mean zero.

| Disposition | Meaning and next step |
|---|---|
| `owner_hold_discard <hold-id>` | A server hold still owns the source. Use `uzi run discard <run-id> --hold <hold-id> --yes`. |
| `resolve_local_record` | A local recovery, settlement or terminal record still holds the source. Resolve it through its existing path; this command adds no new resolution. |
| `local_discard` / `local_protected_discard` | Nothing else holds the source; it may be discarded with the command below (protected for episode or protected sources). |
| `refused` | Named consumers still use the source. The output lists them. |

A canonical clone kept under its plain journal by a park or terminal exit (marker
state `none`, journal state `plain`) has no marker, so the worker cannot tell locally
whether the run is still parked. An absent or released hold does not prove the run is
terminal: a hold can be released (claim released, capture available) or never opened
(`paused` and `limit_wait` parks, workers with recovery disabled) while the run is
still parked. Discard therefore needs positive lifecycle evidence: for a selected
source inspect and discard read the run's status through the worker's existing
ownership probe (`GET /api/worker/runs/{id}/ownership`), and it must be `completed`,
`failed` or `cancelled`. Inspect shows the result as the consumer
`run_state:terminal:<status>`, `run_state:<status>` (a non-terminal status) or
`run_state:unknown`, and `lifecycleRead` (`terminal`, `not_terminal` or `unknown`). An
aggregate inspect reads no lifecycle and shows `run_state:not_read`, with no blocker.
The disposition follows what the worker can see:

- An open server hold gives `owner_hold_discard` (the refusal for an actively parked
  run), a local record gives `resolve_local_record`, and a live flight refuses.
- A status the probe cannot establish (a 404, an error, a timeout or a malformed body)
  refuses with the blocker `run_state_unknown`; any other status refuses with
  `run_state_not_terminal`. Nothing is deleted.
- Otherwise it is `local_discard`, which loses every byte not already in a capture,
  published checkpoint or the tracking ref and clears that plain journal, so a later
  resume seeds fresh.

The probe is scoped to the worker: once the run's `worker_id` moves off this worker (a
requeue after a released claim, or a claim elsewhere) it reads 404, so the leftover is
refused. It stays kept, its bytes retained. An open hold of this worker on the run
gives `owner_hold_discard` (the owner's server-side hold discard) even when the
lifecycle read is unknown, and a cross-worker predecessor's hold can stay open; a local
record gives `resolve_local_record`. With neither, the lifecycle read decides. Its only
automatic reclaim is a later run of that branch's terminal-orphan handling on this
worker, which moves the clone to `<dataDir>/runner-quarantine/` (retained, never deleted
automatically; automatic retention deletion is disabled) and frees
the canonical path; a protected source or an attempt clone is kept in place instead.
Cross-worker reclaim needs a lifecycle read that is not worker-scoped (new API); it is
an open follow-up and is not shipped.

A kept canonical clone with a `uzi-stopped` marker is listed with marker state
`detached` (no active journal names it), or `bound` while its plain journal is
still pending. Marker state `mismatch` means an active journal names the path but
no longer matches the marker's digest; it is always refused (blocker
`marker_mismatch`). A source with mismatched or malformed marker evidence is kept
and is not reclaimable through this channel (fail closed): it needs manual
investigation by the maintainer.

**Discard** is the same invocation with `discard` in place of `inspect` and
`--confirm <descriptor digest from inspect>` added; pass `--protected` as well for an
episode or protected source. It closes new claims briefly, waits at most 5 s for
in-flight claims, requires the worker to be idle, reserves the run, then deletes the
exact source under one lock. It never cancels a run or stops the worker.
Existing fresh server-hold reads, digest confirmation, journal, live-flight,
quarantine, process, Docker and owned-path checks still apply. These checks
do not guarantee that all writers are fenced; the Docker pending-create residual
remains.

- On a Docker-wired worker, discard itself runs the Docker teardown, which
  force-removes containers that bind-mount the clone (with their anonymous
  volumes), and is refused unless that teardown reported `docker_unconfirmed` (a bounded
  observation: two consecutive listings found no known container mounting the clone;
  a container create the Docker daemon had already accepted could still complete afterwards). A `docker_error` or any other state
  refuses the request. Discard clears a journal
  only when it still names that source, never a successor's.
- A busy channel, a drain timeout (claims did not drain in 5 s), a non-idle worker, a
  reserved run, a wrong digest or an unreadable holds read is **refused**, and nothing is
  deleted. If the lookup that locates the source ran out of its budget or could not read a
  repository, the refusal says `inspect_partial` rather than `not_found`; retry the inspect.
- Running out the 30 s operation deadline reports `unknown`; a partial delete reports
  `incomplete`.
- Per-source results are `removed`, `retained`, `missing` or `unknown`. Metadata is
  retired only when every path is verified gone.
- The client exits non-zero on anything other than a successful result.

> **Discard destroys possibly unique work.** Inspect first, and export or back up
> anything you may want before confirming (the `uzi-watcher` skill's `backup-runs.sh`
> is the existing backup procedure). `backup-runs.sh` attributes a detached kept
> canonical clone through its exact `uzi-stopped.<runId>.entry` marker (the
> canonical path only, never an attempt clone). For an active or failed run it captures every
> attributed source on every running worker pod: the primary as `<stem>.tgz` (selection
> unchanged) and each other source as `<stem>.source-N.tgz` (N from 2, members prefixed
> `<stem>.source-N.`, `meta.txt` naming `clone=<path>`). Attributed sources are the journal
> or protected-descriptor sources, the marker canonical, and attempt clones whose last
> ledger entry for the run is `live` or `reclaimed` (an unreadable one counts as attributed
> only on that evidence, or when a journal names it); completed and cancelled runs capture
> protected-descriptor or marker sources and the same `live`/`reclaimed` attempt clones (the
> plain journal is ignored for them, and a retired or abandoned attempt is never a source: a
> terminal status does not prove a predecessor's unique work is preserved). If an attributed source is listed but
> unreadable, any capture is not verified, or a probe of the search failed (a kubectl
> listing or exec, an unreadable bare repository), it logs
> `FAIL <run> (<label>): incomplete coverage: <k> of <m> attributed sources captured (<reasons>)`,
> where `<m>` counts the sources found (clones or the tracking ref the run tried to
> capture) plus attributed unreadable ones, exits 1 and does not advance `latest`; any
> archive that did verify is kept. When nothing is captured at all (no readable clone and
> no owned tracking ref), a failed search instead logs
> `FAIL <run> (<label>) status=<st>: status saved, but the source search was inconclusive (<reasons>); retry`
> (still exit 1, `latest` not advanced); each reason names the failing pod or namespace
> for a kubectl listing or exec failure (`exec into pod <ns>/<pod> failed`, `listing Running pods in
> namespace <ns> failed`), an errored probe, an unreadable `.git` (`a candidate's .git on pod
> <ns>/<pod> could not be read`) and an attributed source listed but unreadable (`attributed source
> <path> on pod <ns>/<pod> is listed but unreadable`); capture failures name the source path. A
> same-stem directory without `.git` is skipped only when it is positively empty; a nonempty
> or unlistable one that the journal (active and failed runs only), a protected descriptor, a `live`/`reclaimed` ledger entry or the stopped marker names is an
> attributed source that is listed but unreadable: incomplete coverage when another source was
> captured, else the no-capture inconclusive FAIL line above; either way exit 1 and `latest`
> not advanced. A directory none of those name is not an attributed source. Cost: every cycle execs into every Running pod in `UZI_WORKER_NS` for each
> bound run, and one pod that cannot be exec'd into fails every bound run's cycle until it
> is fixed (the FAIL line names it and verified artifacts are kept). Do not drop its
> namespace from `UZI_WORKER_NS` (which lists namespaces, not pods) unless you have verified
> that namespace holds no source a tracked run needs: excluding it hides those sources from
> the search, so their work is no longer backed up and the backup could read green without it.

What the channel does not promise:

- **Not a model boundary.** On single-UID workers (the default hosted Kubernetes
  deployment, `uidSplit.enabled: false`) the model's shell runs as the worker's own uid
  and can reach the socket and modify `/app`. The channel adds no capability beyond that
  uid's existing write access to `/data/runner`. Under the uid split the runner uids
  cannot connect.
- **Durability follows the clone's storage.** A same-pod worker process restart keeps
  the clone. Pod replacement loses an `emptyDir` clone (the Docker-tier `/data/runner`)
  even though the bare-config ledger and marker survive; inspect reports such a source as
  missing, never as empty work. PVC teardown loses both.
- **Gitignored files are not covered.** No capture covers them; coverage is `HEAD` plus
  tracked and untracked non-ignored bytes.

## What this is not: the threat model

This is **server-side encryption at rest**, not end-to-end encryption. The
API encrypts every archive under its own master key before storing it, and
only the API ever holds that key — the worker that produced the archive
never sees it. That protects a stolen database dump or a stray backup. It
does **not** protect against an operator who already holds that key: this
system is not designed to hide your history from whoever administers the
uzi instance you're running on, only to keep it from anyone else and from
casual exposure in logs, previews, or other run data.

## Mixed fleets

Durable recovery needs a worker and an API that both support it. A run that
executed on an older worker, or against an older server, is honestly
reported as **unsupported** rather than silently promised a recovery that
was never captured. Upgrading your fleet only protects runs going forward.

V1 held publication needs `recovery_held_publication_v1` and
`UZI_HELD_PUBLICATION` on. Widening to cancelled/completed runs and earlier
generations requires both the server feature and worker capability
`recovery_held_publication_v2`. A new worker with an old API retains V1's
failed-current-generation scope; an old worker with a new API keeps its existing
behavior. Without supported held publication, the archive path remains.

If the API lacks `recovery_inventory_v1` or the claim is not guarded, the
worker emits an **UNGUARDED legacy generation** notice. Local pins may still
retain commits, but complete inventory protection at terminal release and
worker reclamation is unsupported. Once a generation's guard has latched,
later feature loss retains custody rather than falling back to legacy release.

## Publishing terminal held work

Eligible failed and cancelled runs try to put held work on your forge before
deferred archive capture. The work goes to
`refs/uzi-held/<run-id>/<generation>`; custody releases against the verified ref.
The archive is the fallback on the same frozen record. If neither route proves
complete coverage, the source stays protected. The ref is yours to fetch with
plain `git`.

Completed runs without an applicable completed-publication receipt stay
**archive-first**. A valid self-contained archive FINAL or fully verified empty
inventory still qualifies, including for `ci_fix`, prompt and task runs. Only a
prerequisite-bearing thin archive or a structured typed oversize/quota refusal
allows a held alternative on the same frozen record. A success report or
unverified head is not publication proof. A completed-publication receipt covers
its own generation, never a predecessor's work.

This is on by default. An operator turns it off with `UZI_HELD_PUBLICATION`
(see [Configuration](configuration.md)); the knob, its default and its
interaction with `UZI_RECOVERY_READY_RETENTION` are documented there and not
repeated here.

### What is eligible

A run qualifies when all of these hold:

- The server run is terminal: **`failed`, `cancelled` or `completed`** under
  mutually negotiated V2, with failure origin neither `push_secret_blocked` nor
  `worker_residue_blocked`. Parked runs and excluded origins keep their existing
  paths. V1 remains failed-current-generation only.
- It is a run kind that opens a custody hold: issue, MR-rework,
  self-improvement, `ci_fix`, prompt and task runs.
- Its exact positive generation has ended and is no later than the current
  claim generation. The inventory-guarded hold is open and live on its original
  calling custodian, with matching run, user and repository. Under V2 the run's
  current worker need not be that custodian. This permits neither adoption nor
  impersonation, nor settlement of a live predecessor. See
  [Mixed fleets](#mixed-fleets). Earlier-generation work is preserved normally;
  there is no new automatic discard policy.

### What gets published

The ref points at the run's final tip: the commits made since the last
checkpoint or published branch tip uzi could confirm on the forge, plus a
work-in-progress commit holding the clone's uncommitted and untracked changes
when the clone had any.
Gitignored files are never included. Fetching the ref brings the full history
that leads to it, not only the new commits.

### How it happens

1. **The worker scans and uploads.** After terminal custody is proved,
   the worker secret-scans the run's new commits and work-in-progress (commits
   this flight already published can ride along unscanned; see
   [ADR-2545](../adr/2545-failed-run-held-publication.md)), and
   applies the same remembered-finding gate as checkpoints (a commit flagged
   earlier in the run is held back). It rejects a range that changes a
   workflow file, because the stored credential may lack workflow scope, and
   it refuses a pack over **64 MiB**. It then uploads the pack to the API,
   with a per-request cap of 330 seconds, further limited by any remaining
   cancellation-attempt budget. The worker never holds a forge credential.
2. **The API creates the ref.** The API pushes the pack with your stored
   forge connection, **create-only**: an existing ref is refused, never moved
   or overwritten. It records a "may have invoked" marker before the push, so
   the create is sent at most once per run and generation. An outcome that
   cannot be confirmed is only reconciled afterwards (the ref is listed), never
   re-sent.
3. **The worker releases custody.** Once the API answers that the ref was
   created at the expected tip, the worker journals a final
   `held_publication` disposition. The API re-checks the forge, and if the
   ref is advertised at that exact tip it releases the one custody hold the
   publication was made for. A replay of the same request is answered from the
   stored receipt first. The worker, its clone and its disk can then be torn
   down as for any released hold.

The retention clock starts when the release is **acknowledged**, not when the
ref was created. The persisted expiry uses `UZI_RECOVERY_READY_RETENTION`
(default 168 hours / 7 days); reuse and replay do not renew it. The owner can
expire it earlier. Retention below one second refuses new held release; without
a qualifying fallback, custody stays protected.

### When it falls back to the archive

Any of these permits the archive path instead, on the same frozen record.
Custody stays open until a qualifying complete archive FINAL is acknowledged;
committed-only coverage cannot discharge unproven residue:

- the fresh scan finds a secret, or cannot be trusted or completed;
- a finding remembered earlier in the run still holds the range;
- the range cannot be resolved against what the forge already has;
- the range changes a workflow file, or the check cannot tell;
- the pack is over 64 MiB;
- the API reports a definitive create refusal; V1 also retains its existing
  archive fallback for unproved create answers;
- the server predates held publication (no `recovery_held_publication_v1`),
  or `UZI_HELD_PUBLICATION` is off;
- the release step gives a definitive refusal: `held_publication_unsupported`,
  `held_publication_ref_missing`, `held_publication_ref_mismatch` or
  `held_publication_not_eligible`, or a 400 (from an older API that does not know
  the held release, or a request the API rejects as malformed).

A V2 unknown create/upload outcome keeps its invocation and journal for
read-back, without starting an archive or retrying the create. Known settlement
stops fallback entirely; unavailable hold status keeps protection.

A transient answer to the release step (`held_publication_unavailable`, a
transport error or a lost ACK) does not archive: the journaled release is kept
for exact replay. The hold stays protected unless the server has already
accepted that FINAL; cleanup still needs its actual replay ACK. A later pass can
retry within a fresh bounded attempt.

The worker logs a short notice naming the reason code (never file contents or
finding text), for example
`Held publication of <sha> skipped: secret_found; archiving instead.` By the
time a run has failed its feed is closed, so the notice normally lands in the
worker log rather than on the run page.

### Reconciliation and the cancellation deadline

The worker can read its exact original-custodian hold with the optional
`generation=G` query on `GET /api/worker/runs/{id}/recovery-holds` under V2.
The read is scoped to the user, repository and original worker. New publication
requires open guarded live custody for the ended generation of a terminal run.
Closed persisted evidence or observing a receipt is **not** a FINAL ACK.
Unknown, missing, foreign or malformed evidence keeps the source protected.

A saved immutable FINAL replays before mutable checks, including after a lost
ACK; cleanup requires the actual ACK to posting the original receipt. A known
settled or policy-discarded concurrent close permits neither a new FINAL nor
archive fallback. An executing flight can defer FINAL until its tail closes;
existing boot and live recovery retry saved FINALs. Turning publication off
keeps exact reconciliation, saved FINAL replay and cleanup available.

Once cancellation is observed, one **600,000 ms (10 minute)** attempt budget,
measured with `performance.now()`, covers lock waits, freezing, scans, packing,
create/read-back, FINAL and cleanup. Retries and fallback share the remaining
budget, and operation caps cannot extend it. Late results grant no new
authority. On expiry the model stays stopped and custody/source stay protected.
A later existing recovery pass may start a fresh bounded attempt.

The create-marker transaction serializes admission with hold settlement.
Settlement prevents new authorization; it cannot revoke an already admitted
forge mutation, which may finish later. Unknown outcomes are reconciled rather
than resent.

### Accepted limit: terminal late writers

The maintainer accepted on 2026-10-14 that an undetected terminal late writer
can lose bytes written after immutable capture under existing cleanup, matching
failed-run held publication. Detected or unproven writers fail closed; this is
not proof that writers are excluded. No new post-release retention keeps
cancelled source or pins through hosted GC, controller teardown or recycling.
A reviewing peer preferred durable post-release retention across that lifecycle;
that alternative was rejected because of its size and because it would recreate
custody after release. See [ADR-2545](../adr/2545-failed-run-held-publication.md).

This extension adds no model-free blocked/orphan producer (#3031),
live-predecessor widening (#2969), writer design (#2852), plan-rejection
producer/discard policy (#2884), zero-write fast path or workflow-file editing
authority. Full-root/WIP coverage, non-ignored untracked work, secret and
workflow guards remain required; it adds no holdless kinds or model reopening.

Held-ref visibility follows the repository. Copies fetched from a public
repository before ref deletion cannot be revoked.

### Finding and fetching the ref

On the run page, a run with a held publication shows a **Held work**
panel: one badge per publication (for example **Published**, **Publishing**,
**Removing**, **Removed**, **Not published**), the ref and short tip, when it
expires, and a copyable fetch command. It refreshes on its own while a
publication is still settling, then stops after about ten minutes; reload the
page to refresh. On an older server the panel does not appear.

From the CLI:

```sh
uzi run held <run-id>
```

prints one row per generation: `GEN`, `STATE`, `REF`, `TIP`, `EXPIRES`.
`uzi run recovery <run-id>` also names the ref on a hold that was released
against a held publication (`hold <id> held: <ref> @ <tip>`). See
[the CLI reference](cli.md#fetching-and-expiring-terminal-held-refs-uzi-run-held)
for the flags, output and JSON form.

Fetch it from a clone of the repository, naming a local branch so a later
fetch cannot overwrite it and the commit stays reachable after the ref is
deleted:

```sh
git fetch origin refs/uzi-held/<run-id>/<generation>:refs/heads/recovered/<run-id>
```

The bare `git fetch origin refs/uzi-held/<run-id>/<generation>` is fine to
inspect the commit, but it only sets `FETCH_HEAD`.

The publication states are `prepared`, `invoked`, `created`, `create_unknown`,
`refused`, `acknowledged`, `delete_unknown`, `deleted` and `abandoned`. Only
`created`, `acknowledged` and `delete_unknown` (the delete is unconfirmed, so the
ref may still be on the forge) have a ref worth fetching. `refused`
means the forge rejected the create and no ref of uzi's exists (a ref at another
commit may); `create_unknown` and
`delete_unknown` are outcomes uzi has not been able to confirm yet and will
reconcile; `abandoned` means the ref now points at a commit uzi did not
publish, which uzi leaves untouched.

### Expiry and early expiry

Cleanup runs in the server's sweeper, and is always on, even with
`UZI_HELD_PUBLICATION` off, so turning the feature off never strands a ref
that was already published. It deletes:

- **acknowledged** refs once their retention has passed; and
- refs that were **created but never acknowledged**, once the run has no open
  hold.

Every delete is a compare-and-swap on the tip uzi recorded, taken under the
run's database row lock, and never runs while any hold of that run is open. A
ref that has moved to a different commit is left alone and the publication is
marked `abandoned`. A create or delete whose outcome was unknown is reconciled
by listing the ref.

To end the retention early:

```sh
uzi run held expire <run-id>
```

This makes the run's **acknowledged** publications due now; the sweeper
deletes each ref shortly after, and the command itself deletes nothing. A
publication that is not acknowledged is never touched, so work still protected
by an open hold keeps its ref. Fetch the ref first: once it is deleted, the
work survives only in a clone you fetched it into.

### Removing a repo or forge connection

While a held ref may still exist, removing its repo or its forge connection is
refused with a 409 that names the refs (see
[the CLI reference](cli.md#fetching-and-expiring-terminal-held-refs-uzi-run-held)
for the body). The block lifts once each ref is deleted or settled. An
acknowledged ref clears at its retention expiry, or sooner with
`uzi run held expire`.

## Resuming an approved run in a fresh attempt

When a run whose plan was already approved loses its session (for example a worker is replaced during a usage-limit wait) and continues in a fresh attempt, uzi can skip the plan gate for a plan that was already approved at the gate (by you, or automatically on an autopilot run). The new session is told what happened, so it does not mistake the plan for its own:

- **An approval statement.** The first implement prompt says the current plan was approved at the plan gate in an earlier session. Approval of the plan does not prove that any step the plan describes as happening before the gate actually took place.
- **The plan.** The persisted plan is carried over unchanged. No gate revision number is shown.
- **Your earlier follow-ups.** Follow-ups you sent in the earlier session are included once, oldest first, ahead of any new follow-ups, as clearly fenced historical context. The model is told to treat them as untrusted background, not as new instructions. A follow-up you send after the resume is delivered normally and is never duplicated as history.

The earlier follow-ups are bounded: at most 40000 characters of follow-up text and 100 entries in total. uzi never shortens a follow-up to fit.

If uzi cannot restore your earlier follow-ups in full (the lookup failed or was unavailable, or the history is over either bound), the run does not use the reviewed resume. It plans again and waits for your approval at the plan gate, even on an autopilot run. The run's activity shows a status line saying the earlier follow-ups could not be restored. Autopilot is switched off for the run when it parks, so a later recovery of a lost working tree also asks for your approval.

Notes:

- On a deployment whose API predates the follow-up lookup, every such resume waits for your approval. Upgrade the API before the workers.
- A run that started from a supplied plan (seeded) resumes as before: no history, no forced approval.
- If a resume with earlier follow-ups cannot deliver its pending input, the run fails when the failure is definitive and parks when it is transient.
- A Codex run whose resume attempt is rejected mid-claim and falls back to a fresh thread is not covered by this carry-over.

## Salvage copies

Separate from the archive above, a **salvage copy** is a bounded, run-scoped
copy of a failed run's *last published checkpoint* — the state uzi already
pushed to origin before the run failed, which may be behind the run's final
local work. Every surface calls this "checkpointed commits saved," never
"recovered," because it can be stale.

Salvage is **off by default**. An operator turns it on per forge kind with
`UZI_SALVAGE_FORGES`, a comma list of `github`, `gitlab`, `forgejo`; a forge
should be added only after the maintainer's real-forge check for that forge
has passed. With the setting empty, or for an unlisted forge, nothing about
a failed run's checkpoint changes.

On an enabled forge, a periodic sweep verifies the tip is still current —
still at the branch's own checkpoint ref, `refs/uzi-checkpoints/<branch>`,
or at its recovery ref, `refs/uzi-recovery/<run-id>` (see
[Where the work is kept](#where-the-work-is-kept)) — and, only then, copies
it into a run-scoped `refs/uzi-salvage/<run-id>`. A promoted copy expires
after `UZI_RECOVERY_READY_RETENTION` (the same window as the archive
retention above), by CAS-deleting the salvage ref; if that delete keeps failing, uzi
gives up after a bounded number of attempts and the last error names the ref that may
remain. Salvage never deletes or
moves any other ref: the branch checkpoint ref and any recovery ref stay
[custody retention](#where-the-work-is-kept)'s, never salvage's, to manage
— salvage only ever reads them as a source, and only ever creates or deletes
its own ref.

States shown on the run page and by `uzi run get`:

- **Saving** (`pending`) — a copy is being made.
- **Saved** (`promoted`) — the copy exists; the run page shows its ref, tip,
  expiry, and a copyable fetch command.
- **Not saved** — `unavailable` (the published checkpoint no longer matched
  its recorded tip on the forge), `refused` (the salvage ref already pointed
  at a different commit), `failed` (salvage stopped after repeated attempts; the last error names any
  ref that may remain), or
  `skipped_secret` (the run failed on a secret-scan block: never saved).
- **Expired** — the salvage copy's retention ended; uzi CAS-deleted it (or found it
  already gone or moved). If the delete kept failing (hourly after the first few
  tries), uzi stops after a bounded number of attempts; the last error names the ref
  that may remain on the forge for you to delete by hand.
- **Off** (`disabled`) — salvage was turned off for this forge before a copy
  was made.

Fetch a saved copy:

```sh
git fetch origin refs/uzi-salvage/<run-id>
```

That bare form is what the run page and `uzi run get` print, and it is fine to inspect
the commit right away, but it only sets `FETCH_HEAD`, which the next fetch in the same
clone overwrites — so the commit can become unreachable once the salvage ref expires and
is deleted. To keep it, name a local branch instead, the same way as the checkpoint and
recovery refs above:

```sh
git fetch origin refs/uzi-salvage/<run-id>:refs/heads/recovered/<run-id>
```

While a failed run has a live salvage copy — made, or still being made —
removing its repo or its forge connection is refused with a 409 naming the
salvage refs and the runs still pending; the block lifts once each copy
expires, or a pending one settles without being kept.

**A run with no open custody hold usually gets no salvage copy at all.**
Once such a run goes terminal, [custody retention](#where-the-work-is-kept)
usually deletes its checkpoint ref on its own before the next salvage sweep
looks, so salvage almost always settles that run `unavailable` — there is nothing
left for it to verify and copy. Salvage produces a copy only for a run whose
custody hold is still open when the sweep runs (or, occasionally, an older
run whose ref happened to survive from before this feature existed, or a run
failed by auto-stop or a Codex account wait, whose
ref custody retention deletes a little later). If you
need a run's checkpoint and no salvage copy exists, check whether a
[retained checkpoint or recovery ref](#where-the-work-is-kept) is still
open first — that is usually where it is.

### Recovery order

When you're recovering a failed run's work, check in this order: first its
[held ref](#publishing-terminal-held-work), if the run has one (find it with
`uzi run held <run-id>`, then `git fetch origin refs/uzi-held/<run-id>/<generation>`).
It is published from the run's final frozen tip, so it holds the run's last committed
and uncommitted work as of the failure, unlike the salvage copy. Then check whether a
salvage copy exists, and compare its tip against any available
[recovery archive](#downloading-an-archive), the run's
[retained checkpoint or recovery ref](#where-the-work-is-kept), a live
worker clone, and the worker's own tracking ref. Restore whichever verified
source is freshest. Never prefer an older salvage checkpoint merely because
it happens to be the one that's remotely reachable — any of the other
sources may hold newer work the salvage copy never saw.

Related: [Recovering from a transient interruption](run-recovery-wait.md) (a different,
earlier mechanism — a transient in-place retry, not a byte archive) ·
[Hosted workers](hosted-workers.md) · [CLI](cli.md)
