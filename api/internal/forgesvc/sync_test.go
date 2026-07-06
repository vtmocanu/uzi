package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeForge is a mocked Forge whose ListIssues and GetMergeRequest are scripted;
// other methods are unused by the sync/watcher paths under test. The MR-watch
// tests (mr_watch_test.go) drive mr/mrErr and updateErr.
type fakeForge struct {
	issues    []forge.Issue
	listErr   error
	listCalls []forge.ListIssuesOptions

	// MR-close watcher (PRD #24) scripting. mr/mrErr are the default GetMergeRequest
	// result; mrByIID/mrErrByIID override per mrIID (for multi-candidate tests).
	mr          forge.MergeRequest
	mrErr       error
	mrByIID     map[int64]forge.MergeRequest
	mrErrByIID  map[int64]error
	mrCalls     []int64 // mrIIDs GetMergeRequest was asked for
	updateErr   error   // makes AutoMove's UpdateIssueLabels fail (forge-move failure)
	updateCalls []mrUpdateCall
}

// mrUpdateCall records one UpdateIssueLabels invocation so the watcher tests can
// assert the exact label swap AutoMove planned.
type mrUpdateCall struct {
	add    []string
	remove []string
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{}, nil
}
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) { return nil, nil }
func (f *fakeForge) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return nil, nil
}
func (f *fakeForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (f *fakeForge) ListIssues(_ context.Context, _ int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	f.listCalls = append(f.listCalls, opts)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.issues, nil
}
func (f *fakeForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *fakeForge) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *fakeForge) UpdateIssueLabels(_ context.Context, _, _ int64, add, remove []string) error {
	f.updateCalls = append(f.updateCalls, mrUpdateCall{add: add, remove: remove})
	return f.updateErr
}
func (f *fakeForge) GetMergeRequest(_ context.Context, _, mrIID int64) (forge.MergeRequest, error) {
	f.mrCalls = append(f.mrCalls, mrIID)
	if err, ok := f.mrErrByIID[mrIID]; ok {
		return forge.MergeRequest{}, err
	}
	if mr, ok := f.mrByIID[mrIID]; ok {
		return mr, nil
	}
	if f.mrErr != nil {
		return forge.MergeRequest{}, f.mrErr
	}
	return f.mr, nil
}
func (f *fakeForge) UserExists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeForge) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, nil
}
func (f *fakeForge) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, nil
}
func (f *fakeForge) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{}, nil
}
func (f *fakeForge) ProjectRole(context.Context, int64, int64) (int, bool, error) {
	return 0, false, nil
}
func (f *fakeForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, nil
}

// Pipeline reads (PRD #6). Default to "no CI" (ErrNoPipeline); the pipeline-sync
// milestone (M2) gives this fake scriptable pipeline behaviour.
func (f *fakeForge) LatestPipeline(context.Context, int64, string) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *fakeForge) LatestMRPipeline(context.Context, int64, int64) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *fakeForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return nil, nil
}
func (f *fakeForge) JobLogTail(context.Context, int64, int64, int) (string, error) { return "", nil }

// fakeStore records what the sync writes, standing in for *store.Queries. The
// MR-close watcher fields (candidates/issue/columns/mrStateWrites) are exercised
// by mr_watch_test.go; the sync tests leave them zero.
type fakeStore struct {
	upserts     []store.UpsertIssueParams
	deleteCalls []store.DeleteIssuesNotInParams

	// MR-close watcher (PRD #24) scripting + capture.
	candidates    []store.ListMRWatchCandidatesRow
	candidatesErr error
	issue         store.Issue
	issueErr      error
	columns       []store.BoardColumn
	columnsErr    error
	mrStateWrites []store.SetRunMRStateParams
}

func (s *fakeStore) UpsertIssue(_ context.Context, arg store.UpsertIssueParams) (store.Issue, error) {
	s.upserts = append(s.upserts, arg)
	// Echo the labels back so AutoMove's re-cache returns the moved row.
	return store.Issue{RepoID: arg.RepoID, ForgeIssueIid: arg.ForgeIssueIid, State: arg.State, Labels: arg.Labels}, nil
}
func (s *fakeStore) DeleteIssuesNotIn(_ context.Context, arg store.DeleteIssuesNotInParams) (int64, error) {
	s.deleteCalls = append(s.deleteCalls, arg)
	return 0, nil
}
func (s *fakeStore) ListMRWatchCandidates(context.Context, uuid.UUID) ([]store.ListMRWatchCandidatesRow, error) {
	return s.candidates, s.candidatesErr
}
func (s *fakeStore) GetIssueByIID(context.Context, store.GetIssueByIIDParams) (store.Issue, error) {
	return s.issue, s.issueErr
}
func (s *fakeStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	return s.columns, s.columnsErr
}
func (s *fakeStore) SetRunMRState(_ context.Context, arg store.SetRunMRStateParams) (int64, error) {
	s.mrStateWrites = append(s.mrStateWrites, arg)
	return 1, nil
}

// fakeLabels is a fixed LabelConfig for the sync tests.
type fakeLabels struct{ prd string }

func (f fakeLabels) PRDLabel(context.Context) (string, error) { return f.prd, nil }

func newTestService(st IssueStore) *Service {
	// box is nil: FullSync/IncrementalSync operate on a passed-in Forge and
	// never touch the encryption box. Labels resolve to the default so the
	// existing filter assertions hold.
	return New(st, nil, time.Second, fakeLabels{prd: settings.DefaultPRDLabel})
}

func issueAt(iid int64, updated time.Time) forge.Issue {
	return forge.Issue{IID: iid, Title: "t", State: "opened", UpdatedAt: updated}
}

func TestFullSyncEvictsAbsent(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{issues: []forge.Issue{issueAt(1, time.Unix(100, 0)), issueAt(2, time.Unix(200, 0))}}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(st.upserts) != 2 {
		t.Fatalf("expected 2 upserts, got %d", len(st.upserts))
	}
	if len(st.deleteCalls) != 1 {
		t.Fatalf("expected exactly one eviction call, got %d", len(st.deleteCalls))
	}
	got := st.deleteCalls[0].KeepIids
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("eviction keep-set = %v, want [1 2]", got)
	}
}

func TestFullSyncNoEvictionOnListError(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{listErr: errors.New("forge down")}

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err == nil {
		t.Fatal("expected FullSync to propagate the list error")
	}
	// The critical guarantee: a failed fetch must not evict the cache.
	if len(st.deleteCalls) != 0 {
		t.Fatalf("eviction ran despite a fetch error: %+v", st.deleteCalls)
	}
	if len(st.upserts) != 0 {
		t.Fatalf("upserts ran despite a fetch error: %+v", st.upserts)
	}
}

func TestFullSyncEmptySuccessEvictsAll(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{issues: nil} // clean fetch, zero PRD issues → forge is empty

	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(st.deleteCalls) != 1 || len(st.deleteCalls[0].KeepIids) != 0 {
		t.Fatalf("empty clean fetch should evict all (keep=[]), got %+v", st.deleteCalls)
	}
}

func TestIncrementalSyncAdvancesHWM(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	f := &fakeForge{issues: []forge.Issue{issueAt(1, t1), issueAt(2, t2)}}

	start := time.Unix(500, 0)
	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if !got.Equal(t2) {
		t.Fatalf("HWM = %v, want max updated_at %v", got, t2)
	}
	// A non-zero HWM must be sent as updated_after (server-clock boundary).
	if len(f.listCalls) != 1 || f.listCalls[0].UpdatedAfter == nil || !f.listCalls[0].UpdatedAfter.Equal(start) {
		t.Fatalf("expected updated_after=%v, got %+v", start, f.listCalls[0].UpdatedAfter)
	}
	// It never queries incrementally without state=all being enforced by the
	// driver; here we only assert the PRD label filter is applied.
	if len(f.listCalls[0].Labels) != 1 || f.listCalls[0].Labels[0] != settings.DefaultPRDLabel {
		t.Fatalf("expected PRD label filter, got %v", f.listCalls[0].Labels)
	}
}

// TestSyncFiltersOnConfiguredLabel proves the sync queries the label the settings
// resolver reports, not a hardcoded constant (PRD #19 M2): a service configured
// with a custom label filters ListIssues on it in both sync paths.
func TestSyncFiltersOnConfiguredLabel(t *testing.T) {
	const custom = "Feature"
	svc := New(&fakeStore{}, nil, time.Second, fakeLabels{prd: custom})

	full := &fakeForge{}
	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, full); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(full.listCalls) != 1 || len(full.listCalls[0].Labels) != 1 || full.listCalls[0].Labels[0] != custom {
		t.Fatalf("FullSync label filter = %v, want [%s]", full.listCalls[0].Labels, custom)
	}

	inc := &fakeForge{}
	if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, inc, time.Time{}); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if len(inc.listCalls) != 1 || len(inc.listCalls[0].Labels) != 1 || inc.listCalls[0].Labels[0] != custom {
		t.Fatalf("IncrementalSync label filter = %v, want [%s]", inc.listCalls[0].Labels, custom)
	}
}

// TestSyncFallsBackToDefaultLabel proves a nil resolver degrades to the
// compiled-in default rather than filtering on an empty label.
func TestSyncFallsBackToDefaultLabel(t *testing.T) {
	svc := New(&fakeStore{}, nil, time.Second, nil)
	f := &fakeForge{}
	if _, err := svc.FullSync(context.Background(), uuid.New(), 7, f); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(f.listCalls) != 1 || len(f.listCalls[0].Labels) != 1 || f.listCalls[0].Labels[0] != settings.DefaultPRDLabel {
		t.Fatalf("nil resolver label filter = %v, want [%s]", f.listCalls[0].Labels, settings.DefaultPRDLabel)
	}
}

func TestIncrementalSyncKeepsHWMWhenBatchOlder(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{issues: []forge.Issue{issueAt(1, time.Unix(100, 0))}}

	start := time.Unix(9000, 0) // newer than anything returned
	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if !got.Equal(start) {
		t.Fatalf("HWM should not regress: got %v, want %v", got, start)
	}
}

func TestIncrementalSyncZeroHWMSendsNoLowerBound(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{issues: []forge.Issue{issueAt(1, time.Unix(100, 0))}}

	if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, time.Time{}); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if f.listCalls[0].UpdatedAfter != nil {
		t.Fatalf("zero HWM must send no updated_after, got %v", f.listCalls[0].UpdatedAfter)
	}
}
