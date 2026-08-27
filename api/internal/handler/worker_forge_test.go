package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ─────────────────────────────────────────────────────────────────────────────
// Seam. The worker-forge handlers reach a forge driver through TWO concrete
// collaborators — h.wsvc.ForgeConnForRun (a *workersvc.Service over a Store) and
// h.svc.ForgeForConnection (a *forgesvc.Service that decrypts the sealed token and
// calls forge.New). Neither is an interface at the handler, so a fake forge cannot
// be substituted directly. The seam that IS available is the one forge/gitlab_test.go
// uses: build a REAL gitlab driver against an httptest server standing in for the
// GitLab REST API, and make the fake Store hand back a ForgeConn whose BaseUrl is
// that server and whose TokenCiphertext is sealed with the SAME box the forgesvc
// holds. The mock then controls exactly what each driver method returns.
// ─────────────────────────────────────────────────────────────────────────────

const forgeTestProjectID = 4242

// forgeHandlerStore is a minimal workersvc.Store for the worker-forge HTTP tests:
// it overrides only the two reads ForgeConnForRun makes (owned-run claim check and
// the worker-scoped connection lookup).
type forgeHandlerStore struct {
	workersvc.Store
	ownedRun store.Run
	ownedErr error
	connRow  store.GetRunForgeConnForWorkerRow
	connErr  error
}

func (f *forgeHandlerStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return f.ownedRun, f.ownedErr
}

func (f *forgeHandlerStore) GetRunForgeConnForWorker(context.Context, store.GetRunForgeConnForWorkerParams) (store.GetRunForgeConnForWorkerRow, error) {
	return f.connRow, f.connErr
}

func newForgeBox(t *testing.T) *secretbox.Box {
	t.Helper()
	// A fixed 32-byte test key (varied bytes; not an all-identical placeholder).
	key := []byte("0123456789abcdef0123456789abcdef")
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

// newForgeHandler builds a Handler wired with both forge collaborators over the
// same box, so a sealed token in the fake Store's ForgeConn decrypts cleanly in
// forgesvc.ForgeForConnection.
func newForgeHandler(t *testing.T, st workersvc.Store, box *secretbox.Box) *Handler {
	t.Helper()
	return &Handler{
		wsvc: workersvc.New(st, box, workersvc.Params{}),
		svc:  forgesvc.New(nil, box, 5*time.Second, nil),
	}
}

// forgeMockHandler wires a Handler to a mock GitLab server serving routes, with an
// owned repo-bearing run whose connection points at that server.
func forgeMockHandler(t *testing.T, routes map[string]http.HandlerFunc) (*Handler, *httptest.Server) {
	t.Helper()
	box := newForgeBox(t)
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sealed, err := box.Seal([]byte("glpat-fake-forge-token-abcdef123456")) //gitleaks:allow fake fixture PAT (literal "fake"), sealed into a throwaway test secretbox; never a real credential
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	st := &forgeHandlerStore{
		ownedRun: store.Run{ID: uuid.New(), UserID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		connRow: store.GetRunForgeConnForWorkerRow{
			ForgeProjectID:  forgeTestProjectID,
			ForgeType:       "gitlab",
			BaseUrl:         srv.URL,
			TokenCiphertext: sealed,
		},
	}
	return newForgeHandler(t, st, box), srv
}

// forgeReq builds a worker-authenticated request carrying the given chi URL params
// (id/iid/pipeline_id) and query string (target may include "?...").
func forgeReq(method, target string, withWorker bool, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	ctx := req.Context()
	if withWorker {
		ctx = mw.ContextWithWorker(ctx, store.Worker{ID: uuid.New(), UserID: uuid.New()})
	}
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

func errorField(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Error
}

// issuesJSON builds n GitLab issue objects with distinct iids, each carrying a
// web_url so a DTO leak would be visible.
func issuesJSON(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"id": 1000 + i, "iid": i + 1, "title": "issue " + strconv.Itoa(i+1), "state": "opened",
			"labels": []string{"PRD"}, "author": map[string]any{"username": "alice"},
			"updated_at": "2026-07-03T10:00:00Z",
			"web_url":    "https://gitlab.example.com/grp/repo/-/issues/" + strconv.Itoa(i+1),
		})
	}
	return out
}

// ── Item 1: project derivation / SC-4 ───────────────────────────────────────

// TestWorkerForgeUsesProjectIDFromRunNeverRequest proves the handler reads the
// forge project id off the run's connection (ForgeProjectID=4242) and drives the
// driver against THAT project — there is no project-id request parameter. A bogus
// extra query param is present and must be ignored: the read still resolves to
// project 4242 (any other id would 404 the mock route → 502).
func TestWorkerForgeUsesProjectIDFromRunNeverRequest(t *testing.T) {
	hit := false
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, _ *http.Request) {
			hit = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1001, "iid": 11, "title": "Do the thing", "state": "opened",
				"labels": []string{"PRD"}, "description": "hello",
				"web_url": "https://gitlab.example.com/grp/repo/-/issues/11",
			})
		},
	})
	rec := httptest.NewRecorder()
	// The bogus project_id=999 and forge_project_id=1 must be ignored entirely.
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x?project_id=999&forge_project_id=1", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !hit {
		t.Fatal("driver must read project 4242 (off the run's connection); the mock route for 4242 was never hit")
	}
	var dto apitypes.ForgeIssueDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.IID != 11 {
		t.Errorf("iid = %d, want 11", dto.IID)
	}
}

// ── Item 2: auth mapping ─────────────────────────────────────────────────────

func TestWorkerForgeNoWorkerIs401(t *testing.T) {
	// No forge server needed: the 401 fires before any driver build.
	h := newForgeHandler(t, &forgeHandlerStore{}, newForgeBox(t))
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", false,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrAuth {
		t.Errorf("error = %q, want %q", got, forgeErrAuth)
	}
}

func TestWorkerForgeBadRunUUIDIs400(t *testing.T) {
	h := newForgeHandler(t, &forgeHandlerStore{}, newForgeBox(t))
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": "not-a-uuid", "iid": "11"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrInvalid {
		t.Errorf("error = %q, want %q", got, forgeErrInvalid)
	}
}

func TestWorkerForgeNotOwnedIs404(t *testing.T) {
	st := &forgeHandlerStore{ownedErr: pgx.ErrNoRows}
	h := newForgeHandler(t, st, newForgeBox(t))
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrRunNotFound {
		t.Errorf("error = %q, want %q", got, forgeErrRunNotFound)
	}
}

// TestWorkerForgeRepolessIs409BeforeNotFound is the 409-before-404 ordering at the
// handler seam: a repo-less owned run answers 409 forgeErrNoRepo, not 404. The
// connection query is armed to return no-rows (would be 404) to prove the repo gate
// wins.
func TestWorkerForgeRepolessIs409BeforeNotFound(t *testing.T) {
	st := &forgeHandlerStore{
		ownedRun: store.Run{ID: uuid.New(), UserID: uuid.New()}, // repo-less (RepoID invalid)
		connErr:  pgx.ErrNoRows,
	}
	h := newForgeHandler(t, st, newForgeBox(t))
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (repo-less before not-found), body %q", rec.Code, rec.Body.String())
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrNoRepo {
		t.Errorf("error = %q, want %q", got, forgeErrNoRepo)
	}
}

func TestWorkerForgeBadIIDIs400(t *testing.T) {
	// A valid run/conn but a non-numeric {iid} is a bad request (after auth).
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{})
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "abc"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-numeric iid", rec.Code)
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrInvalid {
		t.Errorf("error = %q, want %q", got, forgeErrInvalid)
	}
}

// ── Item 4: error redaction / SC-3 ───────────────────────────────────────────

// TestWorkerForgeRedactsUpstreamCoordinates proves the fixed 502 body carries none
// of the forge coordinates the driver error embeds. The mock 404s with a body that
// echoes a host + project path exactly as the GitLab SDK would ("GET
// https://gitlab.example.com/api/v4/projects/123/issues/9: 404 Not Found"), so the
// driver's err.Error() genuinely contains those substrings; the handler must never
// put it in the response.
func TestWorkerForgeRedactsUpstreamCoordinates(t *testing.T) {
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues/9": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": "GET https://gitlab.example.com/api/v4/projects/123/issues/9: 404 Not Found",
			})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "9"}))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body %q", rec.Code, rec.Body.String())
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrUpstream {
		t.Fatalf("error = %q, want the fixed %q (no driver detail)", got, forgeErrUpstream)
	}
	body := rec.Body.String()
	for _, leak := range []string{"gitlab.example.com", "projects/123", "projects/4242", "/api/v4/"} {
		if strings.Contains(body, leak) {
			t.Errorf("response body leaked forge coordinate %q: %s", leak, body)
		}
	}
}

// ── Item 5: ErrNoPipeline → 200 {"pipeline":null} ────────────────────────────

func TestWorkerForgeLatestPipelineNoPipelineIsNull(t *testing.T) {
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			// An empty list is how the SDK sees "no pipeline for ref" → ErrNoPipeline.
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeLatestPipeline(rec, forgeReq(http.MethodGet, "/x?ref=main", true,
		map[string]string{"id": uuid.New().String()}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (CI-never-ran is not an error), body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeLatestPipelineDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Pipeline != nil {
		t.Errorf("Pipeline = %+v, want nil for a ref that never ran CI", dto.Pipeline)
	}
	if !strings.Contains(rec.Body.String(), `"pipeline":null`) {
		t.Errorf("body must render pipeline:null, got %q", rec.Body.String())
	}
}

// ── Item 6: latest_pipeline selector ─────────────────────────────────────────

func TestWorkerForgeLatestPipelineRefSelector(t *testing.T) {
	refHit, mrHit := false, false
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			refHit = true
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 900, "ref": "main", "sha": "deadbeef", "status": "success",
					"web_url": "https://gitlab.example.com/grp/repo/-/pipelines/900"},
			})
		},
		"/api/v4/projects/4242/merge_requests/5/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			mrHit = true
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeLatestPipeline(rec, forgeReq(http.MethodGet, "/x?ref=main", true,
		map[string]string{"id": uuid.New().String()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !refHit || mrHit {
		t.Fatalf("?ref must call LatestPipeline (branch endpoint) only: refHit=%v mrHit=%v", refHit, mrHit)
	}
	var dto apitypes.ForgeLatestPipelineDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Pipeline == nil || dto.Pipeline.ID != 900 {
		t.Fatalf("want pipeline id 900, got %+v", dto.Pipeline)
	}
}

func TestWorkerForgeLatestPipelineMRSelector(t *testing.T) {
	refHit, mrHit := false, false
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			refHit = true
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
		"/api/v4/projects/4242/merge_requests/5/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			mrHit = true
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 950, "ref": "refs/merge-requests/5/head", "sha": "cafef00d", "status": "running",
					"web_url": "https://gitlab.example.com/grp/repo/-/pipelines/950"},
			})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeLatestPipeline(rec, forgeReq(http.MethodGet, "/x?mr_iid=5", true,
		map[string]string{"id": uuid.New().String()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !mrHit || refHit {
		t.Fatalf("?mr_iid must call LatestMRPipeline only: refHit=%v mrHit=%v", refHit, mrHit)
	}
	var dto apitypes.ForgeLatestPipelineDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Pipeline == nil || dto.Pipeline.ID != 950 {
		t.Fatalf("want pipeline id 950, got %+v", dto.Pipeline)
	}
}

func TestWorkerForgeLatestPipelineSelectorMustBeExactlyOne(t *testing.T) {
	for _, tc := range []struct {
		name, query string
	}{
		{"neither", "/x"},
		{"both", "/x?ref=main&mr_iid=5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No route should be reached; a bare handler with no forge server suffices
			// because the 400 fires before any driver call.
			h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{})
			rec := httptest.NewRecorder()
			h.WorkerForgeLatestPipeline(rec, forgeReq(http.MethodGet, tc.query, true,
				map[string]string{"id": uuid.New().String()}))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s ref/mr_iid, body %q", rec.Code, tc.name, rec.Body.String())
			}
			if got := errorField(t, rec.Body.Bytes()); got != forgeErrInvalid {
				t.Errorf("error = %q, want %q", got, forgeErrInvalid)
			}
		})
	}
}

// ── Item 7: truncation (list_issues, label-events, jobs) ─────────────────────

func TestWorkerForgeListIssuesTruncatesAt50(t *testing.T) {
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Next-Page", "") // single page
			_ = json.NewEncoder(w).Encode(issuesJSON(51))
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeListIssues(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeIssueListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dto.Items) != MaxForgeListItems || dto.Returned != MaxForgeListItems || !dto.Truncated {
		t.Fatalf("51 issues → want items=%d returned=%d truncated=true, got items=%d returned=%d truncated=%v",
			MaxForgeListItems, MaxForgeListItems, len(dto.Items), dto.Returned, dto.Truncated)
	}
}

func TestWorkerForgeListIssuesUnderCapNotTruncated(t *testing.T) {
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode(issuesJSON(3))
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeListIssues(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeIssueListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dto.Items) != 3 || dto.Returned != 3 || dto.Truncated {
		t.Fatalf("3 issues → want items=3 returned=3 truncated=false, got items=%d returned=%d truncated=%v",
			len(dto.Items), dto.Returned, dto.Truncated)
	}
}

func TestWorkerForgeListIssueLabelEventsTruncatesAt50(t *testing.T) {
	events := make([]map[string]any, 0, 51)
	for i := 0; i < 51; i++ {
		events = append(events, map[string]any{
			"id": 500 + i, "action": "add", "created_at": "2026-07-04T09:00:00Z",
			"user":  map[string]any{"id": 42, "username": "carol"},
			"label": map[string]any{"id": 9, "name": "autopilot"},
		})
	}
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues/11/resource_label_events": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode(events)
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeListIssueLabelEvents(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeLabelEventListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dto.Items) != MaxForgeListItems || dto.Returned != MaxForgeListItems || !dto.Truncated {
		t.Fatalf("51 events → want capped to %d truncated=true, got items=%d returned=%d truncated=%v",
			MaxForgeListItems, len(dto.Items), dto.Returned, dto.Truncated)
	}
}

func TestWorkerForgePipelineJobsTruncatesAt50(t *testing.T) {
	jobs := make([]map[string]any, 0, 51)
	for i := 0; i < 51; i++ {
		jobs = append(jobs, map[string]any{
			"id": 700 + i, "name": "job" + strconv.Itoa(i), "stage": "test", "status": "failed",
			"web_url": "https://gitlab.example.com/grp/repo/-/jobs/" + strconv.Itoa(700+i),
		})
	}
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/pipelines/77/jobs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode(jobs)
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgePipelineJobs(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "pipeline_id": "77"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeJobListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dto.Items) != MaxForgeListItems || dto.Returned != MaxForgeListItems || !dto.Truncated {
		t.Fatalf("51 jobs → want capped to %d truncated=true, got items=%d returned=%d truncated=%v",
			MaxForgeListItems, len(dto.Items), dto.Returned, dto.Truncated)
	}
}

// ── Item 8: description byte-safe truncation ─────────────────────────────────

// TestTruncateForgeBody pins the pure truncation helper: a body at/under the cap is
// unchanged, and an over-cap body is cut on a UTF-8 boundary (never mid-rune).
func TestTruncateForgeBody(t *testing.T) {
	// Under the cap: unchanged.
	if got, tr := truncateForgeBody("hello"); got != "hello" || tr {
		t.Errorf("short body: got (%q,%v), want (hello,false)", got, tr)
	}
	// Exactly the cap: unchanged (boundary is len <= cap).
	exact := strings.Repeat("a", MaxForgeBodyBytes)
	if got, tr := truncateForgeBody(exact); got != exact || tr {
		t.Errorf("exact-cap body: truncated=%v (want false), lenGot=%d", tr, len(got))
	}
	// A multibyte rune straddling byte MaxForgeBodyBytes: the cut must drop the
	// partial rune, leaving valid UTF-8 shorter than the cap.
	straddle := strings.Repeat("a", MaxForgeBodyBytes-1) + "世" + strings.Repeat("b", 2000)
	got, tr := truncateForgeBody(straddle)
	if !tr {
		t.Fatal("over-cap body must report truncated=true")
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune: result is not valid UTF-8")
	}
	if strings.ContainsRune(got, '�') {
		t.Fatal("truncation introduced a replacement char")
	}
	if len(got) > MaxForgeBodyBytes {
		t.Fatalf("truncated body len=%d exceeds cap %d", len(got), MaxForgeBodyBytes)
	}
}

// TestWorkerForgeGetIssueDescriptionTruncated drives the same byte-safe cut through
// the full get_issue handler: the DTO must report description_truncated=true with a
// valid-UTF-8 body no longer than the cap.
func TestWorkerForgeGetIssueDescriptionTruncated(t *testing.T) {
	// '世' is 3 bytes; placing it so it starts at byte MaxForgeBodyBytes-1 makes the
	// cap boundary fall INSIDE the rune.
	bigDesc := strings.Repeat("a", MaxForgeBodyBytes-1) + "世" + strings.Repeat("b", 2000)
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1001, "iid": 11, "title": "big", "state": "opened",
				"labels": []string{}, "description": bigDesc,
				"web_url": "https://gitlab.example.com/grp/repo/-/issues/11",
			})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body len %d", rec.Code, rec.Body.Len())
	}
	var dto apitypes.ForgeIssueDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.DescriptionTruncated {
		t.Fatal("description over the cap must set description_truncated=true")
	}
	if !utf8.ValidString(dto.Description) {
		t.Fatal("returned description is not valid UTF-8 (a rune was split)")
	}
	if strings.ContainsRune(dto.Description, '�') {
		t.Fatal("returned description contains a replacement char from a split rune")
	}
	if len(dto.Description) > MaxForgeBodyBytes {
		t.Fatalf("returned description len=%d exceeds cap %d", len(dto.Description), MaxForgeBodyBytes)
	}
}

func TestWorkerForgeGetIssueDescriptionUnderCapUnchanged(t *testing.T) {
	const desc = "a short description with a rune 世 in it"
	h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1001, "iid": 11, "title": "small", "state": "opened",
				"labels": []string{}, "description": desc,
				"web_url": "https://gitlab.example.com/grp/repo/-/issues/11",
			})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeIssueDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.DescriptionTruncated {
		t.Error("an under-cap description must set description_truncated=false")
	}
	if dto.Description != desc {
		t.Errorf("under-cap description = %q, want unchanged %q", dto.Description, desc)
	}
}

// ── PRD #381 M4: get_issue includes bot/system-filtered, bounded comments ────

// forgeMockHandlerBot is forgeMockHandler with the connection's bot_forge_user_id
// set, so the get_issue route can apply the D1 self-filter (a bot id of 0 exercises
// the D9 fail-safe path instead).
func forgeMockHandlerBot(t *testing.T, botForgeUserID int64, routes map[string]http.HandlerFunc) (*Handler, *httptest.Server) {
	t.Helper()
	box := newForgeBox(t)
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sealed, err := box.Seal([]byte("glpat-fake-forge-token-abcdef123456")) //gitleaks:allow fake fixture PAT (literal "fake"), sealed into a throwaway test secretbox; never a real credential
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	st := &forgeHandlerStore{
		ownedRun: store.Run{ID: uuid.New(), UserID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		connRow: store.GetRunForgeConnForWorkerRow{
			ForgeProjectID:  forgeTestProjectID,
			ForgeType:       "gitlab",
			BaseUrl:         srv.URL,
			TokenCiphertext: sealed,
			BotForgeUserID:  botForgeUserID,
		},
	}
	return newForgeHandler(t, st, box), srv
}

// noteJSON builds one GitLab issue-note object (the shape ListIssueNotes returns).
func noteJSON(authorID int64, username, body, createdAt string, system bool) map[string]any {
	return map[string]any{
		"id":         authorID*10 + 1,
		"body":       body,
		"system":     system,
		"created_at": createdAt,
		"author":     map[string]any{"id": authorID, "username": username},
	}
}

// issueAndNotesRoutes serves the get_issue endpoint plus its notes list for iid 11.
func issueAndNotesRoutes(notes []map[string]any) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1001, "iid": 11, "title": "t", "state": "opened",
				"labels": []string{"PRD"}, "description": "d", "author": map[string]any{"username": "alice"},
				"web_url": "https://gitlab.example.com/grp/repo/-/issues/11",
			})
		},
		"/api/v4/projects/4242/issues/11/notes": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode(notes)
		},
	}
}

func getIssueDTO(t *testing.T, h *Handler) apitypes.ForgeIssueDTO {
	t.Helper()
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeIssueDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return dto
}

// TestWorkerForgeGetIssueCommentsFilteredAndOrdered proves the get_issue route drops
// uzi's own bot comment (D1, author id == the connection's bot id), keeps human ones
// oldest-first (D8, driver-guaranteed), and — since the GitLab driver filters System
// notes (D2) — a system note never reaches the DTO.
func TestWorkerForgeGetIssueCommentsFilteredAndOrdered(t *testing.T) {
	const botID = 777
	h, _ := forgeMockHandlerBot(t, botID, issueAndNotesRoutes([]map[string]any{
		noteJSON(10, "alice", "first human", "2026-07-01T09:00:00Z", false),
		noteJSON(botID, "uzi-bot", "uzi status note", "2026-07-01T09:05:00Z", false),
		noteJSON(0, "system", "changed the milestone", "2026-07-01T09:06:00Z", true),
		noteJSON(20, "bob", "second human", "2026-07-01T09:10:00Z", false),
	}))
	dto := getIssueDTO(t, h)
	if dto.CommentsTruncated {
		t.Error("a short thread must not report comments_truncated")
	}
	if len(dto.Comments) != 2 {
		t.Fatalf("want 2 human comments (bot + system dropped), got %d: %+v", len(dto.Comments), dto.Comments)
	}
	if dto.Comments[0].Body != "first human" || dto.Comments[1].Body != "second human" {
		t.Errorf("comments must stay oldest-first human-only, got %+v", dto.Comments)
	}
	if dto.Comments[0].Author != "alice" || dto.Comments[1].Author != "bob" {
		t.Errorf("comment authors = %q,%q, want alice,bob", dto.Comments[0].Author, dto.Comments[1].Author)
	}
	for _, c := range dto.Comments {
		if c.Body == "uzi status note" {
			t.Error("uzi's own bot comment leaked into the DTO (D1 filter failed)")
		}
	}
}

// TestWorkerForgeGetIssueCommentsZeroBotIDOmits proves D9: a connection with an
// unknown (zero) bot id yields no comments at all rather than risk leaking uzi's own.
func TestWorkerForgeGetIssueCommentsZeroBotIDOmits(t *testing.T) {
	h, _ := forgeMockHandlerBot(t, 0, issueAndNotesRoutes([]map[string]any{
		noteJSON(10, "alice", "a human comment", "2026-07-01T09:00:00Z", false),
	}))
	dto := getIssueDTO(t, h)
	if len(dto.Comments) != 0 {
		t.Fatalf("D9: a zero bot id must yield no comments, got %+v", dto.Comments)
	}
	if dto.CommentsTruncated {
		t.Error("D9 omission is not a truncation")
	}
	// The empty slice must marshal as [] not null.
	if !strings.Contains(getIssueRawBody(t, h), `"comments":[]`) {
		t.Error("comments must marshal as [] when empty, not null")
	}
}

func getIssueRawBody(t *testing.T, h *Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true,
		map[string]string{"id": uuid.New().String(), "iid": "11"}))
	return rec.Body.String()
}

// TestWorkerForgeGetIssueCommentsByteCapTruncates proves the get_issue route applies
// its OWN byte cap: an over-cap thread keeps the newest content and sets
// comments_truncated, dropping the oldest.
func TestWorkerForgeGetIssueCommentsByteCapTruncates(t *testing.T) {
	const botID = 777
	big := strings.Repeat("x", MaxForgeBodyBytes)
	h, _ := forgeMockHandlerBot(t, botID, issueAndNotesRoutes([]map[string]any{
		noteJSON(10, "alice", "OLDEST-dropped-"+big, "2026-07-01T09:00:00Z", false),
		noteJSON(20, "bob", "NEWEST-kept", "2026-07-01T09:10:00Z", false),
	}))
	dto := getIssueDTO(t, h)
	if !dto.CommentsTruncated {
		t.Fatal("an over-cap thread must set comments_truncated=true")
	}
	if len(dto.Comments) != 1 || dto.Comments[0].Body != "NEWEST-kept" {
		t.Fatalf("byte cap must keep the newest comment only, got %+v", dto.Comments)
	}
}

// TestWorkerForgeGetIssueCommentsReadFailureIsBestEffort proves a comments read
// failure does NOT fail get_issue: the description still reaches the agent and the
// comment list is empty. The notes route 500s; the issue route succeeds.
func TestWorkerForgeGetIssueCommentsReadFailureIsBestEffort(t *testing.T) {
	const botID = 777
	routes := issueAndNotesRoutes(nil)
	routes["/api/v4/projects/4242/issues/11/notes"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
	}
	h, _ := forgeMockHandlerBot(t, botID, routes)
	dto := getIssueDTO(t, h)
	if dto.Description != "d" {
		t.Errorf("description must still reach the agent on a comments read failure, got %q", dto.Description)
	}
	if len(dto.Comments) != 0 || dto.CommentsTruncated {
		t.Errorf("a comments read failure returns an empty, non-truncated comment list, got %+v tr=%v", dto.Comments, dto.CommentsTruncated)
	}
}

// ── Item 9: DTOs carry no forge coordinates (SC-3) ───────────────────────────

// TestWorkerForgeDTOsHaveNoCoordinates asserts get_issue, get_merge_request, and
// pipeline-jobs response bodies contain no web_url/webUrl key and no host, even
// though every mocked upstream struct has web_url populated.
func TestWorkerForgeDTOsHaveNoCoordinates(t *testing.T) {
	runID := uuid.New().String()

	t.Run("get_issue", func(t *testing.T) {
		h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
			"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 1001, "iid": 11, "title": "t", "state": "opened",
					"labels": []string{"PRD"}, "description": "d", "author": map[string]any{"username": "alice"},
					"web_url": "https://gitlab.example.com/grp/repo/-/issues/11",
				})
			},
		})
		rec := httptest.NewRecorder()
		h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": runID, "iid": "11"}))
		assertNoCoordinates(t, rec)
	})

	t.Run("get_merge_request", func(t *testing.T) {
		h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
			"/api/v4/projects/4242/merge_requests/13": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 5005, "iid": 13, "state": "opened",
					"web_url": "https://gitlab.example.com/grp/repo/-/merge_requests/13",
				})
			},
		})
		rec := httptest.NewRecorder()
		h.WorkerForgeGetMergeRequest(rec, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": runID, "iid": "13"}))
		assertNoCoordinates(t, rec)
	})

	t.Run("pipeline_jobs", func(t *testing.T) {
		h, _ := forgeMockHandler(t, map[string]http.HandlerFunc{
			"/api/v4/projects/4242/pipelines/77/jobs": func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Next-Page", "")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 701, "name": "build", "stage": "build", "status": "failed",
						"web_url": "https://gitlab.example.com/grp/repo/-/jobs/701"},
				})
			},
		})
		rec := httptest.NewRecorder()
		h.WorkerForgePipelineJobs(rec, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": runID, "pipeline_id": "77"}))
		assertNoCoordinates(t, rec)
	})
}

func assertNoCoordinates(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, banned := range []string{"web_url", "webUrl", "gitlab.example.com", "https://", "/api/v4/", "grp/repo"} {
		if strings.Contains(body, banned) {
			t.Errorf("DTO leaked coordinate %q: %s", banned, body)
		}
	}
	// The DTO must also parse into its typed shape (no stray keys smuggled through).
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"web_url", "webUrl", "base_url", "project_id", "forge_project_id"} {
		if _, ok := generic[k]; ok {
			t.Errorf("DTO must not carry key %q, got %s", k, body)
		}
	}
}
