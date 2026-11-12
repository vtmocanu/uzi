package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func appTestDriver(t *testing.T, f *appAuthFixture) *github {
	t.Helper()
	g, err := newGitHubWithApp(f.auth.apiURL, "app-fixture-cache-identity", time.Second, f.auth)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func requireAppInstallationAuth(t *testing.T, f *appAuthFixture, r *http.Request) {
	t.Helper()
	f.auth.mu.Lock()
	expected := f.auth.token
	f.auth.mu.Unlock()
	if expected == "" || r.Header.Get("Authorization") != "Bearer "+expected {
		t.Error("GitHub driver did not use the current installation token")
	}
}

func appTestWriteJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode driver fixture: %v", err)
	}
}

func TestGitHubAppDriverDiscoversInstallationRepositoriesWithoutUserPermissions(t *testing.T) {
	f := newAppAuthFixture(t)
	// The minted credential is scoped to two selected repos. The API's list response
	// deliberately contains no permissions.push booleans, unlike /user/repos.
	f.auth.config.Repositories = []string{"widgets", "services"}
	pages := []string{}
	f.serve = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
			return false
		}
		requireAppInstallationAuth(t, f, r)
		if r.URL.Path != "/api/v3/installation/repositories" {
			t.Errorf("unexpected repository-discovery endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("installation discovery did not use bounded pagination")
		}
		if page == "1" {
			w.Header().Set("Link", "<"+f.auth.apiURL+"/api/v3/installation/repositories?per_page=100&page=2>; rel=\"next\"")
			appTestWriteJSON(t, w, map[string]any{"repositories": []map[string]any{{"id": int64(1), "name": "widgets", "full_name": "acme/widgets", "html_url": "https://github.com/acme/widgets", "default_branch": "main"}}})
		} else {
			appTestWriteJSON(t, w, map[string]any{"repositories": []map[string]any{{"id": int64(2), "name": "services", "full_name": "acme/tools", "html_url": "https://github.com/acme/tools", "default_branch": "main"}, {"id": int64(3), "name": "unselected", "full_name": "acme/unselected"}}})
		}
		return true
	}
	// Priming the token isolates discovery from the minting tests' one-repo scope.
	f.auth.token = "ghs_" + "selected-repositories-fixture"
	f.auth.expires = f.now.Add(time.Hour)
	f.auth.permissions = githubAppPermissions()
	f.auth.repositoryIDs = map[int64]bool{1: true, 2: true}
	g := appTestDriver(t, f)
	projects, err := g.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pages, []string{"1", "2"}) {
		t.Fatalf("installation pagination was not followed: %v", pages)
	}
	expected := []Project{{ForgeProjectID: 1, PathWithNamespace: "acme/widgets", WebURL: "https://github.com/acme/widgets", DefaultBranch: "main"}, {ForgeProjectID: 2, PathWithNamespace: "acme/tools", WebURL: "https://github.com/acme/tools", DefaultBranch: "main"}}
	if !reflect.DeepEqual(projects, expected) {
		t.Fatalf("selected installation projects mismatch: %#v", projects)
	}
}

func TestGitHubAppDriverVerifiesPublicBotIdentity(t *testing.T) {
	for _, bad := range []string{"", "wrong-login", "wrong-type", "site-admin", "missing-id"} {
		t.Run(bad, func(t *testing.T) {
			f := newAppAuthFixture(t)
			f.serve = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
					return false
				}
				requireAppInstallationAuth(t, f, r)
				if r.URL.Path != "/api/v3/users/multica-test[bot]" {
					t.Errorf("bot identity lookup used %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return true
				}
				body := map[string]any{"id": int64(512), "login": "multica-test[bot]", "type": "Bot", "site_admin": false}
				switch bad {
				case "wrong-login":
					body["login"] = "another-app[bot]"
				case "wrong-type":
					body["type"] = "User"
				case "site-admin":
					body["site_admin"] = true
				case "missing-id":
					delete(body, "id")
				}
				appTestWriteJSON(t, w, body)
				return true
			}
			identity, err := appTestDriver(t, f).VerifyToken(context.Background())
			if bad != "" {
				if err == nil {
					t.Fatal("invalid App bot identity accepted")
				}
				return
			}
			if err != nil || identity.ForgeUserID != 512 || identity.Username != "multica-test[bot]" || identity.IsAdmin {
				t.Fatalf("verified App identity mismatch: %#v err=%v", identity, err)
			}
		})
	}
}

func TestGitHubAppDriverProjectRoleUsesEffectiveInstallationPermissions(t *testing.T) {
	for _, mode := range []string{"selected", "not-visible", "outside-selection", "wrong-repository-id", "same-name-outside-installation", "invalid-permissions"} {
		t.Run(mode, func(t *testing.T) {
			f := newAppAuthFixture(t)
			f.serve = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
					return false
				}
				requireAppInstallationAuth(t, f, r)
				if r.URL.Path != "/api/v3/repositories/1" {
					t.Errorf("unexpected role endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return true
				}
				if mode == "not-visible" {
					w.WriteHeader(http.StatusNotFound)
					return true
				}
				body := map[string]any{"id": int64(1), "name": "widgets", "full_name": "acme/widgets"}
				if mode == "outside-selection" {
					body["name"] = "payments"
				}
				if mode == "wrong-repository-id" {
					body["id"] = int64(2)
				}
				appTestWriteJSON(t, w, body)
				return true
			}
			g := appTestDriver(t, f)
			if _, _, err := f.auth.credential(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "invalid-permissions" {
				f.auth.permissions["contents"] = "read"
			}
			if mode == "same-name-outside-installation" {
				f.auth.repositoryIDs = map[int64]bool{99: true}
			}
			role, member, err := g.ProjectRole(context.Background(), 1, 512)
			if mode == "invalid-permissions" {
				if err == nil || member {
					t.Fatal("weaker effective token was treated as Write")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "selected" {
				if role != RoleWrite || !member {
					t.Fatal("selected installation repository lacked Write role")
				}
			} else if role != RoleNone || member {
				t.Fatal("unselected or inaccessible repository was treated as a member")
			}
		})
	}
}

func TestGitHubAppDriverTokenInfoIsExplicitAndDetached(t *testing.T) {
	f := newAppAuthFixture(t)
	info, err := appTestDriver(t, f).TokenInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"actions:read", "administration:read", "checks:read", "contents:write", "issues:write", "metadata:read", "pull_requests:write", "statuses:read"}
	if !info.GitHubApp || !info.Active || !info.ExpiresAt.Equal(f.now.Add(time.Hour)) || !reflect.DeepEqual(info.Scopes, expected) || !GitHubAppPermissionsValid(info.AppPermissions) {
		t.Fatalf("App token info misrepresented effective permission scope: %#v", info)
	}
	info.AppPermissions["contents"] = "read"
	if f.auth.permissions["contents"] != "write" {
		t.Fatal("TokenInfo exposed mutable authentication state")
	}
}

func TestGitHubAppDriverRenewsSDKAncestryAndArtifactAuthentication(t *testing.T) {
	f := newAppAuthFixture(t)
	tokens := []string{}
	f.serve = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
			return false
		}
		requireAppInstallationAuth(t, f, r)
		tokens = append(tokens, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v3/repositories/1":
			appTestWriteJSON(t, w, map[string]any{"id": int64(1), "name": "widgets", "owner": map[string]string{"login": "acme"}})
		case "/api/v3/repos/acme/widgets/branches/main":
			appTestWriteJSON(t, w, map[string]any{"name": "main", "commit": map[string]string{"sha": strings.Repeat("a", 40)}})
		case "/api/v3/repos/acme/widgets/actions/runs/7/artifacts":
			appTestWriteJSON(t, w, map[string]any{"total_count": 0, "artifacts": []any{}})
		default:
			t.Errorf("unexpected credential-renewal endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		return true
	}
	g := appTestDriver(t, f)
	if _, _, err := g.ProjectRole(context.Background(), 1, 512); err != nil {
		t.Fatal(err)
	}
	first := f.auth.token
	f.now = f.now.Add(56 * time.Minute)
	sha, err := g.BranchHead(context.Background(), 1, "main")
	if err != nil || sha != strings.Repeat("a", 40) {
		t.Fatalf("raw ancestry request failed: %v", err)
	}
	second := f.auth.token
	if first == second {
		t.Fatal("ancestry used expiring token")
	}
	f.now = f.now.Add(56 * time.Minute)
	response, err := g.artifactGET(context.Background(), "repos/acme/widgets/actions/runs/7/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	third := f.auth.token
	if second == third {
		t.Fatal("raw artifact request used expiring token")
	}
	f.now = f.now.Add(56 * time.Minute)
	if _, _, err := g.ProjectRole(context.Background(), 1, 512); err != nil {
		t.Fatal(err)
	}
	fourth := f.auth.token
	if third == fourth || f.mints.Load() != 4 {
		t.Fatal("SDK transport did not renew installation authentication")
	}
	if len(tokens) != 5 || tokens[0] != "Bearer "+first || tokens[1] != "Bearer "+second || tokens[2] != "Bearer "+second || tokens[3] != "Bearer "+third || tokens[4] != "Bearer "+fourth {
		t.Fatal("one of the driver paths retained an earlier installation token")
	}
}
