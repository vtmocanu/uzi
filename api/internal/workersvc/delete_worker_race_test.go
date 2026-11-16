package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// #4389: a hibernation cache prune can settle (or a new one start) between a worker DELETE that
// matched no row and the re-read. raceStore sequences the delete path per call so each race is
// deterministic; past the end of a sequence the last value repeats.
type raceStore struct {
	*fakeStore
	deletes     []int64
	rereads     []*store.Worker // nil entry: absent (pgx.ErrNoRows)
	rereadErrs  []error         // non-nil entry overrides the reread at that call
	activeRuns  []int64
	custody     []int64
	deleteCalls int
	rereadCalls int
	activeCalls int
	custodyCall int
}

func seqAt[T any](s []T, i int) T {
	var zero T
	if len(s) == 0 {
		return zero
	}
	if i >= len(s) {
		return s[len(s)-1]
	}
	return s[i]
}

func (r *raceStore) DeleteWorkerForUser(context.Context, store.DeleteWorkerForUserParams) (int64, error) {
	n := seqAt(r.deletes, r.deleteCalls)
	r.deleteCalls++
	return n, nil
}

func (r *raceStore) GetWorkerByIDForUser(context.Context, store.GetWorkerByIDForUserParams) (store.Worker, error) {
	i := r.rereadCalls
	r.rereadCalls++
	if err := seqAt(r.rereadErrs, i); err != nil && i < len(r.rereadErrs) {
		return store.Worker{}, err
	}
	w := seqAt(r.rereads, i)
	if w == nil {
		return store.Worker{}, pgx.ErrNoRows
	}
	return *w, nil
}

func (r *raceStore) CountWorkerNonTerminalRuns(context.Context, store.CountWorkerNonTerminalRunsParams) (int64, error) {
	n := seqAt(r.activeRuns, r.activeCalls)
	r.activeCalls++
	return n, nil
}

func (r *raceStore) CountOpenCustodyHoldsForWorker(context.Context, store.CountOpenCustodyHoldsForWorkerParams) (int64, error) {
	n := seqAt(r.custody, r.custodyCall)
	r.custodyCall++
	return n, nil
}

func phaseRow(phase string) *store.Worker { return &store.Worker{CleanupPhase: phase} }

func newRaceSvc(t *testing.T, rs *raceStore) *Service {
	t.Helper()
	if rs.fakeStore == nil {
		rs.fakeStore = &fakeStore{}
	}
	return New(rs, newBox(t), testParams())
}

// (a) The attempt settles between the first DELETE and the re-read: the retry deletes the
// worker. On the unfixed code this returned ErrWorkerNotFound (404) for a deletable worker.
func TestDeleteWorkerRetriesAfterCleanupSettledMidDelete(t *testing.T) {
	rs := &raceStore{deletes: []int64{0, 1}, rereads: []*store.Worker{phaseRow(CleanupPhaseSettled)}}
	svc := newRaceSvc(t, rs)
	wid := uuid.New()
	svc.outbox.record(wid, []OutboxEntry{{RunID: uuid.New()}}, time.Now())
	if _, ok := svc.outbox.workers[wid]; !ok {
		t.Fatal("precondition: outbox tracker should know the worker")
	}
	if err := svc.DeleteWorker(context.Background(), uuid.New(), wid); err != nil {
		t.Fatalf("DeleteWorker = %v, want nil (the retry deletes the settled worker)", err)
	}
	if rs.deleteCalls != 2 || rs.rereadCalls != 1 {
		t.Fatalf("deletes=%d rereads=%d, want 2 and 1", rs.deleteCalls, rs.rereadCalls)
	}
	// The retry is the WHOLE guarded delete: both guards ran twice.
	if rs.activeCalls != 2 || rs.custodyCall != 2 {
		t.Fatalf("guard calls active=%d custody=%d, want 2 each", rs.activeCalls, rs.custodyCall)
	}
	if _, ok := svc.outbox.workers[wid]; ok {
		t.Fatal("a deleted worker's outbox depth must be evicted")
	}
}

// (b) A new attempt starts before the retry: the same unsettled refusal, not a 404.
func TestDeleteWorkerRetryRefusedWhenNewCleanupUnsettled(t *testing.T) {
	for _, phase := range []string{CleanupPhaseRequested, CleanupPhaseStarted, CleanupPhaseUnknown} {
		rs := &raceStore{deletes: []int64{0, 0}, rereads: []*store.Worker{phaseRow(CleanupPhaseSettled), phaseRow(phase)}}
		err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New())
		var un *WorkerHibernationCleanupUnsettledError
		if !errors.As(err, &un) || un.Phase != phase || !errors.Is(err, ErrWorkerHibernationCleanupUnsettled) {
			t.Fatalf("phase %s: err = %v, want unsettled refusal", phase, err)
		}
		if rs.deleteCalls != 2 {
			t.Fatalf("phase %s: deletes = %d, want 2", phase, rs.deleteCalls)
		}
	}
}

// (c) Both attempts lose a start/settle race and the final re-read finds the worker present and
// settled: the retryable conflict, never a 404, and no third attempt.
func TestDeleteWorkerDoubleRaceIsRetryableConflict(t *testing.T) {
	for _, phase := range []string{CleanupPhaseSettled, CleanupPhaseNone} {
		rs := &raceStore{deletes: []int64{0, 0, 1}, rereads: []*store.Worker{phaseRow(CleanupPhaseSettled), phaseRow(phase)}}
		err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New())
		if !errors.Is(err, ErrWorkerDeleteRaced) || errors.Is(err, ErrWorkerNotFound) {
			t.Fatalf("final phase %s: err = %v, want ErrWorkerDeleteRaced", phase, err)
		}
		if rs.deleteCalls != 2 || rs.rereadCalls != 2 {
			t.Fatalf("final phase %s: deletes=%d rereads=%d, want exactly 2 each (no third attempt)", phase, rs.deleteCalls, rs.rereadCalls)
		}
	}
}

// (d) An active run or a custody hold that appears before the retry is refused by the same
// guard as a first attempt, and the retry never reaches the DELETE.
func TestDeleteWorkerRetryReappliesGuards(t *testing.T) {
	t.Run("active run", func(t *testing.T) {
		rs := &raceStore{
			fakeStore:  &fakeStore{listActiveRows: []store.ListWorkerNonTerminalRunsRow{{ID: uuid.New(), Kind: "issue", Status: "queued"}}},
			deletes:    []int64{0, 1},
			rereads:    []*store.Worker{phaseRow(CleanupPhaseSettled)},
			activeRuns: []int64{0, 1},
		}
		err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New())
		var ar *WorkerHasActiveRunsError
		if !errors.As(err, &ar) || ar.Total != 1 || !errors.Is(err, ErrWorkerHasActiveRuns) {
			t.Fatalf("err = %v, want the active-runs refusal", err)
		}
		if rs.deleteCalls != 1 {
			t.Fatalf("deletes = %d, want 1 (the retry must not reach the DELETE)", rs.deleteCalls)
		}
	})
	t.Run("custody hold", func(t *testing.T) {
		rs := &raceStore{
			fakeStore: &fakeStore{listCustodyRows: []store.ListOpenCustodyHoldsForWorkerRow{{ID: uuid.New(), RunID: uuid.New(), Generation: 1}}},
			deletes:   []int64{0, 1},
			rereads:   []*store.Worker{phaseRow(CleanupPhaseSettled)},
			custody:   []int64{0, 1},
		}
		err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New())
		var ch *WorkerHasCustodyError
		if !errors.As(err, &ch) || ch.Holds != 1 || !errors.Is(err, ErrWorkerHasCustody) {
			t.Fatalf("err = %v, want the custody refusal", err)
		}
		if rs.deleteCalls != 1 {
			t.Fatalf("deletes = %d, want 1 (the retry must not reach the DELETE)", rs.deleteCalls)
		}
	})
}

// (e) Lookup errors other than not-found propagate unchanged from either re-read; a worker that
// vanished before the second re-read is a plain not-found.
func TestDeleteWorkerRereadErrorsAndAbsence(t *testing.T) {
	boom := errors.New("db down")
	t.Run("first reread error", func(t *testing.T) {
		rs := &raceStore{deletes: []int64{0}, rereadErrs: []error{boom}}
		if err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, boom) || errors.Is(err, ErrWorkerNotFound) {
			t.Fatalf("err = %v, want the lookup error unchanged", err)
		}
		if rs.deleteCalls != 1 {
			t.Fatalf("deletes = %d, want 1", rs.deleteCalls)
		}
	})
	t.Run("second reread error", func(t *testing.T) {
		rs := &raceStore{deletes: []int64{0, 0}, rereads: []*store.Worker{phaseRow(CleanupPhaseSettled)}, rereadErrs: []error{nil, boom}}
		if err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the lookup error unchanged", err)
		}
		if rs.deleteCalls != 2 {
			t.Fatalf("deletes = %d, want 2", rs.deleteCalls)
		}
	})
	t.Run("absent on second reread", func(t *testing.T) {
		rs := &raceStore{deletes: []int64{0, 0}, rereads: []*store.Worker{phaseRow(CleanupPhaseSettled), nil}}
		if err := newRaceSvc(t, rs).DeleteWorker(context.Background(), uuid.New(), uuid.New()); err != ErrWorkerNotFound {
			t.Fatalf("err = %v, want bare ErrWorkerNotFound", err)
		}
	})
}
