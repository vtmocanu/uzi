package workersvc

import (
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
