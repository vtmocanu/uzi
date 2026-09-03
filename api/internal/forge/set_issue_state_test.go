package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// SetIssueState (PRD #1034 M1) on all three drivers. Each driver translates the
// neutral StateOpened/StateClosed into its own MUTATE vocabulary, so every test
// asserts the actual serialized request the driver sends — a wrong mapping fails
// the test, not just an error.

func TestGitLabSetIssueStateSendsStateEvent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state IssueState
		want  string // the state_event verb on the wire
	}{
		{"close", StateClosed, "close"},
		{"reopen", StateOpened, "reopen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/issues/5": func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPut {
						t.Errorf("method = %s, want PUT", r.Method)
					}
					raw, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(raw, &body)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": 1001, "iid": 5, "title": "t", "state": "opened",
						"labels": []string{}, "description": "d",
					})
				},
			})
			d := newTestDriver(t, m, "glpat-token-value-123456")

			if err := d.SetIssueState(context.Background(), 7, 5, tc.state); err != nil {
				t.Fatalf("SetIssueState: %v", err)
			}
			if got := body["state_event"]; got != tc.want {
				t.Errorf("state_event = %v, want %q", got, tc.want)
			}
			// Only the state_event travels — nothing else about the issue is sent, so
			// nothing else can be clobbered.
			if _, sent := body["title"]; sent {
				t.Errorf("the GitLab driver must not send a title; body = %v", body)
			}
			if len(body) != 1 {
				t.Errorf("expected exactly one field on the wire, got %v", body)
			}
		})
	}
}

func TestGitHubSetIssueStateSendsStateAndReason(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      IssueState
		wantState  string
		wantReason string
	}{
		{"close", StateClosed, "closed", "completed"},
		{"reopen", StateOpened, "open", "reopened"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/issues/9": func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPatch {
						b, _ := io.ReadAll(r.Body)
						_ = json.Unmarshal(b, &payload)
						// A PATCH that also sent title/body would risk clobbering them —
						// assert only state + state_reason were sent.
						if _, sent := payload["title"]; sent {
							t.Errorf("SetIssueState must not send a title: %v", payload)
						}
						if _, sent := payload["body"]; sent {
							t.Errorf("SetIssueState must not send a body: %v", payload)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "title": "t", "state": tc.wantState})
				},
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

			if err := d.SetIssueState(context.Background(), 7, 9, tc.state); err != nil {
				t.Fatalf("SetIssueState: %v", err)
			}
			if got := payload["state"]; got != tc.wantState {
				t.Errorf("state = %v, want %q", got, tc.wantState)
			}
			if got := payload["state_reason"]; got != tc.wantReason {
				t.Errorf("state_reason = %v, want %q", got, tc.wantReason)
			}
		})
	}
}

func TestForgejoSetIssueStateSendsState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state IssueState
		want  string // the gitea state on the wire
	}{
		{"close", StateClosed, "closed"},
		{"reopen", StateOpened, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patched map[string]any
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, r *http.Request) {
					switch r.Method {
					case http.MethodGet:
						_ = json.NewEncoder(w).Encode(map[string]any{
							"number": 5, "title": "Fix the login flow", "body": "b", "state": "open",
						})
					case http.MethodPatch:
						raw, _ := io.ReadAll(r.Body)
						_ = json.Unmarshal(raw, &patched)
						_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "title": "Fix the login flow"})
					default:
						t.Errorf("unexpected method %s", r.Method)
					}
				},
			})
			d := newForgejoDriver(t, m, "forgejo-token-value-123456")

			if err := d.SetIssueState(context.Background(), 7, 5, tc.state); err != nil {
				t.Fatalf("SetIssueState: %v", err)
			}
			if patched == nil {
				t.Fatal("no PATCH was sent")
			}
			if got := patched["state"]; got != tc.want {
				t.Errorf("state = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestForgejoSetIssueStatePreservesTheTitle pins the same no-omitempty hazard the
// Forgejo UpdateIssueDescription driver works around: EditIssueOption.Title is a
// plain `string` with no `omitempty`, so a naive edit PATCHes `"title": ""` and
// can wipe the issue's title. SetIssueState reads the issue first and sends the
// current title back — this asserts the PATCH body carries that title, not "".
//
// If someone later "simplifies" the driver by dropping the internal read, this is
// the test that reddens, and the empty string in the failure message is the bug.
func TestForgejoSetIssueStatePreservesTheTitle(t *testing.T) {
	const existingTitle = "Fix the login flow"
	var patched map[string]any
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 5, "title": existingTitle, "body": "old body", "state": "open",
				})
			case http.MethodPatch:
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &patched)
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "title": existingTitle, "state": "closed"})
			default:
				t.Errorf("unexpected method %s", r.Method)
			}
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	if err := d.SetIssueState(context.Background(), 7, 5, StateClosed); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	if patched == nil {
		t.Fatal("no PATCH was sent")
	}
	title, _ := patched["title"].(string)
	if title == "" {
		t.Fatalf("the PATCH carried an EMPTY title, which can wipe the issue's title — "+
			"EditIssueOption.Title has no omitempty, so the driver must read the current title first; body = %v", patched)
	}
	if title != existingTitle {
		t.Errorf("title = %q, want the issue's existing %q", title, existingTitle)
	}
}

// SetIssueState's error returns redact the PAT, including the Forgejo driver's
// INTERNAL read — same per-method redaction contract as UpdateIssueDescription.
func TestForgejoSetIssueStateRedactsTokenOnInternalRead(t *testing.T) {
	const token = "forgejo-set-state-redaction-probe-0123456789" //nolint:gosec // G101: fake PAT fixture, the value this test asserts is redacted out of the error, never a real secret; gitleaks:allow
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, _ *http.Request) {
			// Fail the GET, which is the driver's own read.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"denied for token ` + token + `"}`))
		},
	})
	d := newForgejoDriver(t, m, token)
	err := d.SetIssueState(context.Background(), 7, 5, StateClosed)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the PAT leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "read current issue") {
		t.Errorf("the internal read's error should say which call failed; got %v", err)
	}
}
