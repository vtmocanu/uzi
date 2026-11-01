package recovery

import (
	"time"

	"github.com/google/uuid"
)

// CrossRunWitnessBinding records the witness identity before server forge I/O.
type CrossRunWitnessBinding struct {
	MergeBinding
	Kind      string
	IssueIID  int64
	CreatedAt time.Time
}

// VerifiedCrossRunWitnessSnapshot is immutable outside recovery and has no worker
// transport mapping. Its constructor is a server trust seam, not a merge verifier.
type VerifiedCrossRunWitnessSnapshot struct {
	binding    CrossRunWitnessBinding
	verifiedAt time.Time
}

// NewVerifiedCrossRunWitnessSnapshot must only be called after a fresh server
// GetMergeRequest using the exact binding recorded before that call.
func NewVerifiedCrossRunWitnessSnapshot(binding CrossRunWitnessBinding, verifiedAt time.Time, observed *MergeObservation) (VerifiedCrossRunWitnessSnapshot, error) {
	if err := validateMergeBinding(binding.MergeBinding, verifiedAt); err != nil {
		return VerifiedCrossRunWitnessSnapshot{}, err
	}
	if binding.Kind != "issue" || binding.IssueIID <= 0 || !finiteCrossRunTime(binding.CreatedAt) ||
		observed == nil || observed.IID != binding.MRIID || observed.State != "merged" {
		return VerifiedCrossRunWitnessSnapshot{}, ErrBadRequest
	}
	binding.CreatedAt = binding.CreatedAt.UTC().Truncate(time.Microsecond)
	return VerifiedCrossRunWitnessSnapshot{
		binding: binding, verifiedAt: verifiedAt.UTC().Truncate(time.Microsecond),
	}, nil
}

// CrossRunTargetBinding is the target identity supplied by the server caller.
// Eligibility requiring locked database rows is checked by the settlement service.
type CrossRunTargetBinding struct {
	RunID, OwnerID, RepoID uuid.UUID
	Kind, Status           string
	Generation, IssueIID   int64
	FinishedAt             time.Time
}

// Validate checks target shape and its relationship to an immutable witness.
// Times are compared at the same UTC microsecond precision used by the audit.
func (target CrossRunTargetBinding) Validate(snapshot VerifiedCrossRunWitnessSnapshot) error {
	if target.RunID == uuid.Nil || target.OwnerID == uuid.Nil || target.RepoID == uuid.Nil ||
		target.Kind != "issue" || (target.Status != "failed" && target.Status != "cancelled") ||
		target.Generation <= 0 || target.IssueIID <= 0 || !finiteCrossRunTime(target.FinishedAt) {
		return ErrBadRequest
	}
	b := snapshot.binding
	if err := validateMergeBinding(b.MergeBinding, snapshot.verifiedAt); err != nil {
		return err
	}
	if b.Kind != "issue" || b.IssueIID <= 0 || !finiteCrossRunTime(b.CreatedAt) {
		return ErrBadRequest
	}
	if target.RunID == b.RunID || target.OwnerID != b.OwnerID || target.RepoID != b.RepoID ||
		target.IssueIID != b.IssueIID ||
		!target.FinishedAt.UTC().Truncate(time.Microsecond).Before(b.CreatedAt) {
		return ErrMergeSnapshotMismatch
	}
	return nil
}

func finiteCrossRunTime(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}
