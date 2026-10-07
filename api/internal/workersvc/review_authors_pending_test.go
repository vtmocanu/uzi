package workersvc_test

import (
	"slices"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// applyDelta is a test-local model of the ledger's pending set update: union of the adds,
// minus the removes. It is deliberately naive; the real merge (cap, ordering) is pinned by the
// store LiveDB tests.
func applyDelta(pending []int64, plan workersvc.ReviewPlan) []int64 {
	set := map[int64]bool{}
	for _, id := range pending {
		set[id] = true
	}
	for _, id := range plan.PendingAdd {
		set[id] = true
	}
	for _, id := range plan.PendingRemove {
		delete(set, id)
	}
	out := []int64{}
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// An outsider flood must not suppress an eligible finding: the pending set holds one id per
// unverified author, so 250 newer unknown outsider comments cost 250 slots, never X's.
func TestPlanOutsiderFloodKeepsEligibleAuthorsComment(t *testing.T) {
	h := newHarness()
	trusted := []settings.TrustedBot{{BaseURL: "https://github.com", ForgeUserID: coderabbit}}
	comments := []forge.MRComment{inline(100, flakyID, "xavier", "real finding", raT0)}
	for i := int64(0); i < 250; i++ {
		// every flood author is distinct and its lookup fails, so it stays unknown
		comments = append(comments, inline(101+i, 1000+i, "flood", "noise", raT0.Add(time.Duration(i+1)*time.Second)))
	}
	comments = append(comments, inline(400, coderabbit, "coderabbitai[bot]", "bot finding", raT0.Add(time.Hour)))

	p := h.params(comments...)
	p.Trusted = trusted
	res := h.snapshot(t, p)
	plan := res.Plan(0, nil)
	if !plan.HasNew || plan.MaxActionableID != 400 {
		t.Fatalf("plan = %+v, want the trusted bot to fire at mark 400", plan)
	}
	if len(plan.PendingAdd) != 251 || !slices.Contains(plan.PendingAdd, 100) {
		t.Fatalf("pending add has %d ids (contains 100: %t), want one per unverified author (251) including X's 100",
			len(plan.PendingAdd), slices.Contains(plan.PendingAdd, 100))
	}
	pending := applyDelta(nil, plan)

	// X resolves eligible: the next tick fires once, with X's comment in the snapshot.
	h.lookup.answers[flakyID] = forge.AuthorEligible
	p2 := h.params(comments...)
	p2.Trusted = trusted
	p2.HighWater, p2.Pending = 400, pending
	res2 := h.snapshot(t, p2)
	plan2 := res2.Plan(400, pending)
	if !plan2.HasNew {
		t.Fatalf("plan = %+v, want X's pending comment to trigger", plan2)
	}
	if !slices.Contains(bodies(res2), "real finding") {
		t.Fatalf("snapshot bodies lack X's comment: %v", bodies(res2))
	}
	if !slices.Contains(plan2.PendingRemove, 100) {
		t.Fatalf("pending remove = %v, want X's 100 consumed", plan2.PendingRemove)
	}
	pending = applyDelta(pending, plan2)
	plan3 := res2.Plan(400, pending)
	if plan3.HasNew {
		t.Fatalf("plan = %+v, want no second trigger once X's id is consumed", plan3)
	}
}

func TestPlanKeepsOneNewestPendingIDPerAuthor(t *testing.T) {
	h := newHarness()
	// X (unknown) has 100 pending, then 150 and 170 arrive; a member comment at 180 moves the mark.
	res := h.snapshot(t, h.params(
		inline(100, flakyID, "xavier", "a", raT0),
		inline(150, flakyID, "xavier", "b", raT0.Add(time.Second)),
		inline(170, flakyID, "xavier", "c", raT0.Add(2*time.Second)),
		inline(180, memberID, "carol", "d", raT0.Add(3*time.Second)),
	))
	plan := res.Plan(120, []int64{100})
	if !slices.Equal(plan.PendingAdd, []int64{170}) {
		t.Fatalf("pending add = %v, want only the newest id of the author", plan.PendingAdd)
	}
	if !slices.Equal(plan.PendingRemove, []int64{100}) {
		t.Fatalf("pending remove = %v, want the superseded 100", plan.PendingRemove)
	}
	if got := applyDelta([]int64{100}, plan); !slices.Equal(got, []int64{170}) {
		t.Fatalf("pending after the delta = %v, want [170]", got)
	}
}

func TestPlanRepresentativeComesFromTheAcceptedSet(t *testing.T) {
	h := newHarness()
	// Mark 120. X's 100 is pending; X's 105 is at or below the mark and NOT pending (the ledger
	// would drop it), so it must not displace 100. Y's 90 is below the mark and not pending, and
	// Z's 125 is above the new mark: neither is added.
	res := h.snapshot(t, h.params(
		inline(90, flakyID+1, "yolanda", "a", raT0),
		inline(100, flakyID, "xavier", "b", raT0.Add(time.Second)),
		inline(105, flakyID, "xavier", "c", raT0.Add(2*time.Second)),
		inline(110, memberID, "carol", "d", raT0.Add(3*time.Second)),
		inline(125, flakyID+2, "zed", "e", raT0.Add(4*time.Second)),
	))
	plan := res.Plan(120, []int64{100})
	if !slices.Equal(plan.PendingAdd, []int64{100}) {
		t.Fatalf("pending add = %v, want just the already pending 100", plan.PendingAdd)
	}
	if len(plan.PendingRemove) != 0 {
		t.Fatalf("pending remove = %v, want the representative kept", plan.PendingRemove)
	}
}
