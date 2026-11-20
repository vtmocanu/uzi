package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postRefusal reports a stuck size_refused roll signal for the worker through the controller
// status endpoint, as the controller does.
func (f *sizeFx) postRefusal(t *testing.T, id, kind, reason string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"kind": "fleet", "reported_at": f.now, "workers": []any{map[string]any{
		"id": id, "phase": "stuck", "pod_phase": "", "blocking_kind": kind, "blocking_reason": reason,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("controller status = %d %s", rec.Code, rec.Body.String())
	}
}

// GET /api/admin/worker-sizes lists hosted workers whose latest FRESH stored signal is a
// size_refused or pvc_rejected refusal, keyed by size; a stale signal is not listed, and the
// array is present (empty) when nothing is refused.
func TestAdminWorkerSizesRefusalsLiveDB(t *testing.T) {
	f := newSizeFx(t)
	ctx := context.Background()

	var raw map[string]json.RawMessage
	rec := cookieReq(t, f.router, http.MethodGet, "/api/admin/worker-sizes", f.jwt, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw["refusals"]), "[") {
		t.Fatalf("refusals = %s, want an array even when nothing is refused", raw["refusals"])
	}

	suffix := f.admin.String()[:8]
	b := f.seedHostedWorker(t, "zz-b-"+suffix, testSizeName)
	a := f.seedHostedWorker(t, "zz-a-"+suffix, testSizeName)
	stale := f.seedHostedWorker(t, "zz-stale-"+suffix, testSizeName)
	ok := f.seedHostedWorker(t, "zz-ok-"+suffix, testSizeName)
	f.postRefusal(t, b.String(), "size_refused", "size above ceiling")
	f.postRefusal(t, a.String(), "pvc_rejected", "PVC rejected")
	f.postRefusal(t, stale.String(), "size_refused", "old refusal")
	mustExecT(ctx, t, f.h.pool, `UPDATE worker_upgrade_reports SET observed_at = $2 WHERE worker_id = $1`, stale, f.now.Add(-time.Hour))
	// A stuck worker that is not a refusal never lists.
	data, _ := json.Marshal(map[string]any{"kind": "fleet", "reported_at": f.now, "workers": []any{map[string]any{
		"id": ok.String(), "phase": "stuck", "pod_phase": "Pending", "blocking_reason": "ImagePullBackOff",
	}}})
	crec := httptest.NewRecorder()
	f.h.ControllerStatus(crec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))

	got := f.get(t).Refusals
	var mine []string
	for _, r := range got {
		if r.Size != testSizeName {
			continue
		}
		mine = append(mine, r.WorkerName+"|"+r.Kind+"|"+r.Reason)
		if r.WorkerID == "" || r.ReportedAt.IsZero() {
			t.Errorf("refusal %+v lacks worker_id or reported_at", r)
		}
	}
	want := []string{
		"zz-a-" + suffix + "|pvc_rejected|PVC rejected",
		"zz-b-" + suffix + "|size_refused|size above ceiling",
	}
	if strings.Join(mine, ",") != strings.Join(want, ",") {
		t.Fatalf("refusals for %s = %v, want %v (sorted by name; stale and non-refusal absent)", testSizeName, mine, want)
	}
}
