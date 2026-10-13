package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestControllerStatusPacing(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		pacing     any
		update     bool
		accepted   bool
	}{
		{"fleet held", "fleet", map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2", "blocking_reason": "bad\n\u001bpod"}, true, true},
		{"fleet clear", "fleet", nil, true, true},
		{"readiness preserve", "readiness", nil, false, true},
		{"legacy preserve", "", nil, false, true},
		{"legacy additive", "", map[string]any{"state": "active"}, true, true},
		{"completed invalid", "fleet", map[string]any{"state": "completed"}, false, false},
		{"image bound", "fleet", map[string]any{"state": "held", "target_image": strings.Repeat("x", 513)}, false, false},
		{"version bound", "fleet", map[string]any{"state": "held", "target_version": strings.Repeat("x", 129)}, false, false},
		{"reason bound", "fleet", map[string]any{"state": "held", "blocking_reason": strings.Repeat("x", 193)}, false, false},
		{"canary invalid", "fleet", map[string]any{"state": "held", "canary_id": "invalid"}, false, false},
		{"blocker invalid", "fleet", map[string]any{"state": "held", "blocking_worker_id": "invalid"}, false, false},
		{"retained image bound", "fleet", map[string]any{"state": "held", "retained_image": strings.Repeat("x", 513)}, false, false},
		{"retained version bound", "fleet", map[string]any{"state": "held", "retained_version": strings.Repeat("x", 129)}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &statusStore{rows: 1}
			h := newProtocolHandler(t, st)
			now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
			h.now = func() time.Time { return now }
			body, err := json.Marshal(map[string]any{"kind": tc.kind, "reported_at": now.Add(24 * time.Hour), "workers": []any{map[string]any{"id": "11111111-1111-1111-1111-111111111111", "phase": "settled", "pacing": tc.pacing}}})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(body))))
			if rec.Code != 204 {
				t.Fatalf("status %d", rec.Code)
			}
			if !tc.accepted {
				if len(st.upserts) != 0 {
					t.Fatal("invalid whole entry persisted")
				}
				return
			}
			if len(st.upserts) != 1 {
				t.Fatalf("upserts %d", len(st.upserts))
			}
			p := st.upserts[0]
			if p.UpdatePacing != tc.update || !p.ObservedAt.Time.Equal(now) {
				t.Fatalf("authority/receipt: %+v", p)
			}
			if tc.name == "fleet held" {
				var got workersvc.PacingReport
				if err := json.Unmarshal(p.Pacing, &got); err != nil {
					t.Fatal(err)
				}
				if got.BlockingReason == nil || *got.BlockingReason != "badpod" {
					t.Fatalf("reason: %+v", got.BlockingReason)
				}
			}
		})
	}
}

func TestControllerStatusPacingInvalidDoesNotBlockSibling(t *testing.T) {
	st := &statusStore{rows: 1}
	h := newProtocolHandler(t, st)
	body := `{"kind":"fleet","workers":[{"id":"11111111-1111-1111-1111-111111111111","phase":"settled","pacing":{"state":"invalid"}},{"id":"22222222-2222-2222-2222-222222222222","phase":"settled","pacing":{"state":"override"}}]}`
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
	if rec.Code != 204 || len(st.upserts) != 1 || st.upserts[0].WorkerID.String() != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("status=%d upserts=%+v", rec.Code, st.upserts)
	}
}
