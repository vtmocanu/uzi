//go:build analyticsevidence

package handler

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700 (M4): the admin all-users Spend window=all read path under each fold-state
// condition, through the real handler (see analytics_reads_evidence_livedb_test.go for the
// sampler). Compiled only with -tags analyticsevidence; runs only with
// UZI_ANALYTICS_FOLDS_EVIDENCE=1 against a throwaway database (marker required). It rewrites
// run_usage_fold_state / run_usage_model_folds of the seed. The window=all request is refused
// by a tree that has not removed the all-users guard (the M5 change); run it from an export
// that has.
//
// Conditions (UZI_FOLDS_CONDS, comma list, applied and sampled in order):
//
//	missing    DELETE every state row (fresh post-migration state: every run is read live)
//	backfill   missing, then the production UsageFolder (NewUsageFolder default options) Pass by
//	           Pass until nothing is left to fold; times every Pass; creates the restore point
//	fresh      restore point (every eligible run folded and fresh)
//	stale      restore point, then every state row marked stale (fold rows kept, read live)
//	mixedNN    restore point, then NN percent of the eligible runs fall back to the live read:
//	           half of them keep a stale state row, half lose their state row (no state)
//
// The restore point (tables ev_fold_state_bak / ev_fold_rows_bak) is created by backfill and
// replaced every time backfill runs. Every condition ends with VACUUM (ANALYZE) of the two fold
// tables, as autovacuum would after a real backfill.
//
//	UZI_FOLDS_APPLY_ONLY=1  apply the condition and stop (for a separate EXPLAIN run)
//	UZI_READS_N / UZI_READS_WARM / UZI_READS_MODES / UZI_READS_LABEL as in the reads file
//	UZI_FOLDS_MAX_PASSES    safety bound for backfill (default 5000)
func TestAnalyticsFoldsEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_FOLDS_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_FOLDS_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	dsn := rdRequireThrowaway(t)
	ctx := context.Background()
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	label := os.Getenv("UZI_READS_LABEL")
	n, warm := rdEnvInt("UZI_READS_N", 12), rdEnvInt("UZI_READS_WARM", 2)

	for _, cond := range strings.Split(os.Getenv("UZI_FOLDS_CONDS"), ",") {
		cond = strings.TrimSpace(cond)
		if cond == "" {
			continue
		}
		fbPct := ffApply(ctx, t, pool, cond)
		ffVacuum(ctx, t, pool)
		ffCounts(ctx, t, pool, label, cond, fbPct)
		if os.Getenv("UZI_FOLDS_APPLY_ONLY") == "1" {
			continue
		}
		hashes := map[string]string{}
		for _, mode := range rdModes() {
			t.Run(cond+"/"+mode, func(t *testing.T) {
				s := rdNewSession(t, mode)
				for _, c := range rdFilter(rdWindowAllCases()) {
					sum := s.measure(label+"/"+cond, c, n, warm)
					if mode == "auto" {
						s.planModes(label + "/" + cond + "/" + c.name)
					}
					if len(sum.hashes) != 1 {
						t.Errorf("%s %s %s: %d distinct response bodies", cond, mode, c.name, len(sum.hashes))
					}
					for h := range sum.hashes {
						if prev, ok := hashes[c.name]; ok && prev != h {
							t.Errorf("%s %s %s: body hash %s differs from %s of the first mode in this condition", cond, mode, c.name, h, prev)
						}
						hashes[c.name] = h
					}
				}
			})
		}
	}
}

func ffExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	tag, err := pool.Exec(ctx, sql)
	if err != nil {
		t.Fatalf("exec %.160q: %v", sql, err)
	}
	return tag.RowsAffected()
}

// ffBucket is a stable per-run bucket 0..99 (runs ids are random uuids, so it is uniform).
const ffBucket = `((hashtext(run_id::text)::bigint & 2147483647) % 100)`

// ffApply puts the seed into cond and returns the fallback percentage of the eligible runs.
func ffApply(ctx context.Context, t *testing.T, pool *pgxpool.Pool, cond string) int {
	t.Helper()
	switch {
	case cond == "missing":
		rdLogf("FOLDSAPPLY cond=missing deleted_state_rows=%d", ffExec(ctx, t, pool, `DELETE FROM run_usage_fold_state`))
		return 100
	case cond == "backfill":
		ffExec(ctx, t, pool, `DELETE FROM run_usage_fold_state`)
		ffBackfill(ctx, t, pool)
		ffExec(ctx, t, pool, `DROP TABLE IF EXISTS ev_fold_state_bak`)
		ffExec(ctx, t, pool, `DROP TABLE IF EXISTS ev_fold_rows_bak`)
		ffExec(ctx, t, pool, `CREATE TABLE ev_fold_state_bak AS SELECT * FROM run_usage_fold_state`)
		ffExec(ctx, t, pool, `CREATE TABLE ev_fold_rows_bak AS SELECT * FROM run_usage_model_folds`)
		return 0
	}
	ffRestore(ctx, t, pool)
	switch {
	case cond == "fresh":
		return 0
	case cond == "stale":
		rdLogf("FOLDSAPPLY cond=stale marked_stale=%d", ffExec(ctx, t, pool, `UPDATE run_usage_fold_state SET stale = true`))
		return 100
	case strings.HasPrefix(cond, "mixed"):
		pct, err := strconv.Atoi(strings.TrimPrefix(cond, "mixed"))
		if err != nil || pct < 1 || pct > 99 {
			t.Fatalf("bad condition %q (mixedNN, NN 1..99)", cond)
		}
		st := ffExec(ctx, t, pool, fmt.Sprintf(`UPDATE run_usage_fold_state SET stale = true WHERE %[1]s < %[2]d AND %[1]s %% 2 = 0`, ffBucket, pct))
		del := ffExec(ctx, t, pool, fmt.Sprintf(`DELETE FROM run_usage_fold_state WHERE %[1]s < %[2]d AND %[1]s %% 2 = 1`, ffBucket, pct))
		rdLogf("FOLDSAPPLY cond=%s marked_stale=%d deleted_state_rows=%d", cond, st, del)
		return pct
	}
	t.Fatalf("unknown condition %q", cond)
	return 0
}

func ffRestore(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var have int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('ev_fold_state_bak', 'ev_fold_rows_bak')`).Scan(&have); err != nil || have != 2 {
		t.Fatalf("no restore point (err %v, %d of 2 tables): run the backfill condition first", err, have)
	}
	// TRUNCATE does not fire the AFTER DELETE trigger; run_usage_model_folds is truncated with it.
	ffExec(ctx, t, pool, `TRUNCATE run_usage_fold_state CASCADE`)
	ffExec(ctx, t, pool, `INSERT INTO run_usage_fold_state SELECT * FROM ev_fold_state_bak`)
	ffExec(ctx, t, pool, `INSERT INTO run_usage_model_folds SELECT * FROM ev_fold_rows_bak`)
}

func ffVacuum(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ffExec(ctx, t, pool, `VACUUM (ANALYZE) run_usage_fold_state`)
	ffExec(ctx, t, pool, `VACUUM (ANALYZE) run_usage_model_folds`)
}

const ffEligible = `r.status IN ('completed', 'failed', 'cancelled') AND r.kind <> 'chat' AND r.usage_refolded`

// ffCounts logs how the eligible runs are distributed over the fold states.
func ffCounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, label, cond string, fbPct int) {
	t.Helper()
	var runs, elig, none, stale, fresh, foldRows int64
	err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM runs),
	    count(*) FILTER (WHERE `+ffEligible+`),
	    count(*) FILTER (WHERE `+ffEligible+` AND fs.run_id IS NULL),
	    count(*) FILTER (WHERE `+ffEligible+` AND fs.stale),
	    count(*) FILTER (WHERE `+ffEligible+` AND fs.run_id IS NOT NULL AND NOT fs.stale),
	    (SELECT count(*) FROM run_usage_model_folds)
	  FROM runs r LEFT JOIN run_usage_fold_state fs ON fs.run_id = r.id`).Scan(&runs, &elig, &none, &stale, &fresh, &foldRows)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	rdLogf("FOLDSSTATE label=%s cond=%s runs=%d eligible=%d no_state=%d stale=%d fresh=%d fold_rows=%d fresh_pct=%.1f fallback_pct=%.1f (nominal fallback %d%%)",
		label, cond, runs, elig, none, stale, fresh, foldRows, 100*float64(fresh)/float64(elig), 100*float64(none+stale)/float64(elig), fbPct)
}

func ffRemaining(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var rem int64
	err := pool.QueryRow(ctx, `SELECT count(*) FROM runs r LEFT JOIN run_usage_fold_state fs ON fs.run_id = r.id
	  WHERE `+ffEligible+` AND (fs.run_id IS NULL OR fs.stale)`).Scan(&rem)
	return rem, err
}

// ffBackfill folds every eligible run with the production UsageFolder, one Pass at a time, and
// sums the Pass durations. Production runs one Pass per sweeper tick (15 s by default).
func ffBackfill(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	folder := store.NewUsageFolder(pool, store.UsageFolderOptions{})
	maxPasses := rdEnvInt("UZI_FOLDS_MAX_PASSES", 5000)
	start0, err := ffRemaining(ctx, pool)
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	var passes int
	var total int64
	var sum, longest time.Duration
	var zero int
	wall := time.Now()
	for passes < maxPasses {
		passes++
		start := time.Now()
		n, perr := folder.Pass(ctx)
		d := time.Since(start)
		sum += d
		if d > longest {
			longest = d
		}
		total += n
		rdLogf("BACKFILLPASS pass=%d folded=%d ms=%.1f err=%v", passes, n, ms(d), perr)
		if n == 0 {
			zero++
		} else {
			zero = 0
		}
		if passes%10 == 0 || n == 0 {
			rem, err := ffRemaining(ctx, pool)
			if err != nil {
				t.Fatalf("remaining: %v", err)
			}
			if rem == 0 {
				break
			}
			if zero >= 10 {
				t.Errorf("backfill stalled: %d remaining after %d passes, 10 consecutive empty Passes", rem, passes)
				break
			}
		}
	}
	rem, _ := ffRemaining(ctx, pool)
	rdLogf("BACKFILLSUM eligible_unfolded_at_start=%d passes=%d runs_folded=%d remaining=%d sum_pass_ms=%.0f mean_pass_ms=%.1f longest_pass_ms=%.1f mean_folded_per_pass=%.1f harness_wall_ms=%.0f implied_wall_at_15s_tick_s=%.0f",
		start0, passes, total, rem, ms(sum), ms(sum)/float64(passes), ms(longest), float64(total)/float64(passes), ms(time.Since(wall)), 15*float64(passes))
}
