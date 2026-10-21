package workersvc

import (
	"regexp"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/prdpath"
)

// FindSemanticPrdPaths is the Go mirror of agent/src/prd-link.ts findSemanticPrdPaths.
// Both implementations are pinned to fixtures/semantic-plan/prd-links.json. It returns
// every candidate PRD path in document order without choosing a preferred document or
// dropping an unsafe candidate; ValidSemanticPrdPath is the separate accept predicate.
func FindSemanticPrdPaths(text string) []string {
	seen := map[string]bool{}
	paths := []string{}
	for _, token := range strings.FieldsFunc(text, isSemanticLinkSeparator) {
		start := strings.Index(token, "prds/")
		if start < 0 || !strings.Contains(token[start:], ".md") {
			continue
		}
		prefix := token[:start]
		// Blob URLs share the clone-relative core semantics; any other prefix
		// (absolute, traversal, backslash) keeps the whole token as an unsafe path.
		core := token
		if prefix == "" || semanticBlobPrefix.MatchString(prefix) {
			core = token[start:]
		}
		core = semanticTrailingPunct.ReplaceAllString(core, "${1}")
		if !seen[core] {
			seen[core] = true
			paths = append(paths, core)
		}
	}
	return paths
}

// ValidSemanticPrdPath mirrors validatePrdPath through the shared Go validator.
func ValidSemanticPrdPath(p string) bool { return prdpath.Validate(p) == nil }

var (
	semanticBlobPrefix    = regexp.MustCompile(`^https?://.+/(?:-/)?blob/.+/$`)
	semanticTrailingPunct = regexp.MustCompile(`(\.md)[,.;:!]+$`)
)

// isSemanticLinkSeparator mirrors the JS class [^\s`"'<>()[\]?#]: ECMAScript \s
// (not Go's ASCII-only \s) plus the listed punctuation.
func isSemanticLinkSeparator(r rune) bool {
	switch r {
	case '`', '"', '\'', '<', '>', '(', ')', '[', ']', '?', '#',
		'\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}
