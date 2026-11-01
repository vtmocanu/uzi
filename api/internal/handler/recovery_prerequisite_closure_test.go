package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// TestProtocolFeaturesClosureGroupIsLastAndOwn pins the closure token as its own trailing group
// that changes nothing else.
func TestProtocolFeaturesClosureGroupIsLastAndOwn(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, held := range []bool{false, true} {
			for _, resume := range []bool{false, true} {
				on, off := protocolFeatures(active, held, resume, true), protocolFeatures(active, held, resume, false)
				if slices.Contains(off, capability.RecoveryPrerequisiteClosureV1) {
					t.Fatalf("active=%t held=%t resume=%t: advertised while disabled: %v", active, held, resume, off)
				}
				if on[len(on)-1] != capability.RecoveryPrerequisiteClosureV1 || !slices.Equal(on[:len(on)-1], off) {
					t.Fatalf("active=%t held=%t resume=%t: closure must be its own last group: on=%v off=%v", active, held, resume, on, off)
				}
			}
		}
	}
}

// TestAdvertisedFeaturesClosureNeedsKnobPortAndLimiter drives the per-request advertisement
// through the real recovery service and the same limiter wiring mountWorkerRoutes performs. The
// service is built first, as in main, so a boot-time snapshot would read false.
func TestAdvertisedFeaturesClosureNeedsKnobPortAndLimiter(t *testing.T) {
	limiter := mw.NewLimiter(10, time.Minute, nil)
	advertised := func(h *Handler) bool {
		return slices.Contains(h.advertisedFeatures(), capability.RecoveryPrerequisiteClosureV1)
	}
	cases := []struct {
		name    string
		forges  []string
		port    bool
		limiter *mw.Limiter
		want    bool
	}{
		{"all three", []string{"github"}, true, limiter, true},
		{"github among others", []string{"gitlab", "github"}, true, limiter, true},
		{"knob off", nil, true, limiter, false},
		{"knob lacks github", []string{"gitlab"}, true, limiter, false},
		{"no limiter", []string{"github"}, true, nil, false},
		{"no port", []string{"github"}, false, limiter, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProtocolHandler(t, &protocolStore{})
			h.cfg.PrerequisiteClosureForges = tc.forges
			svc := h.RecoveryService()
			if !tc.port {
				// Only the port goes: the knob and the limiter stay wired.
				svc.SetPrerequisiteFinal(nil)
			}
			if advertised(h) {
				t.Fatal("advertised before the limiter was wired")
			}
			h.mountWorkerRoutes(chi.NewRouter(), tc.limiter)
			if got := advertised(h); got != tc.want {
				t.Fatalf("advertised = %t, want %t", got, tc.want)
			}
		})
	}
}

// The heartbeat replaces the worker's feature set, so it must advertise the closure feature
// exactly as register does, or a worker would lose it after its first heartbeat.
func TestWorkerHeartbeatAdvertisesPrerequisiteClosure(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	h.cfg.PrerequisiteClosureForges = []string{"github"}
	h.RecoveryService()
	h.mountWorkerRoutes(chi.NewRouter(), mw.NewLimiter(10, time.Minute, nil))
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, "{}", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Features []string `json:"protocol_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(response.Features, capability.RecoveryPrerequisiteClosureV1) {
		t.Fatalf("heartbeat features %v lack %s", response.Features, capability.RecoveryPrerequisiteClosureV1)
	}
}
