package handler

// Issue #4198 M3 live-DB proofs for the admin run debug LISTING: GET /api/admin/runs with
// any of the keys state, user, since. They drive the REAL h.Routes() router with really
// minted tokens and cookies, and assert the served rows (every status, the excluded kinds,
// ordering with a created_at tie, the owner and since filters, the 500-row cap and the
// truncated flag), the withheld text (report_md null, plan_md and preserved_patch absent),
// the audit record (one per served listing, none on any refusal or validation error) and the
// refusal matrix.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix. No t.Parallel: the audit
// capture swaps the package-level adminRunDebugLogger.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

const (
	adrlReport = "adrl-report-text-must-not-appear"
	adrlPlan   = "adrl-plan-text-must-not-appear"
	adrlPatch  = "adrl-patch-text-must-not-appear"
)

// adrlIssueIID hands out issue numbers that never collide across the tests of one DB.
var adrlIssueIID atomic.Int64

func init() { adrlIssueIID.Store(500000) }

type adrlOwner struct{ user, conn, repo uuid.UUID }

// adrlSeedOwner inserts a non-admin user with one forge connection and one repo.
func adrlSeedOwner(t *testing.T, pool *pgxpool.Pool) adrlOwner {
	t.Helper()
	o := adrlOwner{user: cliSeedUser(t, pool, false), conn: uuid.New(), repo: uuid.New()}
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, o.conn, o.user, []byte{0x1})
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, o.repo, o.conn)
	return o
}

// adrlSeedIssueRun inserts an issue run with the text columns the listing must never serve.
func adrlSeedIssueRun(t *testing.T, pool *pgxpool.Pool, o adrlOwner, id uuid.UUID, status string, createdAt time.Time) {
	t.Helper()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, report_md, plan_md, preserved_patch)
		 VALUES ($1, $2, $3, $4, 'Do X', 'desc', $5, 'issue', $6, $7, $8, $9)`,
		id, o.user, o.repo, adrlIssueIID.Add(1), status, createdAt, adrlReport, adrlPlan, adrlPatch)
}

// adrlIDPair returns two fresh run ids with lo < hi in the order Postgres sorts uuids.
func adrlIDPair() (lo, hi uuid.UUID) {
	a, b := uuid.New(), uuid.New()
	if a.String() > b.String() {
		a, b = b, a
	}
	return a, b
}

// adrlListed decodes a debug listing body into its raw run objects and the truncated flag.
func adrlListed(t *testing.T, body string) (runs []map[string]json.RawMessage, rawRuns string, truncated bool) {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode listing envelope: %v (body %.300s)", err, body)
	}
	if len(env) != 2 {
		t.Errorf("envelope has keys %v, want exactly runs and truncated", env)
	}
	rawRuns = string(env["runs"])
	if err := json.Unmarshal(env["runs"], &runs); err != nil {
		t.Fatalf("decode runs: %v (body %.300s)", err, body)
	}
	if err := json.Unmarshal(env["truncated"], &truncated); err != nil {
		t.Fatalf("decode truncated: %v (body %.300s)", err, body)
	}
	return runs, rawRuns, truncated
}

func adrlRunID(t *testing.T, run map[string]json.RawMessage) string {
	t.Helper()
	var id string
	if err := json.Unmarshal(run["id"], &id); err != nil {
		t.Fatalf("run has no string id: %v (%v)", err, run)
	}
	return id
}

// adrlIDsIn returns the ids of runs that are in the seeded set, in served order.
func adrlIDsIn(t *testing.T, runs []map[string]json.RawMessage, seeded map[string]bool) []string {
	t.Helper()
	var out []string
	for _, r := range runs {
		if id := adrlRunID(t, r); seeded[id] {
			out = append(out, id)
		}
	}
	return out
}

func adrlSameOrder(got []string, want ...uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i].String() {
			return false
		}
	}
	return true
}

// adrlCheckWithheld asserts the summary shape: no plan_md or preserved_patch key, report_md
// present and null, and none of the seeded text anywhere in the body.
func adrlCheckWithheld(t *testing.T, label, body string, runs []map[string]json.RawMessage) {
	t.Helper()
	for _, r := range runs {
		for _, k := range []string{"plan_md", "preserved_patch"} {
			if _, has := r[k]; has {
				t.Errorf("%s: run %s carries key %s", label, adrlRunID(t, r), k)
			}
		}
		raw, has := r["report_md"]
		if !has || string(raw) != "null" {
			t.Errorf("%s: run %s report_md = %s (present=%v), want null", label, adrlRunID(t, r), raw, has)
		}
	}
	for _, text := range []string{adrlReport, adrlPlan, adrlPatch} {
		if strings.Contains(body, text) {
			t.Errorf("%s: body contains withheld text %q", label, text)
		}
	}
}

type adrlListFixture struct {
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool

	admin       uuid.UUID
	adminRO     string
	adminROID   uuid.UUID
	adminUzc    string
	adminJWT    string
	demotedRO   string
	otherUzc    string
	otherRO     string
	revokedRO   string
	expiredRO   string
	storedUser  string
	audit       *adrdAudit
	nonAdminUID uuid.UUID
}

func adrlNewListFixture(t *testing.T) *adrlListFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.cfg.AdminRunDebug = true
	f := &adrlListFixture{h: h, router: router, pool: pool}
	f.admin = cliSeedUser(t, pool, true)
	demoted := cliSeedUser(t, pool, true)
	f.nonAdminUID = cliSeedUser(t, pool, false)
	f.adminRO, f.adminROID = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, nil, false)
	f.adminUzc = cliMintToken(t, pool, f.admin, clitoken.ScopeUser)
	f.adminJWT = cliMintJWT(t, pool, f.admin)
	f.otherUzc = cliMintToken(t, pool, f.nonAdminUID, clitoken.ScopeUser)
	f.otherRO = cliMintToken(t, pool, f.nonAdminUID, clitoken.ScopeAdminRO)
	f.demotedRO = cliMintToken(t, pool, demoted, clitoken.ScopeAdminRO)
	cliMustExec(t, pool, `UPDATE users SET is_admin = false WHERE id = $1`, demoted)
	past := time.Now().Add(-time.Hour)
	f.revokedRO, _ = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, nil, true)
	f.expiredRO, _ = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, &past, false)
	secret, hash, prefix, err := clitoken.Generate(clitoken.ScopeAdminRO)
	if err != nil {
		t.Fatalf("clitoken.Generate: %v", err)
	}
	cliMustExec(t, pool,
		`INSERT INTO cli_tokens (user_id, name, token_hash, token_prefix, scope, expires_at, revoked)
		 VALUES ($1, 'test', $2, $3, 'user', NULL, false)`, f.admin, hash, prefix)
	f.storedUser = secret
	f.audit = adrdCaptureAudit(t)
	return f
}

// get issues one listing request as the live admin_ro token, expects 200 and returns the body.
func (f *adrlListFixture) get(t *testing.T, path string) string {
	t.Helper()
	rec := bearerReq(f.router, http.MethodGet, path, f.adminRO)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin_ro GET %s = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// oneAudit returns the single audit record the previous request must have written.
func (f *adrlListFixture) oneAudit(t *testing.T, label string) map[string]any {
	t.Helper()
	recs := f.audit.take(t)
	if len(recs) != 1 {
		t.Fatalf("%s wrote %d audit records, want 1: %v", label, len(recs), recs)
	}
	rec := recs[0]
	for k, v := range map[string]string{
		"actor_user_id": f.admin.String(), "token_id": f.adminROID.String(), "route": "admin.runs",
	} {
		if rec[k] != v {
			t.Errorf("%s: audit %s = %v, want %s (record %v)", label, k, rec[k], v, rec)
		}
	}
	return rec
}

// TestAdminRunDebugListRunsLiveDB: the all-states listing serves every status of every
// owner, excludes the chat, judge and cross_check kinds, orders newest first with the id as
// tiebreak, honours the owner and (inclusive) since filters, withholds the run text, and
// audits exactly once per served request.
func TestAdminRunDebugListRunsLiveDB(t *testing.T) {
	f := adrlNewListFixture(t)
	pool := f.pool
	a, b := adrlSeedOwner(t, pool), adrlSeedOwner(t, pool)
	// Seeded in the future: the LiveDB sweep shares one database with other packages, whose
	// recent runs could otherwise fill an unfiltered page (500 cap) inside the since window.
	base := time.Now().UTC().Truncate(time.Second).Add(30 * 24 * time.Hour)

	aOld, aCompleted, aFailed, aCancelled := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tieLo, tieHi := adrlIDPair()
	bQueued, bRunning := uuid.New(), uuid.New()
	adrlSeedIssueRun(t, pool, a, aOld, "completed", base.Add(-2*time.Hour))
	adrlSeedIssueRun(t, pool, a, aCompleted, "completed", base)
	adrlSeedIssueRun(t, pool, a, aFailed, "failed", base.Add(10*time.Second))
	adrlSeedIssueRun(t, pool, a, tieLo, "completed", base.Add(20*time.Second))
	adrlSeedIssueRun(t, pool, a, tieHi, "completed", base.Add(20*time.Second))
	adrlSeedIssueRun(t, pool, a, aCancelled, "cancelled", base.Add(30*time.Second))
	adrlSeedIssueRun(t, pool, b, bQueued, "queued", base.Add(5*time.Second))
	adrlSeedIssueRun(t, pool, b, bRunning, "running", base.Add(15*time.Second))

	// The excluded kinds, all newer than every issue run, each with the text columns set.
	chat, judge, cross := uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, created_at, report_md)
		 VALUES ($1, $2, 'chat', 't', 'd', 'completed', $3, $4)`, chat, a.user, base.Add(40*time.Second), adrlReport)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, created_at, target_run_id, report_md)
		 VALUES ($1, $2, 'judge', 't', 'd', 'completed', $3, $4, $5)`, judge, a.user, base.Add(41*time.Second), aCompleted, adrlReport)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, created_at, target_run_id, harness, report_only, budget_wall_seconds, report_md)
		 VALUES ($1, $2, $3, 'cross_check', 't', 'd', 'completed', $4, $5, 'codex', true, 1800, $6)`, cross, a.user, a.repo, base.Add(42*time.Second), aCompleted, adrlReport)

	seeded := map[string]bool{}
	for _, id := range []uuid.UUID{aOld, aCompleted, aFailed, tieLo, tieHi, aCancelled, bQueued, bRunning, chat, judge, cross} {
		seeded[id.String()] = true
	}

	t.Run("all owners, every status, excluded kinds absent, ordered", func(t *testing.T) {
		// since bounds the page to this test's rows without a user filter; aOld is older.
		path := "/api/admin/runs?state=all&since=" + base.Add(-time.Hour).Format(time.RFC3339)
		body := f.get(t, path)
		runs, rawRuns, truncated := adrlListed(t, body)
		if truncated {
			t.Errorf("truncated = true on a small listing")
		}
		if rawRuns == "null" {
			t.Errorf("runs is null")
		}
		got := adrlIDsIn(t, runs, seeded)
		if !adrlSameOrder(got, aCancelled, tieHi, tieLo, bRunning, aFailed, bQueued, aCompleted) {
			t.Errorf("served seeded ids = %v\nwant (newest first, tie by id desc) cancelled=%s tieHi=%s tieLo=%s bRunning=%s failed=%s bQueued=%s completed=%s",
				got, aCancelled, tieHi, tieLo, bRunning, aFailed, bQueued, aCompleted)
		}
		adrlCheckWithheld(t, "unfiltered", body, runs)
		rec := f.oneAudit(t, "unfiltered listing")
		if rec["owner_filter"] != "all" {
			t.Errorf("audit owner_filter = %v, want all", rec["owner_filter"])
		}
		if rec["returned"] != float64(len(runs)) {
			t.Errorf("audit returned = %v, want %d", rec["returned"], len(runs))
		}
	})

	t.Run("user filter with view=full and an offset since", func(t *testing.T) {
		// The owner's own created_at as a +02:00 offset: the bound is inclusive, aOld is cut.
		sinceArg := base.In(time.FixedZone("", 2*3600)).Format(time.RFC3339)
		if !strings.Contains(sinceArg, "+02:00") {
			t.Fatalf("test bug: since %q lacks the +02:00 marker", sinceArg)
		}
		path := "/api/admin/runs?state=all&view=full&user=" + a.user.String() + "&since=" + strings.ReplaceAll(sinceArg, "+", "%2B")
		body := f.get(t, path)
		runs, _, truncated := adrlListed(t, body)
		if truncated {
			t.Errorf("truncated = true")
		}
		var got []string
		for _, r := range runs {
			got = append(got, adrlRunID(t, r))
		}
		if !adrlSameOrder(got, aCancelled, tieHi, tieLo, aFailed, aCompleted) {
			t.Errorf("served ids = %v, want owner A's five eligible runs newest first (cancelled, tieHi, tieLo, failed, completed)", got)
		}
		adrlCheckWithheld(t, "view=full", body, runs)

		rec := f.oneAudit(t, "user+since listing")
		if rec["owner_filter"] != a.user.String() {
			t.Errorf("audit owner_filter = %v, want %s", rec["owner_filter"], a.user)
		}
		if rec["returned"] != float64(5) {
			t.Errorf("audit returned = %v, want 5", rec["returned"])
		}
		if rec["since"] != base.Format(time.RFC3339Nano) {
			t.Errorf("audit since = %v, want the normalised UTC %s", rec["since"], base.Format(time.RFC3339Nano))
		}
	})

	t.Run("audit text carries no raw since or secret", func(t *testing.T) {
		f.audit.take(t)
		// An offset no CI or developer host runs in, so log timestamps cannot carry it.
		sinceArg := base.In(time.FixedZone("", 5*3600+45*60)).Format(time.RFC3339)
		f.get(t, "/api/admin/runs?state=all&user="+b.user.String()+"&since="+strings.ReplaceAll(sinceArg, "+", "%2B"))
		out := f.audit.buf.String()
		if strings.Contains(out, "+05:45") || strings.Contains(out, sinceArg) {
			t.Errorf("audit output echoes the raw since argument:\n%s", out)
		}
		for _, secret := range []string{f.adminRO, f.adminUzc, adrlReport} {
			if strings.Contains(out, secret) {
				t.Errorf("audit output contains %q:\n%s", secret, out)
			}
		}
		rec := f.oneAudit(t, "owner B listing")
		if rec["returned"] != float64(2) {
			t.Errorf("owner B audit returned = %v, want 2 (queued and running)", rec["returned"])
		}
	})

	t.Run("empty result is an empty array and still audited", func(t *testing.T) {
		empty := adrlSeedOwner(t, pool) // an owner with no runs
		body := f.get(t, "/api/admin/runs?state=all&user="+empty.user.String())
		_, rawRuns, truncated := adrlListed(t, body)
		if rawRuns != "[]" {
			t.Errorf("runs = %s, want []", rawRuns)
		}
		if truncated {
			t.Errorf("truncated = true on an empty listing")
		}
		rec := f.oneAudit(t, "empty listing")
		if rec["returned"] != float64(0) || rec["owner_filter"] != empty.user.String() {
			t.Errorf("empty-listing audit = %v, want returned 0 and owner_filter %s", rec, empty.user)
		}
	})
}

// TestAdminRunDebugListRunsTruncationLiveDB: exactly 500 eligible runs are all served with
// truncated=false; a 501st makes the response 500 rows, newest first, truncated=true.
func TestAdminRunDebugListRunsTruncationLiveDB(t *testing.T) {
	f := adrlNewListFixture(t)
	start := time.Now()
	o := adrlSeedOwner(t, f.pool)
	t.Cleanup(func() {
		// Leave no 501 rows behind for the unfiltered listings of other tests.
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM runs WHERE user_id = $1`, o.user)
	})
	base := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	// Run g (1..500) was created g seconds before base: g=1 is the newest.
	cliMustExec(t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
		 SELECT gen_random_uuid(), $1, $2, $3 + g, 'Do X', 'desc', 'completed', 'issue', $4::timestamptz - g * interval '1 second'
		 FROM generate_series(1, 500) AS g`, o.user, o.repo, adrlIssueIID.Add(1000), base)
	t.Logf("500-row fixture seeded in %s", time.Since(start))

	path := "/api/admin/runs?state=all&user=" + o.user.String()
	body := f.get(t, path)
	runs, _, truncated := adrlListed(t, body)
	if len(runs) != 500 || truncated {
		t.Fatalf("500 eligible runs: served %d rows truncated=%v, want 500 and false", len(runs), truncated)
	}
	if rec := f.oneAudit(t, "500-row listing"); rec["returned"] != float64(500) {
		t.Errorf("audit returned = %v, want 500", rec["returned"])
	}
	oldestServed := adrlRunID(t, runs[499])

	newest := uuid.New()
	adrlSeedIssueRun(t, f.pool, o, newest, "running", base.Add(time.Minute))
	start = time.Now()
	body = f.get(t, path)
	t.Logf("501-row listing served in %s", time.Since(start))
	runs, _, truncated = adrlListed(t, body)
	if len(runs) != 500 || !truncated {
		t.Fatalf("501 eligible runs: served %d rows truncated=%v, want 500 and true", len(runs), truncated)
	}
	if id := adrlRunID(t, runs[0]); id != newest.String() {
		t.Errorf("first row = %s, want the newest run %s", id, newest)
	}
	// The oldest of the original 500 is the row the new run displaced from the page.
	for _, r := range runs {
		if adrlRunID(t, r) == oldestServed {
			t.Errorf("run %s (the oldest of the first 500) is still served after a newer run was added", oldestServed)
		}
	}
	if rec := f.oneAudit(t, "truncated listing"); rec["returned"] != float64(500) {
		t.Errorf("truncated audit returned = %v, want 500 (the served count, not 501)", rec["returned"])
	}
}

// TestAdminRunDebugListRunsRefusedLiveDB: every caller that is not a live admin's admin_ro
// Bearer on a flag-on deployment is refused, before parameter validation, with no audit;
// valid-credential validation errors audit nothing; the legacy listing is untouched.
func TestAdminRunDebugListRunsRefusedLiveDB(t *testing.T) {
	f := adrlNewListFixture(t)
	o := adrlSeedOwner(t, f.pool)
	terminalRun := uuid.New()
	adrlSeedIssueRun(t, f.pool, o, terminalRun, "completed", time.Now().UTC().Add(-time.Minute))
	valid := "/api/admin/runs?state=all&user=" + o.user.String()

	refusedBody := func(t *testing.T, label string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403\nbody: %s", label, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), o.user.String()) {
			t.Errorf("%s: refusal body echoes data: %s", label, rec.Body.String())
		}
	}

	t.Run("flag off", func(t *testing.T) {
		f.h.cfg.AdminRunDebug = false
		defer func() { f.h.cfg.AdminRunDebug = true }()
		rec := bearerReq(f.router, http.MethodGet, valid, f.adminRO)
		refusedBody(t, "flag off, live admin uza_", rec)
		if !strings.Contains(rec.Body.String(), adminRunDebugRefusal) {
			t.Errorf("flag-off refusal body lacks the refusal text: %s", rec.Body.String())
		}
		// The predicate precedes validation: an invalid state is still the 403.
		rec = bearerReq(f.router, http.MethodGet, "/api/admin/runs?state=bogus", f.adminRO)
		refusedBody(t, "flag off, state=bogus", rec)
		if recs := f.audit.take(t); len(recs) != 0 {
			t.Errorf("flag-off refusals wrote %d audit records, want 0: %v", len(recs), recs)
		}
	})

	t.Run("flag on, wrong caller", func(t *testing.T) {
		rec := cookieReq(t, f.router, http.MethodGet, valid, f.adminJWT, "")
		refusedBody(t, "admin session cookie", rec)
		if !strings.Contains(rec.Body.String(), adminRunDebugRefusal) {
			t.Errorf("cookie refusal body lacks the refusal text: %s", rec.Body.String())
		}
		for who, token := range map[string]string{
			"admin default uzc_":           f.adminUzc,
			"demoted admin's uza_":         f.demotedRO,
			"non-admin uzc_":               f.otherUzc,
			"non-admin uza_":               f.otherRO,
			"stored scope user, uza_ form": f.storedUser,
		} {
			refusedBody(t, who, bearerReq(f.router, http.MethodGet, valid, token))
		}
		for who, token := range map[string]string{"revoked uza_": f.revokedRO, "expired uza_": f.expiredRO, "no credential": ""} {
			if rec := bearerReq(f.router, http.MethodGet, valid, token); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s = %d, want 401\nbody: %s", who, rec.Code, rec.Body.String())
			}
		}
		if recs := f.audit.take(t); len(recs) != 0 {
			t.Errorf("refused callers wrote %d audit records, want 0: %v", len(recs), recs)
		}
	})

	t.Run("flag on, live admin, invalid parameters", func(t *testing.T) {
		for _, tc := range []struct{ name, path string }{
			{"state=active", "/api/admin/runs?state=active"},
			{"state empty", "/api/admin/runs?state="},
			{"user only", "/api/admin/runs?user=" + o.user.String()},
			{"since only", "/api/admin/runs?since=2026-01-01T00:00:00Z"},
			{"user not a uuid", "/api/admin/runs?state=all&user=notauuid"},
			{"user empty", "/api/admin/runs?state=all&user="},
			{"since not RFC3339", "/api/admin/runs?state=all&since=yesterday"},
			{"since empty", "/api/admin/runs?state=all&since="},
		} {
			rec := bearerReq(f.router, http.MethodGet, tc.path, f.adminRO)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: GET %s = %d, want 400\nbody: %s", tc.name, tc.path, rec.Code, rec.Body.String())
			}
		}
		if recs := f.audit.take(t); len(recs) != 0 {
			t.Errorf("validation errors wrote %d audit records, want 0: %v", len(recs), recs)
		}
	})

	t.Run("legacy listing unchanged without debug keys", func(t *testing.T) {
		const legacy = "/api/admin/runs"
		on := bearerReq(f.router, http.MethodGet, legacy, f.adminRO)
		f.h.cfg.AdminRunDebug = false
		off := bearerReq(f.router, http.MethodGet, legacy, f.adminRO)
		f.h.cfg.AdminRunDebug = true
		if on.Code != http.StatusOK || off.Code != http.StatusOK {
			t.Fatalf("legacy listing: flag on %d, flag off %d, want 200 both\non: %s\noff: %s", on.Code, off.Code, on.Body.String(), off.Body.String())
		}
		// budget_used_seconds is derived from the clock, so two reads may straddle a second.
		if adrlStripClock(on.Body.String()) != adrlStripClock(off.Body.String()) {
			t.Errorf("legacy listing differs with the flag:\non:  %s\noff: %s", on.Body.String(), off.Body.String())
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(on.Body.Bytes(), &env); err != nil {
			t.Fatalf("legacy body is not JSON: %v", err)
		}
		if _, has := env["truncated"]; has {
			t.Errorf("legacy body carries the debug truncated key: %s", on.Body.String())
		}
		if strings.Contains(on.Body.String(), terminalRun.String()) {
			t.Errorf("legacy active listing served the completed run %s", terminalRun)
		}
		if recs := f.audit.take(t); len(recs) != 0 {
			t.Errorf("legacy listing wrote %d audit records, want 0: %v", len(recs), recs)
		}
	})
}

var adrlClockField = regexp.MustCompile(`"budget_used_seconds":-?[0-9]+`)

// adrlStripClock blanks the clock-derived budget_used_seconds values, keeping every other byte.
func adrlStripClock(body string) string {
	return adrlClockField.ReplaceAllString(body, `"budget_used_seconds":0`)
}
