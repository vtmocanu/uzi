package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// stateMappingFixture is the linked repo of newLinkedHandlerFixture (columns "In Progress" and
// "Human Review") with the issue tracker service wired into the Handler and a few States
// seeded in the YouTrack fake beside its default Open, Fixed and Won't fix.
func newStateMappingFixture(t *testing.T) *linkedHandlerFixture {
	t.Helper()
	f := newLinkedHandlerFixture(t, true)
	f.h.SetIssueTrackerService(f.itSvc.UseTransactions(f.pool, func(tx pgx.Tx) issuetrackersvc.Querier { return f.h.q.WithTx(tx) }))
	f.srv.SeedState("Doing", false)
	f.srv.SeedState("In Review", false)
	f.srv.SeedState("Shipped", true)
	return f
}

// callAs drives a handler as user over the repo of the fixture.
func (f *linkedHandlerFixture) callAs(user store.User, handler http.HandlerFunc, method string, body any) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "/x", rdr)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.repo.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	handler(rr, r)
	return rr
}

func (f *linkedHandlerFixture) putStates(body any) *httptest.ResponseRecorder {
	return f.callAs(f.user, f.h.PutIssueTrackerStateMapping, http.MethodPut, body)
}

func (f *linkedHandlerFixture) getStates(t *testing.T) (mapping map[string]any, states []map[string]any) {
	t.Helper()
	rr := f.callAs(f.user, f.h.GetIssueTrackerStateMapping, http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET states = %d\n%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Mapping map[string]any   `json:"mapping"`
		States  []map[string]any `json:"states"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Mapping, out.States
}

func mappingBody(open, closed string, cols ...[2]string) map[string]any {
	cs := make([]map[string]string, 0, len(cols))
	for _, c := range cols {
		cs = append(cs, map[string]string{"label_name": c[0], "state": c[1]})
	}
	return map[string]any{"open_state": open, "closed_state": closed, "columns": cs}
}

func TestIssueTrackerStateMappingRouteLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)

	if m, _ := f.getStates(t); m != nil {
		t.Fatalf("mapping before a save = %v, want null", m)
	}
	rr := f.putStates(mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"}, [2]string{"Human Review", "In Review"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d\n%s", rr.Code, rr.Body.String())
	}
	var saved struct {
		Mapping struct {
			OpenState   string `json:"open_state"`
			ClosedState string `json:"closed_state"`
			Columns     []struct{ LabelName, State string }
		} `json:"mapping"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Mapping.OpenState != "Open" || saved.Mapping.ClosedState != "Fixed" || len(saved.Mapping.Columns) != 2 {
		t.Fatalf("PUT body = %s", rr.Body.String())
	}

	// Stored: the rows hold what was saved, and a GET reads it back.
	var open, closed string
	if err := f.pool.QueryRow(context.Background(), `SELECT open_state, closed_state FROM repo_issue_tracker_states WHERE repo_id = $1`, f.repo).Scan(&open, &closed); err != nil || open != "Open" || closed != "Fixed" {
		t.Fatalf("stored pair = %q %q (%v)", open, closed, err)
	}
	var state string
	if err := f.pool.QueryRow(context.Background(), `SELECT state_name FROM repo_issue_tracker_column_states WHERE repo_id = $1 AND label_name = 'In Progress'`, f.repo).Scan(&state); err != nil || state != "Doing" {
		t.Fatalf("stored column state = %q (%v)", state, err)
	}
	m, states := f.getStates(t)
	if m["open_state"] != "Open" || m["closed_state"] != "Fixed" || len(m["columns"].([]any)) != 2 {
		t.Fatalf("GET mapping = %v", m)
	}

	// The States carry what uzi may do with each.
	by := map[string]map[string]any{}
	for _, s := range states {
		by[s["name"].(string)] = s
	}
	for name, want := range map[string][2]bool{ // closable, openable
		"Open": {false, true}, "Doing": {false, true}, "Fixed": {true, false}, "Shipped": {true, false}, "Won't fix": {false, false},
	} {
		s := by[name]
		if s == nil || s["closable"] != want[0] || s["openable"] != want[1] {
			t.Errorf("state %q = %v, want closable %v openable %v", name, s, want[0], want[1])
		}
	}
	if by["Won't fix"]["resolved"] != true {
		t.Errorf("Won't fix must read as resolved: %v", by["Won't fix"])
	}

	// A second PUT replaces the mapping, columns included.
	if rr := f.putStates(mappingBody("Doing", "Shipped", [2]string{"Human Review", "In Review"})); rr.Code != http.StatusOK {
		t.Fatalf("second PUT = %d\n%s", rr.Code, rr.Body.String())
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM repo_issue_tracker_column_states WHERE repo_id = $1`, f.repo).Scan(&n); err != nil || n != 1 {
		t.Fatalf("column rows after the second PUT = %d (%v), want 1", n, err)
	}
}

func TestIssueTrackerStateMappingRouteRefusalsLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	cases := []struct {
		name string
		body any
		code int
		msg  string
	}{
		{"unknown state", mappingBody("Nope", "Fixed"), http.StatusBadRequest, "Nope"},
		{"resolved column state", mappingBody("Open", "Fixed", [2]string{"In Progress", "Shipped"}), http.StatusBadRequest, "column state"},
		{"won't fix closed", mappingBody("Open", "Won't fix"), http.StatusBadRequest, "Won't fix"},
		{"unresolved closed", mappingBody("Open", "Doing"), http.StatusBadRequest, "closed state"},
		{"label that is not a column", mappingBody("Open", "Fixed", [2]string{"Backlog", "Doing"}), http.StatusBadRequest, "not a board column"},
		{"duplicate states", mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"}, [2]string{"Human Review", "Doing"}), http.StatusBadRequest, "more than once"},
		{"column state equals open", mappingBody("Open", "Fixed", [2]string{"In Progress", "Open"}), http.StatusBadRequest, "more than once"},
		{"duplicate label", mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"}, [2]string{"In Progress", "In Review"}), http.StatusBadRequest, "more than once"},
		{"open equals closed", mappingBody("Fixed", "Fixed"), http.StatusBadRequest, "different"},
		{"control character", mappingBody("Op\ten", "Fixed"), http.StatusBadRequest, "control"},
		{"overlong name", mappingBody(strings.Repeat("x", 201), "Fixed"), http.StatusBadRequest, "200"},
		{"not json", "x", http.StatusBadRequest, "invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.putStates(tc.body)
			if rr.Code != tc.code || !strings.Contains(rr.Body.String(), tc.msg) {
				t.Fatalf("PUT = %d %s, want %d containing %q", rr.Code, rr.Body.String(), tc.code, tc.msg)
			}
		})
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM repo_issue_tracker_states WHERE repo_id = $1`, f.repo).Scan(&n); err != nil || n != 0 {
		t.Fatalf("states rows after every refusal = %d (%v), want 0", n, err)
	}

	// A foreign user gets 404 on both verbs; a repo that is not linked gets 409.
	stranger := store.User{ID: uuid.New()}
	if rr := f.callAs(stranger, f.h.PutIssueTrackerStateMapping, http.MethodPut, mappingBody("Open", "Fixed")); rr.Code != http.StatusNotFound {
		t.Errorf("PUT as a stranger = %d, want 404\n%s", rr.Code, rr.Body.String())
	}
	if rr := f.callAs(stranger, f.h.GetIssueTrackerStateMapping, http.MethodGet, nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET as a stranger = %d, want 404\n%s", rr.Code, rr.Body.String())
	}
	plain := *f
	plain.repo = uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      SELECT $1::uuid, connection_id, 78, 'g/plain-' || $1::text, 'https://forge.test/g/p', 'main', true FROM repos WHERE id = $2`, plain.repo, f.repo); err != nil {
		t.Fatal(err)
	}
	if rr := plain.putStates(mappingBody("Open", "Fixed")); rr.Code != http.StatusConflict {
		t.Errorf("PUT on an unlinked repo = %d, want 409\n%s", rr.Code, rr.Body.String())
	}
	if rr := plain.callAs(plain.user, plain.h.GetIssueTrackerStateMapping, http.MethodGet, nil); rr.Code != http.StatusConflict {
		t.Errorf("GET on an unlinked repo = %d, want 409\n%s", rr.Code, rr.Body.String())
	}
}

// ConfigureColumns on a linked repo writes the board_columns rows, prunes the mapping rows of
// the columns that are gone, and sends nothing to the issue tracker or the forge.
func TestConfigureColumnsOnALinkedRepoPrunesTheMappingLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	if rr := f.putStates(mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"}, [2]string{"Human Review", "In Review"})); rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d\n%s", rr.Code, rr.Body.String())
	}
	f.srv.ResetRequests()

	rr := f.call(f.h.ConfigureColumns, http.MethodPut, map[string]any{"columns": []map[string]string{{"label_name": "In Progress"}, {"label_name": "Backlog"}}})
	if rr.Code != http.StatusOK {
		t.Fatalf("ConfigureColumns = %d\n%s", rr.Code, rr.Body.String())
	}
	f.assertNothingSent(t)

	rows, err := f.pool.Query(context.Background(), `SELECT label_name FROM board_columns WHERE repo_id = $1 ORDER BY position`, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, l)
	}
	rows.Close()
	if len(cols) < 2 || cols[0] != "In Progress" || cols[1] != "Backlog" {
		t.Fatalf("board columns = %v, want In Progress then Backlog first", cols)
	}
	m, _ := f.getStates(t)
	mapped := m["columns"].([]any)
	if len(mapped) != 1 || mapped[0].(map[string]any)["label_name"] != "In Progress" {
		t.Fatalf("mapping after the reconfigure = %v, want only In Progress", m)
	}
	if m["open_state"] != "Open" {
		t.Errorf("the open and closed pair must survive a reconfigure: %v", m)
	}

	// No column at all prunes every mapping row and keeps the pair.
	if rr := f.call(f.h.ConfigureColumns, http.MethodPut, map[string]any{"columns": []map[string]string{}}); rr.Code != http.StatusOK {
		t.Fatalf("empty ConfigureColumns = %d\n%s", rr.Code, rr.Body.String())
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM repo_issue_tracker_column_states WHERE repo_id = $1`, f.repo).Scan(&n); err != nil || n != 0 {
		t.Fatalf("column mapping rows after clearing the columns = %d (%v), want 0", n, err)
	}
}

// A linked repo's column labels are stored and shown back, so ConfigureColumns refuses one the
// YouTrack driver would refuse as a tag name: bidi or zero-width characters, braces, an
// overlong label. Nothing is stored and nothing is sent.
func TestConfigureColumnsOnALinkedRepoRefusesBadLabelsLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	var before int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM board_columns WHERE repo_id = $1`, f.repo).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for name, label := range map[string]string{
		"bidi override": "Do\u202eing",
		"zero width":    "Do\u200bing",
		"line sep":      "Do\u2028ing",
		"brace":         "Do{ing",
		"hangul filler": "Review\u3164",
		"blank filler":  "\u2800",
		"variation sel": "Review\uFE0F",
		"overlong":      strings.Repeat("x", 201),
	} {
		t.Run(name, func(t *testing.T) {
			rr := f.call(f.h.ConfigureColumns, http.MethodPut, map[string]any{"columns": []map[string]string{{"label_name": label}}})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("ConfigureColumns = %d, want 400\n%s", rr.Code, rr.Body.String())
			}
		})
	}
	var after int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM board_columns WHERE repo_id = $1`, f.repo).Scan(&after); err != nil || after != before {
		t.Fatalf("board columns after the refusals = %d (%v), want the %d it had", after, err, before)
	}
	f.assertNothingSent(t)
}
