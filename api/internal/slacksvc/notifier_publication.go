package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Publication-phase forge park notifications (issue #1324, plan D10). A run that met a forge
// outage while publishing parks on recovery_wait / forge_unreachable with recovery_wait_phase
// 'publication'. The Slack surface announces such a park (and the owner hold it can end in) with
// fixed wording, and words the resume that follows as a publication retry. All wording is fixed
// text plus the run link and the branch / merge request, escaped like every other message.

// parkKindForgePublication is the slack_run_messages.park_kind of a publication-phase park. The
// column has no CHECK, and an unknown or legacy kind reads as a usage-limit park in
// resumeThreadBlocks, so a publication kind never reaches that wording.
const parkKindForgePublication = "forge_publication"

// parkKindForgePublicationHold is the park_kind of a publication owner hold. Only this kind
// posts a resume line: a timed wait ends by automatic retry, which is not news (the marker of a
// wait is kept across retries so the next re-park of the same episode is not re-announced).
const parkKindForgePublicationHold = "forge_publication_hold"

// isPublicationParkKind reports either publication park kind.
func isPublicationParkKind(kind string) bool {
	return kind == parkKindForgePublication || kind == parkKindForgePublicationHold
}

const (
	publicationPhaseWire    = "publication"
	forgeUnreachableCause   = "forge_unreachable"
	publicationHoldRetryEnd = "publication_retry_exhausted"
)

// publicationPark describes a publication-phase park: hold is true for an owner hold (no retry
// stamp or a recorded hold reason) and reason is the side row's hold reason ("" when none).
//
// at is the owner episode's start (publication_wait_started_at), the marker value both kinds
// are recorded under. It is NOT the run's status_since: every automatic retry re-parks with a
// fresh status_since, which would announce each retry as a new park.
type publicationPark struct {
	hold   bool
	reason string
	at     pgtype.Timestamptz
}

// kind is the park_kind this park is recorded under.
func (p publicationPark) kind() string {
	if p.hold {
		return parkKindForgePublicationHold
	}
	return parkKindForgePublication
}

// publicationParkOf returns the publication park of rc, or nil when rc is not one. The run
// context row carries neither the phase nor the hold reason, so they are read from the run and its
// active publication intent. A failed read is logged and reads as "not a publication park": a
// notification failure never changes the run.
func (n *Notifier) publicationParkOf(ctx context.Context, rc store.GetSlackRunContextRow) *publicationPark {
	if rc.Status != "recovery_wait" || !rc.RecoveryWaitCause.Valid || rc.RecoveryWaitCause.String != forgeUnreachableCause {
		return nil
	}
	run, err := n.store.GetRunByID(ctx, rc.ID)
	if err != nil {
		n.logf("load publication phase", err)
		return nil
	}
	if !run.RecoveryWaitPhase.Valid || run.RecoveryWaitPhase.String != publicationPhaseWire {
		return nil
	}
	pp := &publicationPark{hold: !rc.RecoveryRetryNotBefore.Valid, at: rc.StatusSince}
	side, err := n.store.GetActivePublicationResume(ctx, rc.ID)
	switch {
	case err == nil:
		if side.HoldReason.Valid && side.HoldReason.String != "" {
			pp.hold = true
			pp.reason = side.HoldReason.String
		}
		if side.WaitStartedAt.Valid {
			pp.at = side.WaitStartedAt
		}
	case !errors.Is(err, pgx.ErrNoRows):
		// Without the episode there is no stable announcement key; stay silent rather than
		// announce every retry.
		n.logf("load publication hold", err)
		return nil
	}
	return pp
}

// publicationHoldSentence is the fixed wording per hold reason. An unknown reason is described
// generically and never echoed, so a newer server's value cannot inject text.
func publicationHoldSentence(reason string) string {
	switch reason {
	case publicationHoldRetryEnd:
		return "the forge stayed unreachable for the whole wait"
	case "publication_custodian_unavailable":
		return "the worker holding the unpublished source is gone; the work is local-only"
	case "publication_model_required":
		return "publishing needs model work a publication-only resume does not do"
	case "publication_source_unavailable":
		return "the unpublished work is not available"
	case "publication_intent_mismatch":
		return "the publication record did not match"
	case "":
		return "no hold reason was reported"
	}
	return "the hold reason is not recognised by this version"
}

// publicationThreadBlocks builds the thread event of a publication park: a timed wait says the
// forge is unreachable and publishing retries on its own; an owner hold says why and offers only
// Resume (a new publication-only attempt, no model work) or Cancel. The branch and merge request
// ride the context when known; the branch is untrusted forge text, escaped and scrubbed.
func publicationThreadBlocks(rc store.GetSlackRunContextRow, pp publicationPark, base string) (blocks []slack.Block, fallback string, ok bool) {
	repo := ScrubSecrets(EscapeMrkdwn(rc.PathWithNamespace))
	var detail []string
	head := "⏸️ *Publishing paused · forge unreachable*"
	fallbackHead := "Publishing paused · forge unreachable"
	if pp.hold {
		head = "⚠️ *Needs your attention · publishing held*"
		fallbackHead = "Needs your attention · publishing held"
		detail = append(detail, publicationHoldSentence(pp.reason),
			"Resume starts a new publication-only attempt (no model work), or Cancel from the run page.")
	} else {
		detail = append(detail, "retrying automatically")
		if rc.RecoveryRetryNotBefore.Valid {
			detail[0] = fmt.Sprintf("retrying automatically · next retry <!date^%d^{time}|%s>",
				rc.RecoveryRetryNotBefore.Time.Unix(), rc.RecoveryRetryNotBefore.Time.UTC().Format("15:04 MST"))
		}
	}
	if b := strings.TrimSpace(rc.Branch.String); rc.Branch.Valid && b != "" {
		detail = append(detail, "branch `"+EscapeMrkdwn(b)+"`")
	}
	blocks = []slack.Block{threadSectionBlock(head)}
	ctxElems := []slack.MixedElement{threadMrkdwnElem(ScrubSecrets(strings.Join(detail, " · ")))}
	if mr := mrContextElem(rc); mr != "" {
		ctxElems = append(ctxElems, threadMrkdwnElem(ScrubSecrets(mr)))
	}
	if u := runLink(base, rc.ID); u != "" {
		ctxElems = append(ctxElems, threadMrkdwnElem(ScrubSecrets("🔗 "+u)))
	}
	blocks = append(blocks, slack.NewContextBlock("slack_thread_publication_ctx", ctxElems...))
	return blocks, fmt.Sprintf("%s · %s#%d", fallbackHead, repo, iid(rc.IssueIid)), true
}
