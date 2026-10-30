package workersvc

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestForgeLegacyCompletionReservationSerializationLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprintf("permit=%t", permit), func(t *testing.T) {
			f := seedForgeCompletion(t, "issue", permit)
			ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
			defer cancel()
			f.req.CompletionFinalHead = nil
			f.w.ProtocolCapabilities = nil
			if !permit {
				f.req.ClaimGeneration = nil // The actual legacy report omits generation too.
			}
			f.e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", f.w.ID)
			holder, err := f.e.pool.Begin(ctx)
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
			if _, err := holder.Exec(ctx, "UPDATE forge_origin_cooldowns SET observation_version=observation_version+1 WHERE worker_id=$1", f.w.ID); err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result StateReportResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := f.svc.SetStateReportWithReconciliation(ctx, f.w, f.id, f.req)
				done <- outcome{result, err}
			}()
			joined := false
			defer func() {
				cancel()
				rollback()
				if !joined {
					<-done
				}
			}()
			// One bounded report and a deadline-bounded readiness poll; cleanup always
			// releases the owned transaction and joins the report, including on Fatal.
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := f.e.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case out := <-done:
					joined = true
					t.Fatalf("completion escaped reservation lock: %+v %v", out.result, out.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if f.e.runStatus(t, f.id) != "running" {
				t.Fatal("completion visible before reservation lifecycle commits")
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			out := <-done
			joined = true
			if out.err != nil || !out.result.Applied || out.result.CompletedPublicationReceipt != nil {
				t.Fatalf("legacy completion: %+v %v", out.result, out.err)
			}
			var preserved bool
			if err := f.e.pool.QueryRow(ctx, `SELECT c.observation_version=2 AND c.reservation_run_id IS NULL
    AND c.reservation_generation IS NULL AND c.reservation_incarnation IS NULL
    AND c.reservation_version IS NULL AND c.reservation_observation_version IS NULL
    AND r.status='completed' AND r.completion_final_head IS NULL AND h.completion_identity IS NULL
    AND ($2::boolean = EXISTS(SELECT 1 FROM run_completion_permits p WHERE p.run_id=r.id AND p.consumed_at IS NOT NULL))
    FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id
    JOIN forge_origin_cooldowns c ON c.worker_id=r.worker_id WHERE r.id=$1`, f.id, permit).Scan(&preserved); err != nil || !preserved {
				t.Fatalf("legacy must preserve new failure and release reservation: preserved=%v err=%v", preserved, err)
			}
		})
	}
}
