package poller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A pending id whose author has recovered to eligible, but whose comment the snapshot caps
// evict, must not wedge the lane: HasTrigger (uncapped) is true while the capped plan has
// nothing new, so without an explicit removal the id stays pending forever and every tick
// repeats the full assessment. The spec: a pending id the caps evict is removed from the
// pending set and falls back to human review.
func TestMRReworkEvictedPendingIDIsRemovedAndNextTickDoesNoAssessment(t *testing.T) {
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers: map[string]store.MrReworkLedger{
			mrwRef: {RepoID: mrwRepoID, Ref: mrwRef, HighWater: 200, PendingUnknownIds: []int64{50}},
		},
	}
	runs := &mrwRuns{}
	base := time.Now().Add(-time.Hour)
	big := strings.Repeat("x", 4096)
	comments := []forge.MRComment{mrwComment(50, base, mrwHeadSHA)}
	for id := int64(101); id <= 110; id++ { // ten 4 KiB member comments: 40 KiB > the 32 KiB cap
		c := mrwComment(id, base.Add(time.Duration(id)*time.Second), mrwHeadSHA)
		c.Body = big
		comments = append(comments, c)
	}
	f := landedForge(comments...)
	d := newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5})

	d.detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("tick 1 created %d runs; nothing in the capped snapshot is new", len(runs.calls))
	}
	if got := st.ledgers[mrwRef].PendingUnknownIds; slices.Contains(got, 50) {
		t.Fatalf("pending after tick 1 = %v, want the evicted id 50 removed", got)
	}
	if got := st.ledgers[mrwRef]; got.AttemptCount != 0 || got.HighWater != 200 {
		t.Fatalf("a pending-only removal must not touch attempt_count or high_water: %+v", got)
	}
	lookupsAfterTick1 := len(f.lookups)
	if lookupsAfterTick1 == 0 {
		t.Fatal("tick 1 should have assessed the pending id's author")
	}

	d.detect(context.Background(), mrwRepoRow(), f)
	if n := len(f.lookups); n != lookupsAfterTick1 {
		t.Fatalf("tick 2 made %d more lookups; the removed id must not be assessed again", n-lookupsAfterTick1)
	}
	if len(runs.calls) != 0 {
		t.Fatalf("tick 2 created %d runs", len(runs.calls))
	}
}
