package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyRevisionGateProjection(t *testing.T) {
	for _, mode := range []string{"gate", "no_gate", "sql_null", "json_null", "corrupt", "partial", "wrong_gate", "wrong_selection", "human_primary", "checker", "interactive", "claude", "child", "task", "prompt", "self_improve", "storage_error"} {
		t.Run(mode, func(t *testing.T) {
			r, source, reader, _ := semanticPolicyGateFixture(t)
			want := mode == "gate" || mode == "no_gate" || mode == "human_primary" || mode == "task" || mode == "prompt" || mode == "self_improve"
			var wantErr error
			switch mode {
			case "no_gate":
				r.GateRevision, r.GatePresentationID, r.GatePayloadDigest, r.GatePresentedPayload = 0, pgconv.UUIDOrNull(uuid.Nil), nil, nil
				source.Gate = nil
			case "wrong_gate":
				source.Gate.PresentationID = uuid.NewString()
			case "wrong_selection":
				r.PolicyCandidateCrossCheckID = pgconv.UUID(uuid.New())
			case "human_primary":
				id := apitypes.PolicyStopDecimal("1")
				source.PrimaryRevision = &apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &id}
			case "task", "prompt", "self_improve":
				r.Kind = mode
			case "checker":
				r.Kind = "cross_check"
			case "interactive":
				r.Interactive = true
			case "claude":
				r.Harness = "claude"
			case "child":
				r.TargetRunID = pgconv.UUID(uuid.New())
			case "storage_error":
				wantErr = errors.New("projection history fault")
				reader.historyErr = wantErr
			}
			r.PolicyRevisionSource = semanticJSON(t, source)
			switch mode {
			case "sql_null":
				r.PolicyRevisionSource = nil
			case "json_null":
				r.PolicyRevisionSource = []byte("null")
			case "corrupt":
				r.PolicyRevisionSource = []byte("{")
			case "partial":
				r.PolicyRevisionSource = []byte(`{"version":1,"gate":null}`)
			}
			got, err := policyRevisionGateProjection(context.Background(), reader, r)
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if !want {
				if got != nil {
					t.Fatalf("unknown projected %s", got)
				}
				return
			}
			if mode == "no_gate" {
				if string(got) != "null" {
					t.Fatalf("verified no-gate=%s", got)
				}
			} else {
				var gate apitypes.PolicyStopGateContext
				if err := json.Unmarshal(got, &gate); err != nil || gate != *source.Gate {
					t.Fatalf("gate=%s error=%v", got, err)
				}
			}
		})
	}
}

func TestPolicyRevisionProjectionNegotiation(t *testing.T) {
	for _, mode := range []string{"enabled", "hold_off", "snapshot_off", "lease_off", "worker_missing", "fence_missing"} {
		t.Run(mode, func(t *testing.T) {
			s := &Service{p: Params{PolicyHoldNegotiated: true, PolicyHoldSnapshotEnabled: true, TerminalPendingLease: time.Minute}}
			w := store.Worker{ProtocolCapabilities: []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}}
			switch mode {
			case "hold_off":
				s.p.PolicyHoldNegotiated = false
			case "snapshot_off":
				s.p.PolicyHoldSnapshotEnabled = false
			case "lease_off":
				s.p.TerminalPendingLease = 0
			case "worker_missing":
				w.ProtocolCapabilities = []string{capability.CredentialSwitchV1}
			case "fence_missing":
				w.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1}
			}
			if got := s.policyRevisionProjectionNegotiated(w); got != (mode == "enabled") {
				t.Fatalf("negotiated=%v", got)
			}
		})
	}
}

func TestPolicyRevisionGateClaimEncoding(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage(`{"revision":"1","presentation_id":"30680000-0000-0000-0000-000000000001","payload_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)} {
		data, err := json.Marshal(ClaimPayload{PolicyRevisionGate: raw})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		value, present := fields["policy_revision_gate"]
		if present != (raw != nil) || string(value) != string(raw) {
			t.Fatalf("projection=%s present=%v want=%s", value, present, raw)
		}
	}
}
