package slacksvc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3813: a completed parent whose advisory Code cross-check failed discloses the
// incomplete check on its existing completed-run thread message. The child itself stays
// DM-free (#3628); this adds no post, root, mention or nudge — only one context block on
// the message the completion already sends, so delivery and repeat semantics are the
// completion's own.

const codeCrossCheckAdvisoryBlockID = "slack_thread_code_cross_check_ctx"

// codeCrossCheckReasonText is the FIXED reason_class → text mapping. Anything else
// (unknown, empty, NULL) reads "reason unavailable": a stored reason is never echoed, so
// no provider error, finding or caller-authored text can reach Slack through it.
func codeCrossCheckReasonText(reason pgtype.Text) string {
	switch reason.String {
	case "worker_unsupported":
		return "worker unsupported"
	case "snapshot_failed":
		return "snapshot failed"
	case "malformed":
		return "checker output malformed"
	case "model_error":
		return "model error"
	case "model_timeout":
		return "model timed out"
	case "checker_unavailable":
		return "checker unavailable"
	case "confinement_failed":
		return "checker confinement failed"
	case "timed_out":
		return "check deadline expired"
	default:
		return "reason unavailable"
	}
}

// codeCrossCheckAdvisoryText renders the advisory for a persisted Code row, or "" when
// none applies. Only outcome, interrupted_at and reason_class are read; the legacy
// verdict column is always 'failed' on code rows (migration 00324) and is ignored.
// "Publication continued." needs a persisted mr_web_url passing mrLink's own guard
// (persistedMRLink); the GitLab mr_iid reconstruction is not proof of publication.
func codeCrossCheckAdvisoryText(cc store.CrossCheck, rc store.GetSlackRunContextRow) string {
	if !cc.Outcome.Valid || cc.Outcome.String != "failed" || cc.InterruptedAt.Valid ||
		cc.ReasonClass.String == "superseded" {
		return ""
	}
	text := "Code cross-check incomplete: " + codeCrossCheckReasonText(cc.ReasonClass) + "."
	if persistedMRLink(rc) != "" {
		text += " Publication continued."
	}
	return text
}

// codeCrossCheckAdvisory reads the owner-bound Code summary for a completed parent. No
// row (disabled / not recorded) is silent; any other read failure is logged (redacted)
// and leaves the advisory absent — it never blocks the completion message nor reads as a
// failed check.
func (n *Notifier) codeCrossCheckAdvisory(ctx context.Context, rc store.GetSlackRunContextRow) string {
	cc, err := n.store.GetCodeCrossCheckForOwner(ctx, store.GetCodeCrossCheckForOwnerParams{LeadRunID: rc.ID, UserID: rc.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		n.logf("load code cross-check summary", err)
		return ""
	}
	return codeCrossCheckAdvisoryText(cc, rc)
}

// withCodeCrossCheckAdvisory appends the advisory as its own context block (a distinct
// block_id from slack_thread_completed_ctx) and to the plain-text fallback. An empty
// advisory returns the inputs unchanged.
func withCodeCrossCheckAdvisory(blocks []slack.Block, fallback, advisory string) ([]slack.Block, string) {
	if advisory == "" {
		return blocks, fallback
	}
	blocks = append(blocks, slack.NewContextBlock(codeCrossCheckAdvisoryBlockID, threadMrkdwnElem(advisory)))
	return blocks, fallback + ". " + advisory
}
