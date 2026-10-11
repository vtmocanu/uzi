---
title: Recovering from a transient interruption
order: 111
audience: user
---

# Recovering from a transient interruption

`recovery_wait` also includes owner-only [worker recovery exhaustion](#worker-recovery-exhausted), which has no timer or automatic resume. The transient-interruption retries below do not describe that hold.

Uzi retries in place and, if the trouble persists, parks the run in
`recovery_wait` on these transient interruptions:

- A **positively empty SDK turn** — a turn that finishes with a reported
  turn count of zero and no model activity, plan, question, or completion.
- A **transient provider error** — the Anthropic API returning 408, 429, or
  any 5xx (500/502/503/504/529), or a status-less transport error.
- A **transient Codex provider error** — on the Codex harness, a turn that
  ends failed because the provider was overloaded (`serverOverloaded`), had an
  internal server error (`internalServerError`), or had no flex capacity
  (`flexUnavailable`), or because Codex's HTTP transport failed (a connection
  or stream failure, or too many failed attempts) with a 408, 429, or 5xx
  status or no status. Codex's own rate-limit and usage-limit errors are not
  included; they keep their existing handling.

Uzi retries the interruption a few times in place. A Codex provider error is
retried on the same thread up to 2 times per turn, after a short wait
(2 seconds, then 4 seconds), on top of Codex's own request and stream retries.
Cancelling, pausing, or reaching the wall-clock budget during that wait works
as usual. If the interruption does not clear, uzi saves a verified local
recovery checkpoint before parking the run in `recovery_wait`.

This recovery is automatic and requires no per-run setting. A missing plan
after a turn that actually did work, missing turn-count metadata, a real
timeout, and cancellation are not classified as a transient recovery.

A **permanent** provider error — for example 401, 403, or 400 (bad
credentials or a bad request) — is not a transient interruption. The run
fails fast with an accurate reason and does not park here. On the Codex
harness, a turn that ends with a transport failure carrying a permanent status
(400, 401, 403, and so on), or with an authentication, model, or other
non-transport failure, is not retried here either.

One empty result is routed elsewhere on purpose: if the turn came back empty
**because that attempt hit a hard usage limit** (its final rate-limit verdict
was a rejection), uzi treats it as a usage limit rather than retrying here. It
enters [`limit_wait`](run-limit-wait.md) and respects your `wait_on_limit`
setting — so it parks for the reset only if you opted into limit waiting, and
otherwise fails fast instead of cycling through recovery. An empty turn from
any other cause, or one where a later signal in the same turn cleared the
limit, still parks here as `recovery_wait`.

## What you'll see

- A **recovery wait** badge on the run.
- A run-view strip reading **Recovering and resuming automatically**.
- Activity-feed messages for in-process retries and the checkpoint saved
  before the park. The feed distinguishes a checkpoint saved on this worker
  from one successfully published for recovery on another worker.

For the empty-turn and provider recovery above, if local capture cannot be verified, the run remains active while the
worker keeps its local work and session and retries capture. The feed says
so explicitly. It does not enter an automatically resumable park with only
an unverified capture attempt.

## What happens automatically

For these empty-turn and provider recoveries, after verified capture, the server parks the run on a capped exponential
backoff and returns it to `queued` when that delay passes. There is no
lifetime cap on recovery parks and no manual resume step.

An ordinary unwired resume can recover from the verified local checkpoint
with same-path session continuity, provided its key has never entered
retained-source recovery. Docker-wired resumes and retained-source recovery
use a fresh attempt path and model session. Publishing a checkpoint to the
forge is best-effort:
a different worker can recover only what was successfully published.
Losing the original worker's storage before publication can therefore
still lose newer work. Recovery does not promise protection against loss
of every copy.

If capture fails during shutdown, the worker retains the source clone and
session. A worker-owned ownership record prevents a later claim from
deleting that clone before recovering it. A later acquired claim discovers
retained source before forge refresh or disk preflight and uses the bounded
recovery below. This protection requires the worker's persistent storage
to survive.

A run whose approved plan and work are available continues implementation
without requesting the same approval again. If a human-approved run loses
its work entirely, the existing plan-review safeguard still applies.

This fix does not reconstruct plan text missing from older runs. A legacy
auto-approved run whose stored plan is empty can still need a new planning
turn; an approval flag alone is not an implementation-ready plan.

## Bounded retained-source recovery

Recovery of an already retained source runs inside the acquired claim,
with a durable budget bound to that source: **3 total reserved iterations**,
counting blocked and nonblocked failures together, and a **five-minute
deadline from the first reservation**. Reservation happens before work and
survives crash, reclaim and successor handoff. Retry delays use the existing
exponential backoff capped at 16 times its base. Capture, publication,
adoption or reclaim does not reset the budget; a trusted, successfully
settled real model turn does.

A permanent blocker or exhausted budget ends the run **failed**, keeping
local work and custody. It does not park for automatic reclaim or offer
Resume. Actual quiescence failure uses `worker_residue_blocked`; a missing
source, invalid recovery clock, preservation or adoption failure, missing
needed prerequisites, or an oversized fallback is not a quiescence failure.
Transient capture, forge or disk failures consume the same reserved budget.
These bounds do not change the healthy provider/empty-turn park budgets or
the owner-only worker-recovery-exhaustion hold below.

An actual verified thin bundle under the size cap can support local
adoption and model execution when its needed prerequisites are locally
available and verified, even if remote publication is unknown. A nonempty
prerequisite list alone is not a blocker; missing or unverifiable needed
prerequisites, or a fallback still over the cap, are blockers. This local
proof does not prove independent recovery, remote durability or custody
release. Predecessor sources, pins, journals and descriptors stay retained
until the existing verified final disposition or explicit discard.

Local verification checks reachable object contents under a 1 GiB delivered
decoded-history cap and the existing deadline. History above that cap keeps
work and custody and fails with a recorded blocker in `uzi run recovery`
and the worker log, even when the thin archive is below 64 MiB. Overflow,
timeout, corruption and interruption never authorize custody release or
source cleanup.

The 1 GiB budget counts delivered object contents cumulatively within a
recovery operation, including repeated verification reads. The delivered-byte
cap does **not** bound Git-internal delta decompression memory. The deadline limits only duration, and the shared worker cgroup
does not isolate this verifier from sibling runs. The resulting residual
resource-exhaustion risk is deferred scope, as authorized by the human
review on 2026-10-09 (#2512).

Retained recovery uses a genuinely fenced fresh successor path and model
session on external restarts too, including on unwired workers. Losing the
retained storage can still lose work. A source-only failed run requires
operator recovery from that storage; export remains limited to a
manifest-bound available archive, with no new download API or failed-run
Resume. Downgrading workers during pending recovery is unsupported: older
workers may drop its durable recovery fields. See
[Recovering unpublished work](./run-recovery.md#retained-source-failure-and-local-proof).

## If it never recovers

The empty-turn and provider recovery park has no timeout or lifetime-attempt failure. You
can **cancel** the run at any time. Normal watchdogs still apply while it
is running, including while it retries capture; recovery does not override
a genuine timeout or cancellation.

## Forge unreachable at clone

A `recovery_wait` park can also come from a different cause: the forge
(GitHub, GitLab, Forgejo) was unreachable when the worker tried to clone or
fetch your repo — a DNS blip, a dropped connection, or a transient 5xx.
On this healthy path, retained-source discovery found no local source,
so there is no checkpoint to capture; the run just parks and retries.
If retained source exists, recovery uses the acquired-claim bound above
instead of this no-local-source forge park.

The run's badge and feed say **waiting for the forge, retry at HH:MM (N of
MAX)**, naming the next retry time and how many of this run's lifetime
forge parks it has used. It auto-resumes on the same capped backoff as an
empty-turn park (`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`) —
no action needed while `N` stays under `MAX`.

Unlike an empty-turn park, this one **does** have a lifetime cap:
`RUN_FORGE_UNREACHABLE_MAX_PARKS` (default 6). If the forge is still
unreachable past that many parks, the run fails instead of parking again,
so a genuinely dead forge does not hold the run open forever. A permanent
forge error (401/403/404 — bad credentials or a deleted repo) fails the run
immediately instead of parking, since retrying would not help. Cancelling
the run at any point during these retries ends it `cancelled`, as usual.

## Codex account unavailable

A Codex subscription run can enter `recovery_wait` before it starts, or
while running at a milestone checkpoint, done checkpoint, finalize or
recovery-capture boundary. The API must confirm that its account is
quarantined at the run's frozen identity and credential revision, or that
a re-login on the same credential is being verified. That account state
holds the run instead of failing it. This applies to issue, ci_fix,
self_improve, prompt, task and mr_rework runs without an egress profile;
chat, judge, job, cross_check and egress-profile runs keep their handling.

For a running run, uzi settles execution without requesting another Codex
credential, commits unfinished work, fetches it back into the worker's
tracking ref and verifies it **before** reporting the park. Publishing the
recovery checkpoint is best-effort: the feed says whether it was published
or saved on this worker. The source custody hold and session stay available
for recovery; this does not promise an available server archive or permanent
local clone. If capture or the park report cannot be verified, the worker
retains the local work and session and retries rather than claiming a park.

The run card, `uzi run get`, `uzi run list`/`uzi admin runs`, and the TUI
board each name one of four states (a state this client does not recognise
reads **Codex account unavailable**):

- **Codex account is reconciling** — the account is repairing its login on
  its own. No action is needed; the run resumes automatically once it
  clears.
- **Re-log in Codex credential \<label\> to continue** — the account has no way back on
  its own. Log in again in Settings, on the **same** credential — the same
  alias, the same ChatGPT account. Logging in with a different account does
  not resume the run (see below).
- **Verifying the new Codex login** — you already logged in again; uzi is
  confirming the new login belongs to the same account before releasing
  anything.
- **Codex account available again** — the account cleared; the run is going
  back into the queue and will pick up where it left off. It prefers the
  worker that holds its source, including when the hold began at a running
  checkpoint; normal worker-affinity limits still apply.

A successful same-credential re-login re-admits the run: the activity feed
records the re-admission, naming the credential and what changed.

### No expiry, no timer

Unlike the forge-unreachable park above, this hold has no lifetime cap and
no countdown. It lasts until the account recovers, you log in again, or you
cancel. Cancel works exactly as it does for any `recovery_wait` run.

### When it fails instead

This checkpoint protection needs an upgraded API and worker. An account
refusal during a turn's app-server refresh, or during startup credential
release, does not gain this account-park handling; their vault-lock handling
stays unchanged. Transient or ambiguous refreshes and non-credential
boundary faults keep their existing handling. A generic credential error
is not evidence of an account hold.

Logging in with a different ChatGPT account does not resume the run: it
fails with `credential_unavailable` and a reason naming the change. The
same happens if the credential is deleted while the run waits, or if the
account's credential revision no longer matches the one the run started
with. `uzi run set-token` does not accept Codex credentials; a Codex subscription run whose
credential is gone cannot be re-pointed at a different one, only
cancelled and re-created.

### Where you'll see it

- The run page's recovery panel, with the per-state copy above and, for a
  re-login hold, a link to Settings.
- `uzi run get <id>` — a `CODEX_ACCOUNT` row with the full sentence.
- `uzi run list` / `uzi admin runs` — the STATUS cell appends a short form
  in parentheses, e.g. `recovery_wait (re-log in Codex credential)`.
- `uzi tui` — a re-login hold is banded into NEEDS YOU as `⚿ codex login`
  and counted in the top summary's `⚿ N` segment. The other three states
  stay in ON THE FLOOR and read `~ codex wait`, like any other
  self-resolving recovery park.

## Vault locked

A `recovery_wait` park can also come from your own vault: a Codex
subscription or api\_key credential refresh or release reached a locked
owner vault. The api only answers this after the request was authorized and
authority still held on a recheck, so it is a typed refusal, not a generic
failure.

Rather than fail the run, uzi parks it. It confirms the run is still
running, brings its Codex processes to a stop without touching the
credential, then makes a verified capture of the work done so far,
publishing it credential-free when it can. It keeps custody of your source
and does not open the merge request while the vault stays locked.

An explicit successful vault unlock (`POST /api/vault/unlock`) makes a
synchronous, best-effort attempt to queue that owner's already parked
`recovery_wait` runs with cause `vault_locked`, regardless of their retry
time or count. Other owners and causes are untouched. This queues the work
promptly; a worker still starts it through the normal claim path, so unlock
does not promise an instant resume.

The scheduled retry remains the fallback: the same capped backoff as an
empty-turn park (`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`),
with no lifetime cap. A database failure leaves unlock successful (204) and
preserves the Codex usage refresh poke; a park reported after the unlock's
queue update also waits for the timer. Login, startup and passphrase
creation do not trigger this early promotion.

If the vault locks again between the unlocked-cache check and the queue
update, the run can be queued while locked, just as on the timer path.
Claiming it then idles; you'll see **your vault is locked, so this run can't
start** until an unlocked claim succeeds. After that, it resumes where it
left off — the resume costs at least one model turn before finalize opens
the merge request.

A vault lock while sealing a refreshed login also keeps the login protected
for recovery. Updated workers survive a lost refresh reply: they reconcile
the same operation once, then retain the work, session and custody if the
outcome remains unknown. A confirmed vault-lock reply uses the vault park;
an unknown outcome uses an ordinary recovery wait without claiming that the
vault is locked. Neither path opens a merge request or completes the run
before a successful resume through the existing completion checks.

If capture proof is temporarily blocked, these credential deferrals retain
the source and retry visibly with bounded backoff instead of failing after
the ordinary blocked-capture limit. Verified capture is still required to
park. Cancellation, shutdown and loss of the claim retain precedence; other
recovery causes keep their existing limit. After unlock, the recovery sweep
promotes the protected login before the run can resume. The original refresh
token is never exchanged again.

Deploy the API before upgrading the affected workers. Older workers keep
their existing first-request vault-lock reply, including pending retention,
but still need the worker update to survive a lost response. See
[ADR-1766](../adr/1766-codex-vault-lock-park.md)
for coordinated API replacement and rollback instructions.

Because the account is quarantined in the directly covered case too, a
resumed claim that arrives before the recovery sweep promotes the refreshed
login may briefly show the Codex-account hold ("reconciling") before it
resumes.

### Where you'll see the vault park

- The run page's recovery panel, reading **waiting for vault unlock** with
  the scheduled retry time. The web run list shows only a generic recovery wait
  status.
- `uzi run get <id>` — a `VAULT` row with the owner-neutral park sentence
  and its scheduled retry fallback time.
- `uzi run list` / `uzi admin runs` — the STATUS cell appends `(waiting for
  vault unlock)`, with no retry time.
- `uzi tui` — this park stays in ON THE FLOOR and reads `~ vault wait`, like
  any other self-resolving recovery park; it also counts toward the board's
  vault-locked indicator alongside runs that are queued and blocked on the
  same lock. Selecting the run shows the park and its scheduled retry fallback time on
  the row's second line and in the run detail, shortened to fit the
  terminal width.
- The repo board's run badge carries no vault-specific tooltip; check the
  run page for the detail above.

## Worker data volume full

A `recovery_wait` park can also come from the worker's own storage: its
data volume filled up, or came close enough that uzi decided not to wait
for it to. This cause has a lifetime cap by default and can **fail** the run
after enough counted parks; operators can configure that cap as unlimited.

Two kinds of park share the cause `data_volume_full`, and only one of them
counts toward the failure cap:

- A **preventive**, uncounted park: the soft, per-run cache cap. At a turn
  boundary, a run whose own caches stay over their per-run cap across a
  few turn boundaries in a row is parked before it can fill the volume. A
  run that keeps triggering this park alone cannot be failed by it.
- A **counted** park: everything else that reaches this park, including
  the hard, per-tick pressure stop — a background check on every stats
  tick that finds the volume at or over a hard threshold and stops
  whichever running Claude run holds the largest caches (a Claude run that
  is not running, such as one at its plan gate or waiting on a question, is
  not stopped; its rebuildable caches are dropped in place instead). Selection
  depends on registered runs with measured cache bytes, so it cannot guarantee
  a particular failing run parks before failure. Counted parks also cover a
  worker-owned clone/fetch write that failed disk-full (a recognised `ENOSPC`/`EDQUOT` signal, or
  git's own disk-full diagnostics, confirmed against the volume's own free
  space and inodes) after uzi ran its background reclaim and retried once,
  or a claim/resume preflight whose re-sampled statfs is still below the
  floor after a reclaim pass — note this last case counts a park with no
  failed write at all. Every counted case counts toward the run's lifetime disk-park cap,
  `UZI_RUN_DISK_PARK_MAX` (default 3; `0` means unlimited). Past the cap,
  the next counted disk-full park fails the run instead, with
  `fail_origin = data_volume_full`.

A counted park can also defer an issue run's otherwise-untyped executor
failure when its worktree and HOME are on the data-volume device and a fresh,
valid byte and inode sample confirms fullness. After execution and reports
settle, uzi waits for one bounded reclaim pass, then parks even if room
returns; it does not replay the command, provider or executor. This does not
attribute the failed write: an unrelated opaque error can coincide with
fullness and be deferred. For actual trusted refusals, only completely erased-origin opaque
failures remain eligible under this accepted coincidence residual. The cap
bounds repetition, not misclassification;
`0` remains unlimited. Recognizable typed, wrapped or trusted
security/guardrail failures remain excluded, including admission, launcher
and plan-wiring refusals. Preserved trusted types and `cause`/`interruption`
wrappers remain excluded. Complete legacy reasons are recognized directly
and through leading `<context>: <reason>` envelopes with a literal colon
and space. Contexts may contain apostrophes; double-quoted or multiline
contexts, alternate separators, quoted reason diagnostics and
producer-domain near-misses are not legacy refusal envelopes. Ordinary
opaque failures keep their existing handling. This boundary is pinned by the exclusion fixtures
in `agent/test/runner-terminal-disk-deferral.test.ts`. Setup,
finalize and settlement failures, typed recovery outcomes, cancellation,
pause and shutdown keep their handling. Unknown accounting, incompatible
APIs and plan-approval revisions are not covered;
existing cache relief at approval gates stays unchanged.

For this executor-failure policy, three nonblocked unverified captures or
five consecutive blocked safety proofs end capture retries. Alternating
blocks allow at most 15 capture calls plus five final proof attempts, using
existing waits and deadlines. Blocked or unverified quiescence fails the run
`worker_residue_blocked`. An affirmative process/supervisor proof can instead
allow a degraded park retaining the original clone, HOME, session, journal
and custody, without claiming a verified checkpoint or published latest work.
ACK reconciliation does not restart capture retries and has no fixed wall-time
promise. A same-worker resume captures dirty work before fresh execution;
newer worker-local work can be lost on another worker. Normal terminal,
cancellation, stale-claim and disk-cap cleanup still applies. See the
[operator policy](../adr/1809-per-run-cache-bounds.md#full-volume-terminal-failure-deferral-1829)
for the exact proof requirements.

Both kinds park and resume on the same capped exponential backoff as an
empty-turn park (`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`);
there is no separate timer for this cause.

**Custody differs by when the park happens.** Every data-volume-full park
that lands **before your repo has been cloned**, with no retained source
found by discovery, first releases this run's
recovery-custody hold — the same release-then-park sequence a
[forge-unreachable park](#forge-unreachable-at-clone) uses, and for the
same reason: nothing has been cloned yet, so there is nothing for this
worker to hold custody over, and the release must be positively confirmed
before the park is allowed to proceed (an unconfirmed release takes the
plain failed path instead of risking a leaked hold). A park that lands
**mid-run**, once your repo is cloned and work is underway, keeps the run's
custody hold instead: only this worker holds the committed work the park
is protecting, so releasing it could let another worker, or a recycle,
remove that work before it is recovered.

### Where you'll see it

- The run page's park panel reads **waiting for disk space**: the
  worker's data volume is full or nearly full, uzi frees space (including
  this run's own build caches), and the run resumes at its next retry. No
  action is needed. The board and run list show only a generic recovery
  wait status; see the STATUS-cell and `uzi tui` rows below for where the
  disk-specific wording does appear.
- `uzi run get <id>` — a `DISK` row with that sentence and the run's
  lifetime count of **counted** disk parks (`counted disk parks: N`); a
  preventive park never moves this number, so it can read 0 across several
  parks. See [`uzi run get`'s disk and checkpoint
  rows](cli.md#disk-usage-and-checkpoint-durability) for the related `HOME`
  and `CHECKPOINT` rows every parked run can show.
- `uzi run list` / `uzi admin runs` — the STATUS cell appends `(waiting for
  disk space)`.
- `uzi tui` — this park stays in ON THE FLOOR, like any other
  self-resolving recovery park.
- If the volume stays full past the park cap, the run fails instead: `uzi
  run get`'s `FAIL_ORIGIN` row reads `data_volume_full (the worker's data
  volume stayed full after N counted disk parks)`.

## Worker recovery exhausted

When automatic worker-death recovery is exhausted, the server decides from
recorded server state under the existing transition locks, without a forge
lookup. A persisted `checkpoint_tip` or available capture, any pending
publication attempt, a `preparing`/`uploading`/`needs_action`/unknown capture,
unsettled source custody across the owner's run at any generation, or
unavailable/unknown server evidence holds the run at `recovery_wait` with
cause `worker_requeue_exhausted`. No recorded recovery evidence or unresolved
custody keeps the existing `worker_lost` failure reason, including a
terminal-record MAC rejection. This does not prove that no unrecorded work
survives on the worker.

1. Open the run. **Worker recovery needs your decision** shows the automatic
   limit (including 0), episode used/remaining allowance, episode number and
   lifetime charged requeue count. Resume and Cancel are available to the
   confirmed owner. Other viewers see the hold without enabled controls.
   Open the run page from a board card to see the cause and allowance.
2. Read the historical evidence and uncertainty:
   Checkpoint copy: `The server recorded checkpoint <tip> before this hold. Current availability and the latest local edits are not verified.`

   Capture copy: `A recovery capture was recorded as available at <time>. It may later expire or be discarded; it may not contain the latest local edits.`

   Pending publication, pending capture, retained source custody and
   unavailable server evidence explain uncertainty, not an archive or
   export guarantee. Local clone availability is not promised; a Docker-lane
   pod loss cannot recover edits that were never recorded elsewhere.
3. Choose **Resume** on the run page, `uzi run resume <id>`, or the existing
   `uzi run resume-now <id>` to release this exact run hold and queue one
   explicit attempt. Each owner Resume starts a recovery episode with the
   current `RUN_MAX_REQUEUES` maximum automatic charges; at 0 it grants no
   automatic requeues. Claims still use normal generation and released-worker
   incarnation fences. Lifetime charged `requeue_count` and generation-proven
   readoption refunds survive: episode spend is count minus the episode
   baseline, not a reset monotonic physical-death counter. The existing wall budget
   survives; held time is banked only if its clock already started, and old
   approval/input waits are banked at park. Cancel uses the usual confirmation.

Historical evidence persists until owner Resume or Cancel. Capture expiry
neither fails nor promotes the hold; there is no automatic reclassification
or background absence finalizer. Timer, credential, vault, account, extend,
approval, follow-up and pause inputs neither release the hold nor renew its
allowance. Even a due retry timestamp does not schedule a self-retry.
Custody is never implicitly released. There is no innocent-sibling exemption:
worker registration cannot attribute which run consumed memory.

The once-per-run finalize-resume allowance (#1742) is restricted to initial
episode 0, with a positive maximum, exhausted allowance and unused lifetime
marker. Charged worker-death retries in owner-started episodes cannot exceed
their configured cap, and owner Resume cannot renew that marker. See [Configuration](configuration.md) and [ADR-1742](../adr/1742-finalize-resume-allowance.md).

### Deliberate schema downgrade

Before executing migration `00309` Down, stop or replace the newer application:
its queries require columns that Down drops. The table alterations and constraint
validation can lock and scan `runs`; plan maintenance around those operations.

This deliberate downgrade converts each `worker_requeue_exhausted` hold in
`recovery_wait` to a terminal `failed` run with `fail_origin = worker_lost` and
an explicit schema-rollback failure reason. It clears the obsolete cause and
retry timestamp, including on rows outside that hold, so the previous application
does not leave an unrecognized wait without a retry time. Exhausted work is not
automatically queued. Other wait causes keep their existing behavior.

Checkpoint tips, publication attempts, captures, source custody and released-claim
fences remain. Terminal checkpoint retention follows the prior recovery lifecycle:
open custody retains the checkpoint; otherwise it enters settling. Retained sources
do not preserve owner exhaustion Resume after downgrade. Reapplying Up creates new
default episode/baseline values and no historical evidence; it neither restores
the removed history nor reverses terminalization. This exception applies solely to
an explicit schema downgrade. During normal operation, the owner-only hold and
historical-evidence rules above remain in force.

## Other waiting states

- `recovery_wait`: a positively-empty SDK turn or a transient provider error
  persisted through bounded retries, the forge was unreachable at clone/fetch
  (see [Forge unreachable at clone](#forge-unreachable-at-clone) above), a
  Codex credential refresh or release found the owner's vault locked (see
  [Vault locked](#vault-locked) above), the worker's own data volume filled
  up or came close to it (see [Worker data volume full](#worker-data-volume-full)
  above), or the Codex subscription account is unavailable (see [Codex
  account unavailable](#codex-account-unavailable) above). The server
  retries timed causes automatically after a backoff. The Codex account
  hold has no timer and resumes when the account does.
  `worker_requeue_exhausted` is an owner-only exhaustion hold: explicit
  Resume starts a new episode; Cancel ends the run. It has no automatic retry.
- `limit_wait`: a usage-limit window must reset. See
  [Paused on a usage limit](run-limit-wait.md).
- `pool_wait`: no token is available in the selected pool. It needs an
  eligible token or an explicit resume attempt, not a recovery timer. See
  [Waiting for a token](anthropic-token.md#waiting-for-a-token).
- `paused`: an owner-requested hold that requires the owner to resume it.
  See [Pausing and resuming a run](run-pause.md).

Related: [Run health](run-health.md) and
[CLI run status](cli.md#run-status-and-what---follow-waits-for).
