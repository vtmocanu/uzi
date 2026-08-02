package workersvc

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// The run-usage cross-language contract (issue #195). This file is the GO HALF; the
// vitest half is web/src/lib/runUsageContract.test.ts. Neither reads the other: each
// folds the SAME recorded result frames with its OWN production code and compares
// against the SAME recorded run_usage rollup, so a failure names the side that
// drifted. fixtures/judge-fidelity/ is the in-repo precedent for the shape.
//
// WHAT THIS PINS. Every rollup surface (run_usage, the board, `uzi run list`,
// /api/usage, /api/admin/usage) folds a result frame's `modelUsage` through
// foldRunUsage. The run page derives its own figures from the message stream
// (web/src/lib/runUsage.ts) and, until #195, read the frame's TOP-LEVEL `usage`
// instead — a different population, low on every field of the recorded run's first
// frame: 2.5x on output, 3.3x on cache_read, 229x on the small input column.
// PRD #40 Decision 3 asserted the two "cannot diverge" and never tested it (M4/M5
// were deferred for want of credentials, prds/done/40-token-usage-reporting.md:84).
// This is that deferred check, from the server's side.
//
// 🔴 THE FIXTURE IS REAL, NOT AUTHORED, AND MUST NOT BE REGENERATED. Both files were
// recorded from the dev-cluster database on 2026-08-02: the frames are two genuine
// result frames of run 84b6a933, and the rollup is what the shipped server actually
// folded from them. There is deliberately no -update flag: a golden any run can
// rewrite is a snapshot, and a snapshot of a regression is green.
//
// 🔴 RUN THIS PACKAGE WITH -count=1 AFTER ANY FIXTURE-ONLY EDIT. runUsageFixtureDir
// points ABOVE api/, so every byte of the fixture is outside this module and
// contributes NOTHING to this package's cache key — cmd/go's own rule is "Do not
// recheck files outside the module, GOPATH, or GOROOT root". A gutted fixture
// therefore prints "ok (cached)". The vitest half has no such cache and needs no
// flag, so THE TWO HALVES ARE NOT SYMMETRIC: "Go green + vitest red means Go
// drifted" has a third explanation, which is that Go never ran.
const runUsageFixtureDir = "../../../fixtures/run-usage"

type recordedFrame struct {
	Seq     int32           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type recordedFrames struct {
	Note   string          `json:"note"`
	RunID  string          `json:"run_id"`
	Frames []recordedFrame `json:"frames"`
}

// recordedUsageRow mirrors the run_usage columns as the fixture recorded them. The
// numeric(12,6) cost lands in a float64 here rather than a pgtype.Numeric: the
// fixture is a JSON transcript of the table, not a query result.
type recordedUsageRow struct {
	Model               string  `json:"model"`
	CostUSD             float64 `json:"cost_usd"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
}

type recordedRollup struct {
	Note  string             `json:"note"`
	RunID string             `json:"run_id"`
	Rows  []recordedUsageRow `json:"rows"`
}

// readFixture is fatal on any failure, never a skip. A skipped contract check is
// indistinguishable from a passing one — the same false-green shape CLAUDE.md records
// for the live-DB suites, where a suite that ran nothing prints "ok".
func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runUsageFixtureDir, name))
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- this contract asserts nothing without it, "+
			"and skipping would look identical to passing", name, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("fixture malformed: %s: %v", name, err)
	}
}

func loadRecordedFrames(t *testing.T) recordedFrames {
	t.Helper()
	var f recordedFrames
	readFixture(t, "result-frames-84b6a933.json", &f)
	return f
}

func loadRecordedRollup(t *testing.T) recordedRollup {
	t.Helper()
	var r recordedRollup
	readFixture(t, "run-usage-84b6a933.json", &r)
	return r
}

// foldRecordedFrames drives the REAL AppendMessages path over the recorded frames and
// merges the resulting upserts the way UpsertRunUsage's ON CONFLICT does — GREATEST
// per (run_id, session_id, model). The merge is reimplemented here rather than
// mocked away because it is half of what the contract is about: the client's
// per-model retention must land on the same population this merge keeps.
func foldRecordedFrames(t *testing.T, frames []recordedFrame) map[string]store.UpsertRunUsageParams {
	t.Helper()
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), SessionID: pgText("sess-84b6a933")}}
	svc := New(fs, newBox(t), testParams())

	msgs := make([]IncomingMessage, 0, len(frames))
	for _, f := range frames {
		msgs = append(msgs, IncomingMessage{Seq: f.Seq, Kind: f.Kind, Agent: "lead", Payload: f.Payload})
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages over the recorded frames: %v", err)
	}

	merged := map[string]store.UpsertRunUsageParams{}
	for _, u := range fs.upsertedUsage {
		prev, ok := merged[u.Model]
		if !ok {
			merged[u.Model] = u
			continue
		}
		prev.InputTokens = max64(prev.InputTokens, u.InputTokens)
		prev.OutputTokens = max64(prev.OutputTokens, u.OutputTokens)
		prev.CacheReadTokens = max64(prev.CacheReadTokens, u.CacheReadTokens)
		prev.CacheCreationTokens = max64(prev.CacheCreationTokens, u.CacheCreationTokens)
		if costFloat(t, u.CostUsd) > costFloat(t, prev.CostUsd) {
			prev.CostUsd = u.CostUsd
		}
		merged[u.Model] = prev
	}
	return merged
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// costFloat reads a numericUSD-built pgtype.Numeric back as dollars. numericUSD
// always emits Exp == -6 (microdollars), so this is exact for anything it produced.
func costFloat(t *testing.T, n pgtype.Numeric) float64 {
	t.Helper()
	if n.Int == nil {
		return 0
	}
	if n.Exp != -6 {
		t.Fatalf("cost numeric has Exp %d, want -6 -- numericUSD's microdollar quantization changed "+
			"and this reader is now wrong", n.Exp)
	}
	return float64(n.Int.Int64()) / 1e6
}

// TestRunUsageFoldMatchesRecordedRollup is the contract: the shipped fold, replayed
// over the recorded frames, must reproduce the recorded run_usage rows exactly.
func TestRunUsageFoldMatchesRecordedRollup(t *testing.T) {
	frames := loadRecordedFrames(t)
	rollup := loadRecordedRollup(t)
	if frames.RunID != rollup.RunID {
		t.Fatalf("fixture broken: the frames are from run %s and the rollup from %s -- "+
			"they must describe the SAME run or this contract compares unrelated data",
			frames.RunID, rollup.RunID)
	}

	got := foldRecordedFrames(t, frames.Frames)
	if len(got) != len(rollup.Rows) {
		t.Fatalf("fold produced %d models, run_usage recorded %d -- a model was dropped or invented",
			len(got), len(rollup.Rows))
	}
	for _, want := range rollup.Rows {
		g, ok := got[want.Model]
		if !ok {
			t.Fatalf("fold produced no row for model %q, which run_usage holds", want.Model)
		}
		if g.InputTokens != want.InputTokens || g.OutputTokens != want.OutputTokens ||
			g.CacheReadTokens != want.CacheReadTokens || g.CacheCreationTokens != want.CacheCreationTokens {
			t.Errorf("model %s tokens disagree with run_usage:\n got in=%d out=%d cr=%d cw=%d\nwant in=%d out=%d cr=%d cw=%d",
				want.Model, g.InputTokens, g.OutputTokens, g.CacheReadTokens, g.CacheCreationTokens,
				want.InputTokens, want.OutputTokens, want.CacheReadTokens, want.CacheCreationTokens)
		}
		// cost_usd is numeric(12,6) on both sides, so the comparison is exact to one
		// microdollar — half a unit in the last place, not a hand-picked epsilon.
		if d := math.Abs(costFloat(t, g.CostUsd) - want.CostUSD); d > 5e-7 {
			t.Errorf("model %s cost disagrees with run_usage: got %v, want %v (delta %v > half a microdollar)",
				want.Model, costFloat(t, g.CostUsd), want.CostUSD, d)
		}
	}
}

// TestRunUsageFixtureDiscriminates is the self-check. Without it a "tidied" fixture --
// one frame, or models stable across frames -- would pass against the very reading
// #195 was, and this file would certify nothing while staying green.
func TestRunUsageFixtureDiscriminates(t *testing.T) {
	frames := loadRecordedFrames(t)
	rollup := loadRecordedRollup(t)

	if len(frames.Frames) < 2 {
		t.Fatalf("fixture broken: only %d result frame(s) -- with one frame every implementation "+
			"agrees, and this contract could not tell a cumulative reader from a summing one",
			len(frames.Frames))
	}

	type framePayload struct {
		Usage      map[string]json.RawMessage  `json:"usage"`
		ModelUsage map[string]resultModelUsage `json:"modelUsage"`
	}
	parsed := make([]framePayload, 0, len(frames.Frames))
	for _, f := range frames.Frames {
		var p framePayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			t.Fatalf("fixture broken: frame seq %d does not parse: %v", f.Seq, err)
		}
		if len(p.ModelUsage) == 0 {
			t.Fatalf("fixture broken: frame seq %d carries no modelUsage -- foldRunUsage skips it, "+
				"so it contributes nothing to fold", f.Seq)
		}
		parsed = append(parsed, p)
	}

	// A model in an earlier frame must be ABSENT from the last one. That is the whole
	// shape: it is what a per-frame sum loses and GREATEST-per-model keeps.
	last := parsed[len(parsed)-1].ModelUsage
	vanished := []string{}
	for _, p := range parsed[:len(parsed)-1] {
		for m := range p.ModelUsage {
			if _, still := last[m]; !still {
				vanished = append(vanished, m)
			}
		}
	}
	if len(vanished) == 0 {
		t.Fatal("fixture broken: every model in an earlier frame is still in the last one -- that is " +
			"exactly the shape a naive per-frame sum handles correctly, so this contract would pass " +
			"against the #195 bug")
	}
	rolled := map[string]bool{}
	for _, r := range rollup.Rows {
		rolled[r.Model] = true
	}
	for _, m := range vanished {
		if !rolled[m] {
			t.Fatalf("fixture broken: model %s vanishes from the frames AND is missing from run_usage -- "+
				"the retention this contract pins is not visible in the rollup either", m)
		}
	}

	// The frames' top-level `usage` must DISAGREE with their own modelUsage. If the
	// two readings ever coincided on this fixture, the contract would be green
	// against the code that shipped the bug.
	disagrees := false
	for i, p := range parsed {
		var top struct {
			InputTokens int64 `json:"input_tokens"`
		}
		raw, err := json.Marshal(p.Usage)
		if err != nil {
			t.Fatalf("fixture broken: frame %d usage does not re-marshal: %v", i, err)
		}
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatalf("fixture broken: frame %d usage has no readable input_tokens: %v", i, err)
		}
		var sum int64
		for _, mu := range p.ModelUsage {
			sum += mu.InputTokens
		}
		if top.InputTokens != sum {
			disagrees = true
		}
	}
	if !disagrees {
		t.Fatal("fixture broken: no frame's top-level usage disagrees with its modelUsage -- the two " +
			"readings this contract exists to separate are indistinguishable on it")
	}
}
