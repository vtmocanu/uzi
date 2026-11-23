---
title: Issue 4609 live inventory queue investigation
order: 4609
audience: contributor
---

# Issue 4609: unavailable-hold visits and first inventory reconstruction

This report answers issue #4609. The question is whether the live inventory
discovery queue delays a newly owed inventory by spending opportunities on
retained contexts that currently have no eligible hold, and what the smallest
safe improvement would be. The report proposes; it changes no policy. Any
change to scheduling, fairness or authority is the owner's decision.

Code anchors are at `add869fc597d6bef732fede7f0576064d1b509db`. The probe
tests are the `#4609` block in `agent/test/recovery-inventory.test.ts`
(helper `issue4609Queue`).

Statements carry these labels. Section 4 (options) and section 5
(recommendation) are inference throughout, except where a statement cites
code.

- **[Code]** is a fact read from the cited source or test line.
- **[Incident]** is a fact reported for the #4570 incident.
- **[Measured]** is a count asserted by the committed controlled probe.
- **[Reported]** is a result an in-run validator reported that the committed
  tree cannot reproduce on its own.
- **[Inference]** is a conclusion drawn from the others and is not measured.

## 1. How the queue works

### Constants and pass spacing

- **[Code]** `agent/src/recovery.ts:1152` `const LIVE_MAX_PER_PASS = 4;` sets
  the per-pass cap.
- **[Code]** `agent/src/recovery.ts:1158` `const LIVE_BACKOFF_BASE_MS = 30_000;`
  sets the spacing between clean passes.
- **[Code]** A clean pass sets `nextPassAt = now + liveBackoffBaseMs`
  (`agent/src/recovery.ts:6222`).
- **[Code]** A transient pass backs off `min(base * 2^n, 15 min)`
  (`agent/src/recovery.ts:6219`).
- **[Code]** `resumeLive` runs only after a successful heartbeat
  (`agent/src/worker.ts:1335-1340`). The default heartbeat interval is 15 s
  (`agent/src/config.ts:692`).
- **[Inference]** On a healthy worker, consecutive live passes are therefore
  roughly 30 to 45 seconds apart, plus the pass's own duration. This is a
  conditional estimate derived from `LIVE_BACKOFF_BASE_MS` and the heartbeat
  interval, not a measured bound. It does not hold after a transient pass, a
  credential block, a slow pass, or a non-default configuration.

### Order within a live pass

**[Code]** `livePass` (`agent/src/recovery.ts:6121-6146`) runs three steps in
this order:

1. Discovery (`snapshotOwedInventory`).
2. The #3614 priority hint (`inventoryPriorityContexts`), only when the cap is
   at least 2.
3. Materialization (`materializeBootInventory`).

### Queue order

- **[Code]** Discovery keeps survivors in their existing queue order. It
  appends newly discovered contexts after them
  (`agent/src/recovery.ts:4349-4356`: `for (const [key, entry] of fresh) if
  (!entries.has(key)) entries.set(key, entry);`).
- **[Code]** Contexts that discovery no longer returns leave the queue.
  Contexts proven settled by complete verified journal records are skipped
  (`agent/src/recovery.ts:4334-4366`).

### Batch selection and per-entry checks

**[Code]** Materialization splices a batch of up to the cap from the front of
the queue. It rotates that batch to the back (`this.bootOwed.push(...batch)`)
**before** any per-entry check (`agent/src/recovery.ts:4405-4410`). The
per-entry checks then run in this order (`agent/src/recovery.ts:4416-4426`):

1. generation;
2. kind;
3. executing (`note("executing")`);
4. inventory feature;
5. journal read (`newReconstructionRecords`);
6. settled check;
7. hold RPC (`listRecoveryHolds`).

**[Code]** A hold miss records `note("hold_unavailable"); continue;`. The
opportunity is spent and the context stays queued. A second inactivity gate,
`inactiveInventory`, runs after the hold check (`agent/src/recovery.ts:4436`).

### What the #3614 priority covers

- **[Code]** The hint marks a context only when its generation has frozen,
  replaceable, inventory-guarded siblings with a matching canonical context
  (`agent/src/recovery.ts:4370-4378`).
- **[Code]** The hint reads the journal once for each distinct run in the
  whole queue, on every materializing live pass
  (`agent/src/recovery.ts:4380-4390`). Passes that are credential-blocked,
  lack discovery or the inventory feature (`agent/src/recovery.ts:6130`), or
  run with a cap below 2 skip it.
- **[Inference]** A newly owed context has no frozen record, so it can never
  be priority. The #3614 reservation covers only invalidated-inventory
  replacement, never first reconstruction.

### What a missing hold means

**[Code]** The ordinary hold listing returns only the caller worker's own
open holds (`api/internal/handler/recovery.go:200-201`). A missing hold is
not proof that custody is settled.

### What the existing decision accepts

**[Code]** ADR-1296's #3614 amendment
(`adr/1296-durable-run-recovery.md:816-845`) already accepts this behaviour:

> It rotates raw contexts (default cap four) before execution/hold checks;
> retained metadata with no eligible hold can therefore consume turns.

The same amendment bounds progress. Priority progresses within P completed
materializing passes, and normal survivors within `ceil(N/(cap-1))`. It also
states that pruning remains #2823's carried #2414 work.

## 2. What the #4570 evidence does and does not establish

The source for these facts is the #4570 incident comment on `573f99e8`, as
summarized in issue #4609.

### Established

- **[Incident]** The phase-42 boundary reported an open generation-1 hold that
  had not settled within the boundary's 120-second wait.
- **[Incident]** Phase 48 reported four fixture holds exceeding that wait.
  Later boundary history showed archive releases.
- **[Incident]** #4598 repaired the fixtures by resolving their custody
  explicitly. It did not change inventory scheduling.

### Not established

- **[Inference]** The incident shows wait expirations and eventual releases.
  It does not show end-to-end settlement time, the queue size at the time,
  completed-pass cadence, or what share of the delay came from
  unavailable-hold visits.
- **[Inference]** Other costs could account for some or all of the delay:
  - capture and upload;
  - FINAL replay;
  - transient backoff, up to 15 minutes;
  - heartbeat timing.

  This investigation does not attribute the incident's delay to the queue.

## 3. Controlled measurements

### Method

The probe drives the real path:

```text
resumeLive
  → livePass
      → snapshotOwedInventory
      → inventoryPriorityContexts
      → materializeBootInventory
```

It uses a stub clock: every visit advances `f.state.now` by 5 s, and fast
re-drive timing is enabled, so every visit runs exactly one pass. There is no
real 120-second sleep.

Diagnostics are counted by wrapping the instance's `noteInventoryDiagnostic`.
The log is not used, because `noteRedriveExit`
(`agent/src/recovery.ts:2143-2158`) deduplicates repeated reasons.

Reads are calls to `newReconstructionRecords` (journal reads), not filesystem operations. They are counted per phase of the pass:

- **skip:** during discovery;
- **hint:** during the priority hint;
- **materialize:** during materialization.

Retained contexts are fixture runs whose hold listing returns no hold.

**[Code]** Under the stub clock every operation takes zero time. The
probe therefore measures counts, not durations. Any time figure in this
report is an inference.

### Scenario 1: first reconstruction behind retained contexts

Setup:

- 12 retained no-hold contexts and a cap of 4.
- A newly owed target with an open guarded hold and no frozen record arrives
  after pass 1.
- When it arrives, the queue front is `r4`, so the target is enqueued behind
  all 12 retained contexts.

| Pass | Batch | Reads skip/hint/materialize | Hold RPCs total/distinct | Outcome |
|---|---|---|---|---|
| 1 | r0-r3 | 0/12/4 | 4/4 | 4 × `hold_unavailable` |
| 2 | r4-r7 | 0/13/4 | 4/4 | 4 × `hold_unavailable` |
| 3 | r8-r11 | 0/13/4 | 4/4 | 4 × `hold_unavailable` |
| 4 | r0-r3 | 0/13/4 | 4/4 | 4 × `hold_unavailable` |
| 5 | target, r4, r5, r6 | 0/13/6 | 14/4 | target `snapshot_created`, 3 × `hold_unavailable`, produced 1, uploaded 1 |

- **[Measured]** The priority set is empty on every pass.
- **[Measured]** 16 opportunities go to `hold_unavailable` on passes 1 to 4,
  before the target's selection pass. 12 of them fall on passes 2 to 4, after
  the target arrived.
- **[Measured]** The target is first selected on pass 5, which is the 4th
  pass after its arrival, and the real coordinator runs its capture and upload through the fixture's fake Git and API seams.
- **[Measured]** On pass 5, 11 of the 14 hold RPCs and all 10 ownership calls
  are the target's own.
- **[Code]** One of the target's hold RPCs is its selection-time check
  (`agent/src/recovery.ts:4424`). The other 10, and all 10 ownership calls,
  follow its selection, in the capture and upload path. The probe asserts the
  counts, not where each call comes from.
- **[Inference]** This matches the hypothesis. A target that arrives behind L
  queued contexts is first selected `ceil((L+1)/cap)` passes after arrival,
  here `ceil(13/4) = 4`. This holds when no #3614 priority reservation is
  active. If one stays active on every pass (as in scenario 2), ordinary
  contexts get only `cap-1` slots, so the wait becomes about
  `ceil((L+1)/(cap-1))`: `ceil(13/3) = 5` passes here.
- **[Inference]** The target arrives at some point between two passes, so
  the wait is three full intervals plus a partial one. Under the conditional
  30 to 45 s cadence, that is roughly 1.5 to 3 minutes, plus pass durations,
  before the target's own capture work begins. In general, every 4
  retained no-hold contexts ahead of a target add one pass of wait.

### Scenario 2: invalidated-replacement control (#3614)

Setup: 12 retained no-hold contexts plus one invalidated, replaceable target P.

- **[Measured]** Batches on passes 1 to 5:
  1. `[P, r0-r2]`
  2. `[P, r3-r5]`
  3. `[P, r6-r8]`
  4. `[P, r9-r11]`
  5. `[P, r0-r2]`
- **[Measured]** P is priority on every pass and takes the reserved slot from
  pass 1 onward. The remaining `cap-1` slots stay FIFO.
- **[Measured]** Each pass reads 0/13/6 and makes 5 hold RPCs over 4 distinct
  runs.
- **[Measured]** This fixture's target produces no fresh capture
  (`cloneHeads = [H2]`). It reports `invalidated_snapshot_reused` and stays
  priority, so the reserved slot is spent every pass.
- **[Inference]** This corresponds to the "permanent refusal may keep the
  reserved turn" case the ADR-1296 amendment names.

### Scenario 3: ordinary FIFO progress

- **[Measured]** Without a priority target, all 12 retained contexts are
  visited exactly once in 3 passes, which is `ceil(N/cap)`.
- **[Measured]** With one priority target, all 12 are visited exactly once in
  4 passes, which is `ceil(N/(cap-1))`.
- **[Measured]** Queue membership and multiplicity are unchanged after every
  pass.

### Scenario 4: a context that becomes eligible later

Setup: a context L placed last in discovery order, with its hold unavailable
at first.

- **[Measured]** While its hold is unavailable, L is selected on passes 4 and
  7. Each time it re-lists holds and never reaches the ownership check.
- **[Measured]** L's hold appears after pass 7. L is selected again on pass
  10, which is one rotation later. It passes the hold check and produces a
  capture.
- **[Measured]** L stays queued after every pass.
- **[Inference]** The current policy has no negative cache, so a context
  whose hold appears later is re-selected by ordinary rotation without any
  extra state.
- **[Inference]** Closed or discarded holds never reopen
  (`agent/src/recovery.ts:4426`). In production, a hold "appears later" only
  when it opens after the context was discovered, or when custody moves to
  this worker so the hold enters this worker's own listing. The probe models
  the transition only as the fake listing starting to return the hold.

### Scenario 5: an executing target becomes inactive

Setup: 8 retained contexts, with the target first in discovery order.

- **[Measured]** While the coordinator or pass predicate reports the target
  executing, it is noted `executing`. It spends its slot without a journal
  read or hold RPC and rotates.
- **[Measured]** After the predicate flips to inactive, the target is
  reselected on pass 3 and produces a capture.
- **[Measured]** A variant drives the later `inactiveInventory` gate. A
  running status gives `inventory_not_inactive` and rotates the target. Once
  the status is failed, the target is reselected on pass 3 and produces a
  capture.

### What the probe proves

- **[Measured]** In scenarios 1 and 4 and in the executing variants of
  scenario 5, the proof is a capture and upload that the real coordinator produces against the fixture's fake Git and API seams (`f.state.produced`
  and `f.state.uploads` each rise by 1 on the selection pass). The
  `inventory_not_inactive` variant asserts only the produced capture. None of
  them uses the capture fallback.
- **[Inference]** This proves selection, progress past the hold check and a
  produced capture. It does not prove a FINAL acknowledgement or custody
  settlement.

### Per-pass cost of a retained no-hold context

- **[Measured]** Each pass reads the journal once per distinct run in the
  queue for the priority hint, whether or not the run is selected. It then
  costs one more journal read and one hold RPC each time the context is
  selected.
- **[Measured]** The probe has no settled records, so the discovery-skip
  phase reads 0 throughout. Its cost was not measured.
- **[Inference]** With N retained no-hold contexts, a pass costs about
  `N + cap` journal reads and `cap` hold RPCs that make no progress. Real
  journal and RPC latencies were not measured.

### Mutation proof

**[Reported]** The in-run tester ran five mutants of `agent/src/recovery.ts`
in a scratch export, one at a time. The unmutated export passed. Each mutant failed its
intended scenario:

| Mutant | Killed by |
|---|---|
| Priority reservation removed | scenario 2 |
| `hold_unavailable` entries dropped from rotation | scenarios 3 and 4, among others |
| Cross-pass negative cache that skips a run after `hold_unavailable` | scenario 4 |
| Fresh arrivals placed before survivors | scenario 1 |
| Executing entries put back at the front of the queue | scenario 5 |

The mutants were never committed.

## 4. Options compared with the #3614 policy

Every option below keeps the fresh checks on every effect: custody, source,
ancestry, immutable FINAL, ACK and receipt, quota, and execution. None prunes,
discards, releases custody, relaxes provenance or sibling checks, or adds a
wall-clock guarantee.

| | Option | Queue delay for a first reconstruction | Added reads, memory and staleness | Later-eligible / executing→inactive | Gate |
|---|---|---|---|---|---|
| A | Keep the current policy. | Unchanged: `ceil((L+1)/cap)` passes after arrival with no active #3614 reservation, or about `ceil((L+1)/(cap-1))` while one stays active. | None. | Both proven by scenarios 4 and 5. | None. ADR-1296 already accepts this trade-off. |
| B | Per-pass memo of hold listings by `runId`. | Unchanged. | Memory is one listing per distinct run in a pass, with staleness of at most one pass. Saves hold RPCs only when one run has several queued contexts in the same batch, which the probe never shows. | Unchanged, because the memo dies with the pass. | Needs a decision that a one-pass stale listing is acceptable, since the capture path re-checks its hold. Small, but it saves nothing in the measured case. |
| F | Put the hold RPC before the journal read for each entry. | Unchanged. | Saves at most one journal read per no-hold selection, `cap` per pass at most. The hint's queue-wide reads are untouched. | Unchanged. | Inverts the provenance-first order (`agent/src/recovery.ts:4421-4424`). A context that is settled but has no hold would then cost an RPC instead of a local read. The saving is small, and the order change needs review. |
| G | Use the exact-generation hold read (`?generation=`). | Unchanged on its own. | RPC count is unchanged. The read returns one generation's hold, including a verified closed observation (`released` or `discarded`) for this worker (`api/internal/recovery/exact_observation.go:20`, `:57`). | Unchanged. | Gated by capability negotiation (`RecoveryHeldPublicationV2` / `RecoveryBindinglessClosedV1`, `api/internal/handler/recovery.go:211-231`) and, for closed observations, the `Uzi-Recovery-Observation` header that guards mixed-version rollouts. It is a different read contract from the ordinary listing, so it needs rollout review. Its closed observation is the input option E would need. |
| C | Bounded skip-ahead: a `hold_unavailable` visit does not consume a capped opportunity. Selection keeps scanning up to K extra entries per pass, and every visited entry rotates. | About `ceil((L+1)/(cap+K))` passes when the entries ahead have no hold. | Up to K extra selection-time journal reads and K extra hold RPCs per pass, on top of the unchanged hint reads and capture-path re-checks. The extra calls lengthen the pass, and the next pass is scheduled from its end. No new memory and no staleness, because every visit is fresh. | Unchanged: no cache, and membership is kept. | Changes the #3614 opportunity contract ("rotates raw contexts … before execution/hold checks"). It also raises the per-pass selection budget, so it falls within the issue's out-of-scope "timer/cap increases". It needs an explicit owner scope decision, not only a policy choice. |
| D | First-arrival reservation: one slot per pass for a context never visited since it entered the queue, analogous to #3614. | With A first arrivals ahead of it, a new arrival is selected within A+1 passes. That helps only if it is ready when selected: an arrival still executing or without an open hold spends its reservation and rotates behind the survivors, unless "visited" is defined as passing the checks, which then needs its own bound. | No added scheduling probes or selected opportunities per pass. The worst-case per-pass bound is unchanged, but moving a ready arrival earlier can move its capture and upload work (scenario 1's selection pass made 14 hold RPCs and 10 ownership calls, against 4 hold RPCs on a no-hold pass) into a previously cheap pass, lengthening that pass. Memory is one visited-key set, bounded by the queue. No staleness. | A context removed and re-added is a new arrival again (as #3614 already treats it), so discovery churn can earn repeat priority. | New fairness contract. With cap ≥ 3, ordinary survivors slow to `ceil(N/(cap-2))` when both reservations are active. Owner decision plus an ADR-1296 amendment. |
| H | Demote instead of keeping position: a context that just reported `hold_unavailable` rotates behind fresh arrivals, not behind its siblings. | New arrivals overtake no-hold contexts. | Memory is one flag per queued entry. No staleness. | Membership is kept, so a context whose hold appears later still progresses. | Fairness change. Under continuous arrivals, demoted contexts can starve, so it needs its own bound. Owner decision plus an ADR-1296 amendment. |
| E | Skip or back off a context in scheduling. Variant E1 caches the absence of a listed hold. Variant E2 (with G) caches only a positively observed closure. | Faster while the cache holds. | Memory is one entry per cached context. E1 needs a staleness bound and invalidation. E2 caches a terminal state, because closed holds never reopen (`agent/src/recovery.ts:4426`). | The tested E1 shape is unsafe: the tester's unbounded cross-pass absence-cache mutant hid a scenario 4 context whose hold appeared later. Bounded expiry or revisit variants were not assessed. E2 is not refuted by scenario 4, but it must still never hide a context that a later generation or a custody move makes eligible. | Needs a closed-hold cache decision. Closed custody alone grants no FINAL or cleanup authority (`api/internal/apitypes/recovery.go:327`). A closed context with pins left is #2823's pruning input, so E2 borders that boundary. Named as a dependency, not proposed. |

## 5. Recommendation

### No independent change

**[Inference]** No change is both independent and safe to ship without an
owner decision while still materially reducing first-reconstruction delay.
The delay is bounded by queue position, at about one pass per 4 retained
no-hold contexts ahead of the target. The existing ADR-1296 trade-off already
accepts it.

- Options B, F and G are cost refinements only. B and F save nothing, or one
  read per selection, in the measured shape. G changes a read contract. None
  of them shortens the wait, and none is warranted without production
  evidence that journal or RPC cost matters.
- Options C, D and H shorten the wait, but each changes the #3614
  fairness or opportunity contract. C also raises the per-pass selection
  budget, which the issue rules out of scope.
- Option E needs a closed-hold cache decision and touches #2823's boundary.
  The tested absence-cache variant (E1, an unbounded cross-pass cache) is unsafe; bounded expiry or revisit designs were not assessed.

The recommendation is to **keep the current policy (A)**.

### If the owner wants lower latency

If the owner decides that first-reconstruction latency matters, the
recommended candidate is **D, a first-arrival reservation**. It keeps:

- the existing per-pass cap and selection budget, so it adds no scheduling
  probes or selected opportunities per pass and does not touch the issue's
  out-of-scope "timer/cap increases". The worst-case work per pass is
  unchanged, but a particular pass can do more work and take longer when it
  now carries a ready arrival's capture and upload;
- fresh checks on every visit, with no cache and therefore no staleness;
- queue membership, so the behaviour proven for later-eligible and
  executing→inactive contexts is unchanged.

Its costs are fairness costs:

- Ordinary survivors slow from `ceil(N/(cap-1))` to `ceil(N/(cap-2))` passes
  while both reservations are in use. This needs cap ≥ 3.
- A context that discovery drops and re-adds earns the reservation again.
- Below cap 3, D must yield or be disabled. At cap 2, a #3614 entry and a
  first arrival would leave ordinary survivors no slot. At cap 1, D would
  take the only slot, which #3614 deliberately never does.
- What counts as "visited" must be defined. If selection counts, an arrival
  that is not yet ready gains nothing. If passing the checks counts, an
  arrival that never becomes ready needs a bound, like #3614's
  permanent-refusal case.

D needs an owner decision on the fairness contract and an ADR-1296
amendment.

C, with K = cap, would cut the wait to about `ceil((L+1)/(2 × cap))` passes
without a new fairness rule. But it adds up to K selection-time reads and
RPCs per pass on top of the hint reads and capture re-checks. It also raises
the per-pass selection budget, which the issue rules out of scope. It is
therefore only available with an explicit scope decision.

Either implementation must ship its own red-then-green regression. Scenario
1's pinned pass number is the natural regression seam.

### Limitations

- The counts come from a single fixture shape: one context per run, a fake Git producer (the capture and upload run against fixture seams, not a live worker), a fake
  API, and no settled records.
- Real journal-read, hold-RPC and capture latencies were not measured.
- Production queue sizes are unknown, and the #4570 incident does not supply
  them.
- The remaining reduction of retained no-hold contexts is #2823's carried
  #2414 pruning work, with its own fail-closed authority. Nothing here grants
  pruning authority.
