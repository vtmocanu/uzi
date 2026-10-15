package ciartifacts

import (
	"context"
	"testing"
)

func TestCollectorInsecurePathModeKeepsSafeEntries(t *testing.T) {
	t.Setenv("GODEBUG", "zipinsecurepath=0")
	c := NewCollector(Limits{}, nil)
	c.Add(context.Background(), "42", "logs", zipFixture(t,
		zipItem{name: "../escape", text: "rejected"},
		zipItem{name: "safe.txt", text: "kept evidence"},
	))
	m, inputs := c.Result()
	if len(inputs) != 1 || string(inputs[0].Content) != "kept evidence" || len(m.Entries) != 2 || m.Entries[0].Reason != "unsafe_path" {
		t.Fatalf("ordinary unsafe path refused unrelated evidence: files=%d manifest=%+v", len(inputs), m)
	}
}
