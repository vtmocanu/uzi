package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestGetFindingIssueDraftLiveDB exercises GET /api/findings/{id}/issue-draft against a REAL
// Postgres (PRD #333 M4): the draft resolves the title/description/location from the STORED
// finding row (D4, never a request body) and routes each through the field-level sanitisers,
// carries the deterministic provenance footer, and is OWNER-SCOPED — a non-owner's request for
// the same id is a 404, no existence oracle.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestGetFindingIssueDraftLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store live-DB harness for coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, cfg: config.Config{}}

	owner, stranger, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner, fmt.Sprintf("m4draft-o-%s@e2e", owner))
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, stranger, fmt.Sprintf("m4draft-s-%s@e2e", stranger))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/draft', 'https://forge.e2e/g/draft', 'main', true)`, repoID, connID)
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
	      VALUES ($1, $2, $3, 55, 'Do X', 'desc', 'completed', 'issue')`, runID, owner, repoID)

	// A stored finding carrying a hostile payload: a leading quick-action "/", a fenced
	// "/label" the description sanitiser must keep inside a breakout-proof fence, and a
	// backtick run in the location the inline-code span must survive.
	f, err := q.InsertFinding(ctx, store.InsertFindingParams{
		RunID: runID, UserID: owner, RepoID: repoID,
		Location:      "internal/sweep.go#sweep``loop",
		Title:         "/label ~backdoor leaked ticker",
		DescriptionMd: "the sweeper leaks\n```\n/label ~pwn\n```\ntail",
		Labels:        []byte(`["bug","perf"]`), Confidence: "high",
	})
	if err != nil {
		t.Fatalf("InsertFinding: %v", err)
	}

	draftReq := func(u uuid.UUID, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/findings/"+id+"/issue-draft", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id)
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), store.User{ID: u}), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.GetFindingIssueDraft(rec, req)
		return rec
	}

	// ── owner: 200, sanitised + provenance ──
	rec := draftReq(owner, f.ID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("owner draft = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto apitypes.IncidentalFindingIssueDraftDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	// Title: single line, leading "/" defanged.
	if strings.HasPrefix(dto.Title, "/") || strings.ContainsAny(dto.Title, "\n\r") {
		t.Errorf("title not sanitised: %q", dto.Title)
	}
	// Location: a breakout-proof inline-code span (delimiter longer than the embedded "``").
	if !strings.HasPrefix(dto.Location, "```") {
		t.Errorf("location not wrapped in a breakout-proof span: %q", dto.Location)
	}
	// Description: the whole body ran through SanitizeFiledBody — no live unfenced "/"-line.
	if issuedraft.StripUnfencedSlashLines(dto.Description) != dto.Description {
		t.Errorf("description carries a live unfenced quick-action line:\n%s", dto.Description)
	}
	// Provenance names the reporting run + the work it was doing (issue #55).
	if !strings.Contains(dto.Provenance, "issue #55") {
		t.Errorf("provenance must name the work, got %q", dto.Provenance)
	}
	// Labels seed from the stored suggestions.
	if len(dto.Labels) != 2 {
		t.Errorf("labels = %v, want the two stored suggestions", dto.Labels)
	}

	// ── non-owner: 404 (no existence oracle) ──
	if rec := draftReq(stranger, f.ID.String()); rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner draft = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// ── unknown id: 404 ──
	if rec := draftReq(owner, uuid.New().String()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id draft = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
