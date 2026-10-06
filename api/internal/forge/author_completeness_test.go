package forge

import (
	"context"
	"net/http"
	"testing"
)

func TestAuthorIncompleteHTTPReads(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "forgejo"} {
		reads := []string{"identity", "repository", "collaborators"}
		if provider == "gitlab" {
			reads = []string{"identity", "membership", "absence-project"}
		}
		if provider == "forgejo" {
			reads = append(reads, "permission", "ownership")
		}
		for _, read := range reads {
			for _, variant := range []string{"206", "200-content-range", "206-content-range"} {
				t.Run(provider+"/"+read+"/"+variant, func(t *testing.T) {
					handlers := map[string]http.HandlerFunc{}
					target := ""
					var d Forge
					switch provider {
					case "github":
						target = map[string]string{"identity": "/user/42", "repository": "/repositories/7", "collaborators": "/repos/acme/widgets/collaborators"}[read]
						handlers["/user/42"] = authorJSON(`{"id":42,"login":"current"}`)
						handlers["/repositories/7"] = authorJSON(`{"id":7,"name":"widgets","owner":{"login":"acme"}}`)
						handlers["/repos/acme/widgets/collaborators"] = authorJSON(`[{"id":42,"login":"current","permissions":{"pull":true,"triage":true,"push":false,"maintain":false,"admin":false}}]`)
					case "gitlab":
						target = map[string]string{"identity": "/api/v4/users/42", "membership": "/api/v4/projects/7/members/all/42", "absence-project": "/api/v4/projects/7"}[read]
						handlers["/api/v4/users/42"] = authorJSON(`{"id":42,"username":"current","state":"active"}`)
						handlers["/api/v4/projects/7"] = authorJSON(`{"id":7}`)
						handlers["/api/v4/projects/7/members/all/42"] = authorJSON(`{"id":42,"username":"current","state":"active","access_level":20}`)
						if read == "absence-project" {
							handlers["/api/v4/projects/7/members/all/42"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }
						}
					case "forgejo":
						target = map[string]string{"identity": "/users/search", "repository": "/repositories/7", "collaborators": "/repos/acme/widgets/collaborators", "permission": "/repos/acme/widgets/collaborators/current/permission", "ownership": "/repos/acme/widgets/teams"}[read]
						handlers["/users/search"] = authorJSON(`{"data":[{"id":42,"login":"current"}],"ok":true}`)
						handlers["/repositories/7"] = authorJSON(`{"id":7,"name":"widgets","full_name":"acme/widgets","owner":{"id":1,"login":"acme"}}`)
						handlers["/repos/acme/widgets/teams"] = authorJSON("[]")
						handlers["/repos/acme/widgets/collaborators"] = authorJSON(`[{"id":42,"login":"current"}]`)
						handlers["/repos/acme/widgets/collaborators/current/permission"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }
						if read == "permission" || read == "ownership" {
							handlers["/repos/acme/widgets/collaborators"] = authorJSON("[]")
							handlers["/repos/acme/widgets/collaborators/current/permission"] = authorJSON(`{"permission":"write","user":{"id":42,"login":"current"}}`)
							if read == "ownership" {
								handlers["/repos/acme/widgets/collaborators/current/permission"] = authorJSON(`{"permission":"read","user":{"id":42,"login":"current"}}`)
							}
						}
					}
					original := handlers[target]
					handlers[target] = func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if variant != "206" {
							w.Header().Set("Content-Range", "items 0-0/2")
						}
						if variant != "200-content-range" {
							w.WriteHeader(http.StatusPartialContent)
						}
						original(w, r)
					}
					switch provider {
					case "github":
						d = newGitHubDriver(t, newMockGitHub(t, handlers), "fixture")
					case "gitlab":
						d = newTestDriver(t, newMockGitLab(t, handlers), "fixture")
					case "forgejo":
						d = newForgejoDriver(t, newMockForgejo(t, handlers), "fixture")
					}
					ctx := BeginAuthorAssessment(context.Background())
					assertAuthor(t, d, ctx, 42, AuthorUnknown)
					assertAuthor(t, d, ctx, 42, AuthorUnknown)
				})
			}
		}
	}
}

func TestAuthorCollaboratorPaginationMetadata(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, tc := range []struct {
			name, link string
			want       AuthorEligibility
		}{
			{"last-without-next", `<http://example.test/?page=2>; rel="last"`, AuthorUnknown},
			{"last-missing-page", `<http://example.test/>; rel="last"`, AuthorUnknown},
			{"last-malformed-page", `<http://example.test/?page=wat>; rel="last"`, AuthorUnknown},
			{"last-backward", `<http://example.test/?page=0>; rel="last"`, AuthorUnknown},
			{"last-duplicate", `<http://example.test/?page=1>; rel="last", <http://example.test/?page=1>; rel="last"`, AuthorUnknown},
			{"last-duplicate-page", `<http://example.test/?page=1&page=2>; rel="last"`, AuthorUnknown},
			{"next-duplicate", `<http://example.test/?page=2>; rel="next", <http://example.test/?page=2>; rel="next"`, AuthorUnknown},
			{"next-backward", `<http://example.test/?page=1>; rel="next"`, AuthorUnknown},
			{"last-malformed-relation", `<http://example.test/?page=2>; rel="last`, AuthorUnknown},
			{"terminal-second-first-prev-last", `<http://example.test/?page=2>; rel="next", <http://example.test/?page=2>; rel="last"`, AuthorEligible},
			{"second-page-last-backward", `<http://example.test/?page=2>; rel="next"`, AuthorUnknown},
			{"last-before-next", `<http://example.test/?page=2>; rel="next", <http://example.test/?page=1>; rel="last"`, AuthorUnknown},
			{"terminal-first-prev-last", `<http://example.test/?page=1>; rel="first", <http://example.test/?page=1>; rel="last"`, AuthorEligible},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				handlers := map[string]http.HandlerFunc{}
				var d Forge
				body := `[{"id":42,"login":"current"}]`
				if provider == "github" {
					body = `[{"id":42,"login":"current","permissions":{"pull":true,"triage":true,"push":false,"maintain":false,"admin":false}}]`
					handlers["/user/42"] = authorJSON(`{"id":42,"login":"current"}`)
				} else {
					handlers["/users/search"] = authorJSON(`{"data":[{"id":42,"login":"current"}],"ok":true}`)
					handlers["/repos/acme/widgets/teams"] = authorJSON("[]")
					handlers["/repos/acme/widgets/collaborators/current/permission"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }
				}
				handlers["/repos/acme/widgets/collaborators"] = func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("page") == "1" {
						w.Header().Set("Link", tc.link)
						authorJSON(body)(w, r)
					} else {
						switch tc.name {
						case "terminal-second-first-prev-last":
							w.Header().Set("Link", `<http://example.test/?page=1>; rel="first", <http://example.test/?page=1>; rel="prev", <http://example.test/?page=2>; rel="last"`)
						case "second-page-last-backward":
							w.Header().Set("Link", `<http://example.test/?page=1>; rel="last"`)
						}
						authorJSON("[]")(w, r)
					}
				}
				if provider == "github" {
					d = newGitHubDriver(t, newMockGitHub(t, handlers), "fixture")
				} else {
					d = newForgejoDriver(t, newMockForgejo(t, handlers), "fixture")
				}
				assertAuthor(t, d, context.Background(), 42, tc.want)
			})
		}
	}
}

// A complete author-specific permission read can authorize independently of a
// partial direct-collaborator read.
func TestForgejoCompletePermissionSurvivesPartialDirectResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPartialContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/users/search":             authorJSON(`{"data":[{"id":42,"login":"current"}],"ok":true}`),
				"/repos/acme/widgets/teams": authorJSON("[]"),
				"/repos/acme/widgets/collaborators": func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Range", "items 0-0/2")
					w.WriteHeader(status)
					authorJSON(`[{"id":42,"login":"current"}]`)(w, r)
				},
				"/repos/acme/widgets/collaborators/current/permission": authorJSON(`{"permission":"write","user":{"id":42,"login":"current"}}`),
			})
			assertAuthor(t, newForgejoDriver(t, m, "fixture"), context.Background(), 42, AuthorEligible)
		})
	}
}
