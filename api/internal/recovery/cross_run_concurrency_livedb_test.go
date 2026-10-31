package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// crossRunRace owns all participants. Cleanup opens every barrier before
// cancelling and joining operations, then closes their pools. No sibling failure
// prevents another participant from draining; each operation has eight seconds.
type crossRunRace struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	gates    []*crossRunBarrier
	pools    []*pgxpool.Pool
	done     []<-chan struct{}
	releases []func()
}

func newCrossRunRace(t *testing.T) *crossRunRace {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	r := &crossRunRace{t: t, ctx: ctx, cancel: cancel}
	t.Cleanup(func() {
		for _, g := range r.gates {
			g.release()
		}
		for _, f := range r.releases {
			f()
		}
		cancel()
		for _, done := range r.done {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("cross-run participant failed to join")
			}
		}
		for _, p := range r.pools {
			p.Close()
		}
	})
	return r
}

type crossRunBarrier struct {
	entered chan int32
	proceed chan struct{}
	once    sync.Once
}

func (r *crossRunRace) barrier() *crossRunBarrier {
	g := &crossRunBarrier{entered: make(chan int32, 1), proceed: make(chan struct{})}
	r.gates = append(r.gates, g)
	return g
}
func (g *crossRunBarrier) release() { g.once.Do(func() { close(g.proceed) }) }
func (g *crossRunBarrier) pause(ctx context.Context, tx pgx.Tx) error {
	var pid int32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()::int").Scan(&pid); err != nil {
		return err
	}
	select {
	case g.entered <- pid:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (g *crossRunBarrier) await(t *testing.T) int32 {
	t.Helper()
	select {
	case pid := <-g.entered:
		return pid
	case <-time.After(5 * time.Second):
		t.Fatal("transaction did not reach barrier")
		return 0
	}
}

type crossRunTraceDB struct {
	DB
	commit, rollback *crossRunBarrier
	pid              atomic.Int32
}
type crossRunTraceTx struct {
	pgx.Tx
	commit, rollback *crossRunBarrier
	committed        bool
}

func (d *crossRunTraceDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return d.trace(ctx, tx)
}
func (d *crossRunTraceDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.DB.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return d.trace(ctx, tx)
}
func (d *crossRunTraceDB) trace(ctx context.Context, tx pgx.Tx) (pgx.Tx, error) {
	var pid int32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()::int").Scan(&pid); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	d.pid.Store(pid)
	return &crossRunTraceTx{Tx: tx, commit: d.commit, rollback: d.rollback}, nil
}
func (tx *crossRunTraceTx) Commit(ctx context.Context) error {
	if tx.commit != nil {
		if err := tx.commit.pause(ctx, tx.Tx); err != nil {
			return err
		}
	}
	err := tx.Tx.Commit(ctx)
	tx.committed = err == nil
	return err
}
func (tx *crossRunTraceTx) Rollback(ctx context.Context) error {
	if !tx.committed && tx.rollback != nil {
		// A cancelled barrier must still release the underlying database locks.
		if err := tx.rollback.pause(ctx, tx.Tx); err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = tx.Tx.Rollback(cleanup)
			return err
		}
	}
	return tx.Tx.Rollback(ctx)
}
func (r *crossRunRace) service(e *inventoryEnv, commit, rollback *crossRunBarrier) (*Service, *crossRunTraceDB, string) {
	r.t.Helper()
	cfg := e.pool.Config().Copy()
	name := "cross-run-" + uuid.NewString()
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	pool, err := pgxpool.NewWithConfig(r.ctx, cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	r.pools = append(r.pools, pool)
	db := &crossRunTraceDB{DB: pool, commit: commit, rollback: rollback}
	return New(e.q, db, e.svc.box, e.svc.limits, nil), db, name
}
func crossRunStart[T any](r *crossRunRace, fn func(context.Context) (T, error)) <-chan reconcileOperation[T] {
	result := make(chan reconcileOperation[T], 1)
	done := make(chan struct{})
	r.done = append(r.done, done)
	go func() { defer close(done); v, err := fn(r.ctx); result <- reconcileOperation[T]{v, err} }()
	return result
}

// waitCrossRunBlock attributes a lock edge to a unique operation AND the true
// blocking backend, never a query substring. Its dedicated observer and polling
// share one five-second deadline. Polls only observe; there are no retries of work.
func waitCrossRunBlock(t *testing.T, e *inventoryEnv, name string, blocker int32) int32 {
	t.Helper()
	if blocker <= 0 {
		t.Fatal("missing actual blocker PID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observer, err := pgx.ConnectConfig(ctx, e.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = observer.Close(closeCtx)
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int32
		err = observer.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
   WHERE datname=current_database() AND application_name=$1 AND wait_event_type='Lock'
   AND $2::int=ANY(pg_blocking_pids(pid))`, name, blocker).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("no lock edge %s -> %d: %v", name, blocker, ctx.Err())
		}
	}
}
func crossRunWantDiscard(t *testing.T, ch <-chan reconcileOperation[int64], want int64) {
	t.Helper()
	got := awaitReconcileOperation(t, ch)
	if got.err != nil || got.value != want {
		t.Fatalf("policy changed=%d err=%v want=%d", got.value, got.err, want)
	}
}
func crossRunWantRefusal(t *testing.T, ch <-chan reconcileOperation[int64]) {
	t.Helper()
	got := awaitReconcileOperation(t, ch)
	if got.value != 0 || !errors.Is(got.err, ErrMergeSnapshotMismatch) {
		t.Fatalf("policy changed=%d err=%v", got.value, got.err)
	}
}

func TestCrossRunDiscardConcurrencyLiveDB(t *testing.T) {
	crossRunExtraGroups(t)
	for _, first := range []bool{true, false} {
		order := map[bool]string{true: "writer_first", false: "discard_first"}[first]
		for _, kind := range []string{"upload", "reserve", "owner"} {
			t.Run(kind+"/"+order, func(t *testing.T) { crossRunStorageRace(t, kind, first) })
		}
		for _, kind := range []string{"insert", "invoke", "invoked", "create_unknown"} {
			t.Run("publication_"+kind+"/"+order, func(t *testing.T) { crossRunPublicationRace(t, kind, first) })
		}
	}
}

func crossRunStorageRace(t *testing.T, kind string, writerFirst bool) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	a, s := crossRunInputs(t, e, w, b)
	id := e.reserve("race")
	// A ready sibling must keep its bytes and TTL through every settlement.
	ready := e.reserve("retained")
	body := e.upload(ready)
	retained, err := mergedCapture(t, e, ready)
	if err != nil {
		t.Fatal(err)
	}
	witnessBefore := crossRunRow(t, e, "runs", w)
	r := newCrossRunRace(t)
	gate := r.barrier()
	var writerGate, policyGate *crossRunBarrier
	if writerFirst && kind != "upload" {
		writerGate = gate
	}
	if !writerFirst {
		policyGate = gate
	}
	policy, _, policyName := r.service(e, policyGate, nil)
	writer, writerDB, writerName := r.service(e, writerGate, nil)
	uploadedAt := time.Now().UTC().Truncate(time.Microsecond)
	writer.now = func() time.Time { return uploadedAt }
	var policyHooks, writerHooks atomic.Int32
	policy.SetCustodySettledHook(func(run uuid.UUID) {
		if run != e.run {
			t.Error("foreign policy hook")
		}
		policyHooks.Add(1)
	})
	writer.SetCustodySettledHook(func(run uuid.UUID) {
		if run != e.run {
			t.Error("foreign owner hook")
		}
		writerHooks.Add(1)
	})
	streamBody := []byte("cross-run streaming bytes")
	manifest := manifestOf(streamBody)
	manifest.ChunkCount = 1
	var reader *mergedBlockingReader
	if writerFirst && kind == "upload" {
		reader = newMergedBlockingReader(t, streamBody)
		r.releases = append(r.releases, reader.release)
	}
	write := func(ctx context.Context) (string, error) {
		switch kind {
		case "upload":
			if reader != nil {
				v, err := writer.Upload(ctx, e.w, e.run, id, manifest, reader)
				return v.State, err
			}
			v, err := writer.Upload(ctx, e.w, e.run, id, manifest, bytes.NewReader(streamBody))
			return v.State, err
		case "reserve":
			v, err := writer.Reserve(ctx, e.w, e.run, apitypes.RecoveryReserveRequest{Generation: &e.gen, IdempotencyKey: "race", SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)})
			return v.CaptureID, err
		default:
			v, err := writer.DiscardHold(ctx, e.w.UserID, e.run, e.hold)
			return fmt.Sprint(v), err
		}
	}
	discard := func(ctx context.Context) (int64, error) { return policy.DiscardCrossRunHolds(ctx, a, s) }
	var wc <-chan reconcileOperation[string]
	var pc <-chan reconcileOperation[int64]
	if writerFirst {
		wc = crossRunStart(r, write)
		var pid int32
		if reader != nil {
			reader.await(t)
			pid = writerDB.pid.Load()
		} else {
			pid = gate.await(t)
		}
		pc = crossRunStart(r, discard)
		waitCrossRunBlock(t, e, policyName, pid)
		if reader != nil {
			reader.release()
		} else {
			gate.release()
		}
	} else {
		pc = crossRunStart(r, discard)
		pid := gate.await(t)
		wc = crossRunStart(r, write)
		waitCrossRunBlock(t, e, writerName, pid)
		gate.release()
	}
	got := awaitReconcileOperation(t, wc)
	wantChanged := int64(1)
	if kind == "owner" && writerFirst {
		wantChanged = 0
	}
	crossRunWantDiscard(t, pc, wantChanged)
	if writerFirst {
		if got.err != nil {
			t.Fatalf("writer: %v", got.err)
		}
		if kind == "upload" && got.value != "available" {
			t.Fatalf("upload state=%s", got.value)
		}
		if kind == "reserve" && got.value != id.String() {
			t.Fatal("idempotent reserve changed identity")
		}
		if kind == "owner" && got.value != "true" {
			t.Fatal("owner did not win")
		}
	} else if kind == "owner" {
		if got.err != nil || got.value != "false" {
			t.Fatalf("late owner=%s err=%v", got.value, got.err)
		}
	} else if !errors.Is(got.err, ErrNotAuthorized) && !errors.Is(got.err, ErrNotAvailable) {
		t.Fatalf("late %s err=%v", kind, got.err)
	}
	cap, err := mergedCapture(t, e, id)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "upload" && writerFirst {
		if cap.State != "available" {
			t.Fatal("uploaded archive lost")
		}
		e.bytes(id, streamBody)
		// Lost upload ACK returns the same receipt only while the hold is open;
		// disposal itself must leave the archive record byte-for-byte intact.
		if !cap.ExpiresAt.Valid || !cap.ExpiresAt.Time.Equal(uploadedAt.Add(writer.limits.ReadyRetention)) {
			t.Fatal("uploaded archive retention changed")
		}
	} else if cap.State != "discarded" || len(chunks) != 0 {
		t.Fatalf("non-ready capture state=%s chunks=%d", cap.State, len(chunks))
	}
	after, err := mergedCapture(t, e, ready)
	if err != nil || !reflect.DeepEqual(retained, after) {
		t.Fatal("ready archive or TTL changed")
	}
	e.bytes(ready, body)
	if crossRunRow(t, e, "runs", w) != witnessBefore {
		t.Fatal("witness changed")
	}
	if policyHooks.Load() != int32(wantChanged) {
		t.Fatalf("policy hooks=%d", policyHooks.Load())
	}
	ownerWant := int32(0)
	if kind == "owner" && writerFirst {
		ownerWant = 1
	}
	if writerHooks.Load() != ownerWant {
		t.Fatalf("owner hooks=%d", writerHooks.Load())
	}
	var evidence string
	if err := e.pool.QueryRow(e.ctx, "SELECT release_evidence FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	expected := "merged_predecessor_discard"
	if ownerWant == 1 {
		expected = "owner_discard"
	}
	if evidence != expected {
		t.Fatalf("winner evidence=%s", evidence)
	}
	settled := crossRunRow(t, e, "recovery_custody_holds", e.hold)
	// Both lost-ACK replays are inert, including hooks.
	if n, err := policy.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 0 {
		t.Fatalf("policy replay=%d %v", n, err)
	}
	if changed, err := writer.DiscardHold(e.ctx, e.w.UserID, e.run, e.hold); err != nil || changed {
		t.Fatalf("owner replay=%v %v", changed, err)
	}
	if policyHooks.Load() != int32(wantChanged) || writerHooks.Load() != ownerWant {
		t.Fatal("replay emitted hook")
	}
	if settled != crossRunRow(t, e, "recovery_custody_holds", e.hold) {
		t.Fatal("lost ACK replay changed immutable evidence")
	}
}

// crossRunPublicationWriter executes heldInvoke's generated-query lock sequence,
// including the V2 terminal-custody eligibility recheck BEFORE the upsert. The
// insertion-only mode stops at prepared to expose that separately guarded state.
// No forge call or outcome is fabricated by this writer.
func crossRunPublicationWriter(ctx context.Context, db DB, e *inventoryEnv, invoke bool) (store.RunHeldPublication, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return store.RunHeldPublication{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	worker, err := q.GetWorkerForUpdate(ctx, e.w.ID)
	if err != nil {
		return store.RunHeldPublication{}, err
	}
	if worker.UserID != e.w.UserID {
		return store.RunHeldPublication{}, ErrNotAuthorized
	}
	supported := false
	for _, c := range worker.ProtocolCapabilities {
		if c == capability.RecoveryHeldPublicationV2 {
			supported = true
		}
	}
	if !supported {
		return store.RunHeldPublication{}, ErrNotAuthorized
	}
	run, err := q.GetRunByIDForUpdate(ctx, e.run)
	if err != nil {
		return store.RunHeldPublication{}, err
	}
	h, err := q.GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{RunID: e.run, UserID: e.w.UserID, WorkerID: e.w.ID, Generation: e.gen})
	if err != nil {
		return store.RunHeldPublication{}, err
	}
	if run.UserID != worker.UserID || run.Kind != "issue" || (run.Status != "cancelled" && run.Status != "failed" && run.Status != "completed") ||
		(run.FailOrigin.Valid && (run.FailOrigin.String == "push_secret_blocked" || run.FailOrigin.String == "worker_residue_blocked")) ||
		run.ClaimGeneration < e.gen || e.gen <= 0 || h.State != "open" || !h.InventoryGuarded ||
		h.RunID != run.ID || h.UserID != worker.UserID || h.OriginalWorkerID != worker.ID ||
		h.LiveWorkerID != pgconv.UUID(worker.ID) || h.LiveRunID != pgconv.UUID(run.ID) ||
		h.Generation != e.gen || !h.RepoID.Valid || h.RepoID != run.RepoID {
		return store.RunHeldPublication{}, ErrNotAuthorized
	}
	p, err := q.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
		RunID: e.run, Generation: e.gen, HoldID: h.ID, UserID: worker.UserID, RepoID: uuid.UUID(h.RepoID.Bytes), WorkerID: worker.ID,
		Ref: fmt.Sprintf("refs/uzi-held/%s/%d", e.run, e.gen), Tip: heldTestTip, CoverageDigest: heldTestDigest,
	})
	if err != nil {
		return p, err
	}
	if p.HoldID != h.ID || p.RunID != run.ID || p.Generation != e.gen || p.UserID != worker.UserID ||
		p.WorkerID != worker.ID || p.RepoID != uuid.UUID(h.RepoID.Bytes) ||
		p.Ref != fmt.Sprintf("refs/uzi-held/%s/%d", e.run, e.gen) || p.Tip != heldTestTip || p.CoverageDigest != heldTestDigest {
		return p, ErrManifestConflict
	}
	if invoke && !p.CreateInvokedAt.Valid {
		p, err = q.MarkHeldCreateInvoked(ctx, p.ID)
		if err != nil {
			return p, err
		}
	}
	return p, tx.Commit(ctx)
}

func crossRunPublicationRace(t *testing.T, kind string, writerFirst bool) {
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	a, s := crossRunInputs(t, e, w, b)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.w.ID, []string{capability.RecoveryInventoryV1, capability.RecoveryHeldPublicationV2})
	ready := e.reserve("publication-retained")
	body := e.upload(ready)
	readyBefore, err := mergedCapture(t, e, ready)
	if err != nil {
		t.Fatal(err)
	}
	var pubID uuid.UUID
	if kind != "insert" {
		state := "prepared"
		if kind != "invoke" {
			state = kind
		}
		pubID = crossRunPublication(t, e, e.hold, e.gen, state)
	}
	var pubBefore store.RunHeldPublication
	if pubID != uuid.Nil {
		pubBefore, err = e.q.GetHeldPublication(e.ctx, pubID)
		if err != nil {
			t.Fatal(err)
		}
	}
	holdsBefore := crossRunRow(t, e, "recovery_custody_holds", e.hold)
	witnessBefore := crossRunRow(t, e, "runs", w)
	r := newCrossRunRace(t)
	gate := r.barrier()
	var wcGate, pcGate, rollbackGate *crossRunBarrier
	if writerFirst {
		wcGate = gate
	} else if kind == "insert" {
		pcGate = gate
	} else {
		rollbackGate = gate
	}
	policy, _, policyName := r.service(e, pcGate, rollbackGate)
	_, writerDB, writerName := r.service(e, wcGate, nil)
	var hooks atomic.Int32
	policy.SetCustodySettledHook(func(run uuid.UUID) {
		if run != e.run {
			t.Error("foreign hook")
		}
		hooks.Add(1)
	})
	outcome := kind == "invoked" || kind == "create_unknown"
	write := func(ctx context.Context) (store.RunHeldPublication, error) {
		if !outcome {
			return crossRunPublicationWriter(ctx, writerDB, e, kind == "invoke")
		}
		// heldRecordOutcome is detached: only the publication UPDATE is in this
		// transaction. Adding a run or hold lock here would test a different protocol.
		tx, err := writerDB.Begin(ctx)
		if err != nil {
			return store.RunHeldPublication{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		p, err := store.New(tx).RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{ID: pubID, State: "created"})
		if err != nil {
			return p, err
		}
		return p, tx.Commit(ctx)
	}
	discard := func(ctx context.Context) (int64, error) { return policy.DiscardCrossRunHolds(ctx, a, s) }
	var wc <-chan reconcileOperation[store.RunHeldPublication]
	var pc <-chan reconcileOperation[int64]
	if writerFirst {
		wc = crossRunStart(r, write)
		pid := gate.await(t)
		pc = crossRunStart(r, discard)
		waitCrossRunBlock(t, e, policyName, pid)
	} else {
		pc = crossRunStart(r, discard)
		pid := gate.await(t)
		wc = crossRunStart(r, write)
		waitCrossRunBlock(t, e, writerName, pid)
		if rollbackGate != nil {
			current, err := e.q.GetHeldPublication(e.ctx, pubID)
			if err != nil || !reflect.DeepEqual(current, pubBefore) {
				t.Fatal("pending publication was reclassified by refusing policy")
			}
			if holdsBefore != crossRunRow(t, e, "recovery_custody_holds", e.hold) {
				t.Fatal("refusing policy changed custody")
			}
		}
	}
	gate.release()
	result := awaitReconcileOperation(t, wc)
	policySuccess := (kind == "insert" && !writerFirst) || (outcome && writerFirst)
	if policySuccess {
		crossRunWantDiscard(t, pc, 1)
	} else {
		crossRunWantRefusal(t, pc)
	}
	if kind == "insert" && !writerFirst {
		if !errors.Is(result.err, ErrNotAuthorized) && !errors.Is(result.err, pgx.ErrNoRows) {
			t.Fatalf("closed-hold writer err=%v", result.err)
		}
		var count int
		if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_held_publications WHERE run_id=$1", e.run).Scan(&count); err != nil || count != 0 {
			t.Fatalf("late insertion count=%d err=%v", count, err)
		}
	} else {
		if result.err != nil {
			t.Fatalf("publication writer: %v", result.err)
		}
		p, err := e.q.GetHeldPublication(e.ctx, result.value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p, result.value) {
			t.Fatal("policy changed publication after writer")
		}
		state := "prepared"
		if kind == "invoke" {
			state = "invoked"
		}
		if outcome {
			state = "created"
		}
		if p.State != state || p.Ref != fmt.Sprintf("refs/uzi-held/%s/%d", e.run, e.gen) ||
			p.Tip != heldTestTip || p.CoverageDigest != heldTestDigest || p.AcknowledgedAt.Valid {
			t.Fatalf("publication state=%s or identity/ACK changed", p.State)
		}
		if pubID != uuid.Nil && (p.ExpiresAt != pubBefore.ExpiresAt || p.AcknowledgedAt != pubBefore.AcknowledgedAt) {
			t.Fatal("publication TTL or ACK changed")
		}
		if outcome {
			if !p.RefCreatedAt.Valid || p.Attempts != pubBefore.Attempts+1 || p.CreateInvokedAt != pubBefore.CreateInvokedAt {
				t.Fatal("detached outcome transition changed invocation authority")
			}
		}
		if kind == "invoke" && (p.Attempts != pubBefore.Attempts+1 || !p.CreateInvokedAt.Valid || p.RefCreatedAt.Valid) {
			t.Fatal("invocation transition fabricated outcome")
		}
		if kind == "insert" && (p.CreateInvokedAt.Valid || p.RefCreatedAt.Valid) {
			t.Fatal("prepared insertion fabricated outcome")
		}
	}
	if policySuccess {
		assertMergedHoldState(t, e, e.hold, "discarded")
		if hooks.Load() != 1 {
			t.Fatalf("hooks=%d", hooks.Load())
		}
	} else {
		if holdsBefore != crossRunRow(t, e, "recovery_custody_holds", e.hold) || hooks.Load() != 0 {
			t.Fatal("refused policy changed hold or emitted hook")
		}
	}
	after, err := mergedCapture(t, e, ready)
	if err != nil || !reflect.DeepEqual(readyBefore, after) {
		t.Fatal("archive/TTL changed")
	}
	e.bytes(ready, body)
	if witnessBefore != crossRunRow(t, e, "runs", w) {
		t.Fatal("witness changed")
	}
}
