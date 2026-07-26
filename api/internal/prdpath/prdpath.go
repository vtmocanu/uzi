// Package prdpath is the one definition of what a repo-relative PRD file path
// looks like (PRD #72 M4/M5).
//
// It exists because two milestones must agree on that grammar: M4 validates the
// path a run's lead declares via `signal_done`, and M5 finds and rewrites that
// path inside an issue description. The agreement IS the contract, so it gets one
// home rather than two implementations that can drift.
//
// stdlib only, so no import cycle is possible from either consumer
// (`workersvc`, `forgesvc`).
//
// NOT a replacement for `forgesvc.prdLinkRe`. That regex gates run creation and
// the board's warning badge; changing its behaviour is out of scope for this PRD.
// This package is the natural eventual home for it — follow-up, not now.
package prdpath

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// MaxPathLen bounds a declared PRD path. The worker mirrors this as a transport
// clamp; THIS constant is the authority.
const MaxPathLen = 512

// Root is the directory every PRD path is rooted at, and Ext the required suffix.
const (
	Root = "prds/"
	Ext  = ".md"
)

// Validate reports whether p is a well-formed repo-relative PRD file path.
//
// Every rule here exists because `forgesvc.prdLinkRe` fails it. That regex is
// unexported, UNANCHORED (so it validates by substring: `rm -rf / prds/x.md`
// passes), accepts a blob-URL prefix and `#`/`?` suffixes, and its `[\w.-]+`
// segment class matches `..` (so `prds/../../../x.md` passes). It is a link
// DETECTOR for prose, and it is unusable as a validator — do not reach for it.
//
// The caller decides what a failure means. M4's caller drops the value and warns;
// it must never fail the run's terminal report on a technicality.
func Validate(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if len(p) > MaxPathLen {
		return fmt.Errorf("path exceeds %d bytes", MaxPathLen)
	}
	// Control bytes, DEL, and backslashes. NUL is caught here, before it can reach
	// the database and raise a 22021; backslash keeps a Windows-style separator
	// from smuggling a segment past the `/` split below.
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return fmt.Errorf("path contains a control character or backslash at byte %d", i)
		}
	}
	// Rooted. This one rule also rejects absolute paths (`/prds/…` has no `prds/`
	// prefix) and every non-PRD directory.
	if !strings.HasPrefix(p, Root) {
		return fmt.Errorf("path is not rooted at %q", Root)
	}
	if !strings.HasSuffix(p, Ext) {
		return fmt.Errorf("path does not end in %q", Ext)
	}
	// A whole-string predicate, never a search — this is what `prdLinkRe` lacks.
	for _, seg := range strings.Split(p, "/") {
		if err := validateSegment(seg); err != nil {
			return err
		}
	}
	// Canonical form: rejects `//`, a trailing `/`, a leading `./`, and traversal.
	if path.Clean(p) != p {
		return fmt.Errorf("path is not in canonical form")
	}
	return nil
}

// NOTE for anyone mutation-testing traversal: THREE rules independently reject
// `..`, so no single-line mutant here is killable and the survival of one is not
// a gap. Measured 2026-07-26 by removing each in turn, then in pairs:
//
//	removed                                    | `prds/../x.md`
//	-------------------------------------------|----------------
//	the `.`/`..` segment check                  | still rejected
//	`path.Clean(p) != p`                        | still rejected
//	the dotfile-prefix check (`..` starts `.`)  | still rejected
//	any TWO of the three                        | still rejected
//	all three                                   | ACCEPTED — tests redden
//
// So the design note calling the explicit `.`/`..` check "the traversal fix" and
// `path.Clean` "belt-and-braces" has it the wrong way round: each is sufficient
// alone, and the dotfile rule (added for `.git`, not for traversal) is a third.
// All three are kept deliberately — this validator gates a forge write against a
// user's issue description, and depth is worth more here than a line count.
//
// Two further asymmetries worth knowing before trusting a red/green here, and the
// first one is a trap rather than a curiosity: `prds/../../../etc/passwd` stays
// rejected under the all-three mutant because it fails the `.md` SUFFIX rule — so
// it is NOT evidence that any traversal guard fires, and the `.md` rule is NOT a
// traversal backstop. Use `prds/../x.md`, which ends in `.md` and is therefore held
// by the traversal rules alone. The second: `prds/.md` and `prds/.git/x.md` are
// held ONLY by the dotfile rule, which is why removing that one alone does redden.
// What is pinned is the accept/reject SETS in prdpath_test.go, not any one rule.

// linkRe finds CANDIDATE PRD-path spans in free text. Its charset is the same one
// validateSegment enforces, which is what makes a path M4 accepts a path M5 can
// find. It is a fixed literal pattern with NO interpolation — see ReplacePath's
// note on why that matters and what to do if a future variant needs one.
var linkRe = regexp.MustCompile(`prds/(?:[A-Za-z0-9._-]+/)*[A-Za-z0-9._-]+\.md`)

// pathByte reports whether c is in the shared segment charset.
func pathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-':
		return true
	}
	return false
}

// boundaryAligned reports whether the span description[start:end] stands alone as a
// path rather than being a fragment of a longer token. This is the one definition
// of "path-aligned", shared by Links and ReplacePath.
//
// Done in Go rather than in the regexp because RE2 has no lookbehind.
//
//   - The byte BEFORE may be `/` (so a blob URL matches: `…/-/blob/main/` +
//     `prds/72-x.md`) or anything outside the segment charset (so `xprds/72-x.md`
//     does not match).
//   - The byte AFTER must be outside the charset, which stops `prds/72-x.md.bak`
//     while letting `#L4`, `?ref=main`, `)`, whitespace and end-of-string through.
//
// ONE REFINEMENT ON THE DESIGN, and it is not cosmetic: a trailing `.` that is NOT
// itself followed by a path byte is a SENTENCE TERMINATOR, not a token
// continuation. The design's rule as written rejected the single most common way a
// PRD is referenced in prose — `Implements prds/72-x.md.` — because `.` is in the
// segment charset. Caught by this package's own test rather than reasoned about:
// the fixture was written the way a human writes a sentence and returned nothing.
// The `.bak` case the rule exists for is unaffected, because there the `.` IS
// followed by a path byte. Only `.` gets this treatment; a trailing `-` or `_` is
// vanishingly rare in prose and stays a rejection.
func boundaryAligned(s string, start, end int) bool {
	if start > 0 {
		if b := s[start-1]; b != '/' && pathByte(b) {
			return false
		}
	}
	if end < len(s) {
		after := s[end]
		if pathByte(after) {
			// A sentence-ending period: `.` with nothing token-like behind it.
			if after != '.' || (end+1 < len(s) && pathByte(s[end+1])) {
				return false
			}
		}
	}
	return true
}

// Links returns the well-formed, path-aligned `prds/`-rooted `.md` paths occurring
// in description, in order of appearance, deduplicated.
//
// It is a FINDER over untrusted text, not a validator: a malformed or
// traversal-bearing occurrence is simply not returned, so a caller can only ever
// act on paths that satisfy Validate.
//
// THE `Validate` CALL IS STRUCTURAL, NOT A BELT-AND-BRACES PASS. Validate's
// acceptance set is narrower than any charset regexp, by rules a charset cannot
// express (the `.`/`..` check, the dotfile-prefix rule, path.Clean). A Links built
// on the charset alone would extract `prds/.git/x.md` from a description — a path
// Validate would never accept — and hand it to ReplacePath as an oldPath, which an
// agent declaring `prds/done/x.md` could then target, since the caller matches on
// basename alone. Filtering through Validate means Links cannot drift from it,
// because it CALLS it; two parallel charsets would need re-aligning by hand every
// time Validate gains a rule.
func Links(description string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, loc := range linkRe.FindAllStringIndex(description, -1) {
		start, end := loc[0], loc[1]
		if !boundaryAligned(description, start, end) {
			continue
		}
		cand := description[start:end]
		if Validate(cand) != nil || seen[cand] {
			continue
		}
		seen[cand] = true
		out = append(out, cand)
	}
	return out
}

// ReplacePath returns description with every path-aligned occurrence of oldPath
// replaced by newPath, and the number of occurrences it changed.
//
// oldPath is a LITERAL, never a pattern: the scan is `strings.Index`, so `.` and
// `-` cannot act as regexp metacharacters. That is deliberate even though
// Validate's charset happens to exclude everything dangerous — relying on the
// charset would be deriving an invariant here from a decision made in another file,
// and Validate is exactly the thing a later change might loosen. linkRe above is a
// fixed pattern with no interpolation; if a future variant ever interpolates a
// stored path into a pattern, it takes regexp.QuoteMeta.
//
// A blob-URL prefix and a `#L4` / `?ref=` suffix survive BY CONSTRUCTION rather
// than by being preserved: they are never inside the matched span, so no code here
// can reach them. That is stronger than preserving them explicitly.
//
// EVERY occurrence, not just the first. A description linking the same PRD twice
// (once bare, once as a blob URL) would otherwise be left half-broken.
func ReplacePath(description, oldPath, newPath string) (string, int) {
	// Nothing to do, and a span already equal to newPath is not a change.
	if oldPath == "" || oldPath == newPath {
		return description, 0
	}
	var b strings.Builder
	changed := 0
	i := 0
	for i < len(description) {
		j := strings.Index(description[i:], oldPath)
		if j < 0 {
			break
		}
		start := i + j
		end := start + len(oldPath)
		b.WriteString(description[i:start])
		if boundaryAligned(description, start, end) {
			b.WriteString(newPath)
			changed++
			i = end
			continue
		}
		// Not path-aligned (e.g. `prds/x.md.bak`): copy one byte and rescan, so an
		// occurrence starting inside this one is still found.
		b.WriteByte(description[start])
		i = start + 1
	}
	b.WriteString(description[i:])
	return b.String(), changed
}

// validateSegment enforces the per-segment charset and the traversal rejection.
//
// The charset is exactly `prdLinkRe`'s `[\w.-]` minus `\w`'s Unicode reach, and
// that alignment is load-bearing rather than cosmetic: it is what guarantees a
// path M4 ACCEPTS is a path M5 can FIND in an issue description. Widening it here
// creates paths that validate and then never match, which fails silently in both
// directions. Do not widen one without the other.
//
// The explicit `.`/`..` rejection is the traversal fix. The character class alone
// admits `..` — that is precisely `prdLinkRe`'s bug, so the class is NOT the guard.
func validateSegment(seg string) error {
	if seg == "" {
		return fmt.Errorf("path contains an empty segment")
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("path contains a %q traversal segment", seg)
	}
	// No dotfiles: keeps `.git`, `.claude`, `.ssh` out of a declared path even
	// though the charset would otherwise admit them.
	if strings.HasPrefix(seg, ".") {
		return fmt.Errorf("path segment %q starts with a dot", seg)
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("path segment %q contains an illegal character %q", seg, c)
		}
	}
	return nil
}
