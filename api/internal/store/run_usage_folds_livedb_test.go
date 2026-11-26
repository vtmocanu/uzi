package store_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700, migration 00543: the materialized per-(run, model) usage fold, its freshness
// state, the write-path lock (fn_run_usage_fold_lock) and MaterializeRunUsageFolds. Every
// test skips without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh). Races are
// synchronised with open transactions, the materializer's step hook and a bounded poll of
// pg_stat_activity for a backend waiting on a Lock, never with sleeps standing in for order.

type foldEnv struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	connID uuid.UUID
	repoID uuid.UUID
}

var foldIIDMu sync.Mutex
var foldIIDNext = time.Now().UnixNano() % 1_000_000_000

func newFoldEnv(t *testing.T) *foldEnv {
	t.Helper()
	ctx, pool := openAnalyticsViewsDB(t)
	userID, repoID := seedAnalyticsViewsUser(ctx, t, pool, "fold")
	var connID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT connection_id FROM repos WHERE id = $1`, repoID).Scan(&connID); err != nil {
		t.Fatalf("connection id: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
	return &foldEnv{ctx: ctx, pool: pool, q: store.New(pool), userID: userID, connID: connID, repoID: repoID}
}

func (e *foldEnv) nextIID() int64 {
	foldIIDMu.Lock()
	defer foldIIDMu.Unlock()
	foldIIDNext++
	return foldIIDNext
}

// run inserts a usage_refolded issue run in the given status.
func (e *foldEnv) run(t *testing.T, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(e.ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, usage_refolded)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, true)`, id, e.userID, e.repoID, e.nextIID(), status)
	return id
}

func (e *foldEnv) chatRun(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(e.ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, usage_refolded)
		 VALUES ($1, $2, 't', 'd', 'completed', 'chat', true)`, id, e.userID)
	return id
}

func numeric(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("numeric %q: %v", s, err)
	}
	return n
}

func upsertArg(t *testing.T, run uuid.UUID, session, model string, epoch int32, in int64) store.UpsertRunUsageParams {
	return store.UpsertRunUsageParams{
		RunID: run, SessionID: session, Model: model, LineageEpoch: epoch,
		InputTokens: in, CacheReadTokens: in / 5, CacheCreationTokens: in / 25, OutputTokens: in / 10,
		CostUsd: numeric(t, "0.5"), Harness: "claude", CostStatus: "metered", UsageBasis: "per_leg",
	}
}

func (e *foldEnv) upsert(t *testing.T, q *store.Queries, run uuid.UUID, session, model string, epoch int32, in int64) error {
	t.Helper()
	return q.UpsertRunUsage(e.ctx, upsertArg(t, run, session, model, epoch, in))
}

func (e *foldEnv) state(t *testing.T, run uuid.UUID) (exists, stale bool) {
	t.Helper()
	err := e.pool.QueryRow(e.ctx, `SELECT stale FROM run_usage_fold_state WHERE run_id = $1`, run).Scan(&stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return true, stale
}

func (e *foldEnv) wantState(t *testing.T, label string, run uuid.UUID, wantExists, wantStale bool) {
	t.Helper()
	if exists, stale := e.state(t, run); exists != wantExists || stale != wantStale {
		t.Fatalf("%s: state (exists=%t stale=%t), want (exists=%t stale=%t)", label, exists, stale, wantExists, wantStale)
	}
}

func (e *foldEnv) foldRows(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, run uuid.UUID) []string {
	t.Helper()
	rows, err := q.Query(e.ctx, `SELECT model || ':' || input_tokens || ':' || cache_read_tokens || ':' || cache_creation_tokens || ':' || output_tokens || ':' || cost_usd::text || ':' || cost_status
		FROM run_usage_model_folds WHERE run_id = $1 ORDER BY model`, run)
	if err != nil {
		t.Fatalf("fold rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *foldEnv) materialize(t *testing.T, ids ...uuid.UUID) int64 {
	t.Helper()
	n, err := store.MaterializeRunUsageFolds(e.ctx, e.pool, ids)
	if err != nil {
		t.Fatalf("MaterializeRunUsageFolds: %v", err)
	}
	return n
}

// opResult is the outcome of an operation started on its own backend.
type opResult struct {
	n   int64
	err error
}

// bgOp runs fn on a dedicated pool connection in a goroutine and remembers its backend pid, so
// the test can wait until that backend is blocked on a lock.
type bgOp struct {
	pid  uint32
	done chan opResult
}

func (e *foldEnv) bg(t *testing.T, fn func(ctx context.Context, conn *pgxpool.Conn) (int64, error)) *bgOp {
	t.Helper()
	conn, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	op := &bgOp{pid: conn.Conn().PgConn().PID(), done: make(chan opResult, 1)}
	go func() {
		defer conn.Release()
		n, err := fn(e.ctx, conn)
		op.done <- opResult{n, err}
	}()
	return op
}

func (op *bgOp) waitBlocked(t *testing.T, e *foldEnv) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var wt *string
		if err := e.pool.QueryRow(e.ctx, `SELECT wait_event_type FROM pg_stat_activity WHERE pid::bigint = $1`, op.pid).Scan(&wt); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if wt != nil && *wt == "Lock" {
			return
		}
		select {
		case r := <-op.done:
			t.Fatalf("operation finished (%v) instead of blocking on a lock", r.err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("operation never blocked on a lock")
}

func (op *bgOp) wait(t *testing.T) opResult {
	t.Helper()
	select {
	case r := <-op.done:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("operation did not finish (deadlock or lost wake-up)")
		return opResult{}
	}
}

// pauseAt installs a materializer hook that parks at the given step until release is closed. It
// returns the channel closed when the step is reached.
func pauseAt(t *testing.T, step string) (reached <-chan struct{}, release func()) {
	t.Helper()
	r := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	restore := store.SetRunUsageFoldStepHook(func(ctx context.Context, s string) {
		if s != step {
			return
		}
		once.Do(func() { close(r) })
		select {
		case <-rel:
		case <-time.After(30 * time.Second):
		}
	})
	var relOnce sync.Once
	release = func() { relOnce.Do(func() { close(rel) }) }
	t.Cleanup(func() { release(); restore() })
	return r, release
}

func mustReach(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("materializer never reached the hooked step")
	}
}

func (e *foldEnv) matBG(t *testing.T, ids ...uuid.UUID) *bgOp {
	return e.bg(t, func(ctx context.Context, _ *pgxpool.Conn) (int64, error) {
		return store.MaterializeRunUsageFolds(ctx, e.pool, ids)
	})
}

// -----------------------------------------------------------------------------------------

func TestRunUsageFoldParityLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	use := func(run uuid.UUID, session, model string, epoch, lineage int, basis string, in int64, status, cost string) {
		t.Helper()
		mustExec(e.ctx, t, e.pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
			     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12)`,
			run, session, model, epoch, lineage, basis, in, in/5, in/25, in/10, cost, status)
	}
	multi := e.run(t, "completed")
	use(multi, "s1", "model-a", 0, 0, "per_leg", 1000, "metered", "1.5")
	use(multi, "s1", "model-a", 1, 0, "per_leg", 300, "metered", "0.5")
	use(multi, "s2", "model-a", 1, 0, "per_leg", 400, "metered", "0.7")
	use(multi, "s1", "model-b", 0, 0, "per_leg", 200, "subscription", "0")
	cum := e.run(t, "failed")
	use(cum, "s1", "model-a", 0, 0, "per_leg", 100, "metered", "0.1")
	use(cum, "s1", "model-a", 1, 0, "session_cumulative", 250, "metered", "0.25")
	use(cum, "s1", "model-a", 2, 0, "session_cumulative", 400, "metered", "0.4")
	use(cum, "s2", "model-a", 3, 1, "session_cumulative", 50, "metered", "0.05")
	use(cum, "s2", "model-b", 0, 0, "per_leg", 50, "metered", "0.05")
	mixed := e.run(t, "cancelled")
	use(mixed, "s1", "model-a", 0, 0, "per_leg", 50, "unreported", "0")
	use(mixed, "s1", "model-b", 0, 0, "per_leg", 100, "metered", "0.02")
	legs := e.run(t, "completed")
	for ep := 0; ep < 3; ep++ {
		for _, m := range []string{"model-a", "model-b", "model-c"} {
			use(legs, "s1", m, ep, 0, "per_leg", int64(50*(ep+1)), "metered", "0.01")
		}
	}
	zero := e.run(t, "completed")
	all := []uuid.UUID{multi, cum, mixed, legs, zero}

	if got := e.materialize(t, all...); got != int64(len(all)) {
		t.Fatalf("materialized %d runs, want %d", got, len(all))
	}
	var missing, extra int
	const cols = `run_id, model, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status`
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM (SELECT `+cols+` FROM run_usage_model_totals WHERE run_id = ANY($1) EXCEPT SELECT `+cols+` FROM run_usage_model_folds WHERE run_id = ANY($1)) d`, all).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM (SELECT `+cols+` FROM run_usage_model_folds WHERE run_id = ANY($1) EXCEPT SELECT `+cols+` FROM run_usage_model_totals WHERE run_id = ANY($1)) d`, all).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	if missing != 0 || extra != 0 {
		t.Fatalf("fold differs from run_usage_model_totals: %d view rows missing from the fold, %d fold rows not in the view", missing, extra)
	}
	wantRows := map[uuid.UUID]int{multi: 2, cum: 2, mixed: 2, legs: 3, zero: 0}
	for id, want := range wantRows {
		if got := len(e.foldRows(t, e.pool, id)); got != want {
			t.Errorf("run %s: %d fold rows, want %d", id, got, want)
		}
		e.wantState(t, "after materialize", id, true, false)
	}
}

func TestRunUsageFoldInvalidationByUpsertLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	run := e.run(t, "completed")
	if err := e.upsert(t, e.q, run, "s1", "model-a", 0, 1000); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, "first upsert creates a stale state", run, true, true)
	if n := e.materialize(t, run); n != 1 {
		t.Fatalf("materialized %d, want 1", n)
	}
	e.wantState(t, "fresh", run, true, false)
	if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
		t.Fatalf("fold = %v", got)
	}

	if err := e.upsert(t, e.q, run, "s1", "model-a", 0, 2000); err != nil { // existing key
		t.Fatal(err)
	}
	e.wantState(t, "existing-key upsert", run, true, true)
	if n := e.materialize(t, run); n != 1 {
		t.Fatalf("materialized %d, want 1", n)
	}
	if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:2000:") {
		t.Fatalf("fold after existing-key upsert = %v", got)
	}

	if err := e.upsert(t, e.q, run, "s1", "model-b", 1, 500); err != nil { // new key
		t.Fatal(err)
	}
	e.wantState(t, "new-key upsert", run, true, true)
	e.materialize(t, run)
	if got := e.foldRows(t, e.pool, run); len(got) != 2 {
		t.Fatalf("fold after new-key upsert = %v", got)
	}

	// DeleteRunUsage marks stale and leaves an empty fold behind once re-materialized.
	if err := e.q.DeleteRunUsage(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, "DeleteRunUsage", run, true, true)
	if got := e.foldRows(t, e.pool, run); len(got) != 2 {
		t.Fatalf("stale fold rows must stay until re-materialized, got %v", got)
	}
	e.materialize(t, run)
	e.wantState(t, "empty re-fold", run, true, false)
	if got := e.foldRows(t, e.pool, run); len(got) != 0 {
		t.Fatalf("empty fold expected, got %v", got)
	}

	// A cancelled run folded, then a late straggler frame upserts: stale again.
	cancelled := e.run(t, "cancelled")
	mustExec(e.ctx, t, e.pool, `UPDATE runs SET claim_generation = 3 WHERE id = $1`, cancelled)
	if err := e.upsert(t, e.q, cancelled, "s1", "model-a", 0, 100); err != nil {
		t.Fatal(err)
	}
	e.materialize(t, cancelled)
	e.wantState(t, "cancelled fold", cancelled, true, false)
	if err := e.upsert(t, e.q, cancelled, "s1", "model-a", 7, 100); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, "late upsert on a cancelled run", cancelled, true, true)
}

func TestRunUsageFoldMissingRunLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	ghost := uuid.New()
	err := e.upsert(t, e.q, ghost, "s1", "model-a", 0, 100)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("UpsertRunUsage for a missing run = %v, want SQLSTATE 23503", err)
	}
	e.wantState(t, "missing run upsert", ghost, false, false)
	if err := e.q.DeleteRunUsage(e.ctx, ghost); err == nil {
		t.Fatal("DeleteRunUsage for a missing run succeeded; want the lock function's 23503")
	}
	e.wantState(t, "missing run delete", ghost, false, false)
}

func TestRunUsageFoldIneligibleAndBatchBoundaryLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	chat := e.chatRun(t)
	running := e.run(t, "running")
	notRefolded := e.run(t, "completed")
	mustExec(e.ctx, t, e.pool, `UPDATE runs SET usage_refolded = false WHERE id = $1`, notRefolded)
	for _, id := range []uuid.UUID{chat, running, notRefolded} {
		mustExec(e.ctx, t, e.pool, `INSERT INTO run_usage_fold_state (run_id, stale) VALUES ($1, true)`, id)
	}
	ghost := uuid.New()
	fresh := e.run(t, "completed") // no state row yet
	if n := e.materialize(t, chat, running, notRefolded, ghost); n != 0 {
		t.Fatalf("ineligible batch materialized %d runs", n)
	}
	for name, id := range map[string]uuid.UUID{"chat": chat, "running": running, "usage_refolded=false": notRefolded} {
		e.wantState(t, name, id, true, true)
	}
	e.wantState(t, "unknown id", ghost, false, false)
	e.wantState(t, "run outside the batch", fresh, false, false)

	// Batch boundary: an eligible stale state row of a run NOT in ids stays untouched.
	in, out := e.run(t, "completed"), e.run(t, "completed")
	for _, id := range []uuid.UUID{in, out} {
		if err := e.upsert(t, e.q, id, "s", "m", 0, 100); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.materialize(t, in); n != 1 {
		t.Fatalf("materialized %d, want 1", n)
	}
	e.wantState(t, "in-batch", in, true, false)
	e.wantState(t, "out-of-batch", out, true, true)
	if got := e.foldRows(t, e.pool, out); len(got) != 0 {
		t.Fatalf("out-of-batch run has fold rows %v", got)
	}

	// The cap is enforced, not silently truncated.
	ids := make([]uuid.UUID, store.MaxRunUsageFoldBatch+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	if _, err := store.MaterializeRunUsageFolds(e.ctx, e.pool, ids); !errors.Is(err, store.ErrRunUsageFoldBatchTooLarge) {
		t.Fatalf("oversized batch error = %v", err)
	}
	if n, err := store.MaterializeRunUsageFolds(e.ctx, e.pool, nil); n != 0 || err != nil {
		t.Fatalf("empty batch = %d, %v", n, err)
	}
}

// A writer that holds an EXISTING state row (open transaction) makes the materializer skip the
// run, both when the row was fresh and when it was already stale (the upsert's DO UPDATE ...
// WHERE is false, which must still hold the row lock). A writer that CREATED the state row is
// the "state_created_by_open_writer" subtest: the materializer does not skip it, it waits and
// the whole batch fails with 55P03 after lock_timeout.
func TestRunUsageFoldWriterHoldsLockMaterializerSkipsLiveDB(t *testing.T) {
	for _, preStale := range []bool{false, true} {
		t.Run(fmt.Sprintf("preexisting_stale_state=%t", preStale), func(t *testing.T) {
			e := newFoldEnv(t)
			run := e.run(t, "completed")
			if err := e.upsert(t, e.q, run, "s1", "model-a", 0, 1000); err != nil {
				t.Fatal(err)
			}
			if !preStale {
				e.materialize(t, run)
			}
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := e.upsert(t, e.q.WithTx(tx), run, "s1", "model-a", 0, 3000); err != nil {
				t.Fatal(err)
			}
			if n := e.materialize(t, run); n != 0 {
				t.Fatalf("materializer folded %d runs while a writer held the run", n)
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			e.wantState(t, "after writer commit", run, true, true)
			if n := e.materialize(t, run); n != 1 {
				t.Fatalf("materialized %d, want 1", n)
			}
			if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:3000:") {
				t.Fatalf("fold = %v, want the writer's 3000", got)
			}
		})
	}

	// The open writer created run A's state row (A had none): step (a) waits on the uncommitted
	// row, lock_timeout (2s) fires and the whole batch [A, B] fails; nothing is folded.
	t.Run("state_created_by_open_writer", func(t *testing.T) {
		e := newFoldEnv(t)
		a := e.run(t, "completed")
		b := e.run(t, "completed")
		if err := e.upsert(t, e.q, b, "s1", "model-b", 0, 2000); err != nil {
			t.Fatal(err)
		}
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := e.upsert(t, e.q.WithTx(tx), a, "s1", "model-a", 0, 1000); err != nil {
			t.Fatal(err)
		}
		e.wantState(t, "A while the writer is open", a, false, false)

		n, err := store.MaterializeRunUsageFolds(e.ctx, e.pool, []uuid.UUID{a, b})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("materialize = %d, %v; want a lock-timeout error (SQLSTATE 55P03)", n, err)
		}
		if got := e.foldRows(t, e.pool, b); len(got) != 0 {
			t.Fatalf("B folded despite the failed batch: %v", got)
		}
		e.wantState(t, "B after the failed batch", b, true, true)

		if err := tx.Commit(e.ctx); err != nil {
			t.Fatal(err)
		}
		e.wantState(t, "A after writer commit", a, true, true)
		if got := e.foldRows(t, e.pool, a); len(got) != 0 {
			t.Fatalf("A has fold rows before any successful materialization: %v", got)
		}

		if n := e.materialize(t, a, b); n != 2 {
			t.Fatalf("retry materialized %d, want 2", n)
		}
		if got := e.foldRows(t, e.pool, a); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
			t.Fatalf("A fold after retry = %v", got)
		}
		if got := e.foldRows(t, e.pool, b); len(got) != 1 || !strings.HasPrefix(got[0], "model-b:2000:") {
			t.Fatalf("B fold after retry = %v", got)
		}
	})
}

// The materializer must stay READ COMMITTED: (d) has to see a writer commit that landed after
// the transaction's first statement. Run A has a stale state row (no lock is taken at (a0) for
// it); a concurrent upsert raises the model's tokens after (a0). Under REPEATABLE READ the
// snapshot taken at (a0) would hide that write from (d) while (e) still marks the run fresh.
func TestRunUsageFoldMaterializerSeesWriteAfterFirstStatementLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	run := e.run(t, "completed")
	if err := e.upsert(t, e.q, run, "s1", "model-a", 0, 1000); err != nil {
		t.Fatal(err)
	}
	e.wantState(t, "precondition", run, true, true)
	reached, release := pauseAt(t, "a0")
	mat := e.matBG(t, run)
	mustReach(t, reached)
	if err := e.upsert(t, e.q, run, "s1", "model-a", 0, 5000); err != nil {
		t.Fatal(err)
	}
	release()
	if r := mat.wait(t); r.err != nil {
		t.Fatalf("materializer: %v", r.err)
	}
	got := e.foldRows(t, e.pool, run)
	_, stale := e.state(t, run)
	if !stale && (len(got) != 1 || !strings.HasPrefix(got[0], "model-a:5000:")) {
		t.Fatalf("run marked fresh with fold %v, want the write committed after (a0) (5000)", got)
	}
}

// Materializer paused after (b): the writer blocks in fn_run_usage_fold_lock, the
// materializer commits its (older) fold, then the writer completes and the run is stale.
func TestRunUsageFoldMaterializerHoldsLockWriterWaitsLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	run := e.run(t, "completed")
	if err := e.upsert(t, e.q, run, "s1", "model-a", 0, 1000); err != nil {
		t.Fatal(err)
	}
	reached, release := pauseAt(t, "b")
	mat := e.matBG(t, run)
	mustReach(t, reached)
	w := e.bg(t, func(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
		return 0, store.New(conn).UpsertRunUsage(ctx, upsertArg(t, run, "s1", "model-a", 0, 5000))
	})
	w.waitBlocked(t, e)
	release()
	if r := mat.wait(t); r.err != nil || r.n != 1 {
		t.Fatalf("materializer = %d, %v", r.n, r.err)
	}
	if r := w.wait(t); r.err != nil {
		t.Fatalf("writer: %v", r.err)
	}
	e.wantState(t, "writer after materializer", run, true, true)
	if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
		t.Fatalf("committed fold = %v, want the pre-write 1000 (stale)", got)
	}
	e.materialize(t, run)
	if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:5000:") {
		t.Fatalf("re-fold = %v", got)
	}
}

func TestRunUsageFoldMaterializerVsRunDeleteLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	orphans := func(label string, ids ...uuid.UUID) {
		t.Helper()
		var s, f int
		if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM run_usage_fold_state WHERE run_id = ANY($1)), (SELECT count(*) FROM run_usage_model_folds WHERE run_id = ANY($1))`, ids).Scan(&s, &f); err != nil {
			t.Fatal(err)
		}
		if s != 0 || f != 0 {
			t.Fatalf("%s: %d state rows and %d fold rows left for deleted runs", label, s, f)
		}
	}

	t.Run("uncommitted delete skips the run", func(t *testing.T) {
		run := e.run(t, "completed")
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		mustExecTx(e.ctx, t, tx, `DELETE FROM runs WHERE id = $1`, run)
		if n := e.materialize(t, run); n != 0 {
			t.Fatalf("materialized %d while the run delete was uncommitted", n)
		}
		if err := tx.Commit(e.ctx); err != nil {
			t.Fatal(err)
		}
		orphans("after commit", run)
	})

	t.Run("delete after (a0) waits for the materializer", func(t *testing.T) {
		run := e.run(t, "completed")
		if err := e.upsert(t, e.q, run, "s", "m", 0, 100); err != nil {
			t.Fatal(err)
		}
		mustExec(e.ctx, t, e.pool, `DELETE FROM run_usage_fold_state WHERE run_id = $1`, run) // no state: a0 selects it
		reached, release := pauseAt(t, "a0")
		mat := e.matBG(t, run)
		mustReach(t, reached)
		del := e.bg(t, func(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
			tag, err := conn.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run)
			return tag.RowsAffected(), err
		})
		del.waitBlocked(t, e)
		release()
		if r := mat.wait(t); r.err != nil || r.n != 1 {
			t.Fatalf("materializer = %d, %v", r.n, r.err)
		}
		if r := del.wait(t); r.err != nil || r.n != 1 {
			t.Fatalf("delete = %d, %v", r.n, r.err)
		}
		orphans("delete after a0", run)
	})

	t.Run("delete of a run holding state while the materializer holds (b)", func(t *testing.T) {
		run := e.run(t, "completed")
		if err := e.upsert(t, e.q, run, "s", "m", 0, 100); err != nil {
			t.Fatal(err)
		}
		reached, release := pauseAt(t, "b")
		mat := e.matBG(t, run)
		mustReach(t, reached)
		del := e.bg(t, func(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
			tag, err := conn.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run)
			return tag.RowsAffected(), err
		})
		del.waitBlocked(t, e)
		release()
		if r := mat.wait(t); r.err != nil || r.n != 1 {
			t.Fatalf("materializer = %d, %v", r.n, r.err)
		}
		if r := del.wait(t); r.err != nil || r.n != 1 {
			t.Fatalf("delete = %d, %v", r.n, r.err)
		}
		orphans("delete during (b)", run)
	})

	t.Run("repo delete cascading over an active batch", func(t *testing.T) {
		userID, repoID := seedAnalyticsViewsUser(e.ctx, t, e.pool, "foldrepo")
		t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
		mustExec(e.ctx, t, e.pool, `UPDATE repos SET enabled = false WHERE id = $1`, repoID)
		var ids []uuid.UUID
		for i := 0; i < 5; i++ {
			id := uuid.New()
			mustExec(e.ctx, t, e.pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, usage_refolded)
				VALUES ($1, $2, $3, $4, 't', 'd', 'completed', true)`, id, userID, repoID, e.nextIID())
			if i%2 == 0 {
				if err := e.upsert(t, e.q, id, "s", "m", 0, 100); err != nil {
					t.Fatal(err)
				}
				mustExec(e.ctx, t, e.pool, `DELETE FROM run_usage_fold_state WHERE run_id = $1`, id)
			}
			ids = append(ids, id)
		}
		reached, release := pauseAt(t, "a0")
		mat := e.matBG(t, ids...)
		mustReach(t, reached)
		del := e.bg(t, func(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
			return store.New(conn).DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: repoID, UserID: userID})
		})
		del.waitBlocked(t, e)
		release()
		mat.wait(t)
		if r := del.wait(t); r.err != nil || r.n != 1 {
			t.Fatalf("repo delete = %d, %v", r.n, r.err)
		}
		orphans("repo delete", ids...)
	})
}

// -----------------------------------------------------------------------------------------

// Atomicity: nothing the materializer did is visible unless it commits.
func TestRunUsageFoldAtomicityLiveDB(t *testing.T) {
	setup := func(e *foldEnv, t *testing.T) uuid.UUID {
		run := e.run(t, "completed")
		if err := e.upsert(t, e.q, run, "s", "model-a", 0, 1000); err != nil {
			t.Fatal(err)
		}
		e.materialize(t, run)
		if err := e.upsert(t, e.q, run, "s", "model-a", 0, 2000); err != nil {
			t.Fatal(err)
		}
		e.wantState(t, "setup", run, true, true)
		return run
	}
	assertIntact := func(e *foldEnv, t *testing.T, run uuid.UUID) {
		t.Helper()
		e.wantState(t, "after interrupted materialization", run, true, true)
		if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
			t.Fatalf("previous fold not intact: %v", got)
		}
	}

	t.Run("context cancelled after (c)", func(t *testing.T) {
		e := newFoldEnv(t)
		run := setup(e, t)
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		restore := store.SetRunUsageFoldStepHook(func(_ context.Context, s string) {
			if s == "c" {
				cancel()
			}
		})
		defer restore()
		if _, err := store.MaterializeRunUsageFolds(ctx, e.pool, []uuid.UUID{run}); err == nil {
			t.Fatal("cancelled materialization returned nil")
		}
		restore()
		assertIntact(e, t, run)
		if n := e.materialize(t, run); n != 1 {
			t.Fatalf("retry materialized %d", n)
		}
	})

	t.Run("backend terminated before commit", func(t *testing.T) {
		e := newFoldEnv(t)
		run := setup(e, t)
		restore := store.SetRunUsageFoldStepHook(func(ctx context.Context, s string) {
			if s != "d" {
				return
			}
			var killed bool
			err := e.pool.QueryRow(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE datname = current_database() AND state = 'idle in transaction' AND query LIKE '%INSERT INTO run_usage_model_folds%'
				ORDER BY query_start DESC LIMIT 1`).Scan(&killed)
			if err != nil || !killed {
				t.Errorf("could not terminate the materializer backend: killed=%t err=%v", killed, err)
			}
		})
		defer restore()
		if _, err := store.MaterializeRunUsageFolds(e.ctx, e.pool, []uuid.UUID{run}); err == nil {
			t.Fatal("materialization on a terminated backend returned nil")
		}
		restore()
		assertIntact(e, t, run)
	})

	t.Run("concurrent snapshot readers", func(t *testing.T) {
		e := newFoldEnv(t)
		run := setup(e, t)
		rr, err := e.pool.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rr.Rollback(context.Background()) }()
		if got := e.foldRows(t, rr, run); len(got) != 1 {
			t.Fatalf("reader setup: %v", got)
		}
		reached, release := pauseAt(t, "d")
		mat := e.matBG(t, run)
		mustReach(t, reached)
		if got := e.foldRows(t, rr, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
			t.Fatalf("REPEATABLE READ reader during an uncommitted replacement saw %v", got)
		}
		if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
			t.Fatalf("fresh-snapshot reader during an uncommitted replacement saw %v", got)
		}
		e.wantState(t, "during replacement", run, true, true)
		release()
		if r := mat.wait(t); r.err != nil || r.n != 1 {
			t.Fatalf("materializer = %d, %v", r.n, r.err)
		}
		if got := e.foldRows(t, rr, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:1000:") {
			t.Fatalf("old snapshot after commit saw %v", got)
		}
		e.wantState(t, "after commit", run, true, false)
		if got := e.foldRows(t, e.pool, run); len(got) != 1 || !strings.HasPrefix(got[0], "model-a:2000:") {
			t.Fatalf("new snapshot after commit saw %v", got)
		}
	})

	t.Run("empty fold commits fresh", func(t *testing.T) {
		e := newFoldEnv(t)
		run := setup(e, t)
		if err := e.q.DeleteRunUsage(e.ctx, run); err != nil {
			t.Fatal(err)
		}
		if n := e.materialize(t, run); n != 1 {
			t.Fatalf("materialized %d", n)
		}
		e.wantState(t, "empty fold", run, true, false)
		if got := e.foldRows(t, e.pool, run); len(got) != 0 {
			t.Fatalf("empty fold expected, got %v", got)
		}
	})
}

// -----------------------------------------------------------------------------------------

// The run-delete races: a claim-shaped writer transaction (BEGIN; SELECT ... FROM runs FOR
// UPDATE; upsert; COMMIT) against a real store delete, in both orders, with a cross_check
// child of the lead in the same repo.
func TestRunUsageFoldWriterVsRunDeleteLiveDB(t *testing.T) {
	type deleter struct {
		name string
		del  func(ctx context.Context, q *store.Queries, e *foldEnv, userID, repoID, connID uuid.UUID) (int64, error)
	}
	deleters := []deleter{
		{"DeleteRepoForUser", func(ctx context.Context, q *store.Queries, e *foldEnv, userID, repoID, _ uuid.UUID) (int64, error) {
			return q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: repoID, UserID: userID})
		}},
		{"DeleteForgeConnectionForUser", func(ctx context.Context, q *store.Queries, e *foldEnv, userID, _, connID uuid.UUID) (int64, error) {
			return q.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: connID, UserID: userID})
		}},
	}
	for _, d := range deleters {
		for _, writerFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/writerFirst=%t", d.name, writerFirst), func(t *testing.T) {
				e := newFoldEnv(t)
				mustExec(e.ctx, t, e.pool, `UPDATE repos SET enabled = false WHERE id = $1`, e.repoID)
				lead := e.run(t, "running")
				child := e.run(t, "completed")
				mustExec(e.ctx, t, e.pool,
					`UPDATE runs SET kind = 'cross_check', issue_iid = NULL, target_run_id = $2, harness = 'codex', report_only = true,
					        budget_wall_seconds = 1800 WHERE id = $1`, child, lead)
				if err := e.upsert(t, e.q, lead, "s", "m", 0, 100); err != nil {
					t.Fatal(err)
				}
				e.materialize(t, child) // child has state fresh, no rows

				writerTx := func(ctx context.Context, tx pgx.Tx) error {
					var id uuid.UUID
					if err := tx.QueryRow(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, lead).Scan(&id); err != nil {
						return err
					}
					return store.New(tx).UpsertRunUsage(ctx, upsertArg(t, lead, "s", "m", 1, 900))
				}

				if writerFirst {
					tx, err := e.pool.Begin(e.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(context.Background()) }()
					if err := writerTx(e.ctx, tx); err != nil {
						t.Fatal(err)
					}
					del := e.bg(t, func(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
						return d.del(ctx, store.New(conn), e, e.userID, e.repoID, e.connID)
					})
					del.waitBlocked(t, e)
					if err := tx.Commit(e.ctx); err != nil {
						t.Fatal(err)
					}
					if r := del.wait(t); r.err != nil || r.n != 1 {
						t.Fatalf("delete = %d, %v", r.n, r.err)
					}
				} else {
					tx, err := e.pool.Begin(e.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(context.Background()) }()
					if n, err := d.del(e.ctx, store.New(tx), e, e.userID, e.repoID, e.connID); err != nil || n != 1 {
						t.Fatalf("delete = %d, %v", n, err)
					}
					w := e.bg(t, func(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
						wtx, err := conn.Begin(ctx)
						if err != nil {
							return 0, err
						}
						defer func() { _ = wtx.Rollback(context.Background()) }()
						if err := writerTx(ctx, wtx); err != nil {
							return 0, err
						}
						return 0, wtx.Commit(ctx)
					})
					w.waitBlocked(t, e)
					if err := tx.Commit(e.ctx); err != nil {
						t.Fatal(err)
					}
					if r := w.wait(t); !errors.Is(r.err, pgx.ErrNoRows) {
						t.Fatalf("writer on a deleted run = %v, want the claim-shaped SELECT to find no row", r.err)
					}
				}
				var states, folds, usage, runs int
				if err := e.pool.QueryRow(e.ctx, `SELECT
					(SELECT count(*) FROM run_usage_fold_state WHERE run_id = ANY($1)),
					(SELECT count(*) FROM run_usage_model_folds WHERE run_id = ANY($1)),
					(SELECT count(*) FROM run_usage WHERE run_id = ANY($1)),
					(SELECT count(*) FROM runs WHERE id = ANY($1))`, []uuid.UUID{lead, child}).Scan(&states, &folds, &usage, &runs); err != nil {
					t.Fatal(err)
				}
				if states+folds+usage+runs != 0 {
					t.Fatalf("leftovers after delete: state=%d folds=%d usage=%d runs=%d", states, folds, usage, runs)
				}
				// The same lead id upserted after the delete is the FK violation, with no state row.
				err := e.upsert(t, e.q, lead, "s", "m", 2, 1)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
					t.Fatalf("upsert after delete = %v, want 23503", err)
				}
				e.wantState(t, "after failed upsert", lead, false, false)
			})
		}
	}
}

// -----------------------------------------------------------------------------------------

// Plan properties of the two statements the protocol leans on, under a generic plan (what a
// prepared pgx statement settles on): the delete's lock EXISTS stays a one-time filter, and the
// fold's run_usage access goes through the primary key, never a sequential scan.
func TestRunUsageFoldPlanPropertiesLiveDB(t *testing.T) {
	e := newFoldEnv(t)
	conn, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	mustExecTx(e.ctx, t, tx, `SET LOCAL plan_cache_mode = force_generic_plan`)
	explain := func(name, prepared, args string) string {
		t.Helper()
		mustExecTx(e.ctx, t, tx, "PREPARE "+name+" AS "+prepared)
		rows, err := tx.Query(e.ctx, "EXPLAIN EXECUTE "+name+"("+args+")", pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			sb.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	id := uuid.New().String()
	del := explain("fold_del", store.DeleteRunUsageSQL, "'"+id+"'")
	if !strings.Contains(del, "One-Time Filter") {
		t.Errorf("DeleteRunUsage generic plan has no One-Time Filter:\n%s", del)
	}
	// The planner may legitimately prefer a sequential scan on a near-empty table; disabling it
	// makes the assertion about reachability: the run_id qual must be pushable into the view's
	// run_usage scan, or the plan would have to read the whole table.
	mustExecTx(e.ctx, t, tx, `SET LOCAL enable_seqscan = off`)
	fold := explain("fold_ins", store.InsertRunUsageModelFoldsSQL, "'{"+id+"}'")
	if !strings.Contains(fold, "run_usage_pkey") {
		t.Errorf("fold insert does not use run_usage_pkey:\n%s", fold)
	}
	if regexp.MustCompile(`Seq Scan on run_usage(\s|$)`).MatchString(fold) {
		t.Errorf("fold insert scans run_usage sequentially:\n%s", fold)
	}
}
