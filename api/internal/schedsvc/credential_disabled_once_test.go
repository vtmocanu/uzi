package schedsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// credential_disabled_once_test.go pins PRD #1732 D2 for one-time schedules: pinned work waits.
// A once row whose stored pin (or pinned harness) is disabled is held un-advanced, with the
// credential_disabled skip recorded in last_fire, instead of being consumed by that skip; a
// recurring row keeps advancing with it.

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

// heldLastFire decodes a persisted last_fire into its skip reasons and started count.
func heldLastFire(t *testing.T, raw []byte) (started int, reasons []string) {
	t.Helper()
	var lf struct {
		Started []json.RawMessage `json:"started"`
		Skips   []struct {
			Reason string `json:"reason"`
		} `json:"skips"`
	}
	if err := json.Unmarshal(raw, &lf); err != nil {
		t.Fatalf("decode persisted last_fire %q: %v", raw, err)
	}
	for _, sk := range lf.Skips {
		reasons = append(reasons, sk.Reason)
	}
	return len(lf.Started), reasons
}

// captureLogs points the harness scheduler's logger at a buffer so a test can assert what a
// tick logged at Warn and above.
func captureLogs(h *harness) *bytes.Buffer {
	var buf bytes.Buffer
	h.sched.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &buf
}

// forgeCalls totals every fake forge call the fire paths can make.
func forgeCalls(f *fakeForge) int {
	return len(f.getIID) + f.listCount + f.createCount + len(f.getMRIID)
}

// TestTickOnceCredentialDisabledHoldsThenFires: a one-time issue, prompt, sweep or
// self_improve schedule whose stored pin (or pinned harness) is disabled neither advances nor
// parks and creates no run. It records the credential_disabled skip in last_fire once (a
// second held tick does not rewrite it), logs no transient-error warning, and fires on a later
// tick once the credential is back. The refusal is caught two ways: by the pre-fire check
// (then no forge call is made at all) and by the fire's own seam refusal (a disable landing
// after the pre-check), which is held the same way.
//
// MUTATION: drop the pre-fire ScheduleCredentialDisabled check in process(); the precheck cases
// then reach the forge and fail on the forge-call count. Drop the post-fire
// holdsOnceCredentialDisabled arm in process(); the seam cases then take advance's transient
// arm, record no last_fire and log the warning, and fail.
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
		// A user-origin self_improve clone can be edited to a one-time row (its config PATCH
		// accepts timing=once), and its pinned harness is threaded, so it is held the same way.
		{"self_improve_disabled_harness", func(h *harness) store.RunSchedule {
			s := h.selfImproveSchedule()
			s.Origin, s.CatalogSlug = "user", pgtype.Text{}
			s.Harness = pgtype.Text{String: string(workersvc.HarnessClaude), Valid: true}
			return s
		}, workersvc.ErrHarnessCredentialDisabled, func(h *harness) int { return len(h.runs.selfImprove) }},
	} {
		for _, via := range []string{"precheck", "seam"} {
			t.Run(tc.name+"/"+via, func(t *testing.T) {
				h := newHarness()
				logs := captureLogs(h)
				h.st.due = []store.RunSchedule{onceOf(tc.sched(h))}
				if via == "precheck" {
					h.runs.credDisabledErr = tc.err
				} else {
					h.runs.err = tc.err
				}

				for tick := 0; tick < 2; tick++ {
					h.sched.Boot(context.Background())
				}

				if len(h.st.advanceCalls) != 0 {
					t.Fatalf("a disabled pin on a once row must NOT advance: advance calls = %d, want 0", len(h.st.advanceCalls))
				}
				if len(h.st.statusCalls) != 0 {
					t.Fatalf("a disabled pin on a once row must NOT park: status calls = %d, want 0", len(h.st.statusCalls))
				}
				if n := tc.fired(h); n != 0 {
					t.Fatalf("runs created while the pin is disabled = %d, want 0", n)
				}
				if len(h.st.heldFireCalls) != 1 {
					t.Fatalf("held last_fire writes over two held ticks = %d, want 1 (recorded once, no churn)", len(h.st.heldFireCalls))
				}
				if started, reasons := heldLastFire(t, h.st.due[0].LastFire); started != 0 || len(reasons) != 1 || reasons[0] != string(SkipCredentialDisabled) {
					t.Fatalf("held last_fire started=%d reasons=%v, want 0 started and [credential_disabled]", started, reasons)
				}
				if strings.Contains(logs.String(), "transient fire error") {
					t.Fatalf("a held once row must not log the per-tick transient warning; logs:\n%s", logs)
				}
				if via == "precheck" {
					if n := forgeCalls(h.fb.f); n != 0 {
						t.Fatalf("forge calls while held = %d, want 0 (the pre-check runs before any forge call)", n)
					}
					if h.st.sweepSelectorParam != "" || len(h.st.activeSelfImproveRepos) != 0 {
						t.Fatal("a held once row must not reach its fire path")
					}
				}

				// The owner enables the credential: the row is still due and fires next tick.
				h.runs.err, h.runs.credDisabledErr = nil, nil
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
}

// TestTickOnceSweepCredentialDisabledMidFireAdvances (PRD #1732 D2): a one-time sweep whose pin
// is disabled after it already started a run this fire is NOT held. Holding it would re-fire
// the whole sweep once the credential is back and start up to max_issues more runs on top of
// the one already started. It records the credential_disabled skip, stops the fan-out, and is
// marked fired like any completed once fire.
//
// MUTATION: return the hold error from fireSweep regardless of len(out.Started); the row is
// then held, never advanced, and this test fails.
func TestTickOnceSweepCredentialDisabledMidFireAdvances(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3}}
	h.runs.errByIssue = map[int64]error{2: workersvc.ErrCredentialDisabled, 3: workersvc.ErrCredentialDisabled}
	h.st.due = []store.RunSchedule{onceOf(h.sweepSchedule(pgtype.Int4{Int32: 3, Valid: true}))}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 || h.runs.autopilot[0].issueIID != 1 {
		t.Fatalf("runs started = %+v, want only issue 1", h.runs.autopilot)
	}
	if len(h.st.heldFireCalls) != 0 {
		t.Fatalf("a sweep that already started a run must not be held: held writes = %d", len(h.st.heldFireCalls))
	}
	if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
		t.Fatalf("advance calls = %+v, want one fired advance", h.st.advanceCalls)
	}
	started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
	if started != 1 || len(reasons) != 1 || reasons[0] != string(SkipCredentialDisabled) {
		t.Fatalf("last_fire started=%d reasons=%v, want 1 started and one credential_disabled skip (fan-out stopped)", started, reasons)
	}
	if got := h.fb.f.getIID; len(got) != 2 {
		t.Fatalf("GetIssue calls = %v, want 2 (candidate 3 is never examined after the refusal)", got)
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
	if h.runs.credDisabledCalls != 0 || len(h.st.heldFireCalls) != 0 {
		t.Fatalf("a recurring row takes no once hold: pre-checks=%d held writes=%d, want 0", h.runs.credDisabledCalls, len(h.st.heldFireCalls))
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
