package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
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
	rollback := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}
	select {
	case b.ready <- struct{}{}:
	case <-ctx.Done():
		rollback()
		return nil, ctx.Err()
	}
	select {
	case <-b.release:
		return tx, nil
	case <-ctx.Done():
		rollback()
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
			case r := <-done:
				t.Fatalf("report returned before transaction barrier: %v", r.err)
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
			var r result
			select {
			case r = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
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

func TestIdleStallConcurrentOwnerCancelLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)
	svc.p.RunCodexIdleStallMaxParks = 3
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	hold := mhOpenHold(t, e, id, 1, w)
	e.exec(t, "UPDATE recovery_custody_holds SET inventory_guarded=true WHERE id=$1", hold)
	e.exec(t, "UPDATE runs SET idle_stall_park_count=2,recovery_wait_count=5,provider_park_count=4,provider_park_streak_started_at='2001-01-01' WHERE id=$1", id)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	readHold := func() []byte {
		t.Helper()
		var raw []byte
		if err := e.pool.QueryRow(ctx, "SELECT to_jsonb(h) FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	beforeHold := readHold()
	before, err := e.q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}, 1), make(chan struct{})
	svc.SetTxBeginner(idleStallTxBarrier{e.pool, ready, release})
	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				t.Error("report did not stop within bounded cleanup")
			}
		}
	}()
	go func() {
		_, applied, err := svc.SetState(ctx, store.Worker{ID: w}, id, idleReport(1, i32Ptr(2)))
		done <- result{applied, err}
	}()
	select {
	case <-ready:
	case r := <-done:
		joined = true
		t.Fatalf("report returned before transaction barrier: %v", r.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The report observed running before Begin; its open transaction is now
	// blocked before the claim lock while another database session stamps cancel.
	input, err := e.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
		RunID: id, Kind: "cancel", StopKind: pgconv.TextOrNull("cancelled"),
		StopReason: pgconv.TextOrNull("owner cancel during idle report"),
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.q.GetRunByID(ctx, id)
	if err != nil || pending.Status != "running" || pending.StopKind.String != "cancelled" {
		t.Fatalf("pending cancel status=%s stop=%s err=%v", pending.Status, pending.StopKind.String, err)
	}
	var pendingInput bool
	if err = e.pool.QueryRow(ctx, "SELECT kind='cancel' AND consumed_at IS NULL AND applied_at IS NULL FROM run_user_inputs WHERE id=$1 AND run_id=$2", input.ID, id).Scan(&pendingInput); err != nil || !pendingInput {
		t.Fatalf("cancel input not pending: %v", err)
	}
	close(release)
	select {
	case r := <-done:
		joined = true
		if r.err != nil || !r.applied {
			t.Fatalf("cancel report applied=%v err=%v", r.applied, r.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after, err := e.q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "cancelled" || after.StopKind.String != "cancelled" ||
		after.StopReason != pending.StopReason || !after.FinishedAt.Valid || after.FailOrigin.Valid ||
		after.RecoveryWaitCause.Valid || after.RecoveryRetryNotBefore.Valid {
		t.Fatal("pending owner cancel lost ordinary terminal semantics")
	}
	if after.IdleStallParkCount != before.IdleStallParkCount ||
		after.RecoveryWaitCount != before.RecoveryWaitCount || after.ProviderParkCount != before.ProviderParkCount ||
		after.ProviderParkStreakStartedAt != before.ProviderParkStreakStartedAt {
		t.Fatal("cancel charged or reset idle, lifetime or provider counters")
	}
	if !bytes.Equal(beforeHold, readHold()) {
		t.Fatal("cancel changed guarded source custody hold")
	}
}

func TestIdleStallCheckpointPublicationNeutralityLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	e.exec(t, "UPDATE runs SET idle_stall_park_count=2,recovery_wait_count=5,provider_park_count=4,provider_park_streak_started_at='2001-01-01',checkpoint_tip='old-tip',checkpoint_tip_at='2001-01-02',checkpoint_attested_source=NULL,session_id='idle-checkpoint-session',updated_at='2001-01-03' WHERE id=$1", id)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	snapshot := func() map[string]json.RawMessage {
		t.Helper()
		var raw []byte
		if err := e.pool.QueryRow(ctx, "SELECT to_jsonb(r) FROM runs r WHERE id=$1", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := snapshot()
	var start, end time.Time
	if err := e.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&start); err != nil {
		t.Fatal(err)
	}
	const tip = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const source = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	n, err := e.q.SetRunCheckpointTip(ctx, store.SetRunCheckpointTipParams{
		ID: id, CheckpointTip: pgconv.TextOrNull(tip), AttestedSource: pgconv.TextOrNull(source),
	})
	if err != nil || n != 1 {
		t.Fatalf("checkpoint publication rows=%d err=%v", n, err)
	}
	if err = e.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&end); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	idleStallPublicationChanges(t, before, after)
	run, err := e.q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.CheckpointTip.String != tip || run.CheckpointAttestedSource.String != source ||
		!run.CheckpointTipAt.Valid || run.CheckpointTipAt.Time.Before(start) || run.CheckpointTipAt.Time.After(end) ||
		run.IdleStallParkCount != 2 || run.RecoveryWaitCount != 5 || run.ProviderParkCount != 4 {
		t.Fatal("checkpoint publication lost tip, stamp, attestation or counters")
	}
}

// Check all columns, including claim, status, session and affinity, in stable order.
func idleStallPublicationChanges(t *testing.T, before, after map[string]json.RawMessage) {
	t.Helper()
	keys := make([]string, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, exists := before[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		equal := bytes.Equal(before[key], after[key])
		switch key {
		case "checkpoint_tip", "checkpoint_tip_at", "checkpoint_attested_source":
			if equal {
				t.Fatalf("publication did not change %s", key)
			}
		default:
			if !equal {
				t.Fatalf("publication changed %s: before=%.200s after=%.200s", key, before[key], after[key])
			}
		}
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
