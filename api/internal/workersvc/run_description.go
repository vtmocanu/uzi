package workersvc

import (
	"strings"
	"unicode/utf8"
)

// guidanceHeader is the fixed delimiter + framing prepended to owner guidance when it is
// composed into a run instruction (PRD #274 M3, moved here from schedsvc by PRD #1202 so
// the on-demand mr_rework endpoint and the scheduler read ONE composer). The wording is
// deliberately fixed: it tells the model the section is HOW-steering authored by the run
// owner, distinct from the issue body above it (the WHAT, and untrusted forge content). Do
// not template user text into this header.
const guidanceHeader = "\n\n---\n\n" +
	"The guidance below was provided by the schedule owner to steer HOW this task is " +
	"approached. It does not change WHAT the task is (described above); apply it where " +
	"relevant.\n\n"

// guidanceTruncMarker is appended when the guidance had to be truncated to keep the
// composed description under MaxIssueDescriptionBytes, so the model (and a human reading
// the run) can tell the guidance was cut rather than authored short.
const guidanceTruncMarker = "\n\n…[guidance truncated]"

// ComposeRunDescription joins an issue body (the task) with optional owner guidance (the
// HOW-steering), returning a single run description (PRD #274 M3; exported and moved to
// workersvc by PRD #1202). It is the ONE composer the scheduler (via a delegate in
// schedsvc/scheduler.go) and the on-demand mr_rework endpoint both use, so the guidance
// framing cannot drift between surfaces.
//
// When guidance is empty/whitespace it returns the body UNCHANGED — no delimiter — so a
// run without guidance produces a byte-identical description to the pre-guidance behaviour.
//
// Otherwise it appends guidanceHeader + guidance. Because a description over
// MaxIssueDescriptionBytes is rejected downstream (ErrDescriptionTooLarge), appending
// guidance must never push a runnable issue over the cap. So when body + full section would
// exceed the cap, the GUIDANCE is truncated (on a UTF-8 rune boundary) with a marker,
// keeping the header when there is room for it, rather than the issue being silently
// dropped. If the body alone already meets/exceeds the cap there is no room for guidance and
// the body is returned unchanged.
func ComposeRunDescription(body, guidance string) string {
	const max = MaxIssueDescriptionBytes

	out := body
	if g := strings.TrimSpace(guidance); g != "" {
		section := guidanceHeader + g
		switch {
		case len(body)+len(section) <= max:
			out = body + section
		default:
			// Truncation path. If the body alone leaves no room, do not touch it. Reserve the
			// header and the truncation marker; whatever remains is the guidance budget. If the
			// header + marker alone will not fit, run the body alone (it still runs).
			if room := max - len(body); room > 0 {
				if avail := room - len(guidanceHeader) - len(guidanceTruncMarker); avail > 0 {
					out = body + guidanceHeader + truncateGuidanceUTF8(g, avail) + guidanceTruncMarker
				}
			}
		}
	}
	return out
}

// truncateGuidanceUTF8 returns the longest prefix of s that is at most n bytes AND does not
// split a multibyte rune. s is assumed valid UTF-8, so backing the cut up to the nearest
// rune start yields a valid string.
func truncateGuidanceUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
