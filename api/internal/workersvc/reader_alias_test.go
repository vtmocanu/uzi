package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type readerAliasStore struct {
	*store.Queries
	owner, alias    uuid.UUID
	lookups, failed int
}

func (s *readerAliasStore) GetCodexCredentialState(_ context.Context, p store.GetCodexCredentialStateParams) (store.CodexCredentialState, error) {
	if p.UserID != s.owner || p.UserSecretID != s.alias {
		panic("changed alias identity")
	}
	return store.CodexCredentialState{UserID: s.owner, UserSecretID: s.alias, Status: codexStatusStaging, MaterialRevision: 3}, nil
}
func (s *readerAliasStore) GetUserSecretCiphertextByID(_ context.Context, p store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	if p.UserID != s.owner || p.ID != s.alias {
		panic("changed alias identity")
	}
	s.lookups++
	// The replacement committed between state capture and the first cipher read.
	return store.GetUserSecretCiphertextByIDRow{ID: s.alias, UserID: s.owner, Kind: store.KindCodexAuth,
		VaultEpoch: 8, EnablementRev: 4, MaterialRevision: pgtype.Int8{Int64: 4, Valid: true}}, nil
}
func (s *readerAliasStore) SetCodexCredentialStateStatus(context.Context, store.SetCodexCredentialStateStatusParams) (int64, error) {
	s.failed++
	return 1, nil
}
func TestReaderAliasReplacementBeforeFirstCipherLookup(t *testing.T) {
	q := &readerAliasStore{owner: uuid.New(), alias: uuid.New()}
	ident := newFakeCodexIdentity()
	// A nil vault makes reaching crypto observable as a different error. The
	// revision fence must reject before any crypto/provider/transaction work.
	r := NewCodexReconciler(q, nil, newBox(t), ident, nil)
	err := r.ReconcileCodexAuthIdentity(context.Background(), q.owner, q.alias)
	if !errors.Is(err, store.ErrVaultSealStale) || q.lookups != 1 || q.failed != 0 || ident.discoverCalls != 0 || ident.refreshCalls != 0 {
		t.Fatalf("replacement reached discovery/failure: err=%v lookups=%d failed=%d discover=%d refresh=%d", err, q.lookups, q.failed, ident.discoverCalls, ident.refreshCalls)
	}
}
