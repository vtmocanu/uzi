//go:build analyticsevidence

package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Issue #4700 (M4): read-path measurement of the analytics Spend endpoints THROUGH THE REAL
// HANDLER (httptest against the production router, the production snapshot with its 8 s
// statement_timeout and jit=off, the production pgxpool). This is a measurement harness, not a
// gate: it is compiled only with -tags analyticsevidence, runs only with
// UZI_ANALYTICS_READS_EVIDENCE=1 and refuses to run unless the database carries the throwaway
// marker row (e2e/test-database-marker.sql). It assumes the PRD #4684 evidence seed
// (api/internal/store/analytics_evidence_livedb_test.go) is already present and only reads it.
//
// This file compiles against the pre-#4700 tree too (it uses only the handler API), so the same
// control cases can be run against a base export. The fold-state conditions live in
// analytics_folds_evidence_livedb_test.go, which needs the #4700 store.
//
//	cd api && go test -c -tags analyticsevidence -o reads.test ./internal/handler
//	UZI_ANALYTICS_READS_EVIDENCE=1 UZI_TEST_DATABASE_URL=... UZI_READS_LABEL=cand-fresh \
//	  ./reads.test -test.run TestAnalyticsReadsControlsEvidenceLiveDB -test.v -test.timeout 50m
//
// Optional environment:
//
//	UZI_READS_LABEL   free text printed on every line
//	UZI_READS_N       measured samples per case and mode (default 12)
//	UZI_READS_WARM    warm-up requests per case and mode, validated but not counted (default 2)
//	UZI_READS_MODES   auto,custom,generic (default all). auto is the production behaviour: no
//	                  plan_cache_mode override, so pgx's cached prepared statement starts with
//	                  custom plans and PostgreSQL may switch to a generic one after 5 executions.
//	UZI_READS_ONLY    comma list of case-name substrings

const rdNowRFC = "2026-11-20T12:00:00Z"

// rdBudget is the request budget (store.AnalyticsSnapshotDeadline, also the statement_timeout).
const rdBudget = 8 * time.Second

func rdLogf(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func rdEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

// rdRequireThrowaway fails the run unless the database carries exactly one throwaway marker row.
func rdRequireThrowaway(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("UZI_TEST_DATABASE_URL not set; the reads evidence harness needs a throwaway database")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("marker check connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*)::int FROM uzi_test_harness.database_marker WHERE marker = 'recovery-reset-v1'`).Scan(&n); err != nil {
		t.Fatalf("refusing to run: uzi_test_harness.database_marker unreadable: %v", err)
	}
	if n != 1 {
		t.Fatalf("refusing to run: throwaway marker count %d, want 1; initialise a throwaway database with e2e/test-database-marker.sql", n)
	}
	return dsn
}

// rdModeDSN returns dsn with the connection-level plan_cache_mode for mode. auto adds nothing.
func rdModeDSN(t *testing.T, dsn, mode string) string {
	t.Helper()
	switch mode {
	case "auto":
		return dsn
	case "custom", "generic":
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		q := u.Query()
		q.Set("plan_cache_mode", "force_"+mode+"_plan")
		u.RawQuery = q.Encode()
		return u.String()
	}
	t.Fatalf("unknown mode %q", mode)
	return ""
}

func rdModes() []string {
	if m := os.Getenv("UZI_READS_MODES"); m != "" {
		return strings.Split(m, ",")
	}
	return []string{"auto", "custom", "generic"}
}

// rdCase is one request. route/user/window are what the response must echo back.
type rdCase struct {
	name                string
	asUser              uuid.UUID // uuid.Nil: the admin session
	path                string
	route, user, window string
}

type rdResult struct {
	status  int
	elapsed time.Duration
	valid   bool
	why     string
	hash    string
}

// rdSession is one mode's handler, router and pool, plus an admin account.
type rdSession struct {
	t     *testing.T
	f     *anFixture
	mode  string
	admin uuid.UUID
}

// rdNewSession builds the production handler over a pool whose connections carry the mode's
// plan_cache_mode. Call it inside a subtest: the DSN override lasts for that subtest.
func rdNewSession(t *testing.T, mode string) *rdSession {
	t.Helper()
	base := rdRequireThrowaway(t)
	t.Setenv("UZI_TEST_DATABASE_URL", rdModeDSN(t, base, mode))
	f := newAnFixture(t, 2026)
	f.now = mustRFC(t, rdNowRFC)
	return &rdSession{t: t, f: f, mode: mode, admin: f.user(true)}
}

func mustRFC(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// do serves one request through the router and validates the response.
func (s *rdSession) do(c rdCase, jwtAdmin, jwtOwner string) rdResult {
	jwt := jwtAdmin
	if c.asUser != uuid.Nil {
		jwt = jwtOwner
	}
	serve := cookieReqPrepared(s.t, s.f.router, http.MethodGet, c.path, jwt, "")
	start := time.Now()
	rec := serve()
	res := rdResult{status: rec.Code, elapsed: time.Since(start)}
	sum := sha256.Sum256(rec.Body.Bytes())
	res.hash = hex.EncodeToString(sum[:6])
	if rec.Code != http.StatusOK {
		res.why = fmt.Sprintf("status %d: %.120s", rec.Code, strings.TrimSpace(rec.Body.String()))
		return res
	}
	var dto apitypes.AnalyticsSpendDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		res.why = "decode: " + err.Error()
		return res
	}
	switch {
	case dto.Scope.Route != c.route:
		res.why = fmt.Sprintf("scope.route %q, want %q", dto.Scope.Route, c.route)
	case dto.Scope.User != c.user || dto.SelectedUser != c.user:
		res.why = fmt.Sprintf("scope.user %q selected_user %q, want %q", dto.Scope.User, dto.SelectedUser, c.user)
	case dto.Window != c.window:
		res.why = fmt.Sprintf("window %q, want %q", dto.Window, c.window)
	case len(dto.Buckets) == 0 || len(dto.Series) != len(dto.Buckets):
		res.why = fmt.Sprintf("buckets %d series %d", len(dto.Buckets), len(dto.Series))
	case !dto.GeneratedAt.Equal(s.f.now):
		res.why = fmt.Sprintf("generated_at %s, want the fixed clock", dto.GeneratedAt)
	case dto.Totals.Current.Runs <= 0 || dto.Totals.Current.CostUSD <= 0:
		res.why = fmt.Sprintf("empty totals: runs=%d cost=%v", dto.Totals.Current.Runs, dto.Totals.Current.CostUSD)
	case c.window == "all" && dto.Totals.Previous != nil:
		res.why = "window=all carries a previous block"
	}
	res.valid = res.why == ""
	return res
}

type rdSummary struct {
	n                       int
	median, p95, max        time.Duration
	all200, allValid, under bool
	bad                     []string
	hashes                  map[string]int
}

// rdMeasure runs warm unscored then n scored samples of c and prints every sample.
func (s *rdSession) measure(label string, c rdCase, n, warm int) rdSummary {
	t := s.t
	t.Helper()
	jwtAdmin := cliMintJWT(t, s.f.pool, s.admin)
	var jwtOwner string
	if c.asUser != uuid.Nil {
		jwtOwner = cliMintJWT(t, s.f.pool, c.asUser)
	}
	sum := rdSummary{all200: true, allValid: true, under: true, hashes: map[string]int{}}
	for i := 0; i < warm; i++ {
		r := s.do(c, jwtAdmin, jwtOwner)
		rdLogf("RDWARM label=%s case=%s mode=%s i=%d status=%d ms=%.1f valid=%v hash=%s %s", label, c.name, s.mode, i, r.status, ms(r.elapsed), r.valid, r.hash, r.why)
	}
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		r := s.do(c, jwtAdmin, jwtOwner)
		rdLogf("RDSAMPLE label=%s case=%s mode=%s i=%d status=%d ms=%.1f valid=%v hash=%s %s", label, c.name, s.mode, i, r.status, ms(r.elapsed), r.valid, r.hash, r.why)
		durs = append(durs, r.elapsed)
		sum.hashes[r.hash]++
		if r.status != http.StatusOK {
			sum.all200 = false
		}
		if !r.valid {
			sum.allValid = false
			sum.bad = append(sum.bad, fmt.Sprintf("(status=%d, %.1fms, %s)", r.status, ms(r.elapsed), r.why))
		}
		if r.elapsed >= rdBudget {
			sum.under = false
			sum.bad = append(sum.bad, fmt.Sprintf("(status=%d, %.1fms, over budget)", r.status, ms(r.elapsed)))
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	sum.n = len(durs)
	if sum.n > 0 {
		if sum.n%2 == 1 {
			sum.median = durs[sum.n/2]
		} else {
			sum.median = (durs[sum.n/2-1] + durs[sum.n/2]) / 2
		}
		sum.p95 = durs[int(math.Ceil(0.95*float64(sum.n)))-1]
		sum.max = durs[sum.n-1]
	}
	hs := make([]string, 0, len(sum.hashes))
	for h, c := range sum.hashes {
		hs = append(hs, fmt.Sprintf("%s*%d", h, c))
	}
	sort.Strings(hs)
	rdLogf("RDSUM label=%s case=%s mode=%s n=%d median_ms=%.1f p95_ms=%.1f max_ms=%.1f all200=%v valid=%v under8s=%v hashes=%s bad=%v",
		label, c.name, s.mode, sum.n, ms(sum.median), ms(sum.p95), ms(sum.max), sum.all200, sum.allValid, sum.under, strings.Join(hs, ","), sum.bad)
	return sum
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// planModes prints, for every pooled connection, how the cached AnalyticsAllSpend statement has
// been planned so far (generic_plans / custom_plans of pg_prepared_statements). pgx's default
// QueryExecModeCacheStatement prepares the statement once per connection, so this is the
// handler path's effective plan mode.
func (s *rdSession) planModes(label string) {
	t := s.t
	ctx := context.Background()
	pool := s.f.pool
	// Only idle connections: the one(s) that served the requests; a fresh connection has no
	// cached statements.
	conns := pool.AcquireAllIdle(ctx)
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	for i, c := range conns {
		rows, err := c.Query(ctx, `SELECT left(name, 24), generic_plans, custom_plans, length(statement),
		        (statement LIKE '%all-only%') FROM pg_prepared_statements WHERE length(statement) > 2000 ORDER BY length(statement) DESC LIMIT 4`)
		if err != nil {
			t.Fatalf("pg_prepared_statements: %v", err)
		}
		for rows.Next() {
			var name string
			var generic, custom int64
			var length int
			var allOnly bool
			if err := rows.Scan(&name, &generic, &custom, &length, &allOnly); err != nil {
				t.Fatalf("scan: %v", err)
			}
			rdLogf("RDPLANMODE label=%s mode=%s conn=%d stmt=%s len=%d candidate_all_stmt=%v generic_plans=%d custom_plans=%d", label, s.mode, i, name, length, allOnly, generic, custom)
		}
		rows.Close()
		if i == 0 {
			var pcm, jit, stmt string
			if err := c.QueryRow(ctx, `SELECT current_setting('plan_cache_mode'), current_setting('jit'), current_setting('statement_timeout')`).Scan(&pcm, &jit, &stmt); err == nil {
				rdLogf("RDSESSION label=%s mode=%s idle_conns=%d session plan_cache_mode=%s jit=%s statement_timeout=%s (the handler's SET LOCAL applies inside its transaction only)", label, s.mode, len(conns), pcm, jit, stmt)
			}
		}
	}
}

// rdWindowAllCases are the admin all-users window=all requests the candidate must serve. The
// first matches the store evidence workload (split=harness, breakdown=kind); the second is the
// handler default (usage_model, repo).
func rdWindowAllCases() []rdCase {
	return []rdCase{
		{name: "admin-all-window-all-harness-kind", path: "/api/admin/analytics/spend?window=all&user=all&split=harness&breakdown=kind", route: "admin", user: "all", window: "all"},
		{name: "admin-all-window-all-default", path: "/api/admin/analytics/spend?window=all", route: "admin", user: "all", window: "all"},
	}
}

func rdFilter(cases []rdCase) []rdCase {
	only := os.Getenv("UZI_READS_ONLY")
	if only == "" {
		return cases
	}
	var out []rdCase
	for _, c := range cases {
		for _, f := range strings.Split(only, ",") {
			if f != "" && strings.Contains(c.name, f) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func rdUserID(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("seed user %s: %v (is the evidence seed present?)", email, err)
	}
	return id
}

// TestAnalyticsReadsControlsEvidenceLiveDB times the scopes that exist before #4700 (owner and
// admin-user Spend, admin all-users 7d/30d/90d) through the handler, so a base export and a
// candidate export can be compared on the same seed. The admin all-users window=all requests
// are NOT here (the base refuses them); see the folds file.
func TestAnalyticsReadsControlsEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_READS_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_READS_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	rdRequireThrowaway(t)
	label := os.Getenv("UZI_READS_LABEL")
	n, warm := rdEnvInt("UZI_READS_N", 12), rdEnvInt("UZI_READS_WARM", 2)
	for _, mode := range rdModes() {
		t.Run(mode, func(t *testing.T) {
			s := rdNewSession(t, mode)
			top := rdUserID(t, s.f.pool, "evid-0001@e2e")
			mid := rdUserID(t, s.f.pool, "evid-0005@e2e")
			var cases []rdCase
			for _, w := range []string{"7d", "30d", "90d", "all"} {
				cases = append(cases,
					rdCase{name: "owner-top-" + w, asUser: top, path: "/api/analytics/spend?window=" + w, route: "owner", user: top.String(), window: w},
					rdCase{name: "owner-mid-" + w, asUser: mid, path: "/api/analytics/spend?window=" + w, route: "owner", user: mid.String(), window: w})
			}
			for _, w := range []string{"7d", "30d", "90d"} {
				cases = append(cases, rdCase{name: "admin-all-" + w, path: "/api/admin/analytics/spend?window=" + w, route: "admin", user: "all", window: w})
			}
			for _, w := range []string{"30d", "all"} {
				cases = append(cases, rdCase{name: "admin-user-top-" + w, path: "/api/admin/analytics/spend?window=" + w + "&user=" + top.String(), route: "admin", user: top.String(), window: w})
			}
			for _, c := range rdFilter(cases) {
				s.measure(label, c, n, warm)
			}
			s.planModes(label)
		})
	}
}
