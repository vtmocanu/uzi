package main

import (
	"strings"
	"testing"
)

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

// evalFixture is a complete, passing set of inputs for evaluate: every case below changes one
// thing and expects the named criterion to flip.
type evalFixture struct {
	r                  *ConfigResult
	live               *LiveBaseline
	attrib             *Attrib
	readers            []int
	largestMsg         int64
	largestTerminalMsg int64
}

func newEvalFixture() *evalFixture {
	const readers = 8
	fx := &evalFixture{readers: []int{1, readers}, largestMsg: 1 << 20, largestTerminalMsg: 1 << 20, attrib: &Attrib{}}
	live := &LiveBaseline{
		ChatTx: ChatCell{Chats: 50, Readers: 1, Latency: Latency{N: 5, P95Ms: 10}},
	}
	r := &ConfigResult{Name: "cfg", Codec: "zstd", ByteBoundKiB: 256, CountCeil: 1000, Sizes: map[string]any{}}
	r.C1 = C1Basis{Basis: "real", Pre: 1000, Post: 400}
	r.Sizes["after_vacuum_run_messages"] = RelReport{Heap: TupleStat{FreeSpace: 500}}
	shapes := []string{"first_page_200", "tail_200", "mid_backward_200", "first_page_1000", "tail_1000", "judge_trace_page_200", "jobs_text_200", "usage_frames_scan"}
	for _, sh := range shapes {
		for _, st := range allStrata {
			r.Latency.PreVacuum = append(r.Latency.PreVacuum, cell(sh, st, readers, 10, 12))
			r.Latency.PostVacuum = append(r.Latency.PostVacuum, cell(sh, st, readers, 10, 12))
			live.Tx = append(live.Tx, cell(sh, st, readers, 10, 10))
		}
	}
	for _, c := range []Cell{cell("full_replay", "largest", readers, minSamplesP95, 12), cell("judge_signal_scan_4000", "large", readers, 10, 100)} {
		r.Latency.PreVacuum = append(r.Latency.PreVacuum, c)
		r.Latency.PostVacuum = append(r.Latency.PostVacuum, c)
	}
	live.Tx = append(live.Tx, cell("full_replay", "largest", readers, minSamplesP95, 10))
	single := cell("first_page_200", "small", 1, 10, 5)
	single.PayloadBytesPerRq, single.AllocBytesPerReq = 1000, 1000
	peak := cell("first_page_200", "small", readers, 10, 5)
	peak.PeakHeapInuse, peak.PeakHeapOverBaseline, peak.HeapBaseline = 1<<20, 1<<20, 1<<20
	r.Latency.PreVacuum = append(r.Latency.PreVacuum, single, peak)
	r.Latency.Chat = []ChatCell{{Chats: 50, Readers: 1, Latency: Latency{N: 5, P95Ms: 15}}}

	r.RoundTrip = RoundTripReport{Runs: 10, Rows: 100, OversizedVerified: 1}
	r.Digests = DigestCheck{ShapeRuns: 10}
	r.Concurrency = &ConcurrencyReport{
		Reads: 10, StragglerInserts: 5, StragglerOverlapped: 2, MixedOnMultiTxRun: 1,
		RoundTrip: &RoundTripReport{Runs: 10}, Digests: &DigestCheck{ShapeRuns: 10},
	}
	r.Backfill = BackfillReport{
		Txs: 10, LargestTx: TxSummary{DurationMs: 100},
		Footprint: FootprintReport{FinalChunks: 300, PeakExtra: 10},
		LiveLoad:  LiveLoadReport{IdleP95Ms: 10, DuringP95: 12, Ratio: 1.2, IdleN: 5, DuringN: 5},
	}
	r.WALTotal = 1000
	fx.r, fx.live = r, live
	return fx
}

func (fx *evalFixture) eval() map[string]Criterion {
	evaluate(fx.r, fx.live, fx.attrib, fx.readers, fx.largestMsg, fx.largestTerminalMsg)
	out := map[string]Criterion{}
	for _, c := range fx.r.Criteria {
		out[c.ID] = c
	}
	return out
}

func dropCells(cells []Cell, shape string) []Cell {
	var out []Cell
	for _, c := range cells {
		if c.Shape != shape {
			out = append(out, c)
		}
	}
	return out
}

func TestEvaluatePassesWithCompleteData(t *testing.T) {
	got := newEvalFixture().eval()
	for _, id := range []string{"C1", "C2", "C3", "C4", "C5", "C6"} {
		if c := got[id]; c.Pass == nil || !*c.Pass {
			t.Errorf("%s must pass on complete data: %+v", id, c)
		}
	}
}

// TestEvaluateFailsInsteadOfPassingVacuously covers every bar whose inputs can be missing or
// zero: each case removes or zeroes one input and expects that criterion to fail.
func TestEvaluateFailsInsteadOfPassingVacuously(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		mutate func(fx *evalFixture)
	}{
		{"C3 chat live p95 zero", "C3", func(fx *evalFixture) { fx.live.ChatTx.Latency.P95Ms = 0 }},
		{"C3 chat live no samples", "C3", func(fx *evalFixture) { fx.live.ChatTx.Latency.N = 0 }},
		{"C3 no archived chat cell", "C3", func(fx *evalFixture) { fx.r.Latency.Chat = nil }},
		{"C3 archived chat cell without samples", "C3", func(fx *evalFixture) { fx.r.Latency.Chat[0].Latency.N = 0 }},
		{"C3 missing judge cell", "C3", func(fx *evalFixture) {
			fx.r.Latency.PreVacuum = dropCells(fx.r.Latency.PreVacuum, "judge_signal_scan_4000")
		}},
		{"C3 missing after-vacuum judge cell", "C3", func(fx *evalFixture) {
			fx.r.Latency.PostVacuum = dropCells(fx.r.Latency.PostVacuum, "judge_signal_scan_4000")
		}},
		{"C5 idle p95 zero", "C5", func(fx *evalFixture) { fx.r.Backfill.LiveLoad.IdleP95Ms = 0 }},
		{"C5 no during samples", "C5", func(fx *evalFixture) { fx.r.Backfill.LiveLoad.DuringN = 0 }},
		{"C5 ratio over 1.5", "C5", func(fx *evalFixture) { fx.r.Backfill.LiveLoad.Ratio = 2 }},
		{"C5 nothing rolled", "C5", func(fx *evalFixture) { fx.r.Backfill.Txs = 0 }},
		{"C5 zero pre footprint", "C5", func(fx *evalFixture) { fx.r.C1.Pre = 0 }},
		{"C4 oversized expected but not verified", "C4", func(fx *evalFixture) { fx.r.RoundTrip.OversizedVerified = 0 }},
		{"C4 straggler never overlapped", "C4", func(fx *evalFixture) { fx.r.Concurrency.StragglerOverlapped = 0 }},
		{"C4 reroll round-trip failure", "C4", func(fx *evalFixture) { fx.r.Concurrency.RerollRoundTripFails = 1 }},
		{"C4 concurrent round trip not recorded", "C4", func(fx *evalFixture) { fx.r.Concurrency.RoundTrip = nil }},
		{"C6 no single-reader cell", "C6", func(fx *evalFixture) {
			var keep []Cell
			for _, c := range fx.r.Latency.PreVacuum {
				if c.Readers != 1 {
					keep = append(keep, c)
				}
			}
			fx.r.Latency.PreVacuum = keep
		}},
		{"C6 no heap peak", "C6", func(fx *evalFixture) {
			for i := range fx.r.Latency.PreVacuum {
				fx.r.Latency.PreVacuum[i].PeakHeapInuse = 0
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEvalFixture()
			tc.mutate(fx)
			c := fx.eval()[tc.id]
			if c.Pass == nil || *c.Pass {
				t.Fatalf("%s must fail: %+v", tc.id, c)
			}
		})
	}
}

func TestEvaluateOversizedNotExpectedIsNotApplicable(t *testing.T) {
	fx := newEvalFixture()
	fx.largestTerminalMsg = 1 << 10 // below the 256 KiB bound
	fx.r.RoundTrip.OversizedVerified = 0
	c := fx.eval()["C4"]
	if c.Pass == nil || !*c.Pass {
		t.Fatalf("C4 must pass when no rolled-up message exceeds the chunk bound: %+v", c)
	}
	if s, _ := c.Detail["oversized"].(string); !strings.HasPrefix(s, "not applicable") {
		t.Fatalf("C4 must say the oversized check is not applicable: %v", c.Detail["oversized"])
	}
}

func TestEvaluateUnmeasuredLiveRatioFailsC5AndOverall(t *testing.T) {
	fx := newEvalFixture()
	fx.r.Backfill.LiveLoad = LiveLoadReport{NotMeasure: "no live runs in the corpus"}
	c := fx.eval()["C5"]
	if c.Pass == nil || *c.Pass || !strings.Contains(c.Note, "no live runs in the corpus") {
		t.Fatalf("a live-reader ratio the harness did not evaluate must fail C5 and carry the reason: %+v", c)
	}
	if fx.r.Overall != "fail" {
		t.Fatalf("overall must not be pass with an unevaluated C5 sub-bar: %q", fx.r.Overall)
	}
}

func TestEvaluateUnobservedMixedStateFailsC4(t *testing.T) {
	fx := newEvalFixture()
	fx.r.Concurrency.MixedOnMultiTxRun = 0
	c := fx.eval()["C4"]
	if c.Pass == nil || *c.Pass || !strings.Contains(c.Note, "mixed archived+live state") {
		t.Fatalf("C4 must fail when the mixed archived+live half was not observed: %+v", c)
	}
}
