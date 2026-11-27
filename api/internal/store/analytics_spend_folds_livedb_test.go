package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4700 M3: AnalyticsAllSpend reads a terminal run's stored fold (run_usage_model_folds)
// when its run_usage_fold_state row is fresh, and folds every other run live from
// run_usage_model_totals. AnalyticsOwnerSpend never reads folds. These tests prove the two
// paths produce byte-identical documents, that freshness is honoured in every direction, and
// that the generic plan runs the live lateral only for the fallback cohort. Every test skips
// without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).
//
// The all-scope statement has no owner predicate, so each fixture owns a calendar year no other
// test uses (claimYear) and puts "now" late in that year, so every window's runs, the previous
// windows' included, stay inside the year.

// foldSpend is a varied all-scope fixture.
type foldSpend struct {
	*spendFixture
	now      time.Time
	owners   []spendOwner
	repos    []uuid.UUID
	all      []uuid.UUID // every run seeded, chat included
	eligible []uuid.UUID // terminal, non-chat: what the materializer folds
	// Roles used by the freshness tests. All four carry usage and were created within 7 days.
	fresh   uuid.UUID // completed, one model
	stale   uuid.UUID // completed, two models
	active  uuid.UUID // running
	missing uuid.UUID // completed
}

var foldTerminal = map[string]bool{"": true, "completed": true, "failed": true, "cancelled": true}

func (fx *foldSpend) add(r spendRun) uuid.UUID {
	id := fx.run(r)
	fx.all = append(fx.all, id)
	if r.Kind != runkind.Chat && foldTerminal[r.Status] {
		fx.eligible = append(fx.eligible, id)
	}
	return id
}

// cumulative adds two session_cumulative legs, the view's other usage basis.
func (fx *foldSpend) cumulative(run uuid.UUID, model string) {
	fx.t.Helper()
	for epoch, v := range []struct {
		in, out int64
		cost    string
	}{{100, 40, "1.000000"}, {260, 90, "2.500000"}} {
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
			     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
			 VALUES ($1, $2, $3, $4, 0, 'session_cumulative', $5, 10, 2, $6, $7::numeric, 'metered')`,
			run, fmt.Sprintf("cum-%s-%d", run, epoch), model, epoch, v.in, v.out, v.cost)
	}
}

// usageFor gives run i one of eight usage shapes: metered, two models, subscription,
// unreported, metered plus subscription (a mixed run), none, three models with an output tie,
// and session cumulative.
func (fx *foldSpend) usageFor(id uuid.UUID, i int) {
	in, rd, cr, out := int64(100+i), int64(50+i%7), int64(i%5), int64(20+(i*13)%97)
	cost := fmt.Sprintf("%d.%06d", i%9, (i*7919)%1000000)
	switch i % 8 {
	case 0:
		fx.usage(id, "model-a", "metered", cost, in, rd, cr, out)
	case 1:
		fx.usage(id, "model-a", "metered", cost, in, rd, cr, out)
		fx.usage(id, "model-b", "metered", "0.250000", in/2, rd, cr, out+5)
	case 2:
		fx.usage(id, "model-b", "subscription", "0", in, rd, cr, out)
	case 3:
		fx.usage(id, "model-c", "unreported", "0", in, rd, cr, out)
	case 4:
		fx.usage(id, "model-a", "metered", cost, in, rd, cr, out)
		fx.usage(id, "model-b", "subscription", "0", in, rd, cr, out+1)
	case 5: // a terminal run with no usage at all
	case 6:
		fx.usage(id, "zeta", "metered", cost, in, rd, cr, 50)
		fx.usage(id, "Alpha", "metered", "0.125000", in, rd, cr, 50)
		fx.usage(id, "mid", "metered", "0.500000", in, rd, cr, 10)
	case 7:
		fx.cumulative(id, "model-a")
	}
}

func seedFoldSpend(t *testing.T, year int) *foldSpend {
	t.Helper()
	f := newSpendFixture(t)
	f.claimYear(year)
	fx := &foldSpend{spendFixture: f, now: utcAt(year, 12, 15, 12, 0, 0)}
	for _, tag := range []string{"fa", "fb", "fc"} {
		o := f.owner(tag)
		fx.owners = append(fx.owners, o)
		r, _ := f.repo(o, tag)
		fx.repos = append(fx.repos, r)
	}
	r2, _ := f.repo(fx.owners[0], "fa2")
	fx.repos = append(fx.repos, r2)

	ago := func(days float64) time.Time { return fx.now.Add(-time.Duration(days * 24 * float64(time.Hour))) }
	// Ages straddle every window: inside 7d, the previous 7d, 30d, its previous, 90d, its
	// previous, and only the all window.
	ages := []float64{0.2, 1, 3, 5.5, 6.5, 8, 12, 20, 29, 35, 45, 65, 85, 95, 130, 200}
	kinds := []string{runkind.Issue, runkind.Prompt, runkind.MRRework, runkind.Judge, runkind.CIFix, runkind.Job, runkind.Task}
	statuses := []string{"completed", "completed", "failed", "cancelled", "completed"}

	anchor := fx.add(spendRun{Owner: fx.owners[0].User, Repo: fx.repos[0], At: ago(10), Finished: ago(10).Add(time.Hour)})
	fx.usage(anchor, "model-a", "metered", "0.750000", 10, 5, 1, 4)
	for i := 0; i < 64; i++ {
		at := ago(ages[i%len(ages)]).Add(-time.Duration(i) * time.Minute)
		r := spendRun{
			Owner: fx.owners[i%3].User, Repo: fx.repos[i%len(fx.repos)], Kind: kinds[i%len(kinds)],
			Status: statuses[i%len(statuses)], Harness: []string{"claude", "codex"}[(i/2)%2], At: at,
		}
		if i%13 == 5 {
			r.Status = "running"
		}
		if r.Status == "failed" {
			r.FailOrigin = "agent_failure"
		}
		if r.Status != "running" {
			r.Finished = at.Add(time.Duration(20+i) * time.Minute)
		}
		switch r.Kind {
		case runkind.Job:
			r.Repo = uuid.Nil
		case runkind.Judge: // runs_kind_shape: a judge has no repo
			r.Repo, r.Target = uuid.Nil, anchor
		case runkind.MRRework:
			r.Target = anchor
		}
		if i%6 == 0 {
			r.MR = int64(200 + i%5)
			r.MRState = "opened"
			if i%12 == 0 {
				r.MRState = "merged"
			}
		}
		fx.usageFor(fx.add(r), i)
	}
	// Chat runs are untracked: no fold, no state, never a cost.
	for i := 0; i < 3; i++ {
		ch := fx.add(spendRun{Owner: fx.owners[1].User, Kind: runkind.Chat, At: ago(float64(2 + i))})
		f.metered(ch, "model-a", "9.990000")
	}

	role := func(days float64, status string, models ...string) uuid.UUID {
		r := spendRun{Owner: fx.owners[0].User, Repo: fx.repos[0], At: ago(days), Status: status}
		if status != "running" {
			r.Finished = r.At.Add(30 * time.Minute)
		}
		id := fx.add(r)
		for i, m := range models {
			f.usage(id, m, "metered", fmt.Sprintf("%d.500000", i+1), 1000, 300, 20, 400+int64(i))
		}
		return id
	}
	fx.fresh = role(3, "completed", "model-a")
	fx.stale = role(2, "completed", "model-a", "model-b")
	fx.active = role(1, "running", "model-a")
	fx.missing = role(4, "completed", "model-b")
	return fx
}

// foldVariant is one statement invocation (one document) of the byte-equality suite.
type foldVariant struct {
	name string
	c    spendCall
}

// variants: every window under every split and breakdown dimension the statement supports,
// plus a harness-filtered and a kind-filtered document.
func (fx *foldSpend) variants() []foldVariant {
	var out []foldVariant
	pairs := [][2]string{{"usage_model", "primary_model"}, {"harness", "harness"}, {"repo", "repo"}, {"kind", "kind"}, {"user", "user"}}
	for _, w := range []analytics.Window{analytics.Window7d, analytics.Window30d, analytics.Window90d, analytics.WindowAll} {
		for _, p := range pairs {
			out = append(out, foldVariant{fmt.Sprintf("%s/%s+%s", w, p[0], p[1]),
				spendCall{Now: fx.now, Window: w, Split: p[0], Breakdown: p[1]}})
		}
	}
	return append(out,
		foldVariant{"30d/harness=codex", spendCall{Now: fx.now, Window: analytics.Window30d, Harness: "codex", Split: "repo", Breakdown: "primary_model"}},
		foldVariant{"all/delivery-kinds", spendCall{Now: fx.now, Window: analytics.WindowAll, Kinds: runkind.Delivery(), Split: "usage_model", Breakdown: "kind"}},
	)
}

func (fx *foldSpend) docs(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, v := range fx.variants() {
		out[v.name] = fx.call(v.c).Raw
	}
	return out
}

// ownerDocs: the owner statement for the first owner, with and without repo reveal.
func (fx *foldSpend) ownerDocs(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, w := range []analytics.Window{analytics.Window30d, analytics.WindowAll} {
		for _, p := range [][2]string{{"usage_model", "primary_model"}, {"harness", "kind"}} {
			for _, reveal := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s+%s/reveal=%t", w, p[0], p[1], reveal)
				out[name] = fx.call(spendCall{Owner: fx.owners[0].User, Reveal: reveal, Now: fx.now, Window: w, Split: p[0], Breakdown: p[1]}).Raw
			}
		}
	}
	return out
}

// outputTokens is the 30d all-scope totals' output tokens, the figure a corrupted fold moves.
func (fx *foldSpend) outputTokens(t *testing.T) int64 {
	t.Helper()
	return fx.call(spendCall{Now: fx.now, Window: analytics.Window30d, Split: "usage_model", Breakdown: "primary_model"}).Doc.Totals.Current.OutputTokens
}

// sanity: every window of the fixture holds runs and cost, so equality is not between two
// empty documents.
func (fx *foldSpend) sanity(t *testing.T) {
	t.Helper()
	for _, w := range []analytics.Window{analytics.Window7d, analytics.Window30d, analytics.Window90d, analytics.WindowAll} {
		cur := fx.call(spendCall{Now: fx.now, Window: w}).Doc.Totals.Current
		if cur.Runs == 0 || ratOf(t, cur.Cost).Sign() <= 0 || cur.OutputTokens == 0 {
			t.Fatalf("window %s of the fixture is empty (runs %d cost %s output %d)", w, cur.Runs, cur.Cost, cur.OutputTokens)
		}
	}
}

func (fx *foldSpend) materialize(t *testing.T, ids []uuid.UUID) int64 {
	t.Helper()
	var total int64
	for len(ids) > 0 {
		n := min(len(ids), store.MaxRunUsageFoldBatch)
		got, err := store.MaterializeRunUsageFolds(fx.ctx, fx.pool, ids[:n])
		if err != nil {
			t.Fatalf("MaterializeRunUsageFolds: %v", err)
		}
		total += got
		ids = ids[n:]
	}
	return total
}

func (fx *foldSpend) eligibleExcept(skip ...uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
next:
	for _, id := range fx.eligible {
		for _, s := range skip {
			if id == s {
				continue next
			}
		}
		out = append(out, id)
	}
	return out
}

func (fx *foldSpend) count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := fx.pool.QueryRow(fx.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (fx *foldSpend) stateRows(t *testing.T, ids []uuid.UUID, fresh bool) int64 {
	t.Helper()
	return fx.count(t, `SELECT count(*) FROM run_usage_fold_state WHERE run_id = ANY($1) AND ($2::boolean IS NOT TRUE OR NOT stale)`, ids, fresh)
}

// corrupt rewrites a run's stored fold rows to a wrong output_tokens and returns the change in
// the sum of output tokens it made.
func (fx *foldSpend) corrupt(t *testing.T, run uuid.UUID) int64 {
	t.Helper()
	var oldSum, n int64
	if err := fx.pool.QueryRow(fx.ctx, `SELECT COALESCE(sum(output_tokens), 0), count(*) FROM run_usage_model_folds WHERE run_id = $1`, run).Scan(&oldSum, &n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("run %s has no fold rows to corrupt", run)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE run_usage_model_folds SET output_tokens = 999999 WHERE run_id = $1`, run)
	return n*999999 - oldSum
}

func (fx *foldSpend) markStale(t *testing.T, ids []uuid.UUID, stale bool) {
	t.Helper()
	mustExec(fx.ctx, t, fx.pool, `UPDATE run_usage_fold_state SET stale = $2 WHERE run_id = ANY($1)`, ids, stale)
}

// requireSameDocs fails with the first differing document and a bounded excerpt around its
// first differing byte.
func requireSameDocs(t *testing.T, what string, want, got map[string][]byte) {
	t.Helper()
	var bad []string
	for name, w := range want {
		if !bytes.Equal(w, got[name]) {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return
	}
	sort.Strings(bad)
	w, g := want[bad[0]], got[bad[0]]
	at := 0
	for at < len(w) && at < len(g) && w[at] == g[at] {
		at++
	}
	excerpt := func(b []byte) string {
		lo, hi := max(at-60, 0), min(at+60, len(b))
		return string(b[lo:hi])
	}
	t.Errorf("%s: %d of %d documents differ; first %q at byte %d (lengths %d vs %d):\n want ...%s...\n got  ...%s...",
		what, len(bad), len(want), bad[0], at, len(w), len(g), excerpt(w), excerpt(g))
}

// TestAnalyticsSpendFoldsByteEqualLiveDB: with no state rows every run is read live; after
// materializing every eligible run the stored path serves them, and every document of every
// window, split and breakdown is byte-identical.
func TestAnalyticsSpendFoldsByteEqualLiveDB(t *testing.T) {
	fx := seedFoldSpend(t, 2007)
	fx.sanity(t)
	if n := fx.stateRows(t, fx.all, false); n != 0 {
		t.Fatalf("%d fixture runs already have a state row; the baseline is not live", n)
	}
	live := fx.docs(t)

	folded := fx.materialize(t, fx.eligible)
	if folded != int64(len(fx.eligible)) || folded < 40 {
		t.Fatalf("folded %d runs, want all %d eligible (and at least 40)", folded, len(fx.eligible))
	}
	if n := fx.stateRows(t, fx.eligible, true); n != folded {
		t.Fatalf("%d fresh state rows after folding %d runs", n, folded)
	}
	if n := fx.count(t, `SELECT count(*) FROM run_usage_model_folds WHERE run_id = ANY($1)`, fx.eligible); n < 40 {
		t.Fatalf("only %d fold rows stored: the stored path would barely be exercised", n)
	}
	if n := fx.stateRows(t, fx.all, false); n != folded {
		t.Fatalf("%d state rows, want only the %d folded runs (chat and running runs have none)", n, folded)
	}
	requireSameDocs(t, "stored vs live", live, fx.docs(t))
}

// TestAnalyticsSpendFoldsRefoldLiveDB: a write through UpsertRunUsage marks the run stale, the
// document then follows the live data, and re-materializing keeps it equal.
func TestAnalyticsSpendFoldsRefoldLiveDB(t *testing.T) {
	fx := seedFoldSpend(t, 2006)
	fx.materialize(t, fx.eligible)
	// Non-vacuity: the run must start fresh (stored path), or the stale check below proves nothing.
	if n := fx.stateRows(t, []uuid.UUID{fx.fresh}, true); n != 1 {
		t.Fatalf("the run to refold is not fresh after materializing (fresh rows = %d, want 1)", n)
	}
	before := fx.docs(t)

	arg := upsertArg(t, fx.fresh, "s-refold", "model-refold", 0, 5000)
	if err := store.New(fx.pool).UpsertRunUsage(fx.ctx, arg); err != nil {
		t.Fatalf("UpsertRunUsage: %v", err)
	}
	if n := fx.stateRows(t, []uuid.UUID{fx.fresh}, true); n != 0 {
		t.Fatal("the refolded run is still marked fresh after a run_usage write")
	}
	afterWrite := fx.docs(t)
	if bytes.Equal(before["30d/usage_model+primary_model"], afterWrite["30d/usage_model+primary_model"]) {
		t.Fatal("the write did not change the document: the refold is not reflected")
	}

	// The oracle: everything live (all state forced stale), the path the write must now follow.
	fx.markStale(t, fx.eligible, true)
	oracle := fx.docs(t)
	requireSameDocs(t, "after the write vs a fresh live computation", oracle, afterWrite)

	fx.materialize(t, fx.eligible)
	requireSameDocs(t, "after re-materializing vs live", oracle, fx.docs(t))
	if n := fx.stateRows(t, fx.eligible, true); n != int64(len(fx.eligible)) {
		t.Fatalf("%d fresh runs after re-materializing, want %d", n, len(fx.eligible))
	}
}

// TestAnalyticsSpendFoldsRealRefoldLiveDB: the real workersvc.RefoldRunUsage replaces a stored,
// fresh run's distinguishable OLD usage with the usage re-folded from its result-frame history.
// The refold marks the run stale, the document follows the refolded usage (equal to an all-live
// oracle), and re-materializing keeps it equal.
func TestAnalyticsSpendFoldsRealRefoldLiveDB(t *testing.T) {
	fx := seedFoldSpend(t, 2011)
	fx.materialize(t, fx.eligible)
	if n := fx.stateRows(t, []uuid.UUID{fx.fresh}, true); n != 1 {
		t.Fatalf("the run to refold is not fresh after materializing (fresh rows = %d, want 1)", n)
	}
	// A pre-migration run: not yet refolded, with a result-frame history whose re-fold differs
	// from the old (collapsed) rows it carries.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET usage_refolded = false WHERE id = $1`, fx.fresh)
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_messages (run_id, seq, kind, agent, payload)
		VALUES ($1, 1, 'status', 'lead', $2::jsonb)`, fx.fresh,
		`{"event":"result","modelUsage":{"model-refolded":{"inputTokens":7777,"outputTokens":3333,"cacheReadInputTokens":11,"cacheCreationInputTokens":3,"costUSD":4.25}}}`)
	before := fx.docs(t)

	run, err := store.New(fx.pool).GetRunByID(fx.ctx, fx.fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := workersvc.RefoldRunUsage(fx.ctx, fx.pool, store.New(fx.pool), run); err != nil {
		t.Fatalf("RefoldRunUsage: %v", err)
	}
	if n := fx.stateRows(t, []uuid.UUID{fx.fresh}, true); n != 0 {
		t.Fatal("the refolded run is still marked fresh")
	}
	if n := fx.count(t, `SELECT count(*) FROM run_usage WHERE run_id = $1 AND model = 'model-refolded'`, fx.fresh); n != 1 {
		t.Fatalf("%d refolded run_usage rows, want 1", n)
	}
	afterRefold := fx.docs(t)
	const probe = "30d/usage_model+primary_model"
	if bytes.Equal(before[probe], afterRefold[probe]) {
		t.Fatal("the refold did not change the document: the test is vacuous")
	}

	fx.markStale(t, fx.eligible, true)
	oracle := fx.docs(t)
	requireSameDocs(t, "after the real refold vs a fresh live computation", oracle, afterRefold)

	fx.materialize(t, fx.eligible)
	requireSameDocs(t, "after re-materializing vs live", oracle, fx.docs(t))
}

// TestAnalyticsSpendFoldsFallbackLiveDB: a run with no state row, a stale run and a
// non-terminal run are read live whatever the stored rows say, and a fresh terminal run's
// stored rows are read (the control).
func TestAnalyticsSpendFoldsFallbackLiveDB(t *testing.T) {
	fx := seedFoldSpend(t, 2008)
	live := fx.docs(t)
	base := fx.outputTokens(t)

	// Missing lane: no state row. Every other eligible run is stored; equality is only possible
	// if the missing run is read live.
	fx.materialize(t, fx.eligibleExcept(fx.missing))
	if fx.stateRows(t, []uuid.UUID{fx.missing}, false) != 0 {
		t.Fatal("the missing-lane run has a state row")
	}
	requireSameDocs(t, "missing lane", live, fx.docs(t))

	// Stale lane: stale = true with deliberately corrupted fold rows is ignored.
	fx.markStale(t, []uuid.UUID{fx.stale}, true)
	fx.corrupt(t, fx.stale)
	requireSameDocs(t, "stale run with corrupted folds", live, fx.docs(t))

	// Control: the same corruption on a FRESH run is read, so the stored path is in use.
	delta := fx.corrupt(t, fx.fresh)
	if got := fx.outputTokens(t); got != base+delta {
		t.Fatalf("a fresh run's corrupted fold moved output_tokens to %d, want %d + %d: the stored path is not read", got, base, delta)
	}
	fx.materialize(t, []uuid.UUID{fx.fresh})
	requireSameDocs(t, "after restoring the control run", live, fx.docs(t))

	// A running run with a fresh state row and corrupted fold rows stays live.
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_usage_fold_state (run_id, stale, folded_at) VALUES ($1, false, now())`, fx.active)
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO run_usage_model_folds (run_id, model, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
		 VALUES ($1, 'model-a', 1, 1, 1, 999999, 123, 'metered')`, fx.active)
	requireSameDocs(t, "running run with a fresh corrupted fold", live, fx.docs(t))
}

// TestAnalyticsSpendFoldsRawInsertNoStateLiveDB: runs written by raw INSERT INTO run_usage
// (no write-path call, so no state row) are read live and correctly, and stay so once folded.
func TestAnalyticsSpendFoldsRawInsertNoStateLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("rawfold")
	repo, _ := f.repo(o, "rawfold")
	a := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 9), Finished: spendDay(3, 10)})
	f.metered(a, "model-a", "1.500000")
	b := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(4, 9), Status: "failed", FailOrigin: "agent_failure", Finished: spendDay(4, 10)})
	f.usage(b, "model-b", "subscription", "0", 7, 3, 1, 5)
	c := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(5, 9), Finished: spendDay(5, 10)})
	ids := []uuid.UUID{a, b, c}
	var n int64
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_usage_fold_state WHERE run_id = ANY($1)`, ids).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw-insert runs have %d state rows (err %v), want none", n, err)
	}

	call := spendCall{Now: spendNow, Repo: repo, Split: "usage_model", Breakdown: "kind"}
	res := f.call(call)
	tot := res.Doc.Totals.Current
	wantDollars(t, "totals cost", tot.Cost, "1.500000")
	if tot.Runs != 3 || tot.InputTokens != 107 || tot.CacheReadTokens != 53 || tot.CacheCreationTokens != 11 || tot.OutputTokens != 25 {
		t.Errorf("totals %+v, want 3 runs, tokens 107/53/11/25", tot)
	}
	if d := tot.Disclosure; d.Metered != 1 || d.Subscription != 1 || d.Missing != 1 || d.Unreported != 0 {
		t.Errorf("disclosure %+v, want one metered, one subscription, one missing", d)
	}

	got, err := store.MaterializeRunUsageFolds(f.ctx, f.pool, ids)
	if err != nil || got != 3 {
		t.Fatalf("materialize: %d, %v", got, err)
	}
	if after := f.call(call); !bytes.Equal(res.Raw, after.Raw) {
		t.Errorf("folding the raw-insert runs changed the document:\n before %s\n after  %s", res.Raw, after.Raw)
	}
}

// TestAnalyticsSpendFoldsOwnerUnchangedLiveDB: the owner statement never reads folds, so its
// documents are byte-equal before and after materialization, and with every stored fold
// corrupted, while the all-scope document does see the corruption.
func TestAnalyticsSpendFoldsOwnerUnchangedLiveDB(t *testing.T) {
	fx := seedFoldSpend(t, 2009)
	ownerLive := fx.ownerDocs(t)
	if fx.call(spendCall{Owner: fx.owners[0].User, Now: fx.now, Window: analytics.Window30d}).Doc.Totals.Current.Runs == 0 {
		t.Fatal("the owner has no runs in the window; the comparison is vacuous")
	}
	allLive := fx.docs(t)

	fx.materialize(t, fx.eligible)
	requireSameDocs(t, "owner documents after materialization", ownerLive, fx.ownerDocs(t))

	tag, err := fx.pool.Exec(fx.ctx,
		`UPDATE run_usage_model_folds SET output_tokens = output_tokens + 777, cost_usd = cost_usd + 1 WHERE run_id = ANY($1)`, fx.eligible)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("corrupting every fold: %d rows, %v", tag.RowsAffected(), err)
	}
	requireSameDocs(t, "owner documents with every fold corrupted", ownerLive, fx.ownerDocs(t))
	const probe = "30d/usage_model+primary_model"
	if bytes.Equal(allLive[probe], fx.call(spendCall{Now: fx.now, Window: analytics.Window30d, Split: "usage_model", Breakdown: "primary_model"}).Raw) {
		t.Fatal("the all-scope document ignored the corrupted folds: the control proves nothing")
	}
}

// TestAnalyticsSpendFoldsGenericPlanLiveDB: under force_generic_plan, window=all over a mix of
// fresh-folded and fallback runs reads the stored fold rows, and the live lateral over
// run_usage_model_totals executes only for the fallback cohort.
func TestAnalyticsSpendFoldsGenericPlanLiveDB(t *testing.T) {
	fx := seedFoldSpend(t, 2010)
	fx.materialize(t, fx.eligibleExcept(fx.missing))
	fx.markStale(t, []uuid.UUID{fx.stale}, true)
	mustExec(fx.ctx, t, fx.pool, `ANALYZE runs`)
	mustExec(fx.ctx, t, fx.pool, `ANALYZE run_usage`)
	mustExec(fx.ctx, t, fx.pool, `ANALYZE run_usage_fold_state`)
	mustExec(fx.ctx, t, fx.pool, `ANALYZE run_usage_model_folds`)

	call := spendCall{Now: fx.now, Window: analytics.WindowAll, Kinds: runkind.All(), Split: "usage_model", Breakdown: "primary_model"}
	b := fx.bounds(call)
	const cohort = `SELECT count(*) FROM runs r WHERE r.created_at >= $1 AND r.created_at < $2 AND r.kind <> 'chat'`
	nonChat := fx.count(t, cohort, b.FoldFrom, b.Now)
	fallback := fx.count(t, cohort+` AND NOT (r.status IN ('completed', 'failed', 'cancelled')
	    AND EXISTS (SELECT 1 FROM run_usage_fold_state s WHERE s.run_id = r.id AND NOT s.stale))`, b.FoldFrom, b.Now)
	if fallback < 5 || fallback >= nonChat-20 {
		t.Fatalf("fallback cohort %d of %d non-chat runs: the fixture does not mix fresh and fallback runs", fallback, nonChat)
	}

	capture := &capturingDBTX{}
	_, _ = store.New(capture).AnalyticsAllSpend(fx.ctx, store.AnalyticsAllSpendParams{
		Now: spendTS(b.Now), From: spendTS(b.From), FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		Kinds: call.Kinds, DeliveryKinds: runkind.Delivery(), Split: call.Split, Breakdown: call.Breakdown,
	})
	if len(capture.args) == 0 {
		t.Fatal("no statement arguments captured")
	}
	var args string
	for i, a := range capture.args {
		if i > 0 {
			args += ", "
		}
		args += sqlLiteral(t, a)
	}

	conn, err := fx.pool.Acquire(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	simple := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(fx.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	simple(`SET plan_cache_mode = force_generic_plan`)
	defer func() {
		_, _ = conn.Exec(context.Background(), `DEALLOCATE ALL`, pgx.QueryExecModeSimpleProtocol)
		_, _ = conn.Exec(context.Background(), `RESET plan_cache_mode`, pgx.QueryExecModeSimpleProtocol)
	}()
	simple(`PREPARE fold_plan AS ` + store.AnalyticsAllSpendSQL)
	var raw []byte
	if err := conn.QueryRow(fx.ctx, `EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE fold_plan(`+args+`)`, pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN EXECUTE: %v", err)
	}
	var generic int64
	if err := conn.QueryRow(fx.ctx, `SELECT generic_plans FROM pg_prepared_statements WHERE name = 'fold_plan'`, pgx.QueryExecModeSimpleProtocol).Scan(&generic); err != nil || generic < 1 {
		t.Fatalf("generic_plans = %d (%v): the EXPLAIN did not run the generic plan", generic, err)
	}
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("decode EXPLAIN JSON: %v", err)
	}
	root := &doc[0].Plan

	var foldRows float64
	var foldScans, usageScans []*planNode
	var visit func(n *planNode)
	visit = func(n *planNode) {
		switch n.RelationName {
		case "run_usage_model_folds":
			foldScans = append(foldScans, n)
			foldRows += n.ActualRows * n.ActualLoops
		case "run_usage":
			usageScans = append(usageScans, n)
		}
		for i := range n.Plans {
			visit(&n.Plans[i])
		}
	}
	visit(root)
	if len(foldScans) == 0 || foldRows == 0 {
		t.Errorf("no run_usage_model_folds node returned rows: the stored fold is not read\n%s", renderPlan(root, 0))
	}
	if len(usageScans) == 0 {
		t.Fatalf("no run_usage scan: the live lateral is missing from the plan\n%s", renderPlan(root, 0))
	}
	for _, n := range usageScans {
		if int64(n.ActualLoops) != fallback {
			t.Errorf("%s on run_usage ran %.0f times, want %d (the fallback cohort; %d non-chat runs in all): the live fold is not limited to non-fresh runs\n%s",
				n.NodeType, n.ActualLoops, fallback, nonChat, renderPlan(root, 0))
		}
	}
	t.Logf("fallback cohort %d of %d non-chat runs; fold rows read %.0f\n%s", fallback, nonChat, foldRows, renderPlan(root, 0))
}
