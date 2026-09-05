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
					case http.MethodPatch:
						raw, _ := io.ReadAll(r.Body)
						_ = json.Unmarshal(raw, &patched)
						_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "title": "Fix the login flow"})
					default:
						// Only a PATCH is expected: the state-only PATCH does not read the
						// issue first, so a GET here would be the reintroduced round-trip.
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
			// Parity with the GitLab and GitHub tests: a state-only PATCH must not
			// carry a title, so a concurrent title edit cannot be clobbered.
			if _, sent := patched["title"]; sent {
				t.Errorf("SetIssueState must not send a title: %v", patched)
			}
		})
	}
}

// TestForgejoSetIssueStatePreservesAConcurrentTitleEdit pins the lost-update bug
// the state-only PATCH exists to prevent. The gitea SDK's EditIssueOption.Title is
// a plain `string` with no `omitempty`, so a driver built on it must read the
// issue and round-trip the current title — and that read-then-write silently
// clobbers a title edited concurrently by someone else (TOCTOU).
//
// The concurrent edit is modelled deterministically, no timing: the issue already
// holds a "renamed concurrently" title, tracked in a mutable var the PATCH handler
// rewrites ONLY when the body carries a "title" key (Forgejo field-only PATCH
// semantics). A correct state-only PATCH omits "title", so the tracked title
// survives; a driver that reads-then-writes the stale title reddens here.
//
// If someone later reintroduces the internal read, the non-PATCH arm's t.Errorf
// fires too — the driver must not GET the issue first.
func TestForgejoSetIssueStatePreservesAConcurrentTitleEdit(t *testing.T) {
	const concurrentTitle = "Renamed concurrently by someone else"
	title := concurrentTitle // mutable: only a PATCH that sends "title" changes it
	var patched map[string]any
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPatch:
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &patched)
				// Forgejo field-only semantics: a title only changes if "title" is sent.
				if t2, sent := patched["title"].(string); sent {
					title = t2
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "title": title, "state": "closed"})
			default:
				t.Errorf("state-only PATCH must not read the issue first; unexpected method %s", r.Method)
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
	if _, sent := patched["title"]; sent {
		t.Errorf("the PATCH carried a title, which clobbers a concurrent rename (lost update) — "+
			"a state-only PATCH must omit \"title\"; body = %v", patched)
	}
	if got := patched["state"]; got != "closed" {
		t.Errorf("state = %v, want %q", got, "closed")
	}
	if title != concurrentTitle {
		t.Errorf("the concurrent rename was lost: title = %q, want %q", title, concurrentTitle)
	}
}

// SetIssueState's error returns redact the PAT. The state-only PATCH is the only
// request the driver makes, so a forge that 403s the PATCH and echoes the token in
// the error body must not leak it — same per-method redaction contract as the
// GitLab and GitHub drivers.
func TestForgejoSetIssueStateRedactsTokenOnPatchError(t *testing.T) {
	const token = "forgejo-set-state-redaction-probe-0123456789" //nolint:gosec // G101: fake PAT fixture, the value this test asserts is redacted out of the error, never a real secret; gitleaks:allow
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, _ *http.Request) {
			// Fail the PATCH and echo the PAT back in the error body.
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
}
