package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func sourceEndCustody(t *testing.T, e interlockLiveDB, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY id)::text,'[]') FROM recovery_custody_holds h WHERE run_id=$1", id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Observe only the two owned database sessions. The context bounds attempts;
// a query failure or unexpected contender completion stops the observation.
func sourceEndWaitBlocked(ctx context.Context, pool *pgxpool.Pool, queryPID, holderPID uint32, done <-chan error) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return errors.Join(errors.New("contender finished before lock contention was observed"), err)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			var blocked bool
			err := pool.QueryRow(ctx, "SELECT COALESCE((SELECT wait_event_type='Lock' AND $2::int=ANY(pg_blocking_pids(pid)) FROM pg_stat_activity WHERE pid=$1),false)", int64(queryPID), int64(holderPID)).Scan(&blocked)
			if err != nil {
				return err
			}
			if blocked {
				return nil
			}
		}
	}
}

func TestSourceEndDeclinedDirectSQLRefusalWithoutRunLockWaitLiveDB(t *testing.T) {
	e, svc, w, id := sourceEndFixture(t, "running")
	if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); err != nil || !applied {
		t.Fatalf("hold applied=%v err=%v", applied, err)
	}
	before := sourceEndRead(t, e, id)
	custody := sourceEndCustody(t, e, id)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(e.ctx) }()
	var locked uuid.UUID
	if err := holder.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	queryConn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer queryConn.Release()
	// H remains open and holds only the run lock. This proves refusal without
	// waiting on that lock, not independent candidate-guard discrimination:
	// demand-driven CTE evaluation may reject before consuming candidates.
	_, err = store.New(queryConn).ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: w.UserID, GlobalTimeoutSeconds: 3600})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("direct Resume (timeouts are failures): %v", err)
	}
	if !reflect.DeepEqual(before, sourceEndRead(t, e, id)) || custody != sourceEndCustody(t, e, id) {
		t.Fatal("direct refusal changed run or custody")
	}
}

func TestSourceEndDeclinedDirectSQLSyntheticFinalRecheckLiveDB(t *testing.T) {
	e, svc, w, id := sourceEndFixture(t, "running")
	if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); err != nil || !applied {
		t.Fatalf("hold applied=%v err=%v", applied, err)
	}
	// Synthetic defensive boundary, not a reachable producer race: the real
	// park writer rejects an already released generic recovery_wait run.
	e.exec(t, "UPDATE runs SET recovery_wait_detail=NULL WHERE id=$1", id)
	before := sourceEndRead(t, e, id)
	custody := sourceEndCustody(t, e, id)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(e.ctx) }()
	var locked uuid.UUID
	if err := holder.QueryRow(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE", w.ID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if err := holder.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	queryConn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer queryConn.Release()
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, err := store.New(queryConn).ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: w.UserID, GlobalTimeoutSeconds: 3600})
		done <- err
	}()
	defer func() { cancel(); _ = holder.Rollback(e.ctx); <-joined }()
	if err := sourceEndWaitBlocked(ctx, e.pool, queryConn.Conn().PgConn().PID(), holder.Conn().PgConn().PID(), done); err != nil {
		t.Fatal(err)
	}
	// The statement snapshot saw no typed source hold and hence an empty
	// source-worker lock set. Change only detail; the final guard must refuse.
	if _, err := holder.Exec(ctx, "UPDATE runs SET recovery_wait_detail=$2 WHERE id=$1", id, RecoveryDetailSourceEndDeclined); err != nil {
		t.Fatal(err)
	}
	committed, err := store.New(holder).GetRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	expected := before
	expected.RecoveryWaitDetail = committed.RecoveryWaitDetail
	if !reflect.DeepEqual(expected, committed) || committed.RecoveryWaitDetail.String != RecoveryDetailSourceEndDeclined {
		t.Fatal("synthetic holder changed more than detail")
	}
	if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("final recheck refusal: %v", err)
	}
	if !reflect.DeepEqual(committed, sourceEndRead(t, e, id)) || custody != sourceEndCustody(t, e, id) {
		t.Fatal("final recheck changed committed run or custody")
	}
}
