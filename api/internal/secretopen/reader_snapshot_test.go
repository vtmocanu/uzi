package secretopen

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type snapshotStore struct {
	*fakeStore
	rows      []store.GetUserSecretCiphertextByIDRow
	args      []store.GetUserSecretCiphertextByIDParams
	epochs    []int64
	reads     int
	lookupErr error
	stateErr  error
}

func (s *snapshotStore) GetUserSecretCiphertextByID(_ context.Context, p store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	s.args = append(s.args, p)
	if len(s.args) > len(s.rows) {
		return store.GetUserSecretCiphertextByIDRow{}, s.lookupErr
	}
	return s.rows[len(s.args)-1], nil
}
func (s *snapshotStore) GetUserVaultState(_ context.Context, owner uuid.UUID) (store.GetUserVaultStateRow, error) {
	if owner != s.rows[0].UserID {
		return store.GetUserVaultStateRow{}, pgx.ErrNoRows
	}
	if s.stateErr != nil {
		return store.GetUserVaultStateRow{}, s.stateErr
	}
	if s.reads >= len(s.epochs) {
		panic("unexpected extra state read")
	}
	epoch := s.epochs[s.reads]
	s.reads++
	return store.GetUserVaultStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: epoch}, nil
}
func snapshotFixture(t *testing.T) (*secretbox.Box, store.GetUserSecretCiphertextByIDRow) {
	t.Helper()
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	row := store.GetUserSecretCiphertextByIDRow{ID: uuid.New(), UserID: uuid.New(), Kind: store.KindCodexAuth,
		EnablementRev: 3, MaterialRevision: pgtype.Int8{Int64: 4, Valid: true}, VaultEpoch: 7, SealedWith: store.SealedWithMasterAAD}
	aad := append(append(append([]byte{}, row.UserID[:]...), 0), []byte(row.Kind)...)
	row.Ciphertext, err = box.SealWithAAD([]byte("original material"), aad)
	if err != nil {
		t.Fatal(err)
	}
	return box, row
}

func TestByIDSnapshotRetryKeepsAuthority(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		box, row := snapshotFixture(t)
		row.Disabled = disabled // shared crypto seam retains started-refresh semantics
		fresh := row
		fresh.VaultEpoch = 8
		q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row, fresh}, epochs: []int64{8, 8}}
		got, err := OpenByIDOfKindAtRevision(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID, row.Kind, 4)
		if err != nil || string(got) != "original material" || len(q.args) != 2 || q.reads != 2 {
			t.Fatalf("snapshot retry: err=%v matches=%v lookups=%d reads=%d", err, string(got) == "original material", len(q.args), q.reads)
		}
		for _, arg := range q.args {
			if arg.ID != row.ID || arg.UserID != row.UserID {
				t.Fatal("retry changed selected identity")
			}
		}
	}
}

func TestByIDSnapshotRejectsChangedAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*store.GetUserSecretCiphertextByIDRow)
	}{
		{"id", func(r *store.GetUserSecretCiphertextByIDRow) { r.ID = uuid.New() }},
		{"owner", func(r *store.GetUserSecretCiphertextByIDRow) { r.UserID = uuid.New() }},
		{"kind", func(r *store.GetUserSecretCiphertextByIDRow) { r.Kind = store.KindOpenAIAPIKey }},
		{"enablement", func(r *store.GetUserSecretCiphertextByIDRow) { r.EnablementRev++ }},
		{"disabled", func(r *store.GetUserSecretCiphertextByIDRow) { r.Disabled = true }},
		{"alias revision", func(r *store.GetUserSecretCiphertextByIDRow) { r.MaterialRevision.Int64++ }},
		{"alias removed", func(r *store.GetUserSecretCiphertextByIDRow) { r.MaterialRevision.Valid = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, row := snapshotFixture(t)
			fresh := row
			fresh.VaultEpoch = 8
			tc.change(&fresh)
			q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row, fresh}, epochs: []int64{8}}
			got, err := OpenByID(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID)
			if err != store.ErrVaultSealStale || got != nil || q.reads != 1 || len(q.args) != 2 {
				t.Fatalf("replacement accepted: err=%v plaintext-present=%v reads=%d lookups=%d", err, got != nil, q.reads, len(q.args))
			}
		})
	}
}

func TestByIDSnapshotSecondTransitionStops(t *testing.T) {
	box, row := snapshotFixture(t)
	fresh := row
	fresh.VaultEpoch = 8
	q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row, fresh}, epochs: []int64{8, 9}}
	got, err := OpenByID(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID)
	if err != store.ErrVaultSealStale || got != nil || len(q.args) != 2 || q.reads != 2 {
		t.Fatalf("unbounded retry: err=%v plaintext-present=%v lookups=%d reads=%d", err, got != nil, len(q.args), q.reads)
	}
}

func TestByIDSnapshotRevisionBeforeFirstOpen(t *testing.T) {
	box, row := snapshotFixture(t)
	q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row}}
	got, err := OpenByIDOfKindAtRevision(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID, row.Kind, 3)
	if err != store.ErrVaultSealStale || got != nil || q.reads != 0 || len(q.args) != 1 {
		t.Fatalf("revision check happened after crypto: err=%v reads=%d", err, q.reads)
	}
}

func TestByIDSnapshotDoesNotRetryInvalidMaterial(t *testing.T) {
	for _, marker := range []string{store.SealedWithMasterAAD, "unknown"} {
		box, row := snapshotFixture(t)
		row.SealedWith = marker
		row.Ciphertext = append([]byte(nil), row.Ciphertext...)
		row.Ciphertext[len(row.Ciphertext)-1] ^= 1
		q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row}, epochs: []int64{7}}
		got, err := OpenByID(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID)
		if err != ErrUndecryptable || got != nil || len(q.args) != 1 {
			t.Fatalf("invalid material retried: err=%v lookups=%d", err, len(q.args))
		}
	}
}

func TestByIDSnapshotPreservesInfrastructure(t *testing.T) {
	infra := errors.New("reader unavailable")
	for _, refetch := range []bool{false, true} {
		box, row := snapshotFixture(t)
		q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row}, epochs: []int64{8}, lookupErr: infra}
		if !refetch {
			q.stateErr = infra
		}
		got, err := OpenByID(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID)
		if !errors.Is(err, infra) || !errors.Is(err, ErrReaderFailure) || errors.Is(err, ErrUndecryptable) || errors.Is(err, ErrVaultLocked) || got != nil {
			t.Fatalf("infrastructure collapsed: %v", err)
		}
	}
}

func TestByIDSnapshotWrappedStaleDoesNotRetry(t *testing.T) {
	box, row := snapshotFixture(t)
	wrapped := fmt.Errorf("state reader: %w", store.ErrVaultSealStale)
	q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row}, stateErr: wrapped}
	got, err := OpenByID(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID)
	if got != nil || !errors.Is(err, wrapped) || !errors.Is(err, ErrReaderFailure) || err == store.ErrVaultSealStale || len(q.args) != 1 {
		t.Fatalf("wrapped stale authorized retry: err=%v lookups=%d", err, len(q.args))
	}
}

func TestByIDSnapshotMissingRefetchIsStale(t *testing.T) {
	box, row := snapshotFixture(t)
	q := &snapshotStore{fakeStore: &fakeStore{}, rows: []store.GetUserSecretCiphertextByIDRow{row}, epochs: []int64{8}, lookupErr: pgx.ErrNoRows}
	got, err := OpenByID(context.Background(), q, readerTestVault(box, q), box, row.UserID, row.ID)
	if err != store.ErrVaultSealStale || got != nil {
		t.Fatalf("missing refetch: %v", err)
	}
}
