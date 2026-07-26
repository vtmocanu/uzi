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
// Two further asymmetries worth knowing before trusting a red/green here:
// `prds/../../../etc/passwd` is held by the `.md` SUFFIX rule as well, so it
// survives even the all-three mutant; and `prds/.md` / `prds/.git/x.md` are held
// ONLY by the dotfile rule, which is why removing that one alone does redden.
// What is pinned is the accept/reject SETS in prdpath_test.go, not any one rule.

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
