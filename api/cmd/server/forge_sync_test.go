package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/poller"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type wiringClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *wiringClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *wiringClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type syncTimeoutError struct{}

func (syncTimeoutError) Error() string           { return "fixture forge timeout" }
func (syncTimeoutError) Class() forge.ErrorClass { return forge.ErrorClassTimeout }

type syncForge struct {
	forgetest.BaseFake
	mu               sync.Mutex
	failures         map[int64]error
	panics           bool
	entered, release chan struct{}
	calls            map[int64]int
}

func (f *syncForge) ListIssues(_ context.Context, id int64, _ forge.ListIssuesOptions) ([]forge.Issue, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[id]++
	if f.panics {
		panic("pre-success panic")
	}
	return nil, f.failures[id]
}

type wiringStore struct {
	healthsvc.Store
	forgesvc.IssueStore
	rows                           []store.ListEnabledReposWithConnectionsRow
	siblingErr                     error
	siblingPanic                   bool
	siblingEntered, siblingRelease chan struct{}
}

func (s *wiringStore) ListEnabledReposWithConnections(context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	return s.rows, nil
}
func (s *wiringStore) ListEnabledRepoIDs(context.Context) ([]uuid.UUID, error) {
	ids := []uuid.UUID{}
	for _, r := range s.rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}
func (s *wiringStore) ListAllWorkers(context.Context) ([]store.ListAllWorkersRow, error) {
	return nil, nil
}
func (s *wiringStore) ListLargestRunDiskForWorkers(context.Context, store.ListLargestRunDiskForWorkersParams) ([]store.WorkerRunDisk, error) {
	return nil, nil
}
func (s *wiringStore) ListOwnersWaitingNoCapacity(context.Context, store.ListOwnersWaitingNoCapacityParams) ([]store.ListOwnersWaitingNoCapacityRow, error) {
	return nil, nil
}
func (s *wiringStore) ListWaitingWorkerRuns(context.Context) ([]store.ListWaitingWorkerRunsRow, error) {
	return nil, nil
}
func (s *wiringStore) OldestUndispatchedTaskRun(context.Context) (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{}, nil
}
func (s *wiringStore) CountUsersPausedWithEnabledSchedules(context.Context, pgtype.Timestamptz) (int64, error) {
	return 0, nil
}
func (s *wiringStore) ListGaveUpColumnMoves(context.Context, store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error) {
	return nil, nil
}
func (s *wiringStore) ListOwnersOverCustodyLimit(context.Context, store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (s *wiringStore) ListCustodyHoldsForOwner(context.Context, store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
	return nil, nil
}
func (s *wiringStore) GetControllerReport(context.Context) (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{}, nil
}
func (s *wiringStore) CountEligibleCIWatchRefsPerRepo(context.Context, pgtype.Timestamptz) ([]store.CountEligibleCIWatchRefsPerRepoRow, error) {
	return nil, nil
}
func (s *wiringStore) DeleteIssuesNotIn(context.Context, store.DeleteIssuesNotInParams) (int64, error) {
	return 0, nil
}
func (s *wiringStore) ListMRWatchCandidates(context.Context, uuid.UUID) ([]store.ListMRWatchCandidatesRow, error) {
	if s.siblingEntered != nil {
		close(s.siblingEntered)
		<-s.siblingRelease
	}
	if s.siblingPanic {
		panic("sibling panic")
	}
	return nil, s.siblingErr
}
func (s *wiringStore) ListBoardFreeMRStateWatchCandidates(context.Context, uuid.UUID) ([]store.ListBoardFreeMRStateWatchCandidatesRow, error) {
	return nil, s.siblingErr
}
func (s *wiringStore) ListPRDLinkPatchCandidates(context.Context, store.ListPRDLinkPatchCandidatesParams) ([]store.ListPRDLinkPatchCandidatesRow, error) {
	return nil, s.siblingErr
}
func (s *wiringStore) ListFiledIssueCloseEdges(context.Context, store.ListFiledIssueCloseEdgesParams) ([]store.ListFiledIssueCloseEdgesRow, error) {
	return nil, s.siblingErr
}
func (s *wiringStore) ListFindingIssueCloseEdges(context.Context, uuid.UUID) ([]store.ListFindingIssueCloseEdgesRow, error) {
	return nil, s.siblingErr
}

type invalidateSink struct{ invalidate func(uuid.UUID) }

func (s *invalidateSink) SetRepoSyncInvalidator(f func(uuid.UUID)) { s.invalidate = f }

func newSyncWiring(t *testing.T, count int, interval time.Duration) (*poller.Engine, *healthsvc.Service, *wiringStore, *syncForge, *wiringClock, *invalidateSink) {
	t.Helper()
	clock := &wiringClock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("fixture-token"))
	if err != nil {
		t.Fatal(err)
	}
	st := &wiringStore{}
	for i := 0; i < count; i++ {
		st.rows = append(st.rows, store.ListEnabledReposWithConnectionsRow{ID: uuid.New(), ForgeProjectID: int64(i + 1), ForgeType: "gitlab", BaseUrl: "https://forge.example", TokenCiphertext: sealed})
	}
	f := &syncForge{failures: make(map[int64]error), calls: make(map[int64]int)}
	svc := forgesvc.NewWithForgeBuilder(st, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return f, nil })
	engine := poller.New(svc, st, interval, 10)
	cfg := wireForgeSync(engine, clock.now)
	if cfg.ForgeSyncInterval != engine.Interval() {
		t.Fatal("effective interval not wired")
	}
	cfg.Store = st
	cfg.Now = clock.now
	sink := &invalidateSink{}
	wireHandlerForgeSync(sink, cfg)
	return engine, healthsvc.New(cfg), st, f, clock, sink
}
func wiringCheck(t *testing.T, svc *healthsvc.Service, want string) apitypes.HealthCheckDTO {
	t.Helper()
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		if c.ID == "forge.sync" {
			if c.Severity != want {
				t.Fatalf("forge.sync=%+v, want %s", c, want)
			}
			return c
		}
	}
	t.Fatal("missing forge.sync")
	return apitypes.HealthCheckDTO{}
}

// TestPollerForgeSyncFailureFeed traverses real PollOnce and forgesvc issue sync,
// using the exact private wiring helpers called by main.
func TestPollerForgeSyncFailureFeed(t *testing.T) {
	e, svc, _, f, clock, _ := newSyncWiring(t, 2, time.Minute)
	f.failures[2] = syncTimeoutError{}
	e.PollOnce(context.Background())
	start := clock.now()
	clock.set(start.Add(3*time.Minute - time.Nanosecond))
	wiringCheck(t, svc, "ok")
	clock.set(start.Add(3 * time.Minute))
	c := wiringCheck(t, svc, "warn")
	if c.Evidence[0].Value != "1 of 2 failing" || c.Evidence[3].Value != "timeout" {
		t.Fatal(c)
	}
	// Another attempt keeps the original failure baseline; the successful repo
	// takes the incremental path while the failed full sync is retried.
	e.PollOnce(context.Background())
	clock.set(start.Add(10 * time.Minute))
	wiringCheck(t, svc, "danger")
	delete(f.failures, 2)
	e.PollOnce(context.Background())
	wiringCheck(t, svc, "ok")
}

func TestPollerForgeSyncAllFailAndPending(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures []int64
		pending  bool
		age      time.Duration
		want     string
	}{
		{"all fail before grace", []int64{1, 2}, false, 3*time.Minute - time.Nanosecond, "ok"},
		{"all fail at grace", []int64{1, 2}, false, 3 * time.Minute, "danger"},
		{"pending with success", nil, true, 3 * time.Minute, "unknown"},
		{"pending with young failure", []int64{1}, true, time.Minute, "unknown"},
		{"pending with aged failure", []int64{1}, true, 3 * time.Minute, "warn"},
		{"pending with long failure", []int64{1}, true, 10 * time.Minute, "danger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, svc, st, f, clock, sink := newSyncWiring(t, 2, time.Minute)
			for _, id := range tc.failures {
				f.failures[id] = syncTimeoutError{}
			}
			e.PollOnce(context.Background())
			if tc.pending {
				sink.invalidate(st.rows[1].ID)
			}
			clock.set(clock.now().Add(tc.age))
			c := wiringCheck(t, svc, tc.want)
			wantPending := "0"
			if tc.pending {
				wantPending = "1"
			}
			if c.Evidence[1].Value != wantPending {
				t.Fatal(c)
			}
		})
	}
}

func TestPollerForgeSyncConstructionFullIncrementalAndPanics(t *testing.T) {
	t.Run("construction", func(t *testing.T) {
		e, svc, st, _, clock, _ := newSyncWiring(t, 1, time.Minute)
		st.rows[0].TokenCiphertext = nil
		e.PollOnce(context.Background())
		clock.set(clock.now().Add(3 * time.Minute))
		c := wiringCheck(t, svc, "danger")
		if c.Evidence[3].Value != "other" {
			t.Fatal(c)
		}
	})
	t.Run("pre-success panic", func(t *testing.T) {
		e, svc, _, f, clock, _ := newSyncWiring(t, 1, time.Minute)
		f.panics = true
		e.PollOnce(context.Background())
		clock.set(clock.now().Add(3 * time.Minute))
		c := wiringCheck(t, svc, "danger")
		if c.Evidence[3].Value != "other" {
			t.Fatal(c)
		}
	})
	t.Run("incremental failure", func(t *testing.T) {
		e, svc, _, f, clock, _ := newSyncWiring(t, 1, time.Minute)
		e.PollOnce(context.Background())
		start := clock.now()
		firstFailure := start.Add(time.Hour)
		clock.set(firstFailure)
		f.failures[1] = syncTimeoutError{}
		e.PollOnce(context.Background())
		wiringCheck(t, svc, "ok")
		clock.set(firstFailure.Add(3*time.Minute - time.Nanosecond))
		wiringCheck(t, svc, "ok")
		clock.set(firstFailure.Add(3 * time.Minute))
		c := wiringCheck(t, svc, "danger")
		if c.Evidence[3].Value != "timeout" || *c.Since != firstFailure.Format(time.RFC3339) {
			t.Fatal(c)
		}
	})
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "sibling failures", true: "sibling panic"}[panics], func(t *testing.T) {
			e, svc, st, _, _, _ := newSyncWiring(t, 1, time.Minute)
			st.siblingErr = errors.New("sibling failure")
			st.siblingPanic = panics
			e.PollOnce(context.Background())
			wiringCheck(t, svc, "ok")
		})
	}
	t.Run("early success blocked sibling", func(t *testing.T) {
		e, svc, st, _, _, _ := newSyncWiring(t, 1, time.Minute)
		st.siblingEntered = make(chan struct{})
		st.siblingRelease = make(chan struct{})
		done := make(chan struct{})
		go func() { e.PollOnce(context.Background()); close(done) }()
		<-st.siblingEntered
		wiringCheck(t, svc, "ok")
		close(st.siblingRelease)
		<-done
	})
	t.Run("effective default interval", func(t *testing.T) {
		e, svc, _, f, clock, _ := newSyncWiring(t, 1, 0)
		f.failures[1] = syncTimeoutError{}
		e.PollOnce(context.Background())
		clock.set(clock.now().Add(3 * time.Minute))
		wiringCheck(t, svc, "danger")
	})
}

func TestPollerForgeSyncQueuedWorkerAfterInvalidation(t *testing.T) {
	e, svc, st, f, clock, sink := newSyncWiring(t, 5, time.Minute)
	f.failures[5] = syncTimeoutError{}
	e.PollOnce(context.Background())
	clock.set(clock.now().Add(3 * time.Minute))
	wiringCheck(t, svc, "warn")
	delete(f.failures, 5)
	f.entered = make(chan struct{}, 64)
	f.release = make(chan struct{})
	done := make(chan struct{})
	go func() { e.PollOnce(context.Background()); close(done) }()
	// Default concurrency is four; worker five cannot begin until a slot is released.
	for i := 0; i < 4; i++ {
		<-f.entered
	}
	sink.invalidate(st.rows[4].ID)
	wiringCheck(t, svc, "unknown")
	close(f.release)
	<-done
	wiringCheck(t, svc, "ok")
}

func TestPollerSyncCompletionExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name                                                                  string
		construction, fullFailure, incrementalFailure, prePanic, siblingPanic bool
		wantOK                                                                bool
		wantClass                                                             forge.ErrorClass
	}{
		{name: "construction", construction: true, wantClass: forge.ErrorClassOther},
		{name: "full", fullFailure: true, wantClass: forge.ErrorClassTimeout},
		{name: "incremental", incrementalFailure: true, wantClass: forge.ErrorClassTimeout},
		{name: "prepanic", prePanic: true, wantClass: forge.ErrorClassOther},
		{name: "siblingpanic", siblingPanic: true, wantOK: true, wantClass: forge.ErrorClassOther},
		{name: "siblings", wantOK: true, wantClass: forge.ErrorClassOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, st, f, _, _ := newSyncWiring(t, 1, time.Minute)
			if tc.incrementalFailure {
				e.PollOnce(context.Background())
			}
			if tc.construction {
				st.rows[0].TokenCiphertext = nil
			}
			if tc.fullFailure || tc.incrementalFailure {
				f.failures[1] = syncTimeoutError{}
			}
			f.panics = tc.prePanic
			st.siblingPanic = tc.siblingPanic
			st.siblingErr = errors.New("sibling")
			begins, completions := 0, 0
			var mu sync.Mutex
			e.SetSyncAttempt(func(id uuid.UUID) func(bool, forge.ErrorClass) {
				mu.Lock()
				begins++
				mu.Unlock()
				if id != st.rows[0].ID {
					t.Errorf("wrong repository %s", id)
				}
				return func(ok bool, class forge.ErrorClass) {
					mu.Lock()
					defer mu.Unlock()
					completions++
					if ok != tc.wantOK || class != tc.wantClass {
						t.Errorf("completion=%v/%s want %v/%s", ok, class, tc.wantOK, tc.wantClass)
					}
				}
			})
			e.PollOnce(context.Background())
			if begins != 1 || completions != 1 {
				t.Fatalf("begin/complete=%d/%d", begins, completions)
			}
		})
	}
}

func TestPollerForgeSyncRuntimeToggleStaleInflight(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			e, svc, st, f, _, sink := newSyncWiring(t, 1, time.Minute)
			if fail {
				f.failures[1] = syncTimeoutError{}
			}
			f.entered = make(chan struct{}, 3)
			f.release = make(chan struct{}, 3)
			done := make(chan struct{})
			go func() { e.PollOnce(context.Background()); close(done) }()
			<-f.entered
			sink.invalidate(st.rows[0].ID) // disable + enable between enumerations
			sink.invalidate(st.rows[0].ID)
			f.release <- struct{}{}
			if !fail {
				for i := 0; i < 2; i++ {
					<-f.entered
					f.release <- struct{}{}
				}
			}
			<-done
			wiringCheck(t, svc, "unknown")
			f.entered = nil
			f.release = nil
			delete(f.failures, 1)
			e.PollOnce(context.Background())
			wiringCheck(t, svc, "ok")
		})
	}
}

func (s *wiringStore) ListRecentUnpricedCodexModels(context.Context, store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
	return nil, nil
}
