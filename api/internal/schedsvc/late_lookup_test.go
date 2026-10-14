package schedsvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type lateLookupRuns struct {
	*fakeRuns
	lookupErr error
	checks    int
}

func (r *lateLookupRuns) CheckStoredCredentialOverrideEnabled(context.Context, uuid.UUID, *workersvc.CredentialOverride) error {
	r.checks++
	return r.lookupErr
}

func TestLateStoredOverrideEnabledPreservesLabelRefusal(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, refusal := range []error{workersvc.ErrNoCredentialForHarness, workersvc.ErrNoUsableCredential, workersvc.ErrHarnessCredentialDisabled} {
			t.Run(fmt.Sprintf("auto=%t/%s", auto, refusal), func(t *testing.T) {
				h := newHarness()
				h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}}
				h.fb.f.issue.Labels = []string{"harness::claude"}
				s := onceOf(h.sweepSchedule(pgtype.Int4{Int32: 1, Valid: true}))
				s.AutoApprove = auto
				s.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
				s.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
				h.runs.errByIssue = map[int64]error{1: refusal}
				runs := &lateLookupRuns{fakeRuns: h.runs}
				h.sched.runs = runs
				out, err := h.sched.RunNow(context.Background(), s)
				want := SkipNoUsableCredential
				if errors.Is(refusal, workersvc.ErrHarnessCredentialDisabled) {
					want = SkipCredentialDisabled
				}
				if err != nil || out.Matched != 2 || len(out.Started) != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != want || runs.checks != 1 {
					t.Fatalf("outcome=%+v error=%v strict checks=%d", out, err, runs.checks)
				}
				if len(h.st.advanceCalls) != 0 || len(h.st.heldFireCalls) != 0 {
					t.Fatal("run-now wrote schedule state")
				}
			})
		}
	}
}

func TestLateStoredOverrideLookupFailure(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			for _, mode := range []string{"tick", "run_now", "recurring"} {
				for _, refusal := range []error{workersvc.ErrNoCredentialForHarness, workersvc.ErrNoUsableCredential, workersvc.ErrHarnessCredentialDisabled} {
					t.Run(fmt.Sprintf("auto=%t/partial=%t/%s/%s", auto, partial, mode, refusal), func(t *testing.T) {
						h := newHarness()
						h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3}}
						h.fb.f.issue.Labels = []string{"harness::claude"}
						s := onceOf(h.sweepSchedule(pgtype.Int4{Int32: 2, Valid: true}))
						s.AutoApprove = auto
						s.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
						s.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
						if mode == "recurring" {
							s.Timing = "recurring"
							s.CronExpr = pgtype.Text{String: "0 * * * *", Valid: true}
						}
						refused := int64(1)
						if partial {
							refused = 2
						}
						h.runs.errByIssue = map[int64]error{refused: fmt.Errorf("create: %w", refusal)}
						cause := errors.New("metadata unavailable")
						runs := &lateLookupRuns{fakeRuns: h.runs, lookupErr: fmt.Errorf("database: %w", cause)}
						h.sched.runs = runs
						h.st.due = []store.RunSchedule{s}
						if mode == "run_now" {
							out, err := h.sched.RunNow(context.Background(), s)
							if partial {
								if err != nil || out.Matched != len(out.Started)+len(out.Skips) || len(out.Started) != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipFetchFailed {
									t.Fatalf("partial outcome=%+v error=%v", out, err)
								}
							} else if !errors.Is(err, cause) || len(out.Started) != 0 || len(out.Skips) != 0 {
								t.Fatalf("zero outcome=%+v error=%v, want wrapped cause", out, err)
							}
						} else {
							h.sched.Boot(context.Background())
						}
						wantPreflight := 1
						if mode != "tick" {
							wantPreflight = 0
						}
						if runs.credDisabledCalls != wantPreflight {
							t.Fatalf("preflight checks=%d want %d", runs.credDisabledCalls, wantPreflight)
						}
						if runs.checks != 1 {
							t.Fatalf("strict checks=%d want 1", runs.checks)
						}
						if len(h.st.heldFireCalls) != 0 || len(h.st.statusCalls) != 0 {
							t.Fatalf("lookup failure held or parked: %+v", h.st)
						}
						if mode == "run_now" || (!partial && mode == "tick") {
							if len(h.st.advanceCalls) != 0 {
								t.Fatal("unexpected cadence/history write")
							}
						} else {
							if len(h.st.advanceCalls) != 1 {
								t.Fatalf("advances=%d", len(h.st.advanceCalls))
							}
							started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
							want := 1
							if mode == "recurring" {
								want = 2
							}
							if started != want || len(reasons) != 1 || reasons[0] != string(SkipFetchFailed) {
								t.Fatalf("started=%d reasons=%v", started, reasons)
							}
						}
						wantFetch := int(refused)
						if mode == "recurring" {
							wantFetch = 3
						}
						if len(h.fb.f.getIID) != wantFetch {
							t.Fatalf("fetches=%v want %d", h.fb.f.getIID, wantFetch)
						}
						if mode == "tick" && !partial {
							runs.lookupErr = nil
							h.runs.errByIssue = nil
							h.sched.Boot(context.Background())
							if len(h.st.advanceCalls) != 1 || len(h.runs.runs)+len(h.runs.autopilot) != 2 {
								t.Fatal("recovery did not fire within start cap")
							}
							if len(h.fb.f.getIID) != 3 {
								t.Fatalf("cap exceeded on recovery: %v", h.fb.f.getIID)
							}
						}
					})
				}
			}
		}
	}
}
