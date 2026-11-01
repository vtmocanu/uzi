package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Publication-phase forge park rendering (issue #1324, plan D10). A run that met a forge outage
// while PUBLISHING keeps recovery_wait_cause forge_unreachable and adds recovery_wait_phase
// "publication". It is a different park from the pre-clone forge park: it has no park-count
// budget (ForgeParkCount / ForgeParkMax mean nothing here and are never rendered), it holds the
// unpublished source, and an owner Resume starts a new publication-only attempt that does no model
// work. The wording lives here and is reached through forgeParkLine / forgeHoldLines, which every
// CLI and TUI surface already shares, so `uzi run get`, `--follow`, the board and the detail
// header cannot give different readings of one park.

// publicationPhase is the RecoveryWaitPhase of a publication-phase park.
const publicationPhase = "publication"

// Publication hold reasons (PublicationHoldReason) a server can set on an owner hold.
const (
	pubHoldRetryExhausted     = "publication_retry_exhausted"
	pubHoldCustodianGone      = "publication_custodian_unavailable"
	pubHoldModelRequired      = "publication_model_required"
	pubHoldSourceUnavailable  = "publication_source_unavailable"
	pubHoldIntentMismatch     = "publication_intent_mismatch"
	publicationHeldWord       = "publish held"
	publicationHeldLeadPrefix = "publishing held: "
)

// publicationPhaseOf is the trimmed phase string, "" for the pre-clone park (null phase).
func publicationPhaseOf(r apitypes.RunDTO) string {
	return strOr(r.RecoveryWaitPhase, "")
}

// isPublicationPark reports a forge park whose phase is exactly "publication".
func isPublicationPark(r apitypes.RunDTO) bool {
	return isForgePark(r) && publicationPhaseOf(r) == publicationPhase
}

// isUnknownPhasePark reports a forge park carrying a phase this client does not know. It is
// rendered with a generic line and never as the pre-clone park.
func isUnknownPhasePark(r apitypes.RunDTO) bool {
	p := publicationPhaseOf(r)
	return isForgePark(r) && p != "" && p != publicationPhase
}

// publicationTarget names the known merge request (preferred) or branch, "" when neither is known.
// Both are server-carried strings, so the branch is folded through cellText.
func publicationTarget(r apitypes.RunDTO) string {
	if in := r.PublicationIntent; in != nil {
		if in.MrIID != nil {
			return " · !" + itoa(int(*in.MrIID))
		}
		if b := cellText(in.Branch); b != "" {
			return " · branch " + b
		}
	}
	if r.MrIID != nil {
		return " · !" + itoa(int(*r.MrIID))
	}
	if b := cellText(strOr(r.Branch, "")); b != "" {
		return " · branch " + b
	}
	return ""
}

// pubDuration renders whole hours as hours ("24h", not shortDuration's "1d") so the limit reads
// the way the operator configured it; anything else uses shortDuration.
func pubDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return itoa(int(d/time.Hour)) + "h"
	}
	return shortDuration(d)
}

// publicationWaitClause is "waiting 12m of 24h" (limit set) or "waiting 12m, no limit".
func publicationWaitClause(r apitypes.RunDTO, now time.Time) string {
	limit := ""
	if r.PublicationWaitLimitSeconds != nil && *r.PublicationWaitLimitSeconds > 0 {
		limit = pubDuration(time.Duration(*r.PublicationWaitLimitSeconds) * time.Second)
	}
	if r.PublicationWaitStartedAt == nil {
		if limit == "" {
			return "waiting, no limit"
		}
		return "waiting, limit " + limit
	}
	elapsed := pubDuration(now.Sub(*r.PublicationWaitStartedAt))
	if limit == "" {
		return "waiting " + elapsed + ", no limit"
	}
	return "waiting " + elapsed + " of " + limit
}

// publicationBlocker is the fixed wording per hold reason; an unknown or missing reason is shown
// honestly as a generic hold.
func publicationBlocker(r apitypes.RunDTO) string {
	reason := strOr(r.PublicationHoldReason, "")
	switch reason {
	case pubHoldRetryExhausted:
		if r.PublicationWaitLimitSeconds != nil && *r.PublicationWaitLimitSeconds > 0 {
			return "the forge stayed unreachable for the whole " +
				pubDuration(time.Duration(*r.PublicationWaitLimitSeconds)*time.Second) + " wait"
		}
		return "the forge stayed unreachable for the whole wait"
	case pubHoldCustodianGone:
		return "the worker holding the unpublished source is gone; the work is local-only"
	case pubHoldModelRequired:
		return "publishing needs model work a publication-only resume does not do"
	case pubHoldSourceUnavailable:
		return "the unpublished work is not available"
	case pubHoldIntentMismatch:
		return "the publication record did not match"
	case "":
		return "reason not reported"
	}
	return "unrecognised hold reason \"" + cellText(reason) + "\""
}

// publicationParkLineAt is the one headline sentence of a publication-phase park. A timed wait is
// "publishing paused: forge unreachable; waiting 12m of 24h, retry at 15:04"; an owner hold is
// "publishing held: <fixed blocker>". now is injected so the elapsed age is testable.
func publicationParkLineAt(r apitypes.RunDTO, now time.Time) string {
	if isForgeOwnerHold(r) {
		return publicationHeldLeadPrefix + publicationBlocker(r) + publicationTarget(r)
	}
	line := "publishing paused: forge unreachable; " + publicationWaitClause(r, now)
	if r.RecoveryRetryNotBefore != nil {
		line += ", retry at " + r.RecoveryRetryNotBefore.Local().Format("15:04")
	} else {
		line += "; retry timing unknown"
	}
	return line + publicationTarget(r)
}

// publicationHoldLines is the owner-hold guidance of a publication park. It never promises
// execution, export or any other budget: Resume starts one new publication-only attempt.
func publicationHoldLines(r apitypes.RunDTO) []string {
	id := providerCommandID(r.ID)
	return []string{
		"Owner Resume or Cancel; no automatic resume.",
		"Resume: uzi run resume " + id + " (starts a new publication-only attempt; no model work)",
		"Cancel: uzi run cancel " + id + " (terminal)",
		"Resume grants no other budget.",
	}
}

// unknownPhaseParkLine is the generic line for a forge park whose phase this client does not know.
func unknownPhaseParkLine(r apitypes.RunDTO) string {
	line := "parked: forge unreachable; recovery phase \"" + cellText(publicationPhaseOf(r)) + "\" not recognised by this client"
	if isForgeOwnerHold(r) {
		return line + "; owner-held, no automatic resume"
	}
	if r.RecoveryRetryNotBefore != nil {
		return line + "; retry at " + r.RecoveryRetryNotBefore.Local().Format("15:04")
	}
	return line + "; retry timing unknown"
}

// unknownPhaseHoldLines are the minimal owner actions for an unrecognised-phase hold.
func unknownPhaseHoldLines(r apitypes.RunDTO) []string {
	id := providerCommandID(r.ID)
	return []string{
		"Owner Resume or Cancel; no automatic resume.",
		"Resume: uzi run resume " + id,
		"Cancel: uzi run cancel " + id + " (terminal)",
	}
}
