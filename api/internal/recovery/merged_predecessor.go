package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// MergeBinding is the recorded identity captured by the server before GetMergeRequest.
// CompletedGeneration is the run's completed claim generation, not the earlier hold's.
type MergeBinding struct {
	RunID, OwnerID, RepoID, ConnectionID  uuid.UUID
	ProjectID, MRIID, CompletedGeneration int64
	ForgeType, BaseURL                    string
}

// MergeObservation carries the returned forge identity. Both the exact recorded IID
// and the normalized merged state are required.
type MergeObservation struct {
	IID   int64
	State string
}

// VerifiedMergeSnapshot is immutable outside recovery. It has no worker transport mapping.
// Its constructor is a server trust seam, not a merge verifier: call it only after
// a fresh server GetMergeRequest using the exact MergeBinding.
type VerifiedMergeSnapshot struct {
	binding    MergeBinding
	verifiedAt time.Time
}

var ErrMergeSnapshotMismatch = errors.New("recovery: completed merge snapshot no longer matches")

var ErrMergedPredecessorWorkLimit = errors.New("recovery: merged predecessor transaction exceeds 1000 holds or 1000 non-ready captures")

const (
	maxMergedPredecessorHolds    = 1000
	maxMergedPredecessorCaptures = 1000
)

func validateMergeBinding(binding MergeBinding, verifiedAt time.Time) error {
	if binding.RunID == uuid.Nil || binding.OwnerID == uuid.Nil || binding.RepoID == uuid.Nil ||
		binding.ConnectionID == uuid.Nil || binding.ProjectID <= 0 || binding.MRIID <= 0 ||
		binding.CompletedGeneration <= 0 || binding.BaseURL == "" ||
		(binding.ForgeType != "github" && binding.ForgeType != "gitlab" && binding.ForgeType != "forgejo") ||
		verifiedAt.IsZero() || verifiedAt.Year() < 1 || verifiedAt.Year() > 9999 || verifiedAt.After(time.Now()) {
		return ErrBadRequest
	}
	return nil
}

func NewVerifiedMergeSnapshot(binding MergeBinding, verifiedAt time.Time, observed *MergeObservation) (VerifiedMergeSnapshot, error) {
	if err := validateMergeBinding(binding, verifiedAt); err != nil {
		return VerifiedMergeSnapshot{}, err
	}
	if observed == nil || observed.IID != binding.MRIID || observed.State != "merged" {
		return VerifiedMergeSnapshot{}, ErrBadRequest
	}
	return VerifiedMergeSnapshot{binding: binding, verifiedAt: verifiedAt.UTC()}, nil
}

// DiscardMergedPredecessorHolds settles only earlier OPEN holds in one transaction.
// Run, repo and connection locks fence binding drift, then holds are locked in generation/id
// order before captures. Each transaction permits at most 1000 holds and 1000 non-ready
// captures across siblings, with one extra row read to detect overflow. Calls use ctx's
// deadline and have no retries. Any refusal aborts all siblings. Available archives are untouched.
func (s *Service) DiscardMergedPredecessorHolds(ctx context.Context, snapshot VerifiedMergeSnapshot) (int64, error) {
	b := snapshot.binding
	if err := validateMergeBinding(b, snapshot.verifiedAt); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	run, err := qtx.LockMergedPredecessorRun(ctx, b.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrMergeSnapshotMismatch
	}
	if err != nil {
		return 0, err
	}
	if run.UserID != b.OwnerID || !run.RepoID.Valid || uuid.UUID(run.RepoID.Bytes) != b.RepoID ||
		run.Status != "completed" || run.ClaimGeneration != b.CompletedGeneration ||
		!run.MrIid.Valid || run.MrIid.Int64 != b.MRIID {
		return 0, ErrMergeSnapshotMismatch
	}
	binding, err := qtx.LockMergedPredecessorBinding(ctx, b.RepoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrMergeSnapshotMismatch
	}
	if err != nil {
		return 0, err
	}
	if binding.RepoID != b.RepoID || binding.ConnectionID != b.ConnectionID ||
		binding.OwnerID != b.OwnerID || binding.ProjectID != b.ProjectID ||
		binding.ForgeType != b.ForgeType || binding.BaseUrl != b.BaseURL {
		return 0, ErrMergeSnapshotMismatch
	}
	holds, err := qtx.LockMergedPredecessorHolds(ctx, store.LockMergedPredecessorHoldsParams{
		RunID: b.RunID, OwnerID: b.OwnerID, RepoID: pgconv.UUID(b.RepoID), CompletedGeneration: b.CompletedGeneration,
	})
	if err != nil || len(holds) == 0 {
		return 0, err
	}
	if len(holds) > maxMergedPredecessorHolds {
		return 0, ErrMergedPredecessorWorkLimit
	}
	remainingCaptures := maxMergedPredecessorCaptures
	for _, hold := range holds {
		audit, err := json.Marshal(struct {
			HoldID              uuid.UUID `json:"hold_id"`
			Generation          int64     `json:"generation"`
			RunID               uuid.UUID `json:"run_id"`
			OwnerID             uuid.UUID `json:"owner_id"`
			RepoID              uuid.UUID `json:"repo_id"`
			ConnectionID        uuid.UUID `json:"connection_id"`
			ForgeType           string    `json:"forge_type"`
			BaseURL             string    `json:"base_url"`
			ProjectID           int64     `json:"project_id"`
			MRIID               int64     `json:"mr_iid"`
			CompletedGeneration int64     `json:"completed_generation"`
			VerifiedAt          time.Time `json:"verified_at"`
		}{hold.ID, hold.Generation, b.RunID, b.OwnerID, b.RepoID, b.ConnectionID,
			b.ForgeType, b.BaseURL, b.ProjectID, b.MRIID, b.CompletedGeneration, snapshot.verifiedAt})
		if err != nil {
			return 0, err
		}
		n, err := qtx.DiscardMergedPredecessorHold(ctx, store.DiscardMergedPredecessorHoldParams{
			HoldID: hold.ID, RunID: b.RunID, OwnerID: b.OwnerID, RepoID: pgconv.UUID(b.RepoID),
			Generation: hold.Generation, CompletedGeneration: b.CompletedGeneration, Binding: audit,
		})
		if err != nil {
			return 0, err
		}
		if n != 1 {
			return 0, fmt.Errorf("recovery: merged predecessor hold %s refused discard", hold.ID)
		}
		captures, err := qtx.LockMergedPredecessorNonReadyCaptures(ctx, store.LockMergedPredecessorNonReadyCapturesParams{
			HoldID: hold.ID, CaptureLimit: int32(remainingCaptures + 1),
		})
		if err != nil {
			return 0, err
		}
		if len(captures) > remainingCaptures {
			return 0, ErrMergedPredecessorWorkLimit
		}
		remainingCaptures -= len(captures)
		n, err = qtx.DiscardNonReadyCapturesForHold(ctx, hold.ID)
		if err != nil {
			return 0, err
		}
		if n != int64(len(captures)) {
			return 0, fmt.Errorf("recovery: merged predecessor hold %s refused capture settlement", hold.ID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.custodySettledAfterCommit(b.RunID)
	return int64(len(holds)), nil
}
