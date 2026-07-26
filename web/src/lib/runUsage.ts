// Client-derived run usage (PRD #40 Decision 5 + 11). The run view builds its
// usage strip, per-phase table, per-agent table, and finish lines from the message
// stream — NOT from run_usage (that table feeds the cheap list/dashboard rollups).
// Full replay is unbounded, so this reduction is complete even for a failed or
// cancelled run whose frames landed before it died.
//
// TWO data paths, deliberately different (Decision 3 verdict b + Decision 11):
//   • Result frames (kind status/error, payload.event === "result") carry
//     CUMULATIVE-across-resume usage, so per-phase figures are DELTAS between
//     consecutive frames, and the run total is the last frame's cumulative (here:
//     the sum of the clamped deltas, which telescopes to that when monotonic — and
//     degrades sanely if a requeue reset makes a delta negative: clamp to 0).
//     duration_ms / num_turns are PER-INVOCATION (they read different CLI state),
//     so they are taken raw per phase and summed for the total.
//   • Assistant frames carry the API call's PER-CALL message.usage, attached by the
//     worker to exactly one emitted message per SDK frame. Those SUM directly, per
//     agent. This is a PURE reduction over the seq-deduped message list, recomputed
//     from state — never an incremental accumulator, which would double-count on the
//     ws→reconnect→REST-replay overlap.
//
// All figures pass through null-coalescing since BetaUsage's cache/creation fields
// are nullable (unlike ModelUsage). This module is pure + unit-tested; the React
// components are thin.

import type { RunMessage } from "./api";

export interface PhaseUsage {
  /** seq of the result-frame message, so the finish line can look up its delta. */
  seq: number;
  /** "Plan" for the first phase, "Implement · iteration N" after. */
  label: string;
  /** Per-invocation, taken raw from the frame (not cumulative). */
  turns: number;
  durationMs: number;
  /** Per-phase DELTAS (this frame's cumulative minus the previous frame's). */
  fresh: number; // input_tokens + cache_creation
  cached: number; // cache_read
  out: number; // output
  costUsd: number;
  /** An error result frame (a failed/cancelled phase still spent tokens). */
  isError: boolean;
}

export interface AgentUsage {
  agent: string;
  fresh: number;
  cached: number;
  out: number;
  /** PRD #93 Decision 4: how many counted usage frames carried each model. A COUNT
   *  map, not a Set, because the primary is the most frequent one and the tie-break
   *  needs the frequencies. Empty for a pre-feature run (no frame carried a model). */
  modelCounts: Record<string, number>;
  /** The derived primary: highest count, ties broken lexicographically ascending.
   *  null when no frame carried a model — never fabricated from the strip's init
   *  model, which is the run's main-thread model, not this agent's (Decision 6). */
  model: string | null;
  /** Distinct models beyond `model` (two distinct models → 1, rendered "+1"). */
  otherModels: number;
}

export interface RunUsage {
  /** True once any result frame carried usage — gates the whole UI (a pre-feature
   *  run has none → the strip/tables never render, never a fabricated 0). */
  hasUsage: boolean;
  phases: PhaseUsage[];
  total: {
    fresh: number;
    cached: number;
    out: number;
    costUsd: number;
    turns: number;
    durationMs: number;
    phaseCount: number;
  };
  /** cached / (fresh + cached) in [0,1], for the "X% from cache" bar. */
  cacheHitRatio: number;
  /** The model from the init frame, if one was seen. */
  model: string | null;
  agents: AgentUsage[];
  agentTotal: { fresh: number; cached: number; out: number };
  /** Distinct models across ALL agents, sorted ascending — the per-agent table's
   *  "Attributed total" cell (one model → that string, >1 → "N models", none → —). */
  agentModels: string[];
  /** Per-result-frame delta, keyed by seq (the finish line reads its own phase). */
  phaseUsageBySeq: Map<number, PhaseUsage>;
}

function rec(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}
function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}
function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** A BetaUsage / NonNullableUsage shape (nullable cache fields → 0). */
function readUsage(u: unknown): { fresh: number; cached: number; out: number } | undefined {
  const r = rec(u);
  if (!r) return undefined;
  return {
    fresh: num(r["input_tokens"]) + num(r["cache_creation_input_tokens"]),
    cached: num(r["cache_read_input_tokens"]),
    out: num(r["output_tokens"]),
  };
}

function isResultFrame(m: RunMessage): boolean {
  if (m.kind !== "status" && m.kind !== "error") return false;
  return rec(m.payload)?.["event"] === "result";
}

const clampDelta = (cur: number, prev: number): number => Math.max(0, cur - prev);

/** An agent row mid-reduction: the model display is derived once, after the fold. */
type AgentAcc = Omit<AgentUsage, "model" | "otherModels">;

/**
 * PRD #93 Decision 4: the primary model is the one seen on the most counted frames;
 * equal counts break lexicographically ascending, so the result is deterministic
 * regardless of frame order. `otherModels` is what the "+K" suffix renders.
 */
function primaryModel(counts: Record<string, number>): { model: string | null; otherModels: number } {
  const entries = Object.entries(counts);
  if (entries.length === 0) return { model: null, otherModels: 0 };
  // Plain codepoint compare (not localeCompare): the tie-break must not vary by locale.
  entries.sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return { model: entries[0][0], otherModels: entries.length - 1 };
}

/**
 * Reduce a run's message list into its usage surfaces. Pure: same messages →
 * same result, so React just re-runs it as the stream grows (Decision 9 live
 * fold-in needs no accumulator).
 */
export function deriveRunUsage(messages: RunMessage[]): RunUsage {
  const phases: PhaseUsage[] = [];
  const phaseUsageBySeq = new Map<number, PhaseUsage>();
  const agentMap = new Map<string, AgentAcc>();
  let model: string | null = null;

  // Previous frame's CUMULATIVE totals, to difference into per-phase deltas.
  let prevFresh = 0;
  let prevCached = 0;
  let prevOut = 0;
  let prevCost = 0;
  let implIteration = 0;

  for (const m of messages) {
    const payload = rec(m.payload);

    // Model heartbeat (system init frame).
    if (m.kind === "status" && payload?.["event"] === "init" && model === null) {
      model = str(payload["model"]) ?? null;
    }

    if (isResultFrame(m)) {
      const u = readUsage(payload?.["usage"]);
      if (!u) continue; // pre-feature result frame carried no usage → skip
      const isError = m.kind === "error";
      const label = phases.length === 0 ? "Plan" : `Implement · iteration ${++implIteration}`;
      const phase: PhaseUsage = {
        seq: m.seq,
        label,
        turns: num(payload?.["num_turns"]),
        durationMs: num(payload?.["duration_ms"]),
        fresh: clampDelta(u.fresh, prevFresh),
        cached: clampDelta(u.cached, prevCached),
        out: clampDelta(u.out, prevOut),
        costUsd: clampDelta(num(payload?.["total_cost_usd"]), prevCost),
        isError,
      };
      phases.push(phase);
      phaseUsageBySeq.set(m.seq, phase);
      prevFresh = u.fresh;
      prevCached = u.cached;
      prevOut = u.out;
      prevCost = num(payload?.["total_cost_usd"]);
      continue;
    }

    // Per-agent: assistant-frame per-call usage (never a result frame — that would
    // fold the cumulative total into the sum). One usage record per SDK frame.
    if (payload && "usage" in payload) {
      const u = readUsage(payload["usage"]);
      if (u) {
        const agent = m.agent ?? "lead";
        const acc = agentMap.get(agent) ?? { agent, fresh: 0, cached: 0, out: 0, modelCounts: {} };
        acc.fresh += u.fresh;
        acc.cached += u.cached;
        acc.out += u.out;
        // PRD #93 Decision 2: `model` is CO-GATED with `usage` by the worker — it
        // rides the same surviving frame — so it is read only here, inside the
        // counted branch. A model-only frame therefore never creates an agent row,
        // and the strip's init model (a different path) never leaks in.
        const fm = str(payload["model"]);
        if (fm) acc.modelCounts[fm] = (acc.modelCounts[fm] ?? 0) + 1;
        agentMap.set(agent, acc);
      }
    }
  }

  const total = phases.reduce(
    (t, p) => ({
      fresh: t.fresh + p.fresh,
      cached: t.cached + p.cached,
      out: t.out + p.out,
      costUsd: t.costUsd + p.costUsd,
      turns: t.turns + p.turns,
      durationMs: t.durationMs + p.durationMs,
      phaseCount: t.phaseCount + 1,
    }),
    { fresh: 0, cached: 0, out: 0, costUsd: 0, turns: 0, durationMs: 0, phaseCount: 0 },
  );

  const inTotal = total.fresh + total.cached;
  const agents: AgentUsage[] = [...agentMap.values()].map((a) => ({ ...a, ...primaryModel(a.modelCounts) }));
  const agentTotal = agents.reduce(
    (t, a) => ({ fresh: t.fresh + a.fresh, cached: t.cached + a.cached, out: t.out + a.out }),
    { fresh: 0, cached: 0, out: 0 },
  );
  // Run-wide distinct models: every model any agent was seen on, not just primaries.
  const agentModels = [...new Set(agents.flatMap((a) => Object.keys(a.modelCounts)))].sort();

  return {
    hasUsage: phases.length > 0,
    phases,
    total,
    cacheHitRatio: inTotal > 0 ? total.cached / inTotal : 0,
    model,
    agents,
    agentTotal,
    agentModels,
    phaseUsageBySeq,
  };
}
