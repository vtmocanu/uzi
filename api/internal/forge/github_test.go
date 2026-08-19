package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockGitHub is an httptest server standing in for the GitHub REST API. It records
// the Authorization header it received (GitHub authenticates classic PATs as
// "Bearer <t>") and the number of PUTs to the issue-labels route, and lets each
// test install per-path handlers. Because the driver's test/fake base is injected
// via go-github's WithEnterpriseURLs, the SDK mounts its routes under /api/v3 — so
// route keys are /api/v3-relative (e.g. "/repos/acme/widgets/issues"). A default
// /repositories/7 handler resolves project id 7 → acme/widgets (push:true) unless
// a test overrides it.
//
// This harness is shared: github_pipelines_test.go references it (newMockGitHub /
// newGitHubDriver / newGitHubRawDriver), so it is defined cleanly here.
type mockGitHub struct {
	srv       *httptest.Server
	gotAuth   string
	labelPUTs int
}

func newMockGitHub(t *testing.T, routes map[string]http.HandlerFunc) *mockGitHub {
	t.Helper()
	m := &mockGitHub{}
	mux := http.NewServeMux()
	if _, ok := routes["/repositories/7"]; !ok {
		mux.HandleFunc("/api/v3/repositories/7", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 7, "name": "widgets", "full_name": "acme/widgets",
				"owner":          map[string]any{"id": 1, "login": "acme"},
				"default_branch": "main",
				"permissions":    map[string]any{"push": true, "admin": false, "pull": true},
			})
		})
	}
	for pattern, h := range routes {
		mux.HandleFunc("/api/v3"+pattern, h)
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

func newGitHubDriver(t *testing.T, m *mockGitHub, token string) Forge {
	t.Helper()
	d, err := New(TypeGitHub, m.srv.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// newGitHubRawDriver returns the concrete *github so a test can reach unexported
// fields (JobLogTail's allowInsecureLogHost). The test file is package forge.
func newGitHubRawDriver(t *testing.T, m *mockGitHub, token string) *github {
	t.Helper()
	d, err := newGitHub(m.srv.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("newGitHub: %v", err)
	}
	return d
}

func TestGitHubVerifyToken(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "uzi-bot-test", "site_admin": false})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	id, err := d.VerifyToken(context.Background())
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.ForgeUserID != 4242 || id.Username != "uzi-bot-test" || id.IsAdmin {
		t.Fatalf("unexpected identity: %+v", id)
	}
	// D3: the classic PAT authenticates as "Bearer <t>".
	if m.gotAuth != "Bearer ghp_classicTokenValue1234567890" {
		t.Fatalf("driver sent wrong Authorization header: %q", m.gotAuth)
	}
}

func TestGitHubVerifyTokenReadsSiteAdmin(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "root", "site_admin": true})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	id, err := d.VerifyToken(context.Background())
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !id.IsAdmin {
		t.Fatal("expected IsAdmin=true for a site_admin bot (a downstream violation)")
	}
}

// TestGitHubVerifyTokenRejectsFineGrained pins H4/D3: a github_pat_-prefixed
// (fine-grained) token is refused UP FRONT at VerifyToken with a classic-only
// message and NEVER reaches the /user call, so the connect flow cannot save it
// on an introspection-unsupported warning.
func TestGitHubVerifyTokenRejectsFineGrained(t *testing.T) {
	userHit := false
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			userHit = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "bot"})
		},
	})
	d := newGitHubDriver(t, m, "github_pat_11ABCDEFG0verylongfinegrainedtoken")

	_, err := d.VerifyToken(context.Background())
	if err == nil {
		t.Fatal("a fine-grained github_pat_ token must be refused at VerifyToken")
	}
	if userHit {
		t.Error("a fine-grained token must be rejected BEFORE any authenticated call")
	}
	if !strings.Contains(err.Error(), "classic PAT") {
		t.Errorf("refusal must name the classic-PAT requirement, got %q", err.Error())
	}
}

func TestGitHubListProjectsFiltersPushAndPaginates(t *testing.T) {
	var srvURL string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user/repos": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				// Advertise a next page via the Link header (how go-github paginates).
				w.Header().Set("Link", `<`+srvURL+`/api/v3/user/repos?page=2>; rel="next"`)
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 10, "full_name": "acme/pushable", "html_url": "https://github.com/acme/pushable", "default_branch": "main", "permissions": map[string]any{"push": true}},
					{"id": 11, "full_name": "acme/readonly", "html_url": "https://github.com/acme/readonly", "default_branch": "main", "permissions": map[string]any{"push": false, "pull": true}},
				})
			default:
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 12, "full_name": "acme/page2", "html_url": "https://github.com/acme/page2", "default_branch": "trunk", "permissions": map[string]any{"push": true}},
				})
			}
		},
	})
	srvURL = m.srv.URL
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	projects, err := d.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	// Read-only acme/readonly must be dropped; the two pushable repos across both
	// pages must survive.
	if len(projects) != 2 {
		t.Fatalf("expected 2 pushable projects, got %d: %+v", len(projects), projects)
	}
	if projects[0].ForgeProjectID != 10 || projects[1].ForgeProjectID != 12 {
		t.Fatalf("wrong projects (push filter or pagination broke): %+v", projects)
	}
	if projects[1].DefaultBranch != "trunk" {
		t.Errorf("page-2 project fields not mapped: %+v", projects[1])
	}
}

// TestGitHubListIssuesFiltersPullRequests pins R4: GitHub's /issues returns PRs
// too (each with a non-nil pull_request object), and they must not reach the board.
func TestGitHubListIssuesFiltersPullRequests(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 1, "title": "real issue", "state": "open", "labels": []map[string]any{{"name": "PRD"}}, "user": map[string]any{"login": "alice"}},
				{"number": 2, "title": "a pull request", "state": "open", "pull_request": map[string]any{"html_url": "https://github.com/acme/widgets/pull/2"}},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	issues, err := d.ListIssues(context.Background(), 7, ListIssuesOptions{})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue (PR filtered out), got %d: %+v", len(issues), issues)
	}
	if issues[0].IID != 1 || issues[0].State != "opened" || issues[0].Author != "alice" {
		t.Fatalf("issue mapping wrong: %+v", issues[0])
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "PRD" {
		t.Fatalf("labels not mapped: %+v", issues[0].Labels)
	}
}

func TestGitHubListIssuesStateAllParam(t *testing.T) {
	var gotState string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues": func(w http.ResponseWriter, r *http.Request) {
			gotState = r.URL.Query().Get("state")
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	if _, err := d.ListIssues(context.Background(), 7, ListIssuesOptions{}); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	// StateAll (the zero value) must send state=all EXPLICITLY — GitHub defaults to
	// "open", so a missing param would silently drop closed issues.
	if gotState != "all" {
		t.Fatalf("expected state=all for the default (StateAll), got %q", gotState)
	}
}

// TestGitHubUpdateIssueLabels pins the single-PUT set-replace, the no-op (zero
// PUTs), and that an unrelated label survives.
func TestGitHubUpdateIssueLabels(t *testing.T) {
	var putBody []string
	install := func() *mockGitHub {
		return newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 5, "title": "t", "state": "open",
					"labels": []map[string]any{{"name": "col:todo"}, {"name": "keep-me"}},
				})
			},
			"/repos/acme/widgets/issues/5/labels": func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &putBody)
				_ = json.NewEncoder(w).Encode([]map[string]any{})
			},
		})
	}

	// (1) Move col:todo → col:done; keep-me must survive; exactly one PUT.
	m := install()
	putBody = nil
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	if err := d.UpdateIssueLabels(context.Background(), 7, 5, []string{"col:done"}, []string{"col:todo"}); err != nil {
		t.Fatalf("UpdateIssueLabels: %v", err)
	}
	if m.labelPUTs != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d", m.labelPUTs)
	}
	got := map[string]bool{}
	for _, l := range putBody {
		got[l] = true
	}
	if !got["col:done"] || !got["keep-me"] || got["col:todo"] {
		t.Fatalf("target set wrong (keep-me must survive, col:todo gone): %v", putBody)
	}

	// (2) No-op: adding a label already present issues ZERO PUTs.
	m2 := install()
	d2 := newGitHubDriver(t, m2, "ghp_classicTokenValue1234567890")
	if err := d2.UpdateIssueLabels(context.Background(), 7, 5, []string{"keep-me"}, nil); err != nil {
		t.Fatalf("UpdateIssueLabels no-op: %v", err)
	}
	if m2.labelPUTs != 0 {
		t.Fatalf("a no-op label move must issue ZERO PUTs, got %d", m2.labelPUTs)
	}
}

// TestGitHubUpdateIssueDescription pins S1/item-14: the body actually changes.
func TestGitHubUpdateIssueDescription(t *testing.T) {
	var gotBody string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/9": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				var payload map[string]any
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &payload)
				if v, ok := payload["body"].(string); ok {
					gotBody = v
				}
				// A PATCH that also sent a title would risk clobbering it — assert only
				// body was sent.
				if _, sentTitle := payload["title"]; sentTitle {
					t.Errorf("UpdateIssueDescription must send only the body, not the title: %v", payload)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "title": "t", "state": "open", "body": gotBody})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	if err := d.UpdateIssueDescription(context.Background(), 7, 9, "rewritten body with PRD link"); err != nil {
		t.Fatalf("UpdateIssueDescription: %v", err)
	}
	if gotBody != "rewritten body with PRD link" {
		t.Fatalf("body not changed on the wire, got %q", gotBody)
	}
}

// TestGitHubMRStateMapping pins item-4: GitHub uses state open/closed with a
// SEPARATE merged bool, and merged wins.
func TestGitHubMRStateMapping(t *testing.T) {
	for _, tc := range []struct {
		state  string
		merged bool
		want   string
	}{
		{"open", false, MRStateOpened},
		{"closed", false, MRStateClosed},
		{"closed", true, MRStateMerged},
	} {
		t.Run(tc.state+"-merged="+boolStr(tc.merged), func(t *testing.T) {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/pulls/3": func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number": 3, "state": tc.state, "merged": tc.merged,
						"html_url": "https://github.com/acme/widgets/pull/3",
					})
				},
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
			mr, err := d.GetMergeRequest(context.Background(), 7, 3)
			if err != nil {
				t.Fatalf("GetMergeRequest: %v", err)
			}
			if mr.State != tc.want {
				t.Fatalf("state=%q merged=%v → %q, want %q", tc.state, tc.merged, mr.State, tc.want)
			}
			if mr.IID != 3 {
				t.Fatalf("PR number → IID wrong: %+v", mr)
			}
		})
	}
}

// TestGitHubListIssueLabelEvents pins item-5: labeled→add, unlabeled→remove,
// carrying actor login + created_at + label name; other events skipped.
func TestGitHubListIssueLabelEvents(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/8/events": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 100, "event": "labeled", "created_at": when.Format(time.RFC3339), "actor": map[string]any{"login": "carol"}, "label": map[string]any{"name": "autopilot"}},
				{"id": 101, "event": "closed", "created_at": when.Format(time.RFC3339), "actor": map[string]any{"login": "carol"}},
				{"id": 102, "event": "unlabeled", "created_at": when.Format(time.RFC3339), "actor": map[string]any{"login": "dave"}, "label": map[string]any{"name": "autopilot"}},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	events, err := d.ListIssueLabelEvents(context.Background(), 7, 8)
	if err != nil {
		t.Fatalf("ListIssueLabelEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 label events (closed skipped), got %d: %+v", len(events), events)
	}
	if events[0].Action != "add" || events[0].LabelName != "autopilot" || events[0].Username != "carol" {
		t.Fatalf("labeled event mapped wrong: %+v", events[0])
	}
	if events[1].Action != "remove" || events[1].Username != "dave" {
		t.Fatalf("unlabeled event mapped wrong: %+v", events[1])
	}
	if !events[0].CreatedAt.Equal(when) {
		t.Errorf("created_at not carried: %v", events[0].CreatedAt)
	}
}

// TestGitHubListIssueComments pins PRD #381 M1 for GitHub: a two-comment response
// maps to two neutral IssueComments in oldest-first order (GitHub's issue-comments
// endpoint is human-only and created-ASC by default) with author id/login/body.
func TestGitHubListIssueComments(t *testing.T) {
	older := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/8/comments": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 601, "body": "please guard on Valid", "created_at": older.Format(time.RFC3339), "user": map[string]any{"id": 42, "login": "carol"}},
				{"id": 602, "body": "and revise the existing test", "created_at": newer.Format(time.RFC3339), "user": map[string]any{"id": 43, "login": "dave"}},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	comments, err := d.ListIssueComments(context.Background(), 7, 8)
	if err != nil {
		t.Fatalf("ListIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d: %+v", len(comments), comments)
	}
	if comments[0].AuthorForgeUserID != 42 || comments[0].AuthorUsername != "carol" || comments[0].Body != "please guard on Valid" {
		t.Fatalf("unexpected first comment: %+v", comments[0])
	}
	if !comments[0].CreatedAt.Equal(older) {
		t.Errorf("created_at not carried: %v", comments[0].CreatedAt)
	}
	if comments[1].AuthorUsername != "dave" || comments[1].Body != "and revise the existing test" {
		t.Fatalf("unexpected second comment: %+v", comments[1])
	}
	if !comments[0].CreatedAt.Before(comments[1].CreatedAt) {
		t.Errorf("comments not oldest-first: %v then %v", comments[0].CreatedAt, comments[1].CreatedAt)
	}
}

// TestGitHubProjectRole pins item-6: repo permissions → Role, and the member
// derivation.
func TestGitHubProjectRole(t *testing.T) {
	for _, tc := range []struct {
		name       string
		perms      map[string]any
		wantRole   Role
		wantMember bool
	}{
		{"write", map[string]any{"push": true, "admin": false, "pull": true}, RoleWrite, true},
		{"admin", map[string]any{"push": true, "admin": true, "pull": true}, RoleAdmin, true},
		{"read", map[string]any{"push": false, "admin": false, "pull": true}, RoleRead, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repositories/7": func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": 7, "name": "widgets", "full_name": "acme/widgets",
						"owner": map[string]any{"id": 1, "login": "acme"}, "permissions": tc.perms,
					})
				},
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
			role, member, err := d.ProjectRole(context.Background(), 7, 1)
			if err != nil {
				t.Fatalf("ProjectRole: %v", err)
			}
			if role != tc.wantRole || member != tc.wantMember {
				t.Fatalf("perms %v → role=%q member=%v, want role=%q member=%v", tc.perms, role, member, tc.wantRole, tc.wantMember)
			}
		})
	}
}

func TestGitHubProjectRole404IsNotMember(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repositories/7": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	role, member, err := d.ProjectRole(context.Background(), 7, 1)
	if err != nil {
		t.Fatalf("a 404 must be (RoleNone,false,nil), got err: %v", err)
	}
	if role != RoleNone || member {
		t.Fatalf("404 → role=%q member=%v, want none/false", role, member)
	}
}

// TestGitHubBranchProtection pins item-7 and the D6 dispositions, including that
// the driver NEVER calls the admin-gated /protection endpoint.
func TestGitHubBranchProtection(t *testing.T) {
	protectionHit := false
	branchRoutes := func(protected bool, rulesetBody string) map[string]http.HandlerFunc {
		routes := map[string]http.HandlerFunc{
			"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": protected})
			},
			"/repos/acme/widgets/branches/main/protection": func(w http.ResponseWriter, _ *http.Request) {
				protectionHit = true
				http.Error(w, `{"message":"Must have admin rights"}`, http.StatusForbidden)
			},
		}
		if rulesetBody != "" {
			routes["/repos/acme/widgets/rules/branches/main"] = func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(rulesetBody))
			}
		}
		return routes
	}

	t.Run("unprotected-is-not-safe", func(t *testing.T) {
		protectionHit = false
		m := newMockGitHub(t, branchRoutes(false, "[]"))
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 1)
		if err != nil {
			t.Fatalf("DefaultBranchProtection: %v", err)
		}
		if bp.Protected || !bp.WriteRoleCanPush || !bp.WriteRoleCanMerge || bp.ProtectionUnverified {
			t.Fatalf("unprotected branch must report CanPush/CanMerge true, verified: %+v", bp)
		}
	})

	t.Run("protected-with-ruleset", func(t *testing.T) {
		protectionHit = false
		m := newMockGitHub(t, branchRoutes(true, `[{"type":"pull_request","parameters":{"required_approving_review_count":1}}]`))
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 1)
		if err != nil {
			t.Fatalf("DefaultBranchProtection: %v", err)
		}
		// A readable, enforced ruleset covers the branch: authoritative false, verified.
		if !bp.Protected || bp.WriteRoleCanPush || bp.WriteRoleCanMerge || bp.ProtectionUnverified {
			t.Fatalf("protected+ruleset must be Protected, Can* false, verified: %+v", bp)
		}
	})

	t.Run("protected-no-readable-ruleset", func(t *testing.T) {
		protectionHit = false
		m := newMockGitHub(t, branchRoutes(true, "[]"))
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		bp, err := d.DefaultBranchProtection(context.Background(), 7, "main", 1)
		if err != nil {
			t.Fatalf("DefaultBranchProtection: %v", err)
		}
		// Legacy branch protection: false + ProtectionUnverified true, never a false "safe".
		if !bp.Protected || bp.WriteRoleCanPush || bp.WriteRoleCanMerge || !bp.ProtectionUnverified {
			t.Fatalf("protected+no-ruleset must be Protected, Can* false, ProtectionUnverified true: %+v", bp)
		}
	})

	if protectionHit {
		t.Fatal("the driver must NEVER call the admin-gated /branches/{b}/protection endpoint")
	}
}

// TestGitHubTokenInfo pins the header-based introspection (D7): X-OAuth-Scopes →
// Scopes, expiry header → ExpiresAt, and the fine-grained (no-header) fallback →
// ErrTokenIntrospectionUnsupported.
func TestGitHubTokenInfo(t *testing.T) {
	t.Run("classic-scopes", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/user": func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-OAuth-Scopes", "repo, read:org")
				w.Header().Set("GitHub-Authentication-Token-Expiration", "2027-01-02 15:04:05 UTC")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "bot"})
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		info, err := d.TokenInfo(context.Background())
		if err != nil {
			t.Fatalf("TokenInfo: %v", err)
		}
		if !info.Active {
			t.Error("a 200 call means Active=true")
		}
		if len(info.Scopes) != 2 || info.Scopes[0] != "repo" || info.Scopes[1] != "read:org" {
			t.Fatalf("scopes not parsed from header: %+v", info.Scopes)
		}
		if info.ExpiresAt.IsZero() {
			t.Error("expiry header not parsed")
		}
	})

	t.Run("fine-grained-no-header", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/user": func(w http.ResponseWriter, _ *http.Request) {
				// No X-OAuth-Scopes header at all → un-introspectable.
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "bot"})
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		_, err := d.TokenInfo(context.Background())
		if err != ErrTokenIntrospectionUnsupported {
			t.Fatalf("no scopes header must yield ErrTokenIntrospectionUnsupported, got %v", err)
		}
	})
}

// TestGitHubRedactsToken pins item-11: an error whose body echoes the PAT is
// scrubbed before it leaves the package.
func TestGitHubRedactsToken(t *testing.T) {
	const token = "ghp_secretTokenValue1234567890abcd"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			// A hostile/broken forge reflecting the token into an error body.
			http.Error(w, `{"message":"boom `+token+` boom"}`, http.StatusInternalServerError)
		},
	})
	d := newGitHubDriver(t, m, token)
	_, err := d.VerifyToken(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in error: %q", err.Error())
	}
	if !strings.Contains(err.Error(), redactPlaceholder) {
		t.Errorf("expected the redaction placeholder in %q", err.Error())
	}
}

// TestGitHubRateLimitSurfaced pins item-13: a primary rate-limit 403 surfaces a
// redacted, non-panicking error (not a misread permission failure).
func TestGitHubRateLimitSurfaced(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Reset", "9999999999")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	_, err := d.VerifyToken(context.Background())
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("rate-limit error not recognized/surfaced: %q", err.Error())
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
