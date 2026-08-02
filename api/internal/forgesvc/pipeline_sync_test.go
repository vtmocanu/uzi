package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// runRef builds a watched-run-ref row: a branch, optionally with an MR iid.
func runRef(branch string, mrIID int64) store.ListWatchedRunRefsForRepoRow {
	row := store.ListWatchedRunRefsForRepoRow{Branch: pgtype.Text{String: branch, Valid: true}}
	if mrIID != 0 {
		row.MrIid = pgtype.Int8{Int64: mrIID, Valid: true}
	}
	return row
}

func pipelineAt(id int64, status string) forge.Pipeline {
	return forge.Pipeline{ID: id, Status: status, SHA: "sha", WebURL: "https://gl/p"}
}

const testMaxRefs = 20

func syncOpts(evict bool) PipelineSyncOptions {
	return PipelineSyncOptions{DefaultBranch: "main", Window: 14 * 24 * time.Hour, MaxRefs: testMaxRefs, Evict: evict}
}

func TestSyncPipelinesCachesDefaultBranch(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{pipelineByRef: map[string]forge.Pipeline{"main": pipelineAt(4242, "failed")}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.pipelineUpserts) != 1 {
		t.Fatalf("expected 1 upsert (default branch), got %d", len(st.pipelineUpserts))
	}
	up := st.pipelineUpserts[0]
	if up.Ref != "main" || up.PipelineID != 4242 || up.Status != "failed" {
		t.Fatalf("unexpected default-branch upsert: %+v", up)
	}
	// Not a reconcile tick → no eviction.
	if len(st.pipelineDeletes) != 0 {
		t.Fatalf("no eviction expected on a non-reconcile tick, got %d", len(st.pipelineDeletes))
	}
}

func TestSyncPipelinesRunBranchWithMRUsesMRPipeline(t *testing.T) {
	st := &fakeStore{watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("agent/issue-9", 55)}}
	svc := newTestService(st)
	// No branch pipeline for the run ref; only the MR has one. If the sync used the
	// branch ref it would get ErrNoPipeline and cache nothing — so a cached row here
	// proves it went through the MR (catching detached/merged-results pipelines).
	f := &fakeForge{
		pipelineByRef: map[string]forge.Pipeline{"main": pipelineAt(1, "success")},
		pipelineByMR:  map[int64]forge.Pipeline{55: pipelineAt(5100, "success")},
	}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(f.latestPipeMRs) != 1 || f.latestPipeMRs[0] != 55 {
		t.Fatalf("run branch with an MR must query LatestMRPipeline(55), got %v", f.latestPipeMRs)
	}
	// The cache row for the run branch keys on the BRANCH (agent/issue-9), not the
	// pipeline's own detached ref — that is what verification later keys on.
	var got *store.UpsertPipelineStatusParams
	for i := range st.pipelineUpserts {
		if st.pipelineUpserts[i].Ref == "agent/issue-9" {
			got = &st.pipelineUpserts[i]
		}
	}
	if got == nil {
		t.Fatalf("expected an upsert keyed on the run branch, got %+v", st.pipelineUpserts)
	}
	if got.PipelineID != 5100 {
		t.Fatalf("run-branch row must carry the MR pipeline id 5100, got %d", got.PipelineID)
	}
}

func TestSyncPipelinesRunBranchWithoutMRUsesBranchPipeline(t *testing.T) {
	st := &fakeStore{watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("agent/issue-9", 0)}}
	svc := newTestService(st)
	f := &fakeForge{pipelineByRef: map[string]forge.Pipeline{
		"main":          pipelineAt(1, "success"),
		"agent/issue-9": pipelineAt(9001, "running"),
	}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(f.latestPipeMRs) != 0 {
		t.Fatalf("a run branch with no MR must NOT query LatestMRPipeline, got %v", f.latestPipeMRs)
	}
	if !contains(f.latestPipeRefs, "agent/issue-9") {
		t.Fatalf("expected a branch-ref pipeline query for agent/issue-9, got %v", f.latestPipeRefs)
	}
}

func TestSyncPipelinesNoCISkipsUpsert(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	// Default branch has no CI (no scripted pipeline → ErrNoPipeline).
	f := &fakeForge{}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(true)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.pipelineUpserts) != 0 {
		t.Fatalf("no-CI ref must not be cached, got %d upserts", len(st.pipelineUpserts))
	}
	// Reconcile tick evicts, and the no-CI default branch is NOT in the keep-set, so
	// any stale row for it is dropped (honest "no CI").
	if len(st.pipelineDeletes) != 1 {
		t.Fatalf("expected one eviction on the reconcile tick, got %d", len(st.pipelineDeletes))
	}
	if len(st.pipelineDeletes[0].KeepRefs) != 0 {
		t.Fatalf("no-CI default branch must not be kept, keep-set = %v", st.pipelineDeletes[0].KeepRefs)
	}
}

func TestSyncPipelinesEvictsUnwatchedOnReconcile(t *testing.T) {
	st := &fakeStore{watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("agent/issue-9", 0)}}
	svc := newTestService(st)
	f := &fakeForge{pipelineByRef: map[string]forge.Pipeline{
		"main":          pipelineAt(1, "success"),
		"agent/issue-9": pipelineAt(2, "failed"),
	}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(true)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.pipelineDeletes) != 1 {
		t.Fatalf("expected one eviction call, got %d", len(st.pipelineDeletes))
	}
	keep := st.pipelineDeletes[0].KeepRefs
	if len(keep) != 2 || !contains(keep, "main") || !contains(keep, "agent/issue-9") {
		t.Fatalf("keep-set must be the freshly cached refs, got %v", keep)
	}
}

func TestSyncPipelinesFetchFailurePreservesRow(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	// The default-branch fetch FAILS (not ErrNoPipeline). The ref must stay in the
	// keep-set so its existing (stale) cache row is not evicted on the reconcile.
	f := &fakeForge{pipelineRefErr: map[string]error{"main": errors.New("forge down")}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(true)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.pipelineUpserts) != 0 {
		t.Fatalf("a failed fetch must not upsert, got %d", len(st.pipelineUpserts))
	}
	if len(st.pipelineDeletes) != 1 {
		t.Fatalf("expected one eviction call, got %d", len(st.pipelineDeletes))
	}
	if keep := st.pipelineDeletes[0].KeepRefs; len(keep) != 1 || keep[0] != "main" {
		t.Fatalf("a fetch-failed ref must be kept (stale row preserved), keep-set = %v", keep)
	}
}

func TestSyncPipelinesDisabledWhenMaxRefsZero(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{pipelineByRef: map[string]forge.Pipeline{"main": pipelineAt(1, "success")}}

	opts := syncOpts(true)
	opts.MaxRefs = 0 // CI_WATCH_MAX_REFS=0 → pipeline sync fully disabled
	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, opts); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(f.latestPipeRefs) != 0 || len(st.pipelineUpserts) != 0 || len(st.pipelineDeletes) != 0 {
		t.Fatalf("MaxRefs=0 must be a full no-op (no forge/store calls), got refs=%v upserts=%d deletes=%d",
			f.latestPipeRefs, len(st.pipelineUpserts), len(st.pipelineDeletes))
	}
}

func TestSyncPipelinesVerifiesFixBranch(t *testing.T) {
	fixRun := store.Run{ID: uuid.New()}
	// A ci_fix run's fix branch is watched (its run has an MR). Its post-fix pipeline
	// PASSED, with an id newer than the failure that spawned the run.
	st := &fakeStore{
		watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("ci-fix/pipeline-4200", 77)},
		stampTarget: fixRun,
	}
	svc := newTestService(st)
	f := &fakeForge{pipelineByMR: map[int64]forge.Pipeline{77: pipelineAt(4300, "success")}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.stamps) != 1 {
		t.Fatalf("expected exactly one verdict stamp, got %d", len(st.stamps))
	}
	if st.stamps[0].ID != fixRun.ID || st.stamps[0].FixVerdict.String != "verified" {
		t.Fatalf("expected 'verified' stamped on the fix run, got %+v", st.stamps[0])
	}
	// The target selector must be keyed on the FIX BRANCH and the observed pipeline
	// id (the "newer than the failure" guard).
	last := st.stampParams[len(st.stampParams)-1]
	if last.Branch.String != "ci-fix/pipeline-4200" || last.ObservedPipelineID.Int64 != 4300 {
		t.Fatalf("stamp-target selection must key on the fix branch + observed id, got %+v", last)
	}
}

func TestSyncPipelinesStampsFixFailedOnRedPipeline(t *testing.T) {
	fixRun := store.Run{ID: uuid.New()}
	st := &fakeStore{
		watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("ci-fix/pipeline-4200", 77)},
		stampTarget: fixRun,
	}
	svc := newTestService(st)
	f := &fakeForge{pipelineByMR: map[int64]forge.Pipeline{77: pipelineAt(4300, "failed")}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.stamps) != 1 || st.stamps[0].FixVerdict.String != "fix_failed" {
		t.Fatalf("a red post-fix pipeline must stamp 'fix_failed', got %+v", st.stamps)
	}
}

// TestSyncPipelinesStampsFixFailedOnForgejoFailure is the cross-vocabulary guard:
// a Forgejo fix branch whose re-run FAILS reports the Actions status "failure", not
// GitLab's "failed". A bare `case "failed"` here never stamped it — the fix run
// stayed unverified forever. Classifying via pipelinestatus fixes it. Mutation
// check: revert maybeStampFixVerdict to `case "failed"` and this reddens while the
// GitLab test above stays green.
func TestSyncPipelinesStampsFixFailedOnForgejoFailure(t *testing.T) {
	fixRun := store.Run{ID: uuid.New()}
	st := &fakeStore{
		watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("ci-fix/pipeline-4200", 77)},
		stampTarget: fixRun,
	}
	svc := newTestService(st)
	f := &fakeForge{pipelineByMR: map[int64]forge.Pipeline{77: pipelineAt(4300, "failure")}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.stamps) != 1 || st.stamps[0].FixVerdict.String != "fix_failed" {
		t.Fatalf("a Forgejo 'failure' post-fix pipeline must stamp 'fix_failed', got %+v", st.stamps)
	}
}

func TestSyncPipelinesDoesNotStampWhileRunning(t *testing.T) {
	st := &fakeStore{
		watchedRefs: []store.ListWatchedRunRefsForRepoRow{runRef("ci-fix/pipeline-4200", 77)},
		stampTarget: store.Run{ID: uuid.New()}, // a target exists...
	}
	svc := newTestService(st)
	// ...but the pipeline has not concluded, so no verdict is stamped yet.
	f := &fakeForge{pipelineByMR: map[int64]forge.Pipeline{77: pipelineAt(4300, "running")}}

	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, syncOpts(false)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if len(st.stamps) != 0 {
		t.Fatalf("a still-running pipeline must not stamp a verdict, got %+v", st.stamps)
	}
}

func TestSyncPipelinesPassesWindowAndCapToQuery(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(st)
	f := &fakeForge{}

	opts := PipelineSyncOptions{DefaultBranch: "main", Window: 14 * 24 * time.Hour, MaxRefs: 20}
	before := time.Now().Add(-opts.Window)
	if err := svc.SyncPipelines(context.Background(), uuid.New(), 7, f, opts); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}
	if st.watchedRefsParam.MaxRefs != 20 {
		t.Fatalf("cap must reach the query, got MaxRefs=%d", st.watchedRefsParam.MaxRefs)
	}
	// FinishedAfter should be ~now-window (computed caller-side). Allow a small skew.
	fa := st.watchedRefsParam.FinishedAfter
	if !fa.Valid || fa.Time.Before(before.Add(-time.Minute)) || fa.Time.After(before.Add(time.Minute)) {
		t.Fatalf("FinishedAfter must be now-window, got %v (want ~%v)", fa.Time, before)
	}
}
