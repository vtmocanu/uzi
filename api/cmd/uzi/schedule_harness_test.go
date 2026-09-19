package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1429 M5: the `schedule create`/`edit` --harness flag and its request-presence
// contract. These are UNIT tests over the CLI's request builder via the FakeClient
// (LastCreateSchedReq / LastPatchSchedReq), mirroring schedule_credential_test.go's --token
// coverage exactly — --harness is built on the identical OptionalHarness/OptionalCredentialOverride
// presence-aware pattern.

// editHarnessFixture seeds a user-origin schedule whose stored harness pin is "codex".
func editHarnessFixture(id string) *uzicli.FakeClient {
	pin := "codex"
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
				Harness:     &pin,
			},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Target: "prompt", Timing: "recurring", CronExpr: "0 4 * * 2", Enabled: true},
	}
}

// TestScheduleEditOmitsHarnessWhenAbsent: a --cron-only edit (no --harness) must leave the
// harness key OMITTED from the PATCH, so the server's presence-aware seed-and-keep preserves
// the stored pin — the pin is NEVER restated from the fetched DTO.
func TestScheduleEditOmitsHarnessWhenAbsent(t *testing.T) {
	fc := editHarnessFixture("sch_harness")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_harness", "--cron", "0 4 * * 2")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastPatchSchedReq.Harness.Present {
		t.Fatalf("harness present=%v, want OMITTED (Present=false) so the server keeps the stored pin",
			fc.LastPatchSchedReq.Harness.Present)
	}
	// The omitzero wire behaviour: the marshaled body carries no harness key at all.
	b, err := json.Marshal(fc.LastPatchSchedReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"harness"`) {
		t.Fatalf("marshaled PATCH must omit harness entirely, got %s", b)
	}
}

// TestScheduleEditHarnessSetsPin: an explicit --harness codex sends a PRESENT pin, changing
// the stored value from claude to codex.
func TestScheduleEditHarnessSetsPin(t *testing.T) {
	fc := editHarnessFixture("sch_harness")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_harness", "--harness", "claude")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	h := fc.LastPatchSchedReq.Harness
	if !h.Present || h.Value == nil || *h.Value != "claude" {
		t.Fatalf("harness = %+v, want a present pin of claude", h)
	}
}

// TestScheduleEditHarnessClearsToImplicit: an explicit --harness "" sends a PRESENT wrapper
// with a nil Value (an explicit clear back to implicit D11 resolution), distinct from
// omission.
func TestScheduleEditHarnessClearsToImplicit(t *testing.T) {
	fc := editHarnessFixture("sch_harness")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_harness", "--harness", "")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	h := fc.LastPatchSchedReq.Harness
	if !h.Present || h.Value != nil {
		t.Fatalf("harness = %+v, want a present wrapper with a nil value (explicit clear)", h)
	}
}

// TestScheduleEditHarnessInvalidValueRefused: a value other than claude|codex|"" is a clean
// client-side usage refusal before any PATCH is sent.
func TestScheduleEditHarnessInvalidValueRefused(t *testing.T) {
	fc := editHarnessFixture("sch_harness")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_harness", "--harness", "gpt4")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage for an invalid harness value; stderr=%q", code, errOut)
	}
	if !containsAll(errOut, "--harness", "claude", "codex") {
		t.Errorf("refusal should name the valid values; got: %q", errOut)
	}
}

// TestScheduleCreateHarnessSetsPin: `schedule create --harness codex` carries a present pin
// on the create body.
func TestScheduleCreateHarnessSetsPin(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedSchedule: apitypes.ScheduleDTO{ID: "new", Target: "prompt", Timing: "recurring", CronExpr: "0 9 * * 1"},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "create",
		"--repo", "r1", "--prompt", "weekly", "--cron", "0 9 * * 1", "--harness", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	h := fc.LastCreateSchedReq.Harness
	if !h.Present || h.Value == nil || *h.Value != "codex" {
		t.Fatalf("harness = %+v, want a present pin of codex", h)
	}
}

// TestScheduleCreateOmitsHarnessWhenAbsent: a plain create (no --harness) leaves the harness
// key omitted, so the server defaults to implicit D11 resolution per fire.
func TestScheduleCreateOmitsHarnessWhenAbsent(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedSchedule: apitypes.ScheduleDTO{ID: "new", Target: "prompt", Timing: "recurring", CronExpr: "0 9 * * 1"},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "create",
		"--repo", "r1", "--prompt", "weekly", "--cron", "0 9 * * 1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastCreateSchedReq.Harness.Present {
		t.Fatalf("harness present=%v, want OMITTED on a create with no --harness",
			fc.LastCreateSchedReq.Harness.Present)
	}
}

// TestScheduleCreateHarnessInvalidValueRefused: an invalid --harness value on create is a
// clean client-side refusal, before any request.
func TestScheduleCreateHarnessInvalidValueRefused(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedSchedule: apitypes.ScheduleDTO{ID: "new", Target: "prompt", Timing: "recurring", CronExpr: "0 9 * * 1"},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "create",
		"--repo", "r1", "--prompt", "weekly", "--cron", "0 9 * * 1", "--harness", "gpt4")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage for an invalid harness value; stderr=%q", code, errOut)
	}
}
