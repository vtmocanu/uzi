---
title: Recovering from a transient interruption
order: 111
audience: user
---

# Recovering from a transient interruption

`recovery_wait` also includes owner-only [worker recovery exhaustion](#worker-recovery-exhausted), which has no timer or automatic resume. [Codex idle stalls](#codex-idle-stalls), [worker data volume full](#worker-data-volume-full) and retained-source exhaustion can also become owner-held. The transient-interruption retries below do not describe those holds.

Uzi retries in place and, if the trouble persists, parks the run in
`recovery_wait` on these transient interruptions:

- A **positively empty SDK turn** — a turn that finishes with a reported
  turn count of zero and no model activity, plan, question, or completion.
- A **transient Claude provider error** — a failed turn carrying a 408, 429,
  or 5xx status, including when its reason is not `api_error`; a failed
  `api_error` with no status also qualifies.
- A **transient Codex provider error** — on the Codex harness, a turn that
  ends failed because the provider was overloaded (`serverOverloaded`), had an
  internal server error (`internalServerError`), or had no flex capacity
  (`flexUnavailable`), or because Codex's HTTP transport failed (a connection
  or stream failure, or too many failed attempts) with a 408, 429, or 5xx
  status or no status. The exact Codex token `rateLimitExceeded` also qualifies.
  `usageLimitExceeded` keeps its [usage-limit handling](./run-limit-wait.md),
  and `sessionBudgetExceeded` keeps its existing handling.

Uzi retries the interruption a few times in place. A Codex provider error is
retried on the same thread up to 2 times per turn, after a short wait
(2 seconds, then 4 seconds), on top of Codex's own request and stream retries.
Cancelling, pausing, or reaching the wall-clock budget during that wait works
as usual. If the interruption does not clear, uzi saves a verified local
recovery checkpoint before parking the run in `recovery_wait`.

This recovery is automatic and requires no per-run setting. A missing plan
after a turn that actually did work, missing turn-count metadata, a real
wall timeout, and cancellation are not classified as a transient recovery.
Measured Codex idle timeouts generally use the [idle-stall policy](#codex-idle-stalls) below; qualifying zero-output silence after a provider-reported retry uses [provider-outage recovery](#provider-outage-streaks) instead.

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
backoff and returns it to `queued` when that delay passes. Empty-turn and
resumed-gate recovery have no lifetime cap or manual resume step. Negotiated
Codex idle stalls use the separate park cap below; typed provider parks use the
elapsed streak bound below.

An ordinary unwired resume can recover from the verified local checkpoint
with same-path session continuity when the saved session is resolvable and its
key has never entered retained-source recovery. Docker-wired resumes and
retained-source recovery use a fresh attempt path and model session. Publishing a checkpoint to the
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

## Codex idle stalls

Except for the provider-retry case below, a compatible worker reports measured Codex idle timeouts as `recovery_wait` /
`codex_idle_stall`, for both no model output (`zero_evidence`) and output that
stalled (`productive`). This covers lead initial planning, automatic plan
revisions and implementation. Human plan-revision productive idle still fails;
zero-evidence revision silence uses its existing in-flight revision driver.
Child turns, Claude and `idleOperation` keep their existing behavior. Cancel,
pause, spent wall, vault-lock, completion-hold and first-trip precedence remain.

Zero-output silence after a qualifying, current-turn provider rate limit or
transport retry goes directly to provider recovery, without another retry on
the interrupted root. The notice says "silence after a provider-reported rate limit or transport retry":
the retry report does not prove what caused the later silence. These recoveries use the [provider-outage streak and owner
hold](#provider-outage-streaks), rather than the idle park count below. A
permanent transport status does not qualify. Retry context is cleared by model
activity and at the next turn; final non-retrying errors retain precedence.
Human plan revisions still use their revision driver, with verified disposal
and a fenced fresh epoch before another turn, rather than an ordinary recovery
park.

By default, **three automatic parks** get finite retry times; the fourth stays
**held for the owner** with a NULL retry. `RUN_CODEX_IDLE_STALL_MAX_PARKS=0`
holds on the first park. The delay still uses lifetime recovery backoff plus
jitter. Positive completed `provider_progress_turns` on an idle park restarts
the idle count at 1; otherwise it increments with saturation. Current stalled
output is not a completed turn. This is a park-observed streak, not an exact
count of unsuccessful turns: progress in another flight that ends without an
idle park may be unseen, so the hold can arrive earlier. Reclaim, checkpointing
and credential parks leave the idle count unchanged.

1. With a finite retry, including one already due, wait for automatic recovery,
   choose **Resume** (`uzi run resume <id>`) to queue it early, or **Cancel**.
2. With a NULL retry, choose **Resume** when ready or **Cancel** to end it.
   Omitted timing means unknown, not held. Default CLI wait and log follow stop
   for the held form and continue through finite or unknown retries; explicit
   `--until` keeps its chosen status set.
3. Resume of either form clears the retry and idle count, queues a fresh wall
   window and revokes the Codex capability. It preserves lifetime backoff,
   provider streak, session, affinity, worker recovery episode and requeue
   baseline. Automatic promotion also resets the wall, so the default allowance
   can buy three extra full wall windows. A timed park can be promoted by
   set-token; a held one returns a named 409 requiring Resume first.

Accepted parks still require persistence, executor cleanup, a successful WIP
commit when dirty, fetch-back and verified ref coverage. Typing requires
`recovery_cause_codex_idle_stall_v1` and a positive, stampable generation before
capture; acceptance requires the exact returned status and cause. A full worker
ACK without a retry timestamp confirms held, whereas an omitted public DTO
field means unknown timing. These non-provider idle-stall errors have
`providerOrigin: false` and do not charge provider-outage accounting.
For those stalls, without feature support, zero-evidence idle keeps legacy untyped recovery and
productive idle keeps terminal failure. After negotiation, HTTP 400 or feature
loss does not trigger an untyped downgrade; the worker keeps the source.

Same-session recovery requires a resolvable ordinary unwired non-Docker path.
Retained-source recovery and Docker attempts use a fresh path/thread; rejected
thread resume keeps the `resume_lineage_break` warning and fresh-thread fallback.
Interrupted-turn notes, reconciliation and snapshots do not guarantee
exactly-once effects or prevent duplicate work.

An execution hold adds no custody retention rule. Ordinary settlement and
exact-generation ACK guards still apply; uncaptured, guarded, unresolved and
507-refused sources remain protected, and open holds still count toward admission.
The cap limits further generation minting for one run; it does not relieve storage
quota or admission pressure (#2969/#2544). Held runs block worker image rollout.
Deploy the API to all replicas before workers; mixed API versions and application
rollback while holds exist are unsupported because an old token promoter can
bypass the hold. See [configuration and destructive schema Down risks](configuration.md#codex-idle-stall-deployment-and-schema-rollback)
and the [#3509 decision](../adr/1197-transient-recovery-park.md#amendment-2026-10-27-3509-bounded-codex-idle-stall-parks).
No live rollout is claimed.

## Provider outage streaks

With a compatible worker and API, a provider-origin park stores
`recovery_wait_cause: provider_outage`. The run surfaces show the provider
park count, streak start and next retry. `provider_park_count` counts the
current streak, separately from the backoff counter; it is not a lifetime
attempt budget. `provider_park_streak_started_at` starts at the first park.

The default bound is **24 hours** (`RUN_PROVIDER_PARK_MAX_DURATION`;
`0` disables the hold while typing and counting remain enabled). At the
next qualifying provider park at or past that bound, the run becomes
`paused` with `hold_reason: provider_unavailable`, waiting for its owner
instead of retrying automatically. This policy does not fail the run.
Detection can lag by backoff, jitter, queue/claim delay and the next attempt;
there is no maximum overshoot promise or deadline-triggered sweep to a hold.

A provider park reporting positive `productive_turns` starts a new streak
at that park with count 1. These are completed, error-free model turns in
the current flight, including planning, implementation and review; claiming
the run or becoming `running` does not count as progress. Progress is not
limited to provider parks: a flight that completed turns and then parks for
another reason, an ordinary (untyped or `empty_turn`), typed idle-stall or disk
(`data_volume_full`) recovery park, reports them as `provider_progress_turns`,
and the API clears an open streak in the same statement that applies that
park. Missing or zero evidence leaves the streak untouched, and the report
must carry a positive `claim_generation`; the generation, status and release
fences reject stale or replayed evidence, so it cannot reset a streak twice.
Provider-outage owner Resume,
early credential-driven promotion and credential switching clear the streak.
Idle-stall owner Resume instead preserves the provider streak.
Automatic and vault-unlock promotion preserve it.

For a **provider unavailable** hold:

1. **Resume** (`uzi run resume <id>`) queues a fresh attempt and clears the
   streak, or **Cancel** (`uzi run cancel <id>`) ends the run.
2. To finalize completed work, eligible **Stop** first sets the scope ceiling,
   then **Resume** separately. From the CLI, run `uzi run stop <id>` and,
   after it succeeds, `uzi run resume <id>`. This is not an atomic endpoint.
   Stop requires a non-interactive milestone issue run with at least one
   completed milestone; other runs show why it is unavailable. Finalization
   still requires the existing completion permit and budget checks. Another
   provider outage during finalization can open a fresh streak.

The hold retains the claim, worker affinity and the issue lock on issue
runs. The worker keeps its clone and HOME; verified local captures can
contain unpublished work. These are not a guarantee of recovery after
losing that worker's storage: another worker can recover published work or
an available verified recovery copy, not an unverified capture or every
latest edit. Pending pause requests are satisfied and cleared by this hold.

Typed reporting requires the negotiated `recovery_cause_provider_outage`
feature. Old workers, or new workers against an API without it, send untyped
parks with the old behavior. If a worker cached the feature before an API
rollback, a positively identified older strict decoder's unknown-field
refusal triggers one untyped resend and disables the cached feature;
ordinary validation errors do not. Recognizing `paused` alone does not
make rollback safe: older APIs lack the provider hold's late-failure guards.

On ordinary and disk parks, `provider_progress_turns` is gated separately by
`provider_progress_on_recovery_park_v1` and a positive claim generation.
Typed idle-stall parks instead require this field, including zero, under
`recovery_cause_codex_idle_stall_v1` alone and do not downgrade. The fallback
below applies to ordinary and disk parks. If the API refuses it as an unknown field
(a positively identified strict-decoder refusal after a rollback), the worker
drops the cached feature and re-sends the same park once without the field,
keeping the fence; a refusal of that retry propagates, and there is no third
variant. An old worker or an old API therefore sends no progress evidence and
the streak is preserved, the conservative direction: the bound may fire
earlier than it would have, never later.

## Bounded retained-source recovery

Recovery of an already retained source runs inside the acquired claim,
with a durable budget bound to that source: **3 total reserved iterations**,
counting blocked and nonblocked failures together, and a **five-minute
deadline from the first reservation**. Reservation happens before work and
survives crash, reclaim and successor handoff. Retry delays use the existing
exponential backoff capped at 16 times its base. Capture, publication,
adoption or reclaim does not reset the budget; a trusted, successfully
settled real model turn does. An owner Resume of a
`worker_requeue_exhausted` hold or the retained-recovery retry described below
can renew it on a fresh claim with a higher recovery episode. Adoption failure
has a separate owner-only retry, described below.

### Retained recovery exhausted

A durably confirmed `budget_exhausted` retained source can park at
`recovery_wait` with cause `retained_recovery_exhausted`, keeping local source
and custody while releasing its execution slot. It does not require a new
capture when the exhausted budget prevents one.

By default, **three automatic parks** retry after roughly **1m, 2m and 4m**,
plus jitter. This cause has its own `retained_park_count`, separate from the
lifetime recovery-wait count and worker-death requeue allowance.
`RUN_RETAINED_RECOVERY_MAX_PARKS` sets the cap; **0 holds immediately**.
The fourth exhaustion at defaults stays held for the owner, without a timer
or another counted park. Resume does not reset that cap. The one exception to
"below the cap is counted" is a capacity-only window, described next.

**Capacity-only window (owner hold below the cap).** When a retained-recovery
window ends with every reservation the worker witnessed refused for
data-volume capacity (including a window that reaches its recovery deadline
or budget after at least one such refusal; a deadline that aborts a consumed
attempt, rather than a capacity refusal ending it, makes the window mixed), the
worker sends its
`retained_recovery_exhausted` park with the request-only field
`retained_capacity_only: true`, and only if the API advertises
`retained_recovery_capacity_hold_v1`. The API then holds the run for the
owner: `retained_park_count` is not incremented, there is no retry time
(`recovery_retry_not_before` stays NULL), and `worker_id` and source custody
are kept. It is not a disk park and does not reset the budget. Nothing new is
stored, so the CLI and web show the same owner-held state as an at-cap hold
(CLI text is unchanged). The worker posts one run-feed status: "retained
recovery held for owner: every recovery attempt in this window was refused for
data volume capacity; owner Resume required; predecessor work and custody
retained". Owner Resume gives one more window; a further all-capacity window
holds again. Mixed windows (any non-capacity failure), deadline-only windows
and every other case keep the ordinary park rules above (counted and timed
below the cap, held at it). An older API never advertises the feature, so it
never receives the marker; if a rolled-back API answers the marked report with
400, the worker falls back to one unmarked pass, which follows the ordinary
park rules above.

Trade-offs: freeing headroom alone no longer auto-resumes such a run, because
owner Resume is required. The hold keeps `worker_id`, so it can delay that
worker's image roll. The later non-claim after Resume is the separate
capability and image-roll cycle tracked in #4047, not part of this hold.

A fresh claim with a higher derived recovery episode charges one of three
reservations in a new five-minute window and must prove quiescence again.
A new claim alone, the same generation or the same episode cannot renew it.
Timer promotion gives a fresh run wall window, up to three at the default cap.
Manual Resume preserves the remaining wall budget instead; it can require an
owner extension first.

1. For a timed retry, wait, choose **Resume** to queue it early, or **Cancel**
   to end the run.
2. For an owner-held retry, choose **Resume** (`uzi run resume <id>`) when ready,
   or **Cancel** (`uzi run cancel <id>`). Both actions use existing owner checks.
3. Read `uzi run recovery <id>` before relying on an archive. Both timed and
   held parks keep source custody; an available earlier archive may omit latest
   local work. Losing the original storage can still lose that work. Another
   worker may resume from a published checkpoint after worker loss or affinity
   expiry without the original local recovery journal.

Finite retry timestamps, including due ones, mean timed recovery. A NULL retry
means an owner hold with no countdown; omitted timing means unknown. Default
CLI wait and log follow stop for the owner-held form, and follow reports a
transition from timed to held even within the same status and cause. Explicit
`--until` keeps its chosen status set. Token or credential changes do not
promote this cause. Both forms can keep storage occupied and block worker
image rolls. See [Configuration](configuration.md) and the
[ADR-1197 #3495 amendment](../adr/1197-transient-recovery-park.md#amendment-2026-10-26-3495-bounded-retained-recovery-parks).

The worker needs API feature support and exact durable journal confirmation.
Without that support or proof, exhaustion keeps the terminal fallback with
custody retained. Uncertain park responses instead stop nonterminal while
bounded reconciliation checks the outcome. Deploy the API fully before
upgrading workers; mixed API replicas and API rollback are unsupported.

### Retained recovery adoption failed

`recovery_wait` / `retained_recovery_adoption_failed` means adoption of the
retained work did not complete: an integrity check refused it, or another
adoption step could not be verified, including an ownership check right after
adoption that failed other than through a connection failure the worker retries
(connection refused or reset, a dropped socket, or a 502/503/504 answer). Local work and custody are preserved. This is an owner hold
with **no automatic retry**, regardless of any displayed retry timestamp.

The same cause also holds a successor run whose terminal predecessor's source on
the worker is under an active secret-scan remediation record (issue #4058): the
agent did not start, nothing was adopted, and neither quiescence nor adoption was
attempted. See the remediation paragraph in [Run recovery](run-recovery.md).

It also holds a run whose predecessor an independent restore does not support (a
canonical-path predecessor, a predecessor met by a hosted worker, or one with no boundary record, when the
earlier checks pass): the worker refused before any source effect, so nothing was
adopted, no adoption blocker was recorded and no quiescence was proven. Resume holds
again on the same release; an owner-held hosted run blocks its worker's ordinary image
roll until you cancel or discard it. See
[Run recovery](run-recovery.md#when-a-restarted-worker-continues-from-a-saved-pin-independent-restore).

1. Read `uzi run recovery <id>` and export an available archive before deciding;
   an earlier archive may omit the latest local work.
2. Choose `uzi run resume <id>` to request another bounded attempt, or
   `uzi run cancel <id>` to end the run. Resume does not guarantee success.
3. If adoption fails again, the run holds again. Without a fresh owner Resume,
   redispatch reholds without another capture or model turn.

A valid legacy budget-exhaustion renewal on an upgraded worker records its raw
owner baseline before recovery work. If adoption then fails, the next owner
Resume can authorize a new verified attempt. An already-blocked adoption journal
without that baseline cannot gain execution authority from Resume alone; retain
and export/recover its work instead.

Resume preserves remaining run wall time and may require an extension. Timers,
token changes and credential changes do not release this hold. Default CLI wait,
log follow and the watcher stop for the owner decision; explicit `--until` keeps
its status set. Retained sources can block image rolls and occupy storage; each
fresh claim opens another counted custody hold and repeated failures can leave
retained successor paths. Keep the original worker storage: a peer after worker
loss or affinity expiry may use a published checkpoint behind the retained work.

A temporary API connection problem during the worker's read-only ownership
check, after the successor was verified and journaled, is retried in place
before any hold: at most 5 attempts (about 15 s at the default recovery retry
interval), inside the iteration's existing deadline, on the same claim and
successor with no recapture and no extra recovery-attempt charge. Only a request
that failed before any response with `ECONNREFUSED`, `ECONNRESET` or
`UND_ERR_SOCKET`, or a 502, 503 or 504 answer, qualifies. Unknown errors,
malformed or truncated responses and other statuses are not retried, and
exhausted retries give the check's existing outcome: for the first check the
`adoption_failed` path described above (this hold when its preconditions hold),
and for the second check in the Codex/safety branch after executor disposal the
existing `quiescence_failed` failure with custody kept, not a hold. A deadline
that actually passes ends as `budget_exhausted`. The retry state is in-process
only. The hold itself still has no automatic retry. See the
[ADR-1197 #3994 amendment](../adr/1197-transient-recovery-park.md#amendment-2026-11-06-3994-transient-api-connection-failures-retry-the-ownership-check-in-place).

A positively identified temporary local resource error during the read-only
renewal check stops the claim without consuming its owner grant or creating a
new owner hold. The existing bounded claim-recovery policy may retry it.
Unverified or invalid authorization remains fail-closed; unknown errors do not
receive this retry treatment.

Owner authorization remains bound to the recorded repository path, device and
inode. A changed storage identity refuses execution; Resume cannot override that
check. With the source journal still valid, the run re-holds instead of failing
and keeps its local work and custody. Export/recover the retained work rather than
expecting Resume alone to rebind changed storage. Reholding does
not rebind the owner record or consume the pending execution grant.

The adoption path (not the issue #4058 remediation hold) requires API feature
support, exact persisted `adoption_failed` evidence, current ownership and proven quiescence. Missing support or unconfirmed
persistence fails closed with custody kept. An uncertain park response uses
bounded reconciliation and a nonterminal stop; it does not confirm the hold.
Deploy the API fully before workers; API rollback remains unsupported. See the
[ADR-1197 #3527 amendment](../adr/1197-transient-recovery-park.md#amendment-2026-10-27-3527-owner-held-retained-adoption-retry).

Other permanent blockers end the run **failed**, keeping local work and
custody, without automatic reclaim or failed-run Resume. The adoption retry
does not clear missing source, invalid clock, preservation failure, missing
needed prerequisites, oversized fallback or decoded-history blockers.
Actual quiescence failure uses `worker_residue_blocked`.
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

Local verification checks the full reachable object history, including
prerequisite history and object hashes, under the worker's configured delivered
decoded-history cap (default **8 GiB**, or 8589934592 bytes) and the existing
deadline. Missing or corrupt objects are refused. History above that cap keeps
work and custody and fails with a recorded blocker in `uzi run recovery`
and the worker log, even when the thin archive is below 64 MiB. Overflow,
timeout, corruption and interruption never authorize custody release or
source cleanup.

The configured budget counts delivered object contents cumulatively within a
recovery operation, including repeated verification reads; nested operations
share the same delivered-byte counter. Object contents stream through a pipe
chunk and bounded header, without content buffers proportional to the cap.
Metadata listings retain the existing 64 MiB output cap (`GIT_MAX_BUFFER`);
object arrays remain bounded by that listing. The delivered-byte cap is **not a
RAM limit** and does **not** bound Git-internal delta decompression memory. The
deadline limits duration, and the shared worker cgroup does not isolate this
verifier from sibling runs. The resulting residual resource-exhaustion risk
remains deferred scope, as authorized by the human review on 2026-10-09 (#2512).

See [Recovery decoded-history limit](./configuration.md#recovery-decoded-history-limit)
for worker startup configuration. Recorded cap errors show exact bytes and GiB
when the cap is at least 1 GiB. Raising the cap and restarting does not unblock
an already blocked journal: it reports previous decoded verification exhaustion,
the current cap, and that the prior cap was not recorded. Cancellation, existing
deadlines, encoded archive caps and storage quotas remain unchanged; there is
no deduplication or history exclusion.

Retained recovery uses a genuinely fenced fresh successor path and model
session on external restarts too, including on unwired workers. Losing the
retained storage can still lose work. A source-only failed run requires
operator recovery from that storage; export remains limited to a
manifest-bound available archive, with no new download API or failed-run
Resume. Downgrading workers during pending recovery is unsupported: older
workers may drop its durable recovery fields. See
[Recovering unpublished work](./run-recovery.md#retained-source-failure-and-local-proof).

### Unwitnessed continuation hold

A retained continuation is a run with an open custody hold from an earlier claim
generation. When the server's guard refuses the new flight's first `running`
report, the run stays `claimed`. (Separately, the worker refuses to reuse a local
source that an earlier flight never ended; a source held on the worker does not by
itself make the server refuse the report.) The
claimed-never-started sweep used to reset it to `queued` uncharged, and each new
claim opened another custody hold until the owner reached the admission limit
(issue #3859). Such a run is now disposed in one sweep transaction:

- The API advertises `run_registration_evidence_v1`; a worker that consumes it
  sends registration evidence (register nonce, claim generation, snapshot epoch)
  on the flight's first `running` report, recorded even when the guard refuses
  that report. After that, or after an earlier active snapshot listed the run at
  its generation (which older workers without the field can also provide), the
  next accepted active snapshot that omits the run stamps a **departure**.
- A witnessed departure is **charged** against `RUN_MAX_REQUEUES` (default 3)
  and requeued. At the episode cap the run parks at `recovery_wait` /
  `worker_requeue_exhausted` for owner Resume or Cancel; it is never failed.
- If the worker shows presence but no departure is witnessed within 2x the
  claim grace (claim grace is 5 minutes, so 10 minutes) of the claim, the run
  parks at `recovery_wait` / `retained_continuation_unwitnessed`. This cause is
  server-only: the claim is released, local work and custody are kept, and
  there is **no automatic retry**.
- Everything else keeps the uncharged reset: first-generation claims,
  non-continuations and flights with neither witness (no registration evidence
  and no earlier snapshot listing of the run at its generation). A run carrying
  a pending stop (for example one you cancelled while it was claimed) also keeps
  the reset, so the reclaimed worker can see that cancel. This is not a delivery
  guarantee: if the worker cannot act on the cancel where it refuses the source,
  the run stays bounded only by the per-run custody limit, and a cancel that
  lands just after a sweep parked the run needs a second `uzi run cancel <id>`.

For `retained_continuation_unwitnessed`, read `uzi run recovery <id>` and export
an available archive, then choose `uzi run resume <id>` or `uzi run cancel <id>`.
Timers, token changes and credential changes do not release the hold. Default
CLI wait, log follow and the watcher stop for the owner decision; the web run
page shows "unwitnessed continuation hold" with Resume and Cancel.

Resume requeues the run on the same worker (its local sources stay there) and
starts a new requeue episode. The worker registration that was released cannot
claim it again: the run continues only after that worker re-registers (for
example after its own restart) or on a different worker once the original
worker's claim affinity lapses (or it goes stale). A re-registration is
not proof that the earlier source use ended. The worker still refuses a source
that is in use, and a repeated refusal is bounded again in the new episode.
Nothing restarts the worker for you.

Boundaries. Workers that provide neither witness (an old agent that sends no
registration evidence and never lists the run in a snapshot) and workers that
re-register between refusals are not witnessed; they remain bounded only by the per-run custody
bound (`custody_hold_limit`, default 8, ADR-1751 D2a), which can still block the
owner. If `custody_hold_limit` is below the requeue budget the run stays
`queued` with health `custody_limit` instead of parking. Migration 00404's Down
refuses while any run is parked at this cause (Resume or Cancel it first) and
clears the cause on other rows.

## If it never recovers

Empty-turn and resumed-gate recovery have no lifetime park cap. Typed provider
recovery stops automatic retries at the elapsed streak bound and asks its
owner to Resume, use eligible Stop then Resume, or Cancel. You can **cancel**
the run at any time. Normal watchdogs still apply while it
is running, including while it retries capture; recovery does not override
a genuine timeout or cancellation.

## Forge unreachable at clone

A `recovery_wait` park can also come from a different cause: the forge
(GitHub, GitLab, Forgejo) was unreachable when the worker tried to clone or
fetch your repo — a DNS blip, a dropped connection, or a transient 5xx.
Local protected-source admission remains unchanged: discovered retained
source uses the acquired-claim recovery bound above. A forge park can also
follow a legacy warm fetch; it does not prove that no local source exists.
Existing source and custody protections remain in force, without promising
a new checkpoint or an available recovery archive.

### Worker-origin cooldown and safe placement

An **online** worker is heartbeating to uzi; it may still be unable to reach
the forge directly. An applied exhausted clone/fetch retry group updates a
cooldown for that authenticated owner, worker and canonical actual Git
origin. Repositories sharing that origin share the cooldown on that worker;
another worker or origin has its own admission decision.

Before the cooldown is due, that worker cannot claim the affected origin.
When due, one ordinary claim reserves a half-open attempt. Time passing does
not prove recovery. Human/model waits keep the reservation in the same
active flight. Negotiated direct clone/fetch success telemetry or an exact
admitted guarded worker-finalize completion for issue, MR rework or
self-improvement can clear it. Other stopped outcomes release the
reservation while leaving reachability unverified. Legacy workers without
direct success reporting serialize the origin during the active flight;
a stopped flight does not keep a perpetual reservation.

A sibling can bypass ordinary affinity immediately only with complete
immutable guarded positive FINAL/source dispositions for generations 1..G
and no open hold in any generation. Missing or nonguarded history,
multiple/conflicting sources and local-only work remain protected.
This placement rule grants no custody release or source deletion.
See [ADR-1392](../adr/1392-forge-unreachable-preclone-park.md)
for the evidence boundary.

### Timed retry or owner hold

`RUN_FORGE_UNREACHABLE_MAX_PARKS` (default 6) allows up to that many timed
forge parks over the run's lifetime. Each follows the capped backoff from
`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`. After the allowance
is consumed, the **next exhausted clone/fetch retry group** holds the run
in `recovery_wait`, with cause `forge_unreachable` and
`recovery_retry_not_before = NULL`. This is an owner hold, without terminal
failure or a judge run. `forge_park_count` remains the consumed timed
allowance; the hold does not increment it, though `recovery_wait_count`
still increments. `0` deliberately permits unlimited timed forge parks.

Explicit owner **Resume** queues one ordinary bounded attempt. It preserves
the counts, custody, forge cooldown and unrelated grants; it does not renew
the timed allowance. Resume preserves remaining wall budget and banks the
hold time after execution has started. If the existing wall budget is
exhausted, an extension may be needed before Resume succeeds. Another
exhausted clone/fetch group returns the run to the owner hold. Queuing does
not promise execution: cooldown, worker cordon, a custody limit of one,
already-saturated custody or worker capacity can still block admission.

This NULL-retry hold requires compatible serving APIs. On those APIs, the
timer, token changes, `set-token` and generic recovery promoters cannot
release it; use explicit Resume. An older API's `set-token` promoter can
still queue a NULL-retry hold, causing an extra attempt that can end in the
legacy cap-failure outcome. Upgrade the serving APIs together; this does
not require a new worker ACK protocol.

An older worker may still say “retry after backoff” without a retry stamp
even when the current server has owner-held the run. For timed parks,
existing badge/feed wording names the next retry time and
the lifetime forge park count; it must not be taken as proof that a
NULL-retry hold will resume automatically.

A permanent forge error (401/403/404 — bad credentials or a deleted repo)
still fails the run immediately instead of parking. Cancel ends the run
`cancelled`, as usual. Archive export is available when an archive exists;
a forge park alone does not create one. The original incident remains
recorded in issue #3585; first delivery is partial and non-closing.
This clone/fetch policy adds no probes, run states or credentials and does
not itself recover publication. A forge outage met later, while the run is
publishing finished work, is a different park with its own bound; see
[Forge unreachable while publishing](#forge-unreachable-while-publishing).

If a park report is sent but its reply is lost, the worker asks uzi whether
the run is parked, resending the report if it is not. When that check is
refused with an HTTP 4xx status other than 404, 408 or 429, or the resend is
refused with a 4xx status other than 400, 408 or 429 (a resend rejected as a
bad request still fails the run, as before), the worker stops retrying and
reports nothing. A 404 on the check still means the run is no longer owned by
this worker, and the worker cleans up as before. A 404 on the resend stops
without a second check and keeps the session. Timeouts, 429, 5xx responses and
transport failures are retried with backoff. A typed acknowledgement returned
for the resend, including a 409 body, is not a refusal and is handled as
before: it can confirm the park or take the failed path (for example
`custody_unsettled`). On a refusal, the worker keeps the session when it can
be resumed on that worker, keeps any publication record and source, leaves
custody unsettled, and leaves the run to the server or its owner. A refusal
is not treated as proof that the run ended. The same applies to the
data-volume park and to a claim already enrolled for publication whose clone
fails; the later publishing park below is a separate path.

## Forge unreachable while publishing

A run whose work is finished can meet the same kind of forge outage at
publication: refreshing the base (`git ls-remote`/fetch), the non-forced
push, or creating the merge/pull request. After the existing short retry
schedule, a sustained transient failure at one of those three steps is
classified as `publication_forge_unreachable`. Permanent failures are not
classified this way and keep their existing outcome: authentication or
permission errors, history or movement proofs, local git errors, aborts and
residue blocks. The hops after the merge request exists are unchanged and
are not covered: description publish, title read/write and PR-head verify.

One refusal that looks permanent is retried: a push refused only with git's
bare `! [remote rejected] <src> -> <dst> (failed)` line, with no other
diagnostic (observed from GitHub; reason undocumented, and whether it is transient is unproven). It is treated as
transient and the same non-forced push is retried on the same schedule, with
the full publication proof on every attempt. If it keeps failing, it is
classified `publication_forge_unreachable` like any other exhausted push. Any
other reason, a `[rejected]` line, authentication, protected-branch or hook
refusals, workflow scope and push protection stay permanent. A deterministic
refusal that prints only `(failed)` is therefore retried too: a run that is not
enrolled pays about 31 s of backoff before failing, and an enrolled run retries
until a publication park at or past the hold threshold
(`RUN_PROVIDER_PARK_MAX_DURATION`); the threshold is checked only at park
time, so it is not a hard duration bound. See
[ADR-284](../adr/0284-forge-push-retry-classifier.md).

### Write-ahead publication intent

Before any remote finalize step, the worker saves a local record and enrolls
it with the API (`POST /api/worker/runs/{id}/publication-intent`). The local
file is `publication-<G>.json` in the worker outbox, sealed with an HMAC
(domain `uzi.outbox.publication.v1`), and carries neither credentials nor
filesystem paths. The intent is at most 64 KiB; the sealed local record,
which adds the sequence numbers and the MAC, may be up to 1 KiB larger. The server copy lives in a side table,
`run_publication_resume`. Enrollment outcomes control whether remote finalize
steps may run:

- **Known not enrolled** (for example no outbox, an oversized record, or a
  server without the feature): ordinary finalize runs, with the diagnostic
  `publication_resume_unavailable` and no resume guarantee.
- **Initial enrollment ACK unknown**: the original live flight stays pending
  at claim generation G, keeping its ordinary active-run snapshot entry and
  durable intent. After the short enrollment retries, it waits with backoff
  and probes authenticated ownership. This applies to a sequence-less record
  with `ack_unknown`, or a typed HTTP 409 `disposition: stale_claim`; a
  reason string alone does not qualify.
- **Already enrolled**: the worker reconciles that exact enrollment and
  candidate before continuing; it does not blindly replay or replace them.
- **Other uncertainty**: conflicts, arbitrary server disagreements and a
  failure to persist the ACK locally (`ack_not_recorded`) keep the
  conservative retain outcome, without entering the initial-enrollment wait.
- A **transformed candidate** (base alignment, bridge) is saved and
  acknowledged before its push. An uncertain candidate update keeps its
  conservative handling; it does not enter the initial-enrollment wait.

During the initial-enrollment wait, a fresh ownership response must say
`running`, `claim_generation = G` and explicitly `claim_released = false`
before one retry of the exact durable intent body. A `queued` response at
that same G with the claim explicitly unreleased waits for ordinary heartbeat
re-adoption ([ADR-1390](../adr/1390-outage-requeue-readoption.md)); it permits
no resend. Unavailable or incomplete ownership evidence also permits no
resend. A later unknown ACK or typed stale-claim response requires another
backoff and fresh ownership check. The worker does not rerun the executor or
models. While enrollment remains uncertain, it makes no remote finalize
attempt and reports neither a terminal outcome nor a recovery park. After a
matching ACK is recorded locally, publication continues through the
applicable guards before forge steps.

The worker log line is `publication intent reconciliation waiting`, with
`run_id`, `claim_generation` (G) and one of two closed reasons:

- `ack_unknown`: the enrollment ACK or the required ownership evidence is
  still uncertain.
- `readoption_pending`: ownership is queued at the same G and explicitly
  unreleased, so the live flight is waiting for re-adoption.

The line is emitted when the waiting reason changes, rather than on each
probe. Cancellation, worker shutdown, the original finalize deadline or
flight abort stop the wait through the existing interruption handling,
retaining the source and session. Ownership loss (404, a changed generation,
a released claim or a terminal run) also stops the flight without publishing.
Other definitive ownership-read HTTP 4xx refusals stop conservatively too;
408 and 429 remain retryable, as do transport failures and 5xx responses.
The wait does not extend the deadline or grant another execution budget.
This is live-flight recovery (#3833); it does not promise recovery after
SIGKILL, shutdown or deadline expiry. The lost-flight residual remains
tracked separately in #3834.

Coverage residual: **feature-on terminal-send loss after enrollment ACK
remains unproved**. Feature-off terminal replay and feature-on case 3's
normal ordering do not establish that coverage. Hosted acceptance cases are
not claimed as executed here.

The worker advertises `publication_resume_v1` and lists acknowledged
enrollments under `publication_resume` in its register snapshot. A local
publication record or detected publication file withholds the run from
`finalize_resume`, even without an ACK or successful authentication. An
unlistable run directory is not detected by that file check. The one-shot
finalize-resume allowance ([ADR-1742](../adr/1742-finalize-resume-allowance.md))
is unchanged; withheld runs take the ordinary orphan requeue, which keeps
the intent. The intent stays active across automatic parks, owner pause or
resume and requeues. It is completed on completion and retired on terminal
failure, on cancel, and
on the owner's completion-hold `--continue`.

### Publication park

For an enrolled run, the worker captures the source first, then reports
`recovery_wait` with cause `forge_unreachable` and
`recovery_wait_phase=publication` (a run-DTO field; null for every other
park). This park:

- never increments `forge_park_count` and is independent of
  `RUN_FORGE_UNREACHABLE_MAX_PARKS`;
- never releases custody or the unpublished source;
- retries on the capped backoff, raised to the failing origin's cooldown
  when that is later (see below);
- is bounded in elapsed time by `RUN_PROVIDER_PARK_MAX_DURATION` (default
  `24h`) measured from the first park of the owner episode. `0` means no
  hold: timed retries only.

At the bound the run becomes an **owner hold**: a NULL retry time and
`publication_hold_reason=publication_retry_exhausted`. A run that is not
enrolled does not park this way; it fails with the typed
`publication_forge_unreachable` reason. A legacy untyped forge park of an
enrolled run is refused (409 `publication_enrolled`) or redirected to the
typed park, and a typed publication park of a run with no active intent is
refused (409 `publication_not_enrolled`).

Owner **Resume** of a publication hold queues one publication-only episode
and restarts the wait clock at its next park. It does no model work and
grants no other budget.

### Publication-only resume

A claim that carries `publication_resume` runs no executor and makes no
model or description-model pass. The worker verifies or adopts the server
intent and restores the recorded guard state; the live claim can only make
that state stricter. It requires the recorded original head, floor and
candidate to be present with ancestry proven, re-runs every publication
guard, and publishes. A resumed delivery that is not under the completion
interlock is non-closing unless its enrollment recorded the close.

When it cannot publish without model work or the recorded source, the run
holds for its owner (NULL retry time, custody kept) instead of failing, with
one of these reasons:

| Hold reason | Meaning |
|---|---|
| `publication_retry_exhausted` | The forge stayed unreachable for the whole bound. |
| `publication_model_required` | Publishing needs model work a publication-only resume does not do. |
| `publication_source_unavailable` | The recorded head, floor or candidate is not available. |
| `publication_intent_mismatch` | The recorded intent did not match what this worker can honour. |
| `publication_custodian_unavailable` | The worker holding the unpublished source is gone; set by the server sweep. |

A peer worker can take the run only when a broker-confirmed checkpoint tip
equals the candidate and the original head and floor are its ancestors.
Otherwise the run is custodian-only, and the server sweep holds it with
`publication_custodian_unavailable` when the custodian cannot serve it.

### API-origin cooldown

The worker-origin cooldown described above also covers a worker's forge API
origin. On GitHub that is `api.github.com` (or `api.<host>` for a GitHub
host); GitLab and Forgejo serve the API from the web host, so it equals the
Git origin and shares one cooldown row. The API origin is derived by the
server from the repository, never from a worker report. The row keeps
separate Git and API unverified flags, and the run's forge cooldown shows
which `surfaces` are unverified. An API-surface failure clears only on the
exact current half-open run and generation success reported by a worker
that negotiated it; a Git success does not clear it.

### Where you'll see it

- **CLI and TUI:** `publishing paused: forge unreachable; waiting 12m of 24h,
  retry at 15:04` for a timed wait, or `publishing held: <reason>` for an
  owner hold, each followed by the merge/pull request or branch when known.
  A hold adds the Resume and Cancel commands. See
  [CLI](cli.md#publication-parks-and-holds).
- **Web:** a **Publishing paused** or **Publishing held** pill on the run
  badge and a panel on the run page that names the reason and the actions.
- **Slack:** one wait line per owner episode, one line per hold, and a
  resume line only after an owner Resume of a hold. See
  [Slack](slack.md#using-it).

### Deployment and rollback

Writers opt in with the database setting `uzi.publication_resume_api='v1'`
and the worker capability `publication_resume_v1`; an older API lacks the
setting, so its write to an enrolled run aborts when it changes the run's
worker or claim generation, re-opens a released claim, or moves the status
into claimed, running or an awaiting state. The migration's Down refuses
while enrolled rows exist. `RUN_PUBLICATION_RESUME_ENABLED` (default on) only
stops advertising the feature and refuses new enrollment; existing intents
keep their meaning.

Before rolling the API back, convert enrolled queued runs and timed
publication waits to NULL holds. Accepted costs of a rollback: an older API
aborts on enrolled runs, and its owner Resume re-queues the held run as an
ordinary attempt rather than a publication-only episode; the older API cannot
then claim it (the guard aborts the claim's worker/generation write), and after
roll-forward the unreset wait clock can re-hold it at the next park.
Mixed-version residual: an older API's clone-success path deletes a shared
cooldown row without checking the API flag. After a 00392 schema rollback, API
evidence on a shared row is lost, because that Down drops the flag columns. See [ADR-1392](../adr/1392-forge-unreachable-preclone-park.md).

Not covered: the post-merge-request hops listed above, probes, new run
states or credentials, and any guarantee of execution after Resume (cooldown,
cordon, custody and capacity limits still apply).

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
  its own after an uncertain renewal or while verifying protected login
  material. Let recovery finish; unlock your vault or enable the login if
  needed. The run resumes automatically if recovery succeeds, or asks you
  to log in again if it cannot recover.
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

Automatic account recovery or a successful same-credential re-login re-admits
the run: the activity feed records the re-admission, naming the credential
and what changed. You can replace the login on that same credential while
recovery is still in progress; uzi verifies it before using it.

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

A `recovery_wait` park can also come from your own protected vault: a Codex
subscription or api\_key credential refresh or release reached a locked
owner vault. The api only answers this after the request was authorized and
authority still held on a recheck, so it is a typed refusal, not a generic
failure.

Rather than fail the run, uzi parks it. It confirms the run is still
running, brings its Codex processes to a stop without touching the
credential, then makes a verified capture of the work done so far,
publishing it credential-free when it can. It keeps custody of your source
and does not open the merge request while the vault stays locked.

A successful explicit vault unlock (`POST /api/vault/unlock`) or eligible
foreground browser restore makes a synchronous, best-effort attempt to queue
that owner's already parked `recovery_wait` runs with cause `vault_locked`, regardless of their retry
time or count. Other owners and causes are untouched. This queues the work
promptly; a worker still starts it through the normal claim path, so unlock
does not promise an instant resume.

The scheduled retry remains the fallback: the same capped backoff as an
empty-turn park (`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`),
with no lifetime cap. A database failure leaves unlock successful (204) and
preserves the Codex usage refresh poke; a park reported after the unlock's
queue update also waits for the timer. Successful protected password login,
passphrase enrollment and explicit enable/disable also use this promotion
path. Seed startup does not. Browser restore needs a live HTTPS grant,
valid session and incoming JWT-bound CSRF header; hidden/passive requests
and WebSockets do not restore. See
[Browser re-unlock](vault-threat-model.md#browser-re-unlock).

If the vault locks again between the unlocked-cache check and the queue
update, the run can be queued while locked, just as on the timer path.
Claiming it then idles; you'll see **your vault is locked, so this run can't
start** until an unlocked claim succeeds. After that, it resumes where it
left off — the resume costs at least one model turn before finalize opens
the merge request.

Off users have no vault-unlock dependency. A successful explicit enable or
disable also best-effort promotes existing vault parks after commit; see
[Vault choice](./anthropic-token.md#vault-choice).

A protected vault lock while sealing a refreshed login retains the successful
provider result in the recovery slot under owner/kind-bound server-key
sealing (`master_aad`). Current mode/epoch fences preserve that same result
across conversion races and background persistence retries without another
provider exchange. Updated workers survive a lost refresh reply: they reconcile
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
promotes the protected login before the run can resume. Recovering this
protected login does not exchange the original refresh token again. After a
separate uncertain renewal failure, uzi may make one automatic recovery attempt;
if that cannot recover the account, follow the request to log in again.

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
for it to. After three timed counted parks by default, the next counted park
waits for owner Resume/Cancel, without terminal failure (#3969). Operators can
configure unlimited timed counted parks.

Two kinds of park share the cause `data_volume_full`, and only one of them
counts toward the timed retry allowance:

- A **preventive**, uncounted park: the soft, per-run cache cap. At a turn
  boundary, a run whose own caches stay over their per-run cap across a
  few turn boundaries in a row is parked before it can fill the volume. A
  run that keeps triggering this park alone keeps timed retries and does not
  consume the counted allowance.
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
  `UZI_RUN_DISK_PARK_MAX` (default 3; `0` means unlimited timed parks). After
  that allowance is consumed, the next counted park stays in `recovery_wait`
  with cause `data_volume_full` and an explicit NULL retry time. It preserves
  `disk_park_count` at the consumed allowance, custody, worker/session affinity
  and unrelated budgets; the lifetime `recovery_wait_count` still increments.

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
newer worker-local work can be lost on another worker. Existing terminal,
cancellation and stale-claim dispositions still apply; cap exhaustion is a
nonterminal hold and grants no cleanup authority. See the
[operator policy](../adr/1809-per-run-cache-bounds.md#full-volume-terminal-failure-deferral-1829)
for the exact proof requirements.

The same bound applies to every park that lands **mid-run** with cause
`data_volume_full`: the counted hard stop and the preventive cache-cap park.
Three nonblocked unverified captures lead to the final safety proof. A
proven-quiescent run takes the degraded park above, with the same status text;
an unproven one fails `worker_residue_blocked` with custody kept. A capture
whose settlement is incomplete counts as a blocked attempt, and five
consecutive blocked attempts fail `worker_residue_blocked`, custody kept,
without running another capture. Cancellation, shutdown and a claim fence keep
precedence; a pending owner or wall-clock pause does not interrupt the proof
and stays pending for resume. A preventive degraded park stays uncounted, so
later preventive parks of the same run can recur; each is bounded on its own,
with no cross-park cap on uncounted parks. `UZI_RUN_DISK_PARK_MAX` bounds
automatic counted retries when finite (`0` is unlimited); owner Resume does not
renew that allowance. One residual
remains: reconciling an owed head after a disk-park publish still waits on the
pause and is skipped until the next claim; it does not block the park.

Preventive parks and counted parks within the allowance use the same capped
exponential backoff as an empty-turn park (`RUN_RECOVERY_PARK_BASE` up to
`RUN_RECOVERY_MAX_PARK`). An exhausted counted allowance has no retry timer.

### Resuming a disk hold

The authenticated run owner can **Resume** (`uzi run resume <id>` or bodyless
`POST /api/runs/{id}/resume-now`) or **Cancel**. Resume queues one ordinary attempt;
it does not reset the disk count, custody, worker/session affinity, recovery
episodes or other budgets. Admission may still refuse execution because of disk
pressure, cooldown, cordon, custody or capacity. A later counted disk park reholds;
a preventive park remains uncounted and timed. Resume credits held time after
execution has started, retaining `started_at` and the remaining wall budget;
an exhausted wall budget can require an extension. Cancel wins over a disk park
or Resume race once its stop verdict is recorded.

Idempotence is limited to the current hold: concurrent/duplicate requests cannot
queue that same hold twice. The bodyless request has no durable identity binding
to the hold the owner observed. A delayed duplicate crossing a later hold is not
durably fenced. That later hold requires another claim and counted park, which can happen
during preflight before model execution starts. Clients send one POST per owner
action and do not automatically retry it. No
concrete single-action replay has been found; this is not a durable replay fence.

On compatible serving APIs, timer, credential and generic promotion cannot wake
a NULL disk hold. Capacity returning is not an auto-resume trigger. Mixed older
API versions retain generic promotion behavior: finish the serving-API rollout
before relying on holds. This is application policy, not a database guarantee.

The hold adds no cleanup or reclaim authority. Existing park stop/quiesce and
safe rebuildable-cache drops bound resource activity, with no executor capacity
polling loop while held and no promise of power-loss durability. Open source
custody stays protected; there is no `forge_no_output` release exception.
Archives remain exportable and expire independently of the hold. Unpublished
worker-local work is not guaranteed across worker or storage loss. The distinct
#3865/#3882 successor-refusal path, #3330 recovery budgets and #3329 genuine-idle
reclaim/no busy-pressure policy remain unchanged.

### Retry timing contract

For `recovery_wait` / `data_volume_full`, an explicit
`"recovery_retry_not_before": null` means owner-held. A finite timestamp means a
timed retry, even if already due; an omitted key means unknown timing, not a hold
or a promised retry. The existing Go DTO's `RecoveryRetryNotBeforePresent`
records JSON key presence alongside its time pointer; this adds no wire field.
Malformed non-null JSON timestamps are rejected by the Go decoder rather than
interpreted as a hold. Surfaces with unknown timing show that uncertainty.

**Custody differs by when the park happens and whether the claim is inventory-guarded.**
An unguarded data-volume-full park that lands **before your repo has been cloned**,
with no retained source found by discovery, first releases this run's
recovery-custody hold. It uses the same release-then-park sequence as a
[forge-unreachable park](#forge-unreachable-at-clone): the release must be positively
confirmed before parking. An inventory-guarded pre-clone claim cannot use that
legacy release; a typed disk park keeps its custody hold intact, including on a
timed retry. A park that lands
**mid-run**, once your repo is cloned and work is underway, keeps the run's
custody hold instead: only this worker holds the committed work the park
is protecting, so releasing it could let another worker, or a recycle,
remove that work before it is recovered.

### Where you'll see it

- The run page's park panel reads **waiting for disk space**: the
  worker's data volume is full or nearly full, uzi frees space (including
  this run's own build caches), and the run retries after its scheduled time,
  subject to admission. A held park instead reads **Disk recovery needs your
  decision**, offering owner Resume/Cancel with no countdown or promised retry.
  Unknown timing is labeled separately.
- `uzi run get <id>` — a `DISK` row with that sentence and the run's
  lifetime count of **counted** disk parks (`counted disk parks: N`); a
  preventive park never moves this number, so it can read 0 across several
  parks. See [`uzi run get`'s disk and checkpoint
  rows](cli.md#disk-usage-and-checkpoint-durability) for the related `HOME`
  and `CHECKPOINT` rows every parked run can show.
- `uzi run list` / `uzi admin runs` — the STATUS cell appends `(waiting for
  disk space)` for timed parks; held parks say `(disk held; owner Resume/Cancel)`.
- `uzi tui` — timed parks stay in ON THE FLOOR; held parks need the owner and
  show Resume/Cancel without a countdown.
- Default `uzi run wait` and `uzi run logs --follow` stop at the owner hold;
  an explicit `run wait --until` keeps its requested status targets. Timed and
  unknown disk waits continue following.
- Historical failed rows remain readable: `uzi run get`'s `FAIL_ORIGIN` row
  can read `data_volume_full (the worker's data volume stayed full after N
  counted disk parks)`. This is historical compatibility, not current cap policy.

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

Each owner Resume also renews an exhausted or partly used retained-source
recovery budget (up to 3 more iterations, at most once per Resume), on the
run's next fresh claim. The retained-recovery parks above also grant bounded
renewals; ordinary server requeue, the
`data_volume_full` park retry, the memory-pressure Resume and a new claim alone
never renew it. See [Bounded retained-source recovery](#bounded-retained-source-recovery)
and the [ADR-1197 #3495 amendment](../adr/1197-transient-recovery-park.md#amendment-2026-10-26-3495-bounded-retained-recovery-parks).

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
  `retained_recovery_exhausted` has bounded timed retries followed by an owner
  hold (a capacity-only window is held for the owner below the cap); see [Retained recovery exhausted](#retained-recovery-exhausted).
  `retained_recovery_adoption_failed` instead requires owner Resume or Cancel,
  with no automatic retry, including for an unsupported restore predecessor; see [Retained recovery adoption failed](#retained-recovery-adoption-failed).
  `retained_continuation_unwitnessed` is likewise owner Resume or Cancel with no
  automatic retry; see [Unwitnessed continuation hold](#unwitnessed-continuation-hold).
- `limit_wait`: a usage-limit window must reset. See
  [Paused on a usage limit](run-limit-wait.md).
- `pool_wait`: no token is available in the selected pool. It needs an
  eligible token or an explicit resume attempt, not a recovery timer. See
  [Waiting for a token](anthropic-token.md#waiting-for-a-token).
- `paused`: an ordinary owner- or lead-requested pause requires owner
  resume. The same status also covers system holds, whose next steps depend
  on the hold reason. See [Pausing and resuming a run](run-pause.md).

Related: [Run health](run-health.md) and
[CLI run status](cli.md#run-status-and-what---follow-waits-for).
