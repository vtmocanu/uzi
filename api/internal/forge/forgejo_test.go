package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gitea "code.gitea.io/sdk/gitea"
)

// mockForgejo is an httptest server standing in for the Forgejo REST API. It
// records the Authorization header it received (Forgejo authenticates PATs as
// "token <t>") and the number of PUTs to the issue-labels route, and lets each
// test install per-path handlers under /api/v1. A repositories/{id} handler is
// installed by default so every repo-scoped method can resolve its slug.
type mockForgejo struct {
	srv       *httptest.Server
	gotAuth   string
	labelPUTs int
}

// newMockForgejo builds the server. routes are keyed by their /api/v1-relative
// path (e.g. "/repos/acme/widgets/issues"). A default /repositories/7 handler
// resolves project id 7 → acme/widgets unless the test overrides it.
func newMockForgejo(t *testing.T, routes map[string]http.HandlerFunc) *mockForgejo {
	t.Helper()
	m := &mockForgejo{}
	mux := http.NewServeMux()
	if _, ok := routes["/repositories/7"]; !ok {
		mux.HandleFunc("/api/v1/repositories/7", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 7, "name": "widgets", "full_name": "acme/widgets",
				"owner": map[string]any{"id": 1, "login": "acme"},
			})
		})
	}
	for pattern, h := range routes {
		mux.HandleFunc("/api/v1"+pattern, h)
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.gotAuth = r.Header.Get("Authorization")
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/labels") {
			m.labelPUTs++
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func newForgejoDriver(t *testing.T, m *mockForgejo, token string) Forge {
	t.Helper()
	d, err := New(TypeForgejo, m.srv.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// versionHandler is the /version handler most tests need so VerifyToken's gate
// passes. It reports the real released string, build-metadata suffix and all.
func versionHandler(v string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": v})
	}
}

func TestForgejoVerifyToken(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/version": versionHandler("16.0.0+gitea-1.22.0"),
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "uzi-bot-test", "is_admin": false})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	id, err := d.VerifyToken(context.Background())
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.ForgeUserID != 4242 || id.Username != "uzi-bot-test" || id.IsAdmin {
		t.Fatalf("unexpected identity: %+v", id)
	}
	if m.gotAuth != "token forgejo-token-value-123456" {
		t.Fatalf("driver sent wrong Authorization header: %q", m.gotAuth)
	}
}

func TestForgejoVerifyTokenReadsIsAdmin(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/version": versionHandler("16.0.0+gitea-1.22.0"),
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			// Forgejo always emits is_admin (no omitempty), so true is authoritative.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "root", "is_admin": true})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	id, err := d.VerifyToken(context.Background())
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !id.IsAdmin {
		t.Fatal("expected IsAdmin=true for an instance admin")
	}
}

// TestForgejoVersionGate is D4a: the version gate is the decision, so it is
// table-driven across every real string. A refused version must surface an error
// that names the required version and NEVER reaches GetMyUserInfo; an accepted
// one connects. Refusing codeberg's live 16.0.0-dev-626 string is the CORRECT
// answer, not a false negative — the -dev class spans builds with and without
// the job-logs route (D4a).
func TestForgejoVersionGate(t *testing.T) {
	for _, tc := range []struct {
		reported string
		accept   bool
	}{
		{"16.0.0+gitea-1.22.0", true},                   // the release; build metadata must not defeat the compare
		{"16.0.1", true},                                // patch above the floor
		{"16.1.0", true},                                // minor above the floor
		{"17.0.0", true},                                // major above the floor
		{"17.0.0-dev-3-abc123+gitea-1.22.0", true},      // major dominates: v17 cycle carries the v16 surface
		{"16.0.0-dev-626-32363b81+gitea-1.22.0", false}, // codeberg live: prerelease < release, refuse
		{"15.0.4", false},                               // below the floor (route genuinely absent)
		{"15.0.5+gitea-1.22.0", false},                  // below the floor
		{"1.21.11-2", false},                            // legacy 1.x scheme
		{"1.21.0-rc1", false},                           // legacy 1.x rc
		{"32363b81+gitea-1.22.0", false},                // bare sha from `git describe --always`: unparseable
		{"", false},                                     // empty: unparseable
	} {
		t.Run(tc.reported, func(t *testing.T) {
			userHit := false
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/version": versionHandler(tc.reported),
				"/user": func(w http.ResponseWriter, _ *http.Request) {
					userHit = true
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "bot", "is_admin": false})
				},
			})
			d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

			_, err := d.VerifyToken(context.Background())
			if tc.accept {
				if err != nil {
					t.Fatalf("version %q should connect, got error: %v", tc.reported, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("version %q should be refused, but VerifyToken succeeded", tc.reported)
			}
			if userHit {
				t.Errorf("a refused version must not reach GetMyUserInfo (the gate is first)")
			}
			// The error must name the required version so it is actionable to the user
			// (CreateConnection surfaces VerifyToken's error verbatim).
			if !strings.Contains(err.Error(), "16.0.0") {
				t.Errorf("refusal error must name the required version, got %q", err.Error())
			}
			// It must wrap the sentinel so the privilege sweep can errors.Is it and
			// raise a distinct downgrade finding (not the generic "could not verify").
			if !errors.Is(err, ErrForgeVersionUnsupported) {
				t.Errorf("refusal error must wrap ErrForgeVersionUnsupported, got %q", err.Error())
			}
		})
	}
}

func TestForgejoListProjectsFiltersPushAndPaginates(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/user/repos": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				// Advertise a next page via the Link header (how the SDK paginates). Only
				// the page= query is read from it, so the host is irrelevant.
				w.Header().Set("Link", `<http://x/api/v1/user/repos?page=2>; rel="next"`)
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 1, "full_name": "grp/pushable", "html_url": "https://fj/grp/pushable", "default_branch": "main",
						"permissions": map[string]any{"admin": false, "push": true, "pull": true}},
					{"id": 2, "full_name": "grp/readonly", "html_url": "https://fj/grp/readonly", "default_branch": "main",
						"permissions": map[string]any{"admin": false, "push": false, "pull": true}},
				})
			case "2":
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 3, "full_name": "grp/second-page", "html_url": "https://fj/grp/second-page", "default_branch": "trunk",
						"permissions": map[string]any{"admin": true, "push": true, "pull": true}},
				})
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	projects, err := d.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	// The read-only repo (id 2) must be filtered; the two pushable repos survive
	// across both pages.
	if len(projects) != 2 {
		t.Fatalf("expected 2 pushable projects across pages, got %d: %+v", len(projects), projects)
	}
	if projects[0].ForgeProjectID != 1 || projects[1].ForgeProjectID != 3 {
		t.Fatalf("unexpected project ids (read-only should be dropped): %+v", projects)
	}
	if projects[0].PathWithNamespace != "grp/pushable" {
		t.Fatalf("unexpected path mapping: %+v", projects[0])
	}
}

// TestForgejoListIssuesFiltersPullRequests is R4 / test #1, the trap most likely
// to ship broken: Forgejo models a PR as an issue with a non-nil pull_request.
// A mixed page comes in; only real issues come out.
func TestForgejoListIssuesFiltersPullRequests(t *testing.T) {
	var gotState, gotType, gotLabels, gotSince string
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			gotState, gotType, gotLabels, gotSince = q.Get("state"), q.Get("type"), q.Get("labels"), q.Get("since")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 100, "number": 11, "title": "A real issue", "state": "open",
					"labels": []map[string]any{{"id": 3, "name": "PRD"}}, "body": "see prds/2-forge.md",
					"html_url": "https://fj/acme/widgets/issues/11", "updated_at": "2026-07-03T10:00:00Z",
					"user": map[string]any{"login": "alice"}},
				{"id": 101, "number": 12, "title": "A pull request", "state": "open",
					"html_url":     "https://fj/acme/widgets/pulls/12",
					"pull_request": map[string]any{"merged": false, "html_url": "https://fj/acme/widgets/pulls/12"}},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	issues, err := d.ListIssues(context.Background(), 7, ListIssuesOptions{Labels: []string{"PRD"}, UpdatedAfter: &after})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("a PR must not survive onto the board; got %d rows: %+v", len(issues), issues)
	}
	got := issues[0]
	if got.IID != 11 || got.Author != "alice" || got.State != "opened" {
		t.Fatalf("unexpected issue (note state 'open'->'opened'): %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "PRD" {
		t.Fatalf("unexpected labels: %v", got.Labels)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("expected non-zero UpdatedAt")
	}
	if gotState != "all" {
		t.Errorf("expected state=all, got %q", gotState)
	}
	if gotType != "issues" {
		t.Errorf("expected type=issues to be requested server-side, got %q", gotType)
	}
	if gotLabels != "PRD" {
		t.Errorf("expected labels=PRD, got %q", gotLabels)
	}
	if gotSince == "" {
		t.Error("expected since to be sent for UpdatedAfter")
	}
}

func TestForgejoGetIssueReturnsDescription(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/11": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 100, "number": 11, "title": "Do the thing", "state": "open",
				"labels":   []map[string]any{{"id": 3, "name": "PRD"}},
				"body":     "body links prds/4-agent-runtime-workers.md",
				"html_url": "https://fj/acme/widgets/issues/11",
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	issue, err := d.GetIssue(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.IID != 11 || issue.Title != "Do the thing" {
		t.Fatalf("unexpected issue: %+v", issue)
	}
	if issue.Description != "body links prds/4-agent-runtime-workers.md" {
		t.Fatalf("GetIssue must carry the body as Description, got %q", issue.Description)
	}
}

func TestForgejoCreateIssueResolvesLabelIDs(t *testing.T) {
	var sentLabelIDs []int64
	var gotTitle, gotBody string
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/labels": func(w http.ResponseWriter, _ *http.Request) {
			// The repo catalog: "PRD" is id 3. CreateIssue must resolve the name to it.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 3, "name": "PRD", "color": "00aabb"},
				{"id": 4, "name": "Later", "color": "999999"},
			})
		},
		"/repos/acme/widgets/issues": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body struct {
				Title  string  `json:"title"`
				Body   string  `json:"body"`
				Labels []int64 `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotTitle, gotBody, sentLabelIDs = body.Title, body.Body, body.Labels
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 200, "number": 42, "title": body.Title, "state": "open",
				"labels": []map[string]any{{"id": 3, "name": "PRD"}}, "html_url": "https://fj/acme/widgets/issues/42",
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	issue, err := d.CreateIssue(context.Background(), 7, "New PRD", "see prds/9-foo.md", []string{"PRD"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.IID != 42 {
		t.Fatalf("expected created issue iid 42, got %d", issue.IID)
	}
	if gotTitle != "New PRD" || gotBody != "see prds/9-foo.md" {
		t.Fatalf("wrong title/body sent: %q / %q", gotTitle, gotBody)
	}
	if len(sentLabelIDs) != 1 || sentLabelIDs[0] != 3 {
		t.Fatalf("expected the PRD label to resolve to id 3, got %v", sentLabelIDs)
	}
}

// TestForgejoCreateIssueAutoCreatesMissingLabel pins the GitLab-parity fix (found
// by the M9 e2e): a label the repo does not yet have — e.g. the PRD trigger label,
// which is never EnsureLabels'd — is CREATED, not errored on, so CreateIssue does
// not 502 on a Forgejo repo that lacks it. Asserts a CreateLabel POST fires and its
// new id is what the issue is created with.
func TestForgejoCreateIssueAutoCreatesMissingLabel(t *testing.T) {
	var createdLabel string
	var sentLabelIDs []int64
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/labels": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]map[string]any{}) // empty catalog: PRD is missing
			case http.MethodPost:
				var body struct {
					Name  string `json:"name"`
					Color string `json:"color"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				createdLabel = body.Name
				if body.Color == "" {
					t.Error("CreateLabel must carry a color (Forgejo validates it)")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 77, "name": body.Name, "color": body.Color})
			}
		},
		"/repos/acme/widgets/issues": func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Labels []int64 `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sentLabelIDs = body.Labels
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 200, "number": 42, "title": "New PRD", "state": "open",
				"labels": []map[string]any{{"id": 77, "name": "PRD"}}, "html_url": "https://fj/acme/widgets/issues/42",
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	issue, err := d.CreateIssue(context.Background(), 7, "New PRD", "see prds/9-foo.md", []string{"PRD"})
	if err != nil {
		t.Fatalf("CreateIssue with an unknown label must auto-create it, not error: %v", err)
	}
	if createdLabel != "PRD" {
		t.Fatalf("the missing PRD label must be created, got created=%q", createdLabel)
	}
	if len(sentLabelIDs) != 1 || sentLabelIDs[0] != 77 {
		t.Fatalf("the issue must be created with the newly-created label id 77, got %v", sentLabelIDs)
	}
	if issue.IID != 42 {
		t.Fatalf("expected created issue iid 42, got %d", issue.IID)
	}
}

// TestForgejoUpdateIssueLabelsFullSetReplace is test #3: exactly one PUT with the
// correct FULL set. Forgejo replaces, not deltas, so an unrelated label already
// on the issue (keep-me) must survive into the PUT, and the target is computed
// client-side as current − remove + add.
func TestForgejoUpdateIssueLabelsFullSetReplace(t *testing.T) {
	var putIDs []int64
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/labels": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 3, "name": "Doing"}, {"id": 4, "name": "Upcoming"},
				{"id": 5, "name": "keep-me"}, {"id": 6, "name": "In Progress"},
			})
		},
		"/repos/acme/widgets/issues/11/labels": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				// Currently on the issue: "In Progress" and an unrelated "keep-me".
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 6, "name": "In Progress"}, {"id": 5, "name": "keep-me"},
				})
			case http.MethodPut:
				var body struct {
					Labels []int64 `json:"labels"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				putIDs = body.Labels
				_ = json.NewEncoder(w).Encode([]map[string]any{})
			}
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	// Move "In Progress" -> "Doing"; keep-me is untouched by uzi but must survive.
	err := d.UpdateIssueLabels(context.Background(), 7, 11, []string{"Doing"}, []string{"In Progress"})
	if err != nil {
		t.Fatalf("UpdateIssueLabels: %v", err)
	}
	if m.labelPUTs != 1 {
		t.Fatalf("expected exactly one PUT, got %d", m.labelPUTs)
	}
	want := map[int64]bool{3: true, 5: true} // Doing (3) + keep-me (5); In Progress (6) removed
	if len(putIDs) != len(want) {
		t.Fatalf("expected the full set {Doing, keep-me}, got ids %v", putIDs)
	}
	for _, id := range putIDs {
		if !want[id] {
			t.Fatalf("unexpected label id %d in PUT set %v (keep-me must survive, In Progress must go)", id, putIDs)
		}
	}
}

// TestForgejoUpdateIssueLabelsNoOp is test #4: when the target set already equals
// the current set, the driver must issue ZERO PUTs.
func TestForgejoUpdateIssueLabelsNoOp(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/11/labels": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 3, "name": "Doing"}})
				return
			}
			t.Errorf("no-op move must not PUT (method %s)", r.Method)
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	// Add "Doing" (already present) and remove nothing: target == current.
	if err := d.UpdateIssueLabels(context.Background(), 7, 11, []string{"Doing"}, nil); err != nil {
		t.Fatalf("UpdateIssueLabels: %v", err)
	}
	if m.labelPUTs != 0 {
		t.Fatalf("a no-op move must issue zero PUTs, got %d", m.labelPUTs)
	}
}

func TestForgejoEnsureLabelsCreatesOnlyMissing(t *testing.T) {
	created := map[string]string{}
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/labels": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 6, "name": "In Progress", "color": "1f75cb"}})
			case http.MethodPost:
				var body struct {
					Name  string `json:"name"`
					Color string `json:"color"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				created[body.Name] = body.Color
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "name": body.Name, "color": body.Color})
			}
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	err := d.EnsureLabels(context.Background(), 7, []Label{
		{Name: "In Progress", Color: "#1f75cb"},
		{Name: "Upcoming", Color: "#6699cc"},
		{Name: "NoColor"}, // must not fail Forgejo's color validator: driver defaults it
	})
	if err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if _, ok := created["In Progress"]; ok {
		t.Error("existing label In Progress should not be re-created")
	}
	if created["Upcoming"] == "" {
		t.Error("missing label Upcoming should be created with its color")
	}
	if created["NoColor"] == "" {
		t.Errorf("a color-less label must be created with a default color, got %q", created["NoColor"])
	}
}

func TestForgejoUserExists(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/users/alice": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 77, "login": "alice"})
		},
		"/users/ghost": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "user does not exist"})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	if exists, err := d.UserExists(context.Background(), "alice"); err != nil || !exists {
		t.Fatalf("alice: got (%v, %v), want (true, nil)", exists, err)
	}
	if exists, err := d.UserExists(context.Background(), "ghost"); err != nil || exists {
		t.Fatalf("ghost (404): got (%v, %v), want (false, nil)", exists, err)
	}
	if exists, err := d.UserExists(context.Background(), "  "); err != nil || exists {
		t.Errorf("blank username: got (%v, %v), want (false, nil)", exists, err)
	}
}

// TestForgejoProjectRoleWrite is the good case: a write collaborator maps to
// RoleWrite and member=true, which the checker reads as "no finding".
func TestForgejoProjectRoleWrite(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/users/search": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("uid") != "4242" {
				t.Errorf("expected uid=4242 lookup, got %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": 4242, "login": "uzi-bot"}}})
		},
		"/repos/acme/widgets/collaborators/uzi-bot/permission": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": "write", "role_name": "Write", "user": map[string]any{"id": 4242, "login": "uzi-bot"}})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	role, member, err := d.ProjectRole(context.Background(), 7, 4242)
	if err != nil {
		t.Fatalf("ProjectRole: %v", err)
	}
	if role != RoleWrite || !member {
		t.Fatalf("expected role=write member=true, got role=%q member=%v", role, member)
	}
}

// TestForgejoProjectRoleRemovedFromPublicRepo is D7 / test #7: a bot removed from
// a PUBLIC repo gets 200 with permission:"read" (NOT a 404). member must be
// derived from the payload — a 404-based check would read this as a fine member
// and never fire the "no longer a member" finding.
func TestForgejoProjectRoleRemovedFromPublicRepo(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/users/search": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": 4242, "login": "uzi-bot"}}})
		},
		"/repos/acme/widgets/collaborators/uzi-bot/permission": func(w http.ResponseWriter, _ *http.Request) {
			// 200, not 404: the public-repo baseline for a non-collaborator.
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": "read", "role_name": "Read"})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	role, member, err := d.ProjectRole(context.Background(), 7, 4242)
	if err != nil {
		t.Fatalf("ProjectRole: %v", err)
	}
	if role != RoleRead {
		t.Fatalf("expected role=read, got %q", role)
	}
	if member {
		t.Fatal("a read-only (removed-on-public) bot must derive member=false, so the finding fires (D7)")
	}
}

// TestForgejoProjectRoleUserGone covers the 404 arm: the collaborator route 404s
// only when the user does not exist (or was removed from a PRIVATE repo). That is
// not-a-member with a nil error, never a crash.
func TestForgejoProjectRoleUserGone(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/users/search": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": 4242, "login": "uzi-bot"}}})
		},
		"/repos/acme/widgets/collaborators/uzi-bot/permission": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "not found"})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	role, member, err := d.ProjectRole(context.Background(), 7, 4242)
	if err != nil {
		t.Fatalf("a 404 (user gone) must be a nil error, got %v", err)
	}
	if member || role != RoleNone {
		t.Fatalf("expected not-a-member (role none, member false), got role=%q member=%v", role, member)
	}
}

// TestForgejoRoleForPermission pins the permission → neutral Role mapping.
func TestForgejoRoleForPermission(t *testing.T) {
	for _, tc := range []struct {
		perm string
		want Role
	}{
		{"none", RoleNone}, {"read", RoleRead}, {"write", RoleWrite},
		{"admin", RoleAdmin}, {"owner", RoleOwner},
	} {
		if got := roleForForgejoPermission(gitea.AccessMode(tc.perm)); got != tc.want {
			t.Errorf("roleForForgejoPermission(%q) = %q, want %q", tc.perm, got, tc.want)
		}
	}
}

// TestForgejoBranchProtectionAuthoritative is test #9: the reader-gated
// GET /branches/{branch} answers push/merge for the calling bot. user_can_push /
// user_can_merge false → no finding; either true → a finding. The driver must
// NEVER call branch_protections/{name} (a write bot 403s it, degrading the
// guardrail to a warning — D6).
func TestForgejoBranchProtectionAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protected bool
		canPush   bool
		canMerge  bool
		wantPush  bool
		wantMerge bool
	}{
		{"protected-and-locked", true, false, false, false, false},
		{"protected-but-pushable", true, true, false, true, false},
		{"protected-but-mergeable", true, false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"name": "main", "protected": tc.protected,
						"user_can_push": tc.canPush, "user_can_merge": tc.canMerge,
						"effective_branch_protection_name": "",
					})
				},
				"/repos/acme/widgets/branch_protections/main": func(w http.ResponseWriter, _ *http.Request) {
					// A write bot 403s this admin-gated route; the driver must never hit it.
					t.Error("driver must not call branch_protections/{name}")
					w.WriteHeader(http.StatusForbidden)
				},
			})
			d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

			bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
			if err != nil {
				t.Fatalf("DefaultBranchProtection: %v", err)
			}
			if !bp.Protected {
				t.Fatalf("expected Protected=true, got %+v", bp)
			}
			if bp.WriteRoleCanPush != tc.wantPush || bp.WriteRoleCanMerge != tc.wantMerge {
				t.Fatalf("push/merge mapping wrong: got %+v, want push=%v merge=%v", bp, tc.wantPush, tc.wantMerge)
			}
			// The GitLab per-user-grant fields have no Forgejo signal; they stay clear.
			if bp.BotCanPush || bp.BotCanMerge {
				t.Fatalf("Bot* fields must stay false on Forgejo, got %+v", bp)
			}
		})
	}
}

// TestForgejoBranchProtectionUnprotectedIsNotSafe is R12 / test #9b: an
// unprotected branch (Forgejo's protected:false early-return, user_can_push /
// user_can_merge true) must report the strongest finding, identically to the
// GitLab driver — WriteRole* true, so no consumer can read false,false as safe.
func TestForgejoBranchProtectionUnprotectedIsNotSafe(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "main", "protected": false,
				"user_can_push": true, "user_can_merge": true,
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("DefaultBranchProtection: %v", err)
	}
	if bp.Protected {
		t.Fatal("expected Protected=false on an unprotected branch")
	}
	if !bp.WriteRoleCanPush || !bp.WriteRoleCanMerge {
		t.Fatalf("an unprotected branch admits write push+merge; reporting false inverts the guardrail: %+v", bp)
	}
}

// TestForgejoBranchProtectionMissingBranch covers the 404 arm: a branch that does
// not exist is reported as unprotected-and-open (the not-safe shape), matching
// the GitLab driver's 404 handling rather than a misleading zero value.
func TestForgejoBranchProtectionMissingBranch(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "branch does not exist"})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 4242)
	if err != nil {
		t.Fatalf("a 404 branch must be a nil error, got %v", err)
	}
	if bp.Protected || !bp.WriteRoleCanPush || !bp.WriteRoleCanMerge {
		t.Fatalf("a missing branch must report unprotected-and-open, got %+v", bp)
	}
}

// TestForgejoErrorsAreRedacted is test #12: every error path must route through
// the PAT redactor. The mock echoes the token in each error body (worst case);
// none may surface it. Slug resolution (/repositories/7) succeeds here, so each
// method fails at its OWN terminal endpoint — proving the deepest error paths
// redact, including the privcheck-critical ProjectRole and
// DefaultBranchProtection, plus the version-gate transport error and the
// slug-resolution path (via ListLabels against an id with no repo).
func TestForgejoErrorsAreRedacted(t *testing.T) {
	const token = "forgejo-supersecret-redaction-probe-0123456789"
	leak := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom " + token})
	}
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/version":                          leak, // VerifyToken (version transport)
		"/user/repos":                       leak, // ListProjects
		"/users/alice":                      leak, // UserExists
		"/repositories/9":                   leak, // slug resolution for a bad project id
		"/repos/acme/widgets/branches/main": leak, // DefaultBranchProtection (past slug)
		"/users/search": func(w http.ResponseWriter, _ *http.Request) { // ProjectRole gets past user resolution
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": 4242, "login": "uzi-bot"}}})
		},
		"/repos/acme/widgets/collaborators/uzi-bot/permission": leak, // ProjectRole terminal
	})
	d := newForgejoDriver(t, m, token)
	ctx := context.Background()

	type check struct {
		name string
		err  error
	}
	_, e1 := d.VerifyToken(ctx)
	_, e2 := d.ListProjects(ctx)
	_, e3 := d.UserExists(ctx, "alice")
	_, e4 := d.ListLabels(ctx, 9) // fails at slug resolution (/repositories/9 leaks)
	_, e5 := d.DefaultBranchProtection(ctx, 7, "main", 4242)
	_, _, e6 := d.ProjectRole(ctx, 7, 4242)

	for _, c := range []check{
		{"VerifyToken", e1}, {"ListProjects", e2}, {"UserExists", e3},
		{"ListLabels(slug)", e4}, {"DefaultBranchProtection", e5}, {"ProjectRole", e6},
	} {
		if c.err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if strings.Contains(c.err.Error(), token) {
			t.Errorf("%s leaked the PAT: %q", c.name, c.err.Error())
		}
	}
}

// TestCheckForgejoVersion unit-tests the gate helper directly, independent of the
// HTTP round-trip, so the D4a comparison semantics are pinned in isolation.
func TestCheckForgejoVersion(t *testing.T) {
	f := newForgejo("https://example.test", "unused-token-placeholder", time.Second)
	for _, tc := range []struct {
		reported string
		wantErr  bool
	}{
		{"16.0.0+gitea-1.22.0", false},
		{"v16.0.0", false},
		{"16.0.1", false},
		{"17.0.0-dev-3-abc+gitea-1.22.0", false},
		{"16.0.0-dev-626-32363b81+gitea-1.22.0", true},
		{"15.0.4", true},
		{"1.21.0-rc1", true},
		{"32363b81+gitea-1.22.0", true},
		{"", true},
	} {
		err := f.checkForgejoVersion(tc.reported)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkForgejoVersion(%q): err=%v, wantErr=%v", tc.reported, err, tc.wantErr)
		}
		if err != nil && !errors.Is(err, ErrForgeVersionUnsupported) {
			t.Errorf("checkForgejoVersion(%q) must wrap ErrForgeVersionUnsupported, got %q", tc.reported, err.Error())
		}
	}
}

// TestForgejoVersionStringRedacted covers the auditor's defense-in-depth point:
// the /version string is the server's self-reported, UNTRUSTED value. A hostile
// instance that holds the PAT could reflect it into the version field; the
// unparseable refusal must not leak it. Asserts the token is scrubbed from the
// error while the sentinel and its errors.Is still survive.
func TestForgejoVersionStringRedacted(t *testing.T) {
	const token = "forgejo-token-reflected-into-version-0123456789"
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/version": versionHandler(token), // {"version":"<token>"}
	})
	d := newForgejoDriver(t, m, token)

	_, err := d.VerifyToken(context.Background())
	if err == nil {
		t.Fatal("a token-as-version string is unparseable and must be refused")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the untrusted version string leaked the PAT: %q", err.Error())
	}
	if !errors.Is(err, ErrForgeVersionUnsupported) {
		t.Errorf("refusal must still wrap ErrForgeVersionUnsupported, got %q", err.Error())
	}
}

// --- M4: merge requests, timeline, notes, token introspection --------------

// TestForgejoGetMergeRequestStateMapping is test #6, verified live on 16.0.0:
// Forgejo says state:"open" (not GitLab's "opened") and carries merged separately,
// so a merged PR is {state:"closed", merged:true}. No "locked" state.
func TestForgejoGetMergeRequestStateMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		merged bool
		want   string
	}{
		{"open", "open", false, MRStateOpened},
		{"merged", "closed", true, MRStateMerged}, // merged wins over closed
		{"closed-not-merged", "closed", false, MRStateClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/pulls/13": func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("expected GET, got %s", r.Method)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": 5005, "number": 13, "state": tc.state, "merged": tc.merged,
						"html_url": "https://fj/acme/widgets/pulls/13",
					})
				},
			})
			d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

			mr, err := d.GetMergeRequest(context.Background(), 7, 13)
			if err != nil {
				t.Fatalf("GetMergeRequest: %v", err)
			}
			if mr.IID != 13 || mr.State != tc.want {
				t.Fatalf("unexpected MR: got %+v, want state %q", mr, tc.want)
			}
			if mr.WebURL != "https://fj/acme/widgets/pulls/13" {
				t.Fatalf("unexpected MR web url: %q", mr.WebURL)
			}
		})
	}
}

// TestForgejoListIssueLabelEventsMapping is test #5 and the R6 convention pin:
// a label event records body=="1" for add, body=="" for remove (verified live on
// 16.0.0). Forgejo serializes the event's label as a SINGLE object, which is why
// the driver hand-parses rather than using the SDK's []*Label-typed timeline. A
// non-label timeline entry (a plain comment) must be filtered out, and order is
// oldest-first, matching the GitLab driver.
func TestForgejoListIssueLabelEventsMapping(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/11/timeline": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			if r.Header.Get("Authorization") != "token forgejo-abcdefabcdef" {
				t.Errorf("hand-rolled GET must carry the token: %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				// A plain comment: must be filtered out (not a label event).
				{"id": 500, "type": "comment", "body": "just a note",
					"user": map[string]any{"login": "dave"}, "created_at": "2026-07-04T08:00:00Z"},
				// label add: body "1", single-object label.
				{"id": 501, "type": "label", "body": "1", "created_at": "2026-07-04T09:00:00Z",
					"user":  map[string]any{"id": 42, "login": "carol"},
					"label": map[string]any{"id": 9, "name": "autopilot", "color": "00aabb"}},
				// label remove: body "".
				{"id": 502, "type": "label", "body": "", "created_at": "2026-07-04T10:00:00Z",
					"user":  map[string]any{"id": 42, "login": "carol"},
					"label": map[string]any{"id": 9, "name": "autopilot", "color": "00aabb"}},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	events, err := d.ListIssueLabelEvents(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("ListIssueLabelEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 label events (the comment filtered out), got %d: %+v", len(events), events)
	}
	add := events[0]
	if add.ID != 501 || add.Action != "add" || add.LabelName != "autopilot" || add.Username != "carol" {
		t.Fatalf("unexpected add event: %+v", add)
	}
	if add.CreatedAt.IsZero() {
		t.Error("expected a non-zero CreatedAt on the add event")
	}
	if events[1].Action != "remove" {
		t.Errorf("expected the second event to be a remove (body \"\"), got %q", events[1].Action)
	}
}

// TestForgejoLabelActionConvention pins the R6 body convention in isolation, so a
// future SDK/Forgejo change to it fails loudly rather than silently attributing
// every event as a remove.
func TestForgejoLabelActionConvention(t *testing.T) {
	if forgejoLabelAction("1") != "add" {
		t.Errorf(`body "1" must map to add`)
	}
	if forgejoLabelAction("") != "remove" {
		t.Errorf(`body "" must map to remove`)
	}
	// Any other body is a remove (only "1" means add), a conservative default.
	if forgejoLabelAction("0") != "remove" {
		t.Errorf(`only body "1" is an add; anything else is a remove`)
	}
}

// TestForgejoListIssueComments pins PRD #381 M1 for Forgejo: a two-comment
// response maps to two neutral IssueComments in oldest-first order (gitea's list
// is already ASC and human-only, so no filter is needed) with author id/username/
// body carried through.
func TestForgejoListIssueComments(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/11/comments": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 601, "body": "please guard on Valid",
					"user":       map[string]any{"id": 42, "login": "carol"},
					"created_at": "2026-07-04T09:00:00Z", "updated_at": "2026-07-04T09:00:00Z"},
				{"id": 602, "body": "and revise the existing test",
					"user":       map[string]any{"id": 43, "login": "dave"},
					"created_at": "2026-07-04T10:00:00Z", "updated_at": "2026-07-04T10:00:00Z"},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	comments, err := d.ListIssueComments(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d: %+v", len(comments), comments)
	}
	if comments[0].AuthorForgeUserID != 42 || comments[0].AuthorUsername != "carol" || comments[0].Body != "please guard on Valid" {
		t.Fatalf("unexpected first comment: %+v", comments[0])
	}
	if comments[0].CreatedAt.IsZero() {
		t.Error("expected a non-zero CreatedAt on the first comment")
	}
	if comments[1].AuthorUsername != "dave" || comments[1].Body != "and revise the existing test" {
		t.Fatalf("unexpected second comment: %+v", comments[1])
	}
	if !comments[0].CreatedAt.Before(comments[1].CreatedAt) {
		t.Errorf("comments not oldest-first: %v then %v", comments[0].CreatedAt, comments[1].CreatedAt)
	}
}

func TestForgejoCreateIssueNoteSendsBody(t *testing.T) {
	var gotBody string
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/11/comments": func(w http.ResponseWriter, r *http.Request) {
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
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

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

// TestForgejoTokenInfoParsesScopes covers the hand-rolled introspection (D5): the
// driver identifies the bot (GET /user), then GETs its token list and picks the
// entry whose token_last_eight matches the PAT it authenticates with. Forgejo
// reports no active flag and no expiry, so Active is true and ExpiresAt is zero;
// scopes come back reordered (verified live) but are returned verbatim for the
// checker to compare as a set.
func TestForgejoTokenInfoParsesScopes(t *testing.T) {
	const token = "forgejo-pat-aaaa-bbbb-11112222"
	last8 := token[len(token)-8:]
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "uzi-bot", "is_admin": false})
		},
		"/users/uzi-bot/tokens": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			if r.Header.Get("Authorization") != "token "+token {
				t.Errorf("hand-rolled GET must carry the token: %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				// A different token the bot also owns: must be skipped.
				{"id": 1, "name": "other", "token_last_eight": "99998888", "scopes": []string{"read:user"}},
				// The authenticating token (matching last-eight), scopes reordered.
				{"id": 2, "name": "uzi", "token_last_eight": last8,
					"scopes": []string{"write:issue", "write:repository", "read:user"}},
			})
		},
	})
	d := newForgejoDriver(t, m, token)

	info, err := d.TokenInfo(context.Background())
	if err != nil {
		t.Fatalf("TokenInfo: %v", err)
	}
	if !info.Active {
		t.Error("a listed, matching, authenticating token must be Active")
	}
	if !info.ExpiresAt.IsZero() {
		t.Error("Forgejo PATs report no expiry; ExpiresAt must be zero")
	}
	want := map[string]bool{"write:issue": true, "write:repository": true, "read:user": true}
	if len(info.Scopes) != len(want) {
		t.Fatalf("expected the matching token's 3 scopes, got %v", info.Scopes)
	}
	for _, s := range info.Scopes {
		if !want[s] {
			t.Fatalf("unexpected scope %q in %v", s, info.Scopes)
		}
	}
}

// TestForgejoTokenInfoAmbiguousCollisionFailsSafe covers the fail-SAFE path: if
// two of the bot's own tokens share token_last_eight (the only fingerprint the API
// exposes), the driver cannot tell which one authenticated and must NOT pick the
// first — an over-scoped authenticating token could otherwise be masked by a
// correctly-scoped sibling, sliding it past D6b's blocking scope check. It returns
// an error, which the checker downgrades to a warning (honest yellow, not false
// green). Mutation check: revert TokenInfo to pick-first and this test reddens.
func TestForgejoTokenInfoAmbiguousCollisionFailsSafe(t *testing.T) {
	const token = "forgejo-pat-aaaa-bbbb-11112222"
	last8 := token[len(token)-8:]
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "uzi-bot", "is_admin": false})
		},
		"/users/uzi-bot/tokens": func(w http.ResponseWriter, _ *http.Request) {
			// Two tokens collide on the last eight: one over-scoped ("all"), one clean.
			// Picking either would be a guess; the driver must refuse.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "name": "godmode", "token_last_eight": last8, "scopes": []string{"all"}},
				{"id": 2, "name": "clean", "token_last_eight": last8,
					"scopes": []string{"write:issue", "write:repository", "read:user"}},
			})
		},
	})
	d := newForgejoDriver(t, m, token)

	info, err := d.TokenInfo(context.Background())
	if err == nil {
		t.Fatalf("a last-eight collision must fail safe (error), not pick a match; got %+v", info)
	}
	// The error must not leak the token and must be generic enough for the checker to
	// downgrade to a warning (it is not the introspection-unsupported sentinel).
	if strings.Contains(err.Error(), token) {
		t.Errorf("collision error leaked the PAT: %q", err.Error())
	}
}

// TestForgejoM4ErrorsAreRedacted is test #12 for the M4 methods, including the two
// HAND-ROLLED paths (timeline, token introspection) that bypass the SDK — the lead
// called these out specifically. Each endpoint echoes the token in a 500 body; no
// method may surface it.
func TestForgejoM4ErrorsAreRedacted(t *testing.T) {
	const token = "forgejo-m4-redaction-probe-0123456789"
	leak := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom " + token})
	}
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) { // TokenInfo gets past bot identification
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "uzi-bot", "is_admin": false})
		},
		"/repos/acme/widgets/pulls/13":           leak, // GetMergeRequest (SDK)
		"/repos/acme/widgets/issues/11/comments": leak, // CreateIssueNote (SDK)
		"/repos/acme/widgets/issues/11/timeline": leak, // ListIssueLabelEvents (hand-rolled)
		"/users/uzi-bot/tokens":                  leak, // TokenInfo (hand-rolled)
	})
	d := newForgejoDriver(t, m, token)
	ctx := context.Background()

	type check struct {
		name string
		err  error
	}
	_, e1 := d.GetMergeRequest(ctx, 7, 13)
	_, e2 := d.CreateIssueNote(ctx, 7, 11, "x")
	_, e3 := d.ListIssueLabelEvents(ctx, 7, 11)
	_, e4 := d.TokenInfo(ctx)

	for _, c := range []check{
		{"GetMergeRequest", e1}, {"CreateIssueNote", e2},
		{"ListIssueLabelEvents(hand-rolled)", e3}, {"TokenInfo(hand-rolled)", e4},
	} {
		if c.err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if strings.Contains(c.err.Error(), token) {
			t.Errorf("%s leaked the PAT: %q", c.name, c.err.Error())
		}
	}
}

// TestForgejoRawGetLimitedStopsAtLimit is the PRD #917 M2 (epic finding S1) pin for
// the byte cap that folding rawGet into rawGetLimited introduced. rawGetLimited is
// now THE raw-GET helper for the three SDK-bypassing endpoints (issue timeline,
// token introspection, job logs); this asserts its io.LimitReader actually bounds the
// TRANSFER, so a hostile forge streaming a multi-GB body cannot OOM the api — the read
// stops at limit bytes and the returned body length is <= limit, never the whole body.
// A small local limit exercises the cap without allocating gigabytes (mirroring the
// job-log over-cap technique in forgejo_pipelines_test.go, which serves a body larger
// than the cap and asserts the read stops). The positive control proves a normal-size
// body under the limit is returned in full, so the cap does not truncate legitimate
// responses.
func TestForgejoRawGetLimitedStopsAtLimit(t *testing.T) {
	const oversized = 4096 // the served body, deliberately larger than the test cap
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/rawprobe": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("A", oversized)))
		},
		"/repos/acme/widgets/rawsmall": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("small-body"))
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")
	f, ok := d.(*forgejo)
	if !ok {
		t.Fatalf("expected *forgejo, got %T", d)
	}

	// Oversized: the io.LimitReader must stop the transfer at the cap.
	const cap64 = int64(64)
	body, err := f.rawGetLimited(context.Background(), "/repos/acme/widgets/rawprobe", cap64)
	if err != nil {
		t.Fatalf("rawGetLimited (oversized): %v", err)
	}
	if int64(len(body)) > cap64 {
		t.Fatalf("read must stop at the cap: got %d bytes, want <= %d", len(body), cap64)
	}
	if int64(len(body)) != cap64 {
		t.Fatalf("an oversized body must be capped to exactly the limit: got %d, want %d", len(body), cap64)
	}

	// Positive control: a body under the cap is returned whole (no truncation).
	got, err := f.rawGetLimited(context.Background(), "/repos/acme/widgets/rawsmall", maxTraceBytes+1)
	if err != nil {
		t.Fatalf("rawGetLimited (normal): %v", err)
	}
	if string(got) != "small-body" {
		t.Fatalf("a normal-size body must be returned in full, got %q", got)
	}
}

// TestForgejoListIssueLabelEventsCapsOversizedTimeline is the end-to-end S1 pin for
// the timeline call site, one of the two former unbounded-rawGet callers now routed
// through rawGetLimited(maxTraceBytes+1). A hostile forge streaming a timeline body
// LARGER than the cap must not be buffered whole: the transfer stops at
// maxTraceBytes+1 bytes and the oversized "A"-run fails JSON parsing, producing the
// same redacted parse-error shape rawGetLimited feeds ListIssueLabelEvents today
// (never a panic or OOM). Mirrors forgejo_pipelines_test.go's over-cap technique:
// serve maxTraceBytes+2 bytes rather than actually streaming gigabytes.
func TestForgejoListIssueLabelEventsCapsOversizedTimeline(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/11/timeline": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat("A", maxTraceBytes+2)))
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	events, err := d.ListIssueLabelEvents(context.Background(), 7, 11)
	if err == nil {
		t.Fatalf("an oversized, unparseable timeline body must error, got %d events", len(events))
	}
	if events != nil {
		t.Fatalf("no events must be returned on the capped/parse-failure path, got %+v", events)
	}
	// The parse failure is routed through the redactor (forgejo-prefixed), never a raw
	// panic; a token was not in play here, so just assert the forgejo op-context shape.
	if !strings.Contains(err.Error(), "forgejo:") {
		t.Fatalf("expected a redacted forgejo-prefixed error, got %q", err.Error())
	}
}

// TestForgejoListIssuesMapsAssignees pins PRD #767 M1: Forgejo/Gitea's inline
// `assignees` array (sdk User.ID int64, nil-guarded) round-trips into
// forge.Issue.Assignees, and an unassigned issue yields a non-nil empty slice.
func TestForgejoListIssuesMapsAssignees(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 100, "number": 11, "title": "assigned", "state": "open",
					"html_url":  "https://fj/acme/widgets/issues/11",
					"assignees": []map[string]any{{"id": 42, "login": "bot"}, {"id": 99}}},
				{"id": 101, "number": 12, "title": "unassigned", "state": "open",
					"html_url": "https://fj/acme/widgets/issues/12"},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	issues, err := d.ListIssues(context.Background(), 7, ListIssuesOptions{})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
	if got := issues[0].Assignees; len(got) != 2 || got[0] != 42 || got[1] != 99 {
		t.Fatalf("assignee ids not mapped: %v", got)
	}
	if issues[1].Assignees == nil {
		t.Fatal("unassigned issue must yield a non-nil empty Assignees slice")
	}
	if len(issues[1].Assignees) != 0 {
		t.Fatalf("unassigned issue must have no assignees, got %v", issues[1].Assignees)
	}
}
