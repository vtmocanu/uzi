package issuetrackersvc

// Live-DB tests of the first enable racing the issue tracker link (PRD #4173 D13), driven
// through provisionsvc.Service.EnableRepo itself. They need UZI_TEST_DATABASE_URL like the
// rest of link_livedb_test.go.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/provisionsvc"
)

// labelRecorder is a forge that counts its EnsureLabels calls and can run a hook inside one.
type labelRecorder struct {
	forgetest.BaseFake
	calls atomic.Int64
	hook  func() // runs inside EnsureLabels; nil for none
}

func (f *labelRecorder) EnsureLabels(context.Context, int64, []forge.Label) error {
	f.calls.Add(1)
	if f.hook != nil {
		f.hook()
	}
	return nil
}

type recorderForges struct{ f *labelRecorder }

func (recorderForges) EncryptToken(pat string) ([]byte, error) { return []byte(pat), nil }
func (r recorderForges) ForgeForToken(forge.Type, string, string) (forge.Forge, error) {
	return r.f, nil
}
func (r recorderForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return r.f, nil
}

// gateGuard allows every repo; entered is closed when the guard runs, and the guard then
// waits for release when one is set.
type gateGuard struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*gateGuard) CheckToken(context.Context, forge.CodeHost, forge.Type, bool) privcheck.TokenReport {
	return privcheck.TokenReport{}
}

func (g *gateGuard) GuardRepo(context.Context, privcheck.GuardInput) privcheck.GuardResult {
	if g.entered != nil {
		g.once.Do(func() { close(g.entered) })
	}
	if g.release != nil {
		<-g.release
	}
	return privcheck.GuardResult{}
}

// provisioner builds a provisioning service over the harness pool with the given fakes.
func (h *liveHarness) provisioner(f *labelRecorder, g *gateGuard) *provisionsvc.Service {
	return provisionsvc.New(h.q, recorderForges{f}, g, func(string) bool { return true }).
		UseTransactions(h.pool, func(tx pgx.Tx) provisionsvc.Store { return h.q.WithTx(tx) })
}

func (h *liveHarness) columnCount(repo uuid.UUID) int64 {
	h.t.Helper()
	n, err := h.q.CountBoardColumns(context.Background(), repo)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// The link commits while the enable is still in its guard: the enable must see the link under
// the row lock, write the column rows only, and never ask the forge for labels.
func TestEnableAfterALinkCommittedWritesNoForgeLabelsLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	repo := h.freshRepo()
	rec := &labelRecorder{}
	guard := &gateGuard{entered: make(chan struct{}), release: make(chan struct{})}
	prov := h.provisioner(rec, guard)

	done := make(chan error, 1)
	go func() {
		_, err := prov.EnableRepo(ctx, h.user, repo)
		done <- err
	}()
	<-guard.entered
	if err := h.link(repo); err != nil {
		t.Fatalf("link: %v", err)
	}
	close(guard.release)
	if err := <-done; err != nil {
		t.Fatalf("enable: %v", err)
	}

	if got := rec.calls.Load(); got != 0 {
		t.Fatalf("EnsureLabels calls = %d, want 0: the forge got label writes for a linked repo", got)
	}
	r := h.repoRow(repo)
	if !r.Enabled || !r.IssueTrackerConnectionID.Valid {
		t.Fatalf("repo enabled = %v, linked = %v; want both true", r.Enabled, r.IssueTrackerConnectionID.Valid)
	}
	if h.columnCount(repo) == 0 {
		t.Fatal("no board column rows were recorded for the linked repo")
	}
}

// The enable holds the row lock while it asks the forge for labels: a link that starts then
// waits on the lock and, once the enable commits, is refused.
func TestLinkDuringTheEnablesForgeLabelRequestIsRefusedLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	repo := h.freshRepo()
	rec := &labelRecorder{}
	linkResult := make(chan error, 1)
	rec.hook = func() {
		go func() { linkResult <- h.link(repo) }()
		waitForLockWait(t, h.pool, "FOR UPDATE OF r")
	}
	if _, err := h.provisioner(rec, &gateGuard{}).EnableRepo(ctx, h.user, repo); err != nil {
		t.Fatalf("enable: %v", err)
	}
	_ = wantKind(t, <-linkResult, KindConflict)

	if got := rec.calls.Load(); got != 1 {
		t.Fatalf("EnsureLabels calls = %d, want 1", got)
	}
	r := h.repoRow(repo)
	if !r.Enabled || r.IssueTrackerConnectionID.Valid {
		t.Fatalf("repo enabled = %v, linked = %v; want enabled and unlinked", r.Enabled, r.IssueTrackerConnectionID.Valid)
	}
}

// Without any ordering between them, every run ends in exactly one D13 outcome, and a linked
// repo never had a forge label request.
func TestEnableRacingLinkEndsInOneOutcomeLiveDB(t *testing.T) {
	h := newLiveHarness(t)
	ctx := context.Background()
	linkedFirst, enabledFirst := 0, 0
	for iter := range 20 {
		repo := h.freshRepo()
		rec := &labelRecorder{}
		prov := h.provisioner(rec, &gateGuard{})
		var linkErr, enableErr error
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		run := func(fn func()) {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				ready.Done()
				<-start
				fn()
			}()
		}
		run(func() { linkErr = h.link(repo) })
		run(func() { _, enableErr = prov.EnableRepo(ctx, h.user, repo) })
		ready.Wait()
		close(start)
		done.Wait()

		if enableErr != nil {
			t.Fatalf("iteration %d: enable: %v", iter, enableErr)
		}
		r := h.repoRow(repo)
		if !r.Enabled || r.AwaitingFirstEnable {
			t.Fatalf("iteration %d: repo not enabled with the marker cleared: %+v", iter, r)
		}
		calls := rec.calls.Load()
		switch {
		case linkErr == nil && r.IssueTrackerConnectionID.Valid:
			if calls != 0 {
				t.Fatalf("iteration %d: linked repo, but EnsureLabels was called %d times", iter, calls)
			}
			linkedFirst++
		case linkErr != nil && !r.IssueTrackerConnectionID.Valid:
			_ = wantKind(t, linkErr, KindConflict)
			if calls != 1 {
				t.Fatalf("iteration %d: unlinked repo, EnsureLabels called %d times, want 1", iter, calls)
			}
			enabledFirst++
		default:
			t.Fatalf("iteration %d: link err = %v but linked = %v", iter, linkErr, r.IssueTrackerConnectionID.Valid)
		}
	}
	t.Logf("link first %d, enable first %d", linkedFirst, enabledFirst)
}
