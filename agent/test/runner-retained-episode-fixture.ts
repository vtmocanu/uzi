// Shared helpers for the runner-retained-episode suites.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { afterEach } from "node:test";
import type { ExecutorFactory } from "../src/runner.js";
import { noProofReseed } from "./helpers.js";
import { api, fx, git, gitlabClaim, homeDir } from "./runner-harness.js";

let callbackFailures: unknown[] = [];

export function installEpisodeFixture(): void {
  afterEach(() => {
    const failures = callbackFailures;
    callbackFailures = [];
    assert.deepEqual(failures, [], "runner must not swallow model callback assertions");
  });
}

export function waitForEpisodeAbort(signal: AbortSignal, observed: { reason?: Error }): Promise<void> {
  return new Promise<void>((_resolve, reject) => {
    const aborted = () => {
      observed.reason = signal.reason;
      reject(signal.reason);
    };
    if (signal.aborted) aborted();
    else signal.addEventListener("abort", aborted, { once: true });
  });
}

export function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
export async function seed(iid = 2512, large = false) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session" });
  if (large) {
    command(fx.originPath, "config", "user.name", "fixture");
    command(fx.originPath, "config", "user.email", "fixture@example.com");
    fs.writeFileSync(path.join(fx.originPath, "large-public"), randomBytes(64 * 1024));
    command(fx.originPath, "add", "large-public");
    command(fx.originPath, "commit", "-m", "public history");
  }
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone, key: `issue-${iid}` };
}

export function readJournal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}

export function factory(run: (ctx: import("../src/executor.js").RunContext) => Promise<never>): ExecutorFactory {
  return runId => ({ homeDir: path.join(homeDir, runId), executor: { run: async ctx => {
    try { return await run(ctx); }
    catch (error) { if (error instanceof assert.AssertionError) callbackFailures.push(error); throw error; }
  } } });
}
