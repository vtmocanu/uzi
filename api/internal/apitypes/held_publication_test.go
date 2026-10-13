package apitypes

import (
	"testing"
	"time"
)

// Issue #2545: the owner held-publication DTOs. Every optional field is omitempty, so the zero
// value carries only the always-present keys and the populated value surfaces them all.
func TestHeldPublicationDTOTags(t *testing.T) {
	assertTags(t, "HeldPublicationDTO", HeldPublicationDTO{},
		"id", "run_id", "generation", "ref", "tip", "state", "created_at")
	now := time.Unix(0, 0)
	full := HeldPublicationDTO{
		RefCreatedAt: &now, AcknowledgedAt: &now, ExpiresAt: &now, OwnerExpiredAt: &now, DeletedAt: &now,
		LastError: "e", RefusalReason: "create_refused",
	}
	assertTags(t, "HeldPublicationDTO(full)", full,
		"id", "run_id", "generation", "ref", "tip", "state", "created_at",
		"ref_created_at", "acknowledged_at", "expires_at", "owner_expired_at", "deleted_at", "last_error", "refusal_reason")
	assertTags(t, "HeldPublicationsDTO", HeldPublicationsDTO{}, "publications")
	assertTags(t, "HeldPublicationsExpireDTO", HeldPublicationsExpireDTO{}, "expired", "publications")
}
