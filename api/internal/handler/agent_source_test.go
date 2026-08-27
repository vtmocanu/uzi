package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestStagedToDTOBodySanitizedFlag verifies the PRD #602 M5 honesty signal: each
// staged role's DTO carries body_sanitized == (display-sanitization changed the body).
// A role whose raw body hides a control/bidi/format char serializes
// "body_sanitized":true and its DTO prompt_body is the SANITIZED form (the hidden char
// dropped); a clean body serializes "body_sanitized":false with prompt_body unchanged.
// Non-live: it builds the staged snapshot struct directly, no DB.
func TestStagedToDTOBodySanitizedFlag(t *testing.T) {
	// A body with U+202E (Cf, RIGHT-TO-LEFT OVERRIDE) — a bidi override SanitizeTTY
	// strips. Written as an escape so this source file carries no raw bidi char.
	const dirtyBody = "Do \u202Ethe thing\n"
	// A body with a bare ESC (control char) — also dropped by SanitizeTTY.
	const escBody = "before\x1bafter\n"
	const cleanBody = "just a normal body\nwith a newline and\ttab\n"

	roles := []agentsource.StagedRole{
		{Name: "bidi-role", OK: true, PromptBody: dirtyBody},
		{Name: "esc-role", OK: true, PromptBody: escBody},
		{Name: "clean-role", OK: true, PromptBody: cleanBody},
	}
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}

	staged := store.AgentSourceStaged{
		FetchedSha: "sha-under-test",
		SourceUrl:  "https://example.com/repo",
		SourceRef:  "main",
		Roles:      rolesJSON,
	}

	dto := stagedToDTO(staged, "" /* lastAppliedSHA */)
	if dto == nil {
		t.Fatal("stagedToDTO returned nil")
	}
	if len(dto.Roles) != 3 {
		t.Fatalf("got %d roles, want 3", len(dto.Roles))
	}

	byName := map[string]agentSourceRoleDTO{}
	for _, r := range dto.Roles {
		byName[r.Name] = r
	}

	// The dirty roles: flag true, and the DTO body no longer carries the hidden char.
	for _, name := range []string{"bidi-role", "esc-role"} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("missing role %q in DTO", name)
		}
		if !r.BodySanitized {
			t.Errorf("%s: BodySanitized = false, want true", name)
		}
		if strings.ContainsRune(r.PromptBody, '\u202E') || strings.ContainsRune(r.PromptBody, '\x1b') {
			t.Errorf("%s: DTO prompt_body still carries a hidden char: %q", name, r.PromptBody)
		}
	}

	// The clean role: flag false, body passed through unchanged.
	if r := byName["clean-role"]; r.BodySanitized {
		t.Errorf("clean-role: BodySanitized = true, want false")
	} else if r.PromptBody != cleanBody {
		t.Errorf("clean-role: prompt_body = %q, want unchanged %q", r.PromptBody, cleanBody)
	}

	// Serialization contract: the web reads these exact JSON keys/values.
	out, err := json.Marshal(dto.Roles)
	if err != nil {
		t.Fatalf("marshal DTO roles: %v", err)
	}
	s := string(out)
	if strings.Count(s, `"body_sanitized":true`) != 2 {
		t.Errorf("expected 2 body_sanitized:true, got JSON: %s", s)
	}
	if strings.Count(s, `"body_sanitized":false`) != 1 {
		t.Errorf("expected 1 body_sanitized:false, got JSON: %s", s)
	}
}
