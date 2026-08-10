package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

	// PRD #102 M6 Decision 9 made both sync paths issue TWO ListIssues calls: the
	// PRD-labelled one (state=all) and an additive open, no-label one. openIssues /
	// openErr script the SECOND; issues / listErr keep scripting the first, so every
	// pre-M6 test reads as "the open fetch returned nothing", which is a legitimate
	// forge answer and leaves those tests' expectations intact.
	//
	// Kept as two independent pairs on purpose: Decision 11's failure modes are
	// asymmetric, and a fake that can only fail both at once cannot express the one
	// that deletes the backlog.
	openIssues []forge.Issue
	openErr    error

	// MR-close watcher (PRD #24) scripting. mr/mrErr are the default GetMergeRequest
	// result; mrByIID/mrErrByIID override per mrIID (for multi-candidate tests).
	mr          forge.MergeRequest
	mrErr       error
	mrByIID     map[int64]forge.MergeRequest
	mrErrByIID  map[int64]error
	mrCalls     []int64 // mrIIDs GetMergeRequest was asked for
	updateErr   error   // makes AutoMove's UpdateIssueLabels fail (forge-move failure)
	updateCalls []mrUpdateCall

	// Pipeline sync (PRD #6) scripting + capture. pipelineByRef keys on a branch
	// ref, pipelineByMR on an MR iid; a missing key returns ErrNoPipeline (the
	// no-CI case). pipelineRefErr/pipelineMRErr force a fetch error for a key.
	// latestPipeRefs/latestPipeMRs record what was queried (branch-vs-MR routing,
	// cap assertions).
	pipelineByRef  map[string]forge.Pipeline
	pipelineByMR   map[int64]forge.Pipeline
	pipelineRefErr map[string]error
	pipelineMRErr  map[int64]error
	latestPipeRefs []string
	latestPipeMRs  []int64

	// SetIssueLabel (PRD #22 M4) scripting + capture.
	ensureErr   error           // makes SetIssueLabel's EnsureLabels fail
	ensureCalls [][]forge.Label // one entry per EnsureLabels call, for the apply path

	// PRD link patch (PRD #72 M5) scripting + capture. issueByIID is what GetIssue
	// returns (the LIVE description, deliberately distinct from the run's queue-time
	// snapshot so a test can tell which one the watcher read); getIssueErrByIID
	// forces a read error. descriptionUpdates records every write, by identity.
	issueByIID         map[int64]forge.Issue
	getIssueErrByIID   map[int64]error
	getIssueCalls      []int64
	updateDescErr      error
	descriptionUpdates []descUpdateCall
}

// descUpdateCall records one UpdateIssueDescription invocation. Captured whole so
// assertions can be made by IDENTITY (which issue, what text) rather than by a
// tally, which is satisfiable by the wrong write happening the right number of
// times.
type descUpdateCall struct {
	projectID   int64
	issueIID    int64
	description string
}

// mrUpdateCall records one UpdateIssueLabels invocation so the watcher tests can
// assert the exact label swap AutoMove planned.
type mrUpdateCall struct {
	add    []string
	remove []string
}

func (f *fakeForge) UpdateIssueDescription(_ context.Context, projectID, issueIID int64, description string) error {
	if f.updateDescErr != nil {
		return f.updateDescErr
	}
	f.descriptionUpdates = append(f.descriptionUpdates, descUpdateCall{
		projectID: projectID, issueIID: issueIID, description: description,
	})
	return nil
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{}, nil
}
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) { return nil, nil }
func (f *fakeForge) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", nil
}
func (f *fakeForge) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return nil, nil
}
func (f *fakeForge) EnsureLabels(_ context.Context, _ int64, labels []forge.Label) error {
	f.ensureCalls = append(f.ensureCalls, labels)
	return f.ensureErr
}
func (f *fakeForge) ListIssues(_ context.Context, _ int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	f.listCalls = append(f.listCalls, opts)
	// The additive fetch is the one that asks for open issues and names no label;
	// routing on that shape rather than on call ORDER means a test still discriminates
	// if the two calls are ever swapped.
	if opts.State == forge.StateOpened && len(opts.Labels) == 0 {
		if f.openErr != nil {
			return nil, f.openErr
		}
		return f.openIssues, nil
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.issues, nil
}

// prdListCalls returns just the PRD-labelled ListIssues calls, so an assertion about
// the PRD fetch's options is not disturbed by the additive open fetch sitting next to
// it in listCalls.
func (f *fakeForge) prdListCalls() []forge.ListIssuesOptions {
	var out []forge.ListIssuesOptions
	for _, c := range f.listCalls {
		if len(c.Labels) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// openListCalls is prdListCalls' sibling for the additive fetch. Both route on the
// call's SHAPE rather than its index, matching ListIssues above: a per-fetch mark
// assertion has to name which fetch it means, and an index would silently follow
// the wrong one if the two calls were ever reordered.
func (f *fakeForge) openListCalls() []forge.ListIssuesOptions {
	var out []forge.ListIssuesOptions
	for _, c := range f.listCalls {
		if c.State == forge.StateOpened && len(c.Labels) == 0 {
			out = append(out, c)
		}
	}
	return out
}
func (f *fakeForge) GetIssue(_ context.Context, _ int64, issueIID int64) (forge.Issue, error) {
	f.getIssueCalls = append(f.getIssueCalls, issueIID)
	if err := f.getIssueErrByIID[issueIID]; err != nil {
		return forge.Issue{}, err
	}
	if i, ok := f.issueByIID[issueIID]; ok {
		return i, nil
	}
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
func (f *fakeForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleNone, false, nil
}
func (f *fakeForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, nil
}

// Pipeline reads (PRD #6). LatestPipeline/LatestMRPipeline are scriptable via the
// pipelineBy* maps; a missing key is the no-CI case (ErrNoPipeline). ListPipelineJobs
// and JobLogTail stay no-ops (unused by the sync milestone).
func (f *fakeForge) LatestPipeline(_ context.Context, _ int64, ref string) (forge.Pipeline, error) {
	f.latestPipeRefs = append(f.latestPipeRefs, ref)
	if err, ok := f.pipelineRefErr[ref]; ok {
		return forge.Pipeline{}, err
	}
	if p, ok := f.pipelineByRef[ref]; ok {
		return p, nil
	}
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *fakeForge) LatestMRPipeline(_ context.Context, _, mrIID int64) (forge.Pipeline, error) {
	f.latestPipeMRs = append(f.latestPipeMRs, mrIID)
	if err, ok := f.pipelineMRErr[mrIID]; ok {
		return forge.Pipeline{}, err
	}
	if p, ok := f.pipelineByMR[mrIID]; ok {
		return p, nil
	}
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

	// Pipeline sync (PRD #6) scripting + capture.
	watchedRefs      []store.ListWatchedRunRefsForRepoRow
	watchedRefsErr   error
	watchedRefsParam store.ListWatchedRunRefsForRepoParams // last args (cap/window assertions)
	pipelineUpserts  []store.UpsertPipelineStatusParams
	pipelineDeletes  []store.DeletePipelineStatusesNotInParams

	// CI-fix verification (PRD #6) scripting + capture. stampTarget is returned by
	// FindCIFixStampTarget (with stampTargetErr, default pgx.ErrNoRows meaning "no
	// run awaiting verification"); stamps records StampFixVerdict calls.
	stampTarget    store.Run
	stampTargetErr error
	stampParams    []store.FindCIFixStampTargetParams
	stamps         []store.StampFixVerdictParams

	// Filed→Done sync (PRD #98 M6) scripting + capture. The EDGE SEMANTICS are NOT
	// modelled here — a fake replaying a snapshot as events would test the model rather
	// than the mechanism — so once-only / Undo-sticks / not-overwritten live in the
	// live-DB suite (handler/judge_issue_close_livedb_test.go). What these DO cover is
	// wiring, and TestSyncFiledIssueClosesWiring asserts on every one of them: an
	// unasserted capture field is worse than an honest gap, because it looks like
	// coverage (audit B8).
	closeEdges     []store.ListFiledIssueCloseEdgesRow
	closeEdgesErr  error
	closeEdgeArgs  []store.ListFiledIssueCloseEdgesParams
	closeApplied   []store.ApplyFiledIssueCloseEdgeParams
	closeApplyErr  error
	closeApplyRows store.ApplyFiledIssueCloseEdgeRow

	// PRD-link patch (PRD #72 M5) scripting + capture.
	prdCandidates    []store.ListPRDLinkPatchCandidatesRow
	prdCandidatesErr error
	prdCandidateArgs []store.ListPRDLinkPatchCandidatesParams
	prdSettled       []uuid.UUID
	prdSettleErr     error
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
func (s *fakeStore) ListWatchedRunRefsForRepo(_ context.Context, arg store.ListWatchedRunRefsForRepoParams) ([]store.ListWatchedRunRefsForRepoRow, error) {
	s.watchedRefsParam = arg
	return s.watchedRefs, s.watchedRefsErr
}
func (s *fakeStore) UpsertPipelineStatus(_ context.Context, arg store.UpsertPipelineStatusParams) (store.PipelineStatus, error) {
	s.pipelineUpserts = append(s.pipelineUpserts, arg)
	return store.PipelineStatus{RepoID: arg.RepoID, Ref: arg.Ref, PipelineID: arg.PipelineID, Status: arg.Status}, nil
}
func (s *fakeStore) DeletePipelineStatusesNotIn(_ context.Context, arg store.DeletePipelineStatusesNotInParams) (int64, error) {
	s.pipelineDeletes = append(s.pipelineDeletes, arg)
	return 0, nil
}
func (s *fakeStore) FindCIFixStampTarget(_ context.Context, arg store.FindCIFixStampTargetParams) (store.Run, error) {
	s.stampParams = append(s.stampParams, arg)
	if s.stampTargetErr != nil {
		return store.Run{}, s.stampTargetErr
	}
	if s.stampTarget.ID == (uuid.UUID{}) {
		return store.Run{}, pgx.ErrNoRows // no ci_fix run awaiting verification
	}
	return s.stampTarget, nil
}
func (s *fakeStore) StampFixVerdict(_ context.Context, arg store.StampFixVerdictParams) (int64, error) {
	s.stamps = append(s.stamps, arg)
	return 1, nil
}
func (s *fakeStore) ListFiledIssueCloseEdges(_ context.Context, arg store.ListFiledIssueCloseEdgesParams) ([]store.ListFiledIssueCloseEdgesRow, error) {
	s.closeEdgeArgs = append(s.closeEdgeArgs, arg)
	return s.closeEdges, s.closeEdgesErr
}
func (s *fakeStore) ApplyFiledIssueCloseEdge(_ context.Context, arg store.ApplyFiledIssueCloseEdgeParams) (store.ApplyFiledIssueCloseEdgeRow, error) {
	s.closeApplied = append(s.closeApplied, arg)
	return s.closeApplyRows, s.closeApplyErr
}

func (s *fakeStore) ListPRDLinkPatchCandidates(_ context.Context, arg store.ListPRDLinkPatchCandidatesParams) ([]store.ListPRDLinkPatchCandidatesRow, error) {
	s.prdCandidateArgs = append(s.prdCandidateArgs, arg)
	return s.prdCandidates, s.prdCandidatesErr
}

// SettlePRDLinkPatch ACTUALLY APPLIES the settle by removing the row from the
// candidate set, mimicking the query's `prd_patch_settled_at IS NULL` predicate.
//
// This is not fake-realism for its own sake. §3.7's "run a second tick, expect zero
// further forge calls" passes VACUOUSLY against a fake whose settle is a no-op —
// tick 2 would re-enumerate the same row, re-patch it, and the test would still see
// "no NEW calls" only if it counted wrongly. Applying the settle is what makes tick
// 2 return nothing BECAUSE OF tick 1, which is the property under test.
func (s *fakeStore) SettlePRDLinkPatch(_ context.Context, id uuid.UUID) (int64, error) {
	if s.prdSettleErr != nil {
		return 0, s.prdSettleErr
	}
	s.prdSettled = append(s.prdSettled, id)
	kept := s.prdCandidates[:0]
	var n int64
	for _, c := range s.prdCandidates {
		if c.ID == id {
			n = 1
			continue
		}
		kept = append(kept, c)
	}
	s.prdCandidates = kept
	return n, nil
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

// TestIncrementalSyncAdvancesHWM: the PRD fetch's mark advances to its own batch
// max. The caller's two marks DIFFER, and the open fetch returns nothing, so what
// is proven is per-field: PRD moves to 2000 while Open stays at the 900 it was
// given. With one shared mark, or with the open half folded in, Open would move too.
func TestIncrementalSyncAdvancesHWM(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	f := &fakeForge{issues: []forge.Issue{issueAt(1, t1), issueAt(2, t2)}}

	start := Marks{PRD: time.Unix(500, 0), Open: time.Unix(900, 0)}
	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if !got.PRD.Equal(t2) {
		t.Fatalf("PRD mark = %v, want max updated_at %v", got.PRD, t2)
	}
	if !got.Open.Equal(start.Open) {
		t.Fatalf("Open mark = %v, want it held at %v — an empty fetch is no evidence", got.Open, start.Open)
	}
	// A non-zero mark must be sent as updated_after (server-clock boundary).
	prd := f.prdListCalls()
	if len(prd) != 1 || prd[0].UpdatedAfter == nil || !prd[0].UpdatedAfter.Equal(start.PRD) {
		t.Fatalf("expected updated_after=%v, got %+v", start.PRD, prd)
	}
	// The PRD fetch leaves State at its zero value, which is StateAll: the Closed
	// column depends on seeing closed PRD issues, and M6's additive fetch is a
	// SECOND call rather than a narrowing of this one.
	if prd[0].State != forge.StateAll {
		t.Fatalf("PRD fetch state = %q, want StateAll", prd[0].State)
	}
	if len(prd[0].Labels) != 1 || prd[0].Labels[0] != settings.DefaultPRDLabel {
		t.Fatalf("expected PRD label filter, got %v", prd[0].Labels)
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
	if fc := full.prdListCalls(); len(fc) != 1 || len(fc[0].Labels) != 1 || fc[0].Labels[0] != custom {
		t.Fatalf("FullSync label filter = %+v, want one call with [%s]", fc, custom)
	}

	inc := &fakeForge{}
	if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, inc, Marks{}); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if ic := inc.prdListCalls(); len(ic) != 1 || len(ic[0].Labels) != 1 || ic[0].Labels[0] != custom {
		t.Fatalf("IncrementalSync label filter = %+v, want one call with [%s]", ic, custom)
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
	if fc := f.prdListCalls(); len(fc) != 1 || len(fc[0].Labels) != 1 || fc[0].Labels[0] != settings.DefaultPRDLabel {
		t.Fatalf("nil resolver label filter = %+v, want one call with [%s]", fc, settings.DefaultPRDLabel)
	}
}

// TestIncrementalSyncKeepsHWMWhenBatchOlder: monotonicity, per field. The two
// caller marks differ so a returned pair that collapsed them (either direction)
// fails here rather than reading as "no regression".
func TestIncrementalSyncKeepsHWMWhenBatchOlder(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{issues: []forge.Issue{issueAt(1, time.Unix(100, 0))}}

	start := Marks{PRD: time.Unix(9000, 0), Open: time.Unix(8000, 0)} // newer than anything returned
	got, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, start)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if !got.PRD.Equal(start.PRD) || !got.Open.Equal(start.Open) {
		t.Fatalf("marks should not regress: got %+v, want %+v", got, start)
	}
}

// TestIncrementalSyncZeroHWMSendsNoLowerBound: the zero guard is PER FIELD, and
// the table runs BOTH mixed directions plus the both-zero base case. The
// symmetry is the point and it is what the docstring on IncrementalSync claims
// ("a zero mark sends no updated_after on ITS OWN fetch and leaves the other's
// bound alone"), so pinning only the PRD direction leaves half a load-bearing
// comment unpinned — and, concretely, leaves `openOpts.UpdatedAfter = &m.Open`
// with no IsZero guard at all indistinguishable from the correct code.
//
// Each mixed row also discriminates a different single-shared-guard bug: with one
// `if !m.PRD.IsZero()` covering both, row 1 drops the open bound and re-reads the
// whole open set; with one `if !m.Open.IsZero()`, row 2 does the same to the PRD
// fetch.
func TestIncrementalSyncZeroHWMSendsNoLowerBound(t *testing.T) {
	set := time.Unix(900, 0)
	cases := []struct {
		name             string
		start            Marks
		wantPRD, wantOpn *time.Time // nil = the fetch must carry NO lower bound
	}{
		{"PRD zero, Open set", Marks{Open: set}, nil, &set},
		{"Open zero, PRD set", Marks{PRD: set}, &set, nil},
		{"both zero: neither fetch is bounded", Marks{}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(&fakeStore{})
			f := &fakeForge{issues: []forge.Issue{issueAt(1, time.Unix(100, 0))}}

			if _, err := svc.IncrementalSync(context.Background(), uuid.New(), 7, f, tc.start); err != nil {
				t.Fatalf("IncrementalSync: %v", err)
			}
			prd, open := f.prdListCalls(), f.openListCalls()
			if len(prd) != 1 || len(open) != 1 {
				t.Fatalf("expected one PRD and one open fetch, got %d/%d: %+v", len(prd), len(open), f.listCalls)
			}
			assertBound(t, "PRD", prd[0].UpdatedAfter, tc.wantPRD)
			assertBound(t, "open", open[0].UpdatedAfter, tc.wantOpn)
		})
	}
}

// assertBound compares one fetch's UpdatedAfter against a want, where nil means
// "this fetch must carry no lower bound at all". Split out so the nil case is
// asserted as deliberately as the set case: `got != nil` on its own reads as a
// guard against a crash rather than as the property under test.
func assertBound(t *testing.T, which string, got, want *time.Time) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s fetch updated_after = %v, want NO lower bound — a zero mark must not be sent, and the OTHER field being set must not supply one", which, got)
	case want != nil && got == nil:
		t.Errorf("%s fetch carried no updated_after, want %v — the other field's zero must not unbound this one", which, want)
	case want != nil && got != nil && !got.Equal(*want):
		t.Errorf("%s fetch updated_after = %v, want %v", which, got, want)
	}
}
