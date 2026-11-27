package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729: shared fixtures for the Flow statement LiveDB tests. They reuse the Spend
// fixture's users, repos and run seeding, and add the gate-evidence tables.
//
// Every Flow test pins now to flowNow (2033-06-15 12:00 UTC) with window 7d, so the daily
// buckets are 2033-06-09 .. 2033-06-15 and bucket index = day - 9.

var flowNow = utcAt(2033, time.June, 15, 12, 0, 0)

// flowAt is 2033-06 day d at h:m UTC.
func flowAt(d, h, m int) time.Time { return utcAt(2033, time.June, d, h, m, 0) }

// patch updates columns of a seeded run: set is a SET list whose placeholders start at $2.
func (f *spendFixture) patch(run uuid.UUID, set string, args ...any) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET `+set+` WHERE id = $1`, append([]any{run}, args...)...)
}

// presentation inserts a run_gate_presentations row.
func (f *spendFixture) presentation(run uuid.UUID, revision int64, at time.Time) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO run_gate_presentations (run_id, presentation_id, revision, created_at) VALUES ($1, $2, $3, $4)`,
		run, uuid.New(), revision, at)
}

// receipt describes one approve_plan run_user_inputs row. Zero values: kind approve_plan,
// binding bound, no disposition.
type receipt struct {
	Binding     string // "" = bound, "legacy" = NULL binding, "unbound"
	Revision    int64
	CreatedAt   time.Time
	AppliedAt   time.Time // zero = NULL (ACK-only when ConsumedAt is set)
	Disposition string    // "" = NULL
	ConsumedAt  time.Time
}

func (f *spendFixture) receipt(run uuid.UUID, r receipt) {
	f.t.Helper()
	var binding, rev, applied, disp, consumed any
	switch r.Binding {
	case "", "bound":
		binding, rev = "bound", r.Revision
	case "unbound":
		binding = "unbound"
	case "legacy":
	default:
		f.t.Fatalf("receipt binding %q", r.Binding)
	}
	if !r.AppliedAt.IsZero() {
		applied = r.AppliedAt
	}
	if r.Disposition != "" {
		disp = r.Disposition
	}
	if !r.ConsumedAt.IsZero() {
		consumed = r.ConsumedAt
	}
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO run_user_inputs (run_id, kind, created_at, applied_at, consumed_at, disposition, gate_binding, gate_revision)
		 VALUES ($1, 'approve_plan', $2, $3, $4, $5, $6, $7)`,
		run, r.CreatedAt, applied, consumed, disp, binding, rev)
}

// flowCall is one Flow statement invocation.
type flowCall struct {
	Owner     uuid.UUID // zero = the all-scope statement
	Now       time.Time // default flowNow
	Zone      string    // default UTC
	SessionTZ string    // session TimeZone to run under; default the pool's
	Window    analytics.Window
	Repo      uuid.UUID
	Harness   string
	Kinds     []string // default runkind.All()
	MinN      int32    // default store.FlowMinPercentileN
}

// flowResult is a decoded response with its raw bytes and the bounds that produced it.
type flowResult struct {
	Raw    []byte
	Doc    store.AnalyticsFlowDoc
	Bounds analytics.Bounds
}

func (f *spendFixture) flow(c flowCall) flowResult {
	f.t.Helper()
	if c.Now.IsZero() {
		c.Now = flowNow
	}
	if c.Kinds == nil {
		c.Kinds = runkind.All()
	}
	if c.Window == "" {
		c.Window = analytics.Window7d
	}
	if c.MinN == 0 {
		c.MinN = store.FlowMinPercentileN
	}
	b := f.bounds(spendCall{Owner: c.Owner, Now: c.Now, Zone: c.Zone, Window: c.Window, Repo: c.Repo, Harness: c.Harness, Kinds: c.Kinds})
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
		raw, err = q.AnalyticsOwnerFlow(f.ctx, store.AnalyticsOwnerFlowParams{
			UserID: c.Owner, Now: spendTS(b.Now), From: spendTS(b.From),
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
			HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: c.MinN,
			RepoID: repo, Harness: harness, Kinds: c.Kinds,
		})
	} else {
		raw, err = q.AnalyticsAllFlow(f.ctx, store.AnalyticsAllFlowParams{
			Now: spendTS(b.Now), From: spendTS(b.From),
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
			HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: c.MinN,
			RepoID: repo, Harness: harness, Kinds: c.Kinds,
		})
	}
	if err != nil {
		f.t.Fatalf("flow statement: %v", err)
	}
	doc, err := store.DecodeAnalyticsFlowDoc(raw)
	if err != nil {
		f.t.Fatalf("%v\n%s", err, raw)
	}
	return flowResult{Raw: raw, Doc: doc, Bounds: b}
}

// wantNum fails unless the nullable number is the value (reason must then be nil).
func wantNum(t *testing.T, what string, got store.AnalyticsNullableNumber, want float64) {
	t.Helper()
	if got.Value == nil || math.Abs(*got.Value-want) > 1e-6 || got.Reason != nil {
		t.Errorf("%s = %v / %s, want %v with no reason", what, fp(got.Value), deref(got.Reason), want)
	}
}

// wantNoNum fails unless the nullable number is null with the reason.
func wantNoNum(t *testing.T, what string, got store.AnalyticsNullableNumber, reason string) {
	t.Helper()
	if got.Value != nil || got.Reason == nil || *got.Reason != reason {
		t.Errorf("%s = %v / %s, want null with reason %q", what, fp(got.Value), deref(got.Reason), reason)
	}
}

func fp(v *float64) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprint(*v)
}
