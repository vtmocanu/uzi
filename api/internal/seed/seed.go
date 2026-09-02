// Package seed provides optional startup seeding that mirrors the admin-user
// seed: it provisions a forge connection and enables a set of repos for the
// seed admin from environment config. Like the admin seed it is create-only —
// it never overwrites an existing connection — and it is a no-op unless a seed
// PAT is configured. It reuses the same verify → encrypt → store primitives the
// connect handler uses rather than duplicating them.
package seed

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// seedForgeType is the forge the startup seed provisions. It is the seed's ONE
// forge-type site: the presence check, the client build, and the persisted row
// below must all name the same forge, and before this constant they were three
// independent literals that merely happened to agree. Disagreement between them
// is not a compile error — it is a seed that verifies one forge, stores another,
// and re-seeds on every boot because its own presence check never matches the row
// it wrote. One name makes that class of bug unrepresentable.
//
// Pinned to GitLab, and NOT read from config, deliberately (PRD #65 M6a). The
// migration alongside this widens the forge_type CHECK to admit 'forgejo' (and,
// since PRD #238 M5, 'github' too), but that widening is only safe while nothing
// can write such a row: handler/forge.go still refuses those types (:158), so the
// API cannot create one. The seed bypasses
// that handler entirely — it calls UpsertForgeConnection directly — so an
// operator-settable seed type would be a second, ungated door to the exact row
// the gate exists to prevent, and PRD #65's dark-landing property ("no forgejo row
// can exist while the handler rejects the type") would be false the moment it
// landed.
//
// Making the seed genuinely forge-aware therefore belongs with the gate flip
// (M6b), not here, and needs a UZI_SEED_FORGE_TYPE in config that does not exist
// yet. When that lands, this constant is what it replaces.
const seedForgeType = forge.TypeGitLab

// Store is the subset of *store.Queries the forge seed needs. Narrowing to an
// interface lets the seed logic be unit-tested against a fake store (and a
// mocked Forge) without a live database, mirroring forgesvc's IssueStore.
type Store interface {
	GetUserByEmail(ctx context.Context, email string) (store.User, error)
	ListForgeConnectionsByUser(ctx context.Context, userID uuid.UUID) ([]store.ForgeConnection, error)
	UpsertForgeConnection(ctx context.Context, arg store.UpsertForgeConnectionParams) (store.ForgeConnection, error)
	UpsertRepo(ctx context.Context, arg store.UpsertRepoParams) (store.Repo, error)
	SetRepoEnabledForUser(ctx context.Context, arg store.SetRepoEnabledForUserParams) (store.Repo, error)
}

// ForgeService is the subset of *forgesvc.Service the seed needs: it builds a
// forge client from a plaintext PAT and seals the PAT for storage — the same
// primitives the connect handler uses.
type ForgeService interface {
	EncryptToken(pat string) ([]byte, error)
	ForgeForToken(forgeType forge.Type, baseURL, token string) (forge.Forge, error)
}

// ForgeConnection seeds the forge connection and enabled repos for the seed
// admin when UZI_SEED_FORGE_PAT is configured. It is safe to call
// unconditionally (a no-op when disabled) and safe to leave enabled across
// restarts (create-only).
//
// Failure stance mirrors the design's deliberate split:
//   - DB errors are returned (boot-fatal), same as the admin seed.
//   - Runtime forge failures (client build, token verify, project listing) are
//     logged and swallowed so a forge outage never blocks boot; the seed
//     retries on the next start.
//
// Static misconfiguration (PAT without seed email, non-allowlisted base URL) is
// ForgeConnection provisions the configured GitLab connection and repositories for the seed administrator.
// It leaves an existing matching connection unchanged and logs runtime forge or encryption failures without
// returning them; database errors are returned.
func ForgeConnection(ctx context.Context, q Store, svc ForgeService, cfg config.Config) error {
	if cfg.SeedForgePAT == "" {
		return nil
	}

	user, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
	if err != nil {
		// The admin seed runs first and provisions this user; a lookup failure
		// here is a real DB/ordering fault, not an expected "absent" case.
		return fmt.Errorf("seed forge: look up seed admin %q: %w", cfg.SeedEmail, err)
	}

	// Never touch an existing connection: if the seed user already has one for
	// (seedForgeType, base_url), do nothing — no re-verify, no overwrite —
	// consistent with the never-touch-existing-user stance of the admin seed.
	conns, err := q.ListForgeConnectionsByUser(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("seed forge: list connections: %w", err)
	}
	for _, c := range conns {
		if c.ForgeType == string(seedForgeType) && c.BaseUrl == cfg.SeedForgeBaseURL {
			slog.Info("seed forge connection already present, leaving untouched",
				"email", cfg.SeedEmail, "base_url", cfg.SeedForgeBaseURL)
			return nil
		}
	}

	// Both forge calls run BEFORE any write, so a forge failure leaves no
	// half-seeded connection — which the guard above would otherwise skip
	// forever, stranding the repos. Once we persist below, the connection and
	// its repo enablement land together in this same pass.
	f, err := svc.ForgeForToken(seedForgeType, cfg.SeedForgeBaseURL, cfg.SeedForgePAT)
	if err != nil {
		slog.Error("seed forge: build forge client failed; skipping seed", "error", err)
		return nil
	}
	identity, err := f.VerifyToken(ctx)
	if err != nil {
		// err is already PAT-redacted by the driver.
		slog.Error("seed forge: token verification failed; skipping seed (retries next boot)", "error", err)
		return nil
	}
	projects, err := f.ListProjects(ctx)
	if err != nil {
		slog.Error("seed forge: listing projects failed; skipping seed (retries next boot)", "error", err)
		return nil
	}

	ciphertext, err := svc.EncryptToken(cfg.SeedForgePAT)
	if err != nil {
		// The master key was validated at boot, so this is unexpected; stay
		// non-fatal to keep boot alive, consistent with the forge-failure stance.
		slog.Error("seed forge: encrypt token failed; skipping seed", "error", err)
		return nil
	}

	conn, err := q.UpsertForgeConnection(ctx, store.UpsertForgeConnectionParams{
		UserID:          user.ID,
		ForgeType:       string(seedForgeType),
		BaseUrl:         cfg.SeedForgeBaseURL,
		BotUsername:     identity.Username,
		BotForgeUserID:  identity.ForgeUserID,
		TokenCiphertext: ciphertext,
	})
	if err != nil {
		return fmt.Errorf("seed forge: upsert connection: %w", err)
	}

	// Upsert every visible project as a repo (enabled=false), mirroring the
	// ListProjects handler, so each addressable repo has a row to enable.
	repoByPath := make(map[string]store.Repo, len(projects))
	for _, p := range projects {
		repo, err := q.UpsertRepo(ctx, store.UpsertRepoParams{
			ConnectionID:      conn.ID,
			ForgeProjectID:    p.ForgeProjectID,
			PathWithNamespace: p.PathWithNamespace,
			WebUrl:            p.WebURL,
			DefaultBranch:     pgconv.TextOrNull(p.DefaultBranch),
		})
		if err != nil {
			return fmt.Errorf("seed forge: upsert repo %q: %w", p.PathWithNamespace, err)
		}
		repoByPath[p.PathWithNamespace] = repo
	}

	// Enable exactly the requested repos; warn (don't fail) on any the bot
	// cannot see, so a typo or a missing membership is visible in the logs
	// rather than silently dropped.
	enabled := 0
	for _, path := range cfg.SeedForgeRepos {
		repo, ok := repoByPath[path]
		if !ok {
			slog.Warn("seed forge: requested repo not visible to the bot; skipping",
				"repo", path, "base_url", cfg.SeedForgeBaseURL)
			continue
		}
		if _, err := q.SetRepoEnabledForUser(ctx, store.SetRepoEnabledForUserParams{
			ID:      repo.ID,
			Enabled: true,
			UserID:  user.ID,
		}); err != nil {
			return fmt.Errorf("seed forge: enable repo %q: %w", path, err)
		}
		enabled++
	}

	slog.Info("seeded forge connection",
		"email", cfg.SeedEmail, "base_url", cfg.SeedForgeBaseURL,
		"bot_username", identity.Username, "projects", len(projects),
		"repos_enabled", enabled, "repos_requested", len(cfg.SeedForgeRepos))
	return nil
}
