import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { getEventListeners } from "node:events";
import { PassThrough, Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, type BoundaryProcessSpawner, type GitCacheOptions } from "../src/git.js";
import { nullLogger } from "./helpers.js";
import { CheckpointSoftDeadlineError, CheckpointUnsafeCleanupError } from "../src/checkpoint-stop.js";

const TIP = "a".repeat(40), FLOOR = "b".repeat(40), DEFAULT = "c".repeat(40);
const TREE = "d".repeat(40), BLOB = "e".repeat(40);
const tick = () => new Promise<void>(resolve => setImmediate(resolve));
/** Track the actual production timers without changing their firing semantics. */
function trackTimers(t: TestContext) {
  const active = new Set<ReturnType<typeof setTimeout>>();
  const schedule = globalThis.setTimeout, clear = globalThis.clearTimeout;
  t.mock.method(globalThis, "setTimeout", ((callback: (...args: unknown[]) => void, ms?: number, ...args: unknown[]) => {
    const timer = schedule(() => { active.delete(timer); callback(...args); }, ms);
    active.add(timer);
    return timer;
  }) as typeof setTimeout);
  t.mock.method(globalThis, "clearTimeout", (timer: ReturnType<typeof setTimeout>) => {
    active.delete(timer); return clear(timer);
  });
  return active;
}
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
const metadata = [
  TIP + "\n",
  TIP + "\n" + TREE + "\n" + BLOB + " private\n",
  BLOB + " private\n",
  TIP + " commit\n" + TREE + " tree\n" + BLOB + " blob\n",
];
function cache(bounds?: GitCacheOptions["checkpointPackBounds"]) {
  const git = new GitCache("/unused", nullLogger(), undefined, { checkpointPackBounds: bounds });
  Object.assign(git, {
    captureCheckpointDefault: async () => DEFAULT,
    trackingTip: async () => TIP,
    scratchPublicationPreflight: async () => TIP,
    checkpointExcludeRef: async () => FLOOR,
    revParse: async (_bare: string, ref: string) => ref.startsWith(DEFAULT) ? DEFAULT : FLOOR,
    validateCheckpointFloor: async () => undefined,
    buildWorkflowOverlay: async () => null,
    buildCheckpointChain: async () => null,
  });
  return git;
}
function controlled(target: number, cap = false) {
  const entered = deferred(), cancelled = deferred(), release = deferred();
  const calls: string[][] = [], pipes: PassThrough[] = [];
  let stage = 0, live = 0;
  const spawner: BoundaryProcessSpawner = async request => {
    const args = [...request.argv];
    calls.push(args);
    const stdout = new PassThrough(), stderr = new PassThrough(), stdin = new PassThrough();
    stdin.resume();
    pipes.push(stdout, stderr, stdin);
    const index = stage++;
    live++;
    const completed = (async () => {
      if (index === target) {
        entered.resolve();
        if (cap) stdout.write(metadata[index]);
        await release.promise;
      } else stdout.write(args.includes("pack-objects") ? "PACK" : metadata[index] ?? "");
      stdout.end(); stderr.end(); stdin.destroy();
      live--;
      return { code: 0 };
    })();
    return { stdout, stderr, stdin, completed, cancel: async () => {
      cancelled.resolve();
      stdout.destroy(); stderr.destroy(); stdin.destroy();
      await release.promise;
      await completed;
    } };
  };
  return { spawner, entered, cancelled, release, calls, pipes, get live() { return live; } };
}
for (const stop of ["signal", "clock"] as const) {
  it(`soft ${stop} before entry remains cooperative without launching a producer`, async () => {
    const git = cache(), soft = new AbortController();
    let launches = 0;
    Object.assign(git, { spawnGit: async () => { launches++; throw new Error("unexpected launch"); } });
    if (stop === "signal") soft.abort();
    await assert.rejects(git.withBoundaryProcessSpawner(
      async () => { throw new Error("unexpected boundary launch"); }, new AbortController().signal,
      () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
        undefined, undefined, { signal: AbortSignal.any([soft.signal]) }),
      { softSignal: soft.signal, softDeadlineAt: 100, now: () => stop === "clock" ? 100 : 0 },
    ), CheckpointSoftDeadlineError);
    assert.equal(launches, 0);
  });

  it(`soft ${stop} during preparation joins disposal and forbids fallback`, async () => {
    const git = cache(), soft = new AbortController(), owner = controlled(0);
    let now = 0, returned = false;
    const pending = git.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
      () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
        undefined, undefined, { signal: AbortSignal.any([soft.signal]) }),
      { softSignal: soft.signal, softDeadlineAt: 60_000, now: () => now });
    const rejected = assert.rejects(pending, CheckpointSoftDeadlineError).then(() => { returned = true; });
    try {
      await owner.entered.promise;
      if (stop === "signal") soft.abort();
      else { now = 60_000; owner.pipes[0]!.end(); }
      await tick();
      assert.equal(returned, false);
      assert.equal(owner.calls.length, 1);
      owner.release.resolve();
      await rejected;
      assert.equal(owner.live, 0);
      assert.equal(owner.calls.length, 1);
    } finally { owner.release.resolve(); await rejected; }
  });

  it(`replay retains cooperative ${stop} after leaving its boundary scope`, async () => {
    const git = cache(), soft = new AbortController();
    let now = 0, preparations = 0, launches = 0;
    Object.assign(git, {
      prepareCheckpointObjects: async () => { preparations++; return Buffer.from(TIP + "\n"); },
      spawnGit: async () => {
        launches++;
        return { stdout: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {} };
      },
    });
    const packed = await git.withBoundaryProcessSpawner(
      async () => { throw new Error("unexpected boundary launch"); }, new AbortController().signal,
      () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
        undefined, undefined, { signal: AbortSignal.any([soft.signal]) }),
      { softSignal: soft.signal, softDeadlineAt: 60_000, now: () => now });
    assert.ok(packed);
    for await (const _chunk of packed.pack) { /* drain initial producer */ }
    assert.equal(await packed.exited, 0);
    if (stop === "signal") soft.abort();
    else now = 60_000;
    await assert.rejects(packed.replay(), CheckpointSoftDeadlineError);
    assert.equal(preparations, 1);
    assert.equal(launches, 1);
  });
}

it("soft interruption during a producer retains typed failure and joins disposal", async () => {
  const git = cache(), soft = new AbortController(), owner = controlled(0);
  Object.assign(git, { prepareCheckpointObjects: async () => Buffer.from(TIP + "\n") });
  const packed = await git.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
    () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
      undefined, undefined, { signal: AbortSignal.any([soft.signal]) }), { softSignal: soft.signal });
  assert.ok(packed);
  let returned = false;
  const drain = (async () => { for await (const _chunk of packed.pack) { /* consume producer */ } })();
  const rejected = assert.rejects(drain, CheckpointSoftDeadlineError).then(() => { returned = true; });
  try {
    await owner.entered.promise;
    soft.abort();
    await owner.cancelled.promise;
    await tick();
    assert.equal(returned, false, "stream failure waits for verified disposal");
    owner.release.resolve();
    await rejected;
    assert.notEqual(await packed.exited, 0);
    assert.equal(owner.live, 0);
    assert.equal(owner.calls.length, 1, "no replacement producer");
  } finally { owner.release.resolve(); await rejected; }
});

it("an independent caller abort stays hard even with a cooperative boundary", async () => {
  const git = cache(), soft = new AbortController(), hard = new AbortController();
  const reason = new DOMException("independent caller stopped", "AbortError");
  let launches = 0;
  Object.assign(git, { spawnGit: async () => { launches++; throw new Error("unexpected launch"); } });
  hard.abort(reason);
  soft.abort();
  await assert.rejects(git.withBoundaryProcessSpawner(
    async () => { throw new Error("unexpected boundary launch"); }, new AbortController().signal,
    () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
      undefined, undefined, { signal: AbortSignal.any([hard.signal, soft.signal]) }),
    { softSignal: soft.signal },
  ), error => error === reason);
  assert.equal(launches, 0);
});

it("a guard's combined caller abort remains cooperative without starting preparation", async () => {
  const git = cache(), soft = new AbortController();
  const signal = AbortSignal.any([soft.signal]);
  let launches = 0;
  Object.assign(git, { spawnGit: async () => { launches++; throw new Error("unexpected launch"); } });
  await assert.rejects(git.withBoundaryProcessSpawner(
    async () => { throw new Error("unexpected boundary launch"); }, new AbortController().signal,
    () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
      async () => { soft.abort(); signal.throwIfAborted(); }, undefined, { signal }),
    { softSignal: soft.signal },
  ), CheckpointSoftDeadlineError);
  assert.equal(launches, 0);
});

it("soft interruption cannot hide unverified preparation cleanup", async () => {
  const git = cache(), soft = new AbortController();
  const unsafe = new CheckpointUnsafeCleanupError("unverified disposal");
  const owner = controlled(0);
  const spawner: BoundaryProcessSpawner = async request => {
    const child = await owner.spawner(request);
    return { ...child, cancel: async () => { await child.cancel(); throw unsafe; } };
  };
  const pending = git.withBoundaryProcessSpawner(spawner, new AbortController().signal,
    () => git.checkpointPack("bare", "task"), { softSignal: soft.signal });
  const rejected = assert.rejects(pending, CheckpointUnsafeCleanupError);
  try {
    await owner.entered.promise;
    soft.abort();
    owner.release.resolve();
    await rejected;
    assert.equal(owner.calls.length, 1);
    assert.equal(owner.live, 0);
  } finally { owner.release.resolve(); await rejected; }
});

for (const stage of ["row", "classification"] as const) {
  it(`a genuine ${stage} failure survives a cap expiring during deferred disposal`, async t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const git = cache(), owner = controlled(stage === "row" ? 0 : 3);
    const pending = git.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
      () => git.checkpointPack("bare", "task"));
    const observed = pending.then(value => ({ value }), error => ({ error }));
    try {
      await owner.entered.promise;
      owner.pipes.at(-3)!.write(stage === "row" ? "invalid row\n" : "invalid classification\n");
      await owner.cancelled.promise;
      t.mock.timers.tick(5000);
      owner.release.resolve();
      const result = await observed;
      assert.ok("error" in result, "a metadata failure must forbid legacy fallback");
      assert.match(String(result.error), stage === "row" ? /invalid checkpoint object row/ : /invalid checkpoint classification/);
      assert.equal(owner.calls.some(args => args.includes("pack-objects")), false);
      assert.equal(owner.live, 0);
    } finally { owner.release.resolve(); await observed; }
  });
}

for (const pinned of [false, true]) for (const replay of [false, true]) {
  for (const stop of ["abort", "expiry", "unsafe"] as const) {
    it(`producer handoff joins verified disposal: pinned=${pinned} replay=${replay} stop=${stop}`, async t => {
      const timers = trackTimers(t);
      let now = 1000, launches = 0, returned = false;
      t.mock.method(Date, "now", () => now);
      const git = cache(), parent = new AbortController(), owner = controlled(replay ? 1 : 0);
      Object.assign(git, { prepareCheckpointObjects: async () => Buffer.from(TIP + "\n") });
      const spawn = git["spawnGit"].bind(git);
      const signals: AbortSignal[] = [];
      Object.assign(git, { spawnGit: async (...args: Parameters<typeof spawn>) => {
        const child = await spawn(...args);
        signals.push(git["recoveryOperation"]()!.signal);
        if (++launches === (replay ? 2 : 1)) {
          if (stop === "expiry") now = 61000;
          else parent.abort();
        }
        return child;
      } });
      const spawner: BoundaryProcessSpawner = async request => {
        const child = await owner.spawner(request);
        return { ...child, cancel: async () => {
          await child.cancel();
          if (stop === "unsafe") throw new Error("unverified handoff cancellation");
        } };
      };
      const pending = git.withBoundaryProcessSpawner(spawner, new AbortController().signal, async () => {
        const packed = await git.checkpointPack("bare", "task", undefined,
          pinned ? { tipSha: TIP, excludeSha: FLOOR, defaultBlobSha: DEFAULT } : undefined,
          undefined, undefined, undefined, undefined, { signal: parent.signal, deadline: 61000 });
        if (replay) {
          assert.ok(packed);
          for await (const _chunk of packed.pack) { /* drain initial producer */ }
          assert.equal(await packed.exited, 0);
          return packed.replay();
        }
        return packed;
      });
      const observed = pending.then(value => ({ value }), error => ({ error })).finally(() => { returned = true; });
      try {
        await owner.entered.promise;
        assert.equal(await Promise.race([
          owner.cancelled.promise.then(() => "cancelled"), observed.then(() => "returned"),
        ]), "cancelled", "a handoff failure must cancel its producer before returning");
        await tick();
        assert.equal(returned, false, "handoff rejection must wait for verified disposal and reap");
        owner.release.resolve();
        const result = await observed;
        assert.ok("error" in result);
        if (stop === "unsafe") assert.ok(result.error instanceof CheckpointUnsafeCleanupError);
        else assert.match(String(result.error), /abort|deadline/i);
        assert.equal(owner.live, 0);
        assert.equal(timers.size, 0);
        assert.ok(signals.every(signal => getEventListeners(signal, "abort").length === 0));
        assert.equal(getEventListeners(parent.signal, "abort").length, 0);
        assert.ok(owner.pipes.every(pipe => pipe.destroyed || pipe.readableEnded));
      } finally { owner.release.resolve(); await observed; }
    });
  }
}

const stages = ["commits", "all objects", "private objects", "classification", "selection"] as const;
for (const stage of stages) for (const stop of ["cancel", "deadline"] as const) {
  it(`parent ${stop} at ${stage} joins preparation and forbids every producer`, async t => {
    const timers = trackTimers(t);
    let now = 1_000;
    t.mock.method(Date, "now", () => now);
    const git = cache(), parent = new AbortController();
    const owner = controlled(Math.min(stages.indexOf(stage), 3));
    const spawn = git["spawnGit"].bind(git);
    const signals: AbortSignal[] = [];
    // Selection has already accepted every classifier row, but disposal is still outstanding.
    Object.assign(git, { spawnGit: async (...args: Parameters<typeof spawn>) => {
      signals.push(git["recoveryOperation"]()!.signal);
      const child = await spawn(...args);
      if (stage === "selection" && args[1].includes("cat-file")) {
        const raw = owner.pipes.at(-3)!;
        raw.end(metadata[3]);
      }
      return child;
    } });
    let returned = false;
    const pending = git.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
      () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
        undefined, undefined, { defaultBranch: "release", signal: parent.signal, deadline: 61_000 }));
    const rejected = assert.rejects(pending, /abort|deadline/i).then(() => { returned = true; });
    try {
      await owner.entered.promise;
      await tick();
      if (stop === "cancel") parent.abort();
      else { now = 61_000; owner.pipes.at(-3)!.end(); }
      await tick();
      assert.equal(returned, false, "parent result must join deferred disposal");
      assert.equal(owner.calls.some(args => args.includes("pack-objects")), false);
      owner.release.resolve();
      await rejected;
      assert.equal(owner.live, 0);
      assert.equal(timers.size, 0, "preparation and absolute deadline timers are cleared");
      assert.ok(signals.every(signal => getEventListeners(signal, "abort").length === 0), "child abort listeners are removed");
      assert.ok(owner.pipes.every(pipe => pipe.destroyed || pipe.readableEnded));
      assert.equal(getEventListeners(parent.signal, "abort").length, 0);
    } finally { owner.release.resolve(); await rejected; }
  });
}

for (const [bound, value] of [["records", 0], ["bytes", 1], ["lineBytes", 1], ["durationMs", 0]] as const) {
  for (const stop of ["live", "cancel", "deadline"] as const) {
    it(`${bound} cap waits for disposal before ${stop === "live" ? "legacy fallback" : stop + " refusal"}`, async t => {
      const timers = trackTimers(t);
      let now = 1_000;
      t.mock.method(Date, "now", () => now);
      const git = cache({ [bound]: value }), parent = new AbortController(), owner = controlled(0, bound !== "durationMs");
      let returned = false;
      const pending = git.withBoundaryProcessSpawner(owner.spawner, new AbortController().signal,
        () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
          undefined, undefined, { defaultBranch: "release", signal: parent.signal, deadline: 61_000 }));
      const observed = pending.then(value => ({ value }), error => ({ error })).finally(() => { returned = true; });
      try {
        await owner.cancelled.promise;
        await tick(); // Let the cap catch path run while disposal is still deliberately blocked.
        assert.equal(returned, false);
        assert.equal(owner.calls.length, 1, "no fallback before disposal");
        if (stop === "cancel") parent.abort();
        if (stop === "deadline") now = 61_000;
        owner.release.resolve();
        const result = await observed;
        if (stop === "live") {
          assert.ok("value" in result && result.value);
          const packed = result.value;
          for await (const _chunk of packed.pack) { /* drain the sole legacy producer */ }
          assert.equal(await packed.exited, 0);
          assert.equal(owner.calls.length, 2);
          assert.ok(owner.calls[1]!.includes("--revs"));
        } else {
          assert.ok("error" in result);
          assert.match(String(result.error), /abort|deadline/i);
          assert.equal(owner.calls.length, 1);
        }
        assert.equal(owner.live, 0);
        assert.equal(timers.size, 0);
      } finally { owner.release.resolve(); await observed; }
    });
  }
}

for (const phase of ["captureCheckpointDefault", "checkpointExcludeRef", "buildWorkflowOverlay",
  "buildCheckpointChain", "scratchPublicationPreflight", "guard"] as const) {
  for (const stop of ["cancel", "deadline"] as const) {
    it(`${stop} during ${phase} cannot launch enumeration or fallback`, async t => {
      let now = 1_000;
      t.mock.method(Date, "now", () => now);
      const git = cache(), parent = new AbortController();
      let launches = 0;
      const halt = async () => { if (stop === "cancel") parent.abort(); else now = 61_000; return null; };
      Object.assign(git, { spawnGit: async () => { launches++; throw new Error("unexpected launch"); } });
      if (phase !== "guard") Object.assign(git, { [phase]: halt });
      await assert.rejects(git.checkpointPack("bare", "task", { defaultBranch: "overlay-fallback" },
        undefined, undefined, undefined, phase === "guard" ? async () => { await halt(); } : undefined,
        [FLOOR], { defaultBranch: "release", signal: parent.signal, deadline: 61_000 }));
      assert.equal(launches, 0);
    });
  }
}

for (const scenario of [
  { name: "no original recovery or deadline", expected: [undefined, undefined] },
  { name: "ambient infinite recovery", parentDeadline: Infinity, expected: [600_000, 600_000] },
  { name: "ambient finite recovery", parentDeadline: 61_000, expected: [60_000, 59_000] },
  { name: "explicit deadline beyond ten minutes", deadline: 901_000, expected: [900_000, 899_000] },
  { name: "boundary soft deadline", softDeadline: 21_000, expected: [20_000, 19_000] },
  { name: "explicit and boundary soft deadlines", deadline: 901_000, softDeadline: 21_000, expected: [20_000, 19_000] },
]) {
  it(`producer timeout preserves ${scenario.name} on initial pack and replay`, async t => {
    let now = 1_000;
    t.mock.method(Date, "now", () => now);
    const timers = trackTimers(t);
    const git = cache(), owner = controlled(-1), parent = new AbortController();
    Object.assign(git, { prepareCheckpointObjects: async () => Buffer.from(TIP + "\n") });
    const timeouts: (number | undefined)[] = [], present: boolean[] = [];
    const spawner: BoundaryProcessSpawner = async request => {
      assert.ok(request.argv.includes("pack-objects"));
      timeouts.push(request.timeoutMs);
      present.push(Object.hasOwn(request, "timeoutMs"));
      return owner.spawner(request);
    };
    const open = () => git.withBoundaryProcessSpawner(spawner, new AbortController().signal,
      () => git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
        undefined, undefined, { defaultBranch: "release", deadline: scenario.deadline }),
      { softDeadlineAt: scenario.softDeadline, now: () => now });
    const packed = await (scenario.parentDeadline === undefined ? open()
      : git.withRecoveryOperation(parent.signal, scenario.parentDeadline, open));
    assert.ok(packed);
    try {
      for await (const _chunk of packed.pack) { /* drain the actual spawnGit output */ }
      assert.equal(await packed.exited, 0);
      now = 2_000;
      const replay = await packed.replay();
      try {
        for await (const _chunk of replay.pack) { /* join replay output before exit */ }
        assert.equal(await replay.exited, 0);
      } finally { replay.cancel(); replay.pack.destroy(); await replay.exited; }
      assert.deepEqual(timeouts, scenario.expected);
      assert.deepEqual(present, scenario.expected.map(timeout => timeout !== undefined));
      await tick();
      assert.equal(owner.live, 0);
      assert.ok(owner.pipes.every(pipe => pipe.destroyed));
      assert.equal(timers.size, 0);
      assert.equal(getEventListeners(parent.signal, "abort").length, 0);
    } finally { packed.cancel(); packed.pack.destroy(); await packed.exited; }
  });
}

it("replay retains the original absolute deadline and captured range", async t => {
  let now = 1_000;
  t.mock.method(Date, "now", () => now);
  const git = cache();
  const deadlines: number[] = [], inputs: unknown[] = [];
  Object.assign(git, {
    prepareCheckpointObjects: async () => Buffer.from(TIP + "\n"),
    spawnGit: async (_bare: string, _args: string[], input: unknown) => {
      deadlines.push(git["recoveryOperation"]()!.deadline); inputs.push(input);
      return { stdout: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {} };
    },
  });
  const packed = await git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
    undefined, undefined, { defaultBranch: "release", deadline: 61_000 });
  assert.ok(packed);
  now = 60_000;
  await packed.replay();
  assert.deepEqual(deadlines, [61_000, 61_000]);
  assert.deepEqual(inputs, [Buffer.from(TIP + "\n"), Buffer.from(TIP + "\n")]);
  now = 61_000;
  await assert.rejects(packed.replay(), /deadline/);
  assert.equal(deadlines.length, 2);
});

for (const stop of ["cancel", "deadline"] as const) {
  it(`replay guard ${stop} blocks re-enumeration under the original lifetime`, async t => {
    let now = 1_000, guards = 0, preparations = 0, launches = 0;
    t.mock.method(Date, "now", () => now);
    const git = cache(), parent = new AbortController();
    Object.assign(git, {
      prepareCheckpointObjects: async () => { preparations++; return Buffer.from(TIP + "\n"); },
      spawnGit: async () => {
        launches++;
        return { stdout: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {} };
      },
    });
    const packed = await git.checkpointPack("bare", "task", undefined, undefined, undefined, undefined,
      async () => {
        if (++guards === 2) {
          if (stop === "cancel") parent.abort();
          else now = 61_000;
        }
      }, undefined, { defaultBranch: "release", signal: parent.signal, deadline: 61_000 });
    assert.ok(packed);
    await assert.rejects(packed.replay(), /abort|deadline/i);
    assert.equal(guards, 2);
    assert.equal(preparations, 1);
    assert.equal(launches, 1);
  });
}

it("native preparation cancellation joins child close and removes its abort listener", async t => {
  const timers = trackTimers(t);
  const root = fs.mkdtempSync(path.resolve("../.uzi/scratch/native-preparation-"));
  const git = cache(), parent = new AbortController(), entered = deferred(), release = deferred();
  execFileSync("git", ["init", "--bare", root], { stdio: "pipe" });
  const spawn = git["spawnGit"].bind(git);
  let child: Awaited<ReturnType<typeof spawn>> | undefined;
  let launches = 0, returned = false;
  Object.assign(git, { spawnGit: async () => {
    launches++;
    // Literal inert Git reader blocks on our owned stdin; no candidate payload executes.
    child = await spawn(root, ["cat-file", "--batch-check"], undefined, { keepStdinOpen: true, joinStderr: true });
    const settled = child.settled;
    entered.resolve();
    return { ...child, settled: async () => { await settled(); await release.promise; } };
  } });
  const pending = git.checkpointPack(root, "task", undefined, undefined, undefined, undefined,
    undefined, undefined, { defaultBranch: "release", signal: parent.signal });
  const rejected = assert.rejects(pending, /abort/i).then(() => { returned = true; });
  try {
    await entered.promise; parent.abort();
    await child!.settled();
    assert.ok(child!.child!.signalCode || child!.child!.exitCode !== null);
    await tick();
    assert.equal(returned, false);
    assert.equal(launches, 1);
    release.resolve(); await rejected;
    assert.equal(getEventListeners(parent.signal, "abort").length, 0);
    assert.equal(timers.size, 0, "native timeout and SIGKILL escalation timers were cleared on close");
  } finally { release.resolve(); child?.cancel(); await rejected; fs.rmSync(root, { recursive: true, force: true }); }
});
