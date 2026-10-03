package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckStateCanonicalLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	worker := store.Worker{ID: workerID, UserID: userID}
	token := strings.Join([]string{"glpat", "abcdefghijklmnopqrst"}, "-")
	rawPlan := "  Check \u202e" + token + "  "
	rawMilestones := []Milestone{{ID: "m1", Title: "  Verify \u202e" + token + "  "}}
	rawJSON, err := json.Marshal(rawMilestones)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NormalizePlanCrossCheckCandidate(PlanCrossCheckCandidate{PlanMd: rawPlan, Milestones: rawJSON,
		RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for caseIndex, name := range []string{"canonical raw report", "real content change", "missing digest", "ordinary unchanged"} {
		t.Run(name, func(t *testing.T) {
			leadID := env.seedCodexRun(t, userID, workerID, repoID)
			env.exec(`UPDATE runs SET issue_iid=$2 WHERE id=$1`, leadID, caseIndex+42)
			required := name != "ordinary unchanged"
			env.exec(`UPDATE runs SET auto_approve=true,plan_source='agent',harness='claude',
     plan_cross_check_required=$2,claim_generation=1 WHERE id=$1`, leadID, required)
			if required {
				childID := uuid.New()
				env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
      VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check')`, childID, userID, repoID, leadID)
				env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,
      required_tools,size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,verdict,deadline_at)
      VALUES ($1,'plan',1,1,$2,$3,'{}','{}','s',$4,'',$5,$6,'codex','approve',now()+interval '30 minutes')`,
					leadID, candidate.PlanMd, candidate.Milestones, candidate.BaseCommit, digest, childID)
			}
			plan := rawPlan
			ms := rawMilestones
			caps := []string{}
			tools := []string{}
			size := "s"
			generation := int64(1)
			req := StateRequest{State: "running", ClaimGeneration: &generation, PlanMd: &plan, Milestones: &ms,
				RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, CandidateDigest: hex.EncodeToString(digest)}
			if name == "real content change" {
				plan = rawPlan + " changed"
			}
			if name == "missing digest" {
				req.CandidateDigest = ""
			}
			if !required {
				req.CandidateDigest = ""
				req.Milestones = nil
				req.RequiredCapabilities = nil
				req.RequiredTools = nil
				req.SizeClass = nil
			}
			_, applied, err := svc.SetState(env.ctx, worker, leadID, req)
			if name == "real content change" || name == "missing digest" {
				if applied || !errors.Is(err, ErrInvalidState) {
					t.Fatalf("unapproved candidate accepted: applied=%v err=%v", applied, err)
				}
				run := mustRun(t, env, leadID)
				if run.PlanMd.Valid {
					t.Fatal("refused candidate persisted")
				}
				return
			}
			if err != nil || !applied {
				t.Fatalf("approved report refused: applied=%v err=%v", applied, err)
			}
			run := mustRun(t, env, leadID)
			if required {
				if run.PlanMd.String != candidate.PlanMd || string(run.MilestonesFrozen) != string(candidate.Milestones) {
					// jsonb pretty-printing is insignificant; compare decoded milestone values.
					var got, want []Milestone
					if json.Unmarshal(run.MilestonesFrozen, &got) != nil || json.Unmarshal(candidate.Milestones, &want) != nil ||
						len(got) != 1 || len(want) != 1 || got[0] != want[0] || run.PlanMd.String != candidate.PlanMd {
						t.Fatal("stored plan differs from normalized approval")
					}
				}
				if strings.Contains(run.PlanMd.String, token) {
					t.Fatal("canonical write leaked raw credential")
				}
			} else if run.PlanMd.String != rawPlan {
				t.Fatal("ordinary plan write changed normalization")
			}
		})
	}
}

func TestPlanCrossCheckWriteEmptyAndLegacyParams(t *testing.T) {
	caps := []string{}
	tools := []string{}
	size := "s"
	ms := []Milestone{}
	plan := "plan"
	gen := int64(1)
	req := StateRequest{PlanMd: &plan, RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, Milestones: &ms, ClaimGeneration: &gen, CandidateDigest: strings.Repeat("a", 64)}
	_, legacyTools, _ := inferredRequirementParams(req)
	if legacyTools != nil {
		t.Fatal("legacy empty tools no longer collapse to NULL")
	}
	p := store.SetRunAutopilotPlanParams{PlanMd: pgconv.Text(plan)}
	if err := bindPlanCrossCheckWrite(&p, store.Run{Kind: "issue"}, &req); err != nil {
		t.Fatal(err)
	}
	if p.InferredTools == nil || p.InferredCapabilities == nil || string(p.MilestonesFrozen) != "[]" {
		t.Fatal("required empty plan fields collapsed to NULL")
	}
}
