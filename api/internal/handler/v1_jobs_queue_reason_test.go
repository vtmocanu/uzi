package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func strp(s string) *string { return &s }

// TestV1JobDTOQueueReasonStaysAdditive pins the additive-only /api/v1 contract (PRD #3522): the
// weekly usage reserve never appears in queue_reason, it is carried by reserve_hold_reason.
func TestV1JobDTOQueueReasonStaysAdditive(t *testing.T) {
	cases := []struct {
		name       string
		view       workersvc.JobView
		wantQueue  string
		wantHold   string
		wantQueueN bool
		wantHoldN  bool
	}{
		{"reserve hold", workersvc.JobView{QueueReason: strp("weekly_reserve"), ReserveHoldReason: strp("weekly usage reserve: 85% >= 80%")}, "", apitypes.V1ReserveHoldText, true, false},
		{"reserve hold, unknown reading", workersvc.JobView{QueueReason: strp("weekly_reserve"), ReserveHoldReason: strp("weekly usage reserve: weekly usage unknown")}, "", apitypes.V1ReserveHoldText, true, false},
		{"maintenance wins", workersvc.JobView{QueueReason: strp("maintenance"), ReserveHoldReason: nil}, "maintenance", "", false, true},
		{"none", workersvc.JobView{}, "", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.view.ID = uuid.New()
			dto := v1JobDTO(tc.view)
			if tc.wantQueueN != (dto.QueueReason == nil) || (dto.QueueReason != nil && *dto.QueueReason != tc.wantQueue) {
				t.Fatalf("queue_reason = %v, want %q (absent=%v)", dto.QueueReason, tc.wantQueue, tc.wantQueueN)
			}
			if tc.wantHoldN != (dto.ReserveHoldReason == nil) || (dto.ReserveHoldReason != nil && *dto.ReserveHoldReason != tc.wantHold) {
				t.Fatalf("reserve_hold_reason = %v, want %q (absent=%v)", dto.ReserveHoldReason, tc.wantHold, tc.wantHoldN)
			}
			b, _ := json.Marshal(dto)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if v, ok := m["queue_reason"]; ok && v == "weekly_reserve" {
				t.Fatalf("v1 JSON leaks weekly_reserve: %s", b)
			}
			// The owner's usage figures stay off /api/v1: a product token is scoped to its jobs.
			if strings.Contains(string(b), "%") {
				t.Fatalf("v1 JSON leaks usage figures: %s", b)
			}
		})
	}
}
