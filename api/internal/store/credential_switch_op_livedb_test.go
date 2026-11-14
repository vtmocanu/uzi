package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4201: the credential switch operation id (trigger-minted), the hold identity columns
// (trigger-cleared), the op-fenced clear/hold, and the child epoch journal. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type switchOpFixture struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	user   uuid.UUID
	worker uuid.UUID
	repo   uuid.UUID
}

func newSwitchOpFixture(t *testing.T, dsn string, ctx context.Context) *switchOpFixture {
	t.Helper()
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &switchOpFixture{ctx: ctx, pool: pool, q: store.New(pool), user: uuid.New(), worker: uuid.New()}
	conn := uuid.New()
	f.repo = uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", f.user, f.user.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, f.user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/switch-op','https://example.com/switch-op','main')`, f.repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,snapshot_register_nonce) VALUES($1,$2,$3,$4,'online','nonce-a')", f.worker, f.user, f.worker.String(), f.worker[:])
	return f
}

var switchOpIssue = 4201000

func (f *switchOpFixture) run(t *testing.T, status string) uuid.UUID {
	t.Helper()
	switchOpIssue++
	id := uuid.New()
	mustExec(f.ctx, t, f.pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
 VALUES($1,$2,$3,$4,'issue',$5,'switch op','fixture',$6,1)`, id, f.user, f.repo, f.worker, switchOpIssue, status)
	return id
}

func (f *switchOpFixture) stamp(t *testing.T, id uuid.UUID) {
	t.Helper()
	n, err := f.q.StampHeldCredentialSwitch(f.ctx, store.StampHeldCredentialSwitchParams{
		Mode: pgtype.Text{String: "auto", Valid: true}, Generation: pgtype.Int8{Int64: 1, Valid: true},
		ID: id, UserID: f.user, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true},
	})
	if err != nil || n != 1 {
		t.Fatalf("StampHeldCredentialSwitch rows=%d err=%v", n, err)
	}
}

func (f *switchOpFixture) op(t *testing.T, id uuid.UUID) pgtype.UUID {
	t.Helper()
	op, err := f.q.GetRunCredentialSwitchOpID(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestCredentialSwitchOpIDTriggerLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "switch_op_")
	if err := store.MigrateTo(ctx, dsn, 441); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Backfill: a stamp pending before the migration gets an id; an unstamped run stays NULL.
	pre := newSwitchOpFixture(t, dsn, ctx)
	stamped, plain := pre.run(t, "running"), pre.run(t, "running")
	mustExec(ctx, t, pool, "UPDATE runs SET credential_switch_requested_at=now(), credential_switch_generation=1 WHERE id=$1", stamped)
	pool.Close()
	if err := store.MigrateTo(ctx, dsn, 447); err != nil {
		t.Fatal(err)
	}
	f := newSwitchOpFixture(t, dsn, ctx)
	if op := f.op(t, stamped); !op.Valid {
		t.Fatal("backfill left a pending stamp without an op id")
	}
	if op := f.op(t, plain); op.Valid {
		t.Fatal("backfill minted an op id for an unstamped run")
	}

	id := f.run(t, "running")
	f.stamp(t, id)
	op1 := f.op(t, id)
	if !op1.Valid {
		t.Fatal("stamp did not mint an op id")
	}
	// Unrelated update keeps it; a writer cannot swap it while the stamp is unchanged.
	mustExec(ctx, t, f.pool, "UPDATE runs SET updated_at=now() WHERE id=$1", id)
	mustExec(ctx, t, f.pool, "UPDATE runs SET credential_switch_op_id=$2 WHERE id=$1", id, uuid.New())
	if got := f.op(t, id); got != op1 {
		t.Fatalf("op id changed without a new stamp: %v -> %v", op1, got)
	}
	// A second stamp mints a new id, even within one transaction (strictly increasing stamp).
	f.stamp(t, id)
	op2 := f.op(t, id)
	if !op2.Valid || op2 == op1 {
		t.Fatalf("second stamp reused the op id: %v %v", op1, op2)
	}
	// Heal on a stamp write: stamp set, id NULL (nulling the trigger-protected value through a
	// stamp-preserving write is impossible, so null it with the trigger disabled for one statement).
	mustExec(ctx, t, f.pool, "ALTER TABLE runs DISABLE TRIGGER runs_credential_switch_op_id_trg")
	mustExec(ctx, t, f.pool, "UPDATE runs SET credential_switch_op_id=NULL WHERE id=$1", id)
	mustExec(ctx, t, f.pool, "ALTER TABLE runs ENABLE TRIGGER runs_credential_switch_op_id_trg")
	if got := f.op(t, id); got.Valid {
		t.Fatal("fixture: expected the op id to be NULL under a pending stamp")
	}
	// The trigger fires on a stamp change (a NULL->NULL op write does not satisfy its WHEN clause).
	mustExec(ctx, t, f.pool, "UPDATE runs SET credential_switch_requested_at=credential_switch_requested_at + interval '1 microsecond' WHERE id=$1", id)
	if got := f.op(t, id); !got.Valid {
		t.Fatal("a pending stamp with a NULL op id did not self-heal")
	}
	// Non-NULL stamp with a NULL op id and a writer-supplied id: the trigger mints its own, never
	// accepting the writer's value.
	mustExec(ctx, t, f.pool, "ALTER TABLE runs DISABLE TRIGGER runs_credential_switch_op_id_trg")
	mustExec(ctx, t, f.pool, "UPDATE runs SET credential_switch_op_id=NULL WHERE id=$1", id)
	mustExec(ctx, t, f.pool, "ALTER TABLE runs ENABLE TRIGGER runs_credential_switch_op_id_trg")
	supplied := uuid.New()
	mustExec(ctx, t, f.pool, "UPDATE runs SET credential_switch_op_id=$2 WHERE id=$1", id, supplied)
	if got := f.op(t, id); !got.Valid || got.Bytes == [16]byte(supplied) {
		t.Fatalf("a writer-supplied op id was accepted under a NULL old id: %v (supplied %v)", got, supplied)
	}
	// An old-binary-shaped plain clear nulls it.
	mustExec(ctx, t, f.pool, "UPDATE runs SET credential_switch_requested_at=NULL WHERE id=$1", id)
	if got := f.op(t, id); got.Valid {
		t.Fatal("clearing the stamp left the op id")
	}
}

// TestCredentialSecretShareLocksAreNowaitLiveDB pins both share-lock modes: the in-place apply's
// LockAnthropicSecretForApply and the claim finisher's LockSecretEnablementForShareNowait must fail
// fast with 55P03 (lock_not_available) while another transaction holds the row FOR UPDATE, never
// wait. Waiting would deadlock against a token delete, whose cascade needs the run row the caller
// already holds.
func TestCredentialSecretShareLocksAreNowaitLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "switch_nowait_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	f := newSwitchOpFixture(t, dsn, ctx)
	sec := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO user_secrets(id,user_id,kind,label,ciphertext) VALUES($1,$2,'anthropic_token','s',$3)`, sec, f.user, []byte{1})

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM user_secrets WHERE id=$1 FOR UPDATE", sec); err != nil {
		t.Fatal(err)
	}
	assertLockNotAvailable := func(name string, call func(q *store.Queries) error) {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		start := time.Now()
		err = call(store.New(tx))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("%s under a held FOR UPDATE: err = %v, want SQLSTATE 55P03", name, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%s waited %v; NOWAIT must fail fast", name, d)
		}
	}
	assertLockNotAvailable("LockAnthropicSecretForApply", func(q *store.Queries) error {
		_, err := q.LockAnthropicSecretForApply(ctx, store.LockAnthropicSecretForApplyParams{ID: sec, UserID: f.user})
		return err
	})
	assertLockNotAvailable("LockSecretEnablementForShareNowait", func(q *store.Queries) error {
		_, err := q.LockSecretEnablementForShareNowait(ctx, store.LockSecretEnablementForShareNowaitParams{ID: sec, UserID: f.user})
		return err
	})
}

func TestCredentialSwitchOpFenceAndHoldIdentityLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "switch_hold_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	f := newSwitchOpFixture(t, dsn, ctx)
	workerID := pgtype.UUID{Bytes: f.worker, Valid: true}

	// Op-fenced clear does not clear a different op, and does clear the matching one.
	id := f.run(t, "running")
	f.stamp(t, id)
	op := f.op(t, id)
	n, err := f.q.ClearCredentialSwitchByWorker(ctx, store.ClearCredentialSwitchByWorkerParams{
		ID: id, WorkerID: workerID, Generation: 1, OpID: pgtype.UUID{Bytes: uuid.New(), Valid: true}})
	if err != nil || n != 0 {
		t.Fatalf("clear with a foreign op id: rows=%d err=%v", n, err)
	}
	n, err = f.q.ClearCredentialSwitchStampByOp(ctx, store.ClearCredentialSwitchStampByOpParams{ID: id, OpID: pgtype.UUID{Bytes: uuid.New(), Valid: true}})
	if err != nil || n != 0 {
		t.Fatalf("ClearCredentialSwitchStampByOp with a foreign op: rows=%d err=%v", n, err)
	}
	n, err = f.q.ClearCredentialSwitchByWorker(ctx, store.ClearCredentialSwitchByWorkerParams{ID: id, WorkerID: workerID, Generation: 1, OpID: op})
	if err != nil || n != 0 {
		t.Fatalf("clear with the matching op id and no register nonce: rows=%d err=%v", n, err)
	}
	n, err = f.q.ClearCredentialSwitchByWorker(ctx, store.ClearCredentialSwitchByWorkerParams{ID: id, WorkerID: workerID, Generation: 1, OpID: op, RegisterNonce: pgtype.Text{String: "nonce-stale", Valid: true}})
	if err != nil || n != 0 {
		t.Fatalf("clear with the matching op id and a wrong register nonce: rows=%d err=%v", n, err)
	}
	n, err = f.q.ClearCredentialSwitchByWorker(ctx, store.ClearCredentialSwitchByWorkerParams{ID: id, WorkerID: workerID, Generation: 1, OpID: op, RegisterNonce: pgtype.Text{String: "nonce-a", Valid: true}})
	if err != nil || n != 1 {
		t.Fatalf("clear with the matching op id: rows=%d err=%v", n, err)
	}
	f.stamp(t, id)
	op = f.op(t, id)
	n, err = f.q.ClearCredentialSwitchStampByOp(ctx, store.ClearCredentialSwitchStampByOpParams{ID: id, OpID: op})
	if err != nil || n != 1 {
		t.Fatalf("ClearCredentialSwitchStampByOp: rows=%d err=%v", n, err)
	}

	// Hold: legacy (nil op) callers are running-only; an op-fenced hold also parks awaiting_input.
	type holdRow struct {
		Op    pgtype.UUID
		Gen   pgtype.Int8
		W     pgtype.UUID
		Nonce pgtype.Text
	}
	readHold := func(id uuid.UUID) holdRow {
		var h holdRow
		if err := f.pool.QueryRow(ctx, `SELECT credential_switch_hold_op_id, credential_switch_hold_generation,
 credential_switch_hold_worker_id, credential_switch_hold_register_nonce FROM runs WHERE id=$1`, id).Scan(&h.Op, &h.Gen, &h.W, &h.Nonce); err != nil {
			t.Fatal(err)
		}
		return h
	}
	hold := func(id uuid.UUID, op pgtype.UUID) int64 {
		n, err := f.q.HoldCredentialSwitchUnsettled(ctx, store.HoldCredentialSwitchUnsettledParams{ID: id, WorkerID: workerID, Generation: 1, RegisterNonce: "nonce-a", OpID: op})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	wait := f.run(t, "awaiting_input")
	f.stamp(t, wait)
	if n := hold(wait, pgtype.UUID{}); n != 0 {
		t.Fatalf("un-fenced hold of an awaiting_input run: rows=%d, want 0", n)
	}
	if n := hold(wait, pgtype.UUID{Bytes: uuid.New(), Valid: true}); n != 0 {
		t.Fatalf("hold with a foreign op id: rows=%d, want 0", n)
	}
	waitOp := f.op(t, wait)
	if n := hold(wait, waitOp); n != 1 {
		t.Fatalf("op-fenced hold of an awaiting_input run: rows=%d, want 1", n)
	}
	h := readHold(wait)
	if h.Op != waitOp || h.Gen.Int64 != 1 || h.W != workerID || h.Nonce.String != "nonce-a" {
		t.Fatalf("hold identity = %+v, want op %v gen 1 worker %v nonce-a", h, waitOp, workerID)
	}
	if got := f.op(t, wait); got.Valid {
		t.Fatal("the hold consumed the stamp but left the op id")
	}
	// settling -> unsettled keeps the identity.
	mustExec(ctx, t, f.pool, "UPDATE runs SET hold_reason='credential_switch_unsettled' WHERE id=$1", wait)
	if got := readHold(wait); got != h {
		t.Fatalf("settling->unsettled changed the hold identity: %+v", got)
	}
	// Leaving the switch holds (any other reason, then NULL) nulls it.
	mustExec(ctx, t, f.pool, "UPDATE runs SET hold_reason=NULL WHERE id=$1", wait)
	if got := readHold(wait); got != (holdRow{}) {
		t.Fatalf("hold identity survived leaving the hold: %+v", got)
	}
	// Legacy running hold, then an old-binary-shaped resume clears it too.
	run := f.run(t, "running")
	f.stamp(t, run)
	if n := hold(run, pgtype.UUID{}); n != 1 {
		t.Fatalf("legacy hold of a running run: rows=%d, want 1", n)
	}
	if h := readHold(run); !h.Op.Valid {
		t.Fatalf("legacy hold did not record the op id: %+v", h)
	}
	mustExec(ctx, t, f.pool, "UPDATE runs SET status='running', hold_reason=NULL WHERE id=$1", run)
	if got := readHold(run); got != (holdRow{}) {
		t.Fatalf("resume left the hold identity: %+v", got)
	}

	// Child epochs: unique op_id, ordinals, max, owner-scoped list, by-op read.
	e1op, e2op := uuid.New(), uuid.New()
	ins := func(op uuid.UUID) (store.RunCredentialSwitchEpoch, error) {
		return f.q.InsertRunCredentialSwitchEpoch(ctx, store.InsertRunCredentialSwitchEpochParams{
			RunID: run, ClaimGeneration: 1, OpID: op, WorkerID: workerID, SecretEnablementRev: 3,
			Label: pgtype.Text{String: "tok", Valid: true}, MessageSeq: 7,
		})
	}
	if m, err := f.q.MaxCommittedCredentialEpochOrdinal(ctx, store.MaxCommittedCredentialEpochOrdinalParams{RunID: run, ClaimGeneration: 1}); err != nil || m != 0 {
		t.Fatalf("max ordinal with no epochs = %d err=%v", m, err)
	}
	r1, err := ins(e1op)
	if err != nil || r1.Ordinal != 1 {
		t.Fatalf("first epoch: %+v err=%v", r1, err)
	}
	r2, err := ins(e2op)
	if err != nil || r2.Ordinal != 2 {
		t.Fatalf("second epoch: %+v err=%v", r2, err)
	}
	if _, err := ins(e1op); err == nil {
		t.Fatal("a repeated op_id inserted a second epoch")
	}
	if m, _ := f.q.MaxCommittedCredentialEpochOrdinal(ctx, store.MaxCommittedCredentialEpochOrdinalParams{RunID: run, ClaimGeneration: 1}); m != 2 {
		t.Fatalf("max ordinal = %d, want 2", m)
	}
	got, err := f.q.GetRunCredentialSwitchEpochByOp(ctx, store.GetRunCredentialSwitchEpochByOpParams{OpID: e2op, RunID: run})
	if err != nil || got.Ordinal != 2 {
		t.Fatalf("by-op read: %+v err=%v", got, err)
	}
	if _, err := f.q.GetRunCredentialSwitchEpochByOp(ctx, store.GetRunCredentialSwitchEpochByOpParams{OpID: e2op, RunID: uuid.New()}); err == nil {
		t.Fatal("by-op read crossed runs")
	}
	if l, err := f.q.ListRunCredentialSwitchEpochs(ctx, store.ListRunCredentialSwitchEpochsParams{RunID: run, UserID: f.user}); err != nil || len(l) != 2 || l[0].Ordinal != 1 {
		t.Fatalf("owner list: %d err=%v", len(l), err)
	}
	if l, _ := f.q.ListRunCredentialSwitchEpochs(ctx, store.ListRunCredentialSwitchEpochsParams{RunID: run, UserID: uuid.New()}); len(l) != 0 {
		t.Fatal("list leaked another user's epochs")
	}

	// GetPriorRunCredentialEpoch unions the base journal and the child epochs.
	mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=3 WHERE id=$1", run)
	if _, err := f.q.GetPriorRunCredentialEpoch(ctx, store.GetPriorRunCredentialEpochParams{RunID: run, ClaimGeneration: 1}); err == nil {
		t.Fatal("prior epoch before generation 1 should not exist")
	}
	mustExec(ctx, t, f.pool, "INSERT INTO run_credential_epochs(run_id,claim_generation,label) VALUES($1,1,'base')", run)
	p, err := f.q.GetPriorRunCredentialEpoch(ctx, store.GetPriorRunCredentialEpochParams{RunID: run, ClaimGeneration: 2})
	if err != nil || p.Label.String != "tok" {
		t.Fatalf("prior epoch should be the newest child of generation 1: %+v err=%v", p, err)
	}
	mustExec(ctx, t, f.pool, "INSERT INTO run_credential_epochs(run_id,claim_generation,label) VALUES($1,2,'base2')", run)
	p, err = f.q.GetPriorRunCredentialEpoch(ctx, store.GetPriorRunCredentialEpochParams{RunID: run, ClaimGeneration: 3})
	if err != nil || p.Label.String != "base2" {
		t.Fatalf("prior epoch should be the generation-2 base: %+v err=%v", p, err)
	}

	// Codex epoch: idempotent, never overwrites.
	for _, l := range []string{"codex-a", "codex-b"} {
		if err := f.q.RecordCodexCredentialEpoch(ctx, store.RecordCodexCredentialEpochParams{RunID: run, ClaimGeneration: 9, Label: pgtype.Text{String: l, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	var label string
	if err := f.pool.QueryRow(ctx, "SELECT label FROM run_credential_epochs WHERE run_id=$1 AND claim_generation=9", run).Scan(&label); err != nil || label != "codex-a" {
		t.Fatalf("codex epoch label = %q err=%v", label, err)
	}

	// Secret lock: only the owner's anthropic_token.
	sec := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO user_secrets(id,user_id,kind,label,ciphertext) VALUES($1,$2,'anthropic_token','s',$3)`, sec, f.user, []byte{1})
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.New(tx).LockAnthropicSecretForApply(ctx, store.LockAnthropicSecretForApplyParams{ID: sec, UserID: f.user})
	if err != nil || row.Kind != "anthropic_token" {
		t.Fatalf("lock secret: %+v err=%v", row, err)
	}
	if _, err := store.New(tx).LockAnthropicSecretForApply(ctx, store.LockAnthropicSecretForApplyParams{ID: sec, UserID: uuid.New()}); err == nil {
		t.Fatal("locked another user's secret")
	}
	_ = tx.Rollback(ctx)
	if err := f.q.StampWorkerCredentialSwitchApplyNonce(ctx, store.StampWorkerCredentialSwitchApplyNonceParams{ID: f.worker, Nonce: pgtype.Text{String: "n1", Valid: true}}); err != nil {
		t.Fatal(err)
	}
}
