package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestM2RunBlockedReasonRowAndJSON(t *testing.T) {
	for _, reasons := range [][]string{nil, {}, {"author_not_eligible"}, {"permission_unknown"}, {"author_not_eligible", "permission_unknown"}} {
		r := apitypes.RunDTO{ID: "same-run", Kind: "issue", Status: "queued", AutoApproveBlockedReasons: reasons}
		var out bytes.Buffer
		p := uzicli.NewPrinter(&out, false, false, true, false)
		if err := renderRunDetail(p, r); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "AUTO_APPROVE_BLOCKED") != (len(reasons) > 0) {
			t.Fatalf("reason row=%q", out.String())
		}
		for _, reason := range reasons {
			if !strings.Contains(out.String(), reason) {
				t.Fatalf("missing reason %q", reason)
			}
		}
		var machine bytes.Buffer
		if err := renderCreatedRun(Env{Stdout: &machine, Stderr: &machine}, &globalFlags{json: true}, r); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Run json.RawMessage `json:"run"`
		}
		if err := json.Unmarshal(machine.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		raw := envelope.Run
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		expected, _ := json.Marshal(reasons)
		var compact bytes.Buffer
		if err := json.Compact(&compact, wire["auto_approve_blocked_reasons"]); err != nil {
			t.Fatal(err)
		}
		if compact.String() != string(expected) {
			t.Fatalf("codes/default changed: %s", raw)
		}
		if string(wire["auto_approve"]) != "false" {
			t.Fatalf("default auto approval=%s", raw)
		}
	}
}
