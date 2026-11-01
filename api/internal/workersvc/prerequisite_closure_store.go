package workersvc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PrerequisiteStore is the existing closure query contract. The executor uses the
// same interface for pool statements and short transactions; no forge I/O occurs
// inside PrerequisiteTransaction. Implementations must honor context deadlines.
type PrerequisiteStore interface {
	ClearPrerequisiteAbsence(context.Context, store.ClearPrerequisiteAbsenceParams) (int64, error)
	ConfirmPrerequisiteCleanupAbsent(context.Context, store.ConfirmPrerequisiteCleanupAbsentParams) (int64, error)
	ConfirmPrerequisiteLost(context.Context, store.ConfirmPrerequisiteLostParams) (int64, error)
	DeferPrerequisiteCleanupListing(context.Context, store.DeferPrerequisiteCleanupListingParams) (int64, error)
	DeferPrerequisitePreparation(context.Context, store.DeferPrerequisitePreparationParams) (int64, error)
	DeferPrerequisiteReconciliation(context.Context, store.DeferPrerequisiteReconciliationParams) (int64, error)
	ExpirePrerequisiteCleanup(context.Context, store.ExpirePrerequisiteCleanupParams) (int64, error)
	ExpirePrerequisitePreparation(context.Context, store.ExpirePrerequisitePreparationParams) (int64, error)
	GetPrerequisiteClosure(context.Context, store.GetPrerequisiteClosureParams) (store.RecoveryPrerequisiteClosure, error)
	ListPrerequisiteAuditDue(context.Context, store.ListPrerequisiteAuditDueParams) ([]store.RecoveryPrerequisiteClosure, error)
	ListPrerequisiteCleanupDue(context.Context, store.ListPrerequisiteCleanupDueParams) ([]store.RecoveryPrerequisiteClosure, error)
	ListPrerequisitePendingDue(context.Context, store.ListPrerequisitePendingDueParams) ([]store.RecoveryPrerequisiteClosure, error)
	ListPrerequisiteReconcileDue(context.Context, store.ListPrerequisiteReconcileDueParams) ([]store.RecoveryPrerequisiteClosure, error)
	ListPrerequisiteStaleInvokedDue(context.Context, store.ListPrerequisiteStaleInvokedDueParams) ([]store.RecoveryPrerequisiteClosure, error)
	LockPrerequisiteCapture(context.Context, uuid.UUID) (store.RecoveryCapture, error)
	LockPrerequisiteClosure(context.Context, uuid.UUID) (store.RecoveryPrerequisiteClosure, error)
	LockPrerequisiteHold(context.Context, uuid.UUID) (store.RecoveryCustodyHold, error)
	LockPrerequisiteRun(context.Context, uuid.UUID) (store.Run, error)
	MarkPrerequisiteCreateUnknown(context.Context, store.MarkPrerequisiteCreateUnknownParams) (int64, error)
	MarkPrerequisiteInvoked(context.Context, store.MarkPrerequisiteInvokedParams) (store.RecoveryPrerequisiteClosure, error)
	RecordPrerequisiteAbsence(context.Context, store.RecordPrerequisiteAbsenceParams) (int64, error)
	RecordPrerequisiteCleanupRetry(context.Context, store.RecordPrerequisiteCleanupRetryParams) (int64, error)
	RecordPrerequisiteCreateRefused(context.Context, store.RecordPrerequisiteCreateRefusedParams) (int64, error)
	RecordPrerequisiteDeleted(context.Context, store.RecordPrerequisiteDeletedParams) (int64, error)
	RecordPrerequisiteForeign(context.Context, store.RecordPrerequisiteForeignParams) (int64, error)
	RecordPrerequisitePinned(context.Context, store.RecordPrerequisitePinnedParams) (int64, error)
	RecordPrerequisitePinnedForeign(context.Context, store.RecordPrerequisitePinnedForeignParams) (int64, error)
	RecordPrerequisitePreparationRetry(context.Context, store.RecordPrerequisitePreparationRetryParams) (int64, error)
	RefusePrerequisitePreparation(context.Context, store.RefusePrerequisitePreparationParams) (int64, error)
	ReservePrerequisiteAudit(context.Context, store.ReservePrerequisiteAuditParams) (store.RecoveryPrerequisiteClosure, error)
	ReservePrerequisiteCleanup(context.Context, store.ReservePrerequisiteCleanupParams) (store.RecoveryPrerequisiteClosure, error)
	ReservePrerequisitePreparation(context.Context, store.ReservePrerequisitePreparationParams) (store.RecoveryPrerequisiteClosure, error)
	SettleStalePrerequisiteInvocation(context.Context, uuid.UUID) (int64, error)
	GetRepoForUser(context.Context, store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
	GetForgeConnectionForUser(context.Context, store.GetForgeConnectionForUserParams) (store.ForgeConnection, error)
}

// PrerequisiteTransaction runs fn atomically and commits only when fn returns nil.
// It is a narrow adapter seam for native-broker fixtures without a database.
type PrerequisiteTransaction func(context.Context, func(PrerequisiteStore) error) error

type prerequisiteCursor struct {
	due pgtype.Timestamptz
	id  uuid.UUID
}
type prerequisiteRuntime struct {
	slot        chan struct{}
	sweep       sync.Mutex
	mu          sync.Mutex
	root        context.Context
	cancel      context.CancelFunc
	stopping    bool
	started     bool
	parentStop  func() bool
	wg          sync.WaitGroup
	drainOnce   sync.Once
	drained     chan struct{}
	lead        int
	cursor      [5]prerequisiteCursor
	auditCursor prerequisiteCursor
	auditFirst  bool
	queries     PrerequisiteStore
	transaction PrerequisiteTransaction
}

func newPrerequisiteRuntime() *prerequisiteRuntime {
	root, cancel := context.WithCancel(context.Background())
	return &prerequisiteRuntime{slot: make(chan struct{}, 1), drained: make(chan struct{}), root: root, cancel: cancel}
}

// SetPrerequisiteStore replaces only the lane's persistence boundary. Configure
// before starting the service. Production defaults to s.q and s.txBeginner.
func (s *Service) SetPrerequisiteStore(q PrerequisiteStore, tx PrerequisiteTransaction) {
	s.prerequisite.queries, s.prerequisite.transaction = q, tx
}

func (s *Service) prerequisiteQueries() (PrerequisiteStore, error) {
	if q := s.prerequisite.queries; q != nil {
		return q, nil
	}
	q, ok := s.q.(PrerequisiteStore)
	if !ok {
		return nil, errors.New("prerequisite: store unavailable")
	}
	return q, nil
}

func (s *Service) prerequisiteTx(ctx context.Context, fn func(PrerequisiteStore) error) error {
	if tx := s.prerequisite.transaction; tx != nil {
		return tx(ctx, fn)
	}
	if s.txBeginner == nil {
		return errors.New("prerequisite: transactions unavailable")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if err = fn(store.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// StartPrerequisiteClosures binds kicks to the server lifetime, never an upload
// request. It launches no goroutine and must precede the first kick.
func (s *Service) StartPrerequisiteClosures(ctx context.Context) {
	r := s.prerequisite
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started && !r.stopping {
		r.started = true
		r.parentStop = context.AfterFunc(ctx, r.cancel)
		if ctx.Err() != nil {
			r.cancel()
		}
	}
}

// KickPrerequisiteClosure admits at most one server-owned goroutine. A busy slot
// drops the kick; the durable row remains eligible for SweepPrerequisiteClosures.
func (s *Service) KickPrerequisiteClosure(userID, captureID uuid.UUID) bool {
	r := s.prerequisite
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || r.stopping || r.root.Err() != nil {
		return false
	}
	select {
	case r.slot <- struct{}{}:
	default:
		return false
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { <-r.slot }()
		ctx, cancel := context.WithTimeout(r.root, prerequisitePassBudget)
		defer cancel()
		q, err := s.prerequisiteQueries()
		if err != nil {
			return
		}
		c, cc := context.WithTimeout(ctx, prerequisiteDBTimeout)
		row, err := q.GetPrerequisiteClosure(c, store.GetPrerequisiteClosureParams{UserID: userID, CaptureID: captureID})
		cc()
		if err != nil {
			return
		}
		p := newPrerequisitePass(s.now())
		_, _ = s.prerequisiteOne(ctx, q, row, p)
	}()
	return true
}

// DrainPrerequisiteClosures stops admission, cancels passes and kicks and joins their one
// in-flight outcome tail. Call with a shutdown context allowing five seconds.
func (s *Service) DrainPrerequisiteClosures(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r := s.prerequisite
	r.mu.Lock()
	r.stopping = true
	r.cancel()
	if r.parentStop != nil {
		r.parentStop()
	}
	r.mu.Unlock()
	r.drainOnce.Do(func() { go func() { r.wg.Wait(); close(r.drained) }() })
	select {
	case <-r.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ PrerequisiteStore = (*store.Queries)(nil)
