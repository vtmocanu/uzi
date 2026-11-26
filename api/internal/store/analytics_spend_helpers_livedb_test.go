package store_test

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684: shared fixtures for the Spend statement LiveDB tests. Every test seeds its own
// users and repos (unique per call), so the shared LiveDB database does not couple tests.
// The Spend statements run against real calendar bounds from analytics.ComputeBounds.

var spendSeq atomic.Int64

// spendFixture seeds users, repos, runs and usage through SQL and calls the Spend statements.
type spendFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	epochs map[string]int
	users  []uuid.UUID // owners this fixture created
}

func newSpendFixture(t *testing.T) *spendFixture {
	t.Helper()
	ctx, pool := openAnalyticsViewsDB(t)
	return &spendFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), epochs: map[string]int{}}
}

// claimYear makes a calendar year private to the calling test: the all-scope statement has no
// owner predicate, so a test that asserts all-scope figures seeds its runs in a year no other
// test uses and deletes every run created in that year before it starts (a rerun against the
// same database) and again when it ends. run_usage rows go with their runs (ON DELETE CASCADE).
func (f *spendFixture) claimYear(year int) {
	f.t.Helper()
	// Deletions are limited to rows of fixture-created users: at the start, any user with the
	// fixture's spend-<tag>-<id>@e2e email (leftovers of an earlier run); at the end, this
	// fixture's own users. An unrelated run in the same year is never touched.
	clear := func(userClause string, arg any) {
		if _, err := f.pool.Exec(context.Background(),
			`DELETE FROM runs WHERE created_at >= make_timestamptz($1, 1, 1, 0, 0, 0, 'UTC')
			                    AND created_at <  make_timestamptz($1 + 1, 1, 1, 0, 0, 0, 'UTC')
			                    AND `+userClause, year, arg); err != nil {
			f.t.Fatalf("clear year %d: %v", year, err)
		}
	}
	clear(`user_id IN (SELECT id FROM users WHERE email LIKE $2)`, "spend-%@e2e")
	f.t.Cleanup(func() { clear(`user_id = ANY($2)`, f.users) })
}

// spendOwner is a user with one forge connection.
type spendOwner struct{ User, Conn uuid.UUID }

func (f *spendFixture) owner(tag string) spendOwner {
	f.t.Helper()
	o := spendOwner{User: uuid.New(), Conn: uuid.New()}
	mustExec(f.ctx, f.t, f.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		o.User, fmt.Sprintf("spend-%s-%s@e2e", tag, o.User))
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, o.Conn, o.User, []byte{0x1})
	f.users = append(f.users, o.User)
	return o
}

// repo adds a repo to the owner's connection and returns its id and path.
func (f *spendFixture) repo(o spendOwner, tag string) (uuid.UUID, string) {
	f.t.Helper()
	return f.repoWithProject(o, tag, spendSeq.Add(1)+9_000_000)
}

// repoWithProject is repo with an explicit forge_project_id, so two owners' connections can
// reference the same external project.
func (f *spendFixture) repoWithProject(o spendOwner, tag string, forgeProjectID int64) (uuid.UUID, string) {
	f.t.Helper()
	id := uuid.New()
	path := fmt.Sprintf("g/spend-%s-%s", tag, id)
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		id, o.Conn, forgeProjectID, path)
	return id, path
}

// spendRun describes one seeded run. Zero values: kind issue, status completed, harness claude.
type spendRun struct {
	Owner      uuid.UUID
	Repo       uuid.UUID // zero = no repo (chat and job only)
	Kind       string
	Status     string
	FailOrigin string
	At         time.Time // created_at
	Finished   time.Time // finished_at; zero = NULL
	StatusAt   time.Time // status_since; zero = At
	Harness    string
	MR         int64 // mr_iid; 0 = none
	MRState    string
	Target     uuid.UUID // judge and mr_rework target run
}

func (f *spendFixture) run(r spendRun) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if r.Kind == "" {
		r.Kind = runkind.Issue
	}
	if r.Status == "" {
		r.Status = "completed"
	}
	if r.Harness == "" {
		r.Harness = "claude"
	}
	if r.StatusAt.IsZero() {
		r.StatusAt = r.At
	}
	var (
		repo       any
		issueIID   any
		branch     any
		pipelineID any
		pipeRef    any
		target     any
		jobType    any
		finished   any
		failOrigin any
		mr         any
		mrState    any
	)
	if r.Repo != uuid.Nil {
		repo = r.Repo
	}
	n := spendSeq.Add(1)
	switch r.Kind {
	case runkind.Issue, runkind.SelfImprove:
		issueIID = n
	case runkind.Task:
		branch = fmt.Sprintf("uzi/task/%s", id)
	case runkind.CIFix:
		pipelineID, pipeRef = n, fmt.Sprintf("refs/pipe/%d", n)
	case runkind.MRRework:
		pipeRef = fmt.Sprintf("refs/mr/%d", n)
		if r.MR == 0 {
			r.MR = n
		}
		target = r.Target
	case runkind.Judge:
		target = r.Target
	case runkind.Job:
		jobType = "research"
	}
	if !r.Finished.IsZero() {
		finished = r.Finished
	}
	if r.FailOrigin != "" {
		failOrigin = r.FailOrigin
	}
	if r.MR != 0 {
		mr = r.MR
	}
	if r.MRState != "" {
		mrState = r.MRState
	}
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   created_at, finished_at, status_since, harness, fail_origin, mr_iid, mr_state,
		                   branch, pipeline_id, pipeline_ref, target_run_id, job_type)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		id, r.Owner, repo, issueIID, r.Status, r.Kind, r.At, finished, r.StatusAt, r.Harness, failOrigin,
		mr, mrState, branch, pipelineID, pipeRef, target, jobType)
	return id
}

// usage adds one per_leg run_usage row; each call for the same run and model uses a fresh
// lineage epoch, so the view sums the calls rather than taking the MAX of one leg.
func (f *spendFixture) usage(run uuid.UUID, model, status, cost string, in, read, creation, out int64) {
	f.t.Helper()
	key := run.String() + "/" + model
	epoch := f.epochs[key]
	f.epochs[key] = epoch + 1
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
		     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
		 VALUES ($1, $2, $3, $4, 0, 'per_leg', $5, $6, $7, $8, $9::numeric, $10)`,
		run, fmt.Sprintf("s-%d", spendSeq.Add(1)), model, epoch, in, read, creation, out, cost, status)
}

// metered adds a plain metered single-model usage row with fixed token counts
// (input 100, cache read 50, cache creation 10, output 20).
func (f *spendFixture) metered(run uuid.UUID, model, cost string) {
	f.t.Helper()
	f.usage(run, model, "metered", cost, 100, 50, 10, 20)
}

// spendCall is one Spend statement invocation.
type spendCall struct {
	Owner     uuid.UUID // zero = the all-scope statement
	Reveal    bool      // owner statement only
	Now       time.Time
	Zone      string // default UTC
	SessionTZ string // session TimeZone to run under; default the pool's
	Window    analytics.Window
	Repo      uuid.UUID
	Harness   string
	Kinds     []string // default runkind.All()
	Split     string   // default harness
	Breakdown string   // default kind
}

// spendResult is a decoded response with its raw bytes and the bounds that produced it.
type spendResult struct {
	Raw    []byte
	Doc    store.AnalyticsSpendDoc
	Bounds analytics.Bounds
}

func spendTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func spendTSs(ts []time.Time) []pgtype.Timestamptz {
	out := make([]pgtype.Timestamptz, len(ts))
	for i, t := range ts {
		out[i] = spendTS(t)
	}
	return out
}

// bounds computes the real calendar for the call, with the scope's earliest run as the
// history start (the same flow the handler uses).
func (f *spendFixture) bounds(c spendCall) analytics.Bounds {
	f.t.Helper()
	if c.Zone == "" {
		c.Zone = "UTC"
	}
	if c.Window == "" {
		c.Window = analytics.Window30d
	}
	if c.Kinds == nil {
		c.Kinds = runkind.All()
	}
	loc, err := time.LoadLocation(c.Zone)
	if err != nil {
		f.t.Fatalf("zone %q: %v", c.Zone, err)
	}
	var repo pgtype.UUID
	if c.Repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: c.Repo, Valid: true}
	}
	var harness pgtype.Text
	if c.Harness != "" {
		harness = pgtype.Text{String: c.Harness, Valid: true}
	}
	var hist pgtype.Timestamptz
	if c.Owner != uuid.Nil {
		hist, err = f.q.AnalyticsOwnerHistoryStart(f.ctx, store.AnalyticsOwnerHistoryStartParams{
			UserID: c.Owner, RepoID: repo, Harness: harness, Kinds: c.Kinds})
	} else {
		hist, err = f.q.AnalyticsAllHistoryStart(f.ctx, store.AnalyticsAllHistoryStartParams{
			RepoID: repo, Harness: harness, Kinds: c.Kinds})
	}
	if err != nil {
		f.t.Fatalf("history start: %v", err)
	}
	var earliest *time.Time
	if hist.Valid {
		e := hist.Time
		earliest = &e
	}
	b, err := analytics.ComputeBounds(c.Now, loc, c.Window, earliest)
	if err != nil {
		f.t.Fatalf("ComputeBounds: %v", err)
	}
	return b
}

// call runs the statement for c and decodes the document.
func (f *spendFixture) call(c spendCall) spendResult {
	f.t.Helper()
	if c.Kinds == nil {
		c.Kinds = runkind.All()
	}
	if c.Split == "" {
		c.Split = "harness"
	}
	if c.Breakdown == "" {
		c.Breakdown = "kind"
	}
	b := f.bounds(c)
	var repo pgtype.UUID
	if c.Repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: c.Repo, Valid: true}
	}
	var harness pgtype.Text
	if c.Harness != "" {
		harness = pgtype.Text{String: c.Harness, Valid: true}
	}
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
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
		raw, err = q.AnalyticsOwnerSpend(f.ctx, store.AnalyticsOwnerSpendParams{
			UserID: c.Owner, RevealRepos: c.Reveal,
			Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
			RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
			RepoID: repo, Harness: harness, Kinds: c.Kinds, DeliveryKinds: runkind.Delivery(),
			Split: c.Split, Breakdown: c.Breakdown,
		})
	} else {
		raw, err = q.AnalyticsAllSpend(f.ctx, store.AnalyticsAllSpendParams{
			Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
			RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
			RepoID: repo, Harness: harness, Kinds: c.Kinds, DeliveryKinds: runkind.Delivery(),
			Split: c.Split, Breakdown: c.Breakdown,
		})
	}
	if err != nil {
		f.t.Fatalf("spend statement: %v", err)
	}
	doc, err := store.DecodeAnalyticsSpendDoc(raw)
	if err != nil {
		f.t.Fatalf("%v\n%s", err, raw)
	}
	return spendResult{Raw: raw, Doc: doc, Bounds: b}
}

// ownerArgs returns the exact positional arguments the generated AnalyticsOwnerSpend method
// passes for c, read through a capturing DBTX so their order is never assumed.
func (f *spendFixture) ownerArgs(c spendCall) []any {
	f.t.Helper()
	if c.Kinds == nil {
		c.Kinds = runkind.All()
	}
	if c.Split == "" {
		c.Split = "harness"
	}
	if c.Breakdown == "" {
		c.Breakdown = "kind"
	}
	b := f.bounds(c)
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
	}
	capture := &capturingDBTX{}
	_, _ = store.New(capture).AnalyticsOwnerSpend(f.ctx, store.AnalyticsOwnerSpendParams{
		UserID: c.Owner, RevealRepos: c.Reveal,
		Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		Kinds: c.Kinds, DeliveryKinds: runkind.Delivery(), Split: c.Split, Breakdown: c.Breakdown,
	})
	return capture.args
}

// ratOf parses an exact decimal string.
func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal: %q", s)
	}
	return r
}

// wantDollars fails unless got is the exact decimal want.
func wantDollars(t *testing.T, what, got, want string) {
	t.Helper()
	if ratOf(t, got).Cmp(ratOf(t, want)) != 0 {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// wantRatio checks a ratio stat's numerator, denominator and (value | reason).
func wantRatio(t *testing.T, what string, got store.AnalyticsRatioStat, num string, den int64, value, reason string) {
	t.Helper()
	wantDollars(t, what+" numerator", got.Numerator, num)
	if got.Denominator != den {
		t.Errorf("%s denominator = %d, want %d", what, got.Denominator, den)
	}
	switch {
	case value != "":
		if got.Value == nil {
			t.Errorf("%s value is null (reason %v), want %s", what, deref(got.Reason), value)
		} else {
			wantDollars(t, what+" value", *got.Value, value)
		}
		if got.Reason != nil {
			t.Errorf("%s has reason %q with a value", what, *got.Reason)
		}
	default:
		if got.Value != nil {
			t.Errorf("%s value = %s, want null (%s)", what, *got.Value, reason)
		}
		if got.Reason == nil || *got.Reason != reason {
			t.Errorf("%s reason = %v, want %q", what, deref(got.Reason), reason)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// wantDisclosure checks the four run counts and the incomplete flag.
func wantDisclosure(t *testing.T, what string, got store.AnalyticsDisclosure, metered, subscription, unreported, missing int64, incomplete bool) {
	t.Helper()
	want := store.AnalyticsDisclosure{Metered: metered, Subscription: subscription, Unreported: unreported, Missing: missing, Incomplete: incomplete}
	if got != want {
		t.Errorf("%s disclosure = %+v, want %+v", what, got, want)
	}
}

// rollCell returns the rolling point for the bucket with label date d.
func rollCell(t *testing.T, res spendResult, p store.AnalyticsRatioPanel, d analytics.Date) store.AnalyticsRatioCell {
	t.Helper()
	for i, bk := range res.Bounds.Buckets {
		if bk.Label == d {
			if i >= len(p.Rolling) || p.Rolling[i].Index != i {
				t.Fatalf("rolling point %d missing or misindexed", i)
			}
			return p.Rolling[i]
		}
	}
	t.Fatalf("no bucket labelled %s", d)
	return store.AnalyticsRatioCell{}
}

// bucketCell returns the panel cell for the bucket with label date d.
func bucketCell(t *testing.T, res spendResult, p store.AnalyticsRatioPanel, d analytics.Date) store.AnalyticsRatioCell {
	t.Helper()
	for i, bk := range res.Bounds.Buckets {
		if bk.Label == d {
			if i >= len(p.Buckets) || p.Buckets[i].Index != i {
				t.Fatalf("bucket cell %d missing or misindexed", i)
			}
			return p.Buckets[i]
		}
	}
	t.Fatalf("no bucket labelled %s", d)
	return store.AnalyticsRatioCell{}
}

func utcAt(y int, m time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, time.UTC)
}
