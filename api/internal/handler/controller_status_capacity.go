package handler

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// rawBlockingKind validates original diagnostics before sanitization. Duplicate
// capacity-critical keys (including case equivalents) invalidate only the cause;
// the ordinary decoder retains its existing last-value behavior. Capacity uses a
// fresh current row so duplicate workers arrays cannot supply inherited fields.
func rawBlockingKind(reported hostedsvc.WorkerStatus, raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	seen := make(map[string]bool)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := token.(string)
		if !ok {
			return ""
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return ""
		}
		// EqualFold mirrors encoding/json field matching, including Unicode aliases.
		for _, critical := range []string{"blocking_kind", "phase", "pod_phase", "blocking_reason",
			"blocking_container", "restart_count", "last_exit_code"} {
			if strings.EqualFold(key, critical) {
				if seen[critical] {
					return ""
				}
				seen[critical] = true
				break
			}
		}
	}
	for _, essential := range []string{"blocking_kind", "phase", "pod_phase", "blocking_reason"} {
		if !seen[essential] {
			return ""
		}
	}
	var ws hostedsvc.WorkerStatus
	if json.Unmarshal(raw, &ws) != nil || ws.ID != reported.ID {
		// The handler already validated reported.ID; it must come from this row,
		// rather than a reused element from an earlier workers array.
		return ""
	}
	var kind string
	if json.Unmarshal(ws.BlockingKind, &kind) != nil {
		return ""
	}
	sig := &workersvc.RollSignal{
		BlockingKind: kind, Phase: ws.Phase, PodPhase: ws.PodPhase,
		RestartCount: ws.RestartCount, LastExitCode: ws.LastExitCode,
	}
	if ws.BlockingContainer != nil {
		sig.BlockingContainer = *ws.BlockingContainer
	}
	if ws.BlockingReason != nil {
		sig.BlockingReason = *ws.BlockingReason
	}
	return workersvc.EffectiveBlockingKind(sig)
}
