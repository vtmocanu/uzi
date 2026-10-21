import { it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Readable } from "node:stream";
import { performance } from "node:perf_hooks";
import { GitCache, type CheckpointRange } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { prepareFrozenHeld, type HeldGit } from "../src/frozen-held.js";
import { nullLogger } from "./helpers.js";

const TIP = "a".repeat(40), FLOOR = "b".repeat(40), DEFAULT = "c".repeat(40);
const policy = { version: 1 as const, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] };
function fixture(defaultBranch?: string) {
  const git = new GitCache("/unused", nullLogger());
  const client = new WorkerClient("http://unused", "test", "test", nullLogger());
  const runner = new RunRunner(client, git, () => ({ executor: { run: async () => ({ branch: "task" }) } }),
    nullLogger(), 1, undefined, { checkpointTestHooks: { retryBackoffMs: [0, 0] } });
  const claim = { repo: { default_branch: defaultBranch }, secrets: {}, last_seq: 0, kind: "issue" } as Parameters<RunRunner["buildFlight"]>[0];
  const initialized = runner["buildFlight"](claim, "test", { run: async () => ({ branch: "task" }) }, undefined, "", []);
  assert.equal(initialized.owedDefaultBranch, defaultBranch?.trim() || undefined, "custody keeps existing semantics");
  const flight = {
    ...initialized,
    runId: "test", runKind: "issue", barePath: "bare",
    checkpointChainForge: false, runLog: nullLogger(), reportedPublishOutcomes: new Set<string>(),
    batcher: { emit() {} },
  } as unknown as Parameters<RunRunner["publishCheckpointOutcome"]>[0];
  Object.assign(runner, { requireTrackingOwned: async () => TIP });
  client.publishCheckpoint = async (_run, _tip, stream) => {
    for await (const _chunk of stream) { /* transport consumes the actual producer */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/task" } };
  };
  return { git, runner, flight };
}

for (const name of ["開発/server", "開発/custom\u00a0", " 開発/custom ", undefined]) {
  for (const overlay of [false, true]) for (const pinned of [false, true]) {
    it(`shared tick/park/pause publication: server=${name ?? "absent"} overlay=${overlay} pinned=${pinned}`, async () => {
      const { git, runner, flight } = fixture(name);
      const signal = new AbortController().signal;
      const range: CheckpointRange = { tipSha: TIP, excludeSha: FLOOR, defaultBlobSha: DEFAULT };
      let calls = 0;
      git.checkpointPack = async (...args) => {
        calls++;
        assert.equal(args[2]?.defaultBranch, overlay ? "untrusted-overlay-fallback" : undefined);
        assert.equal(args[3], pinned ? range : undefined, "preserve the scanned range object including defaultBlobSha");
        assert.equal(args[5], TIP);
        assert.equal(args[8]?.defaultBranch, name, "only the server flight name authorizes omission");
        assert.equal(args[8]?.signal, signal);
        await args[6]?.(TIP);
        return { tipOid: TIP, pack: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {},
          replay: async () => { throw new Error("unexpected replay"); } };
      };
      assert.deepEqual(await runner["publishCheckpointOutcome"](flight, "bare", "task",
        overlay ? { defaultBranch: "untrusted-overlay-fallback" } : undefined, signal, pinned ? range : undefined),
      { published: true });
      assert.equal(calls, 1);
    });
  }

  it(`scan and remediation retain server name ${name ?? "absent"} and exact pinned default`, async () => {
    const { git, runner, flight } = fixture(name);
    const range: CheckpointRange = { tipSha: TIP, excludeSha: FLOOR, defaultBlobSha: DEFAULT };
    const names: unknown[] = [];
    git.resolveCheckpointRange = async (_bare, _branch, opts) => { names.push(opts?.defaultBranch); return range; };
    git.secretScanCheckpointRange = async (_bare, actual) => {
      assert.equal(actual, range);
      return { trusted: true, findings: [] };
    };
    const scanned = await runner["scanCheckpointForPublish"](flight, "bare", "task");
    assert.deepEqual(scanned, { kind: "pinned", range });
    const remediation = await runner["scanRemediationRange"](flight, "bare", "task", TIP);
    assert.equal(remediation.trusted, true);
    if (remediation.trusted) assert.equal(remediation.range, range);
    assert.deepEqual(names, [name, name]);
  });

  it(`held claim server name ${name ?? "absent"} reaches resolver and pinned pack without policy mutation`, async () => {
    const { git, runner, flight } = fixture("must-not-use-flight-fallback");
    const range: CheckpointRange = { tipSha: TIP, excludeSha: FLOOR, ...(name ? { defaultBlobSha: DEFAULT } : {}) };
    const heldPolicy = structuredClone(policy);
    git.resolveCheckpointRange = async (_bare, _branch, opts) => {
      assert.equal(opts?.defaultBranch, name);
      assert.equal(opts?.tipSha, TIP);
      return range;
    };
    git.secretScanCheckpointRange = async (_bare, actual) => {
      assert.equal(actual, range); return { trusted: true, findings: [] };
    };
    git.rangeTouchesWorkflows = async () => "clean";
    let packs = 0;
    git.checkpointPack = async (...args) => {
      packs++;
      assert.equal(args[3], range);
      assert.equal(args[3]?.defaultBlobSha, name ? DEFAULT : undefined);
      return { tipOid: TIP, pack: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {},
        replay: async () => { throw new Error("unexpected replay"); } };
    };
    const claim = { repo: { default_branch: name } } as Parameters<RunRunner["heldPublicationStep"]>[0];
    const record = { branch: "task", sourceSha: TIP, heldPolicy,
      defaultBranch: "fallback-main", defaultIdentity: { ref: "refs/heads/main" } } as unknown as Parameters<RunRunner["heldPublicationStep"]>[2];
    assert.deepEqual(await runner["heldPublicationStep"](claim, flight, record), { pack: Buffer.from("PACK") });
    assert.equal(packs, 1);
    assert.deepEqual(heldPolicy, policy, "the name belongs to the captured range, never FrozenHeldPolicy");
  });
}

for (const expireOnRetry of [false, true]) {
  it(`HTTP retry uses GitCache replay with original deadline: expired=${expireOnRetry}`, async t => {
    let now = 1_000, requests = 0;
    t.mock.method(Date, "now", () => now);
    const { git, runner, flight } = fixture("server");
    const client = runner["client"];
    const signal = new AbortController().signal;
    const deadlines: number[] = [], remaining: number[] = [];
    Object.assign(git, {
      captureCheckpointDefault: async () => DEFAULT,
      trackingTip: async () => TIP,
      scratchPublicationPreflight: async () => TIP,
      checkpointExcludeRef: async () => FLOOR,
      revParse: async (_bare: string, ref: string) => ref.startsWith(DEFAULT) ? DEFAULT : FLOOR,
      validateCheckpointFloor: async () => undefined,
      prepareCheckpointObjects: async () => Buffer.from(TIP + "\\n"),
      spawnGit: async () => {
        const operation = git["recoveryOperation"]()!;
        deadlines.push(operation.deadline);
        remaining.push(operation.deadline - Date.now());
        return { stdout: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {} };
      },
    });
    client.publishCheckpoint = async (_run, _tip, stream, actualSignal) => {
      assert.equal(actualSignal, signal);
      for await (const _chunk of stream) { /* drain each replay */ }
      requests++;
      if (requests === 1) {
        now = expireOnRetry ? 61_000 : 60_000;
        return { ok: false, httpStatus: 500 };
      }
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/task" } };
    };
    const pending = git.withRecoveryOperation(signal, 61_000,
      () => runner["publishCheckpointOutcome"](flight, "bare", "task", undefined, signal));
    if (expireOnRetry) await assert.rejects(pending, /deadline/);
    else assert.equal((await pending).published, true);
    assert.equal(requests, expireOnRetry ? 1 : 2);
    assert.deepEqual(deadlines, expireOnRetry ? [61_000] : [61_000, 61_000]);
    assert.deepEqual(remaining, expireOnRetry ? [60_000] : [60_000, 1_000]);
  });
}

for (const late of ["none", "range", "guard", "preparation"] as const) {
  it(`held without ambient budget starts at entry and shares time across scan/guard/pack: ${late}`, async t => {
    let now = 0;
    t.mock.method(performance, "now", () => now);
    assert.equal(MaintenanceBudget.current(), undefined);
    let owned: MaintenanceBudget | undefined, disposed = 0, packs = 0;
    const deadlines: number[] = [], order: string[] = [];
    const dispose = MaintenanceBudget.prototype.dispose;
    t.mock.method(MaintenanceBudget.prototype, "dispose", function(this: MaintenanceBudget) {
      disposed++; return dispose.call(this);
    });
    const checkScope = (stage: string) => {
      order.push(stage);
      const active = MaintenanceBudget.current();
      assert.ok(active);
      if (!owned) owned = active;
      assert.equal(active, owned);
      assert.equal(active.deadline, 100, "own lifetime begins at prepare entry");
    };
    const git = {
      resolveCheckpointRange: async () => {
        checkScope("range"); now = late === "range" ? 100 : 30;
        return { tipSha: TIP, excludeSha: FLOOR, defaultBlobSha: DEFAULT };
      },
      secretScanCheckpointRange: async (_bare: string, _range: unknown, opts: { deadlineMs: number }) => {
        checkScope("scan"); deadlines.push(opts.deadlineMs); now = 50;
        return { trusted: true, findings: [] };
      },
      rangeTouchesWorkflows: async (_bare: string, _range: unknown, opts: { deadlineMs: number }) => {
        checkScope("workflow"); deadlines.push(opts.deadlineMs); now = 70; return "clean";
      },
      checkpointPack: async () => {
        checkScope("preparation"); packs++;
        now = late === "preparation" ? 100 : 90;
        MaintenanceBudget.current()!.check();
        return { tipOid: TIP, pack: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {} };
      },
    } as unknown as HeldGit;
    const pending = prepareFrozenHeld({ git, barePath: "bare", branch: "task", sourceSha: TIP, policy,
      defaultBranch: "server", deadlineMs: 100,
      remediationHold: async () => { checkScope("guard"); now = late === "guard" ? 100 : 60; return false; } });
    if (late === "none") assert.deepEqual(await pending, { pack: Buffer.from("PACK") });
    else await assert.rejects(pending, /maintenance deadline/);
    assert.equal(packs, late === "none" || late === "preparation" ? 1 : 0);
    assert.deepEqual(deadlines, late === "range" ? [] : late === "guard" ? [70] : [70, 40]);
    assert.equal(order[0], "range");
    assert.equal(disposed, 1);
    assert.equal(MaintenanceBudget.current(), undefined);
    assert.ok(owned);
    assert.equal(getTimer(owned), undefined, "owned timer is disposed on every outcome");
  });
}
function getTimer(budget: MaintenanceBudget) { return budget["timer"]; }

it("held own budget expires inside real selective preparation and joins disposal without a producer", async t => {
  let now = 0, finish!: () => void, entered!: () => void, returned = false, packs = 0;
  t.mock.method(performance, "now", () => now);
  t.mock.method(Date, "now", () => now);
  const release = new Promise<void>(resolve => { finish = resolve; });
  const started = new Promise<void>(resolve => { entered = resolve; });
  const git = new GitCache("/unused", nullLogger());
  let owned: MaintenanceBudget | undefined;
  Object.assign(git, {
    resolveCheckpointRange: async () => {
      owned = MaintenanceBudget.current();
      assert.ok(owned);
      now = 30;
      return { tipSha: TIP, excludeSha: FLOOR, defaultBlobSha: DEFAULT };
    },
    secretScanCheckpointRange: async () => { now = 50; return { trusted: true, findings: [] }; },
    rangeTouchesWorkflows: async () => { now = 70; return "clean"; },
    scratchPublicationPreflight: async () => TIP,
    validateCheckpointFloor: async () => undefined,
    revParse: async () => DEFAULT,
  });
  let cancelled!: () => void;
  const cancelling = new Promise<void>(resolve => { cancelled = resolve; });
  const pending = git.withBoundaryProcessSpawner(async request => {
    if (request.argv.includes("pack-objects")) packs++;
    assert.ok(request.argv.includes("rev-list"), "expiry must stop at the first preparation child");
    const stdout = new PassThrough(), stderr = new PassThrough(), stdin = new PassThrough();
    stdin.resume();
    const completed = release.then(() => {
      stdout.end(); stderr.end(); stdin.destroy();
      return { code: 0 };
    });
    entered();
    return { stdout, stderr, stdin, completed, cancel: async () => {
      cancelled(); stdout.destroy(); stderr.destroy(); stdin.destroy(); await completed;
    } };
  }, new AbortController().signal, () => prepareFrozenHeld({
    git, barePath: "bare", branch: "task", sourceSha: TIP, policy, defaultBranch: "server", deadlineMs: 100,
  }));
  const rejected = assert.rejects(pending, /maintenance deadline/).then(() => { returned = true; });
  try {
    await started;
    assert.equal(owned!.remainingMs(), 30);
    now = 100;
    assert.throws(() => owned!.check(), /maintenance deadline/);
    await cancelling;
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(returned, false, "held cannot return before the preparation owner settles");
    assert.equal(packs, 0);
    finish(); await rejected;
    assert.equal(getTimer(owned!), undefined);
    assert.equal(MaintenanceBudget.current(), undefined);
  } finally { finish(); await rejected; }
});
