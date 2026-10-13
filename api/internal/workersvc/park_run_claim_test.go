package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

func TestClaimNegotiatesParkRunIndependentlyOfPlanApproval(t *testing.T) {
	for _, supported := range []bool{false, true} {
		for _, approved := range []bool{false, true} {
			fs, svc, worker := selfImproveClaimFixture(t, uuid.New(), uuid.New())
			fs.claimRun.Kind = runkind.Issue
			fs.claimRun.AutoApprove = approved
			if approved {
				fs.claimRun.PlanMd = pgconv.TextOrNull("persisted approved plan")
			}
			if supported {
				worker.ProtocolCapabilities = []string{capability.ParkRunV1}
			}
			payload, err := svc.Claim(context.Background(), worker, nil)
			if err != nil || payload == nil {
				t.Fatalf("Claim: payload=%v err=%v", payload, err)
			}
			if payload.ParkRunSupported != supported {
				t.Fatalf("supported=%v approved=%v: park_run_supported=%v", supported, approved, payload.ParkRunSupported)
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), `"park_run_supported":true`) != supported ||
				(!supported && strings.Contains(string(wire), `"park_run_supported"`)) {
				t.Fatalf("supported=%v: additive claim field mismatch", supported)
			}
		}
	}
}
