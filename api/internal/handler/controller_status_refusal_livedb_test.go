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

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// wantRefusalDTO asserts a refused worker, hibernated or not, reads as a failed provisioning or
// upgrade with the controller's reason (fixtures are offline and never registered).
func wantRefusalDTO(t *testing.T, label, kind string, dto apitypes.WorkerDTO) {
	t.Helper()
	if dto.UpgradeStatus != workersvc.UpgradeStatusProvisioningFailed && dto.UpgradeStatus != workersvc.UpgradeStatusUpgradeFailed {
		t.Errorf("%s: upgrade_status = %q, want a failed status", label, dto.UpgradeStatus)
	}
	want := "refused: " + kind
	if dto.UpgradeBlockingReason == nil || *dto.UpgradeBlockingReason != want {
		t.Errorf("%s: upgrade_blocking_reason = %v, want %q", label, dto.UpgradeBlockingReason, want)
	}
	if dto.UpgradeDetail == nil || !strings.Contains(*dto.UpgradeDetail, want) {
		t.Errorf("%s: upgrade_detail = %v, want it to name %q", label, dto.UpgradeDetail, want)
	}
}

// A controller refusal (size_refused, pvc_rejected) is stored as the worker's blocking kind and
// shown in Admin health, for a normal hosted worker and for a hibernated (pod-less) one, which
// the roll check otherwise excludes. It never reads as capacity_blocked.
func TestControllerStatusRefusalKindsPersistAndReachHealthLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	admin := cliSeedUser(t, pool, true)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	h.SetHealthService(healthsvc.New(healthsvc.Config{Store: h.q, Pool: pool, Settings: h.settings}))

	type fx struct {
		id         uuid.UUID
		name, kind string
		hibernated bool
	}
	suffix := uuid.NewString()[:8]
	var fxs []fx
	for _, kind := range []string{workersvc.BlockingKindSizeRefused, workersvc.BlockingKindPVCRejected} {
		for _, hib := range []bool{false, true} {
			id := uuid.New()
			name := "refused-" + kind + "-" + suffix
			if hib {
				name = "hib-" + name
			}
			state := "none"
			if hib {
				state = "hibernated"
			}
			mustExecT(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared,hibernation_state,hibernation_op_id,hibernated_at)
				VALUES($1,$2,$3,$4,'offline','hosted','1.0.0','m','base',$5::text,CASE WHEN $5::text = 'none' THEN NULL ELSE gen_random_uuid() END,CASE WHEN $5::text = 'none' THEN NULL ELSE now() END)`,
				id, admin, name, []byte(id.String()), state)
			fxs = append(fxs, fx{id: id, name: name, kind: kind, hibernated: hib})
		}
	}
	t.Cleanup(func() {
		for _, f := range fxs {
			_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE id = $1`, f.id)
		}
	})

	entries := make([]any, 0, len(fxs))
	for _, f := range fxs {
		entries = append(entries, map[string]any{
			"id": f.id.String(), "phase": "stuck", "pod_phase": "",
			"blocking_kind": f.kind, "blocking_reason": "refused: " + f.kind,
		})
	}
	data, err := json.Marshal(map[string]any{"kind": "fleet", "reported_at": time.Now().UTC(), "workers": entries})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("controller status = %d %s", rec.Code, rec.Body.String())
	}

	for _, f := range fxs {
		var stored *string
		if err := pool.QueryRow(ctx, `SELECT blocking_kind FROM worker_upgrade_reports WHERE worker_id = $1`, f.id).Scan(&stored); err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if stored == nil || *stored != f.kind {
			t.Errorf("%s: stored blocking_kind = %v, want %s", f.name, stored, f.kind)
		}
	}

	// Readers do not mistake a stored refusal for capacity_blocked.
	all, err := h.q.ListAllWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, row := range all {
		for _, f := range fxs {
			if row.Worker.ID != f.id {
				continue
			}
			checked++
			dto := workerDTOFromAdminRow(row, "dev", "", time.Now(), time.Now())
			if dto.UpgradeStatus == workersvc.UpgradeStatusCapacityBlocked || dto.UpgradeBlockingKind != nil {
				t.Errorf("%s: a stored %s reads as capacity (%s, %v)", f.name, f.kind, dto.UpgradeStatus, dto.UpgradeBlockingKind)
			}
			wantRefusalDTO(t, f.name+" (admin row)", f.kind, dto)
		}
	}
	if checked != len(fxs) {
		t.Fatalf("admin list returned %d of %d workers", checked, len(fxs))
	}

	// The owner's worker list (GET /api/workers) shows the same failed status and the refusal
	// reason for the hibernated worker, not an opaque unknown.
	rec = cookieReq(t, router, http.MethodGet, "/api/workers", cliMintJWT(t, pool, admin), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/workers = %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Workers []apitypes.WorkerDTO `json:"workers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, dto := range listed.Workers {
		for _, f := range fxs {
			if dto.ID == f.id.String() {
				seen++
				wantRefusalDTO(t, f.name+" (GET /api/workers)", f.kind, dto)
			}
		}
	}
	if seen != len(fxs) {
		t.Fatalf("GET /api/workers returned %d of %d fixture workers", seen, len(fxs))
	}

	// Admin health names every refused worker, hibernated ones included.
	rec = bearerReq(router, http.MethodGet, "/api/admin/health", uza)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/health = %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Checks []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Evidence []struct {
				Label string `json:"label"`
				Value string `json:"value"`
			} `json:"evidence"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range doc.Checks {
		if c.ID != "fleet.roll" {
			continue
		}
		found = true
		var refused string
		for _, e := range c.Evidence {
			if e.Label == "Refused workers" {
				refused = e.Value
			}
		}
		for _, f := range fxs {
			if !strings.Contains(refused, f.name) {
				t.Errorf("fleet.roll does not name %s (hibernated=%v): refused=%q summary=%q", f.name, f.hibernated, refused, c.Summary)
			}
		}
		if !strings.Contains(c.Summary, "refused: size above ceiling") || !strings.Contains(c.Summary, "refused: PVC rejected") {
			t.Errorf("summary = %q", c.Summary)
		}
	}
	if !found {
		t.Fatal("no fleet.roll check in the health document")
	}
}
