import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { GitCache, gitEnv, type FetchAgentBranchOptions } from "../src/git.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import { assertCaptureLaunch } from "../src/credential-free-capture.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../src/codex/launcher.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";

export function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
}

// Only producer-validated requests from this GitCache enter the production adapter.
// This replaces OS handles, NOT safety/registry/adapter policy. "drained" is modeled.
export async function credentialFreeGitFixture() {
  const fixture = makeFixture({ "capture.txt": "trusted start\n" });
  const git = new GitCache(fixture.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fixture.originPath);
  const branch = "agent/issue-2852";
  const run = "28522852-aaaa-bbbb-cccc-444444444444";
  const clone = (await git.runnerCloneForBranch(bare, branch, "issue-2852", noProofReseed, run)).path;
  const env = gitEnv();
  // Explicit replacement env: no inherited GIT_* can choose the fixture's index/repo.
  const gitIn = (cwd: string, args: string[]) =>
    execFileSync("git", ["-C", cwd, ...args], { env, encoding: "utf8" }).trim();
  const base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const opts: FetchAgentBranchOptions = { context: { runId: run, generation: 1, branch,
    kind: "issue", barePath: bare, defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
  const requests: BoundaryProcessRequest[] = [];
  const specs: CodexEffectLaunchSpec[] = [];
  const primary = latch();
  const disposing = latch();
  let hold: Promise<void> | undefined;
  let disposed = 0;
  let current!: BoundaryProcessRequest;
  const adapter = boundaryProcessSpawnerForTest("required", async spec => {
    assertCaptureLaunch(spec);
    const request = current;
    requests.push(request);
    specs.push(spec);
    assert.ok(request.cwd === (request.identity === "worker_pat" ? "/" : clone), "fixed producer cwd");
    const argv = spec.identity === "command"
      ? spec.args.slice(spec.args.indexOf("--") + 1) : [spec.command, ...spec.args];
    assert.deepEqual(argv, request.argv);
    assert.ok(request.argv[0] === "/usr/bin/git" || request.argv[0] === process.execPath, JSON.stringify(request.argv));
    if (request.argv[0] === process.execPath) {
      assert.equal(request.identity, "command");
      assert.equal(request.argv[1], "-e");
      assert.equal(request.argv[3], clone);
    } else {
      // No caller/candidate payloads enter this fixture: only GitCache's fixed commands.
      assert.ok(request.argv.includes("-C"));
      assert.ok(request.argv.includes(bare) || request.argv.includes(clone));
      for (const value of request.argv.filter(arg => arg.startsWith("file://"))) {
        assert.equal(value, "file://" + clone);
      }
    }
    // Use the original canonical env: the modeled sandbox's private HOME/TMPDIR do
    // not exist here. No uid/sandbox or kernel ECHILD claim is made by this seam.
    const child = spawn(request.argv[0]!, request.argv.slice(1), {
      cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"],
    });
    const exit = new Promise<{ event: "child_exit"; code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => {
        if (request.argv.includes("fetch")) primary.resolve();
        resolve({ event: "child_exit", code: code ?? 1 });
      });
    });
    return {
      started: { event: "started", supervisorPid: 300, childPid: 301, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 300,
      transport: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
      waitChild: async () => exit,
      dispose: async () => {
        await exit;
        if (request.argv.includes("fetch")) {
          disposing.resolve();
          await hold;
        }
        disposed++;
        return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
      },
      failed: undefined, whenFailed: new Promise<Error>(() => undefined),
    } satisfies CodexRootHandle;
  });
  const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unexpected generic spawn"); },
    undefined, undefined, async (request, deadline, cleanup) => {
      current = request;
      return adapter(request, deadline, cleanup);
    });
  return { fixture, git, bare, clone, branch, run, base, opts, registry, safety, requests, specs,
    gitIn, primary, disposing, disposed: () => disposed,
    holdDisposal: (promise: Promise<void>) => { hold = promise; },
    capture: <T>(action: () => Promise<T>) => safety.withCredentialFreeCapture(30_000,
      context => git.withCredentialFreeCapture(context, action)),
    cleanup: () => fixture.cleanup() };
}
