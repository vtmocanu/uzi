package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func loadWorkerMemoryGuard(cfg *Config) error {
	flag := strings.ToLower(strings.TrimSpace(os.Getenv("UZI_WORKER_MEMORY_GUARD_ENABLED")))
	switch flag {
	case "", "0", "false", "no", "off":
		return nil
	case "1", "true", "yes", "on":
		cfg.WorkerMemoryGuardEnabled = true
	default:
		return fmt.Errorf("UZI_WORKER_MEMORY_GUARD_ENABLED must be a boolean")
	}
	// Six independent settings, one parse per setting; the first invalid value
	// stops boot. Limit-relative ordering belongs to the worker cgroup reader.
	for _, setting := range []struct {
		name    string
		dst     *int64
		ceiling int64
	}{
		{"UZI_WORKER_MEMORY_GUARD_RESERVE_BYTES", &cfg.WorkerMemoryGuardReserveBytes, 9007199254740991},
		{"UZI_WORKER_MEMORY_GUARD_SAMPLE_MS", &cfg.WorkerMemoryGuardSampleMs, 2147483647},
		{"UZI_WORKER_MEMORY_GUARD_HYSTERESIS_BYTES", &cfg.WorkerMemoryGuardHysteresisBytes, 9007199254740991},
		{"UZI_WORKER_MEMORY_GUARD_REARM_MS", &cfg.WorkerMemoryGuardRearmMs, 2147483647},
		{"UZI_WORKER_MEMORY_GUARD_RESPONSE_BUDGET_MS", &cfg.WorkerMemoryGuardResponseBudgetMs, 2147483647},
		{"UZI_WORKER_MEMORY_GUARD_MAX_INTERVENTIONS", &cfg.WorkerMemoryGuardMaxInterventions, 9999},
	} {
		raw := strings.TrimSpace(os.Getenv(setting.name))
		decimal := raw != ""
		for _, c := range raw {
			if c < '0' || c > '9' {
				decimal = false
				break
			}
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if !decimal || err != nil || n <= 0 || n > setting.ceiling {
			return fmt.Errorf("%s must be an explicit positive integer <= %d", setting.name, setting.ceiling)
		}
		*setting.dst = n
	}
	return nil
}
