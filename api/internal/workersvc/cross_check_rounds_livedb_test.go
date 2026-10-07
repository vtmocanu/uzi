package workersvc

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func newAutomaticRoundsFixture(t *testing.T, limit int32) (codexTestEnv, *Service, store.Worker, uuid.UUID, PlanCrossCheckCandidate) {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, worker, repo := env.seedCodexInfra(t)
	secret := env.seedStaticAPIKey(t, user, "rounds", "fixture-"+uuid.NewString())
	env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", secret)
	env.exec("UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1','cross_check_rounds_v1'] WHERE id=$1", worker)
	lead := uuid.New()
	env.exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_cross_check_required,claim_generation)
 VALUES($1,$2,$3,$4,2150,'round issue','body','running','claude',true,true,1)`, lead, user, repo, worker)
	p := testParams()
	p.PlanCrossCheckMaxRevisions = limit
	p.PlanCrossCheckMaxRevisionsSet = true
	svc := New(env.q, env.box, p)
	svc.SetTxBeginner(env.pool)
	c := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: []byte("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	return env, svc, store.Worker{ID: worker, UserID: user}, lead, c
}

func TestPlanCrossCheckAutomaticRoundsBudgetLiveDB(t *testing.T) {
	for limit := int32(0); limit <= 4; limit++ {
		t.Run(fmt.Sprintf("budget%d", limit), func(t *testing.T) {
			env, svc, w, lead, c := newAutomaticRoundsFixture(t, limit)
			for round := int32(1); round <= limit+1; round++ {
				cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, round)
				if err != nil {
					t.Fatal(err)
				}
				if !cc.AutomaticRoundsEnabled || cc.AutomaticRevisionLimit != limit || cc.Round != round {
					t.Fatalf("snapshot: %+v", cc)
				}
				// A later config increase must never widen this execution's snapshot.
				svc.p.PlanCrossCheckMaxRevisions = 4
				retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, round)
				if err != nil || retry.ID != cc.ID {
					t.Fatalf("retry: %v", err)
				}
				changed := c
				changed.PlanMd = "different"
				if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, changed, round); !errors.Is(err, ErrCrossCheckInterrupted) {
					t.Fatalf("changed candidate: %v", err)
				}
				child := uuid.UUID(cc.CheckerRunID.Bytes)
				env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
				if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
					t.Fatal(err)
				}
			}
			wantError := ErrCrossCheckRevisionsExhausted
			if limit == 4 {
				// Round 6 exceeds the protocol's maximum candidate number.
				wantError = ErrCrossCheckRefused
			}
			if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, limit+2); !errors.Is(err, wantError) {
				t.Fatalf("budget exceeded: %v, want %v", err, wantError)
			}
			var count, revise int
			if err := env.pool.QueryRow(env.ctx, "SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),revise_count FROM runs WHERE id=$1", lead).Scan(&count, &revise); err != nil {
				t.Fatal(err)
			}
			if count != int(limit+1) || revise != 0 {
				t.Fatalf("count=%d human revisions=%d", count, revise)
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsRecoveryPolicyLiveDB(t *testing.T) {
	cases := []struct {
		name, verdict, reason  string
		expired, gate, allowed bool
	}{
		{"pending before", "pending", "", false, false, true},
		{"pending expired", "pending", "", true, false, false},
		{"revise", "revise", "revise", false, false, true},
		{"unstored approval", "approve", "approve", false, false, true},
		{"block", "block", "block", false, false, false},
		{"timeout", "failed", "timed_out", true, false, false},
		{"model error", "failed", "model_error", false, false, false},
		{"human gate", "revise", "revise", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
			cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.verdict != "pending" {
				env.exec("UPDATE cross_checks SET verdict=$2,reason_class=$3,decided_at=now(),wait_credited=true WHERE id=$1", cc.ID, tc.verdict, tc.reason)
			}
			if tc.expired {
				env.exec("UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", cc.ID)
			}
			if tc.gate {
				env.exec("UPDATE runs SET status='awaiting_approval',auto_approve=false,plan_md='human plan',gate_revision=1 WHERE id=$1", lead)
			}
			env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", lead)
			next, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 2, c, 2)
			if tc.allowed {
				if err != nil || next.Round != 2 || next.CheckerRunID == cc.CheckerRunID {
					t.Fatalf("eligible recovery: %v", err)
				}
				retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 2, c, 2)
				if err != nil || retry.ID != next.ID {
					t.Fatalf("recovery retry: %v", err)
				}
			} else {
				if !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatalf("forbidden recovery: %v", err)
				}
				var children, rounds int
				if err := env.pool.QueryRow(env.ctx, "SELECT (SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1)", lead).Scan(&children, &rounds); err != nil || children != 1 || rounds != 1 {
					t.Fatalf("forbidden children=%d rounds=%d err=%v", children, rounds, err)
				}
				if tc.verdict == "block" || tc.verdict == "failed" {
					status, _, statusErr := svc.PlanCrossCheckStatus(env.ctx, w, lead, 2, 1)
					if statusErr != nil || status.Verdict != tc.verdict || status.ReasonClass.String != tc.reason {
						t.Fatalf("decided fallback changed on status read: verdict=%s reason=%s err=%v", status.Verdict, status.ReasonClass.String, statusErr)
					}
				}
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsCurrentWorkerDowngradeLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
	cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE cross_checks SET verdict='revise',reason_class='revise',decided_at=now(),wait_credited=true WHERE id=$1", cc.ID)
	env.exec("UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1'] WHERE id=$1", w.ID)
	if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 2); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("downgraded worker: %v", err)
	}
	if retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1); err != nil || retry.ID != cc.ID {
		t.Fatalf("downgrade lost immutable retry: %v", err)
	}
}
