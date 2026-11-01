package recovery

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SetPrerequisiteClosure configures S0 once before serving requests. The kick owns
// its lifecycle in workersvc; it accepts no request context.
func (s *Service) SetPrerequisiteClosure(forges []string, kick func(userID, captureID uuid.UUID)) {
	s.prerequisiteForges = slices.Clone(forges)
	s.prerequisiteKick = kick
}

// insertPrerequisiteClosure uses only database facts while the upload holds custody
// then capture. The guarded SQL seam checks the exact binding and leaves live_run_id NULL.
func (s *Service) insertPrerequisiteClosure(ctx context.Context, tx pgx.Tx, w store.Worker, c store.RecoveryCapture) (bool, error) {
	if !c.ManifestBound || c.State != "available" || len(c.PrerequisiteShas) == 0 || len(c.PrerequisiteShas) > 64 ||
		!slices.Contains(w.ProtocolCapabilities, capability.RecoveryPrerequisiteClosureV1) ||
		!slices.Contains(s.prerequisiteForges, "github") {
		return false, nil
	}
	var kind, base string
	err := tx.QueryRow(ctx, `SELECT f.forge_type,f.base_url FROM recovery_custody_holds h
 JOIN repos r ON r.id=h.repo_id JOIN forge_connections f ON f.id=r.connection_id
 WHERE h.id=$1 AND h.user_id=$2 AND f.user_id=$2 AND h.inventory_guarded`, c.HoldID, w.UserID).Scan(&kind, &base)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// E2 reviewed the GitHub.com anchor strategy, not arbitrary enterprise or other forge hosts.
	if kind != "github" || strings.TrimRight(base, "/") != "https://github.com" {
		return false, nil
	}
	_, err = store.New(tx).InsertPrerequisiteClosure(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type prerequisiteStatusStore interface {
	GetPrerequisiteClosure(context.Context, store.GetPrerequisiteClosureParams) (store.RecoveryPrerequisiteClosure, error)
}

// withPrerequisiteStatus is read-only. It neither schedules lane work nor waits for it.
func withPrerequisiteStatus(ctx context.Context, q prerequisiteStatusStore, userID, captureID uuid.UUID, status apitypes.RecoveryCaptureStatusResponse) (apitypes.RecoveryCaptureStatusResponse, error) {
	if status.PrerequisiteClosure == nil {
		return status, nil
	}
	row, err := q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: captureID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return apitypes.RecoveryCaptureStatusResponse{}, err
	}
	status.PrerequisiteClosure = prerequisiteStatus(row)
	return status, nil
}

func noPrerequisiteClosure(bound bool, prerequisites []string, checksum string) *apitypes.RecoveryPrerequisiteClosureStatus {
	if bound && len(prerequisites) == 0 {
		return nil
	}
	out := &apitypes.RecoveryPrerequisiteClosureStatus{State: "unsupported", Reason: "closure_unsupported"}
	if bound {
		out.PrerequisiteShas = slices.Clone(prerequisites)
		out.Checksum = checksum
	}
	return out
}

func prerequisiteStatus(row store.RecoveryPrerequisiteClosure) *apitypes.RecoveryPrerequisiteClosureStatus {
	out := &apitypes.RecoveryPrerequisiteClosureStatus{State: "unsupported", Reason: "closure_unsupported", PrerequisiteShas: slices.Clone(row.PrerequisiteShas), Checksum: row.Checksum}
	switch row.State {
	case "pending":
		out.State, out.Reason = "pending", ""
		if row.Reason == "prerequisite_ancestry_unknown" {
			out.Reason = row.Reason
		}
	case "invoked", "create_unknown":
		out.State, out.Reason = "pending", "pin_unknown"
	case "pinned":
		out.State, out.Reason = "pinned", ""
		if row.AbsentSince.Valid {
			out.State, out.Reason = "pending", "pin_absence_unconfirmed"
		}
	case "refused":
		out.State, out.Reason = "refused", "closure_unsupported"
		switch row.Reason {
		case "closure_unsupported", "prerequisite_not_on_default", "prerequisite_ancestry_unknown", "prerequisite_prepare_budget_exhausted", "pin_refused", "archive_unavailable", "pin_absence_unconfirmed", "pin_unknown", "pin_foreign", "pin_lost":
			out.Reason = row.Reason
		}
	case "foreign":
		out.State, out.Reason = "refused", "pin_foreign"
	case "lost":
		out.State, out.Reason = "lost", "pin_lost"
	case "deleted":
		out.State, out.Reason = "unsupported", "archive_unavailable"
	}
	return out
}
