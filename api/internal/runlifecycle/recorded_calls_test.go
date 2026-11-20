package runlifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Recorded-call tests (PRD 4173 M4): for a repo with no issue-tracker link,
// pin every forge call the run lifecycle makes. The goldens were captured
// before the tracker-link rerouting; regenerate only deliberately, with
// UZI_UPDATE_GOLDEN=1.

// moveTracker is the forge the lifecycle sees: it accepts the label and note
// writes a card move and a terminal comment make and returns success.
type moveTracker struct{ forgetest.BaseFake }

func (*moveTracker) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (*moveTracker) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (*moveTracker) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, nil
}

// cacheStore is the slice of the issue cache the real forgesvc.AutoMove writes.
type cacheStore struct{ forgesvc.IssueStore }

func (cacheStore) UpsertIssueLabels(_ context.Context, arg store.UpsertIssueLabelsParams) (store.Issue, error) {
	return store.Issue{RepoID: arg.RepoID, ForgeIssueIid: arg.ForgeIssueIid, State: arg.State, Labels: arg.Labels}, nil
}

// recordingMover runs the REAL forgesvc.Service.AutoMove against a recorded
// forge, so the card-move calls are the production ones.
type recordingMover struct {
	svc *forgesvc.Service
	rec *forgetest.Recorder
}

func newRecordingMover() *recordingMover {
	return &recordingMover{
		svc: forgesvc.New(cacheStore{}, nil, 0, nil),
		rec: forgetest.NewRecorder(&moveTracker{}),
	}
}

func (m *recordingMover) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return m.rec, nil
}

func (m *recordingMover) AutoMove(ctx context.Context, f forge.IssueTracker, projectID int64, issue store.Issue, cols []store.BoardColumn, target string) (store.Issue, error) {
	return m.svc.AutoMove(ctx, f, projectID, issue, cols, target)
}

func TestRecordedCallsCardMove(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	mv := newRecordingMover()
	l := New(fs, mv, testOrigin)
	l.now = func() time.Time { return testNow }

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fs.recorded) != 1 {
		t.Fatalf("the move must reach the recorded write; recorded = %+v", fs.recorded)
	}
	forgetest.AssertGolden(t, "recorded_calls_runlifecycle_cardmove.golden", mv.rec.Calls())
}

func TestRecordedCallsAutopilotTerminalComment(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: autopilotMoveCtx(repoID, 4, "completed", 42, "https://gitlab.example.com/group/proj"),
		issue:   issueWith(repoID, 4, "opened", "PRD", "Later"),
		columns: defaultCols(),
	}
	mv := newRecordingMover()
	l := New(fs, mv, testOrigin)
	l.now = func() time.Time { return testNow }

	l.notifyOnce(context.Background(), runID, "completed")

	if fs.claimCalls != 1 {
		t.Fatalf("the terminal comment must be claimed once; claims = %d", fs.claimCalls)
	}
	forgetest.AssertGolden(t, "recorded_calls_runlifecycle_terminalcomment.golden", mv.rec.Calls())
}
