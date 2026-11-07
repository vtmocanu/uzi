package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type capacityCase struct {
	name  string
	state string
	cause *string
	value bool
	gen   *int64
}

func (c capacityCase) request() StateRequest {
	return StateRequest{State: c.state, RecoveryCause: c.cause, ClaimGeneration: c.gen, RetainedCapacityOnly: boolPtr(c.value)}
}

// capacityRefusals are every shape the marker must be refused for (issue #4046).
func capacityRefusals() []capacityCase {
	exhausted := strPtr(recoveryCauseRetainedRecoveryExhausted)
	return []capacityCase{
		{"adoption_failed cause", "recovery_wait", strPtr(recoveryCauseRetainedRecoveryAdoptionFailed), true, i64Ptr(1)},
		{"worker_memory_pressure cause", "recovery_wait", strPtr(recoveryCauseWorkerMemoryPressure), true, i64Ptr(1)},
		{"data_volume_full cause", "recovery_wait", strPtr(recoveryCauseDataVolumeFull), true, i64Ptr(1)},
		{"vault_locked cause", "recovery_wait", strPtr(recoveryCauseVaultLocked), true, i64Ptr(1)},
		{"no cause", "recovery_wait", nil, true, i64Ptr(1)},
		{"value false", "recovery_wait", exhausted, false, i64Ptr(1)},
		{"nil generation", "recovery_wait", exhausted, true, nil},
		{"zero generation", "recovery_wait", exhausted, true, i64Ptr(0)},
		{"negative generation", "recovery_wait", exhausted, true, i64Ptr(-1)},
		{"running state", "running", exhausted, true, i64Ptr(1)},
		{"limit_wait state", "limit_wait", exhausted, true, i64Ptr(1)},
		{"paused state", "paused", exhausted, true, i64Ptr(1)},
	}
}

func TestValidateRetainedCapacityOnly(t *testing.T) {
	if err := validateRetainedCapacityOnly(StateRequest{State: "recovery_wait"}); err != nil {
		t.Fatalf("absent marker must validate: %v", err)
	}
	valid := capacityCase{"valid", "recovery_wait", strPtr(recoveryCauseRetainedRecoveryExhausted), true, i64Ptr(3)}
	if err := validateRetainedCapacityOnly(valid.request()); err != nil {
		t.Fatalf("valid marker refused: %v", err)
	}
	for _, tc := range capacityRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRetainedCapacityOnly(tc.request())
			if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "retained_capacity_only") {
				t.Fatalf("err = %v, want ErrInvalidState naming retained_capacity_only", err)
			}
		})
	}
}

// TestSetStateRefusesRetainedCapacityOnlyBeforeAnyWrite drives the real SetState path, so a marker
// riding the worker_memory_pressure early-return park (or any other shape) is refused before
// the run is read or mutated.
func TestSetStateRefusesRetainedCapacityOnlyBeforeAnyWrite(t *testing.T) {
	for _, tc := range capacityRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			run := runningRun(false)
			fs, svc, wkr := limitParkFixture(t, run)
			_, applied, err := svc.SetState(context.Background(), wkr, run.ID, tc.request())
			if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "retained_capacity_only") {
				t.Fatalf("SetState err = %v, want ErrInvalidState naming retained_capacity_only", err)
			}
			if applied || fs.setRecoveryWait != nil || fs.setFailed != nil {
				t.Fatalf("refused marker mutated the run: applied=%v recovery_wait=%v failed=%v", applied, fs.setRecoveryWait, fs.setFailed)
			}
		})
	}
}

// TestReconciliationPreludeRefusesRetainedCapacityOnlyBeforeEvidence: a running report carrying a
// valid registration_evidence object and the marker is refused before the evidence write.
func TestReconciliationPreludeRefusesRetainedCapacityOnlyBeforeEvidence(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)
	epoch := int64(1)
	req := StateRequest{
		State: "running", ClaimGeneration: i64Ptr(1), RetainedCapacityOnly: boolPtr(true),
		RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", ClaimGeneration: 1, RegistrationEpoch: &epoch},
	}
	result, err := svc.SetStateReportWithReconciliation(context.Background(), wkr, run.ID, req)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
	if len(fs.evidenceArgs) != 0 || result.RegistrationEvidence != "" {
		t.Fatalf("registration evidence written before the marker was refused: %v %q", fs.evidenceArgs, result.RegistrationEvidence)
	}
}

func TestStateRequestDecodesRetainedCapacityOnly(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader([]byte(
		`{"status":"recovery_wait","recovery_cause":"retained_recovery_exhausted","retained_capacity_only":true,"claim_generation":2}`)))
	dec.DisallowUnknownFields()
	var req StateRequest
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if req.RetainedCapacityOnly == nil || !*req.RetainedCapacityOnly {
		t.Fatalf("RetainedCapacityOnly = %v, want true", req.RetainedCapacityOnly)
	}
	out, err := json.Marshal(StateRequest{State: "running"})
	if err != nil || strings.Contains(string(out), "retained_capacity_only") {
		t.Fatalf("absent marker must be omitted from the wire: %s %v", out, err)
	}
}
