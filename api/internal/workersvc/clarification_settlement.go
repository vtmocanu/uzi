package workersvc

import (
	"context"
	"fmt"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// stateClarificationSettled is the worker state report (issue #3985, feature
// clarification_revision_settlement_v1) that returns a plan cross-check run parked at
// awaiting_input, after a clarification asked during a human revision was answered, to the
// awaiting_approval gate it left. It is a wire-enum value, not a credential.
const stateClarificationSettled = "clarification_settled"

// ClarificationSettlement is the clarification_settled report's body: the answered question, the
// pending human revise that stays pending, and the gate revision the revise is bound to.
type ClarificationSettlement struct {
	QuestionID      string `json:"question_id"`
	RevisionInputID int64  `json:"revision_input_id"`
	GateRevision    int64  `json:"gate_revision"`
}

// validateClarificationSettlementShape rejects the nested field on any other status and the
// status without the nested field, then validates the settlement itself. It never touches the
// database, so a malformed report is a 400 before any lock is taken.
func validateClarificationSettlementShape(req StateRequest) error {
	if req.State != stateClarificationSettled {
		if req.ClarificationSettlement != nil {
			return fmt.Errorf("%w: clarification_settlement is valid only on %s", ErrInvalidState, stateClarificationSettled)
		}
		return nil
	}
	_, err := normalizeClarificationSettlement(req)
	return err
}

// normalizeClarificationSettlement returns the settlement with its question id cleaned (NUL
// stripped, trimmed) after checking every field the report must carry.
func normalizeClarificationSettlement(req StateRequest) (ClarificationSettlement, error) {
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return ClarificationSettlement{}, fmt.Errorf("%w: %s requires a positive claim_generation", ErrInvalidState, stateClarificationSettled)
	}
	if req.ClarificationSettlement == nil {
		return ClarificationSettlement{}, fmt.Errorf("%w: %s requires clarification_settlement", ErrInvalidState, stateClarificationSettled)
	}
	st := *req.ClarificationSettlement
	clean, _ := stripNUL(st.QuestionID)
	st.QuestionID = strings.TrimSpace(clean)
	if st.QuestionID == "" {
		return ClarificationSettlement{}, fmt.Errorf("%w: clarification_settlement requires question_id", ErrInvalidState)
	}
	if len([]rune(st.QuestionID)) > maxQuestionIDRunes {
		return ClarificationSettlement{}, fmt.Errorf("%w: clarification_settlement question_id is too long", ErrInvalidState)
	}
	if st.RevisionInputID <= 0 || st.GateRevision <= 0 {
		return ClarificationSettlement{}, fmt.Errorf("%w: clarification_settlement requires positive revision_input_id and gate_revision", ErrInvalidState)
	}
	return st, nil
}

// settleClarificationToGate applies the clarification_settled report (issue #3985): the answered
// clarification asked during a human plan revision is settled by returning the run to the
// awaiting_approval gate WITHOUT passing through running. SetRunRunning is deliberately not
// relaxed: an answer never implies approval, so its bound-approval clause keeps refusing a running
// report here.
//
// The caller (setStateWithTx via the claim-mutation path) holds the worker and run locks and the
// run's fence opt-ins. One fenced statement, SettleRunClarificationToGate, does the move; it
// writes no presentation, no checker pass and does not settle the revise (ADR-2149 D16).
//
// IDEMPOTENT REPLAY: a 0-row result runs the read-only GetClarificationGateSettlementReplay. When
// it is true the settlement already applied (a lost ACK) and the report converges to
// (run, true) with no further write, so the awaiting_input gap is banked once. Otherwise the
// result is (run, false): the handler's ordinary 409 carrying the run's real status.
func (s *Service) settleClarificationToGate(ctx context.Context, owned store.Run, wkr store.Worker, req StateRequest) (store.Run, bool, error) {
	st, err := normalizeClarificationSettlement(req)
	if err != nil {
		return store.Run{}, false, err
	}
	gen := *req.ClaimGeneration
	if owned.ClaimGeneration != gen || owned.ClaimReleasedAt.Valid {
		return owned, false, ErrStaleClaim
	}
	rows, err := s.q.SettleRunClarificationToGate(ctx, store.SettleRunClarificationToGateParams{
		ID:              owned.ID,
		WorkerID:        pgconv.UUID(wkr.ID),
		ClaimGeneration: gen,
		QuestionID:      st.QuestionID,
		GateRevision:    st.GateRevision,
		RevisionInputID: st.RevisionInputID,
	})
	if err != nil {
		return store.Run{}, false, err
	}
	applied := rows > 0
	if !applied {
		applied, err = s.q.GetClarificationGateSettlementReplay(ctx, store.GetClarificationGateSettlementReplayParams{
			ID:              owned.ID,
			WorkerID:        pgconv.UUID(wkr.ID),
			ClaimGeneration: gen,
			GateRevision:    st.GateRevision,
			QuestionID:      st.QuestionID,
			RevisionInputID: st.RevisionInputID,
		})
		if err != nil {
			return store.Run{}, false, err
		}
	}
	cur, err := s.runOwnedByWorker(ctx, owned.ID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	return cur, applied, nil
}
