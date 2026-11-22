package forgesvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	activeTestSHA    = "0123456789abcdef0123456789abcdef01234567"
	activeTestBranch = "agent/issue-9"
)

// summaryForge answers GetMergeRequestSummary from a script and counts calls per iid.
type summaryForge struct {
	forgetest.BaseFake
	byIID map[int64]forge.MergeRequestSummary
	errs  map[int64]error
	calls []int64
	// hook runs before each answer (the live-DB reclaim barrier uses it).
	hook func(iid int64)
}

func (f *summaryForge) GetMergeRequestSummary(_ context.Context, _, iid int64) (forge.MergeRequestSummary, error) {
	f.calls = append(f.calls, iid)
	if f.hook != nil {
		f.hook(iid)
	}
	if err := f.errs[iid]; err != nil {
		return forge.MergeRequestSummary{}, err
	}
	return f.byIID[iid], nil
}

type fakeTerminalCanceller struct {
	obs  []workersvc.TerminalTargetObservation
	errs map[uuid.UUID]error
}

func (c *fakeTerminalCanceller) CancelRunForTerminalTarget(_ context.Context, _ uuid.UUID, obs workersvc.TerminalTargetObservation) error {
	c.obs = append(c.obs, obs)
	return c.errs[obs.RunID]
}

func activeRow(id uuid.UUID, kind string, iid int64, branch string) store.ListActiveMRTargetRunsRow {
	return store.ListActiveMRTargetRunsRow{
		ID: id, Kind: kind, Status: "running", ClaimGeneration: 3,
		WorkerID:    pgtype.UUID{Bytes: uuid.New(), Valid: true},
		PipelineRef: pgtype.Text{String: branch, Valid: true},
		TargetMrIid: iid,
	}
}

func goodSummary(iid int64, state string) forge.MergeRequestSummary {
	return forge.MergeRequestSummary{IID: iid, SourceBranch: activeTestBranch, HeadSHA: activeTestSHA, State: state}
}

func TestSyncActiveMRTargetsVerification(t *testing.T) {
	const iid = 41
	long := strings.Repeat("a", 39)
	cases := []struct {
		name    string
		sum     forge.MergeRequestSummary
		err     error
		wantObs bool
	}{
		{name: "merged", sum: goodSummary(iid, forge.MRStateMerged), wantObs: true},
		{name: "closed", sum: goodSummary(iid, forge.MRStateClosed), wantObs: true},
		{name: "wrong iid", sum: goodSummary(iid+1, forge.MRStateMerged)},
		{name: "wrong source branch", sum: func() forge.MergeRequestSummary {
			s := goodSummary(iid, forge.MRStateMerged)
			s.SourceBranch = "agent/issue-10"
			return s
		}()},
		{name: "unsafe branch", sum: func() forge.MergeRequestSummary {
			s := goodSummary(iid, forge.MRStateMerged)
			s.SourceBranch = "agent/\nissue-9"
			return s
		}()},
		{name: "empty branch", sum: func() forge.MergeRequestSummary {
			s := goodSummary(iid, forge.MRStateMerged)
			s.SourceBranch = ""
			return s
		}()},
		{name: "short sha", sum: func() forge.MergeRequestSummary {
			s := goodSummary(iid, forge.MRStateMerged)
			s.HeadSHA = long
			return s
		}()},
		{name: "uppercase sha", sum: func() forge.MergeRequestSummary {
			s := goodSummary(iid, forge.MRStateMerged)
			s.HeadSHA = strings.ToUpper(activeTestSHA)
			return s
		}()},
		{name: "empty sha", sum: func() forge.MergeRequestSummary {
			s := goodSummary(iid, forge.MRStateMerged)
			s.HeadSHA = ""
			return s
		}()},
		{name: "opened", sum: goodSummary(iid, forge.MRStateOpened)},
		{name: "locked", sum: goodSummary(iid, forge.MRStateLocked)},
		{name: "unknown state", sum: goodSummary(iid, "weird")},
		{name: "empty state", sum: goodSummary(iid, "")},
		{name: "forge error", err: errors.New("boom")},
		{name: "404", err: forge.ErrMergeRequestNotFound},
	}
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				run := activeRow(uuid.New(), kind, iid, activeTestBranch)
				st := &fakeStore{activeTargetRuns: []store.ListActiveMRTargetRunsRow{run}}
				f := &summaryForge{byIID: map[int64]forge.MergeRequestSummary{iid: tc.sum}, errs: map[int64]error{iid: tc.err}}
				c := &fakeTerminalCanceller{}
				svc := newTestService(st)
				svc.SetTerminalTargetCanceller(c)

				if err := svc.SyncActiveMRTargets(context.Background(), uuid.New(), testProjectID, f); err != nil {
					t.Fatalf("SyncActiveMRTargets: %v", err)
				}
				if !tc.wantObs {
					if len(c.obs) != 0 {
						t.Fatalf("canceller called for %s: %+v", tc.name, c.obs)
					}
					return
				}
				if len(c.obs) != 1 {
					t.Fatalf("canceller calls = %d, want 1", len(c.obs))
				}
				want := workersvc.TerminalTargetObservation{
					RunID: run.ID, Kind: kind, Status: "running", WorkerID: run.WorkerID,
					ClaimGeneration: 3, TargetMRIID: iid, SourceBranch: activeTestBranch, State: tc.sum.State,
				}
				if c.obs[0] != want {
					t.Fatalf("observation = %+v, want %+v", c.obs[0], want)
				}
			})
		}
	}
}

func TestSyncActiveMRTargetsOneSummaryPerIID(t *testing.T) {
	a := activeRow(uuid.MustParse("00000000-0000-0000-0000-000000000001"), "ci_fix", 41, activeTestBranch)
	b := activeRow(uuid.MustParse("00000000-0000-0000-0000-000000000002"), "mr_rework", 41, activeTestBranch)
	other := activeRow(uuid.MustParse("00000000-0000-0000-0000-000000000003"), "ci_fix", 42, activeTestBranch)
	st := &fakeStore{activeTargetRuns: []store.ListActiveMRTargetRunsRow{a, b, other}}
	f := &summaryForge{byIID: map[int64]forge.MergeRequestSummary{
		41: goodSummary(41, forge.MRStateMerged), 42: func() forge.MergeRequestSummary {
			s := goodSummary(42, forge.MRStateMerged)
			s.SourceBranch = "other"
			return s
		}(),
	}}
	c := &fakeTerminalCanceller{}
	svc := newTestService(st)
	svc.SetTerminalTargetCanceller(c)

	if err := svc.SyncActiveMRTargets(context.Background(), uuid.New(), testProjectID, f); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("summary calls = %v, want one per distinct iid (41, 42)", f.calls)
	}
	if len(c.obs) != 2 {
		t.Fatalf("cancel calls = %d, want 2 (both runs bound to iid 41)", len(c.obs))
	}
}

func TestSyncActiveMRTargetsCancellerErrorRetriesNextPass(t *testing.T) {
	bad := activeRow(uuid.MustParse("00000000-0000-0000-0000-000000000001"), "ci_fix", 41, activeTestBranch)
	good := activeRow(uuid.MustParse("00000000-0000-0000-0000-000000000002"), "ci_fix", 41, activeTestBranch)
	st := &fakeStore{activeTargetRuns: []store.ListActiveMRTargetRunsRow{bad, good}}
	f := &summaryForge{byIID: map[int64]forge.MergeRequestSummary{41: goodSummary(41, forge.MRStateMerged)}}
	c := &fakeTerminalCanceller{errs: map[uuid.UUID]error{bad.ID: errors.New("tx failed")}}
	svc := newTestService(st)
	svc.SetTerminalTargetCanceller(c)

	for pass := 1; pass <= 2; pass++ {
		if err := svc.SyncActiveMRTargets(context.Background(), uuid.New(), testProjectID, f); err != nil {
			t.Fatalf("pass %d: canceller error must not fail the pass: %v", pass, err)
		}
	}
	// Each pass reaches both runs: the failing one does not block its sibling and is retried.
	counts := map[uuid.UUID]int{}
	for _, o := range c.obs {
		counts[o.RunID]++
	}
	if counts[bad.ID] != 2 || counts[good.ID] != 2 {
		t.Fatalf("calls per run = %v, want 2 each", counts)
	}
}

func TestSyncActiveMRTargetsEnumerationErrorIsReturned(t *testing.T) {
	st := &fakeStore{activeTargetErr: errors.New("db down")}
	svc := newTestService(st)
	svc.SetTerminalTargetCanceller(&fakeTerminalCanceller{})
	if err := svc.SyncActiveMRTargets(context.Background(), uuid.New(), testProjectID, &summaryForge{}); err == nil {
		t.Fatal("want the enumeration error returned")
	}
}

func TestSyncActiveMRTargetsNoCancellerIsNoop(t *testing.T) {
	st := &fakeStore{activeTargetErr: errors.New("must not be queried")}
	svc := newTestService(st)
	if err := svc.SyncActiveMRTargets(context.Background(), uuid.New(), testProjectID, &summaryForge{}); err != nil {
		t.Fatalf("unset canceller must skip silently: %v", err)
	}
	if len(st.activeTargetArgs) != 0 {
		t.Fatal("store queried without a canceller")
	}
	var typedNil *workersvc.Service
	svc.SetTerminalTargetCanceller(typedNil)
	if svc.terminalTargetCanceller != nil {
		t.Fatal("typed-nil canceller must read as unset")
	}
}

func TestSyncActiveMRTargetsCursorRotatesPastPageSize(t *testing.T) {
	const total = 40
	var runs []store.ListActiveMRTargetRunsRow
	f := &summaryForge{byIID: map[int64]forge.MergeRequestSummary{}}
	for i := 0; i < total; i++ {
		id := uuid.UUID{}
		id[0] = byte(i + 1)
		iid := int64(100 + i)
		runs = append(runs, activeRow(id, "ci_fix", iid, activeTestBranch))
		f.byIID[iid] = goodSummary(iid, forge.MRStateOpened) // opened: no cancel, only rotation
	}
	st := &fakeStore{activeTargetRuns: runs}
	svc := newTestService(st)
	svc.SetTerminalTargetCanceller(&fakeTerminalCanceller{})
	repo := uuid.New()

	pass := func() []int64 {
		f.calls = nil
		if err := svc.SyncActiveMRTargets(context.Background(), repo, testProjectID, f); err != nil {
			t.Fatal(err)
		}
		return append([]int64(nil), f.calls...)
	}

	p1 := pass()
	if len(p1) != 32 || p1[0] != 100 || p1[31] != 131 {
		t.Fatalf("pass 1 covered %d (%v), want 32 runs 100..131", len(p1), p1)
	}
	p2 := pass()
	if len(p2) != 8 || p2[0] != 132 || p2[7] != 139 {
		t.Fatalf("pass 2 covered %d (%v), want the remaining 8 runs 132..139", len(p2), p2)
	}
	p3 := pass()
	if len(p3) != 32 || p3[0] != 100 {
		t.Fatalf("pass 3 covered %d (%v), want the cursor wrapped to the first 32", len(p3), p3)
	}
	if st.activeTargetArgs[1].AfterID != runs[31].ID || st.activeTargetArgs[2].AfterID != (uuid.UUID{}) {
		t.Fatalf("cursor args = %+v", st.activeTargetArgs)
	}
}
