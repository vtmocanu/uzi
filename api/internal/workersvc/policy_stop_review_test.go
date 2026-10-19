package workersvc

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
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

func TestPolicyStopReviewSourceExactKeys(t *testing.T) {
	gate := `{"revision":"1","presentation_id":"00000000-0000-0000-0000-000000000001","payload_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	human := `{"source":"run_user_input","input_id":"1"}`
	cc := `{"source":"plan_cross_check","cross_check_id":"00000000-0000-0000-0000-000000000002"}`
	source := func(g, primary, associated string) string {
		return `{"version":1,"gate":` + g + `,"primary_revision":` + primary + `,"associated_cross_checks":` + associated + `}`
	}
	canonical := source(gate, human, "["+cc+"]")
	if _, err := apitypes.ParsePolicyRevisionSourceV1([]byte(canonical)); err != nil {
		t.Fatalf("canonical positive control: %v", err)
	}
	cases := map[string]string{
		"missing_primary_counted_alias": `{"version":1,"VERSION":1,"gate":` + gate + `,"associated_cross_checks":[]}`,
	}
	for _, key := range []string{"version", "gate", "primary_revision", "associated_cross_checks"} {
		cases["source_"+key] = strings.Replace(canonical, `"`+key+`":`, `"`+strings.ToUpper(key)+`":`, 1)
	}
	for _, key := range []string{"revision", "presentation_id", "payload_digest"} {
		cases["gate_"+key] = source(strings.Replace(gate, `"`+key+`":`, `"`+strings.ToUpper(key)+`":`, 1), human, "["+cc+"]")
	}
	for _, key := range []string{"source", "input_id"} {
		cases["human_"+key] = source(gate, strings.Replace(human, `"`+key+`":`, `"`+strings.ToUpper(key)+`":`, 1), "[]")
	}
	for _, key := range []string{"source", "cross_check_id"} {
		alias := strings.Replace(cc, `"`+key+`":`, `"`+strings.ToUpper(key)+`":`, 1)
		cases["primary_cc_"+key] = source(gate, alias, "[]")
		cases["associated_cc_"+key] = source(gate, human, "["+alias+"]")
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := apitypes.ParsePolicyRevisionSourceV1([]byte(raw)); err == nil {
				t.Fatal("accepted a casing alias or missing exact required key")
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

func TestPolicyStopAcceptedManifestSelection(t *testing.T) {
	owner, run := uuid.New(), uuid.New()
	cc := "abcdef12-1234-4234-8234-abcdef123456"
	base := apitypes.PolicyStopManifest{
		Version: 1, RunID: run.String(), OutcomeID: uuid.NewString(), SourceGeneration: "1", ReviewGeneration: "2",
		SourceContext: apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}},
		Execution:     apitypes.PolicyStopExecution{ProposedCeiling: 1},
	}
	encode := func(m apitypes.PolicyStopManifest) []byte {
		t.Helper()
		token, err := policyStopManifestToken(m)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(policyStopResolutionV1{Version: 1, ResolutionID: uuid.New(), AcceptedBy: owner,
			AcceptedAt: time.Now(), ScopeInputID: 1, IfMatch: token, Manifest: m})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, associated := range []bool{false, true} {
		m := base
		if associated {
			m.CandidateCrossCheckID = &cc
			m.SourceContext.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{{Source: "plan_cross_check", CrossCheckID: &cc}}
		}
		raw := encode(m)
		if _, err := parsePolicyStopResolution(raw, owner, run); err != nil {
			t.Fatalf("valid immutable manifest: %v", err)
		}
		for _, mutation := range []struct{ name, old, replacement string }{
			{"missing_selection", `"candidate_cross_check_id":` + string(mustStopJSON(t, m.CandidateCrossCheckID)) + `,`, ""},
			{"source_missing_coverage", `"primary_revision":null,`, ""},
			{"source_missing_gate", `"gate":null,`, ""},
			{"source_missing_version", `"source_context":{"version":1,`, `"source_context":{`},
			{"source_extra_field", `"source_context":{"version":1,`, `"source_context":{"unknown":true,"version":1,`},
			{"selection_number", `"candidate_cross_check_id":` + string(mustStopJSON(t, m.CandidateCrossCheckID)), `"candidate_cross_check_id":1`},
			{"selection_alias", `"candidate_cross_check_id":`, `"Candidate_Cross_Check_Id":`},
			{"source_unknown_version", `"source_context":{"version":1`, `"source_context":{"version":99`},
			{"source_null_coverage", `"associated_cross_checks":` + string(mustStopJSON(t, m.SourceContext.AssociatedCrossChecks)), `"associated_cross_checks":null`},
		} {
			t.Run(mutation.name+string(mustStopJSON(t, associated)), func(t *testing.T) {
				changed := strings.Replace(string(raw), mutation.old, mutation.replacement, 1)
				if changed == string(raw) {
					t.Fatal("mutation missed")
				}
				if _, err := parsePolicyStopResolution([]byte(changed), owner, run); err == nil {
					t.Fatal("invalid raw manifest accepted")
				}
			})
		}
	}
	// A manifest-level case alias must not replace the validated canonical V1.
	t.Run("manifest_source_case_alias", func(t *testing.T) {
		aliased := base
		aliased.SourceContext.Version = 99
		badSource := string(mustStopJSON(t, aliased.SourceContext))
		goodSource := string(mustStopJSON(t, base.SourceContext))
		raw := string(encode(aliased))
		changed := strings.Replace(raw, `"source_context":`+badSource,
			`"source_context":`+goodSource+`,"Source_Context":`+badSource, 1)
		if changed == raw {
			t.Fatal("alias regression missed the source")
		}
		if _, err := parsePolicyStopResolution([]byte(changed), owner, run); err == nil {
			t.Fatal("manifest source alias replaced the validated canonical V1")
		}
	})
	// Recompute valid tokens for invalid authority: hashing alone must not validate it.
	for _, selected := range []*string{nil, ptrStopString(""), ptrStopString(uuid.Nil.String()), ptrStopString(strings.ToUpper(cc)), ptrStopString(uuid.NewString())} {
		m := base
		m.SourceContext.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{{Source: "plan_cross_check", CrossCheckID: &cc}}
		m.CandidateCrossCheckID = selected
		if _, err := parsePolicyStopResolution(encode(m), owner, run); err == nil {
			t.Fatalf("invalid selection accepted: %v", selected)
		}
	}
	m := base
	m.CandidateCrossCheckID = &cc
	if _, err := parsePolicyStopResolution(encode(m), owner, run); err == nil {
		t.Fatal("selection outside V1 accepted")
	}
	// Hashing inconsistent gate authority must not authenticate a receipt.
	gate := apitypes.PolicyStopGateContext{Revision: "1", PresentationID: uuid.NewString(), PayloadDigest: strings.Repeat("a", 64)}
	for _, mode := range []string{"source_only", "manifest_only", "different", "matching"} {
		m := base
		if mode != "manifest_only" {
			m.SourceContext.Gate = &gate
		}
		if mode != "source_only" {
			m.Gate = &apitypes.PolicyStopGateSnapshot{PolicyStopGateContext: gate}
		}
		if mode == "different" {
			m.Gate.Revision = "2"
		}
		_, err := parsePolicyStopResolution(encode(m), owner, run)
		if (err == nil) != (mode == "matching") {
			t.Fatalf("rehashed gate/source %s: %v", mode, err)
		}
	}
	a, _ := policyStopManifestToken(base)
	b, _ := policyStopManifestToken(m)
	if a == b {
		t.Fatal("selection did not bind token")
	}
}

func ptrStopString(v string) *string { return &v }
func mustStopJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
