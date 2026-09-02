package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// runTitle picks a human display title for a run row. A chat carries Title; an
// issue/ci_fix run carries an IssueTitle. Both may be empty (a pre-title run),
// in which case the caller shows the id + kind instead.
func runTitle(r apitypes.RunDTO) string {
	if r.Title != nil && *r.Title != "" {
		return *r.Title
	}
	return r.IssueTitle
}

// strOr renders a *string, substituting fallback for nil/empty.
func strOr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

// int64Or renders a *int64, substituting fallback for nil.
func int64Or(p *int64, fallback string) string {
	if p == nil {
		return fallback
	}
	return fmt.Sprintf("%d", *p)
}

// boolStr renders a bool as "true"/"false" for a table cell.
func boolStr(b bool) string { return fmt.Sprintf("%t", b) }

// triStateStr renders a tri-state *bool (PRD #841): nil → "inherit", true → "on",
// false → "off". Used by the MR_REWORK rows in `run get` / `schedule get`, where a nil
// pointer means "inherit the owner default" rather than a concrete on/off — unlike
// boolStr, which cannot express the inherit case.
func triStateStr(b *bool) string {
	if b == nil {
		return "inherit"
	}
	if *b {
		return "on"
	}
	return "off"
}

// mrAbbrev is the compact per-run label for the merge/pull request, forge-aware:
// GitLab "MR", Forgejo AND GitHub "PR" (PRD #65 D2, #238 D2). It is the CLI's twin of
// the web's mrAbbrev (web/src/lib/forgeNoun.ts) and slacksvc's forgeMrAbbrev — same
// mapping, kept in sync so `uzi run get` reads in the run's own forge vocabulary.
// Both PR-forges are named explicitly so a missing github arm never renders "MR".
// Any unknown/absent forge_type is GitLab's form (the default connection kind).
func mrAbbrev(forgeType string) string {
	if forgeType == "forgejo" || forgeType == "github" {
		return "PR"
	}
	return "MR"
}

// effectiveRunStatus is the status a run should RENDER as: "revising" while a "revise"
// replan is in flight (issue #750), "planning" while it is in its pre-approval planning
// phase (issue #321), else its raw status. is_planning and is_revising are server-computed
// display predicates, each meaningful only in one status: is_planning while running
// (chat/judge excluded server-side), is_revising while awaiting_approval (a revise replan
// leaves runs.status at awaiting_approval). CRITICAL: is_revising is NOT status-gated
// server-side, so the awaiting_approval gate is applied HERE — mirroring how is_planning is
// only honoured while running. Mirrors the web helper of the same name so the CLI and SPA
// name the phase identically.
func effectiveRunStatus(status string, isPlanning, isRevising bool) string {
	if isRevising && status == "awaiting_approval" {
		return "revising"
	}
	if isPlanning && status == "running" {
		return "planning"
	}
	return status
}

// sanitizeTTY strips terminal control characters from UNTRUSTED free text before
// it is written to a human TTY (Risk 13). Judge/run content can carry attacker-
// shaped bytes that repo/issue/CI text fed the LLM; printed verbatim, an embedded
// ANSI escape/CSI sequence could clear the screen, recolour, hide, or spoof
// output. It removes every control character (C0, C1 and DEL) except tab and
// newline, plus every Unicode format character (category Cf); all printable UTF-8
// passes through unchanged. It iterates runes (not bytes) so a multibyte codepoint
// whose bytes fall in 0x80–0x9F is never corrupted. Human render path ONLY —
// --json output stays byte-exact, and sanitizing there would corrupt the payload an
// agent decodes.
//
// 🔴 THE REASON --json IS SAFE IS THE DESTINATION, NOT THE ENCODER. This sentence
// used to read "structural JSON encoding already escapes these", where "these" is
// the C0/C1/DEL/Cf set enumerated above — false for three of those four families.
// Measured on encoding/json (issue #144, re-derived independently by two people):
// C0 and U+2028/U+2029 are escaped; DEL (0x7f), the C1 range including U+009B, and
// the Cf family including U+202E and the zero-widths all pass through UNESCAPED.
// What makes --json safe is that its bytes go to a PARSER rather than to a terminal.
// A caller who pipes --json straight to a TTY is outside that guarantee, and no
// encoder property will save them.
//
// (Stated as an escaping fact on purpose. Whether a terminal HONOURS a UTF-8-encoded
// U+009B as a CSI introducer depends on its 8-bit control handling and has not been
// tested here — do not upgrade this into a claim about exploitability.)
//
// The Cf half and DEL arrived with PRD #112 M3. This comment used to say it removed
// "C0 controls (0x00–0x1F) except tab and newline, and C1 controls (0x80–0x9F)",
// which was an accurate description of a predicate with two holes: `r < 0x20` let
// DEL (0x7f) through, and no range test can catch Cf at all.
// It uses CATEGORY PREDICATES, not codepoint ranges, and deliberately the SAME pair
// workersvc.hasUnsafeChar already settled on (agent_selection.go:236-240):
// unicode.IsControl covers C0 and C1 (so the old hand-rolled 0x00-0x1F / 0x80-0x9F
// ranges are subsumed) and it covers DEL 0x7f, which the old `r < 0x20` test let
// through; unicode.In(r, unicode.Cf) covers the format characters a range test can
// never enumerate — the bidi overrides U+202A-202E, the isolates U+2066-2069, U+200F,
// the zero-widths, the BOM, and SHY. A bidi override is the one that matters most: it
// visually reorders text, so an agent label or judge target can be made to READ as
// something it is not, which is precisely the spoof a TUI's fixed-width rails invite.
//
// TWO THINGS THIS DOES NOT COVER, so nobody reads it as more than it is:
//
//   - Combining marks are Mn, not Cf. "Zalgo" text stays a grapheme-WIDTH problem and
//     is not addressed here; a width-aware layout is the fix, not a stripper.
//   - Cf codepoints are ZERO-WIDTH while capCell pads by RUNES, so before this change
//     a label full of them consumed column budget while drawing nothing, silently
//     misaligning the rail. That is the same root cause as the tab bug whose comment
//     notes the rune offset stayed pinned "which is why the existing alignment test
//     could not see it". Stripping Cf fixes the spoof and that drift together.
//
// THE PREDICATE MOVED TO uzicli (#180) AND THIS IS NOW A DELEGATION, not a second
// copy. uzicli.Printer.Table/Printf/Println sanitize at the shared render boundary, so
// the binary would otherwise hold two implementations of the same security rule in two
// packages — and the one that drifts is always the one nobody is looking at. The name
// stays because it is what the D7 guard, the render tests and this file's ~30 call
// sites are written against, and because the call sites below make a genuine
// sanitizeTTY-versus-cellText choice that reads correctly under these names.
func sanitizeTTY(s string) string { return uzicli.SanitizeTTY(s) }

// cellText is compactText plus the two folds a FIXED-WIDTH column needs and the
// shared compactText must not do — it also backs the payload and steer columns,
// which are free text where a tab is ordinary content and where nothing is
// promised about width.
//
// Tab. sanitizeTTY spares `\t` deliberately, and compactText folds only `\n`, so a
// tab in `agent_label` reaches the cell. `%-*s` then pads to actorCellWidth in
// RUNES and a tab is one rune, so every invariant the code checks still holds while
// the terminal expands it to the next 8-column stop and the payload column walks
// right. MEASURED before the fix: a benign label put the payload at rendered column
// 58; `a\tb\tc\td\te` put it at 76; eight interior tabs at 107 — with the rune
// offset pinned at 58 throughout, which is why the existing alignment test could
// not see it. Folded to a space, which preserves the word break the tab was doing.
//
// DEL used to need handling here too, and NO LONGER DOES. This paragraph read "0x7f
// is outside sanitizeTTY's C0 (<0x20) and C1 (0x80–0x9f) ranges, so it survives too",
// which described the predicate PRD #112 M3 replaced: sanitizeTTY now tests
// unicode.IsControl, which is true for 0x7f, so compactText strips DEL before this
// Map ever sees it. The `case 0x7f: return -1` arm was dead — deleting it reddened
// nothing — and is gone. The tab arm stays live, because sanitizeTTY spares tab
// deliberately.
//
// The tab fold is cosmetic — no cursor motion, no erase, no OSC, and `\n` cannot survive
// compactText — but the actor column's whole purpose is to stay aligned down a
// `uzi run logs --follow` stream, and model-authored prose is exactly where a stray
// tab comes from.
func cellText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		return r
	}, compactText(s))
	// Folding can leave an edge space behind (compactText trimmed before the fold).
	return strings.TrimSpace(s)
}

// capCell truncates a table cell to max RUNES, appending an ellipsis. Rune-based
// per the house idiom (workersvc.deriveChatTitle): byte-slicing splits a multibyte
// codepoint into invalid UTF-8. The neighbouring compactText caps on runes for the
// same reason (issue #554); the two are now consistent.
func capCell(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// compactText sanitizes untrusted free text and folds it to a single truncated
// line for a human table cell (Risk 13): C0/C1 controls stripped, newlines
// collapsed to spaces, capped at 200 RUNES. Shared by compactPayload (message
// payloads) and the steer-queue body column.
//
// The cap is rune-based (issue #554): a byte slice at 200 bytes could split a
// multibyte rune straddling the boundary, emitting an orphan continuation byte —
// invalid UTF-8 to a terminal on the direct callers, and a U+FFFD mojibake glyph
// on the paths that re-encode downstream (cellText's strings.Map). capCell above
// caps on runes for the same reason.
func compactText(s string) string {
	s = sanitizeTTY(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 200
	// Range yields each rune's start byte index, so we can slice at the 201st
	// rune's boundary without allocating a []rune for the whole input — payloads
	// forwarded through compactPayload can be up to the client's 32 MiB cap.
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// relAge renders a coarse relative age (e.g. "5s", "12m", "3h", "2d") for a
// timestamp, for the steer-queue AGE column. A zero or future time renders "-"
// and "0s" respectively; sub-second precision is not useful here.
func relAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// shortRecID is the git-style short recommendation id: the first 8 hex characters
// of the rec UUID. The mutation verbs accept it (resolved back to the full id
// against the current review); --json always carries the full id.
func shortRecID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
