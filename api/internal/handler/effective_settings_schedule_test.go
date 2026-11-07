package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestScheduleAlternativesTopLevelRefusal is the helper-level differing-reasons case for
// schedule-preview. Real data cannot reach a refused branch: a chosen-at-create harness needs both
// harnesses usable, and a stored credential override (the only input that makes a Codex branch
// refuse) makes the creation-time policies ineligible, so the schedule is never chosen at create.
// The composition still has to hold when a branch refuses, so it is exercised on constructed
// branches: the top-level refusal is set only when every branch is refused for the same reason.
func TestScheduleAlternativesTopLevelRefusal(t *testing.T) {
	valid := func(h workersvc.Harness) scheduleBranch {
		return scheduleBranch{
			harness: h,
			model:   &apitypes.EffectiveModelCellDTO{Setting: "worker_model"},
			effort:  &apitypes.EffectiveEffortCellDTO{Setting: "worker_effort"},
			credential: &apitypes.CredentialDTO{
				Kind: "anthropic", Source: "claim_selection", FixedAt: "claim"},
			subagents: &apitypes.EffectiveSubagentsDTO{Mode: "own_pins", Pinned: []apitypes.EffectiveSubagentPinDTO{}},
		}
	}
	refused := func(h workersvc.Harness, token, detail string) scheduleBranch {
		return scheduleBranch{harness: h, refused: refusal{token, detail}}
	}
	for _, tc := range []struct {
		name     string
		branches []scheduleBranch
		want     refusal
	}{
		{"both valid", []scheduleBranch{valid("claude"), valid("codex")}, refusal{}},
		{"one refused, one valid: no harness decided", []scheduleBranch{valid("claude"), refused("codex", "codex_override_conflict", "")}, refusal{}},
		{"differing reasons: neither is promoted", []scheduleBranch{
			refused("claude", "credential_disabled", ""), refused("codex", "codex_override_conflict", "")}, refusal{}},
		{"same token with a differing detail stays null", []scheduleBranch{
			refused("claude", "no_credential_for_harness", "credential_disabled"), refused("codex", "no_credential_for_harness", "")}, refusal{}},
		{"same reason on both: promoted", []scheduleBranch{
			refused("claude", "credential_disabled", ""), refused("codex", "credential_disabled", "")}, refusal{"credential_disabled", ""}},
		{"same reason and detail on both: promoted with the detail", []scheduleBranch{
			refused("claude", "no_credential_for_harness", "credential_disabled"), refused("codex", "no_credential_for_harness", "credential_disabled")},
			refusal{"no_credential_for_harness", "credential_disabled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alts, top := scheduleAlternatives(tc.branches)
			if top != tc.want {
				t.Errorf("top-level refusal = %+v, want %+v", top, tc.want)
			}
			if len(alts) != len(tc.branches) {
				t.Fatalf("alternatives = %d, want one per branch", len(alts))
			}
			for i, a := range alts {
				b := tc.branches[i]
				if a.Harness != string(b.harness) {
					t.Errorf("alternative %d harness = %q, want %q (order preserved)", i, a.Harness, b.harness)
				}
				if b.refused.Token == "" {
					if a.Refusal != nil || a.WorkerModel == nil || a.WorkerEffort == nil || a.Credential == nil || a.Subagents == nil {
						t.Errorf("valid alternative %d = %+v", i, a)
					}
					continue
				}
				// A refused branch carries its own reason and nothing harness-dependent.
				if a.Refusal == nil || *a.Refusal != b.refused.Token || (b.refused.Detail != "") != (a.RefusalDetail != nil) ||
					a.WorkerModel != nil || a.WorkerEffort != nil || a.Credential != nil || a.Subagents != nil {
					t.Errorf("refused alternative %d = %+v, want only %+v", i, a, b.refused)
				}
			}
		})
	}
}

func TestScheduleFireRefusalMapsThroughSkipReasons(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{workersvc.ErrCredentialDisabled, "credential_disabled"},
		{workersvc.ErrHarnessCredentialDisabled, "credential_disabled"},
		{workersvc.ErrNoUsableCredential, "no_usable_credential"},
		{workersvc.ErrCredentialOverrideHarnessUnsupported, "codex_override_conflict"},
		{fmt.Errorf("wrapped: %w", workersvc.ErrCodexPoolUnavailable), "codex_pool_unavailable"},
		{workersvc.ErrNoCredentialForHarness, "no_credential_for_harness"},
	} {
		got, ok := scheduleFireRefusal(tc.err)
		if !ok || got.Token != tc.want || got.Detail != "" {
			t.Errorf("scheduleFireRefusal(%v) = %+v, %v; want %q", tc.err, got, ok, tc.want)
		}
	}
	// A skip reason a read cannot produce, and an unrelated error, are failures and never refusals.
	for _, err := range []error{workersvc.ErrActiveRunExists, workersvc.ErrDescriptionTooLarge, errors.New("db down")} {
		if got, ok := scheduleFireRefusal(err); ok {
			t.Errorf("scheduleFireRefusal(%v) = %+v, want not mapped", err, got)
		}
	}
}

func TestScheduleSaveRefusalMapsTheValidatorSentinels(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{workersvc.ErrCredentialOverrideSecretNotFound, "credential_not_found"},
		{fmt.Errorf("%w: self_improve", workersvc.ErrCredentialOverrideLaneNotSwitchable), "credential_override_lane_not_switchable"},
		{workersvc.ErrCredentialOverrideHarnessUnsupported, "credential_override_harness_unsupported"},
		{workersvc.ErrCredentialOverridePinnedNeedsSecret, "credential_override_invalid"},
		{workersvc.ErrCredentialOverrideInvalidMode, "credential_override_invalid"},
		{workersvc.ErrCredentialDisabled, "credential_disabled"},
	} {
		if got, ok := scheduleSaveRefusal(tc.err); !ok || got.Token != tc.want {
			t.Errorf("scheduleSaveRefusal(%v) = %+v, %v; want %q", tc.err, got, ok, tc.want)
		}
	}
	if _, ok := scheduleSaveRefusal(errors.New("db down")); ok {
		t.Error("a store error is a failure, not a refusal")
	}
}

// TestSchedulePreviewRequestKeepsPresence: omitted, null and a value stay distinct, because omitted
// means as stored and null means clear or inherit.
func TestSchedulePreviewRequestKeepsPresence(t *testing.T) {
	decode := func(body string) (apitypes.ScheduleSettingsPreviewRequest, error) {
		var r apitypes.ScheduleSettingsPreviewRequest
		return r, json.Unmarshal([]byte(body), &r)
	}
	r, err := decode(`{"target":"sweep"}`)
	if err != nil || r.Harness.Present || r.Model.Present || r.CredentialOverride.Present {
		t.Fatalf("omitted members = %+v (%v)", r, err)
	}
	r, err = decode(`{"harness":null,"model":null,"credential_override":null}`)
	if err != nil || !r.Harness.Present || r.Harness.Value != nil || !r.Model.Present || r.Model.Value != nil ||
		!r.CredentialOverride.Present || r.CredentialOverride.Value != nil {
		t.Fatalf("null members = %+v (%v)", r, err)
	}
	r, err = decode(`{"harness":"codex","model":"sonnet","credential_override":{"mode":"pinned","secret_id":"x"}}`)
	if err != nil || r.Harness.Value == nil || *r.Harness.Value != "codex" || r.Model.Value == nil || *r.Model.Value != "sonnet" ||
		r.CredentialOverride.Value == nil || r.CredentialOverride.Value.Mode != "pinned" {
		t.Fatalf("value members = %+v (%v)", r, err)
	}
	// An unknown member inside the override is rejected by the strict decode.
	if _, err := decode(`{"credential_override":{"mode":"auto","x":1}}`); err == nil {
		t.Error("an unknown credential_override member decoded")
	}
	// A request re-encodes with only what was present.
	out, _ := json.Marshal(apitypes.ScheduleSettingsPreviewRequest{})
	if string(out) != `{}` {
		t.Errorf("empty request encodes as %s, want {}", out)
	}
}

func TestSchedulePreviewWithoutAUserIs401AndStoreErrorsAre500(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).PreviewScheduleSettings(rec, httptest.NewRequest(http.MethodPost, schedPath, strings.NewReader(`{"target":"sweep"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without a user = %d, want 401", rec.Code)
	}
	q := store.New(failingDB{})
	h := &Handler{q: q, wsvc: workersvc.New(q, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
	for name, body := range map[string]string{
		"a draft reads the store":                  `{"target":"sweep"}`,
		"a schedule id reads the store":            `{"schedule_id":"` + uuid.NewString() + `"}`,
		"a supplied override validates":            `{"target":"sweep","credential_override":{"mode":"auto"}}`,
		"a supplied model still reads the harness": `{"target":"sweep","model":"sonnet"}`,
	} {
		rec := httptest.NewRecorder()
		h.PreviewScheduleSettings(rec, userReq(http.MethodPost, schedPath, body, uuid.New(), nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500 (%s)", name, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"internal error"}` {
			t.Errorf("%s: body %q, want only the generic error envelope", name, got)
		}
	}
}
