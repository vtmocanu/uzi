package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

// TestSetStateDiskParkPreventiveRequiresDataVolumeFull (PRD #1809 M5): disk_park_preventive only
// qualifies a data_volume_full park, so carrying it with another cause, or with no cause, is
// ErrInvalidState (400) before any state SQL — nothing parks and nothing fails.
func TestSetStateDiskParkPreventiveRequiresDataVolumeFull(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause *string
		flag  bool
	}{
		{"vault_locked cause", strPtr(recoveryCauseVaultLocked), true},
		{"forge cause", strPtr("forge_unreachable"), true},
		{"no cause", nil, true},
		{"explicit false with no cause", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runningRun(false)
			fs, svc, wkr := limitParkFixture(t, run)

			_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
				State: "recovery_wait", RecoveryCause: tc.cause, DiskParkPreventive: boolPtr(tc.flag), ClaimGeneration: i64Ptr(1),
			})
			if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "disk_park_preventive") {
				t.Fatalf("SetState err = %v, want ErrInvalidState naming disk_park_preventive", err)
			}
			if applied || fs.setRecoveryWait != nil || fs.setFailed != nil {
				t.Fatalf("a rejected disk_park_preventive mutated the run: applied=%v recovery_wait=%v failed=%v",
					applied, fs.setRecoveryWait, fs.setFailed)
			}
		})
	}
}

// TestSetStateDataVolumeFullRequiresClaimGeneration: like the forge park, a data_volume_full
// park report must carry the claim generation it holds; absent is a protocol error (400), never
// an unfenced park.
func TestSetStateDataVolumeFullRequiresClaimGeneration(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseDataVolumeFull),
	})
	if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "claim_generation") {
		t.Fatalf("SetState err = %v, want ErrInvalidState naming claim_generation", err)
	}
	if applied || fs.setRecoveryWait != nil || fs.setFailed != nil {
		t.Fatalf("a generation-less disk park mutated the run: applied=%v recovery_wait=%v failed=%v",
			applied, fs.setRecoveryWait, fs.setFailed)
	}
}

// TestSetStateDataVolumeFullNeverTakesOrdinaryPark pins the routing: data_volume_full (counted or
// preventive) is accepted by the cause vocabulary and routed to its own transaction, never to the
// ordinary SetRunRecoveryWait park (which would store the cause without counting it or applying
// the cap) and never to a fail. The fake fixture wires no tx beginner, so the transaction refuses
// with a non-validation error and nothing is written.
func TestSetStateDataVolumeFullNeverTakesOrdinaryPark(t *testing.T) {
	for _, preventive := range []*bool{nil, boolPtr(false), boolPtr(true)} {
		run := runningRun(false)
		fs, svc, wkr := limitParkFixture(t, run)
		if svc.txBeginner != nil {
			t.Fatal("fixture unexpectedly wired a tx beginner; this test needs the nil path")
		}

		_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
			State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseDataVolumeFull),
			DiskParkPreventive: preventive, ClaimGeneration: i64Ptr(1),
		})
		if err == nil || errors.Is(err, ErrInvalidState) {
			t.Fatalf("preventive=%v: SetState err = %v, want the no-tx-beginner refusal (not a validation error)", preventive, err)
		}
		if applied || fs.setRecoveryWait != nil || fs.setFailed != nil {
			t.Fatalf("preventive=%v: the disk park fell through to another write: applied=%v recovery_wait=%v failed=%v",
				preventive, applied, fs.setRecoveryWait, fs.setFailed)
		}
	}
}

// TestDataVolumeFullVocabulary: data_volume_full is a WORKER-reportable recovery cause and a
// SERVER-only fail_origin (the cap stamps it; a worker naming it on `failed` is coerced away),
// and the cap-fail is never judged.
func TestDataVolumeFullVocabulary(t *testing.T) {
	if !recoveryWaitCauses[recoveryCauseDataVolumeFull] || serverRecoveryWaitCauses[recoveryCauseDataVolumeFull] {
		t.Fatal("data_volume_full must be a worker-reportable recovery cause")
	}
	if !failOriginSet[recoveryCauseDataVolumeFull] {
		t.Fatal("data_volume_full is not in the fail_origin vocabulary")
	}
	if workerReportableFailOrigins[recoveryCauseDataVolumeFull] {
		t.Fatal("data_volume_full is worker-reportable as a fail_origin; it must be server-derived only")
	}
	if !neverJudgeFailOrigins[recoveryCauseDataVolumeFull] || preStartInfraFailOrigins[recoveryCauseDataVolumeFull] {
		t.Fatal("data_volume_full must skip the judge regardless of iteration_count")
	}
}

// TestStateRequestDecodesDiskParkPreventive: the wire field exists, so a new worker's report is
// not rejected by the strict decoder (httpx.DecodeJSON disallows unknown fields).
func TestStateRequestDecodesDiskParkPreventive(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader([]byte(
		`{"status":"recovery_wait","recovery_cause":"data_volume_full","disk_park_preventive":true,"claim_generation":2}`)))
	dec.DisallowUnknownFields()
	var req StateRequest
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if req.DiskParkPreventive == nil || !*req.DiskParkPreventive {
		t.Fatalf("DiskParkPreventive = %v, want true", req.DiskParkPreventive)
	}
}
