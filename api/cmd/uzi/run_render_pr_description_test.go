package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #1798 M7: `uzi run get`'s DELIVERED / SIZE / PR_UPDATE rows, the CLI twin of the web's
// DeliveredCard (web/src/pages/runView/DeliveredCard.tsx).

// TestPrSizeLineFixture is the Go half of the cross-language size-line golden; the web half is
// DeliveredCard.test.tsx. fixtures/ sits above the api module, so run with -count=1.
func TestPrSizeLineFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "pr-size-line", "cases.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var cases []struct {
		Name     string                     `json:"name"`
		Size     apitypes.PrDescriptionSize `json:"size"`
		Expected *string                    `json:"expected"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			size := c.Size
			body := prSizeBody(&size)
			got := ""
			if body != "" {
				got = "Size: " + body
			}
			want := ""
			if c.Expected != nil {
				want = *c.Expected
			}
			if got != want {
				t.Errorf("size line = %q, want %q", got, want)
			}
		})
	}
	if prSizeBody(nil) != "" {
		t.Error("a nil size must render no SIZE row")
	}
}

func TestDisplayPrText(t *testing.T) {
	for in, want := range map[string]string{
		`\[x\](y)`:                   "[x](y)",
		"&lt;b&gt;bold&lt;/b&gt;":    "<b>bold</b>",
		`\# heading`:                 "# heading",
		"Map&lt;K, V&gt; &amp; more": "Map<K, V> & more",
		"&amp;lt;":                   "&lt;", // one pass: never on to `<`
		`C:\path`:                    `C:\path`,
		`trailing \\`:                `trailing \`,
		// Left to right, as a CommonMark forge reads it: the lead's `a \< b` is stored as
		// `a \&lt; b`, whose escaped `&` consumes the ampersand, so it reads `a &lt; b`.
		`a \&lt; b`: "a &lt; b",
		`\&amp;`:    "&amp;",
	} {
		if got := displayPrText(in); got != want {
			t.Errorf("displayPrText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrDescriptionOutcomeNote(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, o := range []*string{nil, str(""), str("published")} {
		if got := prDescriptionOutcomeNote(o); got != "" {
			t.Errorf("outcome %v must print no note, got %q", o, got)
		}
	}
	seen := map[string]bool{}
	for _, o := range []string{"skipped_human_edit", "skipped_no_region", "skipped_malformed", "skipped_snapshot_moved", "write_failed"} {
		note := prDescriptionOutcomeNote(str(o))
		if !strings.HasPrefix(note, "Last PR update skipped: ") && !strings.HasPrefix(note, "Last PR update failed: ") {
			t.Errorf("outcome %q note %q is not a plain sentence", o, note)
		}
		if seen[note] {
			t.Errorf("outcome %q shares its note with another outcome", o)
		}
		seen[note] = true
	}
	if got := prDescriptionOutcomeNote(str("skipped_new_thing")); got != "Last PR update was not published (skipped_new_thing)." {
		t.Errorf("unknown outcome note = %q", got)
	}
}

func prDescRun(d *apitypes.RunPrDescriptionDTO, outcome *string) apitypes.RunDTO {
	return apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "completed",
		IssueTitle: "do the thing", ForgeType: "github", Health: "ok",
		PrDescription: d, PrDescriptionOutcome: outcome,
	}
}

func TestRenderRunDetailPrDescription(t *testing.T) {
	size := apitypes.PrDescriptionSize{Files: 2, Code: apitypes.PrDescriptionSizeBucket{Added: 1810, Deleted: 12}, Tests: apitypes.PrDescriptionSizeBucket{Added: 40}}
	d := &apitypes.RunPrDescriptionDTO{
		MrIid:  12,
		Source: "generated",
		Fields: apitypes.PrDescriptionFields{Summary: "Adds a token-bucket limiter \\[docs\\] for Map&lt;K, V&gt;."},
		Size:   &size,
	}
	published := "published"
	out := renderDetail(t, prDescRun(d, &published))
	for _, want := range []string{
		"DELIVERED", "Adds a token-bucket limiter [docs] for Map<K, V>.",
		"SIZE", "code +1,810 \u221212 \u00b7 tests +40 \u22120 \u00b7 2 files",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail must render %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PR_UPDATE") {
		t.Errorf("a published outcome must print no PR_UPDATE row, got:\n%s", out)
	}

	skipped := "skipped_human_edit"
	out = renderDetail(t, prDescRun(d, &skipped))
	if !strings.Contains(out, "PR_UPDATE") || !strings.Contains(out, "Last PR update skipped: a human edited the description.") {
		t.Errorf("a skipped outcome must print its note, got:\n%s", out)
	}

	// Nothing ever published (the first write failed or was skipped): the PR_UPDATE row alone.
	failed := "write_failed"
	bare := renderDetail(t, prDescRun(nil, &failed))
	if !strings.Contains(bare, "PR_UPDATE") || !strings.Contains(bare, "Last PR update failed: the forge did not accept the new description.") {
		t.Errorf("a run whose first description write failed must print the PR_UPDATE note, got:\n%s", bare)
	}
	for _, unwanted := range []string{"DELIVERED", "SIZE"} {
		if strings.Contains(bare, unwanted) {
			t.Errorf("a run with no published description must not render %q, got:\n%s", unwanted, bare)
		}
	}

	// No description and no outcome (or a published one): none of the rows.
	for _, o := range []*string{nil, &published} {
		none := renderDetail(t, prDescRun(nil, o))
		for _, unwanted := range []string{"DELIVERED", "SIZE", "PR_UPDATE"} {
			if strings.Contains(none, unwanted) {
				t.Errorf("a run with no description and no note must not render %q, got:\n%s", unwanted, none)
			}
		}
	}

	// A lead_only summary (the editor pass failed) carries the not-checked note; a generated one
	// (the case above) does not.
	if strings.Contains(out, "UNCHECKED") {
		t.Errorf("a generated summary must print no UNCHECKED row, got:\n%s", out)
	}
	leadOnly := renderDetail(t, prDescRun(&apitypes.RunPrDescriptionDTO{
		Source: "lead_only",
		Fields: apitypes.PrDescriptionFields{Summary: "Adds a limiter."},
	}, &published))
	if !strings.Contains(leadOnly, "UNCHECKED") || !strings.Contains(leadOnly, "Summary written by the agent, not checked against the diff.") {
		t.Errorf("a lead_only summary must print the not-checked note, got:\n%s", leadOnly)
	}

	// Deterministic-only: no text, the size row alone.
	unavailable := apitypes.PrDescriptionSize{Unavailable: true}
	only := renderDetail(t, prDescRun(&apitypes.RunPrDescriptionDTO{Source: "deterministic_only", Size: &unavailable}, nil))
	if strings.Contains(only, "DELIVERED") || !strings.Contains(only, "unavailable") {
		t.Errorf("a deterministic-only description renders the SIZE row alone, got:\n%s", only)
	}
}

// TestRenderRunDetailPrDescriptionHostile: the summary is untrusted; after the display unescape
// it still goes through cellText, so an ANSI escape, a bidi override and a newline never reach
// the terminal, while the (inert, in a terminal) `<script>` and `Closes #1` text is shown as is.
func TestRenderRunDetailPrDescriptionHostile(t *testing.T) {
	hostile := "safe\x1b[31mred\u202e &lt;script&gt;alert(1)&lt;/script&gt;\nC\u200bloses #1 \\[x\\](javascript:alert(1))"
	d := &apitypes.RunPrDescriptionDTO{Fields: apitypes.PrDescriptionFields{Summary: hostile}}
	unknown := "x\x1b[2Jy"
	out := renderDetail(t, prDescRun(d, &unknown))
	for _, bad := range []string{"\x1b", "\u202e", "\nC", "&lt;"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile description text reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	for _, want := range []string{"safe", "red", "<script>alert(1)</script>", "Closes #1", "[x](javascript:alert(1))", "Last PR update was not published ("} {
		if !strings.Contains(out, want) {
			t.Errorf("detail must still show %q, got:\n%q", want, out)
		}
	}
}
