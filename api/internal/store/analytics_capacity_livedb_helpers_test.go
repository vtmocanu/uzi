package store_test

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731: fixtures for the Capacity statement LiveDB tests. Owner-scoped tests share the
// LiveDB database and seed their own owners; the all-scope statements have no owner predicate,
// so those tests run in a private database (capPrivateDB) that holds nothing else.

// capPrivateDB creates, migrates and returns a database only the calling test uses.
func capPrivateDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "cap_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create private database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop private database: %v", err)
		}
	})
	owned := dsn + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		q := u.Query()
		q.Del("dbname") // a query dbname would override the path
		u.RawQuery = q.Encode()
		owned = u.String()
	}
	if err := store.Migrate(ctx, owned); err != nil {
		t.Fatalf("migrate private database: %v", err)
	}
	pool, err := store.OpenPool(ctx, owned)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func capFixtureOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *spendFixture {
	return &spendFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), epochs: map[string]int{}}
}

// capRun describes one seeded run with the Capacity-relevant columns.
type capRun struct {
	Owner    uuid.UUID
	Repo     uuid.UUID
	Kind     string // default issue
	Status   string // default completed
	Harness  string // default claude (cross_check is always codex)
	Created  time.Time
	Started  time.Time // first_started_at; zero = NULL
	Finished time.Time // finished_at; zero = NULL
	StatusAt time.Time // status_since; zero = Finished, else Created
	Waits    int       // limit_wait_count
	Lane     bool      // cross_check_lane (cross_check kind only)
}

func (f *spendFixture) capRun(r capRun) uuid.UUID {
	f.t.Helper()
	if r.StatusAt.IsZero() {
		r.StatusAt = r.Finished
	}
	var id uuid.UUID
	if r.Kind == runkind.CrossCheck {
		// The checked run's own parent is a finished issue run from long before any window under
		// test, so it is never a candidate or a usage-wait row.
		old := r.Created.Add(-400 * 24 * time.Hour)
		parent := f.run(spendRun{Owner: r.Owner, Repo: r.Repo, At: old, Finished: old})
		id = uuid.New()
		status := r.Status
		if status == "" {
			status = "completed"
		}
		var finished any
		if !r.Finished.IsZero() {
			finished = r.Finished
		}
		statusAt := r.StatusAt
		if statusAt.IsZero() {
			statusAt = r.Created
		}
		mustExec(f.ctx, f.t, f.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, created_at,
			                   finished_at, status_since, harness, target_run_id, report_only, budget_wall_seconds, cross_check_lane)
			 VALUES ($1, $2, $3, 't', 'd', $4, 'cross_check', $5, $6, $7, 'codex', $8, true, 600, $9)`,
			id, r.Owner, r.Repo, status, r.Created, finished, statusAt, parent, r.Lane)
	} else {
		id = f.run(spendRun{Owner: r.Owner, Repo: r.Repo, Kind: r.Kind, Status: r.Status, Harness: r.Harness,
			At: r.Created, Finished: r.Finished, StatusAt: r.StatusAt})
	}
	if !r.Started.IsZero() || r.Waits > 0 {
		var started any
		if !r.Started.IsZero() {
			started = r.Started
		}
		mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET first_started_at = $2, limit_wait_count = $3 WHERE id = $1`,
			id, started, r.Waits)
	}
	return id
}

// capCall is one Capacity statement invocation; Owner zero = the all-scope statement.
type capCall struct {
	Owner     uuid.UUID
	Now       time.Time
	Zone      string
	SessionTZ string
	Window    analytics.Window // default 7d
	Repo      uuid.UUID
	Harness   string
	Kinds     []string // default runkind.All()
}

type capResult struct {
	Raw    []byte
	Doc    store.AnalyticsCapacityDoc
	Bounds analytics.Bounds
}

func (f *spendFixture) capacity(c capCall) capResult {
	f.t.Helper()
	if c.Window == "" {
		c.Window = analytics.Window7d
	}
	if c.Kinds == nil {
		c.Kinds = runkind.All()
	}
	b := f.bounds(spendCall{Owner: c.Owner, Now: c.Now, Zone: c.Zone, Window: c.Window, Repo: c.Repo, Harness: c.Harness, Kinds: c.Kinds})
	if err := analytics.ValidateBuckets(b); err != nil {
		f.t.Fatalf("calendar does not tile: %v", err)
	}
	var repo pgtype.UUID
	if c.Repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: c.Repo, Valid: true}
	}
	var harness pgtype.Text
	if c.Harness != "" {
		harness = pgtype.Text{String: c.Harness, Valid: true}
	}
	q := f.q
	if c.SessionTZ != "" {
		conn, err := f.pool.Acquire(f.ctx)
		if err != nil {
			f.t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(f.ctx, fmt.Sprintf("SET TIME ZONE '%s'", c.SessionTZ)); err != nil {
			f.t.Fatalf("set time zone: %v", err)
		}
		defer func() { _, _ = conn.Exec(context.Background(), "RESET TIME ZONE") }()
		q = store.New(conn)
	}
	var raw []byte
	var err error
	if c.Owner != uuid.Nil {
		raw, err = q.AnalyticsOwnerCapacity(f.ctx, store.AnalyticsOwnerCapacityParams{
			UserID: c.Owner, Now: spendTS(b.Now), From: spendTS(b.From),
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
			RepoID: repo, Harness: harness, Kinds: c.Kinds,
		})
	} else {
		raw, err = q.AnalyticsAllCapacity(f.ctx, store.AnalyticsAllCapacityParams{
			Now: spendTS(b.Now), From: spendTS(b.From),
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
			RepoID: repo, Harness: harness, Kinds: c.Kinds,
		})
	}
	if err != nil {
		f.t.Fatalf("capacity statement: %v", err)
	}
	doc, err := store.DecodeAnalyticsCapacityDoc(raw)
	if err != nil {
		f.t.Fatalf("%v\n%s", err, raw)
	}
	if len(doc.Occupancy.Buckets) != len(b.BucketStarts) || len(doc.UsageWait.Buckets) != len(b.BucketStarts) {
		f.t.Fatalf("document has %d occupancy and %d usage-wait buckets for %d calendar buckets",
			len(doc.Occupancy.Buckets), len(doc.UsageWait.Buckets), len(b.BucketStarts))
	}
	for i := range doc.Occupancy.Buckets {
		if doc.Occupancy.Buckets[i].Index != i || doc.UsageWait.Buckets[i].Index != i {
			f.t.Fatalf("bucket %d is misindexed", i)
		}
	}
	return capResult{Raw: raw, Doc: doc, Bounds: b}
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// wantOcc checks an occupancy cell: exact run and duration seconds, and the average (nil with
// a reason when avg is nil).
func wantOcc(t *testing.T, what string, got store.AnalyticsCapacityOccupancyCell, runSeconds, durSeconds float64, avg *float64, reason string) {
	t.Helper()
	if !nearly(got.RunSeconds, runSeconds) || !nearly(got.DurationSeconds, durSeconds) {
		t.Errorf("%s: run_seconds/duration_seconds = %v/%v, want %v/%v", what, got.RunSeconds, got.DurationSeconds, runSeconds, durSeconds)
	}
	switch {
	case avg != nil:
		if got.AverageConcurrentRuns == nil || !nearly(*got.AverageConcurrentRuns, *avg) {
			t.Errorf("%s: average = %v, want %v", what, fl(got.AverageConcurrentRuns), *avg)
		}
		if got.Reason != nil {
			t.Errorf("%s: reason %q beside a value", what, *got.Reason)
		}
	default:
		if got.AverageConcurrentRuns != nil {
			t.Errorf("%s: average = %v, want null (%s)", what, *got.AverageConcurrentRuns, reason)
		}
		if got.Reason == nil || *got.Reason != reason {
			t.Errorf("%s: reason = %v, want %q", what, deref(got.Reason), reason)
		}
	}
}

// wantOccSecs checks only an occupancy cell's seconds.
func wantOccSecs(t *testing.T, what string, got store.AnalyticsCapacityOccupancyCell, runSeconds, durSeconds float64) {
	t.Helper()
	if !nearly(got.RunSeconds, runSeconds) || !nearly(got.DurationSeconds, durSeconds) {
		t.Errorf("%s: run_seconds/duration_seconds = %v/%v, want %v/%v", what, got.RunSeconds, got.DurationSeconds, runSeconds, durSeconds)
	}
}

func fl(p *float64) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func f6(v float64) *float64 { return &v }

// wantWait checks a usage-wait cell.
func wantWait(t *testing.T, what string, got store.AnalyticsCapacityUsageWaitCell, count, n int64, share *float64, reason string) {
	t.Helper()
	if got.Count != count || got.N != n {
		t.Errorf("%s: count/n = %d/%d, want %d/%d", what, got.Count, got.N, count, n)
	}
	switch {
	case share != nil:
		if got.Share == nil || !nearly(*got.Share, *share) {
			t.Errorf("%s: share = %v, want %v", what, fl(got.Share), *share)
		}
		if got.Reason != nil {
			t.Errorf("%s: reason %q beside a value", what, *got.Reason)
		}
	default:
		if got.Share != nil {
			t.Errorf("%s: share = %v, want null (%s)", what, *got.Share, reason)
		}
		if got.Reason == nil || *got.Reason != reason {
			t.Errorf("%s: reason = %v, want %q", what, deref(got.Reason), reason)
		}
	}
}

func wantPopulation(t *testing.T, what string, got store.AnalyticsCapacityPopulation, want store.AnalyticsCapacityPopulation) {
	t.Helper()
	if got != want {
		t.Errorf("%s: population = %+v, want %+v", what, got, want)
	}
}

func capDay(day, hour int) time.Time { return utcAt(2026, time.November, day, hour, 0, 0) }
