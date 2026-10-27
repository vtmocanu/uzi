package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// maxAnthropicTokenBytes bounds a seeded credential, mirroring
// handler.maxTokenBytes. Generous enough for both `claude setup-token` OAuth
// tokens and console API keys.
const maxAnthropicTokenBytes = 4096

// SecretStore provides boot lookups and an atomic create-only token write.
type SecretStore interface {
	GetUserByEmail(context.Context, string) (store.User, error)
	ListUserSecretsMeta(context.Context, uuid.UUID) ([]store.ListUserSecretsMetaRow, error)
	WithSecretTx(context.Context, func(SecretTx) error) error
}

// SecretTx holds the advisory guard and vault fence until the insert commits.
type SecretTx interface {
	LockSecretMutation(context.Context, uuid.UUID) error
	FenceVaultWrite(context.Context, uuid.UUID, string, int64) (store.LockUserVaultStateRow, error)
	ListUserSecretsMeta(context.Context, uuid.UUID) ([]store.ListUserSecretsMetaRow, error)
	InsertUserSecret(context.Context, store.InsertUserSecretParams) (store.InsertUserSecretRow, error)
}

type postgresSecretStore struct {
	*store.Queries
	pool *pgxpool.Pool
}

// NewPostgresSecretStore supplies actual transaction-bound queries to the seed.
func NewPostgresSecretStore(pool *pgxpool.Pool) SecretStore {
	return &postgresSecretStore{Queries: store.New(pool), pool: pool}
}

func (s *postgresSecretStore) WithSecretTx(ctx context.Context, fn func(SecretTx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// VaultSealer returns ciphertext and its durable mode/epoch snapshot.
// The writer must fence that snapshot in the same transaction as persistence.
type VaultSealer interface {
	Seal(context.Context, uuid.UUID, string, []byte, vault.SealPurpose) (vault.SealedMaterial, error)
}

// AnthropicToken seeds the seed admin's Anthropic token from
// UZI_SEED_ANTHROPIC_TOKEN when configured. Like the admin and forge seeds it is
// safe to call unconditionally (a no-op when disabled) and create-only.
//
// Testing-credentials policy: it seeds the operator's EXISTING token from the
// environment. It NEVER invokes `claude setup-token`, never mints or generates a
// credential, and does NO network/live validation at boot — only a local format
// check. The token value is never logged (presence/absence only) and never
// appears in any returned error.
//
// Create-only: if the seed admin already has an anthropic_token secret it is
// left untouched. So a UI-set token survives restarts, and a `down/up` that kept
// the DB won't clobber it; only a fresh `down -v` re-seeds from the env.
//
// Failure stance: every failure here is either a DB fault (user lookup, list,
// insert) or a static operator misconfiguration (a malformed configured token),
// both of which the codebase treats as boot-fatal (see loadSeedAdmin /
// loadSeedForge). There is no network/outage condition to defer, so — unlike the
// forge seed — nothing is swallowed.
func AnthropicToken(ctx context.Context, q SecretStore, sealer VaultSealer, cfg config.Config) error {
	if cfg.SeedAnthropicToken == "" {
		return nil
	}

	user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
	if err != nil {
		// The admin seed runs first and provisions this user; a lookup failure
		// here is a real DB/ordering fault, not an expected "absent" case.
		return fmt.Errorf("seed anthropic token: look up seed admin %q: %w", cfg.SeedEmail, err)
	}

	// Create-only: never overwrite an existing token (UI-set or already seeded).
	secrets, err := q.ListUserSecretsMeta(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("seed anthropic token: list secrets: %w", err)
	}
	for _, s := range secrets {
		if s.Kind == store.KindAnthropicToken {
			slog.Info("seed anthropic token already present, leaving untouched", "email", cfg.SeedEmail)
			return nil
		}
	}

	// Format-only check (no network, never mints). The error carries no token
	// bytes (see validateAnthropicTokenFormat).
	token, err := validateAnthropicTokenFormat(cfg.SeedAnthropicToken)
	if err != nil {
		return fmt.Errorf("seed anthropic token: %w", err)
	}

	// Seal before opening the write transaction; a lifecycle change in between
	// is refused by the mode/epoch fence, never retried with old material.
	if sealer == nil {
		return fmt.Errorf("seed anthropic token: seal: %w", vault.ErrNotConfigured)
	}
	sealed, err := sealer.Seal(ctx, user.ID, store.KindAnthropicToken, []byte(token), vault.SealForUse)
	if err != nil {
		return fmt.Errorf("seed anthropic token: seal: %w", err)
	}
	inserted := false
	if err := q.WithSecretTx(ctx, func(tx SecretTx) error {
		if err := tx.LockSecretMutation(ctx, user.ID); err != nil {
			return err
		}
		if _, err := tx.FenceVaultWrite(ctx, user.ID, sealed.Mode, sealed.Epoch); err != nil {
			return err
		}
		// Recheck after serialization: a concurrent creator may have won since
		// the cheap boot lookup. Any token of this kind makes seeding a no-op.
		current, err := tx.ListUserSecretsMeta(ctx, user.ID)
		if err != nil {
			return err
		}
		for _, s := range current {
			if s.Kind == store.KindAnthropicToken {
				return nil
			}
		}
		if _, err := tx.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID:      user.ID,
			Kind:        store.KindAnthropicToken,
			Label:       store.LabelDefaultSecret,
			WantDefault: true,
			Ciphertext:  sealed.Ciphertext,
			SealedWith:  sealed.SealedWith,
		}); err != nil {
			return err
		}
		inserted = true
		return nil
	}); err != nil {
		return fmt.Errorf("seed anthropic token: store: %w", err)
	}
	if !inserted {
		return nil
	}

	slog.Info("seeded anthropic token", "email", cfg.SeedEmail)
	return nil
}

// validateAnthropicTokenFormat trims and sanity-checks a configured token,
// mirroring handler.validateAnthropicToken: it makes no assumption about the
// token's prefix or format, only that it is non-empty, within the length bound,
// and free of interior whitespace and control characters. Errors are
// deliberately generic and NEVER include the token bytes. Kept in sync with the
// handler validator by hand (duplicated to avoid importing the HTTP handler
// package into this boot-time seed).
func validateAnthropicTokenFormat(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", errors.New("token must not be empty")
	}
	if len(token) > maxAnthropicTokenBytes {
		return "", fmt.Errorf("token must be at most %d bytes", maxAnthropicTokenBytes)
	}
	for _, r := range token {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("token must not contain whitespace or control characters")
		}
	}
	return token, nil
}
