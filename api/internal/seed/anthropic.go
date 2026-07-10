package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// maxAnthropicTokenBytes bounds a seeded credential, mirroring
// handler.maxTokenBytes. Generous enough for both `claude setup-token` OAuth
// tokens and console API keys.
const maxAnthropicTokenBytes = 4096

// SecretStore is the subset of *store.Queries the Anthropic-token seed needs.
// Narrowing to an interface lets the seed be unit-tested against a fake store
// without a live database, mirroring the forge seed's Store. ListUserSecretsMeta
// (metadata only) is used for the create-only existence check so the seed never
// fetches ciphertext it does not need.
type SecretStore interface {
	GetUserByEmail(ctx context.Context, email string) (store.User, error)
	ListUserSecretsMeta(ctx context.Context, userID uuid.UUID) ([]store.ListUserSecretsMetaRow, error)
	UpsertUserSecret(ctx context.Context, arg store.UpsertUserSecretParams) (store.UpsertUserSecretRow, error)
}

// VaultSealer is the subset of *vault.Vault the seed needs: it seals the seeded
// token under the seed admin's per-user DEK (PRD #32), the same primitive the
// secrets handler uses. main boot-unlocks the seed admin's vault before this runs,
// so Seal succeeds; if it did not, Seal returns vault.ErrLocked and the seed fails
// loud (consistent with the other boot-fatal seed faults).
type VaultSealer interface {
	Seal(userID uuid.UUID, kind string, plaintext []byte) ([]byte, error)
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
// upsert) or a static operator misconfiguration (a malformed configured token),
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

	// DEK-seal under the seed admin's vault (unlocked at boot by main). The error
	// carries no plaintext; vault.ErrLocked here means the boot-unlock did not run,
	// which is a boot-fatal misconfiguration.
	sealed, err := sealer.Seal(user.ID, store.KindAnthropicToken, []byte(token))
	if err != nil {
		return fmt.Errorf("seed anthropic token: seal: %w", err)
	}

	if _, err := q.UpsertUserSecret(ctx, store.UpsertUserSecretParams{
		UserID:     user.ID,
		Kind:       store.KindAnthropicToken,
		Ciphertext: sealed,
		SealedWith: store.SealedWithDEK,
	}); err != nil {
		return fmt.Errorf("seed anthropic token: store: %w", err)
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
