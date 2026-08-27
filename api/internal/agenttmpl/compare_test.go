package agenttmpl

import "testing"

// base is a definition with all four compared columns populated. Each case below
// mutates exactly one of them, so a comparison that ignores a column goes red on
// that column's case alone and names it.
func base() Definition {
	return Definition{
		Name:        "coder",
		Description: "Implements features.",
		Model:       "opus",
		Tools:       []string{"Bash", "Read", "Edit"},
		PromptBody:  "You are the coder.\n",
	}
}

func TestSameContentDetectsEachMutableColumn(t *testing.T) {
	cases := []struct {
		column string
		mutate func(*Definition)
	}{
		{"description", func(d *Definition) { d.Description = "Implements features and fixes bugs." }},
		{"model", func(d *Definition) { d.Model = "sonnet" }},
		{"tools", func(d *Definition) { d.Tools = []string{"Bash", "Read"} }},
		{"prompt_body", func(d *Definition) { d.PromptBody = "You are the coder. Be terse.\n" }},
	}
	for _, c := range cases {
		t.Run(c.column, func(t *testing.T) {
			edited := base()
			c.mutate(&edited)
			if SameContent(base(), edited) {
				t.Errorf("an edit to %s must not compare as the same content", c.column)
			}
		})
	}

	if !SameContent(base(), base()) {
		t.Error("an unedited definition must compare as the same content")
	}
}

// TestSameContentIgnoresName pins the deliberate four-column scope. Name is the
// lookup key (BuiltinByName matches on it) and is immutable after create, so
// including it would add a term that can never be false.
func TestSameContentIgnoresName(t *testing.T) {
	renamed := base()
	renamed.Name = "something-else"
	if !SameContent(base(), renamed) {
		t.Error("name must not participate in the content comparison")
	}
}

// TestSameContentToolsNilAndEmptyAgree pins the reason for slices.Equal over
// reflect.DeepEqual: a NULL tools column decodes to nil and an empty JSON array
// decodes to []string{}, and both mean inherit-all. DeepEqual would call that
// drift on a semantically identical row.
func TestSameContentToolsNilAndEmptyAgree(t *testing.T) {
	nilTools := base()
	nilTools.Tools = nil
	emptyTools := base()
	emptyTools.Tools = []string{}
	if !SameContent(nilTools, emptyTools) {
		t.Error("nil and empty tools both mean inherit-all and must compare equal")
	}
}

// TestSameContentToolsOrderMatters pins the do-not-sort rule. Render joins the
// list in order, so a reordering changes the rendered subagent file — sorting
// either side would hide a real edit, and nothing else in the suite covers it.
func TestSameContentToolsOrderMatters(t *testing.T) {
	reordered := base()
	reordered.Tools = []string{"Read", "Bash", "Edit"}
	if SameContent(base(), reordered) {
		t.Error("tools order is rendered, so a reordering is a real edit")
	}
}

// TestSameContentDoesNotTrim pins the never-trim rule for both free-text
// columns. A trailing-space edit is invisible in the UI but real in the exported
// file, and a comparison that trimmed would hide it permanently.
func TestSameContentDoesNotTrim(t *testing.T) {
	paddedDesc := base()
	paddedDesc.Description += " "
	if SameContent(base(), paddedDesc) {
		t.Error("a trailing space in the description is an edit, not noise")
	}
	paddedBody := base()
	paddedBody.PromptBody += "\n"
	if SameContent(base(), paddedBody) {
		t.Error("a trailing newline in the prompt body is an edit, not noise")
	}
}

// TestSameContentIgnoresRenderedFrontmatter records, as an executable note, why
// the comparison is over columns rather than over Render's bytes. No case
// discriminates the two implementations TODAY — this asserts the property that
// will make them diverge: Render serializes frontmatter, so any line a future
// release adds there (PRD #85 M2's `version:` stamp) lands on the shipped side
// only, since the stored row has no such column. A definition carrying an extra
// rendered line must still compare as the same CONTENT.
func TestSameContentIgnoresRenderedFrontmatter(t *testing.T) {
	shipped := base()
	stored := base()
	// Name is the stand-in for any frontmatter-only field: it renders, it is not
	// content, and the two sides can legitimately disagree about it.
	shipped.Name = "coder"
	stored.Name = "coder-as-stored"
	if string(Render(shipped)) == string(Render(stored)) {
		t.Fatal("precondition: the two sides must render to different bytes")
	}
	if !SameContent(shipped, stored) {
		t.Error("a rendered-frontmatter difference is not a content difference")
	}
}
