package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRunDTOMemoryHold(t *testing.T) {
	for _, tc := range []struct {
		name, status, cause, policy string
		used                        int32
		want                        string
	}{
		{"known policy", "recovery_wait", "worker_memory_pressure", `{"version":1,"max_interventions":3}`, 2, `{"episode":4,"used":2,"max":3,"remaining":1}`},
		{"exhausted", "recovery_wait", "worker_memory_pressure", `{"version":1,"max_interventions":3}`, 3, `{"episode":4,"used":3,"max":3,"remaining":0}`},
		{"unknown policy", "recovery_wait", "worker_memory_pressure", "", 2, `{"episode":4,"used":2,"max":null,"remaining":null}`},
		{"unsupported policy", "recovery_wait", "worker_memory_pressure", `{"version":2,"max_interventions":3}`, 2, `{"episode":4,"used":2,"max":null,"remaining":null}`},
		{"invalid policy", "recovery_wait", "worker_memory_pressure", `{"version":1,"max_interventions":0}`, 2, `{"episode":4,"used":2,"max":null,"remaining":null}`},
		{"malformed policy", "recovery_wait", "worker_memory_pressure", "{", 2, `{"episode":4,"used":2,"max":null,"remaining":null}`},
		{"ordinary outage", "recovery_wait", "provider_outage", `{"version":1,"max_interventions":3}`, 2, "null"},
		{"requeue hold", "recovery_wait", "worker_requeue_exhausted", "", 2, "null"},
		{"resumed", "queued", "worker_memory_pressure", "", 0, "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := store.Run{Status: tc.status, RecoveryWaitCause: pgconv.Text(tc.cause),
				MemoryEpisode: 4, MemoryInterventionCount: tc.used, MemoryPolicy: []byte(tc.policy),
				RequeueCount: 9, RequeueEpisodeBaseline: 7}
			dto := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 5)
			raw, err := json.Marshal(dto.MemoryHold)
			if err != nil || string(raw) != tc.want {
				t.Fatalf("memory hold=%s err=%v want=%s", raw, err, tc.want)
			}
			if tc.cause == "worker_memory_pressure" && dto.WorkerRecovery != nil {
				t.Fatal("memory hold exposed requeue allowance")
			}
			list := apitypes.RunListItemDTO{RunDTO: dto}
			roundTrip := apitypes.RunSummaryOf(list).ListItem()
			raw, err = json.Marshal(roundTrip.MemoryHold)
			if err != nil || string(raw) != tc.want {
				t.Fatalf("summary memory hold=%s err=%v", raw, err)
			}
		})
	}
}
