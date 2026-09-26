package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestDiscardedApproveIsNotApprovalLiveDB pins issue #1604's DISCARDED receipt: an approve_plan
// the worker dropped as stale is settled (applied_at set, disposition 'superseded') so it leaves
// ListReplayRunInputs, yet it never counts as a human approval. GetRunClaimContext's
// human_plan_approved stays false, the next claim carries plan_approved false and does not
// resume at "implementing", and SetRunRunning keeps refusing awaiting_approval -> running until
// a REAL applied approve exists. Only approve_plan rows can be discarded, and a row already
// applied as a real approval cannot be.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestDiscardedApproveIsNotApprovalLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	box := newBox(t)
	q := store.New(pool)
	svc := New(q, box, testParams())
	svc.SetTxBeginner(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	sealedPAT, err := box.Seal([]byte("bot-pat-disc1604-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedAnthropic, err := box.Seal([]byte("anthropic-disc1604-token-abcdef12345"))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("disc1604-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/disc1604', 'https://forge.e2e/g/disc1604', 'main', true)`, repoID, connID)
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), sealedAnthropic)

	wkrID := uuid.New()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-disc1604', $3, 'offline')`,
		wkrID, userID, wkrID[:])
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: wkrID, ProtocolCapabilities: []string{capability.InputReceiptsV1}}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	wkr, err := q.GetWorkerByID(ctx, wkrID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}

	// A run parked at the plan gate under generation 1, with a session (so an unapproved plan
	// resumes at awaiting_approval and an approved one at implementing).
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
	        claim_generation, auto_approve, plan_source, plan_md, session_id)
	      VALUES ($1, $2, $3, 'issue', 1604, 't', 'd', 'awaiting_approval', $4,
	        1, false, 'agent', '# Plan', 'sess-disc1604')`, runID, userID, repoID, wkrID)
	addInput := func(kind string, generation int64) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, consumed_claim_generation, consumed_worker_id)
		      VALUES ($1, $2, 'x', now(), $3, $4) RETURNING id`, runID, kind, generation, wkrID).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
		return id
	}
	settled := func(id int64) (bool, string) {
		t.Helper()
		var applied bool
		var disposition *string
		if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL, disposition FROM run_user_inputs WHERE id = $1`, id).Scan(&applied, &disposition); err != nil {
			t.Fatal(err)
		}
		if disposition == nil {
			return applied, ""
		}
		return applied, *disposition
	}
	humanApproved := func() bool {
		t.Helper()
		rc, err := q.GetRunClaimContext(ctx, runID)
		if err != nil {
			t.Fatalf("GetRunClaimContext: %v", err)
		}
		return rc.HumanPlanApproved
	}
	setRunning := func() int64 {
		t.Helper()
		rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: runID, WorkerID: pgconv.UUID(wkrID), IterationCount: 1})
		if err != nil {
			t.Fatalf("SetRunRunning: %v", err)
		}
		return rows
	}

	stale := addInput("approve_plan", 1)
	follow := addInput("follow_up", 1)

	// A non-approve row cannot be discarded, alone or in a mixed batch, and nothing is settled.
	for _, ids := range [][]int64{{follow}, {stale, follow}} {
		if _, err := svc.DiscardInputs(ctx, wkr, runID, 1, ids); !errors.Is(err, ErrInputReceiptInvalid) {
			t.Fatalf("DiscardInputs(%v) err = %v, want ErrInputReceiptInvalid", ids, err)
		}
	}
	if applied, _ := settled(stale); applied {
		t.Fatal("a refused mixed discard settled the approve")
	}

	res, err := svc.DiscardInputs(ctx, wkr, runID, 1, []int64{stale})
	if err != nil {
		t.Fatalf("DiscardInputs: %v", err)
	}
	if !res.Active || len(res.Inputs) != 1 || res.Inputs[0].ID != stale {
		t.Fatalf("DiscardInputs = %+v", res)
	}
	if applied, disposition := settled(stale); !applied || disposition != "superseded" {
		t.Fatalf("discarded approve: applied=%v disposition=%q, want applied and superseded", applied, disposition)
	}
	// Idempotent retry of a lost reply.
	if _, err := svc.DiscardInputs(ctx, wkr, runID, 1, []int64{stale}); err != nil {
		t.Fatalf("DiscardInputs retry: %v", err)
	}
	// An APPLIED for a discarded row is refused: it was never an approval.
	if _, err := svc.ApplyInputs(ctx, wkr, runID, 1, []int64{stale}); !errors.Is(err, ErrInputReceiptConflict) {
		t.Fatalf("ApplyInputs on a discarded approve err = %v, want a conflict", err)
	}
	replay, err := q.ListReplayRunInputs(ctx, runID)
	if err != nil {
		t.Fatalf("ListReplayRunInputs: %v", err)
	}
	if slices.ContainsFunc(replay, func(r store.ListReplayRunInputsRow) bool { return r.ID == stale }) {
		t.Fatalf("the discarded approve %d is still on the replay list %+v", stale, replay)
	}
	if !slices.ContainsFunc(replay, func(r store.ListReplayRunInputsRow) bool { return r.ID == follow }) {
		t.Fatalf("the follow_up %d left the replay list %+v", follow, replay)
	}

	// Not an approval: the claim context, the gate transition and the next claim all agree.
	if humanApproved() {
		t.Fatal("human_plan_approved is true with only a discarded approve")
	}
	if rows := setRunning(); rows != 0 {
		t.Fatalf("SetRunRunning left awaiting_approval with only a discarded approve (%d rows)", rows)
	}
	exec(`UPDATE runs SET status = 'queued', claim_released_at = now() WHERE id = $1`, runID)
	payload, err := svc.Claim(ctx, wkr, nil)
	if err != nil {
		t.Fatalf("svc.Claim: %v", err)
	}
	if payload == nil || payload.RunID != runID.String() || payload.ClaimGeneration != 2 {
		t.Fatalf("svc.Claim = %+v, want run %s at generation 2", payload, runID)
	}
	if payload.PlanApproved {
		t.Fatal("claim plan_approved is true with only a discarded approve")
	}
	if payload.ResumePhase != "awaiting_approval" {
		t.Fatalf("claim resume_phase = %q, want awaiting_approval (never implementing on a discarded approve)", payload.ResumePhase)
	}

	// A row already applied as a REAL approval cannot be discarded afterwards.
	exec(`UPDATE runs SET status = 'awaiting_approval' WHERE id = $1`, runID)
	realApprove := addInput("approve_plan", 2)
	if _, err := svc.ApplyInputs(ctx, wkr, runID, 2, []int64{realApprove}); err != nil {
		t.Fatalf("ApplyInputs real approve: %v", err)
	}
	if _, err := svc.DiscardInputs(ctx, wkr, runID, 2, []int64{realApprove}); !errors.Is(err, ErrInputReceiptConflict) {
		t.Fatalf("DiscardInputs on a real approval err = %v, want a conflict", err)
	}
	if applied, disposition := settled(realApprove); !applied || disposition != "" {
		t.Fatalf("real approve after a refused discard: applied=%v disposition=%q", applied, disposition)
	}
	// And with a real applied approve the gate opens and the approval is counted.
	if !humanApproved() {
		t.Fatal("human_plan_approved is false with a real applied approve")
	}
	if rows := setRunning(); rows != 1 {
		t.Fatalf("SetRunRunning refused awaiting_approval with a real applied approve (%d rows)", rows)
	}
}
