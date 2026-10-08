package recovery

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Reconcile locks worker, run, exact original hold, then capture. Its fence and
// chunk deletion commit together before replacement authority is returned.
func (s *Service) Reconcile(ctx context.Context, wkr store.Worker, runID, captureID uuid.UUID, req apitypes.RecoveryReconcileRequest) (apitypes.RecoveryReconcileResponse, error) {
	res := apitypes.RecoveryReconcileResponse{RunID: runID.String(), Generation: req.Generation, CaptureID: captureID.String(), Outcome: "retained"}
	sha, err := validateSha(req.SourceSha, false)
	if err != nil || sha != req.SourceSha || req.Generation <= 0 || req.ByteSize < 0 ||
		!validCoverageDigest(req.CoverageDigest) || !isSHA256Hex(req.Checksum) ||
		!slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryInventoryV1) {
		return res, ErrBadRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.RequestDeadline)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	authError := func(err error) error {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotAuthorized
		}
		return err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT id FROM workers WHERE id=$1 AND user_id=$2 FOR UPDATE", wkr.ID, wkr.UserID).Scan(&id); err != nil {
		return res, authError(err)
	}
	var status string
	var gen int64
	var released pgtype.Timestamptz
	if err := tx.QueryRow(ctx, "SELECT status, claim_generation, claim_released_at FROM runs WHERE id=$1 AND user_id=$2 FOR UPDATE", runID, wkr.UserID).Scan(&status, &gen, &released); err != nil {
		return res, authError(err)
	}
	q := store.New(tx)
	hold, err := q.GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation})
	if err != nil {
		return res, authError(err)
	}
	if !hold.InventoryGuarded {
		return res, ErrNotAuthorized
	}
	// An accepted identity is authoritative even after expiry, and may differ
	// from this request. Echo it unchanged; only the caller can exact-match it.
	if hold.State == "released" {
		if hold.FinalDisposition.String == "archive" || hold.FinalDisposition.String == "settled" {
			res.Outcome = "accepted"
			res.FinalReceipt = &apitypes.RecoveryFinalDisposition{Kind: hold.FinalDisposition.String, SourceSha: hold.FinalSourceSha.String, CoverageDigest: hold.FinalCoverageDigest.String}
			if hold.FinalCaptureID.Valid {
				res.FinalReceipt.CaptureID = uuid.UUID(hold.FinalCaptureID.Bytes).String()
			}
			if hold.FinalDisposition.String == "settled" {
				res.ReleaseEvidence = hold.ReleaseEvidence.String
			}
		} else {
			res.Reason = "hold_released_without_final_receipt"
		}
		return res, nil
	}
	if hold.State != "open" || hold.FinalDisposition.Valid || hold.FinalCaptureID.Valid {
		res.Reason = "hold_not_open"
		return res, nil
	}
	if !hold.LiveWorkerID.Valid || uuid.UUID(hold.LiveWorkerID.Bytes) != wkr.ID {
		res.Reason = "hold_not_original_worker"
		return res, nil
	}
	ended := gen > req.Generation || (gen == req.Generation && (released.Valid || custodyRunTerminalStatuses[status]))
	if !ended {
		res.Reason = "generation_not_ended"
		return res, nil
	}
	capture, err := q.GetFinalInventoryCapture(ctx, store.GetFinalInventoryCaptureParams{ID: captureID, HoldID: hold.ID, RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID})
	if err != nil {
		return res, authError(err)
	}
	if capture.SourceSha != req.SourceSha || !capture.CoverageDigest.Valid || capture.CoverageDigest.String != req.CoverageDigest {
		return res, ErrManifestConflict
	}
	res.Reason = reconcileRetainedReason(capture, req, time.Now())
	if res.Reason != "" {
		return res, nil
	}
	n, err := q.FenceUnacceptedInventoryCapture(ctx, store.FenceUnacceptedInventoryCaptureParams{ID: captureID, HoldID: hold.ID, RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: req.Generation})
	if err != nil {
		return res, err
	}
	if n != 1 {
		res.Reason = "capture_not_replaceable"
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	res.Outcome = "replaceable"
	return res, nil
}

// reconcileRetainedReason returns only bounded protocol reasons. An empty result
// means the locked capture may be fenced, never that a receipt was accepted.
func reconcileRetainedReason(c store.RecoveryCapture, req apitypes.RecoveryReconcileRequest, now time.Time) string {
	if (c.Checksum.Valid && !strings.EqualFold(c.Checksum.String, req.Checksum)) ||
		(c.ByteSize.Valid && c.ByteSize.Int64 != req.ByteSize) {
		return "capture_manifest_integrity_mismatch"
	}
	if c.State == "available" && !c.ManifestBound {
		return "available_capture_manifest_unbound"
	}
	if c.ManifestBound && (!c.Checksum.Valid || !isSHA256Hex(c.Checksum.String) || !c.ByteSize.Valid || c.ByteSize.Int64 < 0 || !c.ChunkCount.Valid || c.ChunkCount.Int32 < 0) {
		return "capture_manifest_invalid"
	}
	if c.Reason.String == "archive integrity check failed" {
		return "capture_failure_requires_attention"
	}
	switch c.State {
	case "needs_action":
		if !slices.Contains([]string{"upload_retry_window_exhausted", "storage quota exceeded", "upload failed; retry available"}, c.Reason.String) {
			return "capture_failure_requires_attention"
		}
	case "preparing", "uploading", "discarded":
		if c.Reason.Valid && c.Reason.String != "" {
			return "capture_failure_requires_attention"
		}
	case "expired":
		// Explicit expiry, including a lost fence response, is replacement authority
		// after the integrity and malformed-manifest checks above.
	case "available":
		if c.Reason.Valid && c.Reason.String != "" {
			return "capture_failure_requires_attention"
		}
		if !c.ExpiresAt.Valid || c.ExpiresAt.InfinityModifier != pgtype.Finite {
			return "available_capture_expiry_invalid"
		}
		if c.ExpiresAt.Time.After(now) {
			return "capture_available"
		}
	default:
		return "capture_state_unknown"
	}
	return ""
}
