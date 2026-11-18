package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The stopped settlement request is one bounded JSON object with exactly the
// four authority fields; anything else is refused as malformed (400) with no
// write, and an accepted stop answers with the minimal acknowledgement even
// when the run carries a large plan.
func TestWorkerCodexStoppedRequestShapeRouteLiveDB(t *testing.T) {
	post := func(e *applyRouteEnv, body string) (int, []byte) {
		rec := e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state", body)
		return rec.Code, rec.Body.Bytes()
	}
	valid := func(w stoppedRouteWire) string {
		body, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	refused := []struct {
		name string
		body func(e *applyRouteEnv, w stoppedRouteWire) string
	}{
		{"body_over_bound", func(_ *applyRouteEnv, w stoppedRouteWire) string {
			return valid(w) + strings.Repeat(" ", 1<<20)
		}},
		{"trailing_value", func(_ *applyRouteEnv, w stoppedRouteWire) string { return valid(w) + "{}" }},
		{"op_id_present", func(e *applyRouteEnv, w stoppedRouteWire) string {
			return strings.TrimSuffix(valid(w), "}") + fmt.Sprintf(`,"op_id":%q}`, e.op)
		}},
		{"unknown_field", func(_ *applyRouteEnv, w stoppedRouteWire) string {
			return strings.TrimSuffix(valid(w), "}") + `,"unknown":true}`
		}},
		{"expected_epoch_on_running", func(_ *applyRouteEnv, w stoppedRouteWire) string {
			return fmt.Sprintf(`{"status":"running","claim_generation":%d,"register_nonce":%q,"expected_epoch":%d}`,
				w.ClaimGeneration, w.RegisterNonce, w.ExpectedEpoch)
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			e, w := setupStoppedGuardsRoute(t)
			withdrawStoppedRoute(t, e, w)
			before := stoppedRouteSnapshot(t, e)
			if code, _ := post(e, tc.body(e, w)); code != http.StatusBadRequest {
				t.Fatalf("malformed stopped request status=%d; want 400", code)
			}
			assertStoppedSnapshot(t, before, stoppedRouteSnapshot(t, e), false)
		})
	}
	t.Run("body_at_bound", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		body := valid(w)
		body += strings.Repeat(" ", (1<<20)-len(body))
		code, raw := post(e, body)
		var ack struct {
			Applied bool `json:"applied"`
		}
		if code != http.StatusOK || json.Unmarshal(raw, &ack) != nil || !ack.Applied {
			t.Fatalf("stopped request at the 1 MiB bound status=%d; want accepted", code)
		}
	})
	t.Run("minimal_ack_large_plan", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		stoppedRouteExec(t, e, "UPDATE runs SET plan_md=$2 WHERE id=$1", e.run, strings.Repeat("p", 64<<10))
		reportStoppedRoute(t, e, w, true, true)
	})
}
