---
title: Recovering from a transient interruption
order: 111
audience: user
---

# Recovering from a transient interruption

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

A same-worker resume can recover from the verified local checkpoint and
retained SDK session. Publishing a checkpoint to the forge is best-effort:
a different worker can recover only what was successfully published.
Losing the original worker's storage before publication can therefore
still lose newer work. Recovery does not promise protection against loss
of every copy.

If capture fails during shutdown, the worker retains the source clone and
session. A worker-owned ownership record prevents a later claim from
deleting that clone before recovering it. This protection requires the
worker's persistent storage to survive.

A run whose approved plan and work are available continues implementation
without requesting the same approval again. If a human-approved run loses
its work entirely, the existing plan-review safeguard still applies.

This fix does not reconstruct plan text missing from older runs. A legacy
auto-approved run whose stored plan is empty can still need a new planning
turn; an approval flag alone is not an implementation-ready plan.

## If it never recovers

The empty-turn and provider recovery park has no timeout or lifetime-attempt failure. You
can **cancel** the run at any time. Normal watchdogs still apply while it
is running, including while it retries capture; recovery does not override
a genuine timeout or cancellation.

## Forge unreachable at clone

A `recovery_wait` park can also come from a different cause: the forge
(GitHub, GitLab, Forgejo) was unreachable when the worker tried to clone or
fetch your repo — a DNS blip, a dropped connection, or a transient 5xx.
Unlike the empty-turn case above, nothing has been cloned yet, so there is
no checkpoint to capture; the run just parks and retries.

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

A `recovery_wait` park can also come from the Codex subscription account
itself: it is quarantined (a safety state, not a defect) or it needs a fresh
login, so uzi holds a Codex subscription run here instead of failing it.
Only the run kinds that hold custody of your source — issue, ci_fix,
self_improve, prompt, task and mr_rework — are held this way; a Codex chat
run still fails immediately (you're present to react to it), and so does a
judge run (it's advisory).

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
  back into the queue and will pick up where it left off. When the hold
  began at claim time, it prefers the worker that held its source.

A successful same-credential re-login re-admits the run: the activity feed
records the re-admission, naming the credential and what changed.

### No expiry, no timer

Unlike the forge-unreachable park above, this hold has no lifetime cap and
no countdown. It lasts until the account recovers, you log in again, or you
cancel. Cancel works exactly as it does for any `recovery_wait` run.

### When it fails instead

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
that lands **before your repo has been cloned** first releases this run's
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

## Other waiting states

- `recovery_wait`: a positively-empty SDK turn or a transient provider error
  persisted through bounded retries, the forge was unreachable at clone/fetch
  (see [Forge unreachable at clone](#forge-unreachable-at-clone) above), a
  Codex credential refresh or release found the owner's vault locked (see
  [Vault locked](#vault-locked) above), the worker's own data volume filled
  up or came close to it (see [Worker data volume full](#worker-data-volume-full)
  above), or the Codex subscription account is unavailable (see [Codex
  account unavailable](#codex-account-unavailable) above). The server
  retries automatically after a backoff, except the
  Codex account hold, which has no timer and instead resumes when the
  account does.
- `limit_wait`: a usage-limit window must reset. See
  [Paused on a usage limit](run-limit-wait.md).
- `pool_wait`: no token is available in the selected pool. It needs an
  eligible token or an explicit resume attempt, not a recovery timer. See
  [Waiting for a token](anthropic-token.md#waiting-for-a-token).
- `paused`: an owner-requested hold that requires the owner to resume it.
  See [Pausing and resuming a run](run-pause.md).

Related: [Run health](run-health.md) and
[CLI run status](cli.md#run-status-and-what---follow-waits-for).
