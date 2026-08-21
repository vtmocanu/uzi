package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SetRunAwaitingApproval's PRD #84 M4 4b requirement-set persistence against a REAL
// Postgres — the absent-safe, escalation-only merge the fake-store unit tests cannot
// cover because they never run the SQL. The query UNION-MERGES the plan-time inferred
// required_capabilities onto the run's existing set (the M2 repo hint) and SETS
// required_tools, with both assignments COALESCE-guarded so a nil param is a no-op
// rather than a NULL wipe of the NOT-NULL array columns.
//
// Reuses fleetFixture (claim_fleet_placement_integration_test.go, same package).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// ownedRun inserts a run OWNED by workerID in a non-terminal ('running') state — the
// precondition SetRunAwaitingApproval's WHERE clause requires (id AND worker_id AND
// status NOT IN terminal) — with an explicit seeded required_capabilities set, and
// returns its id.
func ownedRun(fx *fleetFixture, workerID uuid.UUID, caps []string) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, required_capabilities)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5, $6)`,
		id, fx.userID, fx.repoID, fx.nextIID(), workerID, caps)
	return id
}

// runReqs reads a run's persisted requirement columns.
func runReqs(fx *fleetFixture, runID uuid.UUID) (caps, tools []string, sizeClass string) {
	fx.t.Helper()
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT required_capabilities, required_tools, size_class FROM runs WHERE id = $1`, runID).Scan(&caps, &tools, &sizeClass); err != nil {
		fx.t.Fatalf("read run reqs: %v", err)
	}
	return caps, tools, sizeClass
}

// setAwaiting parks runID owned by workerID at awaiting_approval with the given inferred
// requirement params (nil caps/tools = absent; empty sizeClass = absent size_class param),
// asserting exactly one row updated.
func setAwaiting(fx *fleetFixture, runID, workerID uuid.UUID, caps, tools []string, sizeClass string) {
	fx.t.Helper()
	var sizeParam pgtype.Text
	if sizeClass != "" {
		sizeParam = pgtype.Text{String: sizeClass, Valid: true}
	}
	rows, err := fx.q.SetRunAwaitingApproval(fx.ctx, store.SetRunAwaitingApprovalParams{
		PlanMd:               pgtype.Text{String: "plan", Valid: true},
		ID:                   runID,
		WorkerID:             pgtype.UUID{Bytes: workerID, Valid: true},
		InferredCapabilities: caps,
		InferredTools:        tools,
		SizeClass:            sizeParam,
	})
	if err != nil {
		fx.t.Fatalf("SetRunAwaitingApproval: %v", err)
	}
	if rows != 1 {
		fx.t.Fatalf("SetRunAwaitingApproval affected %d rows, want 1", rows)
	}
}

// TestSetAwaitingRequirementMergeLiveDB proves the escalation-only merge: a run seeded
// with the repo hint {docker} that reports inferred capabilities {jvm}, tools {go,node}
// and size_class "m" ends with the UNION {docker,jvm} (deduped), tools {go,node} and
// size_class "m" (SET).
func TestSetAwaitingRequirementMergeLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	run := ownedRun(fx, w, []string{"docker"}) // M2 repo hint pre-seeded

	setAwaiting(fx, run, w, []string{"jvm"}, []string{"go", "node"}, "m")

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

	// A dedupe check: re-reporting an already-present capability adds nothing (union is
	// idempotent), a new tools set REPLACES the prior one, and a new size_class REPLACES.
	setAwaiting(fx, run, w, []string{"docker"}, []string{"rust"}, "l")
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

// TestSetAwaitingRequirementAbsentNoWipeLiveDB proves the absent-safe path: a run with
// established requirements that reports with BOTH params nil (the "worker sends the array
// only when non-empty" case, and the exact NOT-NULL-array pgx trap) leaves both columns
// unchanged rather than nulling them.
func TestSetAwaitingRequirementAbsentNoWipeLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	run := ownedRun(fx, w, []string{"docker"})

	// First establish a non-empty tools set (and the docker capability from the seed) and
	// a size_class "m".
	setAwaiting(fx, run, w, nil, []string{"go", "node"}, "m")
	caps, tools, size := runReqs(fx, run)
	if !sameSet(caps, []string{"docker"}) || !sameSet(tools, []string{"go", "node"}) || size != "m" {
		t.Fatalf("setup: caps=%v tools=%v size=%q, want {docker}, {go,node} and %q", caps, tools, size, "m")
	}

	// Now report with ALL three params absent — a nil text[] and an invalid pgtype.Text
	// both encode SQL NULL, so the COALESCE guards are what keep the NOT-NULL columns from
	// being wiped.
	setAwaiting(fx, run, w, nil, nil, "")
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

// sameSet reports whether a and b contain the same members (order-independent), the
// right comparison for the union-merged capability array which the query leaves unsorted.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}
