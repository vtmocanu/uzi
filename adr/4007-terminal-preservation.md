# ADR-4007: a run's terminal work is preserved under its own forge ref namespace, independent of custody

**Status**: Accepted (slice A implemented API-side and dark: nothing advertises the capability, so no ref is created, no gate fires and the sweep lists no rows; slices B to E and the retirement switch are deferred, see below)
**Date**: 2026-11-13
**Issue**: [vtmocanu/uzi#4007](https://github.com/vtmocanu/uzi/issues/4007)
**Refs**: #4007, #2545, #1810, #1867, #2507, #1296

## Context

A run's final work can be lost in two ways that custody does not cover.
[ADR-2545](2545-failed-run-held-publication.md) publishes held work to
`refs/uzi-held/<run-id>/<generation>`, but only while a custody hold is open,
and it retires that ref on a retention clock tied to hold release.
[ADR-1810](1810-checkpoint-retention-follows-custody.md) keeps the checkpoint
ref while custody is open and deletes it afterwards. Once custody is gone, a
finished run's last state survives only as long as the PR or branch the owner
happens to keep.

Slice A of #4007 builds the API half of a separate answer: a per-run terminal
ref whose lifetime follows the run's issue or merge request, not its custody.
It is built dark. Every claim below describes what slice A built, not what runs
in production.

## Decision

### A separate namespace and separate tables

A terminal ref is `refs/uzi-terminal/<run-id>`, one per run. It is not a held
publication and not a checkpoint or recovery ref. Two tables carry it. Neither
references `run_held_publications` or `checkpoint_retention`; a contribution
authorized by an open guarded custody hold names that hold (`hold_id`, an
`ON DELETE RESTRICT` foreign key), and is otherwise independent of custody:

- `run_terminal_preservations`: one row per run. States are `bound`,
  `publishing`, `live`, `retiring`, `deleted`, `abandoned` and
  `not_published`. A nullable `live_run_id` is an `ON DELETE RESTRICT` pointer
  to the run, set while the row is `publishing`, `live` or `retiring`, so a run
  delete cannot silently orphan a remote ref. Triggers pin the state machine.
  `retiring` is irrevocable: it may move only to `deleted` or `abandoned`.
- `run_terminal_contributions`: one row per (run, generation), with a
  disposition of `preserved`, `closed_issue_abandoned` or
  `closed_pr_abandoned`.

The reason CHECK on the completed-publication hold is widened (two follow-on
migrations) to admit the two gate reasons below. Migration numbers in the
branch are drafts and are renumbered at landing.

### Typed pushbroker entry points

`api/internal/pushbroker/terminal.go` adds typed create, update, list and
delete for `refs/uzi-terminal/` only. Updates are compare-and-swap
fast-forwards and are never forced; the delete is a CAS on the recorded tip.
The shared `validManagedRef` and the generic delete are unchanged, so no other
code path can delete a terminal ref, and the terminal entry points cannot touch
another namespace.

### Declared coverage, not proof of completeness

Before a push, the broker verifies the worker's declared coverage: every
declared root is reachable from the tip, each declared tree matches, and a
connectivity walk of everything reachable from the tip (the pack plus the
fetched bases) succeeds. A contribution reaching `published`
means only that. It is not proof that the declared inventory is the run's whole
inventory: omission detection is slice B and is not built.

### The worker route and the send-winner rule

`POST /api/worker/runs/{id}/terminal-preservation`
(`api/internal/handler/worker_terminal_preservation.go`, service in
`api/internal/workersvc/terminal_preservation.go`) is the only producer.

- **Authority** is either the current flight or an open guarded hold.
- **Lifetime** is classified once, by the same function the sweep uses: an
  issue-bound run consults only its issue; an issue-less run with a merge
  request consults only the MR; otherwise it is `no_pr`.
- **Send-winner rule**: at most one network send per generation. A marker is
  committed before the push (tx1); only the request that sets it sends. A losing request
  never sends, and a request whose commit is ambiguous never sends; a retry
  reconciles (lists the ref) when the marker is durable, and otherwise may win
  afresh and send once, so the bound stays one send per generation. The outcome
  is recorded in tx2 on a context detached from the request deadline. No forge
  call runs inside a transaction. The shape follows ADR-2545 step A.
- If the issue or MR is proven closed, the route records an abandoned
  disposition and pushes nothing.
- Refusals are typed (`apitypes.TerminalReason*`).
- The route is negotiated by worker capability
  `recovery_terminal_preservation_v1`. Without it the route refuses
  `unsupported` and writes nothing.

### The terminal_preservation sweep and the retire ordering

`SweepTerminalPreservations` (`api/internal/workersvc/terminal_sweep.go`) is
always registered and starts with one bounded, indexed due-row query; an empty
result returns before any credential resolution, forge call or write. Lanes:

- **Reconcile** (live): lists the ref and never sends. A ref at the
  contribution's tip records published; a ref at the prior tip or absent stays
  unknown under backoff; a foreign tip refuses the contribution and abandons the
  preservation without touching the ref.
- **Stale-bound** (live): a bound row with no invoked contribution, after a
  grace, becomes `not_published`. A row whose run no longer exists (repository
  removal cascades runs) is moved under the preservation row lock alone.
- **Retire**: implemented and tested, production-disabled. Its ordering is:
  1. Forge observation of the issue or MR, with the observation clock stamped
     from the database before the forge read.
  2. tx-R1 under the run lock: protections are re-checked, and an observation
     older than 15 s on the database clock is discarded.
  3. Commit the irrevocable `retiring`.
  4. CAS delete with no transaction open.
  5. Record `deleted` (ref absent) or `abandoned` (foreign tip), or stay
     `retiring` with backoff when the outcome is uncertain.

  `no_pr` runs never retire in slice A: they have no closure signal.

Lock order wherever several are taken: worker, run, hold, preservation,
contribution.

### Reopen semantics (narrowed)

A reopen that the forge already reflects before the observation keeps the ref.
A reopen after the observation is not promised recovery: the forge has no
compare-and-set on issue or MR state, so an issue reopened between the forge
answer and the `retiring` commit is not seen, and the decision is final once
committed. The interval is bounded by the 15 s maximum observation age.

### The retire lane is production-disabled, and why

`terminalRetirementEnabled` is a constant `false`, not a setting. While off, a
due `live` or `retiring` row is only pushed out (one scheduling write; no forge
call, no lifetime read, no delete), so every terminal ref is retained in this
delivery. Enabling it needs work that is not built: a consumer-admission fence,
a complete protection list, and indexes for the protection query (slices C and
D). A constant keeps the irrevocable delete unreachable by configuration error
until those exist. Tests enable the lane through an unexported hook only.

### Gate scoping: the completed-release gate

`verifyCompletedPublication` (`api/internal/workersvc/completed_publication.go`)
gains one scoped precondition before the `ReleaseCompletedPublicationHold` write. Only the branch-only completion release is
gated, and only when a terminal contribution exists for exactly the completing
generation:

| Contribution state | Outcome |
|---|---|
| `published`, `recorded` | release proceeds |
| `prepared`, `invoked`, `publish_unknown` | refused `terminal_preservation_pending` |
| `refused` | refused `terminal_preservation_refused` |
| any other state | fails closed as `ancestry_unknown` |
| no row | behavior unchanged |

The owner's discard is never gated. A contribution for another generation does
not gate this release.

### Failure model

Covered: API process crash, lost connection, timeout and lost ACK. This relies
on PostgreSQL defaults (fsync on, synchronous_commit on). There is no storage or
power-loss claim beyond that, and remote durability is the forge's.

| Crash point | Recovery |
|---|---|
| Before tx1 | No row, no send; a retry starts afresh |
| tx1 commit ambiguous | The request does not send; a retry reconciles if the marker is durable, otherwise it may win afresh and send once |
| After tx1, before tx2 | Reconcile |
| tx2 ambiguous | Reconcile |
| After the observation, before tx-R1 | No state change; a fresh observation next time |
| After tx-R1 | `retiring`; only the delete step repeats |
| Delete done, before tx-R2 | `retiring`, then list, then deleted or CAS retry or abandoned; never live |

### Removal guards

Repo and forge-connection removal return a separate 409 while pointer-bearing
terminal refs exist (`api/internal/handler/forge.go`), naming up to five refs
and the total. A race that reaches the RESTRICT FK maps to the same 409.

## Consequences

- **Dark delivery.** No shipped worker advertises the capability (there is no
  server-side switch: the capability is worker-reported), so no row is created,
  no ref is pushed, the gate never fires and the sweep lists zero rows. The
  schema, broker, route, sweep and gate are exercised by tests, not by
  production traffic.
- **Every terminal ref is retained** once the caller exists, until retirement
  is enabled. Retained refs block repo and connection removal.
- **Accepted limit: no owner escape for a removal-blocking ref.** Publication
  itself creates the blocking live pointer, so an owner escape (with CLI) is a
  prerequisite before any slice enables the publishing caller, not only before
  reclaim.
- **Accepted limit: a reopen after the observation is not recovered.** See the
  reopen semantics above.
- **Accepted limit: `published` is declared coverage only.**
- **Independence from custody** means neither `SettleRetainedCheckpoint`, the
  checkpoint supersession path nor the held sweep touches this namespace, and
  `validManagedRef` does not admit it.
- Finished rows are not purged yet.

## Deferred (tracked on #4007; none of this is shipped)

- **B**: worker capture and publication (the caller of the route), omission
  detection, and teaching the agent decoders the two new completion reasons.
  B must not advertise `recovery_terminal_preservation_v1` before D's owner
  escape for removal-blocking refs lands.
- **C**: reclaim and custody.
- **C/D**: retirement enablement: a consumer-admission fence, a complete
  protection list, the indexes for the protection query, and then flipping the
  constant.
- **D**: activation, and an owner escape for removal-blocking refs, with CLI.
- **E**: fleet rollout.
- **Purge** of finished rows.
