package workersize

import (
	"fmt"
	"math/bits"

	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// Validation of a hosted-worker size (PRD #4213). Pure integer arithmetic: the api never
// handles quantity strings and holds no kube dependency. Inputs are the six integers, the
// previous values of the row (nil for a new size), and the chart-rendered ceilings and quota.

// Wire names of the six integer fields, shared by the request body, the field errors and
// the advisories.
const (
	FieldCPURequest = "cpu_request_millicores"
	FieldCPULimit   = "cpu_limit_millicores"
	FieldMemRequest = "memory_request_mib"
	FieldMemLimit   = "memory_limit_mib"
	FieldData       = "data_gib"
	FieldDindData   = "dind_data_gib"
)

// Advisory and warning kinds.
const (
	KindDiskLowered      = "disk_lowered"
	KindQuotaUnavailable = "quota_unavailable"
	KindQuotaZero        = "quota_zero"
	KindQuotaExceeded    = "quota_exceeded"
)

// Spec is a size's six resolved integers: CPU in millicores, memory in MiB, disks in GiB.
type Spec struct {
	CPURequestMillicores int64 `json:"cpu_request_millicores"`
	CPULimitMillicores   int64 `json:"cpu_limit_millicores"`
	MemoryRequestMiB     int64 `json:"memory_request_mib"`
	MemoryLimitMiB       int64 `json:"memory_limit_mib"`
	DataGiB              int64 `json:"data_gib"`
	DinDDataGiB          int64 `json:"dind_data_gib"`
}

// Warning is a non-blocking note the editor shows before or after a save.
type Warning struct {
	Kind    string `json:"kind"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Advisory is a quota note for one enabled tier. It never refuses a save. Tier is empty
// when the enabled tiers are unknown.
type Advisory struct {
	Size    string `json:"size"`
	Tier    string `json:"tier"`
	Kind    string `json:"kind"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Result is the outcome of Validate. A non-empty Fields refuses the save.
type Result struct {
	Fields     map[string]string
	Warnings   []Warning
	Advisories []Advisory
}

// OK reports whether the size may be saved.
func (r Result) OK() bool { return len(r.Fields) == 0 }

// Chart value paths named in messages (deploy/chart/values.yaml).
var (
	cpuCeilingPath = [builtin.TierCount]string{
		"workers.limitRange.max.cpu", "workers.docker.limitRange.max.cpu", "workers.isolatedLane.limitRange.max.cpu"}
	memCeilingPath = [builtin.TierCount]string{
		"workers.limitRange.max.memory", "workers.docker.limitRange.max.memory", "workers.isolatedLane.limitRange.max.memory"}
	pvcCeilingPath = [builtin.TierCount]string{
		"workers.limitRange.maxPVCStorage", "workers.docker.limitRange.maxPVCStorage", ""}
	quotaPathPrefix = [builtin.TierCount]string{
		"workers.quota", "workers.docker.quota", "workers.isolatedLane.quota"}
)

// Ceiling is a binding ceiling: the smallest applicable value, and where the operator
// changes it.
type Ceiling struct {
	Value     int64
	ChartPath string
}

// CeilingKind selects which ceiling Binding resolves.
type CeilingKind int

const (
	CeilingCPULimit CeilingKind = iota
	CeilingMemoryLimit
	CeilingData     // min of the persistent tiers' PVC ceilings
	CeilingDindData // docker tier PVC ceiling; absent when overridden
)

// Binding returns the binding ceiling of a kind, or false when none applies. A tier counts
// only when enabled (every tier counts when UZI_WORKER_SIZE_TIERS was not set).
func Binding(c builtin.Ceilings, kind CeilingKind) (Ceiling, bool) {
	var (
		best Ceiling
		have bool
	)
	consider := func(t builtin.Tier, v builtin.Value, path string) {
		if !c.TierEnabled(t) || !v.Set || path == "" {
			return
		}
		if !have || v.N < best.Value {
			best, have = Ceiling{Value: v.N, ChartPath: path}, true
		}
	}
	switch kind {
	case CeilingCPULimit:
		for t := builtin.Tier(0); t < builtin.TierCount; t++ {
			consider(t, c.Tier[t].CPUMillicores, cpuCeilingPath[t])
		}
	case CeilingMemoryLimit:
		for t := builtin.Tier(0); t < builtin.TierCount; t++ {
			consider(t, c.Tier[t].MemoryMiB, memCeilingPath[t])
		}
	case CeilingData:
		consider(builtin.TierRestricted, c.Tier[builtin.TierRestricted].PVCGiB, pvcCeilingPath[builtin.TierRestricted])
		consider(builtin.TierDocker, c.Tier[builtin.TierDocker].PVCGiB, pvcCeilingPath[builtin.TierDocker])
	case CeilingDindData:
		if !c.DindOverridden {
			consider(builtin.TierDocker, c.Tier[builtin.TierDocker].PVCGiB, pvcCeilingPath[builtin.TierDocker])
		}
	}
	return best, have
}

// Validate checks a size against the absolute caps, request <= limit, the chart ceilings,
// and returns disk-lowering warnings and quota advisories. prev is the row's current
// values, or nil for a size that does not exist yet.
func Validate(name string, s Spec, prev *Spec, c builtin.Ceilings, q builtin.Quota) Result {
	r := Result{Fields: map[string]string{}}

	positive := func(field string, v, limit int64, unit string) bool {
		switch {
		case v <= 0:
			r.Fields[field] = fmt.Sprintf("must be greater than 0 %s", unit)
		case v > limit:
			r.Fields[field] = fmt.Sprintf("%d %s exceeds the absolute maximum of %d %s", v, unit, limit, unit)
		default:
			return true
		}
		return false
	}
	cpuReqOK := positive(FieldCPURequest, s.CPURequestMillicores, builtin.MaxMillicores, "millicores")
	cpuLimOK := positive(FieldCPULimit, s.CPULimitMillicores, builtin.MaxMillicores, "millicores")
	memReqOK := positive(FieldMemRequest, s.MemoryRequestMiB, builtin.MaxMemoryMiB, "MiB")
	memLimOK := positive(FieldMemLimit, s.MemoryLimitMiB, builtin.MaxMemoryMiB, "MiB")
	dataOK := positive(FieldData, s.DataGiB, builtin.MaxDiskGiB, "GiB")
	dindOK := positive(FieldDindData, s.DinDDataGiB, builtin.MaxDiskGiB, "GiB")

	if cpuReqOK && cpuLimOK && s.CPURequestMillicores > s.CPULimitMillicores {
		r.Fields[FieldCPURequest] = fmt.Sprintf("cpu request %d millicores exceeds the cpu limit of %d millicores",
			s.CPURequestMillicores, s.CPULimitMillicores)
	}
	if memReqOK && memLimOK && s.MemoryRequestMiB > s.MemoryLimitMiB {
		r.Fields[FieldMemRequest] = fmt.Sprintf("memory request %d MiB exceeds the memory limit of %d MiB",
			s.MemoryRequestMiB, s.MemoryLimitMiB)
	}

	ceiling := func(field string, ok bool, v int64, kind CeilingKind, what, unit string) {
		if !ok {
			return
		}
		b, have := Binding(c, kind)
		if have && v > b.Value {
			r.Fields[field] = fmt.Sprintf("%s %d %s exceeds the %d %s ceiling (%s)", what, v, unit, b.Value, unit, b.ChartPath)
		}
	}
	ceiling(FieldCPULimit, cpuLimOK, s.CPULimitMillicores, CeilingCPULimit, "cpu limit", "millicores")
	ceiling(FieldMemLimit, memLimOK, s.MemoryLimitMiB, CeilingMemoryLimit, "memory limit", "MiB")
	ceiling(FieldData, dataOK, s.DataGiB, CeilingData, "data disk", "GiB")
	ceiling(FieldDindData, dindOK, s.DinDDataGiB, CeilingDindData, "dind data disk", "GiB")

	if prev != nil {
		lowered := func(field string, now, was int64, what string) {
			if now < was {
				r.Warnings = append(r.Warnings, Warning{
					Kind: KindDiskLowered, Field: field,
					Message: fmt.Sprintf("%s lowered from %d GiB to %d GiB: existing claims keep their size; only new claims use the lower value", what, was, now),
				})
			}
		}
		lowered(FieldData, s.DataGiB, prev.DataGiB, "data disk")
		lowered(FieldDindData, s.DinDDataGiB, prev.DinDDataGiB, "dind data disk")
	}

	r.Advisories = quotaAdvisories(name, s, cpuReqOK, memReqOK, c, q)
	return r
}

func quotaAdvisories(name string, s Spec, cpuReqOK, memReqOK bool, c builtin.Ceilings, q builtin.Quota) []Advisory {
	var out []Advisory
	if !c.TiersKnown {
		// Without the enabled-tier list there is nothing to compare: say so, never treat the
		// missing data as unlimited or as fitting.
		return append(out, Advisory{
			Size: name, Kind: KindQuotaUnavailable,
			Message: "quota advisory unavailable: the enabled worker tiers are not known to the api",
		})
	}
	for t := builtin.Tier(0); t < builtin.TierCount; t++ {
		if !c.Enabled[t] {
			continue
		}
		tq := q.Tier[t]
		tier := t.Name()
		if !tq.Complete() {
			out = append(out, Advisory{
				Size: name, Tier: tier, Kind: KindQuotaUnavailable,
				Message: fmt.Sprintf("quota advisory unavailable for the %s tier: its quota values (%s.*) are not available to the api", tier, quotaPathPrefix[t]),
			})
			continue
		}
		if tq.RequestsCPUMillicores.N == 0 || tq.RequestsMemoryMiB.N == 0 || tq.Deployments.N == 0 {
			out = append(out, Advisory{
				Size: name, Tier: tier, Kind: KindQuotaZero,
				Message: fmt.Sprintf("a quota value for the %s tier is 0 (%s.*): no worker can be admitted on this tier", tier, quotaPathPrefix[t]),
			})
		}
		if cpuReqOK && tq.RequestsCPUMillicores.N > 0 && quotaExceeded(s.CPURequestMillicores, tq.Deployments.N, tq.RequestsCPUMillicores.N) {
			out = append(out, Advisory{
				Size: name, Tier: tier, Kind: KindQuotaExceeded, Field: FieldCPURequest,
				Message: fmt.Sprintf("cpu request %d millicores x %d deployments exceeds the %s tier quota of %d millicores (%s.requestsCPU)",
					s.CPURequestMillicores, tq.Deployments.N, tier, tq.RequestsCPUMillicores.N, quotaPathPrefix[t]),
			})
		}
		if memReqOK && tq.RequestsMemoryMiB.N > 0 && quotaExceeded(s.MemoryRequestMiB, tq.Deployments.N, tq.RequestsMemoryMiB.N) {
			out = append(out, Advisory{
				Size: name, Tier: tier, Kind: KindQuotaExceeded, Field: FieldMemRequest,
				Message: fmt.Sprintf("memory request %d MiB x %d deployments exceeds the %s tier quota of %d MiB (%s.requestsMemory)",
					s.MemoryRequestMiB, tq.Deployments.N, tier, tq.RequestsMemoryMiB.N, quotaPathPrefix[t]),
			})
		}
	}
	return out
}

// quotaExceeded reports whether request x deployments, or a single request, is above the
// quota. Both factors and the quota must be non-negative; the 128-bit product cannot wrap.
func quotaExceeded(request, deployments, quota int64) bool {
	if request < 0 || deployments < 0 || quota < 0 {
		return false
	}
	if request > quota {
		return true
	}
	hi, lo := bits.Mul64(uint64(request), uint64(deployments))
	return hi != 0 || lo > uint64(quota)
}
