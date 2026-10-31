package forgesvc

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type MergedPredecessorStore interface {
	ListCrossRunTargets(context.Context, store.ListCrossRunTargetsParams) ([]store.ListCrossRunTargetsRow, error)
	ListMergedPredecessorCandidates(context.Context, store.ListMergedPredecessorCandidatesParams) ([]store.ListMergedPredecessorCandidatesRow, error)
}

type MergedPredecessorRecovery interface {
	DiscardCrossRunHolds(context.Context, recovery.CrossRunTargetBinding, recovery.VerifiedCrossRunWitnessSnapshot) (int64, error)
	DiscardMergedPredecessorHolds(context.Context, recovery.VerifiedMergeSnapshot) (int64, error)
}

type crossRunContinuation struct {
	witness   uuid.UUID
	createdAt pgtype.Timestamptz
	runID     uuid.UUID
	// Consecutive target-enumeration failures or phase-deadline exits for this
	// witness. Reset by a successful enumeration or a different witness.
	failures int
}

// After this many consecutive failures the witness cursor moves past B so later
// candidates in the repository, legacy same-run discards included, progress.
// B and its targets are rediscovered from an empty key after the cursor wraps.
const crossRunEnumerationFailureLimit = 3

type mergedPredecessorReconciler struct {
	mu       sync.Mutex
	q        MergedPredecessorStore
	recovery MergedPredecessorRecovery
	cursors  map[uuid.UUID]uuid.UUID
	targets  map[uuid.UUID]crossRunContinuation
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
// attempted run cursors without blocking siblings. Cross-run targets retain the
// witness cursor until exhausted; enumeration failures retain their continuation
// until crossRunEnumerationFailureLimit consecutive failures yield past the witness.
// Exhaustion wraps only for a later phase, preventing repeats.
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
	for id := range m.targets {
		if !enabled[id] {
			delete(m.targets, id)
		}
	}
	if len(repos) == 0 {
		m.nextRepo = uuid.Nil
		return
	}
	if m.cursors == nil {
		m.cursors = make(map[uuid.UUID]uuid.UUID)
	}
	if m.targets == nil {
		m.targets = make(map[uuid.UUID]crossRunContinuation)
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
			delete(m.targets, repo)
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
		before := m.cursors[repo]
		m.cursors[repo] = row.RunID
		s.verifyMergedPredecessor(ctx, row, before, &attempts)
		if _, pending := m.targets[repo]; pending {
			exhausted[repo] = true
		}
	}
}

func (s *Service) verifyMergedPredecessor(ctx context.Context, row store.ListMergedPredecessorCandidatesRow, before uuid.UUID, attempts *int) {
	m := &s.merged
	continuation := m.targets[row.RepoID]
	if continuation.witness != row.RunID {
		continuation = crossRunContinuation{witness: row.RunID}
	}
	delete(m.targets, row.RepoID)
	// Freeze every witness field before any forge I/O.
	binding := recovery.MergeBinding{
		RunID: row.RunID, OwnerID: row.OwnerID, RepoID: row.RepoID, ConnectionID: row.ConnectionID,
		ProjectID: row.ProjectID, MRIID: row.MrIid, CompletedGeneration: row.CompletedGeneration,
		ForgeType: row.ForgeType, BaseURL: row.BaseUrl,
	}
	witnessBinding := recovery.CrossRunWitnessBinding{MergeBinding: binding, Kind: row.Kind, IssueIID: row.IssueIid.Int64, CreatedAt: row.CreatedAt.Time}
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
	verifiedAt := time.Now().UTC()
	observation := &recovery.MergeObservation{IID: observed.IID, State: observed.State}
	if row.OriginalBranch {
		snapshot, snapshotErr := recovery.NewVerifiedMergeSnapshot(binding, verifiedAt, observation)
		if snapshotErr != nil {
			slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "invalid_snapshot")
			return
		}
		(*attempts)++
		if _, err = m.recovery.DiscardMergedPredecessorHolds(ctx, snapshot); err != nil {
			slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "settlement_refused")
		}
	}
	if !row.IssueIid.Valid || !row.CreatedAt.Valid || row.CreatedAt.InfinityModifier != pgtype.Finite {
		return
	}
	witness, err := recovery.NewVerifiedCrossRunWitnessSnapshot(witnessBinding, verifiedAt, observation)
	if err != nil {
		return
	}
	// Keep the witness cursor before B until its targets are exhausted. Each phase
	// observes B again and visits at most one target; refusals retry after a wrap.
	m.targets[row.RepoID] = continuation
	m.cursors[row.RepoID] = before
	if *attempts >= 100 {
		// A refused legacy transaction can repeatedly consume the final slot.
		// Resume this repository first so its deferred target gets a fresh budget.
		m.nextRepo = row.RepoID
		return
	}
	if ctx.Err() != nil {
		m.crossRunEnumerationFailed(row, continuation)
		return
	}
	targets, err := m.q.ListCrossRunTargets(ctx, store.ListCrossRunTargetsParams{
		RepoID: pgtype.UUID{Bytes: row.RepoID, Valid: true}, OwnerID: binding.OwnerID,
		IssueIid: row.IssueIid, WitnessRunID: binding.RunID, WitnessCreatedAt: row.CreatedAt,
		AfterCreatedAt: continuation.createdAt, AfterRunID: continuation.runID,
	})
	if ctx.Err() != nil {
		m.crossRunEnumerationFailed(row, continuation)
		return
	}
	if err != nil {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "target_enumeration_failed")
		m.crossRunEnumerationFailed(row, continuation)
		return
	}
	if len(targets) == 0 {
		delete(m.targets, row.RepoID)
		m.cursors[row.RepoID] = row.RunID
		return
	}
	target := targets[0]
	// Advance before entering recovery, including when recovery refuses or panics.
	m.targets[row.RepoID] = crossRunContinuation{witness: row.RunID, createdAt: target.CreatedAt, runID: target.RunID}
	if !target.RepoID.Valid || !target.IssueIid.Valid || !target.FinishedAt.Valid || target.FinishedAt.InfinityModifier != pgtype.Finite {
		return
	}
	(*attempts)++
	if _, err = m.recovery.DiscardCrossRunHolds(ctx, recovery.CrossRunTargetBinding{
		RunID: target.RunID, OwnerID: target.OwnerID, RepoID: uuid.UUID(target.RepoID.Bytes),
		Kind: target.Kind, Status: target.Status, Generation: target.Generation,
		IssueIID: target.IssueIid.Int64, FinishedAt: target.FinishedAt.Time,
	}, witness); err != nil {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "cross_run_settlement_refused")
	}
}

// crossRunEnumerationFailed keeps the continuation for a transient failure and
// yields the witness cursor past B once the consecutive-failure limit is reached.
// The attempt-cap deferral is not a failure and never calls this.
func (m *mergedPredecessorReconciler) crossRunEnumerationFailed(row store.ListMergedPredecessorCandidatesRow, continuation crossRunContinuation) {
	continuation.failures++
	if continuation.failures >= crossRunEnumerationFailureLimit {
		slog.Warn("merged predecessor reconciliation refused", "run_id", row.RunID, "reason", "target_enumeration_yielded")
		delete(m.targets, row.RepoID)
		m.cursors[row.RepoID] = row.RunID
		return
	}
	m.targets[row.RepoID] = continuation
}
