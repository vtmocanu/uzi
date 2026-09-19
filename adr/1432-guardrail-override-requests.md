# ADR-1432: a reachable member→admin path to the #66 guardrail override

**Status**: Accepted
**Date**: 2026-09-19
**Deciders**: Vlad (maintainer) + agent team
**Issue**: [vtmocanu/uzi#1432](https://github.com/vtmocanu/uzi/issues/1432) — the issue body and this run's commits carry the milestone breakdown and the full evidence base; this ADR is the durable subset: the seams a future change must not silently break.
**Numbering**: `1432` is the **issue** number, not an ADR sequence number (the convention ADR-0214/0238 record). A reader who assumes "ADR number == ADR count" will miscount.
**Extends**: [PRD #66](../prds/done/66-guardrail-enforcement.md) (guardrail enforcement, D8 admin per-repo override).

## Context

PRD #66 turned guardrail layer 1 from checked-and-reported into refused: uzi
refuses to enable or run a repo whose default-branch protection would let the bot
push or merge to `main`, and D8 gave an instance admin a per-repo override (a
required reason, actor + timestamp on the `repos` row) as the one sanctioned
escape hatch — no member self-allow path, because a member routing around a block
they consider wrong is exactly the risk (R6) the guardrail exists to close.

That override was **unreachable during first-time onboarding**. A repo the owner
has never successfully enabled stays DISABLED, so it never appeared in the admin's
Blocked-repos list (built from the stored privilege report of enabled repos), and
the only inline "Allow anyway" affordance lived on the admin's own Repos page. A
member whose very first Enable was refused for a waivable reason had no way to
reach an admin, and the admin had no surface on which to see the pending need.

This issue adds the missing path — a member *requests* an exception, an admin
approves or rejects — without weakening the invariant that only an instance admin
may allow a repo through the guardrail, and without giving the member any new
enable power. `protection_unreadable` stays fail-closed and non-waivable, so a
request is never even offered for it.

## The decisions

### A dedicated request table, one open request per repo

`guardrail_override_requests` (`repo_id`, `requested_by`, `reason`, `findings`
jsonb, `status` `pending|approved|rejected` under a CHECK, `created_at`,
`decided_by`, `decided_at`, `decision_note`) holds the queue. A **partial unique
index on `repo_id WHERE status = 'pending'`** caps exactly ONE open request per
repo: a re-request while one is pending is idempotent (upsert onto that index),
and the admin queue holds at most one row per repo. A decided request drops out of
the partial index, so the next refusal can open a fresh one. This is the data
model only; no guardrail gate ever reads this table.

Why a table rather than the `docker_repo_allowlist`-style `app_settings` list #66
weighed for the override itself: the request needs a per-owner audit trail, a
free-text reason, a snapshot of the blocking findings, and a decision record — a
settings list holds none of that.

### FK on-delete: `requested_by` CASCADE, `decided_by` SET NULL

`requested_by` is `ON DELETE CASCADE`, mirroring `notifications.user_id` and the
run/connection rows a user owns: a deleted requester's requests go with them.
`repo_id` is likewise CASCADE (a request is meaningless without its repo).

`decided_by` is `ON DELETE SET NULL`. This looks like the `updated_by` SET-NULL
trap the repo's CLAUDE.md flags and that #66 deliberately avoided for
`repos.guardrail_override_by` (which uses RESTRICT), but the reasoning does not
carry over: `guardrail_override_by` is the actor of a **live** override — nulling
it while the override still waives a block is an audit gap on an active safety
decision. `decided_by` is a **historical audit note on an already-decided
request**; it discriminates nothing live. A settled request is history that can
correctly outlive the deciding admin's account, so SET NULL is right here. The
authoritative live override actor remains `repos.guardrail_override_by` under
RESTRICT — this table never becomes a second home for it.

### The member request endpoint re-runs the LIVE guard, never the stored report

Creating a request re-evaluates the guardrail live with `Overridden: false` — the
same raw refusal set the Enable gate computes, never the cached privilege report
(D2) and never an overridden view. Three consequences, all server-enforced:

- The persisted `findings` snapshot is current at request time, so the admin sees
  *why* the member was refused without a fresh forge read.
- A repo the live guard does not block returns 409 (nothing to request — enable it
  directly); it never manufactures a request for an already-passing repo.
- A refusal whose block set is not fully waivable (`protection_unreadable`, which
  D8/D3 say an override must never waive) is refused **422 with no row persisted**,
  so a doomed request an admin could only reject is never created. Waivability is
  decided write-side, not left to the admin to discover.

The endpoint NEVER sets an override and NEVER enables the repo — only the admin
write and Enable do. It mounts in the same authenticated repos group as the Enable
action, owner-scoped (a non-owned or unknown id is a 404).

### Approval RE-RUNS the live guard before it arms the EXISTING #66 override; it never enables

Approve **first re-runs the live guard** (`GuardRepo` with `Overridden: false` —
the same raw refusal set the request path evaluates, so approve-time and
request-time agree on what "blocked" and "fully waivable" mean). Only when the repo
is *still* blocked by a fully-waivable refusal does approve settle the pending
request and, in the **same transaction** as the status change, set the existing #66
per-repo override on the `repos` row with the member's own reason, the deciding
admin as actor, and `now()`. It deliberately does **not** enable the repo: the
owner retries Enable so the live guard re-runs against current forge state and
remains authoritative. Approving an exception and spending the enable are kept as
separate acts. A request opened while the repo was blocked can go stale — the owner
may have fixed protection or enabled the repo before the admin decides — so
approval no longer blindly arms a snapshot; it re-judges first. This closes the
hole where arming an override on a no-longer-blocked repo would silently waive a
future regression that reintroduces a waivable block.

The revalidation yields one of three outcomes:

- **ARM** (`approveProceed`) — the repo is still blocked by a fully-waivable
  refusal. Fall through to the tx above: settle approved and set the override.
- **DISMISS as moot** (`approveDismiss`) — the repo is already enabled, or a
  successful live guard shows it is no longer blocked. Settle the request
  **rejected** with an explanatory server-authored `decision_note` and return 409.
  There is nothing to waive.
- **REFUSE but LEAVE pending** (`approveRefuse`) — the current block cannot be
  waived (not fully waivable). Because `GuardRepo` **fails closed**, this also
  covers a TRANSIENT forge outage surfaced as `protection_unreadable`. Return 409
  and leave the request pending. The point: a transient forge blip must not
  permanently destroy a still-valid request — an admin can retry approving later
  once protection is readable, or reject it deliberately.

Neither auto-settle path notifies the requester. The generic decided-notification
body ("an instance admin rejected your request") would misdescribe a SYSTEM
dismissal of an approve as an admin rejection; the persisted `decision_note`
explains the state change on the member's Repos page instead. (A deliberate admin
reject still notifies as normal.)

### A successful Enable settles any pending override request for that repo

When Enable succeeds, it deletes any pending override request for that repo
(`DeletePendingGuardrailOverrideRequestsForRepo`, from `SetRepoEnabled`), so a
request the owner rendered moot by enabling does not linger in the admin queue —
which is **not** filtered by enabled state. Enable-only: the shared toggle is also
the disable path, which must not touch requests. Best-effort — the enable already
succeeded, so a cleanup error is logged, never surfaced.

The persisted `findings` jsonb is **display/audit only**. It is never re-evaluated
and never consulted to decide safety at approve or enable time — the block decision
is always recomputed live. Reject settles the request with no override write (a
stale reject is harmless, so reject needs no revalidation).

### Requester notification reuses the generic notifysvc seam — no schema change

The decision notification to the requester goes through the existing generic
notification service under a new free-text kind, `guardrail_override_decided`.
`notifications.kind` is a plain `text` column with no CHECK constraint (it was
built generic, the judge being tenant #1), so a new kind needs no migration.

### Free-text hygiene on both cross-user surfaces

Both the member's `reason` and the admin's optional `decision_note` are screened
write-side with `termsafe.Unsafe`, which rejects C0/C1 control characters (Cc) AND
Unicode format/bidi characters (Cf, including U+202E). Both strings are rendered on
cross-user surfaces — the member's reason on the admin queue, the admin's note back
to the member — so unscreened text could smuggle a forged audit line or a
Trojan-Source display spoof. `IsControl` alone would miss the bidi/format
characters, which is why the check is `termsafe.Unsafe`, not a bare control-char
filter. An empty reason is 400, an over-long one is 422; the same length + unsafe
screen applies to a non-empty note.

### The goose migration number is a draft

The migration landed as `00237_guardrail_override_requests.sql` on this branch.
That number is provisional: renumber it above the live migration head at merge (and
after any sibling PR's migration), the standard renumber-only, filename-plus-comment
step — nothing else references the number.

## Consequences

- The #66 override is now reachable from a cold start: a member with a first-time
  waivable refusal has a path to an admin, and the admin has a queue to act on,
  without any new member enable power and without a member self-allow route.
- The fail-closed invariant is unchanged and now enforced one step earlier: a
  `protection_unreadable` refusal produces no request at all, so the doomed-request
  case cannot even enter the queue.
- `repos.guardrail_override_by` (RESTRICT) stays the single authoritative live
  override actor; the request table's `decided_by` is audit history and may go null
  when an admin account is deleted.
- Enable stays the one authoritative, live-evaluated gate. Approval now ALSO
  live-revalidates before it acts — it re-runs the guard and arms the override only
  when the repo is still blocked by a fully-waivable refusal, so it never blindly
  arms a stale snapshot — but it never substitutes for the live guard, and the
  stored findings never feed a safety decision. A moot request is auto-dismissed
  (on approve, or when a successful Enable settles it); a request whose block can't
  currently be waived is refused and left pending rather than destroyed.

### Accepted risk: the approve-time revalidation is best-effort, not race-free

The approve-time live guard runs before the arming transaction, so a TOCTOU window
remains: an owner could fix forge protection after `revalidateApprove` returns
"proceed" but before `SetRepoGuardrailOverride` commits, arming a standing override
on a repo that is momentarily clean. We accept this deliberately, because its
consequence is bounded by two properties this issue preserves rather than by the
narrowness of the window:

1. The #66 override is by design a standing admin permission to waive every
   *waivable* finding on the repo for its stale-after period (roughly 30 days), not
   a token bound to one finding or one forge snapshot (invariant #4). So "a later
   waivable regression is waived at Enable" is #66's intended behavior, present with
   or without the race, not a defect this flow introduces.
2. Enable stays the authoritative, live-evaluated gate (invariant #6): it re-runs
   the guard, and `protection_unreadable` (which a transient forge outage fails
   closed to) is never waived, override or not. A regression that makes protection
   unverifiable is therefore still a hard block.

Closing the window fully would require a forge-state-bound or one-shot override,
which re-architects #66 and contradicts invariant #4; if that trade is ever wanted
it belongs in a follow-up that revisits #66, not here. The approve-time
revalidation stays as a best-effort hygiene step that dismisses definitively-moot
requests, not as the safety boundary.
