import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  StubExecutor,
  STUB_INFLIGHT_SENTINEL,
  STUB_INTERLEAVE_SENTINEL,
  STUB_INTERLEAVE_STREAM,
  STUB_LOOP_SENTINEL,
  STUB_STALL_SENTINEL,
  type EmittedMessage,
  type RunContext,
} from "../src/executor.js";
import { nullLogger } from "./helpers.js";

// A throwaway git worktree the stub can write its marker into and commit. No
// origin, no network — run() only makes a LOCAL commit (push + MR is the runner).
function makeWorktree(): { path: string; cleanup: () => void } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-stub-wt-"));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  execFileSync("git", ["init", "-b", "main", dir], { env, stdio: "pipe" });
  return { path: dir, cleanup: () => fs.rmSync(dir, { recursive: true, force: true }) };
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emitted: EmittedMessage[] } {
  const emitted: EmittedMessage[] = [];
  const ctx: RunContext = {
    runId: "run-stub-interleave",
    issueIid: 7,
    issueTitle: "E2E interleave",
    issueDescription: "implements prds/43-intra-run-parallel-subagents.md",
    worktreePath: "",
    branch: "agent/issue-7",
    emit: (m) => emitted.push(m),
    ...overrides,
  };
  return { ctx, emitted };
}

// The scripted frames only (worker infra status/text messages filtered out), in
// emit order, projected to the fields the E2E later asserts on after persistence.
function scripted(emitted: EmittedMessage[]): { agent: string | undefined; step: unknown }[] {
  return emitted
    .filter((m) => typeof (m.payload as Record<string, unknown>).step === "number")
    .map((m) => ({ agent: m.agent, step: (m.payload as Record<string, unknown>).step }));
}

describe("StubExecutor — PRD #43 M5 interleaved multi-agent stream", () => {
  it("emits the scripted interleaved stream in order with per-agent attribution", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      issueDescription: `implements prds/43-intra-run-parallel-subagents.md ${STUB_INTERLEAVE_SENTINEL}`,
    });
    try {
      await new StubExecutor(nullLogger()).run(ctx);
    } finally {
      wt.cleanup();
    }

    // Exactly the scripted frames, in emit order, each with the right agent + 1-based step.
    assert.deepStrictEqual(
      scripted(emitted),
      STUB_INTERLEAVE_STREAM.map((f, i) => ({ agent: f.agent, step: i + 1 })),
      "the emitted stream must match STUB_INTERLEAVE_STREAM exactly (order + attribution)",
    );

    // The interleave is real: at least one agent name recurs NON-ADJACENTLY (the
    // property that makes name-based attribution non-trivial to preserve).
    const agents = STUB_INTERLEAVE_STREAM.map((f) => f.agent);
    const hasNonAdjacentRepeat = agents.some(
      (a, i) => agents.indexOf(a) < i - 1 && agents[i - 1] !== a,
    );
    assert.ok(hasNonAdjacentRepeat, "the script must repeat at least one agent name non-adjacently");
  });

  it("emits NO scripted frames when the sentinel is absent (no leak into normal runs)", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path });
    try {
      await new StubExecutor(nullLogger()).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.deepStrictEqual(scripted(emitted), [], "a run without the sentinel must emit no scripted frames");
  });
});

describe("StubExecutor — PRD #47 M6 run-health sentinels", () => {
  // healthPauseMs: 0 makes the stall/loop/in-flight pauses instant so the unit test
  // runs fast; the real E2E leaves it at the STUB_HEALTH_* defaults.
  const runWith = async (sentinel: string) => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path, issueDescription: `x ${sentinel}` });
    try {
      await new StubExecutor(nullLogger(), { healthPauseMs: 0 }).run(ctx);
    } finally {
      wt.cleanup();
    }
    return emitted;
  };
  const tools = (emitted: EmittedMessage[], kind: "tool_use" | "tool_result") =>
    emitted.filter((m) => m.kind === kind);

  it("UZI_STUB_LOOP emits four IDENTICAL tool calls (name+input) so the window hash flags looping", async () => {
    const emitted = await runWith(STUB_LOOP_SENTINEL);
    const uses = tools(emitted, "tool_use");
    assert.equal(uses.length, 4, "four tool_use calls");
    const fingerprints = new Set(
      uses.map((m) => JSON.stringify([m.payload.name, m.payload.input])),
    );
    assert.equal(fingerprints.size, 1, "all four calls share one name+input fingerprint");
    // Each call has its matching result, so none is left in flight.
    assert.equal(tools(emitted, "tool_result").length, 4, "each call has a result");
  });

  it("UZI_STUB_INFLIGHT emits a tool_use held open past the pause, then its result", async () => {
    const emitted = await runWith(STUB_INFLIGHT_SENTINEL);
    const [use] = tools(emitted, "tool_use");
    const [res] = tools(emitted, "tool_result");
    assert.ok(use && res, "exactly one tool_use and one tool_result");
    assert.equal(tools(emitted, "tool_use").length, 1, "exactly one tool_use");
    assert.equal(tools(emitted, "tool_result").length, 1, "exactly one tool_result");
    // The use precedes its result and they share an id (the detector matches on it).
    assert.ok(emitted.indexOf(use) < emitted.indexOf(res), "the tool_use is emitted before its result");
    assert.equal(use.payload.id, res.payload.tool_use_id, "the result references the tool_use id");
  });

  it("UZI_STUB_STALL brackets its pause with a quiet-then-resume status pair", async () => {
    const emitted = await runWith(STUB_STALL_SENTINEL);
    const texts = emitted.filter((m) => m.kind === "status").map((m) => String(m.payload.text));
    assert.ok(texts.some((t) => t.includes("pausing")), "emits a pause marker before going quiet");
    assert.ok(texts.some((t) => t.includes("resuming")), "emits a resume marker (the activity bump that self-clears)");
  });

  it("emits no health tool calls when no sentinel is present", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path });
    try {
      await new StubExecutor(nullLogger(), { healthPauseMs: 0 }).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.equal(tools(emitted, "tool_use").length, 0, "a normal run emits no stub tool calls");
  });
});
