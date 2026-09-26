package handler

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// GHSA-2722: the judge review and task review ingest gates must redact a recognized
// credential that STRADDLES a field's byte cap. Capping first truncates the token below
// the scrubber's match length (a ghp_ body needs 16+ chars) and persists the prefix.
// Neither handler checks raw field lengths (only httpx's 1 MiB body limit), so every
// field here is reachable over HTTP with a straddling value.

// reviewTokenChunk repeats into the credential body at runtime, so no complete
// provider-token literal sits in this file.
const reviewTokenChunk = "Zq9x"

// reviewStraddle returns a value whose first max bytes end 8 bytes into a GitHub
// classic PAT ("ghp_" plus 4 body chars), and the filler prefix a correct
// scrub-then-bound keeps.
func reviewStraddle(max int) (value, keep string) {
	token := "gh" + "p_" + strings.Repeat(reviewTokenChunk, 9)
	keep = strings.Repeat("a", max-8)
	return keep + token + " tail", keep
}

// assertReviewRedacted fails when got carries any fragment of the straddling token.
// Matching is case-insensitive because canonicalizeTarget lowercases and folds the
// underscore. The kept filler is the positive observation that the field was
// populated from the input at all.
func assertReviewRedacted(t *testing.T, field, got, keep string, max int) {
	t.Helper()
	if !strings.HasPrefix(got, keep) {
		t.Fatalf("%s: stored value lost its leading text (len %d), want the %d-byte filler kept", field, len(got), len(keep))
	}
	lower := strings.ToLower(got)
	if strings.Contains(lower, "ghp") || strings.Contains(lower, strings.ToLower(reviewTokenChunk)) {
		t.Errorf("%s: stored value leaks a truncated credential: tail %q", field, got[len(keep):])
	}
	if len(got) > max {
		t.Errorf("%s: stored value is %d bytes, want at most the %d-byte cap", field, len(got), max)
	}
}

func TestValidateAndScrubReviewScrubsCredentialStraddlingCap(t *testing.T) {
	summary, summaryKeep := reviewStraddle(workersvc.ReviewSummaryMaxBytes)
	model, modelKeep := reviewStraddle(workersvc.ReviewModelMaxBytes)
	target, targetKeep := reviewStraddle(workersvc.ReviewTargetMaxBytes)
	rationale, rationaleKeep := reviewStraddle(workersvc.ReviewRationaleMaxBytes)

	req := validReview()
	req.Summary = summary
	req.Model = model
	req.Recommendations[0].Target = target
	req.Recommendations[0].Rationale = rationale

	sub, err := validateAndScrubReview(req)
	if err != nil {
		t.Fatalf("validateAndScrubReview: %v", err)
	}
	if len(sub.Recommendations) != 1 {
		t.Fatalf("recommendations = %d, want 1", len(sub.Recommendations))
	}
	assertReviewRedacted(t, "summary_md", sub.SummaryMd, summaryKeep, workersvc.ReviewSummaryMaxBytes)
	assertReviewRedacted(t, "judge_model", sub.JudgeModel, modelKeep, workersvc.ReviewModelMaxBytes)
	assertReviewRedacted(t, "target", sub.Recommendations[0].Target, targetKeep, workersvc.ReviewTargetMaxBytes)
	assertReviewRedacted(t, "rationale_md", sub.Recommendations[0].RationaleMd, rationaleKeep, workersvc.ReviewRationaleMaxBytes)
}

func TestValidateAndScrubTaskReviewScrubsCredentialStraddlingCap(t *testing.T) {
	summary, summaryKeep := reviewStraddle(workersvc.TaskReviewSummaryMaxBytes)
	file, fileKeep := reviewStraddle(workersvc.TaskReviewFileMaxBytes)
	symbol, symbolKeep := reviewStraddle(workersvc.TaskReviewSymbolMaxBytes)
	fSummary, fSummaryKeep := reviewStraddle(workersvc.TaskReviewFindingSummaryMax)
	rationale, rationaleKeep := reviewStraddle(workersvc.TaskReviewRationaleMaxBytes)

	sub, err := validateAndScrubTaskReview(workerTaskReviewRequest{
		Status:  "complete",
		Summary: summary,
		Findings: []workerTaskReviewFind{{
			File: file, Symbol: symbol, Line: 1, Severity: "warning",
			Summary: fSummary, Rationale: rationale,
		}},
	})
	if err != nil {
		t.Fatalf("validateAndScrubTaskReview: %v", err)
	}
	if len(sub.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(sub.Findings))
	}
	f := sub.Findings[0]
	assertReviewRedacted(t, "summary_md", sub.SummaryMd, summaryKeep, workersvc.TaskReviewSummaryMaxBytes)
	assertReviewRedacted(t, "file", f.File, fileKeep, workersvc.TaskReviewFileMaxBytes)
	assertReviewRedacted(t, "symbol", f.Symbol, symbolKeep, workersvc.TaskReviewSymbolMaxBytes)
	assertReviewRedacted(t, "finding summary_md", f.SummaryMd, fSummaryKeep, workersvc.TaskReviewFindingSummaryMax)
	assertReviewRedacted(t, "rationale_md", f.RationaleMd, rationaleKeep, workersvc.TaskReviewRationaleMaxBytes)
}

// TestValidateAndScrubReviewInvalidUTF8DoesNotCutCredential pins the "normalize WITHOUT
// truncation" step against input that GROWS under normalization: each invalid UTF-8 byte
// becomes a 3-byte U+FFFD, so a "no-cut" bound of len(s)+1 still cuts. 15 invalid bytes
// ahead of a 40-byte token end that bound 11 bytes into the token, below the scrubber's
// match length. (JSON decoding already replaces invalid UTF-8, so this guards the helper
// rather than a live HTTP path.)
func TestValidateAndScrubReviewInvalidUTF8DoesNotCutCredential(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat(reviewTokenChunk, 9)
	req := validReview()
	req.Summary = strings.Repeat("\xff", 15) + token
	req.Recommendations[0].Target = strings.Repeat("\xff", 15) + token

	sub, err := validateAndScrubReview(req)
	if err != nil {
		t.Fatalf("validateAndScrubReview: %v", err)
	}
	for field, got := range map[string]string{"summary_md": sub.SummaryMd, "target": sub.Recommendations[0].Target} {
		lower := strings.ToLower(got)
		if strings.Contains(lower, "ghp") || strings.Contains(lower, strings.ToLower(reviewTokenChunk)) {
			t.Errorf("%s: leaks a cut credential: %q", field, got)
		}
		if !strings.Contains(lower, "redacted") {
			t.Errorf("%s: %q, want the credential replaced by the redaction placeholder", field, got)
		}
	}
}
