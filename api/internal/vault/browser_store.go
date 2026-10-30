package vault

import (
	"context"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// BrowserStore is the transaction-bound storage contract for browser lifecycle
// operations. Crypto-only legacy fixtures need only implement Store.
type BrowserStore interface {
	Store
	GetVaultReunlockDays(context.Context) (string, error)
	GetVaultBrowserSlot(context.Context, store.GetVaultBrowserSlotParams) (store.VaultBrowserSlot, error)
	PrepareVaultBrowserSlot(context.Context, store.PrepareVaultBrowserSlotParams) error
	TrimVaultBrowserSlots(context.Context, uuid.UUID) error
	InsertVaultBrowserSlot(context.Context, store.InsertVaultBrowserSlotParams) error
	SetVaultAutoReunlock(context.Context, store.SetVaultAutoReunlockParams) error
	BumpTokenVersion(context.Context, uuid.UUID) (int32, error)
	UpdatePassword(context.Context, store.UpdatePasswordParams) error
	SetUserActive(context.Context, store.SetUserActiveParams) (store.User, error)
}

var _ BrowserStore = (*store.Queries)(nil)
