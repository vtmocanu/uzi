package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PatchRepo (PRD #16 / #18 / #246) end-to-end through the handler against a real
// Postgres. Handler.q is a concrete *store.Queries, so there is no fake-store seam,
// and the properties worth pinning need a real DB anyway: the atomic both-trust-flag
// COALESCE update (a nil field must leave its column unchanged) and the owner-scoping
// join in the *ForUser query. Both are invisible to a fake.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

type patchRepoFixture struct {
	h        *Handler
	pool     *pgxpool.Pool
	owner    store.User
	stranger store.User
	admin    store.User
	repoID   uuid.UUID
	// strangerRepoID is a repo owned by `stranger`; the owner patching it must be
	// rejected by the *ForUser ownership join (404), never silently written.
	strangerRepoID uuid.UUID
}

func newPatchRepoFixture(ctx context.Context, t *testing.T) patchRepoFixture {
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

	f := patchRepoFixture{
		pool:     pool,
		owner:    store.User{ID: uuid.New(), Email: fmt.Sprintf("pr-owner-%s@e2e", uuid.NewString()[:8])},
		stranger: store.User{ID: uuid.New(), Email: fmt.Sprintf("pr-other-%s@e2e", uuid.NewString()[:8])},
		admin:    store.User{ID: uuid.New(), Email: fmt.Sprintf("pr-admin-%s@e2e", uuid.NewString()[:8]), IsAdmin: true},
	}
	f.h = &Handler{pool: pool, q: q, box: box, cfg: config.Config{}}

	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.stranger.ID, f.stranger.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.admin.ID, f.admin.Email)

	seedRepo := func(userID uuid.UUID, projectID int, path, bot string) uuid.UUID {
		connID, repoID := uuid.New(), uuid.New()
		mustExecT(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.example', $3, $4, $5)`, connID, userID, bot, projectID, sealed)
		mustExecT(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.example/g/pr', 'main', true)`, repoID, connID, projectID, path)
		return repoID
	}
	f.repoID = seedRepo(f.owner.ID, 1, "g/pr", "bot")
	f.strangerRepoID = seedRepo(f.stranger.ID, 2, "g/pr-stranger", "bot2")
	return f
}

func (f patchRepoFixture) patch(t *testing.T, user store.User, repoID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPatch, "/repos/x", bytes.NewBufferString(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.PatchRepo(w, r)
	return w
}

func decodeRepoDTO(t *testing.T, w *httptest.ResponseRecorder) apitypes.RepoDTO {
	t.Helper()
	var resp struct {
		Repo apitypes.RepoDTO `json:"repo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode repo DTO: %v (body %s)", err, w.Body.String())
	}
	return resp.Repo
}

// Setting repo_skills_enabled alone flips only that flag and leaves claudemd untouched
// (the COALESCE leaves the omitted column as-is), round-tripping in the returned DTO.
func TestPatchRepoSkillsAloneLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.owner, f.repoID, `{"repo_skills_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	dto := decodeRepoDTO(t, w)
	if !dto.RepoSkillsEnabled {
		t.Errorf("repo_skills_enabled = false, want true")
	}
	if dto.RepoClaudemdEnabled {
		t.Errorf("repo_claudemd_enabled = true, want false — an omitted trust flag must be left unchanged")
	}
}

// Setting repo_claudemd_enabled alone flips only that flag (PRD #246).
func TestPatchRepoClaudemdAloneLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.owner, f.repoID, `{"repo_claudemd_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	dto := decodeRepoDTO(t, w)
	if !dto.RepoClaudemdEnabled {
		t.Errorf("repo_claudemd_enabled = false, want true")
	}
	if dto.RepoSkillsEnabled {
		t.Errorf("repo_skills_enabled = true, want false — an omitted trust flag must be left unchanged")
	}
}

// The "Trusted repo" master: both flags in one request apply atomically.
func TestPatchRepoBothTrustFlagsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.owner, f.repoID, `{"repo_skills_enabled":true,"repo_claudemd_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	dto := decodeRepoDTO(t, w)
	if !dto.RepoSkillsEnabled || !dto.RepoClaudemdEnabled {
		t.Errorf("both flags should be true, got skills=%v claudemd=%v", dto.RepoSkillsEnabled, dto.RepoClaudemdEnabled)
	}

	// A follow-up patch of one flag to false must not disturb the other (COALESCE).
	w2 := f.patch(t, f.owner, f.repoID, `{"repo_skills_enabled":false}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("second patch status = %d, want 200 (body %s)", w2.Code, w2.Body.String())
	}
	dto2 := decodeRepoDTO(t, w2)
	if dto2.RepoSkillsEnabled {
		t.Errorf("repo_skills_enabled = true, want false after the follow-up patch")
	}
	if !dto2.RepoClaudemdEnabled {
		t.Errorf("repo_claudemd_enabled = false, want the earlier true left unchanged")
	}
}

// repo_devbox_opt_in remains its own exclusive path and still works.
func TestPatchRepoDevboxAloneLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.owner, f.repoID, `{"repo_devbox_opt_in":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if dto := decodeRepoDTO(t, w); !dto.RepoDevboxOptIn {
		t.Errorf("repo_devbox_opt_in = false, want true")
	}
}

// PRD #686 M1/M5: repo_fold_improve_uzi_backlog is its own exclusive group, mirroring
// devbox. The owner sets it on, it round-trips in the DTO, and a follow-up patch flips it
// back off — proving the setter writes the column both directions.
func TestPatchRepoFoldImproveUziBacklogAloneLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.owner, f.repoID, `{"repo_fold_improve_uzi_backlog":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if dto := decodeRepoDTO(t, w); !dto.RepoFoldImproveUziBacklog {
		t.Errorf("repo_fold_improve_uzi_backlog = false, want true")
	}
	// Flip back off — the default is false, so this proves the setter is not write-once.
	w2 := f.patch(t, f.owner, f.repoID, `{"repo_fold_improve_uzi_backlog":false}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("second patch status = %d, want 200 (body %s)", w2.Code, w2.Body.String())
	}
	if dto := decodeRepoDTO(t, w2); dto.RepoFoldImproveUziBacklog {
		t.Errorf("repo_fold_improve_uzi_backlog = true, want false after the follow-up patch")
	}
}

// The admin path is unscoped for the fold flag too: an admin sets it on a repo it does
// not own (the owner's repo, via SetRepoFoldImproveUziBacklog) and the flag lands.
func TestPatchRepoFoldImproveUziBacklogAdminUnscopedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.admin, f.repoID, `{"repo_fold_improve_uzi_backlog":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if dto := decodeRepoDTO(t, w); !dto.RepoFoldImproveUziBacklog {
		t.Errorf("admin patch should set the fold flag on a repo it does not own")
	}
}

// The fold flag is mutually exclusive with the other groups (devbox and the trust flags):
// combining it with either is a 400 and neither writes.
func TestPatchRepoFoldExclusivity400LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	if w := f.patch(t, f.owner, f.repoID, `{"repo_fold_improve_uzi_backlog":true,"repo_skills_enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("fold+skills status = %d, want 400", w.Code)
	}
	if w := f.patch(t, f.owner, f.repoID, `{"repo_fold_improve_uzi_backlog":true,"repo_devbox_opt_in":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("fold+devbox status = %d, want 400", w.Code)
	}
	// None of the rejected requests may have written the fold flag or its neighbours.
	w := f.patch(t, f.owner, f.repoID, `{"repo_fold_improve_uzi_backlog":false}`)
	dto := decodeRepoDTO(t, w)
	if dto.RepoFoldImproveUziBacklog || dto.RepoSkillsEnabled || dto.RepoDevboxOptIn {
		t.Errorf("a rejected combined patch must not write: got fold=%v skills=%v devbox=%v",
			dto.RepoFoldImproveUziBacklog, dto.RepoSkillsEnabled, dto.RepoDevboxOptIn)
	}
}

// Owner-scoping for the fold setter: the owner cannot flip the flag on the stranger's
// repo (the *ForUser join matches nothing → 404), and nothing lands on it.
func TestPatchRepoFoldForeignRepoIs404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	if w := f.patch(t, f.owner, f.strangerRepoID, `{"repo_fold_improve_uzi_backlog":true}`); w.Code != http.StatusNotFound {
		t.Errorf("foreign repo fold patch status = %d, want 404", w.Code)
	}
	// The stranger flipping their own repo works, proving the 404 was authorization.
	w := f.patch(t, f.stranger, f.strangerRepoID, `{"repo_fold_improve_uzi_backlog":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("stranger patching own repo status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if dto := decodeRepoDTO(t, w); !dto.RepoFoldImproveUziBacklog {
		t.Errorf("stranger repo_fold_improve_uzi_backlog = false, want true")
	}
}

// Combining devbox with a trust flag is a 400 (the two paths are disjoint), and an
// empty request is a 400 (at least one field required). Neither writes.
func TestPatchRepoConstraint400LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	if w := f.patch(t, f.owner, f.repoID, `{"repo_devbox_opt_in":true,"repo_skills_enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("devbox+skills status = %d, want 400", w.Code)
	}
	if w := f.patch(t, f.owner, f.repoID, `{"repo_devbox_opt_in":true,"repo_claudemd_enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("devbox+claudemd status = %d, want 400", w.Code)
	}
	if w := f.patch(t, f.owner, f.repoID, `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty request status = %d, want 400", w.Code)
	}

	// None of the rejected requests may have written anything.
	w := f.patch(t, f.owner, f.repoID, `{"repo_skills_enabled":false}`)
	dto := decodeRepoDTO(t, w)
	if dto.RepoSkillsEnabled || dto.RepoClaudemdEnabled || dto.RepoDevboxOptIn {
		t.Errorf("a rejected patch must not write: got skills=%v claudemd=%v devbox=%v",
			dto.RepoSkillsEnabled, dto.RepoClaudemdEnabled, dto.RepoDevboxOptIn)
	}
}

// Owner-scoping: the owner cannot address the stranger's repo (the *ForUser join
// matches nothing → 404), and an unknown id is a 404 too. Nothing lands on the
// stranger's repo.
func TestPatchRepoForeignRepoIs404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	if w := f.patch(t, f.owner, uuid.New(), `{"repo_claudemd_enabled":true}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown repo status = %d, want 404", w.Code)
	}
	if w := f.patch(t, f.owner, f.strangerRepoID, `{"repo_claudemd_enabled":true}`); w.Code != http.StatusNotFound {
		t.Errorf("foreign repo status = %d, want 404", w.Code)
	}
	// The stranger's own patch works, proving the 404 above was authorization, not a
	// broken query, and that nothing leaked from the owner's rejected attempt.
	w := f.patch(t, f.stranger, f.strangerRepoID, `{"repo_claudemd_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("stranger patching own repo status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if dto := decodeRepoDTO(t, w); !dto.RepoClaudemdEnabled {
		t.Errorf("stranger repo_claudemd_enabled = false, want true")
	}
}

// The admin path is unscoped: an admin patches a repo it does not own (the owner's
// repo) and the flag lands.
func TestPatchRepoAdminUnscopedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.admin, f.repoID, `{"repo_claudemd_enabled":true,"repo_skills_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	dto := decodeRepoDTO(t, w)
	if !dto.RepoClaudemdEnabled || !dto.RepoSkillsEnabled {
		t.Errorf("admin patch should set both flags on a repo it does not own, got skills=%v claudemd=%v",
			dto.RepoSkillsEnabled, dto.RepoClaudemdEnabled)
	}
}
