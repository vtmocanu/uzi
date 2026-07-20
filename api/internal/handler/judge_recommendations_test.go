package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// backlogStore serves the PRD #98 M1 grouped read AND the PRD #94 stats aggregate from
// ONE fixture: ListJudgeTriageRowsForUser is DERIVED from the same wide rows, exactly as
// the two SQL queries derive from the same join spine. That is what makes
// TestJudgeBacklogTotalsEqualStats a real proof of the shared BucketOf ladder rather than
// a comparison of two hand-written fixtures that happen to agree.
//
// It embeds workersvc.Store (nil), so ANY method the read path does not use — every
// enqueue/forge/run-create method — panics if reached: the structural "no token spend, no
// forge write" proof.
type backlogStore struct {
	workersvc.Store

	rows []store.ListJudgeRecommendationRowsForUserRow

	backlogUserArg uuid.UUID
	statsUserArg   uuid.UUID
	calls          []string
}

func (s *backlogStore) ListJudgeRecommendationRowsForUser(_ context.Context, userID uuid.UUID) ([]store.ListJudgeRecommendationRowsForUserRow, error) {
	s.calls = append(s.calls, "ListJudgeRecommendationRowsForUser")
	s.backlogUserArg = userID
	return s.rows, nil
}

// ListJudgeTriageRowsForUser projects the wide rows onto the narrow three-column shape —
// the same projection the #94 query's SELECT list performs on the same join.
func (s *backlogStore) ListJudgeTriageRowsForUser(_ context.Context, userID uuid.UUID) ([]store.ListJudgeTriageRowsForUserRow, error) {
	s.calls = append(s.calls, "ListJudgeTriageRowsForUser")
	s.statsUserArg = userID
	out := make([]store.ListJudgeTriageRowsForUserRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, store.ListJudgeTriageRowsForUserRow{
			DispositionStatus: r.DispositionStatus,
			DismissReason:     r.DismissReason,
			FiledSettled:      r.FiledSettled,
		})
	}
	return out, nil
}

// backlogFixtureRows is one owner's backlog: the SAME (install_worker_tool, rg)
// recommendation in two runs (the dedup case), plus a done and a settled-filed coordinate
// so every rung of the ladder is represented.
func backlogFixtureRows() ([]store.ListJudgeRecommendationRowsForUserRow, uuid.UUID) {
	txt := func(s string) pgtype.Text {
		if s == "" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	filedAt := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	runNew, runOld, runDone, runFiled := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rows := []store.ListJudgeRecommendationRowsForUserRow{
		{ReviewID: uuid.New(), RunID: runNew, Verdict: "issues", RunTitle: "newest run", RecID: uuid.New(),
			Category: "install_worker_tool", Target: "rg", RationaleMd: "newest rationale", Confidence: "high"},
		{ReviewID: uuid.New(), RunID: runOld, Verdict: "ok", RunTitle: "older run", RecID: uuid.New(),
			Category: "install_worker_tool", Target: "rg", RationaleMd: "older rationale", Confidence: "low",
			DispositionStatus: txt("dismissed"), DismissReason: txt("not_an_issue")},
		{ReviewID: uuid.New(), RunID: runDone, Verdict: "ok", RunTitle: "done run", RecID: uuid.New(),
			Category: "improve_uzi", Target: "docs", DispositionStatus: txt("done")},
		{ReviewID: uuid.New(), RunID: runFiled, Verdict: "issues", RunTitle: "filed run", RecID: uuid.New(),
			Category: "improve_agent", Target: "coder", FiledSettled: true,
			FiledIssueIid: pgtype.Int8{Int64: 11, Valid: true}, FiledIssueUrl: txt("https://forge/11"),
			FiledAt: pgtype.Timestamptz{Time: filedAt, Valid: true}},
	}
	return rows, runNew
}

// backlogReq builds a GET /api/me/judge/recommendations authenticated as user, with the
// given raw query string (e.g. "bucket=all&run=…").
func backlogReq(user store.User, query string) *http.Request {
	url := "/api/me/judge/recommendations"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), user))
}

func decodeBacklog(t *testing.T, rec *httptest.ResponseRecorder) apitypes.JudgeBacklogDTO {
	t.Helper()
	var got apitypes.JudgeBacklogDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return got
}

// ---- dedup through the handler (the M1 gating test) ---------------------------------

// TestJudgeRecommendationsDedupsAcrossRuns: the same (category, target) in two runs comes
// back as ONE group over the wire, with both runs in its occurrence list, run_count 2,
// open_count 1 (only the undisposed member is open) and the most-recent rationale as the
// preview (PRD #98 Decisions 1/2).
func TestJudgeRecommendationsDedupsAcrossRuns(t *testing.T) {
	rows, newestRun := backlogFixtureRows()
	st := &backlogStore{rows: rows}
	caller := store.User{ID: uuid.New()}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeRecommendations(rec, backlogReq(caller, "bucket=all"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBacklog(t, rec)
	if st.backlogUserArg != caller.ID {
		t.Fatalf("backlog query scoped to %v, want the caller %v (owner-scoped)", st.backlogUserArg, caller.ID)
	}
	if len(got.Groups) != 3 {
		t.Fatalf("want 3 groups (the two rg rows dedup into one), got %d: %+v", len(got.Groups), got.Groups)
	}
	var rg apitypes.JudgeRecommendationGroupDTO
	for _, g := range got.Groups {
		if g.Category == "install_worker_tool" && g.Target == "rg" {
			rg = g
		}
	}
	if rg.RunCount != 2 || rg.OpenCount != 1 || rg.Bucket != "todo" {
		t.Fatalf("rg group = run_count %d / open_count %d / bucket %q, want 2/1/todo", rg.RunCount, rg.OpenCount, rg.Bucket)
	}
	if len(rg.Occurrences) != 2 {
		t.Fatalf("want 2 occurrences, got %+v", rg.Occurrences)
	}
	if rg.Occurrences[0].RunID != newestRun.String() || rg.Occurrences[0].RunTitle != "newest run" {
		t.Errorf("occurrence[0] = %+v, want the most-recent run first", rg.Occurrences[0])
	}
	if rg.Occurrences[0].Bucket != "todo" || rg.Occurrences[1].Bucket != "dismissed" {
		t.Errorf("per-run triage state must survive: %+v", rg.Occurrences)
	}
	if rg.RationalePreview != "newest rationale" {
		t.Errorf("rationale_preview = %q, want the most-recent occurrence's rationale", rg.RationalePreview)
	}
	// The settled filed link rides its occurrence.
	for _, g := range got.Groups {
		if g.Target != "coder" {
			continue
		}
		ref := g.Occurrences[0].FiledIssue
		if ref == nil || ref.IssueIID != 11 || ref.IssueURL != "https://forge/11" {
			t.Errorf("filed occurrence = %+v, want the settled issue link", ref)
		}
	}
}

// ---- the shared-ladder proof --------------------------------------------------------

// TestJudgeBacklogTotalsEqualStats is the M1 no-re-implementation gate: for the SAME
// fixture, the backlog's triage tally is byte-identical to what GET /me/judge/stats
// returns, and the per-group open_count sums to triage.todo. Both numbers come from the
// one shared workersvc.BucketOf ladder (#94 Decision 2) — if anyone re-implemented the
// bucketing in SQL or in the grouper, these would drift and this test would fail.
func TestJudgeBacklogTotalsEqualStats(t *testing.T) {
	rows, _ := backlogFixtureRows()
	st := &backlogStore{rows: rows}
	caller := store.User{ID: uuid.New()}
	h := newRunsHandler(t, st)

	statsRec := httptest.NewRecorder()
	statsReq := httptest.NewRequest(http.MethodGet, "/api/me/judge/stats", nil)
	h.JudgeStats(statsRec, statsReq.WithContext(mw.ContextWithUser(statsReq.Context(), caller)))
	if statsRec.Code != http.StatusOK {
		t.Fatalf("JudgeStats = %d, want 200; body=%s", statsRec.Code, statsRec.Body.String())
	}
	var stats apitypes.TriageDTO
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}

	backlogRec := httptest.NewRecorder()
	h.JudgeRecommendations(backlogRec, backlogReq(caller, "bucket=all"))
	if backlogRec.Code != http.StatusOK {
		t.Fatalf("backlog = %d, want 200", backlogRec.Code)
	}
	got := decodeBacklog(t, backlogRec)

	if got.Triage != stats {
		t.Fatalf("backlog triage = %+v, want the /me/judge/stats aggregate %+v (one shared ladder)", got.Triage, stats)
	}
	// Sanity: the fixture actually spans the ladder, so the equality above is not trivially
	// two zero structs.
	want := apitypes.TriageDTO{Total: 4, Todo: 1, Filed: 1, Done: 1, Dismissed: 1, FalsePositives: 1}
	if stats != want {
		t.Fatalf("fixture aggregate = %+v, want %+v", stats, want)
	}
	// The per-group open_counts are the SAME recommendations the strip counts as todo.
	sumOpen := 0
	for _, g := range got.Groups {
		sumOpen += g.OpenCount
	}
	if sumOpen != stats.Todo {
		t.Fatalf("sum(open_count) = %d, want triage.todo = %d — the group counts must be the strip's, regrouped", sumOpen, stats.Todo)
	}
}

// ---- query-parameter contract -------------------------------------------------------

// TestJudgeRecommendationsDefaultBucketIsTodo: no ?bucket= means the backlog's reason to
// exist — what still needs triage. The done/filed groups are filtered out; the echoed
// bucket names the applied filter so a consumer never has to infer it.
func TestJudgeRecommendationsDefaultBucketIsTodo(t *testing.T) {
	rows, _ := backlogFixtureRows()
	h := newRunsHandler(t, &backlogStore{rows: rows})

	rec := httptest.NewRecorder()
	h.JudgeRecommendations(rec, backlogReq(store.User{ID: uuid.New()}, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBacklog(t, rec)
	if got.Bucket != "todo" {
		t.Errorf("echoed bucket = %q, want todo (the default)", got.Bucket)
	}
	if len(got.Groups) != 1 || got.Groups[0].Target != "rg" {
		t.Fatalf("default view = %+v, want only the group with an open member", got.Groups)
	}
}

// TestJudgeRecommendationsRejectsBadParams: an unknown bucket or an unparseable run id is
// a 400, never a silently-ignored filter — a typo in a CLI flag must not look like an
// empty backlog. A well-formed but unknown run id is NOT an error: it matches nothing, so
// the endpoint leaks no ownership/existence oracle.
func TestJudgeRecommendationsRejectsBadParams(t *testing.T) {
	rows, _ := backlogFixtureRows()
	caller := store.User{ID: uuid.New()}

	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"unknown bucket", "bucket=open", http.StatusBadRequest},
		{"empty-but-present bucket falls back to the default", "bucket=", http.StatusOK},
		{"unparseable run", "run=not-a-uuid", http.StatusBadRequest},
		{"unknown run is simply empty", "run=" + uuid.New().String(), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunsHandler(t, &backlogStore{rows: rows})
			rec := httptest.NewRecorder()
			h.JudgeRecommendations(rec, backlogReq(caller, tc.query))
			if rec.Code != tc.want {
				t.Fatalf("GET ?%s = %d, want %d; body=%s", tc.query, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestJudgeRecommendationsUnauthenticatedIs401: no user in context (the RequireUser
// middleware absent) is a 401 and never reaches the store.
func TestJudgeRecommendationsUnauthenticatedIs401(t *testing.T) {
	st := &backlogStore{}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.JudgeRecommendations(rec, httptest.NewRequest(http.MethodGet, "/api/me/judge/recommendations", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d, want 401", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Fatalf("an unauthenticated request must not reach the store, calls=%v", st.calls)
	}
}

// ---- no spend, no forge write -------------------------------------------------------

// TestJudgeRecommendationsReadsOnly proves the endpoint is a pure read: across every
// parameter combination it calls exactly ONE store method — the backlog query. The
// embedded nil workersvc.Store means any other call would additionally panic; here the
// call set is asserted positively.
func TestJudgeRecommendationsReadsOnly(t *testing.T) {
	rows, anchor := backlogFixtureRows()
	st := &backlogStore{rows: rows}
	caller := store.User{ID: uuid.New()}
	h := newRunsHandler(t, st)

	for _, q := range []string{"", "bucket=all", "bucket=dismissed", "run=" + anchor.String()} {
		h.JudgeRecommendations(httptest.NewRecorder(), backlogReq(caller, q))
	}
	for _, c := range st.calls {
		if c != "ListJudgeRecommendationRowsForUser" {
			t.Fatalf("the backlog read called %q — it must touch nothing but its own query (no spend, no forge write); calls=%v", c, st.calls)
		}
	}
	if len(st.calls) != 4 {
		t.Fatalf("want one query per request, got %v", st.calls)
	}
}
