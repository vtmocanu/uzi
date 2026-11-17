package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// A plain env row marked sealed_with=master (nil AAD) is never opened for a response.
func TestRunEnvDTORefusesMasterMarkedRow(t *testing.T) {
	h := &Handler{vault: &vault.Vault{}}
	row := runEnvRow{Name: "PLAIN_ONE", Ciphertext: []byte("x"), SealedWith: store.SealedWithMaster,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	dto, locked := h.runEnvDTO(uuid.New(), uuid.New(), row)
	if dto.Value != nil || !dto.ValueWithheld || locked {
		t.Fatalf("master-marked row: %+v locked=%v", dto, locked)
	}
}
