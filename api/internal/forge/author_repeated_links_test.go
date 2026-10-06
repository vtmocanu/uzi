package forge

import (
	"context"
	"net/http"
	"testing"
)

func TestAuthorRepeatedLinkHeaderValues(t *testing.T) {
	for _, provider := range []string{"github", "forgejo", "forgejo-ownership"} {
		for _, tc := range []struct {
			name, last string
			incomplete bool
		}{
			{"missing-continuation", `<http://example.test/?page=2>; rel="last"`, true},
			{"complete-terminal-page", `<http://example.test/?page=1>; rel="last"`, false},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				body := `[{"id":42,"login":"current"}]`
				if provider == "github" {
					body = `[{"id":42,"login":"current","permissions":{"pull":true,"triage":true,"push":false,"maintain":false,"admin":false}}]`
				}
				if provider == "forgejo-ownership" {
					body = "[]"
				}
				repeated := func(w http.ResponseWriter, r *http.Request) {
					w.Header().Add("Link", `<http://example.test/?page=1>; rel="first"`)
					w.Header().Add("Link", tc.last)
					authorJSON(body)(w, r)
				}
				var d Forge
				if provider == "github" {
					m := newMockGitHub(t, map[string]http.HandlerFunc{
						"/user/42":                          authorJSON(`{"id":42,"login":"current"}`),
						"/repos/acme/widgets/collaborators": repeated,
					})
					d = newGitHubDriver(t, m, "fixture")
				} else {
					handlers := map[string]http.HandlerFunc{
						"/users/search":                                        authorJSON(`{"data":[{"id":42,"login":"current"}],"ok":true}`),
						"/repos/acme/widgets/teams":                            authorJSON("[]"),
						"/repos/acme/widgets/collaborators":                    repeated,
						"/repos/acme/widgets/collaborators/current/permission": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) },
					}
					if provider == "forgejo-ownership" {
						handlers["/repos/acme/widgets/teams"] = repeated
						handlers["/repos/acme/widgets/collaborators"] = authorJSON("[]")
						handlers["/repos/acme/widgets/collaborators/current/permission"] = authorJSON(`{"permission":"read","user":{"id":42,"login":"current"}}`)
					}
					d = newForgejoDriver(t, newMockForgejo(t, handlers), "fixture")
				}
				want := AuthorEligible
				if provider == "forgejo-ownership" {
					want = AuthorNotEligible
				}
				if tc.incomplete {
					want = AuthorUnknown
				}
				assertAuthor(t, d, BeginAuthorAssessment(context.Background()), 42, want)
			})
		}
	}
}
