//go:build analyticsevidence

package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4771 (m5): build time and read latency of the stored all-time Spend documents against the
// evidence seed that TestAnalyticsPlanEvidenceLiveDB (internal/store, build tag analyticsevidence)
// leaves behind with UZI_EVIDENCE_KEEP=1. A measurement harness, not a gate: compiled only with
// -tags analyticsevidence and run only with UZI_ANALYTICS_PRECOMPUTE_EVIDENCE=1. It does not seed.
// It fails first unless the database carries the throwaway marker row and the retained seed. It
// applies migrations (store.Migrate), writes the stored documents (Publish) and one admin user
// (removed at the end; that delete empties the stored set through the users-delete invalidation
// trigger, as intended). With UZI_PRECOMPUTE_EVIDENCE_FOLD=1 the production folder also writes
// run_usage_fold_state and run_usage_model_folds permanently: measure the unfolded case first,
// or reseed, because nothing restores those rows. A during-build request is issued only after
// a pg_stat_activity probe finds the build backend active; skipped probes are counted apart. Run:
//
//	cd api && UZI_ANALYTICS_PRECOMPUTE_EVIDENCE=1 UZI_TEST_DATABASE_URL='postgres://...' \
//	  go test -tags analyticsevidence -count=1 -v -p 1 -timeout 58m \
//	  -run TestAnalyticsPrecomputeEvidenceLiveDB ./internal/handler
//
// Optional environment:
//
//	UZI_EVIDENCE_LOG                       absolute path, appended
//	UZI_PRECOMPUTE_EVIDENCE_FOLD=1         run the production usage folder until nothing is left to fold
//	UZI_PRECOMPUTE_EVIDENCE_FOLD_DEADLINE  Go duration bounding that fold (default 40m)
//	UZI_PRECOMPUTE_EVIDENCE_BUILDS         full builds to time (default 2)
//	UZI_PRECOMPUTE_EVIDENCE_SNAP_N         snapshot read samples (default 30)
//	UZI_PRECOMPUTE_EVIDENCE_LIVE_N         live idle samples per scope (default 12)
//
// The handler clock is pinned at the seed's now. Every result line starts with
// "PRECOMPUTE-EVIDENCE".

const (
	pcNowRFC = "2026-11-20T12:00:00Z"
	pcPrefix = "PRECOMPUTE-EVIDENCE "
	// pcBuildBound mirrors the scheduler's unexported defaultBuildBound (10 minutes).
	pcBuildBound = 10 * time.Minute
	// pcEligible is what the usage folder folds: finished, non-chat runs flagged for refold.
	pcEligible = `r.status IN ('completed', 'failed', 'cancelled') AND r.kind <> 'chat' AND r.usage_refolded`
)

func pcRank(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func pcMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func pcSorted(in []time.Duration) []time.Duration {
	out := append([]time.Duration(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// pcStats renders min/p50/p95/max of durations in milliseconds.
func pcStats(in []time.Duration) string {
	s := pcSorted(in)
	if len(s) == 0 {
		return "n=0"
	}
	return fmt.Sprintf("n=%d min=%.1fms p50=%.1fms p95=%.1fms max=%.1fms", len(s), pcMs(s[0]), pcMs(pcRank(s, 0.5)), pcMs(pcRank(s, 0.95)), pcMs(s[len(s)-1]))
}

func pcEnvInt(name string, def int) int {
	var v int
	if _, err := fmt.Sscanf(os.Getenv(name), "%d", &v); err == nil && v > 0 {
		return v
	}
	return def
}

// pcPreflight runs on a plain connection before anything is written: it fails, never skips,
// unless the database carries the throwaway marker row and the retained seed.
func pcPreflight(t *testing.T, dsn string) int64 {
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
		t.Fatalf("refusing to run: uzi_test_harness.database_marker lacks recovery-reset-v1 (count %d)", markers)
	}
	var runs int64
	if err := c.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count seed: %v", err)
	}
	if runs < 100000 {
		t.Fatalf("the evidence seed is absent (runs=%d); run the store harness first with UZI_EVIDENCE_KEEP=1", runs)
	}
	return runs
}

// pcCoverage returns the run counts by fold state of the folder-eligible runs.
func pcCoverage(ctx context.Context, pool *pgxpool.Pool) (runs, eligible, noState, stale, fresh int64, err error) {
	err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM runs),
	    count(*) FILTER (WHERE `+pcEligible+`),
	    count(*) FILTER (WHERE `+pcEligible+` AND fs.run_id IS NULL),
	    count(*) FILTER (WHERE `+pcEligible+` AND fs.stale),
	    count(*) FILTER (WHERE `+pcEligible+` AND fs.run_id IS NOT NULL AND NOT fs.stale)
	  FROM runs r LEFT JOIN run_usage_fold_state fs ON fs.run_id = r.id`).Scan(&runs, &eligible, &noState, &stale, &fresh)
	return
}

type pcReq struct {
	status int
	body   string
	dur    time.Duration
}

func pcClass(r pcReq) string {
	switch {
	case r.status == http.StatusOK:
		return "200"
	case r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "timed out"):
		return "503-timeout"
	case r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "busy"):
		return "503-busy"
	case r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "pending"):
		return "503-snapshot-pending"
	}
	return fmt.Sprintf("other-%d", r.status)
}

func pcCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func TestAnalyticsPrecomputeEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_PRECOMPUTE_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_PRECOMPUTE_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	seedRuns := pcPreflight(t, dsn)
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	prod, err := store.OpenPool(ctx, dsn, store.WithMaxConns(20))
	if err != nil {
		t.Fatalf("open production-sized pool: %v", err)
	}
	t.Cleanup(prod.Close)
	h.analyticsDeps = newAnalyticsDeps(prod)
	h.cfg.AnalyticsSnapshotInterval = 4 * time.Hour
	now, err := time.Parse(time.RFC3339, pcNowRFC)
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return now }

	var logFile *os.File
	if p := os.Getenv("UZI_EVIDENCE_LOG"); p != "" {
		logFile, err = os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304/G703: the operator-chosen log path of a measurement harness that never runs in CI
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		t.Cleanup(func() { _ = logFile.Close() })
	}
	logf := func(format string, a ...any) {
		line := pcPrefix + fmt.Sprintf(format, a...)
		if logFile != nil {
			_, _ = logFile.WriteString(line + "\n")
		}
		t.Log(line)
	}

	var top uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'evid-0001@e2e'`).Scan(&top); err != nil {
		t.Fatalf("largest evidence owner: %v", err)
	}
	var topRuns int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE user_id = $1`, top).Scan(&topRuns); err != nil {
		t.Fatal(err)
	}
	admin := cliSeedUser(t, pool, true)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id = $1`, admin) })
	ownerJWT, adminJWT := cliMintJWT(t, pool, top), cliMintJWT(t, pool, admin)
	keys := AllTimeSpendKeys()
	logf("SETUP seed_runs=%d largest_owner=%s owner_runs=%d variants=%d pool_max_conns=%d handler_clock=%s snapshot_interval=%s",
		seedRuns, top, topRuns, len(keys), prod.Config().MaxConns, pcNowRFC, h.cfg.AnalyticsSnapshotInterval)

	// Step 1: fold coverage.
	report := func(label string) {
		runs, elig, none, stale, fresh, cerr := pcCoverage(ctx, prod)
		if cerr != nil {
			t.Fatalf("coverage: %v", cerr)
		}
		pct := 0.0
		if elig > 0 {
			pct = 100 * float64(fresh) / float64(elig)
		}
		logf("FOLD-COVERAGE %s runs=%d eligible=%d no_state=%d stale=%d folded=%d folded_pct=%.1f", label, runs, elig, none, stale, fresh, pct)
	}
	report("before")
	if os.Getenv("UZI_PRECOMPUTE_EVIDENCE_FOLD") == "1" {
		deadline := 40 * time.Minute
		if v := os.Getenv("UZI_PRECOMPUTE_EVIDENCE_FOLD_DEADLINE"); v != "" {
			d, derr := time.ParseDuration(v)
			if derr != nil {
				t.Fatalf("UZI_PRECOMPUTE_EVIDENCE_FOLD_DEADLINE: %v", derr)
			}
			deadline = d
		}
		fctx, cancel := context.WithTimeout(ctx, deadline)
		folder := store.NewUsageFolder(prod, store.UsageFolderOptions{})
		start := time.Now()
		var passes, total int64
		var longest time.Duration
		zero := 0
		for fctx.Err() == nil {
			passes++
			ps := time.Now()
			n, perr := folder.Pass(fctx)
			if d := time.Since(ps); d > longest {
				longest = d
			}
			total += n
			if passes%20 == 0 || perr != nil {
				logf("FOLD-PROGRESS pass=%d folded_total=%d elapsed=%s err=%v", passes, total, time.Since(start).Round(time.Second), perr)
			}
			if n == 0 {
				if zero++; zero >= 3 {
					break
				}
			} else {
				zero = 0
			}
		}
		deadlineHit := fctx.Err() != nil
		cancel()
		logf("FOLD-RESULT passes=%d folded=%d wall=%s longest_pass=%s deadline_hit=%v", passes, total, time.Since(start).Round(time.Second), longest.Round(time.Millisecond), deadlineHit)
		report("after")
		// A partial fold would measure a mixed state: require full coverage of the eligible runs.
		if _, elig, none, stale, _, cerr := pcCoverage(ctx, prod); cerr != nil {
			t.Fatalf("coverage: %v", cerr)
		} else if none != 0 || stale != 0 {
			t.Fatalf("BLOCKED: fold incomplete: eligible=%d no_state=%d stale=%d (deadline_hit=%v); measure nothing until every eligible run is folded", elig, none, stale, deadlineHit)
		}
	}

	// buildOnce runs one full generation on a fresh session: open, RunBuild, Publish, Close.
	// hooks, when non-nil, replace the per-variant hooks.
	var buildPID atomic.Int64
	lockPID := func() int64 {
		var pid int64
		if qerr := prod.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted AND classid::bigint = $1 AND objid = 0 LIMIT 1`,
			int64(store.AnalyticsPrecomputeLockClass)).Scan(&pid); qerr != nil {
			return 0
		}
		return pid
	}
	type buildResult struct {
		total, open, run, publish time.Duration
		perVariant                []time.Duration
		docs                      []store.AnalyticsPrecomputedDoc
		pid                       int64
		err                       error
	}
	buildOnce := func(before, after func(i int, key string)) buildResult {
		var res buildResult
		res.perVariant = make([]time.Duration, len(keys))
		starts := make([]time.Time, len(keys))
		allTimeSpendBeforeVariant = func(i int, key string) error {
			if i == 1 {
				res.pid = lockPID()
				buildPID.Store(res.pid)
			}
			starts[i-1] = time.Now()
			if before != nil {
				before(i, key)
			}
			return nil
		}
		allTimeSpendAfterVariant = func(i int, key string) error {
			res.perVariant[i-1] = time.Since(starts[i-1])
			if after != nil {
				after(i, key)
			}
			return nil
		}
		defer func() { allTimeSpendBeforeVariant, allTimeSpendAfterVariant = nil, nil }()
		// Open, RunBuild and Publish share one 10 minute bound, matching the scheduler's
		// defaultBuildBound (internal/analyticsprecompute, unexported, so mirrored here).
		bctx0, bcancel := context.WithTimeout(ctx, pcBuildBound)
		defer bcancel()
		t0 := time.Now()
		s, ok, oerr := store.OpenAnalyticsPrecomputeSession(bctx0, prod)
		res.open = time.Since(t0)
		if oerr != nil || !ok {
			res.err = fmt.Errorf("open session ok=%v err=%v", ok, oerr)
			return res
		}
		var counter int64
		t1 := time.Now()
		res.err = s.RunBuild(bctx0, func(bctx context.Context, snap *store.AnalyticsSnapshot, c int64) error {
			counter = c
			var berr error
			res.docs, berr = BuildAllTimeSpendDocs(bctx, snap, now)
			return berr
		})
		res.run = time.Since(t1)
		if res.err == nil {
			t2 := time.Now()
			res.err = s.Publish(bctx0, AllTimeSpendSection, counter, now, res.docs)
			res.publish = time.Since(t2)
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = s.Close(cctx)
		cancel()
		res.total = time.Since(t0)
		return res
	}

	// Step 2: full build time.
	for b := 1; b <= pcEnvInt("UZI_PRECOMPUTE_EVIDENCE_BUILDS", 2); b++ {
		r := buildOnce(nil, nil)
		if r.err != nil {
			t.Fatalf("build %d: %v", b, r.err)
		}
		var sizes []int
		var sum int
		for _, d := range r.docs {
			sizes = append(sizes, len(d.Document))
			sum += len(d.Document)
		}
		sort.Ints(sizes)
		logf("BUILD rep=%d total=%.2fs open=%.1fms run_build=%.2fs publish=%.1fms backend_pid=%d docs=%d bytes_total=%d bytes_min=%d bytes_max=%d",
			b, r.total.Seconds(), pcMs(r.open), r.run.Seconds(), pcMs(r.publish), r.pid, len(r.docs), sum, sizes[0], sizes[len(sizes)-1])
		logf("BUILD rep=%d per-variant %s", b, pcStats(r.perVariant))
		for i, d := range r.perVariant {
			logf("BUILD rep=%d variant=%d key=%s ms=%.1f bytes=%d", b, i+1, keys[i], pcMs(d), len(r.docs[i].Document))
		}
	}

	// Step 3: snapshot read latency through the router.
	snapN := pcEnvInt("UZI_PRECOMPUTE_EVIDENCE_SNAP_N", 30)
	snapPaths := []string{
		"/api/admin/analytics/spend?window=all",
		"/api/admin/analytics/spend?window=all&" + keys[0],
		"/api/admin/analytics/spend?window=all&" + keys[len(keys)/2],
		"/api/admin/analytics/spend?window=all&" + keys[len(keys)-1],
	}
	do := func(path, jwt string) pcReq {
		start := time.Now()
		rec := cookieReq(t, router, http.MethodGet, path, jwt, "")
		return pcReq{status: rec.Code, body: rec.Body.String(), dur: time.Since(start)}
	}
	for _, p := range snapPaths {
		do(p, adminJWT) // warm-up, not reported
		counts := map[string]int{}
		var durs []time.Duration
		var size int
		for i := 0; i < snapN; i++ {
			r := do(p, adminJWT)
			counts[pcClass(r)]++
			if r.status == http.StatusOK {
				durs = append(durs, r.dur)
				size = len(r.body)
				if !strings.Contains(r.body, `"snapshot"`) {
					t.Errorf("snapshot read %s: 200 without a snapshot member", p)
				}
			}
		}
		logf("SNAPSHOT-READ path=%s statuses[%s] bytes=%d %s", p, pcCounts(counts), size, pcStats(durs))
		if counts["200"] != snapN {
			t.Errorf("snapshot read %s: %d of %d answered 200", p, counts["200"], snapN)
		}
	}

	// Step 4: live latency, idle.
	liveN := pcEnvInt("UZI_PRECOMPUTE_EVIDENCE_LIVE_N", 12)
	type liveScope struct{ name, path, jwt string }
	scopes := []liveScope{
		{"admin-all-users-30d", "/api/admin/analytics/spend?window=30d", adminJWT},
		{"owner-30d", "/api/analytics/spend?window=30d", ownerJWT},
	}
	for _, sc := range scopes {
		do(sc.path, sc.jwt)
		counts := map[string]int{}
		var durs []time.Duration
		for i := 0; i < liveN; i++ {
			r := do(sc.path, sc.jwt)
			counts[pcClass(r)]++
			if r.status == http.StatusOK {
				durs = append(durs, r.dur)
			}
		}
		logf("LIVE-IDLE scope=%s statuses[%s] %s", sc.name, pcCounts(counts), pcStats(durs))
	}

	// Step 5: live latency while a build runs. Sampling opens at beforeVariant(2) and closes at
	// afterVariant(24). Before each request the build backend is probed in pg_stat_activity (from
	// another pooled connection) and the request is issued only when it is state='active'; a probe
	// that finds it in any other state is counted as skipped and no request is made. Only issued
	// requests are reported. The build can still end between the probe and the request.
	type duringSample struct {
		scope string
		r     pcReq
	}
	started, stop := make(chan struct{}), make(chan struct{})
	var startedOnce, stopOnce atomic.Bool
	done := make(chan buildResult, 1)
	go func() {
		done <- buildOnce(func(i int, _ string) {
			if i == 2 && startedOnce.CompareAndSwap(false, true) {
				close(started)
			}
		}, func(i int, _ string) {
			if i == len(keys)-1 && stopOnce.CompareAndSwap(false, true) {
				close(stop)
			}
		})
	}()
	var samples []duringSample
	var build buildResult
	finished := false
	skipped := map[string]int{}
	// Wait for the window to open (or the build to end first).
	select {
	case <-started:
	case build = <-done:
		finished = true
	}
sampling:
	for !finished {
		select {
		case <-stop:
			break sampling
		case build = <-done:
			finished = true
			break sampling
		default:
		}
		sc := scopes[len(samples)%2] // by issued samples, so skipped probes cannot starve a scope
		state := "unknown"
		if pid := buildPID.Load(); pid != 0 {
			if qerr := prod.QueryRow(ctx, `SELECT coalesce((SELECT state FROM pg_stat_activity WHERE pid = $1), 'gone')`, pid).Scan(&state); qerr != nil {
				state = "error"
			}
		}
		if state != "active" {
			skipped[state]++
			time.Sleep(20 * time.Millisecond)
			continue
		}
		samples = append(samples, duringSample{scope: sc.name, r: do(sc.path, sc.jwt)})
	}
	if !finished {
		build = <-done
	}
	if build.err != nil {
		t.Errorf("during-build: build failed: %v", build.err)
	}
	nSkipped := 0
	for _, n := range skipped {
		nSkipped += n
	}
	logf("DURING-BUILD build total=%.2fs run_build=%.2fs backend_pid=%d issued=%d skipped_non_active=%d skipped_states[%s]",
		build.total.Seconds(), build.run.Seconds(), build.pid, len(samples), nSkipped, pcCounts(skipped))
	for _, sc := range append(scopes, liveScope{name: "all-scopes"}) {
		counts := map[string]int{}
		var ok []time.Duration
		n := 0
		for _, s := range samples {
			if sc.name != "all-scopes" && s.scope != sc.name {
				continue
			}
			n++
			counts[pcClass(s.r)]++
			if s.r.status == http.StatusOK {
				ok = append(ok, s.r.dur)
			}
		}
		logf("DURING-BUILD scope=%s issued=%d statuses[%s] ok: %s", sc.name, n, pcCounts(counts), pcStats(ok))
		if n == 0 {
			t.Errorf("DURING-BUILD scope=%s: no request was issued against an active build backend; the during-build window was not observed", sc.name)
		}
	}
	for i, s := range samples {
		logf("DURING-BUILD sample=%d scope=%s class=%s ms=%.1f", i+1, s.scope, pcClass(s.r), pcMs(s.r.dur))
	}
}
