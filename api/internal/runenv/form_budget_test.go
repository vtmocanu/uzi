package runenv

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const formBudgetFixture = "../../../fixtures/run-env/form-budget.json"

// The Go constants are a copy of the shared fixture, which the worker's test pins
// RUN_ENV_FORM_CHARS_MAX and the per-byte bound against.
func TestFormBudgetEqualsSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(formBudgetFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		FormCharsMax           int `json:"form_chars_max"`
		FormCharsPerSecretByte int `json:"form_chars_per_secret_byte"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatal(err)
	}
	if fx.FormCharsMax != FormCharsMax || fx.FormCharsPerSecretByte != FormCharsPerSecretByte {
		t.Fatalf("fixture form budget %+v differs from FormCharsMax=%d FormCharsPerSecretByte=%d", fx, FormCharsMax, FormCharsPerSecretByte)
	}
}

func TestMaxClaimSecretBytesFitsTheLimits(t *testing.T) {
	if got := FormCharsPerSecretByte * MaxClaimSecretBytes; got > FormCharsMax {
		t.Fatalf("a full claim counts up to %d units, over the %d cap", got, FormCharsMax)
	}
	if FormCharsPerSecretByte*(MaxClaimSecretBytes+1) <= FormCharsMax {
		t.Fatalf("MaxClaimSecretBytes %d is not the largest whole byte count under the cap", MaxClaimSecretBytes)
	}
	// Smaller than a scope's value bytes, so the form budget is the binding limit for secrets.
	if MaxClaimSecretBytes >= MaxScopeValueBytes {
		t.Fatalf("MaxClaimSecretBytes %d is not below MaxScopeValueBytes %d", MaxClaimSecretBytes, MaxScopeValueBytes)
	}
	if MaxClaimSecretBytes != 116959 {
		t.Fatalf("MaxClaimSecretBytes = %d", MaxClaimSecretBytes)
	}
}
