package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// UpdateIssueDescription (PRD #72 M5) on both drivers. The Forgejo half carries the
// assertion that pins a real hazard; see below.

func TestGitLabUpdateIssueDescriptionSendsOnlyTheDescription(t *testing.T) {
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
				"labels": []string{}, "description": "the new body",
			})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	if err := d.UpdateIssueDescription(context.Background(), 7, 5, "the new body"); err != nil {
		t.Fatalf("UpdateIssueDescription: %v", err)
	}
	if got := body["description"]; got != "the new body" {
		t.Errorf("description = %v, want %q", got, "the new body")
	}
	// UpdateIssueOptions.Description is a *string with omitempty, so nothing else
	// about the issue is transmitted and nothing else can be clobbered.
	if _, sent := body["title"]; sent {
		t.Errorf("the GitLab driver must not send a title; body = %v", body)
	}
	if len(body) != 1 {
		t.Errorf("expected exactly one field on the wire, got %v", body)
	}
}

// TestForgejoUpdateIssueDescriptionPreservesTheTitle pins the hazard the whole
// Forgejo implementation exists to work around.
//
// In code.gitea.io/sdk/gitea, EditIssueOption.Title is a plain `string` with the
// json tag "title" and NO `omitempty`. A naive EditIssue therefore PATCHes
// `"title": ""`, which can wipe the issue's title. The driver reads the issue first
// and sends the current title back — this asserts the PATCH body carries that
// title, not "".
//
// If someone later "simplifies" the driver by dropping the internal read, this is
// the test that reddens, and the empty string in the failure message is the bug.
func TestForgejoUpdateIssueDescriptionPreservesTheTitle(t *testing.T) {
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
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "title": existingTitle})
			default:
				t.Errorf("unexpected method %s", r.Method)
			}
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	if err := d.UpdateIssueDescription(context.Background(), 7, 5, "the new body"); err != nil {
		t.Fatalf("UpdateIssueDescription: %v", err)
	}
	if patched == nil {
		t.Fatal("no PATCH was sent")
	}
	if got := patched["body"]; got != "the new body" {
		t.Errorf("body = %v, want %q", got, "the new body")
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

// Redaction is per-method and not automatic: every error return in both drivers
// wraps through the embedded redactor. That includes the Forgejo driver's INTERNAL
// read, which carries the same client and the same PAT.
func TestUpdateIssueDescriptionRedactsTheToken(t *testing.T) {
	t.Run("gitlab", func(t *testing.T) {
		const token = "glpat-update-desc-redaction-probe-0123456789"
		m := newMockGitLab(t, map[string]http.HandlerFunc{
			"/api/v4/projects/7/issues/5": func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				// The forge echoes the PAT back in the error body.
				_, _ = w.Write([]byte(`{"message":"denied for token ` + token + `"}`))
			},
		})
		d := newTestDriver(t, m, token)
		err := d.UpdateIssueDescription(context.Background(), 7, 5, "x")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("the PAT leaked into the error: %v", err)
		}
	})

	t.Run("forgejo, and the error is from the INTERNAL read", func(t *testing.T) {
		const token = "forgejo-update-desc-redaction-probe-0123456789"
		m := newMockForgejo(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, r *http.Request) {
				// Fail the GET, which is the driver's own read, not the caller's.
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"denied for token ` + token + `"}`))
			},
		})
		d := newForgejoDriver(t, m, token)
		err := d.UpdateIssueDescription(context.Background(), 7, 5, "x")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("the PAT leaked into the error: %v", err)
		}
		if !strings.Contains(err.Error(), "read current issue") {
			t.Errorf("the internal read's error should say which call failed; got %v", err)
		}
	})
}
