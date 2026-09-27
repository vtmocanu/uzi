# PRD #1810: Keep a failed run's checkpoint ref while its recovery is open

**Issue**: #1810
**Status**: Draft
**Priority**: High
**Created**: 2026-09-27

Split out of PRD #1809 (worker disk safety), which covers why the run failed. This PRD covers why its work was nearly lost.

## Problem

An issue or self-improve run publishes checkpoints to origin at `refs/uzi-checkpoints/<branch>` through the api push broker (`api/internal/pushbroker/pushbroker.go`). On every terminal transition the api deletes that ref, best-effort (`deleteCheckpointBestEffort`, PRD #1030 M4), from three call sites: `api/internal/workersvc/service.go` ~4080, `api/internal/workersvc/submit.go` ~425 and ~630. The reason is sound for a completed run: a stale ref would later block a new run on the same branch with a `not_descendant` skip.

For `failed` and `cancelled` it removes the wrong thing. A few lines above the `service.go` call, the same code deliberately keeps custody holds open for failed and cancelled runs ("those retain custody for capture or explicit discard"). So uzi keeps custody of the run's work while deleting the one copy of it that lives off the worker. The helper's comment names the worker's `refs/uzi-runner/*` as the primary recovery path, but that path is gone whenever the worker is ephemeral, its volume is lost, or it is unreachable.

Observed on 2026-09-27 on the issue #1798 run:

- 11:06 UTC: `checkpoint published to origin (time-based)`.
- 11:18 UTC: `park checkpoint published to origin`.
- 13:02 UTC: the run failed (a full data volume, PRD #1809).
- Afterwards `git ls-remote origin 'refs/uzi-checkpoints/*'` listed no ref for that branch while other issues' refs remained.
- `uzi run recovery` showed two `source_only` holds with no captures.
- The committed work survived only in the worker's working clone.

This is distinct from #1790, where a park never obtains an off-worker copy. Here the copy existed and was deleted afterwards.

## Goal

The last published checkpoint of a failed or cancelled run stays reachable on origin until its recovery is settled, without ever blocking a new run on the same branch and without a forced ref update.

## Decision log

**D1. Retention follows custody.** The api does not delete a run's checkpoint ref while any custody hold for that run is open. For a completed run with no open hold, today's delete-on-completion stays. A completed generation can coexist with an older open hold of the same run; the ref is then retained while that older hold is open, or its tip is preserved under the run's recovery ref (D2) before the branch ref is freed. A failed or cancelled run with **no** hold (a worker without the recovery capability does not create one) keeps today's delete; the PRD states this rather than widening retention to runs uzi cannot settle.

**D2. Supersession moves the old tip to a run-specific recovery ref, never deletes it.** When a new run on the same branch needs the checkpoint slot (trigger: its first checkpoint publish is refused `not_descendant` because a retained ref from a different run sits there), the api:

1. **persists first**: records on the old run (or its hold) the intent `superseding`, the recovery ref name `refs/uzi-recovery/<old-run-id>` and the expected tip, in one transaction, before touching the forge;
2. creates that recovery ref on origin pointing at the tip, compare-and-swap with old = zero, pack-less (the object is already on the remote). An existing recovery ref **at the expected tip** counts as success (a retry after a crash); one at a **different** tip stops supersession and leaves the branch ref alone;
3. deletes `refs/uzi-checkpoints/<branch>` compare-and-swap on that tip (the existing `DeleteOptions.ExpectedOldTip` path). A ref already absent, or advanced by another run, counts as done for this step;
4. marks the record `superseded` and lets the new run's publish proceed.

Every step is idempotent and CAS; nothing is forced, preserving the push broker's "NEVER FORCED" rule (`pushbroker.go` ~11). Because the record is written before the first forge write, a crash at any point leaves the tip discoverable: the sweeper reconciles a `superseding` record by re-running steps 2-4. Supersession and settlement (D3) serialise on the old run's row lock, so a hold released mid-supersession deletes whichever ref the record names once the record is final. The push broker gains a pack-less ref-create primitive for step 1. The namespace is outside `refs/uzi-checkpoints/` on purpose: every worker bare mirrors `+refs/uzi-checkpoints/*` (`agent/src/git.ts` ~4104) and new runs seed from `refs/uzi-checkpoints/<branch>` (~1178), so a recovery ref there would leak into unrelated runs. It is also outside `refs/heads`.

**D3. The recovery ref is recorded and settled like the hold it protects.** The api records the recovery ref (name and tip) on the run or hold, so `uzi run recovery`, the web recovery view and the live-settle ancestry proof (`ReleasePredecessorCustodyHoldByLiveAncestry`, `api/internal/apitypes/recovery.go` ~222) read it instead of the branch ref once superseded. The ref is deleted (CAS on its recorded tip) after the run's last hold is released or discarded. Every release and discard writer triggers that check after its transaction commits: `ReleaseCustodyHold`, `ReleaseCustodyHoldExact`, `ReleasePredecessorCustodyHoldByAncestry`, `ReleasePredecessorCustodyHoldByLiveAncestry`, `DiscardCustodyHoldForOwner` (`api/internal/store/queries/recovery.sql`). A failed forge delete is retried by a reconciliation pass (the sweeper) rather than dropped, so refs do not accumulate.

## Milestones

| Phase | Milestone | Depends on | Main files |
|---|---|---|---|
| 1 | M1 retention on terminal transitions | none | `api/internal/workersvc/service.go`, `submit.go` |
| 1 | M2 push-broker ref-create primitive | none | `api/internal/pushbroker/` |
| 2 | M3 supersession and recovery ref record | M1, M2 | `api/internal/workersvc/`, migration, `api/internal/store/queries/recovery.sql`, `api/internal/apitypes/recovery.go` |
| 2 | M4 settlement delete and reconciliation | M1, M3 | recovery release/discard callers, sweeper |
| 3 | M5 surfaces, docs, ADR | M3, M4 | `api/cmd/uzi/`, `web/src/`, `docs/`, `specs/human.md`, new ADR |

Every milestone passes `task gate:api` (and `task gate:web` when M5 touches the web) and `task check-docs:web` when it touches docs or PRDs. A migration is numbered at merge time (`task migration:renumber`).

**Constraints for the uzi run:**

- Neither implementation nor validation creates or modifies a file under `.github/workflows/`. Before finalize, `git diff --name-only <base>..HEAD` shows no entry there.
- Forge interactions in tests use the existing fake-forge / in-memory git remote seams; no test pushes to a real forge.

- [ ] **M1: Retention on terminal transitions (D1).** All three deletion call sites skip the delete while the run has an open custody hold; completed runs without an open hold are unchanged; a failed or cancelled run without a hold keeps today's delete. Regression tests (live DB, fake forge): a run that published a checkpoint and then fails with an open hold keeps its ref (fails on current main); the same for cancelled; each of the three call sites is covered; a completed run's ref is still deleted.
- [ ] **M2: Push-broker ref-create primitive (D2).** A pack-less, CAS (old = zero) ref create for an object already on the remote, through the same endpoint, auth, SSRF gate and redaction as `Publish`/`Delete`. Tests: creates when absent; refuses when the ref exists; refuses a missing object; never sends a force flag.
- [ ] **M3: Supersession and the recovery ref record (D2, D3).** The `not_descendant` trigger, the two CAS steps, the new run's publish proceeding, the recorded ref name and tip, and the recovery and live-settle readers using it. Tests: a new run on the branch is not blocked and the old tip survives under `refs/uzi-recovery/<run-id>`; a ref advanced by another run is left alone; `uzi run recovery --json` reports the recovery ref; the live-settle ancestry proof accepts the recovery ref. Interruption regressions: a crash after the record but before the ref create, and after the ref create but before the branch delete, each reconcile to `superseded` with the tip reachable; an existing recovery ref at a different tip stops supersession; concurrent supersession and hold settlement end with exactly one ref deleted and none orphaned.
- [ ] **M4: Settlement delete and reconciliation (D3).** Each release and discard writer triggers the delete check after commit; the branch ref or recovery ref is deleted with CAS once the run's last hold is settled; a failed delete is retried by the sweeper. Tests (live DB): each of the five writers leads to the delete; a hold still open blocks it; a failed forge delete is retried and then succeeds; a completed generation with an older open hold keeps (or preserves) its tip.
- [ ] **M5: Surfaces, docs, ADR.** `uzi run recovery` and the web recovery view show where the work is (branch checkpoint or recovery ref). Update the recovery docs page, then `task docs:sync`; add a terse `specs/human.md` requirement with its `(AI-synced YYYY-MM-DD)` tag; write an ADR for the invariant "a published checkpoint outlives its custody hold, and supersession moves it, never deletes it" (write its path un-backticked in PRDs until it exists). Maintainer-owned live acceptance on hosted k8s: a run that fails after publishing keeps its ref until its hold is discarded; a new run on the same branch then moves it to the recovery ref.

## Success criteria

- A failed or cancelled run's published checkpoint stays on origin while its custody is open.
- A new run on the same branch is never blocked by a retained ref.
- No checkpoint or recovery ref is ever force-updated.
- No retained or recovery ref outlives its settled hold.

## Risks

- **Refs accumulating on origin.** Mitigation: deletion is tied to hold settlement across all five writers, with sweeper reconciliation.
- **A retained ref confusing a new run's seed.** Mitigation: supersession moves it out of `refs/uzi-checkpoints/` before the new run publishes, and the recovery namespace is never mirrored into worker bares.
- **Partial or interrupted supersession.** Mitigation: the intent is persisted before any forge write, every step is idempotent CAS, and the sweeper reconciles a `superseding` record; the tip exists in at least one recorded ref at every point.

## Out of scope

- A park that never obtains an off-worker copy (#1790).
- The disk-full failure itself (PRD #1809).
