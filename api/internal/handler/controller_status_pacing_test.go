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
		// outcome is "" (pacing passes through), "discard" (pacing dropped, row kept)
		// or "omit <key>" (pacing kept without that identifier).
		outcome string
	}{
		{"fleet held", "fleet", map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2", "blocking_reason": "bad\n\u001bpod"}, true, ""},
		{"fleet clear", "fleet", nil, true, ""},
		{"readiness preserve", "readiness", nil, false, ""},
		{"legacy preserve", "", nil, false, ""},
		{"legacy additive", "", map[string]any{"state": "active"}, true, ""},
		{"completed invalid", "fleet", map[string]any{"state": "completed"}, true, "discard"},
		{"image bound", "fleet", map[string]any{"state": "held", "target_image": strings.Repeat("x", 513)}, true, "discard"},
		{"version bound", "fleet", map[string]any{"state": "held", "target_version": strings.Repeat("x", 129)}, true, "discard"},
		{"reason bound", "fleet", map[string]any{"state": "held", "blocking_reason": strings.Repeat("x", 193)}, true, "discard"},
		{"canary invalid", "fleet", map[string]any{"state": "held", "canary_id": "invalid"}, true, "omit canary_id"},
		{"blocker invalid", "fleet", map[string]any{"state": "held", "blocking_worker_id": "invalid"}, true, "omit blocking_worker_id"},
		{"retained image bound", "fleet", map[string]any{"state": "held", "retained_image": strings.Repeat("x", 513)}, true, "discard"},
		{"retained version bound", "fleet", map[string]any{"state": "held", "retained_version": strings.Repeat("x", 129)}, true, "discard"},
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
			if len(st.upserts) != 1 {
				t.Fatalf("upserts %d", len(st.upserts))
			}
			p := st.upserts[0]
			if p.UpdatePacing != tc.update || !p.ObservedAt.Time.Equal(now) {
				t.Fatalf("authority/receipt: %+v", p)
			}
			switch {
			case tc.outcome == "discard":
				if p.Pacing != nil {
					t.Fatalf("invalid pacing persisted: %s", p.Pacing)
				}
			case strings.HasPrefix(tc.outcome, "omit "):
				var got map[string]any
				if err := json.Unmarshal(p.Pacing, &got); err != nil {
					t.Fatal(err)
				}
				if got["state"] != "held" {
					t.Fatalf("pacing state not kept: %s", p.Pacing)
				}
				if _, ok := got[strings.TrimPrefix(tc.outcome, "omit ")]; ok {
					t.Fatalf("invalid identifier kept: %s", p.Pacing)
				}
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
	if rec.Code != 204 || len(st.upserts) != 2 {
		t.Fatalf("status=%d upserts=%+v", rec.Code, st.upserts)
	}
	if st.upserts[0].WorkerID.String() != "11111111-1111-1111-1111-111111111111" || st.upserts[0].Pacing != nil || !st.upserts[0].UpdatePacing {
		t.Fatalf("first row: %+v", st.upserts[0])
	}
	if st.upserts[1].WorkerID.String() != "22222222-2222-2222-2222-222222222222" || st.upserts[1].Pacing == nil {
		t.Fatalf("second row: %+v", st.upserts[1])
	}
}
