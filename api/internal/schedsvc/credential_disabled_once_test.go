package schedsvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// credential_disabled_once_test.go pins PRD #1732 D2 for one-time schedules: pinned work waits.
// A once row whose stored pin (or pinned harness) is disabled is held un-advanced, like the
// issue #1626 ErrBranchInUse exception, instead of being consumed by the benign
// credential_disabled skip; a recurring row keeps advancing with that skip.

func onceOf(s store.RunSchedule) store.RunSchedule {
	s.Timing = "once"
	s.CronExpr = pgtype.Text{} // once carries run_at, not cron
	return s
}

func (h *harness) userPromptSchedule() store.RunSchedule {
	return store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "prompt",
		Origin:      "user",
		Prompt:      pgtype.Text{String: "do the thing", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}
}

// TestTickOnceCredentialDisabledHoldsThenFires: a one-time issue, prompt or sweep schedule
// refused for a disabled pin (or a pinned harness with no enabled credential) neither advances
// nor parks, creates no run, and fires on a later tick once the credential is back.
//
// MUTATION: drop the holdsOnceCredentialDisabled checks in createIssueRun / fireSweep /
// firePrompt; the once row then takes the benign advancing skip, is marked fired, and this
// test fails.
func TestTickOnceCredentialDisabledHoldsThenFires(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sched func(h *harness) store.RunSchedule
		err   error
		fired func(h *harness) int
	}{
		{"issue_disabled_pin", func(h *harness) store.RunSchedule { return h.issueSchedule() },
			workersvc.ErrCredentialDisabled, func(h *harness) int { return len(h.runs.autopilot) }},
		{"issue_disabled_harness", func(h *harness) store.RunSchedule { return h.issueSchedule() },
			workersvc.ErrHarnessCredentialDisabled, func(h *harness) int { return len(h.runs.autopilot) }},
		{"prompt_disabled_pin", func(h *harness) store.RunSchedule { return h.userPromptSchedule() },
			workersvc.ErrCredentialDisabled, func(h *harness) int { return len(h.runs.prompts) }},
		{"sweep_disabled_pin", func(h *harness) store.RunSchedule {
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
			return h.sweepSchedule(pgtype.Int4{})
		}, workersvc.ErrCredentialDisabled, func(h *harness) int { return len(h.runs.autopilot) + len(h.runs.runs) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.st.due = []store.RunSchedule{onceOf(tc.sched(h))}
			h.runs.err = tc.err

			h.sched.Boot(context.Background())

			if len(h.st.advanceCalls) != 0 {
				t.Fatalf("a disabled pin on a once row must NOT advance: advance calls = %d, want 0", len(h.st.advanceCalls))
			}
			if len(h.st.statusCalls) != 0 {
				t.Fatalf("a disabled pin on a once row must NOT park: status calls = %d, want 0", len(h.st.statusCalls))
			}
			if n := tc.fired(h); n != 0 {
				t.Fatalf("runs created while the pin is disabled = %d, want 0", n)
			}

			// The owner enables the credential: the row is still due and fires next tick.
			h.runs.err = nil
			h.sched.Boot(context.Background())
			if n := tc.fired(h); n != 1 {
				t.Fatalf("once retry: runs created = %d, want 1", n)
			}
			if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" || h.st.advanceCalls[0].NextFireAt.Valid {
				t.Fatalf("advance calls = %+v, want one fired advance with no next fire", h.st.advanceCalls)
			}
		})
	}
}

// TestTickRecurringCredentialDisabledStillAdvances: a recurring row keeps the benign,
// advancing credential_disabled skip; only once rows are held.
func TestTickRecurringCredentialDisabledStillAdvances(t *testing.T) {
	h := newHarness()
	h.st.due = []store.RunSchedule{h.issueSchedule()}
	h.runs.err = workersvc.ErrCredentialDisabled

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "active" {
		t.Fatalf("advance calls = %+v, want one active (recurring) advance", h.st.advanceCalls)
	}
	var lf struct {
		Skips []struct {
			Reason string `json:"reason"`
		} `json:"skips"`
	}
	if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &lf); err != nil {
		t.Fatalf("decode persisted last_fire: %v", err)
	}
	if len(lf.Skips) != 1 || lf.Skips[0].Reason != string(SkipCredentialDisabled) {
		t.Fatalf("last_fire skips = %+v, want one credential_disabled", lf.Skips)
	}
}

// TestRunNowOnceCredentialDisabledSkips: RunNow never advances, so a once row's
// credential_disabled hold answers with the benign skip rather than an error (502).
func TestRunNowOnceCredentialDisabledSkips(t *testing.T) {
	h := newHarness()
	h.runs.err = workersvc.ErrCredentialDisabled
	out, err := h.sched.RunNow(context.Background(), onceOf(h.issueSchedule()))
	if err != nil {
		t.Fatalf("RunNow on a held once issue must not surface an error, got %v", err)
	}
	if out.Matched != 1 || len(out.Started) != 0 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipCredentialDisabled {
		t.Fatalf("outcome = %+v, want one credential_disabled skip", out)
	}
	if out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 7 {
		t.Fatalf("skip IssueIID = %v, want 7", out.Skips[0].IssueIID)
	}
	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("RunNow must NOT advance: advance calls = %d, want 0", len(h.st.advanceCalls))
	}
	assertBalances(t, out)
}
