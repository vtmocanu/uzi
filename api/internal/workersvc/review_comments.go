package workersvc

import (
	"context"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
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
type ReviewCommentsSnapshot struct {
	Comments  []ReviewCommentSnapshot `json:"comments"`
	Truncated bool                    `json:"truncated"`
}

// BuildReviewCommentsSnapshot filters, caps, and orders an MR's review comments
// into the structured snapshot the mr_rework run carries (PRD #700 M2). It REUSES
// the #381 issue-comment caps (maxIssueCommentsBytes / maxIssueCommentsCount) and
// the same D1 bot self-filter and D9 unknown-bot-id bail, so the two untrusted-input
// snapshots stay bounded identically.
//
// It returns nil (→ store NULL) whenever the feature should be omitted entirely: an
// unknown bot id (D9), or nothing left after the bot self-filter (D1). The self-filter
// drops the CONNECTION's own bot (botForgeUserID) while KEEPING third-party review
// bots like CodeRabbit — that third-party feedback is the whole point of the feature.
// Input is oldest-first (the M1 driver guarantee) and the output stays oldest-first
// among kept comments; the byte cap charges body bytes only, same truncation semantics
// as buildIssueCommentsSnapshot.
//
// It is exported because the M3 poller detector (poller/mr_review_watch.go) builds the
// snapshot itself — it needs the kept comments to compute the high-water mark and gate
// on review-landedness — then passes it to CreateAutoMRReworkRun, mirroring how the
// ci-autofix detector builds a FailureSnapshot and passes it to CreateAutoCIFixRun.
func BuildReviewCommentsSnapshot(comments []forge.MRComment, botForgeUserID int64) *ReviewCommentsSnapshot {
	// D9 fail-safe: an unknown/zero bot id cannot power the D1 self-filter, so omit
	// the feature rather than risk feeding uzi its own comments back into the prompt.
	if botForgeUserID == 0 {
		return nil
	}

	// D1 self-filter: drop every comment uzi's OWN bot authored (KEEP third-party
	// review bots), preserving order.
	kept := make([]forge.MRComment, 0, len(comments))
	for _, c := range comments {
		if c.AuthorForgeUserID == botForgeUserID {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}

	truncated := false

	// Count cap over the NEWEST tail, applied before the byte cap so a flood of tiny
	// comments cannot retain an unbounded number of entries (metadata amplification).
	if len(kept) > maxIssueCommentsCount {
		kept = kept[len(kept)-maxIssueCommentsCount:]
		truncated = true
	}

	// D4 byte cap over the NEWEST tail. Sum the kept bodies; if they fit, keep all.
	total := 0
	for _, c := range kept {
		total += len(c.Body)
	}
	if total > maxIssueCommentsBytes {
		truncated = true
		// Walk from the newest (last) backward, accumulating until adding the
		// next-older body would exceed the cap; the retained window is [start:].
		// Always keep at least the single newest comment.
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
		// If the single newest comment's body alone exceeds the cap, truncate it
		// byte-safe on a UTF-8 rune boundary (shared with the issue-comment path).
		if len(kept) == 1 && len(kept[0].Body) > maxIssueCommentsBytes {
			kept[0].Body = truncateCommentBody(kept[0].Body)
		}
	}

	out := make([]ReviewCommentSnapshot, 0, len(kept))
	for _, c := range kept {
		out = append(out, ReviewCommentSnapshot{
			ID:                c.ID,
			AuthorUsername:    c.AuthorUsername,
			AuthorForgeUserID: c.AuthorForgeUserID,
			CreatedAt:         c.CreatedAt,
			Body:              c.Body,
			Path:              c.Path,
			Line:              c.Line,
			ReplyID:           c.ReplyID,
			ResolveID:         c.ResolveID,
			HeadSHA:           c.HeadSHA,
			ReviewState:       c.ReviewState,
		})
	}
	return &ReviewCommentsSnapshot{Comments: out, Truncated: truncated}
}

// fetchReviewCommentsSnapshot builds a forge driver from the run's repo connection,
// reads the MR's review comments, and returns the filtered/capped snapshot (PRD #700
// M2). It mirrors fetchIssueCommentsSnapshot but reads the MR (mrIID) rather than the
// issue. Returns nil (→ NULL) on any error or when the D1/D9 filter leaves nothing — a
// review-comment snapshot is best-effort run CONTEXT, never a reason to fail creation.
//
// It has no production caller yet; M3's CreateAutoMRReworkRun will call it. The
// degrade-on-failure test in review_comments_test.go references it so the dead-code
// gate does not flag it.
func (s *Service) fetchReviewCommentsSnapshot(ctx context.Context, row store.GetRepoForUserRow, mrIID int64) *ReviewCommentsSnapshot {
	f, err := s.forges.ForgeForConnection(row.ForgeType, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		slog.Error("workersvc: build forge for review comments", "mr_iid", mrIID, "error", err)
		return nil
	}
	comments, err := f.ListMergeRequestComments(ctx, row.ForgeProjectID, mrIID)
	if err != nil {
		slog.Error("workersvc: list merge request comments", "mr_iid", mrIID, "error", err) // err is PAT-redacted by the driver
		return nil
	}
	return BuildReviewCommentsSnapshot(comments, row.BotForgeUserID)
}
