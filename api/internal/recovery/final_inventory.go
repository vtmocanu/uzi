package recovery

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const emptyInventoryDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func validCoverageDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	bytes, err := hex.DecodeString(digest)
	return err == nil && hex.EncodeToString(bytes) == digest
}

// releaseFinalInventory locks worker, run, hold, then capture. All proof and release
// mutations share this transaction; custody hooks run only after its commit.
func (s *Service) releaseFinalInventory(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.RecoveryReleaseRequest) (apitypes.RecoveryReleaseResponse, error) {
	f := req.FinalDisposition
	if req.Generation == nil || *req.Generation <= 0 ||
		!slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryInventoryV1) ||
		!validCoverageDigest(f.CoverageDigest) {
		return apitypes.RecoveryReleaseResponse{}, ErrBadRequest
	}
	var capture pgtype.UUID
	switch f.Kind {
	case "archive":
		id, err := uuid.Parse(f.CaptureID)
		sha, shaErr := validateSha(f.SourceSha, false)
		if err != nil || shaErr != nil || sha != f.SourceSha {
			return apitypes.RecoveryReleaseResponse{}, ErrBadRequest
		}
		capture = pgconv.UUID(id)
	case "settled":
		if f.CaptureID != "" || f.SourceSha != "" || f.CoverageDigest != emptyInventoryDigest ||
			req.ReleaseEvidence == nil || (*req.ReleaseEvidence != "publication" && *req.ReleaseEvidence != "forge_no_output") {
			return apitypes.RecoveryReleaseResponse{}, ErrBadRequest
		}
	default:
		return apitypes.RecoveryReleaseResponse{}, ErrBadRequest
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT id FROM workers WHERE id=$1 AND user_id=$2 FOR UPDATE", wkr.ID, wkr.UserID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	var status string
	var gen int64
	var worker pgtype.UUID
	var released pgtype.Timestamptz
	var parkCause pgtype.Text
	err = tx.QueryRow(ctx, "SELECT status, claim_generation, worker_id, claim_released_at, recovery_wait_cause FROM runs WHERE id=$1 AND user_id=$2 FOR UPDATE",
		runID, wkr.UserID).Scan(&status, &gen, &worker, &released, &parkCause)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	hold, err := store.New(tx).GetFinalInventoryHold(ctx, store.GetFinalInventoryHoldParams{
		RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: *req.Generation,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReleaseResponse{}, err
	}
	ack := apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: true, HoldsReleased: 1, Generation: req.Generation}
	// Retry authority is immutable original ownership, never the now-null live FK.
	if !hold.InventoryGuarded {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	if hold.State == "released" {
		if hold.FinalDisposition.String == f.Kind && hold.FinalCaptureID == capture &&
			hold.FinalSourceSha.String == f.SourceSha && hold.FinalCoverageDigest.String == f.CoverageDigest &&
			(f.Kind == "archive" || hold.ReleaseEvidence.String == *req.ReleaseEvidence) {
			return ack, nil
		}
		return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
	}
	if hold.State != "open" || !hold.InventoryGuarded || !hold.LiveWorkerID.Valid ||
		uuid.UUID(hold.LiveWorkerID.Bytes) != wkr.ID {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	// Requeue exhaustion releases the claim but leaves custody an owner-only decision
	// (Resume/Cancel); a worker's archive or settled proof must not end it implicitly.
	if status == "recovery_wait" && parkCause.Valid && parkCause.String == "worker_requeue_exhausted" {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	// A forge_unreachable park is a pre-clone park of a generation that may have adopted nothing:
	// the parked run keeps its claim, so only a settled forge_no_output release (the worker's
	// positive no-adopted-source proof, empty digest) may end that exact generation early. An
	// archive release and a publication release keep the terminal/released/newer-claim gate.
	forgeParkedEmpty := f.Kind == "settled" && *req.ReleaseEvidence == "forge_no_output" &&
		status == "recovery_wait" && parkCause.Valid && parkCause.String == "forge_unreachable"
	ended := gen > *req.Generation || (gen == *req.Generation && (released.Valid || custodyRunTerminalStatuses[status] || forgeParkedEmpty))
	if !ended {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	evidence := "archive"
	if f.Kind == "settled" {
		if *req.ReleaseEvidence == "publication" && (status != "completed" || gen != *req.Generation || !worker.Valid || uuid.UUID(worker.Bytes) != wkr.ID) {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
		// forge_no_output is the existing authenticated fresh-forge proof declaration.
		// Both settled classes additionally require the exact generation to have ended.
		evidence = *req.ReleaseEvidence
	} else {
		var cap store.RecoveryCapture
		cap, err = store.New(tx).GetFinalInventoryCapture(ctx, store.GetFinalInventoryCaptureParams{
			ID: uuid.UUID(capture.Bytes), HoldID: hold.ID, RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
			}
			return apitypes.RecoveryReleaseResponse{}, err
		}
		if cap.SourceSha != f.SourceSha || cap.CoverageDigest.String != f.CoverageDigest ||
			!cap.ManifestBound || len(cap.PrerequisiteShas) != 0 || cap.State != "available" || !cap.ExpiresAt.Valid || !cap.ExpiresAt.Time.After(time.Now()) {
			return apitypes.RecoveryReleaseResponse{}, ErrManifestConflict
		}
		if s.limits.ReadyRetention < time.Second {
			return apitypes.RecoveryReleaseResponse{}, ErrBadRequest
		}
		n, err := store.New(tx).ProtectFinalInventoryCapture(ctx, store.ProtectFinalInventoryCaptureParams{
			ID: cap.ID, HoldID: hold.ID, WorkerID: wkr.ID, RetentionSeconds: int64(s.limits.ReadyRetention.Seconds()),
			SourceSha: f.SourceSha, CoverageDigest: pgconv.TextOrNull(f.CoverageDigest),
		})
		if err != nil {
			return apitypes.RecoveryReleaseResponse{}, err
		}
		if n != 1 {
			return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
		}
	}
	n, err := store.New(tx).ReleaseFinalInventoryHold(ctx, store.ReleaseFinalInventoryHoldParams{
		ID: hold.ID, RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: *req.Generation,
		FinalDisposition: f.Kind, FinalCaptureID: capture, FinalSourceSha: pgconv.TextOrNull(f.SourceSha),
		FinalCoverageDigest: f.CoverageDigest, ReleaseEvidence: evidence,
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if n != 1 {
		return apitypes.RecoveryReleaseResponse{}, ErrNotAuthorized
	}
	if err := tx.Commit(ctx); err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	s.custodySettledAfterCommit(runID)
	return ack, nil
}
