package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeOriginAdmissionStaleDueCASLiveDB(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			ctx, cancel := context.WithTimeout(fx.ctx, 20*time.Second)
			defer cancel()
			bounded := *fx
			bounded.ctx = ctx
			w := fx.worker("due-cas", 0, false)
			first, second := fx.unclaimedRun(), fx.unclaimedRun()
			forgeAdmissionCooldown(fx, w, true)
			holder, err := fx.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Release()
			waiter, err := fx.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer waiter.Release()
			tx, err := holder.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			admitted, err := forgeAdmissionClaim(&bounded, store.New(tx), w)
			if err != nil || (admitted.ID != first && admitted.ID != second) {
				t.Fatalf("first due claim: %s %v", admitted.ID, err)
			}
			type result struct {
				run store.Run
				err error
			}
			results := make(chan result, 1)
			drained := false
			defer func() {
				// Unlock before draining on every failure path. Rollback and the
				// single waiter are context bounded; neither outlives this test.
				cleanup, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				_ = tx.Rollback(cleanup)
				cancel()
				if !drained {
					<-results
				}
			}()
			go func() {
				run, err := forgeAdmissionClaim(&bounded, store.New(waiter), w)
				results <- result{run, err}
			}()
			// The worker row is the only shared lock reached before target.
			// Observing this wait proves the second statement took its snapshot
			// while the first reservation was still uncommitted. No sleeps.
			for {
				var blocked bool
				err := fx.pool.QueryRow(ctx, "SELECT $1::integer = ANY(pg_blocking_pids($2::integer))",
					int(holder.Conn().PgConn().PID()), int(waiter.Conn().PgConn().PID())).Scan(&blocked)
				if err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case got := <-results:
					drained = true
					t.Fatalf("waiter returned before worker-lock barrier: %s %v", got.run.ID, got.err)
				default:
				}
			}
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			got := <-results
			drained = true
			if err != nil {
				t.Fatal(err)
			}
			winner := admitted.ID
			if commit {
				if !errors.Is(got.err, pgx.ErrNoRows) {
					t.Fatalf("stale due CAS admitted second run: %s %v", got.run.ID, got.err)
				}
			} else {
				if got.err != nil || (got.run.ID != first && got.run.ID != second) {
					t.Fatalf("rollback waiter failed to claim: %s %v", got.run.ID, got.err)
				}
				winner = got.run.ID
			}
			loser := first
			if loser == winner {
				loser = second
			}
			var exact bool
			err = fx.pool.QueryRow(ctx, `SELECT
				EXISTS (SELECT 1 FROM runs WHERE id=$1::uuid AND status='claimed' AND claim_generation=1 AND worker_id=$3)
				AND EXISTS (SELECT 1 FROM runs WHERE id=$2::uuid AND status='queued' AND claim_generation=0 AND worker_id IS NULL)
				AND (SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1::uuid)=1
				AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds WHERE run_id=$2::uuid)
				AND NOT EXISTS (SELECT 1 FROM run_forge_origins WHERE run_id=$2::uuid)
				AND c.reservation_run_id=$1::uuid AND c.reservation_generation=1
				AND c.observation_version=1 AND c.reservation_version=1
				AND c.reservation_incarnation=c.incarnation AND c.reservation_observation_version=1
				AND m.generation=1 AND m.reservation_incarnation=c.incarnation AND m.reservation_version=1
				FROM forge_origin_cooldowns c JOIN run_forge_origins m ON m.run_id=c.reservation_run_id
				WHERE c.user_id=$4 AND c.worker_id=$3 AND c.origin='https://forge.e2e'`,
				winner, loser, w, fx.userID).Scan(&exact)
			if err != nil || !exact {
				t.Fatalf("CAS winner %s loser %s reservation/G/custody effects: exact=%v err=%v", winner, loser, exact, err)
			}
		})
	}
}
