package handler

import (
	"context"
	"encoding/json"
	"fmt"
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

// A size with more refusals than the per-size cap is still listed (first three by worker name)
// and refusal_totals carries its true fresh count, so a late-sorting size is never cut by an
// early-sorting size's volume. The body is decoded generically so the test names only the wire.
func TestAdminWorkerSizesRefusalTotalsLiveDB(t *testing.T) {
	f := newSizeFx(t)
	ctx := context.Background()
	suffix := f.admin.String()[:8]
	sizeA, sizeV := "zz-a-"+suffix, "zz-v-"+suffix
	for _, name := range []string{sizeA, sizeV} {
		mustExecT(ctx, t, f.h.pool, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
			VALUES ($1, 500, 2000, 1024, 2048, 10, 10, 91)`, name)
	}
	t.Cleanup(func() {
		for _, name := range []string{sizeA, sizeV} {
			_, _ = f.h.pool.Exec(ctx, `DELETE FROM workers WHERE hosted_size = $1`, name)
			_, _ = f.h.pool.Exec(ctx, `DELETE FROM worker_sizes WHERE name = $1`, name)
		}
	})

	fresh := map[string]int{sizeA: 51, testSizeName: 2, sizeV: 4}
	firstNames := map[string][]string{}
	for _, size := range []string{sizeA, testSizeName, sizeV} {
		for i := 0; i < fresh[size]; i++ {
			name := fmt.Sprintf("w%02d-%s-%s", i, size, suffix)
			id := f.seedHostedWorker(t, name, size)
			f.postRefusal(t, id.String(), "size_refused", "size above ceiling")
			if i < 3 {
				firstNames[size] = append(firstNames[size], name)
			}
		}
		// One stale refusal and one stuck non-refusal per size must not be counted.
		stale := f.seedHostedWorker(t, "zstale-"+size+"-"+suffix, size)
		f.postRefusal(t, stale.String(), "pvc_rejected", "old refusal")
		mustExecT(ctx, t, f.h.pool, `UPDATE worker_upgrade_reports SET observed_at = $2 WHERE worker_id = $1`, stale, f.now.Add(-time.Hour))
		other := f.seedHostedWorker(t, "zother-"+size+"-"+suffix, size)
		data, _ := json.Marshal(map[string]any{"kind": "fleet", "reported_at": f.now, "workers": []any{map[string]any{
			"id": other.String(), "phase": "stuck", "pod_phase": "Pending", "blocking_reason": "ImagePullBackOff",
		}}})
		rec := httptest.NewRecorder()
		f.h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("controller status = %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := cookieReq(t, f.router, http.MethodGet, "/api/admin/worker-sizes", f.jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET worker-sizes = %d %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var refusals []struct {
		Size       string `json:"size"`
		WorkerName string `json:"worker_name"`
	}
	if err := json.Unmarshal(raw["refusals"], &refusals); err != nil {
		t.Fatalf("refusals = %s: %v", raw["refusals"], err)
	}
	if len(refusals) > 48 {
		t.Errorf("len(refusals) = %d, want at most 48 (3 per size, 16 sizes)", len(refusals))
	}
	got := map[string][]string{}
	for _, r := range refusals {
		got[r.Size] = append(got[r.Size], r.WorkerName)
	}
	for size := range fresh {
		want := firstNames[size]
		if strings.Join(got[size], ",") != strings.Join(want, ",") {
			t.Errorf("refusals for %s = %v, want %v (first three by worker name)", size, got[size], want)
		}
	}

	if raw["refusal_totals"] == nil {
		t.Fatalf("response lacks refusal_totals: %s", rec.Body.String())
	}
	var totals map[string]int
	if err := json.Unmarshal(raw["refusal_totals"], &totals); err != nil {
		t.Fatalf("refusal_totals = %s: %v", raw["refusal_totals"], err)
	}
	for size, n := range fresh {
		if totals[size] != n {
			t.Errorf("refusal_totals[%s] = %d, want %d (stale and non-refusal excluded)", size, totals[size], n)
		}
	}
	if len(totals) != len(got) {
		t.Errorf("refusal_totals keys %v differ from refusals sizes %v", totals, got)
	}
	for size := range got {
		if _, ok := totals[size]; !ok {
			t.Errorf("size %q in refusals but not in refusal_totals %v", size, totals)
		}
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
