package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The live-DB half of PRD #66 M4 (D1 layer 1): the repo-enable gate. SetRepoEnabled
// runs a live, fail-closed privcheck.GuardRepo before the enable flip, so the things
// that must be right are invisible to a fake store: the GetRepoForUser join that
// feeds the guard, the 422-vs-flip decision on a live forge read, and that the
// disable path is NEVER gated. The forge is a fake GitLab httptest server the real
// driver talks to, so the guard's verdict is derived from real branch-protection
// JSON exactly as production would read it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// guardBotUserID is the bot's forge user id the fake /user endpoint returns; the
// per-user push/merge grant fixtures name it so BotCanPush/BotCanMerge can fire.
const guardBotUserID = 4242

// protMode selects what the fake GitLab returns for a project's default-branch
// protection, so one server drives every enable-gate case from a per-project map.
type protMode int

const (
	// protClean: protected, Maintainer-only push and merge (GitLab "Fully
	// protected" default) → no BLOCK finding → the repo may be enabled.
	protClean protMode = iota
	// protCanPush: protected, but the write role (Developer, 30) may push → a
	// write_role_can_push BLOCK → refused.
	protCanPush
	// protUnprotected: 404 from protected_branches → default_branch_unprotected
	// BLOCK (the Protected-first inversion must not read this as safe).
	protUnprotected
	// protError: a 500 read error → protection_unreadable BLOCK (fail closed).
	protError
)

// fakeGuardForge is an httptest GitLab standing in for the real REST API for the
// three calls GuardRepo makes: GET /user (VerifyToken), GET
// /projects/:id/members/all/:uid (ProjectRole), and GET
// /projects/:id/protected_branches/:branch (DefaultBranchProtection). Protection
// behavior is keyed per forge project id so one server serves many repos.
type fakeGuardForge struct {
	server *httptest.Server
	mu     sync.Mutex
	prot   map[int64]protMode
}

func newFakeGuardForge(t *testing.T) *fakeGuardForge {
	t.Helper()
	fg := &fakeGuardForge{prot: map[int64]protMode{}}
	fg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/api/v4/user"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": guardBotUserID, "username": "uzi-bot"})
			return
		case strings.Contains(path, "/members/all/"):
			// The bot is exactly a Developer (30, the compliant write role): a clean
			// role, so the only findings under test come from branch protection.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": guardBotUserID, "access_level": 30})
			return
		case strings.Contains(path, "/protected_branches/"):
			pid := projectIDFromPath(path)
			fg.mu.Lock()
			mode := fg.prot[pid]
			fg.mu.Unlock()
			switch mode {
			case protUnprotected:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"404 Not found"}`))
			case protError:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"500 Internal"}`))
			case protCanPush:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 1, "name": "main",
					// Developer (30) may push → write_role_can_push BLOCK.
					"push_access_levels":  []map[string]any{{"access_level": 30}},
					"merge_access_levels": []map[string]any{{"access_level": 40}},
				})
			default: // protClean
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 1, "name": "main",
					"push_access_levels":  []map[string]any{{"access_level": 40}},
					"merge_access_levels": []map[string]any{{"access_level": 40}},
				})
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(fg.server.Close)
	return fg
}

// projectIDFromPath pulls the numeric project id out of
// /api/v4/projects/<id>/protected_branches/main.
func projectIDFromPath(path string) int64 {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			var id int64
			_, _ = fmt.Sscanf(parts[i+1], "%d", &id)
			return id
		}
	}
	return 0
}

type enableGuardFixture struct {
	h      *Handler
	pool   *pgxpool.Pool
	owner  store.User
	connID uuid.UUID
	forge  *fakeGuardForge
}

func newEnableGuardFixture(ctx context.Context, t *testing.T) enableGuardFixture {
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
	fg := newFakeGuardForge(t)

	// A real forgesvc builds the real GitLab driver against the fake server, and a
	// real privcheck.Service runs GuardRepo through it — no fakes on the guard path.
	svc := forgesvc.New(q, box, 5*time.Second, nil)
	pcheck := privcheck.NewService(q, svc)

	f := enableGuardFixture{
		pool:   pool,
		owner:  store.User{ID: uuid.New(), Email: fmt.Sprintf("eg-owner-%s@e2e", uuid.NewString()[:8])},
		connID: uuid.New(),
		forge:  fg,
	}
	f.h = &Handler{pool: pool, q: q, box: box, cfg: config.Config{}, svc: svc, pcheck: pcheck}

	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'uzi-bot', $4, $5)`,
		f.connID, f.owner.ID, fg.server.URL, guardBotUserID, sealed)
	return f
}

// seedRepo inserts an enabled/disabled repo with a distinct forge project id whose
// protection is served by mode, and returns its uuid.
func (f enableGuardFixture) seedRepo(ctx context.Context, t *testing.T, projectID int64, enabled bool, mode protMode) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	f.forge.mu.Lock()
	f.forge.prot[projectID] = mode
	f.forge.mu.Unlock()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.example/g/eg', 'main', $5)`,
		repoID, f.connID, projectID, fmt.Sprintf("g/eg-%d", projectID), enabled)
	return repoID
}

func (f enableGuardFixture) setEnabled(t *testing.T, repoID uuid.UUID, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]bool{"enabled": enabled})
	r := httptest.NewRequest(http.MethodPut, "/repos/x/enabled", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.owner), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.SetRepoEnabled(w, r)
	return w
}

func (f enableGuardFixture) repoEnabled(ctx context.Context, t *testing.T, repoID uuid.UUID) bool {
	t.Helper()
	var enabled bool
	if err := f.pool.QueryRow(ctx, `SELECT enabled FROM repos WHERE id = $1`, repoID).Scan(&enabled); err != nil {
		t.Fatalf("read enabled: %v", err)
	}
	return enabled
}

// The bot can push to protected main → enable is refused with 422 and a non-empty
// violations array; the repo stays disabled (D4: refuse the action, keep config).
func TestSetRepoEnableGuardBlocksCanPushLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 101, false, protCanPush)

	w := f.setEnabled(t, repoID, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error      string   `json:"error"`
		Violations []string `json:"violations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 422 body: %v (body %s)", err, w.Body.String())
	}
	if len(resp.Violations) == 0 {
		t.Errorf("violations = %v, want a non-empty block-finding list", resp.Violations)
	}
	if resp.Error == "" {
		t.Errorf("422 body must carry an error message")
	}
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("a refused enable must leave the repo disabled")
	}
}

// An unprotected default branch is refused too — the Protected-first inversion must
// not read false,false as safe.
func TestSetRepoEnableGuardBlocksUnprotectedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 102, false, protUnprotected)

	w := f.setEnabled(t, repoID, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("an unprotected-main repo must not be enabled")
	}
}

// Protected + clean → enable succeeds (200) and the repo is enabled.
func TestSetRepoEnableGuardAllowsCleanLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 103, false, protClean)

	w := f.setEnabled(t, repoID, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !f.repoEnabled(ctx, t, repoID) {
		t.Errorf("a clean repo must be enabled after a 200")
	}
}

// A branch-protection read error refuses the enable (fail closed, D3).
func TestSetRepoEnableGuardFailsClosedOnForgeErrorLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 104, false, protError)

	w := f.setEnabled(t, repoID, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (fail-closed) (body %s)", w.Code, w.Body.String())
	}
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("a repo whose protection read errors must not be enabled")
	}
}

// Disabling is NEVER gated (D4): a would-be-blocked repo that is already enabled can
// always be disabled, with no forge read refusing it.
func TestSetRepoDisableNeverGatedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 105, true, protCanPush)

	w := f.setEnabled(t, repoID, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (disable is never gated) (body %s)", w.Code, w.Body.String())
	}
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("the repo must be disabled after a 200 disable")
	}
}

// An unknown/non-owned repo id on the enable path is a 404 (same as the flip path),
// resolved before any forge read.
func TestSetRepoEnableGuardUnknownRepoIs404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)

	w := f.setEnabled(t, uuid.New(), true)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown repo (body %s)", w.Code, w.Body.String())
	}
}
