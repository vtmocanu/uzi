import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { setImmediate } from "node:timers/promises";
import { GitCache, gitEnv } from "../src/git.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, MAX_POISON_ERRORS, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import { CodexLaunchError, type CodexEffectLaunchSpec, type CodexRootHandle } from "../src/codex/launcher.js";
import type { BoundaryProcessRequest, CredentialFreeCaptureContext } from "../src/harness.js";
import { assertCaptureLaunch, captureGitSpawner, validateCaptureGitRequest } from "../src/credential-free-capture.js";
import { nullLogger } from "./helpers.js";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

// The real GitCache, safety owner, registry, and production sandbox adapter run.
// Only the OS supervisor is replaced: primary exit and ECHILD drain are distinct.
function rig(options: { drain?: Promise<void>; primary?: Promise<void> } = {}) {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
  const specs: CodexEffectLaunchSpec[] = [];
  const primaryExited = deferred<void>();
  const reaping = deferred<void>();
  let disposed = 0;
  const launch = async (spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
    assertCaptureLaunch(spec);
    specs.push(spec);
    const stdout = new PassThrough();
    stdout.end(spec.args.includes("status") ? " M capture-sentinel.txt\n" : "a".repeat(40) + "\n");
    return {
      started: { event: "started", supervisorPid: 300, childPid: 301, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 300,
      transport: { stdin: new PassThrough(), stdout, stderr: new PassThrough().end() },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
      waitChild: async () => {
        await options.primary;
        primaryExited.resolve();
        return { event: "child_exit", code: 0 };
      },
      dispose: async () => {
        disposed++;
        reaping.resolve();
        await options.drain;
        return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
      },
      failed: undefined,
      whenFailed: new Promise<Error>(() => undefined),
    };
  };
  let fireDeadline = () => {};
  const safety = createCodexExecutionSafety(registry,
    async () => { throw new Error("unexpected generic spawn"); }, undefined, undefined,
    boundaryProcessSpawnerForTest("required", launch),
    (_request, _ms, fire) => { fireDeadline = fire; return () => {}; });
  const git = new GitCache(process.env.TMPDIR!, nullLogger());
  return { registry, specs, safety, git, primaryExited, reaping,
    expire: () => fireDeadline(), disposed: () => disposed,
    status: () => safety.withCredentialFreeCapture(10_000,
      context => git.withCredentialFreeCapture(context, () => git.worktreeStatus("/capture/runner"))) };
}

describe("credential-free capture foundation (#2852)", () => {
  it("joins ignored startup and rejected-admission cleanup before releasing capture custody", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
    const launch = deferred<void>();
    const disposing = deferred<void>();
    const cleanup = deferred<void>();
    let reaps = 0;
    const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unused"); },
      undefined, undefined, async () => {
        await launch.promise;
        return {
          root: { kind: "command", // Deliberately mismatches boundary_action admission.
            reap: async () => { reaps++; return { ok: false, error: { category: "tool", message: "still live" } }; },
            dispose: async () => { disposing.resolve(); await cleanup.promise; throw new Error("cleanup unconfirmed"); } },
          waitChild: async () => ({ code: 0 }),
          stdin: null, stdout: null, stderr: null,
        };
      });
    let completed = false;
    const first = safety.withCredentialFreeCapture(10_000, async context => {
      const spawn = captureGitSpawner(context, () => {});
      // Caller discards startup: safety must still own the failure and cleanup.
      void spawn({ argv: ["/usr/bin/git", "status"], cwd: "/capture/runner", identity: "command", env: gitEnv() });
    }).then(value => ({ value }), error => ({ error })).then(value => { completed = true; return value; });
    launch.resolve();
    await disposing.promise;
    let successorEntered = false;
    const successor = safety.withCredentialFreeCapture(10_000, async () => { successorEntered = true; });
    let disposalCompleted = false;
    const disposal = safety.dispose({ boundary: "checkpoint", deadlineMs: 10_000 })
      .then(value => { disposalCompleted = true; return value; });
    try {
      await setImmediate();
      assert.equal(completed, false);
      assert.equal(successorEntered, false);
      assert.equal(disposalCompleted, false);
      assert.equal(registry.rootCount(), 1, "rejected root lost from ownership inventory");
    } finally { cleanup.resolve(); }
    const [a, b, d] = await Promise.all([first, successor, disposal]);
    assert.ok("error" in a);
    assert.equal(b.kind, "incomplete");
    assert.equal(successorEntered, false);
    assert.ok(reaps > 0, "later settlement must revisit the rejected root");
    assert.equal(d.kind, "incomplete");
    assert.equal(registry.state(), "poisoned");
  });

  for (const classification of ["unknown", "started_deadline"] as const) {
    it("refuses later capture and disposal after unauthenticated " + classification + " startup cleanup", async () => {
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
      let tokenCleanup = 0;
      let authorizationSeen = false;
      const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unused"); },
        undefined, () => { tokenCleanup++; }, async (_request, _deadline, authorization) => {
          authorizationSeen = authorization !== undefined;
          // Public fields cannot authenticate cleanup or a genuinely unstarted launch.
          throw new CodexLaunchError(classification, "verified", new Error("unconfirmed"));
        });
      await assert.rejects(safety.withCredentialFreeCapture(10_000, async context => {
        const spawn = captureGitSpawner(context, () => {});
        await spawn({ argv: ["/usr/bin/git", "status"], cwd: "/capture/runner", identity: "command", env: gitEnv() });
      }));
      assert.equal(authorizationSeen, true);
      assert.equal(registry.pendingLaunchCount(), 0);
      assert.equal(registry.rootCount(), 0);
      assert.equal((await safety.settleForCredentialFreeCapture(10_000)).kind, "incomplete");
      let entered = false;
      assert.equal((await safety.withCredentialFreeCapture(10_000, async () => { entered = true; })).kind, "incomplete");
      assert.equal(entered, false);
      assert.equal((await safety.dispose({ boundary: "checkpoint", deadlineMs: 10_000 })).kind, "incomplete");
      assert.equal(tokenCleanup, 1);
      assert.equal(registry.state(), "poisoned");
    });
  }

  it("executes public GitCache status through the production spawner and canonical pins", async () => {
    const r = rig();
    const result = await r.status();
    assert.equal(result.kind, "observed_empty");
    if (result.kind !== "observed_empty") return;
    assert.deepEqual(result.value, [" M capture-sentinel.txt"]);
    assert.equal(r.specs.length, 1);
    assert.equal(r.specs[0]!.identity, "command");
    assert.equal(r.specs[0]!.command, "/usr/local/bin/uzi-codex-command-sandbox");
    const env = r.specs[0]!.env;
    const pairs = Array.from({ length: Number(env.GIT_CONFIG_COUNT) }, (_, i) =>
      [env[`GIT_CONFIG_KEY_${i}`], env[`GIT_CONFIG_VALUE_${i}`]]);
    for (const pin of [["credential.helper", ""], ["core.askpass", ""],
      ["maintenance.auto", "false"], ["gc.auto", "0"]]) {
      assert.equal(pairs.filter(pair => pair[0] === pin[0] && pair[1] === pin[1]).length, 1, pin[0]);
    }
    assert.equal(env.GIT_OPTIONAL_LOCKS, "0");
    assert.equal(env.GIT_TERMINAL_PROMPT, "0");
    assert.equal(r.registry.rootCount(), 1);
    assert.equal(r.safety.acceptsCompletedCapture(result.proof), true);
    assert.equal(r.registry.state(), "poisoned");
  });

  it("drains initially admitted work before entering the capture body", async () => {
    const r = rig();
    const drain = deferred<void>();
    const enteredReap = deferred<void>();
    const reservation = r.registry.reserveLaunch("command");
    assert.equal(reservation.kind, "reserved");
    if (reservation.kind !== "reserved") return;
    r.registry.registerRoot(reservation.reservation, { kind: "command",
      reap: async () => { enteredReap.resolve(); await drain.promise; return { ok: true }; },
      dispose: async () => {} });
    let entered = false;
    const pending = r.safety.withCredentialFreeCapture(10_000, async () => { entered = true; return "drained"; });
    try {
      await enteredReap.promise;
      await setImmediate();
      assert.equal(entered, false, "capture body entered before initially admitted root drained");
    } finally { drain.resolve(); }
    const result = await pending;
    assert.equal(result.kind, "observed_empty");
    assert.equal(entered, true);
  });

  it("keeps public status, next capture and disposal pending after primary exit until full-root drain", async () => {
    const drain = deferred<void>();
    const r = rig({ drain: drain.promise });
    let complete = false;
    let nextEntered = false;
    let disposed = false;
    const first = r.status().then(value => { complete = true; return value; });
    await r.primaryExited.promise;
    await r.reaping.promise;
    const next = r.safety.withCredentialFreeCapture(10_000, async () => { nextEntered = true; });
    const disposal = r.safety.dispose({ boundary: "checkpoint", deadlineMs: 10_000 })
      .then(value => { disposed = true; return value; });
    try {
      await setImmediate();
      assert.equal(complete, false, "capture completed on primary exit before full-root drain");
      assert.equal(nextEntered, false, "next capture entered while predecessor root remained live");
      assert.equal(disposed, false, "terminal disposal overtook capture");
    } finally { drain.resolve(); }
    const [a, b, d] = await Promise.all([first, next, disposal]);
    assert.equal(a.kind, "observed_empty");
    assert.equal(b.kind, "observed_empty");
    assert.equal(d.kind, "disposed");
  });

  it("expires queued captures without bypassing the current owner", async () => {
    const r = rig();
    const hold = deferred<void>();
    const entered = deferred<void>();
    const first = r.safety.withCredentialFreeCapture(10_000, async () => { entered.resolve(); await hold.promise; });
    await entered.promise;
    let called = false;
    const controller = new AbortController();
    const expired = r.safety.withCredentialFreeCapture(10_000, async () => { called = true; }, controller.signal);
    controller.abort();
    const result = await expired;
    const thirdEntered = deferred<void>();
    const third = r.safety.withCredentialFreeCapture(10_000, async () => { thirdEntered.resolve(); });
    let thirdComplete = false;
    void thirdEntered.promise.then(() => { thirdComplete = true; });
    try {
      await setImmediate();
      assert.equal(result.kind, "incomplete");
      assert.equal(called, false);
      assert.equal(thirdComplete, false, "expired queue slot released its successor early");
    } finally { hold.resolve(); }
    await Promise.all([first, third]);
  });

  it("revokes an escaped asynchronous GitCache continuation after callback return", async () => {
    const r = rig();
    const resume = deferred<void>();
    let late!: Promise<string[] | null>;
    const result = await r.safety.withCredentialFreeCapture(10_000, context =>
      r.git.withCredentialFreeCapture(context, async () => {
        late = (async () => { await resume.promise; return r.git.worktreeStatus("/capture/runner"); })();
        return "finished";
      }));
    resume.resolve();
    assert.equal(await late, null);
    assert.equal(result.kind, "observed_empty");
    assert.equal(r.specs.length, 0, "expired context reached launcher");
  });

  it("deadline revocation joins owned root cleanup before rejecting", async () => {
    const drain = deferred<void>();
    const r = rig({ drain: drain.promise });
    let settled = false;
    const pending = r.status().then(value => ({ value }), error => ({ error }))
      .then(value => { settled = true; return value; });
    await r.reaping.promise;
    r.expire();
    try {
      await setImmediate();
      assert.equal(settled, false, "deadline released custody before root cleanup");
    } finally { drain.resolve(); }
    const result = await pending;
    assert.ok("error" in result);
    assert.match(String(result.error), /capture|boundary/i);
  });

  it("uses worker identity for a public bare reader without granting credentials", async () => {
    const r = rig();
    const result = await r.safety.withCredentialFreeCapture(10_000, context =>
      r.git.withCredentialFreeCapture(context, () => r.git.originBranchTip("/capture/worker.git", "main")));
    assert.equal(result.kind, "observed_empty");
    if (result.kind !== "observed_empty") return;
    assert.equal(result.value, "a".repeat(40));
    assert.equal(r.specs.length, 1);
    assert.equal(r.specs[0]!.identity, "worker_pat");
    assert.ok(r.specs[0]!.args.includes("refs/remotes/origin/main^{commit}"));
    assert.equal(Object.values(r.specs[0]!.env).some(value => value?.includes("Authorization:")), false);
  });

  it("invalidates completed continuation proof after later capture work", async () => {
    const r = rig();
    const first = await r.status();
    assert.equal(first.kind, "observed_empty");
    if (first.kind !== "observed_empty") return;
    assert.equal(r.safety.acceptsCompletedCapture(first.proof), true);
    await r.status();
    assert.equal(r.safety.acceptsCompletedCapture(first.proof), false);
  });

  it("keeps exact clean teardown evidence when the capture action fails", async () => {
    const r = rig();
    const primary = new Error("capture primary failure");
    await assert.rejects(r.safety.withCredentialFreeCapture(10_000, async context => {
      await r.git.withCredentialFreeCapture(context, () => r.git.worktreeStatus("/capture/runner"));
      throw primary;
    }), error => error === primary);
    const proof = r.safety.completedCaptureForTeardown();
    assert.ok(proof);
    assert.equal(r.safety.acceptsCompletedCapture(proof), true);
    r.registry.poison({ category: "protocol", message: "later cleanup fault" });
    assert.equal(r.safety.completedCaptureForTeardown(), undefined);
    assert.equal(r.safety.acceptsCompletedCapture(proof), false);
  });

  it("fresh poison invalidates completion even when diagnostic retention is capped", async () => {
    const r = rig();
    const first = await r.status();
    assert.equal(first.kind, "observed_empty");
    if (first.kind !== "observed_empty") return;
    for (let i = 0; i < MAX_POISON_ERRORS; i++) r.registry.poison({ category: "protocol", message: "diagnostic " + i });
    assert.equal(r.registry.poisonErrors().length, MAX_POISON_ERRORS);
    const second = await r.safety.withCredentialFreeCapture(10_000, async () => "diagnostic capture");
    assert.equal(second.kind, "observed_empty");
    if (second.kind !== "observed_empty") return;
    // A separately poisoned epoch cannot mint a continued-epoch teardown proof.
    assert.equal(r.safety.acceptsCompletedCapture(second.proof), false);
    const observation = await r.registry.settleForCapture(10_000);
    const lane = r.registry.beginCapture(observation);
    try {
      assert.equal(lane.live(), true);
      assert.ok(lane.complete(), "completion evidence exists before new poison at cap");
      r.registry.poison({ category: "protocol", message: "after cap" });
      assert.equal(lane.live(), false, "new poison must revoke the lane despite capped diagnostics");
      assert.equal(lane.complete(), undefined);
    } finally { lane.close(); }
    assert.equal(r.registry.poisonErrors().length, MAX_POISON_ERRORS);
    assert.equal(r.safety.acceptsCompletedCapture(first.proof), false);
    assert.equal(r.safety.acceptsCompletedCapture(second.proof), false);
  });
});

// Supplemental policy cells. These use a genuine safety context and the internal
// policy adapter, not a claim that a runner or GitCache exposes arbitrary spawn.
describe("credential-free canonical policy before launcher admission", () => {
  const baseRequest = (): BoundaryProcessRequest => ({
    argv: ["/usr/bin/git", "status", "--porcelain"], cwd: "/capture/runner",
    identity: "command", env: gitEnv(),
  });
  const cases: Array<[string, (request: BoundaryProcessRequest) => BoundaryProcessRequest]> = [
    ["config override", r => ({ ...r, argv: [...r.argv, "-c", "credential.helper=sentinel-helper"] })],
    ["empty duplicate config argv", r => ({ ...r, argv: [...r.argv, "-c", "core.askpass="] })],
    ["indexed duplicate", r => { const env = { ...r.env }; const n = Number(env.GIT_CONFIG_COUNT);
      env.GIT_CONFIG_COUNT = String(n + 1); env[`GIT_CONFIG_KEY_${n}`] = "credential.helper";
      env[`GIT_CONFIG_VALUE_${n}`] = ""; return { ...r, env }; }],
    ["reordered canonical pairs", r => { const env = { ...r.env };
      [env.GIT_CONFIG_KEY_0, env.GIT_CONFIG_KEY_1] = [env.GIT_CONFIG_KEY_1, env.GIT_CONFIG_KEY_0];
      [env.GIT_CONFIG_VALUE_0, env.GIT_CONFIG_VALUE_1] = [env.GIT_CONFIG_VALUE_1, env.GIT_CONFIG_VALUE_0];
      return { ...r, env }; }],
    ...["GIT_ASKPASS", "SSH_ASKPASS", "GIT_CREDENTIAL_HELPER", "GCM_INTERACTIVE",
      "GIT_CONFIG_PARAMETERS", "ANTHROPIC_API_KEY"].map(key =>
      [key, (r: BoundaryProcessRequest) => ({ ...r, env: { ...r.env, [key]: "capture-sentinel" } })] as [string, (r: BoundaryProcessRequest) => BoundaryProcessRequest]),
    ["URL userinfo", r => ({ ...r, argv: [...r.argv, "https://capture-user:capture-sentinel@example.invalid/repo"] })],
    ["rewrite URL userinfo", r => ({ ...r, argv: [...r.argv, "-c", "url.https://capture-user@example.invalid/.insteadOf=local"] })],
    ["authorization config", r => ({ ...r, argv: [...r.argv, "-c", "http.extraHeader=Authorization: capture-sentinel"] })],
    ["include config", r => ({ ...r, argv: [...r.argv, "-c", "include.path=/capture/config"] })],
    ["config-env", r => ({ ...r, argv: [...r.argv, "--config-env=credential.helper=CAPTURE_SENTINEL"] })],
  ];
  for (const [name, alter] of cases) {
    it("rejects " + name + " with zero launcher calls", async () => {
      const r = rig();
      await r.safety.withCredentialFreeCapture(10_000, async (context: CredentialFreeCaptureContext) => {
        const canonical = gitEnv();
        const spawn = captureGitSpawner(context, request =>
          validateCaptureGitRequest(request, [canonical], "/usr/bin/git", process.execPath, "unused-helper"));
        await assert.rejects(async () => spawn(alter(baseRequest())), error => {
          assert.match(String(error), /credential-free capture launch refused/);
          assert.equal(String(error).includes("capture-sentinel"), false);
          return true;
        });
      });
      assert.equal(r.specs.length, 0);
    });
  }
});
