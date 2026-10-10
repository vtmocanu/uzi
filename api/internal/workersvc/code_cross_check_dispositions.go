package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CodeCrossCheckDisposition records advice accepted or declined by the lead.
type CodeCrossCheckDisposition struct {
	ID          string `json:"id"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// FinalizeCodeCrossCheckDispositions commits one bounded batch. Omitted finding
// IDs become not_reported. The lead lock serializes callers; no retries are
// attempted here, and request cancellation bounds database work.
func (s *Service) FinalizeCodeCrossCheckDispositions(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, batch []CodeCrossCheckDisposition) (store.CrossCheck, error) {
	if s.txBeginner == nil || len(batch) > 20 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	supplied := make(map[string]CodeCrossCheckDisposition, len(batch))
	for _, d := range batch {
		if !codeFindingID.MatchString(d.ID) || len(d.Reason) > 1024 || strings.TrimSpace(d.Reason) == "" || !utf8.ValidString(d.Reason) ||
			(d.Disposition != "addressed" && d.Disposition != "declined") {
			return store.CrossCheck{}, ErrCrossCheckRefused
		}
		if _, duplicate := supplied[d.ID]; duplicate {
			return store.CrossCheck{}, ErrCrossCheckRefused
		}
		supplied[d.ID] = d
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if err != nil || lead.UserID != worker.UserID || lead.ClaimGeneration != generation ||
		lead.ClaimReleasedAt.Valid || (lead.Status != "claimed" && lead.Status != "running") {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	cc, err := q.GetCodeCrossCheck(ctx, leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cc, ErrCrossCheckRefused
	}
	if err != nil {
		return cc, err
	}
	if cc.LeadClaimGeneration != generation || cc.InterruptedAt.Valid || cc.Outcome.String != "completed" || !cc.DecidedAt.Valid {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	var findings []CodeCrossCheckFinding
	if json.Unmarshal(cc.Findings, &findings) != nil || findings == nil || len(findings) > 20 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	final := make([]CodeCrossCheckDisposition, 0, len(findings))
	seen := make(map[string]bool, len(findings))
	for _, f := range findings {
		if !codeFindingID.MatchString(f.ID) || seen[f.ID] {
			return store.CrossCheck{}, ErrCrossCheckRefused
		}
		seen[f.ID] = true
		d, present := supplied[f.ID]
		if !present {
			d = CodeCrossCheckDisposition{ID: f.ID, Disposition: "not_reported"}
		}
		final = append(final, d)
		delete(supplied, f.ID)
	}
	if len(supplied) != 0 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if cc.FinalizedAt.Valid {
		var prior []CodeCrossCheckDisposition
		if json.Unmarshal(cc.Dispositions, &prior) != nil || !reflect.DeepEqual(prior, final) {
			return store.CrossCheck{}, ErrCrossCheckRefused
		}
		// Current custody was checked before looking at the prior ACK.
		if err := tx.Commit(ctx); err != nil {
			return cc, err
		}
		return cc, nil
	}
	raw, err := json.Marshal(final)
	if err != nil {
		return cc, err
	}
	cc, err = q.FinalizeCodeCrossCheckDispositions(ctx, store.FinalizeCodeCrossCheckDispositionsParams{
		LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, Dispositions: raw,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return cc, ErrCrossCheckRefused
	}
	if err != nil {
		return cc, err
	}
	if err = tx.Commit(ctx); err != nil {
		return cc, err
	}
	return cc, nil
}
