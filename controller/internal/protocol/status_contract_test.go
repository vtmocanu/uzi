package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// statusContractFixture is the golden for the /status wire (PRD #113 M3).
//
// NOTE THE DIRECTION — it is REVERSED from controller_poll_wire.json and that is
// why this golden lives here rather than under api/. The poll flows api→controller,
// so the api marshals the golden and this package parses it. /status flows
// controller→api, so the PRODUCER is this side: this test marshals, and the api's
// consumer test (M4) reads this same file across the module boundary and decodes it.
// Golden-on-the-producer keeps the rule "the side that writes the bytes owns the
// golden" intact in both directions.
//
// Regenerate with `UPDATE_GOLDEN=1 go test ./internal/protocol/...`.
const statusContractFixture = "testdata/controller_status_wire.json"

// sampleStatusReport covers both halves of the optional-field surface in one
// payload: a stuck worker with EVERY optional field populated, and a settled worker
// with all of them null. That pairing is what makes a dropped field or a
// pointer/value collapse redden — a fixture of two similar workers would let a
// `*int32` quietly become an `int32` (null → 0) with the golden still matching.
//
// Fixed values, no clock reads and no random uuids, so the golden is stable.
func sampleStatusReport() StatusReport {
	phaseSince := time.Date(2026, 7, 26, 9, 46, 0, 0, time.UTC)
	settledSince := time.Date(2026, 7, 26, 9, 58, 30, 0, time.UTC)
	container := "seed-nix"
	reason := "CrashLoopBackOff"
	var exit int32 = 2

	return StatusReport{
		ReportedAt:          time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		PollIntervalSeconds: 10,
		WorkerImageTag:      "0.11.7",
		Workers: []WorkerStatus{
			{
				// The motivating incident, in fixture form: an init container wedged in
				// CrashLoopBackOff reseeding the browser's nix closure.
				ID:                "11111111-1111-1111-1111-111111111111",
				Phase:             PhaseStuck,
				PhaseSince:        &phaseSince,
				TargetImage:       "harbor.example.com/uzi/agent-base:0.11.7",
				PodPhase:          "Pending",
				BlockingContainer: &container,
				BlockingReason:    &reason,
				RestartCount:      6,
				LastExitCode:      &exit,
			},
			{
				// Healthy: nothing blocking, nothing terminated. RestartCount stays a
				// VALUE of 0 while LastExitCode is null — the pair that catches a
				// pointer/value collapse in either direction.
				ID:                "22222222-2222-2222-2222-222222222222",
				Phase:             PhaseSettled,
				PhaseSince:        &settledSince,
				TargetImage:       "harbor.example.com/uzi/agent-jvm:0.11.7",
				PodPhase:          "Running",
				BlockingContainer: nil,
				BlockingReason:    nil,
				RestartCount:      0,
				LastExitCode:      nil,
			},
		},
	}
}

func TestStatusReportWireContract(t *testing.T) {
	got, err := json.MarshalIndent(sampleStatusReport(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(statusContractFixture), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(statusContractFixture, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}

	want, err := os.ReadFile(statusContractFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/protocol/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("status report wire shape drifted from the golden file.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// The phase enum is part of the wire, so pin the literals. The api validates
// against these exact strings and drops an entry carrying anything else; renaming
// one here without renaming it there would silently empty the fleet's roll health
// rather than fail a build.
func TestPhaseEnumLiterals(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{PhaseRolling, "rolling"},
		{PhaseStuck, "stuck"},
		{PhaseSettled, "settled"},
	} {
		if tc.got != tc.want {
			t.Errorf("phase literal = %q, want %q — the api matches on the string, not the constant", tc.got, tc.want)
		}
	}
}
