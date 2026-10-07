package workersvc

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Every report uses the service's owned, generation-fenced state transition.
// Cross-check rows are server-owned fixtures; no provider is invoked.
func TestPlanCrossCheckGateReasonPersistedOutcomeLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, declaration, want string
		rowGeneration, generation        int64
		repoCaps                         bool
	}{
		{"revise", "revise", "block", "revise", 1, 1, false},
		{"block", "block", "revise", "block", 1, 1, false},
		{"revise with repository requirement", "revise", "block", "revise", 1, 1, true},
		{"block with repository requirement", "block", "revise", "block", 1, 1, true},
		{"old generation approve", "approve", "", "interrupted", 1, 2, false},
		{"current approve without refusal", "approve", "", "", 1, 1, false},
		{"current approve with forced interruption", "approve", "interrupted", "interrupted", 1, 1, false},
		{"current approve with stale revise declaration", "approve", "revise", "", 1, 1, false},
		{"current approve with stale block declaration", "approve", "block", "", 1, 1, false},
		{"current approve with bounded candidate refusal", "approve", "candidate_refused", "candidate_refused", 1, 1, false},
		{"current approve with failed submit", "approve", "checker_failed", "checker_failed", 1, 1, false},
		{"current approve with diff refusal", "approve", "planning_diff_refused", "planning_diff_refused", 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			leadID := env.seedCodexRun(t, userID, workerID, repoID)
			env.exec(`UPDATE runs SET status='running',harness='claude',auto_approve=true,
				plan_source='agent',plan_cross_check_required=true,claim_generation=$2 WHERE id=$1`, leadID, tc.generation)
			if tc.repoCaps {
				env.exec(`UPDATE repos SET required_capabilities=$2 WHERE id=$1`, repoID, []string{capability.Docker})
				env.exec(`UPDATE runs SET required_capabilities=$2 WHERE id=$1`, leadID, []string{capability.Docker})
			}
			candidate := PlanCrossCheckCandidate{PlanMd: "Review the initial plan", Milestones: []byte(`[]`),
				RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
			digest, err := candidate.Digest()
			if err != nil {
				t.Fatal(err)
			}
			childID := uuid.New()
			env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
				VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'check','check')`, childID, userID, repoID, leadID)
			env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
				required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
				checker_run_id,checker_harness,verdict,reason_class,findings,deadline_at)
				VALUES ($1,'plan',1,$2,$3,$4,'{}','{}','s',$5,'',$6,$7,'codex',$8,$8,
					'{"summary":"Human review required","items":[]}',now()+interval '30 minutes')`,
				leadID, tc.rowGeneration, candidate.PlanMd, candidate.Milestones, candidate.BaseCommit, digest, childID, tc.verdict)
			before, err := env.q.GetPlanCrossCheck(env.ctx, leadID)
			if err != nil {
				t.Fatal(err)
			}
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			worker := store.Worker{ID: workerID, UserID: userID}
			plan, size := candidate.PlanMd, "s"
			caps, tools, ms := []string{}, []string{}, []Milestone{}
			presentation := uuid.New()
			req := StateRequest{State: "awaiting_approval", ClaimGeneration: &tc.generation,
				PlanMd: &plan, Milestones: &ms, RequiredCapabilities: &caps, RequiredTools: &tools,
				SizeClass: &size, PresentationID: &presentation}
			if tc.declaration != "" {
				req.PlanCrossCheckGateReason = &tc.declaration
			}
			switch tc.declaration {
			case "candidate_refused":
				req.PlanCrossCheckRefusal = "candidate_invalid"
			case "checker_failed":
				req.PlanCrossCheckRefusal = "submit_failed"
			case "planning_diff_refused":
				req.PlanCrossCheckDiffRefusal = "diff_too_large"
			}
			// The diff-refusal sub-code persists with the declaration, survives the
			// retention replays, and outlives the reason once a revision clears it
			// (the DTO masks it then; see TestRunToDTOPlanCrossCheckDiffRefusal).
			wantSub := ""
			if tc.declaration == "planning_diff_refused" {
				wantSub = "diff_too_large"
			}
			report := func(want string) {
				t.Helper()
				_, applied, err := svc.SetState(env.ctx, worker, leadID, req)
				if err != nil || !applied {
					t.Fatalf("human gate refused: applied=%v err=%v", applied, err)
				}
				got := mustRun(t, env, leadID)
				if got.Status != "awaiting_approval" || got.AutoApprove || got.PlanMd.String != plan {
					t.Fatalf("human gate not persisted: status=%s auto=%v plan=%q", got.Status, got.AutoApprove, got.PlanMd.String)
				}
				if got.PlanCrossCheckGateReason.Valid != (want != "") || got.PlanCrossCheckGateReason.String != want {
					t.Fatalf("persisted gate reason=%+v, want %q", got.PlanCrossCheckGateReason, want)
				}
				if got.PlanCrossCheckDiffRefusal.String != wantSub || got.PlanCrossCheckDiffRefusal.Valid != (wantSub != "") {
					t.Fatalf("persisted diff refusal=%+v, want %q (reason %q)", got.PlanCrossCheckDiffRefusal, wantSub, want)
				}
				if tc.repoCaps && !slices.Equal(got.RequiredCapabilities, []string{capability.Docker}) {
					t.Fatalf("repository requirement lost: %v", got.RequiredCapabilities)
				}
			}
			report(tc.want)
			// Replay the original declaration before testing an omitted declaration.
			report(tc.want)
			req.PlanCrossCheckGateReason = nil
			req.PlanCrossCheckRefusal, req.PlanCrossCheckDiffRefusal = "", ""
			report(tc.want)
			if tc.declaration == "planning_diff_refused" {
				// A stale declaration outside the approval-race list retains the
				// refused reason, so its sub-code must be retained with it.
				stale := "revise"
				req.PlanCrossCheckGateReason = &stale
				report(tc.want)
				req.PlanCrossCheckGateReason = nil
			}
			// A new human candidate clears the current reason, without deleting findings.
			plan = "Review a revised human candidate"
			presentation = uuid.New()
			report("")
			// Lost acknowledgments may replay B; historical A must stay historical.
			report("")
			after, err := env.q.GetPlanCrossCheck(env.ctx, leadID)
			if err != nil {
				t.Fatal(err)
			}
			if after.ID != before.ID || after.Verdict != before.Verdict || after.ReasonClass != before.ReasonClass ||
				after.LeadClaimGeneration != before.LeadClaimGeneration || after.PlanMd != before.PlanMd ||
				!bytes.Equal(after.CandidateDigest, before.CandidateDigest) || !bytes.Equal(after.Findings, before.Findings) {
				t.Fatalf("human revision changed historical cross-check: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestPlanCrossCheckGateReasonNoRowDeclarationsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, harness, reason, refusal          string
		stale, foreignOwner, invalid, recovered bool
	}{
		{name: "Codex lead unsupported", harness: "codex", reason: "codex_lead_unsupported"},
		{name: "interrupted recovered generation without a row", harness: "claude", reason: "interrupted", recovered: true},
		{name: "recovered Codex declaration stays interrupted", harness: "codex", reason: "codex_lead_unsupported", recovered: true},
		{name: "recovered diff declaration stays interrupted", harness: "claude", reason: "planning_diff_refused", refusal: "diff_too_large", recovered: true},
		{name: "owned bounded diff refusal", harness: "claude", reason: "planning_diff_refused", refusal: "diff_too_large"},
		{name: "arbitrary reason", harness: "claude", reason: "made_up", invalid: true},
		{name: "explicit empty reason", harness: "claude", reason: "", invalid: true},
		{name: "false no-row revise", harness: "claude", reason: "revise", invalid: true},
		{name: "false no-row block", harness: "claude", reason: "block", invalid: true},
		{name: "false unsupported Claude lead", harness: "claude", reason: "codex_lead_unsupported", invalid: true},
		{name: "unbounded diff refusal", harness: "claude", reason: "planning_diff_refused", refusal: "anything", invalid: true},
		{name: "missing diff refusal", harness: "claude", reason: "planning_diff_refused", invalid: true},
		{name: "stale claim generation", harness: "claude", reason: "planning_diff_refused", refusal: "diff_failed", stale: true},
		{name: "unowned diff refusal", harness: "claude", reason: "planning_diff_refused", refusal: "diff_failed", foreignOwner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			leadID := env.seedCodexRun(t, userID, workerID, repoID)
			generation := int64(1)
			if tc.recovered || tc.stale {
				generation = 2
			}
			wantReason := tc.reason
			if tc.recovered {
				wantReason = "interrupted"
			}
			env.exec(`UPDATE runs SET status='running',harness=$2,auto_approve=true,
				plan_source='agent',plan_cross_check_required=true,claim_generation=$3 WHERE id=$1`, leadID, tc.harness, generation)
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			worker := store.Worker{ID: workerID, UserID: userID}
			if tc.foreignOwner {
				worker.ID = uuid.New()
			}
			gen := generation
			if tc.stale {
				gen = 1
			}
			plan, size := "A candidate for human review", "s"
			caps, tools, ms := []string{}, []string{}, []Milestone{}
			req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan,
				Milestones: &ms, RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size,
				PlanCrossCheckGateReason: &tc.reason, PlanCrossCheckDiffRefusal: tc.refusal}
			_, applied, err := svc.SetState(env.ctx, worker, leadID, req)
			got := mustRun(t, env, leadID)
			if tc.invalid || tc.stale || tc.foreignOwner {
				if applied || err == nil {
					t.Fatalf("invalid declaration accepted: applied=%v err=%v", applied, err)
				}
				if tc.invalid && !errors.Is(err, ErrInvalidState) {
					t.Fatalf("invalid declaration error=%v, want ErrInvalidState", err)
				}
				if tc.stale && !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("stale report error=%v, want ErrStaleClaim", err)
				}
				if got.Status != "running" || got.PlanMd.Valid || got.PlanCrossCheckGateReason.Valid || !got.AutoApprove {
					t.Fatalf("refused report mutated run: status=%s plan=%+v reason=%+v auto=%v",
						got.Status, got.PlanMd, got.PlanCrossCheckGateReason, got.AutoApprove)
				}
			} else if err != nil || !applied || got.Status != "awaiting_approval" || got.AutoApprove ||
				got.PlanMd.String != plan || !got.PlanCrossCheckGateReason.Valid || got.PlanCrossCheckGateReason.String != wantReason {
				t.Fatalf("valid refusal not persisted: applied=%v err=%v status=%s reason=%+v",
					applied, err, got.Status, got.PlanCrossCheckGateReason)
			}
			if _, err := env.q.GetPlanCrossCheck(env.ctx, leadID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("declaration fabricated a cross-check row: %v", err)
			}
		})
	}
}
