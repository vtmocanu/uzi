package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

func TestMilestoneReconciliationPublishCallerFenceLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	calls := 0
	const tip = "3333333333333333333333333333333333333333"
	f.svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		calls++
		if o.DeclaredTip != tip {
			t.Fatalf("declared tip=%q", o.DeclaredTip)
		}
		return pushbroker.Result{Disposition: pushbroker.PublishAdvanced}, nil
	})
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	assertRejected := func() {
		t.Helper()
		before := reconciliationRow(t, f, id)
		var attemptsBefore int
		if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", id).Scan(&attemptsBefore); err != nil {
			t.Fatal(err)
		}
		for _, gen := range []*int64{nil, i64(0), i64(1)} {
			legacy := f.wkr
			legacy.ProtocolCapabilities = nil
			res, err := f.svc.Publish(f.ctx, legacy, id, tip, []byte("pack"), gen)
			want := ErrStaleClaim
			if gen == nil || *gen == 0 {
				want = ErrMissingClaimGeneration
			}
			if res.Published || !errors.Is(err, want) {
				t.Fatalf("generation=%v result=%+v err=%v, want %v", gen, res, err, want)
			}
		}
		var attemptsAfter int
		if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", id).Scan(&attemptsAfter); err != nil {
			t.Fatal(err)
		}
		if calls != 0 || attemptsAfter != attemptsBefore || !reflect.DeepEqual(before, reconciliationRow(t, f, id)) {
			t.Fatalf("rejected publish changed authority: broker calls=%d attempts=%d->%d", calls, attemptsBefore, attemptsAfter)
		}
	}
	assertRejected()
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil || claim.ClaimGeneration != 2 {
		t.Fatalf("same-worker reclaim=%+v err=%v, want generation 2", claim, err)
	}
	assertRejected()
	// Audited terminal publication must also prove caller authority before forge I/O.
	f.exec("UPDATE runs SET status='completed' WHERE id=$1", id)
	assertRejected()
	f.exec("UPDATE runs SET status='running' WHERE id=$1", id)
	res, err := f.svc.Publish(f.ctx, f.wkr, id, tip, []byte("pack"), i64(2))
	if err != nil || !res.Published || calls != 1 {
		t.Fatalf("current publish=%+v err=%v broker calls=%d", res, err, calls)
	}
	if got := reconciliationRow(t, f, id).CheckpointTip; !got.Valid || got.String != tip {
		t.Fatalf("current publish did not persist tip: %v", got)
	}
}

// Hold a request after its transaction starts but before admission locks. The
// owner commits reconciliation while the older legacy request is in flight.
type reconciliationPublishBeginner struct {
	conn    *pgx.Conn
	started chan struct{}
	release chan struct{}
}

func (b *reconciliationPublishBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	close(b.started)
	select {
	case <-b.release:
		return tx, nil
	case <-ctx.Done():
		_ = tx.Rollback(context.Background())
		return nil, ctx.Err()
	}
}

func TestMilestoneReconciliationPublishLegacyAdmissionRaceLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	reconciliationRaceDeadline(t, f)
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	b := &reconciliationPublishBeginner{conn: conn.Conn(), started: make(chan struct{}), release: make(chan struct{})}
	svc := reconciliationRaceService(f.svc, f.q)
	svc.SetTxBeginner(b)
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		t.Error("legacy request reached broker after reconciliation")
		return pushbroker.Result{}, nil
	})
	done := make(chan error, 1)
	legacy := f.wkr
	legacy.ProtocolCapabilities = nil
	go func() {
		_, err := svc.Publish(f.ctx, legacy, id, retentionTestTip, []byte("pack"))
		done <- err
	}()
	reconciliationRaceWait(t, f.ctx, b.started)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		close(b.release)
		<-done
		t.Fatal(err)
	}
	before := reconciliationRaceSnapshot(t, f, id)
	close(b.release)
	if err := <-done; !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("legacy admission after owner commit: %v", err)
	}
	reconciliationRaceUnchanged(t, f, id, before)
	var attempts int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", id).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("legacy admission attempts=%d err=%v", attempts, err)
	}
}

func TestMilestoneReconciliationPublishAdmissionSnapshotLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	calls := 0
	f.svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		calls++
		return pushbroker.Result{}, nil
	})
	// The first allowlist check occurs after the short admission committed.
	// Releasing and reclaiming here must not upgrade the admitted generation 1.
	reclaimed := false
	f.svc.SetForgeBaseURLAllowed(func(u string) bool {
		if !reclaimed {
			reclaimed = true
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
				t.Fatal(err)
			}
			if claim, err := f.svc.Claim(f.ctx, f.wkr, nil); err != nil || claim == nil || claim.ClaimGeneration != 2 {
				t.Fatalf("reclaim=%+v err=%v", claim, err)
			}
		}
		return u == "https://forge.e2e"
	})
	res, err := f.svc.Publish(f.ctx, f.wkr, id, "3333333333333333333333333333333333333333", []byte("pack"), i64(1))
	if err != nil || res.Published || res.Skipped != "not_descendant" || calls != 0 {
		t.Fatalf("released admission publish=%+v err=%v calls=%d", res, err, calls)
	}
	if r := reconciliationRow(t, f, id); r.CheckpointTip.String != retentionTestTip {
		t.Fatalf("stale admission changed tip: %v", r.CheckpointTip)
	}
	var attempts int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", id).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("stale admission attempts=%d err=%v", attempts, err)
	}
}
