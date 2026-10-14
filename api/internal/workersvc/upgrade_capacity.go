package workersvc

const BlockingKindSchedulerCapacity = "scheduler_capacity"

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
	return EffectiveBlockingKind(sig)
}

// IsUpgradeFailure identifies actual failed statuses with blocking diagnostics.
func IsUpgradeFailure(status string) bool {
	return status == UpgradeStatusUpgradeFailed || status == UpgradeStatusProvisioningFailed ||
		status == UpgradeStatusCapacityBlocked
}
