package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// These are M1 API-generated wire cases; the actual controller corpus belongs to M2.
// No sleeps or parallel tests: cliLiveDB's migrated database is shared by the sweep.
type capacityLiveHarness struct {
	h                      *Handler
	router                 http.Handler
	pool                   *pgxpool.Pool
	owner                  uuid.UUID
	ownerToken, adminToken string
	now                    time.Time
}

func newCapacityLiveHarness(t *testing.T) *capacityLiveHarness {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	f := &capacityLiveHarness{h: h, router: router, pool: pool, owner: cliSeedUser(t, pool, false)}
	admin := cliSeedUser(t, pool, true)
	f.ownerToken = cliMintToken(t, pool, f.owner, clitoken.ScopeUser)
	f.adminToken = cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	// Other tests leave hosted rows behind. Their reports are stale at this clock,
	// but remain in Health's real fleet denominator; never delete foreign fixtures.
	f.now = time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	h.now = func() time.Time { return f.now }
	h.wsvc.SetClock(h.now)
	h.version, h.cfg.HostedWorkerVersion = "2.0.0", "2.0.0"
	h.startedAt = f.now.Add(-2 * time.Hour)
	return f
}

func (f *capacityLiveHarness) seed(t *testing.T, version *string) uuid.UUID {
	t.Helper()
	id, hash := uuid.New(), uuid.New()
	cliMustExec(t, f.pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared)
		VALUES($1,$2,'capacity-handler',$3,'offline','hosted',$4,'m','base')`, id, f.owner, hash[:], version)
	t.Cleanup(func() { cliMustExec(t, f.pool, "DELETE FROM workers WHERE id=$1", id) })
	return id
}

func capacityWire(id uuid.UUID, reason string) map[string]any {
	return map[string]any{"id": id.String(), "phase": "stuck", "pod_phase": "Pending",
		"blocking_kind": "scheduler_capacity", "blocking_reason": reason, "restart_count": 0}
}

func (f *capacityLiveHarness) post(t *testing.T, kind string, row map[string]any) {
	t.Helper()
	f.postReported(t, kind, row, f.now.Add(365*24*time.Hour))
}

func (f *capacityLiveHarness) postReported(t *testing.T, kind string, row map[string]any, reported time.Time) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"kind": kind, "reported_at": reported,
		"worker_image_tag": "2.0.0", "poll_interval_seconds": 10, "workers": []any{row}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", bytes.NewReader(data)))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("ControllerStatus=%d body=%s", rec.Code, rec.Body)
	}
}

// Decode the complete public DTO, reject unknown keys/trailing JSON, and bound
// response size. Raw per-worker keys additionally distinguish null from omission.
func capacityDecode(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.Len() > 1<<20 {
		t.Fatalf("GET status=%d bytes=%d", rec.Code, rec.Body.Len())
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		t.Fatalf("trailing JSON: %v", err)
	}
}

func (f *capacityLiveHarness) dto(t *testing.T, id uuid.UUID, admin bool) apitypes.WorkerDTO {
	t.Helper()
	path, token := "/api/workers", f.ownerToken
	if admin {
		path = "/api/admin/workers"
		token = f.adminToken
	}
	rec := bearerReq(f.router, http.MethodGet, path, token)
	var dto apitypes.WorkerDTO
	found := false
	if admin {
		var body struct {
			Workers []apitypes.AdminWorkerDTO `json:"workers"`
		}
		capacityDecode(t, rec, &body)
		for _, w := range body.Workers {
			if w.ID == id.String() {
				dto, found = w.WorkerDTO, true
			}
		}
	} else {
		var body struct {
			Workers []apitypes.WorkerDTO `json:"workers"`
		}
		capacityDecode(t, rec, &body)
		for _, w := range body.Workers {
			if w.ID == id.String() {
				dto, found = w, true
			}
		}
	}
	if !found {
		t.Fatalf("worker %s absent from %s", id, path)
	}
	var raw struct {
		Workers []map[string]json.RawMessage `json:"workers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, w := range raw.Workers {
		var wid string
		if err := json.Unmarshal(w["id"], &wid); err != nil {
			t.Fatal(err)
		}
		if wid == id.String() {
			value, exists := w["upgrade_blocking_kind"]
			if !exists || (dto.UpgradeBlockingKind == nil && string(value) != "null") {
				t.Fatalf("blocking kind must be present, null when absent: %s", value)
			}
		}
	}
	return dto
}

func (f *capacityLiveHarness) assertDTO(t *testing.T, id uuid.UUID, status, kind, reason string) {
	t.Helper()
	for _, admin := range []bool{false, true} {
		w := f.dto(t, id, admin)
		if w.UpgradeStatus != status {
			t.Fatalf("admin=%v status=%s want=%s detail=%v", admin, w.UpgradeStatus, status, w.UpgradeDetail)
		}
		if kind == "" {
			if w.UpgradeBlockingKind != nil {
				t.Fatalf("admin=%v exposed kind=%s", admin, *w.UpgradeBlockingKind)
			}
		} else if w.UpgradeBlockingKind == nil || *w.UpgradeBlockingKind != kind {
			t.Fatalf("admin=%v kind=%v", admin, w.UpgradeBlockingKind)
		}
		if reason != "" && (w.UpgradeBlockingReason == nil || *w.UpgradeBlockingReason != reason ||
			w.UpgradeDetail == nil || !strings.Contains(*w.UpgradeDetail, reason)) {
			t.Fatalf("admin=%v lost reason %q: %+v", admin, reason, w)
		}
	}
}

func (f *capacityLiveHarness) attention(t *testing.T, want int) {
	t.Helper()
	var summary workersvc.UpgradeFleetSummary
	capacityDecode(t, bearerReq(f.router, http.MethodGet, "/api/me/workers/upgrade-summary", f.ownerToken), &summary)
	if summary.Attention != want {
		t.Fatalf("attention=%d want=%d", summary.Attention, want)
	}
	// A separately owned administrator must not inherit the owner's attention.
	capacityDecode(t, bearerReq(f.router, http.MethodGet, "/api/me/workers/upgrade-summary", f.adminToken), &summary)
	if summary.Attention != 0 {
		t.Fatalf("foreign owner attention=%d", summary.Attention)
	}
}

func (f *capacityLiveHarness) health(t *testing.T) apitypes.HealthCheckDTO {
	t.Helper()
	var doc apitypes.HealthDocDTO
	capacityDecode(t, bearerReq(f.router, http.MethodGet, "/api/admin/health", f.adminToken), &doc)
	for _, c := range doc.Checks {
		if c.ID == "fleet.roll" {
			return c
		}
	}
	t.Fatal("fleet.roll absent")
	return apitypes.HealthCheckDTO{}
}

func (f *capacityLiveHarness) rawKind(t *testing.T, id uuid.UUID, want string) {
	t.Helper()
	var kind pgtype.Text
	if err := f.pool.QueryRow(context.Background(), "SELECT blocking_kind FROM worker_upgrade_reports WHERE worker_id=$1", id).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind.Valid != (want != "") || kind.String != want {
		t.Fatalf("physical kind=%+v want=%q", kind, want)
	}
}

func TestControllerStatusCapacityFixedCausesLiveDB(t *testing.T) {
	f := newCapacityLiveHarness(t)
	for _, cause := range []string{"cpu", "memory", "cpu and memory"} {
		t.Run(cause, func(t *testing.T) {
			f.now = f.now.Add(6 * time.Second)
			version := "1.0.0"
			id := f.seed(t, &version)
			reason := "scheduler capacity: insufficient " + cause
			f.post(t, "fleet", capacityWire(id, reason))
			f.rawKind(t, id, "scheduler_capacity")
			var receipt, reported, anchor time.Time
			if err := f.pool.QueryRow(context.Background(), `SELECT observed_at,controller_reported_at,upgrading_since
				FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&receipt, &reported, &anchor); err != nil {
				t.Fatal(err)
			}
			if !receipt.Equal(f.now) || !anchor.Equal(f.now) || !reported.Equal(f.now.Add(365*24*time.Hour)) {
				t.Fatalf("receipt=%v reported=%v anchor=%v", receipt, reported, anchor)
			}
			f.assertDTO(t, id, "capacity_blocked", "scheduler_capacity", reason)
			f.attention(t, 1)
			check := f.health(t)
			var hosted int
			if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM workers WHERE kind='hosted'").Scan(&hosted); err != nil {
				t.Fatal(err)
			}
			wantSeverity := "warn"
			if hosted == 1 {
				wantSeverity = "danger"
			}
			if check.Severity != wantSeverity || check.Action == nil || !strings.Contains(*check.Action, "choose a smaller worker") ||
				!strings.Contains(check.Summary, "1 capacity blocked") || !strings.Contains(check.Summary, reason) {
				t.Fatalf("capacity Health=%+v hosted=%d", check, hosted)
			}
			old := f.now.Add(-workersvc.DefaultMaxUpgradingWindow - time.Minute)
			cliMustExec(t, f.pool, "UPDATE worker_upgrade_reports SET upgrading_since=$2 WHERE worker_id=$1", id, old)
			f.now = f.now.Add(6 * time.Second)
			f.post(t, "fleet", capacityWire(id, reason))
			if err := f.pool.QueryRow(context.Background(), "SELECT observed_at,upgrading_since FROM worker_upgrade_reports WHERE worker_id=$1", id).Scan(&receipt, &anchor); err != nil {
				t.Fatal(err)
			}
			if !receipt.Equal(f.now) || !anchor.Equal(old) {
				t.Fatalf("renewed incident anchor: receipt=%v anchor=%v", receipt, anchor)
			}
			f.assertDTO(t, id, "capacity_blocked", "scheduler_capacity", reason)
			f.attention(t, 1)
			if c := f.health(t); c.Action == nil || !strings.Contains(*c.Action, "choose a smaller worker") {
				t.Fatalf("ceiling hid capacity: %+v", c)
			}
			// Controller time remains far in the future, but API receipt expires.
			f.now = f.now.Add(workersvc.DefaultControllerSignalTTL + time.Second)
			f.assertDTO(t, id, "outdated", "", "")
			if c := f.health(t); c.Action != nil && strings.Contains(*c.Action, "choose a smaller worker") {
				t.Fatalf("stale capacity remedy: %+v", c)
			}
		})
	}
}

func TestControllerStatusCapacityTransitionsLiveDB(t *testing.T) {
	f := newCapacityLiveHarness(t)
	version := "1.0.0"
	id := f.seed(t, &version)
	reason := "scheduler capacity: insufficient cpu"
	capacity := capacityWire(id, reason)
	f.post(t, "fleet", capacity)
	f.now = f.now.Add(6 * time.Second)
	ordinary := map[string]any{"id": id.String(), "phase": "stuck", "pod_phase": "Pending",
		"blocking_container": "seed-nix", "blocking_reason": "FailedMount", "restart_count": 3, "last_exit_code": 1}
	f.post(t, "fleet", ordinary)
	f.rawKind(t, id, "")
	f.assertDTO(t, id, "upgrade_failed", "", "FailedMount")
	f.attention(t, 1)
	// A delayed controller report is accepted by API receipt order; no controller
	// timestamp ordering contract exists in M1.
	f.now = f.now.Add(6 * time.Second)
	f.postReported(t, "fleet", capacity, f.now.Add(-time.Hour))
	f.assertDTO(t, id, "capacity_blocked", "scheduler_capacity", reason)
	var restoredReceipt, delayedControllerTime time.Time
	if err := f.pool.QueryRow(context.Background(), "SELECT observed_at,controller_reported_at FROM worker_upgrade_reports WHERE worker_id=$1", id).Scan(&restoredReceipt, &delayedControllerTime); err != nil {
		t.Fatal(err)
	}
	if !restoredReceipt.Equal(f.now) || !delayedControllerTime.Equal(f.now.Add(-time.Hour)) {
		t.Fatalf("delayed report receipt=%v controller=%v", restoredReceipt, delayedControllerTime)
	}
	for _, phase := range []string{"rolling", "settled"} {
		f.now = f.now.Add(6 * time.Second)
		f.post(t, "fleet", capacity)
		f.now = f.now.Add(6 * time.Second)
		f.post(t, "readiness", map[string]any{"id": id.String(), "phase": phase, "pod_phase": "Running", "phase_since": f.now})
		f.rawKind(t, id, "")
		f.assertDTO(t, id, "upgrading", "", "")
	}
	f.now = f.now.Add(6 * time.Second)
	capacity["pacing"] = map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2.0.0"}
	f.post(t, "fleet", capacity)
	pacingReceipt := f.now
	f.now = f.now.Add(6 * time.Second)
	f.post(t, "readiness", map[string]any{"id": id.String(), "phase": "settled", "pod_phase": "Running", "phase_since": f.now})
	f.rawKind(t, id, "")
	var state string
	var at, receipt time.Time
	if err := f.pool.QueryRow(context.Background(), "SELECT pacing->>'state',pacing_observed_at,observed_at FROM worker_upgrade_reports WHERE worker_id=$1", id).Scan(&state, &at, &receipt); err != nil {
		t.Fatal(err)
	}
	if state != "held" || !at.Equal(pacingReceipt) || !receipt.Equal(f.now) {
		t.Fatalf("readiness pacing=%s at=%v receipt=%v", state, at, receipt)
	}
	f.assertDTO(t, id, "upgrading", "", "")
	if c := f.health(t); !strings.Contains(c.Summary, "held") {
		t.Fatalf("readiness erased pacing: %+v", c)
	}
}

func TestControllerStatusCapacityLegacyRollbackForwardLiveDB(t *testing.T) {
	f := newCapacityLiveHarness(t)
	version := "1.0.0"
	id := f.seed(t, &version)
	f.post(t, "fleet", capacityWire(id, "scheduler capacity: insufficient memory"))
	f.assertDTO(t, id, "capacity_blocked", "scheduler_capacity", "scheduler capacity: insufficient memory")
	f.now = f.now.Add(6 * time.Second)
	legacy, err := os.ReadFile("../store/testdata/worker_roll_health_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Exact frozen pre-change positional mapping, not a new upsert with nil cause.
	result, err := f.pool.Exec(context.Background(), string(legacy),
		"stuck", f.now, "repo:v2", "Pending", "seed-nix", "FailedMount", int32(3), int32(1),
		f.now.Add(time.Hour), f.now, true, "2.0.0", int32(10), true, nil, id)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("legacy upsert rows=%d err=%v", result.RowsAffected(), err)
	}
	f.rawKind(t, id, "scheduler_capacity")
	var reason, container, phase string
	var receipt time.Time
	var restarts, exit int32
	if err := f.pool.QueryRow(context.Background(), `SELECT phase,blocking_reason,blocking_container,restart_count,last_exit_code,observed_at
		FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&phase, &reason, &container, &restarts, &exit, &receipt); err != nil {
		t.Fatal(err)
	}
	if phase != "stuck" || reason != "FailedMount" || container != "seed-nix" || restarts != 3 || exit != 1 || !receipt.Equal(f.now) {
		t.Fatalf("legacy diagnostics phase=%s reason=%s container=%s restarts=%d exit=%d receipt=%v", phase, reason, container, restarts, exit, receipt)
	}
	f.assertDTO(t, id, "upgrade_failed", "", "FailedMount")
	for _, admin := range []bool{false, true} {
		w := f.dto(t, id, admin)
		if w.UpgradeBlockingContainer == nil || *w.UpgradeBlockingContainer != "seed-nix" || w.UpgradeLastExitCode == nil || *w.UpgradeLastExitCode != 1 {
			t.Fatalf("legacy storage diagnostics missing: %+v", w)
		}
	}
	f.attention(t, 1)
	c := f.health(t)
	if c.Action == nil || strings.Contains(*c.Action, "choose a smaller worker") || !strings.Contains(*c.Action, "storage") || !strings.Contains(c.Summary, "FailedMount") {
		t.Fatalf("retained legacy cause misled Health: %+v", c)
	}
	// Forward ingestion physically clears the cause that old SQL retained.
	f.now = f.now.Add(6 * time.Second)
	f.post(t, "fleet", map[string]any{"id": id.String(), "phase": "stuck", "pod_phase": "Pending", "blocking_reason": "FailedMount"})
	f.rawKind(t, id, "")
	f.assertDTO(t, id, "upgrade_failed", "", "FailedMount")
}

func TestControllerStatusCapacityRegistrationAndHistoryLiveDB(t *testing.T) {
	f := newCapacityLiveHarness(t)
	version := "1.0.0"
	registered := f.seed(t, &version)
	f.post(t, "fleet", capacityWire(registered, "scheduler capacity: insufficient cpu"))
	f.rawKind(t, registered, "scheduler_capacity")
	// Pin the widened registration clear predicate: cause alone must be enough,
	// independently of the incident anchor or any ordinary diagnostics.
	cliMustExec(t, f.pool, "UPDATE worker_upgrade_reports SET upgrading_since=NULL,blocking_reason=NULL WHERE worker_id=$1", registered)
	wkr, err := f.h.q.GetWorkerByID(context.Background(), registered)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/register", strings.NewReader(`{"version":"2.0.0"}`))
	req = req.WithContext(mw.ContextWithWorker(req.Context(), wkr))
	rec := httptest.NewRecorder()
	f.h.WorkerRegister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register=%d body=%s", rec.Code, rec.Body)
	}
	f.rawKind(t, registered, "")
	var cleared bool
	if err := f.pool.QueryRow(context.Background(), "SELECT upgrading_since IS NULL AND blocking_reason IS NULL AND blocking_container IS NULL AND restart_count=0 AND last_exit_code IS NULL FROM worker_upgrade_reports WHERE worker_id=$1", registered).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("registration clear=%v err=%v", cleared, err)
	}
	f.assertDTO(t, registered, "upgrade_failed", "", "")
	f.now = f.now.Add(6 * time.Second)
	f.post(t, "readiness", map[string]any{"id": registered.String(), "phase": "settled", "pod_phase": "Running", "phase_since": f.now})
	f.assertDTO(t, registered, "up_to_date", "", "")
	f.attention(t, 0)

	never := f.seed(t, nil)
	unknown := f.seed(t, nil)
	// Versionless legacy registration is unknown history, not positive absence.
	cliMustExec(t, f.pool, "UPDATE workers SET snapshot_register_nonce='legacy-registration' WHERE id=$1", unknown)
	var absent bool
	if err := f.pool.QueryRow(context.Background(), `SELECT status='offline' AND last_heartbeat_at IS NULL AND version IS NULL
		AND snapshot_register_nonce IS NULL AND registered_pod_uid IS NULL AND roll_heartbeat_at IS NULL FROM workers WHERE id=$1`, never).Scan(&absent); err != nil || !absent {
		t.Fatalf("never-registered control=%v err=%v", absent, err)
	}
	for _, phase := range []string{"rolling", "stuck"} {
		f.now = f.now.Add(6 * time.Second)
		for _, id := range []uuid.UUID{never, unknown, registered} {
			f.post(t, "fleet", map[string]any{"id": id.String(), "phase": phase, "pod_phase": "Pending", "blocking_reason": "FailedMount"})
		}
		if phase == "rolling" {
			f.assertDTO(t, never, "provisioning", "", "")
			f.assertDTO(t, unknown, "upgrading", "", "")
			f.assertDTO(t, registered, "upgrading", "", "")
			f.attention(t, 0)
		} else {
			f.assertDTO(t, never, "provisioning_failed", "", "FailedMount")
			f.assertDTO(t, unknown, "upgrade_failed", "", "FailedMount")
			f.assertDTO(t, registered, "upgrade_failed", "", "FailedMount")
			f.attention(t, 3)
			if c := f.health(t); !strings.Contains(c.Summary, "1 first-provision failures") || c.Action == nil || !strings.Contains(*c.Action, "storage") {
				t.Fatalf("history Health=%+v", c)
			}
		}
	}
}
