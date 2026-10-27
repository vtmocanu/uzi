package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCodeCrossCheckCoverageServiceLiveDB(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, []byte("{"), coverageJSON(t, coverageFixture())} {
		name := "legacy"
		if len(raw) > 1 {
			name = "valid"
		} else if len(raw) == 1 {
			name = "malformed"
		}
		t.Run(name, func(t *testing.T) {
			f := codeFixture(t)
			w := wkrRow(t, f.env, f.workerID)
			cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err != nil {
				t.Fatal(err)
			}
			child := uuid.UUID(cc.CheckerRunID.Bytes)
			f.runID = child
			claim := laneClaim(t, f, "cross_check", nil)
			if claim == nil || claim.CrossCheck == nil || !claim.CrossCheck.CoverageSupported {
				t.Fatal("claim lacks coverage capability")
			}
			findings := []CodeCrossCheckFinding{{ID: "coverage-finding", Severity: "major", Title: "guard"}}
			decided, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, child, claim.ClaimGeneration, "completed", "", findings, raw)
			if err != nil || !bytes.Contains(decided.Findings, []byte("coverage-finding")) {
				t.Fatalf("coverage lost findings: %v", err)
			}
			if len(raw) == 0 {
				if len(decided.Coverage) != 0 {
					t.Fatal("legacy gained coverage")
				}
			} else {
				got := decodeCoverage(t, decided.Coverage)
				if len(raw) == 1 && (!got.InventoryIncomplete || len(got.Entries) != 0) {
					t.Fatal("malformed account trusted")
				}
				if len(raw) > 1 && got.InventoryIncomplete {
					t.Fatal("valid account degraded")
				}
			}
			if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, child, claim.ClaimGeneration, "completed", "", nil, raw); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("verdict overwritten: %v", err)
			}
			f.env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", f.lead)
			status, err := f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 2)
			if err != nil || !bytes.Equal(status.Coverage, decided.Coverage) {
				t.Fatalf("custody lost coverage: %v", err)
			}
		})
	}
}

func TestCodeCrossCheckCoverageFailedVerdictLiveDB(t *testing.T) {
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	f.runID = child
	claim := laneClaim(t, f, "cross_check", nil)
	if claim == nil {
		t.Fatal("checker not claimed")
	}
	decided, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, child, claim.ClaimGeneration, "failed", "model_error", nil, coverageJSON(t, coverageFixture()))
	if err != nil || len(decided.Coverage) != 0 || string(decided.Findings) != "[]" {
		t.Fatalf("failed verdict: %+v %v", decided, err)
	}
}
