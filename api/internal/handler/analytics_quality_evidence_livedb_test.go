//go:build analyticsevidence

package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4730 (m3): HTTP request impact of the Quality routes against the evidence seed that
// TestAnalyticsPlanEvidenceLiveDB (internal/store, build tag analyticsevidence) leaves behind
// with UZI_EVIDENCE_KEEP=1. A measurement harness, not a gate: compiled only with
// -tags analyticsevidence and run only with UZI_ANALYTICS_PLAN_EVIDENCE=1. It does not seed
// and does not truncate; it fails first when the seed is absent. The only rows it writes are
// one admin user (removed at the end). Run after the store harness:
//
//	cd api && UZI_ANALYTICS_PLAN_EVIDENCE=1 \
//	  UZI_TEST_DATABASE_URL='postgres://...?sslmode=disable' \
//	  go test -tags analyticsevidence -count=1 -v -p 1 -timeout 30m \
//	  -run TestQualityHTTPEvidenceLiveDB ./internal/handler
//
// Optional environment: UZI_EVIDENCE_LOG (appended report), UZI_EVIDENCE_HTTP_N (measured
// requests per scope, default 25), UZI_EVIDENCE_HTTP_WARMUP (default 2).
//
// The analytics pool is opened with 20 connections (the production default DB_MAX_CONNS); the
// test logs the size and the resulting admission capacity. Requests are sequential through the production router (cookie
// session, CSRF header, the analytics admission gate, the 8 s snapshot statement_timeout), with
// the handler clock fixed at the seed's now so the 30d window holds the seeded runs.

const evHTTPNowRFC = "2026-11-20T12:00:00Z"

type evHTTPScope struct {
	name, path string
	jwt        string
}

type evHTTPResult struct {
	status int
	body   string
	dur    time.Duration
}

func evHTTPClass(r evHTTPResult) string {
	switch {
	case r.status == http.StatusOK:
		return "200"
	case r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "timed out"):
		return "503-timeout"
	case r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "busy"):
		return "503-busy"
	case r.status == http.StatusServiceUnavailable:
		return "503-other"
	}
	return fmt.Sprintf("other-%d", r.status)
}

func evHTTPRank(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func evHTTPMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func evHTTPEnvInt(name string, def int) int {
	var v int
	if _, err := fmt.Sscanf(os.Getenv(name), "%d", &v); err == nil && v > 0 {
		return v
	}
	return def
}

// evHTTPPreflight runs on a plain read-only connection BEFORE cliLiveDB (which migrates and
// writes): it fails, never skips, unless the database carries the throwaway marker row and the
// retained evidence seed. It returns the seed's row counts.
func evHTTPPreflight(t *testing.T, dsn string) (runs, rework, checks, reviews int64) {
	t.Helper()
	if dsn == "" {
		t.Fatal("UZI_TEST_DATABASE_URL not set; the evidence harness needs the retained throwaway database")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("preflight connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	var markers int
	if err := c.QueryRow(ctx,
		`SELECT count(*)::int FROM uzi_test_harness.database_marker WHERE marker = 'recovery-reset-v1'`).Scan(&markers); err != nil {
		t.Fatalf("refusing to run: uzi_test_harness.database_marker unreadable: %v; initialise a throwaway database with e2e/test-database-marker.sql", err)
	}
	if markers != 1 {
		t.Fatalf("refusing to run: uzi_test_harness.database_marker lacks recovery-reset-v1 (count %d); initialise a throwaway database with e2e/test-database-marker.sql", markers)
	}
	if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM runs), (SELECT count(*) FROM runs WHERE kind = 'mr_rework'),
	        (SELECT count(*) FROM cross_checks), (SELECT count(*) FROM run_reviews)`).Scan(&runs, &rework, &checks, &reviews); err != nil {
		t.Fatalf("count seed: %v", err)
	}
	if runs < 100000 || rework == 0 || checks == 0 || reviews == 0 {
		t.Fatalf("the evidence seed is absent (runs=%d mr_rework=%d cross_checks=%d run_reviews=%d); run the store harness first with UZI_EVIDENCE_KEEP=1", runs, rework, checks, reviews)
	}
	return runs, rework, checks, reviews
}

func TestQualityHTTPEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_PLAN_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_PLAN_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	runs, rework, checks, reviews := evHTTPPreflight(t, os.Getenv("UZI_TEST_DATABASE_URL"))
	h, router, pool := cliLiveDB(t)
	// The analytics reads run on a pool sized like production (DB_MAX_CONNS default 20), which
	// also sizes the admission gate; cliLiveDB's own pool only serves auth and the fixtures.
	prod, err := store.OpenPool(context.Background(), os.Getenv("UZI_TEST_DATABASE_URL"), store.WithMaxConns(20))
	if err != nil {
		t.Fatalf("open production-sized pool: %v", err)
	}
	t.Cleanup(prod.Close)
	h.analyticsDeps = newAnalyticsDeps(prod)
	now, err := time.Parse(time.RFC3339, evHTTPNowRFC)
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return now }

	var logf func(format string, a ...any)
	var logFile *os.File
	if p := os.Getenv("UZI_EVIDENCE_LOG"); p != "" {
		logFile, err = os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304/G703: the operator-chosen log path of a measurement harness that never runs in CI
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		t.Cleanup(func() { _ = logFile.Close() })
	}
	logf = func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		if logFile != nil {
			_, _ = logFile.WriteString(line + "\n")
		}
		t.Log(line)
	}

	var top uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = 'evid-0001@e2e'`).Scan(&top); err != nil {
		t.Fatalf("largest evidence owner: %v", err)
	}
	var topRuns int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE user_id = $1`, top).Scan(&topRuns); err != nil {
		t.Fatal(err)
	}
	admin := cliSeedUser(t, pool, true)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id = $1`, admin) })

	logf("HTTP EVIDENCE: analytics pool MaxConns=%d (production default DB_MAX_CONNS=20), admission capacity %d; seed runs=%d mr_rework=%d cross_checks=%d run_reviews=%d; largest owner %s holds %d runs; handler clock fixed at %s",
		prod.Config().MaxConns, h.analyticsDeps.admission.Capacity(), runs, rework, checks, reviews, top, topRuns, evHTTPNowRFC)

	ownerJWT, adminJWT := cliMintJWT(t, pool, top), cliMintJWT(t, pool, admin)
	scopes := []evHTTPScope{
		{"owner window=30d", "/api/analytics/quality?window=30d", ownerJWT},
		{"owner window=all", "/api/analytics/quality?window=all", ownerJWT},
		{"admin all-users window=30d", "/api/admin/analytics/quality?window=30d", adminJWT},
		{"admin user=<largest owner> window=30d", "/api/admin/analytics/quality?user=" + top.String() + "&window=30d", adminJWT},
	}
	n, warmup := evHTTPEnvInt("UZI_EVIDENCE_HTTP_N", 25), evHTTPEnvInt("UZI_EVIDENCE_HTTP_WARMUP", 2)
	if n < 20 {
		t.Fatalf("UZI_EVIDENCE_HTTP_N=%d: at least 20 measured requests per scope are required", n)
	}
	do := func(sc evHTTPScope) evHTTPResult {
		start := time.Now()
		rec := cookieReq(t, router, http.MethodGet, sc.path, sc.jwt, "")
		return evHTTPResult{status: rec.Code, body: rec.Body.String(), dur: time.Since(start)}
	}

	logf("%-40s %4s %5s %5s %5s %6s %8s %8s %8s", "scope", "N", "200", "503t", "503b", "other", "p50 ms", "p95 ms", "max ms")
	for _, sc := range scopes {
		var warm []string
		for i := 0; i < warmup; i++ {
			r := do(sc)
			warm = append(warm, fmt.Sprintf("%s %.1fms", evHTTPClass(r), evHTTPMs(r.dur)))
		}
		counts := map[string]int{}
		var okDur []time.Duration
		var firstOK string
		for i := 0; i < n; i++ {
			r := do(sc)
			c := evHTTPClass(r)
			counts[c]++
			if r.status == http.StatusOK {
				okDur = append(okDur, r.dur)
				if firstOK == "" {
					firstOK = r.body
				}
			} else if c != "503-timeout" && c != "503-busy" {
				t.Errorf("%s: unexpected response %s: %.200s", sc.name, c, r.body)
			}
		}
		sort.Slice(okDur, func(i, j int) bool { return okDur[i] < okDur[j] })
		other := n - counts["200"] - counts["503-timeout"] - counts["503-busy"]
		var maxOK time.Duration
		if len(okDur) > 0 {
			maxOK = okDur[len(okDur)-1]
		}
		logf("%-40s %4d %5d %5d %5d %6d %8.1f %8.1f %8.1f", sc.name, n, counts["200"], counts["503-timeout"], counts["503-busy"], other,
			evHTTPMs(evHTTPRank(okDur, 0.50)), evHTTPMs(evHTTPRank(okDur, 0.95)), evHTTPMs(maxOK))
		logf("   warm-ups (reported separately, not in the table): %s; response bytes (first 200): %d", strings.Join(warm, ", "), len(firstOK))
		if len(okDur) == 0 {
			t.Errorf("%s: no 200 in %d requests", sc.name, n)
		}
	}

	// Internal timing, labelled as such: the deps.quality call inside one analytics snapshot,
	// which excludes HTTP routing, auth, JSON encoding and the admission gate.
	logf("INTERNAL (excludes HTTP/admission): deps.quality inside one snapshot, %d runs per scope", 10)
	for _, sc := range scopes {
		u, perr := url.Parse(sc.path)
		if perr != nil {
			t.Fatal(perr)
		}
		reqUser := top
		admin := strings.HasPrefix(sc.path, "/api/admin")
		if admin {
			reqUser = uuid.New()
		}
		req, msg := parseAnalyticsRequest(u.Query(), admin, analytics.SectionQuality, reqUser)
		if msg != "" {
			t.Fatalf("parse %s: %s", sc.path, msg)
		}
		var durs []time.Duration
		for i := 0; i < 10; i++ {
			start := time.Now()
			if err := store.RunAnalyticsSnapshot(context.Background(), h.analyticsDeps.begin, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
				_, e := h.analyticsDeps.quality(ctx, s, req, now)
				return e
			}); err != nil {
				t.Fatalf("internal %s: %v", sc.name, err)
			}
			durs = append(durs, time.Since(start))
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		logf("   internal %-38s p50=%.1fms p95=%.1fms max=%.1fms", sc.name, evHTTPMs(evHTTPRank(durs, 0.5)), evHTTPMs(evHTTPRank(durs, 0.95)), evHTTPMs(durs[len(durs)-1]))
	}
}
