package schedsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestTickOnceTerminalConsumerGateBusyIsNotConsumed pins issue #4488: a brief terminal-ref
// retirement hold on the repo's consumer gate refuses a scheduled create with
// ErrTerminalConsumerGateBusy (nothing created). Before any start, a one-time schedule stays un-advanced and
// un-parked on every scheduled create path, then fire normally once the gate frees. For the
// sweep the busy refusal must not be recorded as a consumed fetch_failed skip.
func TestTickOnceTerminalConsumerGateBusyIsNotConsumed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sched   func(h *harness) store.RunSchedule
		created func(h *harness) int
	}{
		{"issue", func(h *harness) store.RunSchedule { return h.issueSchedule() },
			func(h *harness) int { return len(h.runs.autopilot) }},
		{"prompt", func(h *harness) store.RunSchedule { return h.userPromptSchedule() },
			func(h *harness) int { return len(h.runs.prompts) }},
		{"sweep", func(h *harness) store.RunSchedule {
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
			return h.sweepSchedule(pgtype.Int4{})
		}, func(h *harness) int { return len(h.runs.autopilot) + len(h.runs.runs) }},
		{"self_improve", func(h *harness) store.RunSchedule { return h.selfImproveSchedule() },
			func(h *harness) int { return len(h.runs.selfImprove) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			s := tc.sched(h)
			s.Timing = "once"
			s.CronExpr = pgtype.Text{} // once carries run_at, not cron
			h.st.due = []store.RunSchedule{s}
			h.runs.err = errors.Join(workersvc.ErrTerminalConsumerGateBusy, errors.New("lock timeout"))

			h.sched.Boot(context.Background())

			if len(h.st.advanceCalls) != 0 {
				t.Fatalf("a busy gate must not advance a once schedule: advance calls = %d (last_fire %s)",
					len(h.st.advanceCalls), h.st.advanceCalls[0].LastFire)
			}
			if len(h.st.statusCalls) != 0 {
				t.Fatalf("a busy gate must not park a once schedule: status calls = %d", len(h.st.statusCalls))
			}
			if n := tc.created(h); n != 0 {
				t.Fatalf("no run may be created while the gate is busy: created = %d", n)
			}

			h.runs.err = nil // the retirement hold ended; the row is still due
			h.sched.Boot(context.Background())

			if n := tc.created(h); n != 1 {
				t.Fatalf("retry tick: created = %d, want 1", n)
			}
			if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
				t.Fatalf("retry tick must advance the once row to fired, advance calls = %+v", h.st.advanceCalls)
			}
		})
	}
}

// The global refusal map stays unchanged: a sweep may record the visible partial-fire
// exception only at its own site, while a fire with no starts remains retryable.
func TestSkipReasonForErrDoesNotMapTerminalConsumerGate(t *testing.T) {
	for _, err := range []error{workersvc.ErrTerminalConsumerGateBusy, workersvc.ErrTerminalConsumerGateIdentity} {
		if r, ok := skipReasonForErr(err); ok {
			t.Fatalf("skipReasonForErr(%v) = %q, want unmapped", err, r)
		}
	}
}

// A recurring sweep that started nothing returns the transient error too, so it retries instead
// of recording fetch_failed.
func TestRecurringSweepTerminalConsumerGateBusyRecordsNoFetchFailed(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
	h.runs.err = workersvc.ErrTerminalConsumerGateBusy
	out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
	if !errors.Is(err, workersvc.ErrTerminalConsumerGateBusy) {
		t.Fatalf("RunNow err = %v, want the busy error returned", err)
	}
	if len(out.Skips) != 0 || len(out.Started) != 0 {
		t.Fatalf("outcome = %+v, want nothing recorded", out)
	}
}
