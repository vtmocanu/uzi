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
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// testSizeName is an admin-style (non-builtin) size the tests edit, so the shared database's
// shipped sizes stay untouched except where a test says otherwise (and restores them).
const testSizeName = "zz-u5"

type sizeFx struct {
	h      *Handler
	router http.Handler
	admin  uuid.UUID
	jwt    string
	uza    string // admin_ro bearer
	uzc    string // user-scope bearer of the admin (masked to non-admin)
	now    time.Time
	owner  uuid.UUID
}

func newSizeFx(t *testing.T) *sizeFx {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	if err := store.ReconcileBuiltinWorkerSizes(ctx, pool); err != nil {
		t.Fatalf("seed builtin sizes: %v", err)
	}
	f := &sizeFx{h: h, router: router}
	f.admin = cliSeedUser(t, pool, true)
	f.owner = cliSeedUser(t, pool, false)
	f.jwt = cliMintJWT(t, pool, f.admin)
	f.uza = cliMintToken(t, pool, f.admin, clitoken.ScopeAdminRO)
	f.uzc = cliMintToken(t, pool, f.admin, clitoken.ScopeUser)
	f.now = time.Now().UTC().Truncate(time.Second)
	h.now = func() time.Time { return f.now }

	c := builtin.Ceilings{TiersKnown: true}
	c.Enabled = [builtin.TierCount]bool{true, true, false}
	c.Tier[builtin.TierRestricted] = builtin.TierCeiling{CPUMillicores: builtin.Present(4000), MemoryMiB: builtin.Present(32768), PVCGiB: builtin.Present(50)}
	c.Tier[builtin.TierDocker] = builtin.TierCeiling{CPUMillicores: builtin.Present(4000), MemoryMiB: builtin.Present(32768), PVCGiB: builtin.Present(50)}
	h.cfg.WorkerSizeCeilings = c

	mustExecT(ctx, t, pool, `DELETE FROM worker_sizes WHERE name = $1`, testSizeName)
	mustExecT(ctx, t, pool, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
		VALUES ($1, 500, 2000, 1024, 2048, 10, 10, 90)`, testSizeName)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE hosted_size = $1`, testSizeName)
		_, _ = pool.Exec(ctx, `DELETE FROM worker_sizes WHERE name = $1`, testSizeName)
		// Restore any shipped size a test edited.
		for _, s := range builtin.Sizes() {
			_, _ = pool.Exec(ctx, `UPDATE worker_sizes SET cpu_request_millicores=$2, cpu_limit_millicores=$3, memory_request_mib=$4, memory_limit_mib=$5,
				data_gib=$6, dind_data_gib=$7, customized=false, updated_by=NULL WHERE name=$1 AND builtin`,
				s.Name, s.CPURequestMillicores, s.CPULimitMillicores, s.MemoryRequestMiB, s.MemoryLimitMiB, s.DataGiB, s.DinDDataGiB)
		}
	})
	return f
}

// advertise records a controller advertisement received at `at`.
func (f *sizeFx) advertise(t *testing.T, version int32, at time.Time) {
	t.Helper()
	err := f.h.q.UpsertControllerHibernationAdvertisement(context.Background(), store.UpsertControllerHibernationAdvertisementParams{
		AdvertisedAt:        pgtype.Timestamptz{Time: at, Valid: true},
		HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
		PollIntervalSeconds: pgtype.Int4{Int32: 3, Valid: true},
		AdminSizesVersion:   pgtype.Int4{Int32: version, Valid: true},
	})
	if err != nil {
		t.Fatalf("advertise: %v", err)
	}
}

func (f *sizeFx) noReport(t *testing.T) {
	t.Helper()
	mustExecT(context.Background(), t, f.h.pool, `UPDATE controller_report_status SET admin_sizes_version = NULL, admin_sizes_advertised_at = NULL`)
}

func sizeBody(cpuReq, cpuLim, memReq, memLim, data, dind int64) string {
	b, _ := json.Marshal(map[string]int64{
		"cpu_request_millicores": cpuReq, "cpu_limit_millicores": cpuLim,
		"memory_request_mib": memReq, "memory_limit_mib": memLim, "data_gib": data, "dind_data_gib": dind,
	})
	return string(b)
}

func (f *sizeFx) get(t *testing.T) apitypes.AdminWorkerSizesDTO {
	t.Helper()
	rec := cookieReq(t, f.router, http.MethodGet, "/api/admin/worker-sizes", f.jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET worker-sizes = %d %s", rec.Code, rec.Body.String())
	}
	var out apitypes.AdminWorkerSizesDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *sizeFx) size(t *testing.T, name string) store.WorkerSize {
	t.Helper()
	row, err := f.h.q.GetWorkerSize(context.Background(), name)
	if err != nil {
		t.Fatalf("get size %s: %v", name, err)
	}
	return row
}

func (f *sizeFx) put(t *testing.T, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, f.router, http.MethodPut, "/api/admin/worker-sizes/"+name, f.jwt, body)
}

func (f *sizeFx) del(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, f.router, http.MethodDelete, "/api/admin/worker-sizes/"+name, f.jwt, "")
}

func (f *sizeFx) reset(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, f.router, http.MethodPost, "/api/admin/worker-sizes/"+name+"/reset", f.jwt, "")
}

func decodeWrite(t *testing.T, rec *httptest.ResponseRecorder) apitypes.AdminWorkerSizeWriteDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out apitypes.AdminWorkerSizeWriteDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// seedHostedWorker inserts a hosted worker of the given size and returns its id.
func (f *sizeFx) seedHostedWorker(t *testing.T, name, size string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(context.Background(), t, f.h.pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared)
		VALUES($1,$2,$3,$4,'offline','hosted','1.0.0',$5,'base')`, id, f.admin, name, []byte(id.String()), size)
	return id
}

func (f *sizeFx) workerFingerprint(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var fp string
	if err := f.h.pool.QueryRow(context.Background(), `SELECT w::text FROM workers w WHERE id = $1`, id).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	return fp
}

// The gate: no report, a stale report and an unsupporting controller refuse every mutation
// with 409 and the reason, touching neither the size nor any workload; a fresh capable
// report accepts edits again.
func TestAdminWorkerSizesGateLiveDB(t *testing.T) {
	f := newSizeFx(t)
	wid := f.seedHostedWorker(t, "gate-"+uuid.NewString()[:8], testSizeName)
	beforeSize := f.size(t, testSizeName)
	beforeWorker := f.workerFingerprint(t, wid)
	body := sizeBody(500, 2000, 1024, 4096, 10, 10)

	cases := []struct {
		name     string
		arrange  func()
		wantCode string
		wantText string
	}{
		{"no controller report yet", func() { f.noReport(t) }, readOnlyNoReport, "No controller report"},
		{"stale report", func() { f.advertise(t, 1, f.now.Add(-time.Minute)) }, readOnlyStale, "stale"},
		{"fresh report, version 0", func() { f.advertise(t, 0, f.now) }, readOnlyUnsupported, "does not support admin worker sizes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.arrange()
			g := f.get(t)
			if g.Editable || g.ReadOnlyReason == nil || g.ReadOnlyCode == nil || *g.ReadOnlyCode != tc.wantCode ||
				!strings.Contains(*g.ReadOnlyReason, tc.wantText) {
				t.Fatalf("GET editable=%v reason=%v code=%v, want read-only %q", g.Editable, g.ReadOnlyReason, g.ReadOnlyCode, tc.wantCode)
			}
			for _, rec := range []*httptest.ResponseRecorder{f.put(t, testSizeName, body), f.reset(t, "m"), f.put(t, "nope", body), f.del(t, testSizeName)} {
				if rec.Code != http.StatusConflict {
					t.Fatalf("mutation while read-only = %d %s, want 409", rec.Code, rec.Body.String())
				}
				var e map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &e)
				if e["reason"] != tc.wantCode || e["read_only_reason"] == "" || e["error"] == "" {
					t.Fatalf("409 body = %v", e)
				}
			}
			if got := f.size(t, testSizeName); got != beforeSize {
				t.Fatalf("a refused mutation changed the size row: %+v -> %+v", beforeSize, got)
			}
			if got := f.workerFingerprint(t, wid); got != beforeWorker {
				t.Fatal("a refused mutation touched a workload row")
			}
		})
	}

	// Recovery: a fresh capable report accepts edits again, and still touches no workload.
	f.advertise(t, 1, f.now)
	if g := f.get(t); !g.Editable || g.ReadOnlyReason != nil || g.ReadOnlyCode != nil {
		t.Fatalf("after a fresh v1 report: editable=%v reason=%v", g.Editable, g.ReadOnlyReason)
	}
	out := decodeWrite(t, f.put(t, testSizeName, body))
	if out.Size.MemoryLimitMiB != 4096 {
		t.Fatalf("edit after recovery = %+v", out.Size)
	}
	if got := f.workerFingerprint(t, wid); got != beforeWorker {
		t.Fatal("an accepted size edit rewrote a workload row (the roll is the controller's paced drift arm)")
	}
}

func TestAdminWorkerSizesGetShapeLiveDB(t *testing.T) {
	f := newSizeFx(t)
	f.advertise(t, 1, f.now)
	rec := cookieReq(t, f.router, http.MethodGet, "/api/admin/worker-sizes", f.jwt, "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sizes", "ceilings", "editable", "read_only_reason", "advisories", "max_sizes"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response lacks %q: %s", k, rec.Body.String())
		}
	}
	if string(raw["read_only_reason"]) != "null" || string(raw["max_sizes"]) != "16" || string(raw["editable"]) != "true" {
		t.Errorf("read_only_reason=%s max_sizes=%s editable=%s", raw["read_only_reason"], raw["max_sizes"], raw["editable"])
	}
	g := f.get(t)
	byName := map[string]apitypes.AdminWorkerSizeDTO{}
	order := []string{}
	for _, s := range g.Sizes {
		byName[s.Name] = s
		order = append(order, s.Name)
	}
	m := byName["m"]
	if !m.Builtin || m.Customized || m.Shipped == nil || m.CPULimitMillicores != 4000 || m.MemoryLimitMiB != 12288 || m.DataGiB != 25 || *m.Shipped != m.WorkerSizeSpec {
		t.Errorf("builtin m = %+v", m)
	}
	z := byName[testSizeName]
	if z.Builtin || z.Shipped != nil || z.DisplayOrder != 90 {
		t.Errorf("admin size = %+v", z)
	}
	if order[0] != "s" || order[len(order)-1] != testSizeName {
		t.Errorf("not in display order: %v", order)
	}
	c := g.Ceilings
	if c.MemoryLimitMiB == nil || c.MemoryLimitMiB.Value != 32768 || c.MemoryLimitMiB.ChartPath != "workers.limitRange.max.memory" ||
		c.CPULimitMillicores == nil || c.CPULimitMillicores.Value != 4000 ||
		c.DataGiB == nil || c.DataGiB.Value != 50 || c.DataGiB.ChartPath != "workers.limitRange.maxPVCStorage" ||
		c.DinDDataGiB == nil || c.DinDDataGiB.ChartPath != "workers.docker.limitRange.maxPVCStorage" || c.DinDOverridden {
		t.Errorf("ceilings = %+v", c)
	}
	if len(c.EnabledTiers) != 2 {
		t.Errorf("enabled_tiers = %v", c.EnabledTiers)
	}
	// Quota is not configured in this fixture: the advisory is "unavailable", never "fits".
	var unavailable int
	for _, a := range g.Advisories {
		if a.Kind == workersizeKindQuotaUnavailable && a.Tier == "restricted" {
			unavailable++
		}
		if a.Kind == "quota_exceeded" {
			t.Errorf("no quota is known, yet exceeded: %+v", a)
		}
	}
	if unavailable < len(g.Sizes) {
		t.Errorf("quota_unavailable advisories = %d for %d sizes", unavailable, len(g.Sizes))
	}
}

const workersizeKindQuotaUnavailable = "quota_unavailable"

func TestAdminWorkerSizesEditAndRefusalsLiveDB(t *testing.T) {
	f := newSizeFx(t)
	f.advertise(t, 1, f.now)

	// Edit an existing admin size: values change, builtin/customized stay false, actor recorded.
	out := decodeWrite(t, f.put(t, testSizeName, sizeBody(500, 2000, 1024, 4096, 20, 10)))
	if out.Size.MemoryLimitMiB != 4096 || out.Size.DataGiB != 20 || out.Size.Customized || out.Size.Builtin {
		t.Fatalf("edit = %+v", out.Size)
	}
	if len(out.Warnings) != 0 || out.Advisories == nil || out.Warnings == nil {
		t.Errorf("warnings=%v advisories=%v (both must be arrays)", out.Warnings, out.Advisories)
	}
	if got := f.size(t, testSizeName); !got.UpdatedBy.Valid || got.UpdatedBy.Bytes != f.admin {
		t.Errorf("updated_by = %+v, want the admin", got.UpdatedBy)
	}

	// Example 5: lowering a disk is accepted with the warning.
	out = decodeWrite(t, f.put(t, testSizeName, sizeBody(500, 2000, 1024, 4096, 10, 10)))
	if len(out.Warnings) != 1 || out.Warnings[0].Kind != "disk_lowered" || out.Warnings[0].Field != "data_gib" ||
		!strings.Contains(out.Warnings[0].Message, "existing claims keep their size") {
		t.Fatalf("warnings = %+v", out.Warnings)
	}

	// Example 3: a memory limit over the ceiling is refused naming the ceiling and chart path.
	before := f.size(t, testSizeName)
	rec := f.put(t, testSizeName, sizeBody(500, 2000, 1024, 65536, 10, 10))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-ceiling = %d %s", rec.Code, rec.Body.String())
	}
	var refusal struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if got := refusal.Fields["memory_limit_mib"]; got != "memory limit 65536 MiB exceeds the 32768 MiB ceiling (workers.limitRange.max.memory)" {
		t.Fatalf("field error = %q", got)
	}
	if after := f.size(t, testSizeName); after != before {
		t.Fatal("a refused edit changed the row")
	}

	// A missing field is a field error, not a silent zero.
	rec = f.put(t, testSizeName, `{"cpu_request_millicores":500}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"memory_limit_mib":"is required"`) {
		t.Fatalf("missing fields = %d %s", rec.Code, rec.Body.String())
	}
	// A body that is not six integers is refused outright.
	for _, bad := range []string{`{"cpu_request_millicores":"lots"}`, `nonsense`, `{"unknown":1}`} {
		if rec := f.put(t, testSizeName, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", bad, rec.Code)
		}
	}

	// A malformed name is refused outright (400): nothing could be stored under it.
	for _, name := range []string{"Bad_Name", "1x", "a-name-that-is-too-long"} {
		if rec := f.put(t, name, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %q = %d, want 400", name, rec.Code)
		}
	}

	// Reset applies to builtins only.
	if rec := f.reset(t, testSizeName); rec.Code != http.StatusBadRequest {
		t.Errorf("reset of a non-builtin = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := f.reset(t, "nope"); rec.Code != http.StatusNotFound {
		t.Errorf("reset of an absent size = %d, want 404", rec.Code)
	}
}

// Example 1 and 5 on a shipped size: an edit marks it customized, putting the shipped values
// back clears the flag, and reset returns it to the shipped values with the disk warning.
func TestAdminWorkerSizesBuiltinEditResetLiveDB(t *testing.T) {
	f := newSizeFx(t)
	f.advertise(t, 1, f.now)

	out := decodeWrite(t, f.put(t, "m", sizeBody(1000, 4000, 8192, 12288, 40, 20)))
	if !out.Size.Customized || out.Size.DataGiB != 40 || out.Size.Shipped == nil || out.Size.Shipped.DataGiB != 25 {
		t.Fatalf("customized m = %+v", out.Size)
	}
	// Values equal to the shipped ones are not "customized".
	out = decodeWrite(t, f.put(t, "m", sizeBody(1000, 4000, 8192, 12288, 25, 20)))
	if out.Size.Customized {
		t.Fatalf("shipped values saved as customized: %+v", out.Size)
	}
	// Raise memory, then reset: shipped values, customized cleared, updated_by cleared.
	decodeWrite(t, f.put(t, "m", sizeBody(1000, 4000, 8192, 24576, 40, 20)))
	out = decodeWrite(t, f.reset(t, "m"))
	if out.Size.Customized || out.Size.MemoryLimitMiB != 12288 || out.Size.DataGiB != 25 || out.Size.DinDDataGiB != 20 {
		t.Fatalf("reset m = %+v", out.Size)
	}
	if len(out.Warnings) != 1 || out.Warnings[0].Kind != "disk_lowered" {
		t.Errorf("reset lowering 40 -> 25 must warn: %+v", out.Warnings)
	}
	if got := f.size(t, "m"); got.UpdatedBy.Valid {
		t.Errorf("reset kept updated_by: %+v", got.UpdatedBy)
	}

	// A reset the chart ceilings cannot honour is refused with field errors, not saved.
	f.h.cfg.WorkerSizeCeilings.Tier[builtin.TierRestricted].MemoryMiB = builtin.Present(8192)
	decodeWrite(t, f.put(t, "m", sizeBody(1000, 4000, 4096, 8192, 25, 20)))
	if rec := f.reset(t, "m"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "workers.limitRange.max.memory") {
		t.Fatalf("reset above ceiling = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminWorkerSizesAuthBoundaryLiveDB(t *testing.T) {
	f := newSizeFx(t)
	f.advertise(t, 1, f.now)
	nonAdmin := cliMintJWT(t, f.h.pool, f.owner)
	path := "/api/admin/worker-sizes/" + testSizeName
	body := sizeBody(500, 2000, 1024, 4096, 10, 10)
	before := f.size(t, testSizeName)

	// Reads: cookie-only (no CLI-readable route). Any bearer 401s; an admin session reads.
	if rec := bearerReq(f.router, http.MethodGet, "/api/admin/worker-sizes", f.uza); rec.Code != http.StatusUnauthorized {
		t.Errorf("uza_ GET = %d, want 401", rec.Code)
	}
	if rec := bearerReq(f.router, http.MethodGet, "/api/admin/worker-sizes", f.uzc); rec.Code != http.StatusUnauthorized {
		t.Errorf("uzc_ GET = %d, want 401", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, "/api/admin/worker-sizes", f.jwt, ""); rec.Code != http.StatusOK {
		t.Errorf("admin session GET = %d, want 200", rec.Code)
	}
	// A safe GET needs no CSRF header: build the request without one.
	{
		req := httptest.NewRequest(http.MethodGet, "/api/admin/worker-sizes", nil)
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("admin session GET without CSRF = %d %s, want 200", rec.Code, rec.Body.String())
		}
	}
	if rec := bearerReq(f.router, http.MethodGet, "/api/admin/worker-sizes", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, "/api/admin/worker-sizes", nonAdmin, ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin GET = %d, want 403", rec.Code)
	}

	// Writes: cookie-only. A bearer (either scope) 401s; a non-admin session is 403; a cookie
	// without the CSRF header is refused; none of them changes the row.
	for _, tok := range []string{f.uza, f.uzc} {
		if rec := bearerReqBody(f.router, http.MethodPut, path, tok, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("bearer PUT = %d, want 401", rec.Code)
		}
		if rec := bearerReqBody(f.router, http.MethodPost, path+"/reset", tok, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("bearer reset = %d, want 401", rec.Code)
		}
	}
	if rec := cookieReq(t, f.router, http.MethodPut, path, nonAdmin, body); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin PUT = %d, want 403", rec.Code)
	}
	for _, tc := range []struct{ method, p, b string }{{http.MethodPut, path, body}, {http.MethodPost, path + "/reset", ""}} {
		req := httptest.NewRequest(tc.method, tc.p, strings.NewReader(tc.b))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF = %d %s, want 403", tc.method, rec.Code, rec.Body.String())
		}
	}
	if after := f.size(t, testSizeName); after != before {
		t.Fatal("a refused request changed the row")
	}
}

// HostedConfig carries the size list for the provision form and stays cookie-only: a CLI
// bearer token is refused.
func TestHostedConfigSizesAndBearerRefusalLiveDB(t *testing.T) {
	f := newSizeFx(t)
	rec := cookieReq(t, f.router, http.MethodGet, "/api/workers/hosted/config", f.jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("HostedConfig = %d %s", rec.Code, rec.Body.String())
	}
	var cfg struct {
		Sizes []apitypes.HostedConfigSizeDTO `json:"sizes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cfg.Sizes {
		names = append(names, s.Name)
		if s.Name == "m" && (s.CPURequestMillicores != 1000 || s.MemoryLimitMiB != 12288 || s.DataGiB != 25 || s.DinDDataGiB != 20) {
			t.Errorf("m = %+v", s)
		}
	}
	if len(names) < 5 || names[0] != "s" || names[1] != "m" || names[2] != "l" || names[3] != "l-highmem" || names[len(names)-1] != testSizeName {
		t.Errorf("sizes not in display order: %v", names)
	}
	// The raw wire keeps the six integer names flat beside name.
	if !strings.Contains(rec.Body.String(), `"name":"s","cpu_request_millicores":250,"cpu_limit_millicores":1000,"memory_request_mib":2048,"memory_limit_mib":4096,"data_gib":5,"dind_data_gib":20`) {
		t.Errorf("wire shape: %s", rec.Body.String())
	}
	for _, tok := range []string{f.uzc, f.uza} {
		if rec := bearerReq(f.router, http.MethodGet, "/api/workers/hosted/config", tok); rec.Code != http.StatusUnauthorized {
			t.Errorf("HostedConfig over a CLI bearer = %d, want 401", rec.Code)
		}
	}
}

// The worker DTO carries the resolved integers of its size for CLI/TUI display.
func TestWorkerDTOHostedSizeSpecLiveDB(t *testing.T) {
	f := newSizeFx(t)
	known := f.seedHostedWorker(t, "spec-known-"+uuid.NewString()[:6], testSizeName)
	unknown := f.seedHostedWorker(t, "spec-unknown-"+uuid.NewString()[:6], "gone")
	t.Cleanup(func() {
		mustExecT(context.Background(), t, f.h.pool, `DELETE FROM workers WHERE id = ANY($1)`, []uuid.UUID{known, unknown})
	})
	external := uuid.New()
	mustExecT(context.Background(), t, f.h.pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind) VALUES($1,$2,$3,$4,'offline','external')`,
		external, f.admin, "spec-ext-"+uuid.NewString()[:6], []byte(external.String()))
	t.Cleanup(func() { mustExecT(context.Background(), t, f.h.pool, `DELETE FROM workers WHERE id = $1`, external) })

	check := func(label string, dtos []apitypes.WorkerDTO) {
		t.Helper()
		seen := 0
		for _, d := range dtos {
			switch d.ID {
			case known.String():
				seen++
				want := apitypes.WorkerSizeSpec{CPURequestMillicores: 500, CPULimitMillicores: 2000, MemoryRequestMiB: 1024, MemoryLimitMiB: 2048, DataGiB: 10, DinDDataGiB: 10}
				if d.HostedSizeSpec == nil || *d.HostedSizeSpec != want {
					t.Errorf("%s: known size spec = %+v", label, d.HostedSizeSpec)
				}
			case unknown.String(), external.String():
				seen++
				if d.HostedSizeSpec != nil {
					t.Errorf("%s: %s must carry no spec, got %+v", label, d.ID, d.HostedSizeSpec)
				}
			}
		}
		if seen != 3 {
			t.Errorf("%s: saw %d of the 3 seeded workers", label, seen)
		}
	}
	rec := cookieReq(t, f.router, http.MethodGet, "/api/workers", f.jwt, "")
	var list struct {
		Workers []apitypes.WorkerDTO `json:"workers"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("GET /api/workers = %d %s", rec.Code, rec.Body.String())
	}
	check("owner list", list.Workers)
	if strings.Contains(rec.Body.String(), `"hosted_size_spec":null`) {
		t.Error("an absent spec must be omitted, not null")
	}

	rec = bearerReq(f.router, http.MethodGet, "/api/admin/workers", f.uza)
	var adminList struct {
		Workers []apitypes.AdminWorkerDTO `json:"workers"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &adminList) != nil {
		t.Fatalf("GET /api/admin/workers = %d %s", rec.Code, rec.Body.String())
	}
	dtos := make([]apitypes.WorkerDTO, 0, len(adminList.Workers))
	for _, w := range adminList.Workers {
		dtos = append(dtos, w.WorkerDTO)
	}
	check("admin list", dtos)
}
