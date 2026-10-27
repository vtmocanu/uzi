# Phase-52 recovery reconstruction investigation (#3544)

Baseline: cddacaeec01233d0a511edc6738ae4519bdd8867, 2026-10-27.
This change adds diagnostics and characterization only. It does not repair
reconstruction, change retry timing, or relax custody and source proofs.

## Observed CI failure

Nightlies 39909250399, 40068252579, 40168226469 and 40279094699, plus
[dispatch 40285302749](https://github.com/vtmocanu/uzi/actions/runs/40285302749),
time out on case 8's cancelled generation 2. The hold has an available,
manifest-bound archive, but no custody FINAL. The old uploaded worker record
is marked `inventory_snapshot_changed` without a saved FINAL.

In the dispatch, the old capture was deferred for `execution_tail_present`
at 07:56:59, excluded as `not_live_candidate` at 07:57:57, and a fresh capture
uploaded during cleanup at 07:59:45. Its later projection shows a minted
FINAL request; an acknowledged settlement was not observed. The approximately
108-second interval does not measure reconstruction service time or prove
starvation. Passing dispatch 40098291884 records case 8's archive receipt.
Different commits prevent a same-commit flake claim. Phase 52 is GitLab-only,
so passing other lanes are not controls for a forge-specific defect.

## Real-source characterization

The `#3544 real GitCache heartbeat reconstruction` tests in
`agent/test/recovery-inventory.test.ts` use real bare repositories, real owed
metadata, a real coverage builder and bundle producer, authenticated on-disk
recovery journals, and the Runner's heartbeat recovery entry point. The API
is an in-process fixture, not a live server or database.

1. Independently publish H and positively confirm it. Freeze/upload an inventory
   while a running claim has no owed candidates; source exclusion defers FINAL.
2. Rewrite the same owned branch to H2 and back to H. GitCache retains H2 as
   owed source. Once the run is cancelled, that uncovered source invalidates
   the old archive.
3. Control: leave H2 owed. After restart, heartbeat discovery creates a fresh
   archive and accepts its exact FINAL ACK through the fixture API.
4. Same-fingerprint case: publish H2 to the separate bare repository, observe
   its exact remote tip, and use the existing positive-confirmation reconciler
   to retire its owed pin. No raw source deletion or fabricated enumeration is
   used. H2 remains independently readable from the publication repository.
5. Restart GitCache and the coordinator. Heartbeat reconstruction recomputes
   the original fingerprint and returns the existing invalidated record. It
   stays `inventory_snapshot_changed`; no fresh capture or FINAL is produced.

Both variants include an earlier retained context and a one-context pass cap.
The earlier context is visited on pass 0 and the target on pass 1. This
control demonstrates queue rotation, not a latency bound for arbitrary CI
backlogs. Virtual clock advances avoid real-time receipt-window sleeps.

This confirms a legitimate source transition exposing the matching-record
liveness gap. It does not establish that this transition caused the historical
CI failures: their artifacts omit the necessary frozen-root/context details.
The characterization intentionally preserves the current retained outcome;
a settlement regression belongs with a separately approved repair.

## Diagnostic interpretation

`recovery: inventory reconstruction` reuses the existing bounded transition
logger with a separate bounded cache, independently of the existing `recovery: re-drive exit` channel.
It carries only path (`boot` or `live`), a fixed reason, run/generation and a
capture id when known. Source paths, heads, context payloads and error text
are excluded. Logger failures cannot affect decisions. A context with no
capture stays deduplicated across live passes; each channel keeps at most 256
entries, without changing the exit channel's pruning or progress clearing.

- `source_ancestry_unproven` and `root_context_uncovered` distinguish the two
  frozen-coverage checks that persist the existing snapshot-change refusal.
- `snapshot_created`, `matching_snapshot_reused`, and
  `invalidated_snapshot_reused` distinguish reconstruction results.
- Eligibility, provenance, source-proof and preferred-context reasons identify
  earlier reconstruction exits. Pending archive/held-publication FINAL
  identities remain protected and are not replaced.

These are observations, not new journal reasons or permission to release or
delete anything. The archive proof, immutable FINAL identity, producer
admission, source boundary, retention and cleanup rules are unchanged.

## Disposition and follow-up

Under #2494, propose a separately reviewed repair for reusing an invalidated
same-fingerprint snapshot after a fresh complete proof. Preserve the old
archive/refusal and every source; pending immutable FINALs must keep their
existing exact replay authority. This diagnostics PR authorizes no repair.

#3152 retains broader diagnostics coverage; reuse its phase-52 log/record
artifacts to attribute the next occurrence. #3213 tracks execution outcomes.
#3031 retains terminal-producer admission and durable-budget policy; this
investigation creates no producer, ledger or renewed allowance.

Phase 52 is unrun locally. Its 120-second archive-receipt predicate, stub
retry pacing, and workflow remain unchanged. The full agent suite is left
to exact-head PR CI; local verification uses the touched recovery test file
and `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`.
