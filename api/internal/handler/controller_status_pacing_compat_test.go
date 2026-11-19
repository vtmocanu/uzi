package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Producer skew between the controller and the api (issue #4467): an older controller
// invented a synthetic blocking_worker_id for pool-level holds; the api must keep the
// rest of that report instead of dropping the row.
func TestControllerStatusMixedPacingCompatibility(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	post := func(t *testing.T, pacing map[string]any) *statusStore {
		t.Helper()
		st := &statusStore{rows: 1}
		h := newProtocolHandler(t, st)
		h.now = func() time.Time { return now }
		body, err := json.Marshal(map[string]any{"kind": "fleet", "workers": []any{map[string]any{
			"id": "11111111-1111-1111-1111-111111111111", "phase": "settled", "pacing": pacing}}})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(body))))
		if rec.Code != http.StatusNoContent || len(st.upserts) != 1 {
			t.Fatalf("status=%d upserts=%d", rec.Code, len(st.upserts))
		}
		if p := st.upserts[0]; !p.UpdatePacing || !p.ObservedAt.Time.Equal(now) {
			t.Fatalf("authority/receipt: %+v", p)
		}
		return st
	}
	stored := func(t *testing.T, st *statusStore) map[string]any {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal(st.upserts[0].Pacing, &got); err != nil {
			t.Fatalf("pacing %q: %v", st.upserts[0].Pacing, err)
		}
		return got
	}

	t.Run("old producer synthetic identifier", func(t *testing.T) {
		st := post(t, map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2",
			"blocking_reason": "participant reservation without canary", "blocking_worker_id": "missing-canary"})
		got := stored(t, st)
		if _, ok := got["blocking_worker_id"]; ok || got["state"] != "held" || got["blocking_reason"] != "participant reservation without canary" {
			t.Fatalf("stored pacing: %v", got)
		}
	})

	t.Run("new producer shape is accepted exactly", func(t *testing.T) {
		st := post(t, map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2", "blocking_reason": "pool paced"})
		want := `{"state":"held","target_image":"repo:v2","target_version":"2","blocking_reason":"pool paced"}`
		if string(st.upserts[0].Pacing) != want {
			t.Fatalf("stored %s, want %s", st.upserts[0].Pacing, want)
		}
	})

	t.Run("real identifiers are kept exactly", func(t *testing.T) {
		canary, blocker := uuid.NewString(), uuid.NewString()
		got := stored(t, post(t, map[string]any{"state": "held", "target_image": "i", "target_version": "v", "canary_id": canary, "blocking_worker_id": blocker}))
		if got["canary_id"] != canary || got["blocking_worker_id"] != blocker {
			t.Fatalf("stored pacing: %v", got)
		}
	})

	t.Run("control characters stripped", func(t *testing.T) {
		got := stored(t, post(t, map[string]any{"state": "held", "blocking_reason": "bad\n\u001bpod"}))
		if got["blocking_reason"] != "badpod" {
			t.Fatalf("stored pacing: %v", got)
		}
	})

	t.Run("oversize content is discarded", func(t *testing.T) {
		st := post(t, map[string]any{"state": "held", "blocking_reason": strings.Repeat("x", 193)})
		if st.upserts[0].Pacing != nil {
			t.Fatalf("oversize pacing persisted: %s", st.upserts[0].Pacing)
		}
	})
}

// The strict contracts the lenient handler decode must not weaken: the service refuses
// a synthetic identifier, and stored pacing carrying one is never rendered.
func TestControllerStatusMixedPacingStrictContracts(t *testing.T) {
	synthetic := "missing-canary"
	h := newProtocolHandler(t, &statusStore{rows: 1})
	_, err := h.wsvc.RecordRollHealth(context.Background(), workersvc.RollHealthReport{
		Kind:        "fleet",
		WorkerID:    uuid.New(),
		Phase:       "settled",
		ObservedAt:  time.Now(),
		Pacing:      &workersvc.PacingReport{State: "held", BlockingWorkerID: &synthetic},
		TargetImage: "repo:v2",
	})
	if err == nil {
		t.Fatal("RecordRollHealth accepted a synthetic blocking_worker_id")
	}

	receipt := time.Now().UTC().Truncate(time.Second)
	rc := pgtype.Timestamptz{Time: receipt, Valid: true}
	for _, field := range []string{"canary_id", "blocking_worker_id"} {
		data := []byte(`{"state":"held","` + field + `":"missing-canary"}`)
		if got := workersvc.ParseFreshPacingReport(data, rc, receipt); got != nil {
			t.Fatalf("%s: stored synthetic identifier rendered: %+v", field, got)
		}
	}
	if got := workersvc.ParseFreshPacingReport([]byte(`{"state":"held"}`), rc, receipt); got == nil {
		t.Fatal("control: valid stored pacing must render")
	}
}

// A wrong-shape pacing value must poison neither the report envelope, the row decode
// nor the capacity cause read from the raw row (#4467): the row still lands with its
// scheduler_capacity blocking kind and no pacing.
func TestControllerStatusWrongShapePacingKeepsRowAndBlockingKind(t *testing.T) {
	for _, pacing := range []string{`"garbage"`, `[]`, `5`, `{"state":5}`, `{"state":"held","target_image":{}}`, `{"state":"held","canary_id":5}`} {
		t.Run(pacing, func(t *testing.T) {
			st := &statusStore{rows: 1}
			h := newProtocolHandler(t, st)
			body := `{"kind":"fleet","workers":[{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","pod_phase":"Pending",` +
				`"blocking_reason":"scheduler capacity: insufficient cpu","blocking_kind":"scheduler_capacity","pacing":` + pacing + `}]}`
			rec := httptest.NewRecorder()
			h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
			if rec.Code != http.StatusNoContent || len(st.upserts) != 1 {
				t.Fatalf("status=%d upserts=%d", rec.Code, len(st.upserts))
			}
			p := st.upserts[0]
			if !p.BlockingKind.Valid || p.BlockingKind.String != "scheduler_capacity" || p.Phase != "stuck" || !p.UpdatePacing {
				t.Fatalf("row: kind=%+v phase=%q update=%v", p.BlockingKind, p.Phase, p.UpdatePacing)
			}
			if pacing == `{"state":"held","canary_id":5}` {
				if !strings.Contains(string(p.Pacing), `"state":"held"`) || strings.Contains(string(p.Pacing), "canary_id") {
					t.Fatalf("identity not omitted alone: %s", p.Pacing)
				}
			} else if len(p.Pacing) != 0 {
				t.Fatalf("malformed pacing persisted: %s", p.Pacing)
			}
		})
	}
}
