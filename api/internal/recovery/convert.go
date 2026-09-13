package recovery

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// captureToStatus builds the worker-facing by-id status from a stored capture. The reason
// is re-sanitized on read as defense in depth (it is already sanitized on write).
func captureToStatus(c store.RecoveryCapture) apitypes.RecoveryCaptureStatusResponse {
	out := apitypes.RecoveryCaptureStatusResponse{
		CaptureID:     c.ID.String(),
		State:         c.State,
		ManifestBound: c.ManifestBound,
	}
	if c.ByteSize.Valid {
		out.ByteSize = int64Ptr(c.ByteSize.Int64)
	}
	if c.Checksum.Valid {
		out.Checksum = c.Checksum.String
	}
	if c.Reason.Valid {
		out.Reason = sanitizeReason(c.Reason.String)
	}
	if c.ExpiresAt.Valid {
		out.ExpiresAt = timePtr(c.ExpiresAt.Time)
	}
	return out
}

// statusFromLocked builds the idempotent ready receipt from the row-locked snapshot the
// upload transaction already read, so a lost-ACK retry needs no extra query.
func statusFromLocked(captureID uuidLike, lc lockedCapture) apitypes.RecoveryCaptureStatusResponse {
	out := apitypes.RecoveryCaptureStatusResponse{
		CaptureID:     captureID.String(),
		State:         lc.state,
		ManifestBound: true,
	}
	if lc.byteSize.Valid {
		out.ByteSize = int64Ptr(lc.byteSize.Int64)
	}
	if lc.checksum.Valid {
		out.Checksum = lc.checksum.String
	}
	if lc.reason.Valid {
		out.Reason = sanitizeReason(lc.reason.String)
	}
	if lc.expiresAt.Valid {
		out.ExpiresAt = timePtr(lc.expiresAt.Time)
	}
	return out
}

// captureToDTO builds the owner-facing archive metadata DTO from a stored capture (D7).
// It carries NO raw bytes and no idempotency key; every optional field is a pointer or
// omitempty so a zero value marshals without a null.
func captureToDTO(c store.RecoveryCapture) apitypes.RecoveryArchiveDTO {
	out := apitypes.RecoveryArchiveDTO{
		ID:               c.ID.String(),
		RunID:            c.RunID.String(),
		State:            c.State,
		SourceSha:        c.SourceSha,
		PrerequisiteShas: c.PrerequisiteShas,
	}
	if c.AttemptedHeadSha.Valid {
		out.AttemptedHeadSha = strPtr(c.AttemptedHeadSha.String)
	}
	if c.ByteSize.Valid {
		out.ByteSize = int64Ptr(c.ByteSize.Int64)
	}
	if c.Checksum.Valid {
		out.Checksum = c.Checksum.String
	}
	if c.Reason.Valid {
		out.Reason = sanitizeReason(c.Reason.String)
	}
	if c.CreatedAt.Valid {
		out.CreatedAt = c.CreatedAt.Time
	}
	if c.ExpiresAt.Valid {
		out.ExpiresAt = timePtr(c.ExpiresAt.Time)
	}
	return out
}

// uuidLike is satisfied by uuid.UUID; it lets statusFromLocked take the capture id
// without importing uuid into this conversion file for a single method set.
type uuidLike interface{ String() string }

func int64Ptr(v int64) *int64        { return &v }
func strPtr(v string) *string        { return &v }
func timePtr(v time.Time) *time.Time { return &v }

// isSHA256Hex reports whether s is a 64-character hex string (a sha256 digest). The
// worker declares the whole-bundle checksum in this form; the server recomputes and
// compares it (D4). It also rejects any control/escape/bidi byte by construction.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
