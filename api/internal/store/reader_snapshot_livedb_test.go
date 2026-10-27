package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type readerTransitionQueries struct {
	*store.Queries
	afterFirst func() error
	args       []store.GetUserSecretCiphertextByIDParams
}

func (q *readerTransitionQueries) GetUserSecretCiphertextByID(ctx context.Context, p store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	q.args = append(q.args, p)
	row, err := q.Queries.GetUserSecretCiphertextByID(ctx, p)
	if err == nil && len(q.args) == 1 && q.afterFirst != nil {
		err = q.afterFirst()
	}
	return row, err
}

func TestReaderSnapshotQueriesLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	f := vlSeed(t, pool, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q := store.New(pool)
	var token, alias, account uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", f.owner).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='codex_auth'", f.owner).Scan(&alias); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT id FROM codex_provider_account WHERE user_id=$1 ORDER BY id LIMIT 1", f.owner).Scan(&account); err != nil {
		t.Fatal(err)
	}
	vlExec(t, pool, "UPDATE users SET vault_epoch=17 WHERE id=$1", f.owner)
	vlExec(t, pool, "INSERT INTO codex_credential_state (user_secret_id,user_id,status,material_revision) VALUES ($1,$2,'staging',6)", alias, f.owner)
	tokenRow, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{ID: token, UserID: f.owner})
	if err != nil || tokenRow.ID != token || tokenRow.UserID != f.owner || tokenRow.VaultEpoch != 17 || tokenRow.EnablementRev != 11 || tokenRow.MaterialRevision.Valid {
		t.Fatalf("non-Codex snapshot missing or alias join lost nullability: %v", err)
	}
	aliasRow, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{ID: alias, UserID: f.owner})
	if err != nil || !aliasRow.Disabled || aliasRow.VaultEpoch != 17 || aliasRow.MaterialRevision != (pgtype.Int8{Int64: 6, Valid: true}) {
		t.Fatalf("alias authority snapshot: %v", err)
	}
	single, err := q.GetAnthropicTokenToPoll(ctx, store.GetAnthropicTokenToPollParams{UserID: f.owner, SecretID: pgtype.UUID{Bytes: token, Valid: true}})
	if err != nil || single.VaultEpoch != 17 || single.ID != token || single.EnablementRev != tokenRow.EnablementRev {
		t.Fatalf("poll snapshot: %v", err)
	}
	bulk, err := q.ListAnthropicTokensToPoll(ctx)
	if err != nil || len(bulk) != 1 || bulk[0].VaultEpoch != 17 || bulk[0].ID != token {
		t.Fatalf("bulk snapshot: %v", err)
	}
	acct, err := q.GetCodexProviderAccountSnapshot(ctx, store.GetCodexProviderAccountSnapshotParams{UserID: f.owner, ID: account})
	if err != nil || acct.VaultEpoch != 17 || acct.CodexProviderAccount.ID != account || acct.CodexProviderAccount.UserID != f.owner {
		t.Fatalf("account snapshot: %v", err)
	}
	foreign := uuid.New()
	if _, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{ID: token, UserID: foreign}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign secret: %v", err)
	}
	if _, err := q.GetCodexProviderAccountSnapshot(ctx, store.GetCodexProviderAccountSnapshotParams{ID: account, UserID: foreign}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign account: %v", err)
	}
}

func TestReaderSnapshotLifecycleRetryLiveDB(t *testing.T) {
	for _, direction := range []string{"enable", "disable"} {
		for _, replace := range []bool{false, true} {
			name := direction
			if replace {
				name += "/credential rotation"
			}
			t.Run(name, func(t *testing.T) {
				pool := vlIsolatedPool(t)
				f := vlSeed(t, pool, true, true)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				q := store.New(pool)
				v := vault.NewPostgres(f.master, pool)
				if err := v.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				if direction == "enable" {
					if err := v.Disable(ctx, f.owner); err != nil {
						t.Fatal(err)
					}
				}
				var id uuid.UUID
				if err := pool.QueryRow(ctx, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", f.owner).Scan(&id); err != nil {
					t.Fatal(err)
				}
				hooked := &readerTransitionQueries{Queries: q}
				hooked.afterFirst = func() error {
					var err error
					if direction == "enable" {
						err = v.Enable(ctx, f.owner, vlLoginPhrase)
					} else {
						err = v.Disable(ctx, f.owner)
					}
					if err != nil {
						return err
					}
					if replace {
						sealed, err := v.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("replacement material"), vault.SealForUse)
						if err != nil {
							return err
						}
						_, err = q.RotateUserSecret(ctx, store.RotateUserSecretParams{ID: id, UserID: f.owner, Ciphertext: sealed.Ciphertext, SealedWith: sealed.SealedWith})
						return err
					}
					return nil
				}
				plain, err := secretopen.OpenByIDOfKind(ctx, hooked, v, f.master, f.owner, id, store.KindAnthropicToken)
				if replace {
					if err != store.ErrVaultSealStale || plain != nil {
						t.Fatalf("adopted replacement after conversion: error=%v plaintext-present=%v", err, plain != nil)
					}
				} else if err != nil || string(plain) != "secret-"+store.KindAnthropicToken {
					t.Fatalf("conversion lost original plaintext: error=%v matches=%v", err, string(plain) == "secret-"+store.KindAnthropicToken)
				}
				if len(hooked.args) != 2 {
					t.Fatalf("same-ID lookup count=%d want=2", len(hooked.args))
				}
				for _, p := range hooked.args {
					if p.ID != id || p.UserID != f.owner {
						t.Fatal("retry changed selected identity")
					}
				}
			})
		}
	}
}
