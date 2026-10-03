package healthsvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *syncClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func syncCheck(t *testing.T, svc *Service) apitypes.HealthCheckDTO {
	t.Helper()
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		if c.ID == "forge.sync" {
			return c
		}
	}
	t.Fatal("missing forge.sync")
	return apitypes.HealthCheckDTO{}
}
func wantSync(t *testing.T, svc *Service, severity string) apitypes.HealthCheckDTO {
	t.Helper()
	c := syncCheck(t, svc)
	if c.Severity != severity {
		t.Fatalf("forge.sync = %+v, want %s", c, severity)
	}
	return c
}

// TestForgeSyncFailureBands isolates evaluator thresholds; the production-feed
// mutation target is TestPollerForgeSyncFailureFeed in cmd/server.
func TestForgeSyncFailureBands(t *testing.T) {
	clock := &syncClock{t: fixedNow}
	a, b := uuid.New(), uuid.New()
	fs := &fakeStore{enabledIDs: []uuid.UUID{a, b}}
	reg := NewSyncRegistry(clock.now)
	svc := New(Config{Store: fs, Now: clock.now, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
	reg.Begin(a)(true, forge.ErrorClassOther)
	reg.Begin(b)(false, forge.ErrorClassTimeout)
	clock.set(fixedNow.Add(3*time.Minute - time.Nanosecond))
	wantSync(t, svc, sevOK)
	clock.set(fixedNow.Add(3 * time.Minute))
	c := wantSync(t, svc, sevWarn)
	if c.Evidence[0].Value != "1 of 2 failing" || c.Evidence[1].Value != "0" || c.Evidence[2].Value != "3m" || c.Evidence[3].Value != "timeout" || c.Since == nil || *c.Since != fixedNow.Format(time.RFC3339) {
		t.Fatalf("evidence/baseline: %+v", c)
	}
	if c.Action == nil || *c.Action != "check outbound reachability from the api pod. If other pods reach the forge and only the api process times out, restarting the api pod is a workaround, not a fix." {
		t.Fatalf("action: %+v", c)
	}
	reg.Begin(b)(false, forge.ErrorClassAuth)
	clock.set(fixedNow.Add(10 * time.Minute))
	c = wantSync(t, svc, sevDanger)
	if c.Evidence[3].Value != "auth" || *c.Since != fixedNow.Format(time.RFC3339) {
		t.Fatalf("repeat failure reset baseline: %+v", c)
	}
	reg.Begin(b)(true, forge.ErrorClassOther)
	wantSync(t, svc, sevOK)
}

func TestForgeSyncFirstFailureAfterSuccessGetsFullGrace(t *testing.T) {
	clock := &syncClock{t: fixedNow}
	a, b := uuid.New(), uuid.New()
	reg := NewSyncRegistry(clock.now)
	svc := New(Config{Store: &fakeStore{enabledIDs: []uuid.UUID{a, b}}, Now: clock.now, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
	reg.Begin(a)(true, forge.ErrorClassOther)
	reg.Begin(b)(true, forge.ErrorClassOther)

	firstFailure := fixedNow.Add(time.Hour)
	clock.set(firstFailure)
	reg.Begin(b)(false, forge.ErrorClassTimeout)
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{0, sevOK},
		{3*time.Minute - time.Nanosecond, sevOK},
		{3 * time.Minute, sevWarn},
		{10*time.Minute - time.Nanosecond, sevWarn},
		{10 * time.Minute, sevDanger},
	} {
		clock.set(firstFailure.Add(tc.age))
		c := wantSync(t, svc, tc.want)
		if c.Since == nil || *c.Since != firstFailure.Format(time.RFC3339) {
			t.Fatalf("failure grace baseline: %+v", c)
		}
		// Repeated failed attempts must not extend the grace period.
		reg.Begin(b)(false, forge.ErrorClassTimeout)
	}
	reg.Begin(b)(true, forge.ErrorClassOther)
	wantSync(t, svc, sevOK)
}

func TestForgeSyncBandsAndPending(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []int
		age     time.Duration
		want    string
	}{
		{"none", nil, 0, sevNA},
		{"only pending", []int{0}, 30 * time.Minute, sevUnknown},
		{"pending success", []int{0, 1}, 3 * time.Minute, sevUnknown},
		{"pending aged failure", []int{0, -1}, 3 * time.Minute, sevWarn},
		{"pending long failure", []int{0, -1}, 10 * time.Minute, sevDanger},
		{"single all fail", []int{-1}, 3 * time.Minute, sevDanger},
		{"several all fail", []int{-1, -1, -1}, 3 * time.Minute, sevDanger},
		{"all fail before band", []int{-1, -1}, 3*time.Minute - time.Nanosecond, sevOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &syncClock{t: fixedNow}
			fs := &fakeStore{}
			reg := NewSyncRegistry(clock.now)
			for _, result := range tc.results {
				id := uuid.New()
				fs.enabledIDs = append(fs.enabledIDs, id)
				if result != 0 {
					reg.Begin(id)(result == 1, forge.ErrorClassOther)
				}
			}
			svc := New(Config{Store: fs, Now: clock.now, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
			clock.set(fixedNow.Add(tc.age))
			wantSync(t, svc, tc.want)
		})
	}
	t.Run("unwired enabled", func(t *testing.T) {
		wantSync(t, New(Config{Store: &fakeStore{enabledIDs: []uuid.UUID{uuid.New()}}, Now: func() time.Time { return fixedNow }}), sevUnknown)
	})
}

func TestForgeSyncBaselinesActionsAndDeterministicClass(t *testing.T) {
	for _, tc := range []struct {
		class  forge.ErrorClass
		action string
	}{
		{forge.ErrorClassAuth, "the connection's token is invalid or lacks scope; re-check the forge connection."},
		{forge.ErrorClassServerError, "forge-side; check the forge's status and rate limits, and wait."},
		{forge.ErrorClassRateLimited, "forge-side; check the forge's status and rate limits, and wait."},
		{forge.ErrorClassOther, "read the api logs for `poller:` errors."},
		{forge.ErrorClass("raw-error-canary"), "read the api logs for `poller:` errors."},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			clock := &syncClock{t: fixedNow}
			id := uuid.New()
			fs := &fakeStore{enabledIDs: []uuid.UUID{id}}
			r := NewSyncRegistry(clock.now)
			// A first failure without any success starts at the failure, not registration.
			complete := r.Begin(id)
			clock.set(fixedNow.Add(time.Hour))
			complete(false, tc.class)
			svc := New(Config{Store: fs, Now: clock.now, ForgeSyncRegistry: r, ForgeSyncInterval: time.Minute})
			clock.set(fixedNow.Add(time.Hour + 3*time.Minute))
			c := wantSync(t, svc, sevDanger)
			if *c.Since != fixedNow.Add(time.Hour).Format(time.RFC3339) || *c.Action != tc.action {
				t.Fatalf("%+v", c)
			}
			if tc.class == forge.ErrorClass("raw-error-canary") && c.Evidence[3].Value != "other" {
				t.Fatalf("invalid class escaped normalization: %+v", c)
			}
			serialized, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(serialized), "canary") {
				t.Fatal(c)
			}
		})
	}
	clock := &syncClock{t: fixedNow}
	a, b := uuid.New(), uuid.New()
	r := NewSyncRegistry(clock.now)
	fs := &fakeStore{enabledIDs: []uuid.UUID{a, b}}
	r.Begin(a)(false, forge.ErrorClassTimeout)
	r.Begin(b)(false, forge.ErrorClassAuth)
	clock.set(fixedNow.Add(3 * time.Minute))
	svc := New(Config{Store: fs, Now: clock.now, ForgeSyncRegistry: r, ForgeSyncInterval: time.Minute})
	c := wantSync(t, svc, sevDanger)
	fs.enabledIDs = []uuid.UUID{b, a}
	reversed := wantSync(t, svc, sevDanger)
	if c.Evidence[3].Value != "auth" || reversed.Evidence[3].Value != "auth" {
		t.Fatalf("tie: %+v / %+v", c, reversed)
	}
	clock.set(fixedNow.Add(4 * time.Minute))
	r.Begin(a)(false, forge.ErrorClassServerError)
	if c = wantSync(t, svc, sevDanger); c.Evidence[3].Value != "server_error" {
		t.Fatal(c)
	}
}

func TestForgeSyncInvalidationAndEnumerationFailure(t *testing.T) {
	clock := &syncClock{t: fixedNow}
	id := uuid.New()
	r := NewSyncRegistry(clock.now)
	fs := &fakeStore{enabledIDs: []uuid.UUID{id}}
	svc := New(Config{Store: fs, Now: clock.now, ForgeSyncRegistry: r, ForgeSyncInterval: time.Minute})
	r.Begin(id)(false, forge.ErrorClassAuth)
	clock.set(fixedNow.Add(3 * time.Minute))
	fs.enabledErr = errors.New("raw-error-canary")
	wantSync(t, svc, sevUnknown)
	fs.enabledErr = nil
	wantSync(t, svc, sevDanger) // error did not prune
	for _, ok := range []bool{true, false} {
		stale := r.Begin(id)
		r.Invalidate(id)
		stale(ok, forge.ErrorClassTimeout)
		wantSync(t, svc, sevUnknown)
		r.Begin(id)(true, forge.ErrorClassOther)
		wantSync(t, svc, sevOK)
	}
	// A missed disable/re-enable enumeration still resets through the runtime hook.
	r.Begin(id)(false, forge.ErrorClassAuth)
	r.Invalidate(id)
	r.Invalidate(id)
	wantSync(t, svc, sevUnknown)
	// A worker that starts Begin after invalidation is eligible.
	r.Begin(id)(true, forge.ErrorClassOther)
	wantSync(t, svc, sevOK)
	fs.enabledIDs = nil
	wantSync(t, svc, sevNA)
	fs.enabledIDs = []uuid.UUID{id}
	wantSync(t, svc, sevUnknown)
}

type blockedSyncStore struct {
	*fakeStore
	entered chan struct{}
	release chan struct{}
	ids     []uuid.UUID
}

func (f *blockedSyncStore) ListEnabledRepoIDs(context.Context) ([]uuid.UUID, error) {
	close(f.entered)
	<-f.release
	return f.ids, nil
}

func TestForgeSyncSnapshotInvalidationBarrier(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(fmtBool(absent), func(t *testing.T) {
			clock := &syncClock{t: fixedNow}
			id := uuid.New()
			reg := NewSyncRegistry(clock.now)
			reg.Begin(id)(false, forge.ErrorClassAuth)
			clock.set(fixedNow.Add(3 * time.Minute))
			fs := &blockedSyncStore{fakeStore: &fakeStore{}, entered: make(chan struct{}), release: make(chan struct{}), ids: []uuid.UUID{id}}
			if absent {
				fs.ids = nil
			}
			svc := New(Config{Store: fs, Now: clock.now, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
			result := make(chan Doc, 1)
			go func() {
				doc, err := svc.Evaluate(context.Background())
				if err != nil {
					panic(err)
				}
				result <- doc
			}()
			<-fs.entered
			reg.Invalidate(id)
			reg.Begin(id)(true, forge.ErrorClassOther)
			close(fs.release)
			doc := <-result
			want := sevUnknown
			if absent {
				want = sevNA
			}
			sev, _ := check(t, doc, "forge.sync")
			if sev != want {
				t.Fatalf("old snapshot counted: %+v", doc)
			}
			next := New(Config{Store: &fakeStore{enabledIDs: []uuid.UUID{id}}, Now: clock.now, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
			wantSync(t, next, sevOK) // old absent snapshot did not prune replacement
		})
	}
}
func fmtBool(b bool) string {
	if b {
		return "absent"
	}
	return "enabled"
}

func TestForgeSyncConcurrentOperations(t *testing.T) {
	clock := &syncClock{t: fixedNow}
	reg := NewSyncRegistry(clock.now)
	id := uuid.New()
	svc := New(Config{Store: &fakeStore{enabledIDs: []uuid.UUID{id}}, Now: clock.now, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
	var wg sync.WaitGroup
	// Fixed 8x100 independent operations; a reset never blocks sibling work.
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch worker % 4 {
				case 0:
					reg.Begin(id)(true, forge.ErrorClassOther)
				case 1:
					reg.Begin(id)(false, forge.ErrorClassTimeout)
				case 2:
					reg.Invalidate(id)
				case 3:
					if _, err := svc.Evaluate(context.Background()); err != nil {
						panic(err)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}
