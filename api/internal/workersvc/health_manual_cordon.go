package workersvc

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const manualCordonWaitPrefix = "waiting for workers cordoned (manual):"

func isManualCordonWaitReason(reason string) bool {
	return strings.HasPrefix(reason, manualCordonWaitPrefix)
}

func manualCordonWaitReason(counts store.CountOnlineWorkersClaimableForRunRow) string {
	if counts.ManualSuitable == 0 || counts.ManualSuitable != counts.ManualCordoned || counts.Claimable > 0 {
		return ""
	}
	var evidence struct {
		WorkerID   uuid.UUID `json:"worker_id"`
		ActorID    uuid.UUID `json:"actor_id"`
		ActorLabel string    `json:"actor_label"`
		At         time.Time `json:"at"`
		Reason     string    `json:"reason"`
	}
	if len(counts.ManualContext) > 8192 || json.Unmarshal(counts.ManualContext, &evidence) != nil ||
		evidence.ActorID == uuid.Nil || evidence.WorkerID == uuid.Nil || evidence.At.IsZero() {
		return ""
	}
	label := store.ManualCordonText(evidence.ActorLabel, 200)
	if strings.TrimSpace(label) == "" {
		label = evidence.ActorID.String()
	}
	reason := store.ManualCordonText(evidence.Reason, 500)
	text := fmt.Sprintf("%s %s (%s) at %s", manualCordonWaitPrefix, label, evidence.ActorID, evidence.At.UTC().Format(time.RFC3339))
	if strings.TrimSpace(reason) != "" {
		text += ": " + reason
	}
	return text
}
