package workersvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func idleReport(gen int64, progress *int32) StateRequest {
	return StateRequest{State: "recovery_wait", RecoveryCause: strPtr("codex_idle_stall"), ClaimGeneration: &gen, ProviderProgressTurns: progress}
}

func TestIdleStallParksAndPromotersLiveDB(t *testing.T) {
	for _, max := range []int{0, 3} {
		t.Run(fmt.Sprint(max), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 3)
			svc.p.RunCodexIdleStallMaxParks = max
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			hold := mhOpenHold(t, e, id, 1, w)
			gen := int64(1)
			for next := 1; next <= max+1; next++ {
				before, err := e.q.GetRunByID(e.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				r, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(gen, nil))
				if err != nil || !ok {
					t.Fatalf("park %d: %v %v", next, ok, err)
				}
				if r.IdleStallParkCount != int32(next) || r.RecoveryWaitCount != before.RecoveryWaitCount+1 || r.RecoveryWaitCause.String != "codex_idle_stall" || r.Status != "recovery_wait" || r.ClaimReleasedAt.Valid {
					t.Fatalf("bad park: %+v", r)
				}
				if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
					t.Fatal("custody released")
				}
				if _, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(gen, nil)); err != nil || ok {
					t.Fatalf("duplicate %v %v", ok, err)
				}
				if next <= max {
					delay := r.RecoveryRetryNotBefore.Time.Sub(start)
					base := time.Minute * time.Duration(1<<uint(next-1))
					if !r.RecoveryRetryNotBefore.Valid || delay < base+recoveryParkJitterMin-time.Second || delay > base+recoveryParkJitterMax+time.Second {
						t.Fatalf("delay %s", delay)
					}
					e.exec(t, "UPDATE runs SET recovery_retry_not_before=now()-interval '1 second' WHERE id=$1", id)
					if _, err = e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
						t.Fatal(err)
					}
					cp := claimRunParams(store.Worker{ID: w, UserID: e.userID})
					cp.RecoveryCapable = true
					cp.IsDockerWorker = true
					cp.DockerRepoAllowlist = []uuid.UUID{e.repoID}
					fresh, err := e.q.ClaimRun(e.ctx, cp)
					if err != nil || fresh.ID != id || fresh.ClaimGeneration != gen+1 || fresh.IdleStallParkCount != int32(next) {
						t.Fatalf("fresh claim %+v %v", fresh, err)
					}
					gen = fresh.ClaimGeneration
					if _, ok, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !ok {
						t.Fatalf("running %v %v", ok, err)
					}
				} else {
					if r.RecoveryRetryNotBefore.Valid {
						t.Fatal("held park has timer")
					}
					n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: e.userID})
					if err != nil || n != 0 {
						t.Fatalf("early held promote %d %v", n, err)
					}
					if _, err = e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now().Add(24*time.Hour))); err != nil {
						t.Fatal(err)
					}
					after, err := e.q.GetRunByID(e.ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if after.Status != "recovery_wait" || after.IdleStallParkCount != int32(next) {
						t.Fatal("timer promoted held run")
					}
					cp := claimRunParams(store.Worker{ID: w, UserID: e.userID})
					cp.RecoveryCapable = true
					cp.IsDockerWorker = true
					cp.DockerRepoAllowlist = []uuid.UUID{e.repoID}
					if _, err = e.q.ClaimRun(e.ctx, cp); !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("held claim must have no eligible row: %v", err)
					}
					afterClaim, err := e.q.GetRunByID(e.ctx, id)
					if err != nil || afterClaim.ClaimGeneration != gen || afterClaim.Status != "recovery_wait" {
						t.Fatalf("held claim changed generation/status: %v", err)
					}
				}
			}
		})
	}
}

func TestIdleStallProgressSaturationAndInterleavingLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)
	svc.p.RunCodexIdleStallMaxParks = 2147483646
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	e.exec(t, "UPDATE runs SET idle_stall_park_count=2147483647, provider_park_count=4, provider_park_streak_started_at=now() WHERE id=$1", id)
	r, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(1, i32Ptr(0)))
	if err != nil || !ok || r.IdleStallParkCount != math.MaxInt32 || r.RecoveryRetryNotBefore.Valid || r.ProviderParkCount != 4 {
		t.Fatalf("saturated %v %v %+v", ok, err, r)
	}
	// Establish a fresh flight after an owner Resume, then seed the prior saturated history.
	if _, err = e.q.ResumeCodexIdleStall(e.ctx, store.ResumeCodexIdleStallParams{ID: id, UserID: e.userID}); err != nil {
		t.Fatal(err)
	}
	e.exec(t, "UPDATE runs SET status='running', claim_generation=2, idle_stall_park_count=2147483647 WHERE id=$1", id)
	r, ok, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(2, i32Ptr(2)))
	if err != nil || !ok || r.IdleStallParkCount != 1 || !r.RecoveryRetryNotBefore.Valid || r.ProviderParkCount != 0 || r.ProviderParkStreakStartedAt.Valid {
		t.Fatalf("progress %v %v %+v", ok, err, r)
	}
	gen := e.progressReclaim(t, svc, store.Worker{ID: w, UserID: e.userID}, id)
	// An untyped park preserves the idle streak and the ordinary NULL-cause early control.
	r, ok, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "recovery_wait", ClaimGeneration: &gen})
	if err != nil || !ok || r.IdleStallParkCount != 1 {
		t.Fatalf("untyped %v %v %+v", ok, err, r)
	}
	e.exec(t, "UPDATE runs SET recovery_retry_not_before=NULL WHERE id=$1", id)
	n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: e.userID})
	if err != nil || n != 1 {
		t.Fatalf("ordinary NULL control %d %v", n, err)
	}
	e.exec(t, "UPDATE runs SET status='running', claim_generation=claim_generation+1 WHERE id=$1", id)
	r, ok, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(gen+1, i32Ptr(0)))
	if err != nil || !ok || r.IdleStallParkCount != 2 || r.RecoveryWaitCause.String != "codex_idle_stall" {
		t.Fatalf("interleaved %v %v %+v", ok, err, r)
	}
}

func TestIdleStallRefusalsLiveDB(t *testing.T) {
	for _, scenario := range []string{"stale", "released", "nonrunning", "missing tx", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 3)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 2)
			gen := int64(2)
			switch scenario {
			case "stale":
				gen = 1
			case "released":
				e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			case "nonrunning":
				e.exec(t, "UPDATE runs SET status='awaiting_input' WHERE id=$1", id)
			case "missing tx":
				svc.txBeginner = nil
			case "foreign":
				w = e.seedWorker(t, nil)
			}
			before, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			_, ok, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, idleReport(gen, nil))
			if ok {
				t.Fatal("refused report applied")
			}
			if scenario == "stale" || scenario == "released" {
				if !errors.Is(err, ErrStaleClaim) {
					t.Fatal(err)
				}
			}
			after, readErr := e.q.GetRunByID(e.ctx, id)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refused report changed row")
			}
		})
	}
}

func TestIdleStallConcurrentDuplicateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)
	svc.p.RunCodexIdleStallMaxParks = 3
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	// Hold the authoritative row until both independent sessions have arrived.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int64
		err error
	}
	results := make(chan result, 2)
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	svc.SetTxBeginner(idleStallTxBarrier{e.pool, ready, release})
	for range 2 {
		go func() {
			_, ok, err := svc.SetState(ctx, store.Worker{ID: w}, id, idleReport(1, nil))
			n := int64(0)
			if ok {
				n = 1
			}
			results <- result{n, err}
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case r := <-results:
			t.Fatalf("report returned before transaction barrier: %v", r.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for range 2 {
		var r result
		select {
		case r = <-results:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if r.err != nil {
			t.Fatal(r.err)
		}
		total += r.n
	}
	row, err := e.q.GetRunByID(ctx, id)
	if err != nil || total != 1 || row.IdleStallParkCount != 1 || row.RecoveryWaitCount != 1 {
		t.Fatalf("duplicate total=%d err=%v row=%+v", total, err, row)
	}
}

func TestIdleStallSetTokenLiveDB(t *testing.T) {
	for _, scenario := range []string{"timed", "held", "race into held"} {
		t.Run(scenario, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			o := seedReevalOwner(t, env, BindModeAuto, false)
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			id := seedRunInStatus(t, env, o, 350900, "recovery_wait", time.Now())
			env.exec("UPDATE runs SET recovery_wait_cause='codex_idle_stall', idle_stall_park_count=3, recovery_retry_not_before=now()+interval '1 hour' WHERE id=$1", id)
			hold := func() { env.exec("UPDATE runs SET recovery_retry_not_before=NULL, status_since=now() WHERE id=$1", id) }
			if scenario == "held" {
				hold()
			}
			var raced store.Run
			if scenario == "race into held" {
				svc.beforeCredentialSwitchTx = func() { hold(); raced = mustRun(t, env, id) }
			}
			before := mustRun(t, env, id)
			_, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok)
			after := mustRun(t, env, id)
			if scenario == "timed" {
				if err != nil || after.Status != "queued" || after.IdleStallParkCount != 3 || after.CredentialOverrideMode.String != CredentialOverrideModePinned {
					t.Fatalf("timed %v %+v", err, after)
				}
			} else {
				if !errors.Is(err, ErrCodexIdleStallHeld) {
					t.Fatalf("held error %v", err)
				}
				if scenario == "race into held" {
					before = raced
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("held override or counter changed")
				}
			}
		})
	}
}
