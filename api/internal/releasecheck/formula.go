package releasecheck

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Port of scripts/brew-tap-guard.py. Formula source is read as text, never executed.
//
// Like the script's read_text(encoding="utf-8"), parseFormulaTag refuses invalid
// UTF-8 and applies universal newlines (\r\n and a lone \r both end a line) before
// matching, so a url line after a lone \r is counted as a line of its own.
//
// Deliberate divergence: Python's \s (str patterns) also matches \v, \x1c-\x1f,
// \x85 and Unicode spaces, while RE2's \s is only [\t\n\f\r ]. spaceClass widens
// the Go class to Python's so a line that Python counts as a url/version line is
// counted here too (otherwise a second, oddly-indented url line could slip past).
// RE2's \b is ASCII-only, so e.g. "urlé" counts as a url line here but not in
// Python; that only makes this parser reject more, never accept more.
const spaceClass = `[\s\x0b\x1c-\x1f\x{85}\p{Z}]`

var (
	formulaURLRe  = regexp.MustCompile(`(?m)^` + spaceClass + `*url` + spaceClass + `+"https://github\.com/vtmocanu/uzi/archive/refs/tags/([^"/]+)\.tar\.gz"` + spaceClass + `*$`)
	formulaAnyURL = regexp.MustCompile(`(?m)^` + spaceClass + `*url\b`)
	formulaVerRe  = regexp.MustCompile(`(?m)^` + spaceClass + `*version(?:_scheme)?\b`)
	formulaTagRe  = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-rc\.[1-9][0-9]*)?$`)
)

// parseFormulaTag returns the uzi tag a tap formula installs, or ("", false) when the
// layout is ambiguous or the tag is not a strict stable or RC uzi tag. stableOnly
// (the uzi-cli formula) additionally rejects an RC tag.
func parseFormulaTag(src string, stableOnly bool) (string, bool) {
	if !utf8.ValidString(src) {
		return "", false
	}
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	urls := formulaURLRe.FindAllStringSubmatch(src, -1)
	if len(urls) != 1 ||
		len(formulaAnyURL.FindAllStringIndex(src, -1)) != 1 ||
		formulaVerRe.MatchString(src) {
		return "", false
	}
	tag := urls[0][1]
	if !formulaTagRe.MatchString(tag) {
		return "", false
	}
	if stableOnly && strings.Contains(tag, "-rc.") {
		return "", false
	}
	return tag, true
}
