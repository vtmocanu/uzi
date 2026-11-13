package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestPrerequisiteManifestEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		size          *int64
		checksum      string
		selfContained bool
	}{
		{name: "unbound"},
		{name: "checksum only", checksum: strings.Repeat("a", 64)},
		{name: "size only", size: i64(1)},
		{name: "negative size", size: i64(-1), checksum: strings.Repeat("a", 64)},
		{name: "short checksum", size: i64(1), checksum: strings.Repeat("a", 40)},
		{name: "invalid checksum", size: i64(1), checksum: strings.Repeat("z", 64)},
		{name: "zero bytes bound", size: i64(0), checksum: strings.Repeat("a", 64), selfContained: true},
		{name: "bound", size: i64(12), checksum: strings.Repeat("a", 64), selfContained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []string{"preparing", "uploading", "needs_action", "available", "expired", "discarded"} {
				a := apitypes.RecoveryArchiveDTO{ID: closureCaptureID, State: state, ByteSize: tc.size, Checksum: tc.checksum}
				text := strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n")
				if strings.Contains(text, "prerequisites: self_contained") != tc.selfContained {
					t.Errorf("%s: self-contained claim does not match manifest evidence: %s", state, text)
				}
				if !tc.selfContained && !strings.Contains(text, "prerequisites: unknown") {
					t.Errorf("%s: missing unknown prerequisite diagnostic: %s", state, text)
				}
			}
		})
	}
}

func TestPrerequisiteCleanupRequiresPinned(t *testing.T) {
	for _, archiveState := range []string{"available", "expired", "discarded"} {
		for _, pinState := range []string{"pending", "invoked", "create_unknown", "pinned", "refused", "foreign", "lost", "deleted"} {
			t.Run(archiveState+"/"+pinState, func(t *testing.T) {
				a := availableCapture(closureCaptureID, nil)
				a.State = archiveState
				a.PrerequisiteShas = []string{strings.Repeat("a", 40)}
				a.PrerequisiteClosure = &apitypes.RecoveryPrerequisiteClosureDTO{State: pinState}
				text := strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n")
				wantPending := pinState == "pinned" && archiveState != "available"
				if strings.Contains(text, "pin cleanup: pending") != wantPending {
					t.Fatalf("incorrect cleanup eligibility: %s", text)
				}
				if pinState == "deleted" && !strings.Contains(text, "pin cleanup: complete") {
					t.Fatalf("recorded deletion lost: %s", text)
				}
				a.PrerequisiteClosure.CleanupExhausted = true
				text = strings.Join(recoverySummaryLines(apitypes.RecoveryArchiveSummaryDTO{Archives: []apitypes.RecoveryArchiveDTO{a}}), "\n")
				if !strings.Contains(text, "pin cleanup: exhausted") || strings.Contains(text, "pin cleanup: pending") {
					t.Fatalf("independent exhaustion flag lost: %s", text)
				}
			})
		}
	}
}

func TestRunRecoveryCaptureDetailsAttribution(t *testing.T) {
	holds := []apitypes.RecoveryCustodyHoldDTO{{ID: "hold-one"}, {ID: "hold-two"}}
	a := availableCapture("capture-one", nil)
	a.HoldID = holds[0].ID
	b := availableCapture("capture-two", nil)
	b.HoldID = holds[1].ID
	legacy := availableCapture("legacy-capture", nil)
	for _, quiet := range []bool{false, true} {
		var out, stderr bytes.Buffer
		err := renderRunRecovery(Env{Stdout: &out, Stderr: &stderr}, &globalFlags{quiet: quiet},
			"run\nwith\ttabs", holds, []apitypes.RecoveryArchiveDTO{b, legacy, a}, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if !strings.HasPrefix(text, "HOLD ID") {
			t.Fatalf("table must precede details: %s", text)
		}
		if quiet {
			if strings.Contains(text, "capture-one") || strings.Contains(text, "capture-two") || strings.Contains(text, "restore guide:") {
				t.Fatalf("quiet output contains capture details: %s", text)
			}
		} else {
			first := strings.Index(text, "hold hold-one captures:\n")
			second := strings.Index(text, "hold hold-two captures:\n")
			capOne := strings.Index(text, "capture-one")
			capTwo := strings.Index(text, "capture-two")
			if first < 0 || capOne <= first || second <= capOne || capTwo <= second {
				t.Fatalf("capture attribution/order lost: %s", text)
			}
		}
		if strings.Contains(text, "legacy-capture") {
			t.Fatalf("unattributed capture attached to a hold: %s", text)
		}
		want := "uzi: 1 capture(s) carry no hold id (server predates it) and are not listed; 'uzi run get run with tabs' lists the available ones\n"
		if stderr.String() != want {
			t.Fatalf("unattributed warning = %q, want %q", stderr.String(), want)
		}
	}
}
