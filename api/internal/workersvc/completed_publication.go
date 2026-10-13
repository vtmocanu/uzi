package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Proof makes at most one summary, branch and comparison request, under one
// deadline. A refusal affects only this hold; completion remains committed.
const completedPublicationDeadline = 5 * time.Second

type completedPublicationReader interface {
	GetCompletedPublicationHold(context.Context, store.GetCompletedPublicationHoldParams) (store.RecoveryCustodyHold, error)
	GetCompletedPublicationBinding(context.Context, store.GetCompletedPublicationBindingParams) (store.GetCompletedPublicationBindingRow, error)
	RecordCompletedPublicationRefusal(context.Context, store.RecordCompletedPublicationRefusalParams) (int64, error)
}

func completedPublicationCapable(w store.Worker, req StateRequest) bool {
	return req.State == "completed" && req.CompletionFinalHead != nil && req.ClaimGeneration != nil &&
		slices.Contains(w.ProtocolCapabilities, capability.RecoveryCompletedPublicationV1)
}

func completedPublicationHead(w store.Worker, req StateRequest) pgtype.Text {
	if completedPublicationCapable(w, req) {
		return pgconv.Text(*req.CompletionFinalHead)
	}
	return pgtype.Text{}
}

func validateCompletedPublicationReport(w store.Worker, req StateRequest) error {
	if req.CompletionFinalHead == nil {
		return nil
	}
	if req.State != "completed" || !isSettleSHA(*req.CompletionFinalHead) {
		return ErrInvalidState
	}
	if slices.Contains(w.ProtocolCapabilities, capability.RecoveryCompletedPublicationV1) && req.ClaimGeneration == nil {
		return ErrMissingClaimGeneration
	}
	return nil
}

func completedPublicationIdentity(h store.RecoveryCustodyHold, w store.Worker, runID uuid.UUID, req StateRequest) (apitypes.CompletionIdentity, bool) {
	var id apitypes.CompletionIdentity
	if !completedPublicationCapable(w, req) || json.Unmarshal(h.CompletionIdentity, &id) != nil {
		return id, false
	}
	return id, h.InventoryGuarded && h.RunID == runID && h.UserID == w.UserID && h.OriginalWorkerID == w.ID &&
		h.Generation == *req.ClaimGeneration && id.HoldID == h.ID.String() && id.RunID == runID.String() &&
		id.OwnerID == w.UserID.String() && id.WorkerID == w.ID.String() && id.Generation == *req.ClaimGeneration &&
		id.FinalHead == *req.CompletionFinalHead && isSettleSHA(id.FinalHead) && h.RepoID.Valid &&
		id.RepoID == uuid.UUID(h.RepoID.Bytes).String() && id.ConnectionID != "" &&
		id.ProjectID > 0 && id.ForgeType != "" && id.BaseURL != ""
}

func completedPublicationReceipt(h store.RecoveryCustodyHold, id apitypes.CompletionIdentity) *apitypes.CompletedPublicationReceipt {
	if h.State != "released" || h.ReleaseEvidence.String != "completed_publication" || h.FinalDisposition.String != "completed_publication" {
		return nil
	}
	var receipt apitypes.CompletedPublicationReceipt
	if json.Unmarshal(h.CompletedPublicationReceipt, &receipt) != nil || !isSettleSHA(receipt.ObservedBranchHead) {
		return nil
	}
	a, err := json.Marshal(receipt.CompletionIdentity)
	if err != nil {
		return nil
	}
	b, err := json.Marshal(id)
	if err != nil || string(a) != string(b) {
		return nil
	}
	return &receipt
}

// Replay precedes mutable run/forge eligibility. It authenticates original hold
// ownership and the delivered head; it never grants authority over another hold.
func (s *Service) replayCompletedPublication(ctx context.Context, w store.Worker, runID uuid.UUID, req StateRequest) (*StateReportResult, error) {
	if !completedPublicationCapable(w, req) {
		return nil, nil
	}
	q, ok := s.q.(completedPublicationReader)
	if !ok {
		return nil, nil
	}
	h, err := q.GetCompletedPublicationHold(ctx, store.GetCompletedPublicationHoldParams{
		RunID: runID, UserID: w.UserID, WorkerID: w.ID, Generation: *req.ClaimGeneration,
	})
	if err != nil {
		return nil, nil
	}
	id, matches := completedPublicationIdentity(h, w, runID, req)
	if !matches {
		return nil, nil
	}
	if req.MrIID != nil && (id.MRIID == nil || *req.MrIID != *id.MRIID) {
		return nil, nil
	}
	// The frozen API branch is proof authority; req.Branch is worker-reported text.
	if req.Head != nil && *req.Head != id.FinalHead {
		return nil, nil
	}
	if receipt := completedPublicationReceipt(h, id); receipt != nil {
		// Receipt authority survives run deletion or a successor claim. This terminal
		// ACK projects only authenticated frozen fields; ancillary run data is unknown.
		run := store.Run{
			ID: runID, UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), RepoID: h.RepoID,
			Status: "completed", ClaimGeneration: id.Generation, Branch: pgconv.Text(id.Branch),
		}
		if id.MRIID != nil {
			run.MrIid = pgconv.Int8Ptr(id.MRIID)
		}
		return &StateReportResult{Run: run, Applied: true, CompletedPublicationReceipt: receipt}, nil
	}
	run, err := s.GetRun(ctx, w.UserID, runID)
	if err != nil {
		return nil, err
	}
	if run.Status == "completed" && run.ClaimGeneration == id.Generation && run.WorkerID == pgconv.UUID(w.ID) {
		receipt, reason := s.verifyCompletedPublication(ctx, w, runID, req)
		return &StateReportResult{Run: run, Applied: true, CompletedPublicationReceipt: receipt, CompletedPublicationReason: reason}, nil
	}
	return nil, nil
}

func safePublicationIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func completedPublicationError(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "forge_timeout"
	}
	return "ancestry_unknown"
}

func proveCompletedPublication(ctx context.Context, f forge.Forge, id apitypes.CompletionIdentity) (string, string) {
	if id.MRIID == nil || *id.MRIID <= 0 {
		return "", "mr_missing"
	}
	if !safePublicationIdentifier(id.Branch) {
		return "", "branch_missing"
	}
	summary, err := f.GetMergeRequestSummary(ctx, id.ProjectID, *id.MRIID)
	if errors.Is(err, forge.ErrMergeRequestNotFound) {
		return "", "mr_missing"
	}
	if err != nil {
		return "", completedPublicationError(ctx, err)
	}
	if !safePublicationIdentifier(summary.SourceBranch) || summary.SourceBranch != id.Branch {
		return "", "branch_mismatch"
	}
	head, err := f.BranchHead(ctx, id.ProjectID, id.Branch)
	if errors.Is(err, forge.ErrRefNotFound) {
		return "", "branch_missing"
	}
	if err != nil {
		return "", completedPublicationError(ctx, err)
	}
	if !isSettleSHA(head) || !isSettleSHA(summary.HeadSHA) || summary.HeadSHA != head {
		return "", "head_mismatch"
	}
	answer, err := f.CompareAncestry(ctx, id.ProjectID, head, id.FinalHead)
	if err != nil {
		return "", completedPublicationError(ctx, err)
	}
	switch answer {
	case forge.AncestryAncestor:
		if ctx.Err() != nil {
			return "", completedPublicationError(ctx, ctx.Err())
		}
		return head, ""
	case forge.AncestryNotAncestor:
		return "", "not_ancestor"
	default:
		return "", "ancestry_unknown"
	}
}

func (s *Service) verifyCompletedPublication(ctx context.Context, w store.Worker, runID uuid.UUID, req StateRequest) (*apitypes.CompletedPublicationReceipt, string) {
	q, ok := s.q.(completedPublicationReader)
	if !ok || !completedPublicationCapable(w, req) {
		return nil, "completion_identity_missing"
	}
	h, err := q.GetCompletedPublicationHold(ctx, store.GetCompletedPublicationHoldParams{
		RunID: runID, UserID: w.UserID, WorkerID: w.ID, Generation: *req.ClaimGeneration,
	})
	if err != nil || len(h.CompletionIdentity) == 0 {
		return nil, "completion_identity_missing"
	}
	id, matches := completedPublicationIdentity(h, w, runID, req)
	if !matches {
		return nil, "identity_changed"
	}
	if receipt := completedPublicationReceipt(h, id); receipt != nil {
		return receipt, ""
	}
	var writeTx pgx.Tx
	refuse := func(reason string) (*apitypes.CompletedPublicationReceipt, string) {
		// The identity CAS and absent-receipt predicate prevent a delayed refusal
		// from overwriting a successful concurrent proof.
		if writeTx != nil {
			_ = writeTx.Rollback(ctx)
			writeTx = nil
		}
		writeCtx, cancel := context.WithTimeout(ctx, completedPublicationDeadline)
		defer cancel()
		_, _ = q.RecordCompletedPublicationRefusal(writeCtx, store.RecordCompletedPublicationRefusalParams{
			HoldID: h.ID, RunID: runID, UserID: w.UserID, WorkerID: w.ID, Generation: id.Generation,
			Identity: h.CompletionIdentity, Reason: pgconv.Text(reason),
		})
		slog.Info("completed publication retained", "run", runID, "hold", h.ID, "reason", reason)
		return nil, reason
	}
	if s.txBeginner == nil || s.forges == nil {
		return refuse("ancestry_unknown")
	}
	proofCtx, cancel := context.WithTimeout(ctx, completedPublicationDeadline)
	defer cancel()
	binding, err := q.GetCompletedPublicationBinding(proofCtx, store.GetCompletedPublicationBindingParams{
		RepoID: uuid.MustParse(id.RepoID), UserID: w.UserID,
	})
	if err != nil {
		return refuse(completedPublicationError(proofCtx, err))
	}
	if binding.ConnectionID.String() != id.ConnectionID || binding.ProjectID != id.ProjectID ||
		binding.ForgeType != id.ForgeType || binding.BaseUrl != id.BaseURL {
		return refuse("identity_changed")
	}
	f, err := s.forges.ForgeForConnection(binding.ForgeType, binding.BaseUrl, binding.TokenCiphertext)
	if err != nil {
		return refuse("ancestry_unknown")
	}
	head, reason := proveCompletedPublication(proofCtx, f, id)
	if reason != "" {
		return refuse(reason)
	}
	// No forge call holds a database lock. All mutation uses worker -> run ->
	// exact hold, then repository/configuration row locks and a final identity CAS.
	writeCtx, writeCancel := context.WithTimeout(ctx, completedPublicationDeadline)
	defer writeCancel()
	tx, err := s.txBeginner.Begin(writeCtx)
	if err != nil {
		return refuse("ancestry_unknown")
	}
	writeTx = tx
	defer func() { _ = tx.Rollback(writeCtx) }()
	qt := store.New(tx)
	if _, err = qt.GetWorkerForUpdate(writeCtx, w.ID); err != nil {
		return refuse("identity_changed")
	}
	run, err := qt.GetRunOwnedByWorkerForUpdate(writeCtx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(w.ID)})
	if err != nil || run.UserID != w.UserID || run.ClaimGeneration != id.Generation || run.ClaimReleasedAt.Valid {
		return refuse("identity_changed")
	}
	if run.Status != "completed" {
		return refuse("not_completed")
	}
	locked, err := qt.GetFinalInventoryHold(writeCtx, store.GetFinalInventoryHoldParams{
		RunID: runID, UserID: w.UserID, WorkerID: w.ID, Generation: id.Generation,
	})
	if err != nil {
		return refuse("identity_changed")
	}
	if receipt := completedPublicationReceipt(locked, id); receipt != nil {
		return receipt, ""
	}
	if locked.ID != h.ID || string(locked.CompletionIdentity) != string(h.CompletionIdentity) || locked.State != "open" {
		return refuse("identity_changed")
	}
	current, err := qt.LockCompletedPublicationBinding(writeCtx, store.LockCompletedPublicationBindingParams{RepoID: uuid.MustParse(id.RepoID), UserID: w.UserID})
	if err != nil || current.ConnectionID.String() != id.ConnectionID || current.ProjectID != id.ProjectID ||
		current.ForgeType != id.ForgeType || current.BaseUrl != id.BaseURL {
		return refuse("identity_changed")
	}
	n, err := qt.ReleaseCompletedPublicationHold(writeCtx, store.ReleaseCompletedPublicationHoldParams{
		HoldID: h.ID, RunID: runID, UserID: w.UserID, WorkerID: w.ID,
		Generation: id.Generation, Identity: h.CompletionIdentity, ObservedBranchHead: head,
	})
	if err != nil || n != 1 {
		_ = tx.Rollback(writeCtx)
		return refuse("identity_changed")
	}
	if err = tx.Commit(writeCtx); err != nil {
		return refuse("ancestry_unknown")
	}
	s.SettleRetainedCheckpoint(runID)
	return &apitypes.CompletedPublicationReceipt{CompletionIdentity: id, ObservedBranchHead: head}, ""
}
