package workersvc_test

import (
	"bytes"
	"cmp"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// applyDelta is a test-local model of mr_rework_merge_pending: the union of the pending set and
// the adds, minus the removes; then each (superseded, superseded-by) pair applies only when both
// ids survived that step, dropping the older id unless it is itself a replacement; finally the
// oldest ReviewPendingCap SLOTS are kept, a replacement taking the slot of the smallest id it
// replaces. The real merge is pinned by the store LiveDB tests.
func applyDelta(pending []int64, plan workersvc.ReviewPlan) []int64 {
	base := map[int64]bool{}
	for _, id := range pending {
		base[id] = true
	}
	for _, id := range plan.PendingAdd {
		base[id] = true
	}
	for _, id := range plan.PendingRemove {
		delete(base, id)
	}
	dropped := map[int64]bool{}
	replacement := map[int64]bool{}
	slot := map[int64]int64{}
	for i, o := range plan.PendingSuperseded {
		n := plan.PendingSupersededBy[i]
		if o == n || !base[o] || !base[n] {
			continue
		}
		dropped[o] = true
		replacement[n] = true
		if cur, ok := slot[n]; !ok || o < cur {
			slot[n] = o
		}
	}
	for n := range replacement {
		delete(dropped, n)
	}
	type kept struct{ x, slot int64 }
	var ks []kept
	for x := range base {
		if dropped[x] {
			continue
		}
		sl := x
		if s, ok := slot[x]; ok {
			sl = min(s, x)
		}
		ks = append(ks, kept{x, sl})
	}
	slices.SortFunc(ks, func(a, b kept) int {
		if a.slot != b.slot {
			return cmp.Compare(a.slot, b.slot)
		}
		return cmp.Compare(a.x, b.x)
	})
	if len(ks) > workersvc.ReviewPendingCap {
		ks = ks[:workersvc.ReviewPendingCap]
	}
	out := []int64{}
	for _, k := range ks {
		out = append(out, k.x)
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
		// every flood author is distinct, its lookup fails so it stays unknown, and it posts
		// three comments: an all-ids pending set would hold 750 ids, one per author holds 250
		for k := int64(0); k < 3; k++ {
			comments = append(comments, inline(101+3*i+k, 1000+i, "flood", "noise", raT0.Add(time.Duration(3*i+k+1)*time.Second)))
		}
	}
	comments = append(comments, inline(900, coderabbit, "coderabbitai[bot]", "bot finding", raT0.Add(time.Hour)))

	p := h.params(comments...)
	p.Trusted = trusted
	res := h.snapshot(t, p)
	plan := res.Plan(0, nil)
	if !plan.HasNew || plan.MaxActionableID != 900 {
		t.Fatalf("plan = %+v, want the trusted bot to fire at mark 900", plan)
	}
	if len(plan.PendingAdd) != 251 || !slices.Contains(plan.PendingAdd, 100) {
		t.Fatalf("pending add has %d ids (contains 100: %t), want one per unverified author (251) including X's 100",
			len(plan.PendingAdd), slices.Contains(plan.PendingAdd, 100))
	}
	pending := applyDelta(nil, plan)
	if len(pending) != 251 || !slices.Contains(pending, 100) {
		t.Fatalf("pending after the merge has %d ids (contains 100: %t), want 251 including X's 100",
			len(pending), slices.Contains(pending, 100))
	}

	// X resolves eligible: the next tick fires once, with X's comment in the snapshot.
	h.lookup.answers[flakyID] = forge.AuthorEligible
	p2 := h.params(comments...)
	p2.Trusted = trusted
	p2.HighWater, p2.Pending = 900, pending
	res2 := h.snapshot(t, p2)
	plan2 := res2.Plan(900, pending)
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
	plan3 := res2.Plan(900, pending)
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
	if len(plan.PendingRemove) != 0 {
		t.Fatalf("pending remove = %v, want supersession kept out of the unconditional removals", plan.PendingRemove)
	}
	if !slices.Equal(plan.PendingSuperseded, []int64{100}) || !slices.Equal(plan.PendingSupersededBy, []int64{170}) {
		t.Fatalf("superseded = %v by %v, want 100 replaced by 170", plan.PendingSuperseded, plan.PendingSupersededBy)
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

// bulkComments returns n unknown-author comments, ids from firstID, one author each.
func bulkComments(n int, firstID int64) []forge.MRComment {
	out := make([]forge.MRComment, 0, n)
	for i := 0; i < n; i++ {
		id := firstID + int64(i)
		out = append(out, inline(id, 100000+id, "bulk", "noise", raT0.Add(time.Duration(id)*time.Millisecond)))
	}
	return out
}

// At the cap the merge keeps the OLDEST ids, so X's newer representative can be the one dropped.
// The older pending id must then stay: X must still have a pending id after the merge.
func TestPlanAtCapKeepsOlderIDWhenSupersedeCouldLoseTheAuthor(t *testing.T) {
	h := newHarness()
	fillers := bulkComments(workersvc.ReviewPendingCap-1, 101) // ids 101..10099
	comments := append([]forge.MRComment{inline(100, flakyID, "xavier", "x old", raT0)}, fillers...)
	comments = append(comments,
		inline(15000, flakyID+1, "yolanda", "outsider", raT0.Add(time.Hour)),
		inline(20000, flakyID, "xavier", "x new", raT0.Add(time.Hour+time.Second)),
		inline(30000, coderabbit, "coderabbitai[bot]", "bot finding", raT0.Add(2*time.Hour)), // allowlisted: eligible without a lookup, so it moves the mark
	)
	pending := []int64{100}
	for _, c := range fillers {
		pending = append(pending, c.ID)
	}
	p := h.params(comments...)
	p.Trusted = []settings.TrustedBot{{BaseURL: "https://github.com", ForgeUserID: coderabbit}}
	res := h.snapshot(t, p)
	plan := res.Plan(99, pending)
	if slices.Contains(plan.PendingRemove, 100) || slices.Contains(plan.PendingSuperseded, 100) {
		t.Fatalf("X's 100 is removed or superseded although the merged set cannot be guaranteed to fit")
	}
	if got := applyDelta(pending, plan); !slices.Contains(got, 100) || len(got) != workersvc.ReviewPendingCap {
		t.Fatalf("after the capped merge X has no pending id (contains 100: %t, size %d)", slices.Contains(got, 100), len(got))
	}
}

// The cap warning counts only genuinely new ids: a full set with nothing new must not warn.
func TestPlanCapWarningIgnoresAlreadyPendingIDs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness()
	comments := bulkComments(workersvc.ReviewPendingCap, 101)
	pending := make([]int64, 0, len(comments))
	for _, c := range comments {
		pending = append(pending, c.ID)
	}
	res := h.snapshot(t, h.params(comments...))
	plan := res.Plan(0, pending)
	if len(plan.PendingAdd) != workersvc.ReviewPendingCap {
		t.Fatalf("pending add = %d ids, want every pending id represented", len(plan.PendingAdd))
	}
	if buf.Len() != 0 {
		t.Fatalf("warned although nothing new is added: %s", buf.String())
	}
}

// The merge subtracts removed ids before it caps, so superseding an author's older id is safe
// whenever the post-merge set fits: 9,999 pending plus two new representatives is 10,001, and
// superseding A's 100 brings it to exactly the cap, keeping both new ids.
func TestPlanSupersedesWhenPostMergeSetFitsTheCap(t *testing.T) {
	h := newHarness()
	fillers := bulkComments(workersvc.ReviewPendingCap-2, 101) // ids 101..10098
	comments := append([]forge.MRComment{inline(100, flakyID, "xavier", "a old", raT0)}, fillers...)
	comments = append(comments,
		inline(15000, flakyID, "xavier", "a new", raT0.Add(time.Hour)),
		inline(20000, flakyID+1, "yolanda", "b new", raT0.Add(time.Hour+time.Second)),
		inline(30000, coderabbit, "coderabbitai[bot]", "bot finding", raT0.Add(2*time.Hour)),
	)
	pending := []int64{100}
	for _, c := range fillers {
		pending = append(pending, c.ID)
	}
	p := h.params(comments...)
	p.Trusted = []settings.TrustedBot{{BaseURL: "https://github.com", ForgeUserID: coderabbit}}
	res := h.snapshot(t, p)
	plan := res.Plan(99, pending)
	if !slices.Contains(plan.PendingSuperseded, 100) {
		t.Fatalf("superseded = %v, want A's older 100 superseded", plan.PendingSuperseded)
	}
	got := applyDelta(pending, plan)
	if len(got) != workersvc.ReviewPendingCap || !slices.Contains(got, 15000) || !slices.Contains(got, 20000) {
		t.Fatalf("after the merge size %d, contains 15000: %t, 20000: %t; want exactly the cap with both new ids",
			len(got), slices.Contains(got, 15000), slices.Contains(got, 20000))
	}
}

// The cap warning does fire when genuinely new ids push the set over the cap.
func TestPlanCapWarningFiresWhenNewIDsExceedTheCap(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness()
	fillers := bulkComments(workersvc.ReviewPendingCap, 101)
	comments := append([]forge.MRComment{}, fillers...)
	comments = append(comments,
		inline(15000, flakyID+1, "yolanda", "new author", raT0.Add(time.Hour)),
		inline(30000, coderabbit, "coderabbitai[bot]", "bot finding", raT0.Add(2*time.Hour)),
	)
	pending := make([]int64, 0, len(fillers))
	for _, c := range fillers {
		pending = append(pending, c.ID)
	}
	p := h.params(comments...)
	p.Trusted = []settings.TrustedBot{{BaseURL: "https://github.com", ForgeUserID: coderabbit}}
	res := h.snapshot(t, p)
	plan := res.Plan(99, pending)
	if !slices.Contains(plan.PendingAdd, 15000) {
		t.Fatalf("pending add lacks the new author's 15000: setup is not exercising a new add")
	}
	if !bytes.Contains(buf.Bytes(), []byte("would exceed its cap")) {
		t.Fatalf("no cap warning although a new id overflows the set; log: %q", buf.String())
	}
}
