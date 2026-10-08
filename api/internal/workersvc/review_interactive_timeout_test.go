package workersvc_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/reviewauthortest"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type deadlineReviewLookup struct {
	*lookupFake
	t         *testing.T
	start     time.Time
	deadlines []time.Duration
}

func (f *deadlineReviewLookup) RepositoryAuthorEligibility(ctx context.Context, project, id int64) (forge.AuthorEligibility, error) {
	deadline, ok := ctx.Deadline()
	index := len(f.looked())
	if !ok || index >= len(f.deadlines) || deadline != f.start.Add(f.deadlines[index]) {
		f.t.Fatalf("lookup %d deadline=%v expected offsets=%v", id, deadline, f.deadlines)
	}
	return f.lookupFake.RepositoryAuthorEligibility(ctx, project, id)
}

type parentReviewStore struct {
	*reviewauthortest.Store
	t                   *testing.T
	parent              context.Context
	mutations, verdicts int
}

func (s *parentReviewStore) check(ctx context.Context) {
	s.t.Helper()
	if ctx != s.parent || ctx.Err() != nil {
		s.t.Fatalf("write lost original live parent: %v", ctx.Err())
	}
}
func (s *parentReviewStore) MutateReviewAuthorQueue(ctx context.Context, repo uuid.UUID, ref string, fn func(workersvc.ReviewAuthorQueueOps) error) error {
	s.check(ctx)
	s.mutations++
	return s.Store.MutateReviewAuthorQueue(ctx, repo, ref, fn)
}
func (s *parentReviewStore) UpsertReviewAuthorVerdict(ctx context.Context, p store.UpsertReviewAuthorVerdictParams) error {
	s.check(ctx)
	s.verdicts++
	return s.Store.UpsertReviewAuthorVerdict(ctx, p)
}

func TestReviewInteractiveSharedTotalDeadline(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		name := "default"
		if interactive {
			name = "interactive"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				parent := context.Background()
				st := &parentReviewStore{Store: reviewauthortest.New(time.Now), t: t, parent: parent}
				f := &deadlineReviewLookup{lookupFake: newLookup(), t: t, start: start}
				f.answers[44] = forge.AuthorEligible
				f.hang[51], f.hang[52], f.hang[53] = true, true, true
				// Begin: eligible, not eligible, one hang. Snapshot: two more hangs, then eligible.
				f.deadlines = []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second}
				if interactive {
					f.deadlines = []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}
				}
				p := workersvc.ReviewAssessParams{
					RepoID: uuid.New(), Ref: "agent/issue-7", ProjectID: 42, BotForgeUserID: botID, Lookup: f, HighWater: 10,
					Comments: []forge.MRComment{
						inline(11, memberID, "carol", "eligible trigger", start),
						inline(12, outsiderID, "mallory", "OUTSIDER_RAW", start.Add(time.Second)),
						inline(13, 51, "hang1", "UNKNOWN_RAW_1", start.Add(2*time.Second)),
						inline(1, 52, "hang2", "UNKNOWN_RAW_2", start.Add(3*time.Second)),
						inline(2, 53, "hang3", "UNKNOWN_RAW_3", start.Add(4*time.Second)),
						inline(3, 44, "later", "later eligible", start.Add(5*time.Second)),
					},
				}
				if interactive {
					p.AssessmentTimeout = issueinput.InteractiveAssessmentTimeout
				}
				a := &workersvc.ReviewAssessor{Store: st, Queue: st, Timeout: 2 * time.Second, Now: time.Now}
				as, err := a.Begin(parent, p)
				if err != nil || as == nil {
					t.Fatal(as, err)
				}
				defer as.Close()
				if time.Now() != start.Add(2*time.Second) || as.Attempted != 3 || !as.HasTrigger() {
					t.Fatalf("Begin elapsed=%v attempted=%d trigger=%v", time.Since(start), as.Attempted, as.HasTrigger())
				}
				res := as.Snapshot(parent)
				elapsed := 6 * time.Second
				wantBodies := []string{"eligible trigger", "later eligible"}
				wantCalls := []int64{memberID, outsiderID, 51, 52, 53, 44}
				unknown := 3
				attempted := 3
				if interactive {
					elapsed = 5 * time.Second
					wantBodies = []string{"eligible trigger"}
					wantCalls = []int64{memberID, outsiderID, 51, 52, 53}
					unknown = 4
					attempted = 2
				}
				if time.Now() != start.Add(elapsed) || !slices.Equal(bodies(res), wantBodies) || !slices.Equal(f.looked(), wantCalls) || res.ContextAttempted != attempted || res.Snapshot.WithheldUnknown != unknown || res.Snapshot.WithheldNotEligible != 1 {
					t.Fatalf("elapsed=%v bodies=%v calls=%v result=%+v snapshot=%+v", time.Since(start), bodies(res), f.looked(), res, res.Snapshot)
				}
				if !as.HasTrigger() || parent.Err() != nil || st.mutations != 2 || st.verdicts != 1 || !slices.Equal(st.Verdicts(p.RepoID), []int64{outsiderID}) {
					t.Fatalf("parent writes mutations=%d verdicts=%d", st.mutations, st.verdicts)
				}
			})
		})
	}
}

func TestReviewTotalDeadlineUnknownNeverTriggers(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		name := "default30"
		total := 30 * time.Second
		if interactive {
			name = "interactive5"
			total = 5 * time.Second
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				parent := context.Background()
				st := &parentReviewStore{Store: reviewauthortest.New(time.Now), t: t, parent: parent}
				f := &deadlineReviewLookup{lookupFake: newLookup(), t: t, start: start, deadlines: []time.Duration{total}}
				f.hang[flakyID] = true
				p := workersvc.ReviewAssessParams{RepoID: uuid.New(), Ref: "agent/issue-7", ProjectID: 42, BotForgeUserID: botID, Lookup: f, Comments: []forge.MRComment{inline(1, flakyID, "flaky", "UNKNOWN_RAW", start)}}
				if interactive {
					p.AssessmentTimeout = issueinput.InteractiveAssessmentTimeout
				}
				// A long individual limit proves the default total is thirty, not five.
				a := &workersvc.ReviewAssessor{Store: st, Queue: st, Timeout: time.Minute, Now: time.Now}
				as, err := a.Begin(parent, p)
				if err != nil || as == nil {
					t.Fatal(as, err)
				}
				defer as.Close()
				res := as.Snapshot(parent)
				if time.Now() != start.Add(total) || as.HasTrigger() || len(bodies(res)) != 0 || res.Snapshot.WithheldUnknown != 1 || res.Snapshot.WithheldNotEligible != 0 || res.PlanAssessed().HasNew {
					t.Fatalf("elapsed=%v trigger=%v snapshot=%+v", time.Since(start), as.HasTrigger(), res.Snapshot)
				}
				if st.mutations != 2 || parent.Err() != nil {
					t.Fatalf("parent writes=%d err=%v", st.mutations, parent.Err())
				}
			})
		})
	}
}
