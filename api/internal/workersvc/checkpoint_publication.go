package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// CheckpointPublicationWithheldFeature is the protocol feature token a worker waits for before it
// sends StateRequest.CheckpointPublication (issue #4257): an older api's strict decoder rejects the
// unknown field.
const CheckpointPublicationWithheldFeature = "checkpoint_publication_withheld_v1"

// Checkpoint publication report states and ACK values (issue #4257). The ACK is empty when the
// request carried no report.
const (
	CheckpointPublicationStateWithheld = "withheld"
	CheckpointPublicationStateCleared  = "cleared"

	CheckpointPublicationRecorded   = "recorded"
	CheckpointPublicationCleared    = "cleared"
	CheckpointPublicationRefused    = "refused"
	CheckpointPublicationUnrecorded = "unrecorded"
)

// CheckpointPublicationReport is the nested wire object of StateRequest.CheckpointPublication. It
// is content-free by construction: a state, a time, and the worker's own fencing and ordering
// markers. Since is required exactly when State is withheld.
type CheckpointPublicationReport struct {
	State           string `json:"state"`
	Since           string `json:"since,omitempty"`
	ClaimGeneration int64  `json:"claim_generation"`
	RegisterNonce   string `json:"register_nonce"`
	ReportSeq       int64  `json:"report_seq"`
}

// ErrInvalidCheckpointPublication is the 400 for a malformed checkpoint_publication object or one
// sent on a non-running report. Nothing was written.
var ErrInvalidCheckpointPublication = errors.New("invalid checkpoint_publication")

// parseCheckpointPublicationSince returns the parsed since of a withheld report.
func parseCheckpointPublicationSince(cp *CheckpointPublicationReport) (time.Time, error) {
	return time.Parse(time.RFC3339, cp.Since)
}

func validateCheckpointPublication(req StateRequest) error {
	cp := req.CheckpointPublication
	if cp == nil {
		return nil
	}
	switch {
	case req.State != "running":
		return fmt.Errorf("%w: only valid on a running report", ErrInvalidCheckpointPublication)
	case cp.State != CheckpointPublicationStateWithheld && cp.State != CheckpointPublicationStateCleared:
		return fmt.Errorf("%w: state must be withheld or cleared", ErrInvalidCheckpointPublication)
	case cp.RegisterNonce == "":
		return fmt.Errorf("%w: register_nonce is required", ErrInvalidCheckpointPublication)
	case cp.ClaimGeneration < 1:
		return fmt.Errorf("%w: claim_generation must be at least 1", ErrInvalidCheckpointPublication)
	case cp.ReportSeq < 1:
		return fmt.Errorf("%w: report_seq must be at least 1", ErrInvalidCheckpointPublication)
	case cp.State == CheckpointPublicationStateCleared && cp.Since != "":
		return fmt.Errorf("%w: since is only valid on a withheld report", ErrInvalidCheckpointPublication)
	}
	if cp.State == CheckpointPublicationStateWithheld {
		if cp.Since == "" {
			return fmt.Errorf("%w: since is required on a withheld report", ErrInvalidCheckpointPublication)
		}
		if _, err := parseCheckpointPublicationSince(cp); err != nil {
			return fmt.Errorf("%w: since must be an RFC3339 time", ErrInvalidCheckpointPublication)
		}
	}
	return nil
}

// recordCheckpointPublication writes the fact and returns the ACK value. It never fails the
// caller: a store error is logged without content and answered "unrecorded".
func (s *Service) recordCheckpointPublication(ctx context.Context, worker store.Worker, runID uuid.UUID, req StateRequest) string {
	cp := req.CheckpointPublication
	if cp == nil {
		return ""
	}
	var outcome string
	var err error
	if cp.State == CheckpointPublicationStateWithheld {
		since, perr := parseCheckpointPublicationSince(cp)
		if perr != nil {
			return CheckpointPublicationUnrecorded // unreachable after validation
		}
		outcome, err = s.q.RecordRunCheckpointWithheld(ctx, store.RecordRunCheckpointWithheldParams{
			ID: runID, WorkerID: worker.ID, Generation: cp.ClaimGeneration, RegisterNonce: cp.RegisterNonce,
			ReportSeq: cp.ReportSeq, Since: pgtype.Timestamptz{Time: since.UTC(), Valid: true},
		})
	} else {
		outcome, err = s.q.ClearRunCheckpointWithheld(ctx, store.ClearRunCheckpointWithheldParams{
			ID: runID, WorkerID: worker.ID, Generation: cp.ClaimGeneration, RegisterNonce: cp.RegisterNonce,
			ReportSeq: cp.ReportSeq,
		})
	}
	if err != nil {
		slog.Error("checkpoint publication fact write failed", "run_id", runID, "worker_id", worker.ID, "state", cp.State, "error", err)
		return CheckpointPublicationUnrecorded
	}
	switch outcome {
	case CheckpointPublicationRecorded, CheckpointPublicationCleared:
		return outcome
	default:
		return CheckpointPublicationRefused
	}
}
