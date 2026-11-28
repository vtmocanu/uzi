package store

import (
	"fmt"
	"strings"
	"testing"
)

// lexSQL is the one left-to-right lexer the run_usage writer guard parses SQL with. It returns
// two strings, each the same length as s:
//
//   - norm: comments blanked (one space per byte, newlines kept), everything else verbatim, quoted
//     text included, so a quoted target such as public."run_usage" can still be read.
//   - mask: norm with the interior of every string and quoted identifier replaced by 'x' (the quote
//     delimiters stay), so structural parsing (statement splitting, parentheses, keywords) never
//     sees text inside a quote. The body of a dollar quote is lexed recursively and carries that
//     body's own mask, so comments inside a plpgsql body are stripped and its code stays visible.
//
// Rules, outside any quote:
//   - "--" runs to the newline (\n or \r); "/* */" nests; an unterminated block comment is an error.
//   - '...' uses a doubled single quote for a quote and treats backslash literally; E'...' / e'...' (opening only when
//     the byte before the E is not an identifier byte) also lets a backslash escape the next byte.
//     A string continues across whitespace containing a newline, 'a'<newline>'b', in the same mode.
//   - "..." uses a doubled double quote for a quote.
//   - A dollar quote ($$ or $tag$, tag = letter/_/high-byte then word bytes) opens only when the
//     previous byte is not an identifier byte, so $1 stays a parameter and foo$bar$ an identifier.
//     The closing tag is found by plain text search, then the body is lexed on its own.
//   - An unterminated string, quoted identifier or dollar quote keeps the rest of the text (or of
//     that dollar body) verbatim, without an error.
//
// Not handled (recorded limits of the guard built on this lexer): dynamic SQL assembled and run
// through EXECUTE format(...); COPY run_usage FROM; U&"..." / U&'...' escapes; function bodies
// defined at or below migration 00549, which the migration scan skips; and TRUNCATE ... CASCADE,
// which empties run_usage through its foreign key (migration 00062) without naming it.
func lexSQL(s string) (norm, mask string, err error) {
	n, m := []byte(s), []byte(s)
	if err := lexSQLInto(s, n, m); err != nil {
		return "", "", err
	}
	return string(n), string(m), nil
}

func isSQLIdentByte(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isSQLTagStart(b byte) bool {
	return b == '_' || b >= 0x80 || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// sqlDollarTag returns the dollar-quote tag s opens with ("$$" or "$tag$"), or "".
func sqlDollarTag(s string) string {
	if len(s) < 2 {
		return ""
	}
	if s[1] == '$' {
		return "$$"
	}
	if !isSQLTagStart(s[1]) {
		return ""
	}
	j := 2
	for j < len(s) && (isSQLTagStart(s[j]) || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[:j+1]
	}
	return ""
}

// sqlQuotedEnd returns the index of the quote closing the one at src[i], or -1 when unterminated.
func sqlQuotedEnd(src string, i int, q byte, backslash bool) int {
	for j := i + 1; j < len(src); j++ {
		switch {
		case backslash && src[j] == '\\':
			j++
		case src[j] == q:
			if j+1 < len(src) && src[j+1] == q {
				j++
				continue
			}
			return j
		}
	}
	return -1
}

func lexSQLInto(src string, n, m []byte) error {
	blank := func(a, b int) {
		for k := a; k < b; k++ {
			if src[k] != '\n' && src[k] != '\r' {
				n[k], m[k] = ' ', ' '
			}
		}
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			end := strings.IndexAny(src[i:], "\r\n")
			if end < 0 {
				end = len(src)
			} else {
				end += i
			}
			blank(i, end)
			i = end
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			depth, j := 1, i+2
			for j < len(src) && depth > 0 {
				switch {
				case src[j] == '/' && j+1 < len(src) && src[j+1] == '*':
					depth++
					j += 2
				case src[j] == '*' && j+1 < len(src) && src[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			if depth > 0 {
				return fmt.Errorf("unterminated /* comment starting at byte %d", i)
			}
			blank(i, j)
			i = j
		case c == '\'' || c == '"':
			esc := c == '\'' && i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i == 1 || !isSQLIdentByte(src[i-2]))
			end := sqlQuotedEnd(src, i, c, esc)
			if end < 0 {
				return nil
			}
			for k := i + 1; k < end; k++ {
				m[k] = 'x'
			}
			i = end + 1
			// A string continues across whitespace holding a newline, as 'a'<newline>'b', in the
			// same escape mode as the segment before it.
			for c == '\'' {
				j, nl := i, false
				for j < len(src) && strings.IndexByte(" \t\r\n\f", src[j]) >= 0 {
					nl = nl || src[j] == '\n' || src[j] == '\r'
					j++
				}
				if !nl || j >= len(src) || src[j] != '\'' {
					break
				}
				next := sqlQuotedEnd(src, j, '\'', esc)
				if next < 0 {
					return nil
				}
				for k := j + 1; k < next; k++ {
					m[k] = 'x'
				}
				i = next + 1
			}
		case c == '$' && (i == 0 || !isSQLIdentByte(src[i-1])):
			tag := sqlDollarTag(src[i:])
			if tag == "" {
				i++
				continue
			}
			bodyStart := i + len(tag)
			k := strings.Index(src[bodyStart:], tag)
			if k < 0 {
				return nil
			}
			bodyEnd := bodyStart + k
			if err := lexSQLInto(src[bodyStart:bodyEnd], n[bodyStart:bodyEnd], m[bodyStart:bodyEnd]); err != nil {
				return err
			}
			i = bodyEnd + len(tag)
		default:
			i++
		}
	}
	return nil
}

func TestLexSQL(t *testing.T) {
	type tc struct{ in, norm, mask string }
	cases := map[string]tc{
		"line comment":        {"a -- b 'c\nd", "a        \nd", "a        \nd"},
		"block comment":       {"a /* b */ c", "a         c", "a         c"},
		"nested comment":      {"a /* b /* c */ d */ e", "a                   e", "a                   e"},
		"string":              {"a 'b/*' c", "a 'b/*' c", "a 'xxx' c"},
		"doubled quote":       {"'a''/*b'", "'a''/*b'", "'xxxxxx'"},
		"backslash literal":   {`'a\' /* c */`, `'a\'        `, `'xx'        `},
		"escape string":       {`E'a\'/*' x`, `E'a\'/*' x`, `E'xxxxx' x`},
		"lower escape":        {`e'\\' /**/`, `e'\\'     `, `e'xx'     `},
		"E in identifier":     {`fooE'a\' /**/`, `fooE'a\'     `, `fooE'xx'     `},
		"quoted ident":        {`"a/*""b" c`, `"a/*""b" c`, `"xxxxxx" c`},
		"param":               {"$1 /* x */", "$1        ", "$1        "},
		"identifier dollar":   {"foo$bar$ 'a'", "foo$bar$ 'a'", "foo$bar$ 'x'"},
		"dollar body":         {"$$ 'a' -- c\nz $$", "$$ 'a'     \nz $$", "$$ 'x'     \nz $$"},
		"tagged dollar":       {"$t$ a /* c */ $t$ b", "$t$ a         $t$ b", "$t$ a         $t$ b"},
		"inner other tag":     {"$t$ a $$ /* c */ $t$ b", "$t$ a $$ /* c */ $t$ b", "$t$ a $$ /* c */ $t$ b"},
		"string continuation": {"E'a'\n'\\' /* ';\nx;\n' */ '", "E'a'\n'\\' /* ';\nx;\n' */ '", "E'x'\n'xxxxxx';\nx;\n'xxxx'"},
		"no continuation":     {"'a' 'b' /* c */", "'a' 'b'        ", "'x' 'x'        "},
		"CR ends comment":     {"a -- b\rc", "a     \rc", "a     \rc"},
		"unterminated str":    {"a 'b /* c", "a 'b /* c", "a 'b /* c"},
		"unterminated $":      {"a $$ b /* c", "a $$ b /* c", "a $$ b /* c"},
		"body unterminated":   {"$$ 'a /* c */ $$ d /* e */", "$$ 'a /* c */ $$ d        ", "$$ 'a /* c */ $$ d        "},
	}
	for name, c := range cases {
		norm, mask, err := lexSQL(c.in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if norm != c.norm || mask != c.mask {
			t.Errorf("%s: lexSQL(%q)\n got norm %q mask %q\nwant norm %q mask %q", name, c.in, norm, mask, c.norm, c.mask)
		}
	}
	for _, in := range []string{"a /* b", "a /* b /* c */", "$$ /* x $$", "SELECT 1; /* unterminated"} {
		if _, _, err := lexSQL(in); err == nil {
			t.Errorf("lexSQL(%q) returned no error for an unterminated comment", in)
		}
	}
}
