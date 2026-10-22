package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestSourcePublicationAdmissionRaceLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		for _, release := range []bool{false, true} {
			t.Run(fmt.Sprintf("permit=%t/release=%t", permit, release), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				ctx, cancel := context.WithTimeout(f.e.ctx, 20*time.Second)
				defer cancel()
				holder, err := f.e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = holder.Rollback(context.Background()) }()
				var pid int
				if err = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					t.Fatal(err)
				}
				if _, err = holder.Exec(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE", f.w.ID); err != nil {
					t.Fatal(err)
				}
				type outcome struct {
					result StateReportResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() { r, e := f.svc.SetStateReportWithReconciliation(ctx, f.w, f.id, f.req); done <- outcome{r, e} }()
				joined := false
				defer func() {
					cancel()
					_ = holder.Rollback(context.Background())
					if !joined {
						<-done
					}
				}()
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for {
					var blocked bool
					if err = f.e.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						break
					}
					select {
					case out := <-done:
						joined = true
						t.Fatalf("completion bypassed worker lock: %+v %v", out.result, out.err)
					case <-ctx.Done():
						t.Fatal("no observable worker lock wait", ctx.Err())
					case <-tick.C:
					}
				}
				probe, err := f.e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, probeErr := probe.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", f.id)
				rollbackErr := probe.Rollback(ctx)
				if probeErr != nil || rollbackErr != nil {
					t.Fatalf("worker must lock before run: %v %v", probeErr, rollbackErr)
				}
				query := "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1"
				if release {
					query = "UPDATE runs SET claim_released_at=now() WHERE id=$1"
				}
				if _, err = holder.Exec(ctx, query, f.id); err != nil {
					t.Fatal(err)
				}
				if err = holder.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				out := <-done
				joined = true
				status, stamp, consumed := f.stored(t)
				if !errors.Is(out.err, ErrStaleClaim) || out.result.Applied || status != "running" || len(stamp) != 0 || consumed {
					t.Fatalf("stale publication escaped fence: %+v %v status=%s stamp=%s consumed=%t", out.result, out.err, status, stamp, consumed)
				}
			})
		}
	}
}

type sourceStampFailureBeginner struct{ TxBeginner }
type sourceStampFailureTx struct{ pgx.Tx }

func (b sourceStampFailureBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return sourceStampFailureTx{tx}, nil
}
func (tx sourceStampFailureTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "-- name: BindSourcePublicationIntent ") {
		return pgconn.CommandTag{}, errors.New("injected source stamp write failure")
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestSourcePublicationStampFailureLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprint("permit=", permit), func(t *testing.T) {
			f := seedSourcePublication(t, permit)
			f.svc.SetTxBeginner(sourceStampFailureBeginner{f.e.pool})
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			status, stamp, consumed := f.stored(t)
			if err == nil || !strings.Contains(err.Error(), "injected source stamp write failure") || result.Applied ||
				status != "running" || len(stamp) != 0 || consumed {
				t.Fatalf("stamp failure unpaired completion: %+v %v status=%s stamp=%s consumed=%t", result, err, status, stamp, consumed)
			}
		})
	}
}

func TestSourcePublicationAbsentProofReplayLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprint("permit=", permit), func(t *testing.T) {
			f := seedSourcePublication(t, permit)
			old := f.req
			old.SourcePublication = nil
			first, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, old)
			if err != nil || !first.Applied {
				t.Fatalf("old completion: %+v %v", first, err)
			}
			status, before, consumed := f.stored(t)
			if status != "completed" || len(before) != 0 || consumed != permit {
				t.Fatal("old completion unexpectedly stamped")
			}
			replay, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			_, after, spent := f.stored(t)
			if err == nil || replay.Applied || len(after) != 0 || spent != consumed {
				t.Fatalf("old completion backfilled proof: %+v %v", replay, err)
			}
		})
		t.Run(fmt.Sprint("receipt/permit=", permit), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			f := seedSerializedPublication(t, e, permit)
			f.proof.ancestry = forge.AncestryAncestor
			first, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, f.req)
			if err != nil || !first.Applied || first.CompletedPublicationReceipt == nil {
				t.Fatalf("old receipt: %+v %v", first, err)
			}
			f.req.SourcePublication = &SourcePublicationReport{Version: 1, PublishedHead: *f.req.CompletionFinalHead}
			replay, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, f.req)
			var stamp []byte
			if readErr := e.pool.QueryRow(e.ctx, "SELECT source_publication_intent FROM runs WHERE id=$1", f.run).Scan(&stamp); readErr != nil {
				t.Fatal(readErr)
			}
			if err == nil || replay.Applied || replay.CompletedPublicationReceipt != nil || len(stamp) != 0 {
				t.Fatalf("receipt manufactured proof: %+v %v stamp=%s", replay, err, stamp)
			}
		})
	}
}
