package handler

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/seed"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func seedEnrollmentConfig() config.Config {
	return config.Config{
		SeedEmail:          uuid.NewString() + "@example.com",
		SeedPassword:       "seed-enrollment-password",
		SeedName:           "Seed admin",
		SeedAnthropicToken: "sk-ant-" + "oat-seed-fixture",
	}
}

func seedEnrollmentWrapper(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM user_vaults WHERE user_id=$1)", id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// Snapshot after any deliberate lifecycle transition, before the refused seed:
// every persisted user/wrapper/secret field must survive the failed write.
func seedEnrollmentSnapshot(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'user', (SELECT to_jsonb(u) FROM users u WHERE id=$1),
 'vault', (SELECT to_jsonb(v) FROM user_vaults v WHERE user_id=$1),
 'secrets', (SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY s.id), '[]'::jsonb) FROM user_secrets s WHERE user_id=$1)
 )::text`, id).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestVaultSeedAdminPolicyLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, policy, mode string }{
		{"optional", "optional", store.VaultModeUnprotected},
		{"required", "required", store.VaultModeProtected},
		{"missing", "", store.VaultModeProtected},
		{"invalid", "invalid", store.VaultModeProtected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enrollmentPolicy(t, pool, tc.policy)
			cfg := seedEnrollmentConfig()
			q := store.New(pool)
			if err := seed.Admin(ctx, pool, cfg); err != nil {
				t.Fatal(err)
			}
			user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
			if err != nil {
				t.Fatal(err)
			}
			if user.VaultMode != tc.mode || user.VaultEpoch != 0 || !user.IsAdmin || !user.IsActive || user.TokenVersion != 1 || user.AutopilotEnabled {
				t.Fatalf("fresh seed defaults: mode=%s epoch=%d admin=%v active=%v version=%d autopilot=%v", user.VaultMode, user.VaultEpoch, user.IsAdmin, user.IsActive, user.TokenVersion, user.AutopilotEnabled)
			}
			if !user.DisplayName.Valid || user.DisplayName.String != cfg.SeedName {
				t.Fatal("seed name missing")
			}
			if seedEnrollmentWrapper(t, pool, user.ID) {
				t.Fatal("admin enrollment must defer wrapper creation to boot unlock")
			}
			v := vault.NewPostgres(oidcLiveBox(t), pool)
			if err := seed.UnlockAdminVault(ctx, q, v, cfg); err != nil {
				t.Fatal(err)
			}
			protected := tc.mode == store.VaultModeProtected
			if seedEnrollmentWrapper(t, pool, user.ID) != protected || v.Unlocked(user.ID) != protected {
				t.Fatal("boot wrapper/cache does not match seed mode")
			}
			after, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
			if err != nil {
				t.Fatal(err)
			}
			wantEpoch := int64(0)
			if protected {
				wantEpoch = 1
			}
			if after.VaultMode != tc.mode || after.VaultEpoch != wantEpoch {
				t.Fatal("boot unlock mode/epoch mismatch")
			}

			// A later seed config must never replace the original password or name.
			before := seedEnrollmentSnapshot(t, pool, user.ID)
			changed := cfg
			changed.SeedPassword, changed.SeedName = "different-seed-password", "Changed name"
			if err := seed.Admin(ctx, pool, changed); err != nil {
				t.Fatal(err)
			}
			if seedEnrollmentSnapshot(t, pool, user.ID) != before {
				t.Fatal("repeat seed changed existing account")
			}
			ok, err := auth.VerifyPassword(cfg.SeedPassword, after.PasswordHash.String)
			if err != nil || !ok {
				t.Fatal("original seed password no longer verifies")
			}
			v.Lock(user.ID)
			if err := seed.UnlockAdminVault(ctx, q, v, changed); protected && err == nil {
				t.Fatal("changed password unlocked protected seed")
			} else if !protected && err != nil {
				t.Fatal(err)
			}
			if v.Unlocked(user.ID) {
				t.Fatal("changed seed password populated DEK cache")
			}
			if seedEnrollmentSnapshot(t, pool, user.ID) != before {
				t.Fatal("changed boot password modified persisted state")
			}
			if err := seed.UnlockAdminVault(ctx, q, v, cfg); err != nil {
				t.Fatal(err)
			}
			if v.Unlocked(user.ID) != protected {
				t.Fatal("correct password boot unlock cache mismatch")
			}
		})
	}
}

func TestVaultSeedExistingOffUnderRequiredLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	enrollmentPolicy(t, pool, "optional")
	cfg := seedEnrollmentConfig()
	if err := seed.Admin(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
	if err != nil {
		t.Fatal(err)
	}
	before := seedEnrollmentSnapshot(t, pool, user.ID)
	mustExecT(ctx, t, pool, "UPDATE app_settings SET value='required' WHERE key='vault_policy'")
	cfg.SeedPassword = "changed-off-seed-password"
	if err := seed.Admin(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	v := vault.NewPostgres(oidcLiveBox(t), pool)
	if err := seed.UnlockAdminVault(ctx, q, v, cfg); err != nil {
		t.Fatal(err)
	}
	if seedEnrollmentSnapshot(t, pool, user.ID) != before || seedEnrollmentWrapper(t, pool, user.ID) || v.Unlocked(user.ID) {
		t.Fatal("required policy converted existing Off seed")
	}
	if err := seed.AnthropicToken(ctx, seed.NewPostgresSecretStore(pool), v, cfg); err != nil {
		t.Fatal(err)
	}
	material, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{UserID: user.ID, Kind: store.KindAnthropicToken})
	if err != nil {
		t.Fatal(err)
	}
	if material.SealedWith != store.SealedWithMasterAAD || seedEnrollmentWrapper(t, pool, user.ID) || v.Unlocked(user.ID) {
		t.Fatal("Off token seed must use master_aad without wrapper/cache")
	}
}

func TestVaultSeedAnthropicModesLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	for _, policy := range []string{"optional", "required"} {
		t.Run(policy, func(t *testing.T) {
			enrollmentPolicy(t, pool, policy)
			cfg := seedEnrollmentConfig()
			q := store.New(pool)
			if err := seed.Admin(ctx, pool, cfg); err != nil {
				t.Fatal(err)
			}
			v := vault.NewPostgres(oidcLiveBox(t), pool)
			if err := seed.UnlockAdminVault(ctx, q, v, cfg); err != nil {
				t.Fatal(err)
			}
			user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
			if err != nil {
				t.Fatal(err)
			}
			adapter := seed.NewPostgresSecretStore(pool)
			if err := seed.AnthropicToken(ctx, adapter, v, cfg); err != nil {
				t.Fatal(err)
			}
			material, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{UserID: user.ID, Kind: store.KindAnthropicToken})
			if err != nil {
				t.Fatal(err)
			}
			want := store.SealedWithMasterAAD
			if policy == "required" {
				want = store.SealedWithDEK
			}
			if material.SealedWith != want {
				t.Fatalf("sealed_with=%s want=%s", material.SealedWith, want)
			}
			plain, err := v.OpenAtEpoch(ctx, user.ID, store.KindAnthropicToken, material.SealedWith, material.Ciphertext, user.VaultEpoch)
			if err != nil || !bytes.Equal(plain, []byte(cfg.SeedAnthropicToken)) {
				t.Fatal("seeded token did not round trip")
			}
			meta, err := q.ListUserSecretsForKind(ctx, store.ListUserSecretsForKindParams{UserID: user.ID, Kind: store.KindAnthropicToken})
			if err != nil {
				t.Fatal(err)
			}
			if len(meta) != 1 || meta[0].Label != store.LabelDefaultSecret || !meta[0].IsDefault {
				t.Fatal("seeded token default metadata mismatch")
			}
			before := seedEnrollmentSnapshot(t, pool, user.ID)
			cfg.SeedAnthropicToken = "invalid replacement with spaces"
			v.Lock(user.ID)
			if err := seed.AnthropicToken(ctx, adapter, nil, cfg); err != nil {
				t.Fatal(err)
			}
			if seedEnrollmentSnapshot(t, pool, user.ID) != before {
				t.Fatal("existing seed token was changed")
			}
		})
	}
}

// The wrapper runs the supplied action after capturing the seal, before
// AnthropicToken opens its write transaction. No timing/sleeps.
type seedEnrollmentSealer struct {
	v     *vault.Vault
	after func() error
}

func (s seedEnrollmentSealer) Seal(ctx context.Context, id uuid.UUID, kind string, raw []byte, purpose vault.SealPurpose) (vault.SealedMaterial, error) {
	material, err := s.v.Seal(ctx, id, kind, raw, purpose)
	if err != nil {
		return material, err
	}
	if err := s.after(); err != nil {
		return vault.SealedMaterial{}, err
	}
	return material, nil
}

func TestVaultSeedAnthropicConcurrentCreateLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	enrollmentPolicy(t, pool, "optional")
	cfg := seedEnrollmentConfig()
	if err := seed.Admin(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
	if err != nil {
		t.Fatal(err)
	}
	v := vault.NewPostgres(oidcLiveBox(t), pool)
	adapter := seed.NewPostgresSecretStore(pool)
	var before string
	sealer := seedEnrollmentSealer{v: v, after: func() error {
		winner := cfg
		winner.SeedAnthropicToken = "concurrent-winner-token"
		if err := seed.AnthropicToken(ctx, adapter, v, winner); err != nil {
			return err
		}
		before = seedEnrollmentSnapshot(t, pool, user.ID)
		return nil
	}}
	if err := seed.AnthropicToken(ctx, adapter, sealer, cfg); err != nil {
		t.Fatal(err)
	}
	if before == "" || seedEnrollmentSnapshot(t, pool, user.ID) != before {
		t.Fatal("seed overwrote concurrent token creator")
	}
}

func TestVaultSeedAnthropicStaleSealLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	ctx := context.Background()
	for _, transition := range []string{"enable", "disable", "disable-enable"} {
		t.Run(transition, func(t *testing.T) {
			enrollmentPolicy(t, pool, "optional")
			cfg := seedEnrollmentConfig()
			q := store.New(pool)
			if err := seed.Admin(ctx, pool, cfg); err != nil {
				t.Fatal(err)
			}
			user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
			if err != nil {
				t.Fatal(err)
			}
			v := vault.NewPostgres(oidcLiveBox(t), pool)
			if transition != "enable" {
				if err := v.Enable(ctx, user.ID, cfg.SeedPassword); err != nil {
					t.Fatal(err)
				}
			}
			// Keep unrelated material present so refusal proves more than an empty DB.
			sibling, err := v.Seal(ctx, user.ID, store.KindOpenAIAPIKey, []byte("unrelated-seed-material"), vault.SealForUse)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
			tq := q.WithTx(tx)
			if err := tq.LockSecretMutation(ctx, user.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tq.FenceVaultWrite(ctx, user.ID, sibling.Mode, sibling.Epoch); err != nil {
				t.Fatal(err)
			}
			if _, err := tq.InsertUserSecret(ctx, store.InsertUserSecretParams{UserID: user.ID, Kind: store.KindOpenAIAPIKey, Label: store.LabelDefaultSecret, WantDefault: true, Ciphertext: sibling.Ciphertext, SealedWith: sibling.SealedWith}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			var before string
			sealer := seedEnrollmentSealer{v: v, after: func() error {
				if transition != "enable" {
					if err := v.Disable(ctx, user.ID); err != nil {
						return err
					}
				}
				if transition != "disable" {
					if err := v.Enable(ctx, user.ID, cfg.SeedPassword); err != nil {
						return err
					}
				}
				before = seedEnrollmentSnapshot(t, pool, user.ID)
				return nil
			}}
			err = seed.AnthropicToken(ctx, seed.NewPostgresSecretStore(pool), sealer, cfg)
			if !errors.Is(err, store.ErrVaultSealStale) {
				t.Fatalf("stale seed error=%v", err)
			}
			if before == "" || seedEnrollmentSnapshot(t, pool, user.ID) != before {
				t.Fatal("stale seed changed existing state/material")
			}
			if _, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{UserID: user.ID, Kind: store.KindOpenAIAPIKey}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
