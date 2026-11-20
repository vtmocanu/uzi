package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// Worker-size ceilings and quota (PRD #4213). The chart renders them from the same values
// that feed the namespace LimitRanges and ResourceQuotas, normalized to base-10 integers
// (CPU in millicores, memory in MiB, storage in GiB). Ceilings are strict: a malformed or
// non-positive value refuses boot, because a mis-parsed ceiling would silently loosen the
// refusal. Quota feeds an advisory only, so a malformed value degrades to "unavailable"
// (never to unlimited) with a warning, and zero is a legitimate value.

// ceilingEnvKeys maps each tier to its ceiling env keys (cpu, memory, pvc); an empty key
// means the tier has no such ceiling.
var ceilingEnvKeys = [builtin.TierCount][3]string{
	builtin.TierRestricted: {"UZI_WORKER_SIZE_CEILING_RESTRICTED_CPU_MILLICORES", "UZI_WORKER_SIZE_CEILING_RESTRICTED_MEMORY_MIB", "UZI_WORKER_SIZE_CEILING_RESTRICTED_PVC_GIB"},
	builtin.TierDocker:     {"UZI_WORKER_SIZE_CEILING_DOCKER_CPU_MILLICORES", "UZI_WORKER_SIZE_CEILING_DOCKER_MEMORY_MIB", "UZI_WORKER_SIZE_CEILING_DOCKER_PVC_GIB"},
	builtin.TierIsolated:   {"UZI_WORKER_SIZE_CEILING_ISOLATED_CPU_MILLICORES", "UZI_WORKER_SIZE_CEILING_ISOLATED_MEMORY_MIB", ""},
}

var quotaEnvKeys = [builtin.TierCount][3]string{
	builtin.TierRestricted: {"UZI_WORKER_SIZE_QUOTA_RESTRICTED_REQUESTS_CPU_MILLICORES", "UZI_WORKER_SIZE_QUOTA_RESTRICTED_REQUESTS_MEMORY_MIB", "UZI_WORKER_SIZE_QUOTA_RESTRICTED_DEPLOYMENTS"},
	builtin.TierDocker:     {"UZI_WORKER_SIZE_QUOTA_DOCKER_REQUESTS_CPU_MILLICORES", "UZI_WORKER_SIZE_QUOTA_DOCKER_REQUESTS_MEMORY_MIB", "UZI_WORKER_SIZE_QUOTA_DOCKER_DEPLOYMENTS"},
	builtin.TierIsolated:   {"UZI_WORKER_SIZE_QUOTA_ISOLATED_REQUESTS_CPU_MILLICORES", "UZI_WORKER_SIZE_QUOTA_ISOLATED_REQUESTS_MEMORY_MIB", "UZI_WORKER_SIZE_QUOTA_ISOLATED_DEPLOYMENTS"},
}

// loadWorkerSizeLimits fills cfg.WorkerSizeCeilings and cfg.WorkerSizeQuota.
func loadWorkerSizeLimits(cfg *Config) error {
	var c builtin.Ceilings
	if raw := strings.TrimSpace(os.Getenv("UZI_WORKER_SIZE_TIERS")); raw != "" {
		c.TiersKnown = true
		for _, part := range strings.Split(raw, ",") {
			name := strings.TrimSpace(part)
			t, ok := builtin.ParseTier(name)
			if !ok {
				return fmt.Errorf("UZI_WORKER_SIZE_TIERS must be a comma list of restricted, docker, isolated; got %q", raw)
			}
			c.Enabled[t] = true
		}
	}
	for t := builtin.Tier(0); t < builtin.TierCount; t++ {
		keys := ceilingEnvKeys[t]
		dst := [3]*builtin.Value{&c.Tier[t].CPUMillicores, &c.Tier[t].MemoryMiB, &c.Tier[t].PVCGiB}
		for i, key := range keys {
			if key == "" {
				continue
			}
			v, err := parsePositiveInt64(key)
			if err != nil {
				return err
			}
			*dst[i] = v
		}
	}
	dind, err := parseOnOff("UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN", false)
	if err != nil {
		return err
	}
	c.DindOverridden = dind
	cfg.WorkerSizeCeilings = c

	var q builtin.Quota
	for t := builtin.Tier(0); t < builtin.TierCount; t++ {
		keys := quotaEnvKeys[t]
		q.Tier[t] = builtin.TierQuota{
			RequestsCPUMillicores: parseQuotaInt64(keys[0]),
			RequestsMemoryMiB:     parseQuotaInt64(keys[1]),
			Deployments:           parseQuotaInt64(keys[2]),
		}
	}
	cfg.WorkerSizeQuota = q
	return nil
}

// parsePositiveInt64 parses a strictly-positive base-10 int64 env var; unset or empty is
// absent. A set-but-malformed or non-positive value is an error (boot refuses).
func parsePositiveInt64(key string) (builtin.Value, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return builtin.Value{}, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return builtin.Value{}, fmt.Errorf("%s must be a positive base-10 integer, got %q", key, raw)
	}
	return builtin.Present(n), nil
}

// parseQuotaInt64 parses a non-negative base-10 int64 quota env var. Unset/empty is absent;
// zero is accepted and kept as zero. A malformed or negative value is also absent (the
// advisory then reads "unavailable"), never a boot failure and never unlimited.
func parseQuotaInt64(key string) builtin.Value {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return builtin.Value{}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		slog.Warn("worker size quota value is not a non-negative integer; the quota advisory is unavailable for it",
			"key", key, "value", raw)
		return builtin.Value{}
	}
	return builtin.Present(n)
}
