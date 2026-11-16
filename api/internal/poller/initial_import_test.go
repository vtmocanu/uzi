package poller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// RequestImport is called from the enable request handler: it must never block and a
// burst must coalesce into one wake plus one entry per distinct repo (issue #4399).
func TestRequestImportNonBlockingAndCoalesces(t *testing.T) {
	e := New(nil, nil, time.Minute, 10)
	a, b := uuid.New(), uuid.New()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					e.RequestImport(a)
				} else {
					e.RequestImport(b)
				}
			}(i)
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RequestImport blocked with no consumer on the wake channel")
	}

	if n := len(e.importWake); n != 1 {
		t.Fatalf("pending import wakes = %d, want 1 (coalesced)", n)
	}
	e.importMu.Lock()
	_, hasA := e.importReq[a]
	_, hasB := e.importReq[b]
	n := len(e.importReq)
	e.importMu.Unlock()
	if n != 2 || !hasA || !hasB {
		t.Fatalf("requested set = %d entries (a=%v b=%v), want exactly the two distinct repos", n, hasA, hasB)
	}
}

// A wake with nothing requested (the set was already drained by an earlier pass) must
// do no work at all: no repo listing, no sync.
func TestImportRequestedWithEmptySetListsNothing(t *testing.T) {
	e := New(nil, nil, time.Minute, 10) // nil RepoStore: a listing would panic
	e.importRequested(context.Background())
}

type recordingImportStore struct {
	mu        sync.Mutex
	claimErr  error
	claimRows int64
	finishes  []store.FinishRepoInitialImportParams
}

func (s *recordingImportStore) ClaimRepoInitialImport(context.Context, store.ClaimRepoInitialImportParams) (int64, error) {
	return s.claimRows, s.claimErr
}

func (s *recordingImportStore) FinishRepoInitialImport(_ context.Context, a store.FinishRepoInitialImportParams) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes = append(s.finishes, a)
	return 1, nil
}

// The persisted class is constrained to the closed set; a value outside it (a future
// forge.ErrorClass) must be written as other, never fail the CHECK and strand the row
// in running.
func TestFinishImportMapsUnknownClassToOther(t *testing.T) {
	s := &recordingImportStore{}
	e := New(nil, nil, time.Minute, 10)
	e.SetImportStateStore(s)
	r := store.ListEnabledReposWithConnectionsRow{ID: uuid.New(), InitialImportGeneration: 7}

	for _, tc := range []struct{ in, want string }{
		{"timeout", "timeout"},
		{"auth", "auth"},
		{"server_error", "server_error"},
		{"rate_limited", "rate_limited"},
		{"other", "other"},
		{"quota_exhausted_v2", "other"},
		{"", "other"},
	} {
		e.finishImport(context.Background(), r, false, forge.ErrorClass(tc.in))
		got := s.finishes[len(s.finishes)-1]
		if got.State != "failed" || !got.ErrorClass.Valid || got.ErrorClass.String != tc.want || got.Generation != 7 || got.ID != r.ID {
			t.Errorf("class %q: wrote %+v, want failed/%s at generation 7", tc.in, got, tc.want)
		}
	}

	e.finishImport(context.Background(), r, true, forge.ErrorClassOther)
	if got := s.finishes[len(s.finishes)-1]; got.State != "succeeded" || got.ErrorClass.Valid {
		t.Errorf("success wrote %+v, want succeeded with a NULL class", got)
	}
}

// A failed claim write is treated as not claimed (the row keeps its state and a later
// tick retries); zero rows means the generation moved.
func TestClaimImportErrorAndLostClaimAreNotClaimed(t *testing.T) {
	r := store.ListEnabledReposWithConnectionsRow{ID: uuid.New(), InitialImportGeneration: 3}
	for _, tc := range []struct {
		name string
		s    *recordingImportStore
		want bool
	}{
		{"claimed", &recordingImportStore{claimRows: 1}, true},
		{"generation moved", &recordingImportStore{claimRows: 0}, false},
		{"write error", &recordingImportStore{claimRows: 1, claimErr: errors.New("db down")}, false},
	} {
		e := New(nil, nil, time.Minute, 10)
		e.SetImportStateStore(tc.s)
		if got := e.claimImport(context.Background(), r); got != tc.want {
			t.Errorf("%s: claimed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type classedErr struct{ class forge.ErrorClass }

func (e classedErr) Error() string           { return "classed" }
func (e classedErr) Class() forge.ErrorClass { return e.class }

func TestImportErrorClass(t *testing.T) {
	live := context.Background()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want forge.ErrorClass
	}{
		{"own deadline", live, fmt.Errorf("list: %w", context.DeadlineExceeded), forge.ErrorClassTimeout},
		{"tick deadline with an unrelated error", expired, errors.New("EOF"), forge.ErrorClassTimeout},
		{"classified auth", live, fmt.Errorf("wrap: %w", classedErr{forge.ErrorClassAuth}), forge.ErrorClassAuth},
		{"classified 5xx", live, classedErr{forge.ErrorClassServerError}, forge.ErrorClassServerError},
		{"classified rate limit", live, classedErr{forge.ErrorClassRateLimited}, forge.ErrorClassRateLimited},
		{"unclassified", live, errors.New("boom"), forge.ErrorClassOther},
		{"cancel is not a timeout", cancelled, errors.New("boom"), forge.ErrorClassOther},
	} {
		if got := importErrorClass(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: class = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestImportOwed(t *testing.T) {
	for _, tc := range []struct {
		state string
		valid bool
		want  bool
	}{
		{"", false, false},
		{"waiting", true, true},
		{"running", true, true},
		{"failed", true, true},
		{"succeeded", true, false},
	} {
		var r store.ListEnabledReposWithConnectionsRow
		r.InitialImportState.String, r.InitialImportState.Valid = tc.state, tc.valid
		if got := importOwed(r); got != tc.want {
			t.Errorf("state %q valid=%v: owed = %v, want %v", tc.state, tc.valid, got, tc.want)
		}
	}
}
