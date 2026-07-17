package hostedsvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/workertmpl"
)

// The cross-module TEMPLATE golden (PRD #58 M3), the exact analogue of
// hosted_sizes.json next to it — read size_contract_test.go first; every word of
// its reasoning applies here unchanged, and the two limits it records (a promise
// until the controller parses it; dev-time drift only, never deployment skew) are
// this file's limits too.
//
// Why a second golden rather than one: DesiredWorker carries TWO names the
// controller resolves against its own tables, and they skew identically. Size
// resolves to cpu/memory/volume quantities; template resolves to a concrete agent
// IMAGE. Add "python" to workertmpl.Names without an image entry in the
// controller's map and the api accepts a provision the controller can never render
// — the worker provisions, no pod is ever built for it, and the row sits pending
// until its token expires, visible only as a worker that never comes online.
// Identical failure, identical cause, so it gets the identical gate.
//
// It differs from the size golden in one way worth knowing: M2 shipped the size
// golden's producer half and left the consumer inert for M3. NEITHER half of this
// one existed before M3, so it arrives complete — producer here, consumer in
// controller/internal/preset/preset_contract_test.go.
//
// Regenerate with `UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...`.
const templateContractFixture = "testdata/hosted_templates.json"

// templateGolden is the fixture's shape. An object rather than a bare array for the
// same reason sizeGolden is: the file can gain a field without breaking a parser.
type templateGolden struct {
	Templates []string `json:"templates"`
}

func TestHostedTemplateNamesContract(t *testing.T) {
	got, err := json.MarshalIndent(templateGolden{Templates: workertmpl.Names}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(templateContractFixture), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(templateContractFixture, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}

	want, err := os.ReadFile(templateContractFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/hostedsvc/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the worker template registry drifted from the cross-module golden.\n"+
			"If this is a deliberate template change, the CONTROLLER's template->image map must change with it "+
			"(a template the api accepts but the controller cannot resolve to an image provisions a worker whose "+
			"pod is never rendered), and M6's CI image build list must publish it.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// The wire golden and the template golden must agree, exactly as the size pair
// does: every template the sample poll response puts on the wire has to be one the
// registry admits.
func TestWireGoldenTemplatesAreValidRegistryNames(t *testing.T) {
	for _, w := range samplePollResponse().Workers {
		if !workertmpl.Valid(w.Template) {
			t.Errorf("the wire golden carries template %q, which workertmpl.Valid rejects — "+
				"the goldens in this directory have drifted apart", w.Template)
		}
	}
}
