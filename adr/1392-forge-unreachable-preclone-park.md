# ADR-1392: pre-clone forge-unreachable park (reuse `recovery_wait`, add a cause)

**Status**: Accepted (PRD #1392 M1-M4 committed on this branch; M5 docs/ADR, M6
hosted acceptance are the maintainer's, out of band).
**Date**: 2026-09-16
**Issue**: [vtmocanu/uzi#1392](https://github.com/vtmocanu/uzi/issues/1392)
**PRD**: [prds/1392-forge-unreachable-preclone-park.md](../prds/1392-forge-unreachable-preclone-park.md) —
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
`empty_turn`, `provider_outage`) is what makes a forge-only cap and forge-specific wording
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

### D3 — Custody is settled in the same transaction as the park

The worker-side release call is best-effort and blind (it swallows transport and server
errors and returns nothing), so "release the hold, then ask the server to park" cannot
promise zero open holds — a release that silently fails would leave the hold open forever
with no worker-visible signal. Instead, `SetState` settles both in one locked transaction:
it requires **exactly one** open hold for the exact `(run, worker, generation)`, releases it
with evidence `no_adopted_source`, and only then parks (or fails past the cap). Zero holds,
several holds, or any failed step rolls the whole transaction back rather than parking with
custody unsettled. This is the same atomicity principle ADR-1296 already applies to every
other release path, extended to a case that ADR didn't anticipate — a generation with no
source to prove anything against.

### D5 — Placement and warmth are independent facts, both tested

A promoted run stays `queued` with `worker_id` kept, so the existing affinity rules decide
**placement**: the owner is preferred while it is live or draining, and a sibling can claim
after the affinity ceiling or once the owner goes stale. Independently, the bare-repo cache
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

## Consequences

- A transient forge blip at clone or fetch no longer fails a run outright; it parks, retries
  on the existing backoff, and resumes on its own. The owner sees "waiting for the forge,
  retry at HH:MM (N of MAX)" instead of a failed run to re-dispatch by hand.
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
