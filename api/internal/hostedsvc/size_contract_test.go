package hostedsvc

import (
	"encoding/json"
	"os"
	"slices"
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
// PRD #4213 CHANGE: the file is no longer generated from workersize.Names. It is the
// BUILTIN SEED CONTRACT, hand-maintained: each shipped size's name plus the integers the
// api seeds and sends on DesiredWorker.Resources. This test pins only the NAMES (and
// their order) against workersize.Names; the integers are pinned on the controller side
// (controller/internal/preset, TestPresetQuantitiesEqualTheSeedContract), where the
// compiled-in preset table lives.
const sizeContractFixture = "testdata/hosted_sizes.json"

// sizeGolden is the fixture's shape. An object rather than a bare array so the file
// can gain a field without breaking every parser that already reads it.
type sizeGolden struct {
	Sizes []sizeGoldenEntry `json:"sizes"`
}

type sizeGoldenEntry struct {
	Name string `json:"name"`
	SizeResources
}

func TestHostedSizeNamesContract(t *testing.T) {
	want, err := os.ReadFile(sizeContractFixture)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var doc sizeGolden
	if err := json.Unmarshal(want, &doc); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	names := make([]string, 0, len(doc.Sizes))
	for _, e := range doc.Sizes {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, workersize.Names) {
		t.Fatalf("the hosted size registry drifted from the cross-module golden.\n"+
			"If this is a deliberate change, the CONTROLLER's preset table must change with it "+
			"(a size the api accepts but the controller cannot resolve provisions a worker whose pod is "+
			"never rendered).\n registry: %v\n golden:   %v", workersize.Names, names)
	}
	for _, e := range doc.Sizes {
		r := e.SizeResources
		for field, v := range map[string]int64{
			"cpu_request_millicores": r.CPURequestMillicores, "cpu_limit_millicores": r.CPULimitMillicores,
			"memory_request_mib": r.MemoryRequestMiB, "memory_limit_mib": r.MemoryLimitMiB,
			"data_gib": r.DataGiB, "dind_data_gib": r.DinDDataGiB,
		} {
			if v <= 0 {
				t.Errorf("golden size %q has non-positive %s = %d", e.Name, field, v)
			}
		}
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
