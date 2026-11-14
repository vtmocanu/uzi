package workersvc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

var errCodexSwitchExpiry = errors.New("codex pending switch expiry failed")

// expireCodexSwitchOnFreshClaim runs under claimCheckpointRun's savepoint and
// run lock. Same-generation readoption never calls this seam. Every write must
// succeed or the caller rolls back the whole claim, including its generation.
func expireCodexSwitchOnFreshClaim(ctx context.Context, q *store.Queries, run store.Run) (store.Run, error) {
	if run.Harness != harnessCodex || !run.CredentialSwitchRequestedAt.Valid ||
		!run.CredentialSwitchGeneration.Valid || run.CredentialSwitchGeneration.Int64 >= run.ClaimGeneration {
		return run, nil
	}
	if !run.CredentialSwitchOpID.Valid {
		return store.Run{}, errCodexSwitchExpiry
	}
	op := uuid.UUID(run.CredentialSwitchOpID.Bytes)
	payload, err := json.Marshal(map[string]any{
		"outcome": ApplyOutcomeNotApplied, "reason": "expired_by_worker_loss",
		"op_id": op.String(), "claim_generation": run.ClaimGeneration,
		"requested_generation": run.CredentialSwitchGeneration.Int64,
	})
	if err != nil {
		return store.Run{}, err
	}
	n, err := q.ClearCredentialSwitchStampByOp(ctx, store.ClearCredentialSwitchStampByOpParams{ID: run.ID, OpID: run.CredentialSwitchOpID})
	if err != nil {
		return store.Run{}, err
	}
	if n != 1 {
		return store.Run{}, errCodexSwitchExpiry
	}
	generation := pgtype.Int8{Int64: run.ClaimGeneration, Valid: true}
	// A collision retries only sequence allocation, at most 32 times. Any
	// database error or generation loss fails immediately; exhaustion fails closed.
	for attempt := 0; attempt < 32; attempt++ {
		seq, err := q.NextPlanCrossCheckMessageSeq(ctx, store.NextPlanCrossCheckMessageSeqParams{
			LeadRunID: run.ID, ClaimGeneration: run.ClaimGeneration,
		})
		if err != nil {
			return store.Run{}, err
		}
		inserted, err := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID: run.ID, Seq: seq, Kind: "credential_switch", Payload: payload, ClaimGeneration: generation,
		})
		if err != nil {
			return store.Run{}, err
		}
		if !inserted.GenerationLive {
			return store.Run{}, errCodexSwitchExpiry
		}
		if !inserted.Inserted {
			continue
		}
		n, err := q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: run.ID, Seq: seq, ClaimGeneration: generation})
		if err != nil {
			return store.Run{}, err
		}
		if n != 1 {
			return store.Run{}, errCodexSwitchExpiry
		}
		// Reload the trigger-cleared pending target and the advanced LastSeq.
		return q.GetRunByID(ctx, run.ID)
	}
	return store.Run{}, errCodexSwitchExpiry
}
