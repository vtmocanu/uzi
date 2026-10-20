package ciartifacts

import (
	"fmt"
	"strings"
	"testing"
)

func TestCIArtifactBoundaryControlsRejected(t *testing.T) {
	for _, raw := range []string{"\nlogs-*", "logs-*\t", "\u0085logs-*"} {
		if normalized, err := NormalizeSelectors([]string{raw}); err == nil {
			t.Errorf("accepted raw control in selector %q as %q", raw, normalized)
		}
	}
	if normalized, err := NormalizeSelectors([]string{" logs-* "}); err != nil || len(normalized) != 1 || normalized[0] != "logs-*" {
		t.Fatalf("ordinary space normalization: %v %v", normalized, err)
	}
}

func TestCIArtifactSmallRenderKeepsBudgetReason(t *testing.T) {
	m := Manifest{Entries: []Entry{{ArtifactID: "budget", Label: strings.Repeat("x", 200), Status: "skipped", Reason: StorageBudget}}}
	got, err := m.RenderWithin(110)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 110 || !strings.Contains(got, "skipped: storage budget") || !strings.Contains(got, "1 more skipped") {
		t.Fatalf("essential refusal reason missing in small render: %q", got)
	}
}

func TestCIArtifactQuotedMetadataPreservesAllFiles(t *testing.T) {
	m := Manifest{}
	for i := 0; i < MaxFiles; i++ {
		data := []byte(fmt.Sprint(i))
		digest, name := Identity(data)
		m.Entries = append(m.Entries, Entry{ArtifactID: strings.Repeat("`", 200), EntryOrdinal: i, Label: strings.Repeat("`", 200), Status: "kept", Name: name, SHA256: digest, Size: int64(len(data))})
	}
	got, err := m.Render()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > RenderMaxBytes || strings.Count(got, " kept: file=") != MaxFiles || strings.Contains(got, "`") || !strings.Contains(got, "[shortened]") {
		t.Fatalf("lost kept evidence under escaped metadata: %d", len(got))
	}
	for _, e := range m.Entries {
		if !strings.Contains(got, e.Name) {
			t.Fatalf("missing file %s", e.Name)
		}
	}
}

func TestCIArtifactDuplicateSourcesPreserveFile(t *testing.T) {
	digest, name := Identity([]byte("same"))
	m := Manifest{}
	for i := 0; i < MaxFiles+MaxSkipped; i++ {
		m.Entries = append(m.Entries, Entry{ArtifactID: fmt.Sprint(i), Label: strings.Repeat("x", 100), Status: "kept", Name: name, SHA256: digest, Size: 4})
	}
	got, err := m.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, " kept: file=") != 1 || !strings.Contains(got, "249 additional kept entries share these files") || strings.Contains(got, "more skipped") {
		t.Fatalf("duplicate sources discarded evidence or falsified skips: %s", got)
	}
}

func TestCIArtifactCountsMustFitKnownEntries(t *testing.T) {
	max := int64(1<<63 - 1)
	m := Manifest{Omitted: &max, Entries: []Entry{{ArtifactID: "one", Status: "skipped", Reason: "binary"}}}
	if _, err := m.Normalize(); err == nil {
		t.Fatal("accepted an unrepresentable total of omitted and known entries")
	}
}
