package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4577: the run OUTCOME population is every kind except chat, defined once by
// fn_outcome_kind; the usage run counts keep fn_usage_primary_kind (#4209). Every expectation
// here is computed in Go from the literal rule below, never by calling the SQL function.

// wantOutcomeKind is the literal outcome rule: chat is the only excluded kind.
func wantOutcomeKind(kind string) bool { return kind != "chat" }

func TestOutcomeKindFunctionLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	for _, kind := range usagePrimaryRegistryKinds(t) {
		var got bool
		if err := e.pool.QueryRow(e.ctx, `SELECT fn_outcome_kind($1)`, kind).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != wantOutcomeKind(kind) {
			t.Errorf("fn_outcome_kind(%q) = %v, want %v", kind, got, wantOutcomeKind(kind))
		}
	}
	// A kind outside the registry is not chat, so it is an outcome kind (explicit contract).
	var got bool
	if err := e.pool.QueryRow(e.ctx, `SELECT fn_outcome_kind('not_a_kind')`).Scan(&got); err != nil || !got {
		t.Errorf("unknown kind: got %v err %v, want true", got, err)
	}
	// Properties that keep the function inlinable and query-only.
	var volatile, parallel string
	var strict, secdef, noConfig bool
	if err := e.pool.QueryRow(e.ctx, `SELECT provolatile::text, proparallel::text, proisstrict, prosecdef, proconfig IS NULL
	    FROM pg_proc WHERE oid = 'fn_outcome_kind(text)'::regprocedure`).Scan(&volatile, &parallel, &strict, &secdef, &noConfig); err != nil {
		t.Fatal(err)
	}
	if volatile != "i" || parallel != "s" || strict || secdef || !noConfig {
		t.Errorf("fn_outcome_kind pg_proc: volatile=%s parallel=%s strict=%v secdef=%v noConfig=%v", volatile, parallel, strict, secdef, noConfig)
	}
}

type okRun struct {
	u       int
	kind    string
	status  string
	origin  string
	age     string
	patch   bool
	harness string
	id      uuid.UUID
	noUsage bool  // seeded without a run_usage row (factor 0)
	factor  int64 // usage factor seeded for the run; 0 = no run_usage row
}

// okInsideWeek reports whether a seeded age falls inside the seven-day window.
func okInsideWeek(age string) bool {
	switch age {
	case "1 day", "2 days", "5 days", "6 days 23 hours":
		return true
	}
	return false
}

type okExp struct {
	fin, comp, canc, rej, div, fail, nl         int64
	l7fin, l7comp, l7canc, l7rej, l7div, l7fail int64
	l7nl                                        int64
	origins, l7origins                          map[string]int64
}

func (e *okExp) vec() [14]int64 {
	return [14]int64{e.fin, e.comp, e.canc, e.rej, e.div, e.fail, e.nl, e.l7fin, e.l7comp, e.l7canc, e.l7rej, e.l7div, e.l7fail, e.l7nl}
}

func TestOutcomeKindRunOutcomesLiveDB(t *testing.T) {
	f, q, users, cleanup := newPrimaryKindFixture(t)
	defer cleanup()
	ctx := f.ctx
	landable := workersvc.AllHumanLandableFailOrigins()
	isLandable := map[string]bool{}
	for _, o := range landable {
		isLandable[o] = true
	}
	kinds := usagePrimaryRegistryKinds(t)

	var recs []okRun
	var factor int64
	seed := func(r okRun) uuid.UUID {
		t.Helper()
		if !r.noUsage {
			factor++
			r.factor = factor
		}
		r.id = f.seed(pkRun{user: users[r.u], kind: r.kind, status: r.status, origin: r.origin, age: r.age, patch: r.patch, harness: r.harness, factor: r.factor})
		recs = append(recs, r)
		return r.id
	}
	// The fixture's own 30-day completed issue run (user 0, no usage) is an outcome.
	recs = append(recs, okRun{u: 0, kind: "issue", status: "completed", age: "30 days", id: f.first})

	// User 0: every registry kind (chat included) completed, cancelled and failed.
	for i, kind := range kinds {
		h := ""
		if i%3 == 0 {
			h = "codex"
		}
		seed(okRun{u: 0, kind: kind, status: "completed", age: "1 day", harness: h})
		seed(okRun{u: 0, kind: kind, status: "cancelled", age: "1 day"})
		seed(okRun{u: 0, kind: kind, status: "failed", origin: "agent_failure", age: "1 day"})
	}
	// Controls and window edges for user 0. Helper kinds appear in every bucket.
	seed(okRun{u: 0, kind: "issue", status: "failed", origin: "plan_rejected", age: "1 day"})
	seed(okRun{u: 0, kind: "mr_rework", status: "failed", origin: "plan_rejected", age: "1 day"})
	seed(okRun{u: 0, kind: "issue", status: "failed", origin: "base_diverged", age: "1 day"})
	seed(okRun{u: 0, kind: "ci_fix", status: "failed", origin: "base_diverged", age: "1 day"})
	// Recoverable failures with a preserved patch: a primary, a helper inside the window and
	// a helper outside it (lifetime needs_landing only).
	seed(okRun{u: 0, kind: "issue", status: "failed", origin: landable[0], age: "1 day", patch: true})
	seed(okRun{u: 0, kind: "ci_fix", status: "failed", origin: landable[0], age: "1 day", patch: true})
	seed(okRun{u: 0, kind: "mr_rework", status: "failed", origin: landable[0], age: "7 days 1 hour", patch: true})
	// Human-stopped runs: queries bucket by status only and never read stop_kind.
	stoppedCancelled := seed(okRun{u: 0, kind: "issue", status: "cancelled", age: "1 day"})
	stoppedFailed := seed(okRun{u: 0, kind: "judge", status: "failed", age: "1 day"}) // NULL origin -> unknown
	f.exec(`UPDATE runs SET stop_kind='cancelled', stop_reason='human stop' WHERE id = ANY($1)`, []uuid.UUID{stoppedCancelled, stoppedFailed})
	// Window edges.
	seed(okRun{u: 0, kind: "ci_fix", status: "completed", age: "6 days 23 hours"})
	seed(okRun{u: 0, kind: "judge", status: "failed", origin: "agent_failure", age: "7 days 1 hour"})
	seed(okRun{u: 0, kind: "cross_check", status: "completed", age: "7 days 1 hour"})

	// Cross-check children keep their status bucket whatever their check verdict says.
	type child struct {
		status, origin, verdict, class string
	}
	for _, c := range []child{
		{"failed", "agent_failure", "failed", "superseded"},
		{"cancelled", "", "failed", "superseded"},
		{"completed", "", "failed", "approved_not_stored"},
	} {
		lead := seed(okRun{u: 0, kind: "issue", status: "completed", age: "5 days"})
		kid := seed(okRun{u: 0, kind: "cross_check", status: c.status, origin: c.origin, age: "1 day"})
		f.exec(`UPDATE runs SET target_run_id = $2 WHERE id = $1`, kid, lead)
		f.exec(`INSERT INTO cross_checks (lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
		            base_commit, candidate_digest, checker_harness, deadline_at)
		        VALUES ($1, $2, 'plan', 1, 1, 'plan', '[]', 's', repeat('a', 40), $3, 'codex', now() + interval '5 minutes')`, lead, kid, []byte("digest"))
		f.exec(`UPDATE cross_checks SET verdict = $2, reason_class = $3, decided_at = now() WHERE checker_run_id = $1`, kid, c.verdict, c.class)
	}

	// User 1: every kind at window-edge ages.
	for _, kind := range kinds {
		seed(okRun{u: 1, kind: kind, status: "completed", age: "2 days"})
		seed(okRun{u: 1, kind: kind, status: "failed", origin: "agent_failure", age: "7 days 1 hour"})
		seed(okRun{u: 1, kind: kind, status: "cancelled", age: "6 days 23 hours"})
	}
	// A failed helper with no run_usage row: an outcome (finished, failed, fail origin) that
	// contributes nothing to the usage figures.
	seed(okRun{u: 1, kind: "ci_fix", status: "failed", origin: "agent_failure", age: "1 day", noUsage: true})
	// User 2: helper-heavy.
	for _, kind := range []string{"judge", "cross_check", "mr_rework", "ci_fix", "chat"} {
		seed(okRun{u: 2, kind: kind, status: "failed", origin: "agent_failure", age: "1 day"})
		seed(okRun{u: 2, kind: kind, status: "completed", age: "1 day"})
	}

	// The newest failure of user 0 is an mr_rework; later completions of mixed kinds follow it,
	// including a chat completion that must not count.
	newestFail := seed(okRun{u: 0, kind: "mr_rework", status: "failed", origin: "agent_failure", age: "1 day"})
	f.exec(`UPDATE runs SET finished_at = now() + interval '1 hour', status_since = now() + interval '1 hour' WHERE id = $1`, newestFail)
	wantSince := int64(0)
	for _, kind := range []string{"issue", "judge", "ci_fix", "chat"} {
		id := seed(okRun{u: 0, kind: kind, status: "completed", age: "1 day"})
		f.exec(`UPDATE runs SET finished_at = now() + interval '2 hours', status_since = now() + interval '2 hours' WHERE id = $1`, id)
		if wantOutcomeKind(kind) {
			wantSince++
		}
	}

	// Expectations, from the recorded rows and the literal rule.
	want := map[int]*okExp{}
	for u := range users {
		want[u] = &okExp{origins: map[string]int64{}, l7origins: map[string]int64{}}
	}
	for _, r := range recs {
		if !wantOutcomeKind(r.kind) {
			continue
		}
		e, in := want[r.u], okInsideWeek(r.age)
		e.fin++
		if in {
			e.l7fin++
		}
		switch {
		case r.status == "completed":
			e.comp++
			if in {
				e.l7comp++
			}
		case r.status == "cancelled":
			e.canc++
			if in {
				e.l7canc++
			}
		case r.origin == "plan_rejected":
			e.rej++
			if in {
				e.l7rej++
			}
		case r.origin == "base_diverged":
			e.div++
			if in {
				e.l7div++
			}
		default:
			o := r.origin
			if o == "" {
				o = "unknown"
			}
			e.fail++
			e.origins[o]++
			if in {
				e.l7fail++
				e.l7origins[o]++
			}
			if isLandable[r.origin] && r.patch {
				e.nl++
				if in {
					e.l7nl++
				}
			}
		}
	}
	if want[0].nl != 3 || want[0].l7nl != 2 {
		t.Fatalf("fixture bug: user 0 needs_landing %d/%d, want 3/2", want[0].nl, want[0].l7nl)
	}

	// SelfRunOutcomes per user.
	for u, id := range users {
		r, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: id, LandableOrigins: landable})
		if err != nil {
			t.Fatal(err)
		}
		e := want[u]
		got := [14]int64{r.LifetimeFinished, r.LifetimeCompleted, r.LifetimeCancelled, r.LifetimePlanRejected, r.LifetimeBaseDiverged, r.LifetimeFailed, r.LifetimeNeedsLanding,
			r.Last7Finished, r.Last7Completed, r.Last7Cancelled, r.Last7PlanRejected, r.Last7BaseDiverged, r.Last7Failed, r.Last7NeedsLanding}
		if got != e.vec() {
			t.Errorf("SelfRunOutcomes user %d: got %v want %v", u, got, e.vec())
		}
		if r.LifetimeFinished != r.LifetimeCompleted+r.LifetimeCancelled+r.LifetimePlanRejected+r.LifetimeBaseDiverged+r.LifetimeFailed {
			t.Errorf("user %d: finished != segments", u)
		}
		assertOrigins(t, fmt.Sprintf("self %d lifetime", u), r.LifetimeFailOrigins, e.origins, r.LifetimeFailed)
		assertOrigins(t, fmt.Sprintf("self %d last7", u), r.Last7FailOrigins, e.l7origins, r.Last7Failed)
		if u == 0 {
			if r.LastFailedRunID != newestFail || r.LastFailedOrigin != "agent_failure" || !r.LastFailedAt.Valid || r.CompletedSinceLastFailure != wantSince {
				t.Errorf("self user 0 recency: run %v origin %q since %d, want %v agent_failure %d", r.LastFailedRunID, r.LastFailedOrigin, r.CompletedSinceLastFailure, newestFail, wantSince)
			}
		} else if !r.LastFailedAt.Valid || r.LastFailedOrigin != "agent_failure" || r.CompletedSinceLastFailure != 0 {
			t.Errorf("self user %d recency: %+v", u, r)
		}
	}

	// Factory and per-user.
	var sum okExp
	sum.origins, sum.l7origins = map[string]int64{}, map[string]int64{}
	for _, e := range want {
		v := e.vec()
		s := sum.vec()
		for i := range v {
			s[i] += v[i]
		}
		sum.fin, sum.comp, sum.canc, sum.rej, sum.div, sum.fail, sum.nl = s[0], s[1], s[2], s[3], s[4], s[5], s[6]
		sum.l7fin, sum.l7comp, sum.l7canc, sum.l7rej, sum.l7div, sum.l7fail, sum.l7nl = s[7], s[8], s[9], s[10], s[11], s[12], s[13]
		sum.origins = mergeCounts(sum.origins, e.origins)
		sum.l7origins = mergeCounts(sum.l7origins, e.l7origins)
	}
	fr, err := q.AdminRunOutcomes(ctx, landable)
	if err != nil {
		t.Fatal(err)
	}
	got := [14]int64{fr.LifetimeFinished, fr.LifetimeCompleted, fr.LifetimeCancelled, fr.LifetimePlanRejected, fr.LifetimeBaseDiverged, fr.LifetimeFailed, fr.LifetimeNeedsLanding,
		fr.Last7Finished, fr.Last7Completed, fr.Last7Cancelled, fr.Last7PlanRejected, fr.Last7BaseDiverged, fr.Last7Failed, fr.Last7NeedsLanding}
	if got != sum.vec() {
		t.Errorf("AdminRunOutcomes: got %v want %v", got, sum.vec())
	}
	assertOrigins(t, "factory lifetime", fr.LifetimeFailOrigins, sum.origins, fr.LifetimeFailed)
	assertOrigins(t, "factory last7", fr.Last7FailOrigins, sum.l7origins, fr.Last7Failed)
	if fr.LastFailedRunID != newestFail || fr.LastFailedUserID != users[0] || fr.LastFailedOrigin != "agent_failure" || fr.CompletedSinceLastFailure != wantSince {
		t.Errorf("factory recency: run %v user %v origin %q since %d, want %v %v agent_failure %d", fr.LastFailedRunID, fr.LastFailedUserID, fr.LastFailedOrigin, fr.CompletedSinceLastFailure, newestFail, users[0], wantSince)
	}

	per, err := q.AdminRunOutcomesPerUser(ctx, landable)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range per {
		for u, id := range users {
			if id != r.UserID {
				continue
			}
			seen++
			e := want[u]
			got := [14]int64{r.Finished, r.Completed, r.Cancelled, r.PlanRejected, r.LifetimeBaseDiverged, r.Failed, r.NeedsLanding,
				r.Last7Finished, r.Last7Completed, r.Last7Cancelled, r.Last7PlanRejected, r.Last7BaseDiverged, r.Last7Failed, r.Last7NeedsLanding}
			if got != e.vec() {
				t.Errorf("AdminRunOutcomesPerUser user %d: got %v want %v", u, got, e.vec())
			}
			assertOrigins(t, fmt.Sprintf("per-user %d lifetime", u), r.FailOrigins, e.origins, r.Failed)
			assertOrigins(t, fmt.Sprintf("per-user %d last7", u), r.Last7FailOrigins, e.l7origins, r.Last7Failed)
			if u == 0 && (r.LastFailedRunID != newestFail || r.LastFailedOrigin != "agent_failure" || r.CompletedSinceLastFailure != wantSince) {
				t.Errorf("per-user 0 recency: run %v origin %q since %d, want %v agent_failure %d", r.LastFailedRunID, r.LastFailedOrigin, r.CompletedSinceLastFailure, newestFail, wantSince)
			}
		}
	}
	if seen != len(users) {
		t.Errorf("per-user outcome rows for the fixture users = %d, want %d", seen, len(users))
	}

	// Usage figures for the same rows are unchanged from before #4577: run counts and harness
	// splits are primary-only, token and cost sums cover every kind except chat.
	type uf struct {
		run, l7run, claude, codex, l7claude, l7codex int64
		in, l7in, cost, l7cost                       int64
	}
	// seed writes cache_read = 2x, cache_creation = 3x and output = 4x the input tokens, so the
	// expected lifetime then last7 cache-read/cache-creation/output sums follow from the Go-side
	// input sums.
	otherTokens := func(a *uf) [6]int64 {
		return [6]int64{2 * a.in, 3 * a.in, 4 * a.in, 2 * a.l7in, 3 * a.l7in, 4 * a.l7in}
	}
	uwant := map[int]*uf{0: {}, 1: {}, 2: {}}
	var fac uf
	for _, r := range recs {
		if r.factor == 0 || r.kind == "chat" { // no usage row (fixture issue, seeded helper); chat usage is excluded
			continue
		}
		in := r.factor
		h := r.harness
		if h == "" {
			h = "claude"
		}
		if r.kind == "cross_check" {
			h = "codex"
		}
		in7 := okInsideWeek(r.age)
		for _, a := range []*uf{uwant[r.u], &fac} {
			a.in += in
			a.cost += in * 5
			if in7 {
				a.l7in += in
				a.l7cost += in * 5
			}
			if !wantPrimaryKinds[r.kind] {
				continue
			}
			a.run++
			if h == "claude" {
				a.claude++
			} else {
				a.codex++
			}
			if in7 {
				a.l7run++
				if h == "claude" {
					a.l7claude++
				} else {
					a.l7codex++
				}
			}
		}
	}
	for u, id := range users {
		s, err := q.SelfUsage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		w := uwant[u]
		if s.RunCount != w.run || s.LifetimeInputTokens != w.in || s.Last7InputTokens != w.l7in || microsOf(t, s.LifetimeCostUsd) != w.cost || microsOf(t, s.Last7CostUsd) != w.l7cost {
			t.Errorf("SelfUsage user %d: run %d in %d/%d cost %d/%d, want %+v", u, s.RunCount, s.LifetimeInputTokens, s.Last7InputTokens, microsOf(t, s.LifetimeCostUsd), microsOf(t, s.Last7CostUsd), *w)
		}
		got := [6]int64{s.LifetimeCacheReadTokens, s.LifetimeCacheCreationTokens, s.LifetimeOutputTokens, s.Last7CacheReadTokens, s.Last7CacheCreationTokens, s.Last7OutputTokens}
		if exp := otherTokens(w); got != exp {
			t.Errorf("SelfUsage user %d cache-read/cache-creation/output tokens: got %v want %v", u, got, exp)
		}
	}
	tot, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tot.RunCount != fac.run || tot.LifetimeInputTokens != fac.in || tot.Last7InputTokens != fac.l7in || microsOf(t, tot.LifetimeCostUsd) != fac.cost || microsOf(t, tot.Last7CostUsd) != fac.l7cost {
		t.Errorf("AdminUsageTotals: run %d in %d/%d, want %+v", tot.RunCount, tot.LifetimeInputTokens, tot.Last7InputTokens, fac)
	}
	if got, exp := [6]int64{tot.LifetimeCacheReadTokens, tot.LifetimeCacheCreationTokens, tot.LifetimeOutputTokens, tot.Last7CacheReadTokens, tot.Last7CacheCreationTokens, tot.Last7OutputTokens}, otherTokens(&fac); got != exp {
		t.Errorf("AdminUsageTotals cache-read/cache-creation/output tokens: got %v want %v", got, exp)
	}
	pu, err := q.AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	usageSeen := 0
	for _, r := range pu {
		for u, id := range users {
			if id != r.UserID {
				continue
			}
			usageSeen++
			w := uwant[u]
			got := [10]int64{r.RunCount, r.Last7RunCount, r.ClaudeRunCount, r.CodexRunCount, r.Last7ClaudeRunCount, r.Last7CodexRunCount, r.InputTokens, r.Last7InputTokens, microsOf(t, r.CostUsd), microsOf(t, r.Last7CostUsd)}
			exp := [10]int64{w.run, w.l7run, w.claude, w.codex, w.l7claude, w.l7codex, w.in, w.l7in, w.cost, w.l7cost}
			if got != exp {
				t.Errorf("AdminUsagePerUser user %d: got %v want %v", u, got, exp)
			}
			if got, exp := [6]int64{r.CacheReadTokens, r.CacheCreationTokens, r.OutputTokens, r.Last7CacheReadTokens, r.Last7CacheCreationTokens, r.Last7OutputTokens}, otherTokens(w); got != exp {
				t.Errorf("AdminUsagePerUser user %d cache-read/cache-creation/output tokens: got %v want %v", u, got, exp)
			}
		}
	}
	if usageSeen != len(users) {
		t.Errorf("per-user usage rows for the fixture users = %d, want %d", usageSeen, len(users))
	}
}
