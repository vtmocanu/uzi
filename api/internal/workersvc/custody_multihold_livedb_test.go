package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1296 M4 (D3) MULTI-HOLD custody safety, against a REAL Postgres. A run can carry MORE
// THAN ONE open custody hold: a cross-worker re-claim after a transient worker loss opens a
// generation-2 hold (a different live_worker_id) while the crashed worker's generation-1 hold
// stays open and is never released. Generation 2 typically reseeds from the default branch, so
// generation 1's committed work is NOT in generation 2's tree — the generation-1 hold protects
// the ONLY copy of that work. The three release paths (reconciler, ListReleasableCustodyHolds
// completed-generation guard, and SetState terminal completion) must therefore release ONLY the
// specific/current-generation hold and NEVER strand a sibling older-generation orphan.
//
// These are the HIGH-finding regression tests. They are LOAD-BEARING: the pre-fix code released
// per-run (ReleaseCustodyForRun nulled every open hold on the run) and selected the completed
// path non-generation-aware, so each assertion that generation-1 STAYS OPEN would FAIL on the
// pre-fix code (per-run release / non-generation-aware select would drop generation-1).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// mhSeedEphemeralWorker inserts an ephemeral hosted worker. ephemeralRunID nil ⇒ NULL
// (an unbound/crashed worker, e.g. after its original run was reassigned via FK SET NULL).
func mhSeedEphemeralWorker(t *testing.T, e interlockLiveDB, ephemeralRunID *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var runArg any
	if ephemeralRunID != nil {
		runArg = *ephemeralRunID
	}
	e.exec(t, `INSERT INTO workers
	             (id, user_id, name, token_hash, template_declared, kind, hosted_size,
	              docker_enabled, ephemeral, ephemeral_run_id, status)
	           VALUES ($1, $2, $3, $4, 'base', 'hosted', 'm', false, true, $5, 'online')`,
		id, e.userID, "eph-"+id.String()[:8], id[:], runArg)
	return id
}

// mhOpenHold opens an OPEN custody hold at the given generation naming liveWorker/runID as its
// live holder. Returns the hold id.
func mhOpenHold(t *testing.T, e interlockLiveDB, runID uuid.UUID, generation int64, liveWorker uuid.UUID) uuid.UUID {
	t.Helper()
	holdID := uuid.New()
	origWorker := uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds
	             (id, user_id, repo_id, run_id, generation, state,
	              original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	           VALUES ($1, $2, $3, $4, $5, 'open', $6, 'ident', $7, $4)`,
		holdID, e.userID, e.repoID, runID, generation, origWorker, liveWorker)
	return holdID
}

// mhHoldState reads a hold's lifecycle state.
func mhHoldState(t *testing.T, e interlockLiveDB, holdID uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&s); err != nil {
		t.Fatalf("read hold state: %v", err)
	}
	return s
}

// mhWorkerExists reports whether the worker row is still present.
func mhWorkerExists(t *testing.T, e interlockLiveDB, workerID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM workers WHERE id = $1`, workerID).Scan(&n); err != nil {
		t.Fatalf("count workers: %v", err)
	}
	return n > 0
}

// TestReconcileMultiHoldReadyCaptureReleasesOnlyCurrentLiveDB is the exact HIGH scenario: a run
// with TWO open holds (generation-1 held by W1, generation-2 held by W2, runs.claim_generation
// = 2), a READY capture bound to generation-2's hold. The reconciler must release ONLY
// generation-2's hold; generation-1's hold stays OPEN and W1 is NOT reapable (its teardown
// DELETE still skips it on the open-hold predicate), preserving the only copy of generation-1's
// committed work. On the pre-fix per-run release this test FAILS (generation-1 would be released
// and W1 reaped).
func TestReconcileMultiHoldReadyCaptureReleasesOnlyCurrentLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	// R is finalization-blocked (failed) at generation 2, owned by W2. W1 is the crashed
	// original claimant, now an unbound ephemeral worker.
	runID := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	           VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`,
		runID, e.userID, e.repoID, iid)
	w1 := mhSeedEphemeralWorker(t, e, nil)    // crashed original: unbound ephemeral
	w2 := mhSeedEphemeralWorker(t, e, &runID) // current claimant: bound to R
	e.exec(t, `UPDATE runs SET status = 'failed', worker_id = $2, claim_generation = 2 WHERE id = $1`, runID, w2)

	gen1 := mhOpenHold(t, e, runID, 1, w1) // orphan: protects the ONLY copy of gen-1's work
	gen2 := mhOpenHold(t, e, runID, 2, w2)
	// A READY capture bound to generation-2's hold — the durable disposition for gen-2 only.
	e.exec(t, `INSERT INTO recovery_captures
	             (hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	           VALUES ($1, $2, $3, 'ident', 'H2', 'k', 'available')`,
		gen2, runID, e.userID)

	if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
		t.Fatalf("ReconcileCustodyReleases: %v", err)
	}
	if s := mhHoldState(t, e, gen2); s != "released" {
		t.Fatalf("generation-2 hold state = %q after reconcile, want released (its capture is ready)", s)
	}
	if s := mhHoldState(t, e, gen1); s != "open" {
		t.Fatalf("generation-1 orphan hold state = %q after reconcile, want OPEN — releasing gen-2's ready capture must NOT release the sibling gen-1 hold (the HIGH hazard)", s)
	}

	// W1 is NOT reapable while its generation-1 hold is open; W2 (gen-2 released) is reaped.
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := store.ReapEphemeralWorkers(e.ctx, e.pool, cutoff, pgtype.Interval{}); err != nil {
		t.Fatalf("ReapEphemeralWorkers: %v", err)
	}
	if !mhWorkerExists(t, e, w1) {
		t.Fatalf("W1 (generation-1 holder) reaped despite its open orphan hold — its last committed copy would be lost")
	}
	if mhWorkerExists(t, e, w2) {
		t.Fatalf("W2 (generation-2, released) still present — a released hold must let its worker be reaped")
	}
}

// TestReconcileMultiHoldCompletedGenerationGuardLiveDB proves the generation-aware completed
// path through the reconciler: a COMPLETED run at claim_generation = 2 carries an orphaned
// generation-1 hold (W1) plus the current generation-2 hold (W2). Only the generation that
// actually completed/published (generation 2 = claim_generation) is releasable; the reconciler
// releases ONLY generation-2's hold and the generation-1 orphan stays open. On the pre-fix
// non-generation-aware select BOTH holds would be listed and per-run release would drop
// generation-1 — so the gen-1-stays-open assertion FAILS on pre-fix code.
func TestReconcileMultiHoldCompletedGenerationGuardLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	runID := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	           VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`,
		runID, e.userID, e.repoID, iid)
	w1 := mhSeedEphemeralWorker(t, e, nil)
	w2 := mhSeedEphemeralWorker(t, e, &runID)
	e.exec(t, `UPDATE runs SET status = 'completed', worker_id = $2, claim_generation = 2 WHERE id = $1`, runID, w2)

	gen1 := mhOpenHold(t, e, runID, 1, w1)
	gen2 := mhOpenHold(t, e, runID, 2, w2)

	if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
		t.Fatalf("ReconcileCustodyReleases: %v", err)
	}
	if s := mhHoldState(t, e, gen2); s != "released" {
		t.Fatalf("current-generation (2) hold state = %q after reconcile, want released (it is the completed generation)", s)
	}
	if s := mhHoldState(t, e, gen1); s != "open" {
		t.Fatalf("orphaned generation-1 hold state = %q after reconcile, want OPEN — a completed run releases ONLY its completed generation (generation guard)", s)
	}
}

// TestSetStateCompletedMultiHoldReleasesOnlyReporterLiveDB proves the terminal-completion path:
// a legacy run owned by W2, carrying an orphaned generation-1 hold (W1) alongside W2's
// generation-2 hold, completed via SetState reported by W2, releases ONLY W2's hold. The gen-1
// orphan stays open, retained until capture/discard. On the pre-fix per-run release (SetState
// called ReleaseCustodyForRun(runID)) BOTH holds would be released — so the gen-1-stays-open
// assertion FAILS on pre-fix code.
func TestSetStateCompletedMultiHoldReleasesOnlyReporterLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	w1 := e.seedWorker(t, nil) // the crashed original worker, still holds gen-1
	w2 := e.seedWorker(t, nil) // owns the run; reports completion
	runID := e.seedLegacyRunningRun(t, w2)
	e.exec(t, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, runID)

	gen1 := mhOpenHold(t, e, runID, 1, w1)
	gen2 := mhOpenHold(t, e, runID, 2, w2)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w2}, runID,
		StateRequest{State: "completed", Branch: strPtr("agent/issue-mh"), Head: strPtr("ignored")})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("completion must apply; applied=%v status=%q", applied, run.Status)
	}
	if s := mhHoldState(t, e, gen2); s != "released" {
		t.Fatalf("reporting worker's (generation-2) hold state = %q after completion, want released", s)
	}
	if s := mhHoldState(t, e, gen1); s != "open" {
		t.Fatalf("generation-1 orphan hold state = %q after W2's completion, want OPEN — the terminal release must be worker-scoped, not per-run", s)
	}
}

// TestDeleteWorkerCustodyGuardOwnerScopedLiveDB is the FIX 2 (LOW) regression: the DeleteWorker
// custody guard's count is OWNER-SCOPED. The true owner deleting a custody-held worker gets the
// custody refusal (ErrWorkerHasCustody), but a FOREIGN owner counts 0 (the query filters on
// user_id) and falls through to the owner-scoped delete → ErrWorkerNotFound (the 404 path every
// other sibling ownership check yields), instead of leaking the worker's existence + hold count
// as a 409. On the pre-fix (no owner filter) code the foreign owner would get ErrWorkerHasCustody.
func TestDeleteWorkerCustodyGuardOwnerScopedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	// A worker owned by e.userID, with a TERMINAL run so the active-runs guard passes, and an
	// OPEN custody hold naming the worker (the state a finalization-blocked run leaves behind).
	w := e.seedWorker(t, nil)
	runID := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	           VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'failed', $5)`,
		runID, e.userID, e.repoID, iid, w)
	mhOpenHold(t, e, runID, 1, w)

	// The TRUE owner is refused with the custody error naming the hold.
	err := svc.DeleteWorker(e.ctx, e.userID, w)
	var ce *WorkerHasCustodyError
	if !errors.As(err, &ce) || ce.Holds != 1 {
		t.Fatalf("true-owner DeleteWorker err = %v, want *WorkerHasCustodyError naming 1 hold", err)
	}

	// A FOREIGN owner counts 0 (owner-scoped) and gets the 404 path — NOT ErrWorkerHasCustody,
	// which would leak the worker's existence and custody state cross-tenant.
	foreignErr := svc.DeleteWorker(e.ctx, uuid.New(), w)
	if errors.Is(foreignErr, ErrWorkerHasCustody) {
		t.Fatalf("foreign-owner DeleteWorker leaked custody (got ErrWorkerHasCustody); the guard must be owner-scoped")
	}
	if !errors.Is(foreignErr, ErrWorkerNotFound) {
		t.Fatalf("foreign-owner DeleteWorker err = %v, want ErrWorkerNotFound (the 404 path)", foreignErr)
	}

	// The worker still exists — neither call deleted it (owner refused on custody, foreign 404).
	if !mhWorkerExists(t, e, w) {
		t.Fatalf("worker %s deleted despite the custody refusal / foreign 404", w)
	}
}

// TestDeleteWorkerCustodyListsLiveHolderHoldsLiveDB pins #3921 against a real Postgres: the
// custody refusal names the holds whose LIVE holder is the worker (the same predicate as the
// count), with the run's kind and title, owner-scoped and capped at 10 with Holds the total.
func TestDeleteWorkerCustodyListsLiveHolderHoldsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	w := e.seedWorker(t, nil) // the worker under test
	x := e.seedWorker(t, nil) // another worker: holds a hold ORIGINALLY from w

	seedRun := func(title string) uuid.UUID {
		id := uuid.New()
		iid := *e.nextIID
		*e.nextIID++
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, title)
		           VALUES ($1, $2, $3, 'issue', $4, 'fallback-issue-title', 'd', 'failed', $5)`,
			id, e.userID, e.repoID, iid, title)
		return id
	}
	run := seedRun("Fix the flaky upload")
	otherRun := seedRun("Belongs to X")

	// (ii) a hold live on W, on a run with a real title.
	live := mhOpenHold(t, e, run, 1, w)

	// (i) a hold ORIGINALLY from W but now live on X: it blocks X, not W.
	moved := uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds
	             (id, user_id, repo_id, run_id, generation, state,
	              original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	           VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $6, $4)`,
		moved, e.userID, e.repoID, otherRun, w, x)

	// (iii) a FOREIGN owner's open hold naming W as live holder: never listed or counted for
	// the true owner. (user_id / repo_id carry no FK on the hold table.)
	foreign := uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds
	             (id, user_id, repo_id, run_id, generation, state,
	              original_worker_id, original_worker_identity, live_worker_id)
	           VALUES ($1, $2, NULL, $3, 1, 'open', $4, 'ident', $5)`,
		foreign, uuid.New(), uuid.New(), uuid.New(), w)

	// (v) a hold of THIS owner, live on Y, whose run_id names ANOTHER owner's run: the join
	// to runs is owner-scoped, so that run's kind and title never leak into the listing.
	y := e.seedWorker(t, nil)
	otherUser := uuid.New()
	e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		otherUser, "r3921-"+otherUser.String()+"@e2e")
	foreignRun := uuid.New()
	foreignIID := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, title)
	           VALUES ($1, $2, $3, 'issue', $4, 'foreign-issue-title', 'd', 'failed', 'Foreign owner title')`,
		foreignRun, otherUser, e.repoID, foreignIID)
	crossOwner := mhOpenHold(t, e, foreignRun, 1, y)

	custodyErr := func(worker uuid.UUID) *WorkerHasCustodyError {
		t.Helper()
		err := svc.DeleteWorker(e.ctx, e.userID, worker)
		var ce *WorkerHasCustodyError
		if !errors.As(err, &ce) {
			t.Fatalf("DeleteWorker(%s) err = %v, want *WorkerHasCustodyError", worker, err)
		}
		return ce
	}

	ce := custodyErr(w)
	if ce.Holds != 1 || len(ce.List) != 1 {
		t.Fatalf("W: Holds=%d List=%+v, want exactly the one live hold (moved and foreign holds excluded)", ce.Holds, ce.List)
	}
	if got := ce.List[0]; got.ID != live || got.RunID != run || got.Generation != 1 ||
		got.RunKind != "issue" || got.RunTitle != "Fix the flaky upload" {
		t.Fatalf("W: List[0] = %+v, want hold %s on run %s with kind issue and its title", got, live, run)
	}

	cx := custodyErr(x)
	if cx.Holds != 1 || len(cx.List) != 1 || cx.List[0].ID != moved || cx.List[0].RunTitle != "Belongs to X" {
		t.Fatalf("X: Holds=%d List=%+v, want the moved hold %s", cx.Holds, cx.List, moved)
	}

	cy := custodyErr(y)
	if cy.Holds != 1 || len(cy.List) != 1 || cy.List[0].ID != crossOwner {
		t.Fatalf("Y: Holds=%d List=%+v, want the cross-owner-run hold %s", cy.Holds, cy.List, crossOwner)
	}
	if got := cy.List[0]; got.RunKind != "" || got.RunTitle != "" {
		t.Fatalf("Y: List[0] kind=%q title=%q, want both empty: another owner's run must not leak", got.RunKind, got.RunTitle)
	}

	// (iv) more than the cap live on W: the list is capped at 10, oldest first, and Holds is
	// the full total.
	for g := int64(2); g <= 12; g++ {
		mhOpenHold(t, e, run, g, w)
	}
	// Pin created_at to the generation so "oldest" is unambiguous (no microsecond ties).
	e.exec(t, `UPDATE recovery_custody_holds SET created_at = now() - interval '1 hour' + generation * interval '1 second'
	           WHERE run_id = $1 AND live_worker_id = $2`, run, w)
	ce = custodyErr(w)
	if ce.Holds != 12 {
		t.Fatalf("W: Holds = %d, want the total 12", ce.Holds)
	}
	if len(ce.List) != 10 {
		t.Fatalf("W: len(List) = %d, want the cap 10", len(ce.List))
	}
	if ce.List[0].ID != live {
		t.Fatalf("W: List[0] = %s, want the oldest hold %s first", ce.List[0].ID, live)
	}
	// created_at follows the generation, so the 10 oldest are exactly generations 1..10.
	seen := map[int64]bool{}
	for _, r := range ce.List {
		seen[r.Generation] = true
	}
	for g := int64(1); g <= 10; g++ {
		if !seen[g] {
			t.Fatalf("W: List generations = %v, want exactly the 10 oldest (1..10)", seen)
		}
	}
	for _, r := range ce.List {
		if r.ID == foreign || r.ID == moved {
			t.Fatalf("W: List carries hold %s that does not name W as live holder for this owner", r.ID)
		}
	}
}

// TestSetStateCompletedSameWorkerMultiGenReleasesOnlyCurrentLiveDB is the D1/D2 CORE regression
// (PRD #1349 M4) — the case the pre-M4 generation-blind completion release got WRONG. ONE worker
// W holds TWO open holds on a run: an uncaptured generation-1 hold plus a generation-2 hold from
// an affinity re-claim after a transient loss (BOTH live_worker_id = W). The run completes at
// claim_generation = 2, reported by W. The generation-EXACT completion release
// (ReleaseCustodyHoldExact, passing run.claim_generation) settles ONLY generation 2's hold;
// generation 1's uncaptured hold stays OPEN, preserving its only copy of that committed work.
//
// This is the DISCRIMINATING test the sibling different-worker test above does NOT cover: on the
// pre-fix per-run+worker release (ReleaseCustodyForRunWorker matched EVERY open hold held live by
// W) BOTH generation-1 AND generation-2 holds would be released, dropping generation 1's only
// copy — so the gen-1-stays-open assertion FAILS on pre-fix code and PASSES only with the exact
// release.
func TestSetStateCompletedSameWorkerMultiGenReleasesOnlyCurrentLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	w := e.seedWorker(t, nil) // the ONE worker that holds BOTH generations live
	runID := e.seedLegacyRunningRun(t, w)
	e.exec(t, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, runID)

	gen1 := mhOpenHold(t, e, runID, 1, w) // uncaptured orphan: protects the ONLY copy of gen-1's work
	gen2 := mhOpenHold(t, e, runID, 2, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "completed", Branch: strPtr("agent/issue-samegen"), Head: strPtr("ignored")})
	if err != nil {
		t.Fatalf("SetState(completed): %v", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("completion must apply; applied=%v status=%q", applied, run.Status)
	}
	if s := mhHoldState(t, e, gen2); s != "released" {
		t.Fatalf("current-generation (2) hold state = %q after completion, want released", s)
	}
	if s := mhHoldState(t, e, gen1); s != "open" {
		t.Fatalf("uncaptured generation-1 hold state = %q after W's generation-2 completion, want OPEN — a generation-blind run+worker release would drop generation 1's only copy (the D1/D2 core hazard)", s)
	}
}
