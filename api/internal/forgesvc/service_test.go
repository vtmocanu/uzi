package forgesvc

import "testing"

func TestHasPRDLink(t *testing.T) {
	match := []string{
		"See prds/2-forge.md for details",
		"relative link: prds/1-simple-webui-user-registration.md",
		"full blob URL https://github.com/vtmocanu/uzi/-/blob/main/prds/2-forge-integration-kanban.md",
		"with anchor prds/2-forge.md#milestones",
		"with query prds/2-forge.md?ref=main",
		"in parens (prds/2-forge.md)",
		"CAPS PRDS not matched but Prds/Case.md is", // "Prds/Case.md" matches case-insensitively
		"subdir prds/done/1-simple-webui-user-registration.md",
		"deep subdir prds/archive/2025/old.md",
		"blob URL to a subdir file with line anchor https://github.com/vtmocanu/uzi/-/blob/main/prds/done/1-simple-webui-user-registration.md#L4",
	}
	for _, m := range match {
		if !HasPRDLink(m) {
			t.Errorf("HasPRDLink(%q) = false, want true", m)
		}
	}

	noMatch := []string{
		"no link here at all",
		"mentions prds but not a file",
		"wrong ext prds/plan.txt",
		"not a prd docs/readme.md",
		"prds/.md", // no basename before .md
	}
	for _, n := range noMatch {
		if HasPRDLink(n) {
			t.Errorf("HasPRDLink(%q) = true, want false", n)
		}
	}
}

// TestHasPRDLinkLabelIndependent pins the PRD-optional intent (PRD #764 M2): PRD
// detection is a pure function of the issue DESCRIPTION and depends on no label. The
// detector takes only the description string, so a prds/*.md link is found regardless
// of what labels (uzi, bug, Planned, or none) the issue carries — the runs/board
// PRD-presence badge is therefore label-independent.
func TestHasPRDLinkLabelIndependent(t *testing.T) {
	const desc = "Implements the feature. Spec: prds/764-uzi-eligibility-label.md"
	if !HasPRDLink(desc) {
		t.Fatalf("HasPRDLink(%q) = false, want true — detection is on the description alone, no label involved", desc)
	}
}
