package workersvc_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #3422: a manual rework (ReviewAssessParams.RecheckNotEligible) asks the forge again
// about an author the six-hour not-eligible cache holds, through the same lookup, queue,
// attempt cap and deadline. Automatic assessments keep the cache.

// cacheOutsider runs one automatic assessment that stores outsiderID as not eligible, then
// moves the clock one hour on, well inside the TTL.
func cacheOutsider(t *testing.T, h *harness) workersvc.ReviewAssessParams {
	t.Helper()
	p := h.params(inline(1, outsiderID, "reviewer", "please rename this", raT0))
	h.snapshot(t, p)
	if v := h.st.Verdicts(raRepo); !slices.Equal(v, []int64{outsiderID}) {
		t.Fatalf("verdicts = %v, want the outsider cached as not eligible", v)
	}
	h.clock = h.clock.Add(time.Hour)
	return p
}

func TestManualRecheckAdmitsACachedNotEligibleAuthorWhoGotAccess(t *testing.T) {
	h := newHarness()
	p := cacheOutsider(t, h)
	h.lookup.answers[outsiderID] = forge.AuthorEligible // the owner added the reviewer

	p.RecheckNotEligible = true
	res := h.snapshot(t, p)
	if got := h.lookup.looked(); !slices.Equal(got, []int64{outsiderID, outsiderID}) {
		t.Fatalf("lookups = %v, want the cached author asked again", got)
	}
	if got := bodies(res); !slices.Equal(got, []string{"please rename this"}) {
		t.Fatalf("bodies = %v, want the re-checked author's comment", got)
	}
	if res.Snapshot.WithheldNotEligible != 0 || res.Snapshot.WithheldUnknown != 0 {
		t.Fatalf("snapshot = %+v, want nothing withheld", res.Snapshot)
	}
	if plan := res.Plan(raState(0)); !plan.HasNew {
		t.Fatalf("plan = %+v, want the comment to count as new", plan)
	}
	// The repo-wide cache is not cleared: the stored verdict stays for automatic callers.
	if v := h.st.Verdicts(raRepo); !slices.Equal(v, []int64{outsiderID}) {
		t.Fatalf("verdicts = %v, want the stored verdict left in place", v)
	}
}

func TestAutomaticAssessmentKeepsTheNotEligibleCache(t *testing.T) {
	h := newHarness()
	p := cacheOutsider(t, h)
	h.lookup.answers[outsiderID] = forge.AuthorEligible

	res := h.snapshot(t, p) // RecheckNotEligible left false, as the watcher leaves it
	if got := h.lookup.looked(); !slices.Equal(got, []int64{outsiderID}) {
		t.Fatalf("lookups = %v, want no lookup inside the TTL", got)
	}
	if len(res.Snapshot.Comments) != 0 || res.Snapshot.WithheldNotEligible != 1 {
		t.Fatalf("snapshot = %+v, want the cached author withheld as not eligible", res.Snapshot)
	}
}

func TestManualRecheckKeepsAnUnverifiableAuthorWithheld(t *testing.T) {
	h := newHarness()
	p := cacheOutsider(t, h)
	delete(h.lookup.answers, outsiderID) // the lookup now errors: access cannot be verified

	p.RecheckNotEligible = true
	res := h.snapshot(t, p)
	if got := h.lookup.looked(); !slices.Equal(got, []int64{outsiderID, outsiderID}) {
		t.Fatalf("lookups = %v, want the cached author asked again", got)
	}
	if len(res.Snapshot.Comments) != 0 {
		t.Fatalf("an unverifiable author's comment reached the snapshot: %+v", res.Snapshot.Comments)
	}
	if res.Snapshot.WithheldUnknown != 1 {
		t.Fatalf("snapshot = %+v, want the author withheld as permission unknown", res.Snapshot)
	}
}

func TestManualRecheckStillNotEligibleStaysWithheldAndIsRecorded(t *testing.T) {
	h := newHarness()
	p := cacheOutsider(t, h) // the lookup still answers not eligible

	p.RecheckNotEligible = true
	res := h.snapshot(t, p)
	if len(res.Snapshot.Comments) != 0 || res.Snapshot.WithheldNotEligible != 1 {
		t.Fatalf("snapshot = %+v, want the author withheld as not eligible", res.Snapshot)
	}
	if v := h.st.Verdicts(raRepo); !slices.Equal(v, []int64{outsiderID}) {
		t.Fatalf("verdicts = %v, want the not-eligible answer stored", v)
	}
}

func TestManualRecheckKeepsTheAttemptCapAndTheQueueOrder(t *testing.T) {
	h := newHarness()
	const a, b, c = int64(51), int64(52), int64(53)
	for _, id := range []int64{a, b, c} {
		h.lookup.answers[id] = forge.AuthorNotEligible
	}
	p := h.params(
		inline(1, a, "a", "x", raT0),
		inline(2, b, "b", "y", raT0.Add(time.Second)),
		inline(3, c, "c", "z", raT0.Add(2*time.Second)),
	)
	h.snapshot(t, p)
	if v := h.st.Verdicts(raRepo); len(v) != 3 {
		t.Fatalf("verdicts = %v, want all three cached", v)
	}
	h.clock = h.clock.Add(time.Hour)
	before := len(h.lookup.looked())

	assessor := h.assessor()
	assessor.MaxAttempts = 1
	p.RecheckNotEligible = true
	as, err := assessor.Begin(context.Background(), p)
	if err != nil || as == nil {
		t.Fatalf("Begin = %v, %v", as, err)
	}
	defer as.Close()
	if as.Attempted != 1 {
		t.Fatalf("Attempted = %d, want the attempt cap of 1 to hold on the manual path", as.Attempted)
	}
	if got := h.lookup.looked()[before:]; !slices.Equal(got, []int64{a}) {
		t.Fatalf("Begin lookups = %v, want only the head of the queue", got)
	}
	if q := h.st.Order(raRepo, "agent/issue-7"); !slices.Equal(q, []int64{b, c, a}) {
		t.Fatalf("queue = %v, want the attempted author moved to the back", q)
	}
}

func TestManualRecheckKeepsThePerLookupDeadline(t *testing.T) {
	h := newHarness()
	p := cacheOutsider(t, h)
	h.timeout = 50 * time.Millisecond
	h.lookup.hang[outsiderID] = true

	p.Comments = append(p.Comments, inline(2, memberID, "carol", "member", raT0.Add(time.Second)))
	p.RecheckNotEligible = true
	res := h.snapshot(t, p)
	h.lookup.mu.Lock()
	lookupErr := h.lookup.canceled[outsiderID]
	h.lookup.mu.Unlock()
	if !errors.Is(lookupErr, context.DeadlineExceeded) {
		t.Fatalf("hanging re-check = %v, want DeadlineExceeded", lookupErr)
	}
	if got := bodies(res); !slices.Equal(got, []string{"member"}) {
		t.Fatalf("bodies = %v, want only the member's comment", got)
	}
	if res.Snapshot.WithheldUnknown != 1 {
		t.Fatalf("snapshot = %+v, want the timed-out author withheld as unknown", res.Snapshot)
	}
}
