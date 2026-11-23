package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// rwStub replaces the store read behind AdminRunWriters for one test and records the call.
type rwStub struct {
	calls   int
	repo    uuid.UUID
	branch  string
	mr      *int64
	maxRows int32
	rows    []store.ListRunWritersForTargetRow
	err     error
}

func rwInstallStub(t *testing.T, s *rwStub) {
	t.Helper()
	prev := listRunWriters
	listRunWriters = func(_ context.Context, _ *pgxpool.Pool, repoID uuid.UUID, branch string, mrIID *int64, maxRows int32) ([]store.ListRunWritersForTargetRow, error) {
		s.calls++
		s.repo, s.branch, s.mr, s.maxRows = repoID, branch, mrIID, maxRows
		return s.rows, s.err
	}
	t.Cleanup(func() { listRunWriters = prev })
}

// rwCaller is one way of reaching the route.
type rwCaller struct {
	name    string
	flag    bool
	cookie  bool
	scope   string
	isAdmin bool
	// routerRefusal: RequireAdminRO answers before the handler is reached.
	routerRefusal bool
}

var rwCallers = []rwCaller{
	{"flag off uza", false, false, clitoken.ScopeAdminRO, true, false},
	{"cookie admin", true, true, "", true, false},
	{"uzc scope of an admin", true, false, clitoken.ScopeUser, true, true},
	{"non-admin uza", true, false, clitoken.ScopeAdminRO, false, true},
}

var rwUza = rwCaller{"admin_ro uza", true, false, clitoken.ScopeAdminRO, true, false}

// rwServe drives the real RequireUser and RequireAdminRO middlewares in front of the
// handler, mounted as routes_admin.go mounts it.
func rwServe(t *testing.T, c rwCaller, target string) *httptest.ResponseRecorder {
	t.Helper()
	uid := uuid.New()
	st := adminDebugFakeStore{user: store.User{ID: uid, IsActive: true, IsAdmin: c.isAdmin}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if c.cookie {
		tok, err := auth.IssueToken(adminDebugCfg(true).JWTSecret, uid.String(), 0, time.Hour)
		if err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: tok}) //nolint:gosec // G124: test-only client cookie.
	} else {
		tok, hash, _, err := clitoken.Generate(c.scope)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		st.row = store.CliToken{ID: uuid.New(), UserID: uid, TokenHash: hash, Scope: c.scope}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	h := &Handler{cfg: adminDebugCfg(c.flag)}
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireUser(st, adminDebugCfg(c.flag)))
		r.Use(mw.RequireAdminRO)
		r.Get("/api/admin/run-writers", h.AdminRunWriters)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func rwValidQuery() url.Values {
	return url.Values{"repo": {uuid.New().String()}, "branch": {"fix/ci"}}
}

func TestAdminRunWritersRefusalIsIdenticalForEverySelector(t *testing.T) {
	audit := captureAdminRunDebug(t)
	stub := &rwStub{}
	rwInstallStub(t, stub)
	selectors := []string{
		"repo=not-a-uuid&branch=&mr_iid=05&repo=x",
		"repo=" + uuid.New().String() + "&branch=nope",
		rwValidQuery().Encode(),
		rwValidQuery().Encode() + "&mr_iid=7;",
		rwValidQuery().Encode() + "&x=1",
	}
	for _, c := range rwCallers {
		var first string
		for i, q := range selectors {
			rec := rwServe(t, c, "/api/admin/run-writers?"+q)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s/%d: code = %d, want 403: %s", c.name, i, rec.Code, rec.Body.String())
			}
			if i == 0 {
				first = rec.Body.String()
			} else if rec.Body.String() != first {
				t.Errorf("%s: refusal body differs across selectors: %q vs %q", c.name, rec.Body.String(), first)
			}
		}
	}
	if stub.calls != 0 {
		t.Errorf("store read ran %d times for refused callers", stub.calls)
	}
	if len(audit.recs) != 0 {
		t.Errorf("audit records on refusal = %d, want 0", len(audit.recs))
	}
}

// uzc_ and demoted callers are refused by RequireAdminRO and never reach the handler; a
// cookie admin and a flag-off uza_ reach the handler and get its refusal text.
func TestAdminRunWritersRouterLevelRefusalSource(t *testing.T) {
	for _, c := range rwCallers {
		rec := rwServe(t, c, "/api/admin/run-writers?"+rwValidQuery().Encode())
		fromHandler := strings.Contains(rec.Body.String(), adminRunDebugRefusal)
		if fromHandler == c.routerRefusal {
			t.Errorf("%s: handler refusal = %v, want %v (body %q)", c.name, fromHandler, !c.routerRefusal, rec.Body.String())
		}
	}
}

func TestAdminRunWritersValidation(t *testing.T) {
	good := uuid.New().String()
	long := strings.Repeat("a", 256)
	cases := []struct {
		name  string
		query string
	}{
		{"missing repo", "branch=b"},
		{"bad repo", "repo=nope&branch=b"},
		{"missing branch", "repo=" + good},
		{"empty branch", "repo=" + good + "&branch="},
		{"branch with newline", "repo=" + good + "&branch=a%0Ab"},
		{"branch with ESC", "repo=" + good + "&branch=a%1Bb"},
		{"branch with bidi override", "repo=" + good + "&branch=a%E2%80%AEb"},
		{"branch invalid utf8", "repo=" + good + "&branch=a%FFb"},
		{"branch too long", "repo=" + good + "&branch=" + long},
		{"mr_iid empty", "repo=" + good + "&branch=b&mr_iid="},
		{"mr_iid plus", "repo=" + good + "&branch=b&mr_iid=%2B5"},
		{"mr_iid leading zero", "repo=" + good + "&branch=b&mr_iid=05"},
		{"mr_iid zero", "repo=" + good + "&branch=b&mr_iid=0"},
		{"mr_iid negative", "repo=" + good + "&branch=b&mr_iid=-3"},
		{"mr_iid word", "repo=" + good + "&branch=b&mr_iid=abc"},
		{"mr_iid overflow", "repo=" + good + "&branch=b&mr_iid=99999999999999999999"},
		{"mr_iid space", "repo=" + good + "&branch=b&mr_iid=%205"},
		{"repeated repo", "repo=" + good + "&repo=" + good + "&branch=b"},
		{"repeated branch", "repo=" + good + "&branch=b&branch=b"},
		{"repeated mr_iid", "repo=" + good + "&branch=b&mr_iid=5&mr_iid=5"},
		{"semicolon pair would be dropped", "repo=" + good + "&branch=b&mr_iid=7;"},
		{"bad percent escape would be dropped", "repo=" + good + "&branch=b&mr_iid=%G7"},
		{"semicolon in a repeated branch", "repo=" + good + "&branch=a;b&branch=B"},
		{"unknown key", "repo=" + good + "&branch=b&mr=7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := captureAdminRunDebug(t)
			stub := &rwStub{}
			rwInstallStub(t, stub)
			rec := rwServe(t, rwUza, "/api/admin/run-writers?"+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if stub.calls != 0 || len(audit.recs) != 0 {
				t.Errorf("store calls = %d audit = %d, want 0 and 0", stub.calls, len(audit.recs))
			}
		})
	}
}

func TestAdminRunWritersBranchIsByteExact(t *testing.T) {
	stub := &rwStub{}
	rwInstallStub(t, stub)
	q := url.Values{"repo": {uuid.New().String()}, "branch": {" fix/ci "}}
	if rec := rwServe(t, rwUza, "/api/admin/run-writers?"+q.Encode()); rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if stub.branch != " fix/ci " {
		t.Errorf("branch handed to the store = %q, want the exact bytes", stub.branch)
	}
}

func TestAdminRunWritersUnknownRepoIs422WithoutAudit(t *testing.T) {
	audit := captureAdminRunDebug(t)
	rwInstallStub(t, &rwStub{err: store.ErrRunWritersRepoNotFound})
	rec := rwServe(t, rwUza, "/api/admin/run-writers?"+rwValidQuery().Encode())
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "repo not found") {
		t.Fatalf("code = %d body %s, want 422 repo not found", rec.Code, rec.Body.String())
	}
	if len(audit.recs) != 0 {
		t.Errorf("audit records = %d, want 0", len(audit.recs))
	}
}

func TestAdminRunWritersDBErrorIs500WithoutAudit(t *testing.T) {
	audit := captureAdminRunDebug(t)
	rwInstallStub(t, &rwStub{err: errors.New("boom-detail")})
	rec := rwServe(t, rwUza, "/api/admin/run-writers?"+rwValidQuery().Encode())
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("code = %d body %s, want 500 internal error", rec.Code, rec.Body.String())
	}
	if len(audit.recs) != 0 {
		t.Errorf("audit records = %d, want 0", len(audit.recs))
	}
}

func rwRow(kind, status string, ref *string, mr *int64) store.ListRunWritersForTargetRow {
	row := store.ListRunWritersForTargetRow{
		ID: uuid.New(), Kind: kind, Status: status, UserID: uuid.New(),
		RepoID:    pgtype.UUID{Bytes: uuid.New(), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
	}
	if ref != nil {
		row.PipelineRef = pgtype.Text{String: *ref, Valid: true}
	}
	if mr != nil {
		row.MrIid = pgtype.Int8{Int64: *mr, Valid: true}
	}
	return row
}

func rwKeys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAdminRunWritersSuccessShapeAndAudit(t *testing.T) {
	audit := captureAdminRunDebug(t)
	ref, mr := "secret-branch-name", int64(42)
	stub := &rwStub{rows: []store.ListRunWritersForTargetRow{rwRow("ci_fix", "running", &ref, nil), rwRow("mr_rework", "queued", &ref, &mr)}}
	rwInstallStub(t, stub)
	repo := uuid.New()
	q := url.Values{"repo": {repo.String()}, "branch": {ref}, "mr_iid": {"42"}}
	rec := rwServe(t, rwUza, "/api/admin/run-writers?"+q.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if stub.maxRows != 501 || stub.repo != repo || stub.mr == nil || *stub.mr != 42 {
		t.Errorf("store call = repo %s mr %v maxRows %d, want the request's repo, 42, 501", stub.repo, stub.mr, stub.maxRows)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if keys := rwKeys(env); !reflect.DeepEqual(keys, []string{"complete", "writers"}) {
		t.Errorf("envelope keys = %v", keys)
	}
	var writers []map[string]json.RawMessage
	if err := json.Unmarshal(env["writers"], &writers); err != nil || len(writers) != 2 {
		t.Fatalf("writers = %s (%v)", env["writers"], err)
	}
	want := []string{"created_at", "kind", "mr_iid", "owner_id", "pipeline_ref", "repo_id", "run_id", "status"}
	for _, w := range writers {
		if got := rwKeys(w); !reflect.DeepEqual(got, want) {
			t.Errorf("writer keys = %v, want %v", got, want)
		}
	}
	if string(writers[0]["mr_iid"]) != "null" || string(writers[1]["mr_iid"]) != "42" {
		t.Errorf("mr_iid values = %s, %s", writers[0]["mr_iid"], writers[1]["mr_iid"])
	}
	if string(env["complete"]) != "true" {
		t.Errorf("complete = %s", env["complete"])
	}

	if len(audit.recs) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.recs))
	}
	attrs := recordAttrs(audit.recs[0])
	if attrs["route"] != "admin.run_writers" || attrs["repo_id"] != repo.String() || attrs["mr_iid"] != int64(42) ||
		attrs["branch_bytes"] != int64(len(ref)) || attrs["returned"] != int64(2) || attrs["complete"] != true {
		t.Errorf("audit attrs = %v", attrs)
	}
	for k, v := range attrs {
		if s, ok := v.(string); ok && strings.Contains(s, ref) {
			t.Errorf("audit attr %s carries the branch text", k)
		}
	}
}

func TestAdminRunWritersEmptyAndOverflow(t *testing.T) {
	t.Run("empty is complete, a non-null array, and audited once without an mr", func(t *testing.T) {
		audit := captureAdminRunDebug(t)
		rwInstallStub(t, &rwStub{rows: nil})
		rec := rwServe(t, rwUza, "/api/admin/run-writers?"+rwValidQuery().Encode())
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"writers":[],"complete":true}` {
			t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
		}
		if len(audit.recs) != 1 {
			t.Fatalf("audit records = %d, want 1", len(audit.recs))
		}
		if attrs := recordAttrs(audit.recs[0]); attrs["mr_iid"] != "none" || attrs["returned"] != int64(0) || attrs["complete"] != true {
			t.Errorf("audit attrs = %v", attrs)
		}
	})
	t.Run("a full 500 is complete, 501 is not and serves 500", func(t *testing.T) {
		ref := "b"
		for _, n := range []int{500, 501} {
			audit := captureAdminRunDebug(t)
			rows := make([]store.ListRunWritersForTargetRow, n)
			for i := range rows {
				rows[i] = rwRow("ci_fix", "queued", &ref, nil)
			}
			rwInstallStub(t, &rwStub{rows: rows})
			rec := rwServe(t, rwUza, "/api/admin/run-writers?"+rwValidQuery().Encode())
			var dto struct {
				Writers  []json.RawMessage `json:"writers"`
				Complete bool              `json:"complete"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			if len(dto.Writers) != 500 || dto.Complete != (n == 500) {
				t.Errorf("n=%d: served %d complete=%v", n, len(dto.Writers), dto.Complete)
			}
			if attrs := recordAttrs(audit.recs[0]); attrs["returned"] != int64(500) || attrs["complete"] != (n == 500) {
				t.Errorf("n=%d: audit attrs = %v", n, attrs)
			}
		}
	})
}

// The CLI admin-surface matrix asserts every path in adminReadPaths answers 200 to a uza_
// with no flag; this route answers 403 without UZI_ADMIN_RUN_DEBUG, so it must stay out.
func TestAdminRunWritersRouteIsNotInAdminReadPaths(t *testing.T) {
	for _, p := range adminReadPaths {
		if strings.Contains(p, "run-writers") {
			t.Errorf("adminReadPaths contains %q", p)
		}
	}
}
