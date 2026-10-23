package schedsvc

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type m2OverflowForge struct {
	*fakeForge
	firstErr error
}

func (f *m2OverflowForge) GetIssue(ctx context.Context, p, i int64) (forge.Issue, error) {
	if i == 7 {
		f.getIID = append(f.getIID, i)
		return forge.Issue{}, f.firstErr
	}
	return f.fakeForge.GetIssue(ctx, p, i)
}

type m2OverflowBuilder struct {
	*fakeBuilder
	driver forge.Forge
}

func (b m2OverflowBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.driver, nil
}

var m2OverflowCases = []struct {
	name      string
	err       error
	permanent bool
}{
	{"ordinary", context.DeadlineExceeded, false},
	{"issue", fmt.Errorf("wrapped: %w", &forge.ResponseTooLargeError{Provenance: forge.ResponseIssueSuccess}), true},
	{"unknown", &forge.ResponseTooLargeError{}, false},
	{"bare", forge.ErrResponseTooLarge, false},
	{"error", &forge.ResponseTooLargeError{Provenance: forge.ResponseError, Outcome: forge.ErrorClassServerError}, false},
	{"prerequisite", &forge.ResponseTooLargeError{Provenance: forge.ResponseSuccess}, false},
}

func TestM2SweepOverflowBackfills(t *testing.T) {
	for _, tc := range m2OverflowCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			source := tc.err
			want := SkipFetchFailed
			if tc.permanent {
				want = SkipDescriptionTooLarge
			}
			f := &m2OverflowForge{fakeForge: h.fb.f, firstErr: source}
			h.sched.forge = m2OverflowBuilder{h.fb, f}
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 7}, {ForgeIssueIid: 8}, {ForgeIssueIid: 9}}
			h.st.sweepCount = 3
			out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 1, Valid: true}))
			if err != nil || out.Matched != 2 || len(out.Started) != 1 || *out.Started[0].IssueIID != 8 || len(out.Skips) != 1 || out.Skips[0].Reason != want || *out.Skips[0].IssueIID != 7 || out.Skips[0].Title != "" || out.Skips[0].WebURL != "" {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if !eqInt64s(f.getIID, []int64{7, 8}) {
				t.Fatalf("fetches=%v", f.getIID)
			}
			assertBalances(t, out)
		})
	}
}

func TestM2IssueOverflowAdvances(t *testing.T) {
	for _, tc := range m2OverflowCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.fb.f.err = tc.err
			h.st.due = []store.RunSchedule{h.issueSchedule()}
			h.sched.Boot(context.Background())
			if !tc.permanent {
				if len(h.st.advanceCalls) != 0 {
					t.Fatal("transient failure advanced")
				}
				return
			}
			if len(h.st.advanceCalls) != 1 {
				t.Fatal("overflow did not advance")
			}
			out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
			if err != nil || out.Matched != 1 || len(out.Started) != 0 || len(out.Skips) != 1 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			skip := out.Skips[0]
			if skip.Reason != SkipDescriptionTooLarge || skip.IssueIID == nil || *skip.IssueIID != 7 || skip.Title != "" || skip.WebURL != "" {
				t.Fatalf("skip=%+v", skip)
			}
		})
	}
}
