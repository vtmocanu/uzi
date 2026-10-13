package schedsvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The sole Claude token is both the issue label's credential and the schedule's
// stored override. Disabling it after preflight makes harness resolution refuse
// before the insertion closure can report ErrCredentialDisabled.
type lateOverrideRuns struct{ *fakeRuns }

func (r *lateOverrideRuns) CheckStoredCredentialOverrideEnabled(context.Context, uuid.UUID, *workersvc.CredentialOverride) error {
	r.credDisabledCalls++
	return workersvc.ErrCredentialDisabled
}
func TestLateStoredOverrideDoesNotBecomeLabelSkip(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			t.Run(map[bool]string{false: "manual", true: "auto"}[auto]+map[bool]string{false: "/before_start", true: "/after_start"}[partial], func(t *testing.T) {
				h := newHarness()
				h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3}}
				h.fb.f.issue.Labels = []string{"harness::claude"}
				s := onceOf(h.sweepSchedule(pgtype.Int4{}))
				s.AutoApprove = auto
				s.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
				s.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
				h.st.due = []store.RunSchedule{s}
				refused := int64(1)
				if partial {
					refused = 2
				}
				h.runs.errByIssue = map[int64]error{refused: workersvc.ErrHarnessCredentialDisabled}
				runs := &lateOverrideRuns{h.runs}
				h.sched = New(h.st, runs, h.fb, h.set, h.notif, h.vault, time.Minute, nil)
				h.sched.now = func() time.Time { return h.now }
				h.sched.Boot(context.Background())
				if runs.credDisabledCalls != 2 {
					t.Fatalf("preflight and refusal checks=%d", runs.credDisabledCalls)
				}
				if partial {
					if len(h.st.heldFireCalls) != 0 || len(h.st.advanceCalls) != 1 || len(h.fb.f.getIID) != 2 {
						t.Fatalf("partial sweep holds=%v advances=%v fetches=%v", h.st.heldFireCalls, h.st.advanceCalls, h.fb.f.getIID)
					}
					started, reasons := heldLastFire(t, h.st.advanceCalls[0].LastFire)
					if started != 1 || len(reasons) != 1 || reasons[0] != string(SkipCredentialDisabled) {
						t.Fatalf("started=%d reasons=%v", started, reasons)
					}
				} else if len(h.st.heldFireCalls) != 1 || len(h.st.advanceCalls) != 0 || len(h.fb.f.getIID) != 1 {
					t.Fatalf("stored override must hold: holds=%v advances=%v fetches=%v", h.st.heldFireCalls, h.st.advanceCalls, h.fb.f.getIID)
				}
			})
		}
	}
}
