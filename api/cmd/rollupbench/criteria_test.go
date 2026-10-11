package main

import "testing"

func cell(shape, stratum string, readers, n int, p95 float64) Cell {
	return Cell{Shape: shape, Stratum: stratum, Readers: readers, Iterations: n, Latency: Latency{N: n, P50Ms: p95 / 2, P95Ms: p95}}
}

func TestCompareFailsOnMissingOrEmptyCells(t *testing.T) {
	live := &LiveBaseline{Tx: []Cell{cell("s", "small", 8, 10, 10)}}
	arch := []Cell{cell("s", "small", 8, 10, 12)}
	post := []Cell{cell("s", "small", 8, 10, 12)}
	if rows, ok := compare(arch, post, live, []string{"s"}, []string{"small"}, 8, 0, 0); !ok || len(rows) != 1 {
		t.Fatalf("a complete pair within 3x must pass: ok=%v rows=%v", ok, rows)
	}
	cases := map[string]struct{ arch, post, live []Cell }{
		"archived missing":    {nil, post, live.Tx},
		"after-vacuum absent": {arch, nil, live.Tx},
		"live missing":        {arch, post, nil},
		"zero iterations":     {[]Cell{cell("s", "small", 8, 0, 0)}, post, live.Tx},
		"errors":              {[]Cell{{Shape: "s", Stratum: "small", Readers: 8, Iterations: 5, Errors: 1, Latency: Latency{N: 5, P95Ms: 1}}}, post, live.Tx},
		"over 3x":             {[]Cell{cell("s", "small", 8, 10, 31)}, post, live.Tx},
	}
	for name, c := range cases {
		if _, ok := compare(c.arch, c.post, &LiveBaseline{Tx: c.live}, []string{"s"}, []string{"small"}, 8, 0, 0); ok {
			t.Errorf("%s: compare passed", name)
		}
	}
	if rows, ok := compare(nil, nil, &LiveBaseline{}, []string{"s"}, []string{"small"}, 8, 0, 0); ok || len(rows) != 1 {
		t.Errorf("a group with no cell at all must fail with a reason: ok=%v rows=%v", ok, rows)
	}
}

func TestCompareUsesP50BelowTheSampleFloor(t *testing.T) {
	live := &LiveBaseline{Tx: []Cell{cell("s", "largest", 8, 10, 100)}}
	slowTail := cell("s", "largest", 8, 10, 1000) // p95 is 10x live; live p50 is 50
	slowTail.Latency.P50Ms = 120                  // 2.4x live p50
	rows, ok := compare([]Cell{slowTail}, []Cell{slowTail}, live, []string{"s"}, []string{"largest"}, 8, 0, minSamplesP95)
	if !ok || rows[0].Why == "" {
		t.Fatalf("10 samples must be judged on p50 (2.4x live) and say so: ok=%v rows=%v", ok, rows)
	}
	slowTail.Latency.N = minSamplesP95
	slowTail.Iterations = minSamplesP95
	live.Tx[0].Latency.N = minSamplesP95
	if _, ok := compare([]Cell{slowTail}, []Cell{slowTail}, live, []string{"s"}, []string{"largest"}, 8, 0, minSamplesP95); ok {
		t.Fatal("with enough samples the p95 (10x live) must fail")
	}
}
