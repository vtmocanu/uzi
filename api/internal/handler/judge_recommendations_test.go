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

	backlogArg   *store.ListJudgeRecommendationRowsForUserParams
	statsUserArg uuid.UUID
	calls        []string
}

func (s *backlogStore) ListJudgeRecommendationRowsForUser(_ context.Context, arg store.ListJudgeRecommendationRowsForUserParams) ([]store.ListJudgeRecommendationRowsForUserRow, error) {
	s.calls = append(s.calls, "ListJudgeRecommendationRowsForUser")
	s.backlogArg = &arg
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
	if st.backlogArg == nil || st.backlogArg.UserID != caller.ID {
		t.Fatalf("backlog query args = %+v, want it scoped to the caller %v (owner-scoped)", st.backlogArg, caller.ID)
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

// TestJudgeBacklogTotalsEqualStats makes two assertions that are easy to conflate, and they
// are NOT equally strong. Stated precisely, because this comment used to claim that both
// would catch a re-implemented ladder and that is false. Each claim below was verified by
// mutation against THIS code, not inferred:
//
//   - `sumOpen == triage.todo` is the LADDER-SHARING GATE — the line that actually bites.
//     sum(open_count) is produced by the grouper (which buckets every occurrence through
//     BucketOf) while triage.todo comes from BucketTriage. Giving the grouper a divergent
//     local ladder fails exactly here: "sum(open_count) = 2, want triage.todo = 1".
//   - `got.Triage == stats` is WEAK, and weaker than it looks. Triage no longer passes
//     through the grouper at all — it is read from the #94 stats query — so a wrong ladder
//     in the grouper cannot move it. And because this test's fake derives its stats rows
//     from the same fixture, the line is close to a tautology here: making the backlog
//     tally triage off its own page rows (the pre-truncation shape, which a LIMIT makes
//     wrong) does NOT fail this test. What catches that regression is
//     TestJudgeRecommendationBacklogTruncates in internal/workersvc, which fails with
//     "triage.total = 2000, want 2025 — the tally must not follow the truncated page".
//     Keep this line for the shape it pins, not for a guarantee it does not provide.
//
// Neither line exercises SQL: the store is faked here. The query's own guarantees are
// pinned in internal/store/judge_recommendations_integration_test.go against real Postgres.
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
// parameter combination it calls exactly TWO store methods — the backlog query and the
// #94 stats query the canonical triage tally comes from. Nothing else. The embedded nil
// workersvc.Store means any other call would additionally panic; here the call set is
// asserted positively.
func TestJudgeRecommendationsReadsOnly(t *testing.T) {
	rows, anchor := backlogFixtureRows()
	st := &backlogStore{rows: rows}
	caller := store.User{ID: uuid.New()}
	h := newRunsHandler(t, st)

	queries := []string{"", "bucket=all", "bucket=dismissed", "run=" + anchor.String()}
	for _, q := range queries {
		h.JudgeRecommendations(httptest.NewRecorder(), backlogReq(caller, q))
	}
	allowed := map[string]bool{
		"ListJudgeRecommendationRowsForUser": true, // the page
		"ListJudgeTriageRowsForUser":         true, // the canonical tally
	}
	for _, c := range st.calls {
		if !allowed[c] {
			t.Fatalf("the backlog read called %q — it must touch nothing but its two reads (no spend, no forge write); calls=%v", c, st.calls)
		}
	}
	if len(st.calls) != 2*len(queries) {
		t.Fatalf("want exactly the two reads per request, got %v", st.calls)
	}
}

// TestJudgeRecommendationsPushesRunAnchorToTheQuery: the ?run= anchor reaches the QUERY as
// a parameter (it is filtered inside the owner-scoped WHERE, not in Go afterwards), and an
// absent anchor is sent as a SQL NULL rather than the all-zero uuid — which would filter
// every unanchored request down to nothing.
func TestJudgeRecommendationsPushesRunAnchorToTheQuery(t *testing.T) {
	rows, anchor := backlogFixtureRows()
	caller := store.User{ID: uuid.New()}

	st := &backlogStore{rows: rows}
	h := newRunsHandler(t, st)
	h.JudgeRecommendations(httptest.NewRecorder(), backlogReq(caller, "run="+anchor.String()))
	if st.backlogArg == nil || !st.backlogArg.RunAnchor.Valid || uuid.UUID(st.backlogArg.RunAnchor.Bytes) != anchor {
		t.Fatalf("run anchor param = %+v, want %v pushed into the query", st.backlogArg, anchor)
	}

	st = &backlogStore{rows: rows}
	h = newRunsHandler(t, st)
	h.JudgeRecommendations(httptest.NewRecorder(), backlogReq(caller, ""))
	if st.backlogArg == nil || st.backlogArg.RunAnchor.Valid {
		t.Fatalf("no ?run= must send a NULL anchor, got %+v", st.backlogArg)
	}
}

// TestJudgeRecommendationsReportsTruncation: the response carries the hard-cap flag so the
// SPA and the CLI can say "showing the most recent N" instead of silently presenting a cut
// backlog as complete.
func TestJudgeRecommendationsReportsTruncation(t *testing.T) {
	caller := store.User{ID: uuid.New()}
	over := make([]store.ListJudgeRecommendationRowsForUserRow, 0, workersvc.JudgeBacklogMaxRows+1)
	for i := 0; i <= workersvc.JudgeBacklogMaxRows; i++ {
		over = append(over, store.ListJudgeRecommendationRowsForUserRow{
			ReviewID: uuid.New(), RunID: uuid.New(), Verdict: "ok", RecID: uuid.New(),
			Category: "improve_uzi", Target: uuid.NewString(),
		})
	}
	h := newRunsHandler(t, &backlogStore{rows: over})
	rec := httptest.NewRecorder()
	h.JudgeRecommendations(rec, backlogReq(caller, "bucket=all"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if got := decodeBacklog(t, rec); !got.Truncated || len(got.Groups) != workersvc.JudgeBacklogMaxRows {
		t.Fatalf("truncated=%v groups=%d, want true / %d", got.Truncated, len(got.Groups), workersvc.JudgeBacklogMaxRows)
	}

	// The un-truncated fixture must NOT set the flag.
	rows, _ := backlogFixtureRows()
	h = newRunsHandler(t, &backlogStore{rows: rows})
	rec = httptest.NewRecorder()
	h.JudgeRecommendations(rec, backlogReq(caller, "bucket=all"))
	if decodeBacklog(t, rec).Truncated {
		t.Fatal("a small backlog must not be flagged truncated")
	}
}
