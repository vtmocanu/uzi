package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// M7 — the part of the /status contract NO golden can cover.
//
// The two goldens do cover field NAMES thoroughly: the consumer decodes the producer's
// byte-for-byte output and asserts every field, so a rename reddens. What they cannot see is
// everything outside the body — the URL path, the bearer header, the Content-Type and the
// 204-with-no-body — because a golden is a file, not a request. A path or auth mismatch is
// invisible to both sides while both stay green.
//
// The PRODUCER half of this is already pinned in the controller
// (TestReportPostsTheFleetWithBearerAuthAndReadsNoResponse asserts method, path, bearer,
// content-type and tolerates 204). This is the CONSUMER half.

type statusStore struct {
	workersvc.Store
	upserts []store.UpsertWorkerRollHealthParams
	rows    int64
}

func (s *statusStore) UpsertWorkerRollHealth(_ context.Context, arg store.UpsertWorkerRollHealthParams) (int64, error) {
	s.upserts = append(s.upserts, arg)
	return s.rows, nil
}

func TestControllerStatusAnswers204WithNoBody(t *testing.T) {
	st := &statusStore{rows: 1}
	h := newProtocolHandler(t, st)

	body := `{"reported_at":"2026-07-26T10:00:00Z","poll_interval_seconds":10,"worker_image_tag":"0.11.7",
	          "workers":[{"id":"11111111-1111-1111-1111-111111111111","phase":"stuck","phase_since":null,
	          "target_image":"repo/agent-base:0.11.7","pod_phase":"Pending","blocking_container":"seed-nix",
	          "blocking_reason":"CrashLoopBackOff","restart_count":6,"last_exit_code":2}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body))
	h.ControllerStatus(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204. The response carrying nothing is what makes display-only "+
			"STRUCTURAL: there is no body for the controller to read state out of, so this endpoint "+
			"cannot quietly become a second desired-state channel. Body: %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if len(st.upserts) != 1 {
		t.Fatalf("recorded %d upserts, want 1", len(st.upserts))
	}
	// The api's OWN clock is what lands in observed_at, and the wire's timestamp goes to the
	// display-only column. Collapsing them would hand freshness to the reporting party.
	got := st.upserts[0]
	if !got.ObservedAt.Valid || got.ObservedAt.Time.Equal(got.ControllerReportedAt.Time) {
		t.Errorf("observed_at (%v) must be the api's own receipt time, distinct from the wire's "+
			"controller_reported_at (%v)", got.ObservedAt.Time, got.ControllerReportedAt.Time)
	}
}

// A phase outside the closed enum drops that ENTRY and still answers 204. One garbage row
// must never blind the whole fleet's report, and it must never be persisted and rendered as
// a free string, since this value selects a badge.
func TestControllerStatusDropsAnUnmodelledPhaseWithout400ingTheReport(t *testing.T) {
	st := &statusStore{rows: 1}
	h := newProtocolHandler(t, st)

	body := `{"reported_at":"2026-07-26T10:00:00Z","workers":[
	  {"id":"11111111-1111-1111-1111-111111111111","phase":"exploded","restart_count":0},
	  {"id":"22222222-2222-2222-2222-222222222222","phase":"settled","restart_count":0}]}`
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: one unmodelled phase must not fail the whole report", rec.Code)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("recorded %d upserts, want exactly 1 — the garbage row dropped, the good row kept", len(st.upserts))
	}
	if st.upserts[0].Phase != "settled" {
		t.Errorf("persisted phase %q; only enum members may reach the store", st.upserts[0].Phase)
	}
}

// The entry cap is its own bound, not a consequence of the body cap: at ~120 bytes an entry a
// legal 1 MiB body still carries thousands of upserts per tick.
func TestControllerStatusRejectsTooManyEntries(t *testing.T) {
	st := &statusStore{rows: 1}
	h := newProtocolHandler(t, st)

	var b strings.Builder
	b.WriteString(`{"reported_at":"2026-07-26T10:00:00Z","workers":[`)
	for i := 0; i <= maxStatusReportWorkers; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"11111111-1111-1111-1111-111111111111","phase":"settled","restart_count":0}`)
	}
	b.WriteString(`]}`)

	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(b.String())))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d for %d entries (cap %d), want 400", rec.Code, maxStatusReportWorkers+1, maxStatusReportWorkers)
	}
	if len(st.upserts) != 0 {
		t.Errorf("recorded %d upserts for an over-cap report, want 0 — the cap must bound WORK, not just "+
			"return an error after doing it", len(st.upserts))
	}
}

// The production decoder must stay LENIENT. A newer controller sending an extra field is the
// normal state during a rollout, and a strict decoder would 400 the entire fleet's report at
// exactly that moment. The contract TEST sets DisallowUnknownFields; this one must not.
func TestControllerStatusToleratesUnknownFields(t *testing.T) {
	st := &statusStore{rows: 1}
	h := newProtocolHandler(t, st)

	body := `{"reported_at":"2026-07-26T10:00:00Z","a_field_from_a_newer_controller":true,
	          "workers":[{"id":"11111111-1111-1111-1111-111111111111","phase":"settled",
	          "restart_count":0,"something_new":"x"}]}`
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d for a body with unknown fields, want 204. D10 requires forward "+
			"compatibility: a strict decoder here turns a version skew into a fleet-wide 400 during "+
			"the rollout that caused it.", rec.Code)
	}
	if len(st.upserts) != 1 {
		t.Errorf("the known fields must still be recorded; got %d upserts", len(st.upserts))
	}
}
