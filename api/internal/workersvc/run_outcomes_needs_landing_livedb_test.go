package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSelfRunOutcomesNeedsLandingLiveDB is the issue #1418 M2 acceptance proof (criterion 2):
// the server-computed needs_landing count on the run-outcomes DTO EQUALS the number of failed
// runs whose per-run landing_state derives to needs_landing, over the SAME window and
// population. It seeds one owner a spread of runs and then computes the expected count two
// INDEPENDENT ways — the SQL FILTER aggregate (via the SelfRunOutcomes passthrough, which
// injects AllHumanLandableFailOrigins so the human-landable set never leaves Go) and a Go loop
// applying workersvc.DeriveLandingState per run — and asserts they are equal for BOTH windows.
//
// The seeded spread covers, for the ONE owner:
//   - each of the 4 human-landable fail_origins, across the four recoverability shapes:
//     (a) no capture + no preserved_patch      -> unrecoverable, NOT counted
//     (b) an 'available' recovery capture       -> needs_landing, counted
//     (c) a preserved_patch                     -> needs_landing, counted
//     (d) a non-'available' ('preparing') cap   -> unrecoverable, NOT counted (state-scoping)
//   - a non-landable failed origin (agent_failure) WITH both an available capture AND a
//     preserved_patch, proving the fail_origin filter excludes it (landing_state == none), and
//   - a completed run, proving only 'failed' rows are ever a needs_landing candidate.
//
// One needs_landing run is aged past the 7-day window so lifetime and last7 differ, exercising
// the window predicate on the needs_landing columns too.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.
func TestSelfRunOutcomesNeedsLandingLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	svc := New(q, newBox(t), testParams())

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("needs-landing-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/needs-landing', 'https://forge.e2e/g/needs-landing', 'main', true)`, repoID, connID)

	// seededRun is one run plus the availability facts the derivation reads. `recent` places it
	// inside the 7-day window (all seeded runs are terminal, kind 'issue' — i.e. IN the counted
	// population — so the Go derivation loop below iterates exactly the SQL population).
	type seededRun struct {
		status            string
		failOrigin        string // "" -> NULL fail_origin (only used by the completed run)
		hasPreservedPatch bool
		captureState      string // "" -> no capture row; else the recovery_captures.state
		recent            bool
	}
	seeds := []seededRun{
		// The 4 human-landable origins across the four recoverability shapes.
		{status: "failed", failOrigin: "finalize_base_align_conflict", recent: true},                      // (a) unrecoverable
		{status: "failed", failOrigin: "workflow_scope_missing", captureState: "available", recent: true}, // (b) needs_landing (in window)
		{status: "failed", failOrigin: "push_secret_blocked", hasPreservedPatch: true, recent: false},     // (c) needs_landing (OUT of window)
		{status: "failed", failOrigin: "history_rewritten", captureState: "preparing", recent: true},      // (d) unrecoverable (state-scoped)
		// A non-landable failed origin, deliberately given BOTH recoverability facts: the
		// fail_origin filter must still exclude it (landing_state == none).
		{status: "failed", failOrigin: "agent_failure", hasPreservedPatch: true, captureState: "available", recent: true},
		// A completed run: never a needs_landing candidate.
		{status: "completed", failOrigin: "", recent: true},
	}

	var iid int64
	for _, s := range seeds {
		iid++
		runID := uuid.New()
		createdAt := time.Now().Add(-1 * time.Hour)
		if !s.recent {
			createdAt = time.Now().Add(-10 * 24 * time.Hour)
		}
		var origin any
		if s.failOrigin != "" {
			origin = s.failOrigin
		}
		var patch any
		if s.hasPreservedPatch {
			patch = "diff --git a b\n"
		}
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, fail_origin, preserved_patch, created_at)
		      VALUES ($1, $2, $3, 'issue', $4, 'seed', 'ctx', $5, $6, $7, $8)`,
			runID, userID, repoID, iid, s.status, origin, patch, createdAt)
		if s.captureState != "" {
			holdID, capID := uuid.New(), uuid.New()
			// live_worker_id/live_run_id stay NULL (nullable FKs), so no worker row is needed;
			// original_worker_id is a NOT NULL value column with no FK.
			exec(`INSERT INTO recovery_custody_holds
			        (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity)
			      VALUES ($1, $2, $3, $4, 1, 'open', $5, 'w-ident')`,
				holdID, userID, repoID, runID, uuid.New())
			exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
			      VALUES ($1, $2, $3, $4, 'w-ident', 'H0', $5, $6)`,
				capID, holdID, runID, userID, "k-"+capID.String(), s.captureState)
		}
	}

	// Independent expectation: apply the ONE derivation (workersvc.DeriveLandingState) per seeded
	// run over the SAME availability facts we seeded, counting LandingStateNeedsLanding. A capture
	// only makes a run "have an available capture" when its state is exactly 'available'.
	var wantLifetime, wantLast7 int64
	for _, s := range seeds {
		if s.status != "completed" && s.status != "failed" && s.status != "cancelled" {
			continue // not in the counted population
		}
		var originPtr *string
		if s.failOrigin != "" {
			o := s.failOrigin
			originPtr = &o
		}
		hasAvailableCapture := s.captureState == "available"
		if DeriveLandingState(originPtr, s.hasPreservedPatch, hasAvailableCapture) == LandingStateNeedsLanding {
			wantLifetime++
			if s.recent {
				wantLast7++
			}
		}
	}

	got, err := svc.SelfRunOutcomes(ctx, userID)
	if err != nil {
		t.Fatalf("SelfRunOutcomes: %v", err)
	}

	// Acceptance criterion 2: the server count EQUALS the per-run landing_state derivation, for
	// the same window and population.
	if got.LifetimeNeedsLanding != wantLifetime {
		t.Fatalf("lifetime needs_landing count (%d) != per-run DeriveLandingState count (%d): the SQL "+
			"aggregate and workersvc.DeriveLandingState disagree over the same population",
			got.LifetimeNeedsLanding, wantLifetime)
	}
	if got.Last7NeedsLanding != wantLast7 {
		t.Fatalf("last7 needs_landing count (%d) != per-run DeriveLandingState count (%d): the SQL "+
			"aggregate and workersvc.DeriveLandingState disagree over the 7-day window",
			got.Last7NeedsLanding, wantLast7)
	}

	// Guard the fixture itself against a vacuous pass: the seed is designed to yield 2 lifetime
	// (shapes b, c) and 1 last7 (shape c is aged out), and needs_landing must stay a strict
	// sub-cut of failed (invariant asserted server-side).
	if wantLifetime != 2 || wantLast7 != 1 {
		t.Fatalf("fixture drift: expected 2 lifetime / 1 last7 needs_landing from the seed, got %d / %d",
			wantLifetime, wantLast7)
	}
	if got.LifetimeNeedsLanding > got.LifetimeFailed || got.Last7NeedsLanding > got.Last7Failed {
		t.Fatalf("needs_landing must be <= failed: lifetime %d/%d, last7 %d/%d",
			got.LifetimeNeedsLanding, got.LifetimeFailed, got.Last7NeedsLanding, got.Last7Failed)
	}
}
