package slacksvc

import (
	"fmt"
	"strings"

	"github.com/slack-go/slack/slackutilsx"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// mrkdwnParser is the goldmark parser SlackMrkdwn walks. ONLY Strikethrough and
// Table are enabled — deliberately NOT extension.GFM, which also bundles Linkify
// (would autolink bare URLs into live <url> markup) and TaskList (adds checkbox
// nodes). Strikethrough gives us ~~x~~ → ~x~; Table lets us recognise and degrade
// a table to escaped plain text rather than mis-parsing its rows as paragraphs.
// The parser is stateless and safe to share across goroutines (goldmark builds a
// fresh AST per Parse call), so it is built once at package init.
var mrkdwnParser = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Table),
).Parser()

// SlackMrkdwn converts an UNTRUSTED CommonMark body (a model- or forge-authored
// blob) into Slack's mrkdwn dialect, injection-safely. It subsumes EscapeMrkdwn's
// &<>-escaping for these whole-blob bodies: it is applied INSTEAD of EscapeMrkdwn
// at the three untrusted model-body render sites (PRD #292 Decision 2), never in
// addition to it (double-escaping would corrupt an emitted <url|label>).
//
// The safety contract (PRD #292): every Text/RawHTML/HTMLBlock/code node's content
// is &<>-escaped via the SAME slackutilsx.EscapeMessage that EscapeMrkdwn wraps, so
// the two escaping notions cannot drift. A <url|label> is emitted ONLY from a Link
// whose (lowercased) scheme is exactly https, and both its URL and label are
// sanitised so a destination or label carrying <, >, | or whitespace cannot re-open
// Slack's <url|label> grammar. goldmark parses an injected <@U123> or
// <https://evil|Open> as literal text / autolink, NOT as a Link, so escaping those
// node kinds neutralises the mention/link. Malformed or pre-truncated input
// (unclosed **, unterminated [l](htt) degrades to literal escaped text because the
// parser degrades unclosed constructs for free.
//
// Empty or whitespace-only input returns "".
func SlackMrkdwn(s string) string {
	out, _ := SlackMrkdwnBlock(s)
	return out
}

// SlackMrkdwnBlock is SlackMrkdwn plus a signal reporting whether the render emitted
// a fenced/indented code block. A caller that blockquotes the body line-by-line needs
// this: Slack's per-line "> " prefix injected into a fenced block's interior lines
// breaks code rendering, so such a body must be emitted as a plain section instead
// (PRD #292 Decision 6). The signal comes from the AST walk (a real CodeBlock/
// FencedCodeBlock node was rendered), NOT from scanning the output for a ``` run — a
// literal ``` in PROSE is a Text node, escaped-but-not-fenced, and must not be
// mistaken for a code fence.
func SlackMrkdwnBlock(s string) (rendered string, hasCodeBlock bool) {
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	src := []byte(s)
	w := &mrkdwnWalker{src: src, atLineStart: true}
	doc := mrkdwnParser.Parse(text.NewReader(src))
	_ = ast.Walk(doc, w.visit)
	// The walk leaves the builder with a trailing newline after the final block;
	// trim it (and any trailing quote padding) so single-line inputs round-trip
	// cleanly (e.g. "# H" -> "*H*", not "*H*\n").
	return strings.TrimRight(w.sb.String(), "\n "), w.sawCodeBlock
}

// escapeMrkdwnText is the single &<>-escaping primitive the walker routes ALL
// untrusted text and code content through — the exact function EscapeMrkdwn wraps, so
// the two notions of "mrkdwn-escaped" cannot drift (PRD #292 safety contract §4).
func escapeMrkdwnText(s string) string { return slackutilsx.EscapeMessage(s) }

// zeroWidthSpace (U+200B) is inserted around each backtick in code content by
// escapeCodeContent to neutralise it as a Slack code delimiter. Slack renders it
// invisibly, so the backtick still shows, but it can no longer close our code span
// or fenced block.
const zeroWidthSpace = "\u200b"

// escapeCodeContent is the ONE shared place code content (both the code-span path and
// the emitCodeBlock fenced/indented path) is escaped and emitted. It &<>-escapes via
// escapeMrkdwnText (so the "cannot drift" invariant of §4 still holds) and then
// neutralises every backtick in the content by flanking it with a zero-width space.
//
// This is load-bearing for the safety contract: Slack's code delimiters are FIXED
// (single ` for spans, ``` for fenced blocks) and cannot be widened, so a backtick
// run inside the content would otherwise close our code context early — a lone
// backtick closes a span, a ``` run closes a fenced block — letting the trailing
// content render as live *_~[]<> markup and leaving the fence output unbalanced.
// Flanking each backtick with a zero-width space means no content backtick is
// adjacent to another backtick or to our own delimiter, so no content run can form or
// act as a closing delimiter, for ANY input.
func escapeCodeContent(s string) string {
	return strings.ReplaceAll(escapeMrkdwnText(s), "`", zeroWidthSpace+"`"+zeroWidthSpace)
}

// mrkdwnWalker accumulates the rendered output. atLineStart + quoteDepth implement
// blockquote line prefixing: when inside a blockquote, the first content written on
// each fresh line is prefixed with "> " so Slack renders it as a quote.
type mrkdwnWalker struct {
	src         []byte
	sb          strings.Builder
	quoteDepth  int
	atLineStart bool
	// sawCodeBlock records that a fenced/indented code block was rendered, so a caller
	// (notificationBlocks) can decide NOT to blockquote a body whose fence the per-line
	// "> " prefix would corrupt (PRD #292 Decision 6).
	sawCodeBlock bool
}

// out writes a chunk that contains no newline, emitting the blockquote prefix first
// when this is the start of a line inside a blockquote.
func (w *mrkdwnWalker) out(s string) {
	if s == "" {
		return
	}
	if w.atLineStart && w.quoteDepth > 0 {
		w.sb.WriteString(strings.Repeat("> ", w.quoteDepth))
	}
	w.sb.WriteString(s)
	w.atLineStart = false
}

// nl ends the current line.
func (w *mrkdwnWalker) nl() {
	w.sb.WriteByte('\n')
	w.atLineStart = true
}

// visit is the ast.Walk callback. Container nodes that map to paired markers
// (Emphasis, Strikethrough) emit on BOTH entering and leaving; opaque nodes (Text,
// code, links, images, raw HTML, tables) are rendered fully on entering and skip
// their children so nested source markup is never re-interpreted.
func (w *mrkdwnWalker) visit(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch node := n.(type) {
	case *ast.Document:
		return ast.WalkContinue, nil

	case *ast.Paragraph:
		if !entering {
			w.nl()
		}
		return ast.WalkContinue, nil

	case *ast.TextBlock: // tight list-item / loose content without blank-line semantics
		if !entering {
			w.nl()
		}
		return ast.WalkContinue, nil

	case *ast.Heading:
		// Slack has no heading syntax; render the heading text as a bold line.
		w.out("*")
		if !entering {
			w.nl()
		}
		return ast.WalkContinue, nil

	case *ast.Blockquote:
		if entering {
			w.quoteDepth++
		} else {
			w.quoteDepth--
		}
		return ast.WalkContinue, nil

	case *ast.List:
		return ast.WalkContinue, nil

	case *ast.ListItem:
		if entering {
			w.out(w.itemMarker(node))
		}
		return ast.WalkContinue, nil

	case *ast.Text:
		if entering {
			w.out(escapeMrkdwnText(string(node.Value(w.src))))
			// Value() strips line breaks; re-emit them so multi-line structure
			// survives (load-bearing for a later milestone's blockquote rendering).
			if node.SoftLineBreak() || node.HardLineBreak() {
				w.nl()
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.String:
		if entering {
			w.out(escapeMrkdwnText(string(node.Value)))
		}
		return ast.WalkSkipChildren, nil

	case *ast.CodeSpan:
		// Opaque: the content passes through escapeCodeContent (&<>-escaped AND its
		// backticks neutralised), so `**x**` in a code span stays literal and an inner
		// backtick cannot close our single-backtick span early. Slack renders
		// single-backtick code spans.
		if entering {
			w.out("`" + escapeCodeContent(nodeText(node, w.src)) + "`")
		}
		return ast.WalkSkipChildren, nil

	case *ast.Emphasis:
		// Level 1 -> italic (_x_); Level >= 2 -> strong/bold (*x*). Emitted on both
		// entering and leaving to wrap the children.
		if node.Level >= 2 {
			w.out("*")
		} else {
			w.out("_")
		}
		return ast.WalkContinue, nil

	case *ast.Link:
		if entering {
			w.emitLink(node)
		}
		return ast.WalkSkipChildren, nil

	case *ast.AutoLink:
		// A bare autolink is NOT a trusted <url|label>; render its text inert and
		// escaped (no surrounding <>), so <https://evil|Open> cannot become a link.
		if entering {
			w.out(escapeMrkdwnText(string(node.Label(w.src))))
		}
		return ast.WalkSkipChildren, nil

	case *ast.Image:
		// No Slack equivalent: degrade to the escaped alt text.
		if entering {
			w.out(escapeMrkdwnText(nodeText(node, w.src)))
		}
		return ast.WalkSkipChildren, nil

	case *ast.RawHTML:
		if entering {
			w.out(escapeMrkdwnText(segmentsText(node.Segments, w.src)))
		}
		return ast.WalkSkipChildren, nil

	case *ast.HTMLBlock:
		if entering {
			w.out(escapeMrkdwnText(string(node.Lines().Value(w.src))))
			if node.HasClosure() {
				w.out(escapeMrkdwnText(string(node.ClosureLine.Value(w.src))))
			}
			w.nl()
		}
		return ast.WalkSkipChildren, nil

	case *ast.FencedCodeBlock:
		if entering {
			w.sawCodeBlock = true
			w.emitCodeBlock(node)
		}
		return ast.WalkSkipChildren, nil

	case *ast.CodeBlock:
		if entering {
			w.sawCodeBlock = true
			w.emitCodeBlock(node)
		}
		return ast.WalkSkipChildren, nil
	}

	// Extension nodes are matched by Kind (their concrete types live in
	// extension/ast, which we import as east).
	switch n.Kind() {
	case east.KindStrikethrough:
		w.out("~")
		return ast.WalkContinue, nil
	case east.KindTable:
		// No Slack equivalent: degrade the whole table to escaped plain text.
		if entering {
			w.out(escapeMrkdwnText(nodeText(n, w.src)))
			w.nl()
		}
		return ast.WalkSkipChildren, nil
	}

	return ast.WalkContinue, nil
}

// itemMarker returns the bullet ("• ") or ordered ("N. ") prefix for a list item.
func (w *mrkdwnWalker) itemMarker(li *ast.ListItem) string {
	list, ok := li.Parent().(*ast.List)
	if !ok || !list.IsOrdered() {
		return "• "
	}
	n := list.Start
	if n == 0 {
		n = 1
	}
	for prev := li.PreviousSibling(); prev != nil; prev = prev.PreviousSibling() {
		if _, ok := prev.(*ast.ListItem); ok {
			n++
		}
	}
	return fmt.Sprintf("%d. ", n)
}

// emitCodeBlock renders a fenced or indented code block as a Slack triple-backtick
// block. The content passes through escapeCodeContent (&<>-escaped AND its backticks
// neutralised); a CLOSING fence is ALWAYS emitted, even when the input's fence was
// unclosed (pre-truncated input). The fences we emit are GUARANTEED balanced: because
// escapeCodeContent flanks every content backtick with a zero-width space, a ``` run
// embedded in the content can no longer form a closing fence, so the only ``` runs in
// our output are the open/close pair we write (PRD #292 safety contract §5).
func (w *mrkdwnWalker) emitCodeBlock(n ast.Node) {
	lines := n.Lines()
	var content strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		content.Write(seg.Value(w.src))
	}
	esc := escapeCodeContent(strings.TrimSuffix(content.String(), "\n"))
	w.out("```")
	w.nl()
	for _, line := range strings.Split(esc, "\n") {
		w.out(line)
		w.nl()
	}
	w.out("```")
	w.nl()
}

// emitLink renders a Link. It emits a <url|label> ONLY when the destination's
// (lowercased) scheme is exactly https AND the sanitised URL is non-empty;
// otherwise it degrades to the escaped label text with no link (PRD #292 safety
// contract §2-3).
func (w *mrkdwnWalker) emitLink(n *ast.Link) {
	// Label: flatten to text, escape &<>, and neutralise | so it cannot split the
	// <url|label> grammar.
	label := strings.ReplaceAll(escapeMrkdwnText(nodeText(n, w.src)), "|", "")
	url, ok := sanitizeHTTPSURL(string(n.Destination))
	if !ok {
		w.out(label)
		return
	}
	w.out("<" + url + "|" + label + ">")
}

// sanitizeHTTPSURL validates that dest is an https URL (scheme compared
// case-insensitively, so HTTPS://x is accepted) and percent-encodes the characters
// that would break out of Slack's <url|label> grammar (< > | and whitespace/control
// bytes). It returns ok=false for a non-https or empty result, in which case the
// caller degrades to escaped label text.
//
// The scheme is extracted by hand (not net/url) precisely BECAUSE a hostile
// destination may contain < > |: those make net/url.Parse reject strings we still
// want to accept-and-sanitise, and a bare-string scheme check is enough to enforce
// the https-only rule (aligning with isHTTPSURL, notifier_state.go, which guards trusted
// forge URLs and is intentionally left case-sensitive there).
func sanitizeHTTPSURL(dest string) (string, bool) {
	if !hasHTTPSScheme(dest) {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(dest); i++ {
		c := dest[i]
		switch {
		case c == '<':
			b.WriteString("%3C")
		case c == '>':
			b.WriteString("%3E")
		case c == '|':
			b.WriteString("%7C")
		case c == ' ' || c <= 0x1f || c == 0x7f:
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	out := b.String()
	if out == "" {
		return "", false
	}
	return out, true
}

// hasHTTPSScheme reports whether dest begins with a well-formed scheme that
// lowercases to "https". Scheme grammar: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ).
func hasHTTPSScheme(dest string) bool {
	i := strings.IndexByte(dest, ':')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := dest[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '+', c == '-', c == '.':
		default:
			return false
		}
	}
	return strings.EqualFold(dest[:i], "https")
}

// nodeText flattens all descendant text (Text/String nodes) of n into a single
// unescaped string. Used for link labels, image alt text, code spans, and table
// degradation; callers escape the result exactly once.
func nodeText(n ast.Node, src []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Value(src))
		case *ast.String:
			b.Write(c.Value)
		default:
			b.WriteString(nodeText(c, src))
		}
	}
	return b.String()
}

// segmentsText concatenates a RawHTML node's source segments.
func segmentsText(segs *text.Segments, src []byte) string {
	if segs == nil {
		return ""
	}
	return string(segs.Value(src))
}
