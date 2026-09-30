package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_credential_disabled_livedb_test.go pins PRD #1908 D-E for the credential_disabled path: a
// job never parks, so a claim whose credential is disabled fails the job (fail_origin
// credential_unavailable), while every other kind still parks on paused/credential_disabled.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway database.

const disableOwnerAnthropicRowsSQL = `UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1
	WHERE user_id = $1 AND kind = 'anthropic_token'`

func assertJobFailedNotParked(t *testing.T, run store.Run) {
	t.Helper()
	if run.Status != "failed" || run.FailOrigin.String != "credential_unavailable" || run.FailureReason.String == "" ||
		!run.FinishedAt.Valid || run.HoldReason.Valid {
		t.Fatalf("job = %s / origin %q / reason %q / finished %v / hold %v; want failed / credential_unavailable with a reason and no hold",
			run.Status, run.FailOrigin.String, run.FailureReason.String, run.FinishedAt.Valid, run.HoldReason)
	}
	// Single prefix: the stored reason must not stack "credential unavailable: credential disabled: ...".
	if !strings.HasPrefix(run.FailureReason.String, "credential unavailable: ") || strings.Contains(run.FailureReason.String, "credential disabled: ") {
		t.Fatalf("failure reason = %q; want a single \"credential unavailable: \" prefix", run.FailureReason.String)
	}
}

// TestJobClaimCredentialDisabledFailsLiveDB: a job whose credential is disabled fails at the
// claim instead of parking as paused/credential_disabled, both when assembly refuses the
// disabled default and when a disable commits after assembly opened it (the finisher's locked
// re-check). A parked job would keep worker_id, read as polled to hasLivePoller and strand a
// cancel.
//
// MUTATION CHECK: removing the runkind.Job arm in finishRunClaimTx parks both subtests as paused.
func TestJobClaimCredentialDisabledFailsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name         string
		disableEarly bool
	}{
		{"disabled before assembly", true},
		{"disabled after assembly opened it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupJobLiveDB(t, 0)
			u := e.seedJobUser(t)
			e.makeTokenDefault(t, u)
			v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
			if err != nil {
				t.Fatal(err)
			}
			workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
			wkr := store.Worker{ID: workerID, UserID: u, Name: "w", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}
			if tc.disableEarly {
				e.exec(disableOwnerAnthropicRowsSQL, u)
			} else {
				e.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					if err != nil || p == nil {
						t.Fatalf("assembly = (%v, %v), want a payload", p != nil, err)
					}
					e.exec(disableOwnerAnthropicRowsSQL, u)
				}}
			}
			if pl, err := e.svc.Claim(e.ctx, wkr, nil); err != nil || pl != nil {
				t.Fatalf("Claim = %+v, %v; want idle", pl, err)
			}
			assertJobFailedNotParked(t, mustRun(t, e.codexTestEnv, v.ID))
			if pl, err := e.svc.Claim(e.ctx, wkr, nil); err != nil || pl != nil {
				t.Fatalf("second Claim = %+v, %v; want idle (terminal, not re-queued)", pl, err)
			}
		})
	}
}

// TestNonJobClaimCredentialDisabledStillParksLiveDB: the same disable-after-assembly on an
// ordinary issue run still parks it paused/credential_disabled, so the job arm is the only change.
func TestNonJobClaimCredentialDisabledStillParksLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil {
			t.Fatalf("assembly = (%v, %v), want a payload", p != nil, err)
		}
		fx.setEnabled(fx.pinTok, false)
	}}
	if pl, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || pl != nil {
		t.Fatalf("Claim = %+v, %v; want idle", pl, err)
	}
	assertHeld(t, fx.env, runID, true)
}
