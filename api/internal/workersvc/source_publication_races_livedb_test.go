package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
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

type sourceStampFailureBeginner struct {
	TxBeginner
	statement string
}
type sourceStampFailureTx struct {
	pgx.Tx
	statement string
}

func (b sourceStampFailureBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return sourceStampFailureTx{tx, b.statement}, nil
}
func (tx sourceStampFailureTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "-- name: "+tx.statement+" ") {
		return pgconn.CommandTag{}, fmt.Errorf("injected %s write failure", tx.statement)
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestSourcePublicationStampFailureLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		for _, statement := range []string{"BindSourcePublicationIntent", "SetRunCompleted"} {
			t.Run(fmt.Sprintf("permit=%t/%s", permit, statement), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				f.svc.SetTxBeginner(sourceStampFailureBeginner{f.e.pool, statement})
				result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
				status, stamp, consumed := f.stored(t)
				if err == nil || !strings.Contains(err.Error(), "injected "+statement+" write failure") || result.Applied ||
					status != "running" || len(stamp) != 0 || consumed {
					t.Fatalf("write failure unpaired completion: %+v %v status=%s stamp=%s consumed=%t", result, err, status, stamp, consumed)
				}
			})
		}
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

// Both transactions open after their unlocked reads while the source is running.
// Each waits on an explicit release; the test completes the first before releasing
// the second. Context cancellation bounds every wait and joins both sessions.
type sourceOverlapBeginner struct {
	TxBeginner
	next    atomic.Int32
	arrived chan int
	release [2]chan struct{}
	writes  atomic.Int32
}
type sourceOverlapTx struct {
	pgx.Tx
	owner *sourceOverlapBeginner
}

func (b *sourceOverlapBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	n := int(b.next.Add(1)) - 1
	if n >= 2 {
		return sourceOverlapTx{tx, b}, nil
	}
	select {
	case b.arrived <- n:
	case <-ctx.Done():
		_ = tx.Rollback(context.Background())
		return nil, ctx.Err()
	}
	select {
	case <-b.release[n]:
		return sourceOverlapTx{tx, b}, nil
	case <-ctx.Done():
		_ = tx.Rollback(context.Background())
		return nil, ctx.Err()
	}
}
func (tx sourceOverlapTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if !strings.HasPrefix(query, "SET TRANSACTION") {
		tx.owner.writes.Add(1)
	}
	return tx.Tx.Exec(ctx, query, args...)
}

type sourceOverlapStore struct {
	Store
	settlements atomic.Int32
}

func (q *sourceOverlapStore) SettleScopeInputDisposition(ctx context.Context, p store.SettleScopeInputDispositionParams) (int64, error) {
	q.settlements.Add(1)
	return q.Store.SettleScopeInputDisposition(ctx, p)
}
func TestSourcePublicationOverlappingReportsLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("permit=%t/conflict=%t", permit, conflict), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				if !permit {
					f.e.exec(t, "UPDATE runs SET milestones_frozen=$2,milestones_completed=$3 WHERE id=$1", f.id, frozenJSON(t, "m1"), idsJSON(t, "m1"))
				}
				if _, err := f.svc.SubmitInput(f.e.ctx, f.w.UserID, f.id, "scope", "100", nil); err != nil {
					t.Fatal(err)
				}
				tracked := &sourceOverlapStore{Store: f.svc.q}
				f.svc.q = tracked
				ctx, cancel := context.WithTimeout(f.e.ctx, 20*time.Second)
				defer cancel()
				b := &sourceOverlapBeginner{TxBeginner: f.e.pool, arrived: make(chan int, 2), release: [2]chan struct{}{make(chan struct{}), make(chan struct{})}}
				f.svc.SetTxBeginner(b)
				type outcome struct {
					r   StateReportResult
					err error
				}
				first, second := make(chan outcome, 1), make(chan outcome, 1)
				joinedFirst, joinedSecond, startedSecond := false, false, false
				defer func() {
					cancel()
					if !joinedFirst {
						<-first
					}
					if startedSecond && !joinedSecond {
						<-second
					}
				}()
				go func() { r, e := f.svc.SetStateReportWithReconciliation(ctx, f.w, f.id, f.req); first <- outcome{r, e} }()
				select {
				case <-b.arrived:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				other := f.req
				report := *other.SourcePublication
				other.SourcePublication = &report
				if conflict {
					report.NonClosing = true
				}
				startedSecond = true
				go func() { r, e := f.svc.SetStateReportWithReconciliation(ctx, f.w, f.id, other); second <- outcome{r, e} }()
				select {
				case <-b.arrived:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				close(b.release[0])
				a := <-first
				joinedFirst = true
				if a.err != nil || !a.r.Applied {
					t.Fatalf("first: %+v %v", a.r, a.err)
				}
				_, before, spentBefore := f.stored(t)
				writes := b.writes.Load()
				if tracked.settlements.Load() != 1 {
					t.Fatalf("first settlement calls=%d", tracked.settlements.Load())
				}
				close(b.release[1])
				z := <-second
				joinedSecond = true
				_, after, spentAfter := f.stored(t)
				if conflict {
					if z.err == nil || z.r.Applied {
						t.Fatalf("conflict accepted: %+v %v", z.r, z.err)
					}
				} else if z.err != nil || !z.r.Applied {
					t.Fatalf("identical replay refused: %+v %v", z.r, z.err)
				}
				if !bytes.Equal(before, after) || spentBefore != spentAfter || b.writes.Load() != writes || tracked.settlements.Load() != 1 {
					t.Fatalf("replay wrote stamp/settlement: writes %d->%d consumed %t->%t", writes, b.writes.Load(), spentBefore, spentAfter)
				}
			})
		}
	}
}
