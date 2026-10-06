package workersvc

import (
	"time"
	"unicode/utf8"
)

// maxIssueCommentsBytes bounds the stored/rendered comment thread, in the spirit
// of handler.MaxForgeBodyBytes (32768). Measured over the sum of comment bodies.
const maxIssueCommentsBytes = 32768

// maxIssueCommentsCount bounds claim and review-comment metadata.
const maxIssueCommentsCount = 200

// IssueCommentSnapshot is one human comment captured at run creation (PRD #381 D7).
type IssueCommentSnapshot struct {
	AuthorUsername    string    `json:"author_username"`
	AuthorForgeUserID int64     `json:"author_forge_user_id"`
	CreatedAt         time.Time `json:"created_at"`
	Body              string    `json:"body"`
	Reason            string    `json:"reason,omitempty"`
}

// IssueCommentsSnapshot is the structured JSONB stored in runs.issue_comments and
// carried on the claim. Truncated is set whenever the thread was clipped to fit the
// bounds: older comments dropped by the count or byte cap, or a single over-cap
// newest body trimmed in place — i.e. the agent is not seeing the whole thread.
type IssueCommentsSnapshot struct {
	Version   int                    `json:"version,omitempty"`
	Withheld  bool                   `json:"withheld,omitempty"`
	Unknown   bool                   `json:"unknown,omitempty"`
	Comments  []IssueCommentSnapshot `json:"comments"`
	Truncated bool                   `json:"truncated"`
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
