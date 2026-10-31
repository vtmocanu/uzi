package workersvc

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	// crossCheckDispositionsHeading is the exact heading the automatic revision prompt asks the
	// lead to include in the COMPLETE revised plan (buildAutomaticRevisionPrompt in the agent).
	crossCheckDispositionsHeading = "Cross-check dispositions"
	// crossCheckResponseMaxBytes caps the response text a client can receive per round.
	crossCheckResponseMaxBytes = 8 * 1024
	// planCandidateNormalizationCap is NormalizePlanCrossCheckCandidate's plan_md bound. A
	// stored candidate at the cap may have lost its tail to scrubThenBound; the slack covers the
	// final rune SanitizeBounded may append before the post-scrub bound.
	planCandidateNormalizationCap = 256 * 1024
	planCandidateCapSlack         = 4
)

var (
	crossCheckFenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	crossCheckATXRe   = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]|$)`)
)

// ExtractCrossCheckDispositions returns the lead's "Cross-check dispositions" section from a
// plan, outside fenced code blocks, and a state: reported, truncated (the section exceeded 8 KiB
// and was cut on a UTF-8 boundary) or not_reported (no such heading, or an empty section).
//
// An ATX heading ("## Cross-check dispositions") ends at the next heading of the same or higher
// level, or at the end of the plan. The bold form ("**Cross-check dispositions**", the form
// docs/cross-check.md uses) ends at the next ATX heading of any level, the next bold-only line,
// or the end of the plan. The text is untrusted lead prose; this function neither verifies nor
// interprets it. It is pure: the row-relative states are assigned by crossCheckRoundResponse.
func ExtractCrossCheckDispositions(planMd string) (text, state string) {
	lines := strings.Split(strings.ReplaceAll(planMd, "\r\n", "\n"), "\n")
	inFence, fenceChar, fenceLen := false, byte(0), 0
	start, level := -1, 0
	bold := false
	var section []string
	for _, line := range lines {
		// Fence tracking: only a fence of the same character, at least as long as the opener
		// and with no info string, closes a block.
		if m := crossCheckFenceRe.FindStringSubmatch(line); m != nil {
			marker := m[1]
			if !inFence {
				inFence, fenceChar, fenceLen = true, marker[0], len(marker)
				if start >= 0 {
					section = append(section, line)
				}
				continue
			}
			if marker[0] == fenceChar && len(marker) >= fenceLen && strings.TrimSpace(m[2]) == "" {
				inFence = false
			}
			if start >= 0 {
				section = append(section, line)
			}
			continue
		}
		if inFence {
			if start >= 0 {
				section = append(section, line)
			}
			continue
		}
		atxLevel := 0
		if m := crossCheckATXRe.FindStringSubmatch(line); m != nil {
			atxLevel = len(m[1])
		}
		boldOnly := atxLevel == 0 && isBoldOnlyLine(line)
		if start < 0 {
			if headingTitle(line, atxLevel, boldOnly) == crossCheckDispositionsHeading {
				start, level, bold = 0, atxLevel, boldOnly
			}
			continue
		}
		if (bold && (atxLevel > 0 || boldOnly)) || (!bold && atxLevel > 0 && atxLevel <= level) {
			break
		}
		section = append(section, line)
	}
	if start < 0 {
		return "", apitypes.PlanCrossCheckResponseNotReported
	}
	body := strings.TrimSpace(strings.Join(section, "\n"))
	if body == "" {
		return "", apitypes.PlanCrossCheckResponseNotReported
	}
	if len(body) > crossCheckResponseMaxBytes {
		cut := crossCheckResponseMaxBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		return strings.TrimSpace(body[:cut]), apitypes.PlanCrossCheckResponseTruncated
	}
	return body, apitypes.PlanCrossCheckResponseReported
}

// isBoldOnlyLine reports a line that is nothing but one bold run, the "bold heading" form.
func isBoldOnlyLine(line string) bool {
	t := strings.TrimSpace(line)
	return len(t) > 4 && strings.HasPrefix(t, "**") && strings.HasSuffix(t, "**") && !strings.Contains(t[2:len(t)-2], "**")
}

// headingTitle trims a heading line to its title: leading '#' and whitespace, a closing '#'
// run on an ATX heading, and surrounding '**'. A plain line with neither '#' nor '**' is not a
// heading and yields "".
func headingTitle(line string, atxLevel int, boldOnly bool) string {
	if atxLevel == 0 && !boldOnly {
		return ""
	}
	t := strings.TrimSpace(line)
	if atxLevel > 0 {
		t = strings.TrimSpace(strings.TrimLeft(t, "#"))
		t = strings.TrimSpace(strings.TrimRight(t, "#"))
	}
	if strings.HasPrefix(t, "**") && strings.HasSuffix(t, "**") && len(t) >= 4 {
		t = strings.TrimSpace(t[2 : len(t)-2])
	}
	return t
}

// crossCheckRoundResponse assigns the response of the lead to round k's findings (PRD #3708).
// following is the next round's row when one exists in the window; lead is the lead run for the
// presented-plan fallback. No resolution is ever inferred from the text.
func crossCheckRoundResponse(row store.ListPlanCrossCheckRoundsRow, following *store.ListPlanCrossCheckRoundsRow, lead *store.Run) apitypes.PlanCrossCheckRoundResponseDTO {
	if following != nil {
		if following.LeadClaimGeneration != row.LeadClaimGeneration {
			// The reclaimed lead never received this round's findings: discovery passes it
			// only the round number, so whatever it wrote is not a response to them.
			return apitypes.PlanCrossCheckRoundResponseDTO{State: apitypes.PlanCrossCheckResponseReplannedAfterInterrupted}
		}
		text, state := ExtractCrossCheckDispositions(following.PlanMd.String)
		if state == apitypes.PlanCrossCheckResponseNotReported && len(following.PlanMd.String) >= planCandidateNormalizationCap-planCandidateCapSlack {
			// Scrubbing can lengthen text and cut the tail, so absence cannot be told apart
			// from truncation of the candidate itself.
			return apitypes.PlanCrossCheckRoundResponseDTO{State: apitypes.PlanCrossCheckResponseUnknown}
		}
		return apitypes.PlanCrossCheckRoundResponseDTO{State: state, Text: boundResponseText(text)}
	}
	// No following row. Only a decided REVISE has findings the lead could answer, and only a
	// presented plan that is the lead's own same-generation, never-human-revised rewrite can be
	// read as that answer (for example when the next round's submit was refused after it revised).
	if lead != nil && row.Verdict == "revise" && lead.PlanMd.Valid && lead.PlanMd.String != row.PlanMd.String &&
		lead.ClaimGeneration == row.LeadClaimGeneration && lead.ReviseCount == 0 {
		text, state := ExtractCrossCheckDispositions(lead.PlanMd.String)
		if state == apitypes.PlanCrossCheckResponseNotReported {
			return apitypes.PlanCrossCheckRoundResponseDTO{State: state}
		}
		return apitypes.PlanCrossCheckRoundResponseDTO{State: apitypes.PlanCrossCheckResponseReportedInPresentedPlan, Text: boundResponseText(text)}
	}
	return apitypes.PlanCrossCheckRoundResponseDTO{State: apitypes.PlanCrossCheckResponseNoFollowingCandidate}
}

// boundResponseText scrubs secrets and control characters from lead prose, then re-applies the
// 8 KiB cap on a rune boundary (scrubbing can lengthen text, and the sanitizer may overshoot by
// the final rune).
func boundResponseText(text string) string {
	value := scrubThenBound(text, crossCheckResponseMaxBytes)
	for len(value) > crossCheckResponseMaxBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
