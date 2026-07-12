import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  StubExecutor,
  STUB_INTERLEAVE_SENTINEL,
  STUB_INTERLEAVE_STREAM,
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
