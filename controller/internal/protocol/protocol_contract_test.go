package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
	if pending.JoinToken == nil {
		t.Fatal("join_token must parse into a non-nil pointer when the api delivers one")
	}
	if *pending.JoinToken != "uzw_EXAMPLE-NOT-A-REAL-TOKEN" {
		t.Fatalf("join_token = %q", *pending.JoinToken)
	}

	// An already-acked worker: null token, still fully desired state. The nil is
	// load-bearing — it means "delivered", not "no token" — so the pointer type must
	// survive the round trip rather than collapsing to "".
	acked := resp.Workers[1]
	if acked.JoinToken != nil {
		t.Fatal("a null join_token must parse as nil")
	}
	if acked.Template != "jvm" || acked.Size != "l" || acked.Generation != 4 {
		t.Fatalf("desired = %+v, want the golden's second worker", acked)
	}
}

// The request side: what this module marshals must be what the api's decoder reads.
func TestPollRequestMarshalsTheAPIsShape(t *testing.T) {
	got, err := json.Marshal(PollRequest{Materialized: []string{"11111111-1111-1111-1111-111111111111"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"materialized":["11111111-1111-1111-1111-111111111111"]}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
