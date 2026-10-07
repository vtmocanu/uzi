package forge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestIssueAuthorIdentityThroughPublicMappings(t *testing.T) {
	for _, kind := range []string{"github", "gitlab", "forgejo"} {
		for _, author := range []string{"stable", "missing", "deleted"} {
			t.Run(kind+"/"+author, func(t *testing.T) {
				id := int64(42)
				authorBody := `{"id":42,"login":"renamed","username":"renamed"}`
				if author == "missing" {
					id = 0
					authorBody = "null"
				}
				if author == "deleted" {
					id = 0
					authorBody = `{"login":"deleted","username":"deleted"}`
				}
				field := "user"
				if kind == "gitlab" {
					field = "author"
				}
				body := fmt.Sprintf(`{"id":11,"number":11,"iid":11,"title":"issue","state":"open","%s":%s}`, field, authorBody)
				handler := func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost || strings.HasSuffix(r.URL.Path, "/11") {
						_, _ = w.Write([]byte(body))
					} else {
						_, _ = fmt.Fprintf(w, "[%s]", body)
					}
				}
				var d Forge
				switch kind {
				case "github":
					m := newMockGitHub(t, map[string]http.HandlerFunc{
						"/repos/acme/widgets/issues":    handler,
						"/repos/acme/widgets/issues/11": handler,
					})
					d = newGitHubDriver(t, m, "fixture")
				case "gitlab":
					m := newMockGitLab(t, map[string]http.HandlerFunc{
						"/api/v4/projects/7/issues":    handler,
						"/api/v4/projects/7/issues/11": handler,
					})
					d = newTestDriver(t, m, "fixture")
				case "forgejo":
					// The Gitea Issue's Poster decodes the wire "user" field.
					m := newMockForgejo(t, map[string]http.HandlerFunc{
						"/repos/acme/widgets/issues":    handler,
						"/repos/acme/widgets/issues/11": handler,
					})
					d = newForgejoDriver(t, m, "fixture")
				}
				ctx := context.Background()
				single, err := d.GetIssue(ctx, 7, 11)
				if err != nil || single.AuthorForgeUserID != id {
					t.Fatalf("get: %+v %v", single, err)
				}
				list, err := d.ListIssues(ctx, 7, ListIssuesOptions{})
				if err != nil || len(list) != 1 || list[0].AuthorForgeUserID != id {
					t.Fatalf("list: %+v %v", list, err)
				}
				created, err := d.CreateIssue(ctx, 7, "title", "description", nil)
				if err != nil || created.AuthorForgeUserID != id {
					t.Fatalf("create: %+v %v", created, err)
				}
				if author == "stable" && (single.Author != "renamed" || list[0].Author != "renamed" || created.Author != "renamed") {
					t.Fatal("author login lost")
				}
			})
		}
	}
}

func TestAuthorMalformedPaginationDoesNotAuthorize(t *testing.T) {
	for _, kind := range []string{"github", "forgejo"} {
		t.Run(kind, func(t *testing.T) {
			list := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", `<http://example.test/?page=garbage>; rel="next"`)
				_, _ = w.Write([]byte(`[{"id":42,"login":"current","permissions":{"pull":true,"triage":true,"push":true,"maintain":false,"admin":false}}]`))
			}
			var d Forge
			if kind == "github" {
				m := newMockGitHub(t, map[string]http.HandlerFunc{
					"/user/42":                          authorJSON(`{"id":42,"login":"current"}`),
					"/repos/acme/widgets/collaborators": list,
				})
				d = newGitHubDriver(t, m, "fixture")
			} else {
				m := newMockForgejo(t, map[string]http.HandlerFunc{
					"/users/search":                                        authorJSON(`{"data":[{"id":42,"login":"current"}],"ok":true}`),
					"/repos/acme/widgets/teams":                            authorJSON("[]"),
					"/repos/acme/widgets/collaborators":                    list,
					"/repos/acme/widgets/collaborators/current/permission": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) },
				})
				d = newForgejoDriver(t, m, "fixture")
			}
			assertAuthor(t, d, context.Background(), 42, AuthorUnknown)
		})
	}
}

func TestGitLabUnresolvedAuthorAndConcealedProject(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		status     int
	}{
		{"deleted", "", 404},
		{"reused", `{"id":43,"username":"current","state":"active"}`, 200},
		{"missing", `{"username":"current","state":"active"}`, 200},
		{"concealed-project", `{"id":42,"username":"current","state":"active"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/users/42": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.user))
				},
				"/api/v4/projects/7/members/all/42": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) },
				"/api/v4/projects/7":                func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) },
			})
			assertAuthor(t, newTestDriver(t, m, "fixture"), context.Background(), 42, AuthorUnknown)
		})
	}
}

func TestForgejoDeletedAndMismatchedRepository(t *testing.T) {
	for _, tc := range []struct{ name, user, repo string }{
		{"deleted", `{"data":[],"ok":true}`, `{"id":7}`},
		{"repo-mismatch", `{"data":[{"id":42,"login":"current"}],"ok":true}`, `{"id":8,"name":"widgets","full_name":"acme/widgets","owner":{"id":1,"login":"acme"}}`},
		{"owner-missing-id", `{"data":[{"id":42,"login":"current"}],"ok":true}`, `{"id":7,"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}`},
		{"address-mismatch", `{"data":[{"id":42,"login":"current"}],"ok":true}`, `{"id":7,"name":"widgets","full_name":"other/widgets","owner":{"id":1,"login":"acme"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/users/search":   authorJSON(tc.user),
				"/repositories/7": authorJSON(tc.repo),
			})
			assertAuthor(t, newForgejoDriver(t, m, "fixture"), context.Background(), 42, AuthorUnknown)
		})
	}
}
