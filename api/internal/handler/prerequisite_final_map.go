package handler

import (
	"context"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// prerequisiteFinalAdapter adapts workersvc's closure-final methods to recovery's port (the two
// packages cannot import each other).
type prerequisiteFinalAdapter struct{ svc *workersvc.Service }

func (a prerequisiteFinalAdapter) Reserve(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (bool, error) {
	return a.svc.PrerequisiteFinalReserve(ctx, row, pass)
}

func (a prerequisiteFinalAdapter) Observe(ctx context.Context, row store.RecoveryPrerequisiteClosure) (recovery.PrerequisiteFinalObservation, error) {
	obs, err := a.svc.PrerequisiteFinalObserve(ctx, row)
	switch obs {
	case workersvc.PrerequisiteFinalPresent:
		return recovery.PrerequisiteFinalPresent, err
	case workersvc.PrerequisiteFinalAbsent:
		return recovery.PrerequisiteFinalAbsent, err
	case workersvc.PrerequisiteFinalForeign:
		return recovery.PrerequisiteFinalForeign, err
	}
	return recovery.PrerequisiteFinalUnknown, err
}

func (a prerequisiteFinalAdapter) RecordAbsence(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error) {
	return a.svc.PrerequisiteFinalRecordAbsence(ctx, row, pass)
}

func (a prerequisiteFinalAdapter) RecordForeign(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error) {
	return a.svc.PrerequisiteFinalRecordForeign(ctx, row, pass)
}

// prerequisiteFinalAllowFor is the closure-backed FINAL's per-worker admission gate: its own
// limiter bucket, keyed apart from the held-ref listing bucket and every route bucket. Allow
// charges one admission and nothing refunds it. A nil limiter yields a nil gate (feature off).
func prerequisiteFinalAllowFor(lim *mw.Limiter) func(workerID uuid.UUID) bool {
	if lim == nil {
		return nil
	}
	return func(workerID uuid.UUID) bool { return lim.Allow("prereq-final-list|" + workerID.String()) }
}
