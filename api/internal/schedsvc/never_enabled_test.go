package schedsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD 4173 D13: a schedule on a repo that has never been enabled fires nothing (issue, sweep,
// prompt and self_improve targets). The
// refusal comes before any forge driver is built: the builder fails the build with a
// different error, so a build would show as the wrong error, and the forge fake's call
// counters stay at zero. advance treats the error as transient (retried, never parked).
func TestFireOnNeverEnabledRepoMakesNoForgeCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sched func(h *harness) store.RunSchedule
	}{
		{"issue", func(h *harness) store.RunSchedule { return h.issueSchedule() }},
		{"sweep", func(h *harness) store.RunSchedule { return h.sweepSchedule(pgtype.Int4{}) }},
		{"prompt", func(h *harness) store.RunSchedule {
			s := h.issueSchedule()
			s.Target = "prompt"
			s.IssueIid = pgtype.Int8{}
			s.Prompt = pgtype.Text{String: "do a thing", Valid: true}
			return s
		}},
		{"self_improve", func(h *harness) store.RunSchedule { return h.selfImproveSchedule() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.st.repoRow.AwaitingFirstEnable = true
			h.fb.err = errors.New("the forge driver must not be built for a never-enabled repo")
			sched := tc.sched(h)

			_, err := h.sched.fireOne(context.Background(), sched)
			if !errors.Is(err, workersvc.ErrRepoNeverEnabled) {
				t.Fatalf("fireOne error = %v, want ErrRepoNeverEnabled", err)
			}
			f := h.fb.f
			if len(f.getIID) != 0 || f.listCount != 0 || f.createCount != 0 || len(f.getMRIID) != 0 {
				t.Errorf("forge calls: GetIssue=%v ListIssues=%d CreateIssue=%d GetMergeRequest=%v; want none",
					f.getIID, f.listCount, f.createCount, f.getMRIID)
			}
			if h.runsCreated() != 0 || len(h.runs.selfImprove) != 0 {
				t.Errorf("runs created = %d, self-improve runs = %d; want none", h.runsCreated(), len(h.runs.selfImprove))
			}

			// The advance rule: transient, so the schedule is not parked and not advanced.
			h.sched.advance(context.Background(), sched, FireOutcome{}, err)
			if len(h.st.statusCalls) != 0 {
				t.Errorf("schedule status changed (%+v); a never-enabled repo must stay transient", h.st.statusCalls)
			}
		})
	}
}
