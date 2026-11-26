package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700 (migration 00543): the run_usage writers in this package (foldUsageFrames for
// result frames, progress notes and summary passes, and RefoldRunUsage) must mark the run's
// stored fold stale. Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

type foldLockFixture struct {
	env   codexTestEnv
	svc   *Service
	wkr   store.Worker
	runID uuid.UUID
}

// newFoldLockFixture seeds a run, delivers the Claude note scenario through the real append
// path, then makes the run terminal and materializes its fold.
func newFoldLockFixture(t *testing.T) *foldLockFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	gen := int64(1)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, claudeNoteScenario(), &gen); err != nil {
		t.Fatalf("append: %v", err)
	}
	env.exec(`UPDATE runs SET status = 'completed', usage_refolded = true WHERE id = $1`, runID)
	f := &foldLockFixture{env: env, svc: svc, wkr: wkr, runID: runID}
	f.materialize(t)
	f.wantStale(t, "after initial materialize", false)
	return f
}

func (f *foldLockFixture) materialize(t *testing.T) {
	t.Helper()
	if n, err := store.MaterializeRunUsageFolds(f.env.ctx, f.env.pool, []uuid.UUID{f.runID}); err != nil || n != 1 {
		t.Fatalf("materialize = %d, %v", n, err)
	}
}

func (f *foldLockFixture) wantStale(t *testing.T, label string, want bool) {
	t.Helper()
	var stale bool
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT stale FROM run_usage_fold_state WHERE run_id = $1`, f.runID).Scan(&stale); err != nil {
		t.Fatalf("%s: state row: %v", label, err)
	}
	if stale != want {
		t.Fatalf("%s: stale = %t, want %t", label, stale, want)
	}
}

func (f *foldLockFixture) run(t *testing.T) store.Run {
	t.Helper()
	return mustRun(t, f.env, f.runID)
}

func TestRunUsageFoldInvalidatedByFrameFoldsLiveDB(t *testing.T) {
	f := newFoldLockFixture(t)
	haiku := map[string]any{"claude-haiku-4-5-20251001": mu(10, 1, nil)}
	cases := []struct {
		name  string
		frame IncomingMessage
	}{
		{"progress note", noteFrame(50, "late note", "m1", haiku)},
		{"summary pass", summaryFrame(51, "plan", haiku)},
		{"result frame", lineResult(52, map[string]any{"claude-opus-4-8": mu(5, 5, map[string]any{"costUSD": 0.01})})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.wantStale(t, "before", false)
			if err := foldUsageFrames(f.env.ctx, f.env.q, f.run(t), []IncomingMessage{c.frame}); err != nil {
				t.Fatalf("foldUsageFrames: %v", err)
			}
			f.wantStale(t, "after the fold", true)
			f.materialize(t)
		})
	}
	// A re-delivered frame (GREATEST no-op) still marks the run stale: the lock is taken before
	// the merge decides whether anything changes.
	if err := foldUsageFrames(f.env.ctx, f.env.q, f.run(t), []IncomingMessage{cases[0].frame}); err != nil {
		t.Fatal(err)
	}
	f.wantStale(t, "after a no-op redelivery", true)
}

func TestRunUsageFoldInvalidatedByRefoldLiveDB(t *testing.T) {
	t.Run("reinsertion", func(t *testing.T) {
		f := newFoldLockFixture(t)
		if err := RefoldRunUsage(f.env.ctx, f.env.pool, f.env.q, f.run(t)); err != nil {
			t.Fatalf("RefoldRunUsage: %v", err)
		}
		f.wantStale(t, "after refold", true)
		assertTotals(t, "refold result", f.env.usageTotals(t, f.runID), claudeNoteTotals)
		f.materialize(t)
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_usage_model_folds WHERE run_id = $1`, f.runID).Scan(&n); err != nil || n == 0 {
			t.Fatalf("fold rows after re-materialize = %d, %v", n, err)
		}
	})

	t.Run("zero results", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		_, runID := env.seedNoteRun(t, "claude")
		env.exec(`UPDATE runs SET status = 'completed', usage_refolded = false WHERE id = $1`, runID)
		env.exec(`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens) VALUES ($1, 's', 'collapsed', 0, 99)`, runID)
		if n, err := store.MaterializeRunUsageFolds(env.ctx, env.pool, []uuid.UUID{runID}); err != nil || n != 0 {
			t.Fatalf("a not-yet-refolded run materialized: %d, %v", n, err)
		}
		env.exec(`UPDATE runs SET usage_refolded = true WHERE id = $1`, runID)
		if n, err := store.MaterializeRunUsageFolds(env.ctx, env.pool, []uuid.UUID{runID}); err != nil || n != 1 {
			t.Fatalf("materialize = %d, %v", n, err)
		}
		if err := RefoldRunUsage(env.ctx, env.pool, env.q, mustRun(t, env, runID)); err != nil {
			t.Fatalf("RefoldRunUsage: %v", err)
		}
		var stale bool
		var rows int
		if err := env.pool.QueryRow(env.ctx, `SELECT (SELECT stale FROM run_usage_fold_state WHERE run_id = $1), (SELECT count(*) FROM run_usage WHERE run_id = $1)`, runID).Scan(&stale, &rows); err != nil {
			t.Fatal(err)
		}
		if !stale || rows != 0 {
			t.Fatalf("after a zero-result refold: stale=%t run_usage rows=%d, want stale and 0", stale, rows)
		}
	})

	t.Run("deleted run returns nil without marking", func(t *testing.T) {
		f := newFoldLockFixture(t)
		run := f.run(t)
		f.env.exec(`DELETE FROM runs WHERE id = $1`, f.runID)
		if err := RefoldRunUsage(f.env.ctx, f.env.pool, f.env.q, run); err != nil {
			t.Fatalf("refold of a deleted run = %v, want nil", err)
		}
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_usage_fold_state WHERE run_id = $1`, f.runID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("state rows for a deleted run = %d, %v", n, err)
		}
	})
}

// waitLockWait polls until some backend waits on a Lock inside fn_run_usage_fold_lock or in
// the refold's runs-row lock (LockRunForUsageRefold).
func (f *foldLockFixture) waitLockWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND (query LIKE '%fn_run_usage_fold_lock%' OR query LIKE '%FROM runs WHERE id = $1 FOR UPDATE%')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no backend blocked on the fold lock")
}

func (f *foldLockFixture) usage(t *testing.T) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(string_agg(session_id||'/'||model||'/'||lineage_epoch||'='||input_tokens, ',' ORDER BY session_id, model, lineage_epoch), '') FROM run_usage WHERE run_id = $1`, f.runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *foldLockFixture) upsertArg(model string, epoch int32, in int64) store.UpsertRunUsageParams {
	var cost pgtype.Numeric
	_ = cost.Scan("0")
	return store.UpsertRunUsageParams{
		RunID: f.runID, SessionID: "writer", Model: model, LineageEpoch: epoch, InputTokens: in,
		CostUsd: cost, Harness: "claude", CostStatus: "metered", UsageBasis: "per_leg",
	}
}

// refold vs a concurrent writer, both orders, for an existing key and a new key.
func TestRunUsageFoldRefoldVsWriterLiveDB(t *testing.T) {
	for _, keyKind := range []string{"existing key", "new key"} {
		arg := func(f *foldLockFixture) store.UpsertRunUsageParams {
			if keyKind == "new key" {
				return f.upsertArg("writer-model", 77, 12345)
			}
			return f.upsertArg("claude-haiku-4-5-20251001", 2, 999999) // the lead's own haiku leg row
		}
		t.Run(keyKind+"/writer holds the lock first", func(t *testing.T) {
			f := newFoldLockFixture(t)
			a := arg(f)
			if keyKind == "existing key" {
				a.SessionID = f.sessionID(t)
			}
			tx, err := f.env.pool.Begin(f.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := f.env.q.WithTx(tx).UpsertRunUsage(f.env.ctx, a); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- RefoldRunUsage(f.env.ctx, f.env.pool, f.env.q, f.run(t)) }()
			f.waitLockWait(t)
			if err := tx.Commit(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("refold: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("refold did not finish after the writer committed")
			}
			// Sequential result: the writer committed first, then the refold rebuilt from frames.
			assertTotals(t, "refold after writer", f.env.usageTotals(t, f.runID), claudeNoteTotals)
			f.wantStale(t, "after the race", true)
		})
		t.Run(keyKind+"/refold holds the lock first", func(t *testing.T) {
			f := newFoldLockFixture(t)
			a := arg(f)
			if keyKind == "existing key" {
				a.SessionID = f.sessionID(t)
			}
			// The refold's first two statements, held open: runs row FOR UPDATE, then the delete.
			tx, err := f.env.pool.Begin(f.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			qtx := f.env.q.WithTx(tx)
			if _, err := qtx.LockRunForUsageRefold(f.env.ctx, f.runID); err != nil {
				t.Fatal(err)
			}
			if err := qtx.DeleteRunUsage(f.env.ctx, f.runID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- f.env.q.UpsertRunUsage(f.env.ctx, a) }()
			f.waitLockWait(t)
			if err := tx.Commit(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("writer: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("writer did not finish after the refold committed")
			}
			// Sequential result: the delete committed first, then the writer's row alone remains.
			want := fmt.Sprintf("%s/%s/%d=%d", a.SessionID, a.Model, a.LineageEpoch, a.InputTokens)
			if got := f.usage(t); got != want {
				t.Fatalf("run_usage = %q, want %q", got, want)
			}
			f.wantStale(t, "after the race", true)
		})
	}
}

func (f *foldLockFixture) sessionID(t *testing.T) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT session_id FROM runs WHERE id = $1`, f.runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Writers in this package (the progress-note upsert through foldUsageFrames, and RefoldRunUsage)
// against a real repo delete and a forge-connection delete, in both orders, with a cross_check
// child of the run in the same repo. Whatever the order: no deadlock, no state/fold/usage row
// without its run, and a writer that lost the race sees a missing run (23503 for the note
// upsert, nil for the refold) rather than resurrecting a state row.
func TestRunUsageFoldWritersVsRunDeleteLiveDB(t *testing.T) {
	type deleter struct {
		name string
		del  func(ctx context.Context, q *store.Queries, userID, repoID, connID uuid.UUID) (int64, error)
	}
	deleters := []deleter{
		{"DeleteRepoForUser", func(ctx context.Context, q *store.Queries, userID, repoID, _ uuid.UUID) (int64, error) {
			return q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: repoID, UserID: userID})
		}},
		{"DeleteForgeConnectionForUser", func(ctx context.Context, q *store.Queries, userID, _, connID uuid.UUID) (int64, error) {
			return q.DeleteForgeConnectionForUser(ctx, store.DeleteForgeConnectionForUserParams{ID: connID, UserID: userID})
		}},
	}
	type writer struct {
		name string
		// run performs the write through q (a pool-backed or tx-backed Queries).
		run func(ctx context.Context, f *foldLockFixture, q *store.Queries, run store.Run, inTx bool) error
		// wantAfterDelete checks the error a writer that ran after the delete committed sees.
		wantAfterDelete func(t *testing.T, err error)
	}
	haiku := map[string]any{"claude-haiku-4-5-20251001": mu(10, 1, nil)}
	writers := []writer{
		{"progress note", func(ctx context.Context, f *foldLockFixture, q *store.Queries, run store.Run, _ bool) error {
			return foldUsageFrames(ctx, q, run, []IncomingMessage{noteFrame(60, "racing note", "m-race", haiku)})
		}, func(t *testing.T, err error) {
			t.Helper()
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
				t.Fatalf("note upsert on a deleted run = %v, want 23503", err)
			}
		}},
		{"refold", func(ctx context.Context, f *foldLockFixture, q *store.Queries, run store.Run, inTx bool) error {
			return refoldWith(ctx, f, q, run, inTx)
		}, func(t *testing.T, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("refold of a deleted run = %v, want nil", err)
			}
		}},
	}
	for _, d := range deleters {
		for _, w := range writers {
			for _, writerFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/writerFirst=%t", d.name, w.name, writerFirst), func(t *testing.T) {
					f := newFoldLockFixture(t)
					var repoID, connID uuid.UUID
					if err := f.env.pool.QueryRow(f.env.ctx, `SELECT r.repo_id, p.connection_id FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.id = $1`, f.runID).Scan(&repoID, &connID); err != nil {
						t.Fatal(err)
					}
					userID := f.wkr.UserID
					f.env.exec(`UPDATE repos SET enabled = false WHERE id = $1`, repoID)
					child := f.env.seedFoldRun(t, userID, f.wkr.ID, repoID, 1, "sess-child-"+uuid.NewString())
					f.env.exec(`UPDATE runs SET kind = 'cross_check', issue_iid = NULL, target_run_id = $2, harness = 'codex', report_only = true,
					        budget_wall_seconds = 1800, status = 'completed' WHERE id = $1`, child, f.runID)
					run := f.run(t)

					holder, err := f.env.pool.Begin(f.env.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = holder.Rollback(context.Background()) }()

					type result struct {
						n   int64
						err error
					}
					done := make(chan result, 1)
					bg := func(ownConn bool, fn func(ctx context.Context, q *store.Queries) (int64, error)) {
						conn, err := f.env.pool.Acquire(f.env.ctx)
						if err != nil {
							t.Fatal(err)
						}
						pid := int64(conn.Conn().PgConn().PID())
						go func() {
							defer conn.Release()
							n, err := fn(f.env.ctx, store.New(conn))
							done <- result{n, err}
						}()
						if !ownConn {
							// RefoldRunUsage opens its own transaction from the pool, so this
							// connection's backend is not the one that blocks.
							f.waitLockWait(t)
							return
						}
						deadline := time.Now().Add(15 * time.Second)
						for time.Now().Before(deadline) {
							var wt *string
							if err := f.env.pool.QueryRow(f.env.ctx, `SELECT wait_event_type FROM pg_stat_activity WHERE pid::bigint = $1`, pid).Scan(&wt); err != nil {
								t.Fatal(err)
							}
							if wt != nil && *wt == "Lock" {
								return
							}
							select {
							case r := <-done:
								t.Fatalf("operation finished (%v) instead of blocking", r.err)
							case <-time.After(20 * time.Millisecond):
							}
						}
						t.Fatal("operation never blocked on a lock")
					}
					wait := func() result {
						select {
						case r := <-done:
							return r
						case <-time.After(20 * time.Second):
							t.Fatal("operation did not finish (deadlock or lost wake-up)")
							return result{}
						}
					}

					if writerFirst {
						if err := w.run(f.env.ctx, f, f.env.q.WithTx(holder), run, true); err != nil {
							t.Fatalf("writer: %v", err)
						}
						bg(true, func(ctx context.Context, q *store.Queries) (int64, error) {
							return d.del(ctx, q, userID, repoID, connID)
						})
						if err := holder.Commit(f.env.ctx); err != nil {
							t.Fatal(err)
						}
						if r := wait(); r.err != nil || r.n != 1 {
							t.Fatalf("delete = %d, %v", r.n, r.err)
						}
					} else {
						if n, err := d.del(f.env.ctx, f.env.q.WithTx(holder), userID, repoID, connID); err != nil || n != 1 {
							t.Fatalf("delete = %d, %v", n, err)
						}
						bg(w.name != "refold", func(ctx context.Context, q *store.Queries) (int64, error) {
							return 0, w.run(ctx, f, q, run, false)
						})
						if err := holder.Commit(f.env.ctx); err != nil {
							t.Fatal(err)
						}
						w.wantAfterDelete(t, wait().err)
					}

					var states, folds, usage, runs int
					if err := f.env.pool.QueryRow(f.env.ctx, `SELECT
						(SELECT count(*) FROM run_usage_fold_state WHERE run_id = ANY($1)),
						(SELECT count(*) FROM run_usage_model_folds WHERE run_id = ANY($1)),
						(SELECT count(*) FROM run_usage WHERE run_id = ANY($1)),
						(SELECT count(*) FROM runs WHERE id = ANY($1))`, []uuid.UUID{f.runID, child}).Scan(&states, &folds, &usage, &runs); err != nil {
						t.Fatal(err)
					}
					if states+folds+usage+runs != 0 {
						t.Fatalf("leftovers after delete: state=%d folds=%d usage=%d runs=%d", states, folds, usage, runs)
					}
				})
			}
		}
	}
}

// refoldWith runs RefoldRunUsage's body on q. RefoldRunUsage opens its own transaction from a
// pool, so for a transaction-held writer the same sequence is replayed on q's transaction: the
// runs-row lock, then the delete (the refold's first two statements).
func refoldWith(ctx context.Context, f *foldLockFixture, q *store.Queries, run store.Run, inTx bool) error {
	if !inTx {
		return RefoldRunUsage(ctx, f.env.pool, f.env.q, run)
	}
	if _, err := q.LockRunForUsageRefold(ctx, run.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	return q.DeleteRunUsage(ctx, run.ID)
}
