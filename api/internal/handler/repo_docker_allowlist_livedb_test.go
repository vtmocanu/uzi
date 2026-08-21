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
