package schedsvc

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Recorded-call tests (PRD 4173 M4): for a repo with no issue-tracker link,
// pin every forge call a schedule fire makes. The goldens were captured before
// the tracker-link rerouting; regenerate only deliberately, with
// UZI_UPDATE_GOLDEN=1.

// recordingBuilder hands the scheduler a Recorder around the harness forge.
type recordingBuilder struct {
	*fakeBuilder
	rec *forgetest.Recorder
}

func (b recordingBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.rec, nil
}

// newRecordedHarness is newHarness with the scheduler's forge wrapped in a
// Recorder. The wrapped fakeForge is unchanged.
func newRecordedHarness() (*harness, *forgetest.Recorder) {
	h := newHarness()
	rec := forgetest.NewRecorder(h.fb.f)
	h.sched = New(h.st, h.runs, recordingBuilder{h.fb, rec}, h.set, h.notif, h.vault, time.Minute, nil)
	h.sched.now = func() time.Time { return h.now }
	return h, rec
}

func TestRecordedCallsFireIssue(t *testing.T) {
	h, rec := newRecordedHarness()
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err != nil || len(out.Started) != 1 {
		t.Fatalf("fire did not start a run: out=%+v err=%v", out, err)
	}
	forgetest.AssertGolden(t, "recorded_calls_schedsvc_fireissue.golden", rec.Calls())
}

func TestRecordedCallsFireSweep(t *testing.T) {
	h, rec := newRecordedHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}}
	out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 2, Valid: true}))
	if err != nil || len(out.Started) != 2 {
		t.Fatalf("sweep did not start two runs: out=%+v err=%v", out, err)
	}
	forgetest.AssertGolden(t, "recorded_calls_schedsvc_firesweep.golden", rec.Calls())
}
