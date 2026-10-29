package workersvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type accountReaderStore struct {
	*rejFakeStore
	rows                []codexAccountSnapshot
	args                []store.GetCodexProviderAccountSnapshotParams
	stateReads          int
	states              []int64
	mode                string
	lookupErr, stateErr error
}

func (q *accountReaderStore) GetCodexProviderAccountSnapshot(_ context.Context, arg store.GetCodexProviderAccountSnapshotParams) (store.GetCodexProviderAccountSnapshotRow, error) {
	q.args = append(q.args, arg)
	if len(q.args) > len(q.rows) {
		return store.GetCodexProviderAccountSnapshotRow{}, q.lookupErr
	}
	row := q.rows[len(q.args)-1]
	return store.GetCodexProviderAccountSnapshotRow{CodexProviderAccount: row.CodexProviderAccount, VaultEpoch: row.VaultEpoch}, nil
}
func (q *accountReaderStore) GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error) {
	q.stateReads++
	if q.stateErr != nil {
		return store.GetUserVaultStateRow{}, q.stateErr
	}
	i := q.stateReads - 1
	if i >= len(q.states) {
		i = len(q.states) - 1
	}
	return store.GetUserVaultStateRow{VaultMode: q.mode, VaultEpoch: q.states[i]}, nil
}
func (*accountReaderStore) GetCodexRefreshIntent(context.Context, store.GetCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	return store.CodexRefreshIntent{}, pgx.ErrNoRows
}

// accountReaderFixture seals each row at its own epoch. The state reader models
// an atomic snapshot followed by a conversion before opening that snapshot.
func accountReaderFixture(t *testing.T, transition string, recovery bool) (*Service, *accountReaderStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	m := newMemVaultStore()
	m.states[owner] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 7}
	v := newMemoryVault(box, m)
	raw := []byte(`{"access_token":"original","refresh_token":"renewal"}`)
	seal := func() vault.SealedMaterial {
		out, err := v.Seal(ctx, owner, store.KindCodexAuth, raw, vault.SealForUse)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	const phrase = "account-reader-fixture-passphrase"
	if transition == "disable" || transition == "ABA" {
		m.states[owner] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 6}
		if err := v.Enable(ctx, owner, phrase); err != nil {
			t.Fatal(err)
		}
	}
	original := seal()
	switch transition {
	case "enable":
		if err := v.Enable(ctx, owner, phrase); err != nil {
			t.Fatal(err)
		}
	case "disable", "ABA":
		if err := v.Disable(ctx, owner); err != nil {
			t.Fatal(err)
		}
		if transition == "ABA" {
			if err := v.Enable(ctx, owner, phrase); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatal("unknown fixture transition")
	}
	fresh := seal()
	mode := fresh.Mode
	if transition == "ABA" {
		if original.SealedWith != store.SealedWithDEK || fresh.SealedWith != store.SealedWithDEK || fresh.Epoch <= original.Epoch {
			t.Fatal("ABA fixture did not retire and replace a protected epoch")
		}
		if _, err := v.Open(owner, store.KindCodexAuth, original.SealedWith, original.Ciphertext); err == nil {
			t.Fatal("ABA fixture still opens the retired DEK's ciphertext")
		}
	}
	acct := store.CodexProviderAccount{ID: uuid.New(), UserID: owner, ProviderUserID: "user", WorkspaceAccountID: "workspace",
		Generation: 12, CredentialRevision: 4, CoordState: codexCoordIdle,
		UpdatedAt:   pgtype.Timestamptz{Time: time.Unix(123, 0).UTC(), Valid: true},
		SealedLogin: original.Ciphertext, SealedWith: original.SealedWith}
	a := codexAccountSnapshot{CodexProviderAccount: acct, VaultEpoch: original.Epoch}
	b := a
	b.VaultEpoch, b.SealedLogin, b.SealedWith = fresh.Epoch, fresh.Ciphertext, fresh.SealedWith
	if recovery {
		a.RecoverySealed, a.RecoverySealedWith = original.Ciphertext, pgtype.Text{String: original.SealedWith, Valid: true}
		b.RecoverySealed, b.RecoverySealedWith = fresh.Ciphertext, pgtype.Text{String: fresh.SealedWith, Valid: true}
	}
	q := &accountReaderStore{rejFakeStore: &rejFakeStore{acct: acct}, rows: []codexAccountSnapshot{a, b},
		states: []int64{fresh.Epoch, fresh.Epoch}, mode: mode}
	s := &Service{q: q, box: box, vlt: v}
	return s, q, vault.WithStateReader(ctx, q)
}
func accountReaderOpen(t *testing.T, s *Service, q *accountReaderStore, ctx context.Context, recovery bool) (codexLoginBlob, error) {
	t.Helper()
	row := q.rows[0]
	snapshot, err := readCodexAccountSnapshot(ctx, q, store.GetCodexProviderAccountSnapshotParams{UserID: row.UserID, ID: row.ID})
	if err != nil {
		return codexLoginBlob{}, err
	}
	return s.openCodexAccountSlot(ctx, row.UserID, snapshot, recovery)
}
func TestCodexAccountSnapshotConversions(t *testing.T) {
	for _, transition := range []string{"enable", "disable", "ABA"} {
		for _, recovery := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovery=%v", transition, recovery), func(t *testing.T) {
				s, q, ctx := accountReaderFixture(t, transition, recovery)
				blob, err := accountReaderOpen(t, s, q, ctx, recovery)
				if err != nil || blob.AccessToken != "original" || blob.RefreshToken != "renewal" || len(q.args) != 2 || q.stateReads != 2 {
					t.Fatalf("conversion: err=%v token-match=%v lookups=%d opens=%d", err, blob.AccessToken == "original", len(q.args), q.stateReads)
				}
				for _, arg := range q.args {
					if arg.UserID != q.rows[0].UserID || arg.ID != q.rows[0].ID {
						t.Fatal("selection changed")
					}
				}
			})
		}
	}
}
func TestCodexAccountSnapshotMetadataRefused(t *testing.T) {
	// Cover every metadata field, so additions to the common model need a proof too.
	typ := reflect.TypeFor[store.CodexProviderAccount]()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		switch field.Name {
		case "SealedLogin", "SealedWith", "RecoverySealed", "RecoverySealedWith":
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			s, q, ctx := accountReaderFixture(t, "ABA", true)
			value := reflect.ValueOf(&q.rows[1].CodexProviderAccount).Elem().FieldByName(field.Name)
			// Change scalar metadata, or nullable validity for pgtype fields.
			switch value.Kind() {
			case reflect.String:
				value.SetString(value.String() + "replacement")
			case reflect.Int64:
				value.SetInt(value.Int() + 1)
			case reflect.Bool:
				value.SetBool(!value.Bool())
			case reflect.Array:
				value.Index(0).SetUint(value.Index(0).Uint() ^ 1)
			case reflect.Struct:
				valid := value.FieldByName("Valid")
				if !valid.IsValid() {
					t.Fatalf("new metadata type needs a mutation: %s", field.Name)
				}
				valid.SetBool(!valid.Bool())
			default:
				t.Fatalf("new metadata kind needs a mutation: %s", field.Name)
			}
			blob, err := accountReaderOpen(t, s, q, ctx, true)
			if err != store.ErrVaultSealStale || blob.AccessToken != "" || len(q.args) != 2 || q.stateReads != 1 {
				t.Fatalf("metadata accepted: err=%v token-present=%v lookups=%d opens=%d", err, blob.AccessToken != "", len(q.args), q.stateReads)
			}
		})
	}
}
func TestCodexAccountSnapshotSlotsAndMarkers(t *testing.T) {
	for _, change := range []string{"live removed", "recovery removed", "live marker", "recovery marker", "recovery discriminator absent"} {
		t.Run(change, func(t *testing.T) {
			s, q, ctx := accountReaderFixture(t, "ABA", true)
			switch change {
			case "live removed":
				q.rows[1].SealedLogin = nil
			case "recovery removed":
				q.rows[1].RecoverySealed = nil
			case "live marker":
				q.rows[1].SealedWith = "unknown"
			case "recovery marker":
				q.rows[1].RecoverySealedWith.String = "unknown"
			case "recovery discriminator absent":
				q.rows[1].RecoverySealedWith.Valid = false
			}
			blob, err := accountReaderOpen(t, s, q, ctx, true)
			if err != store.ErrVaultSealStale || blob.AccessToken != "" || q.stateReads != 1 || len(q.args) != 2 {
				t.Fatalf("slot replacement accepted: %v", err)
			}
		})
	}
}
func TestCodexAccountSnapshotEmptyRecoveryMarkerRefused(t *testing.T) {
	s, q, ctx := accountReaderFixture(t, "ABA", false)
	q.rows[1].RecoverySealedWith = pgtype.Text{String: store.SealedWithMasterAAD, Valid: true}
	blob, err := accountReaderOpen(t, s, q, ctx, false)
	if err != store.ErrVaultSealStale || blob.AccessToken != "" || q.stateReads != 1 || len(q.args) != 2 {
		t.Fatalf("absent recovery slot gained a discriminator: err=%v token-present=%v reads=%d lookups=%d", err, blob.AccessToken != "", q.stateReads, len(q.args))
	}
}

func TestCodexAccountSnapshotSecondTransitionStops(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		s, q, ctx := accountReaderFixture(t, "ABA", recovery)
		q.states[1]++
		blob, err := accountReaderOpen(t, s, q, ctx, recovery)
		if err != store.ErrVaultSealStale || blob.AccessToken != "" || len(q.args) != 2 || q.stateReads != 2 {
			t.Fatalf("unbounded retry: %v reads=%d opens=%d", err, len(q.args), q.stateReads)
		}
	}
}
func TestCodexAccountSnapshotNormalAndInvalid(t *testing.T) {
	for _, mode := range []string{"normal", "unknown", "badtag", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s, q, ctx := accountReaderFixture(t, "ABA", false)
			q.rows = []codexAccountSnapshot{q.rows[1]}
			switch mode {
			case "unknown":
				q.rows[0].SealedWith = "unknown"
			case "badtag":
				q.rows[0].SealedLogin = append([]byte(nil), q.rows[0].SealedLogin...)
				q.rows[0].SealedLogin[len(q.rows[0].SealedLogin)-1] ^= 1
			case "malformed":
				sealed, err := s.vlt.Seal(ctx, q.rows[0].UserID, store.KindCodexAuth, []byte("{"), vault.SealForUse)
				if err != nil {
					t.Fatal(err)
				}
				q.stateReads = 0
				q.rows[0].SealedLogin, q.rows[0].SealedWith, q.rows[0].VaultEpoch = sealed.Ciphertext, sealed.SealedWith, sealed.Epoch
			}
			blob, err := accountReaderOpen(t, s, q, ctx, false)
			switch mode {
			case "normal":
				if err != nil || blob.AccessToken != "original" {
					t.Fatalf("normal: %v", err)
				}
			case "malformed":
				if !errors.Is(err, ErrCodexLoginBlob) {
					t.Fatalf("malformed: %v", err)
				}
			default:
				if !errors.Is(err, errCredentialUnavailable) || blob.AccessToken != "" {
					t.Fatalf("invalid: %v", err)
				}
			}
			if len(q.args) != 1 {
				t.Fatalf("unnecessary credential lookup: %d", len(q.args))
			}
		})
	}
}
func TestCodexAccountSnapshotReaderFailures(t *testing.T) {
	infra := errors.New("snapshot infrastructure unavailable")
	for _, mode := range []string{"initial", "refetch", "state", "wrapped stale", "missing", "nil vault"} {
		t.Run(mode, func(t *testing.T) {
			s, q, ctx := accountReaderFixture(t, "ABA", false)
			switch mode {
			case "initial":
				q.rows = nil
				q.lookupErr = infra
			case "refetch":
				q.rows = q.rows[:1]
				q.lookupErr = infra
			case "state":
				q.stateErr = infra
			case "wrapped stale":
				q.stateErr = fmt.Errorf("wrapped: %w", store.ErrVaultSealStale)
			case "missing":
				q.rows = q.rows[:1]
				q.lookupErr = pgx.ErrNoRows
			case "nil vault":
				s.vlt = nil
			}
			// Preserve the selected identity even when the query returns no row.
			row := q.acct
			snapshot, err := readCodexAccountSnapshot(ctx, q, store.GetCodexProviderAccountSnapshotParams{UserID: row.UserID, ID: row.ID})
			if err == nil {
				_, err = s.openCodexAccountLogin(ctx, row.UserID, snapshot)
			}
			if mode == "missing" {
				if err != store.ErrVaultSealStale {
					t.Fatalf("missing: %v", err)
				}
			} else if !errors.Is(err, secretopen.ErrReaderFailure) || errors.Is(err, errCredentialUnavailable) {
				t.Fatalf("failure collapsed: %v", err)
			}
			if mode == "initial" || mode == "refetch" || mode == "state" {
				if !errors.Is(err, infra) {
					t.Fatalf("cause lost: %v", err)
				}
			}
			if mode == "wrapped stale" && len(q.args) != 1 {
				t.Fatal("wrapped stale retried")
			}
		})
	}
}
func TestCodexAccountUsageCryptoStaleNoProvider(t *testing.T) {
	s, q, ctx := accountReaderFixture(t, "ABA", false)
	q.states[1]++
	calls := 0
	reader := linkedProbeReader(func(context.Context, string, string) (codexauth.UsageReading, error) {
		calls++
		return codexauth.UsageReading{}, nil
	})
	row := q.rows[0]
	snapshot, err := readCodexAccountSnapshot(ctx, q, store.GetCodexProviderAccountSnapshotParams{UserID: row.UserID, ID: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, failure, unauthorized := s.readCodexUsageAttempt(ctx, reader, snapshot)
	if failure == nil || failure.Kind != CodexUsageFailTransient || unauthorized || calls != 0 || q.markReauth != 0 || len(q.args) != 2 {
		t.Fatalf("crypto stale escaped: failure=%v unauthorized=%v provider=%d reauth=%d lookups=%d", failure, unauthorized, calls, q.markReauth, len(q.args))
	}
}
