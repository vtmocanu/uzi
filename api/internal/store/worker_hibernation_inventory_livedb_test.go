package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4208 M1: every query over worker_hibernation_inventories (migration 00448), the batched
// retirement-evaluator reads and the poll select's inventory_status column, executed against a
// real database. Skipped without UZI_TEST_DATABASE_URL (run via e2e/run-store-it.sh).

type invFixture struct {
	*fleetFixture
	worker uuid.UUID
	op     uuid.UUID
	pod    uuid.UUID
}

func newInvFixture(t *testing.T) *invFixture {
	t.Helper()
	fx := newFleetFixture(t)
	// No cleanup of the user: recovery captures and their closures are RESTRICT-protected audit
	// rows, and the user, worker and run ids are fresh per test.
	w := fx.worker("inv", capOf(4), false)
	setHibernationSQL(fx, w, "preparing")
	var op uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx, `SELECT hibernation_op_id FROM workers WHERE id=$1`, w).Scan(&op); err != nil {
		t.Fatal(err)
	}
	return &invFixture{fleetFixture: fx, worker: w, op: op, pod: uuid.New()}
}

func invTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func (f *invFixture) observe(status string) store.WorkerHibernationInventory {
	f.t.Helper()
	pvcs, _ := json.Marshal([]map[string]string{{"role": "data", "name": "d", "uid": uuid.NewString(), "storage_class": "fast", "class_reclaim_policy": "Delete"}})
	row, err := f.q.UpsertHibernationInventoryObserved(f.ctx, store.UpsertHibernationInventoryObservedParams{
		WorkerID: f.worker, OpID: f.op, ObservedAt: invTS(time.Now()), DeploymentUid: "dep-uid",
		DeploymentResourceVersion: "42", PodUid: f.pod, Pvcs: pvcs, Status: status,
	})
	if err != nil {
		f.t.Fatalf("upsert observed: %v", err)
	}
	return row
}

func (f *invFixture) acceptParams(entries string) store.AcceptHibernationInventoryParams {
	return store.AcceptHibernationInventoryParams{
		WorkerID: f.worker, OpID: f.op, RegisterNonce: "nonce", ReceivedAt: invTS(time.Now()), Complete: true,
		Entries: []byte(entries), ReceiptKeys: []byte(`[]`), Digest: strings.Repeat("b", 64), Fingerprint: strings.Repeat("a", 64),
		Lane:      pgtype.Text{String: "dind", Valid: true},
		Mechanism: pgtype.Text{String: "epoch_ex+latch+quiescence", Valid: true},
	}
}

func TestHibernationInventoryLifecycleLiveDB(t *testing.T) {
	f := newInvFixture(t)

	// Upsert, then replace: a second observation for the same worker NULLs every inventory column.
	first := f.observe("awaiting")
	if first.Status != "awaiting" || first.Entries != nil || first.InvalidatedReason.Valid {
		t.Fatalf("fresh row = %+v", first)
	}
	if n, err := f.q.AcceptHibernationInventory(f.ctx, f.acceptParams(`[{"kind":"attempt_tree"}]`)); err != nil || n != 1 {
		t.Fatalf("first accept = %d, %v; want 1 row", n, err)
	}
	full, err := f.q.GetHibernationInventoryForOp(f.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.worker, OpID: f.op})
	if err != nil || full.Status != "accepted" || string(full.Entries) != `[{"kind": "attempt_tree"}]` || !full.Complete.Bool || full.Lane.String != "dind" {
		t.Fatalf("accepted row = %+v, %v", full, err)
	}
	// A second accept is refused (0 rows): the row is no longer awaiting.
	if n, err := f.q.AcceptHibernationInventory(f.ctx, f.acceptParams(`[]`)); err != nil || n != 0 {
		t.Fatalf("second accept = %d, %v; want 0 rows", n, err)
	}
	replaced := f.observe("awaiting")
	if replaced.Status != "awaiting" || replaced.Entries != nil || replaced.Digest.Valid || replaced.Fingerprint.Valid || replaced.RegisterNonce.Valid || replaced.ReceivedAt.Valid || replaced.Complete.Valid {
		t.Fatalf("replace did not NULL the inventory columns: %+v", replaced)
	}

	// The narrow gate row.
	gate, err := f.q.GetHibernationInventoryGateForOp(f.ctx, store.GetHibernationInventoryGateForOpParams{WorkerID: f.worker, OpID: f.op})
	if err != nil || gate.Status != "awaiting" || gate.PodUid != f.pod || gate.DeploymentResourceVersion != "42" || len(gate.Pvcs) == 0 || gate.InvalidatedReason.Valid {
		t.Fatalf("gate = %+v, %v", gate, err)
	}
	if _, err := f.q.GetHibernationInventoryGateForOp(f.ctx, store.GetHibernationInventoryGateForOpParams{WorkerID: f.worker, OpID: uuid.New()}); err == nil {
		t.Fatal("gate for another op returned a row")
	}

	// Invalidation is monotonic, and an invalidated awaiting row cannot be accepted.
	at1 := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	if n, err := f.q.InvalidateHibernationInventory(f.ctx, store.InvalidateHibernationInventoryParams{WorkerID: f.worker, OpID: f.op, Reason: "other_pod", At: invTS(at1)}); err != nil || n != 1 {
		t.Fatalf("invalidate = %d, %v", n, err)
	}
	if n, err := f.q.InvalidateHibernationInventory(f.ctx, store.InvalidateHibernationInventoryParams{WorkerID: f.worker, OpID: f.op, Reason: "exclusion_lost", At: invTS(time.Now())}); err != nil || n != 1 {
		t.Fatalf("second invalidate = %d, %v", n, err)
	}
	inv, _ := f.q.GetHibernationInventoryForOp(f.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.worker, OpID: f.op})
	if inv.InvalidatedReason.String != "other_pod" || !inv.InvalidatedAt.Time.Equal(at1) {
		t.Fatalf("invalidation overwritten: %q at %v", inv.InvalidatedReason.String, inv.InvalidatedAt.Time)
	}
	if n, err := f.q.AcceptHibernationInventory(f.ctx, f.acceptParams(`[]`)); err != nil || n != 0 {
		t.Fatalf("accept after invalidation = %d, %v; want 0 rows", n, err)
	}

	// Set-status moves only an awaiting row.
	f.observe("awaiting")
	if n, err := f.q.SetHibernationInventoryStatus(f.ctx, store.SetHibernationInventoryStatusParams{WorkerID: f.worker, OpID: f.op, Status: "busy", StatusReason: pgtype.Text{String: "run active", Valid: true}}); err != nil || n != 1 {
		t.Fatalf("set status = %d, %v", n, err)
	}
	if n, err := f.q.SetHibernationInventoryStatus(f.ctx, store.SetHibernationInventoryStatusParams{WorkerID: f.worker, OpID: f.op, Status: "timed_out"}); err != nil || n != 0 {
		t.Fatalf("set status from busy = %d, %v; want 0 rows", n, err)
	}
	got, _ := f.q.GetHibernationInventoryForOp(f.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.worker, OpID: f.op})
	if got.Status != "busy" || got.StatusReason.String != "run active" {
		t.Fatalf("status = %q reason %q", got.Status, got.StatusReason.String)
	}

	// Scale-down bookkeeping: the first pod uid and fence generation stick; held never moves back.
	f.observe("awaiting")
	podA, podB := uuid.New(), uuid.New()
	heldAt := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	scale := func(pod uuid.UUID, gen int64, held time.Time) {
		t.Helper()
		if n, err := f.q.RecordHibernationInventoryScaleDown(f.ctx, store.RecordHibernationInventoryScaleDownParams{
			WorkerID: f.worker, OpID: f.op, ScalingDownPodUid: pgtype.UUID{Bytes: pod, Valid: true},
			FenceGeneration: pgtype.Int8{Int64: gen, Valid: true}, LastHeldAt: invTS(held)}); err != nil || n != 1 {
			t.Fatalf("scale down = %d, %v", n, err)
		}
	}
	scale(podA, 5, heldAt)
	scale(podB, 6, heldAt.Add(time.Minute))
	got, _ = f.q.GetHibernationInventoryForOp(f.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.worker, OpID: f.op})
	if got.ScalingDownPodUid.Bytes != podA || got.FenceGeneration.Int64 != 5 || !got.LastHeldAt.Time.Equal(heldAt.Add(time.Minute)) {
		t.Fatalf("scale-down columns = %+v", got)
	}
	if n, err := f.q.RecordHibernationInventoryHeld(f.ctx, store.RecordHibernationInventoryHeldParams{WorkerID: f.worker, OpID: f.op, LastHeldAt: invTS(heldAt)}); err != nil || n != 1 {
		t.Fatalf("held = %d, %v", n, err)
	}
	got, _ = f.q.GetHibernationInventoryForOp(f.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.worker, OpID: f.op})
	if !got.LastHeldAt.Time.Equal(heldAt.Add(time.Minute)) {
		t.Fatalf("held moved last_held_at backwards: %v", got.LastHeldAt.Time)
	}
	scaledAt := time.Now().Truncate(time.Microsecond)
	if n, err := f.q.RecordHibernationInventoryScaledDown(f.ctx, store.RecordHibernationInventoryScaledDownParams{WorkerID: f.worker, OpID: f.op, ScaledDownAt: invTS(scaledAt)}); err != nil || n != 1 {
		t.Fatalf("scaled down = %d, %v", n, err)
	}
	if n, err := f.q.RecordHibernationExclusionFinal(f.ctx, store.RecordHibernationExclusionFinalParams{WorkerID: f.worker, OpID: f.op, At: invTS(scaledAt), Digest: strings.Repeat("b", 64), Fingerprint: strings.Repeat("a", 64)}); err != nil || n != 1 {
		t.Fatalf("exclusion final = %d, %v", n, err)
	}
	got, _ = f.q.GetHibernationInventoryForOp(f.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.worker, OpID: f.op})
	if !got.ScaledDownAt.Time.Equal(scaledAt) || !got.LastHeldAt.Time.Equal(heldAt.Add(time.Minute)) || got.ExclusionFinalDigest.String != strings.Repeat("b", 64) || !got.ExclusionFinalAt.Valid {
		t.Fatalf("scaled-down / final columns = %+v", got)
	}

	// Another op's writes match nothing.
	if n, err := f.q.RecordHibernationInventoryHeld(f.ctx, store.RecordHibernationInventoryHeldParams{WorkerID: f.worker, OpID: uuid.New(), LastHeldAt: invTS(time.Now())}); err != nil || n != 0 {
		t.Fatalf("held for another op = %d, %v; want 0", n, err)
	}
}

func TestHibernationInventoryChecksLiveDB(t *testing.T) {
	f := newInvFixture(t)
	f.observe("awaiting")
	violation := func(name string, err error) {
		t.Helper()
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" {
			t.Fatalf("%s: err = %v, want a 23514 check violation", name, err)
		}
	}
	// An over-1MiB entries array is refused by the CHECK.
	big := `["` + strings.Repeat("x", 1<<20) + `"]`
	_, err := f.q.AcceptHibernationInventory(f.ctx, f.acceptParams(big))
	violation("oversized entries", err)
	// A non-array entries document is refused.
	_, err = f.q.AcceptHibernationInventory(f.ctx, f.acceptParams(`{"a":1}`))
	violation("object entries", err)
	// An incoherent accepted row (no digest) is refused, and so is an unpaired invalidation.
	_, err = f.pool.Exec(f.ctx, `UPDATE worker_hibernation_inventories SET status='accepted' WHERE worker_id=$1`, f.worker)
	violation("incoherent accepted row", err)
	_, err = f.pool.Exec(f.ctx, `UPDATE worker_hibernation_inventories SET invalidated_reason='other_pod' WHERE worker_id=$1`, f.worker)
	violation("reason without timestamp", err)
	_, err = f.q.InvalidateHibernationInventory(f.ctx, store.InvalidateHibernationInventoryParams{WorkerID: f.worker, OpID: f.op, Reason: "not_a_reason", At: invTS(time.Now())})
	violation("unknown invalidation reason", err)
	// The row still accepts a valid inventory after every refused write.
	if n, err := f.q.AcceptHibernationInventory(f.ctx, f.acceptParams(`[]`)); err != nil || n != 1 {
		t.Fatalf("valid accept after refusals = %d, %v", n, err)
	}
}

func TestHibernationInventoryPollSelectLiveDB(t *testing.T) {
	f := newInvFixture(t)
	list := func() *store.ListHostedWorkersForControllerRow {
		t.Helper()
		rows, err := f.q.ListHostedWorkersForController(f.ctx, store.ListHostedWorkersForControllerParams{
			DiskPressureMinStreak: 2, HeartbeatCutoff: invTS(time.Now().Add(-time.Minute))})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			if rows[i].ID == f.worker {
				return &rows[i]
			}
		}
		t.Fatal("worker missing from the poll select")
		return nil
	}
	if r := list(); r.InventoryStatus.Valid {
		t.Fatalf("no inventory row yet, inventory_status = %q", r.InventoryStatus.String)
	}
	f.observe("awaiting")
	if r := list(); r.InventoryStatus.String != "awaiting" {
		t.Fatalf("inventory_status = %+v, want awaiting", r.InventoryStatus)
	}
	// An inventory for a previous op does not surface for the current one.
	mustExec(f.ctx, t, f.pool, `UPDATE worker_hibernation_inventories SET op_id=gen_random_uuid() WHERE worker_id=$1`, f.worker)
	if r := list(); r.InventoryStatus.Valid {
		t.Fatalf("a stale op's inventory leaked into the poll: %q", r.InventoryStatus.String)
	}
}

func TestHibernationInventorySettleAndCandidatesLiveDB(t *testing.T) {
	f := newInvFixture(t)
	f.observe("awaiting")
	// The stale-awaiting list: older than the cutoff only, ordered by worker id.
	ids, err := f.q.ListAwaitingHibernationInventoriesOlderThan(f.ctx, invTS(time.Now().Add(-time.Hour)))
	if err != nil || containsUUID(ids, f.worker) {
		t.Fatalf("fresh row listed as stale: %v, %v", ids, err)
	}
	ids, err = f.q.ListAwaitingHibernationInventoriesOlderThan(f.ctx, invTS(time.Now().Add(time.Hour)))
	if err != nil || !containsUUID(ids, f.worker) {
		t.Fatalf("stale row not listed: %v, %v", ids, err)
	}
	if n, err := f.q.SetHibernationInventoryStatus(f.ctx, store.SetHibernationInventoryStatusParams{WorkerID: f.worker, OpID: f.op, Status: "timed_out"}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	ids, _ = f.q.ListAwaitingHibernationInventoriesOlderThan(f.ctx, invTS(time.Now().Add(time.Hour)))
	if containsUUID(ids, f.worker) {
		t.Fatal("a settled row is still listed as awaiting")
	}

	// Candidates: hosted, non-ephemeral, hibernated or waking only.
	asleep := f.worker
	mustExec(f.ctx, t, f.pool, `UPDATE workers SET hibernation_state='hibernated', hibernated_at=now()-interval '2 days', docker_enabled=true WHERE id=$1`, asleep)
	older := f.sleeper("hibernated", "5 days")
	f.sleeper("waking", "1 day")
	f.sleeper("preparing", "1 day")
	before, err := f.q.CountHibernationRetirementCandidates(f.ctx)
	if err != nil || before < 3 {
		t.Fatalf("candidate count = %d, %v; want at least the 3 fixtures", before, err)
	}
	rows, err := f.q.ListHibernationRetirementCandidates(f.ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mine := map[uuid.UUID]int{}
	for i, r := range rows {
		mine[r.ID] = i
		if r.HibernationState != "hibernated" && r.HibernationState != "waking" {
			t.Fatalf("non-candidate state listed: %+v", r)
		}
		if r.ID == asleep && (r.UserID != f.userID || r.Kind != "hosted" || !r.DockerEnabled.Bool || !r.HibernatedAt.Valid || !r.HibernationOpID.Valid) {
			t.Fatalf("candidate row = %+v", r)
		}
	}
	if len(mine) != len(rows) || int64(len(rows)) != before {
		t.Fatalf("count %d disagrees with the list of %d", before, len(rows))
	}
	if _, ok := mine[asleep]; !ok {
		t.Fatal("hibernated fixture missing")
	}
	if mine[older] >= mine[asleep] {
		t.Fatalf("order: older hibernated_at must sort first (older at %d, newer at %d)", mine[older], mine[asleep])
	}
	if lim, _ := f.q.ListHibernationRetirementCandidates(f.ctx, 1); len(lim) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(lim))
	}
}

func (fx *invFixture) sleeper(state, age string) uuid.UUID {
	w := fx.fleetFixture.worker("c", capOf(4), false)
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base',
		hibernation_state=$2, hibernation_op_id=gen_random_uuid(), hibernation_requested_at=now(),
		hibernated_at=now()-$3::interval, wake_requested_at=now() WHERE id=$1`, w, state, age)
	return w
}

func containsUUID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func TestHibernationInventoryEvaluatorReadsLiveDB(t *testing.T) {
	f := newInvFixture(t)
	other := f.fleetFixture.worker("other", capOf(4), false)
	both := []uuid.UUID{f.worker, other}
	only := func(name string, got []uuid.UUID, err error, want uuid.UUID) {
		t.Helper()
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %v, %v; want exactly [%s]", name, got, err, want)
		}
	}

	// Non-terminal runs: only the worker holding a non-terminal run is named.
	run := f.queuedRun()
	mustExec(f.ctx, t, f.pool, `UPDATE runs SET worker_id=$2 WHERE id=$1`, run, f.worker)
	got, err := f.q.ListWorkersWithNonTerminalRuns(f.ctx, both)
	only("non-terminal runs", got, err, f.worker)
	mustExec(f.ctx, t, f.pool, `UPDATE runs SET status='completed' WHERE id=$1`, run)
	if got, err := f.q.ListWorkersWithNonTerminalRuns(f.ctx, both); err != nil || len(got) != 0 {
		t.Fatalf("terminal run still counted: %v, %v", got, err)
	}

	// Open custody hold plus a capture that names the other worker as its local replica.
	hold := uuid.New()
	mustExec(f.ctx, t, f.pool, `INSERT INTO recovery_custody_holds (id,user_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id)
		VALUES ($1,$2,$3,1,'open',$4,'fixture',$4)`, hold, f.userID, run, f.worker)
	got, err = f.q.ListWorkersWithOpenCustodyHolds(f.ctx, both)
	only("open custody holds", got, err, f.worker)

	sha := strings.Repeat("c", 40)
	capture := func(source string, replica any, expires string, bound bool, state string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(f.ctx, t, f.pool, `INSERT INTO recovery_captures
			(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,local_replica_worker_id,expires_at)
			VALUES($1,$2,$3,$4,$5,'fixture',$6,$7,$8,$9,$10,now()+$11::interval)`,
			id, hold, run, f.userID, f.worker, source, uuid.NewString(), state, bound, replica, expires)
		return id
	}
	// A local replica row needs the retention columns the 00322 trigger checks; try the plain form.
	rep := capture(sha, other, "1 hour", true, "available")
	_ = rep
	got, err = f.q.ListWorkersWithLocalReplicaCaptures(f.ctx, both)
	only("local replica captures", got, err, other)

	// ListValidCaptureReceipts: matching (run, sha) pairs only, with the available/bound/unexpired filter.
	valid := f.run2()
	mustExec(f.ctx, t, f.pool, `UPDATE runs SET status='completed' WHERE id=$1`, valid)
	capFor := func(r uuid.UUID, source, state, expires string, bound bool, prereq any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		h := uuid.New()
		mustExec(f.ctx, t, f.pool, `INSERT INTO recovery_custody_holds (id,user_id,run_id,generation,state,original_worker_id,original_worker_identity)
			VALUES ($1,$2,$3,1,'open',$4,'fixture')`, h, f.userID, r, uuid.New())
		mustExec(f.ctx, t, f.pool, `INSERT INTO recovery_captures
			(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,expires_at,prerequisite_shas)
			VALUES($1,$2,$3,$4,'fixture',$5,$6,$7,$8,now()+$9::interval,$10)`, id, h, r, f.userID, source, uuid.NewString(), state, bound, expires, prereq)
		return id
	}
	shaOK, shaOther := strings.Repeat("1", 40), strings.Repeat("2", 40)
	capFor(valid, shaOK, "available", "1 hour", true, nil)
	runExpired, runUnbound, runPending, runPrereq, runPrereqPinned := f.run2(), f.run2(), f.run2(), f.run2(), f.run2()
	capFor(runExpired, shaOK, "available", "-1 hour", true, nil)
	capFor(runUnbound, shaOK, "available", "1 hour", false, nil)
	capFor(runPending, shaOK, "uploading", "1 hour", true, nil)
	capFor(runPrereq, shaOK, "available", "1 hour", true, []string{strings.Repeat("9", 40)})
	pinnedCapture := capFor(runPrereqPinned, shaOK, "available", "1 hour", true, []string{strings.Repeat("9", 40)})
	f.pinClosure(pinnedCapture, runPrereqPinned, strings.Repeat("9", 40))
	runs := []uuid.UUID{valid, valid, runExpired, runUnbound, runPending, runPrereq, runPrereqPinned}
	shas := []string{shaOK, shaOther, shaOK, shaOK, shaOK, shaOK, shaOK}
	pairs, err := f.q.ListValidCaptureReceipts(f.ctx, store.ListValidCaptureReceiptsParams{RunIds: runs, SourceShas: shas})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]bool{valid: true, runPrereqPinned: true}
	if len(pairs) != len(want) {
		t.Fatalf("receipts = %+v, want exactly runs %v", pairs, want)
	}
	for _, p := range pairs {
		if !want[p.RunID] || p.SourceSha != shaOK {
			t.Fatalf("unexpected receipt %+v", p)
		}
	}
}

func (f *invFixture) run2() uuid.UUID { return f.queuedRun() }

// pinClosure inserts a pinned prerequisite closure for a capture through the same state the
// production writer ends in; the guard trigger only allows a pending insert, so it is created
// pending and walked to pinned with the trigger bypassed for the fixture.
func (f *invFixture) pinClosure(capture, run uuid.UUID, prereq string) {
	f.t.Helper()
	var hold uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT hold_id FROM recovery_captures WHERE id=$1`, capture).Scan(&hold); err != nil {
		f.t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, sql := range []string{
		`SET LOCAL session_replication_role = replica`,
	} {
		if _, err := tx.Exec(f.ctx, sql); err != nil {
			f.t.Fatalf("cannot bypass the closure guard trigger (needs a superuser test database): %v", err)
		}
	}
	if _, err := tx.Exec(f.ctx, `INSERT INTO recovery_prerequisite_closures
		(capture_id,hold_id,run_id,user_id,repo_id,live_run_id,checksum,prerequisite_shas,pin_ref,state,invoked_at,invoked_deadline,pin_tip,connection_id,forge_kind,normalized_base_url,project_id,default_branch)
		VALUES ($1,$2,$3,$4,$5,$3,repeat('a',64),$6,'refs/uzi-prereq/'||$8::text,'pinned',now(),now()+interval '1 hour',repeat('b',40),$7,'gitlab','https://forge.e2e',1,'main')`,
		capture, hold, run, f.userID, f.repoID, []string{prereq}, uuid.New(), capture.String()); err != nil {
		f.t.Fatalf("insert pinned closure: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func TestHibernationInventoryPublicationResumeLiveDB(t *testing.T) {
	f := newInvFixture(t)
	other := f.fleetFixture.worker("other", capOf(4), false)
	run := f.queuedRun()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(f.ctx, `SELECT set_config('uzi.publication_resume_api','v1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state)
		VALUES ($1, 1, 1, $2, json_build_object('run_id', $3::text), 'active')`, run, f.worker, run.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.ListWorkersWithActivePublicationResumeCustody(f.ctx, []uuid.UUID{f.worker, other})
	if err != nil || len(got) != 1 || got[0] != f.worker {
		t.Fatalf("publication resume custody = %v, %v; want exactly [%s]", got, err, f.worker)
	}
	retire, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retire.Rollback(context.Background()) }()
	if _, err := retire.Exec(f.ctx, `SELECT set_config('uzi.publication_resume_api','v1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := retire.Exec(f.ctx, `UPDATE run_publication_resume SET state='retired', retired_reason='fixture' WHERE run_id=$1`, run); err != nil {
		t.Fatal(err)
	}
	if err := retire.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := f.q.ListWorkersWithActivePublicationResumeCustody(f.ctx, []uuid.UUID{f.worker, other}); err != nil || len(got) != 0 {
		t.Fatalf("a retired enrollment still counts: %v, %v", got, err)
	}
}
