// Package poller runs the background sync engine: on a fixed interval it pulls
// forge changes into uzi's issue cache for every enabled repo. Most ticks are
// cheap incremental pulls bounded by a per-repo high-water-mark; every Nth tick
// is a full reconcile that also evicts issues deleted or de-labeled forge-side
// (the incremental filter structurally cannot see those).
//
// This is the ChangeSource seam: a webhook receiver could feed the same
// FullSync/IncrementalSync methods later without touching the cache logic.
package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Engine polls all enabled repos. A single goroutine drives every repo each
// tick, holding per-repo state (high-water-mark + poll counter) in memory. The
// state map self-heals: repos that get disabled drop out of the enabled set and
// their state is discarded; newly enabled repos start with a full reconcile.
type Engine struct {
	svc            *forgesvc.Service
	q              *store.Queries
	interval       time.Duration
	reconcileEvery int

	states map[uuid.UUID]*repoState
}

type repoState struct {
	hwm       time.Time
	pollCount int
}

// New constructs an Engine. reconcileEvery < 1 is clamped to 1 (every poll is a
// full reconcile), so a misconfigured value degrades to "always correct, less
// efficient" rather than "never reconciles".
func New(svc *forgesvc.Service, q *store.Queries, interval time.Duration, reconcileEvery int) *Engine {
	if interval <= 0 {
		interval = time.Minute
	}
	if reconcileEvery < 1 {
		reconcileEvery = 1
	}
	return &Engine{
		svc:            svc,
		q:              q,
		interval:       interval,
		reconcileEvery: reconcileEvery,
		states:         make(map[uuid.UUID]*repoState),
	}
}

// Run blocks until ctx is cancelled, ticking every interval. It does not run an
// immediate tick on start; the first pass happens one interval in (the compose
// stack is up by then and the board's first-open path seeds initial data).
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	slog.Info("poller started", "interval", e.interval.String(), "reconcile_every", e.reconcileEvery)
	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopped")
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick syncs every enabled repo once. Errors on one repo are logged and skipped
// so a single bad connection (revoked PAT, forge down) never stalls the others.
func (e *Engine) tick(ctx context.Context) {
	repos, err := e.q.ListEnabledReposWithConnections(ctx)
	if err != nil {
		slog.Error("poller: list enabled repos", "error", err)
		return
	}

	seen := make(map[uuid.UUID]struct{}, len(repos))
	for _, r := range repos {
		seen[r.ID] = struct{}{}
		e.syncRepo(ctx, r)
	}

	// Drop state for repos no longer enabled so a re-enable starts fresh (full
	// reconcile) and the map does not grow without bound.
	for id := range e.states {
		if _, ok := seen[id]; !ok {
			delete(e.states, id)
		}
	}
}

func (e *Engine) syncRepo(ctx context.Context, r store.ListEnabledReposWithConnectionsRow) {
	f, err := e.svc.ForgeForConnection(r.ForgeType, r.BaseUrl, r.TokenCiphertext)
	if err != nil {
		slog.Error("poller: build forge client", "repo", r.PathWithNamespace, "error", err)
		return
	}

	st := e.states[r.ID]
	if st == nil {
		st = &repoState{}
		e.states[r.ID] = st
	}
	st.pollCount++

	// First poll after (re)enable and every reconcileEvery-th poll: full
	// reconcile with eviction. Otherwise a cheap incremental pull.
	if st.pollCount == 1 || st.pollCount%e.reconcileEvery == 0 {
		maxUpdated, err := e.svc.FullSync(ctx, r.ID, r.ForgeProjectID, f)
		if err != nil {
			slog.Error("poller: full sync", "repo", r.PathWithNamespace, "error", err)
			return
		}
		if maxUpdated.After(st.hwm) {
			st.hwm = maxUpdated
		}
		return
	}

	newHWM, err := e.svc.IncrementalSync(ctx, r.ID, r.ForgeProjectID, f, st.hwm)
	if err != nil {
		slog.Error("poller: incremental sync", "repo", r.PathWithNamespace, "error", err)
		return
	}
	st.hwm = newHWM
}
