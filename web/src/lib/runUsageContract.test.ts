import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { deriveRunUsage } from "./runUsage";
import type { RunMessage } from "./api";

// The run-usage cross-language contract (issue #195). This file is the VITEST HALF;
// the Go half is api/internal/workersvc/run_usage_contract_test.go. Neither reads the
// other: each folds the SAME recorded result frames with its OWN production code and
// compares against the SAME recorded run_usage rollup, so a failure names the side
// that drifted. fixtures/judge-fidelity/ is the in-repo precedent for the shape.
//
// WHAT THIS PINS. The run page derives its usage strip, per-phase table and finish
// lines from the message stream (lib/runUsage.ts); every other surface reads the
// run_usage table the API folds in foldRunUsage. PRD #40 Decision 3 asserted the two
// "cannot diverge" and never tested it — M4/M5 were deferred for want of credentials
// (prds/done/40-token-usage-reporting.md:84) — and they diverged by 2.5-3.3x for the
// whole of PRD #40's life, because the client read the frame's top-level `usage`
// while the server folded `modelUsage`. This is that deferred check.
//
// 🔴 THE FIXTURE IS REAL, NOT AUTHORED, AND MUST NOT BE REGENERATED. Both files were
// recorded from the dev-cluster database on 2026-08-02: the frames are two genuine
// result frames of run 84b6a933, and the rollup is what the shipped server actually
// folded from them. There is deliberately no -update flag and no toMatchSnapshot():
// a golden any run can rewrite is a snapshot, and a snapshot of a regression is
// green. If this test fails, one of the two readers changed — fix the reader.
//
// 🔴 THE GO HALF NEEDS -count=1. The fixture is at the repo root, ABOVE api/, so
// every byte of it is outside that module and contributes NOTHING to the package's
// cache key: a fixture-only edit leaves `go test` printing "ok (cached)". The vitest
// half has no such cache and needs no flag. The two halves are NOT symmetric.
function read(name: string): string {
  const url = new URL(`../../../fixtures/run-usage/${name}`, import.meta.url);
  try {
    return readFileSync(url, "utf8");
  } catch (err) {
    throw new Error(
      `fixture unreadable: ${name}: ${String(err)} -- this contract asserts nothing ` +
        `without it, and skipping would look identical to passing`,
    );
  }
}

type Frame = { seq: number; kind: string; payload: Record<string, unknown> };
type UsageRow = {
  model: string;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
};

const frames: Frame[] = (JSON.parse(read("result-frames-84b6a933.json")) as { frames: Frame[] }).frames;
const rows: UsageRow[] = (JSON.parse(read("run-usage-84b6a933.json")) as { rows: UsageRow[] }).rows;

// PRD #1079: the second recorded case, mirror of the Go half's. Its frames carry all
// four `init` frames AND all four `result` frames of run 02854d5e in seq order, so the
// per-leg fold (marks reset at every init) is exercised end to end; its expected rollup
// is authored (D9: the pre-#1079 server output IS the bug, so it cannot be the golden)
// with one row per (model, lineage_epoch) and per-column totals SUMMED across those rows.
type EpochRow = UsageRow & { lineage_epoch: number };
type Totals = {
  input_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  output_tokens: number;
  cost_usd: number;
};
const frames1079: Frame[] = (JSON.parse(read("result-frames-02854d5e.json")) as { frames: Frame[] }).frames;
const parsed1079 = JSON.parse(read("run-usage-02854d5e.json")) as { rows: EpochRow[]; totals: Totals };
const rows1079: EpochRow[] = parsed1079.rows;
const totals1079: Totals = parsed1079.totals;

function fatal(msg: string): never {
  throw new Error(msg);
}

/** The recorded frames as the run page receives them off /api/runs/:id/messages. */
function messages(): RunMessage[] {
  return frames.map((f) => ({
    seq: f.seq,
    kind: f.kind,
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: f.payload,
    created_at: "2026-08-02T00:00:00Z",
  }));
}

/** The server's total: SUM over models of the run_usage row (GREATEST already applied). */
function serverTotal() {
  return rows.reduce(
    (t, r) => ({
      input: t.input + r.input_tokens,
      output: t.output + r.output_tokens,
      cacheRead: t.cacheRead + r.cache_read_tokens,
      cacheCreation: t.cacheCreation + r.cache_creation_tokens,
      cost: t.cost + r.cost_usd,
    }),
    { input: 0, output: 0, cacheRead: 0, cacheCreation: 0, cost: 0 },
  );
}

// ── Self-check: the fixture must still be able to tell a right reader from a wrong
// one. Without these, a "tidied" fixture (one frame, or stable models across frames)
// would pass against the very implementation this bug was.

describe("run-usage fixture discriminates", () => {
  it("has at least two result frames, so a per-frame reading is observable at all", () => {
    if (frames.length < 2) {
      fatal(
        `fixture broken: only ${frames.length} result frame(s) -- with one frame every ` +
          `implementation agrees, and this contract could not tell a cumulative reader ` +
          `from a summing one`,
      );
    }
  });

  it("has a model present in an earlier frame and ABSENT from the last one", () => {
    const modelsOf = (f: Frame) => Object.keys((f.payload["modelUsage"] ?? {}) as Record<string, unknown>);
    const last = new Set(modelsOf(frames[frames.length - 1]));
    const vanished = frames.slice(0, -1).flatMap(modelsOf).filter((m) => !last.has(m));
    if (vanished.length === 0) {
      fatal(
        `fixture broken: every model in an earlier frame is still in the last one -- that ` +
          `is exactly the shape a naive per-frame sum handles correctly, so this contract ` +
          `would pass against the #195 bug`,
      );
    }
    // The vanished model must also survive in the rollup, or the server would agree
    // with the naive reading too and there would be nothing to pin.
    const rolled = new Set(rows.map((r) => r.model));
    for (const m of vanished) {
      if (!rolled.has(m)) {
        fatal(`fixture broken: model ${m} vanishes from the frames AND is missing from run_usage`);
      }
    }
  });

  it("has a top-level `usage` that DISAGREES with the frame's own modelUsage", () => {
    // The original defect. If the recorded frames ever agreed on both readings, this
    // contract would be green against the code that shipped the bug.
    const disagrees = frames.some((f) => {
      const u = (f.payload["usage"] ?? {}) as Record<string, number>;
      const mu = (f.payload["modelUsage"] ?? {}) as Record<string, Record<string, number>>;
      const sum = Object.values(mu).reduce((n, e) => n + (e["inputTokens"] ?? 0), 0);
      return (u["input_tokens"] ?? 0) !== sum;
    });
    if (!disagrees) {
      fatal(
        `fixture broken: no frame's top-level usage disagrees with its modelUsage -- the ` +
          `two readings this contract exists to separate are indistinguishable on it`,
      );
    }
  });

  it("has a run_usage total that a per-frame sum cannot reach", () => {
    // The naive fix the issue itself proposed, implemented here as the fixture's own
    // negative control: sum modelUsage per frame, difference consecutive sums, clamp.
    let prev = 0;
    let naive = 0;
    for (const f of frames) {
      const mu = (f.payload["modelUsage"] ?? {}) as Record<string, Record<string, number>>;
      const cur = Object.values(mu).reduce((n, e) => n + (e["inputTokens"] ?? 0), 0);
      naive += Math.max(0, cur - prev);
      prev = cur;
    }
    if (naive === serverTotal().input) {
      fatal(
        `fixture broken: a per-frame modelUsage sum already reaches the server's input ` +
          `total (${naive}) -- the per-model retention this contract pins is unobservable on it`,
      );
    }
  });
});

// ── The contract itself.

describe("deriveRunUsage matches the server's run_usage fold", () => {
  // 🔴 THE `cached` COLUMN IS BLIND ON THIS FIXTURE, and a green there proves nothing.
  // haiku — the model that vanishes, and the only source of disagreement here — carried
  // cacheReadInputTokens 0 AND cacheCreationInputTokens 0, so the naive per-frame sum,
  // the correct per-model fold and the server rollup ALL answer 80,649,057. Measured
  // deltas of naive-vs-server: fresh −5,162, out −16, cost −0.0052418, cached 0.
  // So if a control comes back with `cached` green and the rest red, that is the
  // EXPECTED signature of a working control, not partial coverage. Never cite a passing
  // cache_read here as evidence the fold is right.
  it("agrees per model on all five columns, exactly, on real recorded frames", () => {
    // The per-MODEL comparison is the strong one, and it is the only level at which
    // input and cache_creation can be told apart at all: `total` folds them into
    // `fresh`, so a total-level assertion compares THREE token aggregates and an
    // implementation that swapped the two columns would pass it.
    const got = new Map(deriveRunUsage(messages()).modelTotals.map((t) => [t.model, t]));
    expect([...got.keys()].sort()).toEqual(rows.map((r) => r.model).sort());
    for (const want of rows) {
      const g = got.get(want.model);
      if (!g) fatal(`deriveRunUsage produced no row for model ${want.model}, which run_usage holds`);
      expect(g.input, `${want.model} input_tokens`).toBe(want.input_tokens);
      expect(g.cacheCreation, `${want.model} cache_creation_tokens`).toBe(want.cache_creation_tokens);
      expect(g.cached, `${want.model} cache_read_tokens`).toBe(want.cache_read_tokens);
      expect(g.out, `${want.model} output_tokens`).toBe(want.output_tokens);
      // EXACT, not toBeCloseTo. The client quantizes to microdollars where numericUSD
      // does, so each per-model cost is the NEAREST DOUBLE to the stored numeric(12,6)
      // decimal — and so is the fixture's own parse of that same decimal, which is what
      // licenses `toBe`. (Not "bit-equal to the decimal": a double never is one.)
      // A tolerance here would hide a drift in the quantization itself.
      expect(g.costUsd, `${want.model} cost_usd`).toBe(want.cost_usd);
    }
  });

  it("agrees on the run total: three token aggregates and cost", () => {
    const d = deriveRunUsage(messages());
    const server = serverTotal();

    expect(d.hasConfirmed).toBe(true);
    expect(d.phases).toHaveLength(frames.length);

    // THREE aggregates, not four: `fresh` is input + cache_creation on the client side
    // while the server keeps them as separate columns, so they are recombined here.
    // The four-column check is the per-model test above; this one pins the arithmetic
    // that actually reaches the strip.
    expect(d.total.fresh, "input+cache_creation disagrees with run_usage").toBe(server.input + server.cacheCreation);
    expect(d.total.cached, "cache_read disagrees with run_usage (SEE THE BLINDNESS NOTE)").toBe(server.cacheRead);
    expect(d.total.out, "output disagrees with run_usage").toBe(server.output);

    // The per-model costs above are compared EXACTLY; only their SUM needs a budget,
    // and it is float-summation noise (~1e-14 here) rather than rounding error, since
    // the quantization now happens client-side at the same point numericUSD applies
    // it. The budget is kept at the quantization bound anyway — half a microdollar per
    // row — because that is the real ceiling if the client ever stops quantizing, and
    // because it is derived rather than guessed. `toBeCloseTo(_, 6)` is deliberately
    // NOT used: its tolerance is a flat 5e-7, which is BELOW the 3-row bound of 1.5e-6,
    // so it passes on this fixture only because the three roundings happened to go a
    // favourable direction and would redden a CORRECT implementation on a run with
    // more models or less lucky rounding.
    const costBudget = rows.length * 5e-7 + 1e-9;
    expect(
      Math.abs(d.total.costUsd - server.cost),
      `cost disagrees with run_usage beyond the numeric(12,6) quantization budget: ` +
        `client ${d.total.costUsd}, server ${server.cost}`,
    ).toBeLessThanOrEqual(costBudget);
  });

  it("agrees per phase too, so the table and the run total cannot drift apart", () => {
    const d = deriveRunUsage(messages());
    const sum = d.phases.reduce(
      (t, p) => ({ fresh: t.fresh + p.fresh, cached: t.cached + p.cached, out: t.out + p.out, cost: t.cost + p.costUsd }),
      { fresh: 0, cached: 0, out: 0, cost: 0 },
    );
    expect(sum.fresh).toBe(d.total.fresh);
    expect(sum.cached).toBe(d.total.cached);
    expect(sum.out).toBe(d.total.out);
    expect(sum.cost).toBeCloseTo(d.total.costUsd, 9);
    // Every phase is reachable from its finish line by seq.
    for (const p of d.phases) expect(d.phaseUsageBySeq.get(p.seq)).toBe(p);
  });

  it("does not read the frames' top-level usage (low on every field on this run)", () => {
    // A direct, named assertion about the defect rather than an inference from the
    // totals above: the top-level reading is computed here and must NOT be what the
    // module produced.
    const topLevel = frames.reduce(
      (n, f) => {
        const u = (f.payload["usage"] ?? {}) as Record<string, number>;
        return Math.max(n, (u["input_tokens"] ?? 0) + (u["cache_creation_input_tokens"] ?? 0));
      },
      0,
    );
    const d = deriveRunUsage(messages());
    expect(d.total.fresh).not.toBe(topLevel);
    expect(d.total.fresh).toBeGreaterThan(topLevel);
  });
});

// ── PRD #1079: the per-leg case. The frames are four SDK query() legs (planning + three
// implement iterations), each preceded by its own `init` frame; the correct run total is
// the SUM of the legs, and the pre-#1079 fold (a cumulative MAX per model) under-counts
// it by ~2x. This mirrors the Go half over the same two fixture files.

/** The 8 recorded frames (init + result, in seq order) as the run page receives them. */
function messages1079(): RunMessage[] {
  return frames1079.map((f) => ({
    seq: f.seq,
    kind: f.kind,
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: f.payload,
    created_at: "2026-09-03T00:00:00Z",
  }));
}

/** The authored rollup grouped per model — the client's `modelTotals`, which collapses
 *  the per-(model, epoch) rows to one SUM per model. */
function serverModelSums1079(): Map<string, { input: number; cacheRead: number; cacheCreation: number; out: number; cost: number }> {
  const m = new Map<string, { input: number; cacheRead: number; cacheCreation: number; out: number; cost: number }>();
  for (const r of rows1079) {
    const s = m.get(r.model) ?? { input: 0, cacheRead: 0, cacheCreation: 0, out: 0, cost: 0 };
    s.input += r.input_tokens;
    s.cacheRead += r.cache_read_tokens;
    s.cacheCreation += r.cache_creation_tokens;
    s.out += r.output_tokens;
    s.cost += r.cost_usd;
    m.set(r.model, s);
  }
  return m;
}

/** The authored rollup grouped per lineage_epoch, ascending — one entry per leg, so the
 *  derived per-phase figures can be checked leg for leg. */
function serverEpochPhases1079(): { epoch: number; fresh: number; cached: number; out: number; cost: number }[] {
  const byEpoch = new Map<number, { fresh: number; cached: number; out: number; cost: number }>();
  for (const r of rows1079) {
    const p = byEpoch.get(r.lineage_epoch) ?? { fresh: 0, cached: 0, out: 0, cost: 0 };
    p.fresh += r.input_tokens + r.cache_creation_tokens;
    p.cached += r.cache_read_tokens;
    p.out += r.output_tokens;
    p.cost += r.cost_usd;
    byEpoch.set(r.lineage_epoch, p);
  }
  return [...byEpoch.entries()].sort((a, b) => a[0] - b[0]).map(([epoch, v]) => ({ epoch, ...v }));
}

describe("run-usage-02854d5e fixture discriminates a per-leg SUM from a cumulative MAX", () => {
  it("has four init frames and four result frames, in interleaved seq order", () => {
    const inits = frames1079.filter((f) => f.kind === "status" && f.payload["event"] === "init");
    const results = frames1079.filter((f) => f.kind === "status" && f.payload["event"] === "result");
    if (inits.length !== 4 || results.length !== 4) {
      fatal(`fixture broken: expected 4 init + 4 result frames, got ${inits.length} + ${results.length}`);
    }
    // Each result must be preceded by its own init, or the leg boundary vanishes.
    const seqs = frames1079.map((f) => `${f.payload["event"]}`);
    if (seqs.join(",") !== "init,result,init,result,init,result,init,result") {
      fatal(`fixture broken: frames are not init/result interleaved in seq order: ${seqs.join(",")}`);
    }
  });

  it("the pre-#1079 cumulative MAX fold under-counts the SUM (77.185539 / 514572, not 153.582776 / 1021240)", () => {
    // The negative control, computed here over the same frames: a single running MAX per
    // model with NO per-init reset — exactly the pre-#1079 fold. It must reach the known
    // collapsed answer AND fall short of the fixture totals, or this case pins nothing.
    const q = (v: number) => Math.round(Math.min(Math.max(0, v), 999999.999999) * 1e6) / 1e6;
    const marks = new Map<string, { out: number; cost: number }>();
    let maxOut = 0;
    let maxCost = 0;
    for (const f of frames1079) {
      if (f.payload["event"] !== "result") continue;
      const mu = (f.payload["modelUsage"] ?? {}) as Record<string, Record<string, number>>;
      for (const [model, e] of Object.entries(mu)) {
        const prev = marks.get(model) ?? { out: 0, cost: 0 };
        const curOut = Math.max(0, e["outputTokens"] ?? 0);
        const curCost = q(e["costUSD"] ?? 0);
        maxOut += Math.max(0, curOut - prev.out);
        maxCost += Math.max(0, curCost - prev.cost);
        marks.set(model, { out: Math.max(prev.out, curOut), cost: Math.max(prev.cost, curCost) });
      }
    }
    expect(maxOut).toBe(514572);
    expect(maxCost).toBeCloseTo(77.185539, 6);
    // And that collapsed answer is NOT the fixture's — so the run-total assertion below
    // is genuinely red on the pre-fix fold.
    expect(maxOut).not.toBe(totals1079.output_tokens);
    expect(maxCost).not.toBeCloseTo(totals1079.cost_usd, 3);
  });

  it("leg 4 is smaller than leg 3 for opus on every column (why cumulative-MAX is wrong)", () => {
    const opus = (epoch: number) => rows1079.find((r) => r.model === "claude-opus-4-8" && r.lineage_epoch === epoch);
    const l3 = opus(3);
    const l4 = opus(4);
    if (!l3 || !l4) fatal("fixture broken: opus is missing an epoch-3 or epoch-4 row");
    expect(l4.input_tokens).toBeLessThan(l3.input_tokens);
    expect(l4.cache_read_tokens).toBeLessThan(l3.cache_read_tokens);
    expect(l4.cache_creation_tokens).toBeLessThan(l3.cache_creation_tokens);
    expect(l4.output_tokens).toBeLessThan(l3.output_tokens);
    expect(l4.cost_usd).toBeLessThan(l3.cost_usd);
  });
});

describe("deriveRunUsage matches the per-leg run_usage fold (02854d5e)", () => {
  it("agrees per model on all five columns, summed across legs", () => {
    const got = new Map(deriveRunUsage(messages1079()).modelTotals.map((t) => [t.model, t]));
    const want = serverModelSums1079();
    expect([...got.keys()].sort()).toEqual([...want.keys()].sort());
    for (const [model, w] of want) {
      const g = got.get(model);
      if (!g) fatal(`deriveRunUsage produced no row for model ${model}`);
      expect(g.input, `${model} input_tokens`).toBe(w.input);
      expect(g.cacheCreation, `${model} cache_creation_tokens`).toBe(w.cacheCreation);
      expect(g.cached, `${model} cache_read_tokens`).toBe(w.cacheRead);
      expect(g.out, `${model} output_tokens`).toBe(w.out);
      // Per-model cost is a SUM of per-(model, epoch) microdollar-quantized costs on both
      // sides, so only float-summation noise separates them; the per-row values are exact.
      expect(g.costUsd, `${model} cost_usd`).toBeCloseTo(w.cost, 6);
    }
  });

  it("agrees on the run total: three token aggregates and cost (the ~2x under-count fixed)", () => {
    const d = deriveRunUsage(messages1079());
    expect(d.hasConfirmed).toBe(true);
    // Four legs → four phases (one result frame each).
    expect(d.phases).toHaveLength(4);
    // fresh = input + cache_creation; the fixture keeps them as separate total columns.
    expect(d.total.fresh, "input+cache_creation").toBe(totals1079.input_tokens + totals1079.cache_creation_tokens);
    expect(d.total.cached, "cache_read").toBe(totals1079.cache_read_tokens);
    expect(d.total.out, "output — RED (514572) on the pre-#1079 cumulative-MAX fold").toBe(totals1079.output_tokens);
    // Run cost is the SUM of per-model per-leg quantized costs; float noise only.
    expect(
      Math.abs(d.total.costUsd - totals1079.cost_usd),
      `cost — RED (77.185539) on the pre-#1079 fold: client ${d.total.costUsd}, want ${totals1079.cost_usd}`,
    ).toBeLessThanOrEqual(rows1079.length * 5e-7 + 1e-9);
  });

  it("agrees per phase too: each phase equals its leg's fixture rows", () => {
    const d = deriveRunUsage(messages1079());
    const want = serverEpochPhases1079();
    expect(d.phases).toHaveLength(want.length);
    expect(d.phases.map((p) => p.label)).toEqual(["Plan", "Implement · iteration 1", "Implement · iteration 2", "Implement · iteration 3"]);
    d.phases.forEach((p, i) => {
      const w = want[i];
      expect(p.fresh, `leg ${w.epoch} fresh`).toBe(w.fresh);
      expect(p.cached, `leg ${w.epoch} cached`).toBe(w.cached);
      expect(p.out, `leg ${w.epoch} out`).toBe(w.out);
      expect(p.costUsd, `leg ${w.epoch} cost`).toBeCloseTo(w.cost, 6);
    });
    // Phases still sum to the run total (the table and the strip cannot drift apart).
    const sum = d.phases.reduce((t, p) => t + p.out, 0);
    expect(sum).toBe(d.total.out);
  });
});
