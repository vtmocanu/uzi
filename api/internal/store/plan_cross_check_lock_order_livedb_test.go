package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Database operations and polling have deadlines. On failure, cancel and join
// the contender before rolling back with an independent cleanup deadline.
func TestPlanCrossCheckLeadCheckBeforeChildLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	rollback := func(tx pgx.Tx) {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanupCtx)
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running', worker_id = $2,
        claim_generation = 1 WHERE id = $1`, fx.checkerID, f.workerID)

	checkBlocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(checkBlocker)
	if _, err = checkBlocker.Exec(ctx, `SELECT id FROM cross_checks WHERE id = $1 FOR UPDATE`, fx.crossCheckID); err != nil {
		t.Fatal(err)
	}
	childBlocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(childBlocker)
	if _, err = childBlocker.Exec(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, fx.checkerID); err != nil {
		t.Fatal(err)
	}
	entry, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(entry)
	result := make(chan error, 1)
	go func() {
		lead, lockErr := store.New(entry).LockPlanCrossCheckLeadForVerdict(ctx,
			store.LockPlanCrossCheckLeadForVerdictParams{
				ChildID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1,
			})
		if lockErr == nil && lead.ID != f.runID {
			lockErr = errors.New("shared entry returned the wrong lead")
		}
		result <- lockErr
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-result
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Autocommit probes release any acquired lock immediately and do not
		// leave an aborted transaction after the expected NOWAIT failure.
		_, err = f.pool.Exec(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, f.runID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			break
		}
		if err != nil {
			t.Fatalf("probe lead lock: %v", err)
		}
		select {
		case err = <-result:
			joined = true
			t.Fatalf("shared entry returned while check was blocked: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	// Lead is held by the contender while our own check blocker is still
	// active; the shared entry must not have returned.
	select {
	case err = <-result:
		joined = true
		t.Fatalf("shared entry returned while check was blocked: %v", err)
	default:
	}
	if err = checkBlocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		joined = true
		if err != nil {
			t.Fatalf("shared entry with child held: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("shared entry waited on child: %v", ctx.Err())
	}
	for _, probe := range []struct {
		name  string
		query string
		id    any
	}{
		{"lead", `SELECT id FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, f.runID},
		{"check", `SELECT id FROM cross_checks WHERE id = $1 FOR UPDATE NOWAIT`, fx.crossCheckID},
		{"child", `SELECT id FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, fx.checkerID},
	} {
		_, err = f.pool.Exec(ctx, probe.query, probe.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("%s lock not held before caller acquires child: %v", probe.name, err)
		}
	}
}
