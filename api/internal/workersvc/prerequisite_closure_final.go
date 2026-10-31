package workersvc

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Closure-backed FINAL port (issue #3727). The recovery service owns the request: these methods
// run inside its context, start no goroutine and take no prerequisiteRuntime slot. They return
// fixed errors only, so no forge text, URL or credential can reach a log or a response.

// PrerequisiteFinalObservation is the verdict of one pin listing.
type PrerequisiteFinalObservation int

const (
	PrerequisiteFinalUnknown PrerequisiteFinalObservation = iota
	PrerequisiteFinalPresent
	PrerequisiteFinalAbsent
	PrerequisiteFinalForeign
)

var errPrerequisiteFinalUnavailable = errors.New("prerequisite final: pin listing unavailable")

// finalListWindow is the request time handed to one listing: prerequisiteList keeps a 1.5s
// outcome reserve and caps the listing plus its identity checks at 1s.
const finalListWindow = 2500 * time.Millisecond

// prerequisiteFinalReserver is the one query the closure-final reservation needs beyond
// PrerequisiteStore; *store.Queries satisfies it.
type prerequisiteFinalReserver interface {
	ReservePrerequisiteFinalListing(context.Context, store.ReservePrerequisiteFinalListingParams) (store.RecoveryPrerequisiteClosure, error)
}

// PrerequisiteFinalReserve takes a fresh listing pass on a pinned closure under capture then
// closure locks, in one short transaction. false means the fence no longer holds.
func (s *Service) PrerequisiteFinalReserve(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (bool, error) {
	c, cancel := context.WithTimeout(ctx, prerequisiteDBTimeout)
	defer cancel()
	reserved := false
	err := s.prerequisiteTx(c, func(tx PrerequisiteStore) error {
		r, ok := tx.(prerequisiteFinalReserver)
		if !ok {
			return errors.New("prerequisite final: reservation unavailable")
		}
		if _, e := tx.LockPrerequisiteCapture(c, row.CaptureID); e != nil {
			return e
		}
		if _, e := tx.LockPrerequisiteClosure(c, row.ID); e != nil {
			return e
		}
		_, e := r.ReservePrerequisiteFinalListing(c, store.ReservePrerequisiteFinalListingParams{
			ID: row.ID, PassID: prerequisiteID(pass), Generation: row.CleanupAttempts,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		reserved = e == nil
		return e
	})
	if err != nil {
		return false, err
	}
	return reserved, nil
}

// PrerequisiteFinalObserve lists the pin ref once, outside every transaction, with a fresh
// credential-bearing binding, and classifies it. Any error, oversized answer, binding change or
// identity change is Unknown, never absence.
func (s *Service) PrerequisiteFinalObserve(ctx context.Context, row store.RecoveryPrerequisiteClosure) (PrerequisiteFinalObservation, error) {
	q, err := s.prerequisiteQueries()
	if err != nil {
		return PrerequisiteFinalUnknown, errPrerequisiteFinalUnavailable
	}
	b, err := s.prerequisiteBinding(ctx, q, row, true)
	if err != nil || !b.frozen(row) {
		return PrerequisiteFinalUnknown, errPrerequisiteFinalUnavailable
	}
	driver, err := s.prerequisiteDriver(b)
	if err != nil {
		return PrerequisiteFinalUnknown, errPrerequisiteFinalUnavailable
	}
	tip, present, err := s.prerequisiteList(ctx, b, driver, row, s.now().Add(finalListWindow))
	switch {
	case err != nil:
		return PrerequisiteFinalUnknown, errPrerequisiteFinalUnavailable
	case !present:
		return PrerequisiteFinalAbsent, nil
	case !row.PinTip.Valid || tip != row.PinTip.String:
		return PrerequisiteFinalForeign, nil
	}
	return PrerequisiteFinalPresent, nil
}

// prerequisiteFinalOutcome runs one listing outcome under capture then closure locks, bounded
// by the request context and a 1.5s cap (never a detached context).
func (s *Service) prerequisiteFinalOutcome(ctx context.Context, row store.RecoveryPrerequisiteClosure, fn func(context.Context, PrerequisiteStore) (int64, error)) (int64, error) {
	w, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	var n int64
	err := s.prerequisiteTx(w, func(tx PrerequisiteStore) error {
		if _, e := tx.LockPrerequisiteCapture(w, row.CaptureID); e != nil {
			return e
		}
		if _, e := tx.LockPrerequisiteClosure(w, row.ID); e != nil {
			return e
		}
		var e error
		n, e = fn(w, tx)
		return e
	})
	return n, err
}

// PrerequisiteFinalRecordAbsence records a clean absent listing under the pass and generation fence.
func (s *Service) PrerequisiteFinalRecordAbsence(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error) {
	return s.prerequisiteFinalOutcome(ctx, row, func(w context.Context, tx PrerequisiteStore) (int64, error) {
		return tx.RecordPrerequisiteAbsence(w, store.RecordPrerequisiteAbsenceParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: prerequisiteID(pass)})
	})
}

// PrerequisiteFinalRecordForeign records a foreign tip under the pass and generation fence.
func (s *Service) PrerequisiteFinalRecordForeign(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error) {
	return s.prerequisiteFinalOutcome(ctx, row, func(w context.Context, tx PrerequisiteStore) (int64, error) {
		return tx.RecordPrerequisitePinnedForeign(w, store.RecordPrerequisitePinnedForeignParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: prerequisiteID(pass)})
	})
}
