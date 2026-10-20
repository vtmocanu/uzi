package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func stopHistoryRecord(t *testing.T) policyStopResolutionV1 {
	t.Helper()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	gate := apitypes.PolicyStopGateContext{Revision: "2", PresentationID: uuid.NewString(), PayloadDigest: strings.Repeat("a", 64)}
	activeID, origin, gen := apitypes.PolicyStopDecimal("11"), apitypes.PolicyStopDecimal("1"), apitypes.PolicyStopDecimal("3")
	worker := uuid.NewString()
	active := apitypes.PolicyStopHumanFeedback{InputID: activeID, Body: strPtr("active body"), CreatedAt: &now, GateBinding: strPtr("bound"), GateRevision: &origin, ConsumedAt: &now, ConsumedWorkerID: &worker, ConsumedClaimGeneration: &gen, AppliedAt: &now}
	predecessor := active
	predecessor.InputID, predecessor.Body, predecessor.Disposition = "10", strPtr("predecessor body"), strPtr("superseded")
	pending := active
	current := apitypes.PolicyStopDecimal("2")
	pending.InputID, pending.Body, pending.AppliedAt, pending.GateRevision = "12", strPtr("pending body"), nil, &current
	record := policyStopResolutionV1{Version: 1, ResolutionID: uuid.New(), AcceptedBy: uuid.New(), AcceptedAt: now, ScopeInputID: 99,
		Manifest: apitypes.PolicyStopManifest{Version: 1, RunID: uuid.NewString(), OutcomeID: uuid.NewString(), SourceGeneration: gen, ReviewGeneration: gen,
			SourceContext: apitypes.PolicyRevisionSourceV1{Version: 1, Gate: &gate, PrimaryRevision: &apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &activeID}, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}},
			Gate:          &apitypes.PolicyStopGateSnapshot{PolicyStopGateContext: gate},
			Execution:     apitypes.PolicyStopExecution{Contract: json.RawMessage("null"), Frozen: []apitypes.Milestone{{ID: "m1", Title: "one"}}, CompletedIDs: []string{"m1"}, ProposedCeiling: 1},
			HumanFeedback: []apitypes.PolicyStopHumanFeedback{predecessor, active, pending}, CrossCheckFeedback: []apitypes.PolicyStopCrossCheckFeedback{},
		}}
	return record
}

// Rehashing removes token mismatch as an alternative reason for rejection.
func TestPolicyStopHistoryImmutableParser(t *testing.T) {
	mutations := map[string]func(*policyStopResolutionV1){
		"valid": func(*policyStopResolutionV1) {},
		"missing_active": func(r *policyStopResolutionV1) {
			r.Manifest.HumanFeedback = append(r.Manifest.HumanFeedback[:1], r.Manifest.HumanFeedback[2:]...)
		},
		"applied_alone":  func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[1].ConsumedAt = nil },
		"missing_worker": func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[1].ConsumedWorkerID = nil },
		"zero_worker": func(r *policyStopResolutionV1) {
			r.Manifest.HumanFeedback[1].ConsumedWorkerID = strPtr(uuid.Nil.String())
		},
		"future_generation": func(r *policyStopResolutionV1) {
			v := apitypes.PolicyStopDecimal("4")
			r.Manifest.HumanFeedback[1].ConsumedClaimGeneration = &v
		},
		"future_origin": func(r *policyStopResolutionV1) {
			v := apitypes.PolicyStopDecimal("3")
			r.Manifest.HumanFeedback[1].GateRevision = &v
		},
		"current_origin": func(r *policyStopResolutionV1) {
			v := apitypes.PolicyStopDecimal("2")
			r.Manifest.HumanFeedback[1].GateRevision = &v
		},
		"unknown_disposition":    func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[0].Disposition = strPtr("advisory") },
		"pending_superseded":     func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[2].Disposition = strPtr("superseded") },
		"history_included":       func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[1].IncludedAt = &r.AcceptedAt },
		"valid_history_reported": func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[1].InclusionReported = true },
		"history_settled":        func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[0].SettledAt = &r.AcceptedAt },
		"history_stop": func(r *policyStopResolutionV1) {
			v := apitypes.PolicyStopDecimal("99")
			r.Manifest.HumanFeedback[0].SupersededByStopInputID = &v
		},
		"history_wrong_origin": func(r *policyStopResolutionV1) {
			v := apitypes.PolicyStopDecimal("2")
			r.Manifest.HumanFeedback[0].GateRevision = &v
		},
		"history_wrong_worker": func(r *policyStopResolutionV1) {
			r.Manifest.HumanFeedback[0].ConsumedWorkerID = strPtr(uuid.NewString())
		},
		"history_wrong_timestamp": func(r *policyStopResolutionV1) {
			v := r.AcceptedAt.Add(time.Second)
			r.Manifest.HumanFeedback[0].AppliedAt = &v
		},
		"duplicate": func(r *policyStopResolutionV1) {
			r.Manifest.HumanFeedback = append(r.Manifest.HumanFeedback, r.Manifest.HumanFeedback[0])
		},
		"two_active": func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[0].Disposition = nil },
		"primary_predecessor": func(r *policyStopResolutionV1) {
			r.Manifest.SourceContext.PrimaryRevision.InputID = &r.Manifest.HumanFeedback[0].InputID
		},
		"pending_future_binding": func(r *policyStopResolutionV1) {
			v := apitypes.PolicyStopDecimal("9")
			r.Manifest.HumanFeedback[2].GateRevision = &v
		},
		"primary_undelivered": func(r *policyStopResolutionV1) {
			id := r.Manifest.HumanFeedback[2].InputID
			r.Manifest.SourceContext.PrimaryRevision = &apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &id}
			r.Manifest.HumanFeedback[2].ConsumedAt = nil
			r.Manifest.HumanFeedback[2].ConsumedWorkerID = nil
			r.Manifest.HumanFeedback[2].ConsumedClaimGeneration = nil
		},
		"pending_partial_delivery": func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[2].ConsumedWorkerID = nil },
		"valid_pending_reported":   func(r *policyStopResolutionV1) { r.Manifest.HumanFeedback[2].InclusionReported = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := stopHistoryRecord(t)
			mutate(&r)
			token, err := policyStopManifestToken(r.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			r.IfMatch = token
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			run := uuid.MustParse(r.Manifest.RunID)
			parsed, err := parsePolicyStopResolution(raw, r.AcceptedBy, run)
			if !strings.HasPrefix(name, "valid") {
				if err == nil {
					t.Fatal("rehashing accepted malformed snapshot")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			receipt := policyStopReceipt(parsed, true)
			if len(receipt.SupersededRevisions) != 1 || receipt.SupersededRevisions[0].InputID == nil || *receipt.SupersededRevisions[0].InputID != "12" {
				t.Fatalf("history in waiver cohort: %+v", receipt)
			}
		})
	}
}

func TestPolicyStopHistoryParserRequiredFields(t *testing.T) {
	for _, mode := range []string{"missing_included_at", "missing_applied_at", "null_reported", "null_inventory"} {
		t.Run(mode, func(t *testing.T) {
			record := stopHistoryRecord(t)
			var err error
			record.IfMatch, err = policyStopManifestToken(record.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			manifest := value["manifest"].(map[string]any)
			rows := manifest["human_feedback"].([]any)
			// Null-valued and false fields must remain explicitly present in the snapshot.
			pending := rows[2].(map[string]any)
			switch mode {
			case "missing_included_at":
				delete(pending, "included_at")
			case "missing_applied_at":
				delete(pending, "applied_at")
			case "null_reported":
				pending["inclusion_reported"] = nil
			case "null_inventory":
				manifest["human_feedback"] = nil
			}
			raw, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parsePolicyStopResolution(raw, record.AcceptedBy, uuid.MustParse(record.Manifest.RunID)); err == nil {
				t.Fatal("accepted incomplete feedback snapshot")
			}
		})
	}
}

func TestPolicyStopHistoryReceiptProof(t *testing.T) {
	for _, encoded := range []string{
		`{"source_revision":1,"revision_input_id":11,"superseded_revision_input_ids":[10]}`,
		`{"source_revision":1,"revision_input_id":11,"unknown":true}`,
		`{"source_revision":1,"source_revision":1,"revision_input_id":11}`,
		`{"source_revision":2,"revision_input_id":11}`,
		`{"source_revision":0,"revision_input_id":11}`,
		`{"source_revision":1,"revision_input_id":12}`,
		`{"source_revision":1,"revision_input_id":11,"superseded_revision_input_ids":[10,10]}`,
		`{"source_revision":1,"revision_input_id":11,"superseded_revision_input_ids":[11]}`,
		`{"source_revision":1,"revision_input_id":11,"superseded_revision_input_ids":[0]}`,
		`{"source_revision":1,"revision_input_id":11,"superseded_revision_input_ids":null}`,
		`null`,
	} {
		t.Run(encoded, func(t *testing.T) {
			_, valid := policyStopHistoryReceipt([]byte(`{"revision_retry_receipt":`+encoded+`}`), 11, 2)
			want := encoded == `{"source_revision":1,"revision_input_id":11,"superseded_revision_input_ids":[10]}`
			if valid != want {
				t.Fatalf("valid=%v want=%v", valid, want)
			}
		})
	}
}
