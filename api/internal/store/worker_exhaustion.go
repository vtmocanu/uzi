package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// WorkerRecoveryDisposition records the actual committed mutation outcome.
type WorkerRecoveryDisposition struct {
	ID     uuid.UUID
	Status string
}

// WorkerRecoveryEvidence is a historical observation, not a forge availability claim.
type WorkerRecoveryEvidence struct {
	CheckpointTip        *string   `json:"checkpoint_tip"`
	AvailableCapture     bool      `json:"available_capture"`
	PublicationUncertain bool      `json:"publication_uncertain"`
	CaptureUncertain     bool      `json:"capture_uncertain"`
	CustodyUncertain     bool      `json:"custody_uncertain"`
	Unknown              bool      `json:"unknown"`
	RecordedAt           time.Time `json:"recorded_at"`
}

type exhaustionDisposition struct {
	// Server-only transient metadata; only Evidence is persisted on runs.
	ReleaseNonceCaptured bool                   `json:"release_nonce_captured,omitempty"`
	ReleasedWorkerNonce  *string                `json:"released_worker_nonce"`
	Park                 bool                   `json:"park"`
	Evidence             WorkerRecoveryEvidence `json:"evidence"`
}

var exhaustionCheckpointSHA = regexp.MustCompile("^[0-9a-f]{40}$")

// classifyWorkerExhaustion performs exactly one evidence read after the caller has
// completed its locks. A recoverable read/decode failure rolls back only that
// savepoint and produces unknown observations for all targets. Rollback/commit
// failures propagate: an unusable transaction must never look like a committed park.
func (q *Queries) classifyWorkerExhaustion(ctx context.Context, ids []uuid.UUID, nonceOverride *pgtype.Text) ([]byte, error) {
	var releasedNonce *string
	if nonceOverride != nil && nonceOverride.Valid {
		releasedNonce = &nonceOverride.String
	}
	dispositions := make(map[string]exhaustionDisposition, len(ids))
	recordedAt := time.Now().UTC()
	for _, id := range ids {
		dispositions[id.String()] = exhaustionDisposition{ReleaseNonceCaptured: nonceOverride != nil, ReleasedWorkerNonce: releasedNonce, Park: true, Evidence: WorkerRecoveryEvidence{Unknown: true, RecordedAt: recordedAt}}
	}
	beginner, ok := q.db.(TxBeginner)
	if !ok {
		return nil, errors.New("exhaustion evidence savepoint unavailable")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	rows, readErr := New(tx).ReadWorkerExhaustionEvidence(ctx, ids)
	if readErr == nil {
		for _, row := range rows {
			if row.CheckpointTip.Valid && !exhaustionCheckpointSHA.MatchString(row.CheckpointTip.String) {
				readErr = errors.New("invalid exhaustion checkpoint")
				break
			}
		}
	}
	if readErr != nil {
		if err := tx.Rollback(ctx); err != nil {
			return nil, err
		}
		return json.Marshal(dispositions)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if _, ok := dispositions[row.ID.String()]; !ok {
			continue
		}
		evidence := WorkerRecoveryEvidence{
			AvailableCapture: row.AvailableCapture, PublicationUncertain: row.PublicationUncertain,
			CaptureUncertain: row.CaptureUncertain, CustodyUncertain: row.CustodyUncertain, Unknown: row.UnknownEvidence, RecordedAt: recordedAt,
		}
		if row.CheckpointTip.Valid {
			tip := row.CheckpointTip.String
			evidence.CheckpointTip = &tip
		}
		dispositions[row.ID.String()] = exhaustionDisposition{
			ReleaseNonceCaptured: nonceOverride != nil, ReleasedWorkerNonce: releasedNonce,
			Park:     evidence.CheckpointTip != nil || evidence.AvailableCapture || evidence.PublicationUncertain || evidence.CaptureUncertain || evidence.CustodyUncertain || evidence.Unknown,
			Evidence: evidence,
		}
	}
	return json.Marshal(dispositions)
}
