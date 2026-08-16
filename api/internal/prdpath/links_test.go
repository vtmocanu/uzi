package prdpath_test

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/prdpath"
)

// The differential test the design asks for: drive it from the SAME rejection
// table Validate's own test uses, so there is no second source of truth about what
// a well-formed PRD path is.
//
// Every description here ALSO carries a well-formed path that Links must return.
// Without that positive co-presence the assertion is green against a Links that
// returns nothing at all — the negative-row defect §3.7 calls out.
func TestLinksNeverReturnsSomethingValidateRejects(t *testing.T) {
	const good = "prds/72-good.md"
	for _, c := range rejected {
		if c.p == "" {
			continue // an empty string cannot occur in text
		}
		desc := "see " + c.p + " and also " + good + " for context"
		got := prdpath.Links(desc)

		var sawGood bool
		for _, l := range got {
			if l == good {
				sawGood = true
			}
			if l == c.p {
				t.Errorf("Links surfaced %q, which Validate rejects (%s)", c.p, c.why)
			}
			if err := prdpath.Validate(l); err != nil {
				t.Errorf("Links returned %q which fails Validate: %v", l, err)
			}
		}
		if !sawGood {
			t.Errorf("fixture for %q did not yield the co-present good path; the negative assertion above proves nothing", c.p)
		}
	}
}

func TestLinksFindsRealShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc string
		want []string
	}{
		{"bare path", "Implements prds/72-x.md fully.", []string{"prds/72-x.md"}},
		{"already under done", "See prds/done/72-x.md.", []string{"prds/done/72-x.md"}},
		{
			"blob URL keeps being found (the `/` before the span is allowed)",
			"https://gl.example.com/g/p/-/blob/main/prds/72-x.md",
			[]string{"prds/72-x.md"},
		},
		{"line-fragment suffix", "prds/72-x.md#L4", []string{"prds/72-x.md"}},
		{"query suffix", "prds/72-x.md?ref=main", []string{"prds/72-x.md"}},
		{"markdown link", "[the PRD](prds/72-x.md)", []string{"prds/72-x.md"}},
		{"several, in order of appearance", "prds/1-a.md then prds/2-b.md", []string{"prds/1-a.md", "prds/2-b.md"}},
		{"deduplicated", "prds/1-a.md and again prds/1-a.md", []string{"prds/1-a.md"}},
		{"none", "no prd here at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := prdpath.Links(tc.desc)
			if len(got) != len(tc.want) {
				t.Fatalf("Links(%q) = %v, want %v", tc.desc, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Links(%q)[%d] = %q, want %q", tc.desc, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Boundary rows, each paired with a co-present positive so a matcher that never
// matches cannot pass them.
func TestLinksIsPathAligned(t *testing.T) {
	const good = "prds/72-good.md"
	for _, tc := range []struct {
		name, fragment string
	}{
		{"a longer suffix is not a PRD path", "prds/72-x.md.bak"},
		{"a longer prefix is not a PRD path", "xprds/72-x.md"},
		{"an underscore before is still a token character", "a_prds/72-x.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := prdpath.Links(tc.fragment + " " + good)
			for _, l := range got {
				if strings.Contains(tc.fragment, l) && l != good {
					// Only fail when the match came from the fragment itself.
					if strings.HasPrefix(tc.fragment, l) || strings.Contains(tc.fragment, "/"+l) {
						t.Errorf("Links matched %q inside the non-aligned fragment %q", l, tc.fragment)
					}
				}
			}
			var sawGood bool
			for _, l := range got {
				if l == good {
					sawGood = true
				}
			}
			if !sawGood {
				t.Fatalf("co-present good path missing from %v; the negative assertion proves nothing", got)
			}
		})
	}
}

// The non-overlapping-match asymmetry (M5 review nit 1). FindAllStringIndex returns
// NON-OVERLAPPING matches, so without rescanning from start+1 an ALIGNED path
// nested inside a rejected span is lost entirely — which is what ReplacePath below
// deliberately avoids.
//
// NOTE ON THE INPUT, because the obvious one does not demonstrate this. The review
// cited `aprds/x.mdprds/y.md`, but the inner candidate there is preceded by `d`, so
// it is a FRAGMENT of the token `x.mdprds` and the boundary rule rejects it
// correctly — `[]` is the right answer for that string, not a bug. The loss needs
// the nested candidate to be genuinely aligned, which means preceded by `/`.
func TestLinksFindsAPathNestedInsideARejectedSpan(t *testing.T) {
	// The regexp consumes `prds/a/prds/b.md` as ONE span (its `(seg/)*` swallows
	// `a/` and `prds/`), and that span is misaligned because `x` precedes it. The
	// inner `prds/b.md` at offset 8 IS aligned: a `/` precedes it.
	const in = "xprds/a/prds/b.md"
	got := prdpath.Links(in)
	var found bool
	for _, l := range got {
		if l == "prds/b.md" {
			found = true
		}
		if l == "prds/a/prds/b.md" {
			t.Errorf("the misaligned outer span must still be rejected; got %v", got)
		}
	}
	if !found {
		t.Errorf("Links(%q) = %v, want it to contain prds/b.md — an ALIGNED path nested inside a rejected span",
			in, got)
	}
	// The control: the outer span really is rejected, so this is not passing just
	// because everything matches.
	if len(prdpath.Links("xprds/a.md")) != 0 {
		t.Errorf("a misaligned span must still be rejected on its own")
	}
}

func TestReplacePath(t *testing.T) {
	const old, nu = "prds/72-x.md", "prds/done/72-x.md"
	for _, tc := range []struct {
		name, in, want string
		changed        int
	}{
		{"bare", "See prds/72-x.md.", "See prds/done/72-x.md.", 1},
		{
			"blob URL prefix survives by construction",
			"https://gl/g/p/-/blob/main/prds/72-x.md",
			"https://gl/g/p/-/blob/main/prds/done/72-x.md",
			1,
		},
		{"line fragment survives", "prds/72-x.md#L4", "prds/done/72-x.md#L4", 1},
		{"query suffix survives", "prds/72-x.md?ref=main", "prds/done/72-x.md?ref=main", 1},
		{
			"EVERY occurrence, or a twice-linked PRD is left half-broken",
			"prds/72-x.md and https://gl/-/blob/main/prds/72-x.md",
			"prds/done/72-x.md and https://gl/-/blob/main/prds/done/72-x.md",
			2,
		},
		{"a non-aligned occurrence is left alone", "prds/72-x.md.bak", "prds/72-x.md.bak", 0},
		{"absent", "nothing here", "nothing here", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := prdpath.ReplacePath(tc.in, old, nu)
			if got != tc.want {
				t.Errorf("ReplacePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if n != tc.changed {
				t.Errorf("changed = %d, want %d", n, tc.changed)
			}
		})
	}
}

// By IDENTITY, never by tally. "Two links untouched" is satisfied when the WRONG
// two were left alone; only byte-identity of each non-target says what it means.
func TestReplacePathLeavesOtherPRDLinksByteIdentical(t *testing.T) {
	const (
		target = "prds/72-x.md"
		moved  = "prds/done/72-x.md"
		otherA = "prds/40-other.md"
		otherB = "prds/done/13-old.md"
	)
	in := "Related: " + otherA + ", " + otherB + ". Implements " + target + "."
	got, n := prdpath.ReplacePath(in, target, moved)

	if n != 1 {
		t.Fatalf("changed = %d, want exactly 1", n)
	}
	if !strings.Contains(got, "Related: "+otherA+", "+otherB+".") {
		t.Errorf("the two unrelated links are not byte-identical to their input; got %q", got)
	}
	if !strings.Contains(got, "Implements "+moved+".") {
		t.Errorf("the target was not rewritten; got %q", got)
	}
}

// A span already equal to newPath is not a change: this is what makes a re-run
// after a partial write converge instead of looping.
func TestReplacePathIsIdempotent(t *testing.T) {
	const old, nu = "prds/72-x.md", "prds/done/72-x.md"
	once, n1 := prdpath.ReplacePath("See prds/72-x.md.", old, nu)
	twice, n2 := prdpath.ReplacePath(once, old, nu)
	if n1 != 1 {
		t.Fatalf("first pass changed %d, want 1", n1)
	}
	if n2 != 0 || twice != once {
		t.Errorf("second pass changed %d and produced %q; want 0 and no change", n2, twice)
	}
	// And the degenerate case the watcher can hit when a PRD is already in done/.
	if _, n := prdpath.ReplacePath("See "+nu+".", nu, nu); n != 0 {
		t.Errorf("old == new must report 0 changes, got %d", n)
	}
}

// oldPath is a literal, never a pattern. `.` and `-` are regexp metacharacters and
// oldPath originates in agent-adjacent data.
func TestReplacePathTreatsOldPathAsALiteral(t *testing.T) {
	// If `.` were a wildcard, this would match `prds/axbxmd` too.
	in := "prds/a.b.md and prds/axbxmd"
	got, n := prdpath.ReplacePath(in, "prds/a.b.md", "prds/done/a.b.md")
	if n != 1 {
		t.Fatalf("changed = %d, want 1 (a regexp-ish match would find 2)", n)
	}
	if !strings.Contains(got, "prds/axbxmd") {
		t.Errorf("the literal-looking neighbour was rewritten; got %q", got)
	}
}
