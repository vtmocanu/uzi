package main

import (
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const workerMemoryPressureCause = "worker_memory_pressure"

func isMemoryHold(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && strOr(r.RecoveryWaitCause, "") == workerMemoryPressureCause
}

func isOwnerRequiredWorkerHold(r apitypes.RunDTO) bool {
	return isMemoryHold(r) || isWorkerRecoveryExhausted(r)
}

func memoryHoldAllowance(r apitypes.RunDTO) string {
	h := r.MemoryHold
	if h == nil {
		return "memory intervention allowance unknown"
	}
	max, remaining := "unknown", "unknown"
	if h.Max != nil {
		max = fmt.Sprint(*h.Max)
	}
	if h.Remaining != nil {
		remaining = fmt.Sprint(*h.Remaining)
	}
	return fmt.Sprintf("%d of %s memory interventions used · %s remaining · episode %d", h.Used, max, remaining, h.Episode)
}

func memoryHoldLine(r apitypes.RunDTO) string {
	id := cellText(r.ID)
	return "worker memory pressure · " + memoryHoldAllowance(r) +
		" · owner Resume: uzi run resume " + id + "; Cancel: uzi run cancel " + id +
		" · only owner Resume starts a fresh memory episode; Cancel is terminal; no automatic resume"
}

func memoryHoldTUILines(r apitypes.RunDTO) []string {
	return []string{
		"worker memory pressure · owner Resume or Cancel",
		memoryHoldAllowance(r),
		"Resume: uzi run resume " + cellText(r.ID),
		"Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)",
		"Only owner Resume starts a fresh memory episode.",
		"No automatic resume.",
		"Source/archive availability and latest edits unverified.",
		"Historical checkpoints/captures do not verify them.",
	}
}
