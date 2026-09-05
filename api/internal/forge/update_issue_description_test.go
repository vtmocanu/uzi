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

// TestForgejoUpdateIssueDescriptionPreservesTheTitle pins the lost-update bug the
// body-only PATCH exists to prevent.
//
// In code.gitea.io/sdk/gitea, EditIssueOption.Title is a plain `string` with the
// json tag "title" and NO `omitempty`. A driver built on it must read the issue
// and round-trip the current title to avoid PATCHing `"title": ""` (which wipes
// it) — and that read-then-write silently clobbers a title edited concurrently by
// someone else (TOCTOU). A body-only PATCH omits "title" entirely, so the
// concurrent rename survives and no read is needed.
//
// The concurrent edit is modelled deterministically, no timing: the issue already
// holds a "renamed concurrently" title, tracked in a mutable var the PATCH handler
// rewrites ONLY when the body carries a "title" key. The non-PATCH arm t.Errorf's
// so a reintroduced internal read reddens.
func TestForgejoUpdateIssueDescriptionPreservesTheTitle(t *testing.T) {
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
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "title": title})
			default:
				t.Errorf("body-only PATCH must not read the issue first; unexpected method %s", r.Method)
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
	if _, sent := patched["title"]; sent {
		t.Errorf("the PATCH carried a title, which clobbers a concurrent rename (lost update) — "+
			"a body-only PATCH must omit \"title\"; body = %v", patched)
	}
	if title != concurrentTitle {
		t.Errorf("the concurrent rename was lost: title = %q, want %q", title, concurrentTitle)
	}
}

// Redaction is per-method and not automatic: every error return in both drivers
// wraps through the embedded redactor. For Forgejo the body-only PATCH is the only
// request the driver makes, so a forge that 403s it and echoes the PAT in the
// error body must not leak it.
func TestUpdateIssueDescriptionRedactsTheToken(t *testing.T) {
	t.Run("gitlab", func(t *testing.T) {
		// Assembled from parts so no glpat- token SHAPE lives in source: GitHub Push
		// Protection scans literals across the whole push and honours no in-file allow
		// directive (.claude/rules/prds.md); runtime assembly satisfies it and gitleaks.
		const token = "glpat-" + "update-desc-redaction-probe" + "-0123456789" // fake PAT fixture, asserted to be redacted from the error, never a real secret
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

	t.Run("forgejo, and the error is from the PATCH", func(t *testing.T) {
		const token = "forgejo-update-desc-redaction-probe-0123456789" //nolint:gosec // G101: fake PAT fixture, the value this test asserts is redacted out of the error, never a real secret; gitleaks:allow
		m := newMockForgejo(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/issues/5": func(w http.ResponseWriter, _ *http.Request) {
				// Fail the PATCH and echo the PAT back in the error body.
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
	})
}
