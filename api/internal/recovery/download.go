package recovery

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Download streams the decrypted bundle bytes of one owner-owned, available capture to w
// (D4/D6). It never buffers the whole bundle: it iterates the ordered chunk rows in a
// single-statement snapshot (so concurrent expiry/discard cannot remove chunks mid-read),
// decrypts each with its per-chunk AAD, and writes+flushes. A decrypt/AAD failure BEFORE
// the first byte returns an integrity error (the handler answers 500 with no body); a
// failure after streaming has begun aborts the response rather than releasing corrupt or
// unauthenticated plaintext. The response carries private/no-store/nosniff headers, a
// server-generated attachment filename and Content-Length, and no redirect or presigned
// URL. Owner authorization is enforced here (GetCaptureForOwner is owner-scoped) on top of
// the handler's GetRun owner-or-404 check.
func (s *Service) Download(ctx context.Context, w http.ResponseWriter, userID, runID, captureID uuid.UUID) error {
	if s.box == nil {
		return errors.New("recovery: encryption key unavailable")
	}
	c, err := s.store.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: captureID, RunID: runID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCaptureNotFound
		}
		return err
	}
	if c.State != "available" || !c.ManifestBound || !c.ByteSize.Valid {
		return ErrNotAvailable
	}
	byteSize := c.ByteSize.Int64
	chunkCount := 0
	if c.ChunkCount.Valid {
		chunkCount = int(c.ChunkCount.Int32)
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.RequestDeadline)
	defer cancel()
	if err := acquire(ctx, s.download); err != nil {
		return err
	}
	defer release(s.download)

	// A single ORDER BY query is one consistent snapshot: a concurrent expiry (state flip)
	// or discard (chunk delete) cannot alter this running query's result set.
	rows, err := s.pool.Query(ctx, `SELECT chunk_index, length, sealed
		FROM recovery_capture_chunks WHERE capture_id = $1 ORDER BY chunk_index`, captureID)
	if err != nil {
		return err
	}
	defer rows.Close()

	flusher, _ := w.(http.Flusher)
	wroteHeader := false
	writeHeader := func() {
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Cache-Control", "private, no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Disposition", `attachment; filename="`+downloadFilename(captureID)+`"`)
		h.Set("Content-Length", strconv.FormatInt(byteSize, 10))
		w.WriteHeader(http.StatusOK)
		wroteHeader = true
	}

	var expected int32
	var total int64
	for rows.Next() {
		var idx, length int32
		var sealed []byte
		if err := rows.Scan(&idx, &length, &sealed); err != nil {
			if !wroteHeader {
				return err
			}
			return ErrStreamAborted
		}
		if idx != expected {
			// A gap/reorder in the stored inventory.
			if !wroteHeader {
				return ErrIntegrity
			}
			return ErrStreamAborted
		}
		plain, derr := s.box.OpenWithAAD(sealed, chunkAAD(captureID, int(idx), int(length)))
		if derr != nil || len(plain) != int(length) {
			// Never release unauthenticated plaintext from a failed chunk.
			if !wroteHeader {
				return ErrIntegrity
			}
			return ErrStreamAborted
		}
		if !wroteHeader {
			writeHeader()
		}
		if _, err := w.Write(plain); err != nil {
			return ErrStreamAborted // the client disconnected mid-stream.
		}
		if flusher != nil {
			flusher.Flush()
		}
		total += int64(len(plain))
		expected++
	}
	if err := rows.Err(); err != nil {
		if !wroteHeader {
			return err
		}
		return ErrStreamAborted
	}
	// The complete inventory must match the bound manifest: a dropped trailing chunk
	// (truncation) or a short total is an integrity failure, not a servable artifact.
	if int(expected) != chunkCount || total != byteSize {
		if !wroteHeader {
			return ErrIntegrity
		}
		return ErrStreamAborted
	}
	if !wroteHeader {
		// A zero-chunk, zero-byte capture: emit a valid empty 200 body.
		writeHeader()
	}
	return nil
}

// downloadFilename is the server-generated attachment name (D6): never a caller-supplied
// path, so it cannot direct the write anywhere or carry a traversal. The capture id is a
// UUID, so the result is always a safe, printable filename.
func downloadFilename(captureID uuid.UUID) string {
	return "recovery-" + captureID.String() + ".bundle"
}
