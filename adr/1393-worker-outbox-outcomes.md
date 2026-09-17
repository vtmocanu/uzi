# ADR-1393: Write-ahead terminal outcomes, the api fence, and claim protection

**Status**: Accepted (PRD #1391 Run B M3, M4 and M6 committed on this branch; M7 hosted
acceptance is the maintainer's, out of band).
**Date**: 2026-09-17
**Issue**: [vtmocanu/uzi#1393](https://github.com/vtmocanu/uzi/issues/1393)
**PRD**: [prds/1391-worker-outbox-durable-reports.md](../prds/1391-worker-outbox-durable-reports.md) —
carries the full Decision Log (D1-D13), the two-issue split (Run A #1391 ships the message
outbox; Run B #1393, this ADR, ships write-ahead terminal reports, the api fence and claim
protection) and the verified code anchors; this ADR carries only the seams other code must
respect and why they are shaped as they are. It is numbered by the Run B issue (#1393), not
the PRD file's own number (#1391).

## Context

The same 2026-09-15 outage that motivated the message outbox (Run A, #1391 — see
`docs/hosted-workers.md`/`docs/worker-setup.md`'s Message outbox sections and its own e2e
phase) also exposed a sharper failure: a run that actually *finished* during the outage —
its executor done, its PR already open — had no durable place to put that fact. Its
`completed` report exhausted its retries against a dead api, the executor's own fallback
`failed` report exhausted too, and the run was later re-queued and redone from scratch, at
real cost, even though the work was already complete. Spilling messages closes the feed
gap but not this one: an outcome produced and then lost is not a missing byte, it is a
wasted run.

Run B closes that gap with a write-ahead terminal journal (the outcome *is* the journal
from before the first send, not an after-the-fact retry artifact), an api-side fence that
refuses a terminal transition ahead of its own message trace, and claim protection that
keeps a second attempt from ever starting over a pending outcome. It also inherits and
extends four of Run A's mechanics — the authenticated outbox itself (D1), its
bounded/never-silent shape (D2), immutable generation-numbered segments (D12), and the
honest local retirement of a re-claimed attempt's stale work (D11) — because the terminal
journal lives in the same tree and rides the same crash contract.

## The seams

### D1 — Authenticated outbox in worker-owned `/data`; the hosted single-UID residual is named, not hidden

The terminal journal is not a new store: it lives in the same worker-owned, HMAC'd
`<dataDir>/outbox/<run_id>/` tree Run A built, keyed from the same worker-local secret (not
the join token, so token rotation never strands a pending outcome). Everything D1 says about
that tree applies unchanged to a `terminal-<claim_generation>.json` file: on the split-UID
runtime, file modes keep the runner out; on the hosted single-UID runtime (fact 7) the model
process shares the uid and can read the key and forge, truncate or delete any run's journal,
so the outbox — terminal reports included — guarantees durability and integrity against an
*outage*, never against a hostile model, until the container-split follow-up lands. A future
change must not treat the terminal journal as a separately-trusted store; it inherits D1's
residual exactly.

### D2 — Bounded, contiguous, never silent

Run A's shape — bounded, honest about what it drops, never a silent loss — extends to
terminal reports via two Run-B-specific bounds sized off the same physical-reserve
mechanism. The terminal canonicaliser caps each optional string field of a report at
`WORKER_OUTBOX_TERMINAL_MAX_BYTES` (1.25 MiB, above the 1 MiB preserved patch a journal can
carry) — a *per-field* cap, not a whole-report bound. The one preserved patch is truncated
rune-safely at a line boundary (with a marker), and every *other* oversized optional field is
dropped *whole* rather than byte-cut mid-value — either way no value is ever cut mid-token, so
a truncated secret can never leave an unrecognisable credential prefix past the api's own
scrubber. The physical reserve is resized from Run A's one range-record's worth to
`WORKER_OUTBOX_RESERVE_TERMINALS` (4) canonicalised-size journals plus per-record overhead
(about 5.25 MiB at the defaults), so
a terminal outcome can still be journaled crash-safely on a full volume. Past the reserve the
outcome is sent unjournaled exactly as it always was — logged and counted, never a silent
drop — the same degradation shape D2 already established for a message-quota overrun. Any
future terminal-shaped field added to a report must go through the same canonicaliser,
applied identically before the first send and to the journal, so first-send and replay stay
byte-identical.

### D3 — Terminal reports are write-ahead and fenced on `runs.last_seq`

Journaling only after retries are exhausted loses the outcome to a crash mid-retry and to a
typed 409 on the very first attempt; journaling before the first send makes the journal the
outcome from the start, so a crash anywhere after that point still has something durable to
replay. The fence a terminal transition must clear is not `runs.last_seq` alone — that
column is only a maximum, and inserts can leave a hole — but `last_seq` plus a contiguity
count over `[1..messages_through_seq]`, so a terminal report can never overtake its own
message trace: a judge, task-review or notification never fires ahead of a complete run
history. A worker that cannot close the gap fills it page-by-page through the message-gaps
read (bounded by `WORKER_GAP_FILL_MAX`) rather than parking forever; past that bound the api
answers a typed `gap_unrecoverable` 409 instead of implying an unbounded hole. Any future
terminal-kind report (a new executor class) must clear this same fence before it is allowed
any api-side effect.

### D4 — Terminal journals are keyed by generation, carry the phase, and are installed exclusively

Under #1247 a write under a stale generation is refused, so a journal that does not name its
generation cannot replay correctly under the right one, nor retire cleanly as `stale_claim`
under the wrong one. Installation is temp file + `fsync` + a no-replace install (`link()` or
`renameat2(RENAME_NOREPLACE)`, never a bare temp-and-rename, which would silently replace an
existing destination rather than refuse) under a per-generation single-flight, so the first
successful install wins outright: a `failed` report from the permanent-failure hook racing a
`completed` report from the same attempt resolves to whichever lands first, never to
whichever is written last. The phase recorded at journal time is exactly what #1390's
snapshot reports for a pending outcome across a restart. Any future writer of a
terminal-shaped record must install the same way — first-writer-wins is a crash-atomicity
property of the install itself, not an application-level check that could race it.

### D5 — A permanent message failure aborts the attempt

Before this PRD a permanent message failure reported `failed` while the executor kept
running — the split-brain the whole PRD exists to close, in miniature. The permanent-failure
hook now awaits its own `failed` journal's durable installation and then aborts the attempt
outright, so the run's on-disk state and the worker's actual behaviour can never disagree
again. A future executor path that can report a permanent message failure must follow the
same await-then-abort shape, never report-and-continue.

### D11 — Run A records generations and retires stale segments locally; claim protection is what actually closes the window

Before this PRD's M4, a re-claim could still bump a run's generation before Run A's message
replay completed; the old segments could not be written under the old generation, and
rebinding them to the new one would have fabricated frames the new attempt never produced —
so Run A's honest answer was a local retire with a warning, never a forged replay. Run B's
claim protection (M4, see the Consequences below) is what actually closes that window for a
run with a pending terminal outcome: while `terminal_pending` is listed (or the worker is
under `pending_overflow`), the api excludes the run from every claimant's candidate set and
leaves its generation unbumped, so the local-retire path D11 describes for a plain message
spill never needs to fire for an outcome already journaled. A future recovery path must keep
checking the pending-outcome lease before assuming a generation is free to move.

### D12 — Segments are immutable and one generation-numbered manifest is the only metadata authority

The terminal journal is its own file, not a mutation of a Run A segment, and is never
rewritten in place either: a stale-claim retirement or a block marks the journal via the
same crash-safe pattern D12 established for a segment rewrite (install a new state, never
edit in place), so the manifest and the journal can never disagree about a run's pending
status after a crash. Any future terminal-journal state transition (retire, block,
stale-retire) must be a new atomically-installed state, not an in-place field flip.

### D13 — A blocked outcome is surfaced and owner-resolved, never timed out

A journal the api permanently refuses (a completion-permit mismatch, or `messages_pending`
past the gap-fill bound) would otherwise hold the worker's pending set — and, through
`pending_overflow`, every run that worker owns — forever. So it is marked `blocked` with its
reason in the manifest, surfaced on the heartbeat (`blocked_reason`) and on the run
(`RunDTO.outcome_pending`), and resolved *only* by the owner's explicit cancel with
`discard_pending_outcome: true` (`uzi run cancel --discard-pending-outcome`, or the web
discard-confirm modal) — never by a timer, never by an automatic sweep. The resolution is one
atomic, owner-scoped, row-locked `UPDATE` (`CancelRunServerSideWithPendingOutcome`) whose
predicate re-checks the pending-outcome lease at the row lock, so a replayed `SetState` that
wins the race answers with the real terminal state and the cancel's own no-op 409 retires the
journal, rather than the two writers ever disagreeing. Any future code path that could
discard a pending outcome must go through this same confirmed, atomic branch — never a bare
status flip.

## Operational seams

- **`hasLivePoller`'s terminal-pending flip, and its deliberate `mr_rework` side effect.**
  `hasLivePoller` (`api/internal/workersvc/service.go`) now answers `false` for a run whose
  worker heartbeat is fresh but which sits under an unexpired `terminal_pending` lease (or
  its worker is under `pending_overflow`) — D13's positive form of the D11 claim-exclusion
  predicate: the *worker* is alive, but the *poller for this run* is gone, because its
  executor already exited after journaling. Every caller of `hasLivePoller` inherits this,
  and one of them has a consequence worth recording explicitly: the MR-close watcher
  (`forgesvc.cancelReworkOnClosedMR`, wired via `SetReworkCanceller` to
  `workersvc.CancelReworkForMR`) aborts an `mr_rework` run once its MR merges or closes
  (issue #853). `CancelReworkForMR` reads `hasLivePoller` and, on `false`, server-side-cancels
  through the plain `CancelRunServerSide` — deliberately *not* the confirmed
  `CancelRunServerSideWithPendingOutcome` branch D13 built for an owner-initiated cancel,
  because this cancel is system-triggered by the MR closing, not an owner discarding
  something they might still want: a closed MR makes the rework moot regardless of whatever
  outcome it might have journaled, and terminalising it immediately also clears a would-be
  `pending_overflow` stall rather than leaving a moot run occupying a pending-outcome slot
  until an owner happens to notice. This is also a genuine fix, not just a side effect: before
  this PRD, `hasLivePoller` would have read the worker as live purely from its fresh
  heartbeat, and `CancelReworkForMR` would have enqueued a cancel verdict for an executor that
  had already exited — a stuck run, sitting unconsumed. A future change to `hasLivePoller`'s
  pending-outcome branch, or to any caller that assumes "live poller" means "an executor is
  still reading its queue", must re-verify this MR-close path does not regress back to
  enqueuing a cancel nobody will ever consume.

- **Known gap: `CancelChatRun` does not handle `ErrOutcomePendingConfirmationRequired`.**
  `handler.CancelChatRun`'s error switch maps only `ErrRunNotFound` (404) and
  `ErrRunTerminal` (409); `ErrOutcomePendingConfirmationRequired` would fall through to its
  default and answer 500. In practice this is unreachable today: `SubmitInput`'s cancel
  branch (`workersvc/submit.go`) only queries the pending-outcome lease when
  `run.Kind != runkind.Chat && run.WorkerID.Valid`, and chat is excluded from terminal
  journaling and the claim fence entirely (D6; chat has no claim generation to fence on,
  #1390 D10), so a chat run can never produce this error today. It is left unguarded rather
  than fixed here because fixing it would be speculative code for a path no chat run can
  reach; a follow-up should add the explicit mapping (409 rather than 500) the moment chat
  gains any path that could journal a pending outcome, so the gap does not silently reopen
  into a real 500.

## Consequences

- A terminal outcome produced during an outage is now durable from before its first send: it
  survives a worker crash, a process restart, and an api that stays down far longer than any
  retry schedule, closing the "re-queued and redone from scratch" failure the PRD's Problem
  section names.
- The claim system gained a new kind of exclusion — a pending *journaled* outcome, not merely
  a live claim — and every sweep, timeout and requeue path that could move a run out from
  under a replaying journal must honour the `terminal_pending` lease and `pending_overflow`
  closure, the same invariant ADR-1390 D11 states for the api side of the same lease.
- A blocked (permanently refused) outcome is a new terminal-adjacent run state, visible on the
  run and resolved only by an explicit, confirmed owner action — never silently, never on a
  timer — which is the one place in this whole feature where automation deliberately stops
  and hands the decision to a human.
- Chat is out of scope for all of the above by design (D6, #1390 D10): it keeps the message
  outbox (Run A) and today's terminal behaviour unchanged, which is also why the
  `CancelChatRun` gap above is dormant rather than live.
- `WORKER_OUTBOX_TERMINAL_MAX_BYTES`, `WORKER_OUTBOX_RESERVE_TERMINALS` and
  `WORKER_GAP_FILL_MAX` are new operator knobs (see `docs/configuration.md`); their defaults
  are sized off the same physical-reserve and fence mechanics this ADR describes, not
  independent tuning surfaces.
