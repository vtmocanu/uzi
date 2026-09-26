---
title: Recovering from a transient interruption
order: 111
audience: user
---

# Recovering from a transient interruption

Uzi retries in place and, if the trouble persists, parks the run in
`recovery_wait` on two kinds of transient interruption:

- A **positively empty SDK turn** — a turn that finishes with a reported
  turn count of zero and no model activity, plan, question, or completion.
- A **transient provider error** — the Anthropic API returning 408, 429, or
  any 5xx (500/502/503/504/529), or a status-less transport error.

Uzi retries the interruption a few times in place. If it does not clear,
uzi saves a verified local recovery checkpoint before parking the run in
`recovery_wait`.

This recovery is automatic and requires no per-run setting. A missing plan
after a turn that actually did work, missing turn-count metadata, a real
timeout, and cancellation are not classified as a transient recovery.

A **permanent** provider error — for example 401, 403, or 400 (bad
credentials or a bad request) — is not a transient interruption. The run
fails fast with an accurate reason and does not park here.

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

If local capture cannot be verified, the run remains active while the
worker keeps its local work and session and retries capture. The feed says
so explicitly. It does not enter an automatically resumable park with only
an unverified capture attempt.

## What happens automatically

After verified capture, the server parks the run on a capped exponential
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

The recovery park itself has no timeout or lifetime-attempt failure. You
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
a recheck still found the vault locked, so it is a typed refusal, not a
generic failure.

Rather than fail the run, uzi parks it. It confirms the run is still
running, brings its Codex processes to a stop without touching the
credential, then makes a verified capture of the work done so far,
publishing it credential-free when it can. It keeps custody of your source
and does not open the merge request while the vault stays locked.

Unlike the Codex-account hold above, this park **does** resume on a timer:
it takes the same capped backoff as an empty-turn park
(`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`), with no lifetime
cap, rather than waiting on an external signal. Unlocking the vault does
not promote the run early — it still waits for that timer. Once the timer
promotes it back to `queued`, a still-locked vault means claiming it idles;
you'll see it queued with **your vault is locked, so this run can't start**
until the vault is actually unlocked. After that, it is re-claimed and
resumes where it left off — the resume costs at least one model turn before
finalize opens the merge request.

One case is not covered by this park: a vault that locks **after** the
provider exchange already landed, once the account is quarantined for it, is
answered with a same-operation retry refusal instead, and the run still
fails. This is tracked separately; see [issue
#1770](https://github.com/vtmocanu/uzi/issues/1770).

### Where you'll see it

- The run page's recovery panel and the run list, both reading **waiting
  for vault unlock**, with the next retry time.
- `uzi run get <id>` — a `VAULT` row with the owner-neutral park sentence
  and its next retry time.
- `uzi run list` / `uzi admin runs` — the STATUS cell appends `(waiting for
  vault unlock)`.
- `uzi tui` — this park stays in ON THE FLOOR and reads `~ vault wait`, like
  any other self-resolving recovery park; it also counts toward the board's
  vault-locked indicator alongside runs that are queued and blocked on the
  same lock.

## Other waiting states

- `recovery_wait`: a positively-empty SDK turn or a transient provider error
  persisted through bounded retries, the forge was unreachable at clone/fetch
  (see [Forge unreachable at clone](#forge-unreachable-at-clone) above), a
  Codex credential refresh or release found the owner's vault locked (see
  [Vault locked](#vault-locked) above), or the Codex subscription account is
  unavailable (see [Codex account unavailable](#codex-account-unavailable)
  above). The server retries automatically after a backoff, except the
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
