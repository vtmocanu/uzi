package workersvc

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerExhaustionNonOwnerGuardsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	worker := seedSnapshotWorker(t, env, user, "exhaustion-guards")
	run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, 2)
	env.exec("UPDATE runs SET checkpoint_tip=$2 WHERE id=$1", run, strings.Repeat("c", 40))
	hold := uuid.New()
	env.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,$4,2,'open',$5,'guard-fixture',$5,$4)", hold, user, repo, run, worker)
	changed, err := env.q.FailWorkerRunsOverCap(env.ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgconv.UUID(worker), MaxRequeues: 1, FailureReason: pgconv.TextOrNull("worker lost")})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].ID != run {
		t.Fatalf("exhaustion result=%v", changed)
	}
	env.exec("UPDATE runs SET recovery_retry_not_before=now()-interval '1 minute' WHERE id=$1", run)
	before := exhaustionRun(t, env, run)
	if before.Status != "recovery_wait" || before.RecoveryWaitCause.String != "worker_requeue_exhausted" || !before.ClaimReleasedAt.Valid {
		t.Fatalf("not exhausted: %+v", before)
	}
	custody := func() string {
		t.Helper()
		var state string
		if err := env.pool.QueryRow(env.ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	holdBefore := custody()
	assertHeld := func() {
		t.Helper()
		after := exhaustionRun(t, env, run)
		if after.Status != before.Status || after.RecoveryWaitCause != before.RecoveryWaitCause ||
			after.RequeueEpisodeBaseline != before.RequeueEpisodeBaseline || after.WorkerRecoveryEpisode != before.WorkerRecoveryEpisode ||
			after.RequeueCount != before.RequeueCount || after.ClaimReleasedAt != before.ClaimReleasedAt ||
			after.ClaimGeneration != before.ClaimGeneration || after.WorkerID != before.WorkerID ||
			after.ReleasedWorkerID != before.ReleasedWorkerID || after.ReleasedWorkerNonce != before.ReleasedWorkerNonce ||
			after.StaleRequeueGeneration != before.StaleRequeueGeneration || after.FinalizeResumeGeneration != before.FinalizeResumeGeneration ||
			after.RecoveryWaitCount != before.RecoveryWaitCount || after.RecoveryRetryNotBefore != before.RecoveryRetryNotBefore ||
			after.HoldReason != before.HoldReason || after.HoldCapturedHead != before.HoldCapturedHead ||
			after.ScopeCeiling != before.ScopeCeiling || after.BudgetFinalizeSeconds != before.BudgetFinalizeSeconds ||
			after.CheckpointTip != before.CheckpointTip || !bytes.Equal(after.WorkerRecoveryEvidence, before.WorkerRecoveryEvidence) ||
			custody() != holdBefore {
			t.Fatalf("event released hold or renewed episode: before=%+v after=%+v", before, after)
		}
	}
	// Each operation executes its actual guarded write against the held row.
	for _, event := range []struct {
		name  string
		apply func() error
	}{
		{"timer", func() error {
			rows, err := env.q.PromoteRecoveryWaitRuns(env.ctx, pgconv.Time(time.Now()))
			for _, row := range rows {
				if row.ID == run {
					t.Fatal("timer promoted exhaustion")
				}
			}
			return err
		}},
		{"early token promoter", func() error {
			n, err := env.q.PromoteRecoveryWaitRunNow(env.ctx, store.PromoteRecoveryWaitRunNowParams{ID: run, UserID: user})
			if n != 0 {
				t.Fatalf("early promotion rows=%d", n)
			}
			return err
		}},
		{"token change service", func() error {
			_, err := snapshotSvc(env, testParams()).SetRunCredential(env.ctx, user, run, CredentialOverrideModeAuto, nil)
			if !errors.Is(err, ErrCredentialSwitchRaced) {
				t.Fatalf("exhaustion token change error=%v", err)
			}
			return nil
		}},
		{"vault unlock", func() error {
			rows, err := env.q.PromoteVaultLockedRecoveryWaitRuns(env.ctx, user)
			for _, row := range rows {
				if row.ID == run {
					t.Fatal("vault promoted exhaustion")
				}
			}
			return err
		}},
		{"account recovery", func() error {
			n, err := env.q.PromoteCodexAccountWaitRun(env.ctx, run)
			if n != 0 {
				t.Fatalf("account promotion rows=%d", n)
			}
			return err
		}},
		{"pause", func() error {
			_, err := env.q.CreatePauseInput(env.ctx, store.CreatePauseInputParams{ID: run, Mode: pgconv.TextOrNull("now")})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("pause error=%v", err)
			}
			return nil
		}},
		{"cancel pause", func() error {
			_, err := env.q.CancelPauseInput(env.ctx, run)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("cancel pause error=%v", err)
			}
			return nil
		}},
		{"paused resume", func() error {
			_, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{ID: run, UserID: user})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("paused resume error=%v", err)
			}
			return nil
		}},
		{"wall extend resume", func() error {
			total, err := env.q.ExtendAndResumeWallPark(env.ctx, store.ExtendAndResumeWallParkParams{ID: run, UserID: user, Secs: 60, Cap: 3600})
			// ExtendAndResumeWallPark's scalar SELECT returns zero when no row is extended.
			if err != nil || total != 0 {
				t.Fatalf("wall extend total=%d error=%v", total, err)
			}
			if got := exhaustionRun(t, env, run); got.BudgetExtensionSeconds != before.BudgetExtensionSeconds {
				t.Fatalf("wall extend changed budget: before=%d after=%d", before.BudgetExtensionSeconds, got.BudgetExtensionSeconds)
			}
			return nil
		}},
		{"wall stop resume", func() error {
			_, err := env.q.StopWallPark(env.ctx, store.StopWallParkParams{ID: run, UserID: user, ScopeCeiling: 1})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("wall stop error=%v", err)
			}
			return nil
		}},
		{"approve", func() error {
			_, err := env.q.CreateApprovePlanInput(env.ctx, store.CreateApprovePlanInputParams{RunID: run})
			return err
		}},
		{"stale worker running", func() error {
			n, err := env.q.SetRunRunning(env.ctx, store.SetRunRunningParams{ID: run, WorkerID: pgconv.UUID(worker)})
			if n != 0 {
				t.Fatalf("running write rows=%d", n)
			}
			return err
		}},
		{"follow up", func() error {
			_, err := snapshotSvc(env, testParams()).SubmitInput(env.ctx, user, run, "follow_up", "continue", nil)
			return err
		}},
		{"budget extension", func() error {
			total, err := env.q.CreateExtendInput(env.ctx, store.CreateExtendInputParams{ID: run, Body: pgconv.TextOrNull("60"), Secs: 60, Cap: 3600})
			if total != before.BudgetExtensionSeconds+60 {
				t.Fatalf("extension total=%d", total)
			}
			return err
		}},
	} {
		t.Logf("event: %s", event.name)
		if err := event.apply(); err != nil {
			t.Fatalf("%s: %v", event.name, err)
		}
		assertHeld()
	}
	// Ordinary causes remain eligible for both timer and early credential promotion.
	for i, early := range []bool{false, true} {
		control := seedOutageRun(t, env, user, repo, worker, "recovery_wait", "issue", 1, 0)
		env.exec("UPDATE runs SET recovery_wait_cause='provider_outage', recovery_retry_not_before=now()-interval '1 minute' WHERE id=$1", control)
		if early {
			n, err := env.q.PromoteRecoveryWaitRunNow(env.ctx, store.PromoteRecoveryWaitRunNowParams{ID: control, UserID: user})
			if err != nil || n != 1 {
				t.Fatalf("ordinary early=%d,%v", n, err)
			}
		} else {
			rows, err := env.q.PromoteRecoveryWaitRuns(env.ctx, pgconv.Time(time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				found = found || row.ID == control
			}
			if !found {
				t.Fatal("ordinary timer did not promote")
			}
		}
		if got := exhaustionRun(t, env, control); got.Status != "queued" {
			t.Fatalf("control %d status=%s", i, got.Status)
		}
		assertHeld()
	}
}
