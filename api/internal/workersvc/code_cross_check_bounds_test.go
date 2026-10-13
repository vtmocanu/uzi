package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodeCrossCheckUTF8ByteBoundaries(t *testing.T) {
	f := CodeCrossCheckFinding{ID: "a", Severity: "major", Title: "verify"}
	empty, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	remaining := 2048 - len(empty)
	f.Detail = strings.Repeat("é", remaining/2) + strings.Repeat("x", remaining%2)
	raw, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f})
	if err != nil || len(raw) != 2050 {
		t.Fatalf("exact 2 KiB finding: bytes=%d err=%v", len(raw), err)
	}
	f.Detail += "x"
	if _, err = NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err == nil {
		t.Fatal("2 KiB plus one accepted")
	}
	findings := make([]CodeCrossCheckFinding, 16)
	for i := range findings {
		findings[i] = CodeCrossCheckFinding{ID: string(rune('a' + i)), Severity: "major", Title: "verify"}
		empty, err = json.Marshal(findings[i])
		if err != nil {
			t.Fatal(err)
		}
		findings[i].Detail = strings.Repeat("x", 2047-len(empty))
	}
	findings[15].Detail = findings[15].Detail[1:]
	raw, err = NormalizeCodeCrossCheckFindings("completed", "", findings)
	if err != nil || len(raw) != 32768 {
		t.Fatalf("exact 32 KiB findings: bytes=%d err=%v", len(raw), err)
	}
	findings[15].Detail += "x"
	if _, err = NormalizeCodeCrossCheckFindings("completed", "", findings); err == nil {
		t.Fatal("32 KiB plus one accepted")
	}
	f = CodeCrossCheckFinding{ID: strings.Repeat("a", 64), Severity: "minor"}
	if _, err = NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err != nil {
		t.Fatalf("64 byte ASCII id: %v", err)
	}
	f.ID += "a"
	if _, err = NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err == nil {
		t.Fatal("65 byte id accepted")
	}
}
