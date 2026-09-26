import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache, type RunnerClone } from "../src/git.js";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { CodexExecutionSafety } from "../src/harness.js";
import { CodexExecutor, type CodexExecutorOptions } from "../src/codex/codex-executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, gitlabClaim, installHarness } from "./runner-harness.js";

installHarness();

// issue #1769 m1 — the runner asks for a SELF-CONTAINED runner clone iff the executor runs
// model commands in the Codex command sandbox (`executor.sandboxesCommands === true`, known at
// construction). NOT `executor.safety`: a real CodexExecutor populates that only inside run(),
// so it is still unset at seed time. The git double records the option and stops the run at
// the seed, so nothing past phaseClone (and no Codex boundary, no executor.run) is exercised.

const STOP = "stop after recording the clone options (issue #1769 test)";

class RecordingGit extends GitCache {
  readonly seen: Array<{ selfContained?: boolean } | undefined> = [];
  constructor(dataDir: string) {
    super(dataDir, nullLogger());
  }
  override async runnerCloneForBranch(
    _barePath: string,
    _branch: string,
    _key: string,
    _runId?: string,
    _resume?: boolean,
    _expectedCheckpointTip?: string,
    opts?: { selfContained?: boolean },
  ): Promise<RunnerClone> {
    this.seen.push(opts);
    throw new Error(STOP);
  }
}

const neverRun = async (_ctx: RunContext): Promise<ExecutorResult> => {
  throw new Error("the executor must not run: the seed stopped the run");
};

/** A REAL CodexExecutor, never run: the constructor only stores its options, so inert ones do. */
function realCodexExecutor(): CodexExecutor {
  return new CodexExecutor(nullLogger(), "/nonexistent/agent-home", { binding: {}, client: {}, provider: {} } as unknown as CodexExecutorOptions);
}

function fakeSafety(): CodexExecutionSafety {
  return {
    kind: "codex",
    withBoundary: async () => { throw new Error("no boundary in this test"); },
    spawnBoundaryProcess: async () => { throw new Error("no boundary process in this test"); },
    dispose: async () => ({}) as Awaited<ReturnType<CodexExecutionSafety["dispose"]>>,
  };
}

async function seedOptsFor(executor: Executor, issue: number): Promise<Array<{ selfContained?: boolean } | undefined>> {
  const recording = new RecordingGit(fx.dataDir);
  const { gitlab } = fakeGitlab();
  const factory: ExecutorFactory = () => ({ executor });
  const runner = new RunRunner(client, recording, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
  });
  const claim = gitlabClaim(issue);
  await runner.execute(claim);
  assert.ok(
    api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"),
    "the stopped seed fails the run",
  );
  return recording.seen;
}

describe("RunRunner — self-contained runner clone for Codex (issue #1769 m1)", () => {
  it("a Claude/stub executor (no safety) seeds with selfContained false", async () => {
    const seen = await seedOptsFor({ run: neverRun }, 1769);
    assert.deepStrictEqual(seen, [{ selfContained: false }]);
  });

  it("a real CodexExecutor (safety still unset before run) seeds with selfContained true", async () => {
    const executor = realCodexExecutor();
    assert.strictEqual(executor.safety, undefined, "safety is populated only inside run()");
    assert.strictEqual(executor.sandboxesCommands, true);
    const seen = await seedOptsFor(executor, 1770);
    assert.deepStrictEqual(seen, [{ selfContained: true }]);
  });

  it("the signal is sandboxesCommands, not safety: a safety-only executor seeds with selfContained false", async () => {
    const seen = await seedOptsFor({ run: neverRun, safety: fakeSafety() }, 1771);
    assert.deepStrictEqual(seen, [{ selfContained: false }]);
  });
});
