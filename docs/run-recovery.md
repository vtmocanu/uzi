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
torn down. For guarded generations, release requires final acknowledgment
of a covering archive or a verified empty inventory, not just publication
of the current head. Legacy generations can release on proof that their
recorded work is published. If the boundary cannot verify and resolve the
source, it keeps the source held for you to decide (see below). A hard
crash with no shutdown window — a killed node, an out-of-memory kill — is
outside this guarantee.

This is a safety net for a run that already produced real commits, not a
guarantee against every possible failure. It does **not** cover uncommitted
files, the SDK transcript, the worker's HOME directory, or a worker that
died before it reached this protected boundary.

## What you'll see

The run page shows a **Recovery archives** section on any run that failed to
finalize, and on any run that already has a retained capture even before it
reaches a final status. Each retained capture is in one of these six states:

- **Preparing** — the original committed history is being captured at the
  protected boundary; nothing is downloadable yet.
- **Uploading** — the encrypted archive is being stored; nothing is
  downloadable yet.
- **Available** — the original committed history is ready to download.
- **Needs action** — the archive could not be completed (for example, a
  storage limit was hit); the source is still retained and will be retried.
- **Expired** — the ready download window passed and the bytes were purged.
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

Every download surface warns that the original may contain secrets: review
it before publishing anywhere, and if it exposed a real credential, revoke
and rotate it and remove it from the affected history — deleting the
archive alone does not undo that exposure.

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
- **Verified and atomic.** The byte count and checksum are verified against
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
git bundle verify recovered.bundle
git clone <your-forge-repo-url> recovered-repo
cd recovered-repo
git fetch ../recovered.bundle refs/heads/recovered-source:refs/heads/recovered-source
git checkout recovered-source
```

Where possible the bundle relies on your repo's existing default-branch
history as its prerequisite (so it stays small); when no such history was
reachable, it is self-contained instead. The original commits retain
identical commit ids, parent relationships, trees, and binary contents.

For an inventory-guarded capture, `recovered-source` can be a **synthetic
aggregate**, with the current source tree and the retained divergent heads
in its ancestry. It is not necessarily an original agent commit, and its
tree does not combine the contents of those divergent heads. Inspect the
ancestor history, then check out the exact original SHA you need:

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
| Max bundle size | 64 MiB | A larger archive is refused, not truncated; the source stays retained and is retried once capacity allows. |
| Ready payload per owner | 1 GiB | Total bytes of *your* ready (downloadable) archives. |
| Instance byte quota | 4 GiB | Total ready bytes across the whole deployment (scoped to the owner who breached it, never a global stop). |
| Ready-artifact retention | 7 days | Normal ready-download window. Earlier non-final and legacy captures count from readiness; a selected final guarded archive is protected while its local worker exists, then gets this window renewed on physical worker deletion. |
| Automatic upload-retry window | 24 hours | How long uzi keeps retrying a stalled upload before it needs your attention. |
| Captures per claim | 16 | Distinct capture attempts one worker claim can accumulate. |
| Retained captures per owner | 256 | Total captures you can have on file at once. |
| Admission-counted recovery holds per owner | 8 | Open holds count except at most one per run backing a healthy current claim without an owner decision. Total open custody stays protected. At the admission limit, uzi pauses admitting **new** runs until the counted pressure falls. A requeued run with 1–7 of its own owner-scoped open holds remains exempt; at 8 it loses that exemption. This fixed admission gate is not a strict ceiling: concurrent claims and stale heartbeats can raise pressure past 8. Not yet an environment variable. See [ADR-2445](../adr/2445-custody-admission-accounting.md). |

### Shared stored-file budget

Recovery archives and [job files](./jobs.md#job-files) live in the same database, so they share one ceiling: `UZI_STORED_FILES_BUDGET_BYTES` (4 GiB by default, the same figure as the instance byte quota above). Both stores count against it: recovery-archive bytes (ready archives, plus the declared size of an upload still in flight) **plus** job-file bytes (every non-expired job file, reservations included). It is a hard combined limit, and **recovery wins**:

- When a recovery upload would not fit the budget, uzi first reclaims job files to make room, in this order: files already past their expiry, then the oldest `available` job files (those of finished jobs). It **never** reclaims a file attached to a job that is still running, a file whose upload is in flight, or an unattached upload still inside its one-hour window. A reclaimed job file becomes expired: its bytes are deleted and a later download is a 410.
- Admission itself deletes nothing: it only checks that enough reclaimable job-file bytes exist. The reclaim runs once the upload's size and checksum have verified, and only for the bytes that upload actually delivered, so a worker that declares a size and never sends the bytes cannot expire anyone's files.
- Only if reclaiming everything reclaimable still would not fit is the archive refused, exactly as an over-quota archive is refused above. Job files never reclaim recovery archives; a job-file upload over the budget is refused 507 `storage_quota_exceeded`.

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
separate **worker-local, not checkpoint durable** heads from confirmed
remote copies: a local pin does not survive loss of that worker's disk.
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

### Guarded inventory and final custody transfer

A claim is inventory-guarded when it has `inventory_guarded: true` and the
API supports `recovery_inventory_v1`. Its hold stays **open** despite run
completion or an earlier **Available** archive. At a verified source/process
boundary, the worker freezes the unresolved inventory and constructs a
recovery-only aggregate covering the retained roots by ancestry, with one
current source tree. This aggregate never becomes the task branch or a
checkpoint, and introduces no new publication path.

Custody transfers only after the API acknowledges the final disposition:
either the exact selected available archive (capture id, source SHA and
`coverage_digest`) or a verified empty inventory with settlement evidence.
The owner-readable receipt remains visible through `uzi run recovery`, and
the selected available archive remains downloadable through `uzi run export`
after the hold closes, including after worker deletion. This receipt is an
identity acknowledgment; the worker verifies ancestry locally, not the server.
Closed custody does not authorize new archive reservations or uploads, and a
deleted worker's credentials receive 401 from worker authentication.

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
normal ready-retention window (7 days by default); protection is not
perpetual. Earlier non-final and legacy ready captures keep their existing
TTL. Size limits, quotas, owner-only access and explicit discard behavior
are unchanged.

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

## Automatic settlement of an older held generation

The ancestry settlement below applies to legacy, unguarded holds. Guarded
holds use the [final inventory disposition](#guarded-inventory-and-final-custody-transfer)
above; publishing one adopted head does not settle their divergent inventory.

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

Once **8 admission-counted holds per owner** (the *Admission-counted recovery
holds* limit above) accumulate, uzi pauses admitting **new** runs for you.
A run that was interrupted and requeued while it has 1 to 7 of its own open
holds keeps resuming; once that one run alone holds 8, it loses the exemption.
When that happens, the dashboard shows a full-width alert beneath the page
heading with your safety-slot use, how many held sources need a decision, how
many runs are blocked, and a **Review held work** button; if you connected
Slack, you also get one coalesced direct message per blocked episode (not one
per run) carrying your open-hold and blocked-run counts, a link to the surface,
and the exact discard command. The per-worker **retaining work** pills stay as
row context.

The **Workers** settings page is where you resolve them: every retained hold
is grouped by worker with the run it came from, its claim generation, and a
state that separates normal active protection from holds that actually need
you. Each row offers the right action for its state:

- **Export archive** — download a ready archive (same bundle as `uzi run
  export`). Downloading does not itself release custody. Legacy holds can
  release automatically when an archive becomes ready; guarded holds await
  final inventory acknowledgment even if an earlier archive is available.
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
capture failed, before archive readiness or capture progress is considered. An
available archive remains exportable; a latest preparing/uploading capture is not
yet downloadable, and an earlier archive may omit latest worker-local work. These
capture facts do not settle the owner decision or implicitly release custody.
Other terminal holds without a verified available capture report `source_only`,
rather than `active_protected`. An inventory-guarded hold also reports `source_only` while an
earlier archive is downloadable: that archive does not cover the full inventory
and does not settle custody. Recorded evidence or uncertainty at exhaustion holds the
run for owner Resume; the MAC rejection grants no extra allowance. Absence of
recorded evidence does not prove absence of unrecorded worker work:

> no recovery archive; custody of worker `<name>`'s local source is retained (export unavailable; it may be the only copy)

A rejected record cannot supply a source head or make an archive exportable.
Export is offered when an independently verified capture is available; an
independently verified pin must first produce an available archive.

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
settlement described above governs eligible legacy holds; guarded holds
require final inventory acknowledgment.

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
excluded from the later retention sweep. A refusal after a move or copy need
not restore the original layout.

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

If the API lacks `recovery_inventory_v1` or the claim is not guarded, the
worker emits an **UNGUARDED legacy generation** notice. Local pins may still
retain commits, but complete inventory protection at terminal release and
worker reclamation is unsupported. Once a generation's guard has latched,
later feature loss retains custody rather than falling back to legacy release.

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

When you're recovering a failed run's work, check in this order: whether a
salvage copy exists, then compare its tip against any available
[recovery archive](#downloading-an-archive), the run's
[retained checkpoint or recovery ref](#where-the-work-is-kept), a live
worker clone, and the worker's own tracking ref. Restore whichever verified
source is freshest. Never prefer an older salvage checkpoint merely because
it happens to be the one that's remotely reachable — any of the other
sources may hold newer work the salvage copy never saw.

Related: [Recovering from a transient interruption](run-recovery-wait.md) (a different,
earlier mechanism — a transient in-place retry, not a byte archive) ·
[Hosted workers](hosted-workers.md) · [CLI](cli.md)
