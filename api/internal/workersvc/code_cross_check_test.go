package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodeCrossCheckFindingContract(t *testing.T) {
	valid := CodeCrossCheckFinding{ID: "exact_ID-1", Severity: "major", Path: "file.go", Line: 1, Title: "test", Detail: "detail"}
	raw, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{valid})
	var got []CodeCrossCheckFinding
	if err != nil || json.Unmarshal(raw, &got) != nil || len(got) != 1 || got[0].ID != valid.ID {
		t.Fatal("valid finding identity lost")
	}
	for _, id := range []string{"", "with space", "x\u202e", "a\nb", "a\x1b[31mb", "a\x00b", "a\tb", strings.Repeat("a", 65), "a/b"} {
		f := valid
		f.ID = id
		if _, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err == nil {
			t.Errorf("accepted invalid id %q", id)
		}
	}
	if _, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{valid, valid}); err == nil {
		t.Error("accepted duplicate id")
	}
	for _, outcome := range []string{"approve", "revise", "block", "pending"} {
		if _, err := NormalizeCodeCrossCheckFindings(outcome, "", nil); err == nil {
			t.Errorf("accepted outcome %q", outcome)
		}
	}
	if _, err := NormalizeCodeCrossCheckFindings("failed", "model_error", []CodeCrossCheckFinding{valid}); err == nil {
		t.Error("failed attempt carries findings")
	}
	if _, err := NormalizeCodeCrossCheckFindings("failed", "timed_out", nil); err == nil {
		t.Error("worker supplied server reason")
	}
	for _, severity := range []string{"warning", "approve"} {
		f := valid
		f.Severity = severity
		if _, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err == nil {
			t.Error("invalid severity accepted")
		}
	}
	f := valid
	f.Detail = strings.Repeat("x", 2048)
	if _, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err == nil {
		t.Error("oversize finding accepted")
	}
	f = valid
	f.Detail = string([]byte{0xff})
	if _, err := NormalizeCodeCrossCheckFindings("completed", "", []CodeCrossCheckFinding{f}); err == nil {
		t.Error("invalid UTF8 accepted")
	}
	many := make([]CodeCrossCheckFinding, 21)
	if _, err := NormalizeCodeCrossCheckFindings("completed", "", many); err == nil {
		t.Error("21 findings accepted")
	}
	many = make([]CodeCrossCheckFinding, 20)
	for i := range many {
		many[i] = valid
		many[i].ID = string(rune('a' + i))
		many[i].Detail = strings.Repeat("x", 1800)
	}
	if _, err := NormalizeCodeCrossCheckFindings("completed", "", many); err == nil {
		t.Error("oversize total accepted")
	}
	if raw, err := NormalizeCodeCrossCheckFindings("completed", "", nil); err != nil || string(raw) != "[]" {
		t.Fatal("no findings must remain empty array")
	}
}
