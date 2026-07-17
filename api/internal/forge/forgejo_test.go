package forge

import (
	"context"
	"encoding/json"
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
		{"16.0.0+gitea-1.22.0", true},  // the release; build metadata must not defeat the compare
		{"16.0.1", true},               // patch above the floor
		{"16.1.0", true},               // minor above the floor
		{"17.0.0", true},               // major above the floor
		{"17.0.0-dev-3-abc123+gitea-1.22.0", true}, // major dominates: v17 cycle carries the v16 surface
		{"16.0.0-dev-626-32363b81+gitea-1.22.0", false}, // codeberg live: prerelease < release, refuse
		{"15.0.4", false},              // below the floor (route genuinely absent)
		{"15.0.5+gitea-1.22.0", false}, // below the floor
		{"1.21.11-2", false},           // legacy 1.x scheme
		{"1.21.0-rc1", false},          // legacy 1.x rc
		{"32363b81+gitea-1.22.0", false}, // bare sha from `git describe --always`: unparseable
		{"", false},                      // empty: unparseable
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
					"html_url": "https://fj/acme/widgets/pulls/12",
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
				"labels": []map[string]any{{"id": 3, "name": "PRD"}},
				"body":   "body links prds/4-agent-runtime-workers.md",
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
		name       string
		protected  bool
		canPush    bool
		canMerge   bool
		wantPush   bool
		wantMerge  bool
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
		"/version":     leak, // VerifyToken (version transport)
		"/user/repos":  leak, // ListProjects
		"/users/alice": leak, // UserExists
		"/repositories/9": leak, // slug resolution for a bad project id
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
		err := checkForgejoVersion(tc.reported)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkForgejoVersion(%q): err=%v, wantErr=%v", tc.reported, err, tc.wantErr)
		}
	}
}
