package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func codexSwitchClient() *uzicli.FakeClient {
	return &uzicli.FakeClient{
		RunByID:     map[string]apitypes.RunDTO{"run-9": {ID: "run-9", Harness: "codex", Status: "running"}},
		SetTokenRun: apitypes.RunDTO{ID: "run-9", Harness: "codex", Status: "running"},
	}
}

func TestRunSetTokenHarnessKindsAndAmbiguity(t *testing.T) {
	cases := []struct {
		name, harness string
		kinds         []string
		wantID        string
	}{
		{"subscription", "codex", []string{"codex_auth"}, "choice-0"},
		{"api-key", "codex", []string{"openai_api_key"}, "choice-0"},
		{"codex-ignores-anthropic", "codex", []string{"anthropic_token", "codex_auth"}, "choice-1"},
		{"claude-ignores-codex", "claude", []string{"codex_auth", "openai_api_key", "anthropic_token"}, "choice-2"},
		{"codex-rejects-anthropic", "codex", []string{"anthropic_token"}, ""},
		{"claude-rejects-codex", "claude", []string{"codex_auth", "openai_api_key"}, ""},
		{"ambiguous-codex-kinds", "codex", []string{"codex_auth", "openai_api_key"}, ""},
		{"duplicate-codex-kind", "codex", []string{"codex_auth", "codex_auth"}, ""},
		{"duplicate-api-key-kind", "codex", []string{"openai_api_key", "openai_api_key"}, ""},
		{"duplicate-claude-kind", "claude", []string{"anthropic_token", "anthropic_token"}, ""},
		{"unknown-harness", "future", []string{"anthropic_token"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := codexSwitchClient()
			fc.RunByID["run-9"] = apitypes.RunDTO{ID: "run-9", Harness: tc.harness}
			ids := []string{"choice-0", "choice-1", "choice-2"}
			for i, kind := range tc.kinds {
				fc.Secrets = append(fc.Secrets, apitypes.SecretDTO{ID: ids[i], Label: "SHARED", Kind: kind})
			}
			_, _, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "shared")
			if tc.wantID == "" {
				if code != uzicli.ExitUsage || fc.LastSetTokenOverride != nil {
					t.Fatalf("exit=%d override=%+v", code, fc.LastSetTokenOverride)
				}
			} else if code != uzicli.ExitOK || fc.LastSetTokenOverride == nil || *fc.LastSetTokenOverride != (uzicli.SetRunCredentialOverride{Mode: "pinned", SecretID: tc.wantID}) {
				t.Fatalf("exit=%d override=%+v", code, fc.LastSetTokenOverride)
			}
		})
	}
}

func TestRunSetTokenCodexLiveKindsReachServer(t *testing.T) {
	for _, kind := range []string{"issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework"} {
		t.Run(kind, func(t *testing.T) {
			fc := codexSwitchClient()
			fc.RunByID["run-9"] = apitypes.RunDTO{ID: "run-9", Harness: "codex", Status: "running", Kind: kind}
			fc.Secrets = []apitypes.SecretDTO{{ID: "selected", Label: "Account B", Kind: "openai_api_key"}}
			_, _, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "Account B")
			if code != uzicli.ExitOK || fc.LastSetTokenOverride == nil || *fc.LastSetTokenOverride != (uzicli.SetRunCredentialOverride{Mode: "pinned", SecretID: "selected"}) {
				t.Fatalf("exit=%d override=%+v", code, fc.LastSetTokenOverride)
			}
		})
	}
}

func TestRunSetTokenCodexNoModeFallback(t *testing.T) {
	for _, mode := range []string{"auto", "default", "inherit"} {
		t.Run(mode, func(t *testing.T) {
			fc := codexSwitchClient()
			_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "--"+mode)
			if code != uzicli.ExitUsage || fc.LastSetTokenOverride != nil || !strings.Contains(errb, "explicit") {
				t.Fatalf("exit=%d stderr=%q override=%+v", code, errb, fc.LastSetTokenOverride)
			}
		})
	}
}

func TestRunSetTokenGetErrorsNeverFallBack(t *testing.T) {
	for _, exit := range []int{uzicli.ExitNotFound, uzicli.ExitConflict} {
		t.Run(strconv.Itoa(exit), func(t *testing.T) {
			for _, choice := range []string{"shared", "--auto"} {
				fc := codexSwitchClient()
				fc.GetRunHook = func(id string) (apitypes.RunDTO, error) {
					if id != "run-9" {
						t.Fatalf("GET id=%q", id)
					}
					return apitypes.RunDTO{}, uzicli.Exitf(exit, "owner metadata refused")
				}
				fc.Secrets = []apitypes.SecretDTO{{ID: "target", Label: "shared", Kind: "codex_auth"}}
				_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", choice)
				if code != exit || !strings.Contains(errb, "owner metadata refused") || fc.LastSetTokenOverride != nil {
					t.Fatalf("exit=%d stderr=%q override=%+v", code, errb, fc.LastSetTokenOverride)
				}
			}
		})
	}
}

func TestRunSetTokenCodexServerOwnsStateAndPolicy(t *testing.T) {
	cases := []struct {
		status, message string
		exit            int
	}{
		{"running", "owner credential not found", uzicli.ExitNotFound},
		{"running", "policy hold", uzicli.ExitConflict},
		{"claimed", "claim assembling", uzicli.ExitConflict},
		{"queued", "Codex switch ineligible", uzicli.ExitUsage},
		{"paused", "Codex switch ineligible", uzicli.ExitUsage},
		{"recovery_wait", "Codex switch ineligible", uzicli.ExitUsage},
		{"awaiting_approval", "worker unsupported", uzicli.ExitConflict},
	}
	for _, tc := range cases {
		t.Run(tc.status+tc.message, func(t *testing.T) {
			fc := codexSwitchClient()
			fc.RunByID["run-9"] = apitypes.RunDTO{ID: "run-9", Harness: "codex", Status: tc.status}
			fc.Secrets = []apitypes.SecretDTO{{ID: "target", Label: "shared", Kind: "openai_api_key"}}
			fc.SetRunCredentialErr = uzicli.Exitf(tc.exit, "%s", tc.message)
			_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "shared")
			if code != tc.exit || !strings.Contains(errb, tc.message) || fc.LastSetTokenOverride == nil {
				t.Fatalf("exit=%d stderr=%q override=%+v", code, errb, fc.LastSetTokenOverride)
			}
			if strings.Join(fc.RunVerbCalls, ",") != "get_run,set_run_credential" {
				t.Fatalf("calls=%v", fc.RunVerbCalls)
			}
		})
	}
}

func TestRunSetTokenDisabledAndSanitizedLegacyLabels(t *testing.T) {
	label := "legacy\u202e\x1b[31m"
	now := time.Now()
	for _, kind := range []string{"codex_auth", "openai_api_key", "anthropic_token"} {
		for _, disabled := range []bool{false, true} {
			fc := codexSwitchClient()
			secret := apitypes.SecretDTO{ID: "target", Label: label, Kind: kind}
			if disabled {
				secret.DisabledAt = &now
			}
			fc.Secrets = []apitypes.SecretDTO{secret}
			_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", label)
			if disabled || kind == "anthropic_token" {
				if code != uzicli.ExitUsage || fc.LastSetTokenOverride != nil {
					t.Fatalf("exit=%d override=%+v", code, fc.LastSetTokenOverride)
				}
				if strings.ContainsAny(errb, "\u202e\x1b") {
					t.Fatalf("unsafe stderr=%q", errb)
				}
			} else if code != uzicli.ExitOK {
				t.Fatalf("exit=%d stderr=%q", code, errb)
			}
		}
	}
}

func TestRunSetTokenCodexResponsePendingVersusCommitted(t *testing.T) {
	for _, pending := range []bool{true, false} {
		fc := codexSwitchClient()
		fc.Secrets = []apitypes.SecretDTO{{ID: "target", Label: "new", Kind: "codex_auth"}}
		label, id := "old", "old-id"
		generation := int64(7)
		fc.SetTokenRun.ClaimGeneration = &generation
		if pending {
			state := "requested"
			fc.SetTokenRun.CredentialSwitch = &state
		} else {
			label, id = "new", "target"
		}
		fc.SetTokenRun.CodexSecretLabel, fc.SetTokenRun.CodexSecretID = &label, &id
		out, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "new", "--json")
		var got apitypes.RunDTO
		if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, errb)
		}
		if got.CodexSecretID == nil || *got.CodexSecretID != id || got.CodexSecretLabel == nil || *got.CodexSecretLabel != label || got.ClaimGeneration == nil || *got.ClaimGeneration != 7 {
			t.Fatalf("binding changed: %s", out)
		}
		if pending && (got.CredentialSwitch == nil || *got.CredentialSwitch != "requested") {
			t.Fatalf("lost pending state: %s", out)
		}
		if !pending && got.CredentialSwitch != nil {
			t.Fatalf("invented pending state: %s", out)
		}
	}
}

func TestRunSetTokenCodexHumanRenderSanitizesStoredLabel(t *testing.T) {
	fc := codexSwitchClient()
	fc.Secrets = []apitypes.SecretDTO{{ID: "target", Label: "new", Kind: "codex_auth"}}
	label := "old\u202e\x1b[31m"
	state := "requested"
	fc.SetTokenRun.CodexSecretLabel = &label
	fc.SetTokenRun.CredentialSwitch = &state
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "new")
	if code != uzicli.ExitOK || !strings.Contains(out, "old") || !strings.Contains(out, "switch requested") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, errb)
	}
	if strings.ContainsAny(out, "\u202e\x1b") {
		t.Fatalf("unsafe stdout=%q", out)
	}
}

func TestRunSetTokenCodexLiveStatesReachServer(t *testing.T) {
	for _, status := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		t.Run(status, func(t *testing.T) {
			fc := codexSwitchClient()
			fc.RunByID["run-9"] = apitypes.RunDTO{ID: "run-9", Harness: "codex", Status: status}
			fc.Secrets = []apitypes.SecretDTO{{ID: "target", Label: "new", Kind: "codex_auth"}}
			fc.SetTokenRun.Status = status
			_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "new")
			if code != uzicli.ExitOK || fc.LastSetTokenOverride == nil {
				t.Fatalf("exit=%d stderr=%q", code, errb)
			}
		})
	}
}

func TestRunSetTokenUnexpectedMetadataErrorStopsWrite(t *testing.T) {
	fc := codexSwitchClient()
	fc.GetRunHook = func(string) (apitypes.RunDTO, error) {
		return apitypes.RunDTO{}, errors.New("metadata transport failed")
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "new")
	if code == uzicli.ExitOK || !strings.Contains(errb, "metadata transport failed") || fc.LastSetTokenOverride != nil {
		t.Fatalf("exit=%d stderr=%q override=%+v", code, errb, fc.LastSetTokenOverride)
	}
}
