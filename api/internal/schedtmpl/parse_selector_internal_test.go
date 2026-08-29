package schedtmpl

import (
	"strings"
	"testing"
)

// These are white-box tests of parse's PRD #767 M4 selector handling. parse is unexported
// (the catalog is embedded, never user input) and panics at init on error, so its
// error/normalization paths for the new `selector` key can only be exercised in-package.

func mustFrontmatter(target, selector, labels, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("slug: t\n")
	b.WriteString("name: T\n")
	b.WriteString("description: A test job.\n")
	b.WriteString("target: " + target + "\n")
	b.WriteString("cron: 0 2 * * *\n")
	if selector != "" {
		b.WriteString("selector: " + selector + "\n")
	}
	if labels != "" {
		b.WriteString("labels: " + labels + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	b.WriteString("\n")
	return []byte(b.String())
}

func TestParseAssignedSweepWithLabelsRejected(t *testing.T) {
	_, err := parse(mustFrontmatter("sweep", SelectorAssigned, "bug", "guidance"))
	if err == nil {
		t.Fatal("assigned sweep carrying labels must be rejected")
	}
	if !strings.Contains(err.Error(), "must not carry labels") {
		t.Fatalf("error = %v, want a 'must not carry labels' message", err)
	}
}

func TestParseBogusSelectorRejected(t *testing.T) {
	_, err := parse(mustFrontmatter("sweep", "bogus", "bug", "guidance"))
	if err == nil {
		t.Fatal("an unknown selector kind must be rejected")
	}
	if !strings.Contains(err.Error(), "unknown selector") {
		t.Fatalf("error = %v, want an 'unknown selector' message", err)
	}
}

func TestParseNonSweepAssignedSelectorRejected(t *testing.T) {
	_, err := parse(mustFrontmatter("prompt", SelectorAssigned, "", "do the thing"))
	if err == nil {
		t.Fatal("a prompt target carrying selector: assigned must be rejected")
	}
	if !strings.Contains(err.Error(), "must not set a selector") {
		t.Fatalf("error = %v, want a 'must not set a selector' message", err)
	}
}

func TestParseSweepNoSelectorDefaultsToLabel(t *testing.T) {
	j, err := parse(mustFrontmatter("sweep", "", "bug", "guidance"))
	if err != nil {
		t.Fatalf("a label sweep with no selector must parse: %v", err)
	}
	if j.SelectorKind != SelectorLabel {
		t.Fatalf("selector kind = %q, want %q (the default)", j.SelectorKind, SelectorLabel)
	}
}

func TestParseAssignedSweepNoLabelsAccepted(t *testing.T) {
	j, err := parse(mustFrontmatter("sweep", SelectorAssigned, "", "guidance"))
	if err != nil {
		t.Fatalf("an assigned sweep with no labels must parse: %v", err)
	}
	if j.SelectorKind != SelectorAssigned {
		t.Fatalf("selector kind = %q, want %q", j.SelectorKind, SelectorAssigned)
	}
	if len(j.Labels) != 0 {
		t.Fatalf("assigned sweep labels = %v, want none", j.Labels)
	}
	if j.Guidance != "guidance" {
		t.Fatalf("guidance = %q, want %q", j.Guidance, "guidance")
	}
}
