package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4730: LiveDB tests of the Quality statements. They call the generated store methods
// directly with Go-computed bounds and seed their own users, so the shared database does not
// couple tests. Owner-statement tests use the Spend fixture's 30d calendar ending 2026-11-25
// 12:00 UTC; tests of the all-scope statement claim a private year.

var qualNow = spendNow

func TestQualityReworkGroupsLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("rw")
	repo, _ := f.repo(o, "rw")
	call := func() store.AnalyticsQualityRework {
		return f.quality(qualityCall{Owner: o.User, Now: qualNow}).Doc.Rework
	}

	// PR 101: three cohort runs on one PR collapse to one PR; no rework yet, so zero.
	seed101 := f.pr(o, repo, 101, "", qualDay(3, 9))
	f.pr(o, repo, 101, "", qualDay(4, 9))
	f.pr(o, repo, 101, "merged", qualDay(5, 9))
	r := call()
	wantEq(t, "applicable_prs", r.ApplicablePRs, 1)
	wantEq(t, "zero", r.Groups.Zero, 1)
	wantEq(t, "unverified_links", r.UnverifiedLinks, 0)
	// The earliest matching cohort run (day 3) places the PR.
	wantEq(t, "bucket 2 prs", r.Buckets[2].PRs, 1)
	wantEq(t, "bucket 4 prs", r.Buckets[4].PRs, 0)
	wantEq(t, "bucket count", len(r.Buckets), 30)

	// One verified rework moves it to one.
	f.rework(o, repo, 101, seed101, "mr_rework", qualDay(6, 9))
	r = call()
	wantEq(t, "one", r.Groups.One, 1)
	wantEq(t, "zero after rework", r.Groups.Zero, 0)

	// The PRD example: one manual and one automatic rework created before @from appear once,
	// in the two-rework group.
	seed102 := f.pr(o, repo, 102, "merged", qualDay(10, 9))
	f.rework(o, repo, 102, seed102, "manual", qualDay(11, 9))
	f.rework(o, repo, 102, seed102, "mr_rework", qualDay(1, 0).AddDate(0, 0, -26))
	r = call()
	wantEq(t, "applicable_prs", r.ApplicablePRs, 2)
	wantEq(t, "two", r.Groups.Two, 1)
	wantEq(t, "one", r.Groups.One, 1)
	wantEq(t, "three_plus", r.Groups.ThreePlus, 0)

	// A third rework reaches three_plus.
	f.rework(o, repo, 102, seed102, "manual", qualDay(12, 9))
	r = call()
	wantEq(t, "two after third", r.Groups.Two, 0)
	wantEq(t, "three_plus", r.Groups.ThreePlus, 1)

	// A rework whose target carries another MR, and one whose target has no MR, are
	// unverified: counted as links, in no group.
	f.pr(o, repo, 104, "merged", qualDay(13, 9))
	otherMR := f.run(spendRun{Owner: o.User, Repo: repo, MR: 9999, At: qualDay(13, 8)})
	f.rework(o, repo, 104, otherMR, "manual", qualDay(14, 9))
	f.pr(o, repo, 105, "merged", qualDay(15, 9))
	noMR := f.run(spendRun{Owner: o.User, Repo: repo, At: qualDay(15, 8)})
	f.rework(o, repo, 105, noMR, "manual", qualDay(16, 9))
	r = call()
	wantEq(t, "unverified_links", r.UnverifiedLinks, 2)
	wantEq(t, "zero (unverified PRs)", r.Groups.Zero, 2)

	// Deleting the target cascades the rework away: PR 106 goes one -> zero.
	f.pr(o, repo, 106, "merged", qualDay(17, 9))
	tgt := f.run(spendRun{Owner: o.User, Repo: repo, MR: 106, At: qualDay(1, 0).AddDate(0, 0, -5)})
	f.rework(o, repo, 106, tgt, "manual", qualDay(18, 9))
	r = call()
	wantEq(t, "one with 106", r.Groups.One, 2)
	mustExec(f.ctx, t, f.pool, `DELETE FROM runs WHERE id = $1`, tgt)
	r = call()
	wantEq(t, "one after target delete", r.Groups.One, 1)
	wantEq(t, "zero after target delete", r.Groups.Zero, 3)

	// A cohort PR with no merge witness is not applicable. The two helper targets above
	// (MR 9999 and no MR) are cohort runs too: 9999 is an unwitnessed PR.
	f.pr(o, repo, 107, "open", qualDay(19, 9))
	r = call()
	wantEq(t, "unwitnessed_prs", r.UnwitnessedPRs, 2)
	wantEq(t, "applicable_prs final", r.ApplicablePRs, 5)
	sum := r.Groups.Zero + r.Groups.One + r.Groups.Two + r.Groups.ThreePlus
	wantEq(t, "groups sum", sum, r.ApplicablePRs)
	var bucketPRs int64
	for _, b := range r.Buckets {
		bucketPRs += b.PRs
	}
	wantEq(t, "bucket sum", bucketPRs, r.ApplicablePRs)
}

func TestQualityReworkForeignReworkLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	f.claimYear(2012)
	u, v := f.owner("fr-u"), f.owner("fr-v")
	repo, _ := f.repo(u, "fr")
	seed := f.pr(u, repo, 41, "merged", qualAllDay(5, 9))
	f.rework(u, repo, 41, seed, "manual", qualAllDay(6, 9))
	// V's rework on the same (repo_id, mr_iid) with a verified V-owned target.
	vTarget := f.run(spendRun{Owner: v.User, Repo: repo, MR: 41, At: qualAllDay(4, 9)})
	f.rework(v, repo, 41, vTarget, "manual", qualAllDay(7, 9))

	owner := f.quality(qualityCall{Owner: u.User, Now: qualAllNow}).Doc.Rework
	wantEq(t, "owner applicable", owner.ApplicablePRs, 1)
	wantEq(t, "owner one", owner.Groups.One, 1)
	wantEq(t, "owner two", owner.Groups.Two, 0)
	all := f.quality(qualityCall{Now: qualAllNow}).Doc.Rework
	wantEq(t, "all applicable", all.ApplicablePRs, 1)
	wantEq(t, "all two", all.Groups.Two, 1)
	wantEq(t, "all unverified", all.UnverifiedLinks, 0)
}

func TestQualityReworkWitnessScopeLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	f.claimYear(2012)
	u, v := f.owner("ws-u"), f.owner("ws-v")
	repo, _ := f.repo(u, "ws")
	// PR 31: cohort run in window; the same user's witness is another kind, another harness
	// and created before the window.
	f.pr(u, repo, 31, "", qualAllDay(5, 9))
	f.run(spendRun{Owner: u.User, Repo: repo, Kind: runkind.CIFix, Harness: "codex", MR: 31, MRState: "merged",
		At: qualAllDay(1, 0).AddDate(0, 0, -40)})
	// PR 32: the only witness belongs to another user.
	f.pr(u, repo, 32, "", qualAllDay(6, 9))
	f.run(spendRun{Owner: v.User, Repo: repo, MR: 32, MRState: "merged", At: qualAllDay(2, 9)})

	for name, c := range map[string]qualityCall{
		"default":        {Owner: u.User, Now: qualAllNow},
		"harness+kind":   {Owner: u.User, Now: qualAllNow, Harness: "claude", Kinds: []string{runkind.Issue}},
		"repo-filtered":  {Owner: u.User, Now: qualAllNow, Repo: repo},
		"issue-delivery": {Owner: u.User, Now: qualAllNow, DeliveryKinds: []string{runkind.Issue, runkind.CIFix}},
	} {
		res := f.quality(c)
		r := res.Doc.Rework
		if r.ApplicablePRs != 1 || r.UnwitnessedPRs != 1 {
			t.Errorf("%s: owner applicable %d unwitnessed %d, want 1 and 1", name, r.ApplicablePRs, r.UnwitnessedPRs)
		}
		// Totals alone cannot tell which PR was applicable: PR 31 (day 5) is the witnessed
		// one, PR 32 (day 6) the unwitnessed one.
		if len(r.Buckets) != len(res.Bounds.BucketStarts) || len(r.Buckets) < 6 {
			t.Errorf("%s: %d rework buckets, want %d", name, len(r.Buckets), len(res.Bounds.BucketStarts))
			continue
		}
		if r.Buckets[4].PRs != 1 || r.Buckets[5].PRs != 0 {
			t.Errorf("%s: bucket PRs day5=%d day6=%d, want 1 and 0", name, r.Buckets[4].PRs, r.Buckets[5].PRs)
		}
	}
	// The all-scope statement accepts any user's witness.
	all := f.quality(qualityCall{Now: qualAllNow}).Doc.Rework
	if all.ApplicablePRs != 2 || all.UnwitnessedPRs != 0 {
		t.Errorf("all scope: applicable %d unwitnessed %d, want 2 and 0", all.ApplicablePRs, all.UnwitnessedPRs)
	}
	// V's own scope: V's cohort run is its own witness, U's PRs are not V's cohort.
	vDoc := f.quality(qualityCall{Owner: v.User, Now: qualAllNow}).Doc.Rework
	wantEq(t, "V applicable", vDoc.ApplicablePRs, 1)
	wantEq(t, "V unwitnessed", vDoc.UnwitnessedPRs, 0)
}

func TestQualitySharedExternalPRLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	u, v := f.owner("sx-u"), f.owner("sx-v")
	call := func() store.AnalyticsQualityRework {
		return f.quality(qualityCall{Owner: u.User, Now: qualNow}).Doc.Rework
	}
	// Two own connections to the same forge host (case and trailing slash differ) and the
	// same external project each hold the same merge request.
	conn2 := f.connection(u, "gitlab", "https://FORGE.e2e/")
	repoA := f.repoOn(u.Conn, 4242)
	repoB := f.repoOn(conn2, 4242)
	f.pr(u, repoA, 55, "merged", qualDay(5, 9))
	f.pr(u, repoB, 55, "merged", qualDay(6, 9))
	f.pr(u, repoA, 56, "merged", qualDay(7, 9)) // same project, other MR: not shared
	r := call()
	wantEq(t, "applicable", r.ApplicablePRs, 3)
	wantEq(t, "shared_external_prs", r.SharedExternalPRs, 2)
	wantEq(t, "shared_detection_unavailable", r.SharedDetectionUnavailable, 0)

	// A selected PR on another user's connection is outside the detection, even when its
	// external key equals an own PR's.
	repoOwn := f.repoOn(u.Conn, 5151)
	repoForeign := f.repoOn(v.Conn, 5151)
	f.pr(u, repoOwn, 77, "merged", qualDay(8, 9))
	f.pr(u, repoForeign, 77, "merged", qualDay(9, 9))
	r = call()
	wantEq(t, "applicable", r.ApplicablePRs, 5)
	wantEq(t, "shared_external_prs", r.SharedExternalPRs, 2)
	wantEq(t, "shared_detection_unavailable", r.SharedDetectionUnavailable, 1)
}

func TestQualityCIFixLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("cf")
	repo, _ := f.repo(o, "cf")
	mk := func(day, hour int, status, origin, harness string) {
		f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, Status: status, FailOrigin: origin,
			Harness: harness, At: qualDay(day, hour), Finished: qualDay(day, hour+1)})
	}
	mk(3, 9, "completed", "", "claude")
	mk(3, 10, "cancelled", "", "claude")
	mk(5, 9, "failed", "agent_failure", "claude")
	mk(5, 10, "failed", "plan_rejected", "codex")
	mk(7, 9, "failed", "base_diverged", "claude")
	f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, Status: "running", At: qualDay(9, 9)})
	// Outside the window, another kind and another user are all excluded.
	f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: qualDay(1, 0).AddDate(0, 0, -3), Finished: qualDay(1, 0)})
	f.run(spendRun{Owner: o.User, Repo: repo, At: qualDay(4, 9)})
	other := f.owner("cf-other")
	orepo, _ := f.repo(other, "cf-other")
	f.run(spendRun{Owner: other.User, Repo: orepo, Kind: runkind.CIFix, At: qualDay(4, 9)})

	r := f.quality(qualityCall{Owner: o.User, Now: qualNow}).Doc.CIFix
	wantEq(t, "runs", r.Runs, 6)
	wantEq(t, "finished", r.Finished, 5)
	wantEq(t, "in_flight", r.InFlight, 1)
	wantEq(t, "outcomes", r.Outcomes, store.AnalyticsQualityOutcomes{Completed: 1, Cancelled: 1, PlanRejected: 1, BaseDiverged: 1, Failed: 1})
	wantEq(t, "buckets", len(r.Buckets), 30)
	wantEq(t, "bucket 2 runs", r.Buckets[2].Runs, 2)
	wantEq(t, "bucket 2 outcomes", r.Buckets[2].Outcomes, store.AnalyticsQualityOutcomes{Completed: 1, Cancelled: 1})
	wantEq(t, "bucket 8 in_flight", r.Buckets[8].InFlight, 1)
	// An empty bucket is present with zero counts and the right index.
	wantEq(t, "empty bucket", r.Buckets[0], store.AnalyticsQualityCIFixBucket{Index: 0})
	var sum int64
	for i, b := range r.Buckets {
		wantEq(t, "bucket index", b.Index, i)
		sum += b.Runs
	}
	wantEq(t, "bucket sum", sum, r.Runs)

	// Filters apply to the cohort.
	codex := f.quality(qualityCall{Owner: o.User, Now: qualNow, Harness: "codex"}).Doc.CIFix
	wantEq(t, "codex runs", codex.Runs, 1)
	wantEq(t, "codex plan_rejected", codex.Outcomes.PlanRejected, 1)

	// A kind filter excluding ci_fix leaves every count zero.
	none := f.quality(qualityCall{Owner: o.User, Now: qualNow, Kinds: []string{runkind.Issue}}).Doc.CIFix
	wantEq(t, "excluded runs", none.Runs, 0)
	wantEq(t, "excluded finished", none.Finished, 0)
	wantEq(t, "excluded buckets", len(none.Buckets), 30)
}

func TestQualityCrossCheckLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("cc")
	repo, _ := f.repo(o, "cc")
	old := qualDay(1, 0).AddDate(0, 0, -26) // before @from
	at := func(day int) time.Time { return qualDay(day, 9) }
	lead := func(harness string, created time.Time) uuid.UUID { return f.lead(o, repo, harness, created) }
	plan := func(harness string, day int, verdict string, reason any, interrupted bool, checker any) {
		d := qualTS(at(day))
		i := qualNull
		if interrupted {
			i = d
		}
		f.ccPlan(lead(harness, at(day).Add(-2*time.Hour)), verdict, reason, at(day).Add(-time.Hour), d, i, checker)
	}
	code := func(harness string, day int, outcome string, reason, findings any, interrupted bool, checker any) {
		d := qualTS(at(day))
		i := qualNull
		if interrupted {
			i = d
		}
		f.ccCode(lead(harness, at(day).Add(-2*time.Hour)), outcome, reason, findings, at(day).Add(-time.Hour), d, i, checker)
	}

	// Plan attempts decided in the window.
	plan("claude", 5, "approve", "approve", false, "claude")   // passed
	plan("codex", 6, "revise", "revise", false, "claude")      // raised_issues (lead codex, checker claude)
	plan("codex", 7, "block", "block", false, nil)             // raised_issues, checker unknown
	plan("claude", 8, "failed", "timed_out", false, "gemini")  // didnt_finish, unknown checker
	plan("claude", 9, "failed", "superseded", false, "claude") // replaced
	plan("claude", 10, "approve", "approve", true, "codex")    // interrupted at its decision: keeps its verdict
	plan("claude", 11, "failed", nil, true, nil)               // interrupted, no verdict: replaced
	// An old lead and attempt, decided inside the window.
	f.ccPlan(lead("claude", old), "approve", "approve", old, qualTS(at(12)), qualNull, "claude")
	// Decided with a checker child still running: decided, not pending.
	decidedLead := lead("claude", at(13).Add(-2*time.Hour))
	f.checkerRun(o, repo, decidedLead, "codex", at(13).Add(-time.Hour))
	f.ccPlan(decidedLead, "approve", "approve", at(13).Add(-time.Hour), qualTS(at(13)), qualNull, "claude")
	// A code attempt decided while its checker child is still winding down: decided, never pending.
	codeLead := lead("claude", at(15).Add(-2*time.Hour))
	f.ccCode(codeLead, "completed", nil, `[]`, at(15).Add(-time.Hour), qualTS(at(15)), qualNull, "codex")
	f.linkChecker(codeLead, f.checkerRun(o, repo, codeLead, "codex", at(15).Add(-time.Hour)))
	// Decided before the window: counted nowhere.
	f.ccPlan(lead("claude", old), "approve", "approve", old, qualTS(old.Add(time.Hour)), qualNull, "claude")
	// Undated: NULL, infinity, -infinity and backwards decision times.
	f.ccPlan(lead("claude", at(4)), "approve", "approve", at(4), qualNull, qualNull, "claude")
	f.ccPlan(lead("claude", at(4)), "revise", "revise", at(4), qualInf, qualNull, "claude")
	f.ccPlan(lead("claude", at(4)), "block", "block", at(4), qualNegInf, qualNull, "claude")
	f.ccPlan(lead("claude", at(20)), "approve", "approve", at(20), qualTS(at(19)), qualNull, "claude")
	// Undated: created_at is -infinity with a finite decision time inside the window.
	negLead := lead("claude", at(14))
	f.ccPlanTS(negLead, "approve", "approve", qualNegInf, qualTS(at(14)), qualNull, "claude")
	// Undated: created_at is +infinity (malformed) with a finite decision time inside the window.
	f.ccPlanTS(lead("claude", at(14)), "revise", "revise", qualInf, qualTS(at(14)), qualNull, "claude")
	// Undated: created_at far in the future, so the decision precedes it (backwards).
	f.ccPlan(lead("claude", at(14)), "approve", "approve", qualNow.AddDate(0, 0, 365), qualTS(at(14)), qualNull, "claude")
	// Pending plan on an old lead.
	f.ccPlan(lead("claude", old), "pending", nil, old, qualNull, qualNull, "claude")
	// Pending created after the request's Now: live pending has no time predicate.
	f.ccPlan(lead("claude", old), "pending", nil, qualNow.Add(time.Hour), qualNull, qualNull, "claude")

	// Code attempts. The legacy verdict column is 'failed' on every row.
	code("claude", 5, "completed", nil, `[]`, false, "codex")                                               // passed
	code("codex", 6, "completed", nil, `[{"id":"a"}]`, false, "claude")                                     // raised_issues
	code("claude", 7, "completed", nil, `{"x":1}`, false, nil)                                              // unclassified (findings not an array)
	code("claude", 8, "failed", "timed_out", nil, false, "codex")                                           // didnt_finish
	code("claude", 9, "completed", nil, `[]`, true, "codex")                                                // interrupted: replaced
	f.ccCode(lead("claude", old), "completed", nil, `[]`, old, qualTS(at(12)), qualNull, "codex")           // old lead, passed
	f.ccCode(lead("claude", at(4)), "completed", nil, `[]`, at(4), qualNull, qualNull, "codex")             // undated
	f.ccCode(lead("claude", at(20)), "failed", "timed_out", nil, at(20), qualTS(at(19)), qualNull, "codex") // undated
	f.ccCode(lead("claude", old), "pending", nil, `[]`, old, qualNull, qualNull, "codex")                   // pending, old
	f.ccCodeTS(lead("claude", at(14)), "completed", nil, `[]`, qualInf, qualTS(at(14)), qualNull, "codex")  // undated: +infinity created_at
	negCode := lead("claude", at(14))
	f.ccCodeTS(negCode, "completed", nil, `[]`, qualNegInf, qualTS(at(14)), qualNull, "codex") // undated: -infinity created_at

	res := f.quality(qualityCall{Owner: o.User, Now: qualNow})
	cc := res.Doc.CrossCheck

	p := cc.Plan
	wantEq(t, "plan decided", p.DecidedInWindow, 9)
	wantEq(t, "plan stored", p.StoredResult, store.AnalyticsQualityPlanResults{Approve: 4, Revise: 1, Block: 1, Failed: 3})
	wantEq(t, "plan classes", p.Classes, store.AnalyticsQualityClasses{Passed: 4, RaisedIssues: 2, DidntFinish: 1, Replaced: 2})
	wantEq(t, "plan checker harness", p.CheckerHarness, store.AnalyticsQualityCheckerHarness{Claude: 5, Codex: 1, Unknown: 3})
	wantEq(t, "plan undated", p.Undated, store.AnalyticsQualityPlanUndated{Count: 7,
		StoredResult: store.AnalyticsQualityPlanResults{Approve: 4, Revise: 2, Block: 1}})

	c := cc.Code
	wantEq(t, "code decided", c.DecidedInWindow, 7)
	// Stage-specific result field: a completed code attempt with legacy verdict 'failed' is
	// stored as completed.
	wantEq(t, "code stored", c.StoredResult, store.AnalyticsQualityCodeResults{Completed: 6, Failed: 1})
	wantEq(t, "code classes", c.Classes, store.AnalyticsQualityClasses{Passed: 3, RaisedIssues: 1, DidntFinish: 1, Replaced: 1, Unclassified: 1})
	wantEq(t, "code checker harness", c.CheckerHarness, store.AnalyticsQualityCheckerHarness{Claude: 1, Codex: 5, Unknown: 1})
	wantEq(t, "code undated", c.Undated, store.AnalyticsQualityCodeUndated{Count: 4,
		StoredResult: store.AnalyticsQualityCodeResults{Completed: 3, Failed: 1}})

	// The code attempt decided with a running checker child is not pending.
	wantEq(t, "pending", cc.Pending, store.AnalyticsQualityPending{Total: 3, Plan: 2, Code: 1})
	wantEq(t, "code bucket day 15 (running checker)", c.Buckets[14].Decided, 1)

	// Reconciliation: decided in window = stored results = classes = checker harness = buckets.
	for name, s := range map[string]struct {
		decided, stored, classes, harness, buckets int64
	}{
		"plan": {p.DecidedInWindow, p.StoredResult.Approve + p.StoredResult.Revise + p.StoredResult.Block + p.StoredResult.Failed,
			classSum(p.Classes), p.CheckerHarness.Claude + p.CheckerHarness.Codex + p.CheckerHarness.Unknown, planBucketSum(p.Buckets)},
		"code": {c.DecidedInWindow, c.StoredResult.Completed + c.StoredResult.Failed,
			classSum(c.Classes), c.CheckerHarness.Claude + c.CheckerHarness.Codex + c.CheckerHarness.Unknown, codeBucketSum(c.Buckets)},
	} {
		if s.stored != s.decided || s.classes != s.decided || s.harness != s.decided || s.buckets != s.decided {
			t.Errorf("%s does not reconcile: %+v", name, s)
		}
	}
	// Bucketed by decision time, with every bucket present.
	wantEq(t, "plan buckets", len(p.Buckets), 30)
	wantEq(t, "plan bucket day 5", p.Buckets[4].Decided, 1)
	wantEq(t, "plan bucket day 12 (old lead)", p.Buckets[11].Decided, 1)
	wantEq(t, "plan bucket day 5 passed", p.Buckets[4].Classes.Passed, 1)
	wantEq(t, "plan empty bucket", p.Buckets[0], store.AnalyticsQualityPlanBucket{Index: 0})
	wantEq(t, "code bucket day 7 unclassified", c.Buckets[6].Classes.Unclassified, 1)
	wantEq(t, "code bucket day 7 stored", c.Buckets[6].StoredResult.Completed, 1)

	// Lead harness selects attempts; checker harness is separate metadata.
	codexLeads := f.quality(qualityCall{Owner: o.User, Now: qualNow, Harness: "codex"}).Doc.CrossCheck
	wantEq(t, "codex-lead plan decided", codexLeads.Plan.DecidedInWindow, 2)
	wantEq(t, "codex-lead plan checker claude", codexLeads.Plan.CheckerHarness.Claude, 1)
	wantEq(t, "codex-lead plan checker unknown", codexLeads.Plan.CheckerHarness.Unknown, 1)
	wantEq(t, "codex-lead code decided", codexLeads.Code.DecidedInWindow, 1)
	wantEq(t, "codex-lead pending", codexLeads.Pending.Total, 0)

	// A lead kind filter excluding the leads zeroes everything, pending included.
	none := f.quality(qualityCall{Owner: o.User, Now: qualNow, Kinds: []string{runkind.CIFix}}).Doc.CrossCheck
	wantEq(t, "excluded plan decided", none.Plan.DecidedInWindow, 0)
	wantEq(t, "excluded plan undated", none.Plan.Undated.Count, 0)
	wantEq(t, "excluded pending", none.Pending.Total, 0)
}

// The cross-check population follows the lead kinds whose stage can run a cross-check: a plan
// attempt on a task lead (no plan gate) is not counted, a code attempt on it is.
func TestQualityCrossCheckApplicablePopulationLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("cc-pop")
	repo, _ := f.repo(o, "cc-pop")
	at := qualDay(5, 9)
	taskLead := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.Task, Harness: "claude", Status: "running", At: at.Add(-2 * time.Hour)})
	f.ccPlan(taskLead, "approve", "approve", at.Add(-time.Hour), qualTS(at), qualNull, "claude")
	pendingLead := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.Task, Harness: "claude", Status: "running", At: at.Add(-2 * time.Hour)})
	f.ccPlan(pendingLead, "pending", nil, at.Add(-time.Hour), qualNull, qualNull, "claude")
	f.ccCode(taskLead, "completed", nil, `[]`, at.Add(-time.Hour), qualTS(at), qualNull, "codex")
	issueLead := f.lead(o, repo, "claude", at.Add(-2*time.Hour))
	f.ccPlan(issueLead, "approve", "approve", at.Add(-time.Hour), qualTS(at), qualNull, "claude")

	cc := f.quality(qualityCall{Owner: o.User, Now: qualNow, Kinds: []string{runkind.Task}}).Doc.CrossCheck
	wantEq(t, "task-only plan decided", cc.Plan.DecidedInWindow, 0)
	wantEq(t, "task-only plan undated", cc.Plan.Undated.Count, 0)
	wantEq(t, "task-only code decided", cc.Code.DecidedInWindow, 1)
	wantEq(t, "task-only pending", cc.Pending, store.AnalyticsQualityPending{})

	both := f.quality(qualityCall{Owner: o.User, Now: qualNow}).Doc.CrossCheck
	wantEq(t, "default plan decided", both.Plan.DecidedInWindow, 1)
	wantEq(t, "default code decided", both.Code.DecidedInWindow, 1)
}

func classSum(c store.AnalyticsQualityClasses) int64 {
	return c.Passed + c.RaisedIssues + c.DidntFinish + c.Replaced + c.Unclassified
}

func planBucketSum(bs []store.AnalyticsQualityPlanBucket) (n int64) {
	for _, b := range bs {
		n += b.Decided
	}
	return n
}

func codeBucketSum(bs []store.AnalyticsQualityCodeBucket) (n int64) {
	for _, b := range bs {
		n += b.Decided
	}
	return n
}

func TestQualityJudgeLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("jd")
	v := f.owner("jd-v")
	repo, _ := f.repo(o, "jd")
	target := func(day int, status, origin string, iterations int, kind string) uuid.UUID {
		id := f.run(spendRun{Owner: o.User, Repo: repo, Kind: kind, Status: status, FailOrigin: origin, At: qualDay(day, 9),
			Finished: qualDay(day, 10)})
		if iterations != 0 {
			f.set(id, "iteration_count = $2", iterations)
		}
		return id
	}
	reviewedAt := qualDay(30, 0)
	j1 := target(3, "completed", "", 0, runkind.Issue)
	f.review(j1, o.User, "complete", "ideal", reviewedAt)
	j2 := target(4, "completed", "", 0, runkind.Issue)
	f.review(j2, o.User, "complete", "ok", reviewedAt)
	j3 := target(5, "completed", "", 0, runkind.Issue)
	f.review(j3, o.User, "complete", "issues", reviewedAt)
	j4 := target(6, "completed", "", 0, runkind.Issue)
	f.judgeSkip(j4)
	target(7, "completed", "", 0, runkind.Issue)
	j6 := target(8, "failed", "agent_failure", 1, runkind.Issue)
	f.review(j6, o.User, "failed", "ideal", reviewedAt) // a failed review keeps its fallback verdict
	target(9, "failed", "push_secret_blocked", 3, runkind.Issue)
	target(10, "failed", "credential_unavailable", 3, runkind.Issue) // resumed run: still eligible
	j9 := target(11, "failed", "credential_unavailable", 0, runkind.Issue)
	f.review(j9, o.User, "complete", "ok", reviewedAt) // excluded, yet reviewed
	target(12, "failed", "", 2, runkind.Issue)         // NULL origin stays eligible
	target(13, "cancelled", "", 0, runkind.Issue)
	target(14, "running", "", 0, runkind.Issue)
	j13 := target(15, "completed", "", 0, runkind.Issue)
	f.review(j13, v.User, "complete", "ideal", reviewedAt) // another user's review is not this scope's
	target(16, "completed", "", 0, runkind.CIFix)
	target(17, "completed", "", 0, runkind.Task) // not a judge-eligible kind
	old := f.run(spendRun{Owner: o.User, Repo: repo, At: qualDay(1, 0).AddDate(0, 0, -5), Finished: qualDay(1, 0)})
	f.review(old, o.User, "complete", "ok", reviewedAt) // before the window

	call := func() store.AnalyticsQualityJudge {
		return f.quality(qualityCall{Owner: o.User, Now: qualNow}).Doc.Judge
	}
	j := call()
	wantEq(t, "candidates", j.Candidates, store.AnalyticsQualityJudgeCandidates{Total: 12, Completed: 7, Failed: 5})
	wantEq(t, "eligible total", j.Eligible.Total, 10)
	wantEq(t, "unreviewed", j.Eligible.Unreviewed, 6)
	wantEq(t, "unreviewed_skipped", j.Eligible.UnreviewedSkipped, 1)
	wantEq(t, "failed reviews", j.Eligible.Failed, 1)
	wantEq(t, "complete", j.Eligible.Complete, store.AnalyticsQualityJudgeComplete{Total: 3, Ideal: 1, OK: 1, Issues: 1})
	wantEq(t, "excluded", j.Excluded, store.AnalyticsQualityJudgeExcluded{Cancelled: 1, InFlight: 1, SkipOrigin: 2, ReviewedExcluded: 1})
	wantEq(t, "eligible identity", j.Eligible.Unreviewed+j.Eligible.Failed+j.Eligible.Complete.Total, j.Eligible.Total)
	var elig, comp int64
	for _, b := range j.Buckets {
		elig += b.Eligible
		comp += b.Complete
	}
	wantEq(t, "bucket eligible sum", elig, j.Eligible.Total)
	wantEq(t, "bucket complete sum", comp, j.Eligible.Complete.Total)
	wantEq(t, "bucket 2 (j1)", j.Buckets[2], store.AnalyticsQualityJudgeBucket{Index: 2, Eligible: 1, Complete: 1})
	wantEq(t, "bucket 6 (unreviewed)", j.Buckets[6], store.AnalyticsQualityJudgeBucket{Index: 6, Eligible: 1})

	// A rejudge overwrites the review in place: the target stays in its creation bucket and
	// the retained verdict changes.
	mustExec(f.ctx, t, f.pool, `UPDATE run_reviews SET verdict = 'issues', updated_at = $2 WHERE target_run_id = $1`, j1, reviewedAt.Add(time.Hour))
	r := call()
	wantEq(t, "rejudged complete", r.Eligible.Complete, store.AnalyticsQualityJudgeComplete{Total: 3, Ideal: 0, OK: 1, Issues: 2})
	wantEq(t, "rejudged bucket 2", r.Buckets[2], j.Buckets[2])

	// The denominator is a retained-property count: neither the owner's judge opt-in nor the
	// global judge setting changes any of it.
	before, err := json.Marshal(call())
	if err != nil {
		t.Fatal(err)
	}
	var prior *string
	if err := f.pool.QueryRow(f.ctx, `SELECT value FROM app_settings WHERE key = 'judge_enabled'`).Scan(&prior); err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if prior == nil {
			_, _ = f.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key = 'judge_enabled'`)
		} else {
			_, _ = f.pool.Exec(context.Background(), `UPDATE app_settings SET value = $1 WHERE key = 'judge_enabled'`, *prior)
		}
	})
	for _, on := range []bool{true, false} {
		mustExec(f.ctx, t, f.pool, `UPDATE users SET judge_enabled = $2 WHERE id = $1`, o.User, on)
		mustExec(f.ctx, t, f.pool,
			`INSERT INTO app_settings (key, value) VALUES ('judge_enabled', $1) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
			map[bool]string{true: "true", false: "false"}[on])
		after, err := json.Marshal(call())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("judge section changed with judge_enabled=%v:\n before %s\n after  %s", on, before, after)
		}
	}

	// Kind filter on the cohort: no judge kinds leaves nothing eligible.
	empty := f.quality(qualityCall{Owner: o.User, Now: qualNow, JudgeKinds: []string{}}).Doc.Judge
	wantEq(t, "no judge kinds", empty.Eligible.Total, 0)
	wantEq(t, "no judge kinds candidates", empty.Candidates.Total, 0)
}

// The kind filter applies to judge targets too: a judge-eligible kind outside the requested
// kinds is not a candidate.
func TestQualityJudgeKindFilterLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("jd-kind")
	repo, _ := f.repo(o, "jd-kind")
	f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, Status: "completed", At: qualDay(3, 9), Finished: qualDay(3, 10)})
	all := f.quality(qualityCall{Owner: o.User, Now: qualNow}).Doc.Judge
	wantEq(t, "ci_fix eligible by default", all.Eligible.Total, 1)
	issueOnly := f.quality(qualityCall{Owner: o.User, Now: qualNow, Kinds: []string{runkind.Issue}}).Doc.Judge
	wantEq(t, "ci_fix with kinds=[issue] eligible", issueOnly.Eligible.Total, 0)
	wantEq(t, "ci_fix with kinds=[issue] candidates", issueOnly.Candidates.Total, 0)
}

func TestQualityOwnershipLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	u, v := f.owner("own-u"), f.owner("own-v")
	repoU, _ := f.repo(u, "own-u")
	repoV, _ := f.repo(v, "own-v")
	at := func(day int) time.Time { return qualDay(day, 9) }

	// U's evidence in every section.
	seed := f.pr(u, repoU, 10, "merged", at(3))
	f.rework(u, repoU, 10, seed, "manual", at(4))
	f.run(spendRun{Owner: u.User, Repo: repoU, Kind: runkind.CIFix, At: at(5), Finished: at(5).Add(time.Hour)})
	ul := f.lead(u, repoU, "claude", at(5))
	f.ccPlan(ul, "approve", "approve", at(6), qualTS(at(6).Add(time.Hour)), qualNull, "codex")
	f.ccPlan(f.lead(u, repoU, "claude", at(5)), "pending", nil, at(6), qualNull, qualNull, "codex")
	jt := f.run(spendRun{Owner: u.User, Repo: repoU, At: at(7), Finished: at(7).Add(time.Hour)})
	f.review(jt, u.User, "complete", "ok", at(8))
	f.run(spendRun{Owner: u.User, Repo: repoU, At: at(7), Finished: at(7).Add(time.Hour)})

	// Pinned inputs: the same bounds for every call.
	call := qualityCall{Owner: u.User, Now: qualNow}
	bounds := f.bounds(spendCall{Owner: u.User, Now: qualNow})
	before := f.qualityRaw(call, bounds)

	// V's rows: unreferenced (own repo) and referencing U's repo and MR.
	vseed := f.pr(v, repoV, 10, "merged", at(3))
	f.rework(v, repoV, 10, vseed, "manual", at(4))
	f.run(spendRun{Owner: v.User, Repo: repoV, Kind: runkind.CIFix, At: at(5), Finished: at(5).Add(time.Hour)})
	f.ccPlan(f.lead(v, repoV, "claude", at(5)), "approve", "approve", at(6), qualTS(at(6).Add(time.Hour)), qualNull, "codex")
	f.ccPlan(f.lead(v, repoV, "claude", at(5)), "pending", nil, at(6), qualNull, qualNull, "codex")
	vt := f.run(spendRun{Owner: v.User, Repo: repoV, At: at(7), Finished: at(7).Add(time.Hour)})
	f.review(vt, v.User, "complete", "issues", at(8))
	f.connection(v, "github", "https://api.github.e2e")
	vOnU := f.run(spendRun{Owner: v.User, Repo: repoU, MR: 10, MRState: "merged", At: at(3)})
	f.rework(v, repoU, 10, vOnU, "manual", at(4))
	f.run(spendRun{Owner: v.User, Repo: repoU, Kind: runkind.CIFix, At: at(5), Finished: at(5).Add(time.Hour)})
	f.ccPlan(f.lead(v, repoU, "claude", at(5)), "approve", "approve", at(6), qualTS(at(6).Add(time.Hour)), qualNull, "codex")
	f.run(spendRun{Owner: v.User, Repo: repoU, At: at(7), Finished: at(7).Add(time.Hour)})

	after := f.qualityRaw(call, bounds)
	if !bytes.Equal(before, after) {
		t.Errorf("a foreign user's rows changed the owner document:\n before %s\n after  %s", before, after)
	}
	d, err := store.DecodeAnalyticsQualityDoc(before)
	if err != nil {
		t.Fatal(err)
	}
	if d.Rework.ApplicablePRs != 1 || d.CIFix.Runs != 1 || d.CrossCheck.Plan.DecidedInWindow != 1 ||
		d.CrossCheck.Pending.Total != 1 || d.Judge.Eligible.Total != 4 {
		t.Errorf("the pinned document is not the expected non-trivial one: %s", before)
	}

	// A scoped rework whose target belongs to another user is an unverified link; deleting
	// that target cascades the rework away.
	foreignTarget := f.run(spendRun{Owner: v.User, Repo: repoU, MR: 20, At: at(9)})
	f.pr(u, repoU, 20, "merged", at(9))
	f.rework(u, repoU, 20, foreignTarget, "manual", at(10))
	r := f.quality(call).Doc.Rework
	wantEq(t, "foreign target unverified_links", r.UnverifiedLinks, 1)
	mustExec(f.ctx, t, f.pool, `DELETE FROM runs WHERE id = $1`, foreignTarget)
	r = f.quality(call).Doc.Rework
	wantEq(t, "unverified_links after delete", r.UnverifiedLinks, 0)
}

func TestQualityCalendarLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	zone := "America/New_York"
	if _, err := time.LoadLocation(zone); err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	o := f.owner("cal")
	repo, _ := f.repo(o, "cal")

	// DST: America/New_York leaves daylight time on 2026-11-01, so the local day of the
	// change is 25 hours long. Every bucket holds exactly its own boundary runs.
	now := utcAt(2026, 11, 5, 15, 0, 0)
	b := f.bounds(spendCall{Owner: o.User, Now: now, Zone: zone})
	if len(b.BucketStarts) != 30 {
		t.Fatalf("buckets = %d", len(b.BucketStarts))
	}
	var longDay = -1
	for i := range b.BucketStarts {
		if b.BucketEnds[i].Sub(b.BucketStarts[i]) == 25*time.Hour {
			longDay = i
		}
	}
	if longDay < 0 {
		t.Fatalf("no 25-hour bucket in the DST window: %v", b.BucketStarts)
	}
	for i := range b.BucketStarts {
		f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: b.BucketStarts[i]})
		f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: b.BucketEnds[i].Add(-time.Microsecond)})
	}
	call := qualityCall{Owner: o.User, Now: now, Zone: zone}
	res := f.quality(call)
	if len(res.Doc.CIFix.Buckets) != len(b.BucketStarts) {
		t.Fatalf("ci_fix buckets = %d, want %d", len(res.Doc.CIFix.Buckets), len(b.BucketStarts))
	}
	for i, bk := range res.Doc.CIFix.Buckets {
		if bk.Runs != 2 {
			t.Errorf("DST bucket %d (%s..%s) holds %d runs, want 2", i, b.BucketStarts[i], b.BucketEnds[i], bk.Runs)
		}
	}
	// A run on the window's exclusive end (Now) is outside; one at From is inside.
	f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: res.Bounds.Now})
	res = f.quality(call)
	wantEq(t, "total runs", res.Doc.CIFix.Runs, 60)

	// The session TimeZone changes nothing.
	utc := f.qualityRaw(call, res.Bounds)
	call.SessionTZ = zone
	if ny := f.qualityRaw(call, res.Bounds); !bytes.Equal(utc, ny) {
		t.Errorf("document depends on the session TimeZone:\n utc %s\n ny  %s", utc, ny)
	}
}

func TestQualityCalendarAllTimeLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("cal-all")
	repo, _ := f.repo(o, "cal-all")
	now := utcAt(2026, 11, 25, 12, 0, 0)
	// An all-time window over 200 days has weekly buckets.
	f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: now.AddDate(0, 0, -200)})
	b := f.bounds(spendCall{Owner: o.User, Now: now, Window: analytics.WindowAll})
	if b.WidthWeeks == 0 || len(b.BucketStarts) < 20 {
		t.Fatalf("expected weekly all-time buckets, got width %d weeks over %d buckets", b.WidthWeeks, len(b.BucketStarts))
	}
	for i := range b.BucketStarts {
		if i == 0 {
			continue // the seeded run sits in the first bucket
		}
		f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: b.BucketStarts[i]})
		f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: b.BucketEnds[i].Add(-time.Microsecond)})
	}
	res := f.quality(qualityCall{Owner: o.User, Now: now, Window: analytics.WindowAll})
	got := res.Doc.CIFix.Buckets
	if len(got) != len(b.BucketStarts) {
		t.Fatalf("buckets = %d, want %d", len(got), len(b.BucketStarts))
	}
	wantEq(t, "first bucket", got[0].Runs, 1)
	for i := 1; i < len(got); i++ {
		if got[i].Runs != 2 {
			t.Errorf("weekly bucket %d (%s) holds %d runs, want 2", i, b.BucketStarts[i], got[i].Runs)
		}
	}
}

// qualityOldWitnessBlock is the rw_pr CTE as it was before the witness fold became a GROUP BY
// over a UNION ALL. The old lookup was a membership IN over rw_witness, which the planner
// hashes at test scale; this control is a deliberately unhashed stand-in for it (a correlated
// scalar subquery over rw_witness, an unhashed per-row SubPlan whatever the witness set's
// size), so the unhashed-SubPlan check has something to catch. The real old IN form is caught
// by the "rw_witness is read under a hashed SubPlan" branch of qualityShapeProblems. It is the
// negative control of the plan-shape test below, written against the generated statement's $1
// (bucket_starts) and $2 (bucket_ends) parameters.
const qualityOldWitnessBlock = `rw_pr AS MATERIALIZED (
    SELECT p.repo_id, p.mr_iid, p.witnessed,
           CASE WHEN p.wb >= 1 AND p.first_at < ($2::timestamptz[])[p.wb] THEN p.wb END AS bidx
    FROM (
        SELECT s.repo_id, s.mr_iid, s.first_at,
               ((SELECT count(*) FROM rw_witness w WHERE w.repo_id = s.repo_id AND w.mr_iid = s.mr_iid) > 0) AS witnessed,
               width_bucket(s.first_at, $1::timestamptz[]) AS wb
        FROM rw_seed s
    ) p
),
`

// TestQualityGenericPlanShapeLiveDB pins the plan shape the statements get once pgx's cached
// prepared statement switches to a generic plan (CTEs estimated at one row): no unhashed
// SubPlan reads any CTE (a per-row SubPlan over a CTE degrades quadratically once the CTE
// outgrows hash_mem), the merge witness (rw_witness) is folded in by a plain scan, the
// rework-link membership (rw_sel) is a hashed SubPlan, and no rework/PR join falls back to a
// nested loop with a Join Filter on the (repo_id, mr_iid) pair. Both statements are checked,
// and a negative control (a deliberately unhashed stand-in for the old IN witness lookup,
// swapped back in) proves the check can fail.
func TestQualityGenericPlanShapeLiveDB(t *testing.T) {
	f := newQualityFixture(t)
	o := f.owner("plan")
	repo, _ := f.repo(o, "plan")
	for i := int64(0); i < 12; i++ {
		seed := f.pr(o, repo, 500+i, "merged", qualDay(3+int(i%5), 9))
		f.rework(o, repo, 500+i, seed, "mr_rework", qualDay(10, 9))
	}
	b := f.bounds(spendCall{Owner: o.User, Now: qualNow})
	ownerArgs := qualityExecuteArgs(t, f.ctx, o.User, b)
	allArgs := qualityExecuteArgs(t, f.ctx, uuid.Nil, b)

	explain := func(t *testing.T, name, sql, args string) *planNode {
		t.Helper()
		conn, err := f.pool.Acquire(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		stmt := "qual_plan_" + name
		defer func() { _, _ = conn.Exec(context.Background(), "DEALLOCATE "+stmt) }()
		tx, err := conn.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(f.ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.ctx, "PREPARE "+stmt+" AS "+sql); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		var raw []byte
		if err := tx.QueryRow(f.ctx, "EXPLAIN (VERBOSE, FORMAT JSON) EXECUTE "+stmt+"("+args+")").Scan(&raw); err != nil {
			t.Fatalf("explain: %v", err)
		}
		var doc []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
			t.Fatalf("decode EXPLAIN JSON (%d docs): %v", len(doc), err)
		}
		return &doc[0].Plan
	}

	for _, tc := range []struct {
		name, sql, args string
	}{
		{"owner", store.AnalyticsOwnerQualitySQL, ownerArgs},
		{"all", store.AnalyticsAllQualitySQL, allArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := explain(t, tc.name, tc.sql, tc.args)
			for _, p := range qualityShapeProblems(root) {
				t.Errorf("%s\n%s", p, renderPlan(root, 0))
			}
		})
		t.Run(tc.name+" negative control", func(t *testing.T) {
			start := strings.Index(tc.sql, "rw_pr AS MATERIALIZED (")
			end := strings.Index(tc.sql, "rw_sel AS MATERIALIZED (")
			if start < 0 || end < start {
				t.Fatalf("the %s statement no longer has the rw_pr block this control replaces", tc.name)
			}
			control := tc.sql[:start] + qualityOldWitnessBlock + tc.sql[end:]
			root := explain(t, tc.name+"_ctl", control, tc.args)
			var found bool
			for _, p := range qualityShapeProblems(root) {
				if strings.Contains(p, "un-hashed") && strings.Contains(p, "rw_witness") {
					found = true
				}
			}
			if !found {
				t.Fatalf("the per-row witness lookup did not trip the un-hashed SubPlan check (problems: %v); the plan-shape check cannot fail\n%s",
					qualityShapeProblems(root), renderPlan(root, 0))
			}
		})
	}
}
