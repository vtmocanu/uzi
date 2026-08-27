package seed

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SettingsStore is the subset of *store.Queries the Slack-settings seed needs.
// Narrowing to an interface lets the seed be unit-tested against a fake store
// without a live database, mirroring the forge and Anthropic seeds' stores.
type SettingsStore interface {
	ListAppSettings(ctx context.Context) ([]store.AppSetting, error)
	UpsertAppSetting(ctx context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error)
}

// SlackSettings seeds the Slack app_settings rows from UZI_SEED_SLACK_* when
// configured: the two tokens (sealed at rest via the same write-side seam the
// settings PUT uses), slack_enabled="true" alongside them, and optionally
// public_base_url. Like the other seeds it is safe to call unconditionally (a
// no-op when disabled) and CREATE-ONLY — per key: a row that already exists
// (UI-set or previously seeded) is left untouched, so an admin rotation
// survives restarts and only a fresh `down -v` re-seeds from the env. The
// updated_by column is left NULL: no user performed the write.
//
// Unlike the settings PUT there is no live token validation here — a network
// call must not gate boot. A bad seeded token surfaces exactly like a bad
// UI-saved one: the Socket Mode manager fails to connect and the admin status
// chip shows it.
//
// Failure stance mirrors the Anthropic seed: every failure is a DB fault or a
// seal fault (the key was validated at boot), both boot-fatal; there is no
// outage condition to defer. Token values are never logged (key names only).
func SlackSettings(ctx context.Context, q SettingsStore, box *secretbox.Box, cfg config.Config) error {
	if cfg.SeedSlackBotToken == "" && cfg.SeedPublicBaseURL == "" {
		return nil
	}

	rows, err := q.ListAppSettings(ctx)
	if err != nil {
		return fmt.Errorf("seed slack settings: list settings: %w", err)
	}
	existing := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		existing[r.Key] = struct{}{}
	}

	// Desired plaintext per key; secret keys are sealed at write time below.
	// config.Load guarantees the tokens come as a pair (loadSeedSlack).
	desired := map[string]string{}
	if cfg.SeedSlackBotToken != "" {
		desired[settings.KeySlackBotToken] = cfg.SeedSlackBotToken
		desired[settings.KeySlackAppToken] = cfg.SeedSlackAppToken
		// Seeded tokens are only useful live: enable in the same pass. Create-only
		// like every other key, so an admin's later "off" flip is never re-enabled.
		desired[settings.KeySlackEnabled] = "true"
	}
	if cfg.SeedPublicBaseURL != "" {
		desired[settings.KeyPublicBaseURL] = cfg.SeedPublicBaseURL
	}

	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic write/log order

	var seeded, skipped []string
	for _, key := range keys {
		if _, ok := existing[key]; ok {
			skipped = append(skipped, key)
			continue
		}
		toStore, err := settings.ValueForStorage(box, key, desired[key])
		if err != nil {
			return fmt.Errorf("seed slack settings: seal %s: %w", key, err)
		}
		if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
			Key:   key,
			Value: toStore,
		}); err != nil {
			// The key name is safe to report; the value never is.
			return fmt.Errorf("seed slack settings: store %s: %w", key, err)
		}
		seeded = append(seeded, key)
	}

	slog.Info("seeded slack settings", "seeded", seeded, "existing_untouched", skipped)
	return nil
}
