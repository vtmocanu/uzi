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

func TestForgejoAuthorEvidence(t *testing.T) {
	personal := `{"message":"repo is not owned by an organization"}`
	org := `[{"id":3,"name":"read-team","permission":"read","units":["repo.pulls"]},{"id":4,"name":"mixed-team","permission":"write"}]`
	for _, tc := range []struct {
		name                            string
		id                              int64
		user, teams, direct, permission string
		teamsStatus, permissionStatus   int
		want                            AuthorEligibility
	}{
		{"personal-owner", 1, `{"id":1,"login":"acme"}`, personal, "[]", "", 405, 403, AuthorEligible},
		{"personal-nonmember", 42, `{"id":42,"login":"current"}`, personal, "[]", "", 405, 403, AuthorNotEligible},
		{"direct-read", 42, `{"id":42,"login":"current"}`, org, `[{"id":42,"login":"current"}]`, "", 200, 403, AuthorEligible},
		{"direct-with-unknown-type", 42, `{"id":42,"login":"current"}`, "{}", `[{"id":42,"login":"current"}]`, "", 405, 403, AuthorEligible},
		{"team-write", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"write","user":{"id":42,"login":"current"}}`, 200, 200, AuthorEligible},
		{"team-admin", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"admin","user":{"id":42,"login":"current"}}`, 200, 200, AuthorEligible},
		{"team-owner-wire-value", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"owner","user":{"id":42,"login":"current"}}`, 200, 200, AuthorEligible},
		{"read-teams-never-inferred", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"read","user":{"id":42,"login":"current"}}`, 200, 200, AuthorNotEligible},
		{"none", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"none","user":{"id":42,"login":"current"}}`, 200, 200, AuthorNotEligible},
		{"write-bot-forbidden", 42, `{"id":42,"login":"current"}`, org, "[]", "", 200, 403, AuthorUnknown},
		{"concealed", 42, `{"id":42,"login":"current"}`, org, "[]", "", 200, 404, AuthorUnknown},
		{"rate", 42, `{"id":42,"login":"current"}`, org, "[]", "", 200, 429, AuthorUnknown},
		{"arbitrary-405", 1, `{"id":1,"login":"acme"}`, `{"message":"method not allowed"}`, "[]", "", 405, 403, AuthorUnknown},
		{"title-is-not-message", 1, `{"id":1,"login":"acme"}`, `{"title":"repo is not owned by an organization"}`, "[]", "", 405, 403, AuthorUnknown},
		{"unknown-type-public-read", 42, `{"id":42,"login":"current"}`, "null", "[]", `{"permission":"read","user":{"id":42,"login":"current"}}`, 200, 200, AuthorUnknown},
		{"ownership-forbidden", 42, `{"id":42,"login":"current"}`, "{}", "[]", "", 403, 403, AuthorUnknown},
		{"malformed-ownership", 42, `{"id":42,"login":"current"}`, "[{}]", "[]", "", 200, 403, AuthorUnknown},
		{"renamed-identity", 42, `{"id":43,"login":"current"}`, personal, "[]", "", 405, 403, AuthorUnknown},
		{"permission-identity-mismatch", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"owner","user":{"id":43,"login":"current"}}`, 200, 200, AuthorUnknown},
		{"permission-login-mismatch", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"owner","user":{"id":42,"login":"reused"}}`, 200, 200, AuthorUnknown},
		{"missing-permission", 42, `{"id":42,"login":"current"}`, org, "[]", `{"user":{"id":42,"login":"current"}}`, 200, 200, AuthorUnknown},
		{"invalid-permission", 42, `{"id":42,"login":"current"}`, org, "[]", `{"permission":"triage","user":{"id":42,"login":"current"}}`, 200, 200, AuthorUnknown},
		{"malformed-permission", 42, `{"id":42,"login":"current"}`, org, "[]", "{", 200, 200, AuthorUnknown},
		{"malformed-direct", 42, `{"id":42,"login":"current"}`, personal, "[{}]", "", 405, 403, AuthorUnknown},
		{"null-direct", 42, `{"id":42,"login":"current"}`, personal, "null", "", 405, 403, AuthorUnknown},
		{"direct-reused-login", 42, `{"id":42,"login":"current"}`, org, `[{"id":42,"login":"reused"}]`, "", 200, 403, AuthorUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			permissionCalls := 0
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/users/search": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("uid") != fmt.Sprint(tc.id) {
						t.Error("lookup not stable ID")
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []json.RawMessage{json.RawMessage(tc.user)}, "ok": true})
				},
				"/repos/acme/widgets/teams": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.teamsStatus)
					_, _ = w.Write([]byte(tc.teams))
				},
				"/repos/acme/widgets/collaborators": authorJSON(tc.direct),
				"/repos/acme/widgets/collaborators/": func(w http.ResponseWriter, r *http.Request) {
					permissionCalls++
					if !strings.HasSuffix(r.URL.Path, "/permission") {
						t.Error("unexpected collaborator inference")
					}
					w.WriteHeader(tc.permissionStatus)
					_, _ = w.Write([]byte(tc.permission))
				},
				"/orgs/": func(w http.ResponseWriter, _ *http.Request) {
					t.Error("organization membership lookup forbidden")
					w.WriteHeader(500)
				},
				"/repos/acme/widgets/assignees": func(w http.ResponseWriter, _ *http.Request) {
					t.Error("assignee inference forbidden")
					w.WriteHeader(500)
				},
			})
			assertAuthor(t, newForgejoDriver(t, m, "fixture"), context.Background(), tc.id, tc.want)
			if (tc.name == "personal-owner" || tc.name == "personal-nonmember" || tc.name == "direct-read") && permissionCalls != 0 {
				t.Error("unnecessary effective query")
			}
		})
	}
}

func TestForgejoAuthorPaginationAndFreshMetadata(t *testing.T) {
	pages, repos, teams := 0, 0, 0
	broken := false
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repositories/7": func(w http.ResponseWriter, _ *http.Request) {
			repos++
			_, _ = w.Write([]byte(`{"id":7,"name":"widgets","full_name":"acme/widgets","owner":{"id":1,"login":"acme"}}`))
		},
		"/users/search": func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.URL.Query().Get("uid"), 10, 64)
			if err != nil || id <= 0 {
				http.Error(w, "invalid user ID", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": id, "login": fmt.Sprintf("user%d", id)}}, "ok": true})
		},
		"/repos/acme/widgets/teams": func(w http.ResponseWriter, _ *http.Request) {
			teams++
			w.WriteHeader(405)
			_, _ = w.Write([]byte(`{"message":"repo is not owned by an organization"}`))
		},
		"/repos/acme/widgets/collaborators": func(w http.ResponseWriter, r *http.Request) {
			pages++
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<http://example.test/?page=2>; rel="next"`)
				_, _ = w.Write([]byte(`[{"id":42,"login":"user42"}]`))
			} else if broken {
				w.WriteHeader(403)
			} else {
				_, _ = w.Write([]byte("[]"))
			}
		},
		"/repos/acme/widgets/collaborators/user42/permission": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) },
	})
	d := newForgejoDriver(t, m, "fixture")
	ctx := BeginAuthorAssessment(context.Background())
	assertAuthor(t, d, ctx, 42, AuthorEligible)
	assertAuthor(t, d, ctx, 43, AuthorNotEligible)
	if pages != 2 || repos != 1 || teams != 1 {
		t.Fatalf("counts: %d %d %d", pages, repos, teams)
	}
	broken = true
	assertAuthor(t, d, BeginAuthorAssessment(context.Background()), 42, AuthorUnknown)
	if pages != 4 || repos != 2 || teams != 2 {
		t.Fatalf("fresh counts: %d %d %d", pages, repos, teams)
	}
	assertAuthor(t, d, ctx, 42, AuthorEligible)
}

func TestForgejoIndependentEvidenceSurvivesIncompleteDirectList(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/users/search":             authorJSON(`{"data":[{"id":42,"login":"current"}],"ok":true}`),
		"/repos/acme/widgets/teams": authorJSON("[]"),
		"/repos/acme/widgets/collaborators": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<http://example.test/?page=2>; rel="next"`)
				_, _ = w.Write([]byte("[]"))
			} else {
				w.WriteHeader(403)
			}
		},
		"/repos/acme/widgets/collaborators/current/permission": authorJSON(`{"permission":"owner","user":{"id":42,"login":"current"}}`),
	})
	assertAuthor(t, newForgejoDriver(t, m, "fixture"), context.Background(), 42, AuthorEligible)
}
