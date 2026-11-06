import { it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { createCaptureContext, GitCache, type EpochDiscardProof } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { runnerCommand } from "../src/runner-uid.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

type Identity = "command" | "worker_pat";
type Options = { env: NodeJS.ProcessEnv; timeout?: number; maxBuffer?: number; input?: string };
interface PrivateCache {
  withEpochNativeReads<T>(proof: EpochDiscardProof, action: () => Promise<T>): Promise<T>;
  execScoped(command: string, args: string[], options: Options, identity?: Identity): Promise<{ stdout: string; stderr: string }>;
  runRunnerUidDelete(target: string): Promise<void>;
  epochNativeTestDependencies: {
    spawn: typeof spawn; alive: (pid: number, identity: Identity) => boolean;
    kill: (pid: number, identity: Identity) => void; joinMs: number;
  };
  unsettledDeletes: Map<string, number>;
  beginDestructive: () => Promise<{ settle(result: { ok: boolean }): Promise<void> }>;
}

function fixture(deleteLaunch = false, identity: Identity = "worker_pat") {
  const git = new GitCache(path.join(process.env.TMPDIR!, "cache"), nullLogger());
  const internal = git as unknown as PrivateCache;
  let pending = 0, stopped = false, inherited = 0, alive = true, killed = 0;
  const scope: PodEpochReclaimScope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let done = false;
      return { settle() { if (!done) { done = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) {
      assert.ok(pending > 0, "ticket before inheritance");
      inherited++;
      return [...stdio, 42];
    },
    stop() { stopped = true; }, stopped() { return stopped; },
    assertSettled() { assert.equal(pending, 0); assert.equal(stopped, false); },
    finish() { this.assertSettled(); },
  };
  const controller = new AbortController();
  const target = path.join(process.env.TMPDIR!, "source");
  // An epoch proof names one attempt: the canonical selector is refused by the constructor.
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const proof = git.createEpochDiscardProof({
    decision: { eligible: true, attested: true }, scope,
    selector: { runId: "epoch-native", generation: "none", attempt },
    barePath: path.join(process.env.TMPDIR!, "bare"), paths: [target], confirm: "a".repeat(64),
    expectedRawJournal: JSON.stringify({ attemptId: attempt, clonePath: target, runId: "epoch-native" }),
    beforeEffect: async () => {}, checkLive() {}, signal: controller.signal, deadline: Date.now() + 10_000,
  });
  const child = Object.assign(new EventEmitter(), {
    pid: 12345, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: null, signalCode: null,
  });
  let launch!: () => void;
  const launched = new Promise<void>(resolve => { launch = resolve; });
  internal.epochNativeTestDependencies = {
    spawn: ((_command: string, _args: string[], options: { detached: boolean; stdio: unknown; env: NodeJS.ProcessEnv }) => {
      assert.equal(options.detached, true);
      assert.deepEqual(options.stdio, ["pipe", "pipe", "pipe", 42]);
      if (deleteLaunch) {
        assert.deepEqual(options.env, { PATH: "/usr/bin:/bin" });
        const expected = runnerCommand("/bin/rm", ["-rf", "--", target], true);
        assert.equal(_command, expected.command);
        assert.deepEqual(_args, expected.args);
      }
      else {
        assert.equal(options.env.TMPDIR, identity === "command" ? process.env.UZI_RUNNER_TMPDIR ?? process.env.TMPDIR : process.env.TMPDIR);
        assert.equal(options.env.PATH, "/usr/bin:/bin");
        assert.equal(options.env.GIT_OPTIONAL_LOCKS, "0");
        assert.equal(options.env.GIT_CONFIG_GLOBAL, "/dev/null");
        for (const pin of ["core.multiPackIndex=false", "core.commitGraph=false", "pack.writeReverseIndex=false",
          "pack.readReverseIndex=false", "pack.threads=1", "index.threads=1", "core.deltaBaseCacheLimit=100663296"]) {
          const at = _args.indexOf(pin);
          assert.ok(at > 0);
          assert.equal(_args[at - 1], "-c");
        }
      }
      assert.equal(pending, deleteLaunch ? 2 : 1);
      launch();
      return child as unknown as ChildProcess;
    }) as unknown as typeof spawn,
    alive: () => alive,
    kill: pid => { assert.equal(pid, 12345); killed++; },
    joinMs: 60,
  };
  const invoke = (options: Omit<Options, "env"> = {}) => internal.withEpochNativeReads(proof, () => {
    const command = identity === "command" ? runnerCommand("/usr/bin/git", ["version"]) : { command: "/usr/bin/git", args: ["version"] };
    return internal.execScoped(command.command, command.args, { env: {}, ...options }, identity);
  });
  const close = (code = 0) => {
    child.stdout.end(); child.stderr.end(); child.emit("close", code, null);
  };
  const exit = (code = 0) => { child.emit("exit", code, null); };
  return { git, internal, child, scope, proof, invoke, launched, close, exit, controller, target,
    gone() { alive = false; }, pending: () => pending, stopped: () => stopped,
    inherited: () => inherited, killed: () => killed };
}

it("native runner preflight preserves its validated TMPDIR independently of worker TMPDIR", async () => {
  const old = process.env.UZI_RUNNER_TMPDIR;
  process.env.UZI_RUNNER_TMPDIR = path.join(process.env.TMPDIR!, "runner-private");
  try {
    const f = fixture();
    const internal = f.git as unknown as {
      withEpochNativeReads<T>(proof: EpochDiscardProof, action: () => Promise<T>): Promise<T>;
      preflightEpochNative(command: string, args: string[], env: NodeJS.ProcessEnv, identity: Identity): NodeJS.ProcessEnv;
    };
    await internal.withEpochNativeReads(f.proof, async () => {
      const runner = internal.preflightEpochNative("/usr/bin/git", ["version"], {}, "command");
      const worker = internal.preflightEpochNative("/usr/bin/git", ["version"], {}, "worker_pat");
      assert.equal(runner.TMPDIR, process.env.UZI_RUNNER_TMPDIR);
      assert.equal(worker.TMPDIR, process.env.TMPDIR);
      assert.notEqual(runner.TMPDIR, worker.TMPDIR);
    });
    for (const identity of ["command", "worker_pat"] as const) {
      const launched = fixture(false, identity);
      const result = launched.invoke();
      await launched.launched;
      launched.gone(); launched.exit(); launched.close();
      await result;
      launched.scope.assertSettled();
    }
  } finally {
    if (old === undefined) delete process.env.UZI_RUNNER_TMPDIR;
    else process.env.UZI_RUNNER_TMPDIR = old;
  }
});
it("admits only source/bare inventories and bounded worker trusted closure command forms", async () => {
  const f = fixture();
  const internal = f.git as unknown as {
    preflightEpochNative(command: string, args: string[], env: NodeJS.ProcessEnv, identity: Identity): NodeJS.ProcessEnv;
  };
  const bare = path.join(process.env.TMPDIR!, "bare");
  const sha = "a".repeat(40);
  const inventory = ["cat-file", "--batch-all-objects", "--batch-check=%(objectname)"];
  const closure = ["rev-list", "--objects", "--no-object-names", sha];
  await f.internal.withEpochNativeReads(f.proof, async () => {
    const idx = path.join(bare, "objects/pack/pack-" + sha + ".idx");
    const verify = ["-C", bare, "verify-pack", "--", idx];
    assert.throws(() => internal.preflightEpochNative("/usr/bin/git", verify, {}, "worker_pat"), /unaudited/);
    const context = f.git["epochNative"].getStore()!;
    context.allowedIndices!.add(idx);
    internal.preflightEpochNative("/usr/bin/git", verify, {}, "worker_pat");
    assert.throws(() => internal.preflightEpochNative("/usr/bin/git",
      ["-C", bare, "verify-pack", "--stat-only", "--", idx], {}, "worker_pat"), /unaudited/);
    context.allowedIndices!.delete(idx);
    assert.throws(() => internal.preflightEpochNative("/usr/bin/git", verify, {}, "worker_pat"), /unaudited/);
    for (const [cwd, args, identity] of [
      [f.target, inventory, "command"], [bare, inventory, "worker_pat"], [bare, closure, "worker_pat"],
      [bare, [...closure, ...Array<string>(127).fill(sha)], "worker_pat"],
    ] as [string, string[], Identity][]) {
      const env = internal.preflightEpochNative("/usr/bin/git", ["-C", cwd, ...args], {}, identity);
      assert.equal(env.GIT_NO_LAZY_FETCH, "1");
      assert.equal(env.GIT_NO_REPLACE_OBJECTS, "1");
    }
    for (const [cwd, args, identity] of [
      [f.target, inventory, "worker_pat"],
      [bare, inventory, "command"], [f.target, closure, "command"],
      [bare, closure, "command"], [f.target, closure, "worker_pat"],
      [f.target, [...inventory, "--filters"], "command"],
      [f.target, [...inventory, "--textconv"], "command"],
      [f.target, ["cat-file", "--batch-check=%(objectname)"], "command"],
      [bare, [...closure, "--all"], "worker_pat"],
      [bare, closure.slice(0, -1), "worker_pat"],
      [bare, [...closure, ...Array<string>(128).fill(sha)], "worker_pat"],
      [bare, [...closure.slice(0, -1), "A".repeat(40)], "worker_pat"],
      [bare, [...closure.slice(0, -1), sha + "0"], "worker_pat"],
    ] as [string, string[], Identity][])
      assert.throws(() => internal.preflightEpochNative("/usr/bin/git", ["-C", cwd, ...args], {}, identity), /unaudited/);
  });
  assert.equal(f.inherited(), 0);
});

it("exit starts group cleanup before close; success waits for descendant absence and IO", async () => {
  const f = fixture();
  let done = false;
  const result = f.invoke().then(value => { done = true; return value; });
  await f.launched;
  f.child.stdout.write("git version fixture");
  f.exit();
  assert.equal(f.killed(), 0);
  f.close();
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(done, false);
  assert.equal(f.pending(), 1);
  f.gone();
  assert.equal((await result).stdout, "git version fixture");
  f.scope.assertSettled();
});

it("late close and later absence never clear a stopped ticket", async () => {
  const f = fixture();
  const result = f.invoke();
  await f.launched;
  f.exit();
  await assert.rejects(result, /unconfirmed/);
  assert.equal(f.stopped(), true);
  f.gone(); f.close();
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(f.pending(), 1);
});

it("direct success and EOF do not prove group absence", async () => {
  const f = fixture();
  const result = f.invoke();
  await f.launched;
  f.exit(); f.close();
  await assert.rejects(result, /unconfirmed/);
  assert.equal(f.pending(), 1);
  assert.equal(f.stopped(), true);
});

for (const cancellation of ["abort", "timeout"] as const) {
  it(`${cancellation} still joins the group after leader exit`, async () => {
    const f = fixture();
    const result = f.invoke(cancellation === "timeout" ? { timeout: 10 } : {});
    await f.launched;
    f.exit();
    if (cancellation === "abort") f.controller.abort(new Error("fixture abort"));
    else await new Promise<void>(resolve => setTimeout(resolve, 15));
    assert.equal(f.pending(), 1);
    assert.equal(f.killed(), cancellation === "timeout" ? 1 : 0);
    f.gone(); f.close();
    await assert.rejects(result, cancellation === "abort" ? /fixture abort/ : /timed out/);
    f.scope.assertSettled();
  });
}

it("nonzero joined refusal settles its process ticket", async () => {
  const f = fixture();
  const result = f.invoke();
  await f.launched;
  f.gone(); f.exit(1); f.close(1);
  await assert.rejects(result, error => (error as { code: number }).code === 1);
  f.scope.assertSettled();
});

it("error starts cleanup; unknown group identity remains sticky after close", async () => {
  const f = fixture();
  Object.assign(f.child, { pid: undefined });
  const result = f.invoke();
  await f.launched;
  f.child.emit("error", new Error("launch error"));
  await assert.rejects(result, /group unknown/);
  f.close();
  assert.equal(f.pending(), 1);
  assert.equal(f.stopped(), true);
});

it("output join failure retains ticket even with group absent and child closed", async () => {
  const f = fixture();
  const result = f.invoke();
  await f.launched;
  f.gone();
  f.child.stdout.destroy(new Error("broken pipe"));
  f.exit(); f.close();
  await assert.rejects(result, /unconfirmed/);
  assert.equal(f.stopped(), true);
  assert.equal(f.pending(), 1);
});

it("maxBuffer cancellation preserves error code after positive group join", async () => {
  const f = fixture();
  const result = f.invoke({ maxBuffer: 2 });
  await f.launched;
  f.child.stdout.write("oversize");
  assert.equal(f.killed(), 1);
  f.gone(); f.exit(); f.close();
  await assert.rejects(result, error => (error as { code: string }).code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER");
  f.scope.assertSettled();
});

it("metered and ordinary native selection use the same scoped collector", async t => {
  const f = fixture();
  t.mock.method(MaintenanceResources, "active", () => true);
  const consumed: string[] = [];
  t.mock.method(MaintenanceResources, "consumeIfActive", (category: string) => { consumed.push(category); });
  const result = f.invoke();
  await f.launched;
  f.child.stdout.write("version");
  f.gone(); f.exit(); f.close();
  await result;
  assert.ok(consumed.includes("launches"));
  assert.ok(consumed.includes("metadataBytes"));
  f.scope.assertSettled();
});

it("ordinary calls inherit nothing; unsupported executable, env and wrapper chains are refused", async () => {
  const f = fixture();
  assert.match((await f.internal.execScoped("/usr/bin/git", ["version"], { env: {} })).stdout, /git version/);
  assert.equal(f.inherited(), 0);
  for (const [command, args, env] of [
    ["/usr/bin/git", ["-C", f.target + "-other", "config", "--local", "--null", "--list"], {}],
    ["/usr/bin/git", ["-C", f.target, "rev-parse", "--is-bare-repository"], {}],
    ["/usr/bin/git", ["-C", f.target, "fetch", "origin"], {}],
    ["/usr/bin/git", ["-C", f.target, "commit", "-m", "forbidden"], {}],
    ["/usr/bin/git", ["-C", f.target, "maintenance", "run"], {}],
    ["/usr/bin/git", ["version"], { GIT_CONFIG_COUNT: "1", GIT_CONFIG_KEY_0: "http.extraHeader", GIT_CONFIG_VALUE_0: "credential" }],
    ["/usr/bin/git", ["status"], {}],
    ["/usr/bin/git", ["-c", "core.fsmonitor=evil", "status"], {}],
    ["/usr/bin/git", ["version"], { LD_PRELOAD: "/tmp/evil" }],
    ["/bin/setpriv", ["/bin/rm", "-rf", "--", f.target], {}],
  ] as [string, string[], NodeJS.ProcessEnv][]) {
    await assert.rejects(f.internal.withEpochNativeReads(f.proof,
      () => f.internal.execScoped(command, args, { env })), /unaudited/);
  }
  assert.equal(f.inherited(), 0);
  await assert.rejects(async () => f.internal.withEpochNativeReads({} as EpochDiscardProof,
    async () => undefined), /unregistered/);
});

it("boundary overlap is rejected in both directions before inheritance", async () => {
  const f = fixture();
  const signal = new AbortController().signal;
  const spawnBoundary = async () => { throw new Error("must not spawn"); };
  await assert.rejects(f.git.withBoundaryProcessSpawner(spawnBoundary, signal, async () =>
    f.internal.withEpochNativeReads(f.proof, async () => undefined)), /overlap/);
  await assert.rejects(f.internal.withEpochNativeReads(f.proof, async () =>
    f.git.withBoundaryProcessSpawner(spawnBoundary, signal, async () => undefined)), /overlap/);
  await assert.rejects(f.internal.withEpochNativeReads(f.proof, async () =>
    f.git.withCredentialFreeCapture({} as never, async () => undefined)), /overlap/);
  const capture = createCaptureContext(signal, Date.now() + 10_000, spawnBoundary, () => true);
  await assert.rejects(f.git.withCredentialFreeCapture(capture, async () =>
    f.internal.withEpochNativeReads(f.proof, async () => undefined)), /overlap/);
  assert.equal(f.inherited(), 0);
});

it("in-process delete and authority settlement have a separate joined ticket", async () => {
  const f = fixture();
  await fs.mkdir(f.target, { recursive: true });
  await fs.writeFile(path.join(f.target, "data"), "fixture");
  let release!: () => void;
  const barrier = new Promise<void>(resolve => { release = resolve; });
  let settling!: () => void;
  const atSettlement = new Promise<void>(resolve => { settling = resolve; });
  f.internal.beginDestructive = async () => ({
    async settle(result) { assert.equal(result.ok, true); settling(); await barrier; },
  });
  const result = f.internal.withEpochNativeReads(f.proof, () => f.internal.runRunnerUidDelete(f.target));
  await atSettlement;
  assert.equal(f.pending(), 1);
  assert.equal(f.internal.unsettledDeletes.get(f.target), 1);
  await assert.rejects(fs.stat(f.target), { code: "ENOENT" });
  release(); await result;
  f.scope.assertSettled();
  assert.equal(f.internal.unsettledDeletes.size, 0);
});

it("authority settlement failure remains stopped without a second settlement", async () => {
  const f = fixture();
  let calls = 0;
  f.internal.beginDestructive = async () => ({
    async settle() { calls++; throw new Error("settlement failure"); },
  });
  await assert.rejects(f.internal.withEpochNativeReads(f.proof,
    () => f.internal.runRunnerUidDelete(f.target)), /settlement failure/);
  assert.equal(calls, 1);
  assert.equal(f.stopped(), true);
  assert.equal(f.pending(), 1);
  assert.equal(f.internal.unsettledDeletes.get(f.target), 1);
});

it("split-uid launch rejects an unwrapped runner identity before inheritance", async () => {
  const previous = process.env.UZI_UID_SPLIT;
  process.env.UZI_UID_SPLIT = "1";
  try {
    const f = fixture();
    delete (f.internal as Partial<PrivateCache>).epochNativeTestDependencies;
    await assert.rejects(f.internal.withEpochNativeReads(f.proof, () =>
      f.internal.execScoped("/usr/bin/git", ["version"], { env: {} }, "command")), /identity mismatch/);
    assert.equal(f.inherited(), 0);
    f.scope.assertSettled();
  } finally {
    if (previous === undefined) delete process.env.UZI_UID_SPLIT;
    else process.env.UZI_UID_SPLIT = previous;
  }
});

it("probe uncertainty is sticky even if a later probe could report absence", async () => {
  const f = fixture();
  f.internal.epochNativeTestDependencies.alive = () => { throw new Error("probe unknown"); };
  const result = f.invoke();
  await f.launched;
  f.exit();
  await assert.rejects(result, /probe unknown/);
  f.gone(); f.close();
  assert.equal(f.pending(), 1);
  assert.equal(f.stopped(), true);
});

for (const outcome of ["success", "refusal", "unknown"] as const) {
  it(`wrapped runner delete ${outcome} settles only after group and authority joins`, async () => {
    const previous = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    try {
      const f = fixture(true);
      const settlements: boolean[] = [];
      f.internal.beginDestructive = async () => ({
        async settle(result) { settlements.push(result.ok); },
      });
      const result = f.internal.withEpochNativeReads(f.proof,
        () => f.internal.runRunnerUidDelete(f.target));
      await f.launched;
      f.exit(outcome === "refusal" ? 1 : 0);
      assert.equal(f.killed(), 0);
      f.close(outcome === "refusal" ? 1 : 0);
      assert.deepEqual(settlements, []);
      assert.equal(f.pending(), 2);
      if (outcome === "unknown") {
        await assert.rejects(result, /unconfirmed/);
        f.gone(); f.close();
        assert.deepEqual(settlements, []);
        assert.equal(f.pending(), 2);
        assert.equal(f.internal.unsettledDeletes.get(f.target), 1);
        assert.equal(f.stopped(), true);
      } else {
        f.gone();
        if (outcome === "refusal") await assert.rejects(result, /failed/);
        else await result;
        assert.deepEqual(settlements, [outcome === "success"]);
        assert.equal(f.internal.unsettledDeletes.size, 0);
        f.scope.assertSettled();
      }
    } finally {
      if (previous === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = previous;
    }
  });
}

it("abort before leader exit kills and waits for direct IO and group joins", async () => {
  const f = fixture();
  const result = f.invoke();
  await f.launched;
  f.controller.abort(new Error("early abort"));
  assert.equal(f.killed(), 1);
  f.gone();
  assert.equal(f.pending(), 1);
  f.exit(); f.close();
  await assert.rejects(result, /early abort/);
  f.scope.assertSettled();
});

it("descriptor inheritance failure retains the pre-launch ticket and stops", async () => {
  const f = fixture();
  f.scope.inheritStdio = () => { throw new Error("descriptor unknown"); };
  await assert.rejects(f.invoke(), /descriptor unknown/);
  assert.equal(f.pending(), 1);
  assert.equal(f.stopped(), true);
});

it("unconfirmed group signaling never settles on a later close", async () => {
  const f = fixture();
  f.internal.epochNativeTestDependencies.kill = () => { throw new Error("signal denied"); };
  const result = f.invoke();
  await f.launched;
  f.exit();
  await assert.rejects(result, /signal denied/);
  f.gone(); f.close();
  assert.equal(f.pending(), 1);
  assert.equal(f.stopped(), true);
});

for (const stream of ["stdout", "stderr"] as const) {
  it("quiet verify-pack rejects nonempty " + stream + " after joined exit zero", async () => {
    const f = fixture();
    const bare = path.join(process.env.TMPDIR!, "bare");
    const idx = path.join(bare, "objects/pack/pack-" + "a".repeat(40) + ".idx");
    const result = f.internal.withEpochNativeReads(f.proof, async () => {
      f.git["epochNative"].getStore()!.allowedIndices!.add(idx);
      return f.git["runGitWithEnv"](bare, ["verify-pack", "--", idx], {});
    });
    await f.launched;
    f.child[stream].write("unexpected diagnostic");
    f.gone(); f.exit(); f.close();
    await assert.rejects(result, /quiet pack verification emitted output/);
    f.scope.assertSettled();
  });
}
