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

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// PRD #4213 M3: adding and deleting sizes through the admin API, over a real database.

const m3Size = "zz-u9"

// m3Fx is the size fixture plus cleanup of everything M3 tests create.
func newM3Fx(t *testing.T) *sizeFx {
	t.Helper()
	f := newSizeFx(t)
	f.advertise(t, 1, f.now)
	ctx := context.Background()
	clean := func() {
		_, _ = f.h.pool.Exec(ctx, `DELETE FROM worker_sizes WHERE name LIKE 'zz-%' AND name <> $1`, testSizeName)
		_, _ = f.h.pool.Exec(ctx, `DELETE FROM workers WHERE hosted_size LIKE 'zz-%'`)
		_, _ = f.h.pool.Exec(ctx, `DELETE FROM app_settings WHERE key IN ('ephemeral_default_size','hosted_default_size')`)
	}
	clean()
	t.Cleanup(clean)
	return f
}

func (f *sizeFx) sizeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM worker_sizes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *sizeFx) exists(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := f.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM worker_sizes WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

type refusalBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	UsedBy struct {
		Workers  int64    `json:"workers"`
		Settings []string `json:"settings"`
	} `json:"used_by"`
}

func decodeRefusal(t *testing.T, rec *httptest.ResponseRecorder) refusalBody {
	t.Helper()
	var r refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("refusal body %q: %v", rec.Body.String(), err)
	}
	return r
}

func TestAdminWorkerSizesCreateLiveDB(t *testing.T) {
	f := newM3Fx(t)

	rec := f.put(t, m3Size, sizeBody(500, 2000, 1024, 2048, 10, 10))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", rec.Code, rec.Body.String())
	}
	var out struct {
		Size       map[string]any   `json:"size"`
		Warnings   []map[string]any `json:"warnings"`
		Advisories []map[string]any `json:"advisories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Size["name"] != m3Size || out.Size["builtin"] != false || out.Size["customized"] != false ||
		out.Warnings == nil || len(out.Warnings) != 0 || out.Advisories == nil {
		t.Fatalf("create body = %s", rec.Body.String())
	}
	row := f.size(t, m3Size)
	var max int32
	if err := f.h.pool.QueryRow(context.Background(), `SELECT max(display_order) FROM worker_sizes`).Scan(&max); err != nil {
		t.Fatal(err)
	}
	if row.Builtin || row.Customized || row.DisplayOrder != max || row.CpuLimitMillicores != 2000 || !row.UpdatedBy.Valid || row.UpdatedBy.Bytes != f.admin {
		t.Fatalf("stored row = %+v (max display_order %d)", row, max)
	}
	// It is last in the list, and the second PUT of the same name is an EDIT (200), not a duplicate.
	if g := f.get(t); g.Sizes[len(g.Sizes)-1].Name != m3Size {
		t.Errorf("new size is not last: %+v", g.Sizes)
	}
	rec = f.put(t, m3Size, sizeBody(500, 2000, 1024, 4096, 10, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT = %d %s, want 200 (edit)", rec.Code, rec.Body.String())
	}

	// The same validation as an edit: a value over the ceiling is refused with field errors and
	// nothing is created.
	rec = f.put(t, "zz-over", sizeBody(500, 2000, 1024, 65536, 10, 10))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "workers.limitRange.max.memory") {
		t.Fatalf("over-ceiling create = %d %s", rec.Code, rec.Body.String())
	}
	if f.exists(t, "zz-over") {
		t.Fatal("a refused create left a row")
	}
	// A create has no previous row, so lowering a disk cannot warn; a missing field is a field error.
	if rec := f.put(t, "zz-miss", `{"cpu_request_millicores":500}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with missing fields = %d", rec.Code)
	}
}

func TestAdminWorkerSizesCapLiveDB(t *testing.T) {
	f := newM3Fx(t)
	ctx := context.Background()
	for i := 0; f.sizeCount(t) < maxWorkerSizes; i++ {
		mustExecT(ctx, t, f.h.pool, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
			VALUES ($1, 500, 2000, 1024, 2048, 10, 10, 200 + $2)`, "zz-pad"+string(rune('a'+i)), i)
	}
	before := f.sizeCount(t)
	rec := f.put(t, m3Size, sizeBody(500, 2000, 1024, 2048, 10, 10))
	if rec.Code != http.StatusConflict {
		t.Fatalf("add at the cap = %d %s, want 409", rec.Code, rec.Body.String())
	}
	var e map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] != "at most 16 worker sizes" || e["reason"] != "max_sizes" {
		t.Fatalf("cap body = %v", e)
	}
	if f.exists(t, m3Size) || f.sizeCount(t) != before {
		t.Fatal("a refused add changed the table")
	}
	// An edit at the cap is never refused by the count, and neither is a delete; after the
	// delete an add fits again.
	if rec := f.put(t, testSizeName, sizeBody(500, 2000, 1024, 4096, 10, 10)); rec.Code != http.StatusOK {
		t.Fatalf("edit at the cap = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.del(t, "zz-pada"); rec.Code != http.StatusOK {
		t.Fatalf("delete at the cap = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.put(t, m3Size, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusCreated {
		t.Fatalf("add after a delete = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminWorkerSizesDeleteLiveDB(t *testing.T) {
	f := newM3Fx(t)
	ctx := context.Background()

	// Success: the response names the size and the row is gone.
	f.put(t, m3Size, sizeBody(500, 2000, 1024, 2048, 10, 10))
	rec := f.del(t, m3Size)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"deleted":"`+m3Size+`"}` || f.exists(t, m3Size) {
		t.Fatalf("delete = %d %s exists=%v", rec.Code, rec.Body.String(), f.exists(t, m3Size))
	}
	// Absent and malformed names are 404.
	for _, n := range []string{m3Size, "Bad_Name"} {
		if rec := f.del(t, n); rec.Code != http.StatusNotFound {
			t.Errorf("DELETE %q = %d, want 404", n, rec.Code)
		}
	}

	// A builtin is refused.
	rec = f.del(t, "m")
	r := decodeRefusal(t, rec)
	if rec.Code != http.StatusConflict || r.Reason != "builtin" || !strings.Contains(r.Error, "reset it instead") || !f.exists(t, "m") {
		t.Fatalf("delete builtin = %d %s", rec.Code, rec.Body.String())
	}

	// A worker using it refuses, naming the count.
	f.put(t, m3Size, sizeBody(500, 2000, 1024, 2048, 10, 10))
	f.seedHostedWorker(t, "u9-a-"+uuid.NewString()[:6], m3Size)
	f.seedHostedWorker(t, "u9-b-"+uuid.NewString()[:6], m3Size)
	rec = f.del(t, m3Size)
	r = decodeRefusal(t, rec)
	if rec.Code != http.StatusConflict || r.Reason != "in_use" || r.UsedBy.Workers != 2 || len(r.UsedBy.Settings) != 0 || !f.exists(t, m3Size) {
		t.Fatalf("delete in use by workers = %d %s", rec.Code, rec.Body.String())
	}
	mustExecT(ctx, t, f.h.pool, `DELETE FROM workers WHERE hosted_size = $1`, m3Size)

	// Each default setting that names it refuses, naming the key (a padded stored value counts).
	for _, key := range []string{settings.KeyEphemeralDefaultSize, settings.KeyHostedDefaultSize} {
		mustExecT(ctx, t, f.h.pool, `INSERT INTO app_settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, " "+m3Size+" ")
		rec = f.del(t, m3Size)
		r = decodeRefusal(t, rec)
		if rec.Code != http.StatusConflict || r.Reason != "in_use" || len(r.UsedBy.Settings) != 1 || r.UsedBy.Settings[0] != key || !strings.Contains(r.Error, key) {
			t.Fatalf("delete referenced by %s = %d %s", key, rec.Code, rec.Body.String())
		}
		mustExecT(ctx, t, f.h.pool, `DELETE FROM app_settings WHERE key = $1`, key)
	}
	if rec := f.del(t, m3Size); rec.Code != http.StatusOK {
		t.Fatalf("delete once unreferenced = %d %s", rec.Code, rec.Body.String())
	}

	// The gate refuses add and delete exactly like an edit, touching nothing.
	f.put(t, m3Size, sizeBody(500, 2000, 1024, 2048, 10, 10))
	f.noReport(t)
	for _, rec := range []*httptest.ResponseRecorder{f.del(t, m3Size), f.put(t, "zz-gated", sizeBody(500, 2000, 1024, 2048, 10, 10))} {
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), readOnlyNoReport) {
			t.Fatalf("gated mutation = %d %s", rec.Code, rec.Body.String())
		}
	}
	if !f.exists(t, m3Size) || f.exists(t, "zz-gated") {
		t.Fatal("a gated mutation changed the table")
	}
}

func TestAdminWorkerSizesDeleteAuthBoundaryLiveDB(t *testing.T) {
	f := newM3Fx(t)
	nonAdmin := cliMintJWT(t, f.h.pool, f.owner)
	path := "/api/admin/worker-sizes/" + testSizeName
	for _, tok := range []string{f.uza, f.uzc} {
		if rec := bearerReqBody(f.router, http.MethodDelete, path, tok, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("bearer DELETE = %d, want 401", rec.Code)
		}
	}
	if rec := cookieReq(t, f.router, http.MethodDelete, path, nonAdmin, ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin DELETE = %d, want 403", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("DELETE without CSRF = %d, want 403", rec.Code)
	}
	if !f.exists(t, testSizeName) {
		t.Fatal("a refused DELETE removed the row")
	}
}

// The default-size settings may name any EXISTING size: with a row, the PUT stores it and the
// settings reader returns it; without one the PUT is a 400 and nothing is stored.
func TestUpdateSettingsDefaultSizeMembershipLiveDB(t *testing.T) {
	f := newM3Fx(t)
	ctx := context.Background()
	f.h.settings = settings.New(f.h.q, time.Minute)
	put := func(body string) *httptest.ResponseRecorder {
		return cookieReq(t, f.router, http.MethodPut, "/api/admin/settings", f.jwt, body)
	}
	f.put(t, "xs", sizeBody(250, 500, 512, 1024, 5, 5))
	t.Cleanup(func() { _, _ = f.h.pool.Exec(ctx, `DELETE FROM worker_sizes WHERE name = 'xs'`) })

	if rec := put(`{"settings":{"ephemeral_default_size":"xs","hosted_default_size":"xs"}}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT xs with a row = %d %s", rec.Code, rec.Body.String())
	}
	f.h.settings.Invalidate()
	if got, err := f.h.settings.EphemeralDefaultSize(ctx); err != nil || got != "xs" {
		t.Fatalf("EphemeralDefaultSize = %q, %v; want xs", got, err)
	}
	if got, err := f.h.settings.HostedDefaultSize(ctx); err != nil || got != "xs" {
		t.Fatalf("HostedDefaultSize = %q, %v; want xs", got, err)
	}

	// A stored default now refuses the delete; resetting the keys frees it.
	if rec := f.del(t, "xs"); rec.Code != http.StatusConflict {
		t.Fatalf("delete of a default = %d", rec.Code)
	}
	if rec := put(`{"settings":{"ephemeral_default_size":"","hosted_default_size":""}}`); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.del(t, "xs"); rec.Code != http.StatusOK {
		t.Fatalf("delete after reset = %d %s", rec.Code, rec.Body.String())
	}

	// No row: refused with a field error naming the key, nothing stored.
	for _, key := range []string{settings.KeyEphemeralDefaultSize, settings.KeyHostedDefaultSize} {
		rec := put(`{"settings":{"` + key + `":"xs"}}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), key+": unknown worker size") {
			t.Fatalf("PUT %s=xs without a row = %d %s", key, rec.Code, rec.Body.String())
		}
	}
	var n int
	if err := f.h.pool.QueryRow(ctx, `SELECT count(*) FROM app_settings WHERE key IN ('ephemeral_default_size','hosted_default_size') AND value = 'xs'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused PUT stored a value: n=%d err=%v", n, err)
	}
	// A reset ("") needs no row.
	if rec := put(`{"settings":{"hosted_default_size":""}}`); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
}
