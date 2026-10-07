package workersvc

import (
	"slices"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ReviewCommentSnapshot is one MR review comment captured for an mr_rework run
// (PRD #700 M2). It mirrors IssueCommentSnapshot but carries every field the
// detector (M3) and worker (M4) need: the monotonic forge id (the high-water
// anchor), the diff anchor (Path/Line), the reply/resolve thread ids, the head
// SHA the comment was written against (the staleness gate), and the review state
// (inline vs summary). Bodies are UNTRUSTED, attacker-influenceable free text.
type ReviewCommentSnapshot struct {
	ID                int64     `json:"id"`
	AuthorUsername    string    `json:"author_username"`
	AuthorForgeUserID int64     `json:"author_forge_user_id"`
	CreatedAt         time.Time `json:"created_at"`
	Body              string    `json:"body"`
	Path              *string   `json:"path"`
	Line              *int      `json:"line"`
	ReplyID           string    `json:"reply_id"`
	ResolveID         string    `json:"resolve_id"`
	HeadSHA           string    `json:"head_sha"`
	ReviewState       string    `json:"review_state"`
}

// ReviewCommentsSnapshot is the structured JSONB stored in runs.review_comments and
// carried on the mr_rework claim. Truncated is set whenever the thread was clipped
// to fit the shared #381 bounds — i.e. the worker is not seeing every comment.
//
// Version is ReviewSnapshotVersion for every snapshot built since author eligibility reached
// this lane (issue #2347) and 0 for a legacy row, whose comments were never assessed: the
// reply/resolve scope check authorizes nothing from it and the claim replays it empty.
// Comments holds ELIGIBLE comments only; the withheld counts report how many other
// (non-self) comments were left out, without carrying any of their content.
type ReviewCommentsSnapshot struct {
	Version             int                     `json:"version"`
	Comments            []ReviewCommentSnapshot `json:"comments"`
	Truncated           bool                    `json:"truncated"`
	WithheldNotEligible int                     `json:"withheld_not_eligible"`
	WithheldUnknown     int                     `json:"withheld_unknown"`
}

// ReviewSnapshotVersion is the snapshot version whose comments are author-assessed. It is the
// review lane's own contract (pinned to 2 by TestReviewSnapshotVersionIsPinnedToTwo), not an
// alias of the issue lane's issueinput.SnapshotVersion: bumping one lane must not silently
// re-version, and so legacy-withhold, the other's stored snapshots.
const ReviewSnapshotVersion = 2

// CodeRabbit tags its non-actionable top-level notes with these HTML markers. They
// are forge-agnostic: on GitLab/Forgejo CodeRabbit posts as an ordinary user (no
// "[bot]" login suffix), so the marker is the only signal there. Carried verbatim.
const (
	coderabbitSummaryMarker     = "<!-- This is an auto-generated comment: summarize by coderabbit.ai -->"
	coderabbitWalkthroughMarker = "<!-- walkthrough_start -->"
)

// botControlCommands is the single allowlist of verified, standalone review-bot
// commands, keyed by the lowercased mention handle, each mapped to the exact
// normalized command words allowed after it. CodeRabbit's are the ones uzi's own
// watcher workflow posts; Greptile's are its documented on-demand review triggers
// (issue #1695). A bare mention is deliberately absent: it is not a verified trigger.
var botControlCommands = map[string][]string{
	"@coderabbitai":      {"review", "full review", "rate limit", "reviews remaining?"},
	"@coderabbitai[bot]": {"review", "full review", "rate limit", "reviews remaining?"},
	"@greptileai":        {"review"},
	"@greptile":          {"review"},
}

// isBotControlComment recognizes a body consisting solely of one allowlisted bot
// command. A loose mention-prefix check would hide real human feedback such as
// "@coderabbitai review; please also rename X", so normalized bodies containing any
// extra words remain actionable.
func isBotControlComment(body string) bool {
	fields := strings.Fields(strings.ToLower(body))
	if len(fields) < 2 {
		return false
	}
	return slices.Contains(botControlCommands[fields[0]], strings.Join(fields[1:], " "))
}

// IsActionableReviewComment reports whether a kept review comment counts toward the
// mr_rework trigger (issue #1142). An INLINE finding always counts — that is what
// mr_rework exists for, including third-party review bots like CodeRabbit, which put
// their findings inline. A summary / top-level note does NOT count when it is a bot
// walkthrough/status/tips note: authored by a GitHub App bot (login ends in "[bot]"),
// carrying one of CodeRabbit's summary/walkthrough markers, or consisting solely of a
// recognized review-bot control command (botControlCommands: CodeRabbit, Greptile). A
// human top-level note ("please also rename X") stays actionable, including one that
// adds prose after a bot command. Anything whose review state is not the summary
// sentinel defaults to actionable, so the trigger set can only ever shrink relative to
// the pre-#1142 behavior, never grow.
//
// It reads only the fields ReviewCommentSnapshot carries; the poller detector
// (poller/mr_review_watch.go) calls it, through ReviewAssessment, to compute the trigger
// high-water, while the full eligible snapshot — including the non-actionable notes — still
// rides a run that fires (they are useful context; the change is to what COUNTS, not what
// is read). A summary note from an allowlisted review bot is never actionable, whatever its
// text (ReviewAssessment applies that on top of this classifier).
func IsActionableReviewComment(c ReviewCommentSnapshot) bool {
	if c.ReviewState != forge.ReviewCommentSummary {
		return true
	}
	if strings.HasSuffix(c.AuthorUsername, "[bot]") {
		return false
	}
	if strings.Contains(c.Body, coderabbitSummaryMarker) || strings.Contains(c.Body, coderabbitWalkthroughMarker) {
		return false
	}
	if isBotControlComment(c.Body) {
		return false
	}
	return true
}

// capReviewComments applies the shared #381 count and byte caps to the ELIGIBLE comments,
// oldest-first in and out, keeping the NEWEST tail: the count cap first, so a flood of tiny
// comments cannot retain an unbounded number of entries, then the byte cap over body bytes.
// A single newest body over the byte cap is cut on a UTF-8 rune boundary. The caps only ever
// see eligible comments, so a flood of withheld ones cannot evict an eligible comment.
func capReviewComments(kept []ReviewCommentSnapshot) ([]ReviewCommentSnapshot, bool) {
	truncated := false
	if len(kept) > maxIssueCommentsCount {
		kept = kept[len(kept)-maxIssueCommentsCount:]
		truncated = true
	}
	total := 0
	for _, c := range kept {
		total += len(c.Body)
	}
	if total > maxIssueCommentsBytes {
		truncated = true
		// Walk from the newest backward, accumulating until adding the next-older body
		// would exceed the cap; the retained window is [start:]. Always keep the newest.
		start := len(kept) - 1
		sum := len(kept[start].Body)
		for i := len(kept) - 2; i >= 0; i-- {
			if sum+len(kept[i].Body) > maxIssueCommentsBytes {
				break
			}
			sum += len(kept[i].Body)
			start = i
		}
		kept = kept[start:]
		if len(kept) == 1 && len(kept[0].Body) > maxIssueCommentsBytes {
			kept[0].Body = truncateCommentBody(kept[0].Body)
		}
	}
	return kept, truncated
}
