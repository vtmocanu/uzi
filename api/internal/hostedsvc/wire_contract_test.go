package hostedsvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// wireContractFixture is the single golden file both sides of the controller
// protocol validate against, so the api producer and the controller consumer can
// never drift into two lenient fakes. This test pins that the api MARSHALS exactly
// this shape; the controller side
// (controller/internal/protocol/protocol_contract_test.go) pins that it PARSES the
// same file. Same mechanism as the worker protocol's claim contract
// (claim_wire_contract_test.go vs. agent/test/claim-skills-contract.test.ts), which
// is what makes re-declaring the types across the module boundary safe rather than
// hopeful. Regenerate with `UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...`.
const wireContractFixture = "testdata/controller_poll_wire.json"

// samplePollResponse is a representative desired-state response covering both
// token states the wire can carry: one still awaiting delivery, one where no token
// is to be written (already delivered, or expired unread — indistinguishable to the
// controller, and deliberately so: both mean "reconcile this worker, touch no
// Secret"). Values are fixed (no random uuids) so the golden file is stable, and the
// token is an obvious non-credential so secret scanners never flag the fixture.
func samplePollResponse() PollResponse {
	token := "uzw_EXAMPLE-NOT-A-REAL-TOKEN"
	return PollResponse{Workers: []DesiredWorker{
		{
			ID:         "11111111-1111-1111-1111-111111111111",
			Template:   "base",
			Size:       "s",
			Generation: 1,
			// A docker-capable worker: the controller renders it with the privileged
			// DinD native sidecar in the docker namespace (PRD #83 M3).
			Docker: true,
			// Busy true, DrainingSince nil on this worker; the second is Busy false,
			// DrainingSince set — so BOTH states of BOTH fields appear on one wire and a drop
			// of either field on either side reddens (PRD #422 M3/M5), the same both-states
			// rationale as Docker above.
			Busy:          true,
			DrainingSince: nil,
			JoinToken:     &token,
		},
		{
			ID:         "22222222-2222-2222-2222-222222222222",
			Template:   "jvm",
			Size:       "l",
			Generation: 4,
			// docker false: #58's plain single-container worker in the restricted
			// namespace — both docker states on one wire so a drop of the field is a
			// red build on both sides.
			Docker: false,
			// Busy false, DrainingSince set: this worker is cordoned but idle — the mirror of
			// the first worker, so both states of both fields ride one wire (PRD #422 M3/M5).
			// A fixed time so the golden is stable.
			Busy:          false,
			DrainingSince: func() *time.Time { t := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); return &t }(),
			// No token to write: a pod already proved it holds one (its plaintext
			// lives only in the cluster Secret now), or the buffer expired unread.
			JoinToken: nil,
		},
	}}
}

func TestPollResponseWireContract(t *testing.T) {
	got, err := json.MarshalIndent(samplePollResponse(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(wireContractFixture), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(wireContractFixture, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}

	want, err := os.ReadFile(wireContractFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("poll response wire shape drifted from the golden file.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// There is deliberately no request-side contract to pin: the poll is a GET with no
// body. The ack that used to travel in one is gone — delivery is proved by the
// worker's own registration, not asserted by the controller.
