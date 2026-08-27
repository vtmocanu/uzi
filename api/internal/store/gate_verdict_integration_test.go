package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunHasVerdictSinceGateOpenedLiveDB pins issue #182's predicate against a REAL
// Postgres, which is the only place it can be pinned: sqlc regenerating cleanly says
// nothing about whether the statement runs or what it answers, and the workersvc arm
// tests drive a fake that deliberately does NOT re-derive it.
//
// Two independent halves, and the second is the one that matters:
//
//   - WHICH KINDS COUNT. Four of the six legal kinds are verdicts (approve_plan,
//     reject_plan, cancel, revise_plan); follow_up and answer are not. Every kind is
//     asserted, so widening or narrowing the IN list reddens here.
//
//   - 🔴 THE `created_at >= gate_opened_at` BOUNDARY, INCLUDING THE DISCRIMINATING CASE:
//     an input that arrived BEFORE this gate opened must NOT count. That single case is
//     what separates this predicate from the `consumed_at IS NULL` one the issue's
//     description originally proposed and that measurement refuted — a fixture built only
//     from "the user responds at an open gate" agrees with both and proves nothing.
//     The `==` case is asserted for its own reason: CreateApprovePlanInput and
//     CreateStopVerdictInput both SET updated_at = now() in the same statement that
//     inserts the row, and now() is the transaction timestamp, so an undelivered
//     approve-with-selection or cancel lands EXACTLY equal. Under `>` it would report
//     "waiting for the plan to be approved" an hour after the owner acted.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestRunHasVerdictSinceGateOpenedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("gateverdict-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// gateOpen stands in for runs.updated_at as SetRunAwaitingApproval stamped it — the
	// value the detector passes in. Fixed, so every offset below is exact.
	gateOpen := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	iid := 0
	newRun := func() uuid.UUID {
		iid++
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, updated_at)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'awaiting_approval', $5)`,
			runID, userID, repoID, iid, gateOpen)
		return runID
	}
	addInput := func(runID uuid.UUID, kind string, createdAt time.Time) {
		mustExec(ctx, t, pool,
			`INSERT INTO run_user_inputs (run_id, kind, body, created_at) VALUES ($1, $2, 'b', $3)`,
			runID, kind, createdAt)
	}
	ask := func(t *testing.T, runID uuid.UUID) bool {
		t.Helper()
		got, err := q.RunHasVerdictSinceGateOpened(ctx, store.RunHasVerdictSinceGateOpenedParams{
			RunID:        runID,
			GateOpenedAt: pgtype.Timestamptz{Time: gateOpen, Valid: true},
		})
		if err != nil {
			t.Fatalf("RunHasVerdictSinceGateOpened: %v", err)
		}
		return got
	}

	// A run with no inputs at all: the plain "nobody has responded" gate.
	t.Run("no inputs", func(t *testing.T) {
		if ask(t, newRun()) {
			t.Fatal("got true for a run with no steering inputs at all")
		}
	})

	// Every legal kind, each in its own run, each landing EXACTLY at the gate-open
	// instant. The want column is the four-of-six rule: include exactly the kinds the
	// worker's route() turns into a gate event or a cancel.
	t.Run("per kind at the gate-open instant", func(t *testing.T) {
		kinds := []struct {
			kind string
			want bool
			why  string
		}{
			{"approve_plan", true, "route() buffers it as a gate verdict; the human approved and the worker has not resumed"},
			{"reject_plan", true, "route() buffers it as a gate verdict"},
			{"revise_plan", true, "route() queues it for a revision round — issue #182's headline case"},
			{"cancel", true, "route() aborts on it; the run is leaving, not waiting on its owner"},
			{"follow_up", false, "route() pushes it to a buffer that never reaches serviceGate: a message, not an answer, so the gate is STILL waiting on the human"},
			{"answer", false, "submitAnswer refuses unless the run is awaiting_input, so this is unreachable at a plan gate"},
		}
		for _, k := range kinds {
			t.Run(k.kind, func(t *testing.T) {
				runID := newRun()
				addInput(runID, k.kind, gateOpen)
				if got := ask(t, runID); got != k.want {
					t.Fatalf("kind %q → %v, want %v: %s", k.kind, got, k.want, k.why)
				}
			})
		}
	})

	// The boundary, one run per position.
	t.Run("created_at boundary", func(t *testing.T) {
		cases := []struct {
			name   string
			offset time.Duration
			want   bool
		}{
			{"an hour before the gate opened", -time.Hour, false},
			{"one microsecond before the gate opened", -time.Microsecond, false},
			{"exactly at the gate-open instant", 0, true},
			{"one microsecond after the gate opened", time.Microsecond, true},
			{"an hour after the gate opened", time.Hour, true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				runID := newRun()
				addInput(runID, "revise_plan", gateOpen.Add(tc.offset))
				if got := ask(t, runID); got != tc.want {
					t.Fatalf("revise_plan at gate_open%+v → %v, want %v", tc.offset, got, tc.want)
				}
			})
		}
	})

	// 🔴 THE DISCRIMINATING CASE, stated on its own rather than left inside the boundary
	// table above, because it is the whole reason this predicate is not the refuted one.
	// A follow_up submitted while the run was RUNNING, never consumed, then a plan gate
	// opens after it. `consumed_at IS NULL` is satisfied; `created_at >= updated_at` is
	// not. The run is genuinely waiting for a human and must still say approval_idle.
	t.Run("pre-gate unconsumed input does not count", func(t *testing.T) {
		runID := newRun()
		addInput(runID, "follow_up", gateOpen.Add(-30*time.Minute))
		addInput(runID, "revise_plan", gateOpen.Add(-10*time.Minute)) // a PRIOR episode's verdict
		if ask(t, runID) {
			t.Fatal("counted an input that predates this gate — the health flag would then read waiting_worker on a run nobody has responded to, which is the failure the refuted consumed_at predicate had")
		}
		// Same run, same rows, plus one that DOES belong to this episode.
		addInput(runID, "revise_plan", gateOpen)
		if !ask(t, runID) {
			t.Fatal("a current-episode verdict did not count alongside older rows")
		}
	})

	// Scoping: another run's verdict must never answer for this one.
	t.Run("other runs do not leak", func(t *testing.T) {
		mine, theirs := newRun(), newRun()
		addInput(theirs, "approve_plan", gateOpen)
		if ask(t, mine) {
			t.Fatal("another run's verdict leaked across the run_id predicate")
		}
		if !ask(t, theirs) {
			t.Fatal("the run that owns the verdict reported false")
		}
	})

	// consumed_at is deliberately NOT in the predicate: the worker drains inputs every
	// ~3s while parked at the gate, so a pending-only predicate is true for seconds
	// against a 15s sweep and misses the case entirely. A consumed verdict must still
	// count — that is the whole point of timing off created_at.
	t.Run("consumed verdicts still count", func(t *testing.T) {
		runID := newRun()
		addInput(runID, "approve_plan", gateOpen)
		mustExec(ctx, t, pool, `UPDATE run_user_inputs SET consumed_at = now() WHERE run_id = $1`, runID)
		if !ask(t, runID) {
			t.Fatal("a CONSUMED verdict stopped counting — the predicate has grown a consumed_at filter, which is exactly the refuted design")
		}
	})
}
