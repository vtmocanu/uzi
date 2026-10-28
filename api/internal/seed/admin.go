package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Admin provisions the configured admin user if seeding is enabled and no
// user with that email exists yet. It uses the exact same argon2id hashing as
// registration and never touches an existing user's password. A concurrent
// create (unique violation) is treated as "already exists".
func Admin(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) error {
	if cfg.SeedEmail == "" {
		return nil
	}
	q := store.New(pool)
	if _, err := q.GetUserByEmail(ctx, cfg.SeedEmail); err == nil {
		slog.Info("seed admin already present, leaving untouched", "email", cfg.SeedEmail)
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("seed admin lookup: %w", err)
	}

	hash, err := auth.HashPassword(cfg.SeedPassword)
	if err != nil {
		return fmt.Errorf("seed admin hash: %w", err)
	}
	name := pgtype.Text{}
	if cfg.SeedName != "" {
		name = pgtype.Text{String: cfg.SeedName, Valid: true}
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("seed admin begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := q.WithTx(tx)
	if err := qtx.LockSettingsAdmission(ctx); err != nil {
		return fmt.Errorf("seed admin admission: %w", err)
	}
	policy, err := qtx.GetVaultPolicy(ctx)
	if err != nil {
		return fmt.Errorf("seed admin policy: %w", err)
	}
	mode := store.VaultModeProtected
	if policy == "optional" {
		mode = store.VaultModeUnprotected
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.RegistrationLockKey); err != nil {
		return fmt.Errorf("seed admin registration guard: %w", err)
	}
	if _, err := qtx.EnrollUser(ctx, store.EnrollUserParams{
		VaultMode:    mode,
		Email:        cfg.SeedEmail,
		PasswordHash: pgtype.Text{String: hash, Valid: true},
		DisplayName:  name,
		IsAdmin:      true,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil // raced with another creator; the user now exists
		}
		return fmt.Errorf("seed admin create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("seed admin commit: %w", err)
	}
	slog.Info("seeded admin user", "email", cfg.SeedEmail)
	return nil
}

// UnlockAdminVault warms the protected seed admin's DEK cache at boot.
// Off accounts are left untouched, including when the configured password changed.
func UnlockAdminVault(ctx context.Context, q interface {
	GetUserByEmail(context.Context, string) (store.User, error)
}, vlt *vault.Vault, cfg config.Config) error {
	if cfg.SeedEmail == "" {
		return nil
	}
	seedUser, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
	if err != nil {
		return fmt.Errorf("boot-unlock seed admin: look up %q: %w", cfg.SeedEmail, err)
	}
	if seedUser.VaultMode == store.VaultModeUnprotected {
		slog.Info("seed admin vault protection is off; boot unlock skipped", "email", cfg.SeedEmail)
		return nil
	}
	if err := vlt.Unlock(ctx, seedUser.ID, cfg.SeedPassword); err != nil {
		return fmt.Errorf("boot-unlock seed admin vault: %w", err)
	}
	slog.Info("boot-unlocked seed admin vault", "email", cfg.SeedEmail)
	return nil
}
