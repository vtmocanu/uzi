package toolseed

import (
	"maps"
	"testing"
)

// TestStripJSONComments pins the HuJSON comment pass buildSeedSet runs before
// encoding/json. Every row compares the WHOLE output, so a scanner that drops, keeps or
// duplicates a byte fails. Several rows are chosen so that a plausible scanner defect
// turns one row's input into ANOTHER row's expected output (R4 -> R5 when an escape
// consumes one byte instead of two; R9 -> R8 when any slash opens a comment).
func TestStripJSONComments(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// A line comment is removed up to, but not including, its newline: the next
		// line keeps its own token boundary.
		{"R1 line comment keeps newline", "\"a\" // x\n\"b\"", "\"a\" \n\"b\""},
		// Comment markers inside a JSON string are data, copied verbatim.
		{"R2 slashes in string", `"https://h/x"`, `"https://h/x"`},
		{"R3 block markers in string", `"a/*b*/"`, `"a/*b*/"`},
		// An escaped quote does not close the string, so the // after it is data.
		{"R4 escaped quote keeps string open", `"a\"//b"`, `"a\"//b"`},
		// The string closes on the unescaped quote after an escaped one; what follows
		// is a real comment.
		{"R5 string closes after escaped quote", `"a\""//c`, `"a\""`},
		// An escaped backslash right before the quote: the quote closes the string.
		{"R6 escaped backslash then close", `"a\\"//c`, `"a\\"`},
		// A lone * and a lone / inside a block comment do not end it.
		{"R7 block comment with lone star and slash", `1/* a * b / c */2`, `12`},
		// An unterminated block comment swallows the rest of the input, without
		// reading past the end.
		{"R8 unterminated block comment", `1/* x`, `1`},
		// The same with a trailing star: the close check must not peek past the end.
		{"R11 unterminated block comment ending in a star", `1/* x*`, `1`},
		// A single slash outside a string is not a comment.
		{"R9 lone slash", `1/2`, `1/2`},
		// A backslash as the last byte inside an unterminated string is copied, and
		// the escape branch does not read past the end.
		{"R10 trailing backslash in string", `"a\`, `"a\`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(stripJSONComments([]byte(c.in))); got != c.want {
				t.Errorf("stripJSONComments(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// swapSeedManifest replaces the embedded manifest for one test and restores it on
// cleanup. seedSet was built once at package init, so Covered is unaffected by the
// swap; only a direct buildSeedSet() call reads the replacement. No test in this
// package runs in parallel, so the swap is never observed by another test.
func swapSeedManifest(t *testing.T, manifest string) {
	t.Helper()
	orig := seedManifest
	t.Cleanup(func() { seedManifest = orig })
	seedManifest = []byte(manifest)
}

// TestBuildSeedSetReadsCommentedManifest: a HuJSON manifest with a commented-out
// package, a block comment between entries and a string value carrying comment
// markers yields exactly the uncommented packages, normalized to allowlist names.
func TestBuildSeedSetReadsCommentedManifest(t *testing.T) {
	swapSeedManifest(t, `{
  // A header comment mentioning "https://example.com/docs".
  "description": "see https://example.com/x and /* this */",
  "packages": [
    "jq.bin",
    // "cowsay",
    /* a block comment between entries */ "yq-go",
    "ripgrep"
  ]
}`)
	want := map[string]bool{"jq": true, "yq": true, "ripgrep": true}
	if got := buildSeedSet(); !maps.Equal(got, want) {
		t.Fatalf("buildSeedSet() = %v, want %v", got, want)
	}
}

// TestBuildSeedSetFailsClosedOnMalformedManifest: a manifest that does not parse
// yields an empty coverage set instead of panicking in package init. Rebuilding from
// malformed input returns a fresh set and leaves the live, init-time coverage behind
// Covered intact: a baked package and a documented exception stay covered, and an
// un-baked package stays uncovered.
func TestBuildSeedSetFailsClosedOnMalformedManifest(t *testing.T) {
	swapSeedManifest(t, "{not json")
	if got := buildSeedSet(); !maps.Equal(got, map[string]bool{}) {
		t.Fatalf("buildSeedSet() on a malformed manifest = %v, want an empty set", got)
	}
	if !Covered("jq") {
		t.Error(`Covered("jq") = false after a malformed rebuild, want true (baked in the live manifest)`)
	}
	if !Covered("kubectl") {
		t.Error(`Covered("kubectl") = false after a malformed rebuild, want true (a documented exception)`)
	}
	if Covered("cowsay") {
		t.Error(`Covered("cowsay") = true after a malformed rebuild, want false (neither baked nor an exception)`)
	}
}
