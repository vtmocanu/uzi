package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPolicyProtocolFeaturePrerequisites(t *testing.T) {
	for _, snapshots := range []bool{false, true} {
		for _, lease := range []time.Duration{-time.Second, 0, time.Hour} {
			t.Run(fmt.Sprintf("snapshots_%t_lease_%s", snapshots, lease), func(t *testing.T) {
				want := snapshots && lease > 0
				if got := slices.Contains(protocolFeatures(snapshots, true, lease), "provider_policy_hold"); got != want {
					t.Fatalf("policy feature=%t, want %t", got, want)
				}
				for _, heartbeat := range []bool{false, true} {
					h := newProtocolHandler(t, &protocolStore{})
					h.cfg.ActiveSnapshotDisabled = !snapshots
					h.cfg.TerminalPendingLease = lease
					rec := httptest.NewRecorder()
					if heartbeat {
						h.WorkerHeartbeat(rec, workerReq(http.MethodPost, "{}", uuid.New()))
					} else {
						h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"policy-worker","version":"1"}`, uuid.Nil))
					}
					if rec.Code != http.StatusOK {
						t.Fatalf("heartbeat=%t code=%d body=%s", heartbeat, rec.Code, rec.Body.String())
					}
					var response struct {
						Features []string `json:"protocol_features"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if got := slices.Contains(response.Features, "provider_policy_hold"); got != want {
						t.Fatalf("heartbeat=%t policy feature=%t, want %t", heartbeat, got, want)
					}
					if !slices.Contains(response.Features, "claim_generation_fence") {
						t.Fatal("policy negotiation lost its generation-fence prerequisite")
					}
				}
			})
		}
	}
}
