package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the DEDICATED wall-park pin for the D16/D19 claim fence that lives in SetState's Go
// wrapper (service.go), not in the store queries. The store-level tests
// (store/wall_park_livedb_test.go) prove SetRunRunning/SetRunCompleted are generation-BLIND at the
// SQL layer and delegate the WITH/WITHOUT-a-stamped-generation distinction to this wrapper; until now
// that distinction was proven only against a credential_switch RELEASE
// (credential_switch_livedb_test.go's D16 cases). This test proves the same fence against a WALL
// park, so "an old flight's stamped-generation report is rejected, before AND after the resume" is a
// wall-park pin rather than a credential-switch proxy. Skipped unless UZI_TEST_DATABASE_URL is set.

// TestWallParkFenceOldFlightRejectedLiveDB drives a stamped-generation running/completed report from
// the OLD flight through SetState against a SERVER-parked (ParkRunsAtWall) row, and asserts it is
// rejected with no mutation in both windows:
//
//   - BEFORE the owner's resume: the row is paused with claim_released_at set (the fence armed) and
//     worker_id kept, so the FOR UPDATE wrapper's `locked.ClaimReleasedAt.Valid` conjunct
//     (service.go, the generation-fence check) rejects the matching-generation report as ErrStaleClaim.
//     VERIFIED as the pin by reading service.go: dropping that conjunct would let a matching-generation
//     report through the wrapper (its store transition would then be the only guard, changing the
//     error from ErrStaleClaim to a benign 0-row no-op — so the ErrStaleClaim assertion flips).
//   - AFTER ExtendAndResumeWallPark: the server-parked resume nulls worker_id (D19), so SetState's
//     ownership guard (runOwnedByWorker) rejects the old flight with ErrRunNotOwned before the fence
//     even runs. VERIFIED as the pin by reading service.go/wall-park SQL: were worker_id kept on a
//     server-parked resume, the wrapper would admit the report (claim_released_at is cleared and the
//     generation is preserved) and SetRunRunning admits a queued row for the same worker.
func TestWallParkFenceOldFlightRejectedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env) // wires the tx beginner the FOR UPDATE generation fence needs
	userID, repoID := seedWallOwner(t, env)
	gen := int64(3)

	// The OLD flight's worker: STALE (heartbeat 1h ago) + INCAPABLE (no wall_park_v1), so
	// ParkRunsAtWall's stale/incapable arm server-parks its out-of-time run (claim_released_at set,
	// worker_id KEPT). No protocol capabilities: the stamped-generation fence engages purely on the
	// report carrying a generation, matching credential_switch_livedb_test's plain-worker fence cases.
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
	          VALUES ($1, $2, 'oldflight', $3, 'online', now() - interval '1 hour', 'nonce-A')`, workerID, userID, workerID[:])
	wkr := store.Worker{ID: workerID, UserID: userID}

	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, worker_id, started_at, status_since, budget_wall_seconds, claim_generation)
	          VALUES ($1, $2, $3, 'issue', 987201, 't', 'd', 'running', $4, now() - interval '2 hours', now() - interval '30 seconds', 60, $5)`,
		runID, userID, repoID, workerID, gen)

	// Server-park the row. ParkRunsAtWall is a GLOBAL sweep; assert this run is in the parked set.
	now := time.Now().UTC()
	parked, err := env.q.ParkRunsAtWall(env.ctx, store.ParkRunsAtWallParams{
		Now:                  pgconv.Time(now),
		GlobalTimeoutSeconds: 1,
		WorkerStaleCutoff:    pgconv.Time(now.Add(-45 * time.Second)),
		GraceSeconds:         600,
	})
	if err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	inParked := false
	for _, r := range parked {
		if r.ID == runID {
			inParked = true
			break
		}
	}
	if !inParked {
		t.Fatalf("run %s not in the parked set %v", runID, parked)
	}
	// Precondition: paused, claim_released_at set (fence armed), worker_id KEPT (still this flight's).
	before := mustRun(t, env, runID)
	if before.Status != "paused" || !before.ClaimReleasedAt.Valid || !before.WorkerID.Valid {
		t.Fatalf("server-parked precondition = (status %q, released %v, worker %v), want (paused, true, true)",
			before.Status, before.ClaimReleasedAt.Valid, before.WorkerID.Valid)
	}

	// report drives a stamped-generation report from the old flight and asserts it is a no-op
	// rejection matching wantErr, leaving the run at wantStatus.
	report := func(when, state string, wantErr error, wantStatus string) {
		t.Helper()
		_, applied, err := svc.SetState(env.ctx, wkr, runID, StateRequest{State: state, ClaimGeneration: &gen})
		if !errors.Is(err, wantErr) {
			t.Fatalf("%s %s report: err = %v, want %v", when, state, err, wantErr)
		}
		if applied {
			t.Fatalf("%s %s report must NOT be applied", when, state)
		}
		if got := statusOf(t, env, runID); got != wantStatus {
			t.Fatalf("%s %s report moved the run to %q, want it UNCHANGED at %q", when, state, got, wantStatus)
		}
	}

	// BEFORE the resume: the claim_released_at fence rejects the matching-generation report as stale.
	report("before-resume", "running", ErrStaleClaim, "paused")
	report("before-resume", "completed", ErrStaleClaim, "paused")

	// The owner extends-and-resumes: paused -> queued, worker_id NULL (server-parked, D19).
	if _, err := env.q.ExtendAndResumeWallPark(env.ctx, store.ExtendAndResumeWallParkParams{
		ID: runID, UserID: userID, Secs: 3600, Cap: 57600, GlobalTimeoutSeconds: 7200,
		Body: pgtype.Text{String: "extend+resume", Valid: true},
	}); err != nil {
		t.Fatalf("ExtendAndResumeWallPark: %v", err)
	}
	resumed := mustRun(t, env, runID)
	if resumed.Status != "queued" || resumed.WorkerID.Valid {
		t.Fatalf("resumed row = (status %q, worker %v), want (queued, NULL worker)", resumed.Status, resumed.WorkerID.Valid)
	}

	// AFTER the resume: worker_id is NULL, so the ownership guard rejects the old flight (ErrRunNotOwned)
	// before the fence runs — still a no-op, still leaving the run untouched at queued.
	report("after-resume", "running", ErrRunNotOwned, "queued")
	report("after-resume", "completed", ErrRunNotOwned, "queued")
}
