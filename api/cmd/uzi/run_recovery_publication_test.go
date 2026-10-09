package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func publicationRecoveryFixture(t *testing.T) apitypes.RecoveryCustodyHoldDTO {
	t.Helper()
	b, err := os.ReadFile("../../../fixtures/completed-publication/state-ack.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Ack struct {
			Receipt *apitypes.CompletedPublicationReceipt `json:"completed_publication_receipt"`
		} `json:"ack"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Ack.Receipt == nil {
		t.Fatal("shared fixture has no publication receipt")
	}
	r := fixture.Ack.Receipt
	return apitypes.RecoveryCustodyHoldDTO{ID: r.HoldID, RunID: r.RunID, Generation: r.Generation,
		State: "released", Attention: "released", InventoryGuarded: true, CompletedPublicationReceipt: r}
}

func TestRecoveryCompletedPublicationJSON(t *testing.T) {
	for _, exact := range []bool{false, true} {
		h := publicationRecoveryFixture(t)
		h.CompletedPublicationReason = "head_mismatch"
		legacy := apitypes.RecoveryCustodyHoldDTO{ID: "legacy", RunID: h.RunID, State: "open"}
		fc := &uzicli.FakeClient{RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{
			Holds: []apitypes.RecoveryCustodyHoldDTO{h, legacy},
		}, RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{h.RunID: {}}}
		args := []string{"run", "recovery"}
		if exact {
			args = append(args, h.RunID)
		}
		out, stderr, code := runCLI(t, fakeEnv(fc), append(args, "--json")...)
		if code != uzicli.ExitOK {
			t.Fatalf("exit=%d stderr=%s", code, stderr)
		}
		var rows []map[string]json.RawMessage
		if exact {
			if err := json.Unmarshal([]byte(out), &rows); err != nil {
				t.Fatal(err)
			}
		} else {
			var owner struct {
				Holds []map[string]json.RawMessage `json:"holds"`
			}
			if err := json.Unmarshal([]byte(out), &owner); err != nil {
				t.Fatal(err)
			}
			rows = owner.Holds
		}
		if len(rows) != 2 {
			t.Fatalf("rows=%s", out)
		}
		var receipt apitypes.CompletedPublicationReceipt
		if err := json.Unmarshal(rows[0]["completed_publication_receipt"], &receipt); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(&receipt, h.CompletedPublicationReceipt) ||
			string(rows[0]["completed_publication_reason"]) != `"head_mismatch"` {
			t.Fatalf("publication contract lost: %s", out)
		}
		for _, key := range []string{"completed_publication_receipt", "completed_publication_reason"} {
			if _, exists := rows[1][key]; exists {
				t.Fatalf("legacy row unexpectedly has %s: %s", key, out)
			}
		}
	}
}

func TestRecoveryCompletedPublicationHuman(t *testing.T) {
	for _, state := range []string{"released", "open"} {
		for _, available := range []bool{false, true} {
			h := publicationRecoveryFixture(t)
			h.State = state
			h.HasAvailableCapture = available
			h.CaptureState = "uploading"
			fc := &uzicli.FakeClient{RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{
				Holds: []apitypes.RecoveryCustodyHoldDTO{h},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "recovery", h.RunID)
			if code != uzicli.ExitOK {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			for _, want := range []string{
				"fixed final SHA aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"observed branch head dddddddddddddddddddddddddddddddddddddddd",
				"branch agent/issue-7; MR 17", "physical retirement not confirmed",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q: %s", want, out)
				}
			}
			if strings.Contains(out, "custody released") != (state == "released") {
				t.Fatalf("unsupported release claim: %s", out)
			}
			wantArchive := "archive available: false"
			if available {
				wantArchive = "archive available: true"
			}
			if !strings.Contains(out, wantArchive) {
				t.Fatalf("capture conflated with publication: %s", out)
			}
		}
	}
}

func TestRecoveryCompletedPublicationSafeBoundaries(t *testing.T) {
	h := publicationRecoveryFixture(t)
	h.State = "open"
	h.Attention = "source_only"
	h.CompletedPublicationReason = "mismatch\nforged\r\x1b\x07\u202e" + strings.Repeat("z", 500)
	h.CompletedPublicationReceipt.FinalHead = "final\nforged\x1b\u202e"
	h.CompletedPublicationReceipt.ObservedBranchHead = "observed\tforged\x07"
	h.CompletedPublicationReceipt.Branch = "branch\nforged\x1b\u202e"
	for _, exact := range []bool{false, true} {
		args := []string{"run", "recovery"}
		if exact {
			args = append(args, h.RunID)
		}
		out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{
			RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{h}},
		}), args...)
		if code != uzicli.ExitOK {
			t.Fatalf("exit=%d stderr=%s", code, stderr)
		}
		for _, r := range out {
			if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.In(r, unicode.Cf) {
				t.Fatalf("unsafe rune %U in %q", r, out)
			}
		}
		if strings.Contains(out, "\nforged") || strings.Contains(out, strings.Repeat("z", 201)) ||
			!strings.Contains(out, "completed publication refused: mismatch forged") {
			t.Fatalf("unbounded or unsafe refusal: %q", out)
		}
		if exact && (!strings.Contains(out, "fixed final SHA final forged") ||
			!strings.Contains(out, "observed branch head observed forged") ||
			!strings.Contains(out, "branch branch forged")) {
			t.Fatalf("missing safely rendered identity: %q", out)
		}
	}
}

func TestRecoveryCompletedPublicationHelp(t *testing.T) {
	out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "recovery", "--help")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"unguarded hold", "guarded inventory requires verified final coverage",
		"COMPLETED issue/mr_rework/self_improve", "does not backfill old runs",
		"failed/cancelled/parked runs and ci_fix/prompt/task are unchanged"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}
