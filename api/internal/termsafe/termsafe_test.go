package termsafe

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The literal payloads issues #169 and #180 were filed with. Kept as named constants so
// every test below attacks the SAME bytes the issues report rather than a paraphrase.
const (
	// Erase-in-display + cursor-home: blanks the scrollback view and repositions the
	// cursor, which is what lets injected text overwrite what the user already read.
	hostileClearScreen = "\x1b[2J\x1b[1;1H"
	// OSC 0: rewrites the terminal window title, terminated by BEL.
	hostileSetTitle = "\x1b]0;pwned\x07"
	// U+202E RIGHT-TO-LEFT OVERRIDE. Category Cf, so no C0/C1 range test can see it —
	// the discriminating case for the Cf half of the predicate.
	hostileBidi = "safe\u202egnp.exe"
	// The #169 headline: an embedded newline terminates a tabwriter row, so one worker
	// name forges a whole row in an admin's cross-tenant listing.
	hostileForgedRow = "mine\nffffffff-0000-0000-0000-000000000000\tvictim@example.com\tworker\trunning"
)

// TestValidateAgreesWithCellText is THE test this package exists for. It pins the
// biconditional stated in the package comment:
//
//	Validate(field, s) == nil   ⟺   CellText(s) == s
//
// One rule, two consumers. Without this the validator and the renderer drift — which is
// not hypothetical: issue #180's review found three hand-written copies of one character
// list, under a comment asserting they could not drift.
//
// The corpus is deliberately named rather than generated, so a failure says WHICH input
// broke the agreement and every entry documents a decision. TestValidateAgreesOverEveryRune
// below covers the space no hand-written corpus can.
func TestValidateAgreesWithCellText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// changed is what CellText does to the input, asserted independently so this
		// test cannot pass by both halves being broken in the same direction.
		changed bool
	}{
		// --- clean: stored and displayed byte-identically ------------------------
		{"plain-ascii", "laptop-01", false},
		{"internal-spaces", "vlad's build box", false},
		{"accented-latin", "Ștefan's Mac", false},
		{"cjk", "工場ワーカー", false},
		{"single-codepoint-emoji", "🔑 key", false},
		{"variation-selector-emoji", "❤️ favourite", false},
		{"combining-mark", "a\u0301 zalgo\u0300\u0301\u0302", false},
		{"literal-replacement-char", "a\ufffdb", false},
		{"empty", "", false},
		{"at-the-byte-cap", strings.Repeat("x", 200), false},

		// --- control characters --------------------------------------------------
		{"esc-clear-screen", "run " + hostileClearScreen + " title", true},
		{"esc-set-title", "worker" + hostileSetTitle + "-01", true},
		{"bel", "a\x07b", true},
		{"nul", "a\x00b", true},
		{"del", "a\x7fb", true},
		{"c1-nel", "a\u0085b", true},
		{"cr", "a\rb", true},
		{"tab-walks-the-column", "a\tb", true},
		{"newline-forges-a-row", hostileForgedRow, true},

		// --- format characters (Cf) ----------------------------------------------
		{"bidi-override", hostileBidi, true},
		{"bidi-isolate", "a\u2066b\u2069c", true},
		{"zero-width-space", "a\u200bb", true},
		{"zwj-family-emoji", "family 👨\u200d👩\u200d👧 box", true},
		{"bom", "\ufeffname", true},
		{"soft-hyphen", "work\u00adstation", true},

		// --- invalid UTF-8 (the renderer substitutes U+FFFD) ---------------------
		{"lone-continuation-byte", "a\xffb", true},
		{"truncated-multibyte", "a\xe2\x82", true},

		// --- edge whitespace (the renderer trims) --------------------------------
		{"leading-space", " name", true},
		{"trailing-space", "name ", true},
		{"nbsp-tail", "name\u00a0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CellText(tc.in)
			if changed := got != tc.in; changed != tc.changed {
				t.Fatalf("CellText changed=%v, want %v: %q -> %q", changed, tc.changed, tc.in, got)
			}
			err := Validate("name", tc.in)
			if (err != nil) != tc.changed {
				t.Fatalf("Validate err=%v, but CellText changed=%v — the two disagree on %q",
					err, tc.changed, tc.in)
			}
			// A rejection must SAY something. An empty message in a 400 body is the
			// same failure as no validation at all, from the user's side.
			if err != nil && !strings.Contains(err.Error(), "name") {
				t.Fatalf("Validate error does not name the field: %q", err.Error())
			}
		})
	}
}

// TestValidateAgreesOverEveryRune walks all of Unicode. The named corpus above proves the
// agreement on the inputs someone thought of; this proves there is no rune anywhere the
// two halves disagree about — which is the only form of this check that a future edit to
// either half cannot quietly step around.
//
// Three shapes per rune, because the three clauses fail at different positions: infixed
// (any strip), alone (a strip that empties), and trailing (where the renderer's trim and
// the strip interact — a Cf at the edge shields an adjacent space from a trim).
//
// Surrogates are skipped explicitly: string(rune(0xD800)) yields U+FFFD in Go, so those
// iterations would test the replacement character 2048 times rather than a surrogate.
func TestValidateAgreesOverEveryRune(t *testing.T) {
	shapes := []struct {
		name string
		make func(rune) string
	}{
		{"infix", func(r rune) string { return "a" + string(r) + "b" }},
		{"alone", func(r rune) string { return string(r) }},
		{"trailing", func(r rune) string { return "a " + string(r) }},
	}
	var checked int
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		for _, sh := range shapes {
			s := sh.make(r)
			wantErr := CellText(s) != s
			if gotErr := Validate("name", s) != nil; gotErr != wantErr {
				t.Fatalf("disagreement at U+%04X (%s): Validate rejects=%v, CellText changes=%v (%q -> %q)",
					r, sh.name, gotErr, wantErr, s, CellText(s))
			}
			checked++
		}
	}
	// Positive control: a sweep that skipped everything reports the same silence as a
	// sweep that agreed everywhere. 0x110000 - 0x800 surrogates, times three shapes.
	if want := (unicode.MaxRune + 1 - 2048) * len(shapes); checked != want {
		t.Fatalf("swept %d strings, want %d — the loop did not cover Unicode", checked, want)
	}
}

// TestVariationSelectorAndZWJ pins the one casualty and its near-neighbour, because the
// difference between them is not readable from either character and a reviewer WILL ask.
// U+200D (ZWJ) is Cf, so a family emoji is refused; U+FE0F (VS16) is Mn, so a
// variation-selector emoji is stored and displayed intact. Measured here rather than
// asserted in prose: the category lookup runs in the test.
func TestVariationSelectorAndZWJ(t *testing.T) {
	if !unicode.In(0x200D, unicode.Cf) {
		t.Fatal("U+200D is not Cf in this Go's Unicode tables — the ZWJ rejection rests on this")
	}
	if unicode.In(0xFE0F, unicode.Cf) {
		t.Fatal("U+FE0F is Cf in this Go's Unicode tables — ❤️ would be refused, which is not intended")
	}
	if !unicode.In(0xFE0F, unicode.Mn) {
		t.Fatal("U+FE0F is not Mn in this Go's Unicode tables")
	}

	family := "👨\u200d👩\u200d👧"
	if err := Validate("name", family); err == nil {
		t.Fatal("ZWJ family emoji accepted; the renderer would break it into three glyphs")
	}
	// And the message must explain itself: a user hitting this deserves the reason.
	if err := Validate("name", family); !strings.Contains(err.Error(), "emoji") {
		t.Fatalf("ZWJ rejection does not mention emoji: %q", err.Error())
	}

	heart := "❤️ favourite"
	if err := Validate("name", heart); err != nil {
		t.Fatalf("variation-selector emoji refused: %v", err)
	}
	if got := CellText(heart); got != heart {
		t.Fatalf("CellText mangled a variation-selector emoji: %q -> %q", heart, got)
	}
	// Positive control on the pair: the two differ only in the joiner, so this proves
	// the rule is the CATEGORY and not "emoji are rejected".
	if !utf8.ValidString(family) || !utf8.ValidString(heart) {
		t.Fatal("fixture is not valid UTF-8")
	}
}

// TestValidateRejectsHostilePayloads pairs each rejection with the reason the payload is
// dangerous, and with a positive assertion on the same bytes: an absence alone cannot
// distinguish a working validator from one that rejects everything.
func TestValidateRejectsHostilePayloads(t *testing.T) {
	payloads := map[string]string{
		"clear-screen": hostileClearScreen,
		"set-title":    hostileSetTitle,
		"bidi":         hostileBidi,
		"forged-row":   hostileForgedRow,
	}
	for name, p := range payloads {
		t.Run(name, func(t *testing.T) {
			if err := Validate("name", p); err == nil {
				t.Fatalf("accepted hostile payload %q", p)
			}
			// The positive half: strip the hostile runes and the SAME string is
			// accepted, so the rejection is about those runes and not about length,
			// alphabet or anything else in the payload.
			if cleaned := CellText(p); cleaned != "" {
				if err := Validate("name", cleaned); err != nil {
					t.Fatalf("the sanitized form of %q was also refused: %v", p, err)
				}
			}
		})
	}
}

// TestValidateIgnoresEmptyAndLength pins the two checks Validate deliberately does NOT
// make, because both are owned by the callers and folding either in here would break the
// biconditional for exactly the inputs where the two questions differ.
func TestValidateIgnoresEmptyAndLength(t *testing.T) {
	if err := Validate("name", ""); err != nil {
		t.Fatalf("Validate rejected the empty string: %v — CellText(\"\") == \"\", so it must not", err)
	}
	long := strings.Repeat("x", 10_000)
	if err := Validate("name", long); err != nil {
		t.Fatalf("Validate applied a length cap: %v — the handlers own their own caps", err)
	}
}

// TestValidateNamesTheField proves the field argument reaches the message, which is the
// whole reason it is a parameter rather than a hardcoded "name": one predicate serves
// client_desc too, and a 400 saying "name" on a client_desc field misdirects the caller.
func TestValidateNamesTheField(t *testing.T) {
	for _, field := range []string{"name", "client_desc"} {
		for _, bad := range []string{"a\x1bb", "a\u202eb", "a\xffb", " a"} {
			err := Validate(field, bad)
			if err == nil {
				t.Fatalf("%q accepted for field %q", bad, field)
			}
			if !strings.HasPrefix(err.Error(), field+" ") {
				t.Fatalf("message for field %q does not start with it: %q", field, err.Error())
			}
		}
	}
}

// TestSanitizeTTYSparesWhitespace pins the one behavioural difference between the two
// renderers, which is why both exist: flowing text keeps \t and \n, a table cell folds
// them. Validate agrees with CellText (the stricter one), so a name that a handler
// accepts is safe in either.
func TestSanitizeTTYSparesWhitespace(t *testing.T) {
	in := "line one\n\tline two\x1b[2J"
	want := "line one\n\tline two[2J"
	if got := SanitizeTTY(in); got != want {
		t.Fatalf("SanitizeTTY(%q) = %q, want %q", in, got, want)
	}
	if got, want := CellText(in), "line one  line two[2J"; got != want {
		t.Fatalf("CellText(%q) = %q, want %q", in, got, want)
	}
}

// TestUnsafeIsTheDisjointPair records why both halves of the predicate are needed: Cc and
// Cf do not overlap, so IsControl alone never sees U+202E and a Cf test alone never sees
// ESC. A future "simplification" to one test fails here with the reason attached.
func TestUnsafeIsTheDisjointPair(t *testing.T) {
	if unicode.IsControl(0x202E) {
		t.Fatal("U+202E reads as a control character; the Cf half would be redundant, which it is not")
	}
	if unicode.In(0x1B, unicode.Cf) {
		t.Fatal("ESC reads as Cf; the IsControl half would be redundant, which it is not")
	}
	for _, r := range []rune{0x00, 0x07, 0x1B, 0x7F, 0x85, 0x9F, 0x200B, 0x200D, 0x202E, 0x2069, 0xFEFF, 0x00AD} {
		if !Unsafe(r) {
			t.Fatalf("U+%04X classified safe", r)
		}
	}
	for _, r := range []rune{'a', ' ', '~', 0x00E9, 0x5DE5, 0x1F511, 0xFE0F, 0x0301, 0xFFFD} {
		if Unsafe(r) {
			t.Fatalf("U+%04X classified unsafe", r)
		}
	}
}

// TestSanitizeBounded pins the persist-time guard for untrusted flowing markdown. The
// cases are named after the property each defends: the \n/\t exception that keeps this
// separate from the single-line scrubbers, the Cf strip that is the Trojan-Source fix,
// the whole-rune byte bound, the trim-again, and the U+FFFD substitution it shares with
// SanitizeTTY.
func TestSanitizeBounded(t *testing.T) {
	const big = 1 << 20 // a cap far above any case's length, so it never truncates
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"newline-preserved", "line one\nline two", big, "line one\nline two"},
		{"tab-preserved", "col\tsep", big, "col\tsep"},
		{"esc-stripped", "a\x1bb", big, "ab"},
		{"bel-stripped", "a\x07b", big, "ab"},
		{"bidi-override-stripped", "safe\u202egnp.exe", big, "safegnp.exe"},
		{"zero-width-stripped", "a\u200bb", big, "ab"},
		{"leading-trailing-trimmed", "  hi  ", big, "hi"},
		// Trim-again: a Cf sits between the edge whitespace and the content, so the leading
		// TrimSpace (which runs before the strip, and Cf is not White_Space) cannot reach the
		// spaces. The strip removes the Cf, then the final TrimSpace cleans the exposed edges.
		{"trim-again-after-cf-strip", "\u200b  hi  \u200b", big, "hi"},
		{"empty", "", big, ""},
		{"whitespace-only", "   \n\t  ", big, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeBounded(tc.in, tc.max); got != tc.want {
				t.Fatalf("SanitizeBounded(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}

	// The byte bound is checked AFTER each WHOLE rune is written, so a multi-byte rune is
	// never split at the cut. When max lands mid-rune the loop includes the rune that
	// crosses it and then stops, so the output is a whole number of runes, valid UTF-8, and
	// overshoots max by at most one rune (never truncates inside one — the "max..max+3
	// bytes" bound the sibling scrubbers document). It must NOT split, which a byte cut
	// would, yielding invalid UTF-8.
	t.Run("multibyte-rune-not-split", func(t *testing.T) {
		const r = "日" // 3 bytes in UTF-8
		if len(r) != 3 {
			t.Fatalf("fixture assumption broke: %q is %d bytes", r, len(r))
		}
		in := strings.Repeat(r, 8) // 24 bytes
		const max = 10             // lands mid-rune: byte 10 is inside the 4th rune (bytes 9..11)
		got := SanitizeBounded(in, max)
		if !utf8.ValidString(got) {
			t.Fatalf("output %q is not valid UTF-8 — a rune was split at the byte bound", got)
		}
		if len(got)%len(r) != 0 {
			t.Fatalf("output %q is %d bytes, not a whole number of 3-byte runes", got, len(got))
		}
		// It stopped at the first whole-rune boundary at or past max (12 bytes = 4 runes),
		// having neither split the crossing rune nor kept the whole input.
		if want := strings.Repeat(r, 4); got != want {
			t.Fatalf("output %q, want %q (whole runes through the one that crosses max)", got, want)
		}
		if len(got) >= len(in) {
			t.Fatalf("output %q was not bounded: %d bytes >= input %d", got, len(got), len(in))
		}
	})

	// Invalid UTF-8 decodes to U+FFFD as the range loop walks the string, matching
	// SanitizeTTY's documented behaviour; the replacement char is present in the output.
	t.Run("invalid-utf8-becomes-fffd", func(t *testing.T) {
		got := SanitizeBounded("a\xffb", big)
		if !strings.ContainsRune(got, '�') {
			t.Fatalf("SanitizeBounded(%q) = %q, want a U+FFFD replacement char", "a\xffb", got)
		}
	})
}

// FuzzValidateAgreesWithCellText extends the biconditional past the two enumerable
// corpora above to arbitrary BYTE strings — the axis neither reaches, because a rune loop
// can only build well-formed UTF-8 and a named corpus can only hold what someone wrote
// down. It runs its seeds under an ordinary `go test`, so the seeds are chosen to be the
// discriminating ones rather than decoration.
func FuzzValidateAgreesWithCellText(f *testing.F) {
	for _, seed := range []string{
		"", "laptop-01", hostileBidi, hostileClearScreen, hostileForgedRow,
		"👨\u200d👩\u200d👧", "❤️", "a\xffb", " x ", "a\tb", "\u00ad", "\ufffd",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		wantErr := CellText(s) != s
		if gotErr := Validate("name", s) != nil; gotErr != wantErr {
			t.Fatalf("disagreement: Validate rejects=%v, CellText changes=%v for %q (%s)",
				gotErr, wantErr, s, describe(s))
		}
	})
}

// describe renders a failing fuzz input as codepoints, because %q on a string full of
// invisible characters prints something that looks fine and tells you nothing.
func describe(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "U+%04X", r)
	}
	return b.String()
}

// TestUnsafeMatchesSharedCorpus pins Unsafe to the SHARED golden corpus that
// web/src/lib/safeText.test.ts also loads (issue #161), so the Go answer and the JS
// stripUnsafeChars answer cannot silently drift: a change to either predicate that
// disagrees with a listed code point reds that side. Read at test time via a relative
// path (NOT go:embed, which cannot reach outside the package dir) — which is why both Go
// gates mandate -count=1.
func TestUnsafeMatchesSharedCorpus(t *testing.T) {
	data, err := os.ReadFile("../../../fixtures/termsafe/corpus.json")
	if err != nil {
		t.Fatalf("read shared corpus: %v", err)
	}
	var corpus struct {
		Codepoints []struct {
			CP     string `json:"cp"`
			Name   string `json:"name"`
			Unsafe bool   `json:"unsafe"`
		} `json:"codepoints"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("parse shared corpus: %v", err)
	}
	const wantCount = 36 // keep in sync with EXPECTED in web/src/lib/safeText.test.ts; reds on add/remove so a corpus change is deliberate on both sides
	if len(corpus.Codepoints) != wantCount {
		t.Fatalf("shared corpus has %d code points, want %d (bump wantCount here and EXPECTED in safeText.test.ts when you intentionally add/remove one)", len(corpus.Codepoints), wantCount)
	}
	var sawUnsafe, sawSafe bool
	for _, e := range corpus.Codepoints {
		n, err := strconv.ParseInt(e.CP, 16, 32)
		if err != nil {
			t.Fatalf("bad cp %q (%s): %v", e.CP, e.Name, err)
		}
		if got := Unsafe(rune(n)); got != e.Unsafe {
			t.Errorf("Unsafe(U+%s %s) = %v, want %v (shared corpus)", e.CP, e.Name, got, e.Unsafe)
		}
		if e.Unsafe {
			sawUnsafe = true
		} else {
			sawSafe = true
		}
	}
	if !sawUnsafe || !sawSafe {
		t.Fatalf("shared corpus must cover both classes: sawUnsafe=%v sawSafe=%v", sawUnsafe, sawSafe)
	}
}
