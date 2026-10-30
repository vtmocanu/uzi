package workersvc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeCloneSuccessCapabilityRefreshUnderLockLiveDB(t *testing.T) {
	for _, downgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("downgrade=%t", downgrade), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			e.ctx = ctx
			w := e.seedWorker(t, []string{ForgeCloneSuccessFeature})
			id := e.fpSeedRunning(t, w, 1)
			bindForgeSuccessReservation(t, e, id, w)
			row, err := e.q.GetForgeOriginReservation(ctx, store.GetForgeOriginReservationParams{RunID: id, WorkerID: w, Generation: 1})
			if err != nil {
				t.Fatal(err)
			}
			worker := store.Worker{ID: w, UserID: e.userID, ProtocolCapabilities: []string{ForgeCloneSuccessFeature}}
			req := ForgeCloneSuccessRequest{ClaimGeneration: 1, Incarnation: uuid.UUID(row.ReservationIncarnation.Bytes), Version: 7, ObservationVersion: row.ReservationObservationVersion.Int64}
			if !downgrade {
				worker.ProtocolCapabilities = nil // Authentication observed the previous advertisement.
			}
			holder, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rollback := func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = holder.Rollback(cleanup)
			}
			defer rollback()
			var pid int
			if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			caps := []string{ForgeCloneSuccessFeature}
			if downgrade {
				caps = []string{}
			}
			if _, err := holder.Exec(ctx, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, caps); err != nil {
				t.Fatal(err)
			}
			svc := e.forgeParkService(t, 6)
			type outcome struct {
				applied bool
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				applied, err := svc.ReportForgeCloneSuccess(ctx, worker, id, req)
				done <- outcome{applied, err}
			}()
			joined := false
			defer func() {
				cancel()
				rollback()
				if !joined {
					<-done
				}
			}()
			// Poll only until this report demonstrably waits for the in-flight refresh.
			// The context bounds readiness and the report; cleanup joins it on every exit.
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := e.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case out := <-done:
					joined = true
					t.Fatalf("observation escaped worker refresh lock: %+v", out)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			out := <-done
			joined = true
			if downgrade {
				if out.applied || out.err != ErrInvalidState || e.originCount(t, w) != 1 {
					t.Fatalf("stale capability cleared cooldown: %+v", out)
				}
			} else if out.err != nil || !out.applied || e.originCount(t, w) != 0 {
				t.Fatalf("current capability not observed: %+v", out)
			}
		})
	}
}
