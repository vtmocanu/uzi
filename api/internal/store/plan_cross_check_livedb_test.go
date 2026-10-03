package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckSnapshotLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	mustExec(ctx, t, f.pool, `UPDATE users SET plan_cross_check_enabled = true WHERE id = $1`, f.userID)
	run, err := f.q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
		UserID: f.userID, RepoID: f.repoID, IssueIid: pgtype.Int8{Int64: 2, Valid: true},
		IssueTitle: "plan check", IssueDescription: "candidate", Harness: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSelfImproveRun: %v", err)
	}
	if !run.PlanCrossCheckRequired {
		t.Fatal("opted-in self-improve run did not snapshot requirement")
	}
	mustExec(ctx, t, f.pool, `UPDATE users SET plan_cross_check_enabled = false WHERE id = $1`, f.userID)
	var required bool
	if err := f.pool.QueryRow(ctx, `SELECT plan_cross_check_required FROM runs WHERE id = $1`, run.ID).Scan(&required); err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("toggle changed existing run snapshot")
	}
}

func TestPlanCrossCheckCompletionAfterParkLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_required = true,
		auto_approve = true, completion_contract_version = 1 WHERE id = $1`, f.runID)
	rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("review me"),
		ClaimGeneration: pgtype.Int8{Int64: 0, Valid: true},
	})
	if err != nil || rows != 1 {
		t.Fatalf("park: rows=%d err=%v", rows, err)
	}
	var autoApprove bool
	if err := f.pool.QueryRow(ctx, `SELECT auto_approve FROM runs WHERE id = $1`, f.runID).Scan(&autoApprove); err != nil {
		t.Fatal(err)
	}
	if autoApprove {
		t.Fatal("park did not clear auto_approve")
	}

	complete := func(want int64) {
		t.Helper()
		rows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			ID: f.runID, WorkerID: pgU(f.workerID),
		})
		if err != nil || rows != want {
			t.Fatalf("SetRunCompleted: rows=%d err=%v, want %d", rows, err, want)
		}
	}
	attempt := func(want bool) {
		t.Helper()
		count, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
			RunID: f.runID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`),
		})
		if want {
			if err != nil || count != 1 {
				t.Fatalf("RecordCompletionAttempt: count=%d err=%v, want 1", count, err)
			}
		} else if err != pgx.ErrNoRows {
			t.Fatalf("RecordCompletionAttempt: count=%d err=%v, want no rows", count, err)
		}
	}
	assertNoAttempt := func() {
		t.Helper()
		var count int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM run_completion_attempts WHERE run_id = $1`, f.runID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("refused completion still inserted %d attempts", count)
		}
	}

	complete(0)
	attempt(false)
	assertNoAttempt()
	// A reclaim can change status without reviewing the stored plan.
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	complete(0)
	attempt(false)
	assertNoAttempt()

	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'awaiting_approval' WHERE id = $1`, f.runID)
	publishPlanCrossCheckGate(ctx, t, f, "review me")
	input, err := f.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: f.runID, Body: pgT("{}"), AgentSource: pgT("own"), AgentExclusions: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("CreateApprovePlanInput: %v", err)
	}
	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET applied_at = now(), disposition = 'superseded' WHERE id = $1`, input.ID)
	complete(0)
	attempt(false)
	assertNoAttempt()

	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET disposition = 'applied' WHERE id = $1`, input.ID)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	attempt(true)
	complete(1)

	// An unrequired run still accepts both completion writes without a plan.
	var unrequiredID uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO runs
		(user_id, repo_id, worker_id, issue_iid, issue_title, issue_description, status, completion_contract_version)
		VALUES ($1, $2, $3, 2, 'unrequired', 'd', 'running', 1) RETURNING id`,
		f.userID, f.repoID, f.workerID).Scan(&unrequiredID); err != nil {
		t.Fatal(err)
	}
	count, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
		RunID: unrequiredID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`),
	})
	if err != nil || count != 1 {
		t.Fatalf("unrequired attempt: count=%d err=%v", count, err)
	}
	rows, err = f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		ID: unrequiredID, WorkerID: pgU(f.workerID),
	})
	if err != nil || rows != 1 {
		t.Fatalf("unrequired completion: rows=%d err=%v", rows, err)
	}
}

func publishPlanCrossCheckGate(ctx context.Context, t *testing.T, f *awaitingInputFixture, plan string) int64 {
	t.Helper()
	var payload []byte
	if err := f.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		'plan_md', plan_md, 'milestones', milestones_candidate,
		'required_capabilities', ARRAY(SELECT DISTINCT cap FROM unnest(required_capabilities) AS cap ORDER BY cap),
		'required_tools', ARRAY(SELECT DISTINCT tool FROM unnest(required_tools) AS tool ORDER BY tool),
		'size_class', NULLIF(size_class, '')
	) FROM runs WHERE id = $1 AND plan_md = $2`, f.runID, plan).Scan(&payload); err != nil {
		t.Fatalf("gate payload: %v", err)
	}
	revision, err := f.q.PublishRunGatePresentation(ctx, store.PublishRunGatePresentationParams{
		ID: f.runID, PresentedPayload: payload,
		PayloadDigest: []byte("test-digest"),
	})
	if err != nil {
		t.Fatalf("PublishRunGatePresentation: %v", err)
	}
	return revision
}

func TestPlanCrossCheckSecondParkLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_required = true,
		completion_contract_version = 1 WHERE id = $1`, f.runID)
	park := func(plan string, milestones []byte) {
		t.Helper()
		rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT(plan),
			ClaimGeneration:     pgtype.Int8{Int64: 0, Valid: true},
			MilestonesCandidate: milestones,
		})
		if err != nil || rows != 1 {
			t.Fatalf("park %q: rows=%d err=%v", plan, rows, err)
		}
	}
	approve := func(revision int64) {
		t.Helper()
		input, err := f.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
			RunID: f.runID, Body: pgT("{}"), AgentSource: pgT("own"),
			AgentExclusions: []byte("[]"), ExpectedGateRevision: pgtype.Int8{Int64: revision, Valid: true},
		})
		if err != nil {
			t.Fatalf("approve revision %d: %v", revision, err)
		}
		mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET applied_at = now(), disposition = 'applied' WHERE id = $1`, input.ID)
	}
	check := func(stage string, allowed bool) {
		t.Helper()
		rows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: f.runID, WorkerID: pgU(f.workerID)})
		if err != nil || (rows == 1) != allowed {
			t.Fatalf("%s completion: rows=%d err=%v, allowed=%v", stage, rows, err, allowed)
		}
		if allowed {
			return
		}
		_, err = f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
			RunID: f.runID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`),
		})
		if err != pgx.ErrNoRows {
			t.Fatalf("%s attempt: err=%v, want no rows", stage, err)
		}
	}

	park("plan A", []byte(`[]`))
	approve(publishPlanCrossCheckGate(ctx, t, f, "plan A"))
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	// An applied approval for A permits the first plan.
	_, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
		RunID: f.runID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("first approved attempt: %v", err)
	}
	park("plan A", []byte(`[]`))
	var retained bool
	if err := f.pool.QueryRow(ctx, `SELECT gate_presented_payload IS NOT NULL FROM runs WHERE id = $1`, f.runID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("same-plan re-presentation cleared its published snapshot")
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	park("plan A", []byte(`[{"title":"changed"}]`))
	check("same plan, changed milestones before publication", false)
	publishPlanCrossCheckGate(ctx, t, f, "plan A")
	check("same plan, changed milestones after publication", false)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	park("plan B", []byte(`[]`))
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gate_reason = 'interrupted' WHERE id = $1`, f.runID)
	resume := func(stage string, caps []string, want int64) {
		t.Helper()
		rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: f.runID, WorkerID: pgU(f.workerID), InferredCapabilities: caps,
		})
		if err != nil || rows != want {
			t.Fatalf("%s SetRunRunning: rows=%d err=%v, want %d", stage, rows, err, want)
		}
	}
	resume("before B publication", nil, 0)
	check("before B publication", false)
	publishPlanCrossCheckGate(ctx, t, f, "plan B")
	resume("after B publication with A approval", nil, 0)
	check("after B publication", false)
	claimApproved := func(stage string, want bool) {
		t.Helper()
		claim, err := f.q.GetRunClaimContext(ctx, f.runID)
		if err != nil || claim.HumanPlanApproved != want {
			t.Fatalf("%s claim approval=%v err=%v, want %v", stage, claim.HumanPlanApproved, err, want)
		}
	}
	claimApproved("B with A approval", false)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID)
	check("requeued", false)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed' WHERE id = $1`, f.runID)
	claimApproved("reclaimed B with A approval", false)
	resume("reclaimed B with A approval", nil, 0)
	check("reclaimed", false)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'awaiting_approval' WHERE id = $1`, f.runID)
	var revision int64
	if err := f.pool.QueryRow(ctx, `SELECT gate_revision FROM runs WHERE id = $1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	approve(revision)
	rows, err := f.q.ClearRunRequiredCapabilities(ctx, store.ClearRunRequiredCapabilitiesParams{
		ID: f.runID, UserID: f.userID, GateRevision: revision,
	})
	if err != nil || rows != 1 {
		t.Fatalf("owner capability clear: rows=%d err=%v", rows, err)
	}
	claimApproved("approved B", true)
	resume("approved B with new capability", []string{"docker"}, 1)
	var reason pgtype.Text
	if err := f.pool.QueryRow(ctx, `SELECT plan_cross_check_gate_reason FROM runs WHERE id = $1`, f.runID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason.Valid {
		t.Fatalf("applied human approval retained gate reason %q", reason.String)
	}
	rows, err = f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), InferredCapabilities: []string{"python"},
		InferredTools: []string{"go"}, SizeClass: pgT("m"),
	})
	if err != nil || rows != 1 {
		t.Fatalf("postapproval running report: rows=%d err=%v", rows, err)
	}
	var caps, tools []string
	var size pgtype.Text
	if err := f.pool.QueryRow(ctx, `SELECT required_capabilities, required_tools, size_class FROM runs WHERE id = $1`, f.runID).Scan(&caps, &tools, &size); err != nil {
		t.Fatal(err)
	}
	if len(caps) != 0 || len(tools) != 0 || size.String != "" {
		t.Fatalf("postapproval running report changed frozen requirements: caps=%v tools=%v size=%v", caps, tools, size)
	}
	check("approved B", true)
}

func TestPlanCrossCheckInterruptedReasonLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	t.Cleanup(done)
	t.Cleanup(func() {
		mustExec(ctx, t, f.pool, `DELETE FROM users WHERE id = $1`, f.userID)
	})
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed', plan_cross_check_required = true,
		auto_approve = true, plan_source = 'agent', harness = 'claude', claim_generation = 1,
		plan_cross_check_gate_reason = 'interrupted' WHERE id = $1`, f.runID)
	assertReason := func(stage string, want string) {
		t.Helper()
		var reason pgtype.Text
		if err := f.pool.QueryRow(ctx, `SELECT plan_cross_check_gate_reason FROM runs WHERE id = $1`, f.runID).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if reason.Valid != (want != "") || reason.String != want {
			t.Fatalf("%s: gate reason=%v, want %q", stage, reason, want)
		}
	}
	start := func(stage string) {
		t.Helper()
		rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: f.runID, WorkerID: pgU(f.workerID),
		})
		if err != nil || rows != 1 {
			t.Fatalf("%s startup: rows=%d err=%v, want 1", stage, rows, err)
		}
		assertReason(stage, "interrupted")
	}
	start("first startup")
	start("heartbeat")
	// Recovery changes the claim generation without resolving the interrupted check.
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed', claim_generation = 2 WHERE id = $1`, f.runID)
	start("recovered startup")

	childID := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO runs (id, user_id, repo_id, kind, target_run_id, harness, report_only,
		budget_wall_seconds, issue_title, issue_description, status)
		VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','candidate','running')`,
		childID, f.userID, f.repoID, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md,
		milestones, required_capabilities, required_tools, size_class, base_commit, candidate_digest,
		checker_run_id, checker_harness, verdict, deadline_at)
		VALUES ($1,'plan',1,2,'approved','[]','{}','{}','s',repeat('a',40),$2,$3,'codex','revise',now()+interval '30 minutes')`,
		f.runID, []byte("digest"), childID)
	p := store.SetRunAutopilotPlanParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("approved"),
		CandidateDigest: []byte("digest"), InferredCapabilities: []string{},
		InferredTools: []string{}, SizeClass: pgT("s"), MilestonesFrozen: []byte(`[]`),
	}
	apply := func(stage string, want int64) {
		t.Helper()
		rows, err := f.q.SetRunAutopilotPlan(ctx, p)
		if err != nil || rows != want {
			t.Fatalf("%s application: rows=%d err=%v, want %d", stage, rows, err, want)
		}
		if want == 0 {
			assertReason(stage, "interrupted")
			var untouched bool
			if err := f.pool.QueryRow(ctx, `SELECT plan_md IS NULL AND milestones_frozen IS NULL FROM runs WHERE id = $1`, f.runID).Scan(&untouched); err != nil {
				t.Fatal(err)
			}
			if !untouched {
				t.Fatalf("%s: refused application mutated canonical plan", stage)
			}
		}
	}
	apply("REVISE", 0)
	mustExec(ctx, t, f.pool, `UPDATE cross_checks SET verdict = 'approve' WHERE lead_run_id = $1`, f.runID)
	p.CandidateDigest = []byte("different")
	apply("APPROVE with wrong digest", 0)
	p.CandidateDigest = []byte("digest")
	apply("exact APPROVE", 1)
	assertReason("exact APPROVE", "")
	var canonical bool
	if err := f.pool.QueryRow(ctx, `SELECT plan_md = 'approved' AND milestones_frozen = '[]'::jsonb
		AND required_capabilities = '{}' AND required_tools = '{}' AND size_class = 's'
		FROM runs WHERE id = $1`, f.runID).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if !canonical {
		t.Fatal("exact APPROVE did not apply the canonical plan fields")
	}
}

func TestPlanCrossCheckGuardsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_required = true, auto_approve = true, plan_source = 'agent' WHERE id = $1`, f.runID)
	planRows, err := f.q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("unchecked"), CandidateDigest: []byte("digest"),
		InferredCapabilities: []string{}, InferredTools: []string{}, SizeClass: pgT("s"), MilestonesFrozen: []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("SetRunAutopilotPlan: %v", err)
	}
	if planRows != 0 {
		t.Fatalf("unchecked plan wrote %d rows", planRows)
	}
	progressRows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), MilestonesCompleted: []byte(`["m1"]`),
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if progressRows != 0 {
		t.Fatalf("pre-plan progress wrote %d rows", progressRows)
	}
	completedRows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		ID: f.runID, WorkerID: pgU(f.workerID),
	})
	if err != nil {
		t.Fatalf("SetRunCompleted: %v", err)
	}
	if completedRows != 0 {
		t.Fatalf("unchecked run completed: %d rows", completedRows)
	}
}
