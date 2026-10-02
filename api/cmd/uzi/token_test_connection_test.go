package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestTokenTestResolvesKindAndPreservesResult(t *testing.T) {
	fc := &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{
			{ID: "a", Kind: kindAnthropicToken, Label: "Shared"},
			{ID: "c", Kind: kindCodexAuth, Label: "Shared"},
			{ID: "o", Kind: kindOpenAIAPIKey, Label: "Key"},
		},
		SecretTestResult: apitypes.SecretTestResult{Status: "inconclusive", Reason: "vault_locked", Display: "safe detail"},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "test", "shared", "--kind", "codex", "--json")
	if code != uzicli.ExitOK || fc.LastTestKind != kindCodexAuth || fc.LastTestID != "c" {
		t.Fatalf("code=%d target=%s/%s output=%q", code, fc.LastTestKind, fc.LastTestID, out)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"kind": kindCodexAuth, "label": "Shared", "id": "c", "status": "inconclusive", "reason": "vault_locked", "display": "safe detail"} {
		if got[key] != want {
			t.Errorf("%s=%q, want %q", key, got[key], want)
		}
	}
	out, _, code = runCLI(t, fakeEnv(fc), "token", "test", "key")
	if code != uzicli.ExitOK || fc.LastTestID != "o" || !strings.Contains(out, "safe detail") {
		t.Fatalf("unique label: code=%d target=%q output=%q", code, fc.LastTestID, out)
	}
}

func TestTokenTestRejectsAmbiguousDisabledAndInvalidKind(t *testing.T) {
	now := time.Now()
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "a", Kind: kindAnthropicToken, Label: "Shared"},
		{ID: "c", Kind: kindCodexAuth, Label: "Shared"},
		{ID: "d", Kind: kindOpenAIAPIKey, Label: "Disabled", DisabledAt: &now},
	}}
	for _, args := range [][]string{
		{"token", "test", "Shared"},
		{"token", "test", "Shared", "--kind", "bogus"},
		{"token", "test", "Missing"},
	} {
		_, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage || fc.LastTestID != "" {
			t.Fatalf("args=%v code=%d unexpected test=%q", args, code, fc.LastTestID)
		}
	}
	_, _, code := runCLI(t, fakeEnv(fc), "token", "test", "Disabled")
	if code != uzicli.ExitConflict || fc.LastTestID != "" {
		t.Fatalf("disabled code=%d unexpected test=%q", code, fc.LastTestID)
	}
}
