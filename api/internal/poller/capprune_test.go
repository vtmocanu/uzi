package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// capStore serves only the watched-refs query SyncPipelines reads with no default
// branch and no eviction; any other IssueStore method panics on the nil embed.
type capStore struct {
	forgesvc.IssueStore
	refs int
}

func (s capStore) ListWatchedRunRefsForRepo(_ context.Context, arg store.ListWatchedRunRefsForRepoParams) ([]store.ListWatchedRunRefsForRepoRow, error) {
	rows := make([]store.ListWatchedRunRefsForRepoRow, min(s.refs, int(arg.MaxRefs)))
	for i := range rows {
		rows[i].Branch = pgtype.Text{String: fmt.Sprintf("agent/issue-%d", i+1), Valid: true}
	}
	return rows, nil
}

type warnCounter struct {
	mu    *sync.Mutex
	warns *int
}

func (h warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h warnCounter) Handle(_ context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, "forgesvc: pipeline watch hit the ref cap") {
		h.mu.Lock()
		*h.warns++
		h.mu.Unlock()
	}
	return nil
}
func (h warnCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h warnCounter) WithGroup(string) slog.Handler      { return h }

// TestPruneStatesForgetsPipelineCapState pins the poller's wiring for the issue #1483
// follow-up: pruning a repo that is no longer enabled also drops its ref-cap state in
// the sync service, so it does not linger until restart. Observed through the log: the
// pruned repo, synced again while still capped, WARNs a second time. Swaps
// slog.SetDefault (process-global): must not run in parallel.
func TestPruneStatesForgetsPipelineCapState(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var (
		mu    sync.Mutex
		warns int
	)
	slog.SetDefault(slog.New(warnCounter{mu: &mu, warns: &warns}))

	const maxRefs = 2
	svc := forgesvc.New(capStore{refs: maxRefs + 1}, nil, time.Second, nil)
	e := New(svc, nil, time.Minute, 10)
	repo := uuid.New()
	e.states[repo] = &repoState{}
	f := &forgetest.BaseFake{}
	syncCapped := func() {
		t.Helper()
		if err := svc.SyncPipelines(context.Background(), repo, 7, f, forgesvc.PipelineSyncOptions{Window: time.Hour, MaxRefs: maxRefs}); err != nil {
			t.Fatalf("SyncPipelines: %v", err)
		}
	}

	syncCapped()
	syncCapped()
	if warns != 1 {
		t.Fatalf("capped twice before prune: %d WARNs, want 1", warns)
	}

	e.pruneStates(map[uuid.UUID]struct{}{}) // the repo is no longer enabled
	if _, ok := e.states[repo]; ok {
		t.Fatal("poller state for a no-longer-enabled repo survived the prune")
	}
	syncCapped()
	if warns != 2 {
		t.Fatalf("pruned repo back and still capped: %d WARNs, want 2 (the cap state must be pruned too)", warns)
	}
}
