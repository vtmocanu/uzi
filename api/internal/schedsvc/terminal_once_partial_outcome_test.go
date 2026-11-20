package schedsvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type partialGateCancelRuns struct {
	*fakeRuns
	cancel context.CancelFunc
}

func (r *partialGateCancelRuns) CreateScheduledAutopilotRun(ctx context.Context, user, repo uuid.UUID, iid int64, description string, wait *bool, rework *bool, model *string, subagent bool, credential *workersvc.CredentialOverride, harness *workersvc.Harness) (store.Run, error) {
	if iid == 97 {
		r.cancel()
		return store.Run{}, workersvc.ErrTerminalConsumerGateBusy
	}
	return r.fakeRuns.CreateScheduledAutopilotRun(ctx, user, repo, iid, description, wait, rework, model, subagent, credential, harness)
}

type partialGateAdvanceStore struct {
	*fakeStore
	bounded bool
}

func (s *partialGateAdvanceStore) AdvanceSchedule(ctx context.Context, arg store.AdvanceScheduleParams) (store.RunSchedule, error) {
	if err := ctx.Err(); err != nil {
		return store.RunSchedule{}, err
	}
	deadline, ok := ctx.Deadline()
	s.bounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 6*time.Second
	return s.fakeStore.AdvanceSchedule(ctx, arg)
}

func TestPartialSweepGateBusyAfterCancellationRecordsOutcome(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}, {ForgeIssueIid: 97}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &partialGateAdvanceStore{fakeStore: h.st}
	h.sched.store = st
	h.sched.runs = &partialGateCancelRuns{fakeRuns: h.runs, cancel: cancel}
	s := h.sweepSchedule(pgtype.Int4{Int32: 2, Valid: true})
	s.Timing, s.CronExpr = "once", pgtype.Text{}
	h.st.due = []store.RunSchedule{s}
	h.sched.Boot(ctx)
	if ctx.Err() == nil || len(h.runs.autopilot) != 1 {
		t.Fatal("the gate refusal must cancel the context after candidate 96 starts")
	}
	if len(h.st.advanceCalls) != 1 || !st.bounded {
		t.Fatalf("partial outcome lost on cancellation: advances=%+v bounded=%v", h.st.advanceCalls, st.bounded)
	}
	var out lastFireRecord
	if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &out); err != nil {
		t.Fatal(err)
	}
	if h.st.advanceCalls[0].Status != "fired" || len(out.Skips) != 1 || out.Skips[0].Reason != "consumer_gate_busy" {
		t.Fatalf("partial outcome after cancellation = %+v", out)
	}
}

func TestOnceSweepGateBusyBeforeAnyStartRetries(t *testing.T) {
	for _, cap := range []int32{1, 2} {
		h := newHarness()
		h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}, {ForgeIssueIid: 97}}
		h.runs.errByIssue = map[int64]error{96: workersvc.ErrTerminalConsumerGateBusy}
		s := h.sweepSchedule(pgtype.Int4{Int32: cap, Valid: true})
		s.Timing, s.CronExpr = "once", pgtype.Text{}
		h.st.due = []store.RunSchedule{s}
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 0 || len(h.runs.autopilot) != 0 {
			t.Fatalf("cap %d: busy first candidate consumed or created work", cap)
		}
		h.runs.errByIssue = nil
		h.sched.Boot(context.Background())
		if len(h.runs.autopilot) != int(cap) {
			t.Fatalf("cap %d: retry started %d candidates", cap, len(h.runs.autopilot))
		}
		for i, call := range h.runs.autopilot {
			if call.issueIID != int64(96+i) {
				t.Fatalf("cap %d: retry candidate %d = %d", cap, i, call.issueIID)
			}
		}
		if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" || h.st.advanceCalls[0].NextFireAt.Valid {
			t.Fatalf("cap %d: retry advancement = %+v", cap, h.st.advanceCalls)
		}
	}
}

func TestPartialSweepGateBusyRecordsVisibleSkip(t *testing.T) {
	for _, timing := range []string{"once", "recurring"} {
		t.Run(timing, func(t *testing.T) {
			h := newHarness()
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{
				{ForgeIssueIid: 96}, {ForgeIssueIid: 97}, {ForgeIssueIid: 98},
			}
			h.runs.errByIssue = map[int64]error{97: workersvc.ErrTerminalConsumerGateBusy}
			s := h.sweepSchedule(pgtype.Int4{Int32: 2, Valid: true})
			s.Timing = timing
			if timing == "once" {
				s.CronExpr = pgtype.Text{}
			}
			h.st.due = []store.RunSchedule{s}
			h.sched.Boot(context.Background())
			if len(h.runs.autopilot) != 1 || h.runs.autopilot[0].issueIID != 96 {
				t.Fatalf("created %+v, want 96 once with no later candidate", h.runs.autopilot)
			}
			if len(h.st.advanceCalls) != 1 {
				t.Fatalf("partial fire advancement = %+v", h.st.advanceCalls)
			}
			advance := h.st.advanceCalls[0]
			wantStatus := "active"
			if timing == "once" {
				wantStatus = "fired"
			}
			if advance.Status != wantStatus || advance.NextFireAt.Valid != (timing == "recurring") {
				t.Fatalf("partial fire status=%s next=%v", advance.Status, advance.NextFireAt)
			}
			var out lastFireRecord
			if err := json.Unmarshal(advance.LastFire, &out); err != nil {
				t.Fatal(err)
			}
			if out.Matched != 2 || len(out.Started) != 1 || len(out.Skips) != 1 ||
				out.Started[0].IssueIID == nil || *out.Started[0].IssueIID != 96 ||
				out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 97 ||
				out.Skips[0].Reason != "consumer_gate_busy" {
				t.Fatalf("partial outcome = %+v, want started 96 and visible consumer_gate_busy skip for 97", out)
			}
		})
	}
}
