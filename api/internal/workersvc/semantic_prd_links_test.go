package workersvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The corpus is shared with the agent: fixtures/semantic-plan/prd-links.json pins
// agent/src/prd-link.ts findSemanticPrdPaths and validatePrdPath, and this test pins the
// Go mirror (FindSemanticPrdPaths, prdpath.Validate) to the same values. The file lives
// outside the module, so go's test cache cannot see an edit to it: run with -count=1.
func TestSemanticPrdLinksSharedCorpus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "semantic-plan", "prd-links.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Find []struct {
			Name  string   `json:"name"`
			Text  string   `json:"text"`
			Paths []string `json:"paths"`
		} `json:"find"`
		Validate []struct {
			Path  string `json:"path"`
			Valid bool   `json:"valid"`
		} `json:"validate"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Find) < 20 || len(corpus.Validate) < 10 {
		t.Fatalf("corpus too small to pin anything: find=%d validate=%d", len(corpus.Find), len(corpus.Validate))
	}
	for _, c := range corpus.Find {
		t.Run("find/"+c.Name, func(t *testing.T) {
			if got := FindSemanticPrdPaths(c.Text); !reflect.DeepEqual(got, c.Paths) {
				t.Fatalf("FindSemanticPrdPaths(%q)=%q, want %q", c.Text, got, c.Paths)
			}
		})
	}
	for _, c := range corpus.Validate {
		name := c.Path
		if len(name) > 24 {
			name = name[:24]
		}
		t.Run("validate/"+name, func(t *testing.T) {
			if got := ValidSemanticPrdPath(c.Path); got != c.Valid {
				t.Fatalf("ValidSemanticPrdPath(%q)=%v, want %v", c.Path, got, c.Valid)
			}
		})
	}
}
