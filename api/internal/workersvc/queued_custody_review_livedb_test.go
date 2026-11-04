package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type queuedReviewBeginner struct {
	pool  *pgxpool.Pool
	calls atomic.Int32
}

func (b *queuedReviewBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	b.calls.Add(1)
	return b.pool.Begin(ctx)
}

func TestQueuedCustodyDiscoveryOwnHoldBoundLiveDB(t *testing.T) {
	for _, own := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("own_%d_limit_2", own), func(t *testing.T) {
			generations := make([]int64, own)
			for i := range generations {
				generations[i] = int64(i + 1)
			}
			c, id := seededQueuedCustody(t, 3, 3, 4, generations, 2)
			c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", c.workerID)
			before := observeQueuedCustody(t, c.env, id)
			beginner := &queuedReviewBeginner{pool: c.env.pool}
			c.svc.SetTxBeginner(beginner)
			now := time.Now()
			count, err := c.svc.parkStaleQueuedCustody(c.env.ctx, pgconv.Time(now.Add(-time.Minute)), now)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if own >= 2 {
				want = 1
			}
			if count != want || int64(beginner.calls.Load()) != want {
				t.Fatalf("own=%d: parked=%d transactions=%d want=%d; below-bound work must not enter the lock transaction", own, count, beginner.calls.Load(), want)
			}
			requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), want == 1)
		})
	}
}

func TestQueuedCustodyWorkerDeletionGuardsLiveDB(t *testing.T) {
	for _, mode := range []string{"current_claim_hold", "older_holds_without_live_worker"} {
		t.Run(mode, func(t *testing.T) {
			var c *claimantExhaustion
			var id uuid.UUID
			if mode == "current_claim_hold" {
				env := setupCodexLiveDB(t)
				c = newClaimantExhaustion(t, env, true, 1)
				id = c.seedKind(t, "issue")
				c.claimAndRun(t, c.wkr, id, 1, true)
				// The cache-only reader leaves optional queued classification disabled.
				// The real omission writer spends the last retry and leaves a queued run.
				c.omitHeartbeat(t, c.workerID)
				if facts := c.facts(t, id); facts.status != "queued" || facts.requeues != 1 {
					t.Fatalf("real requeue precondition: %+v", facts)
				}
				queuedCustodyCache(t, env, c.svc, 1)
			} else {
				// Historical custody need not have a live pointer to the current worker.
				// The independent nonterminal-run delete guard must still protect it.
				c, id = seededQueuedCustody(t, 3, 3, 3, []int64{1, 2}, 2)
			}
			before := observeQueuedCustody(t, c.env, id)
			if err := c.svc.DeleteWorker(c.env.ctx, c.userID, c.workerID); !errors.Is(err, ErrWorkerHasActiveRuns) {
				t.Fatalf("managed delete must refuse the queued continuation: %v", err)
			}
			if mode == "current_claim_hold" {
				_, err := c.env.q.DeleteWorkerForUser(c.env.ctx, store.DeleteWorkerForUserParams{ID: c.workerID, UserID: c.userID})
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23503" || !strings.Contains(pgErr.ConstraintName, "live_worker_id") {
					t.Fatalf("the actual current-generation custody FK must backstop deletion: %v", err)
				}
			}
			var sameWorker bool
			if err := c.env.pool.QueryRow(c.env.ctx, "SELECT worker_id=$2 FROM runs WHERE id=$1", id, c.workerID).Scan(&sameWorker); err != nil || !sameWorker {
				t.Fatalf("queued continuation was orphaned: same_worker=%v err=%v", sameWorker, err)
			}
			requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
		})
	}
}
