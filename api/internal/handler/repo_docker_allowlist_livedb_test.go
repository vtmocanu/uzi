package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestListReposDockerAllowlistedLiveDB pins PRD #361 M1's computed, caller-scoped
// docker_allowlisted field against a REAL Postgres, through the ListRepos handler
// (GET /api/repos, enabled-only, no forge round-trip and no chi URL param).
//
// It proves three things at once:
//   - membership: a repo whose id IS in the docker_repo_allowlist setting comes back
//     docker_allowlisted:true, one that is NOT comes back false;
//   - owner-scoping: a STRANGER's repo is on the allowlist too, yet the owner's list
//     never carries it (ListEnabledReposForUser is owner-scoped), so the boolean can
//     never leak another user's allowlist membership — the non-owner assertion the PRD
//     asks for;
//   - non-vacuity: exactly the owner's two repos are returned before any field check,
//     so the test cannot pass green on an empty/zero result.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestListReposDockerAllowlistedLiveDB(t *testing.T) {
	ctx := context.Background()
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

	owner := store.User{ID: uuid.New(), Email: fmt.Sprintf("da-owner-%s@e2e", uuid.NewString()[:8])}
	stranger := store.User{ID: uuid.New(), Email: fmt.Sprintf("da-other-%s@e2e", uuid.NewString()[:8])}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, stranger.ID, stranger.Email)

	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	// Distinct base_url per connection: forge_connections is unique on
	// (user_id, forge_type, base_url), so two connections for the same owner must not
	// share a base URL.
	seedRepo := func(userID uuid.UUID, projectID int64, path, bot string) uuid.UUID {
		connID, repoID := uuid.New(), uuid.New()
		baseURL := fmt.Sprintf("https://forge%d.example", projectID)
		mustExecT(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', $3, $4, $5, $6)`, connID, userID, baseURL, bot, projectID, sealed)
		mustExecT(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.example/g/da', 'main', true)`, repoID, connID, projectID, path)
		return repoID
	}
	repoA := seedRepo(owner.ID, 1, "g/da-allowed", "botA")     // allowlisted
	repoB := seedRepo(owner.ID, 2, "g/da-plain", "botB")       // NOT allowlisted
	repoS := seedRepo(stranger.ID, 3, "g/da-stranger", "botS") // allowlisted, but stranger's

	// Allowlist holds repoA (owner) and repoS (stranger). repoB is deliberately out.
	// repoS being present is the sharp part: it must never surface on the owner's list.
	allowlistValue := repoA.String() + "," + repoS.String()
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyDockerRepoAllowlist, Value: allowlistValue},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}

	r := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	r = r.WithContext(mw.ContextWithUser(r.Context(), owner))
	rec := httptest.NewRecorder()
	h.ListRepos(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("ListRepos status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Repos []apitypes.RepoDTO `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	// Non-vacuity + owner-scoping: exactly the owner's two repos, never the stranger's.
	if len(resp.Repos) != 2 {
		t.Fatalf("got %d repos, want 2 (owner's repoA+repoB only); repos=%+v", len(resp.Repos), resp.Repos)
	}
	byID := map[string]apitypes.RepoDTO{}
	for _, d := range resp.Repos {
		if d.ID == repoS.String() {
			t.Fatalf("owner's list leaked stranger's repo %s", repoS)
		}
		byID[d.ID] = d
	}

	da, ok := byID[repoA.String()]
	if !ok {
		t.Fatalf("repoA %s missing from list", repoA)
	}
	if !da.DockerAllowlisted {
		t.Errorf("repoA docker_allowlisted = false, want true (it is on the allowlist)")
	}
	db, ok := byID[repoB.String()]
	if !ok {
		t.Fatalf("repoB %s missing from list", repoB)
	}
	if db.DockerAllowlisted {
		t.Errorf("repoB docker_allowlisted = true, want false (it is NOT on the allowlist)")
	}
}

// TestListReposDockerBlockedLiveDB pins PRD #361 M3's computed, caller-scoped
// docker_blocked field against a REAL Postgres, through the ListRepos handler. It
// executes the ListDockerBlockedReposForUser query against the live fn_worker_can_claim
// (migration 00113) — a green sqlc generate is not evidence the query runs (see
// .claude/rules/go.md) — and drives the four states the field distinguishes.
//
// The eligibility query is OWNER-scoped (it counts the caller's own online workers), and
// a NON-Docker online worker is eligible for EVERY repo. So the "eligible non-Docker
// worker exists" state cannot share a fleet with the "zero eligible workers → blocked"
// state under one owner: the non-Docker worker would make the blocked repo eligible too.
// The four states are therefore driven across two owner-scoped fleets:
//
//   - ownerDocker has ONE online Docker worker (docker_enabled=true). Under it:
//   - repoBlocked    — queued run, NOT allowlisted → zero eligible workers → docker_blocked TRUE.
//   - repoAllowlisted — queued run, allowlisted     → the Docker worker is eligible → FALSE.
//   - repoNoRun      — no queued run, NOT allowlisted → the queued-run predicate fails → FALSE.
//   - ownerEligible has ONE online NON-Docker worker (docker_enabled=false). Under it:
//   - repoEligible   — queued run, NOT allowlisted → a non-Docker worker is eligible → FALSE.
//
// Each fleet's ListRepos response is len-checked (non-vacuity) before the field checks,
// so the test cannot pass green on an empty result.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestListReposDockerBlockedLiveDB(t *testing.T) {
	ctx := context.Background()
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

	ownerDocker := store.User{ID: uuid.New(), Email: fmt.Sprintf("db-docker-%s@e2e", uuid.NewString()[:8])}
	ownerEligible := store.User{ID: uuid.New(), Email: fmt.Sprintf("db-elig-%s@e2e", uuid.NewString()[:8])}
	ownerNoWorker := store.User{ID: uuid.New(), Email: fmt.Sprintf("db-noworker-%s@e2e", uuid.NewString()[:8])}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, ownerDocker.ID, ownerDocker.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, ownerEligible.ID, ownerEligible.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, ownerNoWorker.ID, ownerNoWorker.Email)

	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	// project ids double as the unique base_url discriminator (forge_connections is
	// unique on (user_id, forge_type, base_url)); a global counter keeps them distinct
	// across owners.
	var projSeq int64
	seedRepo := func(userID uuid.UUID, path string) uuid.UUID {
		projSeq++
		connID, repoID := uuid.New(), uuid.New()
		baseURL := fmt.Sprintf("https://forgeb%d.example", projSeq)
		mustExecT(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', $3, $4, $5, $6)`, connID, userID, baseURL, fmt.Sprintf("bot%d", projSeq), projSeq, sealed)
		mustExecT(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.example/g/db', 'main', true)`, repoID, connID, projSeq, path)
		return repoID
	}
	var tokenSeq byte
	seedWorker := func(userID uuid.UUID, dockerEnabled bool) {
		tokenSeq++
		mustExecT(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, docker_enabled)
			 VALUES ($1, $2, $3, $4, 'online', now(), $5)`,
			uuid.New(), userID, fmt.Sprintf("w%d", tokenSeq), []byte{tokenSeq}, dockerEnabled)
	}
	var issueSeq int64
	seedQueuedRun := func(userID, repoID uuid.UUID) {
		issueSeq++
		mustExecT(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 'seed', 'seed', 'queued')`,
			uuid.New(), userID, repoID, issueSeq)
	}

	// ── ownerDocker: one online Docker worker; three repos ────────────────────────
	seedWorker(ownerDocker.ID, true)
	repoBlocked := seedRepo(ownerDocker.ID, "g/db-blocked")         // not allowlisted, queued run → blocked
	repoAllowlisted := seedRepo(ownerDocker.ID, "g/db-allowlisted") // allowlisted, queued run → not blocked
	repoNoRun := seedRepo(ownerDocker.ID, "g/db-norun")             // not allowlisted, NO queued run → not blocked
	seedQueuedRun(ownerDocker.ID, repoBlocked)
	seedQueuedRun(ownerDocker.ID, repoAllowlisted)
	// repoNoRun deliberately gets no queued run.

	// ── ownerEligible: one online NON-Docker worker; one repo ─────────────────────
	seedWorker(ownerEligible.ID, false)
	repoEligible := seedRepo(ownerEligible.ID, "g/db-eligible") // not allowlisted, queued run, but eligible worker → not blocked
	seedQueuedRun(ownerEligible.ID, repoEligible)

	// ── ownerNoWorker: NO online worker at all; one repo with a queued run ─────────
	// The block is "no worker online", NOT the allowlist, so this must read docker_blocked
	// FALSE — pinning the query's ≥1-online-worker EXISTS clause (deleting that clause
	// would flip this to true).
	repoNoWorker := seedRepo(ownerNoWorker.ID, "g/db-noworker") // not allowlisted, queued run, but zero workers → not blocked
	seedQueuedRun(ownerNoWorker.ID, repoNoWorker)

	// The allowlist holds only repoAllowlisted.
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyDockerRepoAllowlist, Value: repoAllowlisted.String()},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}

	listFor := func(u store.User) map[string]apitypes.RepoDTO {
		r := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		r = r.WithContext(mw.ContextWithUser(r.Context(), u))
		rec := httptest.NewRecorder()
		h.ListRepos(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("ListRepos(%s) status = %d, want 200; body=%s", u.Email, rec.Code, rec.Body.String())
		}
		var resp struct {
			Repos []apitypes.RepoDTO `json:"repos"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
		byID := map[string]apitypes.RepoDTO{}
		for _, d := range resp.Repos {
			byID[d.ID] = d
		}
		return byID
	}

	dockerRepos := listFor(ownerDocker)
	if len(dockerRepos) != 3 {
		t.Fatalf("ownerDocker got %d repos, want 3 (repoBlocked+repoAllowlisted+repoNoRun)", len(dockerRepos))
	}
	if d, ok := dockerRepos[repoBlocked.String()]; !ok {
		t.Fatalf("repoBlocked %s missing", repoBlocked)
	} else if !d.DockerBlocked {
		t.Errorf("repoBlocked docker_blocked = false, want true (queued run, not allowlisted, only Docker workers)")
	}
	if d, ok := dockerRepos[repoAllowlisted.String()]; !ok {
		t.Fatalf("repoAllowlisted %s missing", repoAllowlisted)
	} else if d.DockerBlocked {
		t.Errorf("repoAllowlisted docker_blocked = true, want false (Docker worker is eligible for an allowlisted repo)")
	}
	if d, ok := dockerRepos[repoNoRun.String()]; !ok {
		t.Fatalf("repoNoRun %s missing", repoNoRun)
	} else if d.DockerBlocked {
		t.Errorf("repoNoRun docker_blocked = true, want false (no queued run)")
	}

	eligibleRepos := listFor(ownerEligible)
	if len(eligibleRepos) != 1 {
		t.Fatalf("ownerEligible got %d repos, want 1 (repoEligible)", len(eligibleRepos))
	}
	if d, ok := eligibleRepos[repoEligible.String()]; !ok {
		t.Fatalf("repoEligible %s missing", repoEligible)
	} else if d.DockerBlocked {
		t.Errorf("repoEligible docker_blocked = true, want false (an eligible non-Docker worker exists)")
	}

	noWorkerRepos := listFor(ownerNoWorker)
	if len(noWorkerRepos) != 1 {
		t.Fatalf("ownerNoWorker got %d repos, want 1 (repoNoWorker)", len(noWorkerRepos))
	}
	if d, ok := noWorkerRepos[repoNoWorker.String()]; !ok {
		t.Fatalf("repoNoWorker %s missing", repoNoWorker)
	} else if d.DockerBlocked {
		t.Errorf("repoNoWorker docker_blocked = true, want false (no worker online — the block is no-worker, not the allowlist)")
	}
}
