package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SetRunRunning's PRD #84 M4 requirement-set persistence against a REAL Postgres. An
// AUTOPILOT run auto-approves its own plan and NEVER reports awaiting_approval, so it
// rides the plan-time inferred requirement set on its self-contained `running` report
// (runner.ts toolchainReportFields) instead. This proves SetRunRunning consumes it the
// SAME way SetRunAwaitingApproval does — required_capabilities UNION-MERGED
// (escalation-only), required_tools/size_class SET, all COALESCE-guarded so a nil param
// is a no-op rather than a NULL wipe of the NOT-NULL columns. Without the running-path
// clauses the inference is silently lost for every auto-approved run (the sweep uses
// auto-approve), which is the review finding this covers.
//
// Reuses fleetFixture / ownedRun / runReqs / sameSet (same package, see
// claim_fleet_placement_integration_test.go and set_awaiting_required_integration_test.go).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// setRunning applies a `running` report on runID owned by workerID with the given
// inferred requirement params (nil caps/tools = absent; empty sizeClass = absent
// size_class param), asserting exactly one row updated. ownedRun inserts the run in
// 'running' state, which SetRunRunning's WHERE clause admits as the idempotent
// running→running heartbeat — the shape an autopilot's post-approval report takes.
func setRunning(fx *fleetFixture, runID, workerID uuid.UUID, caps, tools []string, sizeClass string) {
	fx.t.Helper()
	var sizeParam pgtype.Text
	if sizeClass != "" {
		sizeParam = pgtype.Text{String: sizeClass, Valid: true}
	}
	rows, err := fx.q.SetRunRunning(fx.ctx, store.SetRunRunningParams{
		IterationCount:       1,
		ID:                   runID,
		WorkerID:             pgtype.UUID{Bytes: workerID, Valid: true},
		InferredCapabilities: caps,
		InferredTools:        tools,
		SizeClass:            sizeParam,
	})
	if err != nil {
		fx.t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 1 {
		fx.t.Fatalf("SetRunRunning affected %d rows, want 1", rows)
	}
}

// TestSetRunningRequirementMergeLiveDB proves the escalation-only merge on the autopilot
// running path: a run seeded with the repo hint {docker} that reports inferred
// capabilities {jvm}, tools {go,node} and size_class "m" ends with the UNION
// {docker,jvm} (deduped), tools {go,node} and size_class "m" (SET) — mirroring the
// awaiting_approval path so autopilot runs are not stripped of their inference.
func TestSetRunningRequirementMergeLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	run := ownedRun(fx, w, []string{"docker"}) // M2 repo hint pre-seeded

	setRunning(fx, run, w, []string{"jvm"}, []string{"go", "node"}, "m")

	caps, tools, size := runReqs(fx, run)
	if !sameSet(caps, []string{"docker", "jvm"}) {
		t.Fatalf("required_capabilities = %v, want union {docker,jvm}", caps)
	}
	if !sameSet(tools, []string{"go", "node"}) {
		t.Fatalf("required_tools = %v, want {go,node}", tools)
	}
	if size != "m" {
		t.Fatalf("size_class = %q, want %q", size, "m")
	}

	// Re-reporting an already-present capability adds nothing (union is idempotent, the
	// heartbeat case), a new tools set REPLACES, and a new size_class REPLACES.
	setRunning(fx, run, w, []string{"docker"}, []string{"rust"}, "l")
	caps, tools, size = runReqs(fx, run)
	if !sameSet(caps, []string{"docker", "jvm"}) {
		t.Fatalf("after re-report required_capabilities = %v, want unchanged union {docker,jvm}", caps)
	}
	if !sameSet(tools, []string{"rust"}) {
		t.Fatalf("required_tools = %v, want replaced {rust}", tools)
	}
	if size != "l" {
		t.Fatalf("after re-report size_class = %q, want replaced %q", size, "l")
	}
}

// TestSetRunningRequirementAbsentNoWipeLiveDB proves the absent-safe path on the running
// heartbeat: an autopilot run establishes its requirement set on the approval report,
// then the ordinary session-id/iteration heartbeats (which omit the fields) must leave
// the columns untouched rather than nulling the NOT-NULL arrays.
func TestSetRunningRequirementAbsentNoWipeLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	run := ownedRun(fx, w, []string{"docker"})

	// Establish a non-empty tools set (+ the seeded docker capability) and a size_class "m".
	setRunning(fx, run, w, nil, []string{"go", "node"}, "m")
	caps, tools, size := runReqs(fx, run)
	if !sameSet(caps, []string{"docker"}) || !sameSet(tools, []string{"go", "node"}) || size != "m" {
		t.Fatalf("setup: caps=%v tools=%v size=%q, want {docker}, {go,node} and %q", caps, tools, size, "m")
	}

	// A plain heartbeat: all three params absent. A nil text[] and an invalid pgtype.Text
	// both encode SQL NULL, so the COALESCE guards are what keep the NOT-NULL columns intact.
	setRunning(fx, run, w, nil, nil, "")
	caps, tools, size = runReqs(fx, run)
	if !sameSet(caps, []string{"docker"}) {
		t.Fatalf("absent params wiped required_capabilities to %v, want unchanged {docker}", caps)
	}
	if !sameSet(tools, []string{"go", "node"}) {
		t.Fatalf("absent params wiped required_tools to %v, want unchanged {go,node}", tools)
	}
	if size != "m" {
		t.Fatalf("absent param wiped size_class to %q, want unchanged %q", size, "m")
	}
}
