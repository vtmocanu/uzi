package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func authorJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}
func assertAuthor(t *testing.T, d Forge, ctx context.Context, id int64, want AuthorEligibility) {
	t.Helper()
	got, err := d.RepositoryAuthorEligibility(ctx, 7, id)
	if got != want || (err != nil) != (want == AuthorUnknown) {
		t.Fatalf("author %d got=%v err=%v want=%v", id, got, err, want)
	}
}

func TestGitHubAuthorBaseThresholds(t *testing.T) {
	for _, role := range []string{"pull", "triage", "push", "maintain", "admin", "custom"} {
		t.Run(role, func(t *testing.T) {
			p := map[string]bool{"pull": true, "triage": false, "push": false, "maintain": false, "admin": false}
			if role != "custom" {
				p[role] = true
			}
			payload, _ := json.Marshal([]any{map[string]any{"id": 42, "login": "renamed", "permissions": p, "role_name": "custom-admin", "permission": "read", "author_association": "OWNER"}})
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/user/42": authorJSON(`{"id":42,"login":"renamed"}`),
				"/repos/acme/widgets/collaborators": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("affiliation") != "all" {
						t.Error("missing affiliation=all")
					}
					_, _ = w.Write(payload)
				},
			})
			want := AuthorEligible
			if role == "pull" || role == "custom" {
				want = AuthorNotEligible
			}
			assertAuthor(t, newGitHubDriver(t, m, "fixture"), context.Background(), 42, want)
		})
	}
}

func TestGitHubAuthorUnknownEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, user, list string
		status           int
	}{
		{"deleted", "", "[]", 404},
		{"reused", `{"id":43,"login":"old"}`, "[]", 200},
		{"missing-id", `{"login":"old"}`, "[]", 200},
		{"null-list", `{"id":42,"login":"old"}`, "null", 200},
		{"missing-permissions", `{"id":42,"login":"old"}`, `[{"id":42,"login":"old"}]`, 200},
		{"partial-permissions", `{"id":42,"login":"old"}`, `[{"id":42,"login":"old","permissions":{"pull":true,"triage":true,"push":false,"maintain":false}}]`, 200},
		{"malformed", `{"id":42,"login":"old"}`, "{", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/user/42": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.user))
				},
				"/repos/acme/widgets/collaborators": authorJSON(tc.list),
			})
			assertAuthor(t, newGitHubDriver(t, m, "fixture"), context.Background(), 42, AuthorUnknown)
		})
	}
	for _, status := range []int{403, 404, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/user/42":                          authorJSON(`{"id":42,"login":"current"}`),
				"/repos/acme/widgets/collaborators": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) },
			})
			assertAuthor(t, newGitHubDriver(t, m, "fixture"), context.Background(), 42, AuthorUnknown)
		})
	}
}

func TestGitHubAuthorPaginationCacheIsOperationScoped(t *testing.T) {
	pages := 0
	incomplete := false
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user/": func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v3/user/"), 10, 64)
			if err != nil || id <= 0 {
				http.Error(w, "invalid user ID", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "login": fmt.Sprintf("user%d", id)})
		},
		"/repos/acme/widgets/collaborators": func(w http.ResponseWriter, r *http.Request) {
			pages++
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<http://example.test/?page=2>; rel="next"`)
				_, _ = w.Write([]byte(`[{"id":42,"login":"user42","permissions":{"pull":true,"triage":true,"push":false,"maintain":false,"admin":false}}]`))
			} else if incomplete {
				w.WriteHeader(403)
			} else {
				_, _ = w.Write([]byte("[]"))
			}
		},
	})
	d := newGitHubDriver(t, m, "fixture")
	ctx := BeginAuthorAssessment(context.Background())
	assertAuthor(t, d, ctx, 42, AuthorEligible)
	assertAuthor(t, d, ctx, 43, AuthorNotEligible)
	if pages != 2 {
		t.Fatalf("pages=%d", pages)
	}
	incomplete = true
	assertAuthor(t, d, BeginAuthorAssessment(context.Background()), 42, AuthorUnknown)
	if pages != 4 {
		t.Fatalf("fresh operation pages=%d", pages)
	}
	// The completed valid list of the first operation remains usable.
	assertAuthor(t, d, ctx, 42, AuthorEligible)
}

func TestGitLabAuthorEffectiveMembership(t *testing.T) {
	for _, tc := range []struct {
		name, member string
		status       int
		want         AuthorEligibility
	}{
		{"guest-custom", `{"id":42,"username":"current","state":"active","access_level":10,"member_role":{"base_access_level":10,"read_code":true}}`, 200, AuthorNotEligible},
		{"no-access", `{"id":42,"username":"current","state":"active","access_level":0}`, 200, AuthorNotEligible},
		{"planner", `{"id":42,"username":"current","state":"active","access_level":15}`, 200, AuthorNotEligible},
		{"reporter", `{"id":42,"username":"current","state":"active","access_level":20}`, 200, AuthorEligible},
		{"security-manager", `{"id":42,"username":"current","state":"active","access_level":25}`, 200, AuthorEligible},
		{"developer", `{"id":42,"username":"current","state":"active","access_level":30}`, 200, AuthorEligible},
		{"blocked", `{"id":42,"username":"current","state":"blocked","access_level":40}`, 200, AuthorNotEligible},
		{"inactive", `{"id":42,"username":"current","state":"active","membership_state":"awaiting","access_level":40}`, 200, AuthorNotEligible},
		{"expired", `{"id":42,"username":"current","state":"active","access_level":20,"expires_at":"2000-01-01"}`, 200, AuthorNotEligible},
		{"absent", "", 404, AuthorNotEligible},
		{"forbidden", "", 403, AuthorUnknown},
		{"rate", "", 429, AuthorUnknown},
		{"mismatch", `{"id":43,"username":"current","state":"active","access_level":20}`, 200, AuthorUnknown},
		{"missing-access", `{"id":42,"username":"current","state":"active"}`, 200, AuthorUnknown},
		{"invalid-access", `{"id":42,"username":"current","state":"active","access_level":21}`, 200, AuthorUnknown},
		{"malformed", "{", 200, AuthorUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7": authorJSON(`{"id":7}`),
				"/api/v4/users/42":   authorJSON(`{"id":42,"username":"current","state":"active"}`),
				"/api/v4/projects/7/members/all/42": func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.member))
				},
			})
			assertAuthor(t, newTestDriver(t, m, "fixture"), context.Background(), 42, tc.want)
		})
	}
}
