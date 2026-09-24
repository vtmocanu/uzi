package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// usageTotalsStore embeds Store so any method other than the one under test panics: a
// test that reaches one has left the path under test.
type usageTotalsStore struct {
	Store
	calls int
	rows  []store.RunUsageTotal
}

func (u *usageTotalsStore) ListRunUsageTotalsForRuns(_ context.Context, _ []uuid.UUID) ([]store.RunUsageTotal, error) {
	u.calls++
	return u.rows, nil
}

// Issue #1620: RunUsageTotalsForRuns keys the page's rollups by run id, leaves a run
// with no usage ABSENT (never a zero row), and issues no query for an empty page.
func TestRunUsageTotalsForRuns(t *testing.T) {
	withUsage, noUsage := uuid.New(), uuid.New()
	fs := &usageTotalsStore{rows: []store.RunUsageTotal{{RunID: withUsage, InputTokens: 42, CostStatus: "metered"}}}
	svc := New(fs, newBox(t), testParams())

	got, err := svc.RunUsageTotalsForRuns(context.Background(), []uuid.UUID{withUsage, noUsage})
	if err != nil {
		t.Fatalf("RunUsageTotalsForRuns: %v", err)
	}
	if row, ok := got[withUsage]; !ok || row.InputTokens != 42 {
		t.Fatalf("run with usage = %+v (present=%t), want 42 input tokens", row, ok)
	}
	if _, ok := got[noUsage]; ok {
		t.Fatal("a run with no usage must be absent from the map, never a zero row")
	}

	empty, err := svc.RunUsageTotalsForRuns(context.Background(), nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty page = %v, %v; want an empty non-nil map", empty, err)
	}
	if fs.calls != 1 {
		t.Fatalf("store called %d times, want 1 (the empty page must not query)", fs.calls)
	}
}
