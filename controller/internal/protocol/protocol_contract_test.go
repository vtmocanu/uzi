package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The controller side of the PRD #58 wire contract. It parses the SAME golden file
// the api's producer test pins (api/internal/hostedsvc/wire_contract_test.go), so
// the two modules can never drift into two lenient fakes — the reason re-declaring
// these types instead of importing the api module is safe. Reaching across the tree
// mirrors how the TypeScript agent validates the worker protocol
// (agent/test/claim-skills-contract.test.ts).
func goldenPath() string {
	return filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "controller_poll_wire.json")
}

func TestControllerParsesTheAPIsPollShape(t *testing.T) {
	raw, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var resp PollResponse
	// DisallowUnknownFields is deliberate: a field the api added and this side has
	// not modelled should fail here, at the gate, rather than be silently dropped in
	// production.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	if len(resp.Workers) != 2 {
		t.Fatalf("%d workers, want 2", len(resp.Workers))
	}

	// A worker whose token is still awaiting delivery.
	pending := resp.Workers[0]
	if pending.ID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("id = %q", pending.ID)
	}
	if pending.Template != "base" || pending.Size != "s" || pending.Generation != 1 {
		t.Fatalf("desired = %+v, want the golden's first worker", pending)
	}
	// The docker opt-in must survive the round trip: this worker gets the privileged
	// DinD sidecar, and a dropped/renamed field would silently render it as a plain
	// #58 worker. DisallowUnknownFields above already fails if the api adds a field
	// this side has not modelled; this pins the value.
	if !pending.Docker {
		t.Fatal("docker must parse as true for the golden's first worker (the sidecar-enabled one)")
	}
	if pending.JoinToken == nil {
		t.Fatal("join_token must parse into a non-nil pointer when the api delivers one")
	}
	if *pending.JoinToken != "uzw_EXAMPLE-NOT-A-REAL-TOKEN" {
		t.Fatalf("join_token = %q", *pending.JoinToken)
	}
	// Busy (bool) and draining_since (nullable timestamp) are distinct fields (PRD #422
	// M3/M5) and must round-trip to their own fields: the golden's first worker is busy
	// but not draining (draining == DrainingSince != nil). Asserting both (not just
	// DrainingSince==nil) fails a swapped/dropped json tag — a Busy field tagged
	// json:"draining_since" would misparse and satisfy DisallowUnknownFields, so only
	// pinning the value catches it.
	if !pending.Busy {
		t.Fatal("busy must parse as true for the golden's first worker (the busy one)")
	}
	if pending.DrainingSince != nil {
		t.Fatal("draining_since must parse as nil for the golden's first worker (not cordoned)")
	}
	// DiskPressure and Ephemeral (PRD #837 M4) are distinct bool fields and must
	// round-trip to their own fields: the golden's first worker has disk pressure
	// but is not ephemeral. Asserting both (not just one) fails a swapped tag — a
	// DiskPressure field tagged json:"ephemeral" would misparse and still satisfy
	// DisallowUnknownFields, so only pinning the value catches it.
	if !pending.DiskPressure {
		t.Fatal("disk_pressure must parse as true for the golden's first worker (under pressure)")
	}
	if pending.Ephemeral {
		t.Fatal("ephemeral must parse as false for the golden's first worker (not run-bound)")
	}

	// A worker needing no Secret written: null token, still fully desired state. The
	// nil is load-bearing — it means "write nothing", not "this worker has no token"
	// — so the pointer type must survive the round trip rather than collapsing to "".
	noToken := resp.Workers[1]
	if noToken.JoinToken != nil {
		t.Fatal("a null join_token must parse as nil")
	}
	if noToken.Template != "jvm" || noToken.Size != "l" || noToken.Generation != 4 {
		t.Fatalf("desired = %+v, want the golden's second worker", noToken)
	}
	if noToken.Docker {
		t.Fatal("docker must parse as false for the golden's second worker (the plain one)")
	}
	// The mirror of the first worker: this one is draining but not busy, so the two
	// assertions together fail either a swapped tag or a field collapse. The timestamp
	// must round-trip to the golden's fixed value.
	if noToken.Busy {
		t.Fatal("busy must parse as false for the golden's second worker (idle)")
	}
	if noToken.DrainingSince == nil {
		t.Fatal("draining_since must parse as non-nil for the golden's second worker (cordoned)")
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !noToken.DrainingSince.Equal(want) {
		t.Fatalf("draining_since = %v, want the golden's fixed %v", noToken.DrainingSince, want)
	}
	// The mirror of the first worker's disk fields: this one is ephemeral and not
	// under disk pressure, so the two assertions together fail either a swapped tag
	// or a field collapse.
	if noToken.DiskPressure {
		t.Fatal("disk_pressure must parse as false for the golden's second worker (not under pressure)")
	}
	if !noToken.Ephemeral {
		t.Fatal("ephemeral must parse as true for the golden's second worker (run-bound)")
	}
}

// There is deliberately no request side to pin: the poll is a GET with no body.
// The ack that used to travel in one is gone — the api derives delivery from the
// worker's own registration, so this controller asserts nothing.
