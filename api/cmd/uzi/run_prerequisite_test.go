package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const closureCaptureID = "00000000-0000-0000-0000-000000000001"

type closureFixture struct {
	PinRef    string    `json:"pin_ref"`
	PinTip    string    `json:"pin_tip"`
	UpdatedAt time.Time `json:"updated_at"`
	Cases     []struct {
		Name  string `json:"name"`
		Input struct {
			Thin         bool   `json:"thin"`
			CaptureState string `json:"capture_state"`
		} `json:"input"`
		Expected struct {
			Closure          *apitypes.RecoveryPrerequisiteClosureDTO `json:"closure"`
			Protection       string                                   `json:"protection"`
			Cleanup          string                                   `json:"cleanup"`
			PermittedActions []string                                 `json:"permitted_actions"`
		} `json:"expected"`
	} `json:"cases"`
	Malformed []struct {
		Name string `json:"name"`
		Wire string `json:"wire"`
	} `json:"malformed"`
}

func readClosureFixture(t *testing.T) closureFixture {
	t.Helper()
	b, err := os.ReadFile("../../../fixtures/recovery-prerequisite-closure/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var f closureFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// This regression uses existing rendering seams and M1 types so it can compile
// unchanged on M1 for the tester's missing-render baseline check.
func TestPrerequisiteSharedCases(t *testing.T) {
	f := readClosureFixture(t)
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			a := availableCapture(closureCaptureID, []byte("bundle bytes"))
			a.HoldID = "hold-1"
			a.SourceSha = f.PinTip
			a.State = tc.Input.CaptureState
			a.PrerequisiteClosure = tc.Expected.Closure
			if tc.Input.Thin {
				a.PrerequisiteShas = []string{f.PinTip}
			}
			summary := apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}
			text := strings.Join(recoverySummaryLines(summary), "\n")
			var out, stderr bytes.Buffer
			env := Env{Stdout: &out, Stderr: &stderr}
			// Closed/completed holds retain all capture metadata too.
			holds := []apitypes.RecoveryCustodyHoldDTO{{ID: "hold-1", State: "released"}}
			if err := renderRunRecovery(env, &globalFlags{}, recoveryHeldRunID, holds, summary.Archives, nil, false, false); err != nil {
				t.Fatal(err)
			}
			fc := &uzicli.FakeClient{RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: holds}, RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{recoveryHeldRunID: summary}}
			fc.RecoveryHoldsResult.Holds[0].RunID = recoveryHeldRunID
			cliOut, cliErr, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
			if code != 0 {
				t.Fatalf("human recovery failed: %s", cliErr)
			}
			for _, rendered := range []string{text, out.String(), cliOut} {
				want := "pin protection: " + tc.Expected.Protection + ";"
				if tc.Expected.Protection == "self_contained" {
					want = "prerequisites: self_contained"
				}
				if !strings.Contains(rendered, want) {
					t.Fatalf("missing semantic meaning %q in %s", want, rendered)
				}
				for _, want := range []string{closureCaptureID, "archive expiry: not recorded", "restore guide: https://github.com/vtmocanu/uzi/blob/main/docs/run-recovery.md#", "does not release custody"} {
					if !strings.Contains(rendered, want) {
						t.Errorf("missing %q", want)
					}
				}
				if c := tc.Expected.Closure; c != nil {
					for _, want := range []string{"pin state: " + c.State, "pin ref: " + f.PinRef, "pin record updated: " + f.UpdatedAt.Format(time.RFC3339)} {
						if !strings.Contains(rendered, want) {
							t.Errorf("missing %q", want)
						}
					}
					if c.PinTip != nil && !strings.Contains(rendered, "pin tip: "+*c.PinTip) {
						t.Error("exact tip lost")
					}
					if c.Reason != "" && !strings.Contains(rendered, "pin reason (recorded): "+c.Reason) {
						t.Error("recorded reason lost")
					}
					if c.AbsenceUnconfirmed && !strings.Contains(rendered, "do not mint a replacement") {
						t.Error("absence overclaimed")
					}
				}
				switch tc.Expected.Cleanup {
				case "none":
					if strings.Contains(rendered, "pin cleanup:") {
						t.Error("cleanup implied without a recorded owned pin")
					}
				case "exhausted":
					if !strings.Contains(rendered, "pin cleanup: exhausted; automatic deletes stopped; this grants no manual deletion permission") {
						t.Error("cleanup exhaustion hidden")
					}
				case "complete":
					if !strings.Contains(rendered, "pin cleanup: complete") {
						t.Error("deletion not explained")
					}
				case "pending":
					if !strings.Contains(rendered, "pin cleanup: pending") {
						t.Error("cleanup pending hidden")
					}
				}
			}
			chosen, err := selectExportCapture(recoveryHeldRunID, summary, a.ID)
			download := false
			for _, action := range tc.Expected.PermittedActions {
				if action == "download" {
					download = true
				}
			}
			if (err == nil) != download {
				t.Fatalf("pin projection changed archive selection: %v", err)
			}
			if download && chosen.ID != a.ID {
				t.Fatal("wrong archive")
			}
		})
	}
	for _, tc := range f.Malformed {
		t.Run(tc.Name, func(t *testing.T) {
			var c apitypes.RecoveryPrerequisiteClosureDTO
			if err := json.Unmarshal([]byte(tc.Wire), &c); err == nil {
				t.Fatal("malformed wire accepted")
			}
		})
	}
}

func TestPrerequisiteRecoveryJSONPreservesMetadata(t *testing.T) {
	f := readClosureFixture(t)
	a := availableCapture(closureCaptureID, []byte("bundle"))
	a.HoldID = "h"
	a.PrerequisiteClosure = f.Cases[3].Expected.Closure
	a.SourceSha = f.PinTip
	a.PrerequisiteShas = []string{strings.Repeat("2", 40), f.PinTip}
	a.AttemptedHeadSha = &f.PinTip
	a.CreatedAt = f.UpdatedAt
	a.ExpiresAt = &f.UpdatedAt
	b := a
	b.ID = "second"
	b.State = "expired"
	var out, stderr bytes.Buffer
	env := Env{Stdout: &out, Stderr: &stderr}
	holds := []apitypes.RecoveryCustodyHoldDTO{{ID: "h", State: "released"}, {ID: "empty", State: "discarded"}}
	if err := renderRunRecovery(env, &globalFlags{json: true}, recoveryHeldRunID, holds, []apitypes.RecoveryArchiveDTO{a, b, {ID: "foreign", HoldID: "other"}}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	start := strings.Index(out.String(), "\"captures\":")
	if start < 0 {
		t.Fatal("capture attachment missing")
	}
	captureJSON := out.String()[start:]
	previous := -1
	for _, key := range []string{"id", "state", "source_sha", "byte_size", "created_at", "checksum", "prerequisite_shas", "expires_at", "attempted_head_sha", "prerequisite_closure"} {
		index := strings.Index(captureJSON, "\""+key+"\":")
		if index <= previous {
			t.Fatalf("capture key order lost at %s", key)
		}
		previous = index
	}
	text := strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n")
	if !strings.Contains(text, "archive expiry: "+f.UpdatedAt.Format(time.RFC3339)) {
		t.Fatal("archive expiry lost")
	}
	var rows []struct {
		ID       string                        `json:"id"`
		Captures []apitypes.RecoveryArchiveDTO `json:"captures"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || len(rows[0].Captures) != 2 || rows[1].Captures == nil || len(rows[1].Captures) != 0 {
		t.Fatalf("hold attachment/order lost: %s", out.String())
	}
	for i, want := range []apitypes.RecoveryArchiveDTO{a, b} {
		got := rows[0].Captures[i]
		want.HoldID = "" // enclosing hold already identifies it; existing capture keys stay flat.
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("capture %d metadata lost: got %+v want %+v", i, got, want)
		}
	}
}

func TestPrerequisiteUnsafeMetadata(t *testing.T) {
	f := readClosureFixture(t)
	attacks := []string{"\x1b[31mred\n\t$(touch BAD)", "[click](https://bad.invalid) `echo BAD`", strings.Repeat("x", 10000)}
	for _, attack := range attacks {
		a := availableCapture(closureCaptureID, []byte("bundle"))
		c := *f.Cases[3].Expected.Closure
		a.PrerequisiteShas = []string{attack}
		a.PrerequisiteClosure = &c
		c.PinRef = attack
		c.PinTip = &attack
		c.Reason = attack
		a.SourceSha = attack
		text := strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n")
		if strings.Contains(text, "pin protection: protected;") || !strings.Contains(text, "pin ref: invalid metadata; no safe ref") || !strings.Contains(text, "pin tip: invalid metadata") {
			t.Fatal("unsafe pin accepted")
		}
		if strings.ContainsAny(text, "\x1b\t\r") || len(text) > 3000 {
			t.Fatal("unbounded/control-bearing output")
		}
		if strings.Contains(text, "git fetch") {
			t.Fatal("untrusted metadata made a copy command")
		}
	}
	for _, field := range []string{"capture_id", "pin_ref", "pin_tip"} {
		t.Run(field, func(t *testing.T) {
			a := availableCapture(closureCaptureID, nil)
			c := *f.Cases[3].Expected.Closure
			a.PrerequisiteClosure = &c
			attack := "$(echo BAD); " + strings.Repeat("x", 1000)
			switch field {
			case "capture_id":
				a.ID = attack
			case "pin_ref":
				c.PinRef = f.PinRef + ";echo BAD"
			case "pin_tip":
				c.PinTip = &attack
			}
			text := strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n")
			if strings.Contains(text, "pin protection: protected;") || strings.Contains(text, "git fetch") || strings.Contains(text, "git update-ref") {
				t.Fatal("unsafe metadata accepted or turned into command")
			}
		})
	}
	for _, id := range []string{"$(echo BAD)", "safe;echo BAD", strings.Repeat("x", 1000)} {
		var out bytes.Buffer
		renderLandingHint(Env{Stdout: &out}, &globalFlags{}, apitypes.RunDTO{ID: id, LandingState: "needs_landing"}, apitypes.RecoveryArchiveSummaryDTO{})
		if strings.Contains(out.String(), id) || !strings.Contains(out.String(), "invalid run id metadata") {
			t.Fatal("unsafe copyable run command")
		}
	}
	for _, tip := range []string{strings.Repeat("0", 40), strings.Repeat("f", 64), strings.Repeat("A", 40)} {
		a := availableCapture(closureCaptureID, nil)
		c := *f.Cases[3].Expected.Closure
		c.PinTip = &tip
		a.PrerequisiteClosure = &c
		a.PrerequisiteShas = []string{f.PinTip}
		if strings.Contains(strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n"), "pin protection: protected;") {
			t.Fatal("invalid persisted-tip format accepted as protection")
		}
	}
	// A typed value with missing timestamp is not positive protection.
	a := availableCapture(closureCaptureID, nil)
	c := *f.Cases[3].Expected.Closure
	c.UpdatedAt = time.Time{}
	a.PrerequisiteClosure = &c
	if strings.Contains(strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n"), "pin protection: protected;") {
		t.Fatal("zero record time accepted")
	}
}

func TestPrerequisiteExportIndependent(t *testing.T) {
	f := readClosureFixture(t)
	for _, tc := range f.Cases {
		if tc.Input.CaptureState != "available" {
			continue
		}
		t.Run(tc.Name, func(t *testing.T) {
			data := []byte("not dependency-verified")
			a := availableCapture(closureCaptureID, data)
			a.PrerequisiteClosure = tc.Expected.Closure
			if tc.Input.Thin {
				a.PrerequisiteShas = []string{f.PinTip}
			}
			fc := &uzicli.FakeClient{RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{"r1": {Archives: []apitypes.RecoveryArchiveDTO{a}}}, RecoveryBytes: map[string][]byte{a.ID: data}}
			dest := filepath.Join(t.TempDir(), "out.bundle")
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest, "--json")
			if code != 0 {
				t.Fatalf("export gated by pin: %s", stderr)
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if len(result) != 7 || result["verified"] != true || readBack(t, dest) != string(data) || !strings.Contains(stderr, "bytes only") {
				t.Fatal("byte verification contract changed")
			}
		})
	}
}
