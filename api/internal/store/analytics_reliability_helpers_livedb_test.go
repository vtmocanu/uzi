package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4685: shared fixtures for the Reliability and latest-failure statement LiveDB tests.
// They reuse the Spend fixture (spendFixture) for users, repos, runs and usage.

// newRelFixture is newSpendFixture whose users, and with them their connections, repos, runs,
// usage, holds and captures, are deleted when the test ends. The Reliability tests seed
// hundreds of rows with current-looking dates, and TestRunUsageGenericPlanLiveDB's plan bounds
// are sensitive to how many runs and repos the shared database holds when it runs, so these
// tests leave it as they found it.
func newRelFixture(t *testing.T) *spendFixture {
	t.Helper()
	f := newSpendFixture(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM recovery_captures WHERE user_id = ANY($1) OR run_id IN (SELECT id FROM runs WHERE user_id = ANY($1))`,
			`DELETE FROM recovery_custody_holds WHERE user_id = ANY($1)`,
			`DELETE FROM runs WHERE user_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`,
		} {
			if _, err := f.pool.Exec(ctx, q, f.users); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	return f
}

// relHarnesses is the per-harness rolling series list a caller passes.
var relHarnesses = []string{"claude", "codex"}

// relCall is one Reliability or latest-failure statement invocation.
type relCall struct {
	Owner     uuid.UUID // zero = the all-scope statement
	Reveal    bool      // owner statement only
	Now       time.Time
	Zone      string
	Window    analytics.Window
	Repo      uuid.UUID
	Harness   string
	Kinds     []string // default runkind.Delivery()
	Breakdown string   // default primary_model
}

type relResult struct {
	Raw    []byte
	Doc    store.AnalyticsReliabilityDoc
	Bounds analytics.Bounds
}

func (c relCall) spend() spendCall {
	kinds := c.Kinds
	if kinds == nil {
		kinds = runkind.Delivery()
	}
	return spendCall{Owner: c.Owner, Now: c.Now, Zone: c.Zone, Window: c.Window, Repo: c.Repo, Harness: c.Harness, Kinds: kinds}
}

func relNullable(c relCall) (repo pgtype.UUID, harness pgtype.Text) {
	if c.Repo != uuid.Nil {
		repo = pgtype.UUID{Bytes: c.Repo, Valid: true}
	}
	if c.Harness != "" {
		harness = pgtype.Text{String: c.Harness, Valid: true}
	}
	return repo, harness
}

// relParams returns the owner and all-scope parameter structs for c and the bounds behind them.
func (f *spendFixture) relParams(c relCall) (store.AnalyticsOwnerReliabilityParams, store.AnalyticsAllReliabilityParams, analytics.Bounds) {
	f.t.Helper()
	sc := c.spend()
	c.Kinds = sc.Kinds
	if c.Breakdown == "" {
		c.Breakdown = "primary_model"
	}
	b := f.bounds(sc)
	repo, harness := relNullable(c)
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
	}
	o := store.AnalyticsOwnerReliabilityParams{
		UserID: c.Owner, RevealRepos: c.Reveal, LandableOrigins: workersvc.AllHumanLandableFailOrigins(),
		Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds), RollHarnesses: relHarnesses,
		RepoID: repo, Harness: harness, Kinds: c.Kinds, Breakdown: c.Breakdown,
	}
	a := store.AnalyticsAllReliabilityParams{
		LandableOrigins: o.LandableOrigins,
		Now:             o.Now, From: o.From, PreviousFrom: o.PreviousFrom, FoldFrom: o.FoldFrom,
		BucketStarts: o.BucketStarts, BucketEnds: o.BucketEnds,
		RollStarts: o.RollStarts, RollEnds: o.RollEnds, RollHarnesses: relHarnesses,
		RepoID: repo, Harness: harness, Kinds: c.Kinds, Breakdown: c.Breakdown,
	}
	return o, a, b
}

// rel runs the Reliability statement for c and decodes the document.
func (f *spendFixture) rel(c relCall) relResult {
	f.t.Helper()
	o, a, b := f.relParams(c)
	var raw []byte
	var err error
	if c.Owner != uuid.Nil {
		raw, err = f.q.AnalyticsOwnerReliability(f.ctx, o)
	} else {
		raw, err = f.q.AnalyticsAllReliability(f.ctx, a)
	}
	if err != nil {
		f.t.Fatalf("reliability statement: %v", err)
	}
	doc, err := store.DecodeAnalyticsReliabilityDoc(raw)
	if err != nil {
		f.t.Fatalf("%v\n%s", err, raw)
	}
	return relResult{Raw: raw, Doc: doc, Bounds: b}
}

// latestRow is the latest-failure result of either scope.
type latestRow struct {
	Found    bool
	FailedAt time.Time
	Origin   string
	RunID    uuid.UUID
	UserID   uuid.UUID
	Since    int64
}

func (f *spendFixture) latest(c relCall) latestRow {
	f.t.Helper()
	sc := c.spend()
	repo, harness := relNullable(c)
	if c.Owner != uuid.Nil {
		r, err := f.q.AnalyticsOwnerLatestFailure(f.ctx, store.AnalyticsOwnerLatestFailureParams{
			UserID: c.Owner, RepoID: repo, Harness: harness, Kinds: sc.Kinds})
		if err != nil {
			f.t.Fatalf("owner latest failure: %v", err)
		}
		return latestRow{r.Found, r.FailedAt.Time, r.Origin, r.RunID, r.FailedUserID, r.CompletedSinceLastFailure}
	}
	r, err := f.q.AnalyticsAllLatestFailure(f.ctx, store.AnalyticsAllLatestFailureParams{
		RepoID: repo, Harness: harness, Kinds: sc.Kinds})
	if err != nil {
		f.t.Fatalf("all latest failure: %v", err)
	}
	return latestRow{r.Found, r.FailedAt.Time, r.Origin, r.RunID, r.FailedUserID, r.CompletedSinceLastFailure}
}

// capture adds a recovery capture of run in the given state, owned by captureUser.
func (f *spendFixture) capture(run, captureUser uuid.UUID, state string) {
	f.t.Helper()
	hold, id := uuid.New(), uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,original_worker_identity)
		 VALUES($1,$2,$3,1,'released',$4,'ident')`, hold, captureUser, run, uuid.New())
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,expires_at)
		 VALUES($1,$2,$3,$4,'ident','aaaa1111',$1::uuid::text,$5,now()+interval '1 hour')`, id, hold, run, captureUser, state)
}

func (f *spendFixture) preservePatch(run uuid.UUID) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET preserved_patch = 'diff --git a/x b/x' WHERE id = $1`, run)
}

func (f *spendFixture) requeue(run uuid.UUID, n int) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET requeue_count = $2 WHERE id = $1`, run, n)
}

// fin is a finished run's spendRun: created at, finished an hour later.
func fin(owner, repo uuid.UUID, status, origin string, at time.Time) spendRun {
	return spendRun{Owner: owner, Repo: repo, Status: status, FailOrigin: origin, At: at, Finished: at.Add(time.Hour)}
}

// wantOutcomes fails unless got is exactly the five classes.
func wantOutcomes(t *testing.T, what string, got store.AnalyticsOutcomes, completed, cancelled, planRejected, baseDiverged, failed int64) {
	t.Helper()
	want := store.AnalyticsOutcomes{Completed: completed, Cancelled: cancelled, PlanRejected: planRejected, BaseDiverged: baseDiverged, Failed: failed}
	if got != want {
		t.Errorf("%s outcomes = %+v, want %+v", what, got, want)
	}
}

// wantRate checks a null-or-number rate with its reason.
func wantRate(t *testing.T, what string, got store.AnalyticsNullableNumber, value float64, reason string) {
	t.Helper()
	if reason != "" {
		if got.Value != nil || got.Reason == nil || *got.Reason != reason {
			t.Errorf("%s = value %v reason %v, want null with reason %q", what, got.Value, deref(got.Reason), reason)
		}
		return
	}
	if got.Value == nil || math.Abs(*got.Value-value) > 6e-7 || got.Reason != nil {
		t.Errorf("%s = value %v reason %v, want %v and no reason", what, got.Value, deref(got.Reason), value)
	}
}

// wantBlock checks a totals block: finished, the five classes and requeued runs.
func wantBlock(t *testing.T, what string, b store.AnalyticsReliabilityBlock, finished, completed, cancelled, planRejected, baseDiverged, failed, requeued int64) {
	t.Helper()
	if b.Finished != finished || b.RequeuedRuns != requeued {
		t.Errorf("%s finished/requeued = %d/%d, want %d/%d", what, b.Finished, b.RequeuedRuns, finished, requeued)
	}
	wantOutcomes(t, what, b.Outcomes, completed, cancelled, planRejected, baseDiverged, failed)
	if sum := b.Outcomes.Completed + b.Outcomes.Cancelled + b.Outcomes.PlanRejected + b.Outcomes.BaseDiverged + b.Outcomes.Failed; sum != b.Finished {
		t.Errorf("%s: classes sum to %d, finished is %d", what, sum, b.Finished)
	}
}

// relBucketIdx returns the index of the bucket starting at start.
func relBucketIdx(t *testing.T, b analytics.Bounds, start time.Time) int {
	t.Helper()
	for i, bk := range b.Buckets {
		if bk.Start.Equal(start) {
			return i
		}
	}
	t.Fatalf("no bucket starting at %s", start)
	return -1
}

func relRowByKey(t *testing.T, rows []store.AnalyticsReliabilityGroup, key string) store.AnalyticsReliabilityGroup {
	t.Helper()
	for _, r := range rows {
		if r.Key != nil && *r.Key == key {
			return r
		}
	}
	t.Fatalf("no breakdown row with key %q (rows %d)", key, len(rows))
	return store.AnalyticsReliabilityGroup{}
}

func relSeries(t *testing.T, d store.AnalyticsReliabilityDoc, harness string) []store.AnalyticsReliabilityPoint {
	t.Helper()
	for _, s := range d.Rolling.Harnesses {
		if s.Harness == harness {
			return s.Points
		}
	}
	t.Fatalf("no rolling series for harness %q", harness)
	return nil
}

// outcomeSet seeds the shared parity population for one owner, all inside the 30d window that
// ends at spendNow, and returns the ids the tests refer to. Expected figures (Delivery only):
// finished 18 = completed 4 + cancelled 2 + plan_rejected 1 + base_diverged 1 + failed 10;
// failed origins agent_failure 3, unknown 1, worker_lost 1, workflow_scope_missing 2,
// push_secret_blocked 1, history_rewritten 1, finalize_base_align_conflict 1; needs_landing 2;
// one in-flight run. other is a second user, owner of one capture that must not count.
type outcomeSet struct {
	latestFailure uuid.UUID // the latest eligible failure (finalize_base_align_conflict)
	lastCompleted uuid.UUID // the only completed run finishing after it
}

func (f *spendFixture) outcomeSet(o spendOwner, repo uuid.UUID, other spendOwner) outcomeSet {
	f.t.Helper()
	day := func(k int) time.Time { return spendDay(k, 9) }
	var first uuid.UUID
	run := func(status, origin string, k int, kind string) uuid.UUID {
		r := fin(o.User, repo, status, origin, day(k))
		r.Kind = kind
		if kind == runkind.MRRework {
			r.Target = first
		}
		return f.run(r)
	}
	first = run("completed", "", 3, runkind.Issue)
	run("completed", "", 4, runkind.MRRework)
	run("completed", "", 5, runkind.Task)
	run("cancelled", "", 6, runkind.Issue)
	run("failed", "plan_rejected", 7, runkind.Issue)
	run("failed", "base_diverged", 8, runkind.Issue)
	run("failed", "agent_failure", 9, runkind.Issue)
	run("failed", "agent_failure", 10, runkind.Issue)
	run("failed", "", 11, runkind.Issue)
	run("failed", "worker_lost", 12, runkind.Issue)
	// needs_landing: a landable origin with an available capture of the run's own owner ...
	n1 := run("failed", "workflow_scope_missing", 13, runkind.Issue)
	f.capture(n1, o.User, "available")
	// ... or a preserved patch.
	n2 := run("failed", "push_secret_blocked", 14, runkind.Issue)
	f.preservePatch(n2)
	// Negatives: landable but nothing recoverable; recoverable but not landable; a capture
	// owned by another user; a capture that is not available.
	run("failed", "workflow_scope_missing", 15, runkind.Issue)
	n4 := run("failed", "agent_failure", 16, runkind.Issue)
	f.preservePatch(n4)
	n5 := run("failed", "history_rewritten", 17, runkind.Issue)
	f.capture(n5, other.User, "available")
	n6 := run("failed", "finalize_base_align_conflict", 18, runkind.Issue)
	f.capture(n6, o.User, "expired")
	// Recoverable work on runs that are not failed does not count, even when the run also
	// carries a human-landable fail_origin (so only the status guard keeps it out of needs_landing).
	c4 := run("completed", "push_secret_blocked", 19, runkind.Issue)
	f.preservePatch(c4)
	c5 := run("cancelled", "workflow_scope_missing", 19, runkind.Issue)
	f.preservePatch(c5)
	f.capture(c5, o.User, "available")
	// In flight: created, never finished.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: day(20)})
	return outcomeSet{latestFailure: n6, lastCompleted: c4}
}

// seedSupporting adds failed judge, cross_check and chat runs, which the Reliability
// statements exclude and the existing outcome aggregates (chat apart) count.
func (f *spendFixture) seedSupporting(o spendOwner, repo uuid.UUID, target uuid.UUID) {
	f.t.Helper()
	at := spendDay(21, 9)
	f.run(spendRun{Owner: o.User, Kind: runkind.Judge, Target: target, Status: "failed", FailOrigin: "agent_failure", At: at, Finished: at.Add(time.Hour)})
	at = spendDay(22, 9)
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, created_at, finished_at,
		                   status_since, harness, fail_origin, target_run_id, report_only, budget_wall_seconds)
		 VALUES ($1, $2, $3, 't', 'd', 'failed', 'cross_check', $4, $5, $4, 'claude', 'agent_failure', $6, true, 600)`,
		uuid.New(), o.User, repo, at, at.Add(time.Hour), target)
	at = spendDay(23, 9)
	f.run(spendRun{Owner: o.User, Kind: runkind.Chat, Status: "failed", FailOrigin: "agent_failure", At: at, Finished: at.Add(time.Hour)})
}
