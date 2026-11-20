package workersvc

const BlockingKindSchedulerCapacity = "scheduler_capacity"

// Controller-reported admission refusals (PRD #4213): the controller declined to render a size
// above its own ceilings, or the cluster rejected a worker's PVC. They are display-only and,
// unlike scheduler_capacity, never reclassify the upgrade status.
const (
	BlockingKindSizeRefused = "size_refused"
	BlockingKindPVCRejected = "pvc_rejected"
)

// NeverRegistered uses only API-owned worker history, never controller reports.
// Every absence is required: version absence alone does not exclude legacy registration.
func NeverRegistered(status string, heartbeat, version, nonce, podUID, rollHeartbeat bool) bool {
	return status == "offline" && !heartbeat && !version && !nonce && !podUID && !rollHeartbeat
}

// EffectiveBlockingKind revalidates all current diagnostics. An older API can
// refresh a row's diagnostics without clearing its newer blocking_kind column.
func EffectiveBlockingKind(s *RollSignal) string {
	if s == nil || s.BlockingKind != BlockingKindSchedulerCapacity ||
		s.Phase != PhaseStuck || s.PodPhase != "Pending" ||
		s.BlockingContainer != "" || s.RestartCount != 0 || s.LastExitCode != nil {
		return ""
	}
	switch s.BlockingReason {
	case "scheduler capacity: insufficient cpu",
		"scheduler capacity: insufficient memory",
		"scheduler capacity: insufficient cpu and memory":
		return BlockingKindSchedulerCapacity
	default:
		return ""
	}
}

// StoredBlockingKind is the kind to PERSIST for a report: scheduler_capacity under the same
// predicate as EffectiveBlockingKind, or size_refused / pvc_rejected when the phase is stuck and
// the report names exactly that kind. Readers use it too, to recognise a stored size_refused /
// pvc_rejected: the classifier's pod-less refusal exception (classifyWithTarget) and the
// handler and health readers. Capacity-blocked interpretation still goes only through
// EffectiveBlockingKind, which yields only scheduler_capacity, so a stored size_refused never
// reads as capacity_blocked and keeps the stuck / upgrade_failed classification.
func StoredBlockingKind(s *RollSignal) string {
	if kind := EffectiveBlockingKind(s); kind != "" {
		return kind
	}
	if s != nil && s.Phase == PhaseStuck &&
		(s.BlockingKind == BlockingKindSizeRefused || s.BlockingKind == BlockingKindPVCRejected) {
		return s.BlockingKind
	}
	return ""
}

func normalizedReportBlockingKind(rep RollHealthReport) string {
	sig := &RollSignal{
		BlockingKind: rep.BlockingKind, Phase: rep.Phase, PodPhase: rep.PodPhase,
		RestartCount: rep.RestartCount, LastExitCode: rep.LastExitCode,
	}
	if rep.BlockingContainer != nil {
		sig.BlockingContainer = *rep.BlockingContainer
	}
	if rep.BlockingReason != nil {
		sig.BlockingReason = *rep.BlockingReason
	}
	return StoredBlockingKind(sig)
}

// IsUpgradeFailure identifies actual failed statuses with blocking diagnostics.
func IsUpgradeFailure(status string) bool {
	return status == UpgradeStatusUpgradeFailed || status == UpgradeStatusProvisioningFailed ||
		status == UpgradeStatusCapacityBlocked
}
