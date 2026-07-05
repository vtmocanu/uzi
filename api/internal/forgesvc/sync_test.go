package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeForge is a mocked Forge whose ListIssues is scripted; other methods are
// unused by the sync paths under test.
type fakeForge struct {
	issues    []forge.Issue
	listErr   error
	listCalls []forge.ListIssuesOptions
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
func (f *fakeForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (f *fakeForge) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{}, nil
}
func (f *fakeForge) ProjectRole(context.Context, int64, int64) (int, bool, error) {
	return 0, false, nil
}
func (f *fakeForge) DefaultBranchProtection(context.Context, int64, string) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, nil
}

// fakeStore records what the sync writes, standing in for *store.Queries.
type fakeStore struct {
	upserts     []store.UpsertIssueParams
	deleteCalls []store.DeleteIssuesNotInParams
}

func (s *fakeStore) UpsertIssue(_ context.Context, arg store.UpsertIssueParams) (store.Issue, error) {
	s.upserts = append(s.upserts, arg)
	return store.Issue{}, nil
}
func (s *fakeStore) DeleteIssuesNotIn(_ context.Context, arg store.DeleteIssuesNotInParams) (int64, error) {
	s.deleteCalls = append(s.deleteCalls, arg)
	return 0, nil
}

func newTestService(st IssueStore) *Service {
	// box is nil: FullSync/IncrementalSync operate on a passed-in Forge and
	// never touch the encryption box.
	return New(st, nil, time.Second)
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
	if len(f.listCalls[0].Labels) != 1 || f.listCalls[0].Labels[0] != PRDLabel {
		t.Fatalf("expected PRD label filter, got %v", f.listCalls[0].Labels)
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
