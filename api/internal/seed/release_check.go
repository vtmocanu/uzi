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

// ReleaseCheckSettings seeds the upstream-release-check app_settings rows from the
// SeedReleaseCheck* config values at boot (PRD #836 M2), mirroring SlackSettings: it is
// safe to call unconditionally and CREATE-ONLY per key — a row that already exists
// (UI-set or previously seeded) is left untouched, so an admin's later "off" flip is
// never re-enabled on reboot (D8). The optional token is sealed at rest via the same
// write-side seam the settings PUT uses (ValueForStorage); the two toggles and the
// interval are ordinary plaintext KV values. The updated_by column is left NULL: no
// user performed the write.
//
// The env var is only the INITIAL default; the admin runtime setting is authoritative
// thereafter. Failure stance mirrors SlackSettings: a DB list/upsert fault or a seal
// fault is boot-fatal. Token values are never logged (key names only).
func ReleaseCheckSettings(ctx context.Context, q SettingsStore, box *secretbox.Box, cfg config.Config) error {
	rows, err := q.ListAppSettings(ctx)
	if err != nil {
		return fmt.Errorf("seed release-check settings: list settings: %w", err)
	}
	existing := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		existing[r.Key] = struct{}{}
	}

	// Desired plaintext per key; the token (a secret key) is sealed at write time below.
	desired := map[string]string{
		settings.KeyReleaseCheckEnabled:       boolStr(cfg.SeedReleaseCheckEnabled),
		settings.KeyReleaseCheckBannerEnabled: boolStr(cfg.SeedReleaseCheckBannerEnabled),
		settings.KeyReleaseCheckInterval:      cfg.SeedReleaseCheckInterval.String(),
	}
	// The token is OPTIONAL: seed it only when configured, so an empty env var never
	// creates an empty sealed row that would mask a later admin-set token.
	if cfg.SeedReleaseCheckToken != "" {
		desired[settings.KeyReleaseCheckToken] = cfg.SeedReleaseCheckToken
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
			return fmt.Errorf("seed release-check settings: seal %s: %w", key, err)
		}
		if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
			Key:   key,
			Value: toStore,
		}); err != nil {
			// The key name is safe to report; the value never is.
			return fmt.Errorf("seed release-check settings: store %s: %w", key, err)
		}
		seeded = append(seeded, key)
	}

	slog.Info("seeded release-check settings", "seeded", seeded, "existing_untouched", skipped)
	return nil
}

// boolStr renders a config bool as the "true"/"false" string app_settings stores.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
