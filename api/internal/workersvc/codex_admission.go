package workersvc

import (
	"context"
	"errors"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

var (
	ErrCodexAdmissionInvalid = errors.New("invalid admission signal")
	ErrCodexAdmissionRefused = errors.New("admission signal refused")
)

// CodexAdmissionSignal carries evidence coordinates only. Time, identity and policy
// parameters are derived by the server. Optional provider coordinates are validated
// but are not persisted or used as quorum identities.
type CodexAdmissionSignal struct {
	ClaimGeneration  int64   `json:"claim_generation"`
	AttemptID        string  `json:"attempt_id"`
	Class            string  `json:"class"`
	ProviderThreadID *string `json:"provider_thread_id,omitempty"`
	ProviderTurnID   *string `json:"provider_turn_id,omitempty"`
}

func (signal CodexAdmissionSignal) Validate() error {
	attempt, err := uuid.Parse(signal.AttemptID)
	if err != nil || attempt == uuid.Nil || attempt.String() != signal.AttemptID ||
		signal.ClaimGeneration <= 0 || signal.ClaimGeneration > 1<<53-1 ||
		(signal.Class != "rate_limit" && signal.Class != "overload") {
		return ErrCodexAdmissionInvalid
	}
	for _, coordinate := range []*string{signal.ProviderThreadID, signal.ProviderTurnID} {
		if coordinate == nil {
			continue
		}
		if len(*coordinate) == 0 || len(*coordinate) > 128 || !utf8.ValidString(*coordinate) {
			return ErrCodexAdmissionInvalid
		}
		for _, r := range *coordinate {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return ErrCodexAdmissionInvalid
			}
		}
	}
	return nil
}

// RecordCodexAdmissionSignal uses a run SHARE lock to serialize evidence with park,
// release and generation changes. Credential reads intentionally use the subsequent
// INSERT snapshot without account/alias row locks: replacement committed before that
// snapshot refuses; replacement committed afterwards may race an accepted signal.
// The initial FK check may wait on an account writer, bounded by lock_timeout.
// No lifecycle fields or credential recovery predicates are changed.
func (s *Service) RecordCodexAdmissionSignal(ctx context.Context, worker store.Worker, runID uuid.UUID, signal CodexAdmissionSignal) error {
	if err := signal.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	if s.txBeginner == nil {
		return ErrCodexAdmissionRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer rollbackCancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '500ms'"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout = '2000ms'"); err != nil {
		return err
	}
	q := store.New(tx)
	if _, err = q.LockCodexAdmissionRun(ctx, store.LockCodexAdmissionRunParams{
		Column1: runID, Column2: worker.UserID, Column3: worker.ID, Column4: signal.ClaimGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodexAdmissionRefused
		}
		return err
	}
	attempt, _ := uuid.Parse(signal.AttemptID) // Validate has checked canonical form.
	if _, err = q.ObserveCodexAdmission(ctx, store.ObserveCodexAdmissionParams{
		Column1: runID, Column2: worker.UserID, Column3: worker.ID,
		Column4: signal.ClaimGeneration, Column5: attempt, Column6: signal.Class,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodexAdmissionRefused
		}
		return err
	}
	return tx.Commit(ctx)
}
