package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const workerRequeueExhaustedCause = "worker_requeue_exhausted"

func isWorkerRecoveryExhausted(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && strOr(r.RecoveryWaitCause, "") == workerRequeueExhaustedCause
}

func workerRecoveryAllowance(r apitypes.RunDTO) string {
	allowance := "automatic requeue allowance unknown"
	if w := r.WorkerRecovery; w != nil {
		allowance = fmt.Sprintf("%d of %d automatic requeues used · %d remaining · episode %d", w.EpisodeUsed, w.AutomaticRequeueLimit, w.EpisodeRemaining, w.Episode)
	}
	return fmt.Sprintf("%s · lifetime charged count %d", allowance, r.RequeueCount)
}

func workerExhaustionLine(r apitypes.RunDTO) string {
	if !isWorkerRecoveryExhausted(r) {
		return ""
	}
	id := cellText(r.ID)
	return "worker recovery exhausted · " + workerRecoveryAllowance(r) +
		" · owner Resume: uzi run resume " + id + "; Cancel: uzi run cancel " + id +
		" · no automatic resume; only owner Resume starts a fresh allowance"
}

// workerExhaustionEvidence describes the server's historical observation, never
// current availability or an entitlement to export.
func workerExhaustionEvidence(r apitypes.RunDTO) []string {
	if !isWorkerRecoveryExhausted(r) {
		return nil
	}
	if r.WorkerRecovery == nil || r.WorkerRecovery.Evidence == nil {
		return []string{"Recovery evidence unavailable or unknown."}
	}
	e := r.WorkerRecovery.Evidence
	var lines []string
	if e.CheckpointTip != nil && *e.CheckpointTip != "" {
		lines = append(lines, "The server recorded checkpoint "+cellText(*e.CheckpointTip)+" before this hold. Current availability and the latest local edits are not verified.")
	}
	if e.AvailableCapture {
		stamp := "an unknown time"
		if !e.RecordedAt.IsZero() {
			stamp = e.RecordedAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, "A recovery capture was recorded as available at "+stamp+". It may later expire or be discarded; it may not contain the latest local edits.")
	}
	var uncertain []string
	if e.PublicationUncertain {
		uncertain = append(uncertain, "publication pending or uncertain")
	}
	if e.CaptureUncertain {
		uncertain = append(uncertain, "capture pending or uncertain")
	}
	if e.CustodyUncertain {
		uncertain = append(uncertain, "retained source custody uncertain")
	}
	if e.Unknown || (len(lines) == 0 && len(uncertain) == 0) {
		uncertain = append(uncertain, "evidence unavailable or unknown")
	}
	if len(uncertain) > 0 {
		lines = append(lines, "Recorded recovery uncertainty: "+strings.Join(uncertain, "; ")+".")
	}
	return lines
}

// workerExhaustionTUILines puts owner actions and historical caveats before
// values that a narrow terminal may clip. Each entry occupies one physical row.
func workerExhaustionTUILines(r apitypes.RunDTO) []string {
	if !isWorkerRecoveryExhausted(r) {
		return nil
	}
	lines := []string{"worker recovery exhausted · owner Resume or Cancel"}
	if w := r.WorkerRecovery; w != nil {
		lines = append(lines, fmt.Sprintf("%d of %d automatic requeues used · %d remaining", w.EpisodeUsed, w.AutomaticRequeueLimit, w.EpisodeRemaining),
			fmt.Sprintf("episode %d · lifetime charged count %d", w.Episode, r.RequeueCount))
	} else {
		lines = append(lines, "automatic requeue allowance unknown", fmt.Sprintf("lifetime charged count %d", r.RequeueCount))
	}
	lines = append(lines, "Resume: uzi run resume "+cellText(r.ID), "Cancel: uzi run cancel "+cellText(r.ID),
		"Only owner Resume starts a fresh allowance; no auto-resume.")
	if r.WorkerRecovery == nil || r.WorkerRecovery.Evidence == nil {
		return append(lines, "Recovery evidence unavailable or unknown.")
	}
	e := r.WorkerRecovery.Evidence
	lines = append(lines, "Historical evidence: availability/latest edits unverified.")
	if e.CheckpointTip != nil && *e.CheckpointTip != "" {
		lines = append(lines, "Recorded checkpoint: "+cellText(*e.CheckpointTip))
	}
	if e.AvailableCapture {
		stamp := "unknown time"
		if !e.RecordedAt.IsZero() {
			stamp = e.RecordedAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, "Capture may expire/discard, lack latest edits: "+stamp)
	}
	if e.PublicationUncertain {
		lines = append(lines, "Recorded publication pending or uncertain.")
	}
	if e.CaptureUncertain {
		lines = append(lines, "Recorded capture pending or uncertain.")
	}
	if e.CustodyUncertain {
		lines = append(lines, "Recorded retained source custody uncertain.")
	}
	if e.Unknown || (e.CheckpointTip == nil && !e.AvailableCapture && !e.PublicationUncertain && !e.CaptureUncertain && !e.CustodyUncertain) {
		lines = append(lines, "Recovery evidence unavailable or unknown.")
	}
	return lines
}

func workerExhaustionRows(r apitypes.RunDTO) [][]string {
	if !isWorkerRecoveryExhausted(r) {
		return nil
	}
	rows := [][]string{{"WORKER_RECOVERY", workerExhaustionLine(r)}}
	for _, line := range workerExhaustionEvidence(r) {
		rows = append(rows, []string{"RECOVERY_EVIDENCE", line})
	}
	return rows
}
