package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// terminalTargetCancelTimeout bounds the one fenced transaction (lock waits included).
const terminalTargetCancelTimeout = 10 * time.Second

// TerminalTargetObservation is what a caller saw about an active ci_fix / mr_rework run and
// the merge request it targets when it decided the MR left the opened state (issue #4616).
// CancelRunForTerminalTarget re-checks the observation under row locks (kind, repo, target MR,
// source branch, worker, claim generation, no stop_kind) and does nothing when the run moved
// since, so a stale observation can never cancel a reclaimed or re-pointed run. Status is
// carried for the caller's context only: it is not compared, the locked row's own status
// drives the decision (terminal statuses never match).
type TerminalTargetObservation struct {
	RunID uuid.UUID
	// RepoID is the repository the run must belong to (uuid.Nil skips the check).
	RepoID uuid.UUID
	// Kind is the observed run kind: ci_fix or mr_rework.
	Kind string
	// Status is the observed run status.
	Status string
	// WorkerID is the observed owning worker (invalid for a worker-less run).
	WorkerID pgtype.UUID
	// ClaimGeneration is the observed claim generation.
	ClaimGeneration int64
	// TargetMRIID is the MR the run targets (ci_fix_mr_iid or mr_iid by kind).
	TargetMRIID int64
	// SourceBranch is the run's pipeline_ref the verified MR's source branch must equal.
	SourceBranch string
	// State is the verified MR state: forge.MRStateMerged or forge.MRStateClosed.
	State string
}

// terminalTargetPolicy decides when a non-live run may be cancelled server-side.
type terminalTargetPolicy struct {
	// requireTerminalState gates on obs.State being merged/closed.
	requireTerminalState bool
	// serverSide reports whether a run with no live poller in this status is cancelled
	// server-side; any other status is left to its own lifecycle (a no-op here).
	serverSide func(status string) bool
}

var (
	// terminalTargetPolicyPoller is the forge poller's policy: only runs that hold no worker
	// flight (queued, limit_wait, pool_wait) are cancelled server-side. Paused, recovery_wait
	// and stale claimed/running runs are left alone; the publication backstop stops them when
	// they next try to publish.
	terminalTargetPolicyPoller = terminalTargetPolicy{
		requireTerminalState: true,
		serverSide: func(status string) bool {
			return status == "queued" || status == "limit_wait" || status == "pool_wait"
		},
	}
	// terminalTargetPolicyLegacyRework keeps CancelReworkForMR's historical reach: every
	// non-live non-terminal status is cancelled server-side. The MR state was already decided
	// by its caller.
	terminalTargetPolicyLegacyRework = terminalTargetPolicy{
		serverSide: func(string) bool { return true },
	}
)

// terminalTargetStopReason is the server-owned stop reason recorded when a target MR is
// merged or closed. The text is fixed (no forge-controlled content) and sanitized like an
// operator's reason.
func terminalTargetStopReason(state string) string {
	switch state {
	case forge.MRStateMerged:
		return "target MR merged; further publication stopped."
	case forge.MRStateClosed:
		return "target MR closed; further publication stopped."
	}
	return "target MR merged or closed; further publication stopped."
}

// TerminalTargetStopReason returns the stop reason for a terminal target MR state, for
// callers (the MR watcher) that pass it to CancelReworkForMR.
func TerminalTargetStopReason(state string) string { return terminalTargetStopReason(state) }

// IsSafeTerminalTargetBranch and IsFullCommitSHA expose the publication identity rules
// (safePublicationIdentifier, isSettleSHA) to the forge poller so its merged/closed
// verification uses the one definition the completed-publication proof uses. They are
// thin wrappers: forgesvc already imports workersvc, so no shared package is needed.
func IsSafeTerminalTargetBranch(branch string) bool { return safePublicationIdentifier(branch) }

// IsFullCommitSHA reports whether s is a 40-char lowercase hex commit id.
func IsFullCommitSHA(s string) bool { return isSettleSHA(s) }

// CancelRunForTerminalTarget stops one active ci_fix / mr_rework run whose verified target MR
// is merged or closed (issue #4616). A run with a live worker gets a stamped cancel input the
// worker drains; a queued, limit_wait or pool_wait run is cancelled server-side; paused,
// recovery_wait and stale claimed/running runs are left untouched (the publication-context
// backstop covers them). It is a no-op (nil) when the run no longer matches obs, already has
// a stop_kind (an operator verdict is never overwritten) or holds a pending terminal outcome.
// A missing transaction beginner or a store failure is an error.
func (s *Service) CancelRunForTerminalTarget(ctx context.Context, repoID uuid.UUID, obs TerminalTargetObservation) error {
	obs.RepoID = repoID
	return s.cancelTerminalTarget(ctx, obs, terminalTargetPolicyPoller, terminalTargetStopReason(obs.State))
}

// cancelTerminalTarget is the shared fenced cancel: one READ COMMITTED transaction locks the
// observed worker and then the run, re-validates the observation against the locked row, and
// either stamps a cancel input for a live worker or cancels server-side per policy.
func (s *Service) cancelTerminalTarget(ctx context.Context, obs TerminalTargetObservation, policy terminalTargetPolicy, reason string) error {
	if s.txBeginner == nil {
		return errors.New("workersvc: terminal-target cancel requires a transaction beginner")
	}
	if policy.requireTerminalState && obs.State != forge.MRStateMerged && obs.State != forge.MRStateClosed {
		return nil
	}
	if obs.Kind != "ci_fix" && obs.Kind != "mr_rework" {
		return nil
	}
	stopReason := stopReasonParam(reason).String
	if stopReason == "" {
		stopReason = terminalTargetStopReason("")
	}

	txCtx, cancel := context.WithTimeout(ctx, terminalTargetCancelTimeout)
	defer cancel()
	tx, err := s.beginReadCommitted(txCtx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(txCtx) }()
	qt := store.New(tx)

	// Lock order worker -> run, as every other worker-scoped run mutation does.
	if obs.WorkerID.Valid {
		if _, err := qt.GetWorkerForUpdate(txCtx, uuid.UUID(obs.WorkerID.Bytes)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
	}
	run, err := qt.GetRunByIDForUpdate(txCtx, obs.RunID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if !terminalTargetMatches(run, obs) {
		return nil
	}

	if run.WorkerID.Valid {
		pending, err := qt.RunHasPendingOutcomeLease(txCtx, run.ID)
		if err != nil {
			return err
		}
		if pending {
			slog.Info("terminal-target cancel deferred: run holds a pending terminal outcome",
				"event", "terminal_target_cancel_deferred", "run", run.ID.String())
			return nil
		}
	}

	live, err := s.hasLivePollerWith(txCtx, qt, run)
	if err != nil {
		return err
	}
	if live {
		_, err := qt.StampTerminalTargetCancelInput(txCtx, store.StampTerminalTargetCancelInputParams{
			StopReason: stopReason, ID: run.ID, WorkerID: run.WorkerID, ClaimGeneration: run.ClaimGeneration,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		return tx.Commit(txCtx)
	}
	if !policy.serverSide(run.Status) {
		return nil
	}
	rows, err := qt.CancelRunServerSideForTerminalTarget(txCtx, store.CancelRunServerSideForTerminalTargetParams{
		StopReason: stopReason, ID: run.ID, ObservedStatus: run.Status,
		ObservedGeneration: run.ClaimGeneration, MrIid: obs.TargetMRIID,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	if err := tx.Commit(txCtx); err != nil {
		return fmt.Errorf("commit terminal-target cancel: %w", err)
	}
	s.terminalTargetCancelledFanout(ctx, run)
	return nil
}

// terminalTargetMatches re-checks the observation against the row-locked run.
func terminalTargetMatches(run store.Run, obs TerminalTargetObservation) bool {
	if run.Status == "completed" || run.Status == "failed" || run.Status == "cancelled" {
		return false
	}
	if obs.RepoID != uuid.Nil && (!run.RepoID.Valid || uuid.UUID(run.RepoID.Bytes) != obs.RepoID) {
		return false
	}
	if run.Kind != obs.Kind || run.StopKind.Valid {
		return false
	}
	target := run.MrIid
	if run.Kind == "ci_fix" {
		target = run.CiFixMrIid
	}
	if !target.Valid || target.Int64 != obs.TargetMRIID {
		return false
	}
	if !run.PipelineRef.Valid || run.PipelineRef.String != obs.SourceBranch {
		return false
	}
	if run.ClaimGeneration != obs.ClaimGeneration || run.WorkerID != obs.WorkerID {
		return false
	}
	return true
}

// terminalTargetCancelledFanout mirrors the operator's server-side cancel (SubmitInput's
// cancel arm) after a committed terminal-target cancel: state broadcast, notification, judge
// gate (cancel is judge-filtered; kept for parity), the scope-directive audit settle and the
// checkpoint retention decision. Everything here is best-effort and never fails the cancel.
func (s *Service) terminalTargetCancelledFanout(ctx context.Context, run store.Run) {
	if s.bcast != nil { // bcast is optional/post-construction: nil-guard required
		s.bcast.PublishState(run.ID, "cancelled")
	}
	s.notify(run.ID, "cancelled")
	s.maybeEnqueueJudgeByID(ctx, run.ID)
	if run.ScopeCeiling.Valid {
		if _, err := s.q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
			RunID: run.ID, Disposition: pgconv.TextOrNull("declined"),
		}); err != nil {
			slog.Warn("settle scope input disposition (terminal-target cancel)", "run", run.ID, "error", err)
		}
	}
	s.retainOrDeleteCheckpoint(ctx, run.ID, run.Kind, run.IssueIid)
}
