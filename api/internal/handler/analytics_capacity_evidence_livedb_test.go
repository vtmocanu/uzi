//go:build analyticsevidence

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #4731 (U4): router-level request timings for Capacity and the Spend comparison, against a
// database the store harness already seeded. It is a measurement harness, not a gate: it is
// compiled only with -tags analyticsevidence and runs only with UZI_ANALYTICS_PLAN_EVIDENCE=1.
//
// Seed a private database first (the store harness, keeping its seed), then point this test at it:
//
//	UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_EVIDENCE_KEEP=1 UZI_EVIDENCE_BASELINE=0 UZI_EVIDENCE_INDEXES=0 \
//	  UZI_TEST_DATABASE_URL=... go test -tags analyticsevidence -run TestAnalyticsPlanEvidenceLiveDB ./internal/store
//	UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_TEST_DATABASE_URL=... \
//	  go test -tags analyticsevidence -count=1 -v -run TestAnalyticsCapacityRequestEvidenceLiveDB ./internal/handler
//
// Optional: UZI_EVIDENCE_LOG (absolute path, appended) and UZI_EVIDENCE_RUNS (default 20). The
// clock is the seed's now (2026-11-20T12:00:00Z). Requests go through the mounted router with a
// real pool and vault, so the figures include auth, the admission gate, the snapshot, the
// statements, the provider gauges and the Codex vault observation.
func TestAnalyticsCapacityRequestEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_PLAN_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_PLAN_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	runs := 20
	if v := os.Getenv("UZI_EVIDENCE_RUNS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &runs); err != nil || runs < 1 {
			t.Fatalf("UZI_EVIDENCE_RUNS = %q", v)
		}
	}
	logf := func(format string, args ...any) { t.Logf(format, args...) }
	if p := os.Getenv("UZI_EVIDENCE_LOG"); p != "" {
		f, err := os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304/G703: the operator-chosen log path of a measurement harness that never runs in CI
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		logf = func(format string, args ...any) {
			t.Logf(format, args...)
			_, _ = fmt.Fprintf(f, format+"\n", args...)
		}
	}

	f := newCapFixture(t, 2026, false)
	f.now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var top uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = 'evid-0001@e2e'`).Scan(&top); err != nil {
		t.Fatalf("the store evidence seed is missing (top owner evid-0001@e2e): %v", err)
	}
	var nRuns int64
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runs`).Scan(&nRuns); err != nil {
		t.Fatal(err)
	}
	if nRuns < 100000 {
		t.Fatalf("database holds %d runs, want the >= 100000 seed", nRuns)
	}
	admin := f.user(true)
	logf("REQUEST EVIDENCE database runs=%d clock=%s measured=%d (plus 1 warm-up) through the mounted router", nRuns, f.now.Format(time.RFC3339), runs)

	for _, c := range []struct {
		name   string
		viewer uuid.UUID
		path   string
	}{
		{"owner capacity 30d (top owner)", top, "/api/analytics/capacity?window=30d"},
		{"admin all-users capacity 7d", admin, "/api/admin/analytics/capacity?window=7d"},
		{"admin all-users capacity 30d", admin, "/api/admin/analytics/capacity?window=30d"},
		{"admin all-users capacity 90d", admin, "/api/admin/analytics/capacity?window=90d"},
		{"admin all-users spend 30d (comparison)", admin, "/api/admin/analytics/spend?window=30d"},
		{"admin all-users spend 90d (comparison)", admin, "/api/admin/analytics/spend?window=90d"},
	} {
		jwt := cliMintJWT(t, f.pool, c.viewer)
		var ds []time.Duration
		codes := map[int]int{}
		var size int
		for i := 0; i <= runs; i++ {
			serve := cookieReqPrepared(t, f.router, http.MethodGet, c.path, jwt, "")
			start := time.Now()
			rec := serve()
			d := time.Since(start)
			codes[rec.Code]++
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d: %s", c.name, rec.Code, rec.Body.String())
			}
			if i == 0 {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
					t.Fatalf("%s: decode: %v", c.name, err)
				}
				size = rec.Body.Len()
				continue
			}
			ds = append(ds, d)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		pct := func(p float64) time.Duration { return ds[min(len(ds)-1, int(p*float64(len(ds))))] }
		logf("REQUEST TIMING %-42s n=%d status=%v bytes=%d median=%s p90=%s max=%s",
			c.name, len(ds), codes, size, ds[len(ds)/2].Round(100*time.Microsecond), pct(0.9).Round(100*time.Microsecond), ds[len(ds)-1].Round(100*time.Microsecond))
	}
}
