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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4205 M2 routes through the real mounted router. Skipped without
// UZI_TEST_DATABASE_URL.

type hibSettings struct{ on bool }

func (s *hibSettings) WorkerHibernationEnabled(context.Context) (bool, error) { return s.on, nil }

type hibRouteFx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router http.Handler
	set    *hibSettings
	admin  uuid.UUID
}

func newHibRouteFx(t *testing.T) *hibRouteFx {
	t.Helper()
	h, _, pool := cliLiveDB(t)
	h.cfg.WorkerHostingEnabled = true
	h.cfg.ControllerTokenSHA256 = jointoken.Hash(testControllerToken)
	set := &hibSettings{}
	h.wsvc.SetTxBeginner(pool)
	h.wsvc.SetHibernationSettings(set)
	// The router is rebuilt so the controller routes (mounted only with hosting on) exist.
	lim := mw.NewLimiter(1_000_000, time.Hour, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	admin := cliSeedUser(t, pool, true)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, admin) })
	return &hibRouteFx{t: t, pool: pool, router: router, set: set, admin: admin}
}

func (f *hibRouteFx) worker() (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	id, pod := uuid.New(), uuid.New()
	cliMustExec(f.t, f.pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared, registered_pod_uid)
		VALUES ($1, $2, 'hib', $3, 'online', now(), 'hosted', 'm', 'base', $4)`, id, f.admin, id[:], pod)
	return id, pod
}

func (f *hibRouteFx) advertise() {
	f.t.Helper()
	if err := store.New(f.pool).UpsertControllerHibernationAdvertisement(context.Background(), store.UpsertControllerHibernationAdvertisementParams{
		AdvertisedAt:        pgconv.Time(time.Now()),
		HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
		PollIntervalSeconds: pgtype.Int4{Int32: 5, Valid: true},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hibRouteFx) controller(path, body string, withToken bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if withToken {
		req.Header.Set("Authorization", "Bearer "+testControllerToken)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *hibRouteFx) state(id uuid.UUID) (state string, op *string) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT hibernation_state, hibernation_op_id::text FROM workers WHERE id=$1`, id).Scan(&state, &op); err != nil {
		f.t.Fatal(err)
	}
	return
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestHibernationTransitionConstantsMatchWireLiveDB(t *testing.T) {
	// workersvc cannot import hostedsvc (cycle); the handler converts, so pin the mirror here.
	for local, wire := range map[string]string{
		workersvc.TransitionRefused:     hostedsvc.TransitionRefused,
		workersvc.TransitionScalingDown: hostedsvc.TransitionScalingDown,
		workersvc.TransitionScaledDown:  hostedsvc.TransitionScaledDown,
		workersvc.TransitionReady:       hostedsvc.TransitionReady,
	} {
		if local != wire {
			t.Fatalf("workersvc transition %q != hostedsvc %q", local, wire)
		}
	}
}

func TestAdminHibernationRoutesLiveDB(t *testing.T) {
	f := newHibRouteFx(t)
	nonAdmin := cliSeedUser(t, f.pool, false)
	uza := cliMintToken(t, f.pool, f.admin, clitoken.ScopeAdminRO)
	adminJWT := cliMintJWT(t, f.pool, f.admin)
	id, _ := f.worker()
	hib, wake := "/api/admin/workers/"+id.String()+"/hibernate", "/api/admin/workers/"+id.String()+"/wake"

	// Auth: Bearer 401, non-admin 403, cookie without CSRF rejected; none changes the row.
	for _, p := range []string{hib, wake} {
		if rec := bearerReqBody(f.router, http.MethodPost, p, uza, `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("uza_ POST %s = %d, want 401", p, rec.Code)
		}
		if rec := cookieReq(t, f.router, http.MethodPost, p, cliMintJWT(t, f.pool, nonAdmin), `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin POST %s = %d, want 403", p, rec.Code)
		}
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: adminJWT}) //nolint:gosec // G124: test-only client cookie on an httptest request; response-side attributes are irrelevant to a cookie a test sends.
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("cookie without CSRF POST %s = %d, want 401/403", p, rec.Code)
		}
	}
	if s, _ := f.state(id); s != "none" {
		t.Fatalf("a rejected request changed the worker: %q", s)
	}

	// Refusal: setting off -> 409 with the stable reason code.
	rec := cookieReq(t, f.router, http.MethodPost, hib, adminJWT, `{"reason":"night"}`)
	if rec.Code != http.StatusConflict || decodeBody(t, rec)["reason_code"] != "setting_off" {
		t.Fatalf("setting off = %d %s", rec.Code, rec.Body.String())
	}
	f.set.on = true
	rec = cookieReq(t, f.router, http.MethodPost, hib, adminJWT, `{}`)
	if rec.Code != http.StatusConflict || decodeBody(t, rec)["reason_code"] != "controller_not_advertising" {
		t.Fatalf("no advertisement = %d %s", rec.Code, rec.Body.String())
	}
	f.advertise()

	// Strict body, unknown worker, then the success path.
	if rec = cookieReq(t, f.router, http.MethodPost, hib, adminJWT, `{"reason":"x","actor":"someone"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field = %d, want 400", rec.Code)
	}
	if rec = cookieReq(t, f.router, http.MethodPost, "/api/admin/workers/"+uuid.NewString()+"/hibernate", adminJWT, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worker = %d, want 404", rec.Code)
	}
	rec = cookieReq(t, f.router, http.MethodPost, hib, adminJWT, `{"reason":"night"}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["state"] != "preparing" {
		t.Fatalf("hibernate = %d %s", rec.Code, rec.Body.String())
	}
	var actor uuid.UUID
	var reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT actor_id, reason FROM worker_hibernation_events WHERE worker_id=$1`, id).Scan(&actor, &reason); err != nil || actor != f.admin || reason != "night" {
		t.Fatalf("audit actor=%v reason=%q err=%v", actor, reason, err)
	}
	if rec = cookieReq(t, f.router, http.MethodPost, hib, adminJWT, ``); rec.Code != http.StatusConflict || decodeBody(t, rec)["reason_code"] != "already_hibernating" {
		t.Fatalf("second hibernate = %d %s", rec.Code, rec.Body.String())
	}
	rec = cookieReq(t, f.router, http.MethodPost, wake, adminJWT, ``)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["state"] != "waking" {
		t.Fatalf("wake = %d %s", rec.Code, rec.Body.String())
	}
	_, op1 := f.state(id)
	if rec = cookieReq(t, f.router, http.MethodPost, wake, adminJWT, ``); rec.Code != http.StatusOK {
		t.Fatalf("repeat wake = %d", rec.Code)
	}
	if _, op2 := f.state(id); op1 == nil || op2 == nil || *op1 != *op2 {
		t.Fatalf("repeat wake changed the op: %v -> %v", op1, op2)
	}
	awake, _ := f.worker()
	if rec = cookieReq(t, f.router, http.MethodPost, "/api/admin/workers/"+awake.String()+"/wake", adminJWT, ``); rec.Code != http.StatusConflict {
		t.Fatalf("wake of an awake worker = %d, want 409", rec.Code)
	}
}

func TestControllerHibernationRouteLiveDB(t *testing.T) {
	f := newHibRouteFx(t)
	f.set.on = true
	f.advertise()
	id, pod := f.worker()
	path := "/api/controller/workers/" + id.String() + "/hibernation"
	if rec := f.controller(path, `{}`, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no controller token = %d, want 401", rec.Code)
	}
	adminJWT := cliMintJWT(t, f.pool, f.admin)
	if rec := cookieReq(t, f.router, http.MethodPost, path, adminJWT, `{}`); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("a user session on the controller route = %d, want 401/403", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodPost, "/api/admin/workers/"+id.String()+"/hibernate", adminJWT, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("hibernate = %d %s", rec.Code, rec.Body.String())
	}
	_, op := f.state(id)
	body := func(extra string) string {
		return `{"op_id":"` + *op + `","transition":"scaling_down","deployment_uid":"dep","resource_version":"7","pod_uid":"` + pod.String() + `"` + extra + `}`
	}

	// Strict decode and validation: 400, row unchanged.
	for name, b := range map[string]string{
		"unknown field": body(`,"surprise":1`),
		"missing op":    `{"transition":"scaling_down","deployment_uid":"dep","resource_version":"7","pod_uid":"` + pod.String() + `"}`,
		"missing pod":   `{"op_id":"` + *op + `","transition":"scaling_down","deployment_uid":"dep","resource_version":"7"}`,
		"bad json":      `{`,
	} {
		if rec := f.controller(path, b, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	var sd *time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT hibernation_scaling_down_at FROM workers WHERE id=$1`, id).Scan(&sd); err != nil || sd != nil {
		t.Fatalf("a 400 changed the row: %v %v", sd, err)
	}
	// 409 stale op, 404 unknown worker, 200 ok.
	stale := strings.Replace(body(``), *op, uuid.NewString(), 1)
	if rec := f.controller(path, stale, true); rec.Code != http.StatusConflict {
		t.Fatalf("stale op = %d, want 409", rec.Code)
	}
	if rec := f.controller("/api/controller/workers/"+uuid.NewString()+"/hibernation", body(``), true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worker = %d, want 404", rec.Code)
	}
	rec := f.controller(path, body(``), true)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["state"] != "preparing" {
		t.Fatalf("scaling_down = %d %s", rec.Code, rec.Body.String())
	}
	illegal := strings.Replace(body(``), "scaling_down", "ready", 1)
	if rec := f.controller(path, illegal, true); rec.Code != http.StatusConflict {
		t.Fatalf("ready from preparing = %d, want 409", rec.Code)
	}
}

func TestControllerStatusRecordsHibernationAdvertisementLiveDB(t *testing.T) {
	f := newHibRouteFx(t)
	cliMustExec(t, f.pool, `UPDATE controller_report_status SET hibernation_version=NULL, hibernation_advertised_at=NULL, poll_interval_seconds=NULL`)
	rec := f.controller("/api/controller/status", `{"kind":"fleet","reported_at":"2026-01-01T00:00:00Z","poll_interval_seconds":7,"worker_image_tag":"x","workers":[],"hibernation":1}`, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var v, p int32
	var at time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT hibernation_version, poll_interval_seconds, hibernation_advertised_at FROM controller_report_status WHERE id=1`).Scan(&v, &p, &at); err != nil {
		t.Fatal(err)
	}
	if v != 1 || p != 7 || time.Since(at) > time.Minute {
		t.Fatalf("advertisement = v%d poll=%d at=%v", v, p, at)
	}
	// A controller that does not speak hibernation reports version 0.
	f.controller("/api/controller/status", `{"kind":"fleet","reported_at":"2026-01-01T00:00:00Z","poll_interval_seconds":7,"worker_image_tag":"x","workers":[]}`, true)
	if err := f.pool.QueryRow(context.Background(), `SELECT hibernation_version FROM controller_report_status WHERE id=1`).Scan(&v); err != nil || v != 0 {
		t.Fatalf("version after a legacy report = %d (%v), want 0", v, err)
	}
}
