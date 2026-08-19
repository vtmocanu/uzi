package forge

import (
	"context"
	"encoding/json"
	"errors"
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

// TestListIssueCommentsFiltersSystemAndNormalizesOrder pins PRD #381 M1 for
// GitLab: a system note is dropped (D2), the human notes come back oldest-first
// (D8) even though GitLab defaults newest-first, and the neutral shape carries
// author id/username/body/created_at. The driver asks GitLab for ascending order
// (sort=asc), so the mock echoes them in that order to model the wire response.
func TestListIssueCommentsFiltersSystemAndNormalizesOrder(t *testing.T) {
	var gotSort, gotOrderBy string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues/11/notes": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			gotSort = r.URL.Query().Get("sort")
			gotOrderBy = r.URL.Query().Get("order_by")
			w.Header().Set("X-Next-Page", "")
			// Ascending (oldest-first) as the driver requests: the earlier human
			// note, a system note, then the later human note.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": 601, "system": false, "body": "please guard on Valid",
					"author":     map[string]any{"id": 42, "username": "carol"},
					"created_at": "2026-07-04T09:00:00Z",
				},
				{
					"id": 602, "system": true, "body": "changed the milestone",
					"author":     map[string]any{"id": 99, "username": "gitlab-bot"},
					"created_at": "2026-07-04T09:30:00Z",
				},
				{
					"id": 603, "system": false, "body": "and revise the existing test",
					"author":     map[string]any{"id": 43, "username": "dave"},
					"created_at": "2026-07-04T10:00:00Z",
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	comments, err := d.ListIssueComments(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if gotSort != "asc" || gotOrderBy != "created_at" {
		t.Errorf("driver did not request oldest-first ordering: sort=%q order_by=%q", gotSort, gotOrderBy)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 human comments (system note dropped), got %d: %+v", len(comments), comments)
	}
	first := comments[0]
	if first.AuthorForgeUserID != 42 || first.AuthorUsername != "carol" || first.Body != "please guard on Valid" {
		t.Fatalf("unexpected first comment shape: %+v", first)
	}
	if first.CreatedAt.IsZero() {
		t.Error("expected a non-zero CreatedAt on the first comment")
	}
	if !comments[0].CreatedAt.Before(comments[1].CreatedAt) {
		t.Errorf("comments not oldest-first: %v then %v", comments[0].CreatedAt, comments[1].CreatedAt)
	}
	if comments[1].AuthorUsername != "dave" || comments[1].Body != "and revise the existing test" {
		t.Fatalf("unexpected second comment: %+v", comments[1])
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
	if _, err := d.ListIssueComments(context.Background(), 7, 11); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("ListIssueComments leaked or did not error: %v", err)
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

func TestTokenInfoParsesIntrospection(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/personal_access_tokens/self": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "uzi-bot", "revoked": false, "active": true,
				"scopes": []string{"api"}, "expires_at": "2026-08-01",
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	info, err := d.TokenInfo(context.Background())
	if err != nil {
		t.Fatalf("TokenInfo: %v", err)
	}
	if len(info.Scopes) != 1 || info.Scopes[0] != "api" {
		t.Fatalf("unexpected scopes: %v", info.Scopes)
	}
	if !info.Active {
		t.Fatal("expected active token")
	}
	if info.ExpiresAt.IsZero() {
		t.Fatal("expected expires_at to be parsed")
	}
}

func TestTokenInfoUnsupportedReturnsSentinel(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		// GitLab < 15.5 has no such endpoint → 404.
		"/api/v4/personal_access_tokens/self": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if _, err := d.TokenInfo(context.Background()); !errors.Is(err, ErrTokenIntrospectionUnsupported) {
		t.Fatalf("a 404 must map to ErrTokenIntrospectionUnsupported, got %v", err)
	}
}

func TestProjectRoleUsesEffectiveMembership(t *testing.T) {
	var gotPath string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/members/all/4242": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "username": "bot", "access_level": 40})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	role, member, err := d.ProjectRole(context.Background(), 7, 4242)
	if err != nil {
		t.Fatalf("ProjectRole: %v", err)
	}
	// Maintainer (40) maps onto the neutral admin role: the driver's job is to
	// translate GitLab's number, never to hand it out.
	if !member || role != RoleAdmin {
		t.Fatalf("expected role=admin member=true, got role=%q member=%v", role, member)
	}
	// Effective membership (direct + inherited) is load-bearing — a group
	// -inherited Maintainer role is invisible to the direct-members endpoint.
	if !strings.Contains(gotPath, "/members/all/") {
		t.Fatalf("must query the effective-membership endpoint, got %q", gotPath)
	}
}

func TestProjectRoleNotAMember(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/members/all/4242": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	role, member, err := d.ProjectRole(context.Background(), 7, 4242)
	if err != nil {
		t.Fatalf("a 404 (not a member) must be a nil error, got %v", err)
	}
	if member || role != RoleNone {
		t.Fatalf("expected not-a-member (role none, member false), got role=%q member=%v", role, member)
	}
}

// TestRoleForAccessLevel pins the GitLab access-level → neutral Role mapping.
// Developer (30) is the hinge: it is exactly the write role uzi's bot must hold,
// so the boundary either side of it is what privcheck's above/below findings key
// on.
func TestRoleForAccessLevel(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  Role
	}{
		{0, RoleNone},
		{5, RoleRead},  // Minimal
		{10, RoleRead}, // Guest
		{20, RoleRead}, // Reporter
		{30, RoleWrite},
		{40, RoleAdmin},
		{50, RoleOwner},
		{60, RoleOwner}, // Admin
	} {
		if got := roleForAccessLevel(tc.level); got != tc.want {
			t.Errorf("roleForAccessLevel(%d) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestDefaultBranchProtectionParsesPushLevels(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/protected_branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "main",
				"push_access_levels": []map[string]any{
					{"access_level": 30},                  // the write role may push → WriteRoleCanPush
					{"access_level": 60, "user_id": 4242}, // a per-user grant naming the bot → BotCanPush
				},
				// Maintainer-only merge: the safe half, so this fixture isolates push.
				"merge_access_levels": []map[string]any{{"access_level": 40}},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("DefaultBranchProtection: %v", err)
	}
	if !bp.Protected {
		t.Fatal("expected Protected")
	}
	if !bp.WriteRoleCanPush {
		t.Fatal("the level-30 push entry must set WriteRoleCanPush")
	}
	if !bp.BotCanPush {
		t.Fatal("the per-user (user_id=4242) push entry must set BotCanPush")
	}
	if bp.WriteRoleCanMerge || bp.BotCanMerge {
		t.Fatalf("Maintainer-only merge must leave the merge fields clear, got %+v", bp)
	}
}

// TestDefaultBranchProtectionParsesMergeLevels covers D6a-1: GitLab's initial
// default puts merge_access_levels at Maintainer, so a Developer bot cannot
// merge — but it is a setting, and this fixture is what a repo looks like once
// someone has changed it. uzi modelled merge on no forge before PRD #65.
func TestDefaultBranchProtectionParsesMergeLevels(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/protected_branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "main",
				// Maintainer-only push: the bot cannot push. It can still merge.
				"push_access_levels": []map[string]any{{"access_level": 40}},
				"merge_access_levels": []map[string]any{
					{"access_level": 30},                  // the write role may merge → WriteRoleCanMerge
					{"access_level": 60, "user_id": 4242}, // a per-user merge grant naming the bot → BotCanMerge
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("DefaultBranchProtection: %v", err)
	}
	if bp.WriteRoleCanPush || bp.BotCanPush {
		t.Fatalf("Maintainer-only push must leave the push fields clear, got %+v", bp)
	}
	if !bp.WriteRoleCanMerge {
		t.Fatal("the level-30 merge entry must set WriteRoleCanMerge")
	}
	if !bp.BotCanMerge {
		t.Fatal("the per-user (user_id=4242) merge entry must set BotCanMerge")
	}
}

// TestGitLabDefaultBranchProtectionSkipsNullAccessLevels is the regression test for
// the nil-pointer panic that a null array element in push_access_levels or
// merge_access_levels used to trigger. GitLab's REST API can return a JSON
// `null` in these arrays; go-gitlab decodes that to a nil
// *BranchAccessDescription, and DefaultBranchProtection dereferenced it without
// a guard. Without the `if pl == nil { continue }` / `if ml == nil { continue }`
// guards in gitlab.go this panics; with them the null is skipped and the valid
// entry alongside it is still processed.
func TestGitLabDefaultBranchProtectionSkipsNullAccessLevels(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/protected_branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "main",
				// A nil map encodes as JSON `null`, reproducing the mixed
				// [null, {...}] array the live API can send. The valid element
				// names the bot at Developer level, so it must still be processed.
				"push_access_levels": []map[string]any{
					nil,
					{"access_level": 30, "user_id": 4242},
				},
				"merge_access_levels": []map[string]any{
					nil,
					{"access_level": 30, "user_id": 4242},
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	// A missing guard makes DefaultBranchProtection panic here rather than return.
	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("DefaultBranchProtection: %v", err)
	}
	if !bp.Protected {
		t.Fatal("expected Protected")
	}
	// The valid element survives the null: level 30 (Developer) sets the write
	// role flags, and user_id 4242 matching the bot sets the per-user flags.
	if !bp.WriteRoleCanPush {
		t.Fatal("the level-30 push entry after the null must set WriteRoleCanPush")
	}
	if !bp.BotCanPush {
		t.Fatal("the per-user (user_id=4242) push entry after the null must set BotCanPush")
	}
	if !bp.WriteRoleCanMerge {
		t.Fatal("the level-30 merge entry after the null must set WriteRoleCanMerge")
	}
	if !bp.BotCanMerge {
		t.Fatal("the per-user (user_id=4242) merge entry after the null must set BotCanMerge")
	}
}

func TestDefaultBranchProtectionCleanBranch(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/protected_branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "main",
				// Maintainer-only push and merge, no per-user grant: this is what
				// GitLab's "Fully protected" initial default produces, and it is the
				// only fixture here that must come back completely clean.
				"push_access_levels":  []map[string]any{{"access_level": 40}},
				"merge_access_levels": []map[string]any{{"access_level": 40}},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("DefaultBranchProtection: %v", err)
	}
	if !bp.Protected || bp.WriteRoleCanPush || bp.BotCanPush || bp.WriteRoleCanMerge || bp.BotCanMerge {
		t.Fatalf("Maintainer-only push+merge should be clean, got %+v", bp)
	}
}

// TestDefaultBranchProtectionUnprotectedIsNotSafe is the R12 regression test.
//
// An unprotected branch is the branch a write-role bot most certainly CAN push
// to and merge into. The 404 path used to return BranchProtection{Protected:
// false} and leave the rest at the zero value, which reads identically to
// "evaluated, and the bot cannot do either" — so a consumer writing the obvious
// `if canPush || canMerge { refuse }` would wave through the single worst case
// in the product. The fields must therefore be evaluated on this path too.
//
// This test fails if the driver reverts to the zero value: drop the two `true`s
// in the 404 arm of DefaultBranchProtection and the assertion below goes red,
// which is what makes BranchProtection's doc comment an assertion rather than a
// hope.
func TestDefaultBranchProtectionUnprotectedIsNotSafe(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/protected_branches/main": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("a 404 (unprotected) must be a nil error, got %v", err)
	}
	if bp.Protected {
		t.Fatal("a 404 must map to Protected=false")
	}
	// The load-bearing half: a consumer that never looks at Protected must still
	// not be able to read this as a clean branch.
	if !bp.WriteRoleCanPush {
		t.Fatal("an unprotected branch admits a write-role push; reporting false inverts the guardrail")
	}
	if !bp.WriteRoleCanMerge {
		t.Fatal("an unprotected branch admits a write-role merge; reporting false inverts the guardrail")
	}
}

// TestNewMethodErrorsAreRedacted covers the redaction discipline for the three
// PRD #5 driver methods on their non-404 error paths, with the PAT echoed in the
// upstream error body (Success Criterion: "redaction tests cover the new driver
// methods").
func TestNewMethodErrorsAreRedacted(t *testing.T) {
	// A non-glpat, low-entropy literal (the redactor scrubs any secret >= 8 chars,
	// not just real PAT formats) so this fixture carries no scanner-tripping token.
	const token = "uzi-redaction-test-token-not-real"
	// 403 (not 5xx/429) so the client-go retryable transport does not back off and
	// retry — the driver still builds a redacted error from the echoed body.
	leak := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "upstream boom " + token})
	}
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/personal_access_tokens/self":        leak,
		"/api/v4/projects/7/members/all/4242":        leak,
		"/api/v4/projects/7/protected_branches/main": leak,
	})
	d := newTestDriver(t, m, token)
	ctx := context.Background()

	_, err1 := d.TokenInfo(ctx)
	_, _, err2 := d.ProjectRole(ctx, 7, 4242)
	_, err3 := d.DefaultBranchProtection(ctx, 7, "main", 4242)

	for _, c := range []struct {
		name string
		err  error
	}{{"TokenInfo", err1}, {"ProjectRole", err2}, {"DefaultBranchProtection", err3}} {
		if c.err == nil {
			t.Errorf("%s: expected an error from a 500", c.name)
			continue
		}
		if strings.Contains(c.err.Error(), token) {
			t.Errorf("%s leaked the PAT: %q", c.name, c.err.Error())
		}
	}
}

// TestGitLabListMethodsSkipNullElements pins the nil-element guards on the five
// list loops. A GitLab REST response is a JSON array, and a hostile or buggy
// forge can put a `null` where an object is expected: `[null, {...}]` decodes to
// a slice whose first element is a nil pointer. Before the guards, the list loops
// dereferenced every element unconditionally, so the nil-first array panicked on
// the very first iteration.
//
// Each sub-test installs a route returning exactly `[null, {<one valid object>}]`
// and asserts the method (1) does not panic and returns no error, and (2) returns
// exactly the one valid element — proving the null was skipped, not that the whole
// page was dropped. Remove any of the five `if x == nil { continue }` guards in
// gitlab.go and the matching sub-test panics (a nil-pointer dereference surfaced by
// the test runner as a failed test), which is what makes this a regression test and
// not just a happy-path parse.
func TestGitLabListMethodsSkipNullElements(t *testing.T) {
	const projectID, issueIID, pipelineID = 7, 11, 55

	tests := []struct {
		name string
		path string
		body string
		// invoke calls the driver method and returns the number of elements it
		// yielded plus a distinguishing field of the (expected single) survivor.
		invoke func(ctx context.Context, d Forge) (n int, got string, err error)
		want   string
	}{
		{
			name: "ListProjects",
			path: "/api/v4/projects",
			body: `[null, {"id": 1, "path_with_namespace": "grp/a", "web_url": "https://gl/grp/a", "default_branch": "main"}]`,
			invoke: func(ctx context.Context, d Forge) (int, string, error) {
				ps, err := d.ListProjects(ctx)
				if err != nil || len(ps) != 1 {
					return len(ps), "", err
				}
				return len(ps), ps[0].PathWithNamespace, nil
			},
			want: "grp/a",
		},
		{
			name: "ListLabels",
			path: "/api/v4/projects/7/labels",
			body: `[null, {"name": "PRD", "color": "#112233"}]`,
			invoke: func(ctx context.Context, d Forge) (int, string, error) {
				ls, err := d.ListLabels(ctx, projectID)
				if err != nil || len(ls) != 1 {
					return len(ls), "", err
				}
				return len(ls), ls[0].Name, nil
			},
			want: "PRD",
		},
		{
			name: "ListIssues",
			path: "/api/v4/projects/7/issues",
			body: `[null, {"id": 1001, "iid": 11, "title": "Do the thing", "state": "opened", "author": {"username": "alice"}}]`,
			invoke: func(ctx context.Context, d Forge) (int, string, error) {
				is, err := d.ListIssues(ctx, projectID, ListIssuesOptions{})
				if err != nil || len(is) != 1 {
					return len(is), "", err
				}
				return len(is), is[0].Title, nil
			},
			want: "Do the thing",
		},
		{
			name: "ListIssueLabelEvents",
			path: "/api/v4/projects/7/issues/11/resource_label_events",
			body: `[null, {"id": 501, "action": "add", "created_at": "2026-07-04T09:00:00Z", "user": {"id": 42, "username": "carol"}, "label": {"id": 9, "name": "autopilot"}}]`,
			invoke: func(ctx context.Context, d Forge) (int, string, error) {
				es, err := d.ListIssueLabelEvents(ctx, projectID, issueIID)
				if err != nil || len(es) != 1 {
					return len(es), "", err
				}
				return len(es), es[0].LabelName, nil
			},
			want: "autopilot",
		},
		{
			name: "ListPipelineJobs",
			path: "/api/v4/projects/7/pipelines/55/jobs",
			body: `[null, {"id": 9, "name": "build", "stage": "build", "status": "success", "web_url": "https://gl/grp/a/-/jobs/9"}]`,
			invoke: func(ctx context.Context, d Forge) (int, string, error) {
				js, err := d.ListPipelineJobs(ctx, projectID, pipelineID)
				if err != nil || len(js) != 1 {
					return len(js), "", err
				}
				return len(js), js[0].Name, nil
			},
			want: "build",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				tc.path: func(w http.ResponseWriter, _ *http.Request) {
					// Empty X-Next-Page → resp.NextPage == 0, so the paginating loop
					// stops after this single page.
					w.Header().Set("X-Next-Page", "")
					_, _ = w.Write([]byte(body))
				},
			})
			d := newTestDriver(t, m, "glpat-abcdefabcdef")

			n, got, err := tc.invoke(context.Background(), d)
			if err != nil {
				t.Fatalf("%s returned an error on a [null, {...}] page: %v", tc.name, err)
			}
			if n != 1 {
				t.Fatalf("%s: expected exactly the one valid element (null skipped), got %d", tc.name, n)
			}
			if got != tc.want {
				t.Fatalf("%s: expected the surviving element's field %q, got %q", tc.name, tc.want, got)
			}
		})
	}
}

func TestErrorsAreRedacted(t *testing.T) {
	const token = "glpat-supersecret-eviltoken-XYZ" //gitleaks:allow // fake PAT fixture: the mock server echoes it back so the driver's redactor is proven to strip it, never a real secret
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
