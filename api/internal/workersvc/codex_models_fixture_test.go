package workersvc

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

// codexModels must equal the "curated" set of fixtures/codex-models/models.json (PRD #3245).
// The fixture is outside the module, so run with -count=1 after editing it.
func TestCodexModelsMatchFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/codex-models/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Curated []string `json:"curated"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	sort.Strings(fx.Curated)
	got := make([]string, 0, len(codexModels))
	for m := range codexModels {
		got = append(got, m)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, fx.Curated) {
		t.Errorf("codexModels = %v, want fixtures/codex-models/models.json curated %v", got, fx.Curated)
	}
}
