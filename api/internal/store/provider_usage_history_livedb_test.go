package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// These LiveDB tests pin issue #4692 M1: the two best-effort history copies
// (CopyAnthropicUsageSample / CopyCodexUsageSample) and the 00547 tables. Every fence test
// goes through the production generated functions and isolates ONE guard: the gauge is
// written by the real upsert, and the copy is then called with parameters that differ from
// the gauge in exactly one respect (or the gauge is moved on by exactly one real write).

const (
	histBucket = 5 * time.Minute
)

// histPast is a bucket-aligned instant in the past (the start of the previous five-minute
// bucket, plus offset), so two timestamps inside it share a bucket and stay admissible.
func histPast(back time.Duration, offset time.Duration) time.Time {
	return time.Now().UTC().Truncate(histBucket).Add(-back).Add(offset).Truncate(time.Microsecond)
}

// awaitBucketRoom sleeps past a bucket boundary that is about to arrive, so a test that
// relies on two writes landing in one bucket cannot straddle it.
func awaitBucketRoom(t *testing.T) {
	t.Helper()
	next := time.Now().UTC().Truncate(histBucket).Add(histBucket)
	if d := time.Until(next); d < 8*time.Second {
		time.Sleep(d + 500*time.Millisecond)
	}
}

func tsz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func i2(v int16) pgtype.Int2 { return pgtype.Int2{Int16: v, Valid: true} }

func tokenRev(ctx context.Context, t *testing.T, q *store.Queries, user, id uuid.UUID) int64 {
	t.Helper()
	row, err := q.GetSecretEnablement(ctx, store.GetSecretEnablementParams{ID: id, UserID: user})
	if err != nil {
		t.Fatalf("GetSecretEnablement: %v", err)
	}
	return row.EnablementRev
}

// anthGauge writes one gauge reading through the production UpsertRateLimits and returns the
// exact parameters it passed. mut adjusts the default reading before the write.
func anthGauge(ctx context.Context, t *testing.T, q *store.Queries, user, sec uuid.UUID, rev int64, at time.Time, mut func(*store.UpsertRateLimitsParams)) store.UpsertRateLimitsParams {
	t.Helper()
	p := store.UpsertRateLimitsParams{
		UserSecretID: sec, UserID: user, EnablementRev: rev,
		FiveHourPct:      i2(40),
		FiveHourResetsAt: tsz(at.Add(2 * time.Hour).Truncate(time.Second)),
		SevenDayPct:      i2(55),
		SevenDayResetsAt: tsz(at.Add(48 * time.Hour).Truncate(time.Second)),
		Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:         tsz(at),
	}
	if mut != nil {
		mut(&p)
	}
	n, err := q.UpsertRateLimits(ctx, p)
	if err != nil || n != 1 {
		t.Fatalf("UpsertRateLimits = (%d, %v), want (1, nil)", n, err)
	}
	return p
}

func copyAnth(ctx context.Context, t *testing.T, q *store.Queries, p store.UpsertRateLimitsParams) int64 {
	t.Helper()
	n, err := q.CopyAnthropicUsageSample(ctx, store.CopyAnthropicUsageSampleParamsFrom(p))
	if err != nil {
		t.Fatalf("CopyAnthropicUsageSample: %v", err)
	}
	return n
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func wantBucketStart(t time.Time) time.Time { return t.UTC().Truncate(histBucket) }

func TestAnthropicUsageCopyHappyPathLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	sec := enablementToken(ctx, t, q, user, "happy", true)
	rev := tokenRev(ctx, t, q, user, sec)

	at1 := histPast(histBucket, 10*time.Second)
	p1 := anthGauge(ctx, t, q, user, sec, rev, at1, nil)
	if n := copyAnth(ctx, t, q, p1); n != 1 {
		t.Fatalf("first copy = %d rows, want 1", n)
	}
	var (
		observed, bucket     time.Time
		gotRev               int64
		source               string
		fivePct, sevenPct    int16
		fiveReset, sevenRest time.Time
	)
	if err := pool.QueryRow(ctx, `SELECT observed_at, bucket_start, enablement_rev, source, five_hour_pct,
		five_hour_resets_at, seven_day_pct, seven_day_resets_at FROM anthropic_usage_samples
		WHERE user_id = $1 AND user_secret_id = $2`, user, sec).
		Scan(&observed, &bucket, &gotRev, &source, &fivePct, &fiveReset, &sevenPct, &sevenRest); err != nil {
		t.Fatalf("read sample: %v", err)
	}
	if !observed.Equal(at1) || !bucket.Equal(wantBucketStart(at1)) || gotRev != rev || source != "usage_endpoint" ||
		fivePct != 40 || sevenPct != 55 || !fiveReset.Equal(p1.FiveHourResetsAt.Time) || !sevenRest.Equal(p1.SevenDayResetsAt.Time) {
		t.Fatalf("sample = observed %v bucket %v rev %d source %s pct %d/%d resets %v/%v; want the accepted reading in bucket %v",
			observed, bucket, gotRev, source, fivePct, sevenPct, fiveReset, sevenRest, wantBucketStart(at1))
	}

	// Same bucket: repeating the copy is a no-op.
	if n := copyAnth(ctx, t, q, p1); n != 0 {
		t.Fatalf("repeat copy in the same bucket = %d rows, want 0", n)
	}
	// A later accepted reading in the SAME bucket also loses: the first accepted copy wins.
	at2 := at1.Add(10 * time.Second)
	p2 := anthGauge(ctx, t, q, user, sec, rev, at2, func(p *store.UpsertRateLimitsParams) { p.SevenDayPct = i2(70) })
	if n := copyAnth(ctx, t, q, p2); n != 0 {
		t.Fatalf("second reading in the same bucket copied %d rows, want 0 (first wins)", n)
	}
	var pct int16
	if err := pool.QueryRow(ctx, `SELECT seven_day_pct FROM anthropic_usage_samples WHERE user_id = $1`, user).Scan(&pct); err != nil || pct != 55 {
		t.Fatalf("stored seven_day_pct = (%d, %v), want the first reading's 55", pct, err)
	}
	// The next bucket gets its own row.
	p3 := anthGauge(ctx, t, q, user, sec, rev, at1.Add(histBucket), func(p *store.UpsertRateLimitsParams) { p.SevenDayPct = i2(71) })
	if n := copyAnth(ctx, t, q, p3); n != 1 {
		t.Fatalf("next-bucket copy = %d rows, want 1", n)
	}
	if got := countRows(ctx, t, pool, "anthropic_usage_samples", user); got != 2 {
		t.Fatalf("rows = %d, want 2 (one per bucket)", got)
	}
}

func TestAnthropicUsageCopyNullFiveHourLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	sec := enablementToken(ctx, t, q, user, "nofive", true)
	rev := tokenRev(ctx, t, q, user, sec)
	p := anthGauge(ctx, t, q, user, sec, rev, histPast(histBucket, 5*time.Second), func(p *store.UpsertRateLimitsParams) {
		p.FiveHourPct = pgtype.Int2{}
		p.FiveHourResetsAt = pgtype.Timestamptz{}
		p.SevenDayResetsAt = pgtype.Timestamptz{}
	})
	if n := copyAnth(ctx, t, q, p); n != 1 {
		t.Fatalf("copy of a reading without a five-hour window = %d rows, want 1", n)
	}
	var five pgtype.Int2
	var fiveReset, sevenReset pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT five_hour_pct, five_hour_resets_at, seven_day_resets_at
		FROM anthropic_usage_samples WHERE user_id = $1`, user).Scan(&five, &fiveReset, &sevenReset); err != nil {
		t.Fatal(err)
	}
	if five.Valid || fiveReset.Valid || sevenReset.Valid {
		t.Fatalf("stored five=%+v fiveReset=%+v sevenReset=%+v, want NULLs, never a fabricated 0", five, fiveReset, sevenReset)
	}
}

// TestAnthropicUsageCopyFencesLiveDB: each case moves ONE thing between the accepted write
// and the copy, and the copy must select nothing.
func TestAnthropicUsageCopyFencesLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, fmt.Sprintf("hist-other-%s@e2e", other))

	type fixture struct {
		sec uuid.UUID
		rev int64
		p   store.UpsertRateLimitsParams
	}
	fresh := func(label string) fixture {
		sec := enablementToken(ctx, t, q, user, label+"-"+uuid.NewString(), false)
		rev := tokenRev(ctx, t, q, user, sec)
		return fixture{sec: sec, rev: rev, p: anthGauge(ctx, t, q, user, sec, rev, histPast(histBucket, 15*time.Second), nil)}
	}
	expectZero := func(name string, p store.UpsertRateLimitsParams) {
		t.Helper()
		if n := copyAnth(ctx, t, q, p); n != 0 {
			t.Errorf("%s: copy = %d rows, want 0", name, n)
		}
	}

	// Binding: wrong owner (same token id), and the same owner's other token.
	f := fresh("bind")
	p := f.p
	p.UserID = other
	expectZero("wrong owner, same user_secret_id", p)
	p = f.p
	p.UserSecretID = enablementToken(ctx, t, q, user, "no-gauge-"+uuid.NewString(), false)
	expectZero("same owner, other token (no gauge row)", p)

	// Each reading column alone.
	for name, mut := range map[string]func(*store.UpsertRateLimitsParams){
		"source equality alone": func(p *store.UpsertRateLimitsParams) { p.Source = pgtype.Text{String: "header_probe", Valid: true} },
		"synced_at alone":       func(p *store.UpsertRateLimitsParams) { p.SyncedAt = tsz(p.SyncedAt.Time.Add(time.Microsecond)) },
		"five_hour_pct alone":   func(p *store.UpsertRateLimitsParams) { p.FiveHourPct = i2(41) },
		"five_hour_pct null":    func(p *store.UpsertRateLimitsParams) { p.FiveHourPct = pgtype.Int2{} },
		"five_hour_reset alone": func(p *store.UpsertRateLimitsParams) {
			p.FiveHourResetsAt = tsz(p.FiveHourResetsAt.Time.Add(time.Second))
		},
		"seven_day_pct alone": func(p *store.UpsertRateLimitsParams) { p.SevenDayPct = i2(56) },
		"seven_day_pct null":  func(p *store.UpsertRateLimitsParams) { p.SevenDayPct = pgtype.Int2{} },
		"seven_day_reset alone": func(p *store.UpsertRateLimitsParams) {
			p.SevenDayResetsAt = tsz(p.SevenDayResetsAt.Time.Add(time.Second))
		},
		"enablement_rev alone": func(p *store.UpsertRateLimitsParams) { p.EnablementRev++ },
	} {
		g := fresh("col")
		p := g.p
		mut(&p)
		expectZero(name, p)
	}

	// The limit_report exclusion alone: the gauge says limit_report with the SAME pcts and the
	// copy passes limit_report too, so source equality holds and only the exclusion refuses it.
	g := fresh("lr")
	mustExec(ctx, t, pool, `UPDATE anthropic_rate_limits SET source = 'limit_report' WHERE user_secret_id = $1`, g.sec)
	p = g.p
	p.Source = pgtype.Text{String: "limit_report", Valid: true}
	expectZero("limit_report exclusion alone", p)

	// A NULL gauge source never matches (even a NULL parameter).
	g = fresh("nullsrc")
	mustExec(ctx, t, pool, `UPDATE anthropic_rate_limits SET source = NULL WHERE user_secret_id = $1`, g.sec)
	p = g.p
	p.Source = pgtype.Text{}
	expectZero("NULL gauge source", p)

	// The real exhaustion marks keep synced_at but write limit_report and the pct (example 3).
	g = fresh("mark5")
	if n, err := q.MarkFiveHourExhausted(ctx, g.sec); err != nil || n != 1 {
		t.Fatalf("MarkFiveHourExhausted = (%d, %v)", n, err)
	}
	expectZero("after MarkFiveHourExhausted, original params", g.p)
	g = fresh("mark7")
	if n, err := q.MarkSevenDayExhausted(ctx, g.sec); err != nil || n != 1 {
		t.Fatalf("MarkSevenDayExhausted = (%d, %v)", n, err)
	}
	expectZero("after MarkSevenDayExhausted, original params", g.p)

	// Disable, then re-enable: the revision moves, so the old reading's params no longer match.
	g = fresh("toggle")
	setEnabled(ctx, t, q, user, g.sec, false)
	expectZero("disabled", g.p)
	newRev := setEnabled(ctx, t, q, user, g.sec, true)
	if newRev == g.rev {
		t.Fatalf("re-enable did not move the revision (%d)", newRev)
	}
	expectZero("re-enabled at a newer revision, old params", g.p)
	moved := g.p
	moved.EnablementRev = newRev
	expectZero("re-enabled, new revision but the gauge still carries the old one", moved)

	// Race: a later accepted poll replaced the gauge before this copy ran.
	g = fresh("race")
	later := anthGauge(ctx, t, q, user, g.sec, g.rev, g.p.SyncedAt.Time.Add(20*time.Second), func(p *store.UpsertRateLimitsParams) { p.SevenDayPct = i2(77) })
	expectZero("gauge replaced by a later accepted poll", g.p)
	if n := copyAnth(ctx, t, q, later); n != 1 {
		t.Errorf("the replacing reading's own copy = %d rows, want 1", n)
	}

	// Nothing but the one legitimate copy landed.
	if got := countRows(ctx, t, pool, "anthropic_usage_samples", user); got != 1 {
		t.Fatalf("rows after the fence cases = %d, want 1 (only the legitimate race copy)", got)
	}
	if got := countRows(ctx, t, pool, "anthropic_usage_samples", other); got != 0 {
		t.Fatalf("the other owner has %d rows, want 0", got)
	}
}

// admissionWindowCases pins the exact bounds of both copies' admission window,
// `> now() - interval '1 hour'` and `<= now() + interval '15 minutes'` (issue #4755). Each
// case runs its timestamp setup and its copy in ONE transaction, so now() (the transaction
// start time) is the same instant in both and the boundary cases are exact: a `>=` lower
// bound admits the exact -1 hour case, a `<` upper bound rejects the exact +15 minutes case.
// The interval literals are single-field on purpose: PostgreSQL signs each field of an
// interval literal separately, so '-59 minutes 59 seconds' means -00:58:01. wantMicros is
// the offset each literal must produce, checked against the database so a sign slip fails.
var admissionWindowCases = []struct {
	name       string
	offset     string
	wantMicros int64
	want       int64
}{
	{"exactly now()-1h", "-1 hour", -3_600_000_000, 0},
	{"1µs inside now()-1h", "-3599.999999 seconds", -3_599_999_999, 1},
	{"older than 1h", "-61 minutes", -3_660_000_000, 0},
	{"exactly now()+15min", "15 minutes", 900_000_000, 1},
	{"1µs past now()+15min", "15 minutes 0.000001 seconds", 900_000_001, 0},
}

// admissionTx opens the per-case transaction. The rollback cleanup is registered after the
// fixture's pool.Close cleanup, so it runs first and a t.Fatal mid-transaction cannot leave
// pool.Close waiting on the acquired connection.
func admissionTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// assertAdmissionOffset checks, in the case's transaction, that ts sits exactly wantMicros
// from now().
func assertAdmissionOffset(ctx context.Context, t *testing.T, tx pgx.Tx, name string, ts time.Time, wantMicros int64) {
	t.Helper()
	var got int64
	if err := tx.QueryRow(ctx, `SELECT (extract(epoch FROM ($1::timestamptz - now())) * 1000000)::bigint`, ts).Scan(&got); err != nil {
		t.Fatalf("%s: offset: %v", name, err)
	}
	if got != wantMicros {
		t.Fatalf("%s: timestamp is %dµs from now(), want %dµs", name, got, wantMicros)
	}
}

func TestAnthropicUsageCopyAdmissionWindowLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	for _, tc := range admissionWindowCases {
		// A fresh token per case: a shared token would let the same-bucket ON CONFLICT turn an
		// expected-0 case green even with an admission guard removed.
		sec := enablementToken(ctx, t, q, user, "window-"+uuid.NewString(), false)
		rev := tokenRev(ctx, t, q, user, sec)

		tx := admissionTx(ctx, t, pool)
		qtx := q.WithTx(tx)
		var ts time.Time
		if err := tx.QueryRow(ctx, `SELECT now() + $1::interval`, tc.offset).Scan(&ts); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		assertAdmissionOffset(ctx, t, tx, tc.name, ts, tc.wantMicros)
		p := anthGauge(ctx, t, qtx, user, sec, rev, ts.UTC(), nil)
		if n := copyAnth(ctx, t, qtx, p); n != tc.want {
			t.Errorf("%s: copy = %d rows, want %d", tc.name, n, tc.want)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("%s: rollback: %v", tc.name, err)
		}
	}
}

// TestAnthropicUsageGapNotZeroLiveDB is example 1: a refused (fenced) poll in the middle
// leaves no row, never a zero row.
func TestAnthropicUsageGapNotZeroLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	sec := enablementToken(ctx, t, q, user, "gap", true)
	rev := tokenRev(ctx, t, q, user, sec)

	b1, b2, b3 := histPast(4*histBucket, 20*time.Second), histPast(3*histBucket, 20*time.Second), histPast(2*histBucket, 20*time.Second)
	if n := copyAnth(ctx, t, q, anthGauge(ctx, t, q, user, sec, rev, b1, nil)); n != 1 {
		t.Fatalf("bucket 1 copy = %d, want 1", n)
	}
	// Bucket 2: the poll's write is refused by the revision fence, so the caller has nothing to copy.
	stale := store.UpsertRateLimitsParams{
		UserSecretID: sec, UserID: user, EnablementRev: rev + 100,
		SevenDayPct: i2(1), Source: pgtype.Text{String: "usage_endpoint", Valid: true}, SyncedAt: tsz(b2),
	}
	if n, err := q.UpsertRateLimits(ctx, stale); err != nil || n != 0 {
		t.Fatalf("fenced upsert = (%d, %v), want (0, nil)", n, err)
	}
	if n := copyAnth(ctx, t, q, anthGauge(ctx, t, q, user, sec, rev, b3, nil)); n != 1 {
		t.Fatalf("bucket 3 copy = %d, want 1", n)
	}
	rows, err := pool.Query(ctx, `SELECT bucket_start FROM anthropic_usage_samples WHERE user_id = $1 ORDER BY bucket_start`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []time.Time
	for rows.Next() {
		var b time.Time
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		got = append(got, b.UTC())
	}
	if len(got) != 2 || !got[0].Equal(wantBucketStart(b1)) || !got[1].Equal(wantBucketStart(b3)) {
		t.Fatalf("buckets = %v, want exactly [%v %v] and no row for the gap", got, wantBucketStart(b1), wantBucketStart(b3))
	}
}

// --- Codex -------------------------------------------------------------------------------

const codexTypical = `[{"id":"codex","display_name":"Codex","allowed":true,"limit_reached":false,` +
	`"primary":{"used_percent":12.5,"limit_window_seconds":18000,"reset_at":1893456000,"reset_after_seconds":300},` +
	`"secondary":{"used_percent":3,"limit_window_seconds":604800,"reset_at":null,"reset_after_seconds":3600}}]`

type codexFx struct {
	acc  uuid.UUID
	sec  uuid.UUID
	p    store.UpsertCodexAccountRateLimitsParams
	last pgtype.Timestamptz
}

// codexGauge writes one reading through the production UpsertCodexAccountRateLimits on a fresh
// linked account and returns the exact params and the last_success_at it returned.
func codexGauge(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, user uuid.UUID, buckets string) codexFx {
	t.Helper()
	acc, sec := mkLinkedCodexAccount(ctx, t, pool, q, user, "hist", false)
	return codexWrite(ctx, t, q, user, acc, sec, buckets)
}

func codexWrite(ctx context.Context, t *testing.T, q *store.Queries, user, acc, sec uuid.UUID, buckets string) codexFx {
	t.Helper()
	p := store.UpsertCodexAccountRateLimitsParams{
		UserID: user, ProviderAccountID: acc, Buckets: []byte(buckets),
		AttemptStatus: "ok", EnablementSig: codexEnablementSig(ctx, t, q, user, acc),
	}
	last, err := q.UpsertCodexAccountRateLimits(ctx, p)
	if err != nil || !last.Valid {
		t.Fatalf("UpsertCodexAccountRateLimits = (%+v, %v), want a written row", last, err)
	}
	return codexFx{acc: acc, sec: sec, p: p, last: last}
}

func copyCodex(ctx context.Context, t *testing.T, q *store.Queries, p store.UpsertCodexAccountRateLimitsParams, last pgtype.Timestamptz) int64 {
	t.Helper()
	n, err := q.CopyCodexUsageSample(ctx, store.CopyCodexUsageSampleParamsFrom(p, last))
	if err != nil {
		t.Fatalf("CopyCodexUsageSample: %v", err)
	}
	return n
}

func sampleBuckets(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user, acc uuid.UUID) (string, int) {
	t.Helper()
	var n int
	var js *string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(buckets::text) FROM codex_usage_samples
		WHERE user_id = $1 AND provider_account_id = $2`, user, acc).Scan(&n, &js); err != nil {
		t.Fatal(err)
	}
	if js == nil {
		return "", n
	}
	return *js, n
}

func jsonEqual(ctx context.Context, t *testing.T, pool *pgxpool.Pool, a, b string) bool {
	t.Helper()
	var eq bool
	if err := pool.QueryRow(ctx, `SELECT $1::jsonb = $2::jsonb`, a, b).Scan(&eq); err != nil {
		t.Fatalf("compare json: %v", err)
	}
	return eq
}

func TestCodexUsageCopyHappyPathLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	awaitBucketRoom(t)
	f := codexGauge(ctx, t, pool, q, user, codexTypical)
	if n := copyCodex(ctx, t, q, f.p, f.last); n != 1 {
		t.Fatalf("first copy = %d rows, want 1", n)
	}
	derived := f.last.Time.Unix() + 3600
	want := fmt.Sprintf(`[{"id":"codex","a":true,"l":false,
		"s1":{"p":12.5,"w":18000,"r":1893456000,"rd":false},
		"s2":{"p":3,"w":604800,"r":%d,"rd":true}}]`, derived)
	got, n := sampleBuckets(ctx, t, pool, user, f.acc)
	if n != 1 || !jsonEqual(ctx, t, pool, got, want) {
		t.Fatalf("stored (%d rows) %s, want the projection %s (display_name dropped, reset derived and flagged approximate)", n, got, want)
	}
	var bucket, observed time.Time
	var gen, rev int64
	if err := pool.QueryRow(ctx, `SELECT bucket_start, observed_at, observed_generation, observed_credential_revision
		FROM codex_usage_samples WHERE user_id = $1`, user).Scan(&bucket, &observed, &gen, &rev); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(f.last.Time) || !bucket.Equal(wantBucketStart(f.last.Time)) || gen != 0 || rev != 0 {
		t.Fatalf("sample observed=%v bucket=%v gen=%d rev=%d, want last_success_at %v in bucket %v", observed, bucket, gen, rev, f.last.Time, wantBucketStart(f.last.Time))
	}
	// Same bucket: a no-op, whether the same reading or a new accepted write.
	if n := copyCodex(ctx, t, q, f.p, f.last); n != 0 {
		t.Fatalf("repeat copy = %d rows, want 0", n)
	}
	f2 := codexWrite(ctx, t, q, user, f.acc, f.sec, `[{"id":"codex","primary":{"used_percent":99}}]`)
	if wantBucketStart(f2.last.Time).Equal(wantBucketStart(f.last.Time)) {
		if n := copyCodex(ctx, t, q, f2.p, f2.last); n != 0 {
			t.Fatalf("second reading in the same bucket = %d rows, want 0 (first wins)", n)
		}
		if again, n := sampleBuckets(ctx, t, pool, user, f.acc); n != 1 || !jsonEqual(ctx, t, pool, again, want) {
			t.Fatalf("the first reading was replaced: %s", again)
		}
	}
}

// TestCodexUsageCopyFencesLiveDB: each case moves ONE thing between the accepted write and the
// copy, and the copy must select nothing.
func TestCodexUsageCopyFencesLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, fmt.Sprintf("codex-hist-other-%s@e2e", other))
	expectZero := func(name string, p store.UpsertCodexAccountRateLimitsParams, last pgtype.Timestamptz) {
		t.Helper()
		if n := copyCodex(ctx, t, q, p, last); n != 0 {
			t.Errorf("%s: copy = %d rows, want 0", name, n)
		}
	}
	buckets := `[{"id":"codex","primary":{"used_percent":10}}]`

	// Sanity: the unmodified fixture copies, so every zero below is the one change.
	ok := codexGauge(ctx, t, pool, q, user, buckets)
	if n := copyCodex(ctx, t, q, ok.p, ok.last); n != 1 {
		t.Fatalf("control copy = %d rows, want 1", n)
	}

	// Binding.
	f := codexGauge(ctx, t, pool, q, user, buckets)
	p := f.p
	p.UserID = other
	expectZero("wrong owner, same account", p, f.last)
	p = f.p
	other2, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "other-acc", false)
	p.ProviderAccountID = other2
	expectZero("same owner, other account (no gauge row)", p, f.last)

	// Each reading column alone.
	f = codexGauge(ctx, t, pool, q, user, buckets)
	p = f.p
	p.Buckets = []byte(`[{"id":"codex","primary":{"used_percent":11}}]`)
	expectZero("buckets bytes alone", p, f.last)
	p = f.p
	p.EnablementSig += "x"
	expectZero("enablement_sig param alone", p, f.last)
	p = f.p
	p.ObservedGeneration++
	expectZero("observed generation param alone", p, f.last)
	p = f.p
	p.ObservedCredentialRevision++
	expectZero("observed credential revision param alone", p, f.last)
	expectZero("last_success_at param alone", f.p, tsz(f.last.Time.Add(time.Microsecond)))

	// Attempt status alone, and attempt time alone (direct writes: each isolates one guard).
	f = codexGauge(ctx, t, pool, q, user, buckets)
	mustExec(ctx, t, pool, `UPDATE codex_account_rate_limits SET attempt_status = 'transport' WHERE user_id = $1 AND provider_account_id = $2`, user, f.acc)
	expectZero("attempt_status alone", f.p, f.last)
	f = codexGauge(ctx, t, pool, q, user, buckets)
	mustExec(ctx, t, pool, `UPDATE codex_account_rate_limits SET last_attempt_at = last_attempt_at + interval '1 second' WHERE user_id = $1 AND provider_account_id = $2`, user, f.acc)
	expectZero("last_attempt_at alone", f.p, f.last)

	// Race: a later failure recorded through the production function before the copy.
	f = codexGauge(ctx, t, pool, q, user, buckets)
	if n, err := q.RecordCodexAccountPollFailure(ctx, store.RecordCodexAccountPollFailureParams{
		UserID: user, ProviderAccountID: f.acc, AttemptStatus: "transport", AttemptError: "x",
		ObservedGeneration: 0, ObservedCredentialRevision: 0, EnablementSig: f.p.EnablementSig,
	}); err != nil || n != 1 {
		t.Fatalf("RecordCodexAccountPollFailure = (%d, %v)", n, err)
	}
	expectZero("later poll failure", f.p, f.last)

	// Race: a later accepted write replaced the gauge.
	f = codexGauge(ctx, t, pool, q, user, buckets)
	later := codexWrite(ctx, t, q, user, f.acc, f.sec, `[{"id":"codex","primary":{"used_percent":20}}]`)
	expectZero("gauge replaced by a later accepted write", f.p, f.last)
	_ = later

	// Rotation: generation, then credential revision, move on the account.
	f = codexGauge(ctx, t, pool, q, user, buckets)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET generation = 1 WHERE id = $1`, f.acc)
	expectZero("account generation rotated", f.p, f.last)
	f = codexGauge(ctx, t, pool, q, user, buckets)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET credential_revision = 1 WHERE id = $1`, f.acc)
	expectZero("account credential revision rotated", f.p, f.last)

	// Unlink: the last linked alias is removed.
	f = codexGauge(ctx, t, pool, q, user, buckets)
	mustExec(ctx, t, pool, `DELETE FROM user_secrets WHERE id = $1`, f.sec)
	expectZero("last linked alias removed", f.p, f.last)

	// Disable the only alias: no enabled linked alias remains (the signature moved too).
	f = codexGauge(ctx, t, pool, q, user, buckets)
	setEnabled(ctx, t, q, user, f.sec, false)
	expectZero("only alias disabled", f.p, f.last)

	// Signature alone: two aliases, a sibling is disabled (an enabled alias remains, so only the
	// recomputed linked-alias signature differs from the one the poll carried).
	f = codexGauge(ctx, t, pool, q, user, buckets)
	sib := addLinkedAlias(ctx, t, pool, q, user, f.acc, "sibling", false)
	_ = sib
	// The gauge row still carries the old signature, so re-write it with the two-alias one first,
	// then disable the sibling.
	f2 := codexWrite(ctx, t, q, user, f.acc, f.sec, buckets)
	setEnabled(ctx, t, q, user, sib, false)
	expectZero("sibling disabled after the write (signature moved, an enabled alias remains)", f2.p, f2.last)

	if got := countRows(ctx, t, pool, "codex_usage_samples", user); got != 1 {
		t.Fatalf("rows after the fence cases = %d, want 1 (the control copy)", got)
	}
	if got := countRows(ctx, t, pool, "codex_usage_samples", other); got != 0 {
		t.Fatalf("the other owner has %d rows, want 0", got)
	}
}

func TestCodexUsageCopyAdmissionWindowLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	for _, tc := range admissionWindowCases {
		// A fresh account per case, written on the pool; only the timestamp move and the copy
		// share the case's transaction (see admissionWindowCases).
		f := codexGauge(ctx, t, pool, q, user, `[]`)

		tx := admissionTx(ctx, t, pool)
		var ts time.Time
		if err := tx.QueryRow(ctx, `UPDATE codex_account_rate_limits
			SET last_success_at = now() + $3::interval, last_attempt_at = now() + $3::interval
			WHERE user_id = $1 AND provider_account_id = $2 RETURNING last_success_at`, user, f.acc, tc.offset).Scan(&ts); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		assertAdmissionOffset(ctx, t, tx, tc.name, ts, tc.wantMicros)
		if n := copyCodex(ctx, t, q.WithTx(tx), f.p, tsz(ts)); n != tc.want {
			t.Errorf("%s: copy = %d rows, want %d", tc.name, n, tc.want)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("%s: rollback: %v", tc.name, err)
		}
	}
}

// TestCodexUsageAliasDedupLiveDB is example 2: two codex_auth aliases linked to ONE account give
// one poll target, and two accepted writes with their copies in one bucket insert 1 then 0.
func TestCodexUsageAliasDedupLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	awaitBucketRoom(t)
	acc, sec1 := mkLinkedCodexAccount(ctx, t, pool, q, user, "alias-one", false)
	addLinkedAlias(ctx, t, pool, q, user, acc, "alias-two", false)

	targets, err := q.ListLinkedCodexAccountsToPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine []store.ListLinkedCodexAccountsToPollRow
	for _, r := range targets {
		if r.UserID == user {
			mine = append(mine, r)
		}
	}
	if len(mine) != 1 || mine[0].ProviderAccountID != acc {
		t.Fatalf("poll targets for the owner = %d, want exactly one for the account", len(mine))
	}
	tg := mine[0]
	write := func(buckets string) (store.UpsertCodexAccountRateLimitsParams, pgtype.Timestamptz) {
		p := store.UpsertCodexAccountRateLimitsParams{
			UserID: tg.UserID, ProviderAccountID: tg.ProviderAccountID, Buckets: []byte(buckets),
			ObservedGeneration: tg.Generation, ObservedCredentialRevision: tg.CredentialRevision,
			AttemptStatus: "ok", EnablementSig: tg.EnablementSig,
		}
		last, err := q.UpsertCodexAccountRateLimits(ctx, p)
		if err != nil || !last.Valid {
			t.Fatalf("upsert = (%+v, %v)", last, err)
		}
		return p, last
	}
	_ = sec1
	p1, l1 := write(`[{"id":"codex","primary":{"used_percent":1}}]`)
	if n := copyCodex(ctx, t, q, p1, l1); n != 1 {
		t.Fatalf("first accepted write's copy = %d, want 1", n)
	}
	p2, l2 := write(`[{"id":"codex","primary":{"used_percent":2}}]`)
	if n := copyCodex(ctx, t, q, p2, l2); n != 0 {
		t.Fatalf("second accepted write's copy in the same bucket = %d, want 0", n)
	}
	if _, n := sampleBuckets(ctx, t, pool, user, acc); n != 1 {
		t.Fatalf("samples for the account = %d, want 1", n)
	}
}

func TestCodexUsagePayloadPolicyLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	id := func(n int) string { return strings.Repeat("a", n) }
	many := func(n int, idLen int, window string) string {
		parts := make([]string, n)
		for i := range parts {
			// A unique, fixed-length id: the index in decimal, left-padded with 'a'.
			sfx := fmt.Sprintf("%d", i)
			parts[i] = fmt.Sprintf(`{"id":%q,"allowed":true,"limit_reached":false,"primary":%s,"secondary":%s}`,
				id(idLen-len(sfx))+sfx, window, window)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	fullWindow := `{"used_percent":99.5,"limit_window_seconds":604800,"reset_at":1893456000,"reset_after_seconds":604800}`

	for _, tc := range []struct {
		name    string
		buckets string
		want    int64
	}{
		// Admitted.
		{"empty array", `[]`, 1},
		{"all nulls and missing keys", `[{"id":"a","allowed":null,"limit_reached":null,"primary":null,"secondary":null},{"id":"b"}]`, 1},
		{"null window fields", `[{"id":"a","primary":{"used_percent":null,"limit_window_seconds":null,"reset_at":null,"reset_after_seconds":null}}]`, 1},
		{"unknown keys ignored", `[{"id":"a","extra":{"x":1},"primary":{"used_percent":1,"mystery":"m"}}]`, 1},
		{"percent bounds 0 and 100", `[{"id":"a","primary":{"used_percent":0},"secondary":{"used_percent":100}}]`, 1},
		{"id of 99 bytes", `[{"id":"` + id(99) + `"}]`, 1},
		{"17 buckets", many(17, 3, `null`), 1},
		{"17 long ids without windows", many(17, 99, `null`), 1},
		// Rejected: payload shape.
		{"JSON null", `null`, 0},
		{"object instead of array", `{"id":"a"}`, 0},
		{"string instead of array", `"x"`, 0},
		{"18 buckets", many(18, 3, `null`), 0},
		{"element is a number", `[1]`, 0},
		{"element is an array", `[[]]`, 0},
		{"missing id", `[{"allowed":true}]`, 0},
		{"numeric id", `[{"id":5}]`, 0},
		{"empty id", `[{"id":""}]`, 0},
		{"id of 100 bytes", `[{"id":"` + id(100) + `"}]`, 0},
		{"duplicate ids", `[{"id":"a"},{"id":"a"}]`, 0},
		{"non-boolean allowed", `[{"id":"a","allowed":"yes"}]`, 0},
		{"non-boolean limit_reached", `[{"id":"a","limit_reached":1}]`, 0},
		{"primary is a string", `[{"id":"a","primary":"x"}]`, 0},
		{"secondary is an array", `[{"id":"a","secondary":[]}]`, 0},
		// Rejected: window fields.
		{"percent is a string", `[{"id":"a","primary":{"used_percent":"5"}}]`, 0},
		{"percent above 100", `[{"id":"a","primary":{"used_percent":100.5}}]`, 0},
		{"negative percent", `[{"id":"a","primary":{"used_percent":-1}}]`, 0},
		{"window length zero", `[{"id":"a","primary":{"limit_window_seconds":0}}]`, 0},
		{"window length fractional", `[{"id":"a","primary":{"limit_window_seconds":1.5}}]`, 0},
		{"window length a string", `[{"id":"a","primary":{"limit_window_seconds":"3"}}]`, 0},
		{"reset_at negative", `[{"id":"a","primary":{"reset_at":-1}}]`, 0},
		{"reset_at fractional", `[{"id":"a","primary":{"reset_at":1.5}}]`, 0},
		{"reset_at huge", `[{"id":"a","primary":{"reset_at":1e30}}]`, 0},
		{"reset_after negative", `[{"id":"a","secondary":{"reset_after_seconds":-5}}]`, 0},
		{"reset_after a string", `[{"id":"a","secondary":{"reset_after_seconds":"5"}}]`, 0},
		{"reset_after fractional", `[{"id":"a","secondary":{"reset_after_seconds":1.5}}]`, 0},
		{"reset_after huge", `[{"id":"a","secondary":{"reset_after_seconds":1e30}}]`, 0},
		// Rejected: the projection exceeds 4 KiB.
		{"17 buckets with 99-byte ids and full windows (normalizer maximum)", many(17, 99, fullWindow), 0},
	} {
		f := codexGauge(ctx, t, pool, q, user, tc.buckets)
		if n := copyCodex(ctx, t, q, f.p, f.last); n != tc.want {
			t.Errorf("%s: copy = %d rows, want %d", tc.name, n, tc.want)
		}
	}

	// The stored projection of an admitted maximum-admissible payload is within the table CHECK.
	var maxLen int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(octet_length(buckets::text)), 0) FROM codex_usage_samples WHERE user_id = $1`, user).Scan(&maxLen); err != nil {
		t.Fatal(err)
	}
	if maxLen == 0 || maxLen > 4096 {
		t.Fatalf("largest stored projection = %d bytes, want 1..4096", maxLen)
	}

	// The normalizer's maximum shape is a gap because its projection exceeds the kept 4 KiB bound.
	maxShape := many(17, 99, fullWindow)
	var projected int
	if err := pool.QueryRow(ctx, `SELECT octet_length(jsonb_agg(jsonb_build_object('id', e->'id', 'a', e->'allowed', 'l', e->'limit_reached',
			's1', jsonb_build_object('p', e->'primary'->'used_percent', 'w', e->'primary'->'limit_window_seconds', 'r', e->'primary'->'reset_at', 'rd', false),
			's2', jsonb_build_object('p', e->'secondary'->'used_percent', 'w', e->'secondary'->'limit_window_seconds', 'r', e->'secondary'->'reset_at', 'rd', false)))::text)
		FROM jsonb_array_elements($1::jsonb) e`, maxShape).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if projected <= 4096 {
		t.Fatalf("the maximum shape projects to %d bytes, want over the 4096 bound (the test premise)", projected)
	}
}

// TestUsageSamplesCredentialDeletionAndOwnerCascadeLiveDB: the identity column has no FK, the owner FK cascades.
func TestUsageSamplesCredentialDeletionAndOwnerCascadeLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	sec := enablementToken(ctx, t, q, user, "cascade", true)
	rev := tokenRev(ctx, t, q, user, sec)
	p := anthGauge(ctx, t, q, user, sec, rev, histPast(histBucket, 5*time.Second), nil)
	if n := copyAnth(ctx, t, q, p); n != 1 {
		t.Fatalf("anthropic copy = %d", n)
	}
	f := codexGauge(ctx, t, pool, q, user, `[]`)
	if n := copyCodex(ctx, t, q, f.p, f.last); n != 1 {
		t.Fatalf("codex copy = %d", n)
	}

	// Deleting the credential (and the account's alias) leaves the history in place.
	mustExec(ctx, t, pool, `DELETE FROM user_secrets WHERE id = ANY($1)`, []uuid.UUID{sec, f.sec})
	if a, c := countRows(ctx, t, pool, "anthropic_usage_samples", user), countRows(ctx, t, pool, "codex_usage_samples", user); a != 1 || c != 1 {
		t.Fatalf("after deleting the credentials: anthropic=%d codex=%d rows, want 1 and 1", a, c)
	}

	// Deleting the owner cascades both tables away.
	mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, user)
	if a, c := countRows(ctx, t, pool, "anthropic_usage_samples", user), countRows(ctx, t, pool, "codex_usage_samples", user); a != 0 || c != 0 {
		t.Fatalf("after deleting the owner: anthropic=%d codex=%d rows, want 0 and 0", a, c)
	}
}

func TestUsageSamplesImmutableOwnerLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, fmt.Sprintf("immut-%s@e2e", other))
	sec := enablementToken(ctx, t, q, user, "immut", true)
	rev := tokenRev(ctx, t, q, user, sec)
	if n := copyAnth(ctx, t, q, anthGauge(ctx, t, q, user, sec, rev, histPast(histBucket, 5*time.Second), nil)); n != 1 {
		t.Fatalf("anthropic copy = %d", n)
	}
	f := codexGauge(ctx, t, pool, q, user, `[]`)
	if n := copyCodex(ctx, t, q, f.p, f.last); n != 1 {
		t.Fatalf("codex copy = %d", n)
	}
	for _, tc := range []struct{ name, sql string }{
		{"anthropic owner", `UPDATE anthropic_usage_samples SET user_id = $2 WHERE user_id = $1`},
		{"anthropic identity", `UPDATE anthropic_usage_samples SET user_secret_id = gen_random_uuid() WHERE user_id = $1 AND $2::uuid IS NOT NULL`},
		{"codex owner", `UPDATE codex_usage_samples SET user_id = $2 WHERE user_id = $1`},
		{"codex identity", `UPDATE codex_usage_samples SET provider_account_id = gen_random_uuid() WHERE user_id = $1 AND $2::uuid IS NOT NULL`},
	} {
		_, err := pool.Exec(ctx, tc.sql, user, other)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || !strings.Contains(pgErr.Message, "immutable") {
			t.Errorf("%s: update error = %v, want the immutability check_violation", tc.name, err)
		}
	}
	// A non-identity column may still change: the trigger guards only owner and identity.
	if _, err := pool.Exec(ctx, `UPDATE anthropic_usage_samples SET enablement_rev = enablement_rev WHERE user_id = $1`, user); err != nil {
		t.Errorf("a non-identity update was refused: %v", err)
	}
}
