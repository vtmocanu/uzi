package ciartifacts

import (
	"context"
	"strings"
	"testing"
)

func TestCollectorWorkReasonSurvivesFullKeptAliasManifest(t *testing.T) {
	c := NewCollector(Limits{MaxInflatedBytes: 4}, nil)
	c.Add(context.Background(), "first", "logs", zipFixture(t, zipItem{name: "a", text: "same"}))
	base := c.manifest.Entries[0]
	for i := 0; i < MaxFiles+MaxSkipped-1; i++ {
		row := base
		row.ArtifactID, row.EntryOrdinal = "alias", i
		row.ArtifactName, row.Path, row.Label = "", "", ""
		row.CompressedSize, row.UncompressedSize = 0, 0
		c.manifest.Entries = append(c.manifest.Entries, row)
	}
	if _, err := c.manifest.Normalize(); err != nil {
		t.Fatal(err)
	}
	c.Add(context.Background(), "last", "logs", zipFixture(t, zipItem{name: "b", text: "more"}))
	m, inputs := c.Result()
	rendered, err := m.Render()
	if err != nil || len(inputs) != 1 || !m.Uninspected || c.inflated != 5 || !strings.Contains(rendered, "inflated_budget") {
		t.Fatalf("lost essential work refusal: reason=%v work=%d rendered=%q", err, c.inflated, rendered)
	}
}
