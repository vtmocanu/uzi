package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// stateFollowupFinalize is the worker state report (issue #4479, feature
// followup_finalize_admission_v1) that moves an interactive task run parked at awaiting_followup
// back to running just before the worker finalizes it (idle timeout or owner stop), so the
// publication intent can be enrolled against a running run. It is a wire-enum value, not a
// credential. It is finalize-only: after an applied admission the run never re-parks on this claim.
const stateFollowupFinalize = "followup_finalize"

// followupFinalizingSQLState is the SQLSTATE raised by the run_user_inputs_followup_finalize_trg
// trigger (migration 00495) when a follow_up is inserted for a run whose finalize admission is
// live. It follows the UZ413/UZ414 convention of dedicated guard codes.
const followupFinalizingSQLState = "UZ415"

// The three refusal reasons of a followup_finalize report. They are the wire values of the
// handler's 409 `reason` field. Stale claims keep the existing ErrStaleClaim mapping.
const (
	FollowupFinalizeReasonFollowupPending = "followup_pending"
	FollowupFinalizeReasonCancelPending   = "cancel_pending"
	FollowupFinalizeReasonRefused         = "followup_finalize_refused"
)

var (
	// ErrFollowupFinalizePending refuses the admission because an owner follow-up the run has not
	// yet delivered is pending: the worker must go back and deliver it instead of finalizing.
	ErrFollowupFinalizePending = errors.New("a follow-up is pending for this park")
	// ErrFollowupFinalizeCancelPending refuses the admission because a cancel is deliverable.
	ErrFollowupFinalizeCancelPending = errors.New("a cancel is pending for this run")
	// ErrFollowupFinalizeRefused is every other refusal (wrong kind, terminal, held, stale seq...).
	ErrFollowupFinalizeRefused = errors.New("followup finalize refused")
	// ErrFollowUpFinalizing rejects an owner follow_up submission for an interactive task whose
	// finalize admission is live: the worker is finishing the run and would never deliver it.
	// It is a run-state conflict, hence 409 (reason follow_up_finalizing).
	ErrFollowUpFinalizing = errors.New("run is finishing; follow-up was not delivered")
)

// FollowupFinalizeRefusalReason maps a refusal error to its wire reason. It returns ("", false)
// for every other error so stale-claim and generic handling stay with the caller.
func FollowupFinalizeRefusalReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrFollowupFinalizePending):
		return FollowupFinalizeReasonFollowupPending, true
	case errors.Is(err, ErrFollowupFinalizeCancelPending):
		return FollowupFinalizeReasonCancelPending, true
	case errors.Is(err, ErrFollowupFinalizeRefused):
		return FollowupFinalizeReasonRefused, true
	default:
		return "", false
	}
}

// mapFollowupFinalizingErr translates the finalize-admission trigger's SQLSTATE into
// ErrFollowUpFinalizing and passes every other error through unchanged.
func mapFollowupFinalizingErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == followupFinalizingSQLState {
		return ErrFollowUpFinalizing
	}
	return err
}

// validateFollowupFinalizeShape rejects followup_park_seq on any other status and the status
// without a positive claim_generation and a positive followup_park_seq. It never touches the
// database, so a malformed report is a 400 before any lock is taken.
func validateFollowupFinalizeShape(req StateRequest) error {
	if req.State != stateFollowupFinalize {
		if req.FollowupParkSeq != nil {
			return fmt.Errorf("%w: followup_park_seq is valid only on %s", ErrInvalidState, stateFollowupFinalize)
		}
		return nil
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return fmt.Errorf("%w: %s requires a positive claim_generation", ErrInvalidState, stateFollowupFinalize)
	}
	if req.FollowupParkSeq == nil || *req.FollowupParkSeq <= 0 {
		return fmt.Errorf("%w: %s requires a positive followup_park_seq", ErrInvalidState, stateFollowupFinalize)
	}
	return nil
}

// admitFollowupFinalize applies the followup_finalize report (issue #4479). The caller
// (setStateWithTx via the claim-mutation path) holds the worker and run locks and the run's fence
// opt-ins. One fenced statement, AdmitRunFollowupFinalize, moves awaiting_followup to running for
// the exact park the worker observed. A 0-row result first tries the read-only replay (a lost ACK
// converges to applied with no second write), then classifies the refusal under the same
// transaction so the locks hold: a stale claim keeps ErrStaleClaim, anything else returns one of
// the typed refusal errors with the run's real row.
func (s *Service) admitFollowupFinalize(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	if err := validateFollowupFinalizeShape(req); err != nil {
		return store.Run{}, false, err
	}
	gen, seq := *req.ClaimGeneration, *req.FollowupParkSeq
	if owned.ClaimGeneration != gen || owned.ClaimReleasedAt.Valid {
		return owned, false, ErrStaleClaim
	}
	rows, err := s.q.AdmitRunFollowupFinalize(ctx, store.AdmitRunFollowupFinalizeParams{
		ID: owned.ID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: gen, ParkSeq: seq,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	if rows == 0 {
		replay, err := s.q.GetFollowupFinalizeReplay(ctx, store.GetFollowupFinalizeReplayParams{
			ID: owned.ID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: gen, ParkSeq: seq,
		})
		if err != nil {
			return store.Run{}, false, err
		}
		if !replay {
			return s.refuseFollowupFinalize(ctx, owned, wkr, gen, seq)
		}
	}
	cur, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	return cur, true, nil
}

// refuseFollowupFinalize classifies a refused admission. The order is: a changed or released claim
// is stale; a deliverable cancel on the SAME park (awaiting_followup at that seq, or already
// admitted at that generation and seq) is cancel_pending; a pending follow-up on a still-parked,
// not-stopped run is followup_pending; everything else is followup_finalize_refused.
func (s *Service) refuseFollowupFinalize(ctx context.Context, owned store.Run, wkr store.Worker, gen, seq int64) (store.Run, bool, error) {
	cur, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	if cur.ClaimGeneration != gen || cur.ClaimReleasedAt.Valid {
		return cur, false, ErrStaleClaim
	}
	blockers, err := s.q.GetFollowupFinalizeBlockers(ctx, owned.ID)
	if err != nil {
		return store.Run{}, false, err
	}
	parked := cur.Status == "awaiting_followup" && cur.FollowupParkSeq == seq
	admitted := cur.Status == "running" && cur.FollowupParkSeq == seq &&
		cur.FollowupFinalizeGeneration.Valid && cur.FollowupFinalizeGeneration.Int64 == gen
	switch {
	case (parked || admitted) && blockers.CancelDeliverable:
		return cur, false, ErrFollowupFinalizeCancelPending
	case parked && cur.StopKind.String != "stopped" && blockers.FollowupPending:
		return cur, false, ErrFollowupFinalizePending
	default:
		return cur, false, ErrFollowupFinalizeRefused
	}
}
