package schedsvc

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// codex_pool_unavailable_test.go pins issue #3719 for schedules: an explicit Codex harness whose
// owner runs Codex account mode 'auto' with a pool that has no bindable alias is refused with
// workersvc.ErrCodexPoolUnavailable. The scheduler treats it exactly like credential_disabled
// (a once row waits, a recurring row advances, nothing falls back) but records its own reason.

func TestSkipReasonCodexPoolUnavailable(t *testing.T) {
	wrapped := fmt.Errorf("create: %w", workersvc.ErrCodexPoolUnavailable)
	got, ok := skipReasonForErr(wrapped)
	if !ok || got != SkipCodexPoolUnavailable {
		t.Fatalf("skipReasonForErr(pool unavailable) = (%q, %t), want codex_pool_unavailable", got, ok)
	}
	if string(SkipCodexPoolUnavailable) != "codex_pool_unavailable" {
		t.Fatalf("wire value = %q", SkipCodexPoolUnavailable)
	}
	// The generic credential refusal stays unmapped: only the pool refusal gets the new arm.
	if r, mapped := skipReasonForErr(workersvc.ErrNoCredentialForHarness); mapped {
		t.Fatalf("generic ErrNoCredentialForHarness mapped to %q", r)
	}
	if !holdsOnceCredentialDisabled(onceOf(store.RunSchedule{}), wrapped) {
		t.Fatal("a once row must be held on the pool refusal")
	}
	if holdsOnceCredentialDisabled(store.RunSchedule{Timing: "recurring"}, wrapped) {
		t.Fatal("a recurring row must not be held on the pool refusal")
	}
}

// MUTATION: map the pool refusal to SkipCredentialDisabled, or hard-code SkipCredentialDisabled
// in credentialDisabledHoldOutcome; the recorded reason then differs and this test fails.
func TestTickOnceCodexPoolUnavailableHoldsRecordingPoolReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sched func(h *harness) store.RunSchedule
		fired func(h *harness) int
	}{
		{"issue", func(h *harness) store.RunSchedule { return h.issueSchedule() }, func(h *harness) int { return len(h.runs.autopilot) }},
		{"prompt", func(h *harness) store.RunSchedule { return h.userPromptSchedule() }, func(h *harness) int { return len(h.runs.prompts) }},
		{"sweep", func(h *harness) store.RunSchedule {
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
			return h.sweepSchedule(pgtype.Int4{})
		}, func(h *harness) int { return len(h.runs.autopilot) + len(h.runs.runs) }},
		{"self_improve", func(h *harness) store.RunSchedule {
			s := h.selfImproveSchedule()
			s.Origin, s.CatalogSlug = "user", pgtype.Text{}
			s.Harness = pgtype.Text{String: string(workersvc.HarnessCodex), Valid: true}
			return s
		}, func(h *harness) int { return len(h.runs.selfImprove) }},
	} {
		for _, via := range []string{"precheck", "seam"} {
			t.Run(tc.name+"/"+via, func(t *testing.T) {
				h := newHarness()
				h.st.due = []store.RunSchedule{onceOf(tc.sched(h))}
				if via == "precheck" {
					h.runs.credDisabledErr = workersvc.ErrCodexPoolUnavailable
				} else {
					h.runs.err = workersvc.ErrCodexPoolUnavailable
				}

				for tick := 0; tick < 2; tick++ {
					h.sched.Boot(context.Background())
				}

				if len(h.st.advanceCalls) != 0 || len(h.st.statusCalls) != 0 {
					t.Fatalf("a held once row must not advance or park: advance=%d status=%d", len(h.st.advanceCalls), len(h.st.statusCalls))
				}
				if n := tc.fired(h); n != 0 {
					t.Fatalf("runs created while the pool is unavailable = %d, want 0", n)
				}
				if len(h.st.heldFireCalls) != 1 {
					t.Fatalf("held last_fire writes = %d, want 1", len(h.st.heldFireCalls))
				}
				if started, reasons := heldLastFire(t, h.st.due[0].LastFire); started != 0 || len(reasons) != 1 || reasons[0] != "codex_pool_unavailable" {
					t.Fatalf("held last_fire started=%d reasons=%v, want 0 started and [codex_pool_unavailable]", started, reasons)
				}

				// The owner fixes the pool: the row fires on the next tick.
				h.runs.err, h.runs.credDisabledErr = nil, nil
				h.sched.Boot(context.Background())
				if n := tc.fired(h); n != 1 {
					t.Fatalf("once retry: runs created = %d, want 1", n)
				}
			})
		}
	}
}

func TestTickRecurringCodexPoolUnavailableAdvances(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sched func(h *harness) store.RunSchedule
	}{
		{"issue", func(h *harness) store.RunSchedule { return h.issueSchedule() }},
		{"prompt", func(h *harness) store.RunSchedule { return h.userPromptSchedule() }},
		{"self_improve", func(h *harness) store.RunSchedule {
			s := h.selfImproveSchedule()
			s.Origin, s.CatalogSlug = "user", pgtype.Text{}
			s.Harness = pgtype.Text{String: string(workersvc.HarnessCodex), Valid: true}
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.st.due = []store.RunSchedule{tc.sched(h)}
			h.runs.err = workersvc.ErrCodexPoolUnavailable

			h.sched.Boot(context.Background())

			if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "active" {
				t.Fatalf("advance calls = %+v, want one active (recurring) advance", h.st.advanceCalls)
			}
			if len(h.st.heldFireCalls) != 0 {
				t.Fatalf("a recurring row takes no hold: held writes = %d", len(h.st.heldFireCalls))
			}
			if started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire); started != 0 || len(reasons) != 1 || reasons[0] != "codex_pool_unavailable" {
				t.Fatalf("last_fire started=%d reasons=%v, want [codex_pool_unavailable]", started, reasons)
			}
		})
	}
}

func TestRunNowOnceCodexPoolUnavailableSkips(t *testing.T) {
	h := newHarness()
	h.runs.err = workersvc.ErrCodexPoolUnavailable
	out, err := h.sched.RunNow(context.Background(), onceOf(h.issueSchedule()))
	if err != nil {
		t.Fatalf("RunNow on a held once issue must not surface an error, got %v", err)
	}
	if out.Matched != 1 || len(out.Started) != 0 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipCodexPoolUnavailable {
		t.Fatalf("outcome = %+v, want one codex_pool_unavailable skip", out)
	}
	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("RunNow must NOT advance: advance calls = %d", len(h.st.advanceCalls))
	}
}

// A one-time sweep that already started a run before the pool became unavailable records the
// pool reason (not credential_disabled), stops the fan-out and advances.
func TestTickOnceSweepCodexPoolUnavailableMidFireRecordsPoolReason(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3}}
	h.runs.errByIssue = map[int64]error{2: workersvc.ErrCodexPoolUnavailable, 3: workersvc.ErrCodexPoolUnavailable}
	h.st.due = []store.RunSchedule{onceOf(h.sweepSchedule(pgtype.Int4{Int32: 3, Valid: true}))}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 || h.runs.autopilot[0].issueIID != 1 {
		t.Fatalf("runs started = %+v, want only issue 1", h.runs.autopilot)
	}
	if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
		t.Fatalf("advance calls = %+v, want one fired advance", h.st.advanceCalls)
	}
	started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
	if started != 1 || len(reasons) != 1 || reasons[0] != "codex_pool_unavailable" {
		t.Fatalf("last_fire started=%d reasons=%v, want 1 started and one codex_pool_unavailable skip", started, reasons)
	}
}
