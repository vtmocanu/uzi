package handler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #209 M1 milestone gate, against a REAL Postgres, through sqlc-generated SQL.
//
// It lives in the handler package (not workersvc) on purpose: ./e2e/run-store-it.sh
// and CI's test:api-store-it sweep only ./internal/store/... and ./internal/handler/...
// for the *LiveDB suffix, so a workersvc-package live test would self-skip forever and
// never run in CI. The service is exercised directly (svc.CreateRun / svc.Claim) — no
// HTTP layer is needed for the create → claim path — while reusing this package's real
// mustExecT / newHandlerTestBox harness (the same one board_order_livedb_test.go uses).
//
// Two tests:
//   - TestSeededRunClaimCarriesApprovedPlanNoInputRowLiveDB is the PRD's stated gate:
//     a seeded run's claim carries plan_md, plan_approved:true and the selection, with
//     NO approve_plan input row anywhere. That last assertion is the one the PRD says
//     would have caught the D8 hole, so it is the milestone's real gate.
//   - TestSeededRunFallThroughToGateDisarmsLiveDB is the D8 reproduction: a seeded run
//     that falls through to the plan gate has SetRunAwaitingApproval flip plan_source
//     to 'agent', and after a stale-worker requeue the re-claim is DISARMED
//     (plan_approved:false), not armed. Without the plan_source='agent' write the final
//     assertion reads true.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type seededPlanFixture struct {
	pool   *pgxpool.Pool
	box    *secretbox.Box
	svc    *workersvc.Service
	owner  uuid.UUID
	repoID uuid.UUID
	wkr    store.Worker
	iid    int64
}

// newSeededPlanFixture stands up one owner with a forge connection, a repo, a cached
// PRD issue (has_prd_link so every create-time gate passes), a default anthropic token,
// and one worker whose heartbeat is NULL (so the D8 requeue path treats it as stale).
// The PAT and the token are sealed with the fixture's own box so the claim can open both.
func newSeededPlanFixture(ctx context.Context, t *testing.T) seededPlanFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	box := newHandlerTestBox(t)
	q := store.New(pool)

	f := seededPlanFixture{
		pool:   pool,
		box:    box,
		svc:    workersvc.New(q, box, workersvc.Params{}),
		owner:  uuid.New(),
		repoID: uuid.New(),
		iid:    42,
	}

	// A unique project id per fixture so parallel LiveDB rows on the shared database
	// never collide on forge_connections' project/bot uniqueness.
	projectID := int64(uuid.New().ID())
	sealedPAT, err := box.Seal([]byte("glpat-dummy-fixture-token-000000"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedTok, err := box.Seal([]byte("anthropic-oauth-fixture-token-000000"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}

	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.owner, fmt.Sprintf("seed-owner-%s@e2e", uuid.NewString()[:8]))
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	          VALUES ($1, $2, 'gitlab', 'https://forge.example', 'uzi-bot', $3, $4)`,
		connID, f.owner, projectID, sealedPAT)
	mustExecT(ctx, t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	          VALUES ($1, $2, $3, 'g/seed', 'https://forge.example/g/seed', 'main', true)`,
		f.repoID, connID, projectID)
	mustExecT(ctx, t, pool, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, 'seed issue', 'opened', '["PRD"]'::jsonb, 'https://x', true, now(), now())`,
		f.repoID, f.iid)
	mustExecT(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'default', true, $3, 'master')`,
		f.owner, store.KindAnthropicToken, sealedTok)

	// A worker with a NULL heartbeat: claimable now, and stale for the D8 requeue path
	// (RequeueRunsOfStaleWorkers matches last_heartbeat_at IS NULL for any cutoff). The
	// token_hash is UNIQUE and the LiveDB runner shares one database, so it is derived
	// from a fresh uuid rather than a fixed constant that would collide across fixtures.
	wkrID := uuid.New()
	tokenHash := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'seed-worker', $3, 'online')`,
		wkrID, f.owner, tokenHash[:])
	f.wkr = store.Worker{ID: wkrID, UserID: f.owner}
	return f
}

func (f seededPlanFixture) planSourceOf(ctx context.Context, t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var src string
	if err := f.pool.QueryRow(ctx, `SELECT plan_source FROM runs WHERE id = $1`, runID).Scan(&src); err != nil {
		t.Fatalf("read plan_source: %v", err)
	}
	return src
}

func (f seededPlanFixture) approvePlanInputCount(ctx context.Context, t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'approve_plan'`, runID).Scan(&n); err != nil {
		t.Fatalf("count approve_plan inputs: %v", err)
	}
	return n
}

func TestSeededRunClaimCarriesApprovedPlanNoInputRowLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSeededPlanFixture(ctx, t)

	const plan = "# Plan\n\nImplement the widget in widget.go. Done when the tests pass."
	sel := workersvc.AgentSelection{Source: workersvc.AgentSourceRepo, Exclusions: []string{"reviewer"}}
	run, err := f.svc.CreateRun(ctx, f.owner, f.repoID, f.iid, "issue body", false, nil,
		&workersvc.SeededPlan{PlanMD: plan, Selection: &sel})
	if err != nil {
		t.Fatalf("CreateRun (seeded): %v", err)
	}
	if run.PlanSource != "seeded" {
		t.Fatalf("created run plan_source = %q, want \"seeded\"", run.PlanSource)
	}
	if run.Status != "queued" {
		t.Fatalf("created run status = %q, want queued", run.Status)
	}

	payload, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a claim payload for the queued seeded run, got idle")
	}

	// THE MILESTONE GATE: the claim carries the seeded plan, is pre-approved, and names
	// the run seeded — with no human approval anywhere behind it.
	if !payload.PlanApproved {
		t.Error("plan_approved = false on a seeded claim; the third disjunct (D8) is the mechanism and must fire")
	}
	if payload.PlanSource != "seeded" {
		t.Errorf("claim plan_source = %q, want \"seeded\"", payload.PlanSource)
	}
	if payload.PlanMd == nil || *payload.PlanMd != plan {
		t.Errorf("claim plan_md = %v, want the seeded plan verbatim", payload.PlanMd)
	}
	if payload.AgentSelection == nil {
		t.Fatal("claim carried no agent selection; the seeded roster must ride the claim")
	}
	if payload.AgentSelection.Source != workersvc.AgentSourceRepo {
		t.Errorf("claim selection source = %q, want %q", payload.AgentSelection.Source, workersvc.AgentSourceRepo)
	}
	if len(payload.AgentSelection.Exclusions) != 1 || payload.AgentSelection.Exclusions[0] != "reviewer" {
		t.Errorf("claim selection exclusions = %v, want [reviewer]", payload.AgentSelection.Exclusions)
	}

	// The assertion the PRD says would have caught the D8 hole: a seeded run reaches
	// approved WITHOUT ever writing an approve_plan input. If a future change routed the
	// approval through the human gate instead of the disjunct, this count would be 1.
	if n := f.approvePlanInputCount(ctx, t, run.ID); n != 0 {
		t.Errorf("approve_plan input rows = %d, want 0 — a seeded run must reach approved with no gate", n)
	}
}

// TestSeededRunFallThroughToGateDisarmsLiveDB reproduces the D8 chain and proves the
// fix disarms it (the PRD's standing caveat: reproduce it before trusting the fix).
//
// A seeded run (plan_approved via the disjunct) that falls through to the plan gate has
// SetRunAwaitingApproval rewrite plan_md with a worker-authored plan. If plan_source
// stayed 'seeded', a stale-worker requeue — a direct UPDATE that bypasses SetRunRunning's
// consumed-approve_plan guard — would re-claim carrying plan_approved:true over an
// unreviewed plan (implement, no gate). The fix sets plan_source='agent' in the SAME
// UPDATE, so the disjunct stops firing and the re-claim re-plans instead.
func TestSeededRunFallThroughToGateDisarmsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSeededPlanFixture(ctx, t)

	sel := workersvc.AgentSelection{Source: workersvc.AgentSourceRepo, Exclusions: []string{"reviewer"}}
	run, err := f.svc.CreateRun(ctx, f.owner, f.repoID, f.iid, "issue body", false, nil,
		&workersvc.SeededPlan{PlanMD: "# Seeded plan\nImplement it.", Selection: &sel})
	if err != nil {
		t.Fatalf("CreateRun (seeded): %v", err)
	}

	// Claim #1: at birth the run is armed (plan_approved true), which is correct — this
	// is the state the D8 fall-through must NOT be able to resurrect after the worker
	// overwrites plan_md.
	first, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if first == nil || !first.PlanApproved {
		t.Fatalf("first claim should be pre-approved at birth; got %+v", first)
	}

	// Fall through to the plan gate: SetRunAwaitingApproval writes the worker's OWN
	// Phase-1 plan. The D8 safety fix is that this same UPDATE flips plan_source.
	q := store.New(f.pool)
	rows, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
		PlanMd:   pgtype.Text{String: "# Worker phase-1 plan\nSomething else entirely.", Valid: true},
		ID:       run.ID,
		WorkerID: pgtype.UUID{Bytes: f.wkr.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunAwaitingApproval: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunAwaitingApproval affected %d rows, want 1", rows)
	}
	if src := f.planSourceOf(ctx, t, run.ID); src != "agent" {
		t.Fatalf("plan_source = %q after the gate rewrote plan_md, want \"agent\" — the D8 fix did not fire", src)
	}

	// Requeue via the stale-worker path (the direct UPDATE that bypasses SetRunRunning's
	// guard — exactly the D8 chain). The fixture worker has a NULL heartbeat, so it is
	// stale for any cutoff.
	requeued, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 5,
		Cutoff:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	found := false
	for _, r := range requeued {
		if r.ID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stale run was not requeued; requeued=%+v", requeued)
	}

	// Re-claim: the run is now an ordinary agent-planned run with no approval behind it,
	// so it must come back DISARMED. Without the plan_source='agent' write above, this
	// assertion reads true and the run would implement a plan no human ever saw.
	second, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if second == nil {
		t.Fatal("expected a payload on re-claim, got idle")
	}
	if second.PlanApproved {
		t.Error("re-claim plan_approved = true — the D8 hole is OPEN: a requeued fall-through run is armed with an unreviewed plan")
	}
	if second.PlanSource != "agent" {
		t.Errorf("re-claim plan_source = %q, want \"agent\"", second.PlanSource)
	}
}
