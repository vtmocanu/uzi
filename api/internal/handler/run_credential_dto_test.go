package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOAnthropicCredential pins how runToDTO maps PRD #111 M1's two columns:
// INDEPENDENTLY, never as a pair.
//
// The middle case is the one that matters and the one a "map them together" shortcut
// silently breaks. Migration 00086's FK nulls anthropic_secret_id when the credential
// is deleted while the label — a claim-time SNAPSHOT — stays. So a historical run
// legitimately carries a label with no id, and that is exactly the run whose
// attribution is least recoverable from anywhere else. Gating the label on the id
// would blank it precisely there.
func TestRunToDTOAnthropicCredential(t *testing.T) {
	secretID := uuid.New()

	t.Run("both present", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:                   uuid.New(),
			AnthropicSecretID:    pgtype.UUID{Bytes: secretID, Valid: true},
			AnthropicSecretLabel: pgtype.Text{String: "console-key", Valid: true},
		})
		if dto.AnthropicSecretID == nil || *dto.AnthropicSecretID != secretID.String() {
			t.Fatalf("id = %v, want %v", dto.AnthropicSecretID, secretID)
		}
		if dto.AnthropicSecretLabel == nil || *dto.AnthropicSecretLabel != "console-key" {
			t.Fatalf("label = %v, want console-key", dto.AnthropicSecretLabel)
		}
	})

	t.Run("token deleted: id null, label kept", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:                   uuid.New(),
			AnthropicSecretID:    pgtype.UUID{}, // the FK nulled it
			AnthropicSecretLabel: pgtype.Text{String: "retired-key", Valid: true},
		})
		if dto.AnthropicSecretID != nil {
			t.Fatalf("id = %v, want nil", *dto.AnthropicSecretID)
		}
		if dto.AnthropicSecretLabel == nil || *dto.AnthropicSecretLabel != "retired-key" {
			t.Fatalf("label = %v, want the snapshot retired-key to survive the token being deleted", dto.AnthropicSecretLabel)
		}
	})

	t.Run("pre-feature or unclaimed run: both null", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New()})
		if dto.AnthropicSecretID != nil || dto.AnthropicSecretLabel != nil {
			t.Fatalf("id=%v label=%v, want both nil — a run with nothing recorded must not fabricate an account",
				dto.AnthropicSecretID, dto.AnthropicSecretLabel)
		}
	})
}
