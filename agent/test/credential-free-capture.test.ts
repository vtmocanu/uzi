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
import { assertCaptureLaunch } from "../src/git.js";
import { validateCaptureGitRequest } from "../src/credential-free-capture.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { credentialFreeGitFixture, latch } from "./credential-free-git-fixture.js";

function scopedRequest(git: GitCache, context: CredentialFreeCaptureContext, request: BoundaryProcessRequest) {
  const internal = git as unknown as {
    execScoped(command: string, args: string[], options: { env: NodeJS.ProcessEnv; cwd: string; timeout?: number },
      identity: BoundaryProcessRequest["identity"]): Promise<{ stdout: string; stderr: string }>;
  };
  return git.withCredentialFreeCapture(context, () => internal.execScoped(request.argv[0]!, request.argv.slice(1),
    { env: request.env, cwd: request.cwd, timeout: request.timeoutMs }, request.identity));
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

// The real GitCache, safety owner, registry, and production sandbox adapter run.
// Only the OS supervisor is replaced: primary exit and ECHILD drain are distinct.
function rig(options: { drain?: Promise<void>; primary?: Promise<void>; output?: (spec: CodexEffectLaunchSpec) => string; setup?: (spec: CodexEffectLaunchSpec) => Promise<void>; drainFor?: (spec: CodexEffectLaunchSpec) => Promise<void> | undefined } = {}) {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
  const specs: CodexEffectLaunchSpec[] = [];
  const primaryExited = deferred<void>();
  const reaping = deferred<void>();
  let disposed = 0;
  const launch = async (spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
    await options.setup?.(spec);
    assertCaptureLaunch(spec);
    specs.push(spec);
    const stdout = new PassThrough();
    stdout.end(options.output?.(spec) ?? (spec.args.includes("status") ? " M capture-sentinel.txt\n" : "a".repeat(40) + "\n"));
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
        await options.drainFor?.(spec);
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
  it("refuses missing process wiring before callback entry or reads without reconciliation", async () => {
    let entries = 0;
    let reads = 0;
    let reconciles = 0;
    const safety = createCodexExecutionSafety(
      new ExecutionRegistry(newLocalExecutionEpoch(2852)),
      async () => { throw new Error("unexpected generic spawn"); },
      async () => { reconciles++; return { kind: "ready" }; },
    );
    const result = await safety.withCredentialFreeCapture(10_000, async () => {
      entries++;
      reads++;
      return "must not read";
    });
    assert.equal(entries, 0, "capture callback entered without process wiring");
    assert.equal(reads, 0);
    assert.equal(reconciles, 0);
    assert.equal(result.kind, "incomplete");
    const legacy = createCodexExecutionSafety(
      new ExecutionRegistry(newLocalExecutionEpoch(2852)),
      async () => { throw new Error("unexpected generic spawn"); },
    );
    assert.equal((await legacy.settleForCredentialFreeCapture(10_000)).kind, "observed_empty");
    const wired = rig();
    let wiredEntries = 0;
    const control = await wired.safety.withCredentialFreeCapture(10_000, async () => {
      wiredEntries++;
      return "wired read";
    });
    assert.equal(control.kind, "observed_empty");
    if (control.kind === "observed_empty") assert.equal(control.value, "wired read");
    assert.equal(wiredEntries, 1);
  });

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
      const git = new GitCache(process.env.TMPDIR!, nullLogger());
      const spawn = (request: BoundaryProcessRequest) => scopedRequest(git, context, request);
      // Caller discards startup: safety must still own the failure and cleanup.
      void spawn({ argv: ["/usr/bin/git", "status"], cwd: "/capture/runner", identity: "command", env: gitEnv() }).catch(() => {});
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
        const git = new GitCache(process.env.TMPDIR!, nullLogger());
        const spawn = (request: BoundaryProcessRequest) => scopedRequest(git, context, request);
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

describe("credential-free real Git producer positives", () => {
  it("verifies full worker-bare recovery closure through exec readers and streaming cat-file with canonical pins", async () => {
    const r = await credentialFreeGitFixture();
    try {
      const result = await r.capture(() => r.git.verifyRecoveryClosure(r.bare, r.base));
      assert.equal(result.kind, "observed_empty");
      if (result.kind !== "observed_empty") return;
      assert.equal(result.value, true, "complete trusted closure must be read and hashed");
      assert.deepEqual(r.requests.map(request => request.argv.slice(3)), [
        ["--no-replace-objects", "rev-parse", "--verify", r.base + "^{commit}"],
        ["--no-replace-objects", "rev-list", "--objects", "--no-object-names", "--missing=error", r.base],
        ["--no-replace-objects", "cat-file", "--batch-check"],
        ["--no-replace-objects", "cat-file", "--batch"],
      ], "all three exec readers and the streaming producer actually launched");
      assert.deepEqual(r.requests.map(request => request.cwd), ["/", "/", "/", r.bare]);
      for (const [i, request] of r.requests.entries()) {
        assert.equal(request.identity, "worker_pat");
        assert.equal(r.specs[i]!.identity, "worker_pat");
        assert.equal(r.specs[i]!.command, "/usr/bin/git");
        assert.equal(request.argv[2], r.bare);
        for (const env of [request.env, r.specs[i]!.env]) {
          const pairs = Array.from({ length: Number(env.GIT_CONFIG_COUNT) }, (_, n) =>
            [env[`GIT_CONFIG_KEY_${n}`], env[`GIT_CONFIG_VALUE_${n}`]]);
          for (const key of ["credential.helper", "core.askpass"]) {
            assert.deepEqual(pairs.filter(pair => pair[0] === key), [[key, ""]], key);
          }
          assert.equal(env.GIT_NO_LAZY_FETCH, "1");
          assert.equal(env.GIT_ALLOW_PROTOCOL, "none");
          assert.equal(env.GIT_NO_REPLACE_OBJECTS, "1");
          assert.equal(env.GIT_SHALLOW_FILE, "/dev/null");
          assert.equal(env.GIT_GRAFT_FILE, "/dev/null");
        }
      }
      assert.equal(r.disposed(), 4, "every exec and streaming root joined full modeled cleanup");
      assert.equal(r.registry.pendingLaunchCount(), 0);
      assert.equal(r.registry.rootCount(), 4, "drained roots remain in the ownership inventory");
      assert.equal(r.safety.acceptsCompletedCapture(result.proof), true);

      // Screen altered request DATA only; no substituted request reaches an OS process.
      const reader = r.requests[0]!;
      for (const [key, value] of [
        ["GIT_ALLOW_PROTOCOL", "file"], ["GIT_ALLOW_PROTOCOL", "https"],
        ["GIT_NO_LAZY_FETCH", "0"], ["GIT_NO_REPLACE_OBJECTS", "0"],
        ["GIT_SHALLOW_FILE", r.bare], ["GIT_GRAFT_FILE", r.bare],
      ]) {
        await r.safety.withCredentialFreeCapture(10_000, async context => {
          const spawn = (request: BoundaryProcessRequest) => scopedRequest(r.git, context, request);
          await assert.rejects(async () => spawn({ ...reader, env: { ...reader.env, [key!]: value } }),
            /credential-free capture launch refused/, key);
        });
      }
      assert.equal(r.requests.length, 4, "wrong transport and decorations never launch");
    } finally { r.cleanup(); }
  });

  it("restores latest worker-bare FETCH content through the public caller and waits for full disposal", async () => {
    const r = await credentialFreeGitFixture();
    const drain = latch();
    try {
      fs.writeFileSync(path.join(r.clone, "capture.txt"), "latest captured content\n");
      r.gitIn(r.clone, ["add", "capture.txt"]);
      r.gitIn(r.clone, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
        "commit", "-m", "latest capture"]);
      const latest = r.gitIn(r.clone, ["rev-parse", "HEAD"]);
      r.holdDisposal(drain.promise);
      let completed = false;
      const pending = r.capture(() => r.git.fetchAgentBranch(r.bare, r.clone, r.branch, r.run, r.opts))
        .then(result => { completed = true; return result; });
      // A policy refusal must surface as that refusal, not hang waiting for a handle.
      const reached = await Promise.race([
        r.disposing.promise.then(() => "disposing"),
        pending.then(() => "completed"),
      ]);
      assert.equal(reached, "disposing");
      try {
        await r.primary.promise;
        await setImmediate();
        assert.equal(completed, false, "primary exit cannot complete capture");
        assert.equal(r.registry.rootCount(), r.requests.length, "fetch root remains registered until disposal");
        assert.equal(r.requests.at(-1)!.argv.includes("fetch"), true);
        assert.equal(r.disposed(), r.requests.length - 1, "fetch disposal is still pending");
      } finally { drain.resolve(); }
      const result = await pending;
      assert.equal(result.kind, "observed_empty");
      if (result.kind !== "observed_empty") return;
      assert.equal(result.value.kind, "updated");
      assert.equal(r.gitIn(r.bare, ["rev-parse", "refs/uzi-runner/" + r.branch]), latest);
      assert.equal(r.gitIn(r.bare, ["show", latest + ":capture.txt"]), "latest captured content");
      const index = r.requests.findIndex(request => request.argv.includes("fetch"));
      assert.ok(index >= 0, "actual fetch launched");
      const fetch = r.requests[index]!;
      assert.equal(fetch.identity, "worker_pat");
      assert.equal(fetch.cwd, "/");
      assert.equal(fetch.argv[fetch.argv.indexOf("-C") + 1], r.bare);
      assert.ok(fetch.argv.includes("file://" + r.clone));
      assert.equal(fetch.argv[fetch.argv.indexOf("protocol.file.allow=user") - 1], "-c");
      assert.equal(r.specs[index]!.command, fetch.argv[0], "worker-bare Git is not the command sandbox");
      for (const request of r.requests) {
        const pairs = Array.from({ length: Number(request.env.GIT_CONFIG_COUNT) }, (_, i) =>
          [request.env[`GIT_CONFIG_KEY_${i}`], request.env[`GIT_CONFIG_VALUE_${i}`]]);
        for (const key of ["credential.helper", "core.askpass"]) {
          assert.equal(pairs.filter(pair => pair[0] === key && pair[1] === "").length, 1, key);
        }
      }
      const cloneRead = await r.capture(() => r.git.worktreeStatus(r.clone));
      assert.equal(cloneRead.kind, "observed_empty");
      assert.equal(r.requests.at(-1)!.identity, "command", "clone Git keeps command identity");
      assert.equal(r.specs.at(-1)!.command, "/usr/local/bin/uzi-codex-command-sandbox");
      assert.equal(r.disposed(), r.requests.length, "every primary joined full modeled disposal");
    } finally { drain.resolve(); r.cleanup(); }
  });

  it("refuses missing generation before launch and mismatched run identity before fetch", async () => {
    const r = await credentialFreeGitFixture();
    try {
      await r.capture(async () => {
        const invalid = { ...r.opts, context: { ...r.opts.context, generation: undefined } };
        await assert.rejects(r.git.fetchAgentBranch(r.bare, r.clone, r.branch, r.run,
          invalid as unknown as typeof r.opts), /invalid owed candidate context/);
        assert.equal(r.requests.length, 0);
        await assert.rejects(r.git.fetchAgentBranch(r.bare, r.clone, r.branch,
          "28522852-aaaa-bbbb-cccc-555555555555", r.opts), /run identity mismatch/);
      });
      assert.equal(r.requests.some(request => request.argv.includes("fetch")), false);
    } finally { r.cleanup(); }
  });

  it("admits the actual fixed cancellation helper as a fully registered command root", async () => {
    const r = await credentialFreeGitFixture();
    try {
      const result = await r.capture(() => r.git.credentialFreeCancelCleanHead(r.clone, r.bare, r.base));
      assert.equal(result.kind, "observed_empty");
      if (result.kind !== "observed_empty") return;
      assert.equal(result.value, r.base, "clean content matches the worker-trusted start");
      const helper = r.requests.find(request => request.argv[0] === process.execPath);
      assert.ok(helper, "the real producer reached its private fixed helper");
      assert.equal(helper.identity, "command");
      assert.equal(helper.argv.length, 5);
      assert.ok(Number.isSafeInteger(Number(helper.argv[4])));
      const pairs = Array.from({ length: Number(helper.env.GIT_CONFIG_COUNT) }, (_, i) =>
        [helper.env[`GIT_CONFIG_KEY_${i}`], helper.env[`GIT_CONFIG_VALUE_${i}`]]);
      for (const key of ["credential.helper", "core.askpass"])
        assert.deepEqual(pairs.filter(pair => pair[0] === key), [[key, ""]]);
      const policy = (request: BoundaryProcessRequest) => validateCaptureGitRequest(
        request, [helper.env], "/usr/bin/git", process.execPath, helper.argv[2]!);
      policy(helper);
      for (const value of ["9", "9".repeat(400), "9007199254740992", "Infinity", "NaN",
        String(Date.now() - 1), "1.5", "+9999999999999", "09999999999999", "1e13", " 9999999999999", ""]) {
        assert.throws(() => policy({ ...helper, argv: [...helper.argv.slice(0, 4), value] }),
          /credential-free capture launch refused/, "numeric policy: " + value);
      }
      assert.throws(() => policy({ ...helper, argv: helper.argv.slice(0, 4) }),
        /credential-free capture launch refused/, "explicit deadline required");
      assert.equal(r.disposed(), r.requests.length, "helper and Git roots fully disposed");
      assert.equal(r.registry.rootCount(), r.requests.length, "roots remain in the ownership inventory");

      // Replay DATA only through the actual validator, before trusted registration.
      // Never execute a substituted script, executable, argv or environment.
      const alters: Array<[string, BoundaryProcessRequest]> = [
        ["raw canonical request", helper],
        ["forged provenance flags", { ...helper, captureCancelContent: true, producer: "credentialFreeCancelCleanHead" } as BoundaryProcessRequest],
        ["script", { ...helper, argv: helper.argv.map((arg, i) => i === 2 ? arg + "\n// substituted" : arg) }],
        ["executable", { ...helper, argv: ["/capture/substituted-node", ...helper.argv.slice(1)] }],
        ["argv", { ...helper, argv: [...helper.argv, "substituted"] }],
        ["environment", { ...helper, env: { ...helper.env, NODE_OPTIONS: "--substituted" } }],
        ["identity", { ...helper, identity: "worker_pat" }],
      ];
      const launches = r.requests.length;
      for (const [name, altered] of alters) {
        await r.safety.withCredentialFreeCapture(10_000, async context => {
          const spawn = (request: BoundaryProcessRequest) => scopedRequest(r.git, context, request);
          await assert.rejects(async () => spawn(altered), /credential-free capture launch refused/, name);
        });
      }
      assert.equal(r.requests.length, launches, "substitutions never reach the actual launcher");
      fs.writeFileSync(path.join(r.clone, "capture.txt"), "untrusted changed content\n");
      const changed = await r.capture(() => r.git.credentialFreeCancelCleanHead(r.clone, r.bare, r.base));
      assert.equal(changed.kind, "observed_empty");
      if (changed.kind === "observed_empty") assert.equal(changed.value, null);
      assert.equal(r.requests.filter(request => request.argv[0] === process.execPath).length, 1,
        "changed clone content never reaches the helper launcher");
    } finally { r.cleanup(); }
  });
});

describe("credential-free fixed helper deadline binding", () => {
  const sha = "a".repeat(40);
  const isHelper = (spec: CodexEffectLaunchSpec) => spec.args.includes(process.execPath);
  const output = (spec: CodexEffectLaunchSpec) => isHelper(spec) ? "clean\n"
    : spec.args.includes("rev-parse") ? sha + "\n" : "";
  const produce = (r: ReturnType<typeof rig>) =>
    r.git.credentialFreeCancelCleanHead("/capture/runner", "/capture/worker.git", sha);

  it("binds the allocated integer and freezes setup, joins cleanup, then revokes within the same capture", async () => {
    const draining = latch(), release = latch(), resume = latch();
    let helperSpec: CodexEffectLaunchSpec | undefined;
    const r = rig({ output, drainFor: spec => {
      if (!isHelper(spec)) return undefined;
      draining.resolve();
      return release.promise;
    }, setup: async spec => {
      if (!isHelper(spec)) return;
      await setImmediate();
      helperSpec = spec;
      assert.equal(Reflect.set(spec.args, spec.args.length - 1, "9"), false);
      assert.equal(Reflect.set(spec.env, "NODE_OPTIONS", "--substituted"), false);
      assertCaptureLaunch(spec);
    } });
    const internal = r.git as any, original = internal.execScoped.bind(r.git);
    let late: Promise<unknown> | undefined;
    let allocated = "";
    internal.execScoped = async (command: string, args: string[], opts: any, identity: any) => {
      if (command !== process.execPath) return original(command, args, opts, identity);
      allocated = args[3]!;
      assert.equal(Object.isFrozen(args), true);
      assert.equal(Object.isFrozen(opts.env), true);
      late = (async () => {
        await resume.promise;
        return original(command, args, opts, identity);
      })();
      return original(command, args, opts, identity);
    };
    let settled = false;
    try {
      const result = await r.safety.withCredentialFreeCapture(10_000, context =>
        r.git.withCredentialFreeCapture(context, async () => {
          const pending = produce(r).then(value => { settled = true; return value; });
          const reached = await Promise.race([draining.promise.then(() => "draining"),
            pending.then(() => "completed")]);
          assert.equal(reached, "draining");
          assert.equal(settled, false, "primary/helper output cannot release full-root custody");
          assert.ok(helperSpec);
          assert.equal(r.registry.rootCount(), r.specs.length);
          assertCaptureLaunch(helperSpec);
          release.resolve();
          assert.equal(await pending, sha);
          assert.throws(() => assertCaptureLaunch(helperSpec!), /credential-free capture launch refused/);
          const before = r.specs.length;
          resume.resolve();
          await assert.rejects(late!, /credential-free capture launch refused/);
          assert.equal(r.specs.length, before, "revoked descendant cannot relaunch while capture is live");
          return sha;
        }));
      assert.equal(result.kind, "observed_empty");
      assert.ok(Number.isSafeInteger(Number(allocated)));
    } finally { release.resolve(); resume.resolve(); await late?.catch(() => undefined); }
  });

  for (const value of ["different future integer", "9", "9".repeat(400), "9007199254740992",
    "Infinity", "NaN", "1.5", "01", "+9", "1e13", ""] as const)
    it("rejects private helper deadline substitution " + (value.length > 30 ? "huge decimal" : JSON.stringify(value)), async () => {
      const r = rig({ output });
      const internal = r.git as any, original = internal.execScoped.bind(r.git);
      let checked = false;
      internal.execScoped = async (command: string, args: string[], opts: any, identity: any) => {
        if (command !== process.execPath) return original(command, args, opts, identity);
        const replacement = value === "different future integer" ? String(Number(args[3]) - 1) : value;
        assert.notEqual(replacement, args[3]);
        if (value === "different future integer") {
          assert.ok(Number.isSafeInteger(Number(replacement)));
          assert.ok(Number(replacement) > Date.now());
        }
        const before = r.specs.length;
        // Validator DATA only; the rig has no OS execution API.
        await assert.rejects(async () => internal.boundaryProcesses.getStore().spawn({
          argv: [command, ...args.slice(0, 3), replacement], cwd: opts.cwd, env: opts.env, identity,
        }), /credential-free capture launch refused/);
        checked = true;
        assert.equal(r.specs.length, before);
        throw new Error("substitution control complete");
      };
      const result = await r.safety.withCredentialFreeCapture(10_000, context =>
        r.git.withCredentialFreeCapture(context, () => produce(r)));
      assert.equal(result.kind, "observed_empty");
      if (result.kind === "observed_empty") assert.equal(result.value, null);
      assert.equal(checked, true, "actual private producer issued its frozen helper");
      assert.equal(r.specs.some(isHelper), false);
    });

  for (const bound of ["maintenance", "capture", "report", "recovery"] as const)
    it("rounds fractional maintenance DOWN and retains the original " + bound + " bound", async () => {
      const clock = Date.now, wall = clock();
      let tick = 0;
      const budget = new MaintenanceBudget(9000.75, () => tick);
      let allocated = 0;
      const r = rig({ output, setup: async spec => {
        if (!isHelper(spec)) return;
        await setImmediate();
        allocated = Number(spec.args.at(-1));
        assert.equal(allocated, wall + (bound === "maintenance" ? 9000 : 4000));
        tick = 1.25;
        assert.equal(Number(spec.args.at(-1)), allocated, "setup cannot renew allocated deadline");
      } });
      Date.now = () => wall;
      try {
        const result = await r.safety.withCredentialFreeCapture(bound === "capture" ? 4000 : 10_000, context =>
          r.git.withCredentialFreeCapture(context, () =>
            r.git.withReportProofBudget({ signal: new AbortController().signal,
              deadline: wall + (bound === "report" ? 4000 : 20_000) }, () =>
              r.git.withRecoveryOperation(new AbortController().signal,
                wall + (bound === "recovery" ? 4000 : 20_000), () => produce(r), budget))));
        assert.equal(result.kind, "observed_empty");
        if (result.kind === "observed_empty") assert.equal(result.value, sha);
        assert.ok(allocated > 0, "actual helper reached final setup");
      } finally { Date.now = clock; budget.dispose(); }
    });

  for (const cell of ["deadline", "maintenance", "report abort", "recovery abort"] as const)
    it("refuses " + cell + " after asynchronous setup before helper spawn", async () => {
      const clock = Date.now, wall = clock();
      let tick = 0, setups = 0;
      const budget = new MaintenanceBudget(9000.75, () => tick);
      const report = new AbortController(), recovery = new AbortController();
      const r = rig({ output, setup: async spec => {
        if (!isHelper(spec)) return;
        await setImmediate();
        setups++;
        if (cell === "deadline") Date.now = () => Number(spec.args.at(-1));
        if (cell === "maintenance") tick = 9001;
        if (cell === "report abort") report.abort(new Error("report setup abort"));
        if (cell === "recovery abort") recovery.abort(new Error("recovery setup abort"));
        assert.throws(() => assertCaptureLaunch(spec),
          /credential-free capture launch refused|maintenance deadline exhausted|setup abort/);
        // Restore only wall time; revoked deadline is also checked by rig immediately below.
      } });
      Date.now = () => wall;
      try {
        await assert.rejects(r.safety.withCredentialFreeCapture(10_000, context =>
          r.git.withCredentialFreeCapture(context, () =>
            r.git.withReportProofBudget({ signal: report.signal, deadline: wall + 15_000 }, () =>
              r.git.withRecoveryOperation(recovery.signal, wall + 15_000, () => produce(r), budget)))),
          /codex boundary failed at action/);
        assert.equal(setups, 1);
        assert.equal(r.specs.some(isHelper), false, "final launch refused before fake spawn");
      } finally { Date.now = clock; budget.dispose(); }
    });

  it("retains flightless legacy helper argv without capture or a new budget", async () => {
    const r = await credentialFreeGitFixture();
    const internal = r.git as any, original = internal.execScoped.bind(r.git);
    let helperArgs: string[] | undefined;
    internal.execScoped = (command: string, args: string[], opts: any, identity: any) => {
      if (args[0] === "-e") {
        helperArgs = [...args];
        assert.equal(identity, "command");
        assert.equal(internal.boundaryProcesses.getStore(), undefined);
        assert.equal(MaintenanceBudget.current(), undefined);
        assert.equal(args.length, 3, "legacy helper retains optional deadline omission");
        assert.equal(args[2], r.clone);
      }
      return original(command, args, opts, identity);
    };
    try {
      assert.equal(await r.git.credentialFreeCancelCleanHead(r.clone, r.bare, r.base), r.base);
      assert.ok(helperArgs, "actual legacy fixed helper executed");
      assert.equal(r.requests.length, 0, "no capture queue, sink, or registry needed");
    } finally { r.cleanup(); }
  });

  it("does not launch with an already exhausted maintenance budget", async () => {
    let tick = 0;
    const budget = new MaintenanceBudget(10_000, () => tick);
    const r = rig({ output });
    try {
      tick = 10_001;
      assert.throws(MaintenanceBudget.run.bind(null, budget, () => produce(r)),
        /maintenance deadline exhausted/);
      assert.equal(r.specs.length, 0);
    } finally { budget.dispose(); }
  });
});

describe("credential-free private synthesis provenance", () => {
  const uuid = "12345678-1234-4234-8234-123456789abc";
  for (const [name, dynamic] of [
    ["well-shaped index", { GIT_INDEX_FILE: path.join(os.tmpdir(), "uzi-ckpt-idx-" + uuid) }],
    ["well-shaped work tree", { GIT_WORK_TREE: path.join(os.tmpdir(), "uzi-ckpt-work-" + uuid) }],
    ["valid date", { GIT_AUTHOR_DATE: "2000-01-01T00:00:00Z" }],
  ] as const) it("rejects arbitrary " + name + " outside the private producer", async () => {
    const r = rig();
    await r.safety.withCredentialFreeCapture(10_000, async context => {
      await assert.rejects(scopedRequest(r.git, context, {
        argv: ["/usr/bin/git", "-C", "/capture/worker.git", "write-tree"],
        cwd: "/", identity: "worker_pat", env: { ...gitEnv(), ...dynamic },
      }), /credential-free capture launch refused/);
    });
    assert.equal(r.specs.length, 0, "shape alone cannot reach launcher admission");
  });

  for (const cell of ["argv", "index", "worktree", "date", "author", "identity", "cwd"] as const)
    it("binds private synthesis " + cell + " before admission after async setup", async () => {
      const r = rig({ output: spec => spec.args.includes("show")
        ? "2000-01-01T00:00:00Z\n" : "a".repeat(40) + "\n" });
      const internal = r.git as any;
      const original = internal.execScoped.bind(r.git);
      let attempted = false, rejected = false;
      internal.execScoped = async (command: string, args: string[], opts: any, identity: any) => {
        const target = cell === "date" || cell === "author" ? opts.env?.GIT_AUTHOR_DATE
          : cell === "worktree" ? opts.env?.GIT_WORK_TREE : opts.env?.GIT_INDEX_FILE;
        if (!target || attempted) return original(command, args, opts, identity);
        attempted = true;
        const before = r.specs.length;
        // Cross a real async setup boundary, then substitute another shape-valid value.
        await setImmediate();
        const env = { ...opts.env };
        if (cell === "index") env.GIT_INDEX_FILE = path.join(os.tmpdir(), "uzi-ckpt-idx-" + uuid);
        if (cell === "worktree") env.GIT_WORK_TREE = path.join(os.tmpdir(), "uzi-ckpt-work-" + uuid);
        if (cell === "date") env.GIT_AUTHOR_DATE = "2001-01-01T00:00:00Z";
        if (cell === "author") env.GIT_AUTHOR_NAME = "another worker";
        try {
          return await original(command, cell === "argv" ? [...args, "--prefix=other"] : args,
            { ...opts, env, ...(cell === "cwd" ? { cwd: "/capture/other" } : {}) },
            cell === "identity" ? "command" : identity);
        } catch (error) {
          rejected = true;
          assert.equal(r.specs.length, before, "substitution reached the actual launcher");
          throw error;
        }
      };
      await r.safety.withCredentialFreeCapture(10_000, context =>
        r.git.withCredentialFreeCapture(context, async () => {
          await assert.rejects(internal.synthesizeWorkflowWrapper("/capture/worker.git",
            "a".repeat(40), "b".repeat(40), ["a".repeat(40)], "ckpt(overlay): test", true));
        }));
      assert.equal(attempted, true, "actual private producer reached the changed field");
      assert.equal(rejected, true);
    });

  it("freezes private values through asynchronous launcher setup and refuses delayed ALS reuse", async () => {
    let setups = 0;
    const r = rig({
      output: spec => spec.args.includes("show") ? "2000-01-01T00:00:00Z\n" : "a".repeat(40) + "\n",
      setup: async spec => {
        await setImmediate();
        if (!spec.env.GIT_INDEX_FILE && !spec.env.GIT_AUTHOR_DATE) return;
        setups++;
        assert.equal(Reflect.set(spec.env, "GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z"), false);
        assert.equal(Reflect.set(spec.args, 0, "substituted"), false);
        assert.equal(Reflect.set(spec, "cwd", "/capture/other"), false);
        assert.equal(Reflect.set(spec, "identity", "command"), false);
        // rig checks assertCaptureLaunch immediately after this awaited setup.
      },
    });
    const internal = r.git as any, original = internal.runGitWithEnv.bind(r.git);
    const resume = latch();
    let late: Promise<string> | undefined;
    internal.runGitWithEnv = (cwd: string, args: readonly string[], env: NodeJS.ProcessEnv) => {
      if (!late && env.GIT_INDEX_FILE) late = (async () => {
        await resume.promise;
        return original(cwd, [...args], { ...env });
      })();
      return original(cwd, args, env);
    };
    await r.safety.withCredentialFreeCapture(10_000, context =>
      r.git.withCredentialFreeCapture(context, async () => {
        try {
          assert.equal(await internal.synthesizeWorkflowWrapper("/capture/worker.git",
            "a".repeat(40), "b".repeat(40), ["a".repeat(40)], "ckpt(overlay): test", true), "a".repeat(40));
          const before = r.specs.length;
          assert.ok(late, "the asynchronous descendant retained the genuine private invocation");
          resume.resolve();
          await assert.rejects(late, /credential-free capture launch refused/);
          assert.equal(r.specs.length, before, "revoked private ALS cannot issue a second launch");
        } finally { resume.resolve(); await late?.catch(() => undefined); }
      }));
    assert.ok(setups >= 4, "dynamic commands reached asynchronous setup and the final launch check");
  });

  it("refuses actual launcher evidence after the private producer completes inside a live capture", async () => {
    const r = rig({ output: spec => spec.args.includes("show")
      ? "2000-01-01T00:00:00Z\n" : "a".repeat(40) + "\n" });
    await r.safety.withCredentialFreeCapture(10_000, context =>
      r.git.withCredentialFreeCapture(context, async () => {
        assert.equal(await (r.git as any).synthesizeWorkflowWrapper("/capture/worker.git",
          "a".repeat(40), "b".repeat(40), ["a".repeat(40)], "ckpt(overlay): test", true), "a".repeat(40));
        const specs = r.specs.filter(s => s.env.GIT_INDEX_FILE || s.env.GIT_AUTHOR_DATE);
        assert.ok(specs.length >= 4);
        for (const spec of specs) assert.throws(() => assertCaptureLaunch(spec),
          /credential-free capture launch refused/, "completed private invocation cannot authorize a delayed launch");
      }));
  });

  it("does not lend synthesis authority to the size-attribute writer", async () => {
    const r = rig();
    await r.safety.withCredentialFreeCapture(10_000, context =>
      r.git.withCredentialFreeCapture(context, async () => {
        await assert.rejects((r.git as any).sizeAttrGit("/capture/worker.git", ["read-tree", "a".repeat(40)],
          { indexFile: path.join(os.tmpdir(), "uzi-ckpt-idx-" + uuid) }), /credential-free capture launch refused/);
      }));
    assert.equal(r.specs.length, 0);
  });

  it("preserves workflow content, parents and identity through checkpoint-chain, bridge and coverage producers", async () => {
    const r = await credentialFreeGitFixture();
    try {
      fs.mkdirSync(path.join(r.clone, ".github/workflows"), { recursive: true });
      fs.writeFileSync(path.join(r.clone, ".github/workflows/check.yml"), "name: preserved\non: push\n");
      r.gitIn(r.clone, ["add", ".github/workflows/check.yml"]);
      const tree = r.gitIn(r.clone, ["write-tree"]);
      const previous = r.gitIn(r.clone, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
        "commit-tree", tree, "-p", r.base, "-m", "ckpt(overlay): fixture workflow"]);
      r.gitIn(r.clone, ["reset", "--hard", r.base]);
      fs.writeFileSync(path.join(r.clone, "capture.txt"), "new real content\n");
      r.gitIn(r.clone, ["add", "capture.txt"]);
      r.gitIn(r.clone, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "real work"]);
      const real = r.gitIn(r.clone, ["rev-parse", "HEAD"]);
      r.gitIn(r.bare, ["fetch", r.clone, r.branch]);
      // The wrapper object remains in the clone after reset; fetch it by its literal OID.
      r.gitIn(r.bare, ["fetch", r.clone, previous]);
      let chain = "";
      const result = await r.capture(async () => {
        // Task-named synthesis seam: the shared fixture deliberately permits only
        // closure streaming, so call the existing producer rather than widening it.
        chain = await (r.git as any).buildCheckpointChain(r.bare, r.branch, real, [previous]);
        assert.ok(chain);
        assert.notEqual(chain, real, "the chain must actually synthesize a wrapper");
        const dynamic = r.specs.filter(s => s.env.GIT_INDEX_FILE || s.env.GIT_AUTHOR_DATE);
        assert.ok(dynamic.length >= 4, "read-tree, rm, write-tree and commit-tree ran");
        for (const spec of dynamic) {
          assert.equal(spec.identity, "worker_pat");
          assert.equal(spec.command, "/usr/bin/git");
        }
      });
      assert.equal(result.kind, "observed_empty");
      assert.equal(r.gitIn(r.bare, ["show", chain + ":capture.txt"]), "new real content");
      assert.equal(r.gitIn(r.bare, ["show", chain + ":.github/workflows/check.yml"]), "name: preserved\non: push");
      assert.equal(r.gitIn(r.bare, ["show", "-s", "--format=%P", chain]), previous + " " + real);
      const commit = r.requests.find(q => q.argv.includes("commit-tree"))!;
      const date = r.gitIn(r.bare, ["show", "-s", "--format=%cI", real]);
      assert.equal(commit.env.GIT_AUTHOR_DATE, date);
      assert.equal(commit.env.GIT_COMMITTER_DATE, date);
      assert.equal(commit.env.GIT_AUTHOR_NAME, "uzi-agent");
      assert.equal(commit.env.GIT_COMMITTER_NAME, "uzi-agent");
      assert.equal(commit.env.GIT_AUTHOR_EMAIL, "uzi-agent@uzi.local");
      assert.equal(commit.env.GIT_COMMITTER_EMAIL, "uzi-agent@uzi.local");
      const bridge = await r.capture(() => r.git.bridgeToFloors(r.bare, real, [previous]));
      assert.equal(bridge.kind, "observed_empty");
      if (bridge.kind === "observed_empty") {
        assert.equal(bridge.value.kind, "built");
        if (bridge.value.kind === "built")
          assert.equal(r.gitIn(r.bare, ["rev-parse", bridge.value.sha + "^{tree}"]), r.gitIn(r.bare, ["rev-parse", real + "^{tree}"]));
      }
      const coverage = await r.capture(() => r.git.buildRecoveryCoverage(r.bare,
        r.opts.context as Parameters<GitCache["buildRecoveryCoverage"]>[1], [previous], real));
      assert.equal(coverage.kind, "observed_empty");
      if (coverage.kind === "observed_empty") {
        assert.equal(r.gitIn(r.bare, ["show", "-s", "--format=%P", coverage.value.sha]),
          [real, previous].sort().join(" "));
        assert.equal(r.gitIn(r.bare, ["rev-parse", coverage.value.sha + "^{tree}"]), r.gitIn(r.bare, ["rev-parse", real + "^{tree}"]));
      }
    } finally { r.cleanup(); }
  });
});

// Supplemental policy cells use a test-only private execScoped invocation.
// GitCache owns and executes the complete canonical policy before registration.
describe("credential-free canonical policy before launcher admission", () => {
  const baseRequest = (): BoundaryProcessRequest => ({
    argv: ["/usr/bin/git", "status", "--porcelain"], cwd: "/capture/runner",
    identity: "command", env: gitEnv(),
  });
  const cases: Array<[string, (request: BoundaryProcessRequest) => BoundaryProcessRequest]> = [
    ...["credential.helper", "core.askpass"].flatMap(key => [
      [key + " missing", (r: BoundaryProcessRequest) => {
        const env = { ...r.env };
        const i = Array.from({ length: Number(env.GIT_CONFIG_COUNT) }, (_, n) => n)
          .find(n => env[`GIT_CONFIG_KEY_${n}`] === key)!;
        delete env[`GIT_CONFIG_VALUE_${i}`];
        return { ...r, env };
      }],
      [key + " nonempty", (r: BoundaryProcessRequest) => {
        const env = { ...r.env };
        const i = Array.from({ length: Number(env.GIT_CONFIG_COUNT) }, (_, n) => n)
          .find(n => env[`GIT_CONFIG_KEY_${n}`] === key)!;
        env[`GIT_CONFIG_VALUE_${i}`] = "capture-sentinel";
        return { ...r, env };
      }],
      [key + " duplicate", (r: BoundaryProcessRequest) => {
        const env = { ...r.env }; const n = Number(env.GIT_CONFIG_COUNT);
        env.GIT_CONFIG_COUNT = String(n + 1);
        env[`GIT_CONFIG_KEY_${n}`] = key; env[`GIT_CONFIG_VALUE_${n}`] = "";
        return { ...r, env };
      }],
    ] as Array<[string, (request: BoundaryProcessRequest) => BoundaryProcessRequest]>),
    ...["", "-1", "02", "2.5", "NaN"].map(count =>
      ["malformed count " + JSON.stringify(count), (r: BoundaryProcessRequest) =>
        ({ ...r, env: { ...r.env, GIT_CONFIG_COUNT: count } })] as [string, (request: BoundaryProcessRequest) => BoundaryProcessRequest]),
    ["caller-added empty helper pin", r => ({ ...r, argv: [...r.argv, "-c", "credential.helper="] })],
    ["subsection helper", r => ({ ...r, argv: [...r.argv, "-c", "credential.https://example.invalid.helper="] })],
    ["includeIf", r => ({ ...r, argv: [...r.argv, "-c", "includeIf.gitdir:/capture/.path=/capture/config"] })],
    ["mixed-case Authorization", r => ({ ...r, argv: [...r.argv, "-c", "http.extraHeader=aUtHoRiZaTiOn: capture-sentinel"] })],
    ["URL rewrite environment key", r => ({ ...r, env: { ...r.env,
      "url.https://capture-user@example.invalid/.insteadOf": "local" } })],
    ["local file pin widened", r => ({ ...r, argv: [...r.argv, "-c", "protocol.file.allow=always"] })],
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
        const spawn = (request: BoundaryProcessRequest) => scopedRequest(r.git, context, request);
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
