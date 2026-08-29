package forgesvc

import (
	"context"
	"log/slog"
	"path"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/prdpath"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRDLinkPatchBatch bounds one repo's candidates per tick, mirroring
// FiledIssueCloseBatch and for the same reason: an unconsumed edge stays
// unconsumed, so deferring the remainder costs nothing, while an unbounded batch
// would run a forge call per candidate serially and delay every repo behind it.
const PRDLinkPatchBatch = 20

// SyncPRDLinkPatches rewrites the PRD link in an issue's description once the run
// that moved the file has merged (PRD #72 M5).
//
// Only an enumeration failure is returned; a per-candidate failure is logged and
// skipped, so one bad issue cannot stall the repo. Same shape as
// SyncFiledIssueCloses, which is the closest analogue — a small purpose-built
// edge-consuming pass — rather than mr_watch.go, which carries a state machine
// this does not need.
//
// READ-MODIFY-WRITE WINDOW, recorded so it is not rediscovered as a bug: a human
// editing the description between our GetIssue and our write is clobbered. It is
// narrowed to one poller tick and to only the occurrences of this run's own PRD
// path; there is nothing further to do about it without a forge primitive that
// does not exist.
func (s *Service) SyncPRDLinkPatches(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) error {
	rows, err := s.q.ListPRDLinkPatchCandidates(ctx, store.ListPRDLinkPatchCandidatesParams{
		RepoID: repoID,
		Lim:    PRDLinkPatchBatch,
	})
	if err != nil {
		return err
	}
	for _, c := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.patchOnePRDLink(ctx, forgeProjectID, f, c)
	}
	return nil
}

// patchOnePRDLink consumes at most one candidate's edge.
//
// THE ORDER OF THE TERMINAL STATES IS LOAD-BEARING, and this is the safe
// direction: MR state is read BEFORE supersession is consulted, so a superseded
// run whose MR actually merged still gets its patch. Applying supersession first
// would drop it.
//
//  1. GetMergeRequest errors        -> leave the marker; the next tick retries.
//  2. merged                        -> read, rewrite, write, settle.
//  3. closed (unmerged)             -> settle, no patch. The branch is dead, so the
//     moved file never reaches the default branch.
//  4. opened AND superseded         -> settle, no patch. This is what bounds the pass.
//  5. opened, not superseded        -> leave the marker; still in flight.
//  6. locked, or an unknown state   -> leave the marker, log.
//
// CASE 6 IS NOT A CATCH-ALL AND IS THE EASY ONE TO GET WRONG. IsKnownMRState
// recognises FOUR states, not three — opened, closed, merged and locked — and
// mr_watch.go records that locked is TRANSIENT DURING MERGE PROCESSING. Folding it
// into case 3 ("any known non-opened state settles") would drop the patch for an MR
// that is about to merge. Settle-without-patch fires on `closed` and nothing else;
// an unknown state is likewise never a settle, because dropping a patch over a
// state we do not understand is strictly worse than one more tick of polling.
//
// FORGE-WRITE FIRST, THEN SETTLE, the ordering mr_watch.go documents. A crash
// between them re-fires the edge next tick, and the re-run finds the description
// already carrying the new path (zero matches -> settle and log). Idempotent by
// construction, which is why this is safe without the single-statement treatment
// ApplyFiledIssueCloseEdge needed.
//
// RESIDUAL EXPOSURE, recorded rather than discovered later: a run that is
// superseded, whose MR is still open at the tick we settle it, and which merges
// LATER, loses its patch. Narrow — the follow-up run normally moves the PRD itself
// and patches through its own marker — and the alternative is polling an abandoned
// open MR forever, which is the unbounded per-tick forge call the PRD forbids.
func (s *Service) patchOnePRDLink(ctx context.Context, forgeProjectID int64, f forge.Forge, c store.ListPRDLinkPatchCandidatesRow) {
	lg := slog.With("run", c.ID, "issue", c.IssueIid.Int64, "mr", c.MrIid.Int64)

	mr, err := f.GetMergeRequest(ctx, forgeProjectID, c.MrIid.Int64)
	if err != nil {
		// Case 1. A failed read is not evidence about the MR.
		lg.Warn("PRD link patch: read merge request", "error", err)
		return
	}

	switch {
	case mr.State == forge.MRStateMerged:
		// Case 2 — fall through to the patch below.
	case mr.State == forge.MRStateClosed:
		s.settlePRDLink(ctx, lg, c.ID, "PRD link patch: merge request closed without merging; nothing to patch")
		return
	case mr.State == forge.MRStateOpened && c.Superseded:
		s.settlePRDLink(ctx, lg, c.ID, "PRD link patch: run superseded by a newer run on the same issue")
		return
	case mr.State == forge.MRStateOpened:
		return // Case 5: still in flight, leave the marker.
	default:
		// Case 6: locked, or a state this build does not know.
		lg.Info("PRD link patch: merge request state is not terminal; leaving the marker", "state", mr.State)
		return
	}

	// The declared path was validated at write time (clampWirePRDDonePath), so a
	// failure here means the row predates the validator or was written by something
	// else. Cheap to re-check, and it makes this function's assumption explicit
	// rather than inherited.
	declared := c.PrdDonePath.String
	if err := prdpath.Validate(declared); err != nil {
		s.settlePRDLink(ctx, lg, c.ID, "PRD link patch: stored path does not validate; refusing to patch")
		return
	}

	// THE BINDING. The agent's declaration says WHERE THE FILE WENT. It does not say
	// WHICH LINK TO TOUCH. Targets come from the run's own queue-time issue snapshot,
	// which is forge-authoritative on both creation paths and is neither
	// caller- nor agent-supplied — so an agent can only ever redirect a link the
	// ISSUE ITSELF already carried.
	//
	// THAT BOUND IS THE WHOLE CLAIM, and an earlier version of this comment
	// overstated it by adding "never an unrelated entry in a Related PRDs list".
	// That is false: a Related-PRDs entry IS a link the issue carried. If the
	// snapshot lists several PRDs, a declaration whose basename matches any of them
	// can repoint that one. Bounded — same issue, basename must match, must pass
	// Validate, no disclosure, no cross-tenant reach — so the damage is description
	// integrity, not security. Tightening it further is a design question rather
	// than a fix: uzi has no notion of THE PRD when an issue links several.
	//
	// This gate ALSO makes Decision 12's no-op for a link-less run mechanical rather
	// than prompt-level: a run on an issue with no PRD link has a snapshot with no PRD
	// link, so `linked` is empty and no forge write can happen even if the lead
	// declares a path anyway (PRD #764 made a PRD link optional, so this is the common
	// case now). It is therefore load-bearing for TWO independent decisions. Weakening it to
	// "the path just has to look like a PRD path" would silently revert Decision 12
	// to prompt-only enforcement while appearing to touch only the target choice.
	//
	// It fires BEFORE any network access.
	var targets []string
	base := path.Base(declared)
	for _, l := range prdpath.Links(c.IssueDescription) {
		if path.Base(l) == base {
			targets = append(targets, l)
		}
	}
	if len(targets) == 0 {
		s.settlePRDLink(ctx, lg, c.ID, "PRD link patch: declared path does not correspond to any PRD this issue linked")
		return
	}

	issue, err := f.GetIssue(ctx, forgeProjectID, c.IssueIid.Int64)
	if err != nil {
		// Case 1 again: a failed read is not evidence about the description.
		lg.Warn("PRD link patch: read issue", "error", err)
		return
	}

	desc := issue.Description
	changed := 0
	for _, t := range targets {
		out, n := prdpath.ReplacePath(desc, t, declared)
		desc, changed = out, changed+n
	}
	if changed == 0 {
		// One terminal state covering three causes — already patched, a human edited
		// it, or the link was already under prds/done/ — because the action is the
		// same and distinguishing them would need evidence we do not have.
		s.settlePRDLink(ctx, lg, c.ID, "PRD link patch: live description no longer contains the linked PRD path")
		return
	}

	if err := f.UpdateIssueDescription(ctx, forgeProjectID, c.IssueIid.Int64, desc); err != nil {
		// Leave the marker: the write is what we are retrying.
		lg.Warn("PRD link patch: update issue description", "error", err)
		return
	}
	s.settlePRDLink(ctx, lg, c.ID, "PRD link patched")
	lg.Info("PRD link patch applied", "path", declared, "changed", changed)
}

// settlePRDLink consumes the edge and logs why. A settle failure is logged and not
// propagated: the forge write (if any) already happened, and the next tick's
// re-run is idempotent.
func (s *Service) settlePRDLink(ctx context.Context, lg *slog.Logger, runID uuid.UUID, reason string) {
	if _, err := s.q.SettlePRDLinkPatch(ctx, runID); err != nil {
		lg.Error("PRD link patch: settle marker", "error", err)
		return
	}
	lg.Info(reason)
}
