package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2661: GET /api/runs and /api/admin/runs accept ?view=summary, which drops the four
// heavy detail keys from every row and changes nothing else.

var summaryHeavyKeys = []string{"plan_md", "repo_agents", "issue_description", "preserved_patch"}

// heavyRun is a store.Run with all four heavy fields populated, so the default view really
// carries them and a leaked key in the summary view is detectable.
func heavyRun(ownerID uuid.UUID) store.Run {
	return store.Run{
		ID:               uuid.New(),
		UserID:           ownerID,
		Status:           "running",
		Kind:             "issue",
		IssueDescription: "the long issue description",
		PlanMd:           pgtype.Text{String: "# the long plan", Valid: true},
		PreservedPatch:   pgtype.Text{String: "diff --git a/x b/x", Valid: true},
		RepoAgents:       []byte(`[{"name":"coder","description":"writes code"}]`),
	}
}

type listBody struct {
	Runs []map[string]json.RawMessage `json:"runs"`
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) listBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var b listBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return b
}

func ids(t *testing.T, b listBody) []string {
	t.Helper()
	out := make([]string, 0, len(b.Runs))
	for _, row := range b.Runs {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatalf("row id: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func rowKeys(row map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func has(row map[string]json.RawMessage, k string) bool {
	_, ok := row[k]
	return ok
}

func assertRowsCarryHeavy(t *testing.T, view string, b listBody) {
	t.Helper()
	for i, row := range b.Runs {
		for _, k := range summaryHeavyKeys {
			if !has(row, k) {
				t.Errorf("%s view row %d lacks %q; want the full legacy row", view, i, k)
			}
		}
		if string(row["plan_md"]) == "null" || string(row["repo_agents"]) == "null" {
			t.Errorf("%s view row %d: plan_md/repo_agents are null, fixture did not populate them", view, i)
		}
	}
}

func assertRowsLackHeavy(t *testing.T, view string, b listBody) {
	t.Helper()
	for i, row := range b.Runs {
		for _, k := range summaryHeavyKeys {
			if has(row, k) {
				t.Errorf("%s view row %d still carries %q (%s); want it omitted", view, i, k, row[k])
			}
		}
	}
}

// assertSummaryIsFullMinusHeavy: the summary row's key set is the full row's minus the four
// heavy keys, and the shared values are byte-identical.
func assertSummaryIsFullMinusHeavy(t *testing.T, full, summary listBody) {
	t.Helper()
	if len(full.Runs) != len(summary.Runs) {
		t.Fatalf("row counts differ: full %d, summary %d", len(full.Runs), len(summary.Runs))
	}
	heavy := map[string]bool{}
	for _, k := range summaryHeavyKeys {
		heavy[k] = true
	}
	for i := range full.Runs {
		want := map[string]json.RawMessage{}
		for k, v := range full.Runs[i] {
			if !heavy[k] {
				want[k] = v
			}
		}
		got := summary.Runs[i]
		if len(want) != len(got) {
			t.Errorf("row %d key sets differ\n full-minus-heavy: %v\n summary: %v", i, rowKeys(want), rowKeys(got))
			continue
		}
		for k, v := range want {
			if string(got[k]) != string(v) {
				t.Errorf("row %d key %q differs: full %s, summary %s", i, k, v, got[k])
			}
		}
	}
}

func ownerListStore() (*runsStore, store.User) {
	ownerID := uuid.New()
	st := &runsStore{ownerID: ownerID}
	for range 3 {
		st.userRuns = append(st.userRuns, store.ListRunsForUserRow{
			Run:      heavyRun(ownerID),
			RepoPath: pgtype.Text{String: "g/r", Valid: true},
		})
	}
	return st, store.User{ID: ownerID}
}

func getOwnerList(t *testing.T, h *Handler, user store.User, target string) listBody {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	return decodeList(t, rec)
}

func TestListRunsSummaryViewOmitsHeavyKeys(t *testing.T) {
	st, user := ownerListStore()
	h := newRunsHandler(t, st)

	full := getOwnerList(t, h, user, "/api/runs")
	summary := getOwnerList(t, h, user, "/api/runs?view=summary")

	assertRowsCarryHeavy(t, "default", full)
	assertRowsLackHeavy(t, "summary", summary)
	assertSummaryIsFullMinusHeavy(t, full, summary)
	if got, want := ids(t, summary), ids(t, full); len(got) != 3 || !equalStrings(got, want) {
		t.Fatalf("summary ids/order = %v, want the default view's %v", got, want)
	}
}

func TestListRunsSummaryViewUnknownValueIsLegacy(t *testing.T) {
	st, user := ownerListStore()
	h := newRunsHandler(t, st)

	for _, target := range []string{"/api/runs?view=bogus", "/api/runs?view=", "/api/runs?view=Summary"} {
		b := getOwnerList(t, h, user, target)
		assertRowsCarryHeavy(t, target, b)
	}
}

func TestListRunsSummaryViewThreadsFiltersIdentically(t *testing.T) {
	st, user := ownerListStore()
	h := newRunsHandler(t, st)
	repoID := uuid.New()
	q := "?repo_id=" + repoID.String() + "&issue_iid=42"

	full := getOwnerList(t, h, user, "/api/runs"+q)
	fullArg := *st.lastRunsArg
	st.lastRunsArg = nil
	summary := getOwnerList(t, h, user, "/api/runs"+q+"&view=summary")
	sumArg := st.lastRunsArg
	if sumArg == nil {
		t.Fatal("summary view did not reach ListRunsForUser")
	}
	if sumArg.UserID != fullArg.UserID || sumArg.RepoID != fullArg.RepoID || sumArg.IssueIid != fullArg.IssueIid {
		t.Fatalf("filters differ between views:\n full:    %+v\n summary: %+v", fullArg, *sumArg)
	}
	if !sumArg.RepoID.Valid || uuid.UUID(sumArg.RepoID.Bytes) != repoID || sumArg.IssueIid.Int64 != 42 {
		t.Fatalf("repo_id/issue_iid not threaded in summary view: %+v", *sumArg)
	}
	assertRowsLackHeavy(t, "summary", summary)
	assertSummaryIsFullMinusHeavy(t, full, summary)
}

func TestListRunsSummaryViewKeepsErrorContract(t *testing.T) {
	st, user := ownerListStore()
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs?view=summary&repo_id=not-a-uuid", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed repo_id with view=summary = %d, want 400", rec.Code)
	}
}

func TestAdminListRunsSummaryViewOmitsHeavyKeys(t *testing.T) {
	st := &runsStore{}
	for range 3 {
		st.activeRuns = append(st.activeRuns, store.ListActiveRunsAllRow{
			Run:        heavyRun(uuid.New()),
			RepoPath:   pgtype.Text{String: "g/r", Valid: true},
			OwnerEmail: "owner@example.test",
		})
	}
	h := newRunsHandler(t, st)
	get := func(target string) listBody {
		rec := httptest.NewRecorder()
		h.AdminListRuns(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return decodeList(t, rec)
	}

	full := get("/api/admin/runs")
	summary := get("/api/admin/runs?view=summary")
	bogus := get("/api/admin/runs?view=bogus")

	assertRowsCarryHeavy(t, "default", full)
	assertRowsCarryHeavy(t, "bogus", bogus)
	assertRowsLackHeavy(t, "summary", summary)
	assertSummaryIsFullMinusHeavy(t, full, summary)
	if !equalStrings(ids(t, summary), ids(t, full)) || len(summary.Runs) != 3 {
		t.Fatalf("summary ids/order = %v, want the default view's %v", ids(t, summary), ids(t, full))
	}
	for i, row := range summary.Runs {
		if string(row["owner_email"]) != `"owner@example.test"` {
			t.Errorf("summary row %d owner_email = %s, want it kept", i, row["owner_email"])
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
