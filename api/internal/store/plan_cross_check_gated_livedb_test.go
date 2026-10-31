package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3708: gated Plan cross-check against a REAL Postgres. The checker runs before the first
// human plan gate but never grants approval, so the SQL backstops (SetRunRunning, the
// enforce_gated_plan_approval trigger, the mode CHECK) are what these tests execute.

func setupGatedRun(ctx context.Context, t *testing.T) *awaitingInputFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	f, done := setupAwaitingInput(ctx, t, dsn)
	t.Cleanup(func() {
		defer done()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := f.pool.Exec(cleanupCtx, `DELETE FROM cross_checks WHERE lead_run_id = $1`, f.runID); err != nil {
			t.Logf("cleanup cross-checks: %v", err)
		}
		if _, err := f.pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
			t.Logf("cleanup tenant: %v", err)
		}
	})
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = true, harness = 'claude',
		claim_generation = 1, completion_contract_version = 1 WHERE id = $1`, f.runID)
	return f
}

// gatedPark reports the awaiting_approval gate the way an upgraded worker does: every
// approval-bearing field present, the claim generation stamped.
func gatedPark(ctx context.Context, t *testing.T, f *awaitingInputFixture, plan string, generation int64, mutate func(*store.SetRunAwaitingApprovalParams)) {
	t.Helper()
	p := store.SetRunAwaitingApprovalParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT(plan),
		ClaimGeneration:     pgtype.Int8{Int64: generation, Valid: true},
		MilestonesCandidate: []byte(`[]`), InferredCapabilities: []string{}, InferredTools: []string{},
		SizeClass: pgT("s"),
	}
	if mutate != nil {
		mutate(&p)
	}
	rows, err := f.q.SetRunAwaitingApproval(ctx, p)
	if err != nil || rows != 1 {
		t.Fatalf("park %q: rows=%d err=%v", plan, rows, err)
	}
}

func gatedApprove(ctx context.Context, t *testing.T, f *awaitingInputFixture, revision int64) {
	t.Helper()
	input, err := f.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: f.runID, Body: pgT("{}"), AgentSource: pgT("own"),
		AgentExclusions: []byte("[]"), ExpectedGateRevision: pgtype.Int8{Int64: revision, Valid: true},
		ContractSource: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("approve revision %d: %v", revision, err)
	}
	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET applied_at = now(), disposition = 'applied' WHERE id = $1`, input.ID)
}

func gatedReason(ctx context.Context, t *testing.T, f *awaitingInputFixture) pgtype.Text {
	t.Helper()
	var reason pgtype.Text
	if err := f.pool.QueryRow(ctx, `SELECT plan_cross_check_gate_reason FROM runs WHERE id = $1`, f.runID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	return reason
}

func requireReason(ctx context.Context, t *testing.T, f *awaitingInputFixture, stage string, want string) {
	t.Helper()
	got := gatedReason(ctx, t, f)
	if want == "" {
		if got.Valid {
			t.Fatalf("%s: gate reason = %q, want NULL", stage, got.String)
		}
		return
	}
	if !got.Valid || got.String != want {
		t.Fatalf("%s: gate reason = %v, want %q", stage, got, want)
	}
}

type gatedCheckRow struct {
	round      int
	generation int64
	verdict    string
	reason     string // reason_class; empty = NULL
	plan       string
	digest     string
	limit      int // automatic revision limit; 0 with rounds=false is the single-round shape
	rounds     bool
}

func insertGatedCheck(ctx context.Context, t *testing.T, f *awaitingInputFixture, c gatedCheckRow) {
	t.Helper()
	var reason any
	if c.reason != "" {
		reason = c.reason
	}
	decided := "now()"
	if c.verdict == "pending" {
		decided = "NULL"
	}
	interrupted := "NULL"
	if c.verdict == "failed" {
		interrupted = "now()"
	}
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation,
		plan_md, milestones, required_capabilities, required_tools, size_class, base_commit, candidate_digest,
		checker_harness, verdict, reason_class, decided_at, deadline_at, interrupted_at,
		automatic_revision_limit, automatic_rounds_enabled, wait_credited)
		VALUES ($1,'plan',$2,$3,$4,'[]','{}','{}','s','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',$5,
		'codex',$6,$7,`+decided+`, now() + interval '30 minutes', `+interrupted+`, $8, $9, true)`,
		f.runID, c.round, c.generation, c.plan, []byte(c.digest), c.verdict, reason, c.limit, c.rounds)
}

func requirePgCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != constraint {
		t.Fatalf("want check_violation on %s, got %v", constraint, err)
	}
}

// The mode CHECK keeps required, gated and neither mutually exclusive, and keeps gated off
// auto-approved rows (so SetRunAutopilotPlan's auto_approve guard excludes gated rows in the
// database itself).
func TestGatedPlanCrossCheckModeCheckLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	for name, assignment := range map[string]string{
		"required and gated": "plan_cross_check_required = true",
		"gated auto_approve": "auto_approve = true",
	} {
		_, err := f.pool.Exec(ctx, "UPDATE runs SET "+assignment+" WHERE id = $1", f.runID)
		if err == nil {
			t.Fatalf("%s: UPDATE accepted", name)
		}
		requirePgCheckViolation(t, err, "runs_plan_cross_check_mode_check")
	}
	// A kind outside issue, prompt and ci_fix cannot be gated.
	_, err := f.pool.Exec(ctx, `UPDATE runs SET kind = 'chat' WHERE id = $1`, f.runID)
	if err == nil {
		t.Fatal("gated chat run accepted")
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("gated chat run: want a check violation, got %v", err)
	}
	// An ordinary row is untouched by the CHECK.
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false, plan_cross_check_required = true, auto_approve = true WHERE id = $1`, f.runID)
}

// The reason CHECK admits worker_unsupported (and still refuses an unknown reason).
func TestGatedPlanCrossCheckReasonCheckLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gate_reason = 'worker_unsupported' WHERE id = $1`, f.runID)
	_, err := f.pool.Exec(ctx, `UPDATE runs SET plan_cross_check_gate_reason = 'bogus' WHERE id = $1`, f.runID)
	requirePgCheckViolation(t, err, "runs_plan_cross_check_gate_reason_check")
}

// SetRunRunning on a gated awaiting_approval row needs an applied, non-superseded approval BOUND to
// the current gate revision; the any-prior-approve shortcut no longer applies to gated rows.
func TestGatedPlanCrossCheckRunningBackstopLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	running := func(stage string, want int64) {
		t.Helper()
		rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{ID: f.runID, WorkerID: pgU(f.workerID)})
		if err != nil || rows != want {
			t.Fatalf("%s SetRunRunning: rows=%d err=%v, want %d", stage, rows, err, want)
		}
	}
	claimApproved := func(stage string, want bool) {
		t.Helper()
		claim, err := f.q.GetRunClaimContext(ctx, f.runID)
		if err != nil || claim.HumanPlanApproved != want {
			t.Fatalf("%s claim approval=%v err=%v, want %v", stage, claim.HumanPlanApproved, err, want)
		}
	}
	completionRefused := func(stage string) {
		t.Helper()
		mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed' WHERE id = $1`, f.runID)
		mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
		_, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
			RunID: f.runID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`)})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s completion attempt: err=%v, want no rows", stage, err)
		}
		rows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: f.runID, WorkerID: pgU(f.workerID)})
		if err != nil || rows != 0 {
			t.Fatalf("%s completion: rows=%d err=%v, want 0", stage, rows, err)
		}
	}

	gatedPark(ctx, t, f, "plan A", 1, nil)
	rev1 := publishPlanCrossCheckGate(ctx, t, f, "plan A")
	running("no approval", 0)
	claimApproved("no approval", false)

	// The any-prior-approve shape: an applied approval that is not bound to a gate revision.
	mustExec(ctx, t, f.pool, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, applied_at, disposition)
		VALUES ($1, 'approve_plan', '{}', now(), now(), 'applied')`, f.runID)
	running("unbound prior approval", 0)
	claimApproved("unbound prior approval", false)

	// A bound approval for revision 1 goes stale once a human-visible revision publishes revision 2.
	gatedApprove(ctx, t, f, rev1)
	gatedPark(ctx, t, f, "plan B", 1, nil)
	rev2 := publishPlanCrossCheckGate(ctx, t, f, "plan B")
	if rev2 <= rev1 {
		t.Fatalf("revision did not advance: %d -> %d", rev1, rev2)
	}
	running("stale approval", 0)
	claimApproved("stale approval", false)

	// Completion of a gated run needs the bound approval too (SetRunCompleted / RecordCompletionAttempt).
	// Completion is exercised from a reclaimed running row, so the row is moved without the approval edge.
	completionRefused("stale approval")
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'awaiting_approval' WHERE id = $1`, f.runID)

	gatedApprove(ctx, t, f, rev2)
	claimApproved("current approval", true)
	running("current approval", 1)
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, f.runID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("status = %q err=%v, want running", status, err)
	}
	if _, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
		RunID: f.runID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`)}); err != nil {
		t.Fatalf("approved completion attempt: %v", err)
	}
	rows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: f.runID, WorkerID: pgU(f.workerID)})
	if err != nil || rows != 1 {
		t.Fatalf("approved completion: rows=%d err=%v", rows, err)
	}
}

// An ordinary (neither required nor gated) row keeps the any-prior-approve arm.
func TestGatedPlanCrossCheckOrdinaryRunningUnchangedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false WHERE id = $1`, f.runID)
	gatedPark(ctx, t, f, "plan A", 1, nil)
	rows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{ID: f.runID, WorkerID: pgU(f.workerID)})
	if err != nil || rows != 0 {
		t.Fatalf("ordinary run without approval: rows=%d err=%v, want 0", rows, err)
	}
	mustExec(ctx, t, f.pool, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, applied_at, disposition)
		VALUES ($1, 'approve_plan', '{}', now(), now(), 'applied')`, f.runID)
	rows, err = f.q.SetRunRunning(ctx, store.SetRunRunningParams{ID: f.runID, WorkerID: pgU(f.workerID)})
	if err != nil || rows != 1 {
		t.Fatalf("ordinary run with a prior approval: rows=%d err=%v, want 1", rows, err)
	}
}

// SetRunAutopilotPlan never writes a plan onto a gated row.
func TestGatedPlanCrossCheckAutopilotPlanRefusedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_source = 'agent', status = 'running' WHERE id = $1`, f.runID)
	rows, err := f.q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("autopilot"), CandidateDigest: []byte("digest"),
		InferredCapabilities: []string{}, InferredTools: []string{}, SizeClass: pgT("s"), MilestonesFrozen: []byte(`[]`),
	})
	if err != nil || rows != 0 {
		t.Fatalf("gated autopilot plan: rows=%d err=%v, want 0", rows, err)
	}
	var stored pgtype.Text
	if err := f.pool.QueryRow(ctx, `SELECT plan_md FROM runs WHERE id = $1`, f.runID).Scan(&stored); err != nil || stored.Valid {
		t.Fatalf("refused autopilot plan mutated plan_md: %v err=%v", stored, err)
	}
}

// The database-level backstop: under ANY api binary (including one that still has the
// any-prior-approve arm) a gated gate cannot start implementing without a current bound approval.
func TestGatedPlanApprovalTriggerLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	raw := func() error {
		_, err := f.pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
		return err
	}
	refused := func(stage string) {
		t.Helper()
		requirePgCheckViolation(t, raw(), "runs_gated_plan_approval")
		var status string
		if err := f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, f.runID).Scan(&status); err != nil || status != "awaiting_approval" {
			t.Fatalf("%s: refused update changed status to %q (err=%v)", stage, status, err)
		}
	}

	gatedPark(ctx, t, f, "plan A", 1, nil)
	rev1 := publishPlanCrossCheckGate(ctx, t, f, "plan A")
	refused("no approval")

	mustExec(ctx, t, f.pool, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, applied_at, disposition)
		VALUES ($1, 'approve_plan', '{}', now(), now(), 'applied')`, f.runID)
	refused("unbound approval")

	gatedApprove(ctx, t, f, rev1)
	gatedPark(ctx, t, f, "plan B", 1, nil)
	rev2 := publishPlanCrossCheckGate(ctx, t, f, "plan B")
	refused("stale approval from an earlier gate revision")

	gatedApprove(ctx, t, f, rev2)
	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET disposition = 'superseded'
		WHERE run_id = $1 AND gate_revision = $2`, f.runID, rev2)
	refused("superseded approval")
	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET disposition = 'applied'
		WHERE run_id = $1 AND gate_revision = $2`, f.runID, rev2)

	mustExec(ctx, t, f.pool, `UPDATE runs SET gate_presented_payload = jsonb_set(gate_presented_payload, '{plan_md}', '"other"') WHERE id = $1`, f.runID)
	refused("approval over a different presented plan")
	mustExec(ctx, t, f.pool, `UPDATE runs SET gate_presented_payload = jsonb_set(gate_presented_payload, '{plan_md}', to_jsonb(plan_md)) WHERE id = $1`, f.runID)

	// Edges the trigger does not guard stay open: leaving the gate for a terminal state.
	if _, err := f.pool.Exec(ctx, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("cancelling a gated gate: %v", err)
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'awaiting_approval' WHERE id = $1`, f.runID)
	if err := raw(); err != nil {
		t.Fatalf("current bound approval refused: %v", err)
	}
}

// The trigger is inert for required and ordinary rows.
func TestGatedPlanApprovalTriggerInertLiveDB(t *testing.T) {
	ctx := context.Background()
	for name, set := range map[string]string{
		"required": "plan_cross_check_gated = false, plan_cross_check_required = true",
		"ordinary": "plan_cross_check_gated = false",
	} {
		t.Run(name, func(t *testing.T) {
			f := setupGatedRun(ctx, t)
			mustExec(ctx, t, f.pool, "UPDATE runs SET "+set+" WHERE id = $1", f.runID)
			gatedPark(ctx, t, f, "plan A", 1, nil)
			if _, err := f.pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID); err != nil {
				t.Fatalf("trigger fired on a %s row: %v", name, err)
			}
		})
	}
}

// SetRunAwaitingApproval derives the gate reason; for a gated run an APPROVE is "passed" only
// when the current-generation approve row is bound to exactly the candidate being presented.
func TestGatedPlanCrossCheckApproveBindingLiveDB(t *testing.T) {
	ctx := context.Background()
	exact := func(p *store.SetRunAwaitingApprovalParams) { p.CandidateDigest = []byte("digest") }
	for _, tc := range []struct {
		name   string
		mutate func(*store.SetRunAwaitingApprovalParams)
		want   string
	}{
		{name: "exact candidate", mutate: exact, want: ""},
		{name: "missing digest", mutate: func(p *store.SetRunAwaitingApprovalParams) {}, want: "interrupted"},
		{name: "digest mismatch", mutate: func(p *store.SetRunAwaitingApprovalParams) { p.CandidateDigest = []byte("other") }, want: "interrupted"},
		{name: "plan mismatch", mutate: func(p *store.SetRunAwaitingApprovalParams) { exact(p); p.PlanMd = pgT("not the checked plan") }, want: "interrupted"},
		{name: "milestones mismatch", mutate: func(p *store.SetRunAwaitingApprovalParams) { exact(p); p.MilestonesCandidate = []byte(`[{"id":"x"}]`) }, want: "interrupted"},
		{name: "missing milestones", mutate: func(p *store.SetRunAwaitingApprovalParams) { exact(p); p.MilestonesCandidate = nil }, want: "interrupted"},
		{name: "capabilities mismatch", mutate: func(p *store.SetRunAwaitingApprovalParams) { exact(p); p.InferredCapabilities = []string{"docker"} }, want: "interrupted"},
		{name: "tools mismatch", mutate: func(p *store.SetRunAwaitingApprovalParams) { exact(p); p.InferredTools = []string{"go"} }, want: "interrupted"},
		{name: "size mismatch", mutate: func(p *store.SetRunAwaitingApprovalParams) { exact(p); p.SizeClass = pgT("m") }, want: "interrupted"},
		{name: "worker refusal keeps its reason", mutate: func(p *store.SetRunAwaitingApprovalParams) {
			exact(p)
			p.PlanCrossCheckGateReason = pgT("planning_diff_refused")
		}, want: "planning_diff_refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupGatedRun(ctx, t)
			insertGatedCheck(ctx, t, f, gatedCheckRow{round: 1, generation: 1, verdict: "approve", plan: "checked plan", digest: "digest"})
			gatedPark(ctx, t, f, "checked plan", 1, tc.mutate)
			requireReason(ctx, t, f, tc.name, tc.want)
		})
	}

	t.Run("older generation approve", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		mustExec(ctx, t, f.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, f.runID)
		insertGatedCheck(ctx, t, f, gatedCheckRow{round: 1, generation: 1, verdict: "approve", plan: "checked plan", digest: "digest"})
		gatedPark(ctx, t, f, "checked plan", 2, exact)
		if got := gatedReason(ctx, t, f); !got.Valid || got.String == "" {
			t.Fatalf("an approve from an earlier claim generation presented as passed")
		}
	})

	t.Run("required runs keep the ordinary approve arm", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false, plan_cross_check_required = true WHERE id = $1`, f.runID)
		insertGatedCheck(ctx, t, f, gatedCheckRow{round: 1, generation: 1, verdict: "approve", plan: "checked plan", digest: "digest"})
		// No digest supplied: a required run is not subject to the gated binding.
		gatedPark(ctx, t, f, "checked plan", 1, nil)
		requireReason(ctx, t, f, "required approve", "")
	})
}

// A check that lost custody on the last allowed round never got a verdict: a gated run derives
// interrupted there, where a required run (and a decided REVISE) derive revisions_exhausted.
func TestGatedPlanCrossCheckLastRoundCustodyLossLiveDB(t *testing.T) {
	ctx := context.Background()
	last := func(verdict, reason string) gatedCheckRow {
		return gatedCheckRow{round: 2, generation: 1, verdict: verdict, reason: reason, plan: "checked plan", digest: "digest", limit: 1, rounds: true}
	}
	for _, tc := range []struct {
		name  string
		gated bool
		row   gatedCheckRow
		want  string
	}{
		{name: "gated superseded on the last round", gated: true, row: last("failed", "superseded"), want: "interrupted"},
		{name: "gated approved_not_stored on the last round", gated: true, row: last("failed", "approved_not_stored"), want: "interrupted"},
		{name: "gated decided revise on the last round", gated: true, row: last("revise", ""), want: "revisions_exhausted"},
		{name: "required superseded on the last round", gated: false, row: last("failed", "superseded"), want: "revisions_exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupGatedRun(ctx, t)
			if !tc.gated {
				mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false, plan_cross_check_required = true WHERE id = $1`, f.runID)
			}
			insertGatedCheck(ctx, t, f, tc.row)
			gatedPark(ctx, t, f, "checked plan", 1, nil)
			requireReason(ctx, t, f, tc.name, tc.want)
		})
	}
}

// Arm (a): a presented gated gate keeps its stored reason when a requeued lead re-presents it, at a
// later claim generation, instead of being re-derived (a presented gate is never re-checked).
func TestGatedPlanCrossCheckReasonRetentionLiveDB(t *testing.T) {
	ctx := context.Background()
	t.Run("worker_unsupported survives requeue", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		gatedPark(ctx, t, f, "plan A", 1, func(p *store.SetRunAwaitingApprovalParams) { p.ServerGateReason = pgT("worker_unsupported") })
		requireReason(ctx, t, f, "gen 1", "worker_unsupported")
		publishPlanCrossCheckGate(ctx, t, f, "plan A")
		mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued', claim_generation = 2 WHERE id = $1`, f.runID)
		gatedPark(ctx, t, f, "plan A", 2, nil)
		requireReason(ctx, t, f, "re-presented at gen 2", "worker_unsupported")
	})
	t.Run("passed survives requeue", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		insertGatedCheck(ctx, t, f, gatedCheckRow{round: 1, generation: 1, verdict: "approve", plan: "checked plan", digest: "digest"})
		gatedPark(ctx, t, f, "checked plan", 1, func(p *store.SetRunAwaitingApprovalParams) { p.CandidateDigest = []byte("digest") })
		requireReason(ctx, t, f, "gen 1", "")
		publishPlanCrossCheckGate(ctx, t, f, "checked plan")
		mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued', claim_generation = 2 WHERE id = $1`, f.runID)
		gatedPark(ctx, t, f, "checked plan", 2, nil)
		requireReason(ctx, t, f, "re-presented at gen 2", "")
	})
	t.Run("a human-revised plan clears the reason", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		gatedPark(ctx, t, f, "plan A", 1, func(p *store.SetRunAwaitingApprovalParams) { p.ServerGateReason = pgT("worker_unsupported") })
		publishPlanCrossCheckGate(ctx, t, f, "plan A")
		gatedPark(ctx, t, f, "plan A, revised", 1, nil)
		requireReason(ctx, t, f, "revised", "")
	})
}

// The server-recorded worker_unsupported reason: only for a gated run with no cross-check row, at
// any claim generation (the no-row rule would otherwise derive interrupted from generation 2).
func TestGatedPlanCrossCheckWorkerUnsupportedLiveDB(t *testing.T) {
	ctx := context.Background()
	server := func(p *store.SetRunAwaitingApprovalParams) { p.ServerGateReason = pgT("worker_unsupported") }
	for _, generation := range []int64{1, 2} {
		f := setupGatedRun(ctx, t)
		mustExec(ctx, t, f.pool, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, f.runID, generation)
		gatedPark(ctx, t, f, "plan A", generation, server)
		requireReason(ctx, t, f, "gated, no check row", "worker_unsupported")
	}
	t.Run("not for an ordinary run", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false WHERE id = $1`, f.runID)
		gatedPark(ctx, t, f, "plan A", 1, server)
		requireReason(ctx, t, f, "ordinary run", "")
	})
	t.Run("not when a check row exists", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		insertGatedCheck(ctx, t, f, gatedCheckRow{round: 1, generation: 1, verdict: "block", reason: "block", plan: "plan A", digest: "digest"})
		gatedPark(ctx, t, f, "plan A", 1, server)
		requireReason(ctx, t, f, "block row", "block")
	})
}

// ListPlanCrossCheckRounds: newest five, owner scoped, findings capped without dropping the round.
func TestListPlanCrossCheckRoundsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := setupGatedRun(ctx, t)
	for round := 1; round <= 7; round++ {
		insertGatedCheck(ctx, t, f, gatedCheckRow{round: round, generation: 1, verdict: "revise", reason: "revise", plan: "plan " + string(rune('A'+round-1)), digest: "d"})
	}
	mustExec(ctx, t, f.pool, `UPDATE cross_checks SET findings = '[{"file":"a.go","severity":"high","summary":"s","rationale":"r"}]'::jsonb
		WHERE lead_run_id = $1 AND round = 7`, f.runID)
	mustExec(ctx, t, f.pool, `UPDATE cross_checks SET findings = jsonb_build_array(jsonb_build_object('rationale', repeat('x', 70000)))
		WHERE lead_run_id = $1 AND round = 6`, f.runID)
	rows, err := f.q.ListPlanCrossCheckRounds(ctx, store.ListPlanCrossCheckRoundsParams{LeadRunID: f.runID, UserID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("rounds = %d, want the newest 5", len(rows))
	}
	for i, row := range rows {
		if want := int32(7 - i); row.Round != want {
			t.Fatalf("row %d round = %d, want %d (newest first)", i, row.Round, want)
		}
	}
	if len(rows[0].Findings) == 0 || !strings.Contains(string(rows[0].Findings), "a.go") {
		t.Fatalf("round 7 findings = %q", rows[0].Findings)
	}
	if rows[1].Findings != nil {
		t.Fatalf("round 6 findings over the cap were returned (%d bytes)", len(rows[1].Findings))
	}
	if !rows[0].PlanMd.Valid || rows[0].PlanMd.String != "plan G" || rows[0].LeadClaimGeneration != 1 || !rows[0].DeadlineAt.Valid {
		t.Fatalf("round 7 candidate/generation/deadline = %+v", rows[0])
	}
	other, err := f.q.ListPlanCrossCheckRounds(ctx, store.ListPlanCrossCheckRoundsParams{LeadRunID: f.runID, UserID: uuid.New()})
	if err != nil || len(other) != 0 {
		t.Fatalf("another owner read %d rounds (err=%v)", len(other), err)
	}
}

// CreatePlanCrossCheckChild admits a lead that is auto-approved AND required, or one that is NOT
// auto-approved and gated; HasLivePlanCrossCheck reads a pending round of either mode.
func TestGatedPlanCrossCheckChildAndLiveLiveDB(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		set   string
		child bool
	}{
		{name: "gated, not auto-approved", set: "plan_cross_check_gated = true", child: true},
		{name: "required, auto-approved", set: "plan_cross_check_gated = false, plan_cross_check_required = true, auto_approve = true", child: true},
		{name: "required, not auto-approved", set: "plan_cross_check_gated = false, plan_cross_check_required = true", child: false},
		{name: "neither", set: "plan_cross_check_gated = false", child: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupGatedRun(ctx, t)
			mustExec(ctx, t, f.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`,
				f.workerID, []string{"codex_harness_v1", "codex_runtime_v2", "cross_check_v1", "codex_custom_model_v1"})
			mustExec(ctx, t, f.pool, "UPDATE runs SET "+tc.set+" WHERE id = $1", f.runID)
			child, err := f.q.CreatePlanCrossCheckChild(ctx, store.CreatePlanCrossCheckChildParams{
				ChildID: uuid.New(), ChildHarness: "codex", BudgetWallSeconds: 300, LeadRunID: f.runID,
				UserID: f.userID, WorkerID: pgU(f.workerID), ClaimGeneration: 1})
			if tc.child != (err == nil) || (err != nil && !errors.Is(err, pgx.ErrNoRows)) {
				t.Fatalf("child creation: err=%v, want child=%v", err, tc.child)
			}
			if err != nil {
				return
			}
			mustExec(ctx, t, f.pool, `INSERT INTO cross_checks (lead_run_id, checker_run_id, stage, round, lead_claim_generation,
				plan_md, milestones, size_class, base_commit, candidate_digest, checker_harness, deadline_at)
				VALUES ($1, $2, 'plan', 1, 1, 'p', '[]', 's', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'd', 'codex', now() + interval '30 minutes')`,
				f.runID, child.ID)
			live, err := f.q.HasLivePlanCrossCheck(ctx, f.runID)
			if err != nil || !live {
				t.Fatalf("pending round not live: live=%v err=%v", live, err)
			}
		})
	}
	t.Run("neither mode has no live check", func(t *testing.T) {
		f := setupGatedRun(ctx, t)
		mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_gated = false WHERE id = $1`, f.runID)
		insertGatedCheck(ctx, t, f, gatedCheckRow{round: 1, generation: 1, verdict: "pending", plan: "p", digest: "d"})
		live, err := f.q.HasLivePlanCrossCheck(ctx, f.runID)
		if err != nil || live {
			t.Fatalf("an ordinary run reports a live check: live=%v err=%v", live, err)
		}
	})
}
