package schedsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestScheduledFreshHarnessBothBranches(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, label := range []string{"harness::codex", "harness::claude"} {
			t.Run(label+map[bool]string{true: "/auto", false: "/manual"}[auto], func(t *testing.T) {
				h := newHarness()
				h.fb.f.issue.Labels = []string{"uzi", label}
				// The implicit default must not substitute for a fresh explicit label.
				h.runs.resolvedHarness = workersvc.HarnessClaude
				s := h.issueSchedule()
				s.AutoApprove = auto
				out, err := h.sched.RunNow(context.Background(), s)
				if err != nil || len(out.Started) != 1 {
					t.Fatalf("out=%+v err=%v", out, err)
				}
				var explicit *workersvc.Harness
				if auto {
					explicit = h.runs.autopilot[0].explicit
				} else {
					explicit = h.runs.runs[0].explicit
				}
				want := workersvc.HarnessCodex
				if label == "harness::claude" {
					want = workersvc.HarnessClaude
				}
				if explicit == nil || *explicit != want {
					t.Fatalf("explicit=%v want=%s", explicit, want)
				}
				if len(h.st.advanceCalls) != 0 || len(h.st.heldFireCalls) != 0 {
					t.Fatal("RunNow changed cadence")
				}
			})
		}
	}
}

func TestLabelSweepContinuesPerCandidate(t *testing.T) {
	for _, timing := range []string{"once", "recurring"} {
		for _, auto := range []bool{false, true} {
			for _, tc := range []struct {
				name   string
				labels []string
				pin    string
				err    error
				reason SkipReason
			}{
				{"invalid", []string{"harness::other"}, "", nil, SkipReason("harness_label_invalid")},
				{"both", []string{"harness::claude", "harness::codex"}, "", nil, SkipReason("harness_label_invalid")},
				{"conflict", []string{"harness::codex"}, "claude", nil, SkipReason("harness_label_conflict")},
				{"disabled", []string{"harness::codex"}, "", workersvc.ErrHarnessCredentialDisabled, SkipCredentialDisabled},
				{"missing", []string{"harness::codex"}, "", workersvc.ErrNoCredentialForHarness, SkipNoUsableCredential},
				{"unusable", []string{"harness::codex"}, "", workersvc.ErrNoUsableCredential, SkipNoUsableCredential},
			} {
				t.Run(timing+"/"+tc.name+map[bool]string{true: "/auto", false: "/manual"}[auto], func(t *testing.T) {
					h := newHarness()
					h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}}
					h.fb.f.labelsByIID = map[int64][]string{1: tc.labels, 2: {"uzi"}}
					h.runs.errByIssue = map[int64]error{1: tc.err}
					s := h.sweepSchedule(pgtype.Int4{Int32: 1, Valid: true})
					if timing == "once" {
						s = onceOf(s)
					}
					s.AutoApprove = auto
					if tc.pin != "" {
						s.Harness = pgtype.Text{String: tc.pin, Valid: true}
					}
					h.st.due = []store.RunSchedule{s}
					h.sched.Boot(context.Background())
					if len(h.st.advanceCalls) != 1 {
						t.Fatalf("advances=%v holds=%v", h.st.advanceCalls, h.st.heldFireCalls)
					}
					started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
					if started != 1 || len(reasons) != 1 || reasons[0] != string(tc.reason) {
						t.Fatalf("started=%d reasons=%v", started, reasons)
					}
					if len(h.fb.f.getIID) != 2 {
						t.Fatalf("fetched=%v", h.fb.f.getIID)
					}
				})
			}
		}
	}

}

func TestLabelOnceIssueDisabledHoldsAndMissingRetries(t *testing.T) {
	for _, err := range []error{workersvc.ErrHarnessCredentialDisabled, workersvc.ErrNoCredentialForHarness, workersvc.ErrNoUsableCredential} {
		t.Run(err.Error(), func(t *testing.T) {
			h := newHarness()
			h.fb.f.issue.Labels = []string{"harness::codex"}
			h.st.due = []store.RunSchedule{onceOf(h.issueSchedule())}
			h.runs.err = err
			h.sched.Boot(context.Background())
			if errors.Is(err, workersvc.ErrHarnessCredentialDisabled) {
				if len(h.st.advanceCalls) != 0 || len(h.st.heldFireCalls) != 1 {
					t.Fatalf("advances=%v holds=%v", h.st.advanceCalls, h.st.heldFireCalls)
				}
				h.runs.err = nil
				h.sched.Boot(context.Background())
				if len(h.runs.autopilot) != 1 || len(h.st.advanceCalls) != 1 {
					t.Fatal("re-enable did not fire")
				}
			} else {
				if len(h.st.heldFireCalls) != 0 {
					t.Fatal("missing/unusable broadened hold")
				}
				if errors.Is(err, workersvc.ErrNoUsableCredential) {
					if len(h.st.advanceCalls) != 1 {
						t.Fatal("unusable should retain its benign advance")
					}
				} else if len(h.st.advanceCalls) != 0 {
					t.Fatal("missing explicit credential must retry without advancing")
				}
			}
		})
	}
}

func TestLabelStoredOverrideDisabledStillHoldsSweep(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}}
	h.fb.f.issue.Labels = []string{"harness::claude"}
	s := onceOf(h.sweepSchedule(pgtype.Int4{}))
	s.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
	s.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	h.st.due = []store.RunSchedule{s}
	// Preflight passes; the authoritative create detects the later disable.
	h.runs.errByIssue = map[int64]error{1: workersvc.ErrCredentialDisabled}
	h.sched.Boot(context.Background())
	if h.runs.credDisabledCalls != 1 || len(h.st.heldFireCalls) != 1 || len(h.st.advanceCalls) != 0 || len(h.fb.f.getIID) != 1 {
		t.Fatalf("preflight=%d held=%v advance=%v fetch=%v", h.runs.credDisabledCalls, h.st.heldFireCalls, h.st.advanceCalls, h.fb.f.getIID)
	}
}

func TestMatchingPinnedHarnessDisabledKeepsSweepHold(t *testing.T) {
	for _, auto := range []bool{false, true} {
		h := newHarness()
		h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}}
		h.fb.f.issue.Labels = []string{"harness::codex"}
		s := onceOf(h.sweepSchedule(pgtype.Int4{}))
		s.AutoApprove = auto
		s.Harness = pgtype.Text{String: "codex", Valid: true}
		h.st.due = []store.RunSchedule{s}
		h.runs.err = workersvc.ErrHarnessCredentialDisabled
		h.sched.Boot(context.Background())
		if len(h.st.heldFireCalls) != 1 || len(h.st.advanceCalls) != 0 || len(h.fb.f.getIID) != 1 {
			t.Fatalf("holds=%v advances=%v fetched=%v", h.st.heldFireCalls, h.st.advanceCalls, h.fb.f.getIID)
		}
	}
}

func TestLabelInvalidRunNowCadenceNeutral(t *testing.T) {
	h := newHarness()
	h.fb.f.issue.Labels = []string{"harness::unknown"}
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err != nil || len(out.Skips) != 1 || out.Skips[0].Reason != SkipReason("harness_label_invalid") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(h.st.advanceCalls)+len(h.st.heldFireCalls)+len(h.st.statusCalls) != 0 {
		t.Fatal("RunNow altered cadence")
	}
}

func TestLabelStoredOverrideDisabledStopsAfterStartedSweep(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3}}
	h.fb.f.issue.Labels = []string{"harness::claude"}
	s := onceOf(h.sweepSchedule(pgtype.Int4{}))
	s.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
	s.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	h.st.due = []store.RunSchedule{s}
	h.runs.errByIssue = map[int64]error{2: workersvc.ErrCredentialDisabled}
	h.sched.Boot(context.Background())
	if len(h.st.advanceCalls) != 1 || len(h.st.heldFireCalls) != 0 || len(h.fb.f.getIID) != 2 {
		t.Fatalf("holds=%v advances=%v fetched=%v", h.st.heldFireCalls, h.st.advanceCalls, h.fb.f.getIID)
	}
	started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
	if started != 1 || len(reasons) != 1 || reasons[0] != string(SkipCredentialDisabled) {
		t.Fatalf("started=%d reasons=%v", started, reasons)
	}
}

func TestLabelCodexOverrideConflictBothBranches(t *testing.T) {
	for _, auto := range []bool{false, true} {
		h := newHarness()
		h.fb.f.issue = forge.Issue{Labels: []string{"harness::codex"}}
		s := h.issueSchedule()
		s.AutoApprove = auto
		s.CredentialOverrideMode = pgtype.Text{String: "auto", Valid: true}
		out, err := h.sched.RunNow(context.Background(), s)
		if err != nil || len(out.Skips) != 1 || out.Skips[0].Reason != SkipCodexOverrideConflict || len(h.runs.runs)+len(h.runs.autopilot) != 0 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	}
}

func TestLabelClaudeOverrideUsesLabelInsteadOfCodexDefault(t *testing.T) {
	for _, auto := range []bool{false, true} {
		h := newHarness()
		h.fb.f.issue.Labels = []string{"harness::claude"}
		h.runs.resolvedHarness = workersvc.HarnessCodex
		s := h.issueSchedule()
		s.AutoApprove = auto
		s.CredentialOverrideMode = pgtype.Text{String: "auto", Valid: true}
		out, err := h.sched.RunNow(context.Background(), s)
		if err != nil || len(out.Started) != 1 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	}
}

func TestLabelOnceIssueSkipsAdvance(t *testing.T) {
	for _, tc := range []struct {
		name, pin, reason string
		labels            []string
	}{
		{"invalid", "", "harness_label_invalid", []string{"harness::unknown"}},
		{"both_pinned", "claude", "harness_label_invalid", []string{"harness::claude", "harness::codex"}},
		{"conflict", "claude", "harness_label_conflict", []string{"harness::codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.fb.f.issue.Labels = tc.labels
			s := onceOf(h.issueSchedule())
			s.Harness = pgtype.Text{String: tc.pin, Valid: tc.pin != ""}
			out, err := h.sched.RunNow(context.Background(), s)
			if err != nil || len(out.Skips) != 1 || string(out.Skips[0].Reason) != tc.reason {
				t.Fatalf("RunNow out=%+v err=%v", out, err)
			}
			if len(h.st.advanceCalls)+len(h.st.heldFireCalls) != 0 {
				t.Fatal("RunNow consumed a one-shot schedule")
			}
			h.st.due = []store.RunSchedule{s}
			h.sched.Boot(context.Background())
			if len(h.st.advanceCalls) != 1 || len(h.st.heldFireCalls) != 0 {
				t.Fatalf("advances=%v holds=%v", h.st.advanceCalls, h.st.heldFireCalls)
			}
			started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
			if started != 0 || len(reasons) != 1 || reasons[0] != tc.reason {
				t.Fatalf("started=%d reasons=%v", started, reasons)
			}
		})
	}
}
