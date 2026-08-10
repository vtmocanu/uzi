package slacksvc

import (
	"strings"
	"testing"
)

// TestSlackMrkdwn_Conversions covers the happy-path CommonMark -> Slack mrkdwn
// conversions (PRD #292 conversion table).
func TestSlackMrkdwn_Conversions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strong_stars", "**bold**", "*bold*"},
		{"strong_unders", "__bold__", "*bold*"},
		{"italic_stars", "*italic*", "_italic_"},
		{"italic_unders", "_italic_", "_italic_"},
		{"heading_h1", "# Heading", "*Heading*"},
		{"heading_h6", "###### Deep", "*Deep*"},
		{"bullet_dash", "- a\n- b", "• a\n• b"},
		{"bullet_star", "* a\n* b", "• a\n• b"},
		{"bullet_plus", "+ a\n+ b", "• a\n• b"},
		{"ordered_kept", "1. a\n2. b", "1. a\n2. b"},
		{"strike", "~~s~~", "~s~"},
		{"link_https", "[label](https://x.com)", "<https://x.com|label>"},
		{"link_https_upper_scheme", "[ok](HTTPS://Good.com/p)", "<HTTPS://Good.com/p|ok>"},
		{"nested_emphasis", "**a _b_ c**", "*a _b_ c*"},
		{"empty", "", ""},
		{"whitespace_only", "   \n\t  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SlackMrkdwn(tc.in); got != tc.want {
				t.Fatalf("SlackMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlackMrkdwn_Injection is the adversarial gate (PRD #292 safety contract).
// Each case asserts that hostile input cannot produce a live mention, a live or
// mis-targeted link, or unescaped markup.
func TestSlackMrkdwn_Injection(t *testing.T) {
	t.Run("mention_escaped_not_live", func(t *testing.T) {
		got := SlackMrkdwn("<@U123>")
		if got != "&lt;@U123&gt;" {
			t.Fatalf("got %q, want escaped mention", got)
		}
		if strings.Contains(got, "<@U123>") {
			t.Fatalf("live mention survived: %q", got)
		}
	})

	t.Run("injected_link_markup_inert", func(t *testing.T) {
		got := SlackMrkdwn("<https://evil|Open>")
		// Must not re-emit Slack's <url|label> grammar.
		if strings.Contains(got, "<https://evil|Open>") || strings.HasPrefix(got, "<") {
			t.Fatalf("injected link became live: %q", got)
		}
	})

	t.Run("url_pipe_and_angle_sanitized", func(t *testing.T) {
		got := SlackMrkdwn("[a](https://x/?q=|<@U0>)")
		// The dangerous chars inside the URL must be percent-encoded, so no raw
		// "|<@U0>" survives inside the emitted <...>.
		if strings.Contains(got, "|<@U0>") {
			t.Fatalf("raw pipe/mention survived in URL: %q", got)
		}
		if !strings.HasPrefix(got, "<https://x/?q=") || !strings.HasSuffix(got, "|a>") {
			t.Fatalf("unexpected sanitized link shape: %q", got)
		}
		if strings.ContainsAny(strings.TrimSuffix(strings.TrimPrefix(got, "<"), "|a>"), "<>") {
			t.Fatalf("unencoded angle bracket left in URL: %q", got)
		}
	})

	t.Run("label_angle_and_pipe_sanitized", func(t *testing.T) {
		got := SlackMrkdwn("[la>b|c](https://ok.com)")
		// The label's ">" is &-escaped and its "|" is neutralised, so neither can
		// terminate or split the <url|label> grammar.
		if strings.Contains(got, "la>b") || strings.Contains(got, "b|c") {
			t.Fatalf("label not sanitized: %q", got)
		}
		if got != "<https://ok.com|la&gt;bc>" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("non_https_http_degrades", func(t *testing.T) {
		got := SlackMrkdwn("[x](http://e)")
		if got != "x" {
			t.Fatalf("http link should degrade to escaped text, got %q", got)
		}
	})

	t.Run("non_https_javascript_degrades", func(t *testing.T) {
		got := SlackMrkdwn("[x](javascript:alert(1))")
		if got != "x" || strings.Contains(got, "<") {
			t.Fatalf("javascript link should degrade to escaped text, got %q", got)
		}
	})

	t.Run("redacted_placeholder_not_a_link", func(t *testing.T) {
		// A ScrubSecrets-style "[REDACTED](http://x)" must not synthesize a live
		// link (non-https anyway -> escaped text).
		got := SlackMrkdwn("[REDACTED](http://x)")
		if got != "REDACTED" || strings.Contains(got, "<") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("code_span_keeps_markup_literal", func(t *testing.T) {
		got := SlackMrkdwn("`**x**`")
		if got != "`**x**`" {
			t.Fatalf("markup inside code span should stay literal, got %q", got)
		}
	})

	t.Run("code_span_inner_backtick_cannot_close_span", func(t *testing.T) {
		// A backtick INSIDE the code content must not close our single-backtick Slack
		// span early; if it did, the trailing "**bold**" would render as live markup.
		// escapeCodeContent flanks the inner backtick with a zero-width space so it is
		// neutralised. Input: a ``-delimited CommonMark code span whose content is
		// literally "code`**bold**".
		got := SlackMrkdwn("``code`**bold**``")
		const zws = "\u200b"
		want := "`code" + zws + "`" + zws + "**bold**`"
		if got != want {
			t.Fatalf("inner backtick not neutralised: got %q, want %q", got, want)
		}
		// The defect signature: a bare backtick immediately abutting "**bold**" means
		// the span closed early and the bold went live. It must NOT appear.
		if strings.Contains(got, "`**bold**") {
			t.Fatalf("inner backtick still abuts **bold**, span closes early (bold live): %q", got)
		}
		// "**bold**" must survive as literal double-star text inside the span, never
		// reduced to a live single-star "*bold*" emphasis.
		if !strings.Contains(got, "**bold**") {
			t.Fatalf("literal **bold** not preserved inside code span: %q", got)
		}
	})

	t.Run("raw_angle_amp_escaped", func(t *testing.T) {
		got := SlackMrkdwn("a < b & c > d")
		if strings.ContainsAny(got, "<>") || strings.Contains(got, " & ") {
			t.Fatalf("raw < > & not escaped: %q", got)
		}
	})
}

// TestSlackMrkdwn_CodeBlocks pins fenced/indented code handling: content is
// &<>-escaped and the block is always balanced, even when the input fence is
// unclosed (pre-truncated input).
func TestSlackMrkdwn_CodeBlocks(t *testing.T) {
	t.Run("fenced_escaped_and_balanced", func(t *testing.T) {
		got := SlackMrkdwn("```\n<script>&\n```")
		if strings.Contains(got, "<script>") || !strings.Contains(got, "&lt;script&gt;&amp;") {
			t.Fatalf("fenced content not escaped: %q", got)
		}
		if strings.Count(got, "```") != 2 {
			t.Fatalf("fences not balanced: %q", got)
		}
	})

	t.Run("unclosed_fence_gets_closing", func(t *testing.T) {
		got := SlackMrkdwn("```\ncode")
		if strings.Count(got, "```") != 2 {
			t.Fatalf("unclosed fence should still be closed: %q", got)
		}
	})

	t.Run("embedded_fence_in_content_stays_balanced", func(t *testing.T) {
		// An indented code block whose CONTENT contains a ``` line must not close our
		// fenced block early: escapeCodeContent neutralises the embedded run so the
		// only ``` runs in the output are the open/close pair we emit, and the trailing
		// "def" stays inside the block rather than escaping to live text.
		got := SlackMrkdwn("\tabc\n\t```\n\tdef")
		fences := strings.Count(got, "```")
		if fences%2 != 0 {
			t.Fatalf("fence count %d is odd (unbalanced): %q", fences, got)
		}
		if fences != 2 {
			t.Fatalf("expected exactly the open/close fence pair, got %d fences: %q", fences, got)
		}
		if !strings.HasPrefix(got, "```\n") || !strings.HasSuffix(got, "\n```") {
			t.Fatalf("output is not a single wrapped code block: %q", got)
		}
		// "def" must sit BEFORE the closing fence, i.e. inside the block.
		idxDef := strings.Index(got, "def")
		idxClose := strings.LastIndex(got, "```")
		if idxDef < 0 || idxDef >= idxClose {
			t.Fatalf("def escaped the code block: %q", got)
		}
	})
}

// TestSlackMrkdwn_Malformed pins that unclosed/half constructs degrade to literal
// escaped text (goldmark does this for free), never a half-open link or dangling
// marker (PRD #292 safety contract §5-6).
func TestSlackMrkdwn_Malformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unbalanced_strong", "**unbalanced", "**unbalanced"},
		{"unterminated_link", "[l](htt", "[l](htt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SlackMrkdwn(tc.in)
			if got != tc.want {
				t.Fatalf("SlackMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "<") {
				t.Fatalf("malformed input produced a half-open token: %q", got)
			}
		})
	}
}

// TestSlackMrkdwn_TableAndImageDegrade pins that constructs with no Slack
// equivalent degrade to escaped plain text (no live markup).
func TestSlackMrkdwn_TableAndImageDegrade(t *testing.T) {
	t.Run("table_to_text", func(t *testing.T) {
		// A table degrades to its escaped cell text; the important property is that
		// it emits no live markup (no unescaped < or >).
		got := SlackMrkdwn("| h1 | h2 |\n|---|---|\n| a<b | b |")
		if strings.ContainsAny(got, "<>") {
			t.Fatalf("table degradation left markup: %q", got)
		}
		if !strings.Contains(got, "h1") || !strings.Contains(got, "h2") {
			t.Fatalf("table cell text lost: %q", got)
		}
	})

	t.Run("image_alt_escaped", func(t *testing.T) {
		got := SlackMrkdwn("![<alt>](https://x/y.png)")
		if strings.Contains(got, "<alt>") {
			t.Fatalf("image alt not escaped: %q", got)
		}
	})
}

// TestTruncateForSlackSection_LinkSafe pins Task 2: a cap landing mid-<url|label>
// must not leave a trailing unbalanced "<" (which would re-open the injection).
func TestTruncateForSlackSection_LinkSafe(t *testing.T) {
	t.Run("cap_mid_link_drops_open_angle", func(t *testing.T) {
		// 2895 filler runes, then a link so the rune slice at 2900 lands after the
		// "<" but before its ">".
		in := strings.Repeat("a", maxSlackSectionRunes-5) + "<https://example.com/x|label>"
		got := truncateForSlackSection(in)
		if !strings.HasSuffix(got, "\n…") {
			t.Fatalf("expected truncation ellipsis, got %q", got[len(got)-10:])
		}
		core := strings.TrimSuffix(got, "\n…")
		if i := strings.LastIndexByte(core, '<'); i >= 0 && !strings.ContainsRune(core[i:], '>') {
			t.Fatalf("truncation left an unbalanced '<': %q", core[i:])
		}
	})

	t.Run("balanced_link_before_cap_preserved", func(t *testing.T) {
		// A complete <...> well before the cap keeps its closing ">".
		link := "<https://ok.com|hi>"
		in := link + strings.Repeat("b", maxSlackSectionRunes+50)
		got := truncateForSlackSection(in)
		if !strings.Contains(got, link) {
			t.Fatalf("balanced link before cap was dropped: %q", got[:40])
		}
	})

	t.Run("no_truncation_unchanged", func(t *testing.T) {
		in := "short <https://ok.com|hi> body"
		if got := truncateForSlackSection(in); got != in {
			t.Fatalf("short input changed: %q", got)
		}
	})
}
