package config

import (
	"fmt"
	"os"
	"strconv"
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
	return loadWorkerGoMemLimit(cfg)
}

// loadWorkerGoMemLimit reads the optional UZI_WORKER_GO_MEMLIMIT. Only ASCII
// space, tab, CR and LF are trimmed. Empty means unset; otherwise the value must
// be a positive decimal <= goConcurrencyCeiling followed by MiB or GiB, and is
// stored canonically (leading zeros stripped).
func loadWorkerGoMemLimit(cfg *Config) error {
	const name = "UZI_WORKER_GO_MEMLIMIT"
	raw := strings.Trim(os.Getenv(name), " \t\r\n")
	if raw == "" {
		return nil
	}
	for _, unit := range []string{"MiB", "GiB"} {
		digits, ok := strings.CutSuffix(raw, unit)
		if !ok {
			continue
		}
		if n, ok := parsePositiveDecimal(digits, goConcurrencyCeiling); ok {
			cfg.WorkerGoMemLimit = strconv.FormatInt(n, 10) + unit
			return nil
		}
		break
	}
	return fmt.Errorf("%s must be a positive integer <= %d followed by MiB or GiB", name, goConcurrencyCeiling)
}
