package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Create-only PUT (If-None-Match: *) on /api/admin/worker-sizes/{name}, over a real database.

const (
	coName  = "zz-co1"
	coRace  = "zz-corace"
	coExtra = "zz-coedit"
)

// cookiePutPrepared is cookieReqPrepared with extra request headers: the request is built on
// the calling goroutine and the closure only serves it, so it is safe on another goroutine.
func cookiePutPrepared(t *testing.T, f *sizeFx, name, body string, hdr map[string]string) func() *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/worker-sizes/"+name, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, f.jwt))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
}

func (f *sizeFx) putIfNoneMatch(t *testing.T, name, body, inm string) *httptest.ResponseRecorder {
	t.Helper()
	return cookiePutPrepared(t, f, name, body, map[string]string{"If-None-Match": inm})()
}

func (f *sizeFx) fingerprint(t *testing.T, name string) string {
	t.Helper()
	var s string
	if err := f.h.pool.QueryRow(context.Background(), `SELECT w::text FROM worker_sizes w WHERE name = $1`, name).Scan(&s); err != nil {
		t.Fatalf("fingerprint %s: %v", name, err)
	}
	return s
}

// waitBlockedByCount polls until at least want backends are blocked by the holder's pid.
func waitBlockedByCount(t *testing.T, f *sizeFx, holderPid int32, want int) {
	t.Helper()
	deadline := time.Now().Add(raceWait)
	for time.Now().Before(deadline) {
		var n int
		if err := f.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND $1::int = ANY(pg_blocking_pids(pid))`, holderPid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d backends were ever blocked by backend pid %d", want, holderPid)
}

func errReason(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var e map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return e["error"], e["reason"]
}

func TestAdminWorkerSizesCreateOnlyRaceLiveDB(t *testing.T) {
	f := newM3Fx(t)
	ctx := context.Background()
	before := f.sizeCount(t)

	// A dedicated connection holds the worker_sizes advisory lock, so both PUTs queue behind it
	// and are released together.
	holder, err := pgx.Connect(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	var pid int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1, 0)`, store.WorkerSizesLockClass); err != nil {
		t.Fatal(err)
	}

	type result struct {
		body string
		rec  *httptest.ResponseRecorder
	}
	bodies := []string{sizeBody(500, 2000, 1024, 2048, 10, 10), sizeBody(750, 3000, 2048, 4096, 12, 8)}
	done := make(chan result, 2)
	for _, b := range bodies {
		serve := cookiePutPrepared(t, f, coRace, b, map[string]string{"If-None-Match": "*"})
		go func() { done <- result{b, serve()} }()
	}
	waitBlockedByCount(t, f, pid, 2)
	var unlocked bool
	if err := holder.QueryRow(ctx, `SELECT pg_advisory_unlock($1, 0)`, store.WorkerSizesLockClass).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("advisory unlock = %v, %v", unlocked, err)
	}

	winnerBody, created, refused := "", 0, 0
	for range bodies {
		r := awaitResult(t, done, "a create-only PUT")
		switch r.rec.Code {
		case http.StatusCreated:
			created++
			winnerBody = r.body
		case http.StatusPreconditionFailed:
			refused++
			if _, reason := errReason(t, r.rec); reason != "size_exists" {
				t.Errorf("412 reason = %q, want size_exists (%s)", reason, r.rec.Body.String())
			}
		default:
			t.Errorf("unexpected status %d %s", r.rec.Code, r.rec.Body.String())
		}
	}
	if created != 1 || refused != 1 {
		t.Fatalf("created=%d refused=%d, want 1 and 1", created, refused)
	}
	// The stored row holds the 201 request's six values, and exactly one row was added.
	var wantReq map[string]int64
	if err := json.Unmarshal([]byte(winnerBody), &wantReq); err != nil {
		t.Fatal(err)
	}
	row := f.size(t, coRace)
	got := map[string]int64{
		"cpu_request_millicores": row.CpuRequestMillicores, "cpu_limit_millicores": row.CpuLimitMillicores,
		"memory_request_mib": row.MemoryRequestMib, "memory_limit_mib": row.MemoryLimitMib,
		"data_gib": row.DataGib, "dind_data_gib": row.DindDataGib,
	}
	if len(wantReq) != 6 {
		t.Fatalf("winner body has %d fields", len(wantReq))
	}
	for k, v := range wantReq {
		if got[k] != v {
			t.Fatalf("stored %s = %d, want %d (winner body %s)", k, got[k], v, winnerBody)
		}
	}
	if !row.UpdatedBy.Valid || row.UpdatedBy.Bytes != f.admin {
		t.Fatalf("updated_by = %+v, want the admin", row.UpdatedBy)
	}
	if n := f.sizeCount(t); n != before+1 {
		t.Fatalf("size count = %d, want %d", n, before+1)
	}
}

func TestAdminWorkerSizesCreateOnlyExistingLiveDB(t *testing.T) {
	f := newM3Fx(t)
	if rec := f.put(t, coName, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{coName, "s"} {
		fp, n := f.fingerprint(t, name), f.sizeCount(t)
		rec := f.putIfNoneMatch(t, name, sizeBody(900, 3000, 2048, 4096, 11, 11), "*")
		msg, reason := errReason(t, rec)
		if rec.Code != http.StatusPreconditionFailed || reason != "size_exists" || msg != "worker size "+name+" already exists" {
			t.Fatalf("create-only on existing %s = %d %s", name, rec.Code, rec.Body.String())
		}
		if f.fingerprint(t, name) != fp || f.sizeCount(t) != n {
			t.Fatalf("a refused create-only changed %s or the table", name)
		}
	}
	// Surrounding whitespace is trimmed.
	if rec := f.putIfNoneMatch(t, coName, sizeBody(900, 3000, 2048, 4096, 11, 11), " * "); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("padded * = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminWorkerSizesCreateOnlyOrdinaryEditLiveDB(t *testing.T) {
	f := newM3Fx(t)
	if rec := f.put(t, coExtra, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.put(t, coExtra, sizeBody(500, 2000, 1024, 4096, 10, 10)); rec.Code != http.StatusOK || f.size(t, coExtra).MemoryLimitMib != 4096 {
		t.Fatalf("unconditional edit = %d %s", rec.Code, rec.Body.String())
	}
	for _, inm := range []string{`"x"`, `*, W/x`, `W/"x"`} {
		rec := f.putIfNoneMatch(t, coExtra, sizeBody(500, 2000, 1024, 3072, 10, 10), inm)
		if rec.Code != http.StatusOK || f.size(t, coExtra).MemoryLimitMib != 3072 {
			t.Fatalf("If-None-Match %s edit = %d %s", inm, rec.Code, rec.Body.String())
		}
	}
	rec := f.putIfNoneMatch(t, coName, sizeBody(500, 2000, 1024, 2048, 10, 10), `"x"`)
	if rec.Code != http.StatusCreated || !f.exists(t, coName) {
		t.Fatalf("If-None-Match \"x\" on absent = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminWorkerSizesCreateOnlyPrecedenceLiveDB(t *testing.T) {
	f := newM3Fx(t)
	ctx := context.Background()
	if rec := f.put(t, coExtra, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d %s", rec.Code, rec.Body.String())
	}

	// Missing fields on an existing name: 400 beats 412.
	rec := f.putIfNoneMatch(t, coExtra, `{"cpu_request_millicores":500}`, "*")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("existing + missing fields = %d %s, want 400", rec.Code, rec.Body.String())
	}
	// A complete body whose values fail workersize.Validate (cpu request above the limit) on an
	// existing name is still 412: the values are not validated before the existence refusal.
	fpExtra := f.fingerprint(t, coExtra)
	rec = f.putIfNoneMatch(t, coExtra, sizeBody(5000, 2000, 1024, 2048, 10, 10), "*")
	if _, reason := errReason(t, rec); rec.Code != http.StatusPreconditionFailed || reason != "size_exists" {
		t.Fatalf("existing + invalid values = %d %s, want 412 size_exists", rec.Code, rec.Body.String())
	}
	if f.fingerprint(t, coExtra) != fpExtra {
		t.Fatal("a refused create-only with invalid values changed the row")
	}
	// A malformed name: 400.
	if rec := f.putIfNoneMatch(t, "Bad_Name", sizeBody(500, 2000, 1024, 2048, 10, 10), "*"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed name = %d, want 400", rec.Code)
	}

	// At the cap an absent name is still refused 409 max_sizes.
	for i := 0; f.sizeCount(t) < maxWorkerSizes; i++ {
		mustExecT(ctx, t, f.h.pool, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
			VALUES ($1, 500, 2000, 1024, 2048, 10, 10, 200 + $2)`, "zz-pad"+string(rune('a'+i)), i)
	}
	rec = f.putIfNoneMatch(t, coName, sizeBody(500, 2000, 1024, 2048, 10, 10), "*")
	if _, reason := errReason(t, rec); rec.Code != http.StatusConflict || reason != "max_sizes" || f.exists(t, coName) {
		t.Fatalf("create-only at the cap = %d %s", rec.Code, rec.Body.String())
	}
	// An existing name at the cap is 412, not a cap refusal.
	if rec := f.putIfNoneMatch(t, coExtra, sizeBody(500, 2000, 1024, 2048, 10, 10), "*"); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("existing at the cap = %d %s, want 412", rec.Code, rec.Body.String())
	}

	// The read-only gate beats 412.
	fp := f.fingerprint(t, coExtra)
	f.noReport(t)
	rec = f.putIfNoneMatch(t, coExtra, sizeBody(500, 2000, 1024, 2048, 10, 10), "*")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), readOnlyNoReport) {
		t.Fatalf("gated create-only = %d %s, want 409 read-only", rec.Code, rec.Body.String())
	}
	if f.fingerprint(t, coExtra) != fp {
		t.Fatal("a gated request changed the row")
	}
}

func TestAdminWorkerSizesCreateOnlyAuthBoundaryLiveDB(t *testing.T) {
	f := newM3Fx(t)
	nonAdmin := cliMintJWT(t, f.h.pool, f.owner)
	path := "/api/admin/worker-sizes/" + coName
	body := sizeBody(500, 2000, 1024, 2048, 10, 10)

	for _, tok := range []string{f.uza, f.uzc} {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-None-Match", "*")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("bearer create-only = %d, want 401", rec.Code)
		}
	}
	// No credentials at all.
	{
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("If-None-Match", "*")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no cookie create-only = %d, want 401", rec.Code)
		}
	}
	// A cookie without the CSRF header.
	{
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
		req.Header.Set("If-None-Match", "*")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("no-CSRF create-only = %d, want 403", rec.Code)
		}
	}
	if f.exists(t, coName) {
		t.Fatal("a refused create-only wrote a row")
	}
	// A non-admin session is refused and writes nothing.
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: nonAdmin}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, nonAdmin))
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin create-only = %d, want 403", rec.Code)
	}
	if f.exists(t, coName) {
		t.Fatal("a refused create-only wrote a row")
	}
	// Control: the admin cookie path with the same body creates the size.
	if rec := cookiePutPrepared(t, f, coName, body, map[string]string{"If-None-Match": "*"})(); rec.Code != http.StatusCreated {
		t.Fatalf("admin create-only = %d %s", rec.Code, rec.Body.String())
	}
}
