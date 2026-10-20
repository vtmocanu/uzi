package ciartifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCIArtifactSelectors(t *testing.T) {
	for _, in := range [][]string{{"a", " a "}, {"["}, {""}, {"x\nq"}, {"x\u200b"}, {"\xff"}, make([]string, 21)} {
		if _, err := NormalizeSelectors(in); err == nil {
			t.Fatalf("accepted %q", in)
		}
	}
	got, err := NormalizeSelectors([]string{" build-* ", "reports/*.txt"})
	if err != nil || got[0] != "build-*" || !Matches(got, "build-linux") || Matches(nil, "build-linux") || Matches(got, "reports/deep/a.txt") {
		t.Fatalf("selector contract: %v %v", got, err)
	}
	if _, err := NormalizeSelectors([]string{strings.Repeat("é", 100)}); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeSelectors([]string{strings.Repeat("é", 101)}); err == nil {
		t.Fatal("byte limit")
	}
}

func TestCIArtifactManifestRendering(t *testing.T) {
	digest, name := Identity([]byte("safe text"))
	zero := int64(0)
	m := Manifest{Entries: []Entry{{ArtifactID: "123", EntryOrdinal: 7, Label: "evil\n" + strings.Repeat("😀", 100), Status: "kept", Name: name, SHA256: digest, Size: 9}}, Omitted: &zero, Uninspected: true}
	normalized, err := m.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	e := normalized.Entries[0]
	if !e.Shortened || !utf8.ValidString(e.Label) || len(e.Label) > 200 || strings.Contains(e.Label, "\n") {
		t.Fatalf("unsafe label %+v", e)
	}
	rendered, err := normalized.Render()
	if err != nil || !strings.Contains(rendered, "artifact=\"123\" entry=7") || !strings.Contains(rendered, "[shortened]") || !strings.Contains(rendered, "0 more skipped") || !strings.Contains(rendered, "count is unknown") || len(rendered) > RenderMaxBytes {
		t.Fatalf("render: %s %v", rendered, err)
	}
	b, err := json.Marshal(normalized)
	if err != nil || len(b) > ManifestMaxBytes {
		t.Fatalf("marshal %v", err)
	}
	if strings.ContainsAny(name, "/\\") || name != digest+".txt" {
		t.Fatal(name)
	}
}
func TestCIArtifactManifestRejectsAmbiguity(t *testing.T) {
	for _, m := range []Manifest{
		{Entries: []Entry{{ArtifactID: "a", Status: "unknown"}}},
		{Entries: []Entry{{ArtifactID: "a", Status: "skipped"}, {ArtifactID: "a", Status: "skipped"}}},
		{Entries: make([]Entry, 251)},
	} {
		if _, err := m.Normalize(); err == nil {
			t.Fatal("accepted malformed manifest")
		}
	}
}

func TestCIArtifactRenderWithinKeepsLateReferences(t *testing.T) {
	digest, name := Identity([]byte("x"))
	omitted := int64(7)
	m := Manifest{Omitted: &omitted, Uninspected: true}
	for i := 0; i < MaxSkipped; i++ {
		m.Entries = append(m.Entries, Entry{ArtifactID: fmt.Sprint(i), Label: strings.Repeat("😀", 100) + "\n\u202e```", Status: "skipped", Reason: StorageBudget})
	}
	m.Entries = append(m.Entries, Entry{ArtifactID: "late", Label: "late\n\u202e```", Status: "kept", Name: name, SHA256: digest, Size: 1})
	for _, budget := range []int{RenderMaxBytes, 1800} {
		got, err := m.RenderWithin(budget)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > budget || !utf8.ValidString(got) {
			t.Fatalf("invalid bounded render %d/%d", len(got), budget)
		}
		if !strings.Contains(got, `artifact="late" entry=0`) || !strings.Contains(got, "kept: file="+name) {
			t.Fatal("late kept ref lost")
		}
		rows := strings.Count(got, "skipped: storage budget")
		want := fmt.Sprintf("%d more skipped\n", 7+MaxSkipped-rows)
		if !strings.Contains(got, want) || !strings.Contains(got, "count is unknown") {
			t.Fatalf("incorrect omission notice: %s", got)
		}
		if strings.ContainsAny(got, "\u202e`") || strings.Contains(got, "late\n") {
			t.Fatal("unsafe metadata escaped its line")
		}
		if budget == 1800 && (rows == 0 || rows == MaxSkipped) {
			t.Fatal("shortened skipped selection not exercised")
		}
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > ManifestMaxBytes {
		t.Fatal("manifest cap", err)
	}
	if _, err := m.RenderWithin(1); !errors.Is(err, ErrRenderTooLarge) {
		t.Fatalf("mandatory metadata: %v", err)
	}
	// Unknown uninspected remainder must never become a guessed numeric count.
	unknown := Manifest{Uninspected: true}
	got, err := unknown.Render()
	if err != nil || strings.Contains(got, "more skipped") || !strings.Contains(got, "count is unknown") {
		t.Fatalf("unknown: %s %v", got, err)
	}
}

func TestCIArtifactRenderFullSelectionAndCount(t *testing.T) {
	omitted := int64(3)
	m := Manifest{Entries: []Entry{{ArtifactID: "a", Label: "ordinary", Status: "skipped", Reason: StorageBudget}, {ArtifactID: "b", Label: "bidi\u202e\n```", Status: "skipped", Reason: "unavailable"}}, Omitted: &omitted}
	got, err := m.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "artifact=") != 2 || !strings.Contains(got, "3 more skipped\n") || !strings.Contains(got, "skipped: storage budget") || strings.ContainsAny(got, "\u202e`") {
		t.Fatalf("full render: %s", got)
	}
	// Exact addition stays representable when known rows join omitted rows.
	max := int64(1<<63 - 1 - len(m.Entries))
	m.Omitted = &max
	full, err := m.Render()
	if err != nil {
		t.Fatal(err)
	}
	reduced, err := m.RenderWithin(len(full) - 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reduced, "9223372036854775806 more skipped") {
		t.Fatal(reduced)
	}
}
