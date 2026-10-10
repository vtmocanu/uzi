package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type pricingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pricingClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *pricingClock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

type pricingStore struct {
	fakeStore
	calls atomic.Int32
	query func(context.Context, store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error)
}

// Evaluate's custody fake normally records arguments in a slice; this concurrent
// fake has no custody owners and needs no mutable recording.
func (f *pricingStore) ListOwnersOverCustodyLimit(context.Context, store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *pricingStore) ListRecentUnpricedCodexModels(ctx context.Context, p store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
	f.calls.Add(1)
	if f.query != nil {
		return f.query(ctx, p)
	}
	return nil, nil
}
func pricingResult(t *testing.T, s *Service) apitypes.HealthCheckDTO {
	t.Helper()
	d, err := s.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Checks {
		if c.ID == "pricing.codex" {
			return c
		}
	}
	t.Fatal("pricing.codex missing from Evaluate")
	return apitypes.HealthCheckDTO{}
}
func TestPricingReasonsAndParameters(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	f := &pricingStore{}
	f.query = func(_ context.Context, p store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
		if !p.Cutoff.Valid || !p.Cutoff.Time.Equal(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("cutoff: %+v", p.Cutoff)
		}
		if !reflect.DeepEqual(p.Priced, []string{"gpt-6-astra", "gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol"}) {
			t.Errorf("priced: %#v", p.Priced)
		}
		return []store.ListRecentUnpricedCodexModelsRow{{Model: "gpt-5.5", Runs: 3}, {Model: "gpt-5.6-sol", Runs: 2}}, nil
	}
	c := pricingResult(t, New(Config{Store: f, Now: func() time.Time { return now }}))
	want := []apitypes.HealthEvidenceDTO{{Label: "gpt-5.5", Value: "3 runs, no price"}, {Label: "gpt-5.6-sol", Value: "2 runs, promotional price expired"}}
	if c.Severity != sevWarn || !reflect.DeepEqual(c.Evidence, want) {
		t.Fatalf("check: %+v", c)
	}
	if c.Scope != "instance" || c.Group != groupHousekeeping || c.Title != "Codex price coverage" || c.Doc == nil || *c.Doc != "admin-health" {
		t.Fatalf("metadata: %+v", c)
	}
}
func TestPricingEvidenceCapAndSanitization(t *testing.T) {
	f := &pricingStore{}
	f.query = func(context.Context, store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
		rows := make([]store.ListRecentUnpricedCodexModelsRow, 11)
		for i := range rows {
			rows[i] = store.ListRecentUnpricedCodexModelsRow{Model: fmt.Sprintf("model-%02d", i), Runs: 11 - int64(i)}
		}
		rows[0].Model = "bad\n\t\u001b" + strings.Repeat("x", 200)
		return rows, nil
	}
	c := pricingResult(t, New(Config{Store: f}))
	if len(c.Evidence) != 11 || c.Evidence[10].Label != "and more" {
		t.Fatalf("cap: %+v", c.Evidence)
	}
	if c.Evidence[0].Label != "bad  "+strings.Repeat("x", 91) || c.Evidence[0].Value != "11 runs, no price" || c.Evidence[9].Label != "model-09" {
		t.Fatalf("evidence: %+v", c.Evidence)
	}
	for _, e := range c.Evidence {
		if strings.ContainsAny(e.Label+e.Value, "\n\t\u001b") || len(e.Label) > 96 || len(e.Value) > 96 {
			t.Fatalf("unsafe: %+v", e)
		}
	}
}
func TestPricingCacheLifecycle(t *testing.T) {
	start := time.Date(2026, 11, 20, 23, 59, 0, 0, time.UTC)
	clock := &pricingClock{t: start}
	f := &pricingStore{}
	var queryErr error
	var rows []store.ListRecentUnpricedCodexModelsRow
	f.query = func(_ context.Context, p store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
		if f.calls.Load() == 1 && !reflect.DeepEqual(p.Priced, []string{"gpt-5.6-sol", "gpt-6-astra", "gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol"}) {
			t.Errorf("pre-expiry priced: %v", p.Priced)
		}
		if f.calls.Load() == 2 && (!p.Cutoff.Time.Equal(time.Date(2026, 11, 14, 0, 9, 0, 0, time.UTC)) || !reflect.DeepEqual(p.Priced, []string{"gpt-6-astra", "gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol"})) {
			t.Errorf("refresh parameters: %+v", p)
		}
		return rows, queryErr
	}
	s := New(Config{Store: f, Now: clock.now})
	c := pricingResult(t, s)
	if c.Severity != sevOK || c.Summary != "no recent Codex usage on unpriced models" || c.Evidence == nil || len(c.Evidence) != 0 {
		t.Fatalf("empty: %+v", c)
	}
	queryErr = errors.New("secret backend error")
	clock.set(start.Add(10*time.Minute - time.Nanosecond))
	if pricingResult(t, s).Severity != sevOK || f.calls.Load() != 1 {
		t.Fatal("cache expired too soon")
	}
	clock.set(start.Add(10 * time.Minute))
	c = pricingResult(t, s)
	if c.Severity != sevUnknown || strings.Contains(c.Summary, "secret") || f.calls.Load() != 2 {
		t.Fatalf("expired ok survived failure: %+v", c)
	}
	queryErr = nil
	rows = []store.ListRecentUnpricedCodexModelsRow{{Model: "unknown", Runs: 4}}
	clock.set(start.Add(20*time.Minute - time.Nanosecond))
	if pricingResult(t, s).Severity != sevUnknown || f.calls.Load() != 2 {
		t.Fatal("unknown not cached")
	}
	clock.set(start.Add(20 * time.Minute))
	if pricingResult(t, s).Severity != sevWarn || f.calls.Load() != 3 {
		t.Fatal("failed to recover")
	}
	rows = nil
	clock.set(start.Add(30 * time.Minute))
	if pricingResult(t, s).Severity != sevOK || f.calls.Load() != 4 {
		t.Fatal("warning not replaced")
	}
}
func TestPricingCopiesAndSeparateInstances(t *testing.T) {
	f := &pricingStore{query: func(context.Context, store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
		return []store.ListRecentUnpricedCodexModelsRow{{Model: "unknown", Runs: 2}}, nil
	}}
	s := New(Config{Store: f})
	mutate := func(c apitypes.HealthCheckDTO) {
		c.Evidence[0].Label = "mutated"
		c.Evidence[0].Value = "mutated"
		*c.Doc = "mutated"
		if c.Action != nil {
			*c.Action = "mutated"
		}
	}
	c := pricingResult(t, s)
	original := c.Evidence[0]
	mutate(c) // refresh result
	hit := pricingResult(t, s)
	if hit.Evidence[0] != original || *hit.Doc != "admin-health" {
		t.Fatalf("refresh aliases cache: %+v", hit)
	}
	mutate(hit) // hit result
	again := pricingResult(t, s)
	if again.Evidence[0] != original || *again.Doc != "admin-health" {
		t.Fatalf("hit aliases cache: %+v", again)
	}
	pricingResult(t, New(Config{Store: f}))
	if f.calls.Load() != 2 {
		t.Fatalf("instances share cache: %d", f.calls.Load())
	}
}
func TestPricingFiveSecondTimeout(t *testing.T) {
	f := &pricingStore{}
	f.query = func(ctx context.Context, _ store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining < 4*time.Second || remaining > 5*time.Second {
			t.Errorf("deadline remaining: %v, present=%v", remaining, ok)
		}
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("timeout: %v", ctx.Err())
		}
		return nil, ctx.Err()
	}
	if c := pricingResult(t, New(Config{Store: f})); c.Severity != sevUnknown {
		t.Fatalf("timeout check: %+v", c)
	}
}
func TestPricingConcurrentEvaluate(t *testing.T) {
	const n = 12
	clock := &pricingClock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	f := &pricingStore{}
	s := New(Config{Store: f, Now: clock.now})
	pricingResult(t, s) // warm empty cache
	clock.set(clock.now().Add(10 * time.Minute))
	entered, release := make(chan struct{}), make(chan struct{})
	f.query = func(context.Context, store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
		if f.calls.Load() == 2 {
			close(entered)
		}
		<-release
		return []store.ListRecentUnpricedCodexModelsRow{{Model: "unknown", Runs: 7}}, nil
	}
	// Every caller reaches the evaluation clock before any may query.
	var arrived atomic.Int32
	barrier := make(chan struct{})
	s.now = func() time.Time {
		if arrived.Add(1) == n {
			close(barrier)
		}
		<-barrier
		return clock.now()
	}
	results := make(chan Doc, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d, err := s.Evaluate(context.Background()); results <- d; errs <- err }()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("query not entered")
	}
	if f.calls.Load() != 2 {
		t.Errorf("queries before release: %d, want warm + one refresh", f.calls.Load())
	}
	close(release)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var checks []apitypes.HealthCheckDTO
	for d := range results {
		for _, c := range d.Checks {
			if c.ID == "pricing.codex" {
				checks = append(checks, c)
			}
		}
	}
	if len(checks) != n || f.calls.Load() != 2 {
		t.Fatalf("checks=%d queries=%d", len(checks), f.calls.Load())
	}
	checks[0].Evidence[0].Label = "mutated"
	*checks[0].Doc = "mutated"
	for _, c := range checks[1:] {
		if c.Evidence[0].Label != "unknown" || *c.Doc != "admin-health" || c.Severity != sevWarn {
			t.Fatalf("shared caller result: %+v", c)
		}
	}
}
