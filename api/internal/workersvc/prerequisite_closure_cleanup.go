package workersvc

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func (s *Service) prerequisiteList(ctx context.Context, b prerequisiteBinding, driver forge.Forge, row store.RecoveryPrerequisiteClosure, end time.Time) (string, bool, error) {
	if ctx.Err() != nil || end.Sub(s.now()) <= 1500*time.Millisecond {
		return "", false, context.DeadlineExceeded
	}
	c, cc := prerequisiteExternalContext(pushbroker.WithoutRedirects(ctx), s, end, 1500*time.Millisecond)
	defer cc()
	if err := b.validate(c, driver); err != nil {
		return "", false, err
	}
	tips, err := pushbroker.ListRefTips(c, pushbroker.ListRefsOptions{CloneURL: b.clone, Username: b.connection.BotUsername, PAT: b.pat}, row.PinRef)
	if identityErr := b.validate(c, driver); identityErr != nil {
		return "", false, identityErr
	}
	tip, ok := tips[row.PinRef]
	return tip, ok, err
}

// Reconciliation never creates or deletes. Even an expired capture must commit
// pinned first; cleanup can only be admitted as a later pass item.
func (s *Service) prerequisiteReconcile(ctx context.Context, q PrerequisiteStore, row store.RecoveryPrerequisiteClosure, p prerequisitePass, end time.Time) (int64, error) {
	b, err := s.prerequisiteBinding(ctx, q, row, true)
	var tip string
	var present bool
	if err == nil && b.frozen(row) {
		driver, buildErr := s.prerequisiteDriver(b)
		if buildErr != nil {
			err = buildErr
		} else {
			tip, present, err = s.prerequisiteList(ctx, b, driver, row, end)
		}
	} else {
		err = errors.New("prerequisite: frozen binding unavailable")
	}
	return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
		if err == nil && present {
			if tip == row.PinTip.String {
				return tx.RecordPrerequisitePinned(w, store.RecordPrerequisitePinnedParams{ID: row.ID, Generation: row.Attempts})
			}
			return tx.RecordPrerequisiteForeign(w, store.RecordPrerequisiteForeignParams{ID: row.ID, Generation: row.Attempts})
		}
		// Absence and failures retain the live pointer indefinitely.
		return tx.DeferPrerequisiteReconciliation(w, store.DeferPrerequisiteReconciliationParams{
			ID: row.ID, Generation: row.Attempts, ReconciliationGeneration: row.ReconciliationAttempts, LastError: "prerequisite listing inconclusive",
		})
	})
}

func prerequisiteCleanupEligible(c store.RecoveryCapture) bool {
	return c.State == "expired" || c.State == "discarded"
}

func (s *Service) prerequisitePinned(ctx context.Context, q PrerequisiteStore, row store.RecoveryPrerequisiteClosure, p prerequisitePass, end time.Time) (int64, error) {
	var cleanup bool
	// Reserve only a listing under capture -> closure locks. An ambiguous or
	// crashed delete is recovered by observation before spending another attempt.
	c, cc := context.WithTimeout(ctx, prerequisiteDBTimeout)
	err := s.prerequisiteTx(c, func(tx PrerequisiteStore) error {
		cap, e := tx.LockPrerequisiteCapture(c, row.CaptureID)
		if e != nil {
			return e
		}
		current, e := tx.LockPrerequisiteClosure(c, row.ID)
		if e != nil {
			return e
		}
		if current.State != "pinned" {
			return pgx.ErrNoRows
		}
		cleanup = prerequisiteCleanupEligible(cap) && !p.auditOnly
		if p.cleanupOnly && !cleanup {
			return pgx.ErrNoRows
		}
		// Locks may spend the admission transaction budget; preserve binding,
		// listing and outcome time before changing any scheduling facts.
		if c.Err() != nil || end.Sub(s.now()) < prerequisiteListingReserve-prerequisiteDBTimeout {
			return pgx.ErrNoRows
		}
		if current.CleanupAttempts == 8 && current.CleanupDeadline.Valid && !current.CleanupDeadline.Time.After(s.now()) && !current.CleanupExhaustedAt.Valid {
			_, e = tx.ExpirePrerequisiteCleanup(c, store.ExpirePrerequisiteCleanupParams{ID: row.ID, Generation: 8, PassID: current.LastAuditPassID})
			if e != nil {
				return e
			}
		}
		row, e = tx.ReservePrerequisiteAudit(c, store.ReservePrerequisiteAuditParams{ID: row.ID, PassID: p.id})
		return e
	})
	cc()
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	b, err := s.prerequisiteBinding(ctx, q, row, true)
	if err != nil || !b.frozen(row) {
		return s.prerequisitePinnedRetry(ctx, row, p, false)
	}
	driver, err := s.prerequisiteDriver(b)
	if err != nil {
		return s.prerequisitePinnedRetry(ctx, row, p, false)
	}
	tip, present, err := s.prerequisiteList(ctx, b, driver, row, end)
	if err != nil {
		return s.prerequisitePinnedRetry(ctx, row, p, false)
	}
	if present && tip != row.PinTip.String {
		return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
			return tx.RecordPrerequisitePinnedForeign(w, store.RecordPrerequisitePinnedForeignParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id})
		})
	}
	if !cleanup {
		return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
			if present {
				return tx.ClearPrerequisiteAbsence(w, store.ClearPrerequisiteAbsenceParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id})
			}
			if row.AbsentSince.Valid {
				return tx.ConfirmPrerequisiteLost(w, store.ConfirmPrerequisiteLostParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id})
			}
			return tx.RecordPrerequisiteAbsence(w, store.RecordPrerequisiteAbsenceParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id})
		})
	}
	if !present {
		return s.prerequisiteDeleted(ctx, row, p, false)
	}
	if row.CleanupAttempts >= 8 || row.CleanupExhaustedAt.Valid {
		return s.prerequisitePinnedRetry(ctx, row, p, false)
	}
	// Recheck the frozen binding after reservation/listing and immediately before
	// delete. Each broker operation is capped separately; the item retains a 1.5s
	// database outcome reserve. An error never proves absence.
	fresh, e := s.prerequisiteBinding(ctx, q, row, false)
	if e != nil || !b.same(fresh) || ctx.Err() != nil || end.Sub(s.now()) < 6500*time.Millisecond {
		return s.prerequisitePinnedRetry(ctx, row, p, false)
	}
	// Only a positive listing at the expected tip may reserve a delete generation.
	// Recheck every fence while holding capture -> closure, including the listing
	// pass, so an older listing cannot authorize work after a sibling reservation.
	c, cc = context.WithTimeout(ctx, prerequisiteDBTimeout)
	var reserved store.RecoveryPrerequisiteClosure
	err = s.prerequisiteTx(c, func(tx PrerequisiteStore) error {
		cap, e := tx.LockPrerequisiteCapture(c, row.CaptureID)
		if e != nil {
			return e
		}
		current, e := tx.LockPrerequisiteClosure(c, row.ID)
		if e != nil {
			return e
		}
		if !prerequisiteCleanupEligible(cap) || current.State != "pinned" || current.CleanupAttempts != row.CleanupAttempts || current.LastAuditPassID != p.id || current.CleanupAttempts >= 8 || current.CleanupExhaustedAt.Valid || current.NextAttemptAt.Time.After(s.now()) || current.CleanupDeadline.Valid && current.CleanupDeadline.Time.After(s.now()) {
			return pgx.ErrNoRows
		}
		latest, e := s.prerequisiteBinding(c, tx, current, false)
		if e != nil {
			return e
		}
		if !b.same(latest) || !latest.frozen(current) || c.Err() != nil || end.Sub(s.now()) < 6*time.Second {
			return pgx.ErrNoRows
		}
		reserved, e = tx.ReservePrerequisiteCleanup(c, store.ReservePrerequisiteCleanupParams{ID: row.ID, PassID: p.id, LeaseSeconds: 8})
		return e
	})
	cc()
	if errors.Is(err, pgx.ErrNoRows) {
		return s.prerequisitePinnedRetry(ctx, row, p, false)
	}
	if err != nil {
		return 0, err
	}
	row = reserved
	fresh, e = s.prerequisiteBinding(ctx, q, row, false)
	if e != nil || !b.same(fresh) {
		return s.prerequisitePinnedRetry(ctx, row, p, true)
	}
	d, dc := context.WithTimeout(pushbroker.WithoutRedirects(ctx), max(time.Duration(0), end.Sub(s.now())-2500*time.Millisecond))
	if err := b.validate(d, driver); err != nil {
		dc()
		return s.prerequisitePinnedRetry(ctx, row, p, true)
	}
	_ = pushbroker.Delete(d, pushbroker.DeleteOptions{CloneURL: b.clone, Username: b.connection.BotUsername, PAT: b.pat, Ref: row.PinRef, ExpectedOldTip: row.PinTip.String})
	identityErr := b.validate(d, driver)
	dc()
	if identityErr != nil {
		return s.prerequisitePinnedRetry(ctx, row, p, true)
	}
	// Delete returning nil is not absence (a moved tip deliberately returns nil).
	tip, present, err = s.prerequisiteList(ctx, b, driver, row, end)
	if err == nil && !present {
		return s.prerequisiteDeleted(ctx, row, p, true)
	}
	if err == nil && tip != row.PinTip.String {
		return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
			return tx.RecordPrerequisitePinnedForeign(w, store.RecordPrerequisitePinnedForeignParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id})
		})
	}
	return s.prerequisitePinnedRetry(ctx, row, p, true)
}

func (s *Service) prerequisiteDeleted(ctx context.Context, row store.RecoveryPrerequisiteClosure, p prerequisitePass, lease bool) (int64, error) {
	return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
		if lease {
			return tx.RecordPrerequisiteDeleted(w, store.RecordPrerequisiteDeletedParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id})
		}
		return tx.ConfirmPrerequisiteCleanupAbsent(w, store.ConfirmPrerequisiteCleanupAbsentParams{ID: row.ID, Generation: row.CleanupAttempts, PassID: uuidFromPrerequisitePass(p)})
	})
}
func uuidFromPrerequisitePass(p prerequisitePass) [16]byte { return p.id.Bytes }

func (s *Service) prerequisitePinnedRetry(ctx context.Context, row store.RecoveryPrerequisiteClosure, p prerequisitePass, lease bool) (int64, error) {
	return s.prerequisiteOutcome(ctx, row, p, func(w context.Context, tx PrerequisiteStore) (int64, error) {
		if lease {
			return tx.RecordPrerequisiteCleanupRetry(w, store.RecordPrerequisiteCleanupRetryParams{
				ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id, LastError: "prerequisite cleanup inconclusive",
			})
		}
		return tx.DeferPrerequisiteCleanupListing(w, store.DeferPrerequisiteCleanupListingParams{
			ID: row.ID, Generation: row.CleanupAttempts, PassID: p.id, LastError: "prerequisite listing deferred",
		})
	})
}
