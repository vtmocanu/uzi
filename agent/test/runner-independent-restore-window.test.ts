// Issue #3644 Gate B (amendment 15, m1, plan 3.4): from the moment discovery selects the restore route until the seed succeeds, the
// flight's executor, HOME and worktreePath still name the PREDECESSOR P, whose processes belong to another incarnation. Whatever
// interrupts the flight in that window - a credential switch, a claim fence, a seed failure, an owner-hold park of every ACK shape -
// must leave P's processes alive and P's clone tree, HOME and provision directory byte-identical.
//
// The rows use REAL process operations: a child outside the worker's process tree carries HOME=<P's agent-home/<runId>> and works
// inside P's clone, P's executor reaps by HOME through the real run-procs scan, and the quiescence view is the real procfs scoped to
// that child. The shared test preload makes both vacuous by default, so each row opts in explicitly and restores the default.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import type { Executor } from "../src/executor.js";
import { quiesceRunAttempt, setQuiescenceViewForTests, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { reapRunProcesses } from "../src/run-procs.js";
import { api, client, git, installHarness } from "./runner-harness.js";
import { HERMETIC_VIEW } from "./setup/hermetic-proc.js";
import { scopedRealView } from "./fake-proc.js";
import {
  FEATURES, PARK_FEATURE, assertStoppedUntouched, buildRig, cleanupRig, drive, failed, logged, resetRigRoots, rigDefaults, type Rig,
  type RigOptions,
} from "./restore-route-rig.js";
import { RequestError } from "../src/client.js";

installHarness();
rigDefaults.independentRestore = true;

const PARK_FEATURES = [...FEATURES, PARK_FEATURE];
const REFUSED = "run quiescence refused: the restore route's predecessor is untouchable";

const planted: number[] = [];
afterEach(() => {
  cleanupRig();
  setQuiescenceViewForTests(HERMETIC_VIEW);
  for (const pid of planted.splice(0)) { try { process.kill(pid, "SIGKILL"); } catch { /* already gone */ } }
});

/** True while the process exists and is not a zombie (a reaped child of init may linger as one for a moment). */
function alive(pid: number): boolean {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3) !== "Z";
  } catch { return false; }
}

/** A process that is NOT a child of this worker (the real reaper never kills a worker's direct child), carrying `home` and working in `cwd`. */
async function plantChild(home: string, cwd: string): Promise<number> {
  const sh = spawn("sh", ["-c", "sleep 600 >/dev/null 2>&1 & echo $!"], { cwd, env: { PATH: process.env.PATH ?? "", HOME: home }, stdio: ["ignore", "pipe", "ignore"] });
  let out = "";
  sh.stdout.on("data", d => { out += String(d); });
  await new Promise(resolve => sh.on("close", resolve));
  const pid = Number(out.trim());
  assert.ok(Number.isSafeInteger(pid) && pid > 1, `planted pid ${out}`);
  planted.push(pid);
  return pid;
}

interface Window {
  r: Rig;
  child: number;
  quiesce: QuiesceRunRequest[];
  /** The flight, captured when the route starts. */
  flight(): FlightView;
  reaps: { p: number; s: number };
}
interface FlightView {
  steering: { pendingCredentialSwitch(): number | undefined; claimFence: () => string | undefined };
  runId: string;
  parked?: boolean;
  restorePredecessor?: unknown;
  restoreUnsupportedHold?: boolean;
}

/** P's executor reaps by its HOME through the real scan; the successor's reaps S's own. */
function reapingExecutor(home: () => string, count: () => void): Executor {
  return {
    run: async () => { throw new Error("no model runs in the window"); },
    reclaimCachesInPlace: async () => ({ dropped: [], absent: [], failed: [], skipped: [] }),
    killAgentTree: () => {},
    reapAttributedProcesses: async opts => { count(); return reapRunProcesses(home(), undefined, [], opts); },
  } as Executor;
}

async function window(o: RigOptions = {}): Promise<Window> {
  const holder: { home: string; r?: Rig } = { home: "" };
  const reaps = { p: 0, s: 0 };
  const quiesce: QuiesceRunRequest[] = [];
  const seen: { flight?: FlightView } = {};
  const r = await buildRig({
    addrAware: true,
    executor: reapingExecutor(() => holder.home, () => { reaps.p++; }),
    successorExecutor: reapingExecutor(() => path.join(holder.r!.homeRoot, `${holder.r!.runId}.g${holder.r!.claim.claim_generation}`), () => { reaps.s++; }),
    runnerExtra: { quiesceRun: async req => { quiesce.push(req); return quiesceRunAttempt(req); } },
    ...o,
  });
  holder.home = r.runHome;
  holder.r = r;
  const runner = r.runner as unknown as Record<string, (...a: unknown[]) => unknown>;
  const route = runner.runRestoreRoute!.bind(r.runner);
  mock.method(runner, "runRestoreRoute", (...args: unknown[]) => { seen.flight = args[1] as FlightView; return route(...args); });
  const child = await plantChild(r.runHome, r.pPath);
  setQuiescenceViewForTests(scopedRealView({ pids: [child] }));
  return { r, child, quiesce, flight: () => seen.flight!, reaps };
}

async function until(label: string, cond: () => boolean, ms = 10_000): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${label}`);
    await new Promise(resolve => setTimeout(resolve, 5));
  }
}

/** Asks the api for a credential switch at the claim's generation and waits until the flight's steering has tripped on it. */
async function tripSwitch(flight: FlightView): Promise<void> {
  api.requestCredentialSwitch(flight.runId, 2);
  await until("the credential switch to trip", () => flight.steering.pendingCredentialSwitch() === 2);
}

/** Runs `hook` once, with the flight, the first time the (private) method is called, then continues into the original. */
function onceBefore(target: object, name: string, hook: (flight: FlightView) => Promise<void> | void, argIndexOfFlight: number): void {
  const original = (target as Record<string, (...a: unknown[]) => unknown>)[name]!.bind(target);
  let done = false;
  mock.method(target as Record<string, (...a: unknown[]) => unknown>, name, async (...args: unknown[]) => {
    if (!done) { done = true; await hook(args[argIndexOfFlight] as FlightView); }
    return original(...args);
  });
}

const touchesP = (r: Rig, req: QuiesceRunRequest): boolean =>
  req.targetPaths.some(t => t === r.pPath || t.startsWith(r.pPath + path.sep) || r.pPath.startsWith(t + path.sep));

/** P is as it was: the child lives, nothing of P is scanned or reaped, and the bytes are identical. */
async function assertPUntouched(w: Window, before: Awaited<ReturnType<typeof drive>>["before"], traps: Awaited<ReturnType<typeof drive>>["traps"]): Promise<void> {
  assert.equal(alive(w.child), true, "P's process survived");
  assert.deepEqual(w.quiesce.filter(req => touchesP(w.r, req)).map(req => req.site), [], "no quiescence request named P's path");
  assert.equal(w.reaps.p, 0, "P's executor was never asked to reap by HOME");
  await assertStoppedUntouched(w.r, before, traps);
}

describe("the window between route selection and the seed leaves P's processes and bytes alone (issue #3644 3.4)", () => {
  beforeEach(() => { resetRigRoots(); });

  describe("after selection, before the route runs (the policy and memory awaits)", () => {
    it("(i) a credential switch: the retained switch stops before settling, reports nothing, and is nonterminal", async () => {
      const w = await window();
      onceBefore(w.r.runner as unknown as object, "memoryReparkOwner", tripSwitch, 1);
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(logged(w.r, "retained credential switch deferred: the restore route's predecessor is untouchable until the successor is seeded").length, 1);
      assert.equal(api.states.filter(s => s.runId === w.r.runId && String(s.body.status).startsWith("credential_switch")).length, 0, "no switch report");
      assert.equal(failed(w.r).length, 0);
    });

    it("(ii) a claim fence: the stopped-source retention is refused its quiescence proof", async () => {
      const w = await window();
      onceBefore(w.r.runner as unknown as object, "memoryReparkOwner", flight => { flight.steering.claimFence = () => "stale"; }, 1);
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.ok(logged(w.r, REFUSED).length >= 1, "the guard refused the proof");
      assert.ok(w.r.logs.lines.some(l => String((l as { msg: string }).msg).startsWith("input receipt fenced this claim")), "the claim-fence arm ran");
    });
  });

  it("(iii) a credential switch during classification", async () => {
    const w = await window();
    const read = git.readRestoreAttribution.bind(git);
    let done = false;
    mock.method(git, "readRestoreAttribution", async (...args: Parameters<typeof read>) => {
      if (!done) { done = true; await tripSwitch(w.flight()); }
      return read(...args);
    });
    const { before, traps } = await drive(w.r);
    await assertPUntouched(w, before, traps);
    assert.equal(logged(w.r, "retained credential switch deferred: the restore route's predecessor is untouchable until the successor is seeded").length, 1);
  });

  describe("after the executor swap, before the seed returns", () => {
    function seedFailsAfter(w: Window, before: (flight: FlightView) => Promise<void> | void): void {
      mock.method(git, "seedIndependentAttempt", async () => {
        await before(w.flight());
        throw new Error("the seed was interrupted");
      });
    }

    it("(v) a claim fence", async () => {
      const w = await window();
      seedFailsAfter(w, flight => { flight.steering.claimFence = () => "stale"; });
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(w.r.factoryCalls.some(c => c.generation !== undefined), true, "the executor was swapped first");
      assert.ok(logged(w.r, REFUSED).length >= 1, "the stopped-source proof of P was refused");
    });

    it("(vi) a credential switch", async () => {
      const w = await window();
      seedFailsAfter(w, tripSwitch);
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(w.r.factoryCalls.some(c => c.generation !== undefined), true, "the executor was swapped first");
      assert.equal(logged(w.r, "retained credential switch deferred: the restore route's predecessor is untouchable until the successor is seeded").length, 1);
    });

    it("(vii) a seed failure: P is untouched through the stop and the teardown, and the guard field stays set", async () => {
      const w = await window();
      seedFailsAfter(w, () => {});
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(logged(w.r, "independent restore refused; predecessor retained")[0]?.reason, "seed_failed");
      assert.notEqual(w.flight().restorePredecessor, undefined, "P stays untouchable on every exit but the seed's success");
    });

    it("(vii-b) P's executor is refused even for explicit targets unrelated to P", async () => {
      const w = await window();
      const outcomes: { blocked: boolean }[] = [];
      seedFailsAfter(w, async flight => {
        const p = (flight.restorePredecessor as { executor: Executor }).executor;
        const unrelated = path.join(w.r.homeRoot, "unrelated-target");
        const runner = w.r.runner as unknown as { quiesceRun(f: unknown, e: Executor, o: unknown): Promise<{ blocked: boolean }> };
        outcomes.push(await runner.quiesceRun(flight, p, { mode: "seed", site: "executor_arm_probe", targetPaths: [unrelated] }));
      });
      const { before, traps } = await drive(w.r);
      assert.equal(outcomes.length, 1);
      assert.equal(outcomes[0]!.blocked, true, "the request was refused");
      assert.equal(logged(w.r, REFUSED).filter(l => l.site === "executor_arm_probe").length, 1);
      await assertPUntouched(w, before, traps);
    });
  });

  describe("(viii) the owner-hold park of an unsupported predecessor, whatever its ACK", () => {
    const park = (confirm: boolean, hook?: (body: Record<string, unknown>, original: () => Promise<unknown>) => Promise<unknown>): void => {
      const real = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
      mock.method(client, "reportState", (async (...args: unknown[]) => {
        const body = args[1] as Record<string, unknown>;
        const original = () => real(...args);
        const ack = (hook ? await hook(body, original) : await original()) as Record<string, unknown>;
        return confirm && body.status === "recovery_wait" && typeof body.recovery_cause === "string" ? { ...ack, recoveryCause: body.recovery_cause } : ack;
      }) as never);
    };

    it("a confirmed park: the park teardown neither reaps nor drops P's HOME", async () => {
      const w = await window({ pShape: "canonical", features: PARK_FEATURES });
      park(true);
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(logged(w.r, "restore unsupported predecessor held").length, 1);
      assert.equal(w.flight().parked, true);
      assert.equal(w.flight().restoreUnsupportedHold, true);
    });

    it("a refused park (400): nonterminal, P untouched", async () => {
      const w = await window({ pShape: "canonical", features: PARK_FEATURES });
      park(true, async (body, original) => {
        if (body.status === "recovery_wait") throw new RequestError("POST", "/state", 400, "unsupported recovery_cause");
        return original();
      });
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(logged(w.r, "restore unsupported predecessor hold unconfirmed").length, 1);
    });

    it("an unknown ACK (status only, no typed cause): unconfirmed, P untouched", async () => {
      const w = await window({ pShape: "canonical", features: PARK_FEATURES });
      park(false);
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(logged(w.r, "restore unsupported predecessor hold unconfirmed").length, 1);
    });

    it("a credential switch during the park reconciliation", async () => {
      const w = await window({ pShape: "canonical", features: PARK_FEATURES });
      let tripped = false;
      park(true, async (body, original) => {
        if (body.status === "recovery_wait" && !tripped) { tripped = true; await tripSwitch(w.flight()); }
        return original();
      });
      const { before, traps } = await drive(w.r);
      await assertPUntouched(w, before, traps);
      assert.equal(tripped, true);
      assert.equal(logged(w.r, "retained credential switch deferred: the restore route's predecessor is untouchable until the successor is seeded").length, 1);
      assert.equal(logged(w.r, "restore unsupported predecessor held").length, 0, "the switch preempted the confirmation");
    });
  });

  describe("positive rows", () => {
    it("the seed's own S-only quiescence still runs, and a successful seed releases the predecessor guard", async () => {
      const w = await window();
      const { before } = await drive(w.r);
      void before;
      assert.equal(logged(w.r, "independent restore seeded").length, 1, JSON.stringify(w.r.logs.lines.filter(l => /restore/i.test(String((l as { msg: string }).msg)))));
      const seed = w.quiesce.filter(req => req.site === "attempt_seed");
      assert.equal(seed.length, 1, "the seed's sweep ran through the same quiesceRun");
      assert.ok(seed.every(req => !touchesP(w.r, req)), "its targets are P-free");
      assert.equal(logged(w.r, REFUSED).length, 0, "the guard refused none of S's own requests");
      assert.equal(w.flight().restorePredecessor, undefined, "cleared by the seed's success");
      assert.equal(alive(w.child), true, "P's process is not S's to reap");
    });
  });
});
