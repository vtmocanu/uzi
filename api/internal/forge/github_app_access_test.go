package forge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGitHubAppBranchProtection(t *testing.T) {
	const effectiveUpdate = `[{"type":"update","ruleset_id":11}]`
	const safe = `{"id":11,"target":"branch","enforcement":"active","rules":[{"type":"update"}],"bypass_actors":[]}`
	unknown := BranchProtection{Protected: true, ProtectionUnverified: true}
	tests := []struct {
		name      string
		effective string
		ruleset   string
		status    int
		want      BranchProtection
	}{
		{"explicit-empty-bypass-list", effectiveUpdate, safe, 200, BranchProtection{Protected: true}},
		{"omitted-bypass-list", effectiveUpdate, strings.Replace(safe, `,"bypass_actors":[]`, "", 1), 200, unknown},
		{"null-bypass-list", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":null`, 1), 200, unknown},
		{"unreadable-full-ruleset", effectiveUpdate, safe, 403, unknown},
		{"classic-protection-only", `[]`, "", 200, unknown},
		{"force-push-only-is-not-update-protection", `[{"type":"non_fast_forward","ruleset_id":11}]`, safe, 200, unknown},
		{"pr-required-still-allows-approved-merge", `[{"type":"pull_request","ruleset_id":11}]`, safe, 200, unknown},
		{"missing-ruleset-id", `[{"type":"update"}]`, safe, 200, unknown},
		{"wrong-ruleset-id", effectiveUpdate, strings.Replace(safe, `"id":11`, `"id":12`, 1), 200, unknown},
		{"inactive-full-ruleset", effectiveUpdate, strings.Replace(safe, `"active"`, `"evaluate"`, 1), 200, unknown},
		{"wrong-full-target", effectiveUpdate, strings.Replace(safe, `"branch"`, `"tag"`, 1), 200, unknown},
		{"missing-full-update-rule", effectiveUpdate, strings.Replace(safe, `"update"`, `"pull_request"`, 1), 200, unknown},
		{"malformed-effective-rules", `{`, safe, 200, unknown},
		{"malformed-full-ruleset", effectiveUpdate, `{`, 200, unknown},
		{"oversize-full-ruleset", effectiveUpdate, strings.Repeat(" ", int(objectResponseBytes)+1) + safe, 200, unknown},
		{"app-always-bypass", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_id":42,"actor_type":"Integration","bypass_mode":"always"}]`, 1), 200, BranchProtection{Protected: true, BotCanPush: true, BotCanMerge: true}},
		{"app-pr-only-bypass-still-can-merge", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_id":42,"actor_type":"Integration","bypass_mode":"pull_request"}]`, 1), 200, BranchProtection{Protected: true, BotCanMerge: true}},
		{"other-app-bypass", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_id":99,"actor_type":"Integration","bypass_mode":"always"}]`, 1), 200, BranchProtection{Protected: true}},
		{"human-role-is-not-app", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_id":4,"actor_type":"RepositoryRole","bypass_mode":"always"}]`, 1), 200, BranchProtection{Protected: true}},
		{"unknown-actor-kind", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_type":"FutureActor","bypass_mode":"always"}]`, 1), 200, unknown},
		{"missing-integration-identity", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_type":"Integration","bypass_mode":"always"}]`, 1), 200, unknown},
		{"unknown-app-bypass-mode", effectiveUpdate, strings.Replace(safe, `"bypass_actors":[]`, `"bypass_actors":[{"actor_id":42,"actor_type":"Integration","bypass_mode":"future"}]`, 1), 200, unknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
					_, _ = fmt.Fprint(w, `{"name":"main","protected":true}`)
				},
				"/repos/acme/widgets/rules/branches/main": func(w http.ResponseWriter, _ *http.Request) {
					_, _ = fmt.Fprint(w, tc.effective)
				},
				"/repos/acme/widgets/rulesets/11": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("includes_parents") != "true" {
						t.Error("inherited rulesets must be included")
					}
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, tc.ruleset)
				},
			})
			g := newGitHubRawDriver(t, m, "test-installation-credential")
			got, err := g.githubAppBranchProtection(context.Background(), 7, "main", 42)
			if err != nil {
				t.Fatalf("githubAppBranchProtection: %v", err)
			}
			if got != tc.want {
				t.Fatalf("protection = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGitHubAppUnprotectedBranchIsUnsafe(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, `{"name":"main","protected":false}`)
				},
			})
			g := newGitHubRawDriver(t, m, "test-installation-credential")
			got, err := g.githubAppBranchProtection(context.Background(), 7, "main", 42)
			if err != nil || got.Protected || !got.WriteRoleCanPush || !got.WriteRoleCanMerge || got.ProtectionUnverified {
				t.Fatalf("an unprotected or absent branch must be unsafe: %+v, %v", got, err)
			}
		})
	}
}

func TestGitHubAppProtectionRefusesRulesRedirect(t *testing.T) {
	redirected := false
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"name":"main","protected":true}`)
		},
		"/repos/acme/widgets/rules/branches/main": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/api/v3/other-branch", http.StatusFound)
		},
		"/other-branch": func(w http.ResponseWriter, _ *http.Request) {
			redirected = true
			_, _ = fmt.Fprint(w, `[]`)
		},
	})
	g := newGitHubRawDriver(t, m, "test-installation-credential")
	got, err := g.githubAppBranchProtection(context.Background(), 7, "main", 42)
	if err != nil || !got.ProtectionUnverified || redirected {
		t.Fatalf("redirected protection evidence must be refused: %+v, %v, followed=%v", got, err, redirected)
	}
}

func TestGitHubAppProtectionRejectsDifferentBranch(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/branches/main": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"name":"renamed-main","protected":true}`)
		},
	})
	g := newGitHubRawDriver(t, m, "test-installation-credential")
	got, err := g.githubAppBranchProtection(context.Background(), 7, "main", 42)
	if err != nil || !got.ProtectionUnverified {
		t.Fatalf("another branch must not be accepted as protection evidence: %+v, %v", got, err)
	}
}
