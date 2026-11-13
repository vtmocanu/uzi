package apitypes

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The route's body decodes strictly: the documented shape round-trips, and any unknown field, at the
// top level or nested, is an error (the handler uses DisallowUnknownFields on this type).
func TestTerminalPreservationRequestStrictDecode(t *testing.T) {
	const full = `{"generation":2,"tip":"a","prior_tip":"b","disposition":"preserve","pack":"UEFDSw==",
		"manifest":{"run_id":"r","generation":2,"worker_id":"w",
		"roots":[{"source_id":"s","attempt_id":"a","commit":"c","tree":"t"}],
		"abandoned":[{"class":"ignored","source_id":"s","extents":{"files":1,"bytes":2}}]}}`
	decode := func(s string) (TerminalPreservationRequest, error) {
		var r TerminalPreservationRequest
		dec := json.NewDecoder(bytes.NewReader([]byte(s)))
		dec.DisallowUnknownFields()
		return r, dec.Decode(&r)
	}
	r, err := decode(full)
	if err != nil || r.Generation != 2 || r.Manifest.Roots[0].Tree != "t" || r.Manifest.Abandoned[0].Extents.Bytes != 2 || r.Pack != "UEFDSw==" {
		t.Fatalf("decode = %+v, %v", r, err)
	}
	for name, bad := range map[string]string{
		"top level":          `{"generation":1,"extra":true}`,
		"manifest":           `{"manifest":{"extra":1}}`,
		"root":               `{"manifest":{"roots":[{"nope":1}]}}`,
		"abandoned":          `{"manifest":{"abandoned":[{"nope":1}]}}`,
		"extents":            `{"manifest":{"abandoned":[{"extents":{"nope":1}}]}}`,
		"wrong type":         `{"generation":"2"}`,
		"foreign root field": `{"manifest":{"roots":[{"pat":"x"}]}}`,
	} {
		if _, err := decode(bad); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
}

// The wire reasons are distinct, non-empty tokens, and the stored ones match the column's CHECK.
func TestTerminalReasonVocabulary(t *testing.T) {
	all := []string{
		TerminalReasonCreateRefused, TerminalReasonUpdateRefused, TerminalReasonUnsupported, TerminalReasonHoldMissing,
		TerminalReasonIdentityChanged, TerminalReasonExcludedOrigin, TerminalReasonManifestInvalid, TerminalReasonManifestMismatch,
		TerminalReasonGenerationBound, TerminalReasonPriorTipMismatch, TerminalReasonPreservationRetired, TerminalReasonPublicationInFlight,
		TerminalReasonForgeUnavailable, TerminalReasonPackInvalid, TerminalReasonPackTooLarge, TerminalReasonTipMissing,
		TerminalReasonCIConfigGuard, TerminalReasonNotDescendant, TerminalReasonRootUnreachable, TerminalReasonTreeMismatch,
		TerminalReasonWalkTooLarge, TerminalReasonForeignTip,
	}
	seen := map[string]bool{}
	for _, r := range all {
		if r == "" || seen[r] {
			t.Fatalf("reason %q is empty or duplicated", r)
		}
		seen[r] = true
	}
	if TerminalReasonCreateRefused != "create_refused" || TerminalReasonUpdateRefused != "update_refused" {
		t.Fatal("the stored refusal reasons must match run_terminal_contributions_refusal_reason_check")
	}
}
