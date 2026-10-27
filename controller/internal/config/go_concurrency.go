package config

import (
	"fmt"
	"os"
	"strings"
)

const goConcurrencyCeiling = 2147483647

// loadWorkerGoConcurrency reads the optional Go build/runtime caps. Empty means
// unset (0); anything else must be a positive decimal <= goConcurrencyCeiling.
// The agent and the chart apply the same rule.
func loadWorkerGoConcurrency(cfg *Config) error {
	for _, setting := range []struct {
		name string
		dst  *int
	}{
		{"UZI_WORKER_GO_BUILD_PARALLELISM", &cfg.WorkerGoBuildParallelism},
		{"UZI_WORKER_GO_MAXPROCS", &cfg.WorkerGoMaxProcs},
	} {
		raw := strings.TrimSpace(os.Getenv(setting.name))
		if raw == "" {
			continue
		}
		n, ok := parsePositiveDecimal(raw, goConcurrencyCeiling)
		if !ok {
			return fmt.Errorf("%s must be a positive integer <= %d", setting.name, goConcurrencyCeiling)
		}
		*setting.dst = int(n)
	}
	return nil
}
