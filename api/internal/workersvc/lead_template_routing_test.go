package workersvc

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// Routing cannot treat a template pin as custom when assembly would reject that
// role-model spelling and use the curated fallback instead (or vice versa).
func TestLeadTemplateRoutingRoleVocabulary(t *testing.T) {
	raw, err := os.ReadFile("../store/queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	lists := regexp.MustCompile(`CASE WHEN t\.codex_model IN \(([^)]+)\) THEN t\.codex_model END`).FindAllSubmatch(raw, -1)
	if len(lists) == 0 {
		t.Fatal("no template routing model vocabularies found")
	}
	want := make([]string, 0, len(agenttmpl.CodexRoleModels))
	for model := range agenttmpl.CodexRoleModels {
		want = append(want, model)
	}
	slices.Sort(want)
	quoted := regexp.MustCompile(`'([^']+)'`)
	for i, list := range lists {
		var got []string
		for _, match := range quoted.FindAllSubmatch(list[1], -1) {
			got = append(got, string(match[1]))
		}
		slices.Sort(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("routing vocabulary %d=%v, want CodexRoleModels %v", i, got, want)
		}
	}
}
