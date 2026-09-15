package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1392 M3 (api half): RESUME PLACEMENT and the SC2 custody-hold invariant, against a REAL
// Postgres. A FORGE-PARKED run (its gen-N hold released 'no_adopted_source' by the M1 park)
// promotes on schedule and is re-claimed — by its owner while the owner is live, or by a live
// sibling once the owner is stale-and-not-draining. EACH re-claim opens its OWN generation hold
// with the parked generation already released, so the owner's OPEN-hold count never grows across
// parks (SC2: parks do not accumulate holds toward the custodyHoldLimit admission ceiling).
//
// These reuse the M1 forge-park scaffolding (forgeParkService / fpSeedRunning / fpRun /
// fpHoldEvidence in forgepark_livedb_test.go, mhOpenHold in custody_multihold_livedb_test.go,
// seedWorker / interlockLiveDB in completion_interlock_livedb_test.go). The park is driven
// through the real Service.SetState (forge_unreachable cause); promotion through the UNCHANGED
// PromoteRecoveryWaitRuns; the re-claim through the real ClaimRun query with the SAME
// recovery-capable wiring Service.Claim uses (recovery_archive_v1 -> RecoveryCapable, the worker
// identity on the hold, the production custodyHoldLimit gate) so the claim opens a new-generation
// custody hold in the same statement.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// fpRecoveryClaimParams mirrors Service.Claim's ClaimRunParams for a RECOVERY-CAPABLE claim by
// workerID: it advertises recovery_archive_v1 (RecoveryCapable), records the worker identity on
// the hold, and passes the production custodyHoldLimit admission gate. That combination is what
// makes ClaimRun open exactly one NEW-generation custody hold in the same statement (the hold CTE
// fires only for a recovery-capable claim on a code-publishing kind), so a re-claim's hold
// accounting is exercised, not stubbed. heartbeatCutoff decides owner-vs-sibling placement: a
// run's prior owner holds the resume pin while its worker row is present AND its heartbeat is
// at/after the cutoff (or it is draining), and a sibling may claim only once the owner is
// heartbeat-stale (before the cutoff) AND not draining. AffinityCutoff is set well in the past of
// any freshly-promoted run so the affinity-ceiling rung never fires here — placement is decided
// purely by the worker_id match (owner) or the stale-heartbeat pin fall-open (sibling).
func (e interlockLiveDB) fpRecoveryClaimParams(workerID uuid.UUID, heartbeatCutoff time.Time) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		WorkerID:              pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:                e.userID,
		HeartbeatCutoff:       pgtype.Timestamptz{Time: heartbeatCutoff, Valid: true},
		AffinityCutoff:        pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		WorkerCaps:            []string{},
		CapabilityAware:       false,
		WorkerProtocolCaps:    []string{capability.RecoveryArchiveV1},
		CustodyHoldLimit:      custodyHoldLimit,
		RecoveryCapable:       true,
		WorkerIdentity:        "w-" + workerID.String()[:8],
	}
}

// fpOpenHoldCount is the run's OPEN (unresolved, state='open') custody-hold count — the SC2
// accounting under test. Scoped to state='open' so the released history rows the parks leave
// behind never inflate it.
func (e interlockLiveDB) fpOpenHoldCount(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, runID).Scan(&n); err != nil {
		t.Fatalf("count open holds: %v", err)
	}
	return n
}

// fpHoldCountByState counts the run's custody holds in a given lifecycle state — 'open' (the live
// accounting) vs 'released' (the history rows the parks accumulate).
func (e interlockLiveDB) fpHoldCountByState(t *testing.T, runID uuid.UUID, state string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND state = $2`, runID, state).Scan(&n); err != nil {
		t.Fatalf("count holds in state %q: %v", state, err)
	}
	return n
}

// fpPromoteDue forces recovery_retry_not_before into the past and runs the promotion pass,
// returning true iff the run was promoted (recovery_wait -> queued). PromoteRecoveryWaitRuns is
// UNCHANGED by #1392: it flips a due recovery_wait run to queued and keeps worker_id for affinity.
func (e interlockLiveDB) fpPromoteDue(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	e.exec(t, `UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1`, runID)
	promoted, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now()))
	if err != nil {
		t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
	}
	for _, p := range promoted {
		if p.ID == runID {
			return true
		}
	}
	return false
}

// fpRunWorkerID reads the run's current worker_id (the affinity/placement column).
func (e interlockLiveDB) fpRunWorkerID(t *testing.T, runID uuid.UUID) pgtype.UUID {
	t.Helper()
	var wid pgtype.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT worker_id FROM runs WHERE id = $1`, runID).Scan(&wid); err != nil {
		t.Fatalf("read worker_id: %v", err)
	}
	return wid
}

// fpOpenHold reads the run's single OPEN custody hold's generation + live/original worker (used
// to prove the re-claim opened the NEXT generation held by the expected worker). Fails if the
// run does not have exactly one open hold.
func (e interlockLiveDB) fpOpenHold(t *testing.T, runID uuid.UUID) (generation int64, liveWorker, originalWorker pgtype.UUID) {
	t.Helper()
	if n := e.fpOpenHoldCount(t, runID); n != 1 {
		t.Fatalf("fpOpenHold: run has %d open holds, want exactly 1", n)
	}
	if err := e.pool.QueryRow(e.ctx,
		`SELECT generation, live_worker_id, original_worker_id FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, runID).
		Scan(&generation, &liveWorker, &originalWorker); err != nil {
		t.Fatalf("read open hold: %v", err)
	}
	return generation, liveWorker, originalWorker
}

// TestForgeParkOwnerWarmResumePlacementLiveDB (SC4/SC2): a forge-parked run promotes on schedule
// and, with its owner still LIVE, is re-claimed BY THE OWNER (placement: owner preferred while
// live). The re-claim opens a NEW generation (N+1) hold; the parked gen-N hold is already
// released 'no_adopted_source'; and the owner's OPEN-hold count for the run is EXACTLY 1 (the new
// gen-N+1 hold), never 2 — the released gen-N hold is history, not an open hold.
func TestForgeParkOwnerWarmResumePlacementLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	owner := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, owner, 1)
	genHold := mhOpenHold(t, e, runID, 1, owner) // the gen-1 hold the park releases

	// Forge-park at generation 1: releases the gen-1 hold with no_adopted_source and parks.
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: owner}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)}); err != nil || !applied {
		t.Fatalf("forge park: applied=%v err=%v", applied, err)
	}
	if state, ev := e.fpHoldEvidence(t, genHold); state != "released" || ev == nil || *ev != "no_adopted_source" {
		t.Fatalf("parked gen-1 hold state=%q evidence=%v, want released/no_adopted_source", state, ev)
	}
	if n := e.fpOpenHoldCount(t, runID); n != 0 {
		t.Fatalf("open-hold count after the park = %d, want 0 (the park released the generation's only hold)", n)
	}

	// Promote on schedule: recovery_wait -> queued, worker_id still the owner (affinity kept).
	if !e.fpPromoteDue(t, runID) {
		t.Fatal("PromoteRecoveryWaitRuns did not return the run; a due forge-parked run must promote to queued")
	}
	if status, _, _, _, _ := e.fpRun(t, runID); status != "queued" {
		t.Fatalf("status after promote = %q, want queued", status)
	}
	if wid := e.fpRunWorkerID(t, runID); !wid.Valid || uuid.UUID(wid.Bytes) != owner {
		t.Fatalf("worker_id after promote = %v, want the owner %v (affinity preserved)", wid, owner)
	}

	// Owner is LIVE (fresh heartbeat), so the resume pin prefers it: the owner re-claims its own
	// promoted run.
	e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, owner)
	claimed, err := e.q.ClaimRun(e.ctx, e.fpRecoveryClaimParams(owner, time.Now().Add(-45*time.Second)))
	if err != nil {
		t.Fatalf("owner ClaimRun on the promoted run: %v", err)
	}
	if claimed.ID != runID {
		t.Fatalf("owner claimed %v, want the promoted run %v", claimed.ID, runID)
	}
	if !claimed.WorkerID.Valid || uuid.UUID(claimed.WorkerID.Bytes) != owner {
		t.Fatalf("re-claimed run worker_id = %v, want the owner %v (placement: owner preferred while live)", claimed.WorkerID, owner)
	}
	if claimed.ClaimGeneration != 2 {
		t.Fatalf("claim_generation after re-claim = %d, want 2 (a new generation N+1)", claimed.ClaimGeneration)
	}

	// The parked gen-1 hold is STILL released; the re-claim opened exactly ONE open hold
	// (generation 2, held live by the owner), so the owner's open-hold count is 1, not 2.
	if state, _ := e.fpHoldEvidence(t, genHold); state != "released" {
		t.Fatalf("parked gen-1 hold state = %q after the re-claim, want still released", state)
	}
	if n := e.fpOpenHoldCount(t, runID); n != 1 {
		t.Fatalf("owner OPEN-hold count for the run = %d, want exactly 1 (the new gen-2 hold, NOT the parked gen-1 too)", n)
	}
	openGen, liveWorker, _ := e.fpOpenHold(t, runID)
	if openGen != 2 {
		t.Fatalf("open hold generation = %d, want 2 (the re-claim opened the next generation)", openGen)
	}
	if !liveWorker.Valid || uuid.UUID(liveWorker.Bytes) != owner {
		t.Fatalf("open hold live_worker_id = %v, want the owner %v", liveWorker, owner)
	}
	// Total holds = the released gen-1 history row + the one open gen-2 hold.
	if total := e.fpHoldCountByState(t, runID, "open") + e.fpHoldCountByState(t, runID, "released"); total != 2 {
		t.Fatalf("total holds = %d, want 2 (1 released history + 1 open)", total)
	}
}

// TestForgeParkSiblingResumePlacementLiveDB (SC4/SC2): after a forge-park + promote, when the
// OWNER is unavailable (heartbeat stale past the cutoff AND not draining) the resume pin falls
// open and a LIVE SIBLING claims the promoted run. The sibling opens its OWN generation hold; the
// parked generation stays released; and the SIBLING — not the owner — is the live claimant.
func TestForgeParkSiblingResumePlacementLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	owner := e.seedWorker(t, nil)
	sibling := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, owner, 1)
	genHold := mhOpenHold(t, e, runID, 1, owner)

	// Forge-park + promote, exactly as the owner case.
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: owner}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)}); err != nil || !applied {
		t.Fatalf("forge park: applied=%v err=%v", applied, err)
	}
	if !e.fpPromoteDue(t, runID) {
		t.Fatal("promote did not return the run")
	}

	// Make the OWNER ineligible for the resume pin: heartbeat stale (well past the 45s cutoff)
	// and NOT draining. The ClaimRun pin's NOT EXISTS then becomes true (no live/draining owner
	// row), so a live sibling may claim the promoted run. This is the ADR-628 D3a fall-open case.
	e.exec(t, `UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes', draining_since = NULL WHERE id = $1`, owner)

	claimed, err := e.q.ClaimRun(e.ctx, e.fpRecoveryClaimParams(sibling, time.Now().Add(-45*time.Second)))
	if err != nil {
		t.Fatalf("sibling ClaimRun on the promoted run (owner stale): %v", err)
	}
	if claimed.ID != runID {
		t.Fatalf("sibling claimed %v, want the promoted run %v", claimed.ID, runID)
	}
	// The SIBLING — not the owner — is the live claimant.
	if !claimed.WorkerID.Valid || uuid.UUID(claimed.WorkerID.Bytes) != sibling {
		t.Fatalf("re-claimed run worker_id = %v, want the SIBLING %v (owner stale-and-not-draining -> sibling claims)", claimed.WorkerID, sibling)
	}
	if uuid.UUID(claimed.WorkerID.Bytes) == owner {
		t.Fatal("the stale owner re-claimed its own run; a stale-and-not-draining owner must yield the pin to a live sibling")
	}
	if claimed.ClaimGeneration != 2 {
		t.Fatalf("claim_generation after the sibling re-claim = %d, want 2", claimed.ClaimGeneration)
	}

	// The parked gen stays released; the sibling opened its OWN gen-2 hold, held live by IT.
	if state, _ := e.fpHoldEvidence(t, genHold); state != "released" {
		t.Fatalf("parked gen-1 hold state = %q after the sibling re-claim, want still released", state)
	}
	openGen, liveWorker, originalWorker := e.fpOpenHold(t, runID)
	if openGen != 2 {
		t.Fatalf("open hold generation = %d, want 2", openGen)
	}
	if !liveWorker.Valid || uuid.UUID(liveWorker.Bytes) != sibling || uuid.UUID(originalWorker.Bytes) != sibling {
		t.Fatalf("open hold live/original worker = %v/%v, want the SIBLING %v (the sibling opened its own generation hold)", liveWorker, originalWorker, sibling)
	}
}

// TestForgeParkOpenHoldCountStableAcrossParksLiveDB is the SC2 invariant: across REPEATED
// forge-park -> promote -> owner re-claim cycles, the owner's OPEN-hold count for the run holds at
// exactly 1 — it never climbs toward the custodyHoldLimit admission ceiling, because each park
// releases the current generation's hold BEFORE the next claim opens the next one. The released
// holds accumulate as state='released' history rows (one per park); the open holds do not.
func TestForgeParkOpenHoldCountStableAcrossParksLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	// Unlimited forge cap: the invariant under test is custody-hold accounting, not the forge
	// park cap, so no cycle must cap-fail.
	svc := e.forgeParkService(t, 0)

	owner := e.seedWorker(t, nil)
	e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, owner) // live: the owner keeps re-claiming
	runID := e.fpSeedRunning(t, owner, 1)
	mhOpenHold(t, e, runID, 1, owner) // the gen-1 hold the first park releases

	const cycles = 3
	for cycle := 1; cycle <= cycles; cycle++ {
		gen := int64(cycle) // the current claim generation entering this cycle (1, 2, 3)

		// Park at the current generation: releases that generation's single open hold.
		if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: owner}, runID,
			StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(gen)}); err != nil || !applied {
			t.Fatalf("cycle %d forge park (gen %d): applied=%v err=%v", cycle, gen, applied, err)
		}
		// Right after the park the run has ZERO open holds — the release returned the count to 0
		// before any re-claim, which is exactly why parks cannot accumulate open holds.
		if n := e.fpOpenHoldCount(t, runID); n != 0 {
			t.Fatalf("cycle %d: open-hold count right after the park = %d, want 0 (the park released the generation's hold)", cycle, n)
		}
		if _, _, fpc, _, _ := e.fpRun(t, runID); fpc != int32(cycle) {
			t.Fatalf("cycle %d: forge_park_count = %d, want %d (one increment per park)", cycle, fpc, cycle)
		}

		// Promote and re-claim on the same live owner: opens the next generation's hold.
		if !e.fpPromoteDue(t, runID) {
			t.Fatalf("cycle %d: promote did not return the run", cycle)
		}
		claimed, err := e.q.ClaimRun(e.ctx, e.fpRecoveryClaimParams(owner, time.Now().Add(-45*time.Second)))
		if err != nil {
			t.Fatalf("cycle %d: owner ClaimRun: %v", cycle, err)
		}
		if claimed.ID != runID || !claimed.WorkerID.Valid || uuid.UUID(claimed.WorkerID.Bytes) != owner {
			t.Fatalf("cycle %d: re-claim placed on run=%v worker=%v, want the owner %v on run %v", cycle, claimed.ID, claimed.WorkerID, owner, runID)
		}
		if claimed.ClaimGeneration != gen+1 {
			t.Fatalf("cycle %d: claim_generation = %d, want %d", cycle, claimed.ClaimGeneration, gen+1)
		}

		// THE SC2 INVARIANT: after the re-claim the owner holds EXACTLY ONE open hold for the run,
		// no matter how many times it has parked.
		if n := e.fpOpenHoldCount(t, runID); n != 1 {
			t.Fatalf("cycle %d: owner OPEN-hold count = %d, want exactly 1 (parks must not accumulate open holds)", cycle, n)
		}
		// The consequence the PRD names: the open count never climbs toward the custodyHoldLimit
		// admission ceiling, so a run that parks repeatedly is never blocked from re-claiming.
		if n := e.fpOpenHoldCount(t, runID); n >= custodyHoldLimit {
			t.Fatalf("cycle %d: open-hold count %d reached the custodyHoldLimit %d — parks are leaking holds", cycle, n, custodyHoldLimit)
		}
		// The re-claim's open hold is the NEXT generation.
		if openGen, _, _ := e.fpOpenHold(t, runID); openGen != gen+1 {
			t.Fatalf("cycle %d: open hold generation = %d, want %d", cycle, openGen, gen+1)
		}
		// The released holds accumulate as history: one per park so far.
		if got := e.fpHoldCountByState(t, runID, "released"); got != cycle {
			t.Fatalf("cycle %d: released-hold rows = %d, want %d (each park leaves a released history row)", cycle, got, cycle)
		}

		// Return to 'running' so the next cycle's park (status='running' guard) applies. In
		// production the worker reports running via SetState after picking the claim up; a raw
		// UPDATE is enough to satisfy the guard here.
		e.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, runID)
	}

	// Final: three parks left three released history rows and exactly one open hold — the open
	// count held at 1 across every cycle, well under the custodyHoldLimit (8).
	if got := e.fpHoldCountByState(t, runID, "released"); got != cycles {
		t.Fatalf("final released-hold rows = %d, want %d", got, cycles)
	}
	if got := e.fpOpenHoldCount(t, runID); got != 1 {
		t.Fatalf("final open-hold count = %d, want 1", got)
	}
}
