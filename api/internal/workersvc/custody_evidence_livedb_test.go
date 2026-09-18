package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1392 M1 (D3) release-evidence coverage for the SERVER-DERIVED release paths, against a
// REAL Postgres: the terminal-completion release stamps 'publication', and the reconciler
// stamps the per-hold class ListReleasableCustodyHolds computes ('publication' for a completed
// backstop, 'archive' for a ready capture). Skipped unless UZI_TEST_DATABASE_URL is set.

// TestSetStateCompletedStampsPublicationEvidenceLiveDB: a completed run's custody release stamps
// release_evidence='publication'.
func TestSetStateCompletedStampsPublicationEvidenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	w := e.seedWorker(t, nil)
	runID := e.seedLegacyRunningRun(t, w)
	e.exec(t, `UPDATE runs SET claim_generation = 1 WHERE id = $1`, runID)
	hold := mhOpenHold(t, e, runID, 1, w)

	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID,
		StateRequest{State: "completed", Branch: strPtr("agent/issue-pub"), Head: strPtr("ignored")}); err != nil || !applied {
		t.Fatalf("SetState(completed): applied=%v err=%v", applied, err)
	}
	if state, ev := e.fpHoldEvidence(t, hold); state != "released" || ev == nil || *ev != "publication" {
		t.Fatalf("completed-run hold state=%q evidence=%v, want released/publication", state, ev)
	}
}

// TestReconcileStampsArchiveAndPublicationEvidenceLiveDB: the reconciler stamps 'archive' for a
// hold released on the strength of a ready capture and 'publication' for a completed-run
// backstop hold — the per-hold class computed by ListReleasableCustodyHolds.
func TestReconcileStampsArchiveAndPublicationEvidenceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	// (a) A FAILED run with an open hold covered by a READY capture → releasable via the
	// available-capture disjunct → evidence 'archive'.
	archiveRun := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
	           VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'failed', 1)`, archiveRun, e.userID, e.repoID, iid)
	archiveW := e.seedWorker(t, nil)
	archiveHold := mhOpenHold(t, e, archiveRun, 1, archiveW)
	e.exec(t, `INSERT INTO recovery_captures
	             (hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	           VALUES ($1, $2, $3, 'ident', 'HH', 'kk', 'available')`, archiveHold, archiveRun, e.userID)

	// (b) A COMPLETED run at claim_generation=1 with the generation-1 hold → releasable via the
	// completed backstop → evidence 'publication'.
	pubRun := uuid.New()
	iid = *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
	           VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', 1)`, pubRun, e.userID, e.repoID, iid)
	pubW := e.seedWorker(t, nil)
	pubHold := mhOpenHold(t, e, pubRun, 1, pubW)

	if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
		t.Fatalf("ReconcileCustodyReleases: %v", err)
	}
	if state, ev := e.fpHoldEvidence(t, archiveHold); state != "released" || ev == nil || *ev != "archive" {
		t.Fatalf("ready-capture hold state=%q evidence=%v, want released/archive", state, ev)
	}
	if state, ev := e.fpHoldEvidence(t, pubHold); state != "released" || ev == nil || *ev != "publication" {
		t.Fatalf("completed-backstop hold state=%q evidence=%v, want released/publication", state, ev)
	}
}
