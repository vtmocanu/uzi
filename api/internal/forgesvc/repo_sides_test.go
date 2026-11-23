package forgesvc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type sidesFake struct{ forgetest.BaseFake }

// GetRepoIssueSourceForUser satisfies IssueStore for the sync fakes that never
// resolve a repo side.
func (*fakeStore) GetRepoIssueSourceForUser(context.Context, store.GetRepoIssueSourceForUserParams) (store.GetRepoIssueSourceForUserRow, error) {
	return store.GetRepoIssueSourceForUserRow{}, pgx.ErrNoRows
}

// sourceStore serves the one owner-scoped row the resolvers read, keyed by repo id,
// and answers pgx.ErrNoRows for any other user like the real query.
type sourceStore struct {
	IssueStore // nil: the resolvers use nothing else
	owner      map[uuid.UUID]uuid.UUID
	rows       map[uuid.UUID]store.GetRepoIssueSourceForUserRow
}

func (s *sourceStore) GetRepoIssueSourceForUser(_ context.Context, a store.GetRepoIssueSourceForUserParams) (store.GetRepoIssueSourceForUserRow, error) {
	row, ok := s.rows[a.ID]
	if !ok || s.owner[a.ID] != a.UserID {
		return store.GetRepoIssueSourceForUserRow{}, pgx.ErrNoRows
	}
	return row, nil
}

// stubDriver is the issue tracker driver the fake driver source hands out. It embeds
// the interface so only the methods a test drives exist; a write that reaches it is
// recorded.
type stubDriver struct {
	issuetracker.Driver
	reads, writes []string
	getIssueErr   error
}

func (d *stubDriver) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	d.reads = append(d.reads, "GetIssue")
	return forge.Issue{}, d.getIssueErr
}
func (d *stubDriver) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	d.reads = append(d.reads, "ListIssues")
	return nil, nil
}
func (d *stubDriver) EnsureLabels(context.Context, int64, []forge.Label) error {
	d.writes = append(d.writes, "EnsureLabels")
	return nil
}
func (d *stubDriver) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	d.writes = append(d.writes, "UpdateIssueLabels")
	return nil
}
func (d *stubDriver) UpdateIssueDescription(context.Context, int64, int64, string) error {
	d.writes = append(d.writes, "UpdateIssueDescription")
	return nil
}
func (d *stubDriver) MoveToColumn(context.Context, int64, int64, forge.ColumnMove) error {
	d.writes = append(d.writes, "MoveToColumn")
	return nil
}
func (d *stubDriver) ListIssuesWithState(context.Context, int64, forge.ListIssuesOptions) ([]issuesource.StatedIssue, error) {
	d.reads = append(d.reads, "ListIssuesWithState")
	return []issuesource.StatedIssue{{Issue: forge.Issue{IID: 3}, State: "In Progress"}}, nil
}
func (d *stubDriver) SetIssueState(context.Context, int64, int64, forge.IssueState) error {
	d.writes = append(d.writes, "SetIssueState")
	return nil
}
func (d *stubDriver) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	d.writes = append(d.writes, "CreateIssue")
	return forge.Issue{}, nil
}
func (d *stubDriver) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	d.writes = append(d.writes, "CreateIssueNote")
	return forge.IssueNote{}, nil
}

type fakeDrivers struct {
	driver           *stubDriver
	err              error
	calls            int
	gotUser, gotConn uuid.UUID
	gotBinding       issuetracker.Binding
	gotStates        issuetracker.StateMapping
}

func (f *fakeDrivers) DriverFor(_ context.Context, userID, connID uuid.UUID, b issuetracker.Binding, states issuetracker.StateMapping) (issuetracker.Driver, error) {
	f.calls++
	f.gotUser, f.gotConn, f.gotBinding = userID, connID, b
	f.gotStates = states
	if f.err != nil {
		return nil, f.err
	}
	return f.driver, nil
}

type sidesHarness struct {
	svc      *Service
	rec      *forgetest.Recorder
	drivers  *fakeDrivers
	user     uuid.UUID
	built    int
	builtURL string
}

// newSidesHarness builds a Service whose forge builder returns a recorder, and one
// repo row per call of addRepo.
func newSidesHarness(t *testing.T) (*sidesHarness, *sourceStore) {
	t.Helper()
	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	h := &sidesHarness{
		rec:     forgetest.NewRecorder(&sidesFake{}),
		drivers: &fakeDrivers{driver: &stubDriver{}},
		user:    uuid.New(),
	}
	src := &sourceStore{owner: map[uuid.UUID]uuid.UUID{}, rows: map[uuid.UUID]store.GetRepoIssueSourceForUserRow{}}
	h.svc = NewWithForgeBuilder(src, box, time.Second, nil, func(_ forge.Type, u, _ string, _ time.Duration) (forge.Forge, error) {
		h.built++
		h.builtURL = u
		return h.rec, nil
	})
	return h, src
}

func (h *sidesHarness) addRepo(t *testing.T, src *sourceStore, owner uuid.UUID, link func(*store.GetRepoIssueSourceForUserRow)) uuid.UUID {
	t.Helper()
	sealed, err := h.svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	row := store.GetRepoIssueSourceForUserRow{
		ID: id, ForgeProjectID: 4242, ForgeType: string(forge.TypeGitLab),
		BaseUrl: "https://forge.example.test", TokenCiphertext: sealed,
	}
	if link != nil {
		link(&row)
	}
	src.rows[id] = row
	src.owner[id] = owner
	return id
}

func linkTo(conn uuid.UUID, resolved bool) func(*store.GetRepoIssueSourceForUserRow) {
	return func(r *store.GetRepoIssueSourceForUserRow) {
		r.IssueTrackerConnectionID = pgtype.UUID{Bytes: conn, Valid: true}
		r.IssueTrackerProjectID = pgtype.Text{String: "0-7", Valid: true}
		r.IssueTrackerProjectShort = pgtype.Text{String: "ABC", Valid: true}
		r.EligibilityTagID = pgtype.Text{String: "5-1", Valid: true}
		r.EligibilityTagName = pgtype.Text{String: "uzi", Valid: true}
		r.LinkedConnectionID = pgtype.UUID{Bytes: conn, Valid: resolved}
	}
}

// writeAll drives every write the issue side offers and returns how many were
// accepted (returned no error).
func writeAll(ctx context.Context, tr forge.IssueTracker, projectID int64) (accepted int, errs []error) {
	ops := []func() error{
		func() error { return tr.UpdateIssueLabels(ctx, projectID, 9, []string{"a"}, nil) },
		func() error { _, err := tr.CreateIssueNote(ctx, projectID, 9, "note"); return err },
		func() error { _, err := tr.CreateIssue(ctx, projectID, "t", "d", nil); return err },
		func() error { return tr.SetIssueState(ctx, projectID, 9, forge.StateClosed) },
		func() error { return tr.MoveToColumn(ctx, projectID, 9, forge.ColumnMove{Column: "In Progress"}) },
		func() error { return tr.UpdateIssueDescription(ctx, projectID, 9, "d") },
		func() error { return tr.EnsureLabels(ctx, projectID, []forge.Label{{Name: "x"}}) },
	}
	for _, op := range ops {
		if err := op(); err != nil {
			errs = append(errs, err)
			continue
		}
		accepted++
	}
	return accepted, errs
}

// The resolver-level recorded test (PRD #4173 M4): a repo that was never linked
// reaches the forge for every issue write, exactly as before.
func TestIssueTrackerForRepoUnlinkedWritesReachTheForge(t *testing.T) {
	h, src := newSidesHarness(t)
	repo := h.addRepo(t, src, h.user, nil)

	tr, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo)
	if err != nil {
		t.Fatal(err)
	}
	// BaseFake answers its writes with a not-stubbed error; what matters is that each
	// call reached the recorder (the forge), and that no guard answered first.
	_, errs := writeAll(context.Background(), tr, 4242)
	for _, e := range errs {
		if errors.Is(e, ErrIssueWriteNotOffered) {
			t.Fatalf("an unlinked repo's write was refused by the guard: %v", e)
		}
	}
	// The six other writes are one call each; the move is logged in Moves() and reaches
	// the recorder as its one label call (forge.MoveByLabels), seven calls in all.
	if got := len(h.rec.Calls()); got != 7 {
		t.Fatalf("recorder saw %d calls, want 7: %v", got, h.rec.Calls())
	}
	if got := len(h.rec.Moves()); got != 1 {
		t.Fatalf("recorder saw %d moves, want 1: %v", got, h.rec.Moves())
	}
	if h.drivers.calls != 0 {
		t.Fatalf("the issue tracker driver source was used for an unlinked repo")
	}
	if tr != forge.IssueTracker(h.rec) {
		t.Fatalf("an unlinked repo's issue side must be the forge driver itself, got %T", tr)
	}
}

func TestIssueTrackerForRepoLinkedBuildsTheBoundDriverBehindTheGuard(t *testing.T) {
	h, src := newSidesHarness(t)
	h.svc.SetIssueTrackerDrivers(h.drivers)
	conn := uuid.New()
	repo := h.addRepo(t, src, h.user, linkTo(conn, true))

	tr, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo)
	if err != nil {
		t.Fatal(err)
	}
	d := h.drivers
	want := issuetracker.Binding{
		Project:        issuetracker.ProjectRef{ID: "0-7", ShortName: "ABC"},
		ProjectID:      4242,
		EligibilityTag: issuetracker.TagRef{ID: "5-1", Name: "uzi"},
	}
	if d.calls != 1 || d.gotUser != h.user || d.gotConn != conn || d.gotBinding != want {
		t.Fatalf("DriverFor got (calls %d, user %v, conn %v, binding %+v), want the repo row's binding for its owner", d.calls, d.gotUser, d.gotConn, d.gotBinding)
	}
	if d.gotStates.Field != "" || d.gotStates.Open != "" || d.gotStates.Closed != "" || len(d.gotStates.Columns) != 0 {
		t.Errorf("a repo with no saved state mapping must pass the zero mapping, got %+v", d.gotStates)
	}
	if h.built != 0 {
		t.Errorf("a linked repo's issue side must not build the forge driver")
	}

	// Every write is offered (PRD #4173 M5, U6 lifts the note and description); only the
	// eligibility-tag removal is refused, below.
	accepted, errs := writeAll(context.Background(), tr, 4242)
	if accepted != 7 || len(errs) != 0 {
		t.Fatalf("accepted %d writes with errors %v, want all 7 accepted", accepted, errs)
	}
	wantWrites := []string{"UpdateIssueLabels", "CreateIssueNote", "CreateIssue", "SetIssueState", "MoveToColumn", "UpdateIssueDescription", "EnsureLabels"}
	if !slices.Equal(d.driver.writes, wantWrites) {
		t.Fatalf("writes that reached the driver = %v, want %v", d.driver.writes, wantWrites)
	}
	// The guard was built with the row's stored eligibility name ("uzi"): removing it is
	// refused before the driver, adding it is not.
	if err := tr.UpdateIssueLabels(context.Background(), 4242, 9, nil, []string{"uzi"}); !errors.Is(err, ErrIssueWriteNotOffered) {
		t.Fatalf("removing the eligibility name: %v, want ErrIssueWriteNotOffered", err)
	}
	if err := tr.UpdateIssueLabels(context.Background(), 4242, 9, []string{"uzi"}, nil); err != nil {
		t.Fatalf("adding the eligibility name: %v", err)
	}
	if len(d.driver.writes) != len(wantWrites)+1 {
		t.Fatalf("writes that reached the driver = %v, want only the add after the refused removal", d.driver.writes)
	}
	if _, err := tr.ListIssues(context.Background(), 4242, forge.ListIssuesOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(d.driver.reads) != 1 {
		t.Fatalf("reads must pass through, got %v", d.driver.reads)
	}
}

// The stored open, closed and column States of the repo row reach DriverFor (PRD #4173 M5);
// the State field stays empty so the driver's default applies.
func TestIssueTrackerForRepoPassesTheStoredStateMapping(t *testing.T) {
	h, src := newSidesHarness(t)
	h.svc.SetIssueTrackerDrivers(h.drivers)
	repo := h.addRepo(t, src, h.user, func(r *store.GetRepoIssueSourceForUserRow) {
		linkTo(uuid.New(), true)(r)
		r.OpenState = pgtype.Text{String: "To Do", Valid: true}
		r.ClosedState = pgtype.Text{String: "Done", Valid: true}
		r.ColumnStates = []byte(`{"In Progress":"Doing","Review":"In Review"}`)
	})
	if _, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo); err != nil {
		t.Fatal(err)
	}
	got := h.drivers.gotStates
	if got.Field != "" || got.Open != "To Do" || got.Closed != "Done" ||
		len(got.Columns) != 2 || got.Columns["In Progress"] != "Doing" || got.Columns["Review"] != "In Review" {
		t.Fatalf("DriverFor got %+v, want the stored mapping", got)
	}
}

// A malformed stored column map fails closed rather than building a driver without it.
func TestIssueTrackerForRepoRefusesAMalformedColumnMap(t *testing.T) {
	h, src := newSidesHarness(t)
	h.svc.SetIssueTrackerDrivers(h.drivers)
	repo := h.addRepo(t, src, h.user, func(r *store.GetRepoIssueSourceForUserRow) {
		linkTo(uuid.New(), true)(r)
		r.ColumnStates = []byte(`[1]`)
	})
	if _, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo); err == nil || h.drivers.calls != 0 {
		t.Fatalf("err = %v, DriverFor calls = %d, want an error before any driver is built", err, h.drivers.calls)
	}
}

func TestIssueTrackerForRepoLinkedFailsClosed(t *testing.T) {
	t.Run("connection missing for the owner", func(t *testing.T) {
		h, src := newSidesHarness(t)
		h.svc.SetIssueTrackerDrivers(h.drivers)
		repo := h.addRepo(t, src, h.user, linkTo(uuid.New(), false))
		tr, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo)
		if !errors.Is(err, ErrLinkedRepoUnresolvable) || tr != nil {
			t.Fatalf("got (%v, %v), want (nil, ErrLinkedRepoUnresolvable)", tr, err)
		}
		if h.drivers.calls != 0 || h.built != 0 {
			t.Fatalf("no driver of either kind may be built (drivers %d, forge %d)", h.drivers.calls, h.built)
		}
	})
	t.Run("no driver source wired", func(t *testing.T) {
		h, src := newSidesHarness(t)
		repo := h.addRepo(t, src, h.user, linkTo(uuid.New(), true))
		tr, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo)
		if !errors.Is(err, ErrLinkedRepoUnresolvable) || tr != nil {
			t.Fatalf("got (%v, %v), want (nil, ErrLinkedRepoUnresolvable)", tr, err)
		}
		if h.built != 0 {
			t.Fatal("a linked repo must never fall back to its forge driver")
		}
	})
	t.Run("driver build error", func(t *testing.T) {
		h, src := newSidesHarness(t)
		boom := errors.New("not allowed")
		h.drivers.err = boom
		h.svc.SetIssueTrackerDrivers(h.drivers)
		repo := h.addRepo(t, src, h.user, linkTo(uuid.New(), true))
		tr, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo)
		if !errors.Is(err, boom) || tr != nil || h.built != 0 {
			t.Fatalf("got (%v, %v), forge built %d", tr, err, h.built)
		}
	})
	t.Run("another owner", func(t *testing.T) {
		h, src := newSidesHarness(t)
		h.svc.SetIssueTrackerDrivers(h.drivers)
		repo := h.addRepo(t, src, h.user, linkTo(uuid.New(), true))
		for name, call := range map[string]func() error{
			"issue side": func() error { _, err := h.svc.IssueTrackerForRepo(context.Background(), uuid.New(), repo); return err },
			"code host":  func() error { _, err := h.svc.CodeHostForRepo(context.Background(), uuid.New(), repo); return err },
		} {
			if err := call(); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("%s: error = %v, want pgx.ErrNoRows", name, err)
			}
		}
		if h.drivers.calls != 0 || h.built != 0 {
			t.Fatal("a foreign owner must reach no driver")
		}
	})
}

// The code-host side never reads the link (S2).
func TestCodeHostForRepoIgnoresTheLink(t *testing.T) {
	h, src := newSidesHarness(t)
	h.svc.SetIssueTrackerDrivers(h.drivers)
	repo := h.addRepo(t, src, h.user, linkTo(uuid.New(), false))

	ch, err := h.svc.CodeHostForRepo(context.Background(), h.user, repo)
	if err != nil {
		t.Fatal(err)
	}
	if ch != forge.CodeHost(h.rec) {
		t.Fatalf("CodeHostForRepo = %T, want the forge driver", ch)
	}
	if h.builtURL != "https://forge.example.test" || h.drivers.calls != 0 {
		t.Fatalf("forge url %q, issue tracker drivers used %d times", h.builtURL, h.drivers.calls)
	}
}

func TestRepoSidesPassBuilderErrorThrough(t *testing.T) {
	h, src := newSidesHarness(t)
	boom := errors.New("builder failed")
	h.svc.forgeBuilder = func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return nil, boom }
	repo := h.addRepo(t, src, h.user, nil)
	tr, err := h.svc.IssueTrackerForRepo(context.Background(), h.user, repo)
	if !errors.Is(err, boom) || tr != nil {
		t.Errorf("IssueTrackerForRepo = (%v, %v), want (nil, builder error)", tr, err)
	}
	ch, err := h.svc.CodeHostForRepo(context.Background(), h.user, repo)
	if !errors.Is(err, boom) || ch != nil {
		t.Errorf("CodeHostForRepo = (%v, %v), want (nil, builder error)", ch, err)
	}
}

// errStore fails the repo-source read with a fixed error.
type errStore struct {
	IssueStore
	err error
}

func (s errStore) GetRepoIssueSourceForUser(context.Context, store.GetRepoIssueSourceForUserParams) (store.GetRepoIssueSourceForUserRow, error) {
	return store.GetRepoIssueSourceForUserRow{}, s.err
}

func TestIssueTrackerForRepoClassifiesARepoSourceReadFailure(t *testing.T) {
	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	build := func(q IssueStore) *Service {
		return NewWithForgeBuilder(q, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
			return nil, errors.New("unused")
		})
	}
	ctx := context.Background()

	t.Run("a database error is transient and keeps its cause", func(t *testing.T) {
		inner := errors.New("conn reset")
		_, err := build(errStore{err: inner}).IssueTrackerForRepo(ctx, uuid.New(), uuid.New())
		if !errors.Is(err, errRepoSourceRead) || !errors.Is(err, inner) {
			t.Fatalf("err = %v, want errRepoSourceRead wrapping the inner error", err)
		}
	})
	t.Run("a missing row is permanent", func(t *testing.T) {
		_, err := build(errStore{err: pgx.ErrNoRows}).IssueTrackerForRepo(ctx, uuid.New(), uuid.New())
		if err == nil || errors.Is(err, errRepoSourceRead) {
			t.Fatalf("err = %v, want a non-transient error", err)
		}
	})
	t.Run("no store is not transient", func(t *testing.T) {
		_, err := build(nil).IssueTrackerForRepo(ctx, uuid.New(), uuid.New())
		if !errors.Is(err, errNoRepoStore) || errors.Is(err, errRepoSourceRead) {
			t.Fatalf("err = %v, want errNoRepoStore only", err)
		}
	})
}
