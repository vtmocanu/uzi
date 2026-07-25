package main

import (
	"strings"
	"testing"
	"unicode"
)

// PRD #112 D7. Three separable properties: what sanitizeTTY strips, the ORDER it runs
// in relative to Glamour, and what neither can fix.

// The ORDER is a functional requirement. Glamour EMITS the escapes that make styled
// output work, so sanitizing after it strips its own styling and dumps the literal SGR
// text on screen. This measures both directions rather than asserting the rule.
func TestTUIRenderOrderIsSanitizeThenGlamour(t *testing.T) {
	r, err := newTUIRenderer(80, true)
	if err != nil {
		t.Fatalf("newTUIRenderer: %v", err)
	}
	const md = "# Heading\n\nsome **bold** text\n"

	// The shipped order.
	correct := r.Markdown(md)
	correctESC := strings.Count(correct, "\x1b")
	if correctESC == 0 {
		t.Fatalf("sanitize→Glamour produced NO escape sequences, so Glamour is not styling at all: %q", correct)
	}

	// The reversed order, computed here so the comparison is measured rather than
	// asserted. sanitizeTTY strips ESC (it is a control character), so running it after
	// Glamour removes every escape Glamour just emitted and leaves the SGR parameter
	// text — "[38;5;39;1m##" and friends — as visible garbage.
	raw, err := r.md.Render(md)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	reversed := sanitizeTTY(raw)
	if got := strings.Count(reversed, "\x1b"); got != 0 {
		t.Fatalf("reversed order still has %d escapes; sanitizeTTY is not stripping ESC, so this test is not measuring the ordering at all", got)
	}
	if !strings.Contains(reversed, "[38;5;") {
		t.Errorf("reversed order did not leave visible SGR parameter text; the ordering claim rests on that being what the user sees, so verify this on the current glamour before trusting it.\ngot: %q", reversed)
	}

	t.Logf("sanitize→Glamour: %d escapes (styled). Glamour→sanitize: 0 escapes, literal SGR text on screen.", correctESC)
}

// A render failure must degrade to the SANITIZED text, never to the raw input — the
// whole reason this path exists is that the input is attacker-influenceable.
func TestTUIRendererFallsBackToSanitizedNotRaw(t *testing.T) {
	var nilRenderer *tuiRenderer
	got := nilRenderer.Markdown("a\x1b[2Jb\u202Ec")
	for _, r := range got {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			t.Fatalf("the fallback path emitted %U, so it returned RAW input; a renderer failure must still sanitize", r)
		}
	}
}

// sanitizeTTY's two new holes, both of which a codepoint-range predicate cannot close.
func TestSanitizeTTYStripsControlAndFormatChars(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		// The ESC BYTE is what makes a sequence an escape sequence; stripping it leaves
		// "[2J" as inert literal text, which is correct and is exactly what the ordering
		// test above relies on being visible. An earlier version of this test expected
		// "ab" here — that asserted a behaviour sanitizeTTY does not and should not have.
		{"CSI cursor move loses its ESC", "a\x1b[2Jb", "a[2Jb"},
		// C1 as a RUNE (U+009B), not as the raw byte 0x9b: a lone 0x9b is invalid UTF-8
		// and decodes to U+FFFD, which is a printable replacement char, not a control.
		// The predicate has always operated on decoded runes.
		{"C1 control rune", "a\u009bb", "ab"},
		// DEL: the old `r < 0x20` predicate let 0x7f straight through.
		{"DEL", "a\x7fb", "ab"},
		// Cf: no range test catches these. The bidi override is the dangerous one —
		// it visually REORDERS text, so a label can be made to read as something else.
		{"RLO bidi override", "a\u202Eb", "ab"},
		{"LRO bidi override", "a\u202Db", "ab"},
		{"bidi isolate", "a\u2066b", "ab"},
		{"RTL mark", "a\u200Fb", "ab"},
		{"zero-width space", "a\u200Bb", "ab"},
		{"zero-width joiner", "a\u200Db", "ab"},
		{"BOM", "a\uFEFFb", "ab"},
		{"soft hyphen", "a\u00ADb", "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTTY(tc.in); got != tc.want {
				t.Errorf("sanitizeTTY(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Whatever survives, it is never a control or a format character — the property
	// that actually matters, asserted independently of any per-case expectation.
	for _, in := range []string{"a\x1b[2Jb", "a\u009bb", "a\x7fb", "a\u202Eb", "a\uFEFFb", "\x9b\xff"} {
		for _, r := range sanitizeTTY(in) {
			if r == '\t' || r == '\n' {
				continue
			}
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				t.Errorf("sanitizeTTY(%q) let %U through", in, r)
			}
		}
	}

	// Tab and newline SURVIVE: they are the two controls the human render path needs,
	// and dropping them would reflow every multi-line detail panel.
	if got := sanitizeTTY("a\tb\nc"); got != "a\tb\nc" {
		t.Errorf("sanitizeTTY dropped tab/newline: %q", got)
	}
	// Printable UTF-8 passes through untouched, including astral-plane runes whose
	// bytes fall in the old C1 range.
	const printable = "h\u00e9llo \U0001D11E \u65e5\u672c\u8a9e \u2713"
	if got := sanitizeTTY(printable); got != printable {
		t.Errorf("sanitizeTTY corrupted printable text: %q -> %q", printable, got)
	}
}

// The Cf strip also fixes a WIDTH bug, which is a second reason to want it: Cf runes
// are zero-width but capCell pads by runes, so before the fix a label full of them
// consumed column budget while drawing nothing.
func TestSanitizeTTYRemovesZeroWidthColumnTheft(t *testing.T) {
	// Twenty zero-width joiners plus "hi": 22 runes of budget, 2 columns drawn.
	label := strings.Repeat("\u200D", 20) + "hi"
	cell := cellText(label)
	if n := len([]rune(cell)); n != 2 {
		t.Errorf("cellText(%d zero-width runes + \"hi\") is %d runes, want 2 — zero-width runes must not consume the rune budget capCell pads by", 20, n)
	}
}

// What sanitization CANNOT fix, pinned so nobody reads D7 as more than it is:
// structural markdown spoofing survives, because the text IS valid markdown. The
// defence is the provenance box owning the chrome around it.
func TestProvenanceBoxOwnsTheChromeAroundSpoofableText(t *testing.T) {
	r, err := newTUIRenderer(60, true)
	if err != nil {
		t.Fatalf("newTUIRenderer: %v", err)
	}
	// Judge text pretending to be a verdict heading. It survives sanitizeTTY intact,
	// which is the point: stripping markdown structure would defeat using Glamour.
	const spoof = "# VERDICT: APPROVED"
	if !strings.Contains(sanitizeTTY(spoof), "# VERDICT") {
		t.Fatal("the spoof fixture no longer survives sanitization, so this test is not measuring what it claims")
	}
	body := r.Markdown(spoof)
	boxed := provenanceBox("judge summary (model-authored)", body, 60, newPalette(true))
	if !strings.Contains(boxed, "judge summary") {
		t.Error("the provenance box lost its label; the label is the defence — it tells the reader which frame a heading is inside")
	}
	// The label is drawn OUTSIDE the region the untrusted string can reach: the box
	// title comes first, so no amount of markdown in the body can precede it.
	titleAt := strings.Index(boxed, "judge summary")
	spoofAt := strings.Index(boxed, "VERDICT")
	if titleAt < 0 || spoofAt < 0 || titleAt > spoofAt {
		t.Errorf("the provenance label (at %d) does not precede the untrusted body (at %d); untrusted text must never be able to render above its own frame label", titleAt, spoofAt)
	}
}

// The plain path is for fixed-width cells, where sanitizeTTY ALONE is insufficient —
// D7 names only sanitizeTTY, and a raw newline in a cell breaks the rail's alignment.
func TestRendererPlainFoldsWhitespaceForCells(t *testing.T) {
	r, _ := newTUIRenderer(80, true)
	got := r.Plain("line one\nline two\twith a tab", 100)
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("Plain(%q) = %q, want newlines and tabs folded — sanitizeTTY keeps both, so a cell needs cellText on top of it", "line one\nline two\twith a tab", got)
	}
	if n := len([]rune(r.Plain(strings.Repeat("x", 200), 20))); n > 20 {
		t.Errorf("Plain did not clamp to the requested rune budget: got %d runes", n)
	}
}
