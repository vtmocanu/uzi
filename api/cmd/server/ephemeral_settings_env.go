package main

import (
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// ephemeralSettingsEnv builds the ENV-overlay entries for the ephemeral worker
// settings (PRD #4212). A key is added only when its raw environment variable is
// set to a non-blank value: cfg.Ephemeral* always holds a value (defaults
// included), so building from cfg alone would lock every field in the admin UI.
// The value stored is the canonical one config.Load produced, so an env-locked
// field shows exactly what the api runs with and the typed accessors never see a
// raw unparsed string.
func ephemeralSettingsEnv(lookup func(string) (string, bool), cfg *config.Config) map[string]string {
	out := map[string]string{}
	set := func(envName, key, canonical string) {
		if raw, ok := lookup(envName); ok && strings.TrimSpace(raw) != "" {
			out[key] = canonical
		}
	}
	set("UZI_EPHEMERAL_MAX_PER_USER", settings.KeyEphemeralMaxPerUser, strconv.Itoa(cfg.EphemeralMaxPerUser))
	set("UZI_EPHEMERAL_DEFAULT_SIZE", settings.KeyEphemeralDefaultSize, cfg.EphemeralDefaultSize)
	set("UZI_EPHEMERAL_LEASE", settings.KeyEphemeralLease, settings.FormatEphemeralLease(cfg.EphemeralLease))
	return out
}
