package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type policyOwnerStopReadRace struct {
	Store
	afterRead func()
}

func (s policyOwnerStopReadRace) GetRunByIDForUser(ctx context.Context, p store.GetRunByIDForUserParams) (store.Run, error) {
	r, err := s.Store.GetRunByIDForUser(ctx, p)
	if err == nil {
		s.afterRead()
	}
	return r, err
}

func TestPolicyOwnerStopLockedCountLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET kind='issue',issue_iid=101,milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',milestones_completed='["m1"]' WHERE id=$1`, e.runID); err != nil {
		t.Fatal(err)
	}
	e.svc.q = policyOwnerStopReadRace{Store: e.svc.q, afterRead: func() {
		if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET milestones_completed='["m1","m2"]' WHERE id=$1`, e.runID); err != nil {
			t.Fatal(err)
		}
	}}
	res, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "", nil, SubmitInputOptions{ActionIfMatch: PolicyHoldActionToken(held)})
	if err != nil || res.ScopeCeiling == nil || *res.ScopeCeiling != 2 {
		t.Fatalf("locked completed count=%+v err=%v", res, err)
	}
}

func TestPolicyOwnerStopLiveDB(t *testing.T) {
	for _, completed := range []string{`["m1"]`, `["m1","m2"]`} {
		t.Run(completed, func(t *testing.T) {
			e := setupPolicyHold(t)
			held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET kind='issue',issue_iid=101,milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',milestones_completed=$2 WHERE id=$1`, e.runID, completed); err != nil {
				t.Fatal(err)
			}
			for _, token := range []string{"", "stale"} {
				if _, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "finish only", nil, SubmitInputOptions{ActionIfMatch: token}); !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("token=%q err=%v", token, err)
				}
				got := mustRun(t, e.codexTestEnv, e.runID)
				if got.ScopeCeiling.Valid || got.Status != "paused" || got.StopKind.Valid {
					t.Fatal("rejected stop wrote state")
				}
			}
			res, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "finish only", nil, SubmitInputOptions{ActionIfMatch: PolicyHoldActionToken(held)})
			if err != nil || !res.Resumed || res.ScopeCeiling == nil {
				t.Fatalf("stop=%+v err=%v", res, err)
			}
			want := 1
			if completed != `["m1"]` {
				want = 2
			}
			got := mustRun(t, e.codexTestEnv, e.runID)
			if int(got.ScopeCeiling.Int32) != want || got.Status != "queued" || got.StopKind.Valid || got.PolicyFreshClaimGeneration.Valid {
				t.Fatalf("scope=%v status=%s stop=%v", got.ScopeCeiling, got.Status, got.StopKind)
			}
			var audits int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('scope','resume')", e.runID).Scan(&audits); err != nil || audits != 2 {
				t.Fatalf("audits=%d err=%v", audits, err)
			}
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET status='claimed',claim_generation=2 WHERE id=$1", e.runID); err != nil {
				t.Fatal(err)
			}
			req := PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 2, ClaimGeneration: 2, Detail: "resume_rejected"}
			held, _, err = e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, req)
			if err != nil {
				t.Fatal(err)
			}
			got, err = e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Mode: "fresh"})
			if err != nil || int(got.ScopeCeiling.Int32) != want {
				t.Fatalf("scope lost on fresh retry=%v err=%v", got.ScopeCeiling, err)
			}
		})
	}
}

func TestPolicyOwnerStopTokenRefusedLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET kind='issue',issue_iid=101,milestones_frozen='[{\"id\":\"m1\",\"title\":\"one\"}]',milestones_completed='[\"m1\"]',session_id='keep-stop-session',policy_fresh_claim_generation=1 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "\"stale\""} {
		if _, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "finish", nil, SubmitInputOptions{ActionIfMatch: token}); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("token=%q err=%v", token, err)
		}
		got := mustRun(t, e.codexTestEnv, e.runID)
		if got.Status != "paused" || got.ScopeCeiling.Valid || got.SessionID.String != "keep-stop-session" || !got.PolicyFreshClaimGeneration.Valid {
			t.Fatal("rejected stop changed state")
		}
		var n int
		if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('scope','resume')", e.runID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("inputs=%d err=%v", n, err)
		}
	}
}

func TestPolicyOwnerStopRefusalsLiveDB(t *testing.T) {
	for _, mutation := range []string{
		"kind='task',issue_iid=NULL,branch='agent/task-policy-owner'", "interactive=true", "milestones_completed='[]'", "milestones_frozen='[]'",
		"hold_reason='budget_exhausted',started_at=now()-interval '2 hours',status_since=now(),budget_wall_seconds=60",
		"hold_reason='completion_blocked'",
	} {
		t.Run(mutation, func(t *testing.T) {
			e := setupPolicyHold(t)
			held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET kind='issue',issue_iid=101,milestones_frozen='[{"id":"m1","title":"one"}]',milestones_completed='["m1"]' WHERE id=$1`, e.runID); err != nil {
				t.Fatal(err)
			}
			// Each mutation is a literal test fixture, never an owner-provided command.
			if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET "+mutation+" WHERE id=$1", e.runID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.SubmitInputWithOptions(e.ctx, e.userID, e.runID, "stop", "", nil, SubmitInputOptions{ActionIfMatch: PolicyHoldActionToken(held)}); err == nil {
				t.Fatal("invalid stop accepted")
			}
			got := mustRun(t, e.codexTestEnv, e.runID)
			if got.Status != "paused" || got.ScopeCeiling.Valid || got.StopKind.Valid {
				t.Fatal("refused stop wrote state")
			}
			var n int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('scope','resume')", e.runID).Scan(&n); err != nil || n != 0 {
				t.Fatalf("inputs=%d err=%v", n, err)
			}
		})
	}
}

func TestPolicyOwnerBudgetAndD19LiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	held, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET hold_reason='budget_exhausted',started_at=now()-interval '2 hours',status_since=now(),budget_wall_seconds=60,claim_released_at=now(),released_worker_id=worker_id WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{Guidance: "no write"}); err == nil {
		t.Fatal("spent budget resumed")
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET budget_extension_seconds=10000 WHERE id=$1", e.runID); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.ResumePolicyHold(e.ctx, e.userID, e.runID, PolicyHoldActionToken(held), PolicyResumeRequest{})
	if err != nil || got.WorkerID.Valid || !got.ReleasedWorkerID.Valid || got.HoldReason.Valid || !got.StartedAt.Valid {
		t.Fatalf("D19 status=%s worker=%v released=%v hold=%v err=%v", got.Status, got.WorkerID, got.ReleasedWorkerID, got.HoldReason, err)
	}
}

func TestPolicyOwnerCredentialLiveDB(t *testing.T) {
	for _, scenario := range []string{"success", "missing", "stale", "disabled", "newhold", "codex"} {
		t.Run(scenario, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			o := seedReevalOwner(t, env, BindModeAuto, false)
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			id := seedRunInStatus(t, env, o, 5810, "paused", time.Now())
			outcome := uuid.New()
			// A stored historical constraint exercises the lower owner path without
			// widening the public Codex credential-switch validator.
			env.exec("UPDATE runs SET policy_refusal_count=1,policy_last_outcome_id=$2,hold_reason='provider_policy_refusal',session_id='keep-session',policy_fresh_claim_generation=1 WHERE id=$1", id, outcome)
			held := mustRun(t, env, id)
			token := PolicyHoldActionToken(held)
			switch scenario {
			case "missing":
				token = ""
			case "stale":
				token = "stale"
			case "disabled":
				svc.beforeCredentialSwitchTx = func() { env.exec("UPDATE user_secrets SET disabled_at=now() WHERE id=$1", o.altTok) }
			case "newhold":
				svc.beforeCredentialSwitchTx = func() { env.exec("UPDATE runs SET policy_last_outcome_id=$2 WHERE id=$1", id, uuid.New()) }
			case "codex":
				env.exec("UPDATE runs SET harness='codex' WHERE id=$1", id)
			}
			_, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok, token)
			got := mustRun(t, env, id)
			if scenario == "success" {
				if err != nil || got.Status != "queued" || got.SessionID.String != "keep-session" || got.PolicyFreshClaimGeneration.Valid || got.CredentialOverrideMode.String != CredentialOverrideModePinned {
					t.Fatalf("success status=%s err=%v", got.Status, err)
				}
			} else {
				if err == nil || got.Status != "paused" || got.CredentialOverrideMode.Valid || got.SessionID.String != "keep-session" || got.PolicyFreshClaimGeneration != held.PolicyFreshClaimGeneration {
					t.Fatalf("refusal changed run: status=%s override=%v err=%v", got.Status, got.CredentialOverrideMode, err)
				}
				if (scenario == "missing" || scenario == "stale") && !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("precondition error=%v", err)
				}
			}
		})
	}
}
