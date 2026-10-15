package ciartifacts

import (
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
