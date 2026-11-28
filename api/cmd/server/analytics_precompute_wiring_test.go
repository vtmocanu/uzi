package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/sweeper"
)

func fakePasses() []sweeper.Pass {
	noop := func(context.Context) (int64, error) { return 0, nil }
	return []sweeper.Pass{{Name: "first", Run: noop}, {Name: "usage_fold_materialize", Run: noop}}
}

func passNames(ps []sweeper.Pass) []string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Name)
	}
	return n
}

func TestWireAnalyticsPrecomputeOnPlacesPassBeforeUsageFold(t *testing.T) {
	got, sched := wireAnalyticsPrecompute(context.Background(), nil, 4*time.Hour, fakePasses())
	if sched == nil {
		t.Fatal("no scheduler with the setting on")
	}
	want := []string{"first", "analytics_precompute", "usage_fold_materialize"}
	names := passNames(got)
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("passes = %v, want %v", names, want)
	}
	if got[1].Run == nil {
		t.Fatal("the pass has no Run")
	}
}

func TestWireAnalyticsPrecomputeOffRegistersNothing(t *testing.T) {
	got, sched := wireAnalyticsPrecompute(context.Background(), nil, 0, fakePasses())
	if sched != nil {
		t.Fatal("a scheduler was constructed with the setting off")
	}
	if names := passNames(got); len(names) != 2 || names[0] != "first" || names[1] != "usage_fold_materialize" {
		t.Fatalf("passes = %v, want the input unchanged", names)
	}
}

// main.go is not unit-testable end to end, so pin from source that run() wires the scheduler
// through wireAnalyticsPrecompute with the process context and the sweep slice, and joins it at
// shutdown.
func TestMainWiresAnalyticsPrecomputeAndWait(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, want := range []string{
		"= wireAnalyticsPrecompute(ctx, pool, cfg.AnalyticsSnapshotInterval, sweepPasses)",
		"analyticsPrecompute.Wait()",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
	wire := strings.Index(text, "wireAnalyticsPrecompute(ctx")
	newSweeper := strings.Index(text, "sweeper.New(wsvc, cfg.SweepInterval, sweepPasses...)")
	if wire < 0 || newSweeper < 0 || wire > newSweeper {
		t.Errorf("wireAnalyticsPrecompute must run before sweeper.New (wire %d, new %d)", wire, newSweeper)
	}
}
