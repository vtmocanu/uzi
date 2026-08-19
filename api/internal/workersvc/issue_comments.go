package workersvc

import (
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// maxIssueCommentsBytes bounds the stored/rendered comment thread, in the spirit
// of handler.MaxForgeBodyBytes (32768). Measured over the sum of comment bodies.
const maxIssueCommentsBytes = 32768

// IssueCommentSnapshot is one human comment captured at run creation (PRD #381 D7).
type IssueCommentSnapshot struct {
	AuthorUsername    string    `json:"author_username"`
	AuthorForgeUserID int64     `json:"author_forge_user_id"`
	CreatedAt         time.Time `json:"created_at"`
	Body              string    `json:"body"`
}

// IssueCommentsSnapshot is the structured JSONB stored in runs.issue_comments and
// carried on the claim. Truncated is set when the D4 byte cap dropped older comments.
type IssueCommentsSnapshot struct {
	Comments  []IssueCommentSnapshot `json:"comments"`
	Truncated bool                   `json:"truncated"`
}

// buildIssueCommentsSnapshot filters, caps, and orders an issue's comments into the
// structured snapshot stored at run creation (PRD #381 M2b). It returns nil (→ store
// NULL) whenever the feature should be omitted entirely: an unknown bot id (D9), or
// nothing left after the bot self-filter (D1). Input is oldest-first (the M1 driver
// guarantee) and the output stays oldest-first among kept comments.
func buildIssueCommentsSnapshot(comments []forge.IssueComment, botForgeUserID int64) *IssueCommentsSnapshot {
	// D9 fail-safe: an unknown/zero bot id cannot power the D1 self-filter, so omit
	// the feature rather than risk feeding uzi its own comments back into the prompt.
	if botForgeUserID == 0 {
		return nil
	}

	// D1 self-filter: drop every comment uzi's own bot authored, preserving order.
	kept := make([]forge.IssueComment, 0, len(comments))
	for _, c := range comments {
		if c.AuthorForgeUserID == botForgeUserID {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}

	// D4 byte cap over the NEWEST tail. Sum the kept bodies; if they fit, keep all.
	total := 0
	for _, c := range kept {
		total += len(c.Body)
	}
	truncated := false
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
		// byte-safe on a UTF-8 rune boundary (mirroring handler.truncateForgeBody).
		if len(kept) == 1 && len(kept[0].Body) > maxIssueCommentsBytes {
			kept[0].Body = truncateCommentBody(kept[0].Body)
		}
	}

	out := make([]IssueCommentSnapshot, 0, len(kept))
	for _, c := range kept {
		out = append(out, IssueCommentSnapshot{
			AuthorUsername:    c.AuthorUsername,
			AuthorForgeUserID: c.AuthorForgeUserID,
			CreatedAt:         c.CreatedAt,
			Body:              c.Body,
		})
	}
	return &IssueCommentsSnapshot{Comments: out, Truncated: truncated}
}

// truncateCommentBody caps s at maxIssueCommentsBytes without splitting a UTF-8 rune,
// trimming any partial trailing rune left by the byte-boundary slice. It mirrors
// handler.truncateForgeBody's rune-safe cut so a single over-cap comment body never
// ships an invalid-UTF-8 fragment.
func truncateCommentBody(s string) string {
	if len(s) <= maxIssueCommentsBytes {
		return s
	}
	b := []byte(s)[:maxIssueCommentsBytes]
	for len(b) > 0 {
		if r, size := utf8.DecodeLastRune(b); r == utf8.RuneError && size <= 1 {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return string(b)
}
