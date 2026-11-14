package workersvc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CodexCredentialSwitchApplyFeature advertises the transactional Codex apply response.
const CodexCredentialSwitchApplyFeature = "codex_credential_switch_apply_v1" //nolint:gosec // G101: protocol vocabulary, not a credential.

// codexSwitchMaterial is transaction-local. Never attach it to a preflight result.
type codexSwitchMaterial struct {
	auth            ClaimCodexSecrets
	label           string
	revision        int64
	key             pgtype.Text
	accountRevision pgtype.Int8
}

// applyCodexSwitchLocked owns the commit: every error leaves the caller's deferred
// rollback to undo binding, mint, journal, stamp and message together.
func (s *Service) applyCodexSwitchLocked(ctx context.Context, q *store.Queries, tx pgx.Tx, w store.Worker, run store.Run, req CredentialSwitchApplyRequest, op uuid.UUID, cumulative []byte) (CredentialSwitchApplyResult, error) {
	var target codexSwitchMaterial
	decision, err := s.validateCodexSwitchLocked(ctx, q, w, run, CodexSwitchPreflightRequest{
		CredentialSwitchApplyRequest: req, TargetSecretID: uuid.UUID(run.CodexSwitchTargetSecretID.Bytes),
	}, &target)
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if decision.Outcome != "ready" {
		return CredentialSwitchApplyResult{Outcome: decision.Outcome, Reason: decision.Reason}, nil
	}
	n, err := q.ApplyRunCodexBinding(ctx, store.ApplyRunCodexBindingParams{
		ID: run.ID, UserID: run.UserID, WorkerID: w.ID, ClaimGeneration: run.ClaimGeneration,
		OpID: op, RegisterNonce: req.RegisterNonce,
		TargetSecretID: uuid.UUID(run.CodexSwitchTargetSecretID.Bytes),
		AuthMode:       target.auth.AuthMode, Label: pgconv.TextOrNull(target.label),
		MaterialRevision: target.revision, AccountKey: target.key, AccountRevision: target.accountRevision,
		OldSecretID: run.CodexSecretID, OldAuthMode: run.CodexAuthMode,
		OldMaterialRevision: run.CodexMaterialRevision, OldAccountKey: run.CodexAccountKey,
		OldAccountRevision: run.CodexAccountRevision, OldClaimEpoch: run.CodexClaimEpoch,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if n != 1 {
		return CredentialSwitchApplyResult{}, ErrStaleClaim
	}
	plaintext, hash := mintCodexCapability()
	capabilityEpoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		ID: run.ID, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: run.ClaimGeneration, Hash: hash,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	target.auth.Capability = formatCodexCapability(capabilityEpoch, plaintext)
	// Validate the entire discriminated auth bundle before committing any write.
	if target.auth.AccessToken == "" || capabilityEpoch <= 0 {
		return CredentialSwitchApplyResult{}, errors.New("incomplete Codex switch material")
	}
	if _, err := json.Marshal(target.auth); err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	maxSeq, err := q.MaxRunMessageSeq(ctx, run.ID)
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	seq := max(run.LastSeq, maxSeq) + 1
	epoch, err := q.InsertCodexRunCredentialSwitchEpoch(ctx, store.InsertCodexRunCredentialSwitchEpochParams{
		RunID: run.ID, UserID: run.UserID, WorkerID: w.ID, ClaimGeneration: run.ClaimGeneration,
		CapabilityEpoch: capabilityEpoch, OpID: op, MessageSeq: seq, OldCumulative: cumulative,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	n, err = q.ClearCredentialSwitchStampByOp(ctx, store.ClearCredentialSwitchStampByOpParams{ID: run.ID, OpID: pgconv.UUID(op)})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if n != 1 {
		return CredentialSwitchApplyResult{}, ErrStaleClaim
	}
	payload, err := json.Marshal(credentialSwitchMessagePayload{
		Label: labelPtr(target.label), SelectReason: "pinned",
		SecretID: uuid.UUID(run.CodexSwitchTargetSecretID.Bytes).String(), Ordinal: epoch.Ordinal,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	ins, err := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID: run.ID, Seq: seq, Kind: credentialSwitchMessageKind, Payload: payload,
		ClaimGeneration: pgconv.Int8Ptr(&run.ClaimGeneration),
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if !ins.Inserted {
		return CredentialSwitchApplyResult{}, errors.New("Codex switch message sequence occupied")
	}
	n, err = q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{
		ID: run.ID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&run.ClaimGeneration),
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if n != 1 {
		return CredentialSwitchApplyResult{}, ErrStaleClaim
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if s.bcast != nil {
		s.bcast.PublishMessage(run.ID, seq, credentialSwitchMessageKind, "", "", "", payload, s.now())
		s.bcast.PublishState(run.ID, run.Status)
	}
	return CredentialSwitchApplyResult{Outcome: ApplyOutcomeApplied, Codex: &target.auth,
		CapabilityEpoch: capabilityEpoch, Ordinal: epoch.Ordinal, MessageSeq: seq}, nil
}
