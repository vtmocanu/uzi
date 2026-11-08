package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// bindingAnswer is one scripted GetMergeRequestSummary answer.
type bindingAnswer struct {
	sum   forge.MergeRequestSummary
	err   error
	block bool // wait for ctx.Done and return its error
	hook  func()
}

// bindingForge scripts GetMergeRequestSummary per iid and counts calls. It is shared by
// the unit tests here and the LiveDB tests in this package.
type bindingForge struct {
	forgetest.BaseFake
	mu      sync.Mutex
	answers map[int64]bindingAnswer
	calls   int
}

func (f *bindingForge) GetMergeRequestSummary(ctx context.Context, _, iid int64) (forge.MergeRequestSummary, error) {
	f.mu.Lock()
	f.calls++
	a, ok := f.answers[iid]
	f.mu.Unlock()
	if !ok {
		return forge.MergeRequestSummary{}, forge.ErrMergeRequestNotFound
	}
	if a.hook != nil {
		a.hook()
	}
	if a.block {
		<-ctx.Done()
		return forge.MergeRequestSummary{}, ctx.Err()
	}
	return a.sum, a.err
}

func (f *bindingForge) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// mergedEverywhereForge answers every PR as merged, so a ci_fix create that finds recorded
// PRs on its branch ends unbound. Existing LiveDB tests that only need the create to succeed
// inject it.
type mergedEverywhereForge struct{ forgetest.BaseFake }

func (*mergedEverywhereForge) GetMergeRequestSummary(_ context.Context, _, iid int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{IID: iid, State: forge.MRStateMerged, SourceBranch: "merged"}, nil
}

type bindingBuilder struct{ f forge.Forge }

func (b bindingBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

func bindSum(iid int64, state, branch string) bindingAnswer {
	return bindingAnswer{sum: forge.MergeRequestSummary{IID: iid, State: state, SourceBranch: branch}}
}

const bindRef = "agent/issue-77"

func bindingSvc(t *testing.T, cands func() ([]int64, error), f forge.Forge) (*Service, *fakeStore) {
	t.Helper()
	fs := &fakeStore{ciFixRunResult: store.Run{ID: uuid.New(), Kind: runkind.CIFix}, ciFixCandidatesFn: cands}
	svc := New(fs, newBox(t), testParams())
	if f != nil {
		svc.SetForges(bindingBuilder{f})
	}
	return svc, fs
}

func fixedCands(ids ...int64) func() ([]int64, error) {
	return func() ([]int64, error) { return append([]int64{}, ids...), nil }
}

func TestCIFixBindingClassification(t *testing.T) {
	opened := func(iid int64) bindingAnswer { return bindSum(iid, forge.MRStateOpened, bindRef) }
	tests := []struct {
		name       string
		cands      []int64
		answers    map[int64]bindingAnswer
		wantBound  int64 // 0 = unbound
		wantErr    error // errors.Is target
		wantReason string
	}{
		{"opened on same branch binds", []int64{5}, map[int64]bindingAnswer{5: opened(5)}, 5, nil, ""},
		{"merged is not open", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, forge.MRStateMerged, bindRef)}, 0, nil, ""},
		{"closed is not open", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, forge.MRStateClosed, bindRef)}, 0, nil, ""},
		{"locked is unavailable", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, forge.MRStateLocked, bindRef)}, 0, ErrCIFixPRUnavailable, ""},
		{"iid mismatch", []int64{5}, map[int64]bindingAnswer{5: opened(6)}, 0, ErrCIFixPRUnresolved, "pull request !5 returned an unexpected answer"},
		{"empty state", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, "", bindRef)}, 0, ErrCIFixPRUnresolved, "pull request !5 returned an unexpected answer"},
		{"empty head branch", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, forge.MRStateOpened, "")}, 0, ErrCIFixPRUnresolved, "pull request !5 returned an unexpected answer"},
		{"unknown state", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, "draft?", bindRef)}, 0, ErrCIFixPRUnresolved, "pull request !5 returned an unexpected answer"},
		{"opened on other branch", []int64{5}, map[int64]bindingAnswer{5: bindSum(5, forge.MRStateOpened, "other")}, 0, ErrCIFixPRUnresolved, "pull request !5 is open on a different branch"},
		{"404", []int64{5}, map[int64]bindingAnswer{5: {err: forge.ErrMergeRequestNotFound}}, 0, ErrCIFixPRUnresolved, "pull request !5 was not found"},
		{"401 is unavailable", []int64{5}, map[int64]bindingAnswer{5: {err: errors.New("forge: 401 unauthorized")}}, 0, ErrCIFixPRUnavailable, ""},
		{"transport error is unavailable", []int64{5}, map[int64]bindingAnswer{5: {err: errors.New("dial tcp: connection refused")}}, 0, ErrCIFixPRUnavailable, ""},
		{"two open", []int64{5, 6}, map[int64]bindingAnswer{5: opened(5), 6: opened(6)}, 0, ErrCIFixPRUnresolved, "more than one open pull request uses this branch"},
		{"open and merged binds the open", []int64{5, 6}, map[int64]bindingAnswer{5: bindSum(5, forge.MRStateMerged, bindRef), 6: opened(6)}, 6, nil, ""},
		{"open, open, transport error is unavailable", []int64{5, 6, 7}, map[int64]bindingAnswer{5: opened(5), 6: opened(6), 7: {err: errors.New("boom")}}, 0, ErrCIFixPRUnavailable, ""},
		{"unresolvable then unavailable is unavailable", []int64{5, 6}, map[int64]bindingAnswer{5: {err: forge.ErrMergeRequestNotFound}, 6: {err: errors.New("boom")}}, 0, ErrCIFixPRUnavailable, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ff := &bindingForge{answers: tc.answers}
			svc, fs := bindingSvc(t, fixedCands(tc.cands...), ff)
			_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if tc.wantReason != "" {
					var u *CIFixPRUnresolvedError
					if !errors.As(err, &u) || u.Reason != tc.wantReason {
						t.Fatalf("err = %v, want reason %q", err, tc.wantReason)
					}
				}
				if fs.ciFixRunParams != nil {
					t.Fatal("a refused binding must not insert a run")
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateCIFixRun: %v", err)
			}
			got := fs.ciFixRunParams.CiFixMrIid
			if tc.wantBound == 0 && got.Valid {
				t.Fatalf("CiFixMrIid = %d, want unbound", got.Int64)
			}
			if tc.wantBound != 0 && (!got.Valid || got.Int64 != tc.wantBound) {
				t.Fatalf("CiFixMrIid = %+v, want %d", got, tc.wantBound)
			}
		})
	}
}

func TestCIFixBindingRefusesInvalidCandidatesWithoutForgeCalls(t *testing.T) {
	nine := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9}
	for name, cands := range map[string][]int64{
		"overflow":      nine,
		"zero id":       {0},
		"negative id":   {-3},
		"above 2^53-1":  {9007199254740992},
		"valid+invalid": {4, 0},
	} {
		t.Run(name, func(t *testing.T) {
			ff := &bindingForge{answers: map[int64]bindingAnswer{}}
			svc, fs := bindingSvc(t, fixedCands(cands...), ff)
			_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
			if !errors.Is(err, ErrCIFixPRUnresolved) {
				t.Fatalf("err = %v, want ErrCIFixPRUnresolved", err)
			}
			if ff.callCount() != 0 || fs.ciFixRunParams != nil {
				t.Fatalf("forge calls = %d, insert = %v; want none", ff.callCount(), fs.ciFixRunParams != nil)
			}
		})
	}
}

func TestCIFixBindingForgeTimeoutIsUnavailable(t *testing.T) {
	old := ciFixBindingLookupBudget
	ciFixBindingLookupBudget = 50 * time.Millisecond
	t.Cleanup(func() { ciFixBindingLookupBudget = old })

	ff := &bindingForge{answers: map[int64]bindingAnswer{
		5: bindSum(5, forge.MRStateOpened, bindRef),
		6: bindSum(6, forge.MRStateOpened, bindRef),
		7: {block: true},
	}}
	svc, fs := bindingSvc(t, fixedCands(5, 6, 7), ff)
	_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
	if !errors.Is(err, ErrCIFixPRUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want unavailable wrapping DeadlineExceeded", err)
	}
	if fs.ciFixRunParams != nil {
		t.Fatal("a timed-out binding must not insert a run")
	}
}

func TestCIFixBindingNilForgesWithCandidatesIsUnavailable(t *testing.T) {
	svc, _ := bindingSvc(t, fixedCands(5), nil)
	_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
	if !errors.Is(err, ErrCIFixPRUnavailable) {
		t.Fatalf("err = %v, want ErrCIFixPRUnavailable", err)
	}
}

func TestCIFixBindingCandidateReadFailureIsUnavailable(t *testing.T) {
	svc, _ := bindingSvc(t, func() ([]int64, error) { return nil, context.DeadlineExceeded }, nil)
	_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
	if !errors.Is(err, ErrCIFixPRUnavailable) {
		t.Fatalf("err = %v, want ErrCIFixPRUnavailable", err)
	}
}

func TestCIFixBindingNoCandidatesIsUnboundWithoutForge(t *testing.T) {
	svc, fs := bindingSvc(t, fixedCands(), nil) // no forge builder at all
	if _, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil); err != nil {
		t.Fatalf("CreateCIFixRun: %v", err)
	}
	if fs.ciFixRunParams.CiFixMrIid.Valid {
		t.Fatalf("CiFixMrIid = %+v, want unbound", fs.ciFixRunParams.CiFixMrIid)
	}
}

func TestCIFixBindingChangedBetweenCheckAndInsert(t *testing.T) {
	ff := &bindingForge{answers: map[int64]bindingAnswer{5: bindSum(5, forge.MRStateOpened, bindRef)}}
	reads := 0
	svc, fs := bindingSvc(t, func() ([]int64, error) {
		reads++
		if reads == 1 {
			return []int64{5}, nil
		}
		return []int64{9, 5}, nil // a new PR was recorded after the forge check
	}, ff)
	_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
	if !errors.Is(err, ErrCIFixBindingChanged) {
		t.Fatalf("err = %v, want ErrCIFixBindingChanged", err)
	}
	if fs.ciFixRunParams != nil {
		t.Fatal("a changed binding must not insert a run")
	}
}

func TestCIFixBindingCandidateOrderDoesNotCountAsChange(t *testing.T) {
	ff := &bindingForge{answers: map[int64]bindingAnswer{
		5: bindSum(5, forge.MRStateMerged, bindRef),
		6: bindSum(6, forge.MRStateOpened, bindRef),
	}}
	reads := 0
	svc, fs := bindingSvc(t, func() ([]int64, error) {
		reads++
		if reads == 1 {
			return []int64{6, 5}, nil
		}
		return []int64{5, 6}, nil
	}, ff)
	if _, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil); err != nil {
		t.Fatalf("CreateCIFixRun: %v", err)
	}
	if got := fs.ciFixRunParams.CiFixMrIid; !got.Valid || got.Int64 != 6 {
		t.Fatalf("CiFixMrIid = %+v, want 6", got)
	}
}

func TestCIFixPrecheckBeatsForgeError(t *testing.T) {
	ff := &bindingForge{answers: map[int64]bindingAnswer{5: {err: errors.New("boom")}}}
	svc, fs := bindingSvc(t, fixedCands(5), ff)
	fs.activeBranchRuns = 1
	_, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), bindRef, "t", "d", sampleSnapshot(), nil)
	if err != ErrBranchInUse {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
	if ff.callCount() != 0 {
		t.Fatalf("forge called %d times before the pool pre-check refused", ff.callCount())
	}
}

func TestCIFixUnresolvedErrorMessageHasNoForgeText(t *testing.T) {
	err := error(&CIFixPRUnresolvedError{Reason: "pull request !5 was not found"})
	if !strings.Contains(err.Error(), "pull request !5 was not found") || !errors.Is(err, ErrCIFixPRUnresolved) {
		t.Fatalf("unexpected error %q", err)
	}
}
