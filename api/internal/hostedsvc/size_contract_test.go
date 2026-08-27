package hostedsvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersize"
)

// The cross-module size golden (PRD #58 Decision 7).
//
// DesiredWorker.Size carries a preset NAME, which the controller resolves against
// its own copy of the preset table — the api may not know cpu/memory, because it may
// not be the authority on a pod spec (Decision 1). That leaves the LEGAL VALUES of
// that field as a contract spanning two separately-built Go modules with nothing
// but this file joining them. It sits beside controller_poll_wire.json rather than
// in workersize/testdata precisely because it is part of the SAME contract: one
// directory, one place to look.
//
// This is the producer half: workersize.Names IS the golden. The consumer half is
// M3's, in the controller module, which reads this same file the way
// protocol_contract_test.go already reaches the wire golden across the module
// boundary.
//
// TWO HONEST LIMITS, recorded here rather than discovered later:
//
//  1. Until M3's controller test parses this file, the golden is a PROMISE, not a
//     gate. M2's half alone catches nothing — it only makes the api's list explicit
//     and stable enough to be worth checking against.
//  2. Even complete, it catches DEV-TIME drift, never DEPLOYMENT SKEW. api and
//     controller are separate images; even under Model B's version pinning a rollout
//     has a window where an old controller polls a new api. So M3 must tolerate an
//     unknown size at runtime regardless (log and skip that worker; never crash the
//     reconcile, never render a guessed pod spec). A build-time gate cannot make a
//     runtime mismatch impossible — only loud when it is our own mistake.
//
// Regenerate with `UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...`.
const sizeContractFixture = "testdata/hosted_sizes.json"

// sizeGolden is the fixture's shape. An object rather than a bare array so the file
// can gain a field (a description map, a deprecation marker) without breaking every
// parser that already reads it.
type sizeGolden struct {
	Sizes []string `json:"sizes"`
}

func TestHostedSizeNamesContract(t *testing.T) {
	got, err := json.MarshalIndent(sizeGolden{Sizes: workersize.Names}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(sizeContractFixture), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(sizeContractFixture, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}

	want, err := os.ReadFile(sizeContractFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the hosted size registry drifted from the cross-module golden.\n"+
			"If this is a deliberate preset change, the CONTROLLER's preset table must change with it "+
			"(a size the api accepts but the controller cannot resolve provisions a worker whose pod is "+
			"never rendered).\n got:\n%s\nwant:\n%s", got, want)
	}
}

// The wire golden and the size golden must agree with each other: every size the
// sample poll response puts on the wire has to be a size the registry admits.
// Without this they are two files that drift independently, which is the failure the
// pair exists to prevent.
func TestWireGoldenSizesAreValidRegistryNames(t *testing.T) {
	for _, w := range samplePollResponse().Workers {
		if !workersize.Valid(w.Size) {
			t.Errorf("the wire golden carries size %q, which workersize.Valid rejects — "+
				"the two goldens in this directory have drifted apart", w.Size)
		}
	}
}
