package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Subtests run serially; each service claim has a bounded context.
func laneFinishFixture(t *testing.T) crossCheckContentionFixture {
	t.Helper()
	f := laneFixture(t)
	ctx, cancel := context.WithTimeout(f.env.ctx, 5*time.Second)
	t.Cleanup(cancel)
	f.env.ctx = ctx
	return f
}

func assertLaneFinishLeadPending(t *testing.T, f crossCheckContentionFixture, before store.Run) {
	t.Helper()
	if !reflect.DeepEqual(before, mustRun(t, f.env, f.lead)) {
		t.Fatal("claim finishing changed the current lead")
	}
	var pending bool
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT verdict='pending' AND decided_at IS NULL
		AND lead_run_id=$2 AND lead_claim_generation=$3 AND checker_run_id=$1
		FROM cross_checks WHERE checker_run_id=$1`, f.runID, f.lead, before.ClaimGeneration).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("claim finishing settled or replaced the pending check")
	}
}

func TestCrossCheckLaneFinishGenerationLiveDB(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "success"
		if stale {
			name = "stale-generation"
		}
		t.Run(name, func(t *testing.T) {
			f := laneFinishFixture(t)
			lead := mustRun(t, f.env, f.lead)
			var won store.Run
			called := false
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
				called = true
				if err != nil || p == nil {
					t.Fatalf("assembly = (%v, %v), want a successful payload", p != nil, err)
				}
				f.claimed(t, p)
				if run.ID != f.runID || !mustRun(t, f.env, f.runID).CrossCheckLane {
					t.Fatal("assembly did not claim the own cross-check lane child")
				}
				if stale {
					f.env.exec(`UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1`, f.runID)
				}
				won = mustRun(t, f.env, f.runID)
			}}
			p, err := f.svc.ClaimCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), nil)
			if !called {
				t.Fatal("claim did not reach afterAssembly")
			}
			if err != nil {
				t.Fatalf("ClaimCrossCheck: %v", err)
			}
			if stale {
				if p != nil {
					t.Fatal("stale generation delivered an executable payload")
				}
				if won.ClaimGeneration != 2 {
					t.Fatal("race did not supersede the assembled generation")
				}
			} else {
				f.claimed(t, p)
			}
			f.unchanged(t, won)
			assertLaneFinishLeadPending(t, f, lead)
		})
	}
}

func TestCrossCheckLaneFinishLateCredentialDisableLiveDB(t *testing.T) {
	f := laneFinishFixture(t)
	lead := mustRun(t, f.env, f.lead)
	var minted store.Run
	called := false
	f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
		called = true
		if err != nil || p == nil {
			t.Fatalf("assembly = (%v, %v), want a successful payload", p != nil, err)
		}
		f.claimed(t, p)
		minted = mustRun(t, f.env, f.runID)
		if run.ID != f.runID || !minted.CrossCheckLane {
			t.Fatal("assembly did not claim the own cross-check lane child")
		}
		f.env.exec(`UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1
			WHERE id=(SELECT codex_secret_id FROM runs WHERE id=$1)`, f.runID)
	}}
	p, err := f.svc.ClaimCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), nil)
	if !called {
		t.Fatal("claim did not reach afterAssembly")
	}
	if err != nil || p != nil {
		t.Fatalf("ClaimCrossCheck = (%v, %v), want idle after late disable", p != nil, err)
	}
	parked := assertHeld(t, f.env, f.runID, true)
	if !parked.ClaimReleasedAt.Valid || len(parked.CodexCapHash) != 0 ||
		parked.CodexClaimEpoch != minted.CodexClaimEpoch+1 || parked.FailOrigin.Valid || parked.RecoveryWaitCause.Valid {
		t.Fatal("credential-disabled park did not release and revoke the undelivered claim without a failure")
	}
	if parked.Kind != "cross_check" || !parked.CrossCheckLane ||
		parked.WorkerID != minted.WorkerID || parked.ClaimGeneration != minted.ClaimGeneration ||
		parked.CodexSecretID != minted.CodexSecretID || parked.CodexAuthMode != minted.CodexAuthMode ||
		parked.CodexAccountKey != minted.CodexAccountKey ||
		parked.CodexMaterialRevision != minted.CodexMaterialRevision ||
		parked.CodexAccountRevision != minted.CodexAccountRevision ||
		parked.SessionID != minted.SessionID || parked.StartedAt != minted.StartedAt ||
		parked.CheckpointTip != minted.CheckpointTip || parked.CheckpointTipAt != minted.CheckpointTipAt ||
		parked.BudgetPausedSeconds != minted.BudgetPausedSeconds {
		t.Fatal("credential-disabled park lost claim ownership, credential binding, or retained execution state")
	}
	f.unchanged(t, parked) // A normal report-only child opens no custody.
	assertLaneFinishLeadPending(t, f, lead)
}

// The injected hold models a post-claim fence violation, not normal checker custody.
// The zero-hold case also proves assembly-time disable still reaches the fenced park.
func TestCrossCheckLaneFinishCustodyRecheckLiveDB(t *testing.T) {
	for _, inject := range []bool{false, true} {
		name := "zero-holds-park"
		if inject {
			name = "unexpected-open-hold-refuses"
		}
		t.Run(name, func(t *testing.T) {
			f := laneFinishFixture(t)
			lead := mustRun(t, f.env, f.lead)
			var claimed store.Run
			var holdBefore string
			beforeCalled, afterCalled := false, false
			f.svc.claimHooks = &claimTestHooks{
				beforeAssembly: func(_ context.Context, run store.Run) {
					beforeCalled = true
					if run.ID != f.runID || run.Status != "claimed" || !run.CrossCheckLane ||
						run.Kind != "cross_check" || run.ClaimGeneration != 1 {
						t.Fatal("beforeAssembly did not observe the committed lane claim")
					}
					f.env.exec(`UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1
						WHERE id=(SELECT codex_secret_id FROM runs WHERE id=$1)`, f.runID)
				},
				afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
					afterCalled = true
					if p != nil || !errors.Is(err, errCredentialDisabled) {
						t.Fatalf("assembly = (%v, %v), want credential-disabled refusal", p != nil, err)
					}
					claimed = mustRun(t, f.env, f.runID)
					f.unchanged(t, claimed) // Prove ClaimCrossCheck itself opened zero holds.
					if inject {
						holdID := claimRecoveryHold(t, f.env, f.runID, f.workerID, run.ClaimGeneration)
						assertClaimRecoveryHold(t, f.env, holdID, "open", false)
						if err := f.env.pool.QueryRow(f.env.ctx, `SELECT to_jsonb(h)::text
							FROM recovery_custody_holds h WHERE run_id=$1 AND generation=$2`,
							f.runID, run.ClaimGeneration).Scan(&holdBefore); err != nil {
							t.Fatal(err)
						}
					}
					claimed = mustRun(t, f.env, f.runID)
				},
			}
			p, err := f.svc.ClaimCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), nil)
			if !beforeCalled || !afterCalled {
				t.Fatal("claim did not reach both assembly hooks")
			}
			if p != nil {
				t.Fatal("disabled credential delivered an executable payload")
			}
			if inject {
				if !errors.Is(err, errClaimRecoveryCustody) {
					t.Fatalf("ClaimCrossCheck: %v, want errClaimRecoveryCustody", err)
				}
				if !reflect.DeepEqual(claimed, mustRun(t, f.env, f.runID)) {
					t.Fatal("custody refusal changed the whole claimed run")
				}
				var holdAfter string
				if err := f.env.pool.QueryRow(f.env.ctx, `SELECT to_jsonb(h)::text
					FROM recovery_custody_holds h WHERE run_id=$1 AND generation=$2`,
					f.runID, claimed.ClaimGeneration).Scan(&holdAfter); err != nil {
					t.Fatal(err)
				}
				if holdAfter != holdBefore {
					t.Fatal("custody refusal changed the injected open hold")
				}
			} else {
				if err != nil {
					t.Fatalf("ClaimCrossCheck: %v", err)
				}
				parked := assertHeld(t, f.env, f.runID, true)
				if !parked.ClaimReleasedAt.Valid || len(parked.CodexCapHash) != 0 ||
					parked.CodexClaimEpoch != claimed.CodexClaimEpoch+1 ||
					parked.WorkerID != claimed.WorkerID || parked.ClaimGeneration != claimed.ClaimGeneration ||
					parked.CodexSecretID != claimed.CodexSecretID || !parked.CrossCheckLane ||
					parked.FailOrigin.Valid || parked.RecoveryWaitCause.Valid {
					t.Fatal("zero-hold credential-disabled park lost or failed the claim")
				}
				f.unchanged(t, parked)
			}
			assertLaneFinishLeadPending(t, f, lead)
		})
	}
}
