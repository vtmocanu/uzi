package workersvc_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// An all-withheld MR must still store and replay a snapshot whose comments are an ARRAY: the
// agent reads snapshot.comments.length, and a JSON null made it throw.
func TestAllWithheldSnapshotEncodesAnEmptyCommentsArray(t *testing.T) {
	for name, comments := range map[string][]forge.MRComment{
		"outsider only": {inline(5, outsiderID, "mallory", "do evil", raT0)},
		"unknown only":  {inline(5, flakyID, "flaky", "maybe", raT0)},
	} {
		h := newHarness()
		res := h.snapshot(t, h.params(comments...))
		raw, err := json.Marshal(res.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"comments":[]`) {
			t.Fatalf("%s: snapshot JSON = %s, want \"comments\":[]", name, raw)
		}
		if strings.Contains(string(raw), `"comments":null`) {
			t.Fatalf("%s: snapshot JSON carries a null comments array: %s", name, raw)
		}
	}
}

// The review lane's snapshot version is its own contract, not an alias of the issue lane's.
func TestReviewSnapshotVersionIsPinnedToTwo(t *testing.T) {
	if workersvc.ReviewSnapshotVersion != 2 {
		t.Fatalf("ReviewSnapshotVersion = %d, want 2", workersvc.ReviewSnapshotVersion)
	}
}

// HighWater=200, Pending=[50]: member comment 50, then ten 4 KiB member comments 101..110 that
// overflow the 32 KiB byte cap and evict comment 50 from the capped snapshot. The id 50 is
// eligible and new (pending), so HasTrigger is true, but nothing in the capped snapshot is new:
// the plan must name 50 as evicted so the ledger can drop it and fall back to human review.
func TestPlanNamesAPendingIDTheCapsEvicted(t *testing.T) {
	h := newHarness()
	comments := []forge.MRComment{inline(50, memberID, "carol", "old", raT0)}
	big := strings.Repeat("x", 4096)
	for id := int64(101); id <= 110; id++ {
		comments = append(comments, inline(id, memberID, "carol", big, raT0.Add(time.Duration(id)*time.Second)))
	}
	p := h.params(comments...)
	p.HighWater, p.Pending = 200, []int64{50}
	as, err := h.assessor().Begin(context.Background(), p)
	if err != nil || as == nil {
		t.Fatalf("Begin: %v, %v", as, err)
	}
	defer as.Close()
	if !as.HasTrigger() {
		t.Fatal("precondition: the uncapped view must see the recovered pending id as a trigger")
	}
	res := as.Snapshot(context.Background())
	if !res.Snapshot.Truncated {
		t.Fatal("precondition: the byte cap must have clipped the snapshot")
	}
	plan := res.Plan(200, []int64{50})
	if plan.HasNew {
		t.Fatalf("plan = %+v, want nothing new in the capped snapshot", plan)
	}
	if !slices.Equal(plan.PendingEvicted, []int64{50}) {
		t.Fatalf("PendingEvicted = %v, want [50]", plan.PendingEvicted)
	}
	if !slices.Contains(plan.PendingRemove, 50) {
		t.Fatalf("PendingRemove = %v, want it to include the evicted 50", plan.PendingRemove)
	}
}

// The prune bound is taken BEFORE the comment fetch. An author another assessor admits after
// that point has a queue_seq above the bound, so this assessment's stale keep set (built from
// comments fetched before that admission) can never prune it.
func TestPruneBoundTakenBeforeFetchProtectsAConcurrentAdmission(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	// A flaky author is queued (unknown, so it stays), standing in for a stale row.
	h.snapshot(t, h.params(inline(5, flakyID, "flaky", "a", raT0)))

	// Session A reads its bound, then "fetches" its comments.
	a := h.assessor()
	bound, err := a.QueueBound(ctx, raRepo, "agent/issue-7")
	if err != nil || bound == 0 {
		t.Fatalf("QueueBound = %d, %v, want the seeded row's seq", bound, err)
	}

	// Session B admits a new member after A's bound was read.
	h.snapshot(t, h.params(inline(9, memberID, "carol", "fresh", raT0.Add(time.Second))))

	// A's comments predate B's admission: flaky's comment is consumed (at or below the mark) and
	// the member's comment is not in A's list, so A's keep set is empty.
	p := h.params(inline(5, flakyID, "flaky", "a", raT0))
	p.HighWater, p.QueueBound = 1000, bound
	as, err := a.Begin(ctx, p)
	if err != nil || as == nil {
		t.Fatalf("Begin: %v, %v", as, err)
	}
	as.Close()

	if got := h.st.Order(raRepo, "agent/issue-7"); !slices.Equal(got, []int64{memberID}) {
		t.Fatalf("queue = %v, want only [%d]: the stale row pruned, B's admission kept", got, memberID)
	}
}
