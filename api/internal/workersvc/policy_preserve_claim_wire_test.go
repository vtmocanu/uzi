package workersvc

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyResumeInputsClaimWire(t *testing.T) {
	for _, pending := range []bool{false, true} {
		f := newClaimFixture(t)
		f.fs.claimCtx.PolicyResumeInputsPending = pending
		payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner}, nil)
		if err != nil || payload == nil {
			t.Fatalf("claim: %v, %v", payload, err)
		}
		wire, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if !pending {
			if _, present := got["policy_resume_inputs_pending"]; present {
				t.Fatal("ordinary/delivered claim must omit the input barrier marker")
			}
			continue
		}
		want := readClaimFixture(t, "policy-resume-inputs-claim.json")
		if got["policy_resume_inputs_pending"] != want["policy_resume_inputs_pending"] {
			t.Fatalf("pending guidance claim %s, fixture %v", wire, want)
		}
		if payload.PolicyPreserveOnly || payload.PolicyFreshSession || payload.PolicySavedWorkHead != nil {
			t.Fatal("the input marker must grant no preserving, Fresh or recovery-head authority")
		}
	}
}

func TestPolicyResumeContextClaimWire(t *testing.T) {
	for _, pending := range []bool{false, true} {
		f := newClaimFixture(t)
		f.fs.claimCtx.PolicyResumeContextPending = pending
		payload, err := f.svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: f.owner}, nil)
		if err != nil || payload == nil {
			t.Fatalf("claim: %v, %v", payload, err)
		}
		wire, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(wire, &got); err != nil {
			t.Fatal(err)
		}
		if !pending {
			if _, present := got["policy_resume_context_pending"]; present {
				t.Fatal("ordinary or session-reported claim must omit the context marker")
			}
		} else if want := readClaimFixture(t, "policy-resume-context-claim.json"); got["policy_resume_context_pending"] != want["policy_resume_context_pending"] {
			t.Fatalf("context claim %s, fixture %v", wire, want)
		}
		if payload.PolicyPreserveOnly || payload.PolicyFreshSession || payload.PolicySavedWorkHead != nil {
			t.Fatal("the context marker must grant no preserving, Fresh or recovery-head authority")
		}
	}
}

// The agent reads the same file (fixtures/claims/policy-preserve-claim.json), so the two sides
// cannot drift on the claim keys that carry the preserving constraint.
func TestPolicyPreserveClaimSharedWire(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/policy-preserve-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	head, _ := want["policy_expected_head"].(string)
	if len(want) != 2 || head == "" || want["policy_preserve_only"] != true {
		t.Fatalf("fixture must carry exactly policy_preserve_only=true and a head: %s", raw)
	}
	run := store.Run{PolicyPreserveRequired: true, PolicyRequiredHead: pgconv.Text(head)}
	b, err := json.Marshal(ClaimPayload{PolicyPreserveOnly: run.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(run)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	picked := map[string]any{}
	for k := range want {
		picked[k] = got[k]
	}
	if !reflect.DeepEqual(picked, want) {
		t.Fatalf("claim keys %v, fixture %v", picked, want)
	}
}

func TestPolicyExpectedHeadOnlyWhilePreserving(t *testing.T) {
	head := pgconv.Text("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	for name, run := range map[string]store.Run{
		"no constraint":           {PolicyRequiredHead: head},
		"constraint without head": {PolicyPreserveRequired: true},
	} {
		if got := policyExpectedHead(run); got != nil {
			t.Fatalf("%s: expected no head, got %q", name, *got)
		}
		b, err := json.Marshal(ClaimPayload{PolicyPreserveOnly: run.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(run)})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, ok := m["policy_expected_head"]; ok {
			t.Fatalf("%s: head key present", name)
		}
		if _, ok := m["policy_preserve_only"]; ok != run.PolicyPreserveRequired {
			t.Fatalf("%s: preserve key presence %v", name, ok)
		}
	}
}

// The agent reads fixtures/claims/policy-saved-work-claim.json too: a run resolved
// fresh_from_saved_work carries only policy_saved_work_head, never the preserve keys.
func TestPolicySavedWorkClaimSharedWire(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/policy-saved-work-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	head, _ := want["policy_saved_work_head"].(string)
	if len(want) != 1 || !policyRecoveryHead.MatchString(head) {
		t.Fatalf("fixture must carry exactly one lowercase 40/64-hex policy_saved_work_head: %s", raw)
	}
	run := store.Run{PolicyRequiredHead: pgconv.Text(head)}
	b, err := json.Marshal(ClaimPayload{PolicyPreserveOnly: run.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(run), PolicySavedWorkHead: policySavedWorkHead(run)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	picked := map[string]any{}
	for k := range got {
		if len(k) > 7 && k[:7] == "policy_" {
			picked[k] = got[k]
		}
	}
	if !reflect.DeepEqual(picked, want) {
		t.Fatalf("policy claim keys %v, fixture %v", picked, want)
	}
	// A preserving run never carries the saved-work key.
	run.PolicyPreserveRequired = true
	if policySavedWorkHead(run) != nil {
		t.Fatal("preserving run must not project policy_saved_work_head")
	}
}
