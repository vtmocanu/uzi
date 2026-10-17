package runpresentation

import (
	"encoding/json"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PauseOrigin selects only the newest paused marker in the current status epoch.
// ready describes current message coverage, independently of older unloaded history.
func PauseOrigin(run apitypes.RunDTO, messages []apitypes.MessageDTO, ready bool) string {
	if !ready || run.Status != "paused" || (run.HoldReason != nil && *run.HoldReason != "") || run.StatusSince == nil || run.StatusSince.IsZero() {
		return ""
	}
	var newest *apitypes.MessageDTO
	for i := range messages {
		m := &messages[i]
		if m.Kind == "paused" && (newest == nil || m.Seq > newest.Seq) {
			newest = m
		}
	}
	if newest == nil || newest.Seq <= 0 || newest.CreatedAt.IsZero() || newest.CreatedAt.Before(*run.StatusSince) {
		return ""
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(newest.Payload, &payload) != nil {
		return ""
	}
	var origin string
	if json.Unmarshal(payload["origin"], &origin) != nil {
		return ""
	}
	switch origin {
	case "lead_park", "owner":
		return origin
	}
	return ""
}

// PauseLabel accepts only fixed origins; payload text never reaches a terminal.
func PauseLabel(origin string, personal bool) string {
	switch origin {
	case "lead_park":
		return "paused by the run's lead"
	case "owner":
		if personal {
			return "paused by you"
		}
		return "paused by its owner"
	default:
		return "paused"
	}
}

// ContiguousMessages reports whether a loaded suffix covers every sequence in its bounds.
func ContiguousMessages(messages []apitypes.MessageDTO, after, through int32) bool {
	if after < 0 || through < after {
		return false
	}
	if through == after {
		return true
	}
	seen := make(map[int32]bool, len(messages))
	for _, m := range messages {
		if m.Seq > after && m.Seq <= through {
			seen[m.Seq] = true
		}
	}
	return int64(len(seen)) == int64(through)-int64(after)
}
