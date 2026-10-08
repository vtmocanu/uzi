package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCheckerSubmitContextBoundsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", f.aliasID)
	original := mustRun(t, env, f.runID)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	candidate := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage("[]"),
		SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	for _, tc := range []struct {
		name  string
		cap   int
		title bool
	}{
		{"title", 4096, true}, {"body", 262144, false},
	} {
		for _, extra := range []int{0, 1} {
			t.Run(tc.name+strconv.Itoa(extra), func(t *testing.T) {
				id := uuid.New()
				title, body := "title", "body"
				value := strings.Repeat("é", tc.cap/2) + strings.Repeat("x", extra)
				if tc.title {
					title = value
				} else {
					body = value
				}
				env.exec("INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_cross_check_required,claim_generation) VALUES ($1,$2,$3,$4,42,$5,$6,'running','claude',true,true,1)", id, f.userID, original.RepoID, f.workerID, title, body)
				cc, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, id, 1, candidate)
				if extra == 0 {
					if err != nil {
						t.Fatal(err)
					}
					child := mustRun(t, env, uuid.UUID(cc.CheckerRunID.Bytes))
					if child.IssueTitle != title || child.IssueDescription != body {
						t.Fatal("submission truncated context")
					}
					env.exec("UPDATE runs SET issue_title=$2 WHERE id=$1", id, strings.Repeat("x", 4097))
					if _, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, id, 1, candidate); !errors.Is(err, ErrCrossCheckRefused) {
						t.Fatal("oversized retry accepted")
					}
				} else {
					if !errors.Is(err, ErrCrossCheckRefused) {
						t.Fatal("oversized submit accepted")
					}
					var count int
					if err := env.pool.QueryRow(env.ctx, "SELECT (SELECT count(*) FROM runs WHERE target_run_id=$1)+(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1)", id).Scan(&count); err != nil || count != 0 {
						t.Fatal("refused submission created child or cross check")
					}
				}
				env.exec("UPDATE runs SET status='completed' WHERE id=$1", id)
			})
		}
	}
}

func TestCheckerAssemblyRefusalFinishesExactClaimLiveDB(t *testing.T) {
	for _, scenario := range []string{"missing candidate", "encoded envelope", "stored context"} {
		t.Run(scenario, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newSubscriptionFixture(t, env)
			resealBotPAT(t, env, f.userID)
			original := mustRun(t, env, f.runID)
			leadID := uuid.New()
			env.exec("INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_cross_check_required,claim_generation) VALUES ($1,$2,$3,$4,42,'title','body','running','claude',true,true,1)", leadID, f.userID, original.RepoID, f.workerID)
			env.exec("UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,report_only=true,budget_wall_seconds=1800,status='claimed',claim_generation=1 WHERE id=$1", f.runID, leadID)
			if scenario == "encoded envelope" {
				c := PlanCrossCheckCandidate{PlanMd: strings.Repeat("<", 256*1024), Milestones: json.RawMessage("[]"), SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
				digest, err := c.Digest()
				if err != nil {
					t.Fatal(err)
				}
				env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,deadline_at) VALUES ($1,'plan',1,1,$2,$3,'s',$4,'',$5,$6,'codex',now()+interval '30 minutes')`, leadID, c.PlanMd, c.Milestones, c.BaseCommit, digest, f.runID)
				env.exec("UPDATE runs SET issue_description=$2 WHERE id=$1", f.runID, strings.Repeat("<", 262144))
			}
			if scenario == "stored context" {
				env.exec("UPDATE runs SET issue_description=$2 WHERE id=$1", f.runID, strings.Repeat("x", 262145))
			}
			worker := checkerBoundsWorker(f)
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			// Missing candidates and oversized stored context are refused before
			// minting; encoded-envelope bounds still exercise post-mint recovery.
			run := mustRun(t, env, f.runID)
			payload, assemblyErr := svc.assembleClaim(env.ctx, worker, run)
			if payload != nil || !errors.Is(assemblyErr, ErrCrossCheckRefused) {
				t.Fatalf("assembly refusal missing: %v", assemblyErr)
			}
			var minted *codexMintedClaimError
			if gotMint := errors.As(assemblyErr, &minted); gotMint != (scenario == "encoded envelope") {
				t.Fatal("assembly refusal carried the wrong mint identity")
			}
			if scenario != "encoded envelope" && mustRun(t, env, run.ID).CodexClaimEpoch != run.CodexClaimEpoch {
				t.Fatal("preflight refusal reached credential mint")
			}
			out, err := svc.finishRunClaim(env.ctx, run, payload, assemblyErr, claimRecoveryIdentity{workerID: worker.ID})
			if err != nil || out != nil {
				t.Fatalf("finish refusal: %v", err)
			}
			after := mustRun(t, env, f.runID)
			if after.Status != "failed" || after.FailOrigin.String != "guardrail_blocked" {
				t.Fatalf("refusal left claim %s", after.Status)
			}
		})
	}
}

func checkerBoundsWorker(f codexRunFixture) store.Worker {
	return store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}
}

func TestCheckerStoredAssemblyContextBoundsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", f.aliasID)
	original := mustRun(t, env, f.runID)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	worker := checkerBoundsWorker(f)
	// Ordinary oversized issue context still passes the real assembly seam.
	env.exec("UPDATE runs SET issue_title=$2,issue_description=$3 WHERE id=$1", f.runID, strings.Repeat("é", 2049), strings.Repeat("é", 131073))
	if _, err := svc.assembleClaim(env.ctx, worker, mustRun(t, env, f.runID)); err != nil {
		t.Fatal(err)
	}
	leadID := uuid.New()
	env.exec("INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_cross_check_required,claim_generation) VALUES ($1,$2,$3,$4,42,'title','body','running','claude',true,true,1)", leadID, f.userID, original.RepoID, f.workerID)
	candidate := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage("[]"), SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	cc, err := svc.SubmitPlanCrossCheck(env.ctx, f.wkr, leadID, 1, candidate)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.UUID(cc.CheckerRunID.Bytes)
	env.exec("UPDATE runs SET worker_id=$2,status='claimed',claim_generation=1 WHERE id=$1", id, f.workerID)
	for _, tc := range []struct {
		name  string
		cap   int
		title bool
	}{{"title", 4096, true}, {"body", 262144, false}} {
		for _, extra := range []int{0, 1} {
			t.Run(tc.name+strconv.Itoa(extra), func(t *testing.T) {
				title, body := "title", "body"
				value := strings.Repeat("é", tc.cap/2) + strings.Repeat("x", extra)
				if tc.title {
					title = value
				} else {
					body = value
				}
				env.exec("UPDATE runs SET issue_title=$2,issue_description=$3 WHERE id=$1", id, title, body)
				before := mustRun(t, env, id)
				p, err := svc.assembleClaim(env.ctx, worker, before)
				if extra == 0 {
					if err != nil || p == nil {
						t.Fatalf("exact context assembly: %v", err)
					}
					if p.IssueTitle != title || p.IssueDescription != body {
						t.Fatal("assembly truncated context")
					}
				} else {
					if !errors.Is(err, ErrCrossCheckRefused) || p != nil {
						t.Fatal("oversized stored context accepted")
					}
					if mustRun(t, env, id).CodexClaimEpoch != before.CodexClaimEpoch {
						t.Fatal("oversized context reached credential mint")
					}
				}
			})
		}
	}
	// A stored-row change after the early snapshot must be refused before input commit.
	env.exec("UPDATE runs SET issue_title='title',issue_description='body' WHERE id=$1", id)
	svc.claimHooks = &claimTestHooks{afterMint: func(_ context.Context, _ store.Run) {
		env.exec("UPDATE runs SET issue_description=$2 WHERE id=$1", id, strings.Repeat("x", 262145))
	}}
	p, err := svc.assembleClaim(env.ctx, worker, mustRun(t, env, id))
	if p != nil || !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("locked oversized context accepted")
	}
}
