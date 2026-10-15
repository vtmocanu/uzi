package forgesvc

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type MergedPredecessorStore interface {
	ListMergedPredecessorCandidates(context.Context, store.ListMergedPredecessorCandidatesParams) ([]store.ListMergedPredecessorCandidatesRow, error)
}

type MergedPredecessorRecovery interface {
	DiscardMergedPredecessorHolds(context.Context, recovery.VerifiedMergeSnapshot) (int64, error)
}

type mergedPredecessorReconciler struct {
	mu       sync.Mutex
	q        MergedPredecessorStore
	recovery MergedPredecessorRecovery
	cursors  map[uuid.UUID]uuid.UUID
	nextRepo uuid.UUID
	// Internal context seam for deterministic deadline tests; nil uses real timers.
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

func (m *mergedPredecessorReconciler) timeout(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if m.withTimeout != nil {
		return m.withTimeout(parent, budget)
	}
	return context.WithTimeout(parent, budget)
}

// SetMergedPredecessorRecovery wires the server recovery authority before polling starts.
func (s *Service) SetMergedPredecessorRecovery(q MergedPredecessorStore, r MergedPredecessorRecovery) {
	s.merged.q, s.merged.recovery = q, r
}

// ReconcileMergedPredecessors runs after ordinary polling using its original parent.
// At most 100 repository visits and attempts share 30 seconds; each observation
// gets at most five seconds and the client's existing timeout. Failures advance
// attempted run cursors without blocking siblings; enumeration failures retain
// their run cursor. Exhaustion wraps only for a later phase, preventing repeats.
func (s *Service) ReconcileMergedPredecessors(parent context.Context, repoIDs []uuid.UUID) {
	m := &s.merged
	if m.q == nil || m.recovery == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := m.timeout(parent, 30*time.Second)
	defer cancel()
	repos := slices.Clone(repoIDs)
	slices.SortFunc(repos, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	repos = slices.Compact(repos)
	enabled := make(map[uuid.UUID]bool, len(repos))
	for _, id := range repos {
		enabled[id] = true
	}
	for id := range m.cursors {
		if !enabled[id] {
			delete(m.cursors, id)
		}
	}
	if len(repos) == 0 {
		m.nextRepo = uuid.Nil
		return
	}
	if m.cursors == nil {
		m.cursors = make(map[uuid.UUID]uuid.UUID)
	}
	index := 0
	for index < len(repos) && slices.Compare(repos[index][:], m.nextRepo[:]) < 0 {
		index++
	}
	if index == len(repos) {
		index = 0
	}
	exhausted := make(map[uuid.UUID]bool)
	attempted := make(map[uuid.UUID]bool)
	for visits, attempts := 0, 0; visits < 100 && attempts < 100 && len(exhausted) < len(repos); visits++ {
		if ctx.Err() != nil {
			return
		}
		repo := repos[index]
		index = (index + 1) % len(repos)
		m.nextRepo = repos[index]
		if exhausted[repo] {
			continue
		}
		rows, err := m.q.ListMergedPredecessorCandidates(ctx, store.ListMergedPredecessorCandidatesParams{RepoID: repo, AfterRunID: m.cursors[repo]})
		if ctx.Err() != nil {
			m.nextRepo = repo
			return
		}
		if err != nil {
			slog.Warn("merged predecessor reconciliation refused", "repo_id", repo, "reason", "enumeration_failed")
			exhausted[repo] = true
			continue
		}
		if len(rows) == 0 {
			m.cursors[repo] = uuid.Nil
			exhausted[repo] = true
			continue
		}
		row := rows[0]
		if ctx.Err() != nil {
			m.nextRepo = repo
			return
		}
		if attempted[row.RunID] {
			exhausted[repo] = true
			continue
		}
		attempted[row.RunID] = true
		m.cursors[repo] = row.RunID
		attempts++
		s.verifyMergedPredecessor(ctx, row)
	}
}

func (s *Service) verifyMergedPredecessor(ctx context.Context, row store.ListMergedPredecessorCandidatesRow) {
	// Isolate each attempted candidate, including a post-commit settlement hook.
	// A panic cannot undo a committed discard; the existing retention backstop retries settlement.
	// Never log the recovered value: it may contain forge response data or credentials.
	defer func() {
		if recover() != nil {
			slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "candidate_panicked")
		}
	}()
	// The candidate query takes a fresh exact owner-scoped connection snapshot.
	// Never reuse the ordinary tick's connection or cached MR state.
	f, err := s.ForgeForConnection(row.ForgeType, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "client_unavailable")
		return
	}
	observationCtx, cancel := s.merged.timeout(ctx, 5*time.Second)
	defer cancel()
	observed, err := f.GetMergeRequest(observationCtx, row.ProjectID, row.MrIid)
	observationErr := observationCtx.Err()
	cancel()
	if observationErr != nil || err != nil || observed.IID != row.MrIid || observed.State != forge.MRStateMerged {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "merge_unverified")
		return
	}
	snapshot, err := recovery.NewVerifiedMergeSnapshot(recovery.MergeBinding{
		RunID: row.RunID, OwnerID: row.OwnerID, RepoID: row.RepoID, ConnectionID: row.ConnectionID,
		ProjectID: row.ProjectID, MRIID: row.MrIid, CompletedGeneration: row.CompletedGeneration,
		ForgeType: row.ForgeType, BaseURL: row.BaseUrl,
	}, time.Now(), &recovery.MergeObservation{IID: observed.IID, State: observed.State})
	if err != nil {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "invalid_snapshot")
		return
	}
	if _, err = s.merged.recovery.DiscardMergedPredecessorHolds(ctx, snapshot); err != nil {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "settlement_refused")
	}
}
