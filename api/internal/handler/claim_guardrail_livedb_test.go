package handler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of PRD #66 M6 (D1 layer 3): the claim backstop. A run can be
// QUEUED while main is protected and only reach the claim after protection is
// removed (worker offline in between), so the single ForgePAT-attach choke point
// re-runs the same live, fail-closed privcheck.GuardRepo. This proves the Success
// Criterion end-to-end against a real Postgres and real branch-protection JSON:
//
//   - bot-can-push at claim → the run ends `failed` with a guardrail failure_reason
//     and the worker gets NO payload (idle) — it does not push;
//   - clean at claim → the claim succeeds and returns a payload.
//
// The run is INSERTED directly as `queued` (not via CreateRun), which is precisely
// the layer-1/2 bypass this backstop exists to catch: a run that was already queued
// before the capability was granted. Reuses M4/M5's fake-forge helper
// (newStartRunFakeForge, protMode, guardBotUserID) — same package.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

type claimGuardFixture struct {
	pool   *pgxpool.Pool
	svc    *workersvc.Service
	owner  uuid.UUID
	connID uuid.UUID
	wkr    store.Worker
	forge  *startRunFakeForge
}

func newClaimGuardFixture(ctx context.Context, t *testing.T) claimGuardFixture {
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
	q := store.New(pool)
	box := newHandlerTestBox(t)
	fg := newStartRunFakeForge(t)

	// Real forgesvc + real privcheck.Service + a real workersvc wired with the guard
	// via SetRepoGuard — the M6 seam under test, no fakes on the guard path.
	svc := forgesvc.New(q, box, 5*time.Second, nil)
	pcheck := privcheck.NewService(q, svc)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetRepoGuard(pcheck)

	f := claimGuardFixture{
		pool:   pool,
		svc:    wsvc,
		owner:  uuid.New(),
		connID: uuid.New(),
		forge:  fg,
	}

	sealedPAT, err := box.Seal([]byte("glpat-dummy-claim-token-000000")) //gitleaks:allow synthetic PAT fixture, sealed, never a real credential
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedTok, err := box.Seal([]byte("anthropic-oauth-claim-fixture-000000"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}

	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.owner, fmt.Sprintf("claim-owner-%s@e2e", uuid.NewString()[:8]))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'uzi-bot', $4, $5)`,
		f.connID, f.owner, fg.server.URL, guardBotUserID, sealedPAT)
	mustExecT(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'default', true, $3, 'master')`,
		f.owner, store.KindAnthropicToken, sealedTok)

	// A worker with a NULL heartbeat is claimable now; being the only worker for this
	// user, it never defers to a peer under the fleet-aware spread.
	wkrID := uuid.New()
	tokenHash := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'claim-worker', $3, 'online')`,
		wkrID, f.owner, tokenHash[:])
	f.wkr = store.Worker{ID: wkrID, UserID: f.owner}
	return f
}

// seedEnabledRepoAndQueuedRun inserts an enabled repo whose default-branch protection
// is served by mode, plus a QUEUED run for it — the exact state the claim backstop
// exists to catch (queued before the capability, claimed after). Returns the run id.
func (f claimGuardFixture) seedEnabledRepoAndQueuedRun(ctx context.Context, t *testing.T, projectID int64, iid int64, mode protMode) uuid.UUID {
	t.Helper()
	repoID, runID := uuid.New(), uuid.New()
	f.forge.mu.Lock()
	f.forge.prot[projectID] = mode
	f.forge.mu.Unlock()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.example/g/cg', 'main', true)`,
		repoID, f.connID, projectID, fmt.Sprintf("g/cg-%d", projectID))
	mustExecT(ctx, t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, $4, 'cg issue', 'body', 'queued', 'issue')`,
		runID, f.owner, repoID, iid)
	return runID
}

func (f claimGuardFixture) runStatusAndReason(ctx context.Context, t *testing.T, runID uuid.UUID) (string, string) {
	t.Helper()
	var status, reason string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, COALESCE(failure_reason, '') FROM runs WHERE id = $1`, runID).Scan(&status, &reason); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return status, reason
}

// The bot can push to protected main AT CLAIM → the run ends `failed` with a
// guardrail failure_reason and the worker gets NO payload (idle, never a 500).
func TestClaimGuardBlocksCanPushLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newClaimGuardFixture(ctx, t)
	runID := f.seedEnabledRepoAndQueuedRun(ctx, t, 501, 7, protCanPush)

	payload, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("Claim must report idle (nil error) for a guardrail-blocked run, got %v", err)
	}
	if payload != nil {
		t.Fatalf("a guardrail-blocked claim must return NO payload, got %+v", payload)
	}
	status, reason := f.runStatusAndReason(ctx, t, runID)
	if status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
	if !strings.Contains(reason, "default-branch guardrail") {
		t.Fatalf("failure_reason = %q, want it to name the guardrail", reason)
	}
}

// An unprotected default branch at claim is refused too — the Protected-first
// inversion must not read false,false as safe at the backstop either.
func TestClaimGuardBlocksUnprotectedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newClaimGuardFixture(ctx, t)
	runID := f.seedEnabledRepoAndQueuedRun(ctx, t, 502, 7, protUnprotected)

	payload, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatalf("an unprotected-main claim must return NO payload, got %+v", payload)
	}
	if status, _ := f.runStatusAndReason(ctx, t, runID); status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
}

// A branch-protection read error at claim refuses the run (fail closed, D3/R4).
func TestClaimGuardFailsClosedOnForgeErrorLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newClaimGuardFixture(ctx, t)
	runID := f.seedEnabledRepoAndQueuedRun(ctx, t, 503, 7, protError)

	payload, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatalf("a claim whose protection read errors must return NO payload, got %+v", payload)
	}
	if status, _ := f.runStatusAndReason(ctx, t, runID); status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
}

// Protected + clean at claim → the claim succeeds and returns a payload (the guard
// clears the repo and the run is handed the PAT).
func TestClaimGuardAllowsCleanLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newClaimGuardFixture(ctx, t)
	runID := f.seedEnabledRepoAndQueuedRun(ctx, t, 504, 7, protClean)

	payload, err := f.svc.Claim(ctx, f.wkr)
	if err != nil {
		t.Fatalf("Claim (clean): %v", err)
	}
	if payload == nil {
		t.Fatal("a clean repo must yield a claim payload, got idle")
	}
	if payload.RunID != runID.String() {
		t.Fatalf("payload RunID = %q, want %q", payload.RunID, runID)
	}
	if payload.Secrets.ForgePAT == "" {
		t.Error("a cleared claim must carry the bot PAT")
	}
	if status, reason := f.runStatusAndReason(ctx, t, runID); status == "failed" {
		t.Fatalf("a cleared run must not be failed, got failed with %q", reason)
	}
}
