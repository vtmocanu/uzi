package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestLinkedHandoffGetIssueIdentity(t *testing.T) {
	for _, driver := range []string{"github", "forgejo"} {
		for _, state := range []string{"open", "closed", "pull_request"} {
			t.Run(driver+"/"+state, func(t *testing.T) {
				payload := map[string]any{"id": 11, "number": 11, "title": "existing issue", "state": state, "labels": []any{}}
				if state == "pull_request" {
					payload["state"] = "open"
					payload["pull_request"] = map[string]any{"url": "https://example.com/pulls/11"}
				}
				route := map[string]http.HandlerFunc{"/repos/acme/widgets/issues/11": func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("method=%s", r.Method)
					}
					_ = json.NewEncoder(w).Encode(payload)
				}}
				var f Forge
				if driver == "github" {
					f = newGitHubDriver(t, newMockGitHub(t, route), "fixture")
				} else {
					f = newForgejoDriver(t, newMockForgejo(t, route), "fixture")
				}
				issue, err := f.GetIssue(context.Background(), 7, 11)
				if err != nil {
					t.Fatal(err)
				}
				if issue.IID != 11 || issue.IsPullRequest != (state == "pull_request") {
					t.Fatalf("issue=%+v", issue)
				}
				if state != "pull_request" && issue.State != map[string]string{"open": "opened", "closed": "closed"}[state] {
					t.Fatalf("state=%q", issue.State)
				}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "IsPullRequest") || strings.Contains(string(raw), "pull_request") || strings.Contains(string(raw), "true") {
					t.Fatalf("internal flag leaked: %s", raw)
				}
			})
		}
	}
}

func TestLinkedHandoffGitLabGetIssueIdentity(t *testing.T) {
	for _, state := range []string{"opened", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := newTestDriver(t, newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/issues/11": func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("method=%s", r.Method)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"iid": 11, "state": state, "title": "existing issue"})
				},
			}), "fixture")
			issue, err := f.GetIssue(context.Background(), 7, 11)
			if err != nil || issue.IID != 11 || issue.State != state || issue.IsPullRequest {
				t.Fatalf("issue=%+v error=%v", issue, err)
			}
		})
	}
}

func TestLinkedHandoffGetIssueNotFound(t *testing.T) {
	for _, driver := range []string{"github", "forgejo", "gitlab"} {
		for _, status := range []int{http.StatusNotFound, http.StatusBadRequest} {
			t.Run(driver+"/"+http.StatusText(status), func(t *testing.T) {
				const token = "fixture-private-credential"
				handler := func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("method=%s", r.Method)
					}
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]string{"message": "issue unavailable " + token})
				}
				var f Forge
				switch driver {
				case "github":
					f = newGitHubDriver(t, newMockGitHub(t, map[string]http.HandlerFunc{"/repos/acme/widgets/issues/11": handler}), token)
				case "forgejo":
					f = newForgejoDriver(t, newMockForgejo(t, map[string]http.HandlerFunc{"/repos/acme/widgets/issues/11": handler}), token)
				case "gitlab":
					f = newTestDriver(t, newMockGitLab(t, map[string]http.HandlerFunc{"/api/v4/projects/7/issues/11": handler}), token)
				}
				_, err := f.GetIssue(context.Background(), 7, 11)
				var missing *IssueNotFoundError
				if err == nil || errors.As(err, &missing) != (status == http.StatusNotFound) {
					t.Fatalf("error=%v want not found=%v", err, status == http.StatusNotFound)
				}
				if driver == "gitlab" && status == http.StatusNotFound {
					// The GitLab SDK suppresses the response body for GetIssue 404s.
					if err.Error() != "gitlab: get issue: 404 Not Found" {
						t.Fatalf("message changed: %v", err)
					}
				} else if !strings.HasPrefix(err.Error(), driver+": get issue: ") || !strings.Contains(err.Error(), "issue unavailable "+redactPlaceholder) {
					t.Fatalf("message changed: %v", err)
				}
				if Class(err) != ErrorClassOther {
					t.Fatalf("class changed: %s", Class(err))
				}
				if missing != nil && (missing.Error() != missing.Unwrap().Error() || Class(missing.Unwrap()) != Class(err)) {
					t.Fatalf("wrapper changed message/class: %v", err)
				}
				for safe := err; safe != nil; safe = errors.Unwrap(safe) {
					if strings.Contains(safe.Error(), token) {
						t.Fatal("credential retained in error chain")
					}
				}
			})
		}
	}
}
