package workersvc

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Pause after a real transaction opens, rather than before the goroutine calls
// SetState, so both sessions demonstrably overlap before the lock is released.
type idleStallTxBarrier struct {
	pool interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	ready   chan<- struct{}
	release <-chan struct{}
}

func (b idleStallTxBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	select {
	case b.ready <- struct{}{}:
	case <-ctx.Done():
		_ = tx.Rollback(context.Background())
		return nil, ctx.Err()
	}
	select {
	case <-b.release:
		return tx, nil
	case <-ctx.Done():
		_ = tx.Rollback(context.Background())
		return nil, ctx.Err()
	}
}

func TestIdleStallReportPromotionResumeRaceLiveDB(t *testing.T) {
	for _, action := range []string{"resume", "timer", "early"} {
		t.Run(action, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 3)
			svc.p.RunCodexIdleStallMaxParks = 3
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
			defer cancel()
			ready := make(chan struct{}, 1)
			release := make(chan struct{})
			svc.SetTxBeginner(idleStallTxBarrier{e.pool, ready, release})
			type result struct {
				applied bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				_, applied, err := svc.SetState(ctx, store.Worker{ID: w}, id, idleReport(1, nil))
				done <- result{applied, err}
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// The report read a running claim, but has not locked it. A competing
			// accepted park and owner/timer transition supersede that observation.
			e.exec(t, "UPDATE runs SET status='recovery_wait',recovery_wait_cause='codex_idle_stall',idle_stall_park_count=2,recovery_wait_count=2,recovery_retry_not_before=now()-interval '1 second' WHERE id=$1", id)
			switch action {
			case "resume":
				e.exec(t, "UPDATE runs SET recovery_retry_not_before=NULL WHERE id=$1", id)
				if _, err := e.q.ResumeCodexIdleStall(ctx, store.ResumeCodexIdleStallParams{ID: id, UserID: e.userID}); err != nil {
					t.Fatal(err)
				}
			case "timer":
				if _, err := e.q.PromoteRecoveryWaitRuns(ctx, pgconv.Time(time.Now())); err != nil {
					t.Fatal(err)
				}
			case "early":
				n, err := e.q.PromoteRecoveryWaitRunNow(ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: e.userID})
				if err != nil || n != 1 {
					t.Fatalf("early promotion %d %v", n, err)
				}
			}
			before, err := e.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			r := <-done
			if r.err != nil || r.applied {
				t.Fatalf("superseded report applied=%v err=%v", r.applied, r.err)
			}
			after, err := e.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("superseded report changed promoted row")
			}
		})
	}
}

func TestIdleStallCredentialParksPreserveCountLiveDB(t *testing.T) {
	for _, quarantined := range []bool{false, true} {
		env := setupCodexLiveDB(t)
		fx, w, _ := runningAccountParkFixture(t, env)
		env.exec("UPDATE runs SET idle_stall_park_count=2 WHERE id=$1", fx.runID)
		if quarantined {
			setCoordState(t, env, fx.accountID, "quarantined")
		}
		before := mustRun(t, env, fx.runID)
		_, applied, err := reportAccountPark(t, fx, w)
		if err != nil || !applied {
			t.Fatalf("credential park applied=%v err=%v", applied, err)
		}
		after := assertAccountParkCustody(t, fx, before, quarantined)
		if after.IdleStallParkCount != 2 {
			t.Fatal("credential park changed idle streak")
		}
	}
}
