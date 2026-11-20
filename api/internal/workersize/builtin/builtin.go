// Package builtin holds the shipped hosted-worker size presets (PRD #4213) and the
// name rules every size must satisfy. It is a leaf: it imports nothing from this module,
// so the store (which seeds the table), config (which validates the env default) and
// settings can all depend on it without a cycle.
//
// The values here are the builtin seed contract: hostedsvc/testdata/hosted_sizes.json is
// pinned against Sizes (hostedsvc.TestBuiltinSizesMatchSeedContract), and the controller's
// compiled-in preset table is pinned against the same file on its side.
package builtin

import "regexp"

// Absolute per-field sanity caps (PRD #4213). They mirror the worker_sizes CHECK
// constraints and always apply, whatever chart ceilings are configured.
const (
	MaxMillicores int64 = 64000
	MaxMemoryMiB  int64 = 524288
	MaxDiskGiB    int64 = 2048
)

// NamePattern is the size-name vocabulary: lowercase, starting with a letter, at most
// 16 characters. Names go on the wire verbatim and are stored in workers.hosted_size, so
// this is also what keeps free text off a pod spec.
var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// Size is one shipped size's name and resolved integers: CPU in millicores, memory in
// MiB, disks in GiB. DisplayOrder is smallest first.
type Size struct {
	Name                 string
	CPURequestMillicores int64
	CPULimitMillicores   int64
	MemoryRequestMiB     int64
	MemoryLimitMiB       int64
	DataGiB              int64
	DinDDataGiB          int64
	DisplayOrder         int32
}

// Sizes returns a fresh copy of the shipped presets in display order.
func Sizes() []Size {
	return []Size{
		{Name: "s", CPURequestMillicores: 250, CPULimitMillicores: 1000, MemoryRequestMiB: 2048, MemoryLimitMiB: 4096, DataGiB: 5, DinDDataGiB: 20, DisplayOrder: 0},
		{Name: "m", CPURequestMillicores: 1000, CPULimitMillicores: 4000, MemoryRequestMiB: 8192, MemoryLimitMiB: 12288, DataGiB: 25, DinDDataGiB: 20, DisplayOrder: 1},
		{Name: "l", CPURequestMillicores: 1000, CPULimitMillicores: 4000, MemoryRequestMiB: 14336, MemoryLimitMiB: 20480, DataGiB: 50, DinDDataGiB: 50, DisplayOrder: 2},
		{Name: "l-highmem", CPURequestMillicores: 1000, CPULimitMillicores: 4000, MemoryRequestMiB: 24576, MemoryLimitMiB: 32768, DataGiB: 50, DinDDataGiB: 50, DisplayOrder: 3},
	}
}

// Names returns the shipped size names in display order, smallest first.
func Names() []string {
	sizes := Sizes()
	names := make([]string, len(sizes))
	for i, s := range sizes {
		names[i] = s.Name
	}
	return names
}

// ValidBuiltin reports whether name is one of the shipped presets. It is the only check
// that can run before the database exists (the UZI_EPHEMERAL_DEFAULT_SIZE env value is
// validated at config load), so the env stays builtins-only.
func ValidBuiltin(name string) bool {
	for _, s := range Sizes() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// WellFormed reports whether name satisfies NamePattern. A format check only: it says
// nothing about whether such a size exists.
func WellFormed(name string) bool {
	return NamePattern.MatchString(name)
}
