package main

import (
	"log/slog"
	"sort"

	"github.com/vtmocanu/uzi/controller/internal/preset"
)

// warnSharedCPU describes available presets once at boot, without observing fleet
// occupancy. Work is bounded by SizeNames; a resolution failure skips only that size.
func warnSharedCPU(log *slog.Logger, slots int, resolver preset.Resolver) {
	if slots <= 1 {
		return
	}
	sizes := preset.SizeNames()
	sort.Strings(sizes)
	for _, size := range sizes {
		spec, err := resolver.Resolve("base", size)
		if err != nil {
			log.Warn("hosted-worker CPU configuration resolution failed",
				"size", size, "configured_run_slots", slots, "error", err)
			continue
		}
		log.Warn("hosted-worker-shared-cpu",
			"size", size,
			"configured_run_slots", slots,
			"cpu_limit", spec.Size.CPULimit.String(),
			"advisory", "Available hosted-worker configuration: concurrent runs share a fixed CPU quota; measure workload performance or lower concurrency. This describes available configurations, not actual fleet or active occupancy.")
	}
}
