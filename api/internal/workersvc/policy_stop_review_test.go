package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestPolicyStopReviewSourceParser(t *testing.T) {
	good := `{"version":1,"gate":null,"primary_revision":{"source":"run_user_input","input_id":"9007199254740993"},"associated_cross_checks":[]}`
	got, err := apitypes.ParsePolicyRevisionSourceV1([]byte(good))
	if err != nil || string(*got.PrimaryRevision.InputID) != "9007199254740993" {
		t.Fatalf("precision: %v %v", got, err)
	}
	raw, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"input_id":"9007199254740993"`) {
		t.Fatalf("wire precision: %s %v", raw, err)
	}
	cc := `{"source":"plan_cross_check","cross_check_id":"00000000-0000-0000-0000-000000000001"}`
	for name, bad := range map[string]string{
		"null":                "null",
		"version":             strings.Replace(good, `"version":1`, `"version":2`, 1),
		"duplicate_key":       strings.Replace(good, `"version":1`, `"version":1,"version":1`, 1),
		"extra":               strings.Replace(good, `"version":1`, `"extra":true,"version":1`, 1),
		"missing_primary":     strings.Replace(good, `"primary_revision":{"source":"run_user_input","input_id":"9007199254740993"},`, "", 1),
		"unknown_association": strings.Replace(good, `"associated_cross_checks":[]`, `"associated_cross_checks":null`, 1),
		"number":              strings.Replace(good, `"9007199254740993"`, `9007199254740993`, 1),
		"leading_zero":        strings.Replace(good, "9007199254740993", "09007199254740993", 1),
		"overflow":            strings.Replace(good, "9007199254740993", "9223372036854775808", 1),
		"extra_union_arm":     strings.Replace(good, `"input_id":"9007199254740993"`, `"input_id":"9007199254740993","cross_check_id":null`, 1),
		"unknown_source":      strings.Replace(good, "run_user_input", "latest", 1),
		"trailing":            good + " {}",
		"human_association":   strings.Replace(good, `"associated_cross_checks":[]`, `"associated_cross_checks":[{"source":"run_user_input","input_id":"1"}]`, 1),
		"duplicate_ref":       strings.Replace(good, `"associated_cross_checks":[]`, `"associated_cross_checks":[`+cc+","+cc+`]`, 1),
		"uppercase_uuid":      strings.Replace(good, `"associated_cross_checks":[]`, `"associated_cross_checks":[{"source":"plan_cross_check","cross_check_id":"AAAAAAAA-0000-0000-0000-000000000001"}]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := apitypes.ParsePolicyRevisionSourceV1([]byte(bad)); err == nil {
				t.Fatal("accepted malformed source")
			}
		})
	}
}

func TestPolicyStopReviewZeroMarshal(t *testing.T) {
	if _, err := json.Marshal(apitypes.PolicyStopReview{}); err != nil {
		t.Fatalf("zero DTO cannot be recorded: %v", err)
	}
	var decimal apitypes.PolicyStopDecimal
	if err := json.Unmarshal([]byte(`""`), &decimal); err == nil {
		t.Fatal("zero DTO representation must not be accepted as source authority")
	}
}

func TestPolicyStopReviewCanonicalToken(t *testing.T) {
	// Public source parser and manifest DTO are the future confirmation seams.
	source, err := apitypes.ParsePolicyRevisionSourceV1([]byte(`{"version":1,"gate":null,"primary_revision":null,"associated_cross_checks":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := apitypes.PolicyStopManifest{Version: 1, SourceGeneration: "1", ReviewGeneration: "9007199254740993", SourceContext: source,
		Gate:      &apitypes.PolicyStopGateSnapshot{PolicyStopGateContext: apitypes.PolicyStopGateContext{Revision: "1"}, Payload: apitypes.PolicyStopGatePayload{Milestones: json.RawMessage("null")}},
		Execution: apitypes.PolicyStopExecution{Contract: json.RawMessage(`{"z":9007199254740993,"a":1}`)}}
	a, err := policyStopManifestToken(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Execution.Contract = json.RawMessage(`{ "a":1, "z":9007199254740993 }`)
	b, err := policyStopManifestToken(m)
	if err != nil || a != b {
		t.Fatalf("canonical token changed: %s %s %v", a, b, err)
	}
	m.Execution.Contract = json.RawMessage(`{"a":1,"z":9007199254740992}`)
	c, err := policyStopManifestToken(m)
	if err != nil || a == c {
		t.Fatal("precision lost in hash")
	}
	if !strings.HasPrefix(a, `"policy-stop-v1-`) || !strings.HasSuffix(a, `"`) {
		t.Fatalf("token syntax %s", a)
	}
}
