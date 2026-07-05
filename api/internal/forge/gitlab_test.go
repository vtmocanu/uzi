package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// mockGitLab is an httptest server standing in for the GitLab REST API. It
// records the PRIVATE-TOKEN it received and lets each test install per-path
// handlers. Two-page pagination is exercised by the projects handler.
type mockGitLab struct {
	srv       *httptest.Server
	gotToken  string
	lastQuery url.Values
}

func newMockGitLab(t *testing.T, routes map[string]http.HandlerFunc) *mockGitLab {
	t.Helper()
	m := &mockGitLab{}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.gotToken = r.Header.Get("PRIVATE-TOKEN")
		m.lastQuery = r.URL.Query()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func newTestDriver(t *testing.T, m *mockGitLab, token string) Forge {
	t.Helper()
	d, err := New(TypeGitLab, m.srv.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestVerifyToken(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/user": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "username": "uzi-bot-test"})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	id, err := d.VerifyToken(context.Background())
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.ForgeUserID != 4242 || id.Username != "uzi-bot-test" {
		t.Fatalf("unexpected identity: %+v", id)
	}
	if m.gotToken != "glpat-token-value-123456" {
		t.Fatalf("driver sent wrong PRIVATE-TOKEN: %q", m.gotToken)
	}
}

func TestListProjectsPaginates(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects": func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if r.URL.Query().Get("membership") != "true" {
				t.Errorf("expected membership=true, got query %v", r.URL.Query())
			}
			switch page {
			case "", "1":
				w.Header().Set("X-Next-Page", "2")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 1, "path_with_namespace": "grp/a", "web_url": "https://gl/grp/a", "default_branch": "main"},
				})
			case "2":
				w.Header().Set("X-Next-Page", "")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 2, "path_with_namespace": "grp/b", "web_url": "https://gl/grp/b", "default_branch": "master"},
				})
			default:
				t.Errorf("unexpected page %q", page)
			}
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	projects, err := d.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects across pages, got %d", len(projects))
	}
	if projects[0].ForgeProjectID != 1 || projects[1].ForgeProjectID != 2 {
		t.Fatalf("unexpected project ids: %+v", projects)
	}
}

func TestListIssuesAlwaysQueriesStateAll(t *testing.T) {
	var gotState, gotLabels, gotUpdatedAfter string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			gotState = q.Get("state")
			gotLabels = q.Get("labels")
			gotUpdatedAfter = q.Get("updated_after")
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": 1001, "iid": 11, "title": "Do the thing", "state": "opened",
					"labels": []string{"PRD", "In Progress"}, "description": "see prds/2-forge.md",
					"web_url": "https://gl/grp/a/-/issues/11", "updated_at": "2026-07-03T10:00:00Z",
					"author": map[string]any{"username": "alice"},
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	issues, err := d.ListIssues(context.Background(), 7, ListIssuesOptions{
		Labels:       []string{"PRD"},
		UpdatedAfter: &after,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if gotState != "all" {
		t.Errorf("expected state=all, got %q", gotState)
	}
	if gotLabels != "PRD" {
		t.Errorf("expected labels=PRD, got %q", gotLabels)
	}
	if gotUpdatedAfter == "" {
		t.Error("expected updated_after to be sent")
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	got := issues[0]
	if got.IID != 11 || got.Author != "alice" || got.State != "opened" {
		t.Fatalf("unexpected issue: %+v", got)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "PRD" {
		t.Fatalf("unexpected labels: %v", got.Labels)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("expected non-zero UpdatedAt")
	}
}

func TestGetIssueReturnsDescription(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues/11": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1001, "iid": 11, "title": "Do the thing", "state": "opened",
				"labels": []string{"PRD"}, "description": "body links prds/4-agent-runtime-workers.md",
				"web_url": "https://gl/grp/a/-/issues/11",
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	issue, err := d.GetIssue(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.IID != 11 || issue.Title != "Do the thing" {
		t.Fatalf("unexpected issue: %+v", issue)
	}
	if issue.Description != "body links prds/4-agent-runtime-workers.md" {
		t.Fatalf("GetIssue must carry the description (the run-create snapshot source), got %q", issue.Description)
	}
}

func TestCreateIssueSendsTitleDescriptionAndLabels(t *testing.T) {
	var gotTitle, gotDescription, gotLabels string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Labels      string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotTitle, gotDescription, gotLabels = body.Title, body.Description, body.Labels
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 2002, "iid": 42, "title": body.Title, "state": "opened",
				"labels": []string{"PRD"}, "web_url": "https://gl/grp/a/-/issues/42",
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	issue, err := d.CreateIssue(context.Background(), 7, "New PRD", "see prds/9-foo.md", []string{"PRD"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.IID != 42 {
		t.Fatalf("expected created issue iid 42, got %d", issue.IID)
	}
	if gotTitle != "New PRD" || gotDescription != "see prds/9-foo.md" {
		t.Fatalf("wrong title/description sent: %q / %q", gotTitle, gotDescription)
	}
	if gotLabels != "PRD" {
		t.Fatalf("expected labels=PRD to be sent, got %q", gotLabels)
	}
}

func TestUpdateIssueLabelsSendsAtomicAddRemove(t *testing.T) {
	var addLabels, removeLabels string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues/11": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				t.Errorf("expected PUT, got %s", r.Method)
			}
			// client-go sends update fields as a JSON body; LabelOptions
			// marshals to a comma-joined string.
			var body struct {
				AddLabels    string `json:"add_labels"`
				RemoveLabels string `json:"remove_labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			addLabels = body.AddLabels
			removeLabels = body.RemoveLabels
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "iid": 11, "state": "opened"})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	err := d.UpdateIssueLabels(context.Background(), 7, 11, []string{"Upcoming"}, []string{"In Progress", "Later"})
	if err != nil {
		t.Fatalf("UpdateIssueLabels: %v", err)
	}
	if addLabels != "Upcoming" {
		t.Errorf("expected add_labels=Upcoming, got %q", addLabels)
	}
	if !strings.Contains(removeLabels, "In Progress") || !strings.Contains(removeLabels, "Later") {
		t.Errorf("expected both columns removed, got remove_labels=%q", removeLabels)
	}
}

func TestEnsureLabelsCreatesOnlyMissing(t *testing.T) {
	created := map[string]bool{}
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/labels": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("X-Next-Page", "")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"name": "In Progress", "color": "#111111"},
				})
			case http.MethodPost:
				var body struct {
					Name string `json:"name"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				created[body.Name] = true
				_ = json.NewEncoder(w).Encode(map[string]any{"name": body.Name})
			}
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	err := d.EnsureLabels(context.Background(), 7, []Label{
		{Name: "In Progress"}, {Name: "Upcoming"}, {Name: "Later"},
	})
	if err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if created["In Progress"] {
		t.Error("existing label In Progress should not be re-created")
	}
	if !created["Upcoming"] || !created["Later"] {
		t.Errorf("missing labels should be created, got %v", created)
	}
}

func TestUserExistsByUsername(t *testing.T) {
	var gotUsername string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/users": func(w http.ResponseWriter, r *http.Request) {
			gotUsername = r.URL.Query().Get("username")
			if gotUsername == "ghost" {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 77, "username": gotUsername},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	exists, err := d.UserExists(context.Background(), "alice")
	if err != nil {
		t.Fatalf("UserExists: %v", err)
	}
	if !exists {
		t.Error("expected alice to exist")
	}
	if gotUsername != "alice" {
		t.Errorf("expected the username filter to be sent, got %q", gotUsername)
	}

	// A username the forge does not know comes back as an empty list → not found.
	missing, err := d.UserExists(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("UserExists(ghost): %v", err)
	}
	if missing {
		t.Error("expected ghost to not exist")
	}

	// A blank username is not a forge lookup at all.
	if exists, err := d.UserExists(context.Background(), "  "); err != nil || exists {
		t.Errorf("blank username: got (%v, %v), want (false, nil)", exists, err)
	}
}

func TestListIssueLabelEventsParses(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues/11/resource_label_events": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": 501, "action": "add", "created_at": "2026-07-04T09:00:00Z",
					"user":  map[string]any{"id": 42, "username": "carol"},
					"label": map[string]any{"id": 9, "name": "autopilot"},
				},
				{
					"id": 502, "action": "remove", "created_at": "2026-07-04T10:00:00Z",
					"user":  map[string]any{"id": 42, "username": "carol"},
					"label": map[string]any{"id": 9, "name": "autopilot"},
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	events, err := d.ListIssueLabelEvents(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("ListIssueLabelEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	add := events[0]
	if add.ID != 501 || add.Action != "add" || add.LabelName != "autopilot" || add.Username != "carol" {
		t.Fatalf("unexpected add event: %+v", add)
	}
	if add.CreatedAt.IsZero() {
		t.Error("expected a non-zero CreatedAt on the add event")
	}
	if events[1].Action != "remove" {
		t.Errorf("expected the second event to be a remove, got %q", events[1].Action)
	}
}

func TestCreateIssueNoteSendsBody(t *testing.T) {
	var gotBody string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues/11/notes": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = body.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "body": body.Body})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	note, err := d.CreateIssueNote(context.Background(), 7, 11, "autopilot could not resolve a user")
	if err != nil {
		t.Fatalf("CreateIssueNote: %v", err)
	}
	if note.ID != 9001 {
		t.Fatalf("expected created note id 9001, got %d", note.ID)
	}
	if gotBody != "autopilot could not resolve a user" {
		t.Fatalf("wrong note body sent: %q", gotBody)
	}
}

// The redaction contract must hold for the new methods too: even if the forge
// echoes the PAT back in an error body, the driver must not surface it. The probe
// token below is a fake, long enough for the redactor to act on (it ignores
// secrets under 8 chars); its exact shape is irrelevant to what redaction proves.
func TestNewMethodsRedactErrors(t *testing.T) {
	const token = "fake-pat-redaction-probe-0123456789"
	leak := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom " + token})
	}
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/users": leak,
		"/api/v4/projects/7/issues/11/resource_label_events": leak,
		"/api/v4/projects/7/issues/11/notes":                 leak,
	})
	d := newTestDriver(t, m, token)

	if _, err := d.UserExists(context.Background(), "alice"); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("UserExists leaked or did not error: %v", err)
	}
	if _, err := d.ListIssueLabelEvents(context.Background(), 7, 11); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("ListIssueLabelEvents leaked or did not error: %v", err)
	}
	if _, err := d.CreateIssueNote(context.Background(), 7, 11, "x"); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("CreateIssueNote leaked or did not error: %v", err)
	}
}

func TestGetMergeRequestReturnsState(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 5005, "iid": 13, "state": "closed",
				"web_url": "https://gl/grp/a/-/merge_requests/13",
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	mr, err := d.GetMergeRequest(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("GetMergeRequest: %v", err)
	}
	if mr.IID != 13 || mr.State != "closed" {
		t.Fatalf("unexpected MR: %+v", mr)
	}
	if mr.WebURL != "https://gl/grp/a/-/merge_requests/13" {
		t.Fatalf("unexpected MR web url: %q", mr.WebURL)
	}
}

func TestGetMergeRequestRedactsError(t *testing.T) {
	const token = "glpat-supersecret-mrtoken-XYZ" //gitleaks:allow // fake PAT fixture: proves GetMergeRequest redacts, never a real secret
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "bad token " + token})
		},
	})
	d := newTestDriver(t, m, token)

	if _, err := d.GetMergeRequest(context.Background(), 7, 13); err == nil {
		t.Fatal("expected an error from a 401")
	} else if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the PAT: %q", err.Error())
	}
}

func TestErrorsAreRedacted(t *testing.T) {
	const token = "glpat-supersecret-eviltoken-XYZ"
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		// Echo the token back inside the error body — worst case: the server
		// itself leaks it. The driver must still not surface it.
		"/api/v4/user": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "bad token " + token})
		},
	})
	d := newTestDriver(t, m, token)

	_, err := d.VerifyToken(context.Background())
	if err == nil {
		t.Fatal("expected an error from a 401")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the PAT: %q", err.Error())
	}
}
