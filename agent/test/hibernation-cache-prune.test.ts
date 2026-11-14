import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { rmSync } from "node:fs";
import { HibernationCachePruneController, type StepExit } from "../src/hibernation-cache-prune.js";
import { DindPruneGate, dockerExecEnvironment, type DockerExec, type GateOwner } from "../src/dind-prune.js";
import type { HibernationCachePrune, HibernationCachePruneReport } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

after(() => {
  // The worker-owned DOCKER_CONFIG directory is created once per process.
  rmSync(dockerExecEnvironment("tcp://dind:2375").DOCKER_CONFIG ?? "", { recursive: true, force: true });
});

const NONCE = "registration-nonce";
const OP = "22222222-2222-2222-2222-222222222222";
const OP2 = "33333333-3333-3333-3333-333333333333";
const DANGLING = "image prune -f";
const BUILDER = "builder prune -f";
const PROBE = "version --format {{.Server.APIVersion}}";
const OK: StepExit = { code: 0, signal: null };

function deferred<T = void>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}
const flush = () => new Promise<void>((r) => setImmediate(r));

function harness() {
  let time = 1_700_000_000_000;
  const gate = new DindPruneGate();
  const calls: { kind: "probe" | "step"; line: string; opts: unknown }[] = [];
  const state = {
    idle: true,
    probeFails: false,
    run: async (_line: string): Promise<StepExit> => OK,
  };
  const exec: DockerExec = async (argv, opts) => {
    calls.push({ kind: "probe", line: argv.join(" "), opts });
    if (state.probeFails) throw new Error("daemon unreachable");
    return "1.45";
  };
  const ctl = new HibernationCachePruneController({
    gate, dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000, isIdle: () => state.idle,
    log: nullLogger(), exec, now: () => time, sleep: async () => {},
    runStep: async (argv, opts) => {
      calls.push({ kind: "step", line: argv.join(" "), opts });
      return state.run(argv.join(" "));
    },
  });
  ctl.register(NONCE);
  const proj = (phase: HibernationCachePrune["phase"], extra: Partial<HibernationCachePrune> = {}): HibernationCachePrune => ({
    op_id: OP, register_nonce: NONCE, phase, cancel_requested: false, reconcile_seq: 1, ...extra,
  });
  /** One heartbeat's observation, stamped with the (advanced) send time. */
  const hb = (p: HibernationCachePrune | null | undefined, retaining: boolean | undefined = false) => {
    time += 1000;
    ctl.observe(p, retaining, time);
  };
  /** Requested op to a sent ready report. */
  const toReady = async (extra: Partial<HibernationCachePrune> = {}): Promise<HibernationCachePruneReport> => {
    hb(proj("requested", extra));
    await ctl.tick(); // closes the gate; custody predates the close
    hb(proj("requested", extra));
    await ctl.tick(); // probe, armed
    const r = ctl.report();
    assert.equal(r?.kind, "ready");
    ctl.reported(r, "sent");
    return r;
  };
  const count = (line: string) => calls.filter((c) => c.line === line).length;
  return { ctl, gate, state, calls, proj, hb, toReady, count, advance: (ms: number) => { time += ms; }, now: () => time };
}

function resultOf(r: HibernationCachePruneReport | undefined) {
  assert.equal(r?.kind, "result");
  return { steps: r.steps?.map((s) => `${s.command}:${s.state}`), throughout: r.gate_exclusive_throughout };
}

describe("hibernation cache prune: the pruning flow", () => {
  it("prunes with no pressure precondition, through the frozen argv only", async () => {
    const h = harness();
    const ready = await h.toReady();
    assert.deepEqual(
      { local_claims: ready.local_claims, local_executions: ready.local_executions, custody_clear: ready.custody_clear, gate_exclusive: ready.gate_exclusive },
      { local_claims: 0, local_executions: 0, custody_clear: true, gate_exclusive: true });
    assert.equal(ready.custody_checked_at, new Date(h.now()).toISOString());
    assert.ok(h.gate.onlyHeldBy("hibernation"));
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    await flush();
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    assert.deepEqual(resultOf(h.ctl.report()), {
      steps: ["image_prune_dangling:exited_ok", "builder_prune:exited_ok"], throughout: true });
    assert.deepEqual(h.calls.map((c) => c.line), [PROBE, DANGLING, BUILDER]);
    for (const c of h.calls) {
      for (const token of c.line.split(" ")) assert.ok(!["volume", "system", "container", "rm", "--volumes", "-a"].includes(token), c.line);
    }
    const step = h.calls[1]!.opts as Record<string, unknown>;
    assert.equal("signal" in step, false, "a prune step has no abort signal");
    assert.equal("timeoutMs" in step, false, "a prune step has no timeout");
  });

  it("releases only on settled, and only its own key", async () => {
    const h = harness();
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.gate.close("maintenance");
    h.hb(h.proj("settled", { reconcile_seq: 3 }));
    assert.equal(h.gate.heldBy("hibernation"), false);
    assert.equal(h.gate.heldBy("maintenance"), true, "another owner's key is never opened");
  });

  it("releases on a null projection when no child runs, and after the child closes otherwise", async () => {
    const idle = harness();
    await idle.toReady();
    idle.hb(null);
    assert.equal(idle.gate.claimsClosed(), false);

    const busy = harness();
    const step = deferred<StepExit>();
    busy.state.run = () => step.promise;
    await busy.toReady();
    busy.hb(busy.proj("started"));
    await flush();
    busy.hb(null);
    assert.equal(busy.gate.heldBy("hibernation"), true, "a running child keeps the gate");
    step.resolve(OK);
    busy.state.run = async () => OK;
    await flush();
    await flush();
    assert.equal(busy.gate.claimsClosed(), false);
  });

  it("treats a failed probe as daemon_unreachable and runs nothing", async () => {
    const h = harness();
    h.state.probeFails = true;
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    const r = h.ctl.report();
    assert.deepEqual({ kind: r?.kind, reason: r?.reason }, { kind: "refused", reason: "daemon_unreachable" });
    assert.equal(h.count(DANGLING), 0);
  });

  it("a mismatched nonce is ignored entirely", async () => {
    const h = harness();
    h.hb(h.proj("requested", { register_nonce: "someone-else" }));
    await h.ctl.tick();
    assert.equal(h.gate.claimsClosed(), false);
    assert.equal(h.ctl.report(), undefined);
    h.hb(h.proj("started", { register_nonce: "someone-else" }));
    assert.equal(h.calls.length, 0);
  });

  it("a new registration nonce clears dispositions", async () => {
    const h = harness();
    await h.toReady();
    h.ctl.register("second-nonce");
    assert.equal(h.gate.claimsClosed(), false, "the old attempt's hold is released at once");
    h.hb(h.proj("started"));
    assert.equal(h.calls.filter((c) => c.kind === "step").length, 0);
    h.hb(h.proj("requested", { register_nonce: "second-nonce" }));
    await h.ctl.tick();
    assert.ok(h.gate.onlyHeldBy("hibernation"));
  });
});

describe("hibernation cache prune: refusals", () => {
  const bound = 61_000;

  it("stale custody refuses at the bound", async () => {
    const h = harness();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.advance(bound);
    await h.ctl.tick();
    const r = h.ctl.report();
    assert.deepEqual({ kind: r?.kind, reason: r?.reason }, { kind: "refused", reason: "custody_stale" });
  });

  it("custody that may be retaining work is not clear", async () => {
    const h = harness();
    h.hb(h.proj("requested"), true);
    await h.ctl.tick();
    h.hb(h.proj("requested"), true);
    h.advance(bound);
    h.advance(1000);
    h.ctl.observe(h.proj("requested"), undefined, h.now());
    await h.ctl.tick();
    assert.equal(h.ctl.report()?.reason, "custody_stale");
  });

  it("active work and an in-flight claim refuse at the bound with their own reasons", async () => {
    const work = harness();
    work.state.idle = false;
    work.hb(work.proj("requested"));
    await work.ctl.tick();
    work.advance(bound);
    await work.ctl.tick();
    assert.equal(work.ctl.report()?.reason, "active_work");

    const claim = harness();
    assert.equal(claim.gate.tryEnterClaim(), true);
    claim.hb(claim.proj("requested"));
    await claim.ctl.tick();
    claim.advance(bound);
    await claim.ctl.tick();
    assert.equal(claim.ctl.report()?.reason, "claim_in_flight");
  });

  it("cancel while requested reports refused cancelled", async () => {
    const h = harness();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.hb(h.proj("requested", { cancel_requested: true }));
    const r = h.ctl.report();
    assert.deepEqual({ kind: r?.kind, reason: r?.reason }, { kind: "refused", reason: "cancelled" });
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
  });

  it("revalidates just before ready: work that appears after arming refuses", async () => {
    const h = harness();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.state.idle = false;
    assert.equal(h.ctl.report()?.reason, "active_work");
  });
});

describe("hibernation cache prune: exclusivity", () => {
  const owners: GateOwner[] = ["cache", "maintenance", "retained_ops", "terminal_maintenance"];

  it("waits for a competing cache hold, then proceeds once it is released", async () => {
    const h = harness();
    h.gate.close("cache");
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.advance(20_000);
    await h.ctl.tick();
    assert.equal(h.gate.heldBy("hibernation"), false);
    h.gate.open("cache");
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    assert.equal(h.ctl.report()?.kind, "ready");
  });

  for (const owner of owners) {
    it(`${owner} held through the bound refuses gate_not_exclusive`, async () => {
      const h = harness();
      h.gate.close(owner);
      h.hb(h.proj("requested"));
      await h.ctl.tick();
      h.advance(61_000);
      await h.ctl.tick();
      assert.equal(h.ctl.report()?.reason, "gate_not_exclusive");
      assert.equal(h.gate.heldBy("hibernation"), false);
      assert.equal(h.gate.heldBy(owner), true);
    });

    it(`${owner} closing after the hibernation close but before ready refuses gate_not_exclusive`, async () => {
      const h = harness();
      h.hb(h.proj("requested"));
      await h.ctl.tick();
      h.gate.close(owner);
      h.hb(h.proj("requested"));
      await h.ctl.tick();
      assert.equal(h.ctl.report()?.reason, "gate_not_exclusive");
      assert.equal(h.calls.filter((c) => c.kind === "step").length, 0);
    });
  }

  it("another owner closing between started and a step stops further steps", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = () => first.promise;
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.gate.close("maintenance");
    first.resolve(OK);
    await flush();
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    assert.deepEqual(resultOf(h.ctl.report()), { steps: ["image_prune_dangling:exited_ok", "builder_prune:not_started"], throughout: false });
    assert.equal(h.count(BUILDER), 0);
  });

  it("detects any generation change, even a close and open that leaves the gate looking unchanged", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = () => first.promise;
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.gate.close("cache");
    h.gate.open("cache");
    assert.ok(h.gate.onlyHeldBy("hibernation"));
    first.resolve(OK);
    await flush();
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    assert.deepEqual(resultOf(h.ctl.report()), { steps: ["image_prune_dangling:exited_ok", "builder_prune:not_started"], throughout: false });
  });

  it("work appearing after started stops the next step", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = () => first.promise;
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.state.idle = false;
    first.resolve(OK);
    await flush();
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    assert.equal(h.count(BUILDER), 0);
    assert.equal(resultOf(h.ctl.report()).throughout, false);
  });
});

describe("hibernation cache prune: cancel and step outcomes", () => {
  it("cancel mid-prune finishes the running step, spawns no further one, and never signals the child", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = () => first.promise;
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("started", { cancel_requested: true, reconcile_seq: 2 }));
    await flush();
    assert.equal(h.ctl.report(), undefined, "no result while the child is unclosed");
    first.resolve(OK); // a slow prune survives the cancel and ends cleanly
    await flush();
    h.hb(h.proj("started", { cancel_requested: true, reconcile_seq: 3 }));
    assert.deepEqual(resultOf(h.ctl.report()), { steps: ["image_prune_dangling:exited_ok", "builder_prune:not_started"], throughout: true });
    assert.equal(h.count(BUILDER), 0);
  });

  it("a slow second step survives cancel and reports exited_ok", async () => {
    const h = harness();
    const second = deferred<StepExit>();
    h.state.run = (line) => line === BUILDER ? second.promise : Promise.resolve(OK);
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("started", { cancel_requested: true, reconcile_seq: 2 }));
    second.resolve(OK);
    await flush();
    h.hb(h.proj("started", { cancel_requested: true, reconcile_seq: 3 }));
    assert.deepEqual(resultOf(h.ctl.report()), { steps: ["image_prune_dangling:exited_ok", "builder_prune:exited_ok"], throughout: true });
  });

  const failures: [string, StepExit][] = [
    ["non-zero exit", { code: 1, signal: null }],
    ["killed by a signal", { code: null, signal: "SIGKILL" }],
    ["a spawn error", { error: "spawn ENOENT" }],
  ];
  for (const [name, exit] of failures) {
    it(`${name} is unknown, never not_started, and stops the next step`, async () => {
      const h = harness();
      h.state.run = async () => exit;
      await h.toReady();
      h.hb(h.proj("started"));
      await flush();
      h.hb(h.proj("started", { reconcile_seq: 2 }));
      assert.deepEqual(resultOf(h.ctl.report()), { steps: ["image_prune_dangling:unknown", "builder_prune:not_started"], throughout: true });
      assert.equal(h.count(BUILDER), 0);
    });
  }

  it("a thrown runner is unknown", async () => {
    const h = harness();
    h.state.run = async () => { throw new Error("boom"); };
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    const r = resultOf(h.ctl.report());
    assert.equal(r.steps?.[0], "image_prune_dangling:unknown");
    assert.equal(r.throughout, false);
  });

  it("shutdown reports the in-flight step unknown without killing the child", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = () => first.promise;
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    const stop = new AbortController();
    const flushed: HibernationCachePruneReport[] = [];
    const done = h.ctl.loop(stop.signal, async (r) => { flushed.push(r); });
    stop.abort();
    await done;
    assert.equal(flushed.length, 1);
    assert.deepEqual(resultOf(flushed[0]), { steps: ["image_prune_dangling:unknown", "builder_prune:not_started"], throughout: false });
    assert.equal(h.count(BUILDER), 0);
    first.resolve(OK);
    await flush();
    assert.equal(h.count(BUILDER), 0, "no further step once stopping");
  });
});

describe("hibernation cache prune: start rule", () => {
  it("a started arriving over 45s after ready declines with every step not_started", async () => {
    const h = harness();
    await h.toReady();
    h.advance(46_000);
    h.hb(h.proj("started"));
    await flush();
    assert.equal(h.calls.filter((c) => c.kind === "step").length, 0);
    assert.deepEqual(resultOf(h.ctl.report()), { steps: ["image_prune_dangling:not_started", "builder_prune:not_started"], throughout: true });
    h.hb(h.proj("started"));
    await flush();
    assert.equal(h.calls.filter((c) => c.kind === "step").length, 0, "no execution after a not_started report");
  });

  it("a started for a key whose ready was never sent declines, and a later requested is ignored", async () => {
    const h = harness();
    h.hb(h.proj("started"));
    await flush();
    assert.equal(h.calls.length, 0);
    assert.equal(resultOf(h.ctl.report()).steps?.every((s) => s.endsWith("not_started")), true);
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    assert.equal(h.gate.claimsClosed(), false);
  });

  it("a cancel observed with started spawns nothing", async () => {
    const h = harness();
    await h.toReady();
    h.hb(h.proj("started", { cancel_requested: true }));
    await flush();
    assert.equal(h.calls.filter((c) => c.kind === "step").length, 0);
  });

  it("never readies a new op while an earlier child is unclosed", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = () => first.promise;
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("requested", { op_id: OP2 }));
    await h.ctl.tick();
    h.hb(h.proj("requested", { op_id: OP2 }));
    await h.ctl.tick();
    assert.equal(h.ctl.report(), undefined);
    assert.equal(h.count(PROBE), 1);
    h.state.run = async () => OK;
    first.resolve(OK);
    await flush();
    await flush();
    h.hb(h.proj("requested", { op_id: OP2 }));
    await h.ctl.tick();
    h.hb(h.proj("requested", { op_id: OP2 }));
    await h.ctl.tick();
    assert.equal(h.ctl.report()?.kind, "ready");
    assert.equal(h.gate.onlyHeldBy("hibernation"), true);
  });
});

describe("hibernation cache prune: at most once", () => {
  const steps = (h: ReturnType<typeof harness>) => h.calls.filter((c) => c.kind === "step").map((c) => c.line);

  it("duplicate started before, during and after execution runs each command once", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = (line) => line === DANGLING ? first.promise : Promise.resolve(OK);
    await h.toReady();
    h.hb(h.proj("started"));
    h.hb(h.proj("started")); // before the child could close
    await flush();
    h.hb(h.proj("started")); // during
    first.resolve(OK);
    await flush();
    await flush();
    h.hb(h.proj("started")); // after
    h.hb(h.proj("started", { reconcile_seq: 4 }));
    assert.deepEqual(steps(h), [DANGLING, BUILDER]);
  });

  it("a reordered requested delayed after started is ignored", async () => {
    const h = harness();
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    const generation = h.gate.generation();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    assert.equal(h.gate.generation(), generation);
    assert.equal(h.count(PROBE), 1);
    assert.notEqual(h.ctl.report()?.kind, "ready");
    assert.deepEqual(steps(h), [DANGLING, BUILDER]);
  });

  it("a settlement racing a replayed started does not re-run, and releases after the child", async () => {
    const h = harness();
    const first = deferred<StepExit>();
    h.state.run = (line) => line === DANGLING ? first.promise : Promise.resolve(OK);
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("settled", { reconcile_seq: 2 }));
    assert.equal(h.gate.heldBy("hibernation"), true, "the running child keeps the gate");
    h.hb(h.proj("started"));
    first.resolve(OK);
    await flush();
    await flush();
    assert.equal(h.gate.claimsClosed(), false);
    h.hb(h.proj("started"));
    assert.deepEqual(steps(h), [DANGLING, BUILDER]);
  });

  it("settled, then a replayed requested and started: no ready, no re-close, no invocation, even after the gate was released", async () => {
    const h = harness();
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("settled", { reconcile_seq: 2 }));
    assert.equal(h.gate.claimsClosed(), false);
    const generation = h.gate.generation();
    const before = h.calls.length;
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    assert.equal(h.ctl.report(), undefined);
    h.hb(h.proj("started"));
    await flush();
    await h.ctl.tick();
    assert.equal(h.gate.generation(), generation, "the gate is never touched again");
    assert.equal(h.calls.length, before);
    assert.equal(h.count(DANGLING), 1);
    assert.equal(h.count(BUILDER), 1);
  });

  it("a tombstoned key never re-closes the gate or readies even when the api resends requested after a null", async () => {
    const h = harness();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.hb(h.proj("requested", { cancel_requested: true }));
    h.hb(null);
    assert.equal(h.gate.claimsClosed(), false);
    const generation = h.gate.generation();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    assert.equal(h.gate.generation(), generation);
    assert.equal(h.calls.length, 0);
  });

  it("settled for a key never acted on only tombstones it", async () => {
    const h = harness();
    h.hb(h.proj("settled"));
    assert.equal(h.ctl.report(), undefined);
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    assert.equal(h.gate.generation(), 0);
    assert.equal(h.calls.length, 0);
  });
});

describe("hibernation cache prune: report delivery", () => {
  it("a result is resent after a failed heartbeat and on reconcile_seq change, not otherwise", async () => {
    const h = harness();
    await h.toReady();
    h.hb(h.proj("started"));
    await flush();
    h.hb(h.proj("started", { reconcile_seq: 2 }));
    const first = h.ctl.report();
    assert.equal(first?.kind, "result");
    h.ctl.reported(first!, "failed");
    assert.equal(h.ctl.report()?.kind, "result");
    h.ctl.reported(h.ctl.report()!, "sent");
    assert.equal(h.ctl.report(), undefined);
    h.hb(h.proj("unknown", { reconcile_seq: 2 }));
    assert.equal(h.ctl.report(), undefined);
    h.hb(h.proj("unknown", { reconcile_seq: 3 }));
    assert.equal(h.ctl.report()?.kind, "result");
    assert.equal(h.count(DANGLING), 1);
  });

  it("a dropped (409) report is not resent: ready and refused", async () => {
    const h = harness();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    h.hb(h.proj("requested"));
    await h.ctl.tick();
    const ready = h.ctl.report()!;
    h.ctl.reported(ready, "dropped");
    assert.equal(h.ctl.report(), undefined);

    const r = harness();
    r.hb(r.proj("requested", { cancel_requested: true }));
    const refused = r.ctl.report()!;
    assert.equal(refused.kind, "refused");
    r.ctl.reported(refused, "dropped");
    assert.equal(r.ctl.report(), undefined);
  });
});

describe("hibernation cache prune: the default step runner", () => {
  const make = (dockerBin: string) => new HibernationCachePruneController({
    gate: new DindPruneGate(), dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000, isIdle: () => true,
    log: nullLogger(), exec: async () => "1.45", dockerBin, sleep: async () => {},
  });
  async function drive(ctl: HibernationCachePruneController) {
    const p = (phase: HibernationCachePrune["phase"]): HibernationCachePrune =>
      ({ op_id: OP, register_nonce: NONCE, phase, cancel_requested: false, reconcile_seq: 1 });
    ctl.register(NONCE);
    ctl.observe(p("requested"), false, Date.now());
    await ctl.tick();
    await new Promise((r) => setTimeout(r, 5));
    ctl.observe(p("requested"), false, Date.now());
    await ctl.tick();
    const ready = ctl.report();
    assert.equal(ready?.kind, "ready");
    ctl.observe(p("started"), false, Date.now());
    for (let i = 0; i < 200 && ctl.report() === undefined; i++) await new Promise((r) => setTimeout(r, 10));
    ctl.observe(p("started"), false, Date.now());
    return ctl.report();
  }

  it("a real child exiting 0 is exited_ok", async () => {
    const r = await drive(make("/bin/true"));
    assert.deepEqual(resultOf(r), { steps: ["image_prune_dangling:exited_ok", "builder_prune:exited_ok"], throughout: true });
  });

  it("a command that cannot be spawned is unknown", async () => {
    const r = await drive(make("/nonexistent/docker"));
    assert.deepEqual(resultOf(r), { steps: ["image_prune_dangling:unknown", "builder_prune:not_started"], throughout: true });
  });
});
