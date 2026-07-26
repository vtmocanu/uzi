package hostedsvc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The CONSUMER half of the /status wire contract (PRD #113 M4).
//
// The golden lives under controller/, not here, and the direction is the reason: the
// poll flows api→controller so the api marshals that golden and the controller parses
// it; /status flows controller→api, so the controller marshals this one and the api
// parses it. The producer owns the golden in both cases. A test that built its own
// fixture here would pin this side against itself and catch no drift at all.
//
// ⚠ CACHE HAZARD, and it is why -count=1 is mandatory on the api gate rather than
// merely advisable. This file reads an input OUTSIDE the api module. Go's test cache
// hashes only files inside the module root, so editing the golden changes nothing in
// this module's cache key: `go test ./internal/hostedsvc/` can print `ok (cached)`
// against a contract that has already drifted. This is now the THIRD cross-module test
// input in the repo and the first in this direction (api reading controller/).
const statusGolden = "../../../controller/internal/protocol/testdata/controller_status_wire.json"

func TestStatusReportConsumerContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(statusGolden))
	if err != nil {
		t.Fatalf("read the controller's golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/protocol/... in controller/): %v", err)
	}

	// DisallowUnknownFields HERE and deliberately NOT in the production decoder. The
	// asymmetry is the design: this test wants drift to fail at the gate, while the
	// handler must tolerate a newer controller sending an extra field, because a strict
	// production decoder turns a version skew into a 400 for the whole fleet's report.
	// Two decoders, opposite policies, one wire — do not "unify" them.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var got StatusReport
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the api cannot decode the controller's own golden: %v", err)
	}

	if got.WorkerImageTag != "0.11.7" {
		t.Errorf("WorkerImageTag = %q, want 0.11.7 (Decision 9's hosted target)", got.WorkerImageTag)
	}
	if got.PollIntervalSeconds != 10 {
		t.Errorf("PollIntervalSeconds = %d, want 10", got.PollIntervalSeconds)
	}
	if got.ReportedAt.IsZero() {
		t.Error("ReportedAt did not decode; it is display-only but it must still arrive")
	}
	if len(got.Workers) != 2 {
		t.Fatalf("decoded %d workers, want 2 — the golden pairs a stuck worker with a settled one on purpose", len(got.Workers))
	}

	// The stuck worker: every optional field populated.
	stuck := got.Workers[0]
	if stuck.Phase != "stuck" {
		t.Errorf("workers[0].Phase = %q, want stuck", stuck.Phase)
	}
	if stuck.BlockingContainer == nil || *stuck.BlockingContainer != "seed-nix" {
		t.Errorf("workers[0].BlockingContainer = %v, want seed-nix", stuck.BlockingContainer)
	}
	if stuck.BlockingReason == nil || *stuck.BlockingReason != "CrashLoopBackOff" {
		t.Errorf("workers[0].BlockingReason = %v, want CrashLoopBackOff", stuck.BlockingReason)
	}
	if stuck.RestartCount != 6 {
		t.Errorf("workers[0].RestartCount = %d, want 6", stuck.RestartCount)
	}
	if stuck.LastExitCode == nil || *stuck.LastExitCode != 2 {
		t.Errorf("workers[0].LastExitCode = %v, want 2", stuck.LastExitCode)
	}
	if stuck.PhaseSince == nil {
		t.Error("workers[0].PhaseSince is nil; the golden carries a timestamp")
	}

	// The settled worker: the optional fields are null, and this is the row that
	// catches a pointer/value collapse in either direction. A *int32 quietly becoming
	// an int32 would turn this null into 0 — indistinguishable from "exited cleanly" —
	// while RestartCount must stay a VALUE of 0, since 0 restarts is a real
	// observation rather than an absence.
	settled := got.Workers[1]
	if settled.Phase != "settled" {
		t.Errorf("workers[1].Phase = %q, want settled", settled.Phase)
	}
	if settled.BlockingContainer != nil {
		t.Errorf("workers[1].BlockingContainer = %q, want nil", *settled.BlockingContainer)
	}
	if settled.BlockingReason != nil {
		t.Errorf("workers[1].BlockingReason = %q, want nil", *settled.BlockingReason)
	}
	if settled.LastExitCode != nil {
		t.Errorf("workers[1].LastExitCode = %d, want nil — null and 0 are different facts here", *settled.LastExitCode)
	}
	if settled.RestartCount != 0 {
		t.Errorf("workers[1].RestartCount = %d, want 0 as a VALUE", settled.RestartCount)
	}
}
