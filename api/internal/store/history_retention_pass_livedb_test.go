package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/historyretention"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// history_retention_pass_livedb_test.go runs the production historyretention.Pass against
// PostgreSQL (issue #4695): the off switch and the cross-replica lock, each with a positive
// control on the same rows so a vacuous "deleted nothing" cannot pass. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// hrSeedMixed gives every table 5 expired rows (48h old), 3 fresh rows (the current bucket) and,
// for pr_history_events, 2 expired first_merge rows, plus 3 expired pr_history_heads rows with every
// column populated (which no pass may touch). It returns the current bucket and the
// expired total a 24h pass deletes.
func hrSeedMixed(f *hrFx) (fresh time.Time, expired int64) {
	fresh = time.Now().UTC().Truncate(5 * time.Minute)
	old := fresh.Add(-48 * time.Hour)
	for _, table := range hrTables {
		f.seed(table, 5, old)
		f.seed(table, 3, fresh)
		expired += 5
	}
	f.seedFirstMerge(2, old)
	// Expired heads (older than the 24h cutoff): the pass must leave them untouched.
	f.seedHeads(3, old)
	return fresh, expired
}

// hrWantHeadsUnchanged fails when pr_history_heads differs by value from the snapshot before.
func hrWantHeadsUnchanged(t *testing.T, f *hrFx, before []string, when string) {
	t.Helper()
	if diff := hrHeadsEqual(before, f.heads()); diff != "" {
		t.Errorf("pr_history_heads changed %s: %s", when, diff)
	}
}

func hrWantAfterPass(t *testing.T, f *hrFx, fresh time.Time) {
	t.Helper()
	for _, table := range hrTables {
		want := 3
		if table == store.HistoryRetentionPRHistoryEvents {
			want = 3 + 2 // fresh events plus the protected first_merge ones
		}
		if got := f.total(table); got != want {
			t.Errorf("%s has %d rows after the pass, want %d", table, got, want)
		}
		if got := f.countAt(table, fresh); got != 3 {
			t.Errorf("%s has %d fresh rows, want all 3 kept", table, got)
		}
	}
}

// TestHistoryRetentionPassLiveDB runs the production pass on one shared database, one subtest per
// case (each starts from emptied history tables).
func TestHistoryRetentionPassLiveDB(t *testing.T) {
	root := newHrFx(t)
	root.run(t, "off then on", hrPassOffThenOn)
	root.run(t, "contended", hrPassContended)
	root.run(t, "capacity sampler stamps the current bucket", hrCapacitySamplerBucket)
}

func hrPassOffThenOn(t *testing.T, f *hrFx) {
	fresh, expired := hrSeedMixed(f)
	heads := f.heads()
	if len(heads) != 3 {
		t.Fatalf("seeded %d heads, want 3", len(heads))
	}
	before := map[store.HistoryRetentionTable]int{}
	for _, table := range hrTables {
		before[table] = f.total(table)
	}

	off := historyretention.New(f.pool, 0)
	if n, err := off.Pass(f.ctx); n != 0 || err != nil {
		t.Fatalf("retention off: Pass = (%d, %v), want (0, nil)", n, err)
	}
	for _, table := range hrTables {
		if got := f.total(table); got != before[table] {
			t.Fatalf("retention off changed %s: %d rows, want %d", table, got, before[table])
		}
	}

	hrWantHeadsUnchanged(t, f, heads, "with retention off")

	// positive control: the same rows, enabled
	on := historyretention.New(f.pool, 24*time.Hour)
	n, err := on.Pass(f.ctx)
	if err != nil || n != expired {
		t.Fatalf("retention on: Pass = (%d, %v), want (%d, nil)", n, err, expired)
	}
	hrWantAfterPass(t, f, fresh)
	hrWantHeadsUnchanged(t, f, heads, "by the pass")
}

func hrPassContended(t *testing.T, f *hrFx) {
	fresh, expired := hrSeedMixed(f)
	heads := f.heads()
	if len(heads) != 3 {
		t.Fatalf("seeded %d heads, want 3", len(heads))
	}
	pass := historyretention.New(f.pool, 24*time.Hour)

	holder, got, err := store.OpenHistoryRetentionSession(f.ctx, f.ctx, f.pool, time.Second, time.Second)
	if err != nil || !got {
		t.Fatalf("holder open: got=%v err=%v", got, err)
	}
	t.Cleanup(func() { _, _ = holder.Close(context.Background()) })
	before := map[store.HistoryRetentionTable]int{}
	for _, table := range hrTables {
		before[table] = f.total(table)
	}
	if n, err := pass.Pass(f.ctx); n != 0 || err != nil {
		t.Fatalf("contended: Pass = (%d, %v), want (0, nil)", n, err)
	}
	for _, table := range hrTables {
		if got := f.total(table); got != before[table] {
			t.Fatalf("a contended pass changed %s: %d rows, want %d", table, got, before[table])
		}
	}

	// positive control: release the lock and the same pass deletes
	if unlocked, err := holder.Close(f.ctx); err != nil || !unlocked {
		t.Fatalf("holder close = (%v, %v), want a proven unlock", unlocked, err)
	}
	n, err := pass.Pass(f.ctx)
	if err != nil || n != expired {
		t.Fatalf("after release: Pass = (%d, %v), want (%d, nil)", n, err, expired)
	}
	hrWantAfterPass(t, f, fresh)
	hrWantHeadsUnchanged(t, f, heads, "by the pass")
	if n := f.lockHolders(); n != 0 {
		t.Fatalf("%d sessions hold the lock after the pass, want 0", n)
	}
}

// hrCapacitySamplerBucket characterizes the UNCHANGED capacity sampler: it stamps the database
// clock, so the sample it writes is in the current five-minute bucket.
func hrCapacitySamplerBucket(t *testing.T, f *hrFx) {
	owner := f.user()
	hb := time.Now().UTC()
	w := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	row, err := f.q.SampleWorkerCapacityBatch(f.ctx, store.SampleWorkerCapacityBatchParams{
		StaleAfter: pgtype.Interval{Microseconds: wcsStaleAfter.Microseconds(), Valid: true},
		BatchSize:  10,
	})
	// Workers outlive the per-subtest truncate, so other fresh workers may be sampled in the same
	// batch: assert on this subtest's own worker only.
	if err != nil || row.Inserted < 1 {
		t.Fatalf("sampler = (%+v, %v), want at least 1 inserted", row, err)
	}
	var n int
	var micros int64
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*), coalesce(max((extract(epoch FROM (now() - bucket_start)) * 1000000)::bigint), 0) FROM worker_capacity_samples WHERE worker_id = $1`, w).Scan(&n, &micros); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d samples for the subtest's worker, want 1", n)
	}
	age := time.Duration(micros) * time.Microsecond
	if age < 0 || age > 5*time.Minute+time.Minute {
		t.Errorf("sample bucket age = %v, want the current five-minute bucket (0..5m)", age)
	}
}

// TestHistoryRetentionWritersAdmitOnlyInWindowLiveDB characterizes the UNCHANGED writers (the
// retention change touches none): the provider usage copies admit a reading only when it is
// newer than one hour on the database clock, and the capacity sampler stamps the database clock,
// so no writer produces a row older than about 65 minutes (the capacity sampler half is
// hrCapacitySamplerBucket, in TestHistoryRetentionPassLiveDB). That is what keeps a retention of
// 24h or more from deleting a row a writer has just admitted.
func TestHistoryRetentionWritersAdmitOnlyInWindowLiveDB(t *testing.T) {
	const maxAge = 65 * time.Minute

	t.Run("anthropic usage copy", func(t *testing.T) {
		ctx, pool, q, user := rateLimitEnablementDB(t)
		for _, tc := range []struct {
			name string
			age  time.Duration
			want int64
		}{
			{"2h old reading is not admitted", 2 * time.Hour, 0},
			{"61m old reading is not admitted", 61 * time.Minute, 0},
			{"59m old reading is admitted", 59 * time.Minute, 1},
		} {
			sec := enablementToken(ctx, t, q, user, "hr-"+uuid.NewString(), false)
			rev := tokenRev(ctx, t, q, user, sec)
			p := anthGauge(ctx, t, q, user, sec, rev, time.Now().UTC().Add(-tc.age), nil)
			if n := copyAnth(ctx, t, q, p); n != tc.want {
				t.Errorf("%s: copy = %d rows, want %d", tc.name, n, tc.want)
			}
		}
		var tooOld int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM anthropic_usage_samples WHERE user_id = $1 AND now() - bucket_start > $2::interval`,
			user, pgtype.Interval{Microseconds: maxAge.Microseconds(), Valid: true}).Scan(&tooOld); err != nil || tooOld != 0 {
			t.Errorf("anthropic samples older than %v: %d (err %v), want 0", maxAge, tooOld, err)
		}
		if got := countRows(ctx, t, pool, "anthropic_usage_samples", user); got != 1 {
			t.Errorf("anthropic samples = %d, want exactly the one admitted", got)
		}
	})

	t.Run("codex usage copy", func(t *testing.T) {
		ctx, pool, q, user := codexLiveDB(t)
		for _, tc := range []struct {
			name string
			age  time.Duration
			want int64
		}{
			{"2h old reading is not admitted", 2 * time.Hour, 0},
			{"59m old reading is admitted", 59 * time.Minute, 1},
		} {
			fx := codexGauge(ctx, t, pool, q, user, `[]`)
			var ts time.Time
			if err := pool.QueryRow(ctx, `UPDATE codex_account_rate_limits
				SET last_success_at = now() - $3::interval, last_attempt_at = now() - $3::interval
				WHERE user_id = $1 AND provider_account_id = $2 RETURNING last_success_at`,
				user, fx.acc, pgtype.Interval{Microseconds: tc.age.Microseconds(), Valid: true}).Scan(&ts); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if n := copyCodex(ctx, t, q, fx.p, tsz(ts)); n != tc.want {
				t.Errorf("%s: copy = %d rows, want %d", tc.name, n, tc.want)
			}
		}
		var tooOld int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_usage_samples WHERE user_id = $1 AND now() - bucket_start > $2::interval`,
			user, pgtype.Interval{Microseconds: maxAge.Microseconds(), Valid: true}).Scan(&tooOld); err != nil || tooOld != 0 {
			t.Errorf("codex samples older than %v: %d (err %v), want 0", maxAge, tooOld, err)
		}
		if got := countRows(ctx, t, pool, "codex_usage_samples", user); got != 1 {
			t.Errorf("codex samples = %d, want exactly the one admitted", got)
		}
	})
}
