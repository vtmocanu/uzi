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
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// startRunFakeForge is the httptest GitLab for the M5 issue-lane test. It serves the
// three calls GuardRepo makes (keyed per project by protMode, exactly like M4's
// fakeGuardForge) AND the GET issue call StartRunForUser makes before CreateRun —
// which M4's helper does not, and whose response must carry an "id" (the client-go
// Issue.UnmarshalJSON panics on a missing id). The protMode enum and guardBotUserID
// are shared with repo_enable_guardrail_livedb_test.go (same package).
type startRunFakeForge struct {
	server *httptest.Server
	mu     sync.Mutex
	prot   map[int64]protMode
}

func newStartRunFakeForge(t *testing.T) *startRunFakeForge {
	t.Helper()
	fg := &startRunFakeForge{prot: map[int64]protMode{}}
	fg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/api/v4/user"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": guardBotUserID, "username": "uzi-bot"})
		case strings.Contains(path, "/members/all/"):
			// The bot is exactly a Developer (30, the compliant write role).
			_ = json.NewEncoder(w).Encode(map[string]any{"id": guardBotUserID, "access_level": 30})
		case strings.Contains(path, "/protected_branches/"):
			fg.mu.Lock()
			mode := fg.prot[projectIDFromPath(path)]
			fg.mu.Unlock()
			switch mode {
			case protUnprotected:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"404 Not found"}`))
			case protError:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"500 Internal"}`))
			case protCanPush:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 1, "name": "main",
					"push_access_levels":  []map[string]any{{"access_level": 30}},
					"merge_access_levels": []map[string]any{{"access_level": 40}},
				})
			default: // protClean
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 1, "name": "main",
					"push_access_levels":  []map[string]any{{"access_level": 40}},
					"merge_access_levels": []map[string]any{{"access_level": 40}},
				})
			}
		case strings.Contains(path, "/issues/"):
			// StartRunForUser's forge GetIssue snapshot. Must carry an "id" or the
			// client-go Issue.UnmarshalJSON panics.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 9001, "iid": 7, "title": "sr issue", "description": "do the thing",
				"state": "opened", "labels": []string{"PRD"}, "web_url": "https://x",
			})
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(fg.server.Close)
	return fg
}

// The live-DB half of PRD #66 M5 (D1 layer 2): the service-layer gate on the issue
// lane. CreateRun → StartRunForUser → workersvc.CreateRun runs the same live,
// fail-closed privcheck.GuardRepo the enable gate uses, but at run-start rather than
// enable. This proves the seam end-to-end: the guard is wired into the run-create
// service (SetRepoGuard), a bot-can-push forge yields a 422 with a non-empty
// violations array and NO run row, and a clean forge lets the run be created. The
// guard's verdict is derived from real branch-protection JSON (served by
// startRunFakeForge above) exactly as production reads it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

type startRunGuardFixture struct {
	h      *Handler
	pool   *pgxpool.Pool
	owner  store.User
	connID uuid.UUID
	forge  *startRunFakeForge
}

func newStartRunGuardFixture(ctx context.Context, t *testing.T) startRunGuardFixture {
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

	// Real forgesvc + real privcheck.Service, exactly as main.go wires them, and a
	// real workersvc with BOTH SetForges (for StartRunForUser's GetIssue) and
	// SetRepoGuard (the M5 seam under test) — no fakes on the guard path.
	svc := forgesvc.New(q, box, 5*time.Second, nil)
	pcheck := privcheck.NewService(q, svc)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetForges(svc)
	wsvc.SetRepoGuard(pcheck)

	f := startRunGuardFixture{
		pool:   pool,
		owner:  store.User{ID: uuid.New(), Email: fmt.Sprintf("sr-owner-%s@e2e", uuid.NewString()[:8])},
		connID: uuid.New(),
		forge:  fg,
	}
	f.h = &Handler{pool: pool, q: q, box: box, cfg: config.Config{}, svc: svc, pcheck: pcheck, wsvc: wsvc}

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

// seedEnabledRepoWithIssue inserts an ENABLED repo (the guard runs at run-start, not
// enable) whose default-branch protection is served by mode, plus a cached issue
// labelled uzi (so the single run-eligibility gate passes — PRD #764 M1) with a PRD link
// so every OTHER create-time gate passes and the guard is the only thing that can refuse.
func (f startRunGuardFixture) seedEnabledRepoWithIssue(ctx context.Context, t *testing.T, projectID int64, iid int64, mode protMode) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	f.forge.mu.Lock()
	f.forge.prot[projectID] = mode
	f.forge.mu.Unlock()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.example/g/sr', 'main', true)`,
		repoID, f.connID, projectID, fmt.Sprintf("g/sr-%d", projectID))
	mustExecT(ctx, t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'sr issue', 'opened', '["PRD","uzi"]'::jsonb, 'https://x', true, now(), now())`,
		repoID, iid)
	return repoID
}

func (f startRunGuardFixture) createRun(t *testing.T, repoID uuid.UUID, iid int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"issue_iid": iid})
	r := httptest.NewRequest(http.MethodPost, "/repos/x/runs", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.owner), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.CreateRun(w, r)
	return w
}

func (f startRunGuardFixture) runCount(ctx context.Context, t *testing.T, repoID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE repo_id = $1`, repoID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

// The bot can push to protected main → StartRun is refused with 422 and a non-empty
// violations array, and NO run row is created (D1 layer 2 refuses before the insert).
func TestStartRunGuardBlocksCanPushLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 201, 7, protCanPush)

	w := f.createRun(t, repoID, 7)
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
	if n := f.runCount(ctx, t, repoID); n != 0 {
		t.Errorf("a refused run must not be inserted, found %d run rows", n)
	}
}

// An unprotected default branch is refused too — the Protected-first inversion must
// not read false,false as safe at the service gate either.
func TestStartRunGuardBlocksUnprotectedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 202, 7, protUnprotected)

	w := f.createRun(t, repoID, 7)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
	if n := f.runCount(ctx, t, repoID); n != 0 {
		t.Errorf("an unprotected-main repo must not get a run, found %d run rows", n)
	}
}

// A branch-protection read error refuses the run (fail closed, D3).
func TestStartRunGuardFailsClosedOnForgeErrorLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 203, 7, protError)

	w := f.createRun(t, repoID, 7)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (fail-closed) (body %s)", w.Code, w.Body.String())
	}
	if n := f.runCount(ctx, t, repoID); n != 0 {
		t.Errorf("a repo whose protection read errors must not get a run, found %d run rows", n)
	}
}

// Protected + clean → the run is created (201) and a run row exists (the guard clears
// the repo and the create path proceeds past the gate).
func TestStartRunGuardAllowsCleanLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 204, 7, protClean)

	w := f.createRun(t, repoID, 7)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	if n := f.runCount(ctx, t, repoID); n != 1 {
		t.Errorf("a clean repo must get exactly one run, found %d", n)
	}
}
