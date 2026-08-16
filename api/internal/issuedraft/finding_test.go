package issuedraft

import (
	"strings"
	"testing"
)

// TestRenderFindingRoutesEveryUntrustedFieldThroughItsSanitiser is the security-load-bearing
// pin for the finding draft (PRD #333 D4): title→SanitizeTitle, description→FenceBlock+
// SanitizeFiledBody, location→SafeInlineCode. It feeds a hostile payload carrying a leading
// quick-action "/", a backtick fence-breakout attempt and an unfenced "/label" line, and
// asserts each field comes back inert/fenced. (Bidi/control stripping is the M2 INGEST layer's
// job — termsafe.SanitizeBounded runs before storage — not these field sanitisers, whose
// contract is fence-breakout and quick-action safety.)
func TestRenderFindingRoutesEveryUntrustedFieldThroughItsSanitiser(t *testing.T) {
	d := RenderFinding(FindingDraftInput{
		// A leading "/" would be a quick-action character; SanitizeTitle defangs it.
		Title: "/label ~backdoor bug in the sweeper",
		// A ``` inside the body must NOT let the description break out of its fence, and a
		// column-0 "/label" outside a fence must be stripped by SanitizeFiledBody.
		Description: "does a thing\n```\n/label ~backdoor\n```\nmore text",
		// A backtick run in the location must not close the inline-code span early
		// (SafeInlineCode picks a strictly-longer delimiter).
		Location:   "api/internal/sweep.go#sweep``Loop",
		RepoPath:   "g/a",
		RunShortID: "1a2b3c4d",
		RunKind:    "issue",
		IssueIID:   11,
	})

	// Title: single line, no leading "/".
	if strings.ContainsAny(d.Title, "\n\r") {
		t.Errorf("title must be a single line, got %q", d.Title)
	}
	if strings.HasPrefix(d.Title, "/") {
		t.Errorf("title must not open with a quick-action '/', got %q", d.Title)
	}

	// Location: an inert inline-code span whose delimiter is a backtick run STRICTLY LONGER
	// than the longest run in the content (3 backticks here, wrapping the "``"), so the
	// embedded backticks cannot close the span early.
	if !strings.HasPrefix(d.Location, "```") || !strings.HasSuffix(d.Location, "```") {
		t.Errorf("location must be wrapped in a breakout-proof inline-code span, got %q", d.Location)
	}

	// Description: the whole body ran through SanitizeFiledBody, so no line's first
	// non-space character is a "/" outside a fence (the injected "/label" line, which sits
	// inside the ``` the agent supplied, is kept — but the fence is breakout-proof, so the
	// body never contains a live column-0 quick-action). Assert the write-boundary invariant
	// directly: no unfenced "/"-line survives.
	if hasUnfencedSlashLine(d.Description) {
		t.Errorf("description carries a live unfenced quick-action line:\n%s", d.Description)
	}

	// Provenance footer names the run and the work it was doing, from server/enum/integer
	// values only.
	if !strings.Contains(d.Provenance, "uzi run 1a2b3c4d") {
		t.Errorf("provenance must name the reporting run, got %q", d.Provenance)
	}
	if !strings.Contains(d.Provenance, "issue #11") {
		t.Errorf("provenance must name the work (kind + issue iid), got %q", d.Provenance)
	}
	// The footer is embedded in the body too.
	if !strings.Contains(d.Description, d.Provenance) {
		t.Errorf("the body must carry the provenance footer")
	}
}

// TestRenderFindingProvenanceWithoutIssue drops the "while working …" clause when the
// reporting run has no issue iid (a prompt/self_improve lane), keeping the footer deterministic.
func TestRenderFindingProvenanceWithoutIssue(t *testing.T) {
	d := RenderFinding(FindingDraftInput{
		Title: "t", Description: "d", Location: "a/b.go#f", RunShortID: "deadbeef",
	})
	if strings.Contains(d.Provenance, "while working") {
		t.Errorf("no issue iid ⇒ no 'while working' clause, got %q", d.Provenance)
	}
	if !strings.Contains(d.Provenance, "uzi run deadbeef") {
		t.Errorf("provenance must still name the run, got %q", d.Provenance)
	}
}

// hasUnfencedSlashLine reports whether body carries a line whose first non-space char is "/"
// outside a fenced block — the exact hazard StripUnfencedSlashLines exists to remove. It
// re-uses the package's own fence tracker via a round-trip: if stripping changes nothing, the
// body already had no live quick-action line.
func hasUnfencedSlashLine(body string) bool {
	return StripUnfencedSlashLines(body) != body
}
