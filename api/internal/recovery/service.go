package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// maxPrerequisiteShas bounds the prerequisite closure recorded on the manifest (D5). A
// bundle imports against a small verified public closure, never an unbounded list.
const maxPrerequisiteShas = 64

// ErrAmbiguous: a v1/no-generation worker holds MORE THAN ONE open custody hold on the run, so
// the server cannot pick which generation a capture-reserve belongs to (PRD #1349 M4, D1/D2). It
// refuses rather than guess a newest hold — the hold stays open and the worker must name its
// generation (v2) or the owner disposes explicitly. Defined here beside the Reserve/Release
// exact-generation logic that raises it (the shared sentinels live in recovery.go); the handler
// maps it (a 409 Conflict is ideal, but mapRecoveryError's default 500 is acceptable).
var ErrAmbiguous = errors.New("recovery: ambiguous open custody generation")

// ── Worker-facing operations (D2/D6/D7). Each enforces the ORIGINAL worker identity and
// an OPEN hold; none mutates run state or retargets another owner/repo/run. ──────────────

// Reserve creates (or idempotently re-reserves) a capture under the EXACT custody hold this
// capture belongs to (PRD #1349 M4, D1/D2). A v2 worker that names req.Generation binds to the
// hold it took at that claim generation, fail-closed (ErrNotAuthorized) if no such open hold
// exists; a v1/no-generation worker binds only when it holds a SINGLE open hold — more than one
// is ErrAmbiguous (the hold stays open, nothing is guessed). A lost ACK re-reserves the SAME
// capture id (idempotency key), never a duplicate. Capture-admission bounds (per-claim,
// per-owner) gate a genuinely new capture; a retry of an existing key is exempt. Reserve never
// touches the runs table.
func (s *Service) Reserve(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.RecoveryReserveRequest) (apitypes.RecoveryReserveResponse, error) {
	sourceSha, err := validateSha(req.SourceSha, false)
	if err != nil {
		return apitypes.RecoveryReserveResponse{}, ErrBadRequest
	}
	attempted, err := validateSha(req.AttemptedHeadSha, true)
	if err != nil {
		return apitypes.RecoveryReserveResponse{}, ErrBadRequest
	}
	key, err := validateSafeLabel(strings.TrimSpace(req.IdempotencyKey), maxIdempotencyKeyLen)
	if err != nil || key == "" {
		return apitypes.RecoveryReserveResponse{}, ErrBadRequest
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apitypes.RecoveryReserveResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve and row-lock the EXACT custody hold this capture belongs to (PRD #1349 M4,
	// D1/D2). FOR UPDATE serializes concurrent reserves under the one hold so the per-claim cap
	// is exact. A v2 worker names its claim generation, so the capture binds to the ONE hold it
	// took at that generation — never an arbitrary newest same-worker hold; no such open hold
	// → not authorized (fail closed). A v1/no-generation worker's hold is resolved only when it
	// is UNAMBIGUOUS: exactly one open hold binds; MORE than one is ErrAmbiguous (refuse rather
	// than guess, so the hold stays open); zero is ErrNotAuthorized.
	var (
		holdID     uuid.UUID
		generation int64
	)
	if slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryArchiveV2) && req.Generation != nil {
		generation = *req.Generation
		err = tx.QueryRow(ctx, `SELECT id FROM recovery_custody_holds
			WHERE run_id = $1 AND user_id = $2 AND original_worker_id = $3 AND generation = $4 AND state = 'open'
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
			runID, wkr.UserID, wkr.ID, generation).Scan(&holdID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apitypes.RecoveryReserveResponse{}, ErrNotAuthorized
			}
			return apitypes.RecoveryReserveResponse{}, err
		}
	} else {
		holdID, generation, err = s.resolveSoleOpenHold(ctx, tx, runID, wkr.ID)
		if err != nil {
			return apitypes.RecoveryReserveResponse{}, err
		}
	}

	// Is this an idempotent retry of an existing capture identity? If so it is already
	// counted; skip the admission caps.
	var existing bool
	var existingState string
	err = tx.QueryRow(ctx, `SELECT state FROM recovery_captures WHERE hold_id = $1 AND idempotency_key = $2`,
		holdID, key).Scan(&existingState)
	switch {
	case err == nil:
		existing = true
	case errors.Is(err, pgx.ErrNoRows):
		existing = false
	default:
		return apitypes.RecoveryReserveResponse{}, err
	}

	if !existing {
		var perClaim int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM recovery_captures WHERE hold_id = $1`, holdID).Scan(&perClaim); err != nil {
			return apitypes.RecoveryReserveResponse{}, err
		}
		if s.limits.MaxCapturesPerClaim > 0 && perClaim >= s.limits.MaxCapturesPerClaim {
			return apitypes.RecoveryReserveResponse{}, ErrQuota
		}
		var perOwner int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM recovery_captures WHERE user_id = $1 AND state <> 'discarded'`, wkr.UserID).Scan(&perOwner); err != nil {
			return apitypes.RecoveryReserveResponse{}, err
		}
		if s.limits.MaxCapturesPerOwner > 0 && perOwner >= s.limits.MaxCapturesPerOwner {
			return apitypes.RecoveryReserveResponse{}, ErrQuota
		}
	}

	cap, err := store.New(tx).ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{
		RunID:                  runID,
		UserID:                 wkr.UserID,
		OriginalWorkerID:       pgconv.UUID(wkr.ID),
		OriginalWorkerIdentity: workerIdentity(wkr),
		SourceSha:              sourceSha,
		AttemptedHeadSha:       pgconv.TextOrNull(attempted),
		IdempotencyKey:         key,
		Generation:             generation,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryReserveResponse{}, ErrNotAuthorized
		}
		return apitypes.RecoveryReserveResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return apitypes.RecoveryReserveResponse{}, err
	}
	return apitypes.RecoveryReserveResponse{CaptureID: cap.ID.String(), State: cap.State}, nil
}

// lockedCapture is the row-locked snapshot the upload transaction reads before streaming.
type lockedCapture struct {
	runID      uuid.UUID
	userID     uuid.UUID
	origWorker pgtype.UUID
	state      string
	bound      bool
	byteSize   pgtype.Int8
	checksum   pgtype.Text
	holdState  string
	expiresAt  pgtype.Timestamptz
	reason     pgtype.Text
}

// Upload streams one octet-stream bundle into AAD-sealed ~1 MiB chunks and commits them
// with the ready transition in ONE bounded transaction (D4). body MUST already be wrapped
// by the handler in http.MaxBytesReader(w, r.Body, MaxBundleBytes) so an over-cap body
// ERRORS rather than truncating. The caller's identity must own the capture's open hold; a
// terminated run is fine (post-terminal recovery authority) as long as the hold is open.
// A retry starts from zero under the same capture id; a lost ACK returns the existing
// ready receipt without re-charging quota or overwriting bytes. Upload never mutates run
// state. On oversize/quota/transport/integrity failure it records needs_action with a
// bounded sanitized reason and returns the matching sentinel.
func (s *Service) Upload(ctx context.Context, wkr store.Worker, runID, captureID uuid.UUID, manifest apitypes.RecoveryUploadManifest, body io.Reader) (apitypes.RecoveryCaptureStatusResponse, error) {
	status, err := s.doUpload(ctx, wkr, runID, captureID, manifest, body)
	if err != nil {
		// A capacity/transport/integrity failure records needs_action and retains the
		// source; recordFailure re-verifies ownership and ignores auth/not-found/busy/
		// malformed causes, so an early oversize reject still surfaces needs_action.
		s.recordFailure(ctx, wkr, runID, captureID, err)
	}
	return status, err
}

// doUpload validates the manifest, bounds concurrency, and runs the transactional upload.
func (s *Service) doUpload(ctx context.Context, wkr store.Worker, runID, captureID uuid.UUID, manifest apitypes.RecoveryUploadManifest, body io.Reader) (apitypes.RecoveryCaptureStatusResponse, error) {
	if s.box == nil {
		return apitypes.RecoveryCaptureStatusResponse{}, errors.New("recovery: encryption key unavailable")
	}
	if manifest.ByteSize < 0 || manifest.ByteSize > s.limits.MaxBundleBytes {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrOversize
	}
	if !isSHA256Hex(manifest.Checksum) {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrBadRequest
	}
	if len(manifest.PrerequisiteShas) > maxPrerequisiteShas {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrBadRequest
	}
	for _, p := range manifest.PrerequisiteShas {
		if _, err := validateSha(p, false); err != nil {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrBadRequest
		}
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.RequestDeadline)
	defer cancel()
	if err := acquire(ctx, s.uploads); err != nil {
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	defer release(s.uploads)

	return s.upload(ctx, wkr, runID, captureID, manifest, body)
}

// upload runs the transactional body of Upload. Its errors are handled (needs_action
// recording) by the caller.
func (s *Service) upload(ctx context.Context, wkr store.Worker, runID, captureID uuid.UUID, manifest apitypes.RecoveryUploadManifest, body io.Reader) (apitypes.RecoveryCaptureStatusResponse, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lc lockedCapture
	err = tx.QueryRow(ctx, `SELECT c.run_id, c.user_id, c.original_worker_id, c.state, c.manifest_bound,
			c.byte_size, c.checksum, c.expires_at, c.reason, h.state
		FROM recovery_captures c JOIN recovery_custody_holds h ON h.id = c.hold_id
		WHERE c.id = $1 FOR UPDATE OF c`, captureID).Scan(
		&lc.runID, &lc.userID, &lc.origWorker, &lc.state, &lc.bound,
		&lc.byteSize, &lc.checksum, &lc.expiresAt, &lc.reason, &lc.holdState)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrCaptureNotFound
		}
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}

	// Authorize: the capture must belong to this run + owner, be held by THIS worker, and
	// its hold must be open. A different/later worker cannot adopt authority on a terminal
	// run; the run's own status is irrelevant here (post-terminal recovery, D2).
	if lc.runID != runID || lc.userID != wkr.UserID {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrCaptureNotFound
	}
	if !lc.origWorker.Valid || uuid.UUID(lc.origWorker.Bytes) != wkr.ID {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrNotAuthorized
	}
	if lc.holdState != "open" {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrNotAuthorized
	}

	// Lost-ACK idempotency: an already-ready capture returns its existing receipt without
	// re-uploading, re-charging quota or overwriting bytes.
	if lc.state == "available" {
		return statusFromLocked(captureID, lc), nil
	}
	// A different manifest already bound under this id is a conflict, never an overwrite.
	if lc.bound && (lc.byteSize.Int64 != manifest.ByteSize || !strings.EqualFold(lc.checksum.String, manifest.Checksum)) {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrManifestConflict
	}

	// Quota pre-check on the DECLARED size (the actual bytes are verified == declared
	// below, so this is sound). The instance ceiling is applied to THIS owner's upload —
	// it records needs_action and retains the source, never a global stop or an eviction.
	if s.limits.ReadyPayloadPerOwner > 0 {
		var ownerReady int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(byte_size),0) FROM recovery_captures
			WHERE user_id = $1 AND state = 'available' AND id <> $2`, wkr.UserID, captureID).Scan(&ownerReady); err != nil {
			return apitypes.RecoveryCaptureStatusResponse{}, err
		}
		if ownerReady+manifest.ByteSize > s.limits.ReadyPayloadPerOwner {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrQuota
		}
	}
	if s.limits.InstanceBytes > 0 {
		var instanceReady int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(byte_size),0) FROM recovery_captures
			WHERE state = 'available' AND id <> $1`, captureID).Scan(&instanceReady); err != nil {
			return apitypes.RecoveryCaptureStatusResponse{}, err
		}
		if instanceReady+manifest.ByteSize > s.limits.InstanceBytes {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrQuota
		}
	}

	// A retry starts from zero: drop any chunk rows from a prior attempt under this id.
	if _, err := tx.Exec(ctx, `DELETE FROM recovery_capture_chunks WHERE capture_id = $1`, captureID); err != nil {
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}

	total, chunkCount, checksum, err := s.sealStream(ctx, tx, captureID, body)
	if err != nil {
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}

	// Verify the ACTUAL stream against the authenticated manifest — never Content-Length or
	// a client-declared size. The complete streamed length AND the checksum must match; a
	// truncated valid-prefix (short stream, or dropped trailing chunks) is rejected here.
	if total != manifest.ByteSize {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrIntegrity
	}
	if !strings.EqualFold(checksum, manifest.Checksum) {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrIntegrity
	}
	// The server derives the ordered-chunk inventory count from the actual chunking. When
	// the worker declares one it must agree (belt-and-suspenders beside the checksum); the
	// bound manifest records the server-derived count either way.
	if manifest.ChunkCount != 0 && manifest.ChunkCount != chunkCount {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrIntegrity
	}

	qtx := store.New(tx)
	if _, err := qtx.BindCaptureManifest(ctx, store.BindCaptureManifestParams{
		ByteSize:         pgtype.Int8{Int64: total, Valid: true},
		Checksum:         pgconv.Text(strings.ToLower(checksum)),
		ChunkCount:       pgtype.Int4{Int32: int32(chunkCount), Valid: true}, //nolint:gosec // G115: chunkCount <= total/1 <= MaxBundleBytes bytes; at any sane bundle ceiling it is far below math.MaxInt32
		PrerequisiteShas: manifest.PrerequisiteShas,
		ID:               captureID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrManifestConflict
		}
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	expires := s.now().Add(s.limits.ReadyRetention)
	ready, err := qtx.MarkCaptureReady(ctx, store.MarkCaptureReadyParams{
		ExpiresAt: pgconv.Time(expires),
		ID:        captureID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrIntegrity
		}
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	return captureToStatus(ready), nil
}

// sealStream reads body in chunkPlaintextSize plaintext blocks, seals each with its
// per-chunk AAD and inserts it into the open transaction, returning the total plaintext
// byte count, the number of chunks, and the lowercase hex sha256 of the whole plaintext.
// An over-cap body surfaces as ErrOversize (the handler's http.MaxBytesReader trips); any
// other read fault surfaces as ErrIntegrity. Nothing is written outside tx, so an error
// rolls back every chunk from this attempt.
func (s *Service) sealStream(ctx context.Context, tx pgx.Tx, captureID uuid.UUID, body io.Reader) (int64, int, string, error) {
	qtx := store.New(tx)
	hasher := sha256.New()
	buf := make([]byte, chunkPlaintextSize)
	var total int64
	index := 0
	for {
		n, readErr := io.ReadFull(body, buf)
		if n > 0 {
			plain := buf[:n]
			_, _ = hasher.Write(plain)
			sealed, err := s.box.SealWithAAD(plain, chunkAAD(captureID, index, n))
			if err != nil {
				return 0, 0, "", err
			}
			if err := qtx.InsertCaptureChunk(ctx, store.InsertCaptureChunkParams{
				CaptureID:  captureID,
				ChunkIndex: int32(index),
				Length:     int32(n), //nolint:gosec // G115: 0 <= n <= chunkPlaintextSize (1 MiB), within int32
				Sealed:     sealed,
			}); err != nil {
				return 0, 0, "", err
			}
			total += int64(n)
			index++
		}
		if readErr == nil || errors.Is(readErr, io.ErrUnexpectedEOF) {
			if readErr == nil {
				continue
			}
			break // ErrUnexpectedEOF: a final short block was read and sealed above.
		}
		if errors.Is(readErr, io.EOF) {
			break // clean end on a chunk boundary.
		}
		var tooLarge *http.MaxBytesError
		if errors.As(readErr, &tooLarge) {
			return 0, 0, "", ErrOversize
		}
		return 0, 0, "", ErrIntegrity
	}
	return total, index, hex.EncodeToString(hasher.Sum(nil)), nil
}

// recordFailure marks a capture needs_action with a bounded sanitized reason after a
// failed upload attempt (D4), retaining the pinned source for a later retry. Best-effort:
// authorization/not-found failures record nothing (there is no owned capture to mark), and
// a busy/deadline failure is transient. The recording runs on a fresh short-lived context
// so a cancelled request context does not prevent it.
func (s *Service) recordFailure(ctx context.Context, wkr store.Worker, runID, captureID uuid.UUID, cause error) {
	switch {
	case errors.Is(cause, ErrCaptureNotFound), errors.Is(cause, ErrNotAuthorized),
		errors.Is(cause, ErrManifestConflict), errors.Is(cause, ErrBusy), errors.Is(cause, ErrBadRequest):
		return
	}
	// Re-verify ownership cheaply before writing, so this can never mark a foreign capture.
	cap, err := s.store.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: captureID, RunID: runID, UserID: wkr.UserID})
	if err != nil || !cap.OriginalWorkerID.Valid || uuid.UUID(cap.OriginalWorkerID.Bytes) != wkr.ID {
		return
	}
	if cap.State == "available" {
		return // a concurrent success won; do not clobber a ready capture.
	}
	reason := sanitizeReason(uploadFailureReason(cause))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = s.store.MarkCaptureState(writeCtx, store.MarkCaptureStateParams{
		State:  "needs_action",
		Reason: pgconv.TextOrNull(reason),
		ID:     captureID,
	})
}

// uploadFailureReason maps a sentinel to a bounded, sanitized-safe owner-facing reason.
// It never echoes request bytes or a secret sample (D6).
func uploadFailureReason(cause error) string {
	switch {
	case errors.Is(cause, ErrOversize):
		return "bundle exceeds the maximum size"
	case errors.Is(cause, ErrQuota):
		return "storage quota exceeded"
	case errors.Is(cause, ErrIntegrity):
		return "archive integrity check failed"
	default:
		return "upload failed; retry available"
	}
}

// Status returns the worker's by-id status poll of a capture (D2). The caller must own
// the capture's hold (original worker identity).
func (s *Service) Status(ctx context.Context, wkr store.Worker, runID, captureID uuid.UUID) (apitypes.RecoveryCaptureStatusResponse, error) {
	cap, err := s.store.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: captureID, RunID: runID, UserID: wkr.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apitypes.RecoveryCaptureStatusResponse{}, ErrCaptureNotFound
		}
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	if !cap.OriginalWorkerID.Valid || uuid.UUID(cap.OriginalWorkerID.Bytes) != wkr.ID {
		return apitypes.RecoveryCaptureStatusResponse{}, ErrNotAuthorized
	}
	return captureToStatus(cap), nil
}

// ListHoldsForWorkerRun returns the caller worker's OWN open custody holds on the run — the
// worker-facing post-clone generation-exact inventory (PRD #1349 M1, D3). Scoped to holds
// this worker ORIGINALLY took (original_worker_id = wkr.ID), so a cross-worker reclaim never
// sees a crashed worker's holds. The slice is initialized non-nil so it marshals as [] (never
// null) when the worker holds nothing on the run.
func (s *Service) ListHoldsForWorkerRun(ctx context.Context, wkr store.Worker, runID uuid.UUID) (apitypes.RecoveryHoldsResponse, error) {
	rows, err := s.store.ListCustodyHoldsForWorkerRun(ctx, store.ListCustodyHoldsForWorkerRunParams{
		RunID:    runID,
		WorkerID: wkr.ID,
	})
	if err != nil {
		return apitypes.RecoveryHoldsResponse{}, err
	}
	holds := make([]apitypes.RecoveryHoldDTO, 0, len(rows))
	for _, r := range rows {
		holds = append(holds, apitypes.RecoveryHoldDTO{
			HoldID:              r.ID.String(),
			Generation:          r.Generation,
			HasAvailableCapture: r.HasAvailableCapture,
			CaptureState:        r.CaptureState,
		})
	}
	return apitypes.RecoveryHoldsResponse{RunID: runID.String(), Holds: holds}, nil
}

// Release settles the caller worker's custody hold for the EXACT generation it completed
// (PRD #1349 M4, D1/D2/D3): it nulls the live FKs (dropping the ON DELETE RESTRICT that blocks
// teardown), flips state to 'released' and stamps released_at, for that one hold. A v2 worker
// names req.Generation and the server settles EXACTLY that generation — so a newer same-worker
// generation can never release an older generation whose source it did not inherit (the D2
// hazard). A v1/no-generation worker settles its hold only when it holds a SINGLE open hold;
// when it holds MORE THAN ONE it RETAINS (releases nothing, Retained=true) so an owner disposes
// explicitly rather than the server dropping an uncaptured sibling generation. Scoped to the
// caller's own holds — a foreign worker releases nothing. Idempotent: a repeat once none remain
// open settles zero (Released=false). It NEVER falls back to a generation-blind run+worker bulk
// release.
func (s *Service) Release(ctx context.Context, wkr store.Worker, runID uuid.UUID, req apitypes.RecoveryReleaseRequest) (apitypes.RecoveryReleaseResponse, error) {
	// v2 + explicit generation: settle EXACTLY that generation (fail-safe by rowcount).
	if slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryArchiveV2) && req.Generation != nil {
		n, err := store.New(s.pool).ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
			RunID:      runID,
			Generation: *req.Generation,
			WorkerID:   wkr.ID,
		})
		if err != nil {
			return apitypes.RecoveryReleaseResponse{}, err
		}
		return apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: n > 0, HoldsReleased: int(n)}, nil
	}

	// v1 or no generation: resolve the caller's OPEN holds. Release the SINGLE one; RETAIN on
	// ambiguity; idempotent no-op when none remain open. FOR UPDATE row-locks the resolution.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, generation, err := s.resolveSoleOpenHold(ctx, tx, runID, wkr.ID)
	switch {
	case errors.Is(err, ErrNotAuthorized):
		// No open hold owned by this worker: an idempotent no-op (nothing to release).
		return apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: false, HoldsReleased: 0}, nil
	case errors.Is(err, ErrAmbiguous):
		// RETAIN: more than one open generation for this worker; an explicit disposition is
		// required so an uncaptured sibling generation is never dropped.
		return apitypes.RecoveryReleaseResponse{
			RunID:         runID.String(),
			Released:      false,
			HoldsReleased: 0,
			Retained:      true,
			Reason:        "ambiguous: multiple open generations for this worker; explicit disposition required",
		}, nil
	case err != nil:
		return apitypes.RecoveryReleaseResponse{}, err
	}

	n, err := store.New(tx).ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
		RunID:      runID,
		Generation: generation,
		WorkerID:   wkr.ID,
	})
	if err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return apitypes.RecoveryReleaseResponse{}, err
	}
	return apitypes.RecoveryReleaseResponse{RunID: runID.String(), Released: n > 0, HoldsReleased: int(n)}, nil
}

// resolveSoleOpenHold row-locks the caller worker's OPEN custody holds on the run and returns
// the SINGLE one's (id, generation) when exactly one is open (PRD #1349 M4, D1/D2). It refuses
// to guess: zero open holds → ErrNotAuthorized (fail closed), more than one → ErrAmbiguous.
// Scoped to holds this worker ORIGINALLY took (original_worker_id); a hold is opened with
// original_worker_id == live_worker_id and that never changes while open, so the resolved
// generation is exactly reservable/releasable by this worker. Must run inside tx: the FOR UPDATE
// holds the row lock(s) for the caller's transaction.
func (s *Service) resolveSoleOpenHold(ctx context.Context, tx pgx.Tx, runID, workerID uuid.UUID) (uuid.UUID, int64, error) {
	rows, err := tx.Query(ctx, `SELECT id, generation FROM recovery_custody_holds
		WHERE run_id = $1 AND original_worker_id = $2 AND state = 'open'
		ORDER BY created_at DESC FOR UPDATE`, runID, workerID)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer rows.Close()
	var (
		holdID uuid.UUID
		gen    int64
		count  int
	)
	for rows.Next() {
		var (
			id uuid.UUID
			g  int64
		)
		if err := rows.Scan(&id, &g); err != nil {
			return uuid.Nil, 0, err
		}
		if count == 0 {
			holdID, gen = id, g
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, 0, err
	}
	switch count {
	case 0:
		return uuid.Nil, 0, ErrNotAuthorized
	case 1:
		return holdID, gen, nil
	default:
		return uuid.Nil, 0, ErrAmbiguous
	}
}

// ── Owner-facing operations (D6/D7). Owner authorization (GetRun owner-or-404) is done by
// the handler; these queries are additionally owner-scoped by user_id. ──────────────────

// Summary returns the per-run recovery aggregate + every retained capture, owner-scoped.
func (s *Service) Summary(ctx context.Context, userID, runID uuid.UUID) (apitypes.RecoveryArchiveSummaryDTO, error) {
	sum, err := s.store.GetRecoverySummaryForRun(ctx, store.GetRecoverySummaryForRunParams{RunID: runID, UserID: userID})
	if err != nil {
		return apitypes.RecoveryArchiveSummaryDTO{}, err
	}
	caps, err := s.store.ListCapturesForRunOwner(ctx, store.ListCapturesForRunOwnerParams{RunID: runID, UserID: userID})
	if err != nil {
		return apitypes.RecoveryArchiveSummaryDTO{}, err
	}
	archives := make([]apitypes.RecoveryArchiveDTO, 0, len(caps))
	for _, c := range caps {
		archives = append(archives, captureToDTO(c))
	}
	return apitypes.RecoveryArchiveSummaryDTO{
		Supported:   sum.Supported,
		Legacy:      !sum.Supported,
		HasOpenHold: sum.HasOpenHold,
		Counts: apitypes.RecoveryArchiveStateCountsDTO{
			Preparing:   int(sum.PreparingCount),
			Uploading:   int(sum.UploadingCount),
			Available:   int(sum.AvailableCount),
			NeedsAction: int(sum.NeedsActionCount),
			Expired:     int(sum.ExpiredCount),
			Discarded:   int(sum.DiscardedCount),
		},
		Archives: archives,
	}, nil
}

// Discard marks one owner-owned capture 'discarded' and deletes its bytes (D7). Returns
// whether a capture was discarded (false for a foreign/absent id).
func (s *Service) Discard(ctx context.Context, userID, runID, captureID uuid.UUID) (bool, error) {
	n, err := s.store.DiscardCaptureForOwner(ctx, store.DiscardCaptureForOwnerParams{ID: captureID, RunID: runID, UserID: userID})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListHoldsForOwner returns the owner's exact custody holds plus the owner-level aggregate
// (PRD #1349 M5, D6/D7/D10). By default it lists ALL the owner's holds (every state) — the CLI's
// all-states contract via `uzi run recovery`; when openOnly is set (the web hot poll, PRD #1371)
// it lists only live custody (state='open'), dropping resolved released/discarded history. It
// stamps each hold with a server-derived Attention, and folds the aggregate: OpenHolds and
// BlockedRuns come from the aggregate query (the SAME predicate ClaimRun/health gate on),
// CustodyHoldLimit from the configured ceiling, and DecisionNeeded is the count of holds whose
// derived attention awaits an owner decision (needs_action or source_only) — active protection and
// self-releasing archive_ready rows are excluded (D10). The aggregate stays owner-wide and
// independent of openOnly (it is computed by a separate query, never from the returned rows), so a
// filtered list never skews open_holds/decision_needed/blocked_runs. Owner authorization is by
// user_id in every query; the handler additionally gates the owner via RequireUser. Holds is
// always non-nil so it marshals as [] (never null). No run scope — this is the owner-wide list;
// the CLI narrows by run.
func (s *Service) ListHoldsForOwner(ctx context.Context, userID uuid.UUID, openOnly bool) (apitypes.RecoveryCustodyHoldsDTO, error) {
	params := store.ListCustodyHoldsForOwnerParams{UserID: userID}
	if openOnly {
		// Bound the list to live custody (PRD #1371): drop resolved (released/discarded)
		// history from the hot web poll. The hold-state domain is open/released/discarded,
		// enforced by convention (one INSERT + the release/discard UPDATEs), NOT a DB CHECK;
		// every decision-bearing hold (needs_action/source_only) is state='open', so this
		// filter never drops a hold the owner must act on, and DecisionNeeded stays exact.
		params.State = pgtype.Text{String: "open", Valid: true}
	}
	rows, err := s.store.ListCustodyHoldsForOwner(ctx, params)
	if err != nil {
		return apitypes.RecoveryCustodyHoldsDTO{}, err
	}
	holds := make([]apitypes.RecoveryCustodyHoldDTO, 0, len(rows))
	decisionNeeded := 0
	for _, r := range rows {
		dto := custodyHoldToDTO(r)
		if isDecisionAttention(dto.Attention) {
			decisionNeeded++
		}
		holds = append(holds, dto)
	}
	agg, err := s.store.GetCustodyAggregateForOwner(ctx, store.GetCustodyAggregateForOwnerParams{
		UserID:           userID,
		CustodyHoldLimit: s.limits.CustodyHoldLimit,
	})
	if err != nil {
		return apitypes.RecoveryCustodyHoldsDTO{}, err
	}
	return apitypes.RecoveryCustodyHoldsDTO{
		Aggregate: apitypes.RecoveryCustodyAggregateDTO{
			OpenHolds:        int(agg.OpenHolds),
			CustodyHoldLimit: int(s.limits.CustodyHoldLimit),
			DecisionNeeded:   decisionNeeded,
			BlockedRuns:      int(agg.BlockedRuns),
		},
		Holds: holds,
	}, nil
}

// DiscardHold discards ONE owner-owned OPEN custody hold and settles its non-ready captures in
// ONE locked transaction (PRD #1349 M5, D7). It:
//   - marks the exact hold 'discarded' and nulls its live worker/run FKs — the mutating SQL
//     RE-VERIFIES user_id + run + hold + state='open' (belt-and-braces beside the handler's
//     owner-or-404 gate), so a foreign owner, wrong run/hold id, or an already-released/
//     discarded hold matches zero rows and the whole call is a no-op returning false;
//   - marks that hold's preparing/uploading/needs_action captures 'discarded' and frees their
//     partial byte chunks, NEVER touching an 'available' archive (that survives for export; its
//     deletion is the separate DiscardRecoveryArchive owner choice);
//   - locks the hold FIRST, then its captures, matching the worker upload/release/retry order.
//     Either discard wins (state='open' guard makes it terminal, so a retry cannot revive it)
//     or a concurrent upload wins and leaves an 'available' archive that survives while the hold
//     still settles safely.
//
// Sibling holds and other generations are left untouched. Returns whether a hold was discarded.
func (s *Service) DiscardHold(ctx context.Context, userID, runID, holdID uuid.UUID) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := store.New(tx)
	n, err := qtx.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{
		HoldID: holdID,
		RunID:  runID,
		UserID: userID,
	})
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Foreign/absent/non-open: nothing to discard. Leave captures untouched (an available
		// archive on a released/discarded/foreign hold must survive) and roll back.
		return false, nil
	}
	if _, err := qtx.DiscardNonReadyCapturesForHold(ctx, holdID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
