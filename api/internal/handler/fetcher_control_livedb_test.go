package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// fetcher_control_livedb_test.go pins PRD #1906 M3's fetcher control routes and the owner
// read through the REAL h.Routes() router against a real Postgres: the service-token
// mount, the credential rules, admission under concurrency, the idempotent log write, the
// fail-closed log failure and the stale sweep. Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway Postgres (./e2e/run-store-it.sh).

// fcEnv is one router over the shared database, with its own fetch caps (they are instance
// settings, so each cap scenario builds its own handler).
type fcEnv struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	router  http.Handler
	tls     http.Handler
	svcTok  string
	caps    *settings.Cache
	userID  uuid.UUID
	repoID  uuid.UUID
	profile uuid.UUID
	pname   string
	iid     int64
}

// fcCaps is a fetch-cap override set (settings keys to values).
type fcCaps map[string]string

func newFCEnv(t *testing.T, caps fcCaps, withToken bool) *fcEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	rows := make([]store.AppSetting, 0, len(caps))
	for k, v := range caps {
		rows = append(rows, store.AppSetting{Key: k, Value: v})
	}
	e := &fcEnv{t: t, ctx: ctx, pool: pool, caps: settings.New(&settingsStore{rows: rows}, time.Minute), iid: 1}
	// The service token is assembled at runtime from a fresh uuid: not a token-shaped literal.
	e.svcTok = "fetcher-svc-" + uuid.NewString()
	cfg := config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}
	if withToken {
		sum := sha256.Sum256([]byte(e.svcTok))
		cfg.FetcherTokenSHA256 = sum[:]
	}
	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{pool: pool, q: q, box: box, cfg: cfg, wsvc: workersvc.New(q, box, workersvc.Params{}), settings: e.caps, hub: hub.New()}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim)
	e.tls = h.WorkerRoutes(lim)

	e.userID = cliSeedUser(t, pool, false)
	connID := uuid.New()
	e.repoID = uuid.New()
	e.profile = uuid.New()
	e.pname = "fc-" + strings.ReplaceAll(e.profile.String(), "-", "")[:20]
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, e.userID, []byte{0x1})
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/fc', 'main', true)`, e.repoID, connID, "g/fc-"+e.repoID.String())
	e.exec(`INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com,*.vendor.example}')`, e.profile, e.pname)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM runs WHERE user_id = $1`, e.userID)
		_, _ = pool.Exec(ctx, `DELETE FROM egress_profiles WHERE id = $1`, e.profile)
	})
	return e
}

func (e *fcEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

// boundRun seeds a profile-bound issue run in status at claim generation runGen, with a
// credential minted at credGen and a claim-time snapshot, and returns the run and the
// plaintext credential.
func (e *fcEnv) boundRun(status string, runGen, credGen int64) (uuid.UUID, string) {
	e.t.Helper()
	id := uuid.New()
	e.iid++
	snap := fmt.Sprintf(`{"profile":%q,"entries":["docs.example.com","*.vendor.example"]}`, e.pname)
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
	                          claim_generation, egress_profile_id, egress_snapshot)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, $8::jsonb)`,
		id, e.userID, e.repoID, e.iid, status, runGen, e.profile, snap)
	tok, hash, err := fetchctl.GenerateCredential()
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO run_fetch_credentials (run_id, token_hash, claim_generation) VALUES ($1, $2, $3)`, id, hash, credGen)
	return id, tok
}

func (e *fcEnv) post(router http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func beginBody(cred string) string {
	b, _ := json.Marshal(apitypes.FetcherBeginRequest{Credential: cred, URL: "https://docs.example.com/a"})
	return string(b)
}

func (e *fcEnv) begin(cred string) *httptest.ResponseRecorder {
	return e.post(e.router, fetchctl.BeginPath, e.svcTok, beginBody(cred))
}

func (e *fcEnv) mustBegin(cred string) apitypes.FetcherBeginResponse {
	e.t.Helper()
	rec := e.begin(cred)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("begin = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var out apitypes.FetcherBeginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode begin: %v", err)
	}
	return out
}

func completeReq(cred, resID string) apitypes.FetcherCompleteRequest {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return apitypes.FetcherCompleteRequest{
		Credential: cred, ReservationID: resID, URL: "https://docs.example.com/a", FinalURL: "https://docs.example.com/a",
		Verdict: fetchctl.VerdictAllowed, HTTPStatus: 200, ContentType: "text/html", Bytes: 100,
		SHA256: strings.Repeat("ab", 32), StartedAt: ts, FinishedAt: ts.Add(time.Second),
	}
}

func (e *fcEnv) complete(r apitypes.FetcherCompleteRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(r)
	return e.post(e.router, fetchctl.CompletePath, e.svcTok, string(b))
}

type fcCounters struct{ reserved, used, files, inflight, attempts int64 }

func (e *fcEnv) counters(runID uuid.UUID) fcCounters {
	e.t.Helper()
	var c fcCounters
	if err := e.pool.QueryRow(e.ctx, `SELECT reserved_bytes, used_bytes, files, inflight, attempts FROM run_fetch_credentials WHERE run_id = $1`,
		runID).Scan(&c.reserved, &c.used, &c.files, &c.inflight, &c.attempts); err != nil {
		e.t.Fatalf("read counters: %v", err)
	}
	return c
}

func (e *fcEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func controlReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var dto apitypes.FetcherControlErrorDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	return dto.Reason
}

// TestFetcherRoutesMountAndServiceAuthLiveDB: unset UZI_FETCHER_TOKEN_SHA256 leaves both
// routes unmounted on both listeners (404); configured, they answer on both, and the
// service Bearer is required (missing, empty and wrong are 401; a run credential or a
// user's CLI token is not a service token).
func TestFetcherRoutesMountAndServiceAuthLiveDB(t *testing.T) {
	off := newFCEnv(t, nil, false)
	_, cred := off.boundRun("running", 1, 1)
	for _, p := range []string{fetchctl.BeginPath, fetchctl.CompletePath} {
		for name, r := range map[string]http.Handler{"plain": off.router, "tls": off.tls} {
			if rec := off.post(r, p, off.svcTok, beginBody(cred)); rec.Code != http.StatusNotFound {
				t.Errorf("token unset, %s %s = %d, want 404 (not mounted)", name, p, rec.Code)
			}
		}
	}

	on := newFCEnv(t, nil, true)
	run, cred := on.boundRun("running", 1, 1)
	userTok := cliMintToken(t, on.pool, on.userID, clitoken.ScopeUser)
	for _, p := range []string{fetchctl.BeginPath, fetchctl.CompletePath} {
		for name, bearer := range map[string]string{"missing": "", "wrong": on.svcTok + "x", "run credential": cred, "user uzc_": userTok} {
			if rec := on.post(on.router, p, bearer, beginBody(cred)); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s bearer on %s = %d, want 401", name, p, rec.Code)
			}
		}
		// An empty Bearer (the scheme with nothing after it) is refused before hashing.
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(beginBody(cred)))
		req.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		on.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("empty Bearer on %s = %d, want 401", p, rec.Code)
		}
	}
	if c := on.counters(run); c.attempts != 0 || c.inflight != 0 {
		t.Fatalf("refused service auth still touched the run: %+v", c)
	}
	if rec := on.post(on.tls, fetchctl.BeginPath, on.svcTok, beginBody(cred)); rec.Code != http.StatusOK {
		t.Fatalf("TLS listener begin = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
}

// TestFetcherBeginCredentialRulesLiveDB: a live credential is admitted with the run's
// claim-time snapshot and the per-file cap; revoked, unknown (foreign), stale-generation,
// empty and non-running credentials are 403 credential_invalid and count no attempt; a
// body naming a run is refused by the strict decode before anything is read.
func TestFetcherBeginCredentialRulesLiveDB(t *testing.T) {
	e := newFCEnv(t, fcCaps{settings.KeyFetchMaxFileBytes: "1000"}, true)

	run, cred := e.boundRun("running", 1, 1)
	// An admin edit of the live profile after the snapshot changes nothing Begin returns.
	e.exec(`UPDATE egress_profiles SET hosts = '{evil.example.com}' WHERE id = $1`, e.profile)
	adm := e.mustBegin(cred)
	if adm.ReservationID == "" || adm.MaxBytes != 1000 {
		t.Fatalf("begin = %+v, want a reservation and max_bytes 1000", adm)
	}
	if got := strings.Join(adm.Entries, ","); got != "docs.example.com,*.vendor.example" {
		t.Fatalf("entries = %q, want the claim-time snapshot, not the edited live list", got)
	}
	if c := e.counters(run); c != (fcCounters{reserved: 1000, files: 1, inflight: 1, attempts: 1}) {
		t.Fatalf("counters after one admit = %+v", c)
	}

	revokedRun, revoked := e.boundRun("running", 1, 1)
	e.exec(`UPDATE run_fetch_credentials SET revoked_at = now() WHERE run_id = $1`, revokedRun)
	staleRun, stale := e.boundRun("running", 2, 1)
	claimedRun, claimed := e.boundRun("claimed", 1, 1)
	parkedRun, parked := e.boundRun("running", 1, 1)
	e.exec(`UPDATE runs SET status = 'limit_wait' WHERE id = $1`, parkedRun) // the trigger revokes it too
	doneRun, done := e.boundRun("running", 1, 1)
	e.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, doneRun)
	foreign, _, _ := fetchctl.GenerateCredential()

	for name, c := range map[string]string{
		"revoked": revoked, "stale generation": stale, "claimed (not running)": claimed,
		"parked": parked, "terminal": done, "foreign": foreign, "empty": "",
	} {
		rec := e.begin(c)
		if rec.Code != http.StatusForbidden || controlReason(t, rec) != fetchctl.ReasonCredentialInvalid {
			t.Errorf("%s credential: begin = %d %s, want 403 credential_invalid", name, rec.Code, rec.Body.String())
		}
	}
	for _, r := range []uuid.UUID{revokedRun, staleRun, claimedRun, parkedRun, doneRun} {
		if c := e.counters(r); c.attempts != 0 || c.inflight != 0 {
			t.Errorf("a refused credential touched run %s: %+v", r, c)
		}
	}

	// A body that names a run (or anything but credential and url) is a 400: the run comes
	// only from the credential.
	other, _ := e.boundRun("running", 1, 1)
	for _, body := range []string{
		fmt.Sprintf(`{"credential":%q,"url":"https://docs.example.com/a","run_id":%q}`, cred, other),
		fmt.Sprintf(`{"credential":%q,"url":"https://docs.example.com/a","Run_ID":%q}`, cred, other),
		fmt.Sprintf(`{"credential":%q,"url":"https://docs.example.com/a"}{"run_id":%q}`, cred, other),
	} {
		if rec := e.post(e.router, fetchctl.BeginPath, e.svcTok, body); rec.Code != http.StatusBadRequest {
			t.Errorf("begin with a run field = %d, want 400\nbody: %s", rec.Code, body)
		}
	}
	if c := e.counters(run); c.attempts != 1 {
		t.Fatalf("a refused body counted an attempt on the credential's run: %+v", c)
	}
	if c := e.counters(other); c.attempts != 0 {
		t.Fatalf("a body naming a run touched that run: %+v", c)
	}
}

// TestFetcherCompleteLiveDB: Complete writes exactly one escaped source-log row, keyed from
// the credential; a second Complete for the same reservation is a 2xx no-op; another run's
// credential cannot complete the reservation; a refused attempt refunds its file slot; an
// oversize or malformed record and a body naming a run are refused; a failed log write is
// non-2xx and leaves nothing behind.
func TestFetcherCompleteLiveDB(t *testing.T) {
	e := newFCEnv(t, fcCaps{settings.KeyFetchMaxFileBytes: "1000"}, true)
	run, cred := e.boundRun("running", 1, 1)
	otherRun, otherCred := e.boundRun("running", 1, 1)

	adm := e.mustBegin(cred)
	hostile := completeReq(cred, adm.ReservationID)
	hostile.URL = "https://docs.example.com/\x1b[2Ja\u202Eb\\c"
	hostile.FinalURL = "https://docs.example.com/\u200Bz"
	hostile.ContentType = "text/html\x07"

	// Another run's credential cannot complete this reservation.
	stolen := hostile
	stolen.Credential = otherCred
	if rec := e.complete(stolen); rec.Code != http.StatusNotFound {
		t.Fatalf("complete with another run's credential = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := e.count(`SELECT count(*) FROM run_fetches WHERE run_id IN ($1, $2)`, run, otherRun); n != 0 {
		t.Fatalf("a refused complete wrote %d rows", n)
	}
	unknown := hostile
	unknown.Credential, _, _ = fetchctl.GenerateCredential()
	if rec := e.complete(unknown); rec.Code != http.StatusForbidden {
		t.Fatalf("complete with an unknown credential = %d, want 403", rec.Code)
	}
	// A body naming a run is a 400.
	b, _ := json.Marshal(hostile)
	smuggled := strings.TrimSuffix(string(b), "}") + fmt.Sprintf(`,"run_id":%q}`, otherRun)
	if rec := e.post(e.router, fetchctl.CompletePath, e.svcTok, smuggled); rec.Code != http.StatusBadRequest {
		t.Fatalf("complete with a run_id field = %d, want 400", rec.Code)
	}
	over := hostile
	over.Bytes = 1001
	if rec := e.complete(over); rec.Code != http.StatusBadRequest {
		t.Fatalf("complete over the reservation = %d, want 400", rec.Code)
	}

	// A failed log write: nothing is written, the reservation stays open, non-2xx.
	fn := "fc_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	e.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected log failure'; END; $$ LANGUAGE plpgsql`, fn))
	e.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON run_fetches FOR EACH ROW WHEN (NEW.run_id = '%s') EXECUTE FUNCTION %s()`, fn, run, fn))
	rec := e.complete(hostile)
	e.exec(fmt.Sprintf(`DROP TRIGGER %s ON run_fetches`, fn))
	e.exec(fmt.Sprintf(`DROP FUNCTION %s()`, fn))
	if rec.Code >= 200 && rec.Code <= 299 {
		t.Fatalf("complete with a failing log write = %d, want non-2xx (the fetcher then refuses)", rec.Code)
	}
	if c := e.counters(run); c != (fcCounters{reserved: 1000, files: 1, inflight: 1, attempts: 1}) {
		t.Fatalf("a failed log write moved the counters: %+v", c)
	}

	// The real write.
	if rec := e.complete(hostile); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	want := fcCounters{reserved: 0, used: 100, files: 1, inflight: 0, attempts: 1}
	if c := e.counters(run); c != want {
		t.Fatalf("counters after complete = %+v, want %+v", c, want)
	}
	var url, finalURL, ctype, verdict, sha string
	var rowRun uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT run_id, url, final_url, content_type, verdict, sha256 FROM run_fetches WHERE reservation_id = $1`,
		adm.ReservationID).Scan(&rowRun, &url, &finalURL, &ctype, &verdict, &sha); err != nil {
		t.Fatalf("read log row: %v", err)
	}
	if rowRun != run || verdict != "allowed" || sha != strings.Repeat("ab", 32) {
		t.Fatalf("row = run %s verdict %s sha %s", rowRun, verdict, sha)
	}
	if url != `https://docs.example.com/\u{001B}[2Ja\u{202E}b\\c` || finalURL != `https://docs.example.com/\u{200B}z` || ctype != `text/html\u{0007}` {
		t.Fatalf("stored text not escaped: url=%q final=%q type=%q", url, finalURL, ctype)
	}

	// Idempotent: the same Complete again (even with different numbers) is a 2xx no-op.
	again := hostile
	again.Bytes = 5
	if rec := e.complete(again); rec.Code != http.StatusNoContent {
		t.Fatalf("second complete = %d, want 204", rec.Code)
	}
	if n := e.count(`SELECT count(*) FROM run_fetches WHERE reservation_id = $1`, adm.ReservationID); n != 1 {
		t.Fatalf("rows for the reservation = %d, want 1", n)
	}
	if c := e.counters(run); c != want {
		t.Fatalf("a second complete moved the counters: %+v, want %+v", c, want)
	}

	// A refused attempt refunds its file slot and returns nothing.
	adm2 := e.mustBegin(cred)
	ref := completeReq(cred, adm2.ReservationID)
	ref.Verdict, ref.Reason, ref.SHA256, ref.Bytes, ref.HTTPStatus, ref.ContentType = fetchctl.VerdictRefused, "off_list", "", 0, 0, ""
	if rec := e.complete(ref); rec.Code != http.StatusNoContent {
		t.Fatalf("refused complete = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	if c := e.counters(run); c != (fcCounters{used: 100, files: 1, attempts: 2}) {
		t.Fatalf("counters after a refused attempt = %+v", c)
	}
}

// parallelBegins fires n Begins for cred at once through the router and returns how many
// were admitted and the refusal reasons of the rest.
func (e *fcEnv) parallelBegins(cred string, n int) (int, map[string]int) {
	e.t.Helper()
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		ok      int
		reasons = map[string]int{}
		start   = make(chan struct{})
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := e.begin(cred)
			mu.Lock()
			defer mu.Unlock()
			switch rec.Code {
			case http.StatusOK:
				ok++
			case http.StatusTooManyRequests:
				var dto apitypes.FetcherControlErrorDTO
				_ = json.Unmarshal(rec.Body.Bytes(), &dto)
				reasons[dto.Reason]++
			default:
				reasons[fmt.Sprintf("status %d", rec.Code)]++
			}
		}()
	}
	close(start)
	wg.Wait()
	return ok, reasons
}

// TestFetcherAdmissionParallelLiveDB: parallel Begins from an empty run under READ
// COMMITTED can never jointly exceed a cap: each scenario tightens one cap and fires more
// requests than it allows at once; exactly the cap is admitted and every other request is
// refused with that cap's reason. Every request counts as an attempt, admitted or not.
func TestFetcherAdmissionParallelLiveDB(t *testing.T) {
	const n = 24
	loose := fcCaps{
		settings.KeyFetchMaxFileBytes: "10", settings.KeyFetchMaxRunBytes: "100000",
		settings.KeyFetchMaxRunFiles: "10000", settings.KeyFetchMaxConcurrentRun: "32",
		settings.KeyFetchMaxRunAttempts: "100000",
	}
	with := func(k, v string) fcCaps {
		c := fcCaps{}
		for kk, vv := range loose {
			c[kk] = vv
		}
		c[k] = v
		return c
	}
	for _, sc := range []struct {
		name   string
		caps   fcCaps
		want   int
		reason string
	}{
		{"run bytes", with(settings.KeyFetchMaxRunBytes, "55"), 5, fetchctl.AdmissionRunBytes},
		{"run files", with(settings.KeyFetchMaxRunFiles, "3"), 3, fetchctl.AdmissionRunFiles},
		{"concurrency", with(settings.KeyFetchMaxConcurrentRun, "4"), 4, fetchctl.AdmissionConcurrency},
		{"attempts", with(settings.KeyFetchMaxRunAttempts, "6"), 6, fetchctl.AdmissionAttempts},
	} {
		t.Run(sc.name, func(t *testing.T) {
			e := newFCEnv(t, sc.caps, true)
			run, cred := e.boundRun("running", 1, 1)
			ok, reasons := e.parallelBegins(cred, n)
			if ok != sc.want || reasons[sc.reason] != n-sc.want || len(reasons) != 1 {
				t.Fatalf("admitted %d, refusals %v; want exactly %d admitted and %d refused %q", ok, reasons, sc.want, n-sc.want, sc.reason)
			}
			c := e.counters(run)
			if c.inflight != int64(sc.want) || c.files != int64(sc.want) || c.reserved != int64(10*sc.want) {
				t.Fatalf("counters %+v disagree with %d admits", c, sc.want)
			}
			if n := e.count(`SELECT count(*) FROM run_fetch_reservations WHERE run_id = $1`, run); n != sc.want {
				t.Fatalf("reservation rows = %d, want %d", n, sc.want)
			}
			wantAttempts := int64(n)
			if sc.reason == fetchctl.AdmissionAttempts {
				wantAttempts = int64(sc.want)
			}
			if c.attempts != wantAttempts {
				t.Fatalf("attempts = %d, want %d (a refusal by a byte/file/concurrency cap still counts)", c.attempts, wantAttempts)
			}
		})
	}
}

// TestFetchReservationSweepLiveDB: the fetch_reservations_stale pass releases a reservation
// older than the bound (bytes and the concurrency slot back to the run; the file slot stays
// counted) and leaves a fresh one alone; a late Complete for the swept reservation still logs
// the attempt without releasing anything twice.
func TestFetchReservationSweepLiveDB(t *testing.T) {
	e := newFCEnv(t, fcCaps{settings.KeyFetchMaxFileBytes: "1000"}, true)
	run, cred := e.boundRun("running", 1, 1)
	stale := e.mustBegin(cred)
	fresh := e.mustBegin(cred)
	e.exec(`UPDATE run_fetch_reservations SET created_at = now() - interval '1 hour' WHERE id = $1`, stale.ReservationID)

	n, err := fetchctl.New(e.pool, e.caps).SweepStale(e.ctx)
	if err != nil || n < 1 {
		t.Fatalf("SweepStale = %d, %v; want at least the stale reservation", n, err)
	}
	if c := e.counters(run); c != (fcCounters{reserved: 1000, files: 2, inflight: 1, attempts: 2}) {
		t.Fatalf("after the sweep = %+v, want the stale reservation's bytes and slot released", c)
	}
	var settled bool
	if err := e.pool.QueryRow(e.ctx, `SELECT settled FROM run_fetch_reservations WHERE id = $1`, fresh.ReservationID).Scan(&settled); err != nil || settled {
		t.Fatalf("fresh reservation settled=%v err=%v, want untouched", settled, err)
	}
	if rec := e.complete(completeReq(cred, stale.ReservationID)); rec.Code != http.StatusNoContent {
		t.Fatalf("late complete = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	if c := e.counters(run); c != (fcCounters{reserved: 1000, used: 100, files: 2, inflight: 1, attempts: 2}) {
		t.Fatalf("after the late complete = %+v, want used bytes added and nothing released twice", c)
	}
	if n := e.count(`SELECT count(*) FROM run_fetches WHERE reservation_id = $1`, stale.ReservationID); n != 1 {
		t.Fatalf("late complete rows = %d, want 1", n)
	}
}

// TestRunFetchesOwnerReadLiveDB: GET /api/runs/{id}/fetches through the real router is
// owner-only: the owner's uzc_ Bearer and session cookie read the log; another user's uzc_
// and an admin's uza_ get 404; no credential is 401.
func TestRunFetchesOwnerReadLiveDB(t *testing.T) {
	e := newFCEnv(t, nil, true)
	run, cred := e.boundRun("running", 1, 1)
	adm := e.mustBegin(cred)
	if rec := e.complete(completeReq(cred, adm.ReservationID)); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d", rec.Code)
	}
	owner := cliMintToken(t, e.pool, e.userID, clitoken.ScopeUser)
	ownerJWT := cliMintJWT(t, e.pool, e.userID)
	other := cliSeedUser(t, e.pool, false)
	otherTok := cliMintToken(t, e.pool, other, clitoken.ScopeUser)
	admin := cliSeedUser(t, e.pool, true)
	adminTok := cliMintToken(t, e.pool, admin, clitoken.ScopeAdminRO)
	path := "/api/runs/" + run.String() + "/fetches"

	read := func(rec *httptest.ResponseRecorder, who string) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET = %d, want 200\nbody: %s", who, rec.Code, rec.Body.String())
		}
		var dto apitypes.RunFetchesDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(dto.Fetches) != 1 || dto.Fetches[0].Verdict != "allowed" || dto.Fetches[0].Bytes != 100 ||
			dto.Fetches[0].SHA256 != strings.Repeat("ab", 32) {
			t.Fatalf("%s read %+v, want the one logged attempt", who, dto.Fetches)
		}
	}
	read(bearerReq(e.router, http.MethodGet, path, owner), "owner uzc_")
	read(cookieReq(t, e.router, http.MethodGet, path, ownerJWT, ""), "owner cookie")
	if rec := bearerReq(e.router, http.MethodGet, path, otherTok); rec.Code != http.StatusNotFound {
		t.Errorf("another user's uzc_ GET = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(e.router, http.MethodGet, path, adminTok); rec.Code != http.StatusNotFound {
		t.Errorf("an admin's uza_ GET = %d, want 404 (owner-only, like the archives)", rec.Code)
	}
	if rec := bearerReq(e.router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential GET = %d, want 401", rec.Code)
	}
	// An unbound run (no fetches) reads as an empty array, not null.
	plain := uuid.New()
	e.iid++
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`, plain, e.userID, e.repoID, e.iid)
	rec := bearerReq(e.router, http.MethodGet, "/api/runs/"+plain.String()+"/fetches", owner)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"fetches":[]`) {
		t.Fatalf("empty log = %d %s, want 200 with an empty array", rec.Code, rec.Body.String())
	}
}
