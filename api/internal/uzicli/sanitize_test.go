package uzicli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The literal payloads issue #180 was filed with, measured against a hostile fake
// server. Kept as named constants so every test below attacks the SAME bytes the issue
// reports rather than a paraphrase of them.
const (
	// Erase-in-display + cursor-home: blanks the scrollback view and repositions the
	// cursor, which is what lets injected text overwrite what the user already read.
	hostileClearScreen = "\x1b[2J\x1b[1;1H"
	// OSC 0: rewrites the terminal window title, terminated by BEL.
	hostileSetTitle = "\x1b]0;pwned\x07"
	// U+202E RIGHT-TO-LEFT OVERRIDE. Category Cf, so no C0/C1 range test can see it,
	// and json.Marshal does not escape it either — the discriminating case for both
	// halves of this change.
	hostileBidi = "safe\u202egnp.exe"
)

// TestSanitizeTTYStripsHostilePayloads pairs every negative with a positive on the same
// string: an absence alone cannot tell a working stripper from one that emptied its
// input, or from a test that fed it nothing.
func TestSanitizeTTYStripsHostilePayloads(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"issue-clear-screen", "run " + hostileClearScreen + " title", "run [2J[1;1H title"},
		{"issue-set-title", "worker" + hostileSetTitle + "-01", "worker]0;pwned-01"},
		{"bidi-override", hostileBidi, "safegnp.exe"},
		{"del", "a\x7fb", "ab"},
		{"nul", "a\x00b", "ab"},
		{"tab-and-newline-spared", "a\tb\nc", "a\tb\nc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeTTY(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizeTTY(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The negative half, stated on the byte that does the damage rather than on
			// the whole payload: a partial strip that left a bare ESC behind would still
			// satisfy a "does not contain the full sequence" check.
			if strings.ContainsRune(got, 0x1b) {
				t.Errorf("SanitizeTTY(%q) still carries a raw ESC: %q", tc.in, got)
			}
		})
	}
}

// TestSanitizeTTYStripsByCategoryNotByRange is the half a C0-only stripper fails. Every
// codepoint here is invisible in an editor, so they are built from explicit rune values
// rather than pasted: a reader cannot tell "this case covers U+200B" from "something ate
// it", and a literal BOM is not even legal Go (`illegal byte order mark` — which is how
// the BOM case below was confirmed to be present rather than assumed).
func TestSanitizeTTYStripsByCategoryNotByRange(t *testing.T) {
	cases := []struct {
		name string
		r    rune
	}{
		{"c1-csi", 0x009B},           // C1 CSI: one codepoint doing what ESC [ does
		{"c1-nel", 0x0085},           // C1 NEL: moves the cursor to the next line
		{"zero-width-space", 0x200B}, // Cf, invisible, silently pads a fixed-width rail
		{"zero-width-joiner", 0x200D},
		{"bom", 0xFEFF},
		{"soft-hyphen", 0x00AD},
		{"rtl-mark", 0x200F},
		{"rtl-override", 0x202E}, // the spoof: reorders a name so it reads as another
		{"pop-directional", 0x202C},
		{"lrt-isolate", 0x2066},
		{"pop-isolate", 0x2069},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := "wo" + string(tc.r) + "rd"
			got := SanitizeTTY(in)
			// Negative: the codepoint is gone.
			if strings.ContainsRune(got, tc.r) {
				t.Errorf("SanitizeTTY left U+%04X in place: %q", tc.r, got)
			}
			// Positive on the same string, so an empty return cannot satisfy this.
			if got != "word" {
				t.Errorf("SanitizeTTY(%q) = %q, want %q", in, got, "word")
			}
		})
	}
}

// TestSanitizeTTYPreservesLegitimateText is the control that keeps the stripper honest:
// the target is control characters, not "non-ASCII". Without it, `return ""` passes
// every assertion above.
func TestSanitizeTTYPreservesLegitimateText(t *testing.T) {
	for _, s := range []string{
		"ordinary ascii title",
		"héllo wörld",         // combining/precomposed Latin
		"日本語のタイトル",            // CJK
		"🚀 ship it ✅",         // emoji, no ZWJ
		"emoji with VS16: ❤️", // U+FE0F is Mn, not Cf
		"ελληνικά",            // Greek
		"العربية",             // Arabic letters (not the bidi CONTROLS)
		"a/b-c_d.e:f@g,h;i()", // punctuation
	} {
		if got := SanitizeTTY(s); got != s {
			t.Errorf("SanitizeTTY(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestSanitizeTTYBreaksZWJSequences pins the one real casualty of stripping by Unicode
// category, so it is a recorded decision rather than a surprise. U+200D (ZERO WIDTH
// JOINER) is Cf, the same category as the bidi overrides this exists to remove, so a
// family emoji decomposes into its members. Cosmetic; a reordered worker name is not.
func TestSanitizeTTYBreaksZWJSequences(t *testing.T) {
	// Built from runes for the reason the category test above states: a pasted ZWJ is
	// indistinguishable from a missing one, and here a missing one would make the test
	// assert nothing.
	const zwj = 0x200D
	family := string(rune(0x1F468)) + string(rune(zwj)) + string(rune(0x1F469)) + string(rune(zwj)) + string(rune(0x1F467))
	got := SanitizeTTY(family)
	if want := string(rune(0x1F468)) + string(rune(0x1F469)) + string(rune(0x1F467)); got != want {
		t.Fatalf("SanitizeTTY(family emoji) = %q, want %q", got, want)
	}
	// Positive: the three base emoji all survive; only the joiners went.
	for _, r := range []rune{0x1F468, 0x1F469, 0x1F467} {
		if !strings.ContainsRune(got, r) {
			t.Errorf("base emoji U+%04X was dropped, not just the joiner: %q", r, got)
		}
	}
}

// TestCellTextFoldsLayoutControls covers the half SanitizeTTY deliberately does not:
// in a column, \n and \t are layout control, and a newline in a cell forges a table row
// (#169).
func TestCellTextFoldsLayoutControls(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"newline-folded", "worker-a\nfake-row", "worker-a fake-row"},
		{"tab-folded", "worker-a\tshifted", "worker-a shifted"},
		{"edge-whitespace-trimmed", "\n  name  \n", "name"},
		{"esc-and-newline-together", hostileClearScreen + "a\nb", "[2J[1;1Ha b"},
		{"printable-untouched", "日本語 title", "日本語 title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CellText(tc.in)
			if got != tc.want {
				t.Fatalf("CellText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\n\t") {
				t.Errorf("CellText(%q) left a layout control in the cell: %q", tc.in, got)
			}
		})
	}
}

// TestSanitizersAreIdempotent is what makes the boundary safe to add UNDER the call
// sites that already sanitize (cellText on worker names, token labels, actor cells).
// If it were not idempotent, this change would alter their shipped output.
func TestSanitizersAreIdempotent(t *testing.T) {
	for _, s := range []string{
		hostileClearScreen + "x", hostileSetTitle, hostileBidi,
		"a\tb\nc", "日本語", "plain",
	} {
		if once, twice := SanitizeTTY(s), SanitizeTTY(SanitizeTTY(s)); once != twice {
			t.Errorf("SanitizeTTY not idempotent on %q: %q vs %q", s, once, twice)
		}
		if once, twice := CellText(s), CellText(CellText(s)); once != twice {
			t.Errorf("CellText not idempotent on %q: %q vs %q", s, once, twice)
		}
	}
}

// TestTableSanitizesEveryCell drives the real render boundary rather than the helper,
// because the defect was never in the helper: it was that the render path did not call
// one. It asserts on CONTENT (the rendered table text), not on a count.
func TestTableSanitizesEveryCell(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, false, false, false, false)
	err := p.Table(
		[]string{"ID", "NAME"},
		[][]string{
			{"w1", "hostile" + hostileClearScreen},
			{"w2", "title" + hostileSetTitle},
			{"w3", "forged\nrow"},
			{"w4", "日本語 worker"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Negative: not one control byte escaped, on any row.
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("table output carries a raw ESC: %q", out)
	}
	if strings.ContainsRune(out, 0x07) {
		t.Errorf("table output carries a raw BEL: %q", out)
	}
	// Positive, and this is the one that proves the table still rendered: every id and
	// the printable remnant of every name is present, and the CJK cell is byte-exact.
	for _, want := range []string{"w1", "w2", "w3", "w4", "hostile", "title", "forged row", "日本語 worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output lost %q — the sanitizer ate more than the controls: %q", want, out)
		}
	}
	// The forged-row payload must not have produced an extra line. Header + 4 rows + the
	// trailing newline's empty split element.
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n != 5 {
		t.Errorf("table rendered %d lines, want 5 (header + 4 rows) — an embedded newline forged a row:\n%q", n, out)
	}
}

// TestTableHeadersKeepTheirOwnColourEscapes pins the ordering inside Table: headers are
// sanitized BEFORE the bold codes are added, so the Printer's own escapes survive while
// the caller's text cannot smuggle any.
func TestTableHeadersKeepTheirOwnColourEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	p := NewPrinter(&buf, true, false, false, false) // TTY ⇒ colour on
	if !p.Color {
		t.Fatal("colour must be on for this test to mean anything")
	}
	if err := p.Table([]string{"NAME" + hostileClearScreen}, [][]string{{"x"}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[1mNAME") {
		t.Errorf("the header lost the Printer's own bold escape: %q", out)
	}
	if strings.Contains(out, "\x1b[2J") {
		t.Errorf("a hostile header escape survived: %q", out)
	}
}

// TestPrintfAndPrintlnSanitizeStringArgs covers the non-table human path.
func TestPrintfAndPrintlnSanitizeStringArgs(t *testing.T) {
	t.Run("printf-arg", func(t *testing.T) {
		var buf bytes.Buffer
		NewPrinter(&buf, false, false, false, false).Printf("verdict: %s\n", "pass"+hostileClearScreen)
		out := buf.String()
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("Printf passed a raw ESC through: %q", out)
		}
		if !strings.Contains(out, "verdict: pass") {
			t.Errorf("Printf lost the legitimate text: %q", out)
		}
	})
	t.Run("printf-format", func(t *testing.T) {
		// The format string is a compiled-in literal at every call site today; sanitizing
		// it anyway closes the p.Printf(untrustedString) shape before someone writes it.
		var buf bytes.Buffer
		NewPrinter(&buf, false, false, false, false).Printf("note " + hostileSetTitle + " here\n")
		if out := buf.String(); strings.ContainsRune(out, 0x1b) || !strings.Contains(out, "note ") {
			t.Errorf("Printf format not sanitized, or over-sanitized: %q", out)
		}
	})
	t.Run("println-arg", func(t *testing.T) {
		var buf bytes.Buffer
		NewPrinter(&buf, false, false, false, false).Println("summary" + hostileBidi)
		out := buf.String()
		if strings.ContainsRune(out, 0x202e) {
			t.Errorf("Println passed a bidi override through: %q", out)
		}
		if !strings.Contains(out, "summarysafegnp.exe") {
			t.Errorf("Println lost the legitimate text: %q", out)
		}
	})
	t.Run("newlines-spared", func(t *testing.T) {
		// The deliberate difference from Table: renderReview prints multi-line judge
		// markdown through here, so folding newlines would collapse it.
		var buf bytes.Buffer
		NewPrinter(&buf, false, false, false, false).Println("line one\nline two")
		if out := buf.String(); out != "line one\nline two\n" {
			t.Errorf("Println folded a legitimate newline: %q", out)
		}
	})
	t.Run("non-string-args-untouched", func(t *testing.T) {
		var buf bytes.Buffer
		NewPrinter(&buf, false, false, false, false).Printf("%d %t %.2f\n", 42, true, 1.5)
		if out := buf.String(); out != "42 true 1.50\n" {
			t.Errorf("non-string operands were mangled: %q", out)
		}
	})
	t.Run("caller-slice-not-mutated", func(t *testing.T) {
		var buf bytes.Buffer
		args := []any{"a" + hostileClearScreen}
		NewPrinter(&buf, false, false, false, false).Println(args...)
		if args[0] != "a"+hostileClearScreen {
			t.Errorf("Println mutated the caller's slice: %q", args[0])
		}
	})
}

// TestJSONOutputIsNotSanitized is the proof the lead asked for: --json must be
// untouched. Two channels reach stdout in JSON mode and both are covered.
func TestJSONOutputIsNotSanitized(t *testing.T) {
	// Deliberately mixes a byte json.Marshal DOES escape (ESC) with two it does NOT
	// (U+202E, U+200D): an ESC-only fixture could never show the second pair being
	// silently stripped out of an agent's NDJSON. Rune-built, since both are invisible.
	payload := "run \x1b[2J title " + string(rune(0x202E)) + " reversed " + string(rune(0x200D)) + " joined"

	t.Run("json-document", func(t *testing.T) {
		var buf bytes.Buffer
		p := NewPrinter(&buf, false, true, false, false)
		if err := p.JSON(map[string]any{"title": payload}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		// Positive on the escaping: the ESC is present, encoded, not deleted.
		if !strings.Contains(out, "u001b") {
			t.Errorf("JSON did not carry the escaped ESC: %q", out)
		}
		// Negative: and never as a raw byte.
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("JSON emitted a RAW ESC: %q", out)
		}
		// The strongest positive available — a decode must return the server's exact
		// bytes. This is what "lossless" means and what a stripper would break.
		var back map[string]string
		if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
			t.Fatal(err)
		}
		if back["title"] != payload {
			t.Errorf("JSON round-trip is lossy: got %q, want %q", back["title"], payload)
		}
	})

	t.Run("ndjson-through-println", func(t *testing.T) {
		// renderMessage streams NDJSON through Println, not JSON(), for `run logs
		// --follow`. Sanitizing there would corrupt the agent contract.
		line, err := json.Marshal(map[string]string{"payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		NewPrinter(&buf, false, true, false, false).Println(string(line))
		if got := buf.String(); got != string(line)+"\n" {
			t.Errorf("Println altered an NDJSON line in --json mode:\n got %q\nwant %q", got, string(line)+"\n")
		}
		var back map[string]string
		if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
			t.Fatal(err)
		}
		if back["payload"] != payload {
			t.Errorf("NDJSON round-trip is lossy: got %q, want %q", back["payload"], payload)
		}
	})

	t.Run("human-mode-would-have-stripped-it", func(t *testing.T) {
		// The discriminator for the gate above. Same call, same bytes, table mode: this
		// asserts the gate is what spared the JSON, not that the payload was harmless.
		var buf bytes.Buffer
		NewPrinter(&buf, false, false, false, false).Println(payload)
		out := buf.String()
		if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x202e) || strings.ContainsRune(out, 0x200d) {
			t.Fatalf("human mode did not strip the payload, so the JSON test proves nothing: %q", out)
		}
		if !strings.Contains(out, "run [2J title  reversed  joined") {
			t.Errorf("human mode lost the legitimate text: %q", out)
		}
	})
}
