package workersvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func coverageFixture() apitypes.CodeCrossCheckCoverage {
	return apitypes.CodeCrossCheckCoverage{Version: 1,
		Entries:           []apitypes.CodeCrossCheckCoverageEntry{{Path: "a.go", Status: "examined", Reason: "checked contracts", RecordedRead: true}},
		OmittedCountExact: true, ActivityComplete: true, Reasons: []string{}}
}
func coverageJSON(t *testing.T, account apitypes.CodeCrossCheckCoverage) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func decodeCoverage(t *testing.T, raw []byte) apitypes.CodeCrossCheckCoverage {
	t.Helper()
	var account apitypes.CodeCrossCheckCoverage
	if err := json.Unmarshal(raw, &account); err != nil {
		t.Fatal(err)
	}
	return account
}
func TestCodeCrossCheckCoverageNormalization(t *testing.T) {
	valid := coverageJSON(t, coverageFixture())
	if got := NormalizeCodeCrossCheckCoverage("completed", valid); !bytes.Equal(got, valid) {
		t.Fatalf("valid account changed: %s", got)
	}
	for _, raw := range []json.RawMessage{nil, []byte("null"), []byte(" null ")} {
		if NormalizeCodeCrossCheckCoverage("completed", raw) != nil {
			t.Fatal("legacy coverage invented")
		}
	}
	if NormalizeCodeCrossCheckCoverage("failed", valid) != nil {
		t.Fatal("failed verdict retained coverage")
	}
	for name, raw := range map[string]json.RawMessage{
		"malformed":        []byte("{"),
		"scalar":           []byte("true"),
		"partial":          []byte(`{"version":1,"entries":[]}`),
		"over-cap":         []byte(strings.Repeat(" ", codeCoverageBytes+1)),
		"duplicate key":    bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"wrong key case":   bytes.Replace(valid, []byte(`"version"`), []byte(`"Version"`), 1),
		"null boolean":     bytes.Replace(valid, []byte(`"activity_complete":true`), []byte(`"activity_complete":null`), 1),
		"unsafe count":     bytes.Replace(valid, []byte(`"omitted_paths":0`), []byte(`"omitted_paths":9007199254740992`), 1),
		"fractional count": bytes.Replace(valid, []byte(`"omitted_paths":0`), []byte(`"omitted_paths":0.5`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", raw))
			if !got.InventoryIncomplete || got.ActivityComplete || got.OmittedCountExact || len(got.Entries) != 0 {
				t.Fatalf("unsafe fallback: %+v", got)
			}
		})
	}
	for _, mutate := range []func(*apitypes.CodeCrossCheckCoverage){
		func(c *apitypes.CodeCrossCheckCoverage) { c.OmittedPaths = -1 },
		func(c *apitypes.CodeCrossCheckCoverage) { c.Reasons = []string{"model-authored reason"} },
		func(c *apitypes.CodeCrossCheckCoverage) { c.Entries = append(c.Entries, c.Entries[0]) },
		func(c *apitypes.CodeCrossCheckCoverage) { c.Version = 2 },
		func(c *apitypes.CodeCrossCheckCoverage) {
			c.Entries = append(c.Entries, c.Entries[0])
			c.Entries[0].Status = "invalid"
		},
	} {
		account := coverageFixture()
		mutate(&account)
		got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, account)))
		if !got.InventoryIncomplete || len(got.Entries) != 0 {
			t.Fatalf("invalid account trusted: %+v", got)
		}
	}
}
func TestCodeCrossCheckCoverageMixedEntries(t *testing.T) {
	expected := coverageFixture().Entries[0]
	raw := bytes.Replace(coverageJSON(t, coverageFixture()), []byte(`"entries":[`), []byte(`"entries":[{"path":"b.go","status":"examined","reason":"checked","recorded_read":"yes"},`), 1)
	got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", raw))
	if len(got.Entries) != 1 || got.Entries[0] != expected || got.OmittedPaths != 1 || !got.InventoryIncomplete || got.OmittedCountExact {
		t.Fatalf("mixed account lost the valid sibling or omission evidence: %+v", got)
	}
}

func TestCodeCrossCheckCoverageBoundaries(t *testing.T) {
	account := coverageFixture()
	account.Entries[0].Path = strings.Repeat("é", 256)
	account.Entries[0].Reason = strings.Repeat("é", 128)
	if got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, account))); got.InventoryIncomplete || len(got.Entries) != 1 {
		t.Fatal("exact UTF-8 bounds refused")
	}
	for _, field := range []string{"path", "reason", "status", "nul path", "nul reason"} {
		c := coverageFixture()
		switch field {
		case "path":
			c.Entries[0].Path = strings.Repeat("é", 257)
		case "reason":
			c.Entries[0].Reason = strings.Repeat("é", 129)
		case "status":
			c.Entries[0].Status = "invented"
		case "nul path":
			c.Entries[0].Path = "a\u0000.go"
		case "nul reason":
			c.Entries[0].Reason = "reason\u0000"
		}
		got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, c)))
		if !got.InventoryIncomplete || len(got.Entries) != 0 || got.OmittedPaths != 1 {
			t.Fatalf("%s: invalid entry promoted: %+v", field, got)
		}
	}
	account = coverageFixture()
	account.Entries = nil
	for i := 0; i < 129; i++ {
		account.Entries = append(account.Entries, apitypes.CodeCrossCheckCoverageEntry{Path: fmt.Sprintf("file-%03d", i), Status: "unassessed"})
	}
	got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, account)))
	if len(got.Entries) != 128 || got.Entries[127].Path != "file-127" || got.OmittedPaths != 1 || !got.OmittedCountExact || !got.InventoryIncomplete {
		t.Fatalf("overflow: %+v", got)
	}
	account.OmittedPaths = codeCoverageSafeInteger
	got = decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, account)))
	if got.OmittedPaths != codeCoverageSafeInteger || got.OmittedCountExact {
		t.Fatal("omission count overflow")
	}
}
func TestCodeCrossCheckCoverageJSONByteCap(t *testing.T) {
	raw := coverageJSON(t, coverageFixture())
	atCap := append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), codeCoverageBytes-len(raw))...)
	if got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", atCap)); got.InventoryIncomplete {
		t.Fatal("exact JSON byte cap refused")
	}
	overCap := append(atCap, ' ')
	if got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", overCap)); !got.InventoryIncomplete || len(got.Entries) != 0 {
		t.Fatal("over-cap account trusted")
	}
	for _, mutate := range []func(*apitypes.CodeCrossCheckCoverage){
		func(c *apitypes.CodeCrossCheckCoverage) { c.UnknownJudgments = 1 },
		func(c *apitypes.CodeCrossCheckCoverage) { c.InvalidJudgments = 1 },
		func(c *apitypes.CodeCrossCheckCoverage) { c.OmittedJudgments = 1 },
		func(c *apitypes.CodeCrossCheckCoverage) { c.ActivityComplete = false },
	} {
		c := coverageFixture()
		mutate(&c)
		if got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, c))); !got.InventoryIncomplete {
			t.Fatal("partial account claimed complete inventory")
		}
	}
	c := coverageFixture()
	c.Reasons = []string{"inventory_truncated", "inventory_failed", "invalid_judgments", "unknown_judgments", "judgments_truncated", "activity_incomplete", "coverage_invalid", "coverage_too_large"}
	if got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, c))); len(got.Reasons) != 8 || len(got.Entries) != 1 {
		t.Fatal("eight fixed reasons refused")
	}
	c.Reasons = append(c.Reasons, "coverage_invalid")
	if got := decodeCoverage(t, NormalizeCodeCrossCheckCoverage("completed", coverageJSON(t, c))); len(got.Entries) != 0 {
		t.Fatal("over-cap reason list trusted")
	}
}
func TestCodeCrossCheckCoverageClaimRollout(t *testing.T) {
	p := guidanceWireFixture(t)
	digest := p.CrossCheck.CandidateDigest
	wire, wireErr := MarshalCrossCheckClaim(p)
	if wireErr != nil || !bytes.Contains(wire, []byte(`"coverage_supported":true`)) {
		t.Fatalf("coverage claim wire: %v", wireErr)
	}
	if !p.CrossCheck.CoverageSupported {
		t.Fatal("new code claim lacks support")
	}
	p.CrossCheck.CoverageSupported = false
	if err := ValidateCrossCheckClaim(p); err != nil {
		t.Fatalf("legacy code claim refused: %v", err)
	}
	p.CrossCheck.CoverageSupported = true
	if err := ValidateCrossCheckClaim(p); err != nil || p.CrossCheck.CandidateDigest != digest {
		t.Fatalf("support changed candidate: %v", err)
	}
	p = boundedCheckerFixture(t)
	raw, err := MarshalCrossCheckClaim(p)
	if err != nil || bytes.Contains(raw, []byte("coverage_supported")) {
		t.Fatalf("plan gained coverage: %v", err)
	}
	p.CrossCheck.CoverageSupported = true
	if ValidateCrossCheckClaim(p) == nil {
		t.Fatal("plan admitted code capability")
	}
}
