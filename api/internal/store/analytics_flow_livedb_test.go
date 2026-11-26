package store_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729: LiveDB tests of the Flow statements. Every test seeds its own users and repos
// and pins now to flowNow (2033-06-15 12:00 UTC), so daily buckets are 06-09 .. 06-15 and a
// bucket index is the day of June minus 9.

// seedFlowActivity seeds the waiting / created / finished dataset and returns its owner and
// repo. See TestAnalyticsFlowWaitingAndActivityLiveDB for what each run is for.
func seedFlowActivity(f *spendFixture) (spendOwner, uuid.UUID, string) {
	o := f.owner("flowact")
	repo, path := f.repo(o, "flowact")
	run := func(r spendRun) uuid.UUID { r.Owner, r.Repo = o.User, repo; return f.run(r) }
	chat := func(r spendRun) uuid.UUID { r.Owner, r.Kind = o.User, runkind.Chat; return f.run(r) }
	// Waiting: an old run created long before the window, a queued run, a paused run and a
	// chat run waiting for input.
	run(spendRun{Status: "awaiting_approval", At: utcAt(2033, time.March, 1, 10, 0, 0)})
	run(spendRun{Status: "queued", At: flowAt(14, 10, 0)})
	run(spendRun{Status: "paused", At: flowAt(10, 9, 0), StatusAt: flowAt(11, 9, 0)})
	chat(spendRun{Status: "awaiting_input", At: flowAt(13, 8, 0)})
	// Finished: created before the window and finished inside it; plan rejected; cancelled
	// with no finished_at (end time is status_since); a chat run that finished (no outcome class).
	run(spendRun{Status: "completed", At: utcAt(2033, time.May, 20, 9, 0, 0), Finished: flowAt(11, 8, 0)})
	run(spendRun{Status: "failed", FailOrigin: "plan_rejected", At: flowAt(11, 7, 0), Finished: flowAt(11, 9, 0)})
	run(spendRun{Status: "cancelled", At: flowAt(12, 7, 0), StatusAt: flowAt(13, 5, 0)})
	chat(spendRun{Status: "completed", At: flowAt(12, 6, 0), Finished: flowAt(12, 7, 0)})
	// A running run, a run finished before the window, a failed run finished today and a run
	// finished exactly at now (half-open: not finished inside the window).
	run(spendRun{Status: "running", At: flowAt(13, 12, 0)})
	run(spendRun{Status: "completed", At: flowAt(8, 22, 0), Finished: flowAt(8, 23, 0)})
	run(spendRun{Status: "failed", At: flowAt(15, 8, 0), Finished: flowAt(15, 11, 0)})
	run(spendRun{Status: "completed", At: flowAt(14, 9, 0), Finished: flowNow})
	return o, repo, path
}

func TestAnalyticsFlowWaitingAndActivityLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o, _, _ := seedFlowActivity(f)
	res := f.flow(flowCall{Owner: o.User})
	d := res.Doc
	if d.GeneratedAt != "2033-06-15T12:00:00.000000Z" {
		t.Errorf("generated_at = %q", d.GeneratedAt)
	}

	w := d.WaitingNow
	if w.TimeBasis != "now" || w.Population == "" {
		t.Errorf("waiting basis %q population %q", w.TimeBasis, w.Population)
	}
	wantStates := []string{"queued", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "limit_wait", "pool_wait", "recovery_wait"}
	wantCounts := map[string]int64{"queued": 1, "awaiting_approval": 1, "awaiting_input": 1, "paused": 1}
	if len(w.States) != len(wantStates) {
		t.Fatalf("waiting states = %+v", w.States)
	}
	for i, s := range w.States {
		if s.State != wantStates[i] || s.Count != wantCounts[s.State] {
			t.Errorf("waiting state %d = %+v, want %s x%d", i, s, wantStates[i], wantCounts[wantStates[i]])
		}
		if (s.Count == 0) != (s.OldestStatusSince == nil && s.OldestAgeSeconds == nil) {
			t.Errorf("waiting state %s oldest fields disagree with count: %+v", s.State, s)
		}
	}
	if w.Total != 4 {
		t.Errorf("waiting total = %d, want 4", w.Total)
	}
	q := w.States[0]
	if q.OldestStatusSince == nil || *q.OldestStatusSince != "2033-06-14T10:00:00.000000Z" || q.OldestAgeSeconds == nil || *q.OldestAgeSeconds != 26*3600 {
		t.Errorf("queued oldest = %v / %v", q.OldestStatusSince, q.OldestAgeSeconds)
	}
	// An old live wait counts regardless of the window; so does it in a 7d window of any size.
	if ap := w.States[1]; ap.OldestStatusSince == nil || *ap.OldestStatusSince != "2033-03-01T10:00:00.000000Z" {
		t.Errorf("awaiting_approval oldest = %v", ap.OldestStatusSince)
	}
	// paused is aged from status_since, not created_at.
	if p := w.States[4]; p.OldestStatusSince == nil || *p.OldestStatusSince != "2033-06-11T09:00:00.000000Z" {
		t.Errorf("paused oldest = %v", p.OldestStatusSince)
	}

	c := d.Activity.Created
	if c.TimeBasis != "created_at" || c.Total != 9 {
		t.Errorf("created basis %q total %d, want created_at / 9", c.TimeBasis, c.Total)
	}
	if len(c.Buckets) != 7 || len(res.Bounds.Buckets) != 7 {
		t.Fatalf("created buckets = %d (bounds %d)", len(c.Buckets), len(res.Bounds.Buckets))
	}
	for i, want := range []int64{0, 1, 1, 2, 2, 2, 1} {
		if c.Buckets[i].Index != i || c.Buckets[i].Count != want {
			t.Errorf("created bucket %d = %+v, want count %d", i, c.Buckets[i], want)
		}
	}

	fin := d.Activity.Finished
	if fin.TimeBasis != "finished_at" || len(fin.ExcludedKinds) != 1 || fin.ExcludedKinds[0] != "chat" {
		t.Errorf("finished basis %q excluded %v", fin.TimeBasis, fin.ExcludedKinds)
	}
	if want := (store.AnalyticsFlowOutcomeCounts{Completed: 1, Cancelled: 1, PlanRejected: 1, Failed: 1, Total: 4}); fin.Total != want {
		t.Errorf("finished total = %+v, want %+v", fin.Total, want)
	}
	if len(fin.Buckets) != 7 {
		t.Fatalf("finished buckets = %d", len(fin.Buckets))
	}
	wantFin := map[int]store.AnalyticsFlowOutcomeCounts{
		2: {Completed: 1, PlanRejected: 1, Total: 2}, // created before the window, finished inside it
		4: {Cancelled: 1, Total: 1},                  // end time is status_since
		6: {Failed: 1, Total: 1},
	}
	for i, cell := range fin.Buckets {
		if cell.Index != i || cell.AnalyticsFlowOutcomeCounts != wantFin[i] {
			t.Errorf("finished bucket %d = %+v, want %+v", i, cell, wantFin[i])
		}
	}

	// Without chat in the kind selector chat is in neither waiting nor created.
	noChat := f.flow(flowCall{Owner: o.User, Kinds: []string{runkind.Issue}}).Doc
	if noChat.WaitingNow.Total != 3 || noChat.Activity.Created.Total != 7 {
		t.Errorf("issue-only waiting %d created %d, want 3 and 7", noChat.WaitingNow.Total, noChat.Activity.Created.Total)
	}
	if noChat.Activity.Finished.Total != fin.Total {
		t.Errorf("issue-only finished %+v, want %+v (chat never counted)", noChat.Activity.Finished.Total, fin.Total)
	}
}

// The document never names a repo, so owner repo masking has nothing to leak.
func TestAnalyticsFlowDocumentNamesNoRepoLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o, repo, path := seedFlowActivity(f)
	for _, c := range []flowCall{{Owner: o.User}, {Owner: o.User, Repo: repo}} {
		res := f.flow(c)
		for _, secret := range []string{repo.String(), path, "repo_id", "repo_path", "path_with_namespace"} {
			if strings.Contains(string(res.Raw), secret) {
				t.Errorf("document contains %q", secret)
			}
		}
	}
}

// The document does not depend on the session TimeZone.
func TestAnalyticsFlowSessionTimeZoneIndependenceLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o, _, _ := seedFlowActivity(f)
	for _, zone := range []string{"UTC", "America/New_York"} {
		base := f.flow(flowCall{Owner: o.User, Zone: zone})
		for _, tz := range []string{"Pacific/Auckland", "America/Los_Angeles", "Asia/Kolkata"} {
			got := f.flow(flowCall{Owner: o.User, Zone: zone, SessionTZ: tz})
			if !bytes.Equal(base.Raw, got.Raw) {
				t.Errorf("zone %s: document under session TimeZone %s differs:\n%s\nvs\n%s", zone, tz, base.Raw, got.Raw)
			}
		}
	}
}

func TestAnalyticsFlowFirstStartDelayLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("flowfs")
	repo, _ := f.repo(o, "flowfs")
	run := func(r spendRun, startedAt time.Time) {
		r.Owner = o.User
		if r.Kind != runkind.Chat {
			r.Repo = repo
		}
		id := f.run(r)
		if !startedAt.IsZero() {
			f.patch(id, "first_started_at = $2", startedAt)
		}
	}
	run(spendRun{At: flowAt(10, 10, 0)}, flowAt(10, 10, 10))
	run(spendRun{At: flowAt(10, 11, 0)}, flowAt(10, 11, 20))
	run(spendRun{At: flowAt(11, 11, 0)}, flowAt(11, 11, 30))
	run(spendRun{Status: "queued", At: flowAt(12, 9, 0)}, time.Time{})
	run(spendRun{Kind: runkind.Chat, Status: "running", At: flowAt(12, 9, 0)}, time.Time{})
	run(spendRun{At: flowAt(12, 10, 0)}, flowAt(12, 9, 0)) // negative
	// Outside the creation cohort: never counted even though it started.
	run(spendRun{At: flowAt(2, 10, 0)}, flowAt(2, 10, 5))

	got := f.flow(flowCall{Owner: o.User}).Doc.FirstStartDelay
	if got.TimeBasis != "created_at" || !got.Approximate || len(got.Limitations) != 1 || got.Limitations[0] != "first_started_at_legacy_backfill" {
		t.Errorf("first_start_delay header = %+v", got)
	}
	if got.Observed != 3 || got.NotStarted != 2 || got.Invalid != 1 {
		t.Errorf("observed/not_started/invalid = %d/%d/%d, want 3/2/1", got.Observed, got.NotStarted, got.Invalid)
	}
	wantNum(t, "p50", got.P50Seconds, 1200)
	wantNum(t, "p90", got.P90Seconds, 1680)

	empty := f.flow(flowCall{Owner: f.owner("flowfs0").User}).Doc.FirstStartDelay
	if empty.Observed != 0 {
		t.Errorf("empty observed = %d", empty.Observed)
	}
	wantNoNum(t, "empty p50", empty.P50Seconds, store.AnalyticsReasonNoObservations)
	wantNoNum(t, "empty p90", empty.P90Seconds, store.AnalyticsReasonNoObservations)
}

// gateWant is the expected gate-derived part of a Flow document for one fixture owner.
type gateWant struct {
	runs        int64
	n           int64
	p50         float64 // when n > 0
	reasons     store.AnalyticsFlowResponseReasons
	notApproved int64
	planning    store.AnalyticsFlowPlanning
	disc        store.AnalyticsFlowFunnelDisclosure
	human, auto int64
	approvalUnk store.AnalyticsFlowApprovalReasons
}

// Gate fixture times (2033-06-12): a presentation at 10:05, a human input at 10:35 applied at
// 10:36, so a timed pair measures 1800 seconds.
var (
	gatePres    = flowAt(12, 10, 5)
	gateIn      = flowAt(12, 10, 35)
	gateApplied = flowAt(12, 10, 36)
)

func TestAnalyticsFlowGateEvidenceLiveDB(t *testing.T) {
	elig := receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: gateApplied}
	cases := []struct {
		name  string
		setup func(f *spendFixture, gr func(rev int64) uuid.UUID)
		want  gateWant
	}{
		{"applied null disposition is timed and proven", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, elig)
		}, gateWant{runs: 1, n: 1, p50: 1800, planning: store.AnalyticsFlowPlanning{SettledHuman: 1}, human: 1}},

		{"replacement revision with only the earlier approval is historical", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(2)
			f.presentation(r, 1, gatePres)
			f.presentation(r, 2, flowAt(12, 11, 0))
			f.receipt(r, elig)
		}, gateWant{runs: 1, n: 1, p50: 1800, notApproved: 1, planning: store.AnalyticsFlowPlanning{Unknown: 1},
			disc: store.AnalyticsFlowFunnelDisclosure{HistoricalApprovalOnly: 1}, approvalUnk: store.AnalyticsFlowApprovalReasons{HistoricalApprovalOnly: 1}}},

		{"same revision number on two runs is never cross-matched", func(f *spendFixture, gr func(int64) uuid.UUID) {
			f.presentation(gr(1), 1, gatePres) // presentation only
			f.receipt(gr(1), elig)             // receipt only, on the other run
		}, gateWant{runs: 2, reasons: store.AnalyticsFlowResponseReasons{NoPresentation: 1}, notApproved: 1,
			planning: store.AnalyticsFlowPlanning{SettledHuman: 1, Unknown: 1}, human: 1, approvalUnk: store.AnalyticsFlowApprovalReasons{NoEvidence: 1}}},

		{"late adoption proves approval but not timing", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, flowAt(12, 10, 40)) // created after the input
			f.receipt(r, elig)
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{InvalidChronology: 1}, planning: store.AnalyticsFlowPlanning{SettledHuman: 1}, human: 1}},

		{"applied before created is invalid chronology and no proof", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: flowAt(12, 10, 30)})
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{InvalidChronology: 1}, planning: store.AnalyticsFlowPlanning{Unknown: 1},
			approvalUnk: store.AnalyticsFlowApprovalReasons{NoEvidence: 1}}},

		{"superseded receipt with applied_at is excluded", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: gateApplied, Disposition: "superseded"})
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{ExcludedDisposition: 1}, planning: store.AnalyticsFlowPlanning{Unknown: 1},
			approvalUnk: store.AnalyticsFlowApprovalReasons{NoEvidence: 1}}},

		{"declined and advisory receipts are excluded", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: gateApplied, Disposition: "declined"})
			f.receipt(r, receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: gateApplied, Disposition: "advisory"})
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{ExcludedDisposition: 1}, planning: store.AnalyticsFlowPlanning{Unknown: 1},
			approvalUnk: store.AnalyticsFlowApprovalReasons{NoEvidence: 1}}},

		{"ACK-only receipt is not an approval", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, receipt{Revision: 1, CreatedAt: gateIn, ConsumedAt: gateApplied})
		}, gateWant{runs: 1, notApproved: 1, planning: store.AnalyticsFlowPlanning{Unknown: 1}, approvalUnk: store.AnalyticsFlowApprovalReasons{NoEvidence: 1}}},

		{"two eligible current receipts are ambiguous", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, elig)
			f.receipt(r, receipt{Revision: 1, CreatedAt: flowAt(12, 10, 37), AppliedAt: flowAt(12, 10, 38)})
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{Ambiguous: 1}, planning: store.AnalyticsFlowPlanning{Unknown: 1},
			disc: store.AnalyticsFlowFunnelDisclosure{AmbiguousCurrent: 1}, approvalUnk: store.AnalyticsFlowApprovalReasons{Ambiguous: 1}}},

		{"one eligible plus one superseded receipt is unambiguous", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, receipt{Revision: 1, CreatedAt: flowAt(12, 10, 20), AppliedAt: flowAt(12, 10, 21), Disposition: "superseded"})
			f.receipt(r, elig)
		}, gateWant{runs: 1, n: 1, p50: 1800, planning: store.AnalyticsFlowPlanning{SettledHuman: 1}, human: 1}},

		{"reconciliation row proves approval but is not timed", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.presentation(r, 1, gatePres)
			f.receipt(r, receipt{Revision: 1, CreatedAt: flowAt(12, 10, 20), AppliedAt: flowAt(12, 10, 21), Disposition: "superseded"})
			f.receipt(r, receipt{Revision: 1, CreatedAt: gateIn, AppliedAt: gateApplied, Disposition: "applied"})
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{ReconciliationOnly: 1}, planning: store.AnalyticsFlowPlanning{SettledHuman: 1}, human: 1}},

		{"id-less gate has no presentation but proves approval", func(f *spendFixture, gr func(int64) uuid.UUID) {
			f.receipt(gr(1), elig)
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{NoPresentation: 1}, planning: store.AnalyticsFlowPlanning{SettledHuman: 1}, human: 1}},

		{"presentation without approval is disclosed as not approved", func(f *spendFixture, gr func(int64) uuid.UUID) {
			f.presentation(gr(1), 1, gatePres)
		}, gateWant{runs: 1, notApproved: 1, planning: store.AnalyticsFlowPlanning{Unknown: 1}, approvalUnk: store.AnalyticsFlowApprovalReasons{NoEvidence: 1}}},

		{"legacy NULL-binding and unbound receipts count once and prove nothing", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(0)
			f.receipt(r, receipt{Binding: "legacy", CreatedAt: gateIn, AppliedAt: gateApplied})
			f.receipt(r, receipt{Binding: "unbound", CreatedAt: gateIn, AppliedAt: gateApplied})
		}, gateWant{runs: 1, reasons: store.AnalyticsFlowResponseReasons{LegacyUnbound: 1}, planning: store.AnalyticsFlowPlanning{Unknown: 1},
			disc: store.AnalyticsFlowFunnelDisclosure{LegacyEvidence: 1}, approvalUnk: store.AnalyticsFlowApprovalReasons{LegacyUnbound: 1}}},

		{"human evidence beats auto-approve configuration", func(f *spendFixture, gr func(int64) uuid.UUID) {
			r := gr(1)
			f.patch(r, "auto_approve = true, plan_md = 'p'")
			f.presentation(r, 1, gatePres)
			f.receipt(r, elig)
		}, gateWant{runs: 1, n: 1, p50: 1800, planning: store.AnalyticsFlowPlanning{SettledHuman: 1}, human: 1}},

		{"auto-approve configuration alone settles auto and is not an approval event", func(f *spendFixture, gr func(int64) uuid.UUID) {
			f.patch(gr(0), "auto_approve = true, plan_md = 'p'")
		}, gateWant{runs: 1, planning: store.AnalyticsFlowPlanning{SettledAuto: 1}, auto: 1}},

		{"seeded plan at revision 0 is skipped and a plain completed run is unknown", func(f *spendFixture, gr func(int64) uuid.UUID) {
			f.patch(gr(0), "plan_source = 'seeded', plan_md = 'p'")
			gr(0)
		}, gateWant{runs: 2, planning: store.AnalyticsFlowPlanning{Skipped: 1, Unknown: 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpendFixture(t)
			o := f.owner("flowgate")
			repo, _ := f.repo(o, "flowgate")
			gr := func(rev int64) uuid.UUID {
				id := f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: flowAt(12, 10, 0), Finished: flowAt(12, 12, 0)})
				f.patch(id, "gate_revision = $2", rev)
				return id
			}
			tc.setup(f, gr)
			d := f.flow(flowCall{Owner: o.User}).Doc

			pr := d.PlanResponse
			if pr.TimeBasis != "created_at" || !pr.Approximate {
				t.Errorf("plan_response header = %+v", pr)
			}
			if pr.N != tc.want.n {
				t.Errorf("plan_response n = %d, want %d", pr.N, tc.want.n)
			}
			if tc.want.n > 0 {
				wantNum(t, "plan_response p50", pr.P50Seconds, tc.want.p50)
				wantNum(t, "plan_response p90", pr.P90Seconds, tc.want.p50)
			} else {
				wantNoNum(t, "plan_response p50", pr.P50Seconds, store.AnalyticsReasonNoObservations)
				wantNoNum(t, "plan_response p90", pr.P90Seconds, store.AnalyticsReasonNoObservations)
			}
			r := tc.want.reasons
			if pr.Unknown.Reasons != r {
				t.Errorf("plan_response unknown reasons = %+v, want %+v", pr.Unknown.Reasons, r)
			}
			if sum := r.Ambiguous + r.NoPresentation + r.InvalidChronology + r.ExcludedDisposition + r.ReconciliationOnly + r.LegacyUnbound; pr.Unknown.Total != sum {
				t.Errorf("plan_response unknown total = %d, want %d", pr.Unknown.Total, sum)
			}
			if pr.NotApproved != tc.want.notApproved {
				t.Errorf("plan_response not_approved = %d, want %d", pr.NotApproved, tc.want.notApproved)
			}

			fn := d.DeliveryFunnel
			if !fn.Applicable || fn.Reason != nil || fn.Stages.Created != tc.want.runs {
				t.Errorf("funnel applicable %v reason %v created %d, want true / nil / %d", fn.Applicable, fn.Reason, fn.Stages.Created, tc.want.runs)
			}
			if fn.Planning != tc.want.planning {
				t.Errorf("funnel planning = %+v, want %+v", fn.Planning, tc.want.planning)
			}
			if fn.Stages.PlanSettled != tc.want.planning.SettledHuman+tc.want.planning.SettledAuto {
				t.Errorf("plan_settled = %d, want settled_human + settled_auto", fn.Stages.PlanSettled)
			}
			if fn.Disclosure != tc.want.disc {
				t.Errorf("funnel disclosure = %+v, want %+v", fn.Disclosure, tc.want.disc)
			}
			if fn.Outcomes.Completed != tc.want.runs {
				t.Errorf("funnel completed = %d, want %d", fn.Outcomes.Completed, tc.want.runs)
			}

			ap := d.Approval
			un := tc.want.approvalUnk
			unTotal := un.Ambiguous + un.LegacyUnbound + un.HistoricalApprovalOnly + un.NoEvidence
			if ap.Human != tc.want.human || ap.AutoApproveConfigured != tc.want.auto || ap.Unknown.Reasons != un ||
				ap.Unknown.Total != unTotal || ap.Total != tc.want.human+tc.want.auto+unTotal {
				t.Errorf("approval = %+v, want human %d auto %d unknown %+v", ap, tc.want.human, tc.want.auto, un)
			}
		})
	}
}

// TestAnalyticsFlowFunnelAndPRUnitsLiveDB pins PR deduplication, earliest in-filter
// attribution, witnesses outside the filters, owner isolation and run-level stages.
func TestAnalyticsFlowFunnelAndPRUnitsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o, o2 := f.owner("flowpr"), f.owner("flowpr2")
	repo, _ := f.repo(o, "flowpr")
	run := func(r spendRun) uuid.UUID { r.Owner, r.Repo = o.User, repo; return f.run(r) }
	// PR 100: two runs of one PR; the earliest has a stale open state, the later one is merged.
	run(spendRun{At: flowAt(10, 10, 0), Finished: flowAt(10, 12, 0), MR: 100, MRState: "open"})
	run(spendRun{At: flowAt(12, 10, 0), Finished: flowAt(12, 12, 0), MR: 100, MRState: "merged"})
	// PR 200: an earlier claude run excluded by the harness filter and two codex runs.
	run(spendRun{At: flowAt(9, 8, 0), MR: 200, MRState: "open"})
	run(spendRun{At: flowAt(11, 9, 0), MR: 200, MRState: "open", Harness: "codex"})
	run(spendRun{At: flowAt(13, 9, 0), MR: 200, MRState: "open", Harness: "codex"})
	// PR 300: an in-filter codex run, merged only by an out-of-window, out-of-filter run.
	run(spendRun{At: flowAt(11, 11, 0), MR: 300, MRState: "open", Harness: "codex"})
	run(spendRun{At: utcAt(2033, time.May, 1, 10, 0, 0), MR: 300, MRState: "merged"})
	// PR 400: merged only by another user.
	run(spendRun{At: flowAt(12, 11, 0), MR: 400, MRState: "open", Harness: "codex"})
	f.run(spendRun{Owner: o2.User, Repo: repo, At: flowAt(12, 12, 0), MR: 400, MRState: "merged"})
	// No PR: a running run, a self_improve run and a task run (not a funnel kind).
	run(spendRun{Status: "running", At: flowAt(14, 10, 0), Harness: "codex"})
	run(spendRun{Kind: runkind.SelfImprove, At: flowAt(14, 11, 0)})
	run(spendRun{Kind: runkind.Task, At: flowAt(14, 12, 0)})

	pc := func(i int, present, merged int64) store.AnalyticsFlowPRUnitCell {
		return store.AnalyticsFlowPRUnitCell{Index: i, Present: present, Merged: merged}
	}
	cell := func(fn store.AnalyticsFlowFunnel, idx int) store.AnalyticsFlowPRUnitCell {
		return fn.PRUnits.Buckets[idx]
	}

	fn := f.flow(flowCall{Owner: o.User}).Doc.DeliveryFunnel
	if want := (store.AnalyticsFlowStages{Created: 9, PlanSettled: 0, Completed: 8, PRPresent: 7, Merged: 3}); fn.Stages != want {
		t.Errorf("stages = %+v, want %+v", fn.Stages, want)
	}
	if want := (store.AnalyticsFlowOutcomes{Completed: 8, InProgress: 1}); fn.Outcomes != want {
		t.Errorf("outcomes = %+v, want %+v", fn.Outcomes, want)
	}
	if want := (store.AnalyticsFlowPlanning{Unknown: 8, InProgress: 1}); fn.Planning != want {
		t.Errorf("planning = %+v, want %+v", fn.Planning, want)
	}
	if fn.PRUnits.Present != 4 || fn.PRUnits.Merged != 2 || len(fn.PRUnits.Buckets) != 7 {
		t.Fatalf("pr units = %+v, want 4 present, 2 merged, 7 buckets", fn.PRUnits)
	}
	wantCells := []store.AnalyticsFlowPRUnitCell{pc(0, 1, 0), pc(1, 1, 1), pc(2, 1, 1), pc(3, 1, 0), pc(4, 0, 0), pc(5, 0, 0), pc(6, 0, 0)}
	for i, w := range wantCells {
		if got := cell(fn, i); got != w {
			t.Errorf("pr unit bucket %d = %+v, want %+v", i, got, w)
		}
	}

	// Harness filter: PR 200 is attributed to its earliest in-filter run (bucket 2), not to the
	// excluded claude run (bucket 0); PR 300 stays merged through the out-of-filter witness.
	fn = f.flow(flowCall{Owner: o.User, Harness: "codex"}).Doc.DeliveryFunnel
	if want := (store.AnalyticsFlowStages{Created: 5, Completed: 4, PRPresent: 4, Merged: 1}); fn.Stages != want {
		t.Errorf("codex stages = %+v, want %+v", fn.Stages, want)
	}
	if fn.PRUnits.Present != 3 || fn.PRUnits.Merged != 1 {
		t.Errorf("codex pr units = %d/%d, want 3/1", fn.PRUnits.Present, fn.PRUnits.Merged)
	}
	wantCells = []store.AnalyticsFlowPRUnitCell{pc(0, 0, 0), pc(1, 0, 0), pc(2, 2, 1), pc(3, 1, 0), pc(4, 0, 0), pc(5, 0, 0), pc(6, 0, 0)}
	for i, w := range wantCells {
		if got := cell(fn, i); got != w {
			t.Errorf("codex pr unit bucket %d = %+v, want %+v", i, got, w)
		}
	}

	// The other user is its own owner scope: its single run is one merged unit.
	fn2 := f.flow(flowCall{Owner: o2.User}).Doc.DeliveryFunnel
	if fn2.PRUnits.Present != 1 || fn2.PRUnits.Merged != 1 || fn2.Stages.Created != 1 {
		t.Errorf("other owner funnel = %+v", fn2)
	}
}

// TestAnalyticsFlowApplicabilityLiveDB: the funnel needs issue or self_improve and elapsed
// needs issue in the kind selector.
func TestAnalyticsFlowApplicabilityLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("flowapp")
	repo, _ := f.repo(o, "flowapp")
	f.run(spendRun{Owner: o.User, Repo: repo, At: flowAt(12, 10, 0), Finished: flowAt(12, 12, 0)})
	f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.Task, At: flowAt(12, 10, 0), Finished: flowAt(12, 12, 0)})

	d := f.flow(flowCall{Owner: o.User, Kinds: []string{runkind.Task}}).Doc
	if d.DeliveryFunnel.Applicable || d.DeliveryFunnel.Reason == nil || *d.DeliveryFunnel.Reason != store.AnalyticsReasonNotApplicable ||
		d.DeliveryFunnel.Stages != (store.AnalyticsFlowStages{}) {
		t.Errorf("task-only funnel = %+v", d.DeliveryFunnel)
	}
	if d.Approval.Applicable || d.Approval.Reason == nil || *d.Approval.Reason != store.AnalyticsReasonNotApplicable || d.Approval.Total != 0 {
		t.Errorf("task-only approval = %+v", d.Approval)
	}
	if d.IssueElapsed.Applicable || d.IssueElapsed.Reason == nil || *d.IssueElapsed.Reason != store.AnalyticsReasonNotApplicable || d.IssueElapsed.N != 0 {
		t.Errorf("task-only elapsed = %+v", d.IssueElapsed)
	}
	if d.Activity.Created.Total != 1 {
		t.Errorf("task-only created = %d, want 1", d.Activity.Created.Total)
	}

	d = f.flow(flowCall{Owner: o.User, Kinds: []string{runkind.SelfImprove}}).Doc
	if !d.DeliveryFunnel.Applicable || d.IssueElapsed.Applicable {
		t.Errorf("self_improve-only: funnel applicable %v, elapsed applicable %v", d.DeliveryFunnel.Applicable, d.IssueElapsed.Applicable)
	}
	d = f.flow(flowCall{Owner: o.User, Kinds: []string{runkind.Issue}}).Doc
	if !d.DeliveryFunnel.Applicable || !d.IssueElapsed.Applicable || d.IssueElapsed.Reason != nil || d.DeliveryFunnel.Stages.Created != 1 {
		t.Errorf("issue-only: %+v / %+v", d.DeliveryFunnel, d.IssueElapsed)
	}
}

// elapsedRun seeds one issue run created at `at` that started 10 minutes later and finished
// dur after that.
func elapsedRun(f *spendFixture, owner, repo uuid.UUID, kind string, at time.Time, dur time.Duration) {
	id := f.run(spendRun{Owner: owner, Repo: repo, Kind: kind, At: at, Finished: at.Add(10*time.Minute + dur)})
	f.patch(id, "first_started_at = $2", at.Add(10*time.Minute))
}

func TestAnalyticsFlowIssueElapsedLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("flowel")
	repo, _ := f.repo(o, "flowel")
	s := func(n int) time.Duration { return time.Duration(n) * time.Second }
	// idx0 (old): one valid run and one unfinished run.
	elapsedRun(f, o.User, repo, runkind.Issue, flowAt(9, 0, 0), s(270000))
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: flowAt(9, 1, 0)})
	// idx1: five valid runs, so percentiles are shown.
	for i, d := range []int{100, 200, 300, 400, 500} {
		elapsedRun(f, o.User, repo, runkind.Issue, flowAt(10, 8+i, 0), s(d))
	}
	// Noise that never enters the issue-run elapsed population: other kinds.
	elapsedRun(f, o.User, repo, runkind.Task, flowAt(10, 14, 0), s(5000))
	elapsedRun(f, o.User, repo, runkind.SelfImprove, flowAt(10, 15, 0), s(6000))
	// idx2: four valid runs (suppressed, n kept) and one unfinished run.
	for i := 0; i < 4; i++ {
		elapsedRun(f, o.User, repo, runkind.Issue, flowAt(11, 8+i, 0), s(1000))
	}
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "awaiting_approval", At: flowAt(11, 14, 0)})
	// idx3: three valid runs.
	for i, d := range []int{2000, 4000, 20000} {
		elapsedRun(f, o.User, repo, runkind.Issue, flowAt(12, 8+i, 0), s(d))
	}
	// idx4: two terminal runs without a valid pair: no first_started_at, and a negative one.
	f.run(spendRun{Owner: o.User, Repo: repo, At: flowAt(13, 8, 0), Finished: flowAt(13, 9, 0)})
	neg := f.run(spendRun{Owner: o.User, Repo: repo, At: flowAt(13, 10, 0), Finished: flowAt(13, 10, 5)})
	f.patch(neg, "first_started_at = $2", flowAt(13, 10, 10))
	// idx6 (today): one unfinished run.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: flowAt(15, 8, 0)})

	res := f.flow(flowCall{Owner: o.User})
	el := res.Doc.IssueElapsed
	if el.TimeBasis != "created_at" || !el.Applicable || el.Reason != nil {
		t.Errorf("elapsed header = %+v", el)
	}
	if el.N != 13 || el.Unfinished != 3 || el.Invalid != 2 || !el.Incomplete {
		t.Errorf("elapsed n/unfinished/invalid/incomplete = %d/%d/%d/%v, want 13/3/2/true", el.N, el.Unfinished, el.Invalid, el.Incomplete)
	}
	wantNum(t, "elapsed p50", el.P50Seconds, 1000)
	wantNum(t, "elapsed p90", el.P90Seconds, 16800)

	if len(el.Histogram) != len(store.FlowHistogramEdgesSeconds) {
		t.Fatalf("histogram has %d bins, want %d", len(el.Histogram), len(store.FlowHistogramEdgesSeconds))
	}
	wantHist := []int64{5, 4, 1, 1, 0, 1, 0, 0, 1}
	var histSum int64
	for i, b := range el.Histogram {
		histSum += b.Count
		last := i == len(el.Histogram)-1
		if b.LowerSeconds != store.FlowHistogramEdgesSeconds[i] || b.Count != wantHist[i] ||
			(last && b.UpperSeconds != nil) || (!last && (b.UpperSeconds == nil || *b.UpperSeconds != store.FlowHistogramEdgesSeconds[i+1])) {
			t.Errorf("histogram bin %d = %+v, want lower %v count %d", i, b, store.FlowHistogramEdgesSeconds[i], wantHist[i])
		}
	}
	if histSum != el.N {
		t.Errorf("histogram sums to %d, want n = %d", histSum, el.N)
	}

	type cellWant struct {
		n, unfinished, invalid int64
		p50, p90               float64
		reason                 string
	}
	insufficient, none := store.AnalyticsReasonInsufficientObservations, store.AnalyticsReasonNoObservations
	wantCells := []cellWant{
		{1, 1, 0, 0, 0, insufficient},
		{5, 0, 0, 300, 460, ""},
		{4, 1, 0, 0, 0, insufficient},
		{3, 0, 0, 0, 0, insufficient},
		{0, 0, 2, 0, 0, none},
		{0, 0, 0, 0, 0, none},
		{0, 1, 0, 0, 0, none},
	}
	if len(el.Buckets) != len(wantCells) {
		t.Fatalf("elapsed buckets = %d, want %d", len(el.Buckets), len(wantCells))
	}
	for i, w := range wantCells {
		c := el.Buckets[i]
		if c.Index != i || c.N != w.n || c.Unfinished != w.unfinished || c.Invalid != w.invalid || c.Incomplete != (w.unfinished > 0) {
			t.Errorf("elapsed bucket %d = %+v, want %+v", i, c, w)
		}
		if w.reason == "" {
			wantNum(t, "bucket p50", c.P50Seconds, w.p50)
			wantNum(t, "bucket p90", c.P90Seconds, w.p90)
		} else {
			wantNoNum(t, "bucket p50", c.P50Seconds, w.reason)
			wantNoNum(t, "bucket p90", c.P90Seconds, w.reason)
		}
	}

	// The suppression threshold is a parameter: 0 < n < min is hidden, n >= min is shown.
	for _, tc := range []struct {
		min       int32
		visibleAt map[int]bool
	}{
		{3, map[int]bool{1: true, 2: true, 3: true}},
		{5, map[int]bool{1: true}},
		{6, map[int]bool{}},
	} {
		got := f.flow(flowCall{Owner: o.User, MinN: tc.min}).Doc.IssueElapsed
		for _, i := range []int{1, 2, 3} {
			if shown := got.Buckets[i].P50Seconds.Value != nil; shown != tc.visibleAt[i] {
				t.Errorf("min %d: bucket %d percentile shown = %v, want %v (n=%d)", tc.min, i, shown, tc.visibleAt[i], got.Buckets[i].N)
			}
		}
	}
}

// TestAnalyticsFlowAllTimeCoarseBucketsLiveDB: with week-wide buckets the percentiles are
// computed directly over each (two-week) bucket's population, not combined from weeks.
func TestAnalyticsFlowAllTimeCoarseBucketsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("flowall")
	repo, _ := f.repo(o, "flowall")
	// The anchor sets the history start two-plus years before now (more than 104 weeks).
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: utcAt(2031, time.January, 6, 10, 0, 0)})
	b := f.bounds(spendCall{Owner: o.User, Now: flowNow, Window: analytics.WindowAll})
	if b.WidthWeeks < 2 {
		t.Fatalf("expected coarse buckets, width = %d weeks", b.WidthWeeks)
	}
	const k = 10
	start := b.Buckets[k].Start
	// Three runs in the bucket's first week and two in its second: five valid in one bucket.
	durs := []int{100, 200, 300, 400, 500}
	for i, d := range durs {
		at := start.Add(24 * time.Hour)
		if i >= 3 {
			at = start.Add(9 * 24 * time.Hour)
		}
		elapsedRun(f, o.User, repo, runkind.Issue, at.Add(time.Duration(i)*time.Hour), time.Duration(d)*time.Second)
	}
	res := f.flow(flowCall{Owner: o.User, Window: analytics.WindowAll})
	if len(res.Doc.IssueElapsed.Buckets) != len(b.Buckets) {
		t.Fatalf("elapsed buckets = %d, want %d", len(res.Doc.IssueElapsed.Buckets), len(b.Buckets))
	}
	c := res.Doc.IssueElapsed.Buckets[k]
	if c.Index != k || c.N != 5 || c.Incomplete {
		t.Fatalf("coarse bucket = %+v", c)
	}
	wantNum(t, "coarse p50", c.P50Seconds, 300)
	wantNum(t, "coarse p90", c.P90Seconds, 460)
	if first := res.Doc.IssueElapsed.Buckets[0]; first.Unfinished != 1 || !first.Incomplete || first.N != 0 {
		t.Errorf("anchor bucket = %+v", first)
	}
}

// TestAnalyticsFlowAllScopeLiveDB: the all-users statement counts every user and a merge
// witnessed by any user.
func TestAnalyticsFlowAllScopeLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	f.claimYear(2033)
	o1, o2 := f.owner("flowall1"), f.owner("flowall2")
	repo, _ := f.repo(o1, "flowall1")
	f.run(spendRun{Owner: o1.User, Repo: repo, At: flowAt(12, 11, 0), MR: 400, MRState: "open"})
	f.run(spendRun{Owner: o2.User, Repo: repo, At: flowAt(13, 12, 0), MR: 400, MRState: "merged"})
	f.run(spendRun{Owner: o2.User, Repo: repo, Status: "queued", At: flowAt(14, 12, 0)})

	all := f.flow(flowCall{}).Doc
	if all.Activity.Created.Total != 3 {
		t.Errorf("all-users created = %d, want 3", all.Activity.Created.Total)
	}
	if all.DeliveryFunnel.PRUnits.Present != 1 || all.DeliveryFunnel.PRUnits.Merged != 1 || all.DeliveryFunnel.Stages.Created != 3 {
		t.Errorf("all-users funnel = %+v", all.DeliveryFunnel)
	}
	if q := all.WaitingNow.States[0]; q.State != "queued" || q.Count < 1 || q.OldestStatusSince == nil {
		t.Errorf("all-users queued = %+v", q)
	}
	// The owner statement does not accept the other user's witness.
	if own := f.flow(flowCall{Owner: o1.User}).Doc.DeliveryFunnel; own.PRUnits.Present != 1 || own.PRUnits.Merged != 0 {
		t.Errorf("owner funnel = %+v, want one unmerged unit", own.PRUnits)
	}
}
