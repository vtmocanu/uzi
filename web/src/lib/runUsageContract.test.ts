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
