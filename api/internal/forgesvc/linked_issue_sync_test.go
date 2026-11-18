package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// linkedSyncStore is a fakeStore whose owner-scoped repo row is scripted, so the
// linked sync reads its project short name and eligibility tag name as in production.
type linkedSyncStore struct {
	*fakeStore
	row       store.GetRepoIssueSourceForUserRow
	rowErr    error
	upsertErr error
	gotUser   uuid.UUID
}

func (s *linkedSyncStore) GetRepoIssueSourceForUser(_ context.Context, a store.GetRepoIssueSourceForUserParams) (store.GetRepoIssueSourceForUserRow, error) {
	s.gotUser = a.UserID
	return s.row, s.rowErr
}

func (s *linkedSyncStore) UpsertIssue(ctx context.Context, arg store.UpsertIssueParams) (store.Issue, error) {
	if s.upsertErr != nil {
		return store.Issue{}, s.upsertErr
	}
	return s.fakeStore.UpsertIssue(ctx, arg)
}

func (s *linkedSyncStore) UpsertIssueIfChanged(ctx context.Context, arg store.UpsertIssueIfChangedParams) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	return s.fakeStore.UpsertIssueIfChanged(ctx, arg)
}

func newLinkedSyncStore() *linkedSyncStore {
	return &linkedSyncStore{fakeStore: &fakeStore{}, row: store.GetRepoIssueSourceForUserRow{
		IssueTrackerProjectShort: pgtype.Text{String: "ABC", Valid: true},
		EligibilityTagName:       pgtype.Text{String: "uzi-ready", Valid: true},
	}}
}

// linkedTracker is the issue side of a linked repo: a forge fake behind the write guard
// the resolver puts on every linked driver.
func linkedTracker(f *fakeForge) *issueWriteGuard { return &issueWriteGuard{inner: f} }

func TestSyncIssuesDispatchesOnTheLink(t *testing.T) {
	marks := Marks{PRD: time.Unix(100, 0)}
	user, repo := uuid.New(), uuid.New()

	t.Run("linked full: lists the eligibility tag only, writes issue_key, evicts the rest", func(t *testing.T) {
		f := &fakeForge{issues: []forge.Issue{issueAt(7, recordedMarkBase), issueAt(12, recordedMarkBase.Add(time.Minute))}}
		st := newLinkedSyncStore()
		got, err := newTestService(st).SyncIssues(context.Background(), user, repo, testProjectID, linkedTracker(f))
		if err != nil {
			t.Fatal(err)
		}
		if len(f.listCalls) != 1 || len(f.listCalls[0].Labels) != 1 || f.listCalls[0].Labels[0] != "uzi-ready" || f.listCalls[0].UpdatedAfter != nil {
			t.Fatalf("list calls = %+v, want one call for the eligibility tag with no UpdatedAfter", f.listCalls)
		}
		if st.gotUser != user {
			t.Fatalf("repo row read for user %s, want the owner %s", st.gotUser, user)
		}
		if len(st.upserts) != 2 || st.upserts[0].IssueKey.String != "ABC-7" || !st.upserts[0].IssueKey.Valid || st.upserts[1].IssueKey.String != "ABC-12" {
			t.Fatalf("upserts = %+v, want issue_key ABC-7 and ABC-12", st.upserts)
		}
		if st.changedUpserts != 2 {
			t.Fatalf("changed-only upserts = %d, want both writes through UpsertIssueIfChanged", st.changedUpserts)
		}
		if len(st.deleteCalls) != 1 || len(st.deleteCalls[0].KeepIids) != 2 {
			t.Fatalf("delete calls = %+v, want one eviction keeping both", st.deleteCalls)
		}
		if !got.PRD.Equal(recordedMarkBase.Add(time.Minute)) {
			t.Fatalf("marks = %+v, want PRD advanced to the newest updated time", got)
		}
	})

	t.Run("linked incremental: same listing, never evicts, ignores marks later than updated", func(t *testing.T) {
		f := &fakeForge{issues: []forge.Issue{issueAt(7, time.Unix(50, 0))}}
		st := newLinkedSyncStore()
		got, err := newTestService(st).SyncIssuesIncremental(context.Background(), user, repo, testProjectID, linkedTracker(f), marks)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.listCalls) != 1 || f.listCalls[0].UpdatedAfter != nil {
			t.Fatalf("list calls = %+v, want one call with no UpdatedAfter", f.listCalls)
		}
		if len(st.upserts) != 1 || st.upserts[0].ForgeIssueIid != 7 {
			t.Fatalf("upserts = %+v, want the issue older than the marks cached", st.upserts)
		}
		if len(st.deleteCalls) != 0 {
			t.Fatalf("an incremental linked sync evicted: %+v", st.deleteCalls)
		}
		if got != marks {
			t.Fatalf("marks = %+v, want %+v (never lowered, nothing newer returned)", got, marks)
		}
	})

	t.Run("linked: a failure before the writes finish evicts nothing and keeps the marks", func(t *testing.T) {
		boom := errors.New("boom")
		cases := map[string]func(*fakeForge, *linkedSyncStore){
			"listing":  func(f *fakeForge, _ *linkedSyncStore) { f.listErr = boom },
			"row read": func(_ *fakeForge, s *linkedSyncStore) { s.rowErr = boom },
			"upsert":   func(_ *fakeForge, s *linkedSyncStore) { s.upsertErr = boom },
		}
		for name, arm := range cases {
			t.Run(name, func(t *testing.T) {
				f := &fakeForge{issues: []forge.Issue{issueAt(7, recordedMarkBase)}}
				st := newLinkedSyncStore()
				arm(f, st)
				svc := newTestService(st)
				if _, err := svc.SyncIssues(context.Background(), user, repo, testProjectID, linkedTracker(f)); !errors.Is(err, boom) {
					t.Fatalf("SyncIssues error = %v, want the cause", err)
				}
				got, err := svc.SyncIssuesIncremental(context.Background(), user, repo, testProjectID, linkedTracker(f), marks)
				if !errors.Is(err, boom) || got != marks {
					t.Fatalf("SyncIssuesIncremental = (%+v, %v), want the cause and the marks kept", got, err)
				}
				if len(st.deleteCalls) != 0 {
					t.Fatalf("a failed sync evicted: %+v", st.deleteCalls)
				}
			})
		}
	})

	t.Run("linked: a row with no link fields is unresolvable, not a forge listing", func(t *testing.T) {
		f := &fakeForge{}
		st := newLinkedSyncStore()
		st.row = store.GetRepoIssueSourceForUserRow{}
		if _, err := newTestService(st).SyncIssues(context.Background(), user, repo, testProjectID, linkedTracker(f)); !errors.Is(err, ErrLinkedRepoUnresolvable) {
			t.Fatalf("error = %v, want ErrLinkedRepoUnresolvable", err)
		}
		if len(f.listCalls) != 0 || len(st.deleteCalls) != 0 {
			t.Fatalf("calls = %+v, deletes = %+v", f.listCalls, st.deleteCalls)
		}
	})

	t.Run("unowned repo row is an error and evicts nothing", func(t *testing.T) {
		f := &fakeForge{}
		st := newLinkedSyncStore()
		st.rowErr = pgx.ErrNoRows
		if _, err := newTestService(st).SyncIssues(context.Background(), user, repo, testProjectID, linkedTracker(f)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("error = %v", err)
		}
		if len(st.deleteCalls) != 0 {
			t.Fatalf("evicted: %+v", st.deleteCalls)
		}
	})

	t.Run("unlinked: the forge sync, same calls", func(t *testing.T) {
		viaDispatcher := &fakeForge{issues: []forge.Issue{issueAt(1, recordedMarkBase)}}
		direct := &fakeForge{issues: []forge.Issue{issueAt(1, recordedMarkBase)}}
		if _, err := newTestService(&fakeStore{}).SyncIssues(context.Background(), uuid.New(), uuid.New(), testProjectID, viaDispatcher); err != nil {
			t.Fatal(err)
		}
		if _, err := newTestService(&fakeStore{}).FullSync(context.Background(), uuid.New(), testProjectID, direct); err != nil {
			t.Fatal(err)
		}
		if len(viaDispatcher.listCalls) == 0 || len(viaDispatcher.listCalls) != len(direct.listCalls) {
			t.Fatalf("dispatcher made %d list calls, direct FullSync %d", len(viaDispatcher.listCalls), len(direct.listCalls))
		}
	})
}
