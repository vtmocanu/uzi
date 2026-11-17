package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// A plain env row marked sealed_with=master (nil AAD) is never opened for a response.
func TestRunEnvDTORefusesMasterMarkedRow(t *testing.T) {
	// A real nil-AAD ciphertext under a configured master box: without the master-marker
	// check the vault would open it, so only that check keeps the value withheld.
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("legacy-plain"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{vault: vault.New(box, nil, func(context.Context, func(context.Context, vault.Store) error) error {
		return errors.New("unused")
	})}
	row := runEnvRow{Name: "PLAIN_ONE", Ciphertext: sealed, SealedWith: store.SealedWithMaster,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	dto, locked := h.runEnvDTO(uuid.New(), uuid.New(), row)
	if dto.Value != nil || !dto.ValueWithheld || locked {
		t.Fatalf("master-marked row: %+v locked=%v", dto, locked)
	}
}
