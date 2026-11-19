package builtin

// Chart-rendered cluster limits (PRD #4213 Ceilings). The chart normalizes its quantity
// strings to base-10 integers (rounded down) and renders them into the api env; config
// parses them into these types and workersize validates against them. They live in the
// leaf so config can build them without importing workersize.

// Tier is a worker namespace tier. Sizes are global, so a size must fit every enabled tier.
type Tier int

const (
	TierRestricted Tier = iota
	TierDocker
	TierIsolated
	// TierCount is the number of tiers; it sizes the per-tier arrays.
	TierCount
)

// Name is the tier's wire/env name.
func (t Tier) Name() string {
	switch t {
	case TierRestricted:
		return "restricted"
	case TierDocker:
		return "docker"
	case TierIsolated:
		return "isolated"
	default:
		return "unknown"
	}
}

// ParseTier maps an env/wire tier name to a Tier.
func ParseTier(name string) (Tier, bool) {
	for t := Tier(0); t < TierCount; t++ {
		if t.Name() == name {
			return t, true
		}
	}
	return 0, false
}

// Value is an optional integer: N is meaningful only when Set. A ceiling that is not Set
// is "no ceiling of that kind"; a quota that is not Set is "unavailable" (never unlimited).
type Value struct {
	N   int64
	Set bool
}

// Present builds a set Value.
func Present(n int64) Value { return Value{N: n, Set: true} }

// TierCeiling is one tier's ceilings. Every set value is strictly positive.
type TierCeiling struct {
	CPUMillicores Value // LimitRange max.cpu
	MemoryMiB     Value // LimitRange max.memory
	PVCGiB        Value // LimitRange maxPVCStorage (unused by the isolated tier)
}

// Ceilings are the cluster ceilings a size must fit.
type Ceilings struct {
	// TiersKnown is false when UZI_WORKER_SIZE_TIERS was not set (compose, an older chart):
	// every ceiling that is present then applies.
	TiersKnown bool
	Enabled    [TierCount]bool
	Tier       [TierCount]TierCeiling
	// DindOverridden is true when workers.docker.dindDataSize overrides the per-size DinD
	// claim, so dind_data_gib is not validated against the docker PVC ceiling.
	DindOverridden bool
}

// TierEnabled reports whether a tier counts toward the ceilings.
func (c Ceilings) TierEnabled(t Tier) bool {
	if !c.TiersKnown {
		return true
	}
	return c.Enabled[t]
}

// TierQuota is one tier's ResourceQuota values. Each value may be zero (a legitimate
// "admit nothing" quota) and may be absent.
type TierQuota struct {
	RequestsCPUMillicores Value // quota.requestsCPU
	RequestsMemoryMiB     Value // quota.requestsMemory
	Deployments           Value // quota.deployments
}

// Complete reports whether all three values are present.
func (q TierQuota) Complete() bool {
	return q.RequestsCPUMillicores.Set && q.RequestsMemoryMiB.Set && q.Deployments.Set
}

// Quota is the per-tier ResourceQuota the advisory compares against.
type Quota struct {
	Tier [TierCount]TierQuota
}
