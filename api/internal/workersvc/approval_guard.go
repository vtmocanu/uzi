package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ErrPlanRevisionPending prevents approval until the worker settles feedback.
var ErrPlanRevisionPending = errors.New("plan revision is pending; wait for the revised plan before approving")

// requireNoPendingRevision must be a new statement after GetRunByIDForUpdate,
// so READ COMMITTED observes revisions committed by a preceding lock holder.
func requireNoPendingRevision(ctx context.Context, tx pgx.Tx, run store.Run) error {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_user_inputs
 WHERE run_id=$1 AND kind='revise_plan' AND gate_binding='bound'
 AND gate_revision=$2 AND applied_at IS NULL
 AND disposition IS DISTINCT FROM 'superseded')`, run.ID, run.GateRevision).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return ErrPlanRevisionPending
	}
	return nil
}

// approvalQueries permits injected transactional stores to preserve their query
// implementation. Production binds every query to the pgx transaction.
func approvalQueries(tx pgx.Tx) Store {
	if bound, ok := tx.(interface{ ApprovalStore() Store }); ok {
		return bound.ApprovalStore()
	}
	return store.New(tx)
}

func (s *Service) admitApproval(ctx context.Context, owner, runID uuid.UUID, body string, sel *AgentSelection, opts SubmitInputOptions) (SubmitInputResult, error) {
	beginner := s.txBeginner
	if beginner == nil {
		beginner, _ = s.q.(TxBeginner)
	}
	if beginner == nil {
		return SubmitInputResult{}, errors.New("approval requires transaction support")
	}
	// Keep the transaction opener local: the bound facade must never open a nested transaction.
	opener := &Service{txBeginner: beginner}
	tx, err := opener.beginReadCommitted(ctx)
	if err != nil {
		return SubmitInputResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := approvalQueries(tx)
	run, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubmitInputResult{}, ErrRunNotFound
	}
	if err != nil {
		return SubmitInputResult{}, err
	}
	if run.UserID != owner {
		return SubmitInputResult{}, ErrRunNotFound
	}
	// Stop may commit after the outer read; recheck its refusal under the lock.
	if len(run.PolicyStopResolution) != 0 {
		return SubmitInputResult{}, stopReviewConflict("policy_stop_gate_resolved")
	}
	// Preserve expected-revision and terminal refusal precedence.
	if opts.ExpectedGateRevision != nil {
		if err := checkExpectedGateRevision(run, *opts.ExpectedGateRevision); err != nil {
			return SubmitInputResult{}, err
		}
	}
	if terminalStatuses[run.Status] {
		return SubmitInputResult{}, ErrRunTerminal
	}
	if err := requireNoPendingRevision(ctx, tx, run); err != nil {
		return SubmitInputResult{}, err
	}
	locked := s.boundClaimControlService(q)
	result, err := locked.submitInputForRun(ctx, owner, runID, run, "approve_plan", body, sel, opts)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SubmitInputResult{}, err
	}
	return result, nil
}
