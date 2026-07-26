package prdpath_test

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/prdpath"
)

func TestValidateAccepts(t *testing.T) {
	for _, p := range []string{
		"prds/72-prd-lifecycle-in-run.md",
		// Decision 2's corollary: a PRD already under done/ is a VALID declaration
		// and a downstream no-op. Rejecting it would make a follow-up run's honest
		// report look like an attack.
		"prds/done/72-prd-lifecycle-in-run.md",
		"prds/sub/dir/9-a_b.md",
		"prds/x.md",
		"prds/UPPER-Case_1.2.md",
	} {
		if err := prdpath.Validate(p); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", p, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		p   string
		why string
	}{
		{"prds/../../../etc/passwd", "traversal"},
		{"prds/../x.md", "traversal, single hop"},
		{"prds/./x.md", "dot segment"},
		{"/prds/x.md", "absolute"},
		{"docs/x.md", "not rooted at prds/"},
		{"prds/x.txt", "wrong extension"},
		// The headline `prdLinkRe` failure: it validates by SUBSTRING, so this
		// passes it. An anchored whole-string predicate is what kills it.
		{"rm -rf / prds/x.md", "unanchored prefix"},
		{"https://host/g/p/-/blob/main/prds/x.md", "a URL is not a repo path"},
		{"prds/", "no file"},
		{"prds/.md", "dotfile segment"},
		{"prds/x.md#L4", "fragment suffix"},
		{"prds/x.md?ref=main", "query suffix"},
		{"prds//x.md", "empty segment"},
		{"./prds/x.md", "leading ./"},
		{"prds/.git/x.md", "dotfile directory"},
		{"prds/x.md\n", "trailing newline"},
		{"prds/a\nb.md", "embedded newline"},
		{"prds/a\x00b.md", "NUL"},
		{"prds\\x.md", "backslash separator"},
		{"prds/sub\\..\\x.md", "backslash traversal"},
		{"", "empty"},
	}
	for _, c := range cases {
		if err := prdpath.Validate(c.p); err == nil {
			t.Errorf("Validate(%q) = nil, want an error (%s)", c.p, c.why)
		}
	}
}

func TestValidateRejectsOverlongPath(t *testing.T) {
	long := "prds/" + strings.Repeat("a", prdpath.MaxPathLen) + ".md"
	if err := prdpath.Validate(long); err == nil {
		t.Errorf("Validate(<%d bytes>) = nil, want an error", len(long))
	}
	// Exactly at the cap is fine — the bound is inclusive.
	name := strings.Repeat("a", prdpath.MaxPathLen-len("prds/")-len(".md"))
	atCap := "prds/" + name + ".md"
	if len(atCap) != prdpath.MaxPathLen {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(atCap), prdpath.MaxPathLen)
	}
	if err := prdpath.Validate(atCap); err != nil {
		t.Errorf("Validate(<exactly %d bytes>) = %v, want nil", prdpath.MaxPathLen, err)
	}
}

// The alignment M5 depends on: every path Validate accepts must be findable by
// the same segment charset M5 will scan issue descriptions with. This pins the
// class rather than the implementation, so widening one side without the other
// reddens here.
func TestAcceptedPathsUseOnlyTheSharedSegmentCharset(t *testing.T) {
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
	for _, p := range []string{
		"prds/72-x.md",
		"prds/done/72-x.md",
		"prds/sub/dir/9-a_b.md",
		"prds/UPPER-Case_1.2.md",
	} {
		if err := prdpath.Validate(p); err != nil {
			t.Fatalf("fixture %q must be accepted: %v", p, err)
		}
		for _, seg := range strings.Split(p, "/") {
			for _, r := range seg {
				if !strings.ContainsRune(allowed, r) {
					t.Errorf("accepted path %q has segment char %q outside the shared class", p, r)
				}
			}
		}
	}
}
