package forgesvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The recorded-call tests below pin, for a repo with NO issue-tracker link,
// every forge call each forgesvc sync path makes (PRD 4173 M4: a repo with no
// link is byte-for-byte unchanged in its forge calls). The golden files under
// testdata/ were captured on the code before the tracker-link rerouting; a
// change to one is a change to the forge traffic of every unlinked repo and
// must be deliberate (regenerate with UZI_UPDATE_GOLDEN=1).

var recordedMarkBase = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func TestRecordedCallsFullSync(t *testing.T) {
	st := &fakeStore{}
	f := &fakeForge{
		issues:        []forge.Issue{issueAt(1, recordedMarkBase)},
		openIssues:    []forge.Issue{issueAt(2, recordedMarkBase.Add(time.Minute))},
		findingIssues: []forge.Issue{issueAt(3, recordedMarkBase.Add(2*time.Minute))},
	}
	rec := forgetest.NewRecorder(f)
	if _, err := newTestService(st).FullSync(context.Background(), uuid.New(), testProjectID, rec); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	forgetest.AssertGolden(t, "recorded_calls_forgesvc_fullsync.golden", rec.Calls())
}

func TestRecordedCallsIncrementalSync(t *testing.T) {
	st := &fakeStore{}
	f := &fakeForge{
		issues:        []forge.Issue{issueAt(1, recordedMarkBase.Add(time.Hour))},
		openIssues:    []forge.Issue{issueAt(2, recordedMarkBase.Add(time.Hour))},
		findingIssues: []forge.Issue{issueAt(3, recordedMarkBase.Add(time.Hour))},
	}
	rec := forgetest.NewRecorder(f)
	marks := Marks{PRD: recordedMarkBase, Open: recordedMarkBase.Add(time.Minute), Finding: recordedMarkBase.Add(2 * time.Minute)}
	if _, err := newTestService(st).IncrementalSync(context.Background(), uuid.New(), testProjectID, rec, marks); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	forgetest.AssertGolden(t, "recorded_calls_forgesvc_incrementalsync.golden", rec.Calls())
}

// TestRecordedCallsSyncMRStates drives one open->closed edge and one
// closed->open edge, each through its own recorder, into one golden.
func TestRecordedCallsSyncMRStates(t *testing.T) {
	var lines []string
	for _, tc := range []struct {
		name, stored, observed, from string
	}{
		{"opened->closed", "opened", "closed", board.ColumnHumanReview},
		{"closed->opened", "closed", "opened", board.ColumnInProgress},
	} {
		runID, repoID := uuid.New(), uuid.New()
		st := &fakeStore{
			candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt(tc.stored))},
			issue:      mrIssue(repoID, 9, "opened", "PRD", tc.from),
			columns:    mrCols(),
		}
		f := &fakeForge{mr: forgeMR(13, tc.observed)}
		rec := forgetest.NewRecorder(f)
		if err := newTestService(st).SyncMRStates(context.Background(), repoID, testProjectID, rec, rec); err != nil {
			t.Fatalf("SyncMRStates %s: %v", tc.name, err)
		}
		lines = append(lines, "# "+tc.name)
		lines = append(lines, rec.Calls()...)
	}
	forgetest.AssertGolden(t, "recorded_calls_forgesvc_mrstates.golden", lines)
}

func TestRecordedCallsSyncBoardFreeMRStates(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{boardFreeCandidates: []store.ListBoardFreeMRStateWatchCandidatesRow{
		boardFreeCand(runID, 13, strptr(forge.MRStateOpened)),
	}}
	f := &fakeForge{mr: forgeMR(13, forge.MRStateMerged)}
	svc := newTestService(st)
	svc.SetReworkCanceller(&fakeReworkCanceller{})
	rec := forgetest.NewRecorder(f)
	if err := svc.SyncBoardFreeMRStates(context.Background(), repoID, testProjectID, rec); err != nil {
		t.Fatalf("SyncBoardFreeMRStates: %v", err)
	}
	forgetest.AssertGolden(t, "recorded_calls_forgesvc_boardfreemrstates.golden", rec.Calls())
}

func TestRecordedCallsSyncPRDLinkPatches(t *testing.T) {
	s, _, f, _ := newPRDFixture(t, nil)
	rec := forgetest.NewRecorder(f)
	if err := s.SyncPRDLinkPatches(context.Background(), uuid.New(), fixProject, rec, rec); err != nil {
		t.Fatalf("SyncPRDLinkPatches: %v", err)
	}
	if len(f.descriptionUpdates) != 1 {
		t.Fatalf("fixture must reach the description write; updates = %+v", f.descriptionUpdates)
	}
	forgetest.AssertGolden(t, "recorded_calls_forgesvc_prdlinkpatches.golden", rec.Calls())
}

// emptyBoardStore is a BoardColumnStore for a repo that has no columns yet.
type emptyBoardStore struct{}

func (emptyBoardStore) CountBoardColumns(context.Context, uuid.UUID) (int64, error) { return 0, nil }
func (emptyBoardStore) InsertDefaultBoardColumns(context.Context, store.InsertDefaultBoardColumnsParams) error {
	return nil
}

func TestRecordedCallsEnsureBoardColumns(t *testing.T) {
	rec := forgetest.NewRecorder(&fakeForge{})
	if err := EnsureBoardColumns(context.Background(), emptyBoardStore{}, rec, uuid.New(), testProjectID); err != nil {
		t.Fatalf("EnsureBoardColumns: %v", err)
	}
	forgetest.AssertGolden(t, "recorded_calls_forgesvc_ensureboardcolumns.golden", rec.Calls())
}
