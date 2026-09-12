package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The live-DB half of PRD #1184 M3 (admin "All users" filing). The draft render and the
// forge-first file path are unreachable from a fake store (Handler.pool/.q/.svc are concrete),
// and the two properties that MUST be right are invisible to a fake: that both the draft and the
// file resolve the SINGLE newest OPEN occurrence of a coordinate ACROSS USERS through
// NewestOpenOccurrenceForCoord, and that the file resolves AGAIN at file time so a fresher review
// moves the link. The forge is the same httptest stub the owner filer's tests use, so the
// description that reaches CreateIssue is exactly what the forge would receive.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// adminOcc is one seeded cross-user occurrence: the run owner, its judged review and the single
// open recommendation on the (category, target) coordinate, with a controllable judged_at so the
// newest-resolution and resolve-at-file-time orderings are deterministic on the shared DB.
type adminOcc struct {
	ownerID  uuid.UUID
	runID    uuid.UUID
	reviewID uuid.UUID
	recID    uuid.UUID
}

// seedAdminOccurrence inserts a fresh owner + connection + repo + completed run, then a judged
// review (judged_at = both created_at and updated_at) with ONE open recommendation on
// (category, target). No disposition and no filed row, so the coordinate is OPEN. Raw INSERTs
// (not UpsertRunReviewWithRecommendations) so judged_at is set exactly, which is what makes the
// ORDER BY-driven "newest" deterministic across two owners on one shared database.
func seedAdminOccurrence(ctx context.Context, t *testing.T, pool *pgxpool.Pool, box *secretbox.Box, forgeURL, category, target string, judgedAt time.Time) adminOcc {
	t.Helper()
	o := adminOcc{ownerID: uuid.New(), runID: uuid.New(), reviewID: uuid.New()}
	conn, repo := uuid.New(), uuid.New()
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		o.ownerID, fmt.Sprintf("occ-%s@e2e", uuid.NewString()[:8]))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-occ', 1, $4)`, conn, o.ownerID, forgeURL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/occ', 'https://forge.example/g/occ', 'main', true)`, repo, conn)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'd', 'completed', 'issue')`, o.runID, o.ownerID, repo)
	mustExecT(ctx, t, pool,
		`INSERT INTO run_reviews (id, target_run_id, user_id, verdict, summary_md, judge_model, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'issues', 's', 'haiku', 'complete', $4, $4)`, o.reviewID, o.runID, o.ownerID, judgedAt)
	if err := pool.QueryRow(ctx,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, $2, $3, 'the reviewer skipped a check', 'medium') RETURNING id`,
		o.reviewID, category, target).Scan(&o.recID); err != nil {
		t.Fatalf("seed recommendation: %v", err)
	}
	return o
}

// adminDraftReq builds a GET AdminGetJudgeIssueDraft request authenticated as user with
// ?category=&target= query params. Calling the handler directly exercises it without the router.
func adminDraftReq(user store.User, category, target string) *http.Request {
	q := url.Values{}
	q.Set("category", category)
	q.Set("target", target)
	r := httptest.NewRequest(http.MethodGet, "/x?"+q.Encode(), nil)
	return r.WithContext(mw.ContextWithUser(r.Context(), user))
}

// adminFileReq builds a POST AdminFileJudgeIssue request authenticated as user with a JSON body.
func adminFileReq(user store.User, category, target string, repoID uuid.UUID, title, description string) *http.Request {
	body, _ := json.Marshal(adminFileIssueRequest{
		Category: category, Target: target, RepoID: repoID.String(), Title: title, Description: description,
	})
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body)))
	return r.WithContext(mw.ContextWithUser(r.Context(), user))
}

// ── Draft: the one surface that shows attribution — Provenance is populated (Decision 8) ─────
func TestAdminGetJudgeIssueDraftLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL) // for the admin caller

	category := "improve_uzi"
	target := "m3-draft-" + uuid.NewString()
	seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now())

	rr := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rr, adminDraftReq(f.admin, category, target))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin draft: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp draftResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	// The draft renders a title and a body — a coordinate resolved and rendered.
	if strings.TrimSpace(resp.Draft.Title) == "" {
		t.Errorf("admin draft has an empty title; the coordinate did not render")
	}
	// The one deliberate attribution surface (Decision 8): filing publishes a user's worker text,
	// so the draft MUST name whose text it is. The owner is a DIFFERENT user than the admin, so
	// producerHandle resolves to that owner and the provenance line is non-empty.
	if strings.TrimSpace(resp.Draft.Provenance) == "" {
		t.Errorf("admin draft has an empty provenance line; Decision 8 requires the draft to name whose text it is")
	}
	if len(resp.Draft.Labels) != 1 || resp.Draft.Labels[0] != "uzi" {
		t.Errorf("draft labels = %v, want [uzi] (assembled server-side)", resp.Draft.Labels)
	}

	// Validation: an unknown category is a 400 (closed enum), an empty target is a 400.
	rrBadCat := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rrBadCat, adminDraftReq(f.admin, "not_a_category", target))
	if rrBadCat.Code != http.StatusBadRequest {
		t.Errorf("bad category: status = %d, want 400", rrBadCat.Code)
	}
	rrNoTarget := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rrNoTarget, adminDraftReq(f.admin, category, ""))
	if rrNoTarget.Code != http.StatusBadRequest {
		t.Errorf("empty target: status = %d, want 400", rrNoTarget.Code)
	}

	// A coordinate with no open occurrence is a 404.
	rr404 := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rr404, adminDraftReq(f.admin, category, "m3-unknown-"+uuid.NewString()))
	if rr404.Code != http.StatusNotFound {
		t.Errorf("unknown coordinate: status = %d, want 404; body=%s", rr404.Code, rr404.Body.String())
	}
}

// TestAdminJudgeIssueDraftExcludesDisposedLiveDB pins the d.status IS NULL half of the open
// predicate in NewestOpenOccurrenceForCoord. The f.filed_at IS NULL half is pinned by the newest-
// occurrence test below (a filed occurrence is skipped); this pins the disposition half: once a
// coordinate's only occurrence carries a disposition (a human/admin done or dismissed), it is no
// longer OPEN, so the admin draft (and file, which resolves the same way) must 404 rather than
// re-draft a coordinate the owner already resolved. Without this, a regression dropping
// `AND d.status IS NULL` would let the admin re-file a settled recommendation, uncaught.
func TestAdminJudgeIssueDraftExcludesDisposedLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL) // the admin caller
	category := "improve_uzi"
	target := "m3-disposed-" + uuid.NewString()
	occ := seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now())

	// Open: the draft resolves and renders (200).
	rrOpen := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rrOpen, adminDraftReq(f.admin, category, target))
	if rrOpen.Code != http.StatusOK {
		t.Fatalf("pre-disposition draft = %d, want 200; body=%s", rrOpen.Code, rrOpen.Body.String())
	}

	// Dispose the only occurrence (a human 'done'); d.status is now NOT NULL for the coordinate.
	mustExecT(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, rationale_hash)
		 VALUES ($1, $2, $3, 'done', 'x')`, occ.reviewID, category, target)

	// No longer open: NewestOpenOccurrenceForCoord returns no row, so the draft must 404.
	rrDisposed := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rrDisposed, adminDraftReq(f.admin, category, target))
	if rrDisposed.Code != http.StatusNotFound {
		t.Fatalf("post-disposition draft = %d, want 404 (a disposed occurrence is not open); body=%s",
			rrDisposed.Code, rrDisposed.Body.String())
	}
}

// ── File: the link lands on the NEWEST open occurrence; the older one stays open ─────────────
func TestAdminFileJudgeIssueNewestOccurrenceLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL) // for f.admin + f.adminRepo

	category := "improve_uzi"
	target := "m3-newest-" + uuid.NewString()
	// Two owners hit the SAME coordinate; older was judged 2h ago, newer just now.
	older := seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now().Add(-2*time.Hour))
	newer := seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now())

	rr := httptest.NewRecorder()
	h.AdminFileJudgeIssue(rr, adminFileReq(f.admin, category, target, f.adminRepo, "Improve uzi", "make it better"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin file: status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	// The filed link landed on the NEWER occurrence only — that owner's row moves to `filed`.
	if n := filedRowCount(ctx, t, pool, newer.reviewID); n != 1 {
		t.Errorf("filed rows on the newest occurrence = %d, want 1", n)
	}
	if n := filedRowCount(ctx, t, pool, older.reviewID); n != 0 {
		t.Errorf("filed rows on the OLDER occurrence = %d, want 0 (the link lands on the newest only)", n)
	}
	// The just-filed occurrence is settled (filed_at set), so the OPEN predicate now excludes it:
	// the next resolve returns the OLDER, still-open occurrence — proving both the newest ordering
	// and the (f.filed_at IS NULL) rung of the open predicate.
	next, err := h.wsvc.AdminNewestOpenOccurrence(ctx, category, target)
	if err != nil {
		t.Fatalf("resolve after filing the newest: %v", err)
	}
	if next.ReviewID != older.reviewID {
		t.Errorf("after filing the newest, the next open occurrence is %s, want the older %s", next.ReviewID, older.reviewID)
	}
	// The forge saw exactly one create, labelled server-side [uzi].
	if fs.count() != 1 {
		t.Fatalf("forge CreateIssue calls = %d, want 1", fs.count())
	}
	if got := fs.creates[0].Labels; len(got) != 1 || got[0] != "uzi" {
		t.Errorf("labels sent to forge = %v, want [uzi]", got)
	}
}

// ── Resolve-at-file-time: draft occurrence A, then a NEWER review B lands, file → B, not A ────
func TestAdminFileJudgeIssueResolvesAtFileTimeLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL) // for f.admin + f.adminRepo

	category := "improve_uzi"
	target := "m3-race-" + uuid.NewString()

	// A is the only occurrence when the admin fetches the draft.
	a := seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now().Add(-time.Hour))
	rrDraft := httptest.NewRecorder()
	h.AdminGetJudgeIssueDraft(rrDraft, adminDraftReq(f.admin, category, target))
	if rrDraft.Code != http.StatusOK {
		t.Fatalf("draft of A: status = %d, want 200; body=%s", rrDraft.Code, rrDraft.Body.String())
	}

	// A FRESHER review B on the SAME coordinate lands between the draft and the file.
	b := seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now())

	rrFile := httptest.NewRecorder()
	h.AdminFileJudgeIssue(rrFile, adminFileReq(f.admin, category, target, f.adminRepo, "Improve uzi", "d"))
	if rrFile.Code != http.StatusCreated {
		t.Fatalf("file: status = %d, want 201; body=%s", rrFile.Code, rrFile.Body.String())
	}
	// The file resolved AGAIN at file time (decision log 2026-09-07): the link lands on the newer
	// B, NOT the A the draft was rendered from. A regression that reused the draft's occurrence, or
	// flipped the ORDER BY to oldest-first, files A here and reddens.
	if n := filedRowCount(ctx, t, pool, b.reviewID); n != 1 {
		t.Errorf("filed rows on the newer B = %d, want 1 (the file resolves at file time)", n)
	}
	if n := filedRowCount(ctx, t, pool, a.reviewID); n != 0 {
		t.Errorf("filed rows on the drafted-from A = %d, want 0 (a fresher review moves the link)", n)
	}
}

// ── No open occurrence, and a repo the admin does not own → 404 without a forge call ─────────
func TestAdminFileJudgeIssueNotFoundLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	category := "improve_uzi"

	// (a) A coordinate with no occurrence at all → 404, no claim, no forge.
	rrUnknown := httptest.NewRecorder()
	h.AdminFileJudgeIssue(rrUnknown, adminFileReq(f.admin, category, "m3-none-"+uuid.NewString(), f.adminRepo, "t", "d"))
	if rrUnknown.Code != http.StatusNotFound {
		t.Fatalf("unknown coordinate: status = %d, want 404; body=%s", rrUnknown.Code, rrUnknown.Body.String())
	}
	if fs.count() != 0 {
		t.Errorf("a 404 reached the forge %d times, want 0", fs.count())
	}

	// (b) A real open occurrence, but the admin files into a repo they do NOT own (the owner's
	// repo) → caller-owns-repo 404, before any claim or forge call.
	target := "m3-repo-" + uuid.NewString()
	occ := seedAdminOccurrence(ctx, t, pool, box, fs.server.URL, category, target, time.Now())
	rrRepo := httptest.NewRecorder()
	h.AdminFileJudgeIssue(rrRepo, adminFileReq(f.admin, category, target, f.ownerRepo, "t", "d"))
	if rrRepo.Code != http.StatusNotFound {
		t.Fatalf("non-owned repo: status = %d, want 404; body=%s", rrRepo.Code, rrRepo.Body.String())
	}
	if fs.count() != 0 || filedRowCount(ctx, t, pool, occ.reviewID) != 0 {
		t.Errorf("a non-owned-repo write must touch neither forge (%d) nor claim table (%d)", fs.count(), filedRowCount(ctx, t, pool, occ.reviewID))
	}
}

// TestAdminJudgeIssueRoutesCeilingLiveDB pins PRD #1184 M3's read/write split for the admin issue
// DRAFT and FILE routes against the REAL h.Routes() router — the only place the middleware chain
// that gates them is wired. The draft is a CLI-reachable READ (RequireUser + RequireAdminRO): a
// uza_ admin_ro token reaches it, a masked uzc_ / non-admin session is 403. The file is a
// cookie-only WRITE (RequireAuth + RequireAdmin): a uza_ Bearer 401s before the handler, a
// non-admin session is 403, an admin session passes the gate.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestAdminJudgeIssueRoutesCeilingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	box := newHandlerTestBox(t)

	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)

	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // keeps IsAdmin
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // masked to IsAdmin=false
	memberUzc := cliMintToken(t, pool, member, clitoken.ScopeUser)
	memberJWT := cliMintJWT(t, pool, member)
	adminJWT := cliMintJWT(t, pool, admin)

	// A fresh, unique OPEN coordinate so the uza_ draft read reaches a deterministic 200.
	category := "improve_uzi"
	target := "m3route-" + uuid.NewString()
	_ = seedAdminOccurrence(context.Background(), t, pool, box, "https://forge.e2e", category, target, time.Now())

	// ---- DRAFT (CLI-reachable read) -------------------------------------------------------------
	draftPath := "/api/admin/judge/recommendations/issue-draft?" + url.Values{"category": {category}, "target": {target}}.Encode()
	if rec := bearerReq(router, http.MethodGet, draftPath, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential GET draft = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodGet, draftPath, adminUzc); rec.Code != http.StatusForbidden {
		t.Errorf("admin uzc_ GET draft = %d, want 403 (masked to IsAdmin=false)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodGet, draftPath, memberUzc); rec.Code != http.StatusForbidden {
		t.Errorf("member uzc_ GET draft = %d, want 403\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodGet, draftPath, memberJWT, ""); rec.Code != http.StatusForbidden {
		t.Errorf("member session GET draft = %d, want 403\nbody: %s", rec.Code, rec.Body.String())
	}
	// The admin_ro token is the one credential that reaches the handler, and the seeded coordinate
	// resolves → 200.
	if rec := bearerReq(router, http.MethodGet, draftPath, adminUza); rec.Code != http.StatusOK {
		t.Errorf("admin uza_ GET draft = %d, want 200 (admin_ro reads the draft)\nbody: %s", rec.Code, rec.Body.String())
	}

	// ---- FILE (cookie-only write) ---------------------------------------------------------------
	const filePath = "/api/admin/judge/recommendations/issue"
	// A nonexistent coordinate + a random repo, so the one credential that PASSES the auth gate (the
	// admin session) reaches a clean pre-forge 404 (no open occurrence) rather than the forge — the
	// ceiling test wires no forge stub, and the filing behaviour is proven in the direct-handler
	// tests above. The 401/403 credentials fail before the handler, so the body is irrelevant to them.
	fileBody := fmt.Sprintf(`{"category":%q,"target":%q,"repo_id":%q,"title":"t","description":"d"}`,
		category, "m3route-none-"+uuid.NewString(), uuid.New().String())
	if rec := bearerReqBody(router, http.MethodPost, filePath, "", fileBody); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential POST file = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	// A uza_ admin_ro token is a BEARER, and the write group is cookie-only: RequireAuth 401s
	// before the handler — the structural guarantee a read-only token cannot reach a write.
	if rec := bearerReqBody(router, http.MethodPost, filePath, adminUza, fileBody); rec.Code != http.StatusUnauthorized {
		t.Errorf("admin uza_ POST file = %d, want 401 (cookie-only RequireAuth; a Bearer never reaches the write)\nbody: %s", rec.Code, rec.Body.String())
	}
	// A non-admin browser session: RequireAdmin 403s.
	if rec := cookieReq(t, router, http.MethodPost, filePath, memberJWT, fileBody); rec.Code != http.StatusForbidden {
		t.Errorf("member session POST file = %d, want 403\nbody: %s", rec.Code, rec.Body.String())
	}
	// The admin session passes the whole auth gate — the route IS mounted behind the write group
	// (an absent route would 404 here). It then reaches a clean 404 (the nonexistent coordinate),
	// proving the gate passed without a forge call.
	if rec := cookieReq(t, router, http.MethodPost, filePath, adminJWT, fileBody); rec.Code != http.StatusNotFound {
		t.Errorf("admin session POST file = %d, want 404 (auth passes; the nonexistent coordinate is a clean pre-forge 404)\nbody: %s", rec.Code, rec.Body.String())
	}
}
