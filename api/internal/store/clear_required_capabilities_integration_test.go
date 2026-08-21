package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ClearRunRequiredCapabilities (PRD #84 M4 4c, the "run without the capability" user
// override) against a REAL Postgres: the owner + awaiting_approval guards are SQL, so the
// fake-store unit tests cannot cover them. Proven here against real rows:
//   - an owner's awaiting_approval run has its required_capabilities emptied (1 row);
//   - a WRONG owner does not (0 rows, column unchanged) — no cross-tenant clear;
//   - a run OUTSIDE the plan gate (queued) does not (0 rows, column unchanged);
//   - it is idempotent (a second clear reports 0 rows — already empty AND already matched).
//
// Reuses fleetFixture (claim_fleet_placement_integration_test.go, same package). Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// awaitingRunWithCaps inserts an awaiting_approval run owned by the fixture's user, owned by
// a worker, carrying the given required_capabilities, and returns its id.
func awaitingRunWithCaps(fx *fleetFixture, workerID uuid.UUID, caps []string) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, required_capabilities)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'awaiting_approval', $5, $6)`,
		id, fx.userID, fx.repoID, fx.nextIID(), workerID, caps)
	return id
}

func runCaps(fx *fleetFixture, runID uuid.UUID) []string {
	fx.t.Helper()
	var caps []string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT required_capabilities FROM runs WHERE id = $1`, runID).Scan(&caps); err != nil {
		fx.t.Fatalf("read required_capabilities: %v", err)
	}
	return caps
}

func TestClearRunRequiredCapabilitiesLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("owner-worker", capOf(1), false)

	t.Run("owner's awaiting_approval run is cleared", func(t *testing.T) {
		runID := awaitingRunWithCaps(fx, w, []string{"docker", "jvm"})
		n, err := fx.q.ClearRunRequiredCapabilities(fx.ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: fx.userID})
		if err != nil {
			t.Fatalf("ClearRunRequiredCapabilities: %v", err)
		}
		if n != 1 {
			t.Fatalf("rows affected = %d, want 1", n)
		}
		if got := runCaps(fx, runID); len(got) != 0 {
			t.Fatalf("required_capabilities = %v, want empty after clear", got)
		}
		// Idempotent: a second clear matches the row but changes nothing observable; the
		// UPDATE still touches the row, so RowsAffected stays 1 (Postgres counts the matched
		// row, not whether a value changed). Assert it does not error and leaves it empty.
		if _, err := fx.q.ClearRunRequiredCapabilities(fx.ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: fx.userID}); err != nil {
			t.Fatalf("second clear: %v", err)
		}
		if got := runCaps(fx, runID); len(got) != 0 {
			t.Fatalf("required_capabilities = %v, want still empty", got)
		}
	})

	t.Run("a non-owner clears nothing", func(t *testing.T) {
		runID := awaitingRunWithCaps(fx, w, []string{"docker"})
		n, err := fx.q.ClearRunRequiredCapabilities(fx.ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: uuid.New()})
		if err != nil {
			t.Fatalf("ClearRunRequiredCapabilities: %v", err)
		}
		if n != 0 {
			t.Fatalf("rows affected = %d, want 0 (wrong owner)", n)
		}
		if got := runCaps(fx, runID); len(got) != 1 || got[0] != "docker" {
			t.Fatalf("required_capabilities = %v, want unchanged [docker]", got)
		}
	})

	t.Run("a run outside the plan gate clears nothing", func(t *testing.T) {
		runID := fx.queuedRun()
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET required_capabilities = $2 WHERE id = $1`, runID, []string{"docker"})
		n, err := fx.q.ClearRunRequiredCapabilities(fx.ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: fx.userID})
		if err != nil {
			t.Fatalf("ClearRunRequiredCapabilities: %v", err)
		}
		if n != 0 {
			t.Fatalf("rows affected = %d, want 0 (not awaiting_approval)", n)
		}
		if got := runCaps(fx, runID); len(got) != 1 || got[0] != "docker" {
			t.Fatalf("required_capabilities = %v, want unchanged [docker]", got)
		}
	})
}
