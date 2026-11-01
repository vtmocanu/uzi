# ADR-1392: pre-clone forge-unreachable park (reuse `recovery_wait`, add a cause)

**Status**: Accepted; amended 2026-10-29 for #3585 M1-M4 behavior and M5
documentation, and 2026-11-01 for the #1324 publication phase. The #3585
delivery was partial and non-closing; no hosted acceptance is claimed.
**Date**: 2026-09-16
**Issue**: [vtmocanu/uzi#1392](https://github.com/vtmocanu/uzi/issues/1392)
**PRD**: [prds/done/1392-forge-unreachable-preclone-park.md](../prds/done/1392-forge-unreachable-preclone-park.md) —
carries the full Decision Log (D1-D10), the milestone breakdown and the verified code
anchors; this ADR carries the durable design shape and the seams a future change must
not silently break.

## Context

A transient forge failure at clone or fetch time — a DNS blip, a dropped connection, a
5xx — used to escape straight into the generic terminal failure path:
`fail_origin = agent_failure`, the run's slot released, the owner left to notice and
re-dispatch by hand. A real incident measured this at 33 seconds end to end: cluster DNS
blipped on one node for well under a minute, and the same worker fetched the same host
successfully twenty seconds later, but `withForgeRetry`'s schedule (six attempts, 31s of
sleeping) had already been spent and exhausted before the DNS came back.

The runtime already has a park for a transient condition it cannot fix by retrying
in-process: an empty SDK turn parks in `recovery_wait` and the sweeper promotes it back
on a capped backoff (ADR-1197, issue #1197). But that existing park cannot be reused
as-is — it assumes a clone already exists to verify and capture, its custody rules have
no "never had a source" case, it carries no cause and no lifetime cap, and its release
call is best-effort rather than transactional. This ADR records the decisions that adapt
it for the pre-clone case without disturbing any of that.

## Decision

### D1 — Reuse `recovery_wait`, add a cause; no new state

The state already carries the promotion cadence, the owner-cancel path, and the run
surfaces (run page, TUI, `uzi run get`). A fourth park state would touch the thirteen-value
status list on every surface for no semantic gain the cause column doesn't already buy.
`runs.recovery_wait_cause` (nullable text, CHECK'd against `forge_unreachable`,
`empty_turn`, `provider_outage` at introduction; later causes are additive) makes a forge-only cap and forge-specific wording
possible without changing the empty-turn park's own contract (no lifetime cap, unchanged).
NULL means "untyped/legacy" — mapping an absent cause to `empty_turn` would mislabel the
existing provider park (PR #1385) that emits untyped today.

Because this reuses `recovery_wait` rather than introducing a competing mechanism, the two
ADRs that already govern that state and its custody rules are amended in place rather than
superseded:

- [adr/1197-transient-recovery-park.md](1197-transient-recovery-park.md) (2026-09-16
  amendment) — a pre-clone park is the one path that enters `recovery_wait` **without** a
  verified capture, because there is nothing yet to capture.
- [adr/1296-durable-run-recovery.md](1296-durable-run-recovery.md) (2026-09-16 amendment)
  — a fifth release-evidence class, `no_adopted_source`, for a hold whose generation never
  adopted a source.

### D3 — Original legacy custody settlement is atomic with the park

At introduction, the worker-side release call was best-effort and blind (it swallowed
transport and server errors and returned nothing), so "release the hold, then ask the server to park" cannot
promise zero open holds — a release that silently fails would leave the hold open forever
with no worker-visible signal. Instead, `SetState` settles both in one locked transaction:
it requires **exactly one** open hold for the exact `(run, worker, generation)`, releases it
with evidence `no_adopted_source`, and only then parks. The original terminal-cap
outcome is superseded by the 2026-10-29 owner-hold amendment below. Zero holds,
several holds, or any failed step rolls the whole transaction back rather than parking with
custody unsettled. This is the same atomicity principle ADR-1296 already applies to every
other release path, extended to a case that ADR didn't anticipate — a generation with no
source to prove anything against. This records the original legacy-hold policy;
inventory-guarded generations now retain the hold until exact settled empty-inventory
FINAL evidence, as described in [run recovery](../docs/run-recovery.md).
The 2026-10-29 hold/Resume policy does not weaken that boundary.

### D5 — Placement and warmth are independent facts, both tested

A promoted run stays `queued` with `worker_id` kept. Ordinary affinity still applies,
subject to the guarded immediate-escape policy in the 2026-10-29 amendment below:
a live heartbeat is not proof of direct Git reachability. Independently, the bare-repo cache
decides **warmth**: `ensureClone` warm-fetches when a bare already exists and cold-clones
otherwise, and a failed cold clone removes its own partial bare. The owner is typically warm
on a fetch failure (the bare survived) and cold after a failed first clone (the partial bare
was removed); a sibling may be warm from another run of the same repo entirely. This PRD
promises neither "same worker" nor "warm" — it promises both paths work, and tests each
independently (placement on the api's live-DB suite, warmth on the agent's real-Git tests)
rather than asserting a single conflated "resume is fast" claim that would only hold by
coincidence.

### D7 — The cause is additive and degrades capability-aware; the generation never degrades

A worker reporting `recovery_cause: "forge_unreachable"` to an api that predates the field
gets a strict-decode 400 (the wire boundary rejects unknown fields by design). The retry
strips the cause and, depending on what that older api advertised at claim time via its
`protocol_features` register, either keeps or drops `claim_generation` too — the #1247 state
fence accepts a bare generation on the state report; the `6603f793` baseline does not (its
generation lives only on the release request). Because an older api's untyped park touches
no custody at all, the fallback is allowed **only after a positively confirmed exact
release** (`released: true` for that exact generation) — never a guess, never a leaked hold.
The two protocol tokens this register carries for this PRD are `recovery_park_cause` (the
typed cause and cap) and `recovery_release_exact_echo` (the generation-exact release this
fallback depends on); a worker checks both before deciding whether it can even attempt the
typed report, so a mixed fleet degrades honestly rather than probing blind. The accepted
degradation on such a fleet is therefore: an untyped, uncapped park exactly like today's, or
today's safe failure — never a leaked hold, and the generation identity itself is never
allowed to degrade or get inferred.

## Amendment — 2026-10-29: owner hold and evidence-based placement (#3585)

(AI-synced 2026-10-29)

The #3585 incident remains the record of the live-worker/direct-Git failure.
Heartbeat liveness proves contact with the API, not reachability of the forge.
The original bounded-failure and ordinary-affinity policies above are amended
to stop repeated failing placement without sacrificing unverified local work.
The first delivery was partial and non-closing; publication-only recovery
(#1324) followed it and is the amendment below. No probes, new run states or
credentials are added by either.

### Cooldown and the ordinary half-open claim

An applied exhausted clone/fetch retry group updates one cooldown keyed by
authenticated owner, worker and canonical actual Git origin, not the forge API
host or repository path. Replayed/stale reports cannot count a second failure.
The hard admission filter excludes that worker/origin before the deadline.
When due, one ordinary claim reserves the half-open attempt under the existing
claim locks; expiry alone leaves reachability unverified.
Runs that never contact the forge are outside this filter: repo-less, chat and
egress-profile runs, and code cross-check children, which read only the lead's
local snapshot and take no reservation. A repository whose web URL has no
canonical https origin is not filtered either; its claim snapshot check refuses
it visibly instead of leaving it queued on every worker.

The reservation remains through human/model waits in the same active flight.
Optional negotiated direct clone/fetch success telemetry can clear the exact
reservation. Independently, an admitted worker-finalize completion for
`issue`, `mr_rework` or `self_improve` can clear it when the guarded hold,
completion identity, current authorized origin and reservation versions match.
Later broker publication, a receipt alone, a timer or an online heartbeat is
not this evidence. A newer failure invalidates old success evidence.

Other stopped outcomes release the reservation and leave the origin unverified.
A worker without direct success reporting therefore serializes that origin
for the active flight until an upgrade can report success (or qualifying
guarded completion clears it); a stopped flight does not retain a perpetual
reservation. See `forge_origin_reservation_lifecycle` in
[the success migration](../api/internal/store/migrations/00385_forge_origin_success.sql)
and [the authenticated observation/success queries](../api/internal/store/queries/forge_origin.sql).

### Immediate affinity escape requires the complete history

The ordinary affinity pin can be bypassed immediately for an unverified
worker/origin only when generations 1..G have complete immutable guarded
positive FINAL/source dispositions and no open hold in any generation.
The accepted dispositions and exact identity/source/coverage checks live in
`fn_forge_source_history_safe` in
[the placement migration](../api/internal/store/migrations/00382_forge_origin_placement.sql).
Missing generations, nonguarded evidence, multiple/conflicting sources,
prerequisite-dependent captures or local-only work do not qualify.
A bare available capture is not a FINAL disposition. Custody admission and
source/deletion guards remain independent; safe placement does not settle a
hold or prove a sibling is warm.

### Finite automatic allowance, explicit owner retry

After up to `RUN_FORGE_UNREACHABLE_MAX_PARKS` timed parks (default 6), the next
exhausted group remains `recovery_wait` / `forge_unreachable` with a NULL retry.
It neither fails terminally nor enqueues a judge. The lifetime forge count
stays saturated at the consumed allowance; the recovery backoff count still
increments. `0` preserves unlimited timed parks.

Dedicated owner Resume queues one bounded ordinary attempt, preserving
counts, custody, origin cooldown, unrelated grants and remaining wall budget.
Hold time is banked after execution has started; spent wall may require an
extension. Another exhausted group reholds. Cooldown, cordon, custody limits
(including one or already-saturated custody) and worker capacity can still
prevent admission: Resume is not an execution promise.

Compatible serving APIs prevent timers, token changes, `set-token` and generic
promotion from releasing NULL holds. An old API rollback can queue one extra
attempt via `set-token`, with legacy terminal-cap behavior. Older workers may
still say “retry after backoff”; current servers distinguish timed and held.
No new worker ACK protocol is required. See
[the hold/Resume writers](../api/internal/store/queries/runtime.sql) and
[owner Resume](../api/internal/handler/resume.go).

Archive export is an option when an archive is available, not a consequence
of a forge park. Local protected-source admission is unchanged.
Active [#2506](../prds/2506-prerequisite-closure.md) owns prerequisite closure;
[#2746](../prds/2746-automatic-hold-settlement.md) owns the automatic-settlement
programme. The #3585 delivery implements neither programme and no publication recovery;
the publication phase is the 2026-11-01 amendment below.

## Amendment — 2026-11-01: the publication phase (#1324)

(AI-synced 2026-11-01)

The pre-clone park above covers an outage at clone or fetch. A forge outage
met while **publishing** finished work (floor refresh, non-forced push, or
merge/pull request create) is a different park with a different budget. There
is no separate ADR or PRD for #1324; this amendment and
[ADR-1742](1742-finalize-resume-allowance.md#amendment-2026-11-01--1324-a-publication-intent-is-not-a-finalize-allowance)
carry the decisions.

### Publication phase

After the existing retry schedule, a sustained transient failure at those three
steps is `publication_forge_unreachable`. Auth errors, history or movement
proofs, local git errors, aborts and residue stay permanent. For an enrolled run
the agent captures source first, then reports `recovery_wait` /
`forge_unreachable` with `recovery_wait_phase=publication` (a run DTO field).
Unlike the pre-clone park it never bumps `forge_park_count`, is independent of
`RUN_FORGE_UNREACHABLE_MAX_PARKS`, and never releases custody or source. A
legacy untyped forge park of an enrolled run is refused (409
`publication_enrolled`) or redirected; a typed publication park of a run with no
active intent is refused (`publication_not_enrolled`). Post-merge-request hops
(description publish, title read/write, PR-head verify) are out of scope and
unchanged.

### Elapsed bound and the NULL hold

Timed retries use the capped backoff, raised to the failing origin's cooldown
when later. The wait is bounded in elapsed time by
`RUN_PROVIDER_PARK_MAX_DURATION` (default 24h), measured from the first park of
the owner episode and not reset by a later park, generation or worker; `0`
means no hold. At the bound the run becomes an owner hold: a NULL retry stamp
and `publication_hold_reason=publication_retry_exhausted`. Owner Resume queues
one publication-only episode (new wait clock; intent, custody and source kept);
it does no model work and grants no other budget. A publication-only resume
that cannot publish holds the same way with `publication_model_required`,
`publication_source_unavailable` or `publication_intent_mismatch`; the server
sweep holds a custodian-only run with `publication_custodian_unavailable`. A
peer may take the run only when a broker-confirmed checkpoint tip equals the
candidate and the original head and floor are its ancestors.

### API-origin extension of the cooldown

Migration 00392 extends the advisory placement cooldown of #3585 from a
worker's Git origin to its forge API origin. The API origin is derived on the
server from the repository (GitHub: `api.github.com` or `api.<host>`; GitLab and
Forgejo: the web host, equal to the Git origin), never from a worker report. The
same table carries per-surface `git_unverified` / `api_unverified` flags and the
run DTO's cooldown lists `surfaces`. An API failure clears only on the exact
current half-open run and generation success; a Git success does not clear it.

### Rollback and residuals

Writers opt in with the setting `uzi.publication_resume_api='v1'` plus the
worker capability `publication_resume_v1` (the pattern of [ADR-3343](3343-task-checkpoint-coordinates.md)), so an older
API's write to an enrolled run aborts when it changes the worker or claim
generation, re-opens a released claim, or moves the status into claimed,
running or an awaiting state. The 00391 Down refuses while enrolled rows exist.
`RUN_PUBLICATION_RESUME_ENABLED` (default on) stops advertising and new
enrollment only. Runbook: before an API rollback, convert enrolled queued runs
and timed waits to NULL holds. Accepted costs: an older API aborts on those
writes, and its owner Resume re-queues the held run as an ordinary attempt, not a
publication-only episode; the older API cannot then claim it, and after
roll-forward the unreset wait clock can re-hold it at the next park. Residuals:
in a mixed-version fleet an older API's clone-success path deletes a shared
cooldown row without checking the API flag, and after a 00392 schema rollback
API evidence on a shared row is lost because that Down drops the flag columns;
and the #3585 legacy extra-attempt residual above (an older API's
`set-token` promoter queuing one extra attempt against a NULL hold) is unchanged.

### Verification boundary

E2E phase 73 gains cases D and E for the publication park and owner hold
(`e2e/phases/73-forge-unreachable-park.sh`). They are written but were not run
in the delivery that added them; no hosted acceptance is claimed.

## Consequences

- A transient forge blip at clone or fetch parks on timed backoff within the allowance;
  exhaustion now holds for owner Resume/Cancel, as amended below. A timed retry is
  distinct from both worker-origin cooldown and an owner-held NULL retry.
- The forge-only cap (`RUN_FORGE_UNREACHABLE_MAX_PARKS`, default 6, `0` = unlimited) is a
  genuinely new lifetime limit on this one cause; the empty-turn park's "no lifetime cap"
  contract is unchanged, and the two counters (`forge_park_count` vs. the shared backoff
  counter) are tracked separately so one cause's cap can never leak into the other's
  schedule.
- Every custody-release path gains a fifth evidence class it must recognize alongside the
  original four; a future change to the release-evidence CHECK constraint or its Go
  vocabulary must account for `no_adopted_source` the same way it already must for
  `publication`/`archive`/`forge_no_output`/`owner_discard`.
- A mixed-version fleet is explicitly supported and tested: an older api gets today's
  behavior (untyped, uncapped park, or safe failure) rather than a wire error, as long as the
  worker can positively confirm the exact-generation release first.
- A permanent forge error (401/403/404) and an owner cancel during the retries are
  unaffected — both keep their existing immediate-terminal semantics; only the transient
  case is newly parked.
