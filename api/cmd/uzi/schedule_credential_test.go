package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1247 M6 (Task 9): the `schedule create`/`edit` --token flag and its request-presence
// contract. These are UNIT tests over the CLI's request builder via the FakeClient, which
// captures the whole apitypes.ScheduleRequest (LastCreateSchedReq / LastPatchSchedReq) before
// any marshal, so a test reads the presence wrapper directly.

// editTokenFixture seeds a user-origin schedule whose stored credential override is a PINNED
// token (the response DTO shape {mode,label}), plus one anthropic_token secret so a --token
// <label> edit can resolve. Mirrors editModelFixture but for the credential override.
func editTokenFixture(id string) *uzicli.FakeClient {
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			id: {
				ID:          id,
				Target:      "prompt",
				Prompt:      "do a thing",
				Timing:      "recurring",
				CronExpr:    "0 9 * * 1",
				Timezone:    "UTC",
				AutoApprove: true,
				WaitOnLimit: true,
				Enabled:     true,
				Status:      "active",
				// The stored override, in the response DTO shape. The CLI must NOT restate it.
				CredentialOverride: &apitypes.CredentialOverrideDTO{Mode: "pinned", Label: sptr("prod-token")},
			},
		},
		Secrets: []apitypes.SecretDTO{
			{ID: "11111111-1111-1111-1111-111111111111", Kind: "anthropic_token", Label: "prod-token"},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Target: "prompt", Timing: "recurring", CronExpr: "0 4 * * 2", Enabled: true},
	}
}

// TestScheduleEditOmitsCredentialOverrideWhenTokenAbsent is the CLI half of blocker 2: a
// --cron-only edit (no --token) must leave credential_override OMITTED from the PATCH, so the
// server's presence-aware seed-and-keep preserves the stored override — unlike model/mr_rework,
// it is NEVER restated. This mirrors TestScheduleEditRestatesModel but proves the inverse:
// omission, not restatement.
func TestScheduleEditOmitsCredentialOverrideWhenTokenAbsent(t *testing.T) {
	fc := editTokenFixture("sch_tok")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_tok", "--cron", "0 4 * * 2")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastPatchSchedReq.CredentialOverride.Present {
		t.Fatalf("credential_override present=%v, want OMITTED (Present=false) so the server keeps the stored override",
			fc.LastPatchSchedReq.CredentialOverride.Present)
	}
	// And the omitzero wire behaviour: the marshaled body carries no credential_override key.
	b, err := json.Marshal(fc.LastPatchSchedReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "credential_override") {
		t.Fatalf("marshaled PATCH must omit credential_override entirely, got %s", b)
	}
}

// TestScheduleEditTokenInherit: an explicit --token inherit sends a PRESENT {"mode":"inherit"}
// override, which the server validates (and clears on a switchable lane).
func TestScheduleEditTokenInherit(t *testing.T) {
	fc := editTokenFixture("sch_tok")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_tok", "--token", "inherit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	co := fc.LastPatchSchedReq.CredentialOverride
	if !co.Present || co.Value == nil || co.Value.Mode != "inherit" || co.Value.SecretID != nil {
		t.Fatalf("credential_override = %+v, want a present {mode:inherit} with no secret", co)
	}
}

// TestScheduleEditTokenAuto: --token auto sends a present bare {"mode":"auto"} (no round-trip).
func TestScheduleEditTokenAuto(t *testing.T) {
	fc := editTokenFixture("sch_tok")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_tok", "--token", "auto")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	co := fc.LastPatchSchedReq.CredentialOverride
	if !co.Present || co.Value == nil || co.Value.Mode != "auto" || co.Value.SecretID != nil {
		t.Fatalf("credential_override = %+v, want a present {mode:auto} with no secret", co)
	}
}

// TestScheduleEditTokenLabel: --token <label> resolves the label to a PINNED override naming
// the anthropic_token id (client-side via ListSecrets), the same as run create/approve.
func TestScheduleEditTokenLabel(t *testing.T) {
	fc := editTokenFixture("sch_tok")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_tok", "--token", "prod-token")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	co := fc.LastPatchSchedReq.CredentialOverride
	if !co.Present || co.Value == nil || co.Value.Mode != "pinned" ||
		co.Value.SecretID == nil || *co.Value.SecretID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("credential_override = %+v, want a present pinned override on the resolved secret id", co)
	}
}

// TestScheduleEditTokenUnknownLabel: an unknown token label is a clean usage refusal before any
// PATCH is sent (no request captured).
func TestScheduleEditTokenUnknownLabel(t *testing.T) {
	fc := editTokenFixture("sch_tok")
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_tok", "--token", "nope")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage for an unknown token label", code)
	}
}

// TestScheduleCreateTokenLabel: `schedule create --token <label>` carries a present pinned
// override on the create body.
func TestScheduleCreateTokenLabel(t *testing.T) {
	fc := &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{
			{ID: "22222222-2222-2222-2222-222222222222", Kind: "anthropic_token", Label: "prod-token"},
		},
		CreatedSchedule: apitypes.ScheduleDTO{ID: "new", Target: "prompt", Timing: "recurring", CronExpr: "0 9 * * 1"},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "create",
		"--repo", "r1", "--prompt", "weekly", "--cron", "0 9 * * 1", "--token", "prod-token")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	co := fc.LastCreateSchedReq.CredentialOverride
	if !co.Present || co.Value == nil || co.Value.Mode != "pinned" ||
		co.Value.SecretID == nil || *co.Value.SecretID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("credential_override = %+v, want a present pinned override on the resolved secret id", co)
	}
}

// TestScheduleCreateOmitsTokenWhenAbsent: a plain create (no --token) leaves credential_override
// omitted, so the server defaults it to inherit.
func TestScheduleCreateOmitsTokenWhenAbsent(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedSchedule: apitypes.ScheduleDTO{ID: "new", Target: "prompt", Timing: "recurring", CronExpr: "0 9 * * 1"},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "create",
		"--repo", "r1", "--prompt", "weekly", "--cron", "0 9 * * 1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastCreateSchedReq.CredentialOverride.Present {
		t.Fatalf("credential_override present=%v, want OMITTED on a create with no --token",
			fc.LastCreateSchedReq.CredentialOverride.Present)
	}
}
