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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestListReposGithubProjectSyncLiveDB pins PRD #576 M2's caller-scoped sync-health
// field against a REAL Postgres, through the ListRepos handler (GET /api/repos). It
// executes the new ListGithubProjectLinksByRepoIDs batch query and the syncHealthForLink
// mapper end to end — a green sqlc generate is not evidence the query runs
// (.claude/rules/go.md), which is why this is a LiveDB test and not an offline one
// (Handler.q is a concrete *store.Queries).
//
// Three repos under one owner drive the three states:
//   - repoHealthy  — linked, last synced with no error → github_project_sync{linked,healthy}.
//   - repoErrored  — linked, last_error stamped        → github_project_sync{linked,!healthy,last_error}.
//   - repoUnlinked — no link row                        → github_project_sync nil.
//
// The response is len-checked (non-vacuity) before any field check, so the test cannot
// pass green on an empty result.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestListReposGithubProjectSyncLiveDB(t *testing.T) {
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

	owner := store.User{ID: uuid.New(), Email: fmt.Sprintf("gps-owner-%s@e2e", uuid.NewString()[:8])}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, owner.Email)

	sealed, err := box.Seal([]byte("ghp-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	// One github connection; forge_connections is unique on (user_id, forge_type, base_url).
	connID := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'github', 'https://github.com', 'uzi-bot', 1, $3)`, connID, owner.ID, sealed)

	var projSeq int64
	seedRepo := func(path string) uuid.UUID {
		projSeq++
		repoID := uuid.New()
		mustExecT(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://github.com/g/gps', 'main', true)`, repoID, connID, projSeq, path)
		return repoID
	}
	seedLink := func(repoID uuid.UUID, opts string) {
		if _, err := q.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
			RepoID:        repoID,
			ProjectNodeID: "PVT_node",
			ProjectNumber: projSeq,
			StatusFieldID: "PVTSSF_field",
			StatusOptions: []byte(opts),
			OwnedByUzi:    true,
		}); err != nil {
			t.Fatalf("UpsertGithubProjectLink(%s): %v", repoID, err)
		}
	}

	repoHealthy := seedRepo("g/gps-healthy")
	repoErrored := seedRepo("g/gps-errored")
	repoUnlinked := seedRepo("g/gps-unlinked") // deliberately no link row

	seedLink(repoHealthy, `{"To Do":"opt_todo"}`)
	// Healthy: record a clean sync (stamps last_synced_at, clears last_error).
	if err := q.ClearGithubProjectLinkError(ctx, repoHealthy); err != nil {
		t.Fatalf("ClearGithubProjectLinkError(healthy): %v", err)
	}

	seedLink(repoErrored, `{"To Do":"opt_todo"}`)
	if err := q.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
		RepoID:    repoErrored,
		LastError: pgtype.Text{String: "provision failed: owner mismatch", Valid: true},
	}); err != nil {
		t.Fatalf("SetGithubProjectLinkError(errored): %v", err)
	}

	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyDockerRepoAllowlist, Value: ""},
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

	// Non-vacuity: exactly the owner's three enabled repos before any field check.
	if len(resp.Repos) != 3 {
		t.Fatalf("got %d repos, want 3 (healthy+errored+unlinked); repos=%+v", len(resp.Repos), resp.Repos)
	}
	byID := map[string]apitypes.RepoDTO{}
	for _, d := range resp.Repos {
		byID[d.ID] = d
	}

	healthy, ok := byID[repoHealthy.String()]
	if !ok {
		t.Fatalf("repoHealthy %s missing", repoHealthy)
	}
	if healthy.GithubProjectSync == nil {
		t.Fatalf("repoHealthy github_project_sync = nil, want a linked/healthy summary")
	}
	if !healthy.GithubProjectSync.Linked || !healthy.GithubProjectSync.Healthy {
		t.Errorf("repoHealthy sync = %+v, want linked=true healthy=true", *healthy.GithubProjectSync)
	}
	if healthy.GithubProjectSync.LastError != nil {
		t.Errorf("repoHealthy last_error = %q, want nil", *healthy.GithubProjectSync.LastError)
	}
	if healthy.GithubProjectSync.LastSyncedAt == nil {
		t.Errorf("repoHealthy last_synced_at = nil, want a timestamp (a clean sync was recorded)")
	}

	errored, ok := byID[repoErrored.String()]
	if !ok {
		t.Fatalf("repoErrored %s missing", repoErrored)
	}
	if errored.GithubProjectSync == nil {
		t.Fatalf("repoErrored github_project_sync = nil, want a linked/unhealthy summary")
	}
	if !errored.GithubProjectSync.Linked || errored.GithubProjectSync.Healthy {
		t.Errorf("repoErrored sync = %+v, want linked=true healthy=false", *errored.GithubProjectSync)
	}
	if errored.GithubProjectSync.LastError == nil || *errored.GithubProjectSync.LastError != "provision failed: owner mismatch" {
		t.Errorf("repoErrored last_error = %v, want the stamped message", errored.GithubProjectSync.LastError)
	}

	unlinked, ok := byID[repoUnlinked.String()]
	if !ok {
		t.Fatalf("repoUnlinked %s missing", repoUnlinked)
	}
	if unlinked.GithubProjectSync != nil {
		t.Errorf("repoUnlinked github_project_sync = %+v, want nil (no link row)", *unlinked.GithubProjectSync)
	}
}
