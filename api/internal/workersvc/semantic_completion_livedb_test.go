package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Main executes these tests in the serial LiveDB sweep. No production enrollment
// writer is used: only this test injects a semantic contract into the existing row.
func TestSemanticCompletionOwnerAuthorizationReplayLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	fixture := semanticCompletionTwoMaintainers(t)
	e.exec(t, "UPDATE runs SET completion_contract=$2, milestones_frozen=$3 WHERE id=$1", id, fixture.CompletionContract, fixture.MilestonesFrozen)
	e.parkCompletionBlocked(t, id)
	e.seedPermit(t, id, wid, 1, "old-head")
	c, _ := parseCompletionContract(fixture.CompletionContract)
	dec := CompletionDecisionInput{Decision: "authorize_maintainer", Maintainers: semanticMaintainers(c)[:1], Reason: "authorize frozen owner steps", ContractRevision: 1}
	foreignAdmin := uuid.New()
	e.exec(t, "INSERT INTO users (id,email,password_hash,is_admin) VALUES ($1,$2,'hash',true)", foreignAdmin, "m4-"+foreignAdmin.String()+"@example.com")
	trackLiveDBUser(t, e.pool, foreignAdmin)
	if _, err := svc.DecideCompletion(e.ctx, foreignAdmin, id, dec); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign admin: %v", err)
	}
	run, err := svc.DecideCompletion(e.ctx, e.userID, id, dec)
	if err != nil || run.ContractRevision.Int32 != 2 {
		t.Fatalf("decision revision=%v err=%v", run.ContractRevision, err)
	}
	if !e.permitConsumed(t, id, 1, "old-head") {
		t.Fatal("old permit survived authorization revision")
	}
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, dec); err != nil {
		t.Fatal(err)
	}
	if n := e.countInputs(t, id, "completion_decision"); n != 1 {
		t.Fatalf("replay wrote %d decisions", n)
	}
	raw, rev := e.readContract(t, id)
	if rev == nil || *rev != 2 {
		t.Fatal("replay bumped revision again")
	}
	run.CompletionContract = raw
	pending, err := CompletionPending(run)
	if err != nil || len(pending.Criteria) != 2 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	for _, criterion := range pending.Criteria {
		want := 1
		if criterion.CriterionID == dec.Maintainers[0].ID {
			want = 2
		}
		if criterion.AuthorizationRevision != want || criterion.Status != "unverified" {
			t.Fatalf("selected authorization=%+v want revision %d", criterion, want)
		}
	}
	all := dec
	all.Maintainers = semanticMaintainers(c)
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, all); !errors.Is(err, ErrCompletionRevisionConflict) {
		t.Fatalf("all-authorizations falsely replayed selected decision: %v", err)
	}
	dec.Maintainers[0].Conditions = []string{"different"}
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, dec); !errors.Is(err, ErrCompletionRevisionConflict) {
		t.Fatalf("changed replay: %v", err)
	}
}

// TestSemanticCompletionSingleConnectionReplayLiveDB cancels only a nested
// acquire while the transaction holds the pool's sole connection.
func TestSemanticCompletionSingleConnectionReplayLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	fixture := semanticCompletionTwoMaintainers(t)
	e.exec(t, "UPDATE runs SET completion_contract=$2, milestones_frozen=$3 WHERE id=$1", id, fixture.CompletionContract, fixture.MilestonesFrozen)
	e.parkCompletionBlocked(t, id)
	c, err := parseCompletionContract(fixture.CompletionContract)
	if err != nil {
		t.Fatal(err)
	}
	dec := CompletionDecisionInput{Decision: "authorize_maintainer", Maintainers: semanticMaintainers(c)[:1], Reason: "renew selected", ContractRevision: 1}
	applied, err := e.permitService(t).DecideCompletion(e.ctx, e.userID, id, dec)
	if err != nil {
		t.Fatal(err)
	}

	cfg := e.pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	guard := &semanticReplayAcquireGuard{}
	cfg.ConnConfig.Tracer = guard
	pool, err := pgxpool.NewWithConfig(e.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Prove the guard cancels a second acquire from this same single-connection
	// pool before relying on its zero count during replay.
	extra, acquireErr := pool.Acquire(e.ctx)
	if extra != nil {
		extra.Release()
	}
	held.Release()
	if !errors.Is(acquireErr, context.Canceled) || guard.nested.Load() != 1 {
		t.Fatalf("guard control: acquire=%v nested=%d", acquireErr, guard.nested.Load())
	}
	guard.nested.Store(0)
	single := e
	single.pool, single.q = pool, store.New(pool)
	replayed, err := single.permitService(t).DecideCompletion(e.ctx, e.userID, id, dec)
	if err != nil {
		t.Fatalf("replay: %v; nested acquires=%d", err, guard.nested.Load())
	}
	if guard.nested.Load() != 0 {
		t.Fatal("replay borrowed another pool connection")
	}
	if replayed.ID != applied.ID || replayed.ContractRevision != applied.ContractRevision || replayed.Status != applied.Status {
		t.Fatalf("replay row changed: %+v", replayed)
	}
	if n := e.countInputs(t, id, "completion_decision"); n != 1 {
		t.Fatalf("replay wrote %d decisions", n)
	}
}

type semanticReplayAcquireGuard struct{ nested atomic.Int32 }

func (*semanticReplayAcquireGuard) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*semanticReplayAcquireGuard) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (g *semanticReplayAcquireGuard) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if pool.Stat().AcquiredConns() > 0 {
		g.nested.Add(1)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled
	}
	return ctx
}
func (*semanticReplayAcquireGuard) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func TestSemanticCompletionPauseFailedParentTransactionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	fixture := semanticCompletionFixture(t, false)
	e.exec(t, "UPDATE runs SET completion_contract=$2, milestones_frozen=$3, pause_requested_at=now(), pause_mode='now' WHERE id=$1", id, fixture.CompletionContract, fixture.MilestonesFrozen)
	run, applied, _, err := e.permitService(t).SetStateReport(e.ctx, store.Worker{ID: wid, UserID: e.userID}, id, StateRequest{State: "pause_failed"})
	if err != nil || !applied || run.Status != "running" || run.PauseRequestedAt.Valid {
		t.Fatalf("parent transaction: status=%s applied=%v pause=%v err=%v", run.Status, applied, run.PauseRequestedAt.Valid, err)
	}
	persisted, err := e.q.GetRunByID(e.ctx, id)
	if err != nil || persisted.PauseRequestedAt.Valid {
		t.Fatalf("pause clear did not commit: %v", err)
	}
}

func TestSemanticCompletionPendingPermitLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, nil, false)
	fixture := semanticCompletionFixture(t, false)
	e.exec(t, "UPDATE runs SET completion_contract=$2, milestones_frozen=$3 WHERE id=$1", id, fixture.CompletionContract, fixture.MilestonesFrozen)
	out, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: wid, UserID: e.userID}, id, CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: "exact-head"})
	if err != nil || !out.Granted || out.Permit.CompletionPending == nil {
		t.Fatalf("permit=%+v err=%v", out, err)
	}
	run, err := e.q.GetRunByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := DecodeMilestoneIDs(run.MilestonesCompleted)
	if err != nil || len(completed) != 0 {
		t.Fatalf("pending marked milestone done: %v %v", completed, err)
	}
	// The completion transaction stamps the existing nonclosing scope-reduced
	// outcome; the pending criterion stays unverified in the same contract.
	_, applied, _, err := svc.SetStateReport(e.ctx, store.Worker{ID: wid, UserID: e.userID}, id, StateRequest{State: "completed", Branch: strPtr("agent/issue-1"), Head: strPtr("exact-head")})
	if err != nil || !applied {
		t.Fatalf("completion applied=%v err=%v", applied, err)
	}
	run, err = e.q.GetRunByID(e.ctx, id)
	if err != nil || run.StopKind.String != "scope_reduced" {
		t.Fatalf("nonclosing stop=%v err=%v", run.StopKind, err)
	}
	pending, err := CompletionPending(run)
	if err != nil || pending.Criteria[0].Status != "unverified" {
		t.Fatalf("completion verified pending: %+v %v", pending, err)
	}
}

// TestSemanticReportOnlyOwnerPartialDeliveryLiveDB: after a real owner partial decision on a
// semantic contract (keep m1, defer m2), a headless report-only completion of m1 is delivered.
func TestSemanticReportOnlyOwnerPartialDeliveryLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, nil, false)
	fixture := semanticCompletionFixture(t, false)
	e.exec(t, "UPDATE runs SET completion_contract=$2 WHERE id=$1", id, fixture.CompletionContract)
	e.parkCompletionBlocked(t, id)
	if _, err := svc.DecideCompletion(e.ctx, e.userID, id, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1"}, Reason: "ship m1 only; defer m2", ContractRevision: 1,
	}); err != nil {
		t.Fatalf("DecideCompletion partial: %v", err)
	}
	e.exec(t, "UPDATE runs SET status='running', worker_id=$2, milestones_completed='[\"m1\"]', claim_generation=1 WHERE id=$1", id, wid)
	w := store.Worker{ID: wid, UserID: e.userID}

	mixed := reportOnlyRequest()
	mixed.Head, mixed.Branch = strPtr("h"), strPtr("b")
	before := reportOnlySnapshot(t, e, id)
	if _, ok, err := svc.SetState(e.ctx, w, id, mixed); ok || !errors.Is(err, ErrInvalidState) {
		t.Fatalf("mixed request: applied=%v err=%v", ok, err)
	}
	if !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatal("mixed refusal mutated run")
	}
	stale := reportOnlyRequest()
	two := int64(2)
	stale.ClaimGeneration = &two
	if _, ok, err := svc.SetState(e.ctx, w, id, stale); ok || !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation: applied=%v err=%v", ok, err)
	}
	if !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatal("stale refusal mutated run")
	}

	req := reportOnlyRequest()
	run, applied, err := svc.SetState(e.ctx, w, id, req)
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("semantic owner partial delivery: applied=%v status=%s err=%v", applied, run.Status, err)
	}
	var stopKind string
	var done []byte
	if err := e.pool.QueryRow(e.ctx, "SELECT stop_kind, milestones_completed FROM runs WHERE id=$1", id).Scan(&stopKind, &done); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(done, &ids); err != nil || !slices.Equal(ids, []string{"m1"}) || stopKind != "scope_reduced" {
		t.Fatalf("stop_kind=%q milestones_completed=%s err=%v", stopKind, done, err)
	}
	before = reportOnlySnapshot(t, e, id)
	if _, applied, err = svc.SetState(e.ctx, w, id, req); err != nil || !applied {
		t.Fatalf("identical retry: %v %v", applied, err)
	}
	if !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatal("identical retry changed row")
	}
}
