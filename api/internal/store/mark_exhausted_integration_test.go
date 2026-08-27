package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These *LiveDB tests pin PRD #217 M1's park-time exhaustion writes
// (MarkFiveHourExhausted / MarkSevenDayExhausted) against REAL Postgres. The whole
// point of Success Criterion 1 is a STRICT-SUBSET write — exactly one *_pct column
// and `source` move, and synced_at, the other *_pct and BOTH reset columns are
// byte-identical to before — and that subset property is a fact about the SQL
// UPDATE's SET list, invisible to any fake store. sqlc generating clean Go proves
// nothing about which columns Postgres actually touches; only executing the
// statement and re-reading every column does.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix
// via `-run 'LiveDB$'`).

// openExhaustStore is the per-test harness the sibling *LiveDB tests inline: skip
// without a DSN, migrate, open a pool (closed on cleanup), and hand back a *Queries.
func openExhaustStore(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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
	return ctx, pool, store.New(pool)
}

// mkExhaustSecret seeds a fresh user and one anthropic_token secret, and returns the
// secret id. Each test owns its own user so re-runs against a re-used DB never
// collide (users.email is UNIQUE, so the email is derived from the fresh uuid, like
// every other test in this package).
func mkExhaustSecret(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, label string) uuid.UUID {
	t.Helper()
	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user, fmt.Sprintf("exhaust-%s@e2e", user))
	row, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: true,
		Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert secret %q: %v", label, err)
	}
	return row.ID
}

// gaugeRow is every column of anthropic_rate_limits that M1's writes could touch,
// read straight from the row so the before/after comparison is over what Postgres
// actually stored (microsecond-truncated, UTC) rather than the nanosecond Go values
// that were handed to the upsert.
type gaugeRow struct {
	FivePct    pgtype.Int2
	FiveReset  pgtype.Timestamptz
	SevenPct   pgtype.Int2
	SevenReset pgtype.Timestamptz
	Source     pgtype.Text
	SyncedAt   pgtype.Timestamptz
}

func readGauge(ctx context.Context, t *testing.T, pool *pgxpool.Pool, secretID uuid.UUID) gaugeRow {
	t.Helper()
	var g gaugeRow
	err := pool.QueryRow(ctx,
		`SELECT five_hour_pct, five_hour_resets_at, seven_day_pct, seven_day_resets_at, source, synced_at
		   FROM anthropic_rate_limits WHERE user_secret_id = $1`, secretID).
		Scan(&g.FivePct, &g.FiveReset, &g.SevenPct, &g.SevenReset, &g.Source, &g.SyncedAt)
	if err != nil {
		t.Fatalf("read gauge %s: %v", secretID, err)
	}
	return g
}

func sameInt2(a, b pgtype.Int2) bool {
	return a.Valid == b.Valid && (!a.Valid || a.Int16 == b.Int16)
}

func sameTS(a, b pgtype.Timestamptz) bool {
	return a.Valid == b.Valid && (!a.Valid || a.Time.Equal(b.Time))
}

// TestMarkFiveHourExhaustedLiveDB is the core SC1/D3/D4 assertion for the five-hour
// window: the pct and source move to (100, limit_report) and NOTHING ELSE does. The
// per-column values are distinct (five_hour_pct=40 vs seven_day_pct=30, and two
// different reset instants) so a SET list that wrote the wrong column, or copied one
// window's value into the other's, would be caught rather than hidden behind equal
// fixtures. The baseline is re-read FROM the DB after the seed so the untouched-column
// checks compare stored value against stored value.
func TestMarkFiveHourExhaustedLiveDB(t *testing.T) {
	ctx, pool, q := openExhaustStore(t)
	sec := mkExhaustSecret(ctx, t, pool, q, "five-hour-key")

	base := time.Now().UTC()
	if err := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID:     sec,
		UserID:           gaugeOwner(ctx, t, pool, sec),
		FiveHourPct:      pgtype.Int2{Int16: 40, Valid: true},
		SevenDayPct:      pgtype.Int2{Int16: 30, Valid: true},
		FiveHourResetsAt: pgtype.Timestamptz{Time: base.Add(time.Hour), Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: base.Add(72 * time.Hour), Valid: true},
		Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:         pgtype.Timestamptz{Time: base.Add(-10 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
	before := readGauge(ctx, t, pool, sec)

	n, err := q.MarkFiveHourExhausted(ctx, sec)
	if err != nil {
		t.Fatalf("MarkFiveHourExhausted: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkFiveHourExhausted affected %d rows, want exactly 1 (the one gauge row)", n)
	}
	after := readGauge(ctx, t, pool, sec)

	// The two columns that MUST move.
	if !after.FivePct.Valid || after.FivePct.Int16 != 100 {
		t.Errorf("five_hour_pct = %+v, want 100 (the window is now exhausted)", after.FivePct)
	}
	if !after.Source.Valid || after.Source.String != "limit_report" {
		t.Errorf("source = %+v, want 'limit_report' (D6: an operator can tell an inference from a poll)", after.Source)
	}

	// The four that MUST be byte-identical (SC1). synced_at is the load-bearing one
	// (D3): bumping it would re-freshen the OTHER window and can CREATE the re-pick
	// this write exists to prevent. The reset columns are D4: the cross-check reads a
	// poller-measured timestamp, never a park-time one.
	if !sameTS(after.SyncedAt, before.SyncedAt) {
		t.Errorf("synced_at moved %v -> %v; D3 says it must NOT — bumping it re-freshens the seven-day "+
			"window and can promote the dead token from skipped-as-stale to ranked-as-below-threshold",
			before.SyncedAt.Time, after.SyncedAt.Time)
	}
	if !sameInt2(after.SevenPct, before.SevenPct) {
		t.Errorf("seven_day_pct moved %+v -> %+v; the five-hour write must not touch the other window", before.SevenPct, after.SevenPct)
	}
	if !sameTS(after.FiveReset, before.FiveReset) {
		t.Errorf("five_hour_resets_at moved %v -> %v; D4 says the reset columns stay the poller's", before.FiveReset.Time, after.FiveReset.Time)
	}
	if !sameTS(after.SevenReset, before.SevenReset) {
		t.Errorf("seven_day_resets_at moved %v -> %v; D4 says the reset columns stay the poller's", before.SevenReset.Time, after.SevenReset.Time)
	}
}

// TestMarkSevenDayExhaustedLiveDB is the symmetric assertion: seven_day_pct and
// source move; five_hour_pct, both reset columns and synced_at do not. All four
// seven-day rate-limit spellings map onto this one query (see the query comment), so
// this is the store-layer proof for every one of them.
func TestMarkSevenDayExhaustedLiveDB(t *testing.T) {
	ctx, pool, q := openExhaustStore(t)
	sec := mkExhaustSecret(ctx, t, pool, q, "seven-day-key")

	base := time.Now().UTC()
	if err := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID:     sec,
		UserID:           gaugeOwner(ctx, t, pool, sec),
		FiveHourPct:      pgtype.Int2{Int16: 40, Valid: true},
		SevenDayPct:      pgtype.Int2{Int16: 30, Valid: true},
		FiveHourResetsAt: pgtype.Timestamptz{Time: base.Add(time.Hour), Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: base.Add(72 * time.Hour), Valid: true},
		Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:         pgtype.Timestamptz{Time: base.Add(-10 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
	before := readGauge(ctx, t, pool, sec)

	n, err := q.MarkSevenDayExhausted(ctx, sec)
	if err != nil {
		t.Fatalf("MarkSevenDayExhausted: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkSevenDayExhausted affected %d rows, want exactly 1", n)
	}
	after := readGauge(ctx, t, pool, sec)

	if !after.SevenPct.Valid || after.SevenPct.Int16 != 100 {
		t.Errorf("seven_day_pct = %+v, want 100", after.SevenPct)
	}
	if !after.Source.Valid || after.Source.String != "limit_report" {
		t.Errorf("source = %+v, want 'limit_report'", after.Source)
	}
	if !sameTS(after.SyncedAt, before.SyncedAt) {
		t.Errorf("synced_at moved %v -> %v; D3 says it must NOT", before.SyncedAt.Time, after.SyncedAt.Time)
	}
	if !sameInt2(after.FivePct, before.FivePct) {
		t.Errorf("five_hour_pct moved %+v -> %+v; the seven-day write must not touch the other window", before.FivePct, after.FivePct)
	}
	if !sameTS(after.FiveReset, before.FiveReset) {
		t.Errorf("five_hour_resets_at moved %v -> %v; D4", before.FiveReset.Time, after.FiveReset.Time)
	}
	if !sameTS(after.SevenReset, before.SevenReset) {
		t.Errorf("seven_day_resets_at moved %v -> %v; D4", before.SevenReset.Time, after.SevenReset.Time)
	}
}

// TestMarkFiveHourExhaustedNullPctLiveDB covers D4's one hazardous shape: a gauge row
// whose five_hour_pct is NULL. The sole PRODUCTION writer (the poller) cannot make
// that row — anthropic.Window.Pct is a non-pointer int — but e2e/run-e2e.sh INSERTs
// rows directly and can, so this is the shape that turns an Unmeasured row Measured.
// The write must land 100 (non-null) into the previously-NULL column while still
// leaving every other column untouched, exactly as the fully-populated case does.
func TestMarkFiveHourExhaustedNullPctLiveDB(t *testing.T) {
	ctx, pool, q := openExhaustStore(t)
	sec := mkExhaustSecret(ctx, t, pool, q, "null-pct-key")

	base := time.Now().UTC()
	if err := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID:     sec,
		UserID:           gaugeOwner(ctx, t, pool, sec),
		FiveHourPct:      pgtype.Int2{}, // NULL — the Unmeasured five-hour window
		SevenDayPct:      pgtype.Int2{Int16: 30, Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: base.Add(72 * time.Hour), Valid: true},
		Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:         pgtype.Timestamptz{Time: base.Add(-10 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
	before := readGauge(ctx, t, pool, sec)
	if before.FivePct.Valid {
		t.Fatalf("precondition failed: five_hour_pct came back non-NULL (%+v); the NULL shape is the "+
			"whole point of this case", before.FivePct)
	}

	n, err := q.MarkFiveHourExhausted(ctx, sec)
	if err != nil {
		t.Fatalf("MarkFiveHourExhausted: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkFiveHourExhausted affected %d rows, want 1", n)
	}
	after := readGauge(ctx, t, pool, sec)

	if !after.FivePct.Valid || after.FivePct.Int16 != 100 {
		t.Errorf("five_hour_pct = %+v, want a non-NULL 100 — the write must make the previously "+
			"Unmeasured window Measured", after.FivePct)
	}
	if !after.Source.Valid || after.Source.String != "limit_report" {
		t.Errorf("source = %+v, want 'limit_report'", after.Source)
	}
	// Everything else still frozen, NULL five-hour reset included.
	if !sameTS(after.SyncedAt, before.SyncedAt) {
		t.Errorf("synced_at moved %v -> %v; D3", before.SyncedAt.Time, after.SyncedAt.Time)
	}
	if !sameInt2(after.SevenPct, before.SevenPct) {
		t.Errorf("seven_day_pct moved %+v -> %+v", before.SevenPct, after.SevenPct)
	}
	if !sameTS(after.FiveReset, before.FiveReset) {
		t.Errorf("five_hour_resets_at moved %v -> %v (was NULL: %v)", before.FiveReset.Time, after.FiveReset.Time, before.FiveReset.Valid)
	}
	if !sameTS(after.SevenReset, before.SevenReset) {
		t.Errorf("seven_day_resets_at moved %v -> %v", before.SevenReset.Time, after.SevenReset.Time)
	}
}

// TestMarkExhaustedNoGaugeRowLiveDB is D7: the statement is UPDATE-only, so a
// user_secret_id with no gauge row is a ZERO-ROW result and a SUCCESS, never an
// error. A missing gauge row already classifies StatusNoReading (Select skips it),
// so there is simply nothing to mark down. Both window writers must behave this way.
func TestMarkExhaustedNoGaugeRowLiveDB(t *testing.T) {
	ctx, pool, q := openExhaustStore(t)
	sec := mkExhaustSecret(ctx, t, pool, q, "no-gauge-key") // secret exists; deliberately no UpsertRateLimits

	n, err := q.MarkFiveHourExhausted(ctx, sec)
	if err != nil {
		t.Fatalf("MarkFiveHourExhausted on a row-less secret errored, but a missing gauge row means "+
			"'never picked' and is success (D7): %v", err)
	}
	if n != 0 {
		t.Fatalf("MarkFiveHourExhausted affected %d rows, want 0 (nothing to mark; UPDATE-only, D7)", n)
	}
	n, err = q.MarkSevenDayExhausted(ctx, sec)
	if err != nil {
		t.Fatalf("MarkSevenDayExhausted on a row-less secret errored: %v", err)
	}
	if n != 0 {
		t.Fatalf("MarkSevenDayExhausted affected %d rows, want 0 (D7)", n)
	}
	// And it did not conjure a row (it is never an INSERT).
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM anthropic_rate_limits WHERE user_secret_id = $1`, sec).Scan(&rows); err != nil {
		t.Fatalf("count gauge: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a gauge row appeared (%d) — the mark is UPDATE-only and must never INSERT (D7)", rows)
	}
}

// TestMarkFiveHourExhaustedShiftsNextAvailableLiveDB is R1b at the store seam. The
// mark's pct write is the INPUT that changes a sibling candidate's contribution to
// autoselect.NextAvailable: at five_hour_pct=40 the candidate is Eligible and
// contributes `now` (a sibling would promote immediately); after the mark it reads
// 100, classifies BelowThreshold, and contributes its binding-window reset instead —
// so a sibling's park LENGTHENS. This runs the real ListAutoSelectCandidates query
// (source is deliberately not projected there, so the projection is unaffected by the
// new value) through the real pgtype unwrapping. The NextAvailable arithmetic itself
// belongs to the workersvc/autoselect unit; what is proven HERE is that the stored
// row the selector reads back really did flip the classification.
func TestMarkFiveHourExhaustedShiftsNextAvailableLiveDB(t *testing.T) {
	ctx, pool, q := openExhaustStore(t)
	sec := mkExhaustSecret(ctx, t, pool, q, "sibling-input-key")
	owner := gaugeOwner(ctx, t, pool, sec)
	// The selector only ranks pooled tokens; without this the candidate classifies
	// StatusNotPooled and never reaches the Eligible/BelowThreshold distinction R1b
	// turns on.
	if _, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
		ID: sec, UserID: owner, Kind: store.KindAnthropicToken, AutoEligible: true,
	}); err != nil {
		t.Fatalf("pool sibling secret: %v", err)
	}

	base := time.Now().UTC()
	if err := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID:     sec,
		UserID:           owner,
		FiveHourPct:      pgtype.Int2{Int16: 40, Valid: true},
		SevenDayPct:      pgtype.Int2{Int16: 30, Valid: true},
		FiveHourResetsAt: pgtype.Timestamptz{Time: base.Add(time.Hour), Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: base.Add(72 * time.Hour), Valid: true},
		Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:         pgtype.Timestamptz{Time: base.Add(-time.Minute), Valid: true}, // fresh
	}); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}

	policy := autoselect.Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute}
	now := time.Now()
	deadCred := uuid.New() // the excluded (just-parked) credential; NextAvailable requires a non-nil exclude

	project := func() autoselect.Candidate {
		rows, err := q.ListAutoSelectCandidates(ctx, owner)
		if err != nil {
			t.Fatalf("ListAutoSelectCandidates: %v", err)
		}
		for _, r := range rows {
			if r.UserSecretID == sec {
				return autoselectrow.FromCandidateRow(r)
			}
		}
		t.Fatalf("sibling candidate %s missing from ListAutoSelectCandidates output", sec)
		return autoselect.Candidate{}
	}

	// Before the mark: fresh at 40% consumed -> Eligible -> contributes `now`.
	beforeCand := project()
	if st := autoselect.Classify(beforeCand, policy, now).Status; st != autoselect.StatusEligible {
		t.Fatalf("before mark the sibling classified %q, want Eligible (headroom 60 clears MinHeadroom 15)", st)
	}
	beforeFloor, beforeOK := autoselect.NextAvailable([]autoselect.Candidate{beforeCand}, deadCred, policy, now)
	if !beforeOK || !beforeFloor.Equal(now) {
		t.Fatalf("before mark NextAvailable = (%v, %v), want (now, true) — an Eligible sibling promotes immediately", beforeFloor, beforeOK)
	}

	if n, err := q.MarkFiveHourExhausted(ctx, sec); err != nil || n != 1 {
		t.Fatalf("MarkFiveHourExhausted = (%d, %v), want (1, nil)", n, err)
	}

	// After the mark: 100% consumed -> BelowThreshold -> contributes its binding
	// (five-hour) reset, which is in the future, so the sibling now waits.
	afterCand := project()
	if afterCand.FiveHourPct == nil || *afterCand.FiveHourPct != 100 {
		t.Fatalf("after mark the projected five_hour_pct = %v, want 100", afterCand.FiveHourPct)
	}
	if st := autoselect.Classify(afterCand, policy, now).Status; st != autoselect.StatusBelowThreshold {
		t.Fatalf("after mark the sibling classified %q, want BelowThreshold (headroom 0 < MinHeadroom 15)", st)
	}
	afterFloor, afterOK := autoselect.NextAvailable([]autoselect.Candidate{afterCand}, deadCred, policy, now)
	if !afterOK {
		t.Fatalf("after mark NextAvailable returned no floor; a BelowThreshold sibling with a binding-window reset must contribute it")
	}
	if !afterFloor.After(now) {
		t.Fatalf("after mark NextAvailable floor = %v, want strictly after now (%v) — R1b: withdrawing the "+
			"`now` contribution lengthens a sibling's park", afterFloor, now)
	}
	if afterCand.FiveResetsAt == nil || !afterFloor.Equal(*afterCand.FiveResetsAt) {
		t.Fatalf("after mark NextAvailable floor = %v, want the binding five-hour reset %v", afterFloor, afterCand.FiveResetsAt)
	}
}

// gaugeOwner resolves the user_id that owns a secret, so the gauge upsert can pass the
// matching half of the composite FK without the test threading the user id around.
func gaugeOwner(ctx context.Context, t *testing.T, pool *pgxpool.Pool, secretID uuid.UUID) uuid.UUID {
	t.Helper()
	var owner uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM user_secrets WHERE id = $1`, secretID).Scan(&owner); err != nil {
		t.Fatalf("resolve owner of %s: %v", secretID, err)
	}
	return owner
}
