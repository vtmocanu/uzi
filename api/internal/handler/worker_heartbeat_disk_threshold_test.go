package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1809 D5: the heartbeat response carries the api's configured disk-pressure
// threshold so the worker derives its soft/hard disk thresholds from it. A value outside
// (0,1] (here: an unset Params) is omitted, and the worker falls back to 0.90.
func TestWorkerHeartbeatCarriesDiskPressureThreshold(t *testing.T) {
	cases := []struct {
		name      string
		threshold float64
		want      *float64
	}{
		{name: "default 0.90 flows through", threshold: 0.90, want: ptrFloat(0.90)},
		{name: "non-default 0.75 flows through", threshold: 0.75, want: ptrFloat(0.75)},
		{name: "unset threshold is omitted", threshold: 0, want: nil},
		{name: "out-of-range threshold above 1 is omitted", threshold: 1.5, want: nil},
		{name: "exactly 1 flows through", threshold: 1, want: ptrFloat(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			box, err := secretbox.New(make([]byte, secretbox.KeySize))
			if err != nil {
				t.Fatalf("new box: %v", err)
			}
			h := &Handler{wsvc: workersvc.New(&protocolStore{}, box, workersvc.Params{DiskPressureThreshold: c.threshold})}
			rec := httptest.NewRecorder()
			h.WorkerHeartbeat(rec, workerReq(http.MethodPost, `{"version":"1"}`, uuid.Nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
			}
			var resp struct {
				Worker map[string]json.RawMessage `json:"worker"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v body %q", err, rec.Body.String())
			}
			raw, ok := resp.Worker["disk_pressure_threshold"]
			if c.want == nil {
				if ok {
					t.Fatalf("disk_pressure_threshold = %s, want the key omitted", raw)
				}
				return
			}
			if !ok {
				t.Fatalf("disk_pressure_threshold missing from heartbeat response %q", rec.Body.String())
			}
			var got float64
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode threshold %s: %v", raw, err)
			}
			if got != *c.want {
				t.Fatalf("disk_pressure_threshold = %v, want %v", got, *c.want)
			}
		})
	}
}

// The threshold is heartbeat-only: the shared DTO builder used by the list/admin
// surfaces leaves it nil, so their JSON is unchanged.
func TestWorkerDTOOmitsDiskPressureThresholdOutsideHeartbeat(t *testing.T) {
	now := time.Now()
	dto := workerDTOFromWorker(store.Worker{ID: uuid.New(), UserID: uuid.New()}, 0, false, "", "", "", now, now)
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw, ok := m["disk_pressure_threshold"]; ok {
		t.Fatalf("non-heartbeat WorkerDTO carries disk_pressure_threshold=%s, want omitted", raw)
	}
}

func ptrFloat(v float64) *float64 { return &v }
