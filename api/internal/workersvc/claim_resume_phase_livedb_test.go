package workersvc

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1247 M5 (D13): it EXECUTES the new LatestPlanSeqForRun
// query and the resume_phase derivation through the REAL assembleClaim path against a REAL
// Postgres — sqlc's type deduction is not Postgres's, and a run driven into awaiting_approval by
// the AUTHENTIC writer (SetRunAwaitingApproval, which sets plan_md + plan_source='agent' +
// session_id and clears auto_approve) is what proves the derivation reads the columns that writer
// actually leaves. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (setupCodexLiveDB skips).

// seedResumeClaimInfra seeds the infra a real assembleClaim needs — a forge connection whose bot
// PAT decrypts (seedCodexInfra leaves a placeholder box.Open cannot open) and a default Anthropic
// token the credential resolution spends — and returns the owner coordinates. Mirrors the setup in
// TestAssembleClaimPausePendingTrueLiveDB.
func seedResumeClaimInfra(t *testing.T, env codexTestEnv) (userID, workerID, repoID uuid.UUID) {
	t.Helper()
	userID, workerID, repoID = env.seedCodexInfra(t)
	env.sealBotPAT(t, userID)
	// A DEFAULT Anthropic token so the ordinary run's credential resolution opens one.
	env.seedAnthropicSecret(t, userID, "anthropic-"+uuid.NewString(), true)
	return userID, workerID, repoID
}

// TestAssembleClaimResumePhaseAwaitingApprovalLiveDB drives a claimed run into awaiting_approval
// via SetRunAwaitingApproval (the authentic writer: plan_md + plan_source='agent' + session_id,
// auto_approve cleared) and appends a mixed set of run_messages frames, then asserts the assembled
// claim carries ResumePhase=="awaiting_approval" and ResumePlanSeq equal to the LATEST plan-gate
// frame's seq (MAX over {plan, plan_revising}, ignoring text/tool_use). This is the D13 crux —
// an UNAPPROVED submitted plan resumes the GATE with that plan's seq rather than re-planning.
func TestAssembleClaimResumePhaseAwaitingApprovalLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := seedResumeClaimInfra(t, env)
	runID := env.seedCodexRun(t, userID, workerID, repoID) // status 'claimed', worker_id set

	// The authentic held-state writer: sets plan_md + plan_source='agent' + session_id and clears
	// auto_approve + open_question_id. WHERE id = @id AND worker_id = @worker_id, so it needs the
	// claiming worker.
	rows, err := env.q.SetRunAwaitingApproval(env.ctx, store.SetRunAwaitingApprovalParams{
		PlanMd:    pgconv.TextOrNull("# Plan\n\n1. do the thing"),
		SessionID: pgconv.TextOrNull("sess-resume-1"),
		ID:        runID,
		WorkerID:  pgconv.UUID(workerID),
		// Issue #1626 (B1): the candidate breakdown the gate resume must re-deliver.
		MilestonesCandidate: []byte(`[{"id":"m1","title":"First"},{"id":"m2","title":"Second"}]`),
	})
	if err != nil {
		t.Fatalf("SetRunAwaitingApproval: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunAwaitingApproval affected %d rows, want 1", rows)
	}

	// Mixed frames: only {plan, plan_revising} count toward LatestPlanSeqForRun, and MAX(seq)
	// wins — so a later text/tool_use frame must NOT raise the answer above the plan frame at 7.
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 3, 'text', '{}')`, runID)
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 5, 'plan_revising', '{}')`, runID)
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 7, 'plan', '{}')`, runID)
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 9, 'tool_use', '{}')`, runID)

	// LatestPlanSeqForRun in isolation against the real rows: MAX plan-ish seq, ignoring the
	// text (3) and tool_use (9) frames.
	seq, err := env.q.LatestPlanSeqForRun(env.ctx, runID)
	if err != nil {
		t.Fatalf("LatestPlanSeqForRun: %v", err)
	}
	if seq != 7 {
		t.Fatalf("LatestPlanSeqForRun = %d, want 7 (MAX over plan/plan_revising, ignoring text/tool_use)", seq)
	}

	// Sanity-check the row the derivation reads: SetRunAwaitingApproval left it unapproved with a
	// resumable session and no open question.
	run := mustRun(t, env, runID)
	if !run.PlanMd.Valid || run.PlanMd.String == "" {
		t.Fatalf("run plan_md = %+v, want a submitted plan", run.PlanMd)
	}
	if !run.SessionID.Valid {
		t.Fatalf("run session_id = %+v, want a resumable session", run.SessionID)
	}
	if run.AutoApprove {
		t.Fatalf("run auto_approve = true, want false (SetRunAwaitingApproval clears it)")
	}
	if run.PlanSource != "agent" {
		t.Fatalf("run plan_source = %q, want agent", run.PlanSource)
	}
	if run.OpenQuestionID.Valid {
		t.Fatalf("run open_question_id = %+v, want NULL (cleared by SetRunAwaitingApproval)", run.OpenQuestionID)
	}

	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	wkr := store.Worker{ID: workerID, UserID: userID}
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	if payload.ResumePhase != "awaiting_approval" {
		t.Fatalf("assembled claim ResumePhase = %q, want awaiting_approval", payload.ResumePhase)
	}
	if payload.ResumePlanSeq != 7 {
		t.Fatalf("assembled claim ResumePlanSeq = %d, want 7 (the submitted plan frame's seq)", payload.ResumePlanSeq)
	}
	// The gate must resume the SAME unapproved plan, not skip to implementing.
	if payload.PlanApproved {
		t.Fatalf("assembled claim PlanApproved = true, want false for an awaiting_approval resume")
	}
	// Gate restoration carries the candidate and its exact plan for legacy runs too.
	wantMilestones := []Milestone{{ID: "m1", Title: "First"}, {ID: "m2", Title: "Second"}}
	if !reflect.DeepEqual(payload.Milestones, wantMilestones) || (payload.PlanMd == nil || *payload.PlanMd != run.PlanMd.String) || len(run.MilestonesFrozen) != 0 {
		t.Fatalf("legacy gate restore: milestones=%+v plan=%v frozen=%s", payload.Milestones, payload.PlanMd, run.MilestonesFrozen)
	}

	// Issue #1626 (B1): an INTERLOCKED run's gate resume carries the unapproved CANDIDATE
	// (nothing is frozen yet), so the worker re-presents the plan WITH its milestones rather than
	// without them.
	env.exec(`UPDATE runs SET completion_contract_version = 1 WHERE id = $1`, runID)
	interlocked := mustRun(t, env, runID)
	payload, err = svc.assembleClaim(env.ctx, wkr, interlocked)
	if err != nil {
		t.Fatalf("assembleClaim (interlocked): %v", err)
	}
	if payload.ResumePhase != "awaiting_approval" {
		t.Fatalf("interlocked assembled claim ResumePhase = %q, want awaiting_approval", payload.ResumePhase)
	}
	if !reflect.DeepEqual(payload.Milestones, wantMilestones) || (payload.PlanMd == nil || *payload.PlanMd != run.PlanMd.String) {
		t.Fatalf("interlocked gate restore: milestones=%+v plan=%v", payload.Milestones, payload.PlanMd)
	}
	// A populated execution contract must never fill in a missing or invalid gate list.
	env.exec(`UPDATE runs SET milestones_frozen=$2::jsonb WHERE id=$1`, runID, string(run.MilestonesCandidate))
	for _, candidate := range []any{"[]", nil, `[null]`, `["bad"]`} {
		env.exec(`UPDATE runs SET milestones_candidate=$2::jsonb WHERE id=$1`, runID, candidate)
		payload, err = svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
		if err != nil || (payload.PlanMd == nil || *payload.PlanMd != run.PlanMd.String) ||
			(candidate == "[]" && (payload.Milestones == nil || len(payload.Milestones) != 0)) ||
			(candidate != "[]" && payload.Milestones != nil) ||
			payload.ExecutionMilestones == nil || !reflect.DeepEqual(*payload.ExecutionMilestones, wantMilestones) {
			t.Fatalf("candidate %v restore: milestones=%+v plan=%v err=%v", candidate, payload.Milestones, payload.PlanMd, err)
		}
	}
	// Executing an approved claim uses the frozen definitions, even with a moved candidate.
	env.exec(`UPDATE runs SET auto_approve=true, milestones_frozen=$2::jsonb,
		milestones_candidate='[{"id":"m3","title":"Moved"}]' WHERE id=$1`, runID, string(run.MilestonesCandidate))
	payload, err = svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
	if err != nil || !payload.PlanApproved || !reflect.DeepEqual(payload.Milestones, wantMilestones) || (payload.PlanMd == nil || *payload.PlanMd != run.PlanMd.String) {
		t.Fatalf("approved restore: milestones=%+v approved=%v plan=%v err=%v", payload.Milestones, payload.PlanApproved, payload.PlanMd, err)
	}
}

// TestAssembleClaimResumePhaseImplementingLiveDB pins the approved-plan branch: a run carrying a
// plan and an approved verdict (here autopilot: auto_approve=true) with a resumable session
// resumes "implementing" — the worker skips the planning turn. ResumePlanSeq stays 0 because the
// scoped LatestPlanSeqForRun query fires ONLY on the awaiting_approval branch, keeping it off the
// hot path for every other resume.
func TestAssembleClaimResumePhaseImplementingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := seedResumeClaimInfra(t, env)

	// An approved (autopilot) run carrying a plan and a resumable session, seeded directly so the
	// approval is auto_approve=true (SetRunAwaitingApproval would clear it). status 'claimed'.
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, worker_id, plan_md, session_id, auto_approve, plan_source)
	          VALUES ($1, $2, $3, 'issue', 7, 't', 'd', 'claimed', $4, '# Plan\n', 'sess-impl-1', true, 'agent')`,
		runID, userID, repoID, workerID)

	run := mustRun(t, env, runID)
	if !run.AutoApprove {
		t.Fatalf("seeded run auto_approve = false, want true (an approved plan)")
	}

	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	wkr := store.Worker{ID: workerID, UserID: userID}
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	if payload.ResumePhase != "implementing" {
		t.Fatalf("assembled claim ResumePhase = %q, want implementing", payload.ResumePhase)
	}
	if payload.ResumePlanSeq != 0 {
		t.Fatalf("assembled claim ResumePlanSeq = %d, want 0 (the seq query is scoped to awaiting_approval)", payload.ResumePlanSeq)
	}
	if !payload.PlanApproved {
		t.Fatalf("assembled claim PlanApproved = false, want true for an approved-plan resume")
	}
}

// TestLatestPlanSeqForRunEmptyLiveDB pins the no-plan-frame case: a run that has emitted no
// plan-gate frame yet yields COALESCE(MAX(seq), 0) = 0, so a fresh queued run's would-be
// awaiting_approval seq degrades cleanly rather than erroring.
func TestLatestPlanSeqForRunEmptyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedCodexRun(t, userID, workerID, repoID)

	// A non-plan frame exists but must not count.
	env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', '{}')`, runID)

	seq, err := env.q.LatestPlanSeqForRun(env.ctx, runID)
	if err != nil {
		t.Fatalf("LatestPlanSeqForRun: %v", err)
	}
	if seq != 0 {
		t.Fatalf("LatestPlanSeqForRun = %d, want 0 (no plan/plan_revising frame)", seq)
	}
}
