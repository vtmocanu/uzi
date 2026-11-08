import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { it } from "node:test";
import type { Readable } from "node:stream";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { recordingLogger } from "./helpers.js";
import { client, deferred, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// issue #4066 - a Claude shutdown whose budget race elapses abandons the durability sequence still running (it may
// still hold the authority transaction and owe a settlement). runner.ts registers it with the authority runtime as a
// "claude_shutdown_durability" producer so the shutdown barrier joins it before sealing. Own file: W is exclusive per
// data dir and has no release, and the abandoned sequence must not leak into other tests' authority.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string): void {
  fs.writeFileSync(path.join(treePath, file), "work before the shutdown\n");
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], { env: GIT_ENV, stdio: "pipe" });
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", () => resolve());
    stream.on("close", () => resolve());
  });
}

it("a Claude shutdown whose budget race times out registers the abandoned durability sequence as a producer", async (t) => {
  const BUDGET = 54_323;
  const iid = 406601;
  const { gitlab } = fakeGitlab();
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-4066-claude-"));
  const lifetime = await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: "fixture-claude-producer", timeoutMs: 5000 });
  const tracked = t.mock.method(authority, "trackProducer");
  execFileSync("git", ["-C", await git.ensureClone(fx.originPath), "config", `uzi-attemptmode.issue-${iid}.enabled`, "true"], { env: GIT_ENV });

  const entered = deferred();
  const release = deferred();
  const original = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    void drain(pack);
    entered.resolve();
    await release.promise;
    return { ok: false, httpStatus: 599 };
  };
  // The budget timer (the only setTimer arm of this delay) fires the moment the hung publish is entered.
  const setTimer = (cb: () => void, ms: number): (() => void) => {
    if (ms === BUDGET) {
      void entered.promise.then(cb);
      return () => {};
    }
    const timer = setTimeout(cb, ms);
    timer.unref?.();
    return () => clearTimeout(timer);
  };
  const started = deferred();
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitInTree(ctx.worktreePath, "PRODUCER.txt");
        started.resolve();
        await new Promise<void>((resolve) => {
          if (ctx.signal!.aborted) resolve();
          else ctx.signal!.addEventListener("abort", () => resolve(), { once: true });
        });
        throw new Error("aborted mid-run");
      },
    },
  });
  const { logger, lines } = recordingLogger();
  try {
    const runner = runnerWith(factory, gitlab, undefined, logger, { shutdownPublishTimeoutMs: BUDGET, setTimer, authority });
    const running = runner.execute(gitlabClaim(iid));
    await started.promise;
    runner.shutdown();
    // The abandoned sequence may hold the authority transaction until the publish is released, so the run's own
    // teardown can wait on it: wait for the registration, then release.
    const until = Date.now() + 30_000;
    while (tracked.mock.callCount() === 0 && Date.now() < until) await new Promise((r) => setTimeout(r, 10));
    assert.deepEqual(tracked.mock.calls.map((c) => c.arguments[1]), ["claude_shutdown_durability"]);
    release.resolve();
    await running;
    const outcome = (lines as Array<Record<string, unknown>>).find((l) => l.msg === "shutdown checkpoint outcome")?.outcome;
    assert.equal(outcome, "timeout");
  } finally {
    release.resolve();
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = original;
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
});
