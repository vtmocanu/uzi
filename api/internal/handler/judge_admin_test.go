package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// adminBacklogStore serves the PRD #1184 M1 admin aggregate read AND the all-users triage
// aggregate from a fixture. Unlike backlogStore the two come from SEPARATE slices, because the
// two SQL queries are separate (the *All backlog query drops dismiss_reason, which only the
// triage query carries). It embeds workersvc.Store (nil), so ANY method the read path does not
// use panics — the structural "no token spend, no forge write" proof.
type adminBacklogStore struct {
	workersvc.Store

	backlogRows []store.ListJudgeRecommendationRowsAllRow
	triageRows  []store.ListJudgeTriageRowsAllRow

	backlogArg *store.ListJudgeRecommendationRowsAllParams
	calls      []string
}

func (s *adminBacklogStore) ListJudgeRecommendationRowsAll(_ context.Context, arg store.ListJudgeRecommendationRowsAllParams) ([]store.ListJudgeRecommendationRowsAllRow, error) {
	s.calls = append(s.calls, "ListJudgeRecommendationRowsAll")
	s.backlogArg = &arg
	// Mirror the query's `rr.category = ANY(@categories)` push-down: a nil/empty slice is the
	// "all labels" no-op, otherwise only rows whose category is in the set survive.
	if len(arg.Categories) == 0 {
		return s.backlogRows, nil
	}
	keep := make(map[string]bool, len(arg.Categories))
	for _, c := range arg.Categories {
		keep[c] = true
	}
	out := make([]store.ListJudgeRecommendationRowsAllRow, 0, len(s.backlogRows))
	for _, r := range s.backlogRows {
		if keep[r.Category] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *adminBacklogStore) ListJudgeTriageRowsAll(_ context.Context) ([]store.ListJudgeTriageRowsAllRow, error) {
	s.calls = append(s.calls, "ListJudgeTriageRowsAll")
	return s.triageRows, nil
}

// adminFixture is two owners sharing the SAME (improve_uzi, docs) coordinate — the cross-user
// dedup case this view exists for — plus a third coordinate under one owner. Both docs
// occurrences are open (no disposition), so the group is todo with open_count 2, run_count 2,
// user_count 2. Returns the two seeded user ids and run ids so a test can assert they never
// appear in the response.
func adminFixture() (rows []store.ListJudgeRecommendationRowsAllRow, ownerA, ownerB, runA, runB uuid.UUID) {
	ownerA, ownerB = uuid.New(), uuid.New()
	runA, runB = uuid.New(), uuid.New()
	runC := uuid.New()
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
	rows = []store.ListJudgeRecommendationRowsAllRow{
		// ownerA, docs — open.
		{UserID: ownerA, RunID: runA, Verdict: "issues", Category: "improve_uzi", Target: "docs", RationaleMd: "ownerA docs rationale", Confidence: "high"},
		// ownerB, docs — open. Same coordinate, DIFFERENT user: dedups into one group.
		{UserID: ownerB, RunID: runB, Verdict: "ok", Category: "improve_uzi", Target: "docs", RationaleMd: "ownerB docs rationale", Confidence: "low"},
		// ownerA, a done coordinate under its own target.
		{UserID: ownerA, RunID: runC, Verdict: "ok", Category: "improve_agent", Target: "coder", DispositionStatus: txt("done")},
	}
	return rows, ownerA, ownerB, runA, runB
}

// adminTriageFixture matches adminFixture's coordinates for a sane strip: two todo, one done.
func adminTriageFixture() []store.ListJudgeTriageRowsAllRow {
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
	return []store.ListJudgeTriageRowsAllRow{
		{},                               // ownerA docs — todo
		{},                               // ownerB docs — todo
		{DispositionStatus: txt("done")}, // ownerA coder — done
	}
}

func adminJudgeReq(user store.User, path, query string) *http.Request {
	url := path
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

// adminJudgeWriteReq builds a PUT/DELETE disposition request carrying a JSON body, authenticated
// as user via the context (the middleware chain is exercised separately in the live-DB router
// tests; these unit tests drive the handler directly to pin its body validation).
func adminJudgeWriteReq(user store.User, method, body string) *http.Request {
	req := httptest.NewRequest(method, "/api/admin/judge/recommendations/disposition", strings.NewReader(body))
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

// ---- 401: no user in context, and the store is never touched -------------------------------

func TestAdminJudgeHandlersUnauthenticatedIs401(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request)
		path string
	}{
		{"recommendations", func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.AdminJudgeRecommendations(rec, req)
		}, "/api/admin/judge/recommendations"},
		{"stats", func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) { h.AdminJudgeStats(rec, req) }, "/api/admin/judge/stats"},
		{"category-stats", func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.AdminJudgeCategoryStats(rec, req)
		}, "/api/admin/judge/category-stats"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{}
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			tc.call(h, rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated GET = %d, want 401", rec.Code)
			}
			if len(st.calls) != 0 {
				t.Fatalf("an unauthenticated request must not reach the store, calls=%v", st.calls)
			}
		})
	}
}

// ---- 403 / 200: the admin READ group gates on IsAdmin ------------------------------------

// TestAdminJudgeReadGroupGatesOnIsAdmin drives the three handlers behind mw.RequireAdminRO —
// the exact middleware the admin read group mounts — with an admin, a non-admin, and no user.
// This is what makes a masked uzc_ token (IsAdmin cleared by cli_auth.go) a 403 and a uza_
// admin_ro token a 200; the token masking itself is proven end-to-end in the live-DB suite.
func TestAdminJudgeReadGroupGatesOnIsAdmin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(h *Handler) http.HandlerFunc
	}{
		{"recommendations", func(h *Handler) http.HandlerFunc { return h.AdminJudgeRecommendations }},
		{"stats", func(h *Handler) http.HandlerFunc { return h.AdminJudgeStats }},
		{"category-stats", func(h *Handler) http.HandlerFunc { return h.AdminJudgeCategoryStats }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, _, _, _ := adminFixture()
			st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
			h := newRunsHandler(t, st)
			gated := mw.RequireAdminRO(tc.handler(h))

			// admin → 200
			rec := httptest.NewRecorder()
			gated.ServeHTTP(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/x", ""))
			if rec.Code != http.StatusOK {
				t.Errorf("admin GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			// non-admin (a masked uzc_ token yields exactly this: IsAdmin=false) → 403
			rec = httptest.NewRecorder()
			gated.ServeHTTP(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: false}, "/x", ""))
			if rec.Code != http.StatusForbidden {
				t.Errorf("non-admin GET = %d, want 403 (a masked uzc_ token is IsAdmin=false)", rec.Code)
			}
			// no user in context → 401 (RequireAdminRO's own guard)
			rec = httptest.NewRecorder()
			gated.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("no-user GET = %d, want 401", rec.Code)
			}
		})
	}
}

// ---- the cross-user dedup + counts, attribution hidden -----------------------------------

// TestAdminJudgeRecommendationsDedupsAcrossUsers: the same (category, target) in two DIFFERENT
// users' runs comes back as ONE group with user_count 2, run_count 2, open_count 2, and its
// occurrences carry NO run id, user id or any other identifier — asserted both structurally
// (the DTO has no such field) and by scanning the serialized response for the seeded UUIDs.
func TestAdminJudgeRecommendationsDedupsAcrossUsers(t *testing.T) {
	rows, ownerA, ownerB, runA, runB := adminFixture()
	st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.AdminJudgeRecommendations(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/recommendations", "bucket=all"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got apitypes.JudgeAdminBacklogDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.Bucket != "all" {
		t.Errorf("echoed bucket = %q, want all", got.Bucket)
	}
	var docs apitypes.JudgeAdminGroupDTO
	found := false
	for _, g := range got.Groups {
		if g.Category == "improve_uzi" && g.Target == "docs" {
			docs, found = g, true
		}
	}
	if !found {
		t.Fatalf("no (improve_uzi, docs) group in %+v", got.Groups)
	}
	if docs.UserCount != 2 || docs.RunCount != 2 || docs.OpenCount != 2 || docs.Bucket != "todo" {
		t.Fatalf("docs group = user_count %d / run_count %d / open_count %d / bucket %q, want 2/2/2/todo",
			docs.UserCount, docs.RunCount, docs.OpenCount, docs.Bucket)
	}
	if len(docs.Occurrences) != 2 {
		t.Fatalf("want 2 occurrences, got %+v", docs.Occurrences)
	}
	// The response must not contain the seeded owner or run UUIDs anywhere — the value-level
	// companion to the wire-tag test (a leak riding a permitted field's value).
	body := rec.Body.String()
	for _, id := range []uuid.UUID{ownerA, ownerB, runA, runB} {
		if strings.Contains(body, id.String()) {
			t.Errorf("the admin response leaked an identifier %s\nbody: %s", id, body)
		}
	}
}

// TestAdminJudgeRecommendationsDefaultBucketIsTodo: no ?bucket= means todo; the done group is
// filtered out and the bucket is echoed.
func TestAdminJudgeRecommendationsDefaultBucketIsTodo(t *testing.T) {
	rows, _, _, _, _ := adminFixture()
	st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.AdminJudgeRecommendations(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/recommendations", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got apitypes.JudgeAdminBacklogDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bucket != "todo" {
		t.Errorf("echoed bucket = %q, want todo (the default)", got.Bucket)
	}
	if len(got.Groups) != 1 || got.Groups[0].Target != "docs" {
		t.Fatalf("default view = %+v, want only the group with an open member", got.Groups)
	}
}

// TestAdminJudgeRecommendationsRejectsBadParamsAndIgnoresRun: an unknown bucket or category is
// a 400; an absent/empty category is all labels; and a ?run= param is NOT an anchor on the
// admin path — it is simply ignored (never parsed, never reaches the query), so a malformed
// one is still a 200 rather than the 400 the owner handler returns.
func TestAdminJudgeRecommendationsRejectsBadParamsAndIgnoresRun(t *testing.T) {
	rows, _, _, _, _ := adminFixture()
	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"unknown bucket", "bucket=open", http.StatusBadRequest},
		{"empty-but-present bucket falls back to the default", "bucket=", http.StatusOK},
		{"unknown category", "category=nope", http.StatusBadRequest},
		{"one good one bad category still 400", "category=improve_uzi,nope", http.StatusBadRequest},
		{"empty-but-present category is all labels", "category=", http.StatusOK},
		{"category with only empty tokens is all labels", "category=,", http.StatusOK},
		{"a run param is ignored, not an anchor — still 200", "run=not-a-uuid", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			h.AdminJudgeRecommendations(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/recommendations", tc.query))
			if rec.Code != tc.want {
				t.Fatalf("GET ?%s = %d, want %d; body=%s", tc.query, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestAdminJudgeRecommendationsPushesCategoriesToTheQuery: the ?category= filter is parsed,
// normalized and deduped, then handed to the ALL-users query as a NIL slice when absent (→ SQL
// NULL → all labels), never []string{}.
func TestAdminJudgeRecommendationsPushesCategoriesToTheQuery(t *testing.T) {
	rows, _, _, _, _ := adminFixture()
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"absent category is a nil slice", "", nil},
		{"present-but-empty category is a nil slice", "category=", nil},
		{"single value", "category=improve_uzi", []string{"improve_uzi"}},
		{"repeated value is deduped", "category=improve_uzi,improve_uzi", []string{"improve_uzi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			h.AdminJudgeRecommendations(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/recommendations", tc.query))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET ?%s = %d, want 200; body=%s", tc.query, rec.Code, rec.Body.String())
			}
			if st.backlogArg == nil {
				t.Fatalf("the query was never called")
			}
			got := st.backlogArg.Categories
			if len(got) != len(tc.want) {
				t.Fatalf("Categories param = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Categories param = %#v, want %#v", got, tc.want)
				}
			}
			if tc.want == nil && got != nil {
				t.Fatalf("no ?category= must send a NIL slice (→ SQL NULL → all labels), got %#v", got)
			}
		})
	}
}

// TestAdminJudgeStatsReturnsTheAllUsersTally: GET /admin/judge/stats returns the cross-user
// TriageDTO from ListJudgeTriageRowsAll (two todo, one done here).
func TestAdminJudgeStatsReturnsTheAllUsersTally(t *testing.T) {
	st := &adminBacklogStore{triageRows: adminTriageFixture()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.AdminJudgeStats(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/stats", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got apitypes.TriageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := apitypes.TriageDTO{Total: 3, Todo: 2, Done: 1}
	if got != want {
		t.Fatalf("all-users stats = %+v, want %+v", got, want)
	}
}

// TestAdminJudgeCategoryStatsReturnsTheMatrix: GET /admin/judge/category-stats runs the shared
// rollup over the uncapped all-users load and returns the bucket → category → count matrix,
// parses no ?run= (the admin path has no anchor).
func TestAdminJudgeCategoryStatsReturnsTheMatrix(t *testing.T) {
	rows, _, _, _, _ := adminFixture()
	st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.AdminJudgeCategoryStats(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/category-stats", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got apitypes.JudgeCategoryStatsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CountsByBucket["todo"]["improve_uzi"] != 1 {
		t.Errorf("todo/improve_uzi = %d, want 1 (the deduped docs group is todo)", got.CountsByBucket["todo"]["improve_uzi"])
	}
	if got.CountsByBucket["done"]["improve_agent"] != 1 {
		t.Errorf("done/improve_agent = %d, want 1", got.CountsByBucket["done"]["improve_agent"])
	}
	if got.CountsByBucket["all"]["improve_uzi"] != 1 || got.CountsByBucket["all"]["improve_agent"] != 1 {
		t.Errorf("all-bucket matrix = %+v, want one group per category", got.CountsByBucket["all"])
	}
}

// TestAdminJudgeRecommendationsReportsTruncation: the all-users backlog carries the hard-cap
// flag when the query returns cap+1 rows, and trims to the cap — the same semantics as the
// owner backlog. Deterministic (a fake returning cap+1 rows), the SQL LIMIT binding is proven
// in the live-DB test.
func TestAdminJudgeRecommendationsReportsTruncation(t *testing.T) {
	over := make([]store.ListJudgeRecommendationRowsAllRow, 0, workersvc.JudgeBacklogMaxRows+1)
	for i := 0; i <= workersvc.JudgeBacklogMaxRows; i++ {
		over = append(over, store.ListJudgeRecommendationRowsAllRow{
			UserID: uuid.New(), RunID: uuid.New(), Verdict: "ok",
			Category: "improve_uzi", Target: uuid.NewString(),
		})
	}
	st := &adminBacklogStore{backlogRows: over, triageRows: adminTriageFixture()}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	h.AdminJudgeRecommendations(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/recommendations", "bucket=all"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var got apitypes.JudgeAdminBacklogDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// cap+1 distinct targets → cap+1 groups before the trim; the trim drops the oldest row, so
	// at most JudgeBacklogMaxRows groups survive.
	if !got.Truncated || len(got.Groups) > workersvc.JudgeBacklogMaxRows {
		t.Fatalf("truncated=%v groups=%d, want true / <= %d", got.Truncated, len(got.Groups), workersvc.JudgeBacklogMaxRows)
	}

	// The un-truncated fixture must NOT set the flag.
	rows, _, _, _, _ := adminFixture()
	h = newRunsHandler(t, &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()})
	rec = httptest.NewRecorder()
	h.AdminJudgeRecommendations(rec, adminJudgeReq(store.User{ID: uuid.New(), IsAdmin: true}, "/api/admin/judge/recommendations", "bucket=all"))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Truncated {
		t.Fatal("a small backlog must not be flagged truncated")
	}
}

// ---- the admin cross-user Mark done / Undo body validation (PRD #1184 M2) ------------------

// TestAdminSetJudgeDispositionRejectsBadBody pins the PUT handler's validation, every case of
// which rejects BEFORE the service is reached — so the embedded nil workersvc.Store (which panics
// on any call) is itself the proof that nothing was written. The strict decoder is what turns a
// stray `scope`/`reason` field into a 400 rather than a silent no-op, and `status` must be exactly
// "done" (there is no cross-user Dismiss, so "dismissed" is a 400).
func TestAdminSetJudgeDispositionRejectsBadBody(t *testing.T) {
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"dismissed is not allowed cross-user", `{"items":[{"category":"improve_uzi","target":"docs"}],"status":"dismissed"}`, http.StatusBadRequest},
		{"empty status", `{"items":[{"category":"improve_uzi","target":"docs"}],"status":""}`, http.StatusBadRequest},
		{"a scope field is rejected by the strict decoder", `{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done","scope":"all"}`, http.StatusBadRequest},
		{"a reason field is rejected by the strict decoder", `{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done","reason":"wont_do"}`, http.StatusBadRequest},
		{"an unknown field is rejected", `{"items":[],"status":"done","bogus":1}`, http.StatusBadRequest},
		{"empty items with a valid status", `{"items":[],"status":"done"}`, http.StatusBadRequest},
		{"malformed JSON", `{`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{} // nil Store: any service call panics, proving the reject is pre-service
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			h.AdminSetJudgeDisposition(rec, adminJudgeWriteReq(admin, http.MethodPut, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d; body=%s", tc.body, rec.Code, tc.want, rec.Body.String())
			}
			if len(st.calls) != 0 {
				t.Fatalf("a rejected PUT must not reach the store, calls=%v", st.calls)
			}
		})
	}
}

// TestAdminUndoJudgeDispositionRejectsBadBody: the DELETE ignores status (the undo is
// coordinate-only) but still rejects an unknown field via the shared strict decoder and requires
// non-empty items. All cases reject before the service.
func TestAdminUndoJudgeDispositionRejectsBadBody(t *testing.T) {
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty items", `{"items":[]}`, http.StatusBadRequest},
		{"a scope field is rejected by the strict decoder", `{"items":[{"category":"improve_uzi","target":"docs"}],"scope":"all"}`, http.StatusBadRequest},
		{"malformed JSON", `{`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{}
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			h.AdminUndoJudgeDisposition(rec, adminJudgeWriteReq(admin, http.MethodDelete, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("DELETE %s = %d, want %d; body=%s", tc.body, rec.Code, tc.want, rec.Body.String())
			}
			if len(st.calls) != 0 {
				t.Fatalf("a rejected DELETE must not reach the store, calls=%v", st.calls)
			}
		})
	}
}

// TestAdminJudgeWriteHandlersUnauthenticatedIs401: no user in context is a 401 before the body is
// even decoded, and the store is never touched.
func TestAdminJudgeWriteHandlersUnauthenticatedIs401(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request)
		verb string
	}{
		{"set", func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.AdminSetJudgeDisposition(rec, req)
		}, http.MethodPut},
		{"undo", func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.AdminUndoJudgeDisposition(rec, req)
		}, http.MethodDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{}
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.verb, "/api/admin/judge/recommendations/disposition",
				strings.NewReader(`{"items":[{"category":"improve_uzi","target":"docs"}],"status":"done"}`))
			tc.call(h, rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated %s = %d, want 401", tc.verb, rec.Code)
			}
			if len(st.calls) != 0 {
				t.Fatalf("an unauthenticated write must not reach the store, calls=%v", st.calls)
			}
		})
	}
}

// TestAdminJudgeWriteRejectsTooManyItems pins the amplification guard on BOTH write verbs: more
// than JudgeDispositionMaxItems distinct coordinates in one body is a 400 (workersvc.ErrTooManyItems),
// and the reject happens in the service's dedupe-then-cap BEFORE any store query runs — so the nil
// embedded Store is never reached (no resolve, no fan-out). This is the bound that keeps one request
// from resolving an unbounded cross-user member set; without it a caller could drive the resolve
// with an arbitrarily long coordinate list.
func TestAdminJudgeWriteRejectsTooManyItems(t *testing.T) {
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	items := make([]apitypes.JudgeDispositionCoordDTO, 0, workersvc.JudgeDispositionMaxItems+1)
	for i := 0; i <= workersvc.JudgeDispositionMaxItems; i++ { // MaxItems+1 DISTINCT coordinates
		items = append(items, apitypes.JudgeDispositionCoordDTO{Category: "improve_uzi", Target: fmt.Sprintf("t%d", i)})
	}
	body, err := json.Marshal(apitypes.JudgeAdminDispositionRequest{Items: items, Status: "done"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	for _, tc := range []struct {
		name string
		verb string
		call func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request)
	}{
		{"set", http.MethodPut, func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.AdminSetJudgeDisposition(rec, req)
		}},
		{"undo", http.MethodDelete, func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.AdminUndoJudgeDisposition(rec, req)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &adminBacklogStore{} // nil Store: a fan-out resolve would panic, proving the cap fires first
			h := newRunsHandler(t, st)
			rec := httptest.NewRecorder()
			tc.call(h, rec, adminJudgeWriteReq(admin, tc.verb, string(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s with %d items = %d, want 400; body=%s", tc.verb, len(items), rec.Code, rec.Body.String())
			}
			if len(st.calls) != 0 {
				t.Fatalf("an over-cap %s must reject before any store query, calls=%v", tc.verb, st.calls)
			}
		})
	}
}

// TestAdminJudgeReadsAreReadOnly: across parameter combinations the admin backlog touches only
// its two all-users reads and nothing else. The embedded nil workersvc.Store makes any other
// call panic; here the call set is asserted positively.
func TestAdminJudgeReadsAreReadOnly(t *testing.T) {
	rows, _, _, _, _ := adminFixture()
	st := &adminBacklogStore{backlogRows: rows, triageRows: adminTriageFixture()}
	h := newRunsHandler(t, st)
	admin := store.User{ID: uuid.New(), IsAdmin: true}

	for _, q := range []string{"", "bucket=all", "bucket=dismissed", "category=improve_uzi"} {
		h.AdminJudgeRecommendations(httptest.NewRecorder(), adminJudgeReq(admin, "/api/admin/judge/recommendations", q))
	}
	allowed := map[string]bool{
		"ListJudgeRecommendationRowsAll": true,
		"ListJudgeTriageRowsAll":         true,
	}
	for _, c := range st.calls {
		if !allowed[c] {
			t.Fatalf("the admin backlog read called %q — it must touch nothing but its two reads; calls=%v", c, st.calls)
		}
	}
}
