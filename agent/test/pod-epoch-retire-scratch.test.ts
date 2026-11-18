// Issue #4263 M4: the retire-scratch phase of runPodEpochShadow, driven through the verified pass with real git bares,
// real bound scratch parents and the real `empty-fd` helper (the uid split is simulated by stripping the setpriv wrapper
// from the spawn, as retire-scratch-discard.test.ts does), a fake daemon guard, a fake authority and fake API reads.
import { afterEach, beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { execFileSync, spawn, type ChildProcess, type StdioOptions } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import fsp from "node:fs/promises";
import path from "node:path";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { runPodEpochShadow, PodEpochSettlementError } from "../src/pod-epoch-reclaim.js";
import type { PodEpochDaemonGuard } from "../src/pod-epoch-daemon.js";
import {
  GitCache, evaluateRetireBinding, evaluateRetireRecovery, parseRetireBinding, readRetireScratchReclaimActivation, retireBindingKey, retireBindingValue, storesNameRetireParent,
  type RetireBinding, type RetireBindingStep, type RetireDispositionClass, type RetireScratchBareEvidence, type RetireScratchPassReport,
} from "../src/git.js";
import { protectedLeaves } from "../src/pod-epoch-scratch.js";
import { buildPodEpochRetireDeps } from "../src/main.js";
import { WorkerClient } from "../src/client.js";
import { dockerRunningContainers } from "../src/run-quiescence.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { runnerCommand } from "../src/runner-uid.js";
import { restoreTreeWritability } from "../src/rmtree.js";
import type { PodEpochOwnership } from "../src/pod-epoch-lock.js";
import type { Logger } from "../src/log.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { ENABLE_RETIRE_FOR_TEST } from "./pod-epoch-reclaim-support.js";

const bytes = await readFile(new URL("../../fixtures/pod-epoch/v2/positive.json", import.meta.url));
const evidence = JSON.parse(bytes.toString());
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const BRANCH = "agent/issue-4263";
const HAS_PROC_FD = fs.existsSync("/proc/self/fd");
const FUTURE_FENCE = () => ({ t0Ms: Date.now() + 3_600_000 });
const PROC = { skip: !HAS_PROC_FD };

type Options = Parameters<typeof runPodEpochShadow>[0];
type Outcome = "real" | "hang" | { exit: number };

interface Seed {
  uuid: string; name: string; parent: string; clone: string; clonePath: string; attemptId: string; runId: string;
  binding: RetireBinding; raw: string;
}
interface SeedSpec {
  cls?: RetireDispositionClass; phase?: RetireBinding["phase"]; runId?: string; ledger?: "retired" | "live" | "none";
  withParent?: boolean; n?: number; uuid?: string;
}

class World {
  fx!: Fixture;
  cache!: GitCache;
  bare!: string;
  runnerRoot!: string;
  clock = 1_000_000;
  steps: RetireBindingStep[] = [];
  stepHook?: (s: RetireBindingStep) => void | Promise<void>;
  events: string[] = [];
  handovers = 0;
  begins = 0;
  spawns: Array<{ args: string[] }> = [];
  outcome: Outcome = "real";
  onChild?: () => void;
  // authority fakes
  slots = new Map<string, { state: string; pending: unknown }>();
  held = new Set<string>();
  eligible = true;
  txLost = false;
  txCount = 0;
  beforeB?: () => void | Promise<void>;
  reserveCalls: string[] = [];
  runStatus = "completed";
  openHolds = 0;
  // seams
  fence: () => { t0Ms: number } | { refused: "pod_entry_invalid" } = FUTURE_FENCE;
  mounts: () => "ok" | "mount_unknown" | "mount_unexpected" = () => "ok";
  statfs: () => "ok" | "fs_unsupported" = () => "ok";
  binds: () => ReturnType<NonNullable<PodEpochDaemonGuard["bindSources"]>> = () => ({ state: "read", sources: [] });
  scan: (names: readonly string[]) => { held: Set<string> } | { refused: "not_idle" | "self_descriptor_unreadable" } = () => ({ held: new Set() });
  quiesce: (bound?: { signal: AbortSignal; deadline: number }) => Promise<boolean> = async () => true;
  discoverComplete = true;
  activation: boolean | undefined = true;
  gate = true;
  pending = 0;
  stopped = false;
  /** Replaces the helper deadline of the next discard (a hung child is joined only at the deadline). */
  helperDeadlineMs: number | undefined;
  exFd = -1;
  options!: Options;
  parent = new AbortController();
  discovered = { census: 0 };
  logs: Array<{ message: string; fields: Record<string, unknown> }> = [];
  extra: Array<() => void> = [];

  async init(): Promise<void> {
    this.fx = makeFixture();
    const logger: Logger = {
      ...nullLogger(),
      info: (message: string, fields?: Record<string, unknown>) => { this.logs.push({ message, fields: fields ?? {} }); },
      warn: (message: string, fields?: Record<string, unknown>) => { this.logs.push({ message, fields: fields ?? {} }); },
      child: () => logger,
    };
    this.cache = new GitCache(this.fx.dataDir, logger, undefined, testGitCacheOptions({
      retentionDelete: { split: true },
      retireBindingStep: async (s: RetireBindingStep) => { this.steps.push(s); await this.stepHook?.(s); },
    }));
    this.bare = await this.cache.ensureClone(this.fx.originPath);
    fs.chmodSync(path.dirname(this.bare), 0o700);
    fs.chmodSync(this.bare, 0o700);
    this.runnerRoot = path.join(this.fx.dataDir, "runner");
    fs.mkdirSync(path.join(this.runnerRoot, "slug"), { recursive: true });
    fs.chmodSync(this.runnerRoot, 0o1700);
    const exFile = path.join(this.fx.dataDir, "ex-lock");
    fs.writeFileSync(exFile, "");
    this.exFd = fs.openSync(exFile, fs.constants.O_RDONLY);
    this.installSpawn();
    const scope = {
      assertExclusive: () => { if (this.stopped) throw new Error("stopped"); },
      get pendingMutations() { return w.pending; },
      beginMutation: () => { this.pending++; let done = false; return { settle: () => { if (!done) { done = true; this.pending--; } } }; },
      inheritStdio: (stdio: unknown[] = ["pipe", "pipe", "pipe"]) => { assert.ok(this.pending > 0); return [...stdio, this.exFd] as StdioOptions; },
      stop: () => { this.stopped = true; }, stopped: () => this.stopped,
      assertSettled: () => { if (this.pending !== 0 || this.stopped) throw new Error("unsettled"); },
      finish: () => { this.events.push("finish"); scope.assertSettled(); },
    };
    let tx: object | undefined;
    const guard: PodEpochDaemonGuard = {
      signal: new AbortController().signal,
      assertHealthy: () => undefined,
      close: async () => undefined,
      bindSources: () => this.binds(),
      refreshInventory: async () => undefined,
    };
    const ownership: PodEpochOwnership = {
      mode: "exclusive", consume: async () => { throw new Error("consume already performed"); },
      matchesEntry: () => true,
      reacquireShared: async () => { this.events.push("handover"); this.handovers++; },
      close: () => { throw new Error("must retain ownership"); },
      beginReclaim: () => { this.begins++; return scope as never; },
    };
    const retire: NonNullable<Options["retire"]> = {
        git: this.splitGit(),
        runnerRoot: this.runnerRoot,
        quiesce: bound => this.quiesce(bound),
        authority: {
          withTransaction: async <T,>(fn: (t: object) => Promise<T>): Promise<T> => {
            tx = {};
            this.txCount++;
            const mine = tx;
            try { await this.beforeB?.(); return await fn(mine); } finally { tx = undefined; }
          },
          active: () => (this.txLost ? {} : tx) as never,
          peekSlot: async (p: string) => (this.slots.get(p) ?? null) as never,
          heldByOtherSink: (p: string) => this.held.has(p),
        },
        reserve: async (runId, action) => { this.reserveCalls.push(runId); return action(() => this.eligible); },
      seams: {
        split: true,
        readFence: () => this.fence(),
        readMounts: () => this.mounts(),
        statfs: () => this.statfs(),
        scanProcesses: names => this.scan(names),
        bindShared: this.fx.dataDir,
        env: {},
      },
    };
    Object.defineProperties(retire, {
      retireTestGate: { enumerable: true, get: () => (this.gate ? ENABLE_RETIRE_FOR_TEST : undefined) },
      activation: { enumerable: true, get: () => this.activation },
    });
    this.options = {
      bootstrap: { podUid: evidence.successor, runtime: evidence.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
      ownership, consumption: { result: "fresh", mayEvaluate: true }, signal: this.parent.signal,
      readEvidence: async () => bytes, kernelRelease: () => "6.1.0-test",
      now: () => this.clock,
      client: {
        listRecoveryHolds: async run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: this.openHolds } }) as never,
        getRunOwnership: async () => ({ status: this.runStatus }) as never,
      },
      git: {
        discoverEpochRetainedSources: async () => ({ groups: [], inventoryComplete: this.discoverComplete, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 }),
        inspectEpochRetainedGroups: async () => { throw new Error("unexpected inspection"); },
      },
      openDaemonGuard: async () => guard,
      retire,
    };
  }

  /**
   * The cache as the phase sees it. The uid split is process-wide env; this sandbox runs as the runner uid itself, so
   * the split-aware bare checks would refuse every bare. Only the discard (launch audit and wrapper) needs the split, and
   * its durable `removing` write must run without it, so the env is held for exactly the span between them.
   */
  splitGit(): NonNullable<Options["retire"]>["git"] {
    const cache = this.cache;
    return {
      assertNoUnsettledDeletes: () => cache.assertNoUnsettledDeletes(),
      readRetireScratchEvidence: o => cache.readRetireScratchEvidence(o),
      collectEpochScratchProtectedPaths: o => cache.collectEpochScratchProtectedPaths(o),
      withRetireScratchBare: (b, o, fn) => cache.withRetireScratchBare(b, o, fn),
      measureRetireScratchParent: (n, o) => cache.measureRetireScratchParent(n, o),
      discardRetireScratchParent: async input => {
        process.env.UZI_UID_SPLIT = "1";
        try {
          return await cache.discardRetireScratchParent({ ...input, ...(w.helperDeadlineMs === undefined ? {} : { deadline: Date.now() + w.helperDeadlineMs }),
            beforeEffect: async () => {
            delete process.env.UZI_UID_SPLIT;
            try { await input.beforeEffect(); } finally { process.env.UZI_UID_SPLIT = "1"; }
          } });
        } finally { delete process.env.UZI_UID_SPLIT; }
      },
    };
  }

  /** A fake spawn that strips the setpriv wrapper (single uid) and runs the real helper, or fakes a result. */
  installSpawn(): void {
    const internal = this.cache as unknown as { epochNativeTestDependencies?: unknown };
    const wrapLen = runnerCommand(process.execPath, [], true).args.length;
    internal.epochNativeTestDependencies = {
      spawn: ((_command: string, args: string[], options: { stdio: StdioOptions; env: NodeJS.ProcessEnv; cwd?: string; detached: boolean }) => {
        this.spawns.push({ args });
        if (this.outcome === "real") { this.onChild?.(); return spawn(process.execPath, args.slice(wrapLen), options); }
        const child = Object.assign(new EventEmitter(), {
          pid: 4242, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), exitCode: null, signalCode: null,
        });
        if (typeof this.outcome === "object") {
          const code = this.outcome.exit;
          setImmediate(() => {
            this.onChild?.();
            child.stdout.end(""); child.stderr.end("");
            child.emit("exit", code, null); child.emit("close", code, null);
          });
        }
        return child as unknown as ChildProcess;
      }) as unknown as typeof spawn,
      alive: (pid: number) => { if (this.outcome !== "real") return this.outcome === "hang"; try { process.kill(-pid, 0); return true; } catch { return false; } },
      kill: (pid: number) => { if (this.outcome === "real") { try { process.kill(-pid, "SIGKILL"); } catch { /* gone */ } } },
      joinMs: 60,
    };
  }

  git(...args: string[]): string { return execFileSync("git", ["-C", this.bare, ...args], { encoding: "utf8", env: ENV }).trim(); }
  setConfig(key: string, value: string, add = false): void { this.git("config", "--local", ...(add ? ["--add"] : []), key, value); }
  unsetConfig(key: string): void { try { this.git("config", "--local", "--unset-all", key); } catch { /* absent */ } }

  seed(spec: SeedSpec = {}): Seed {
    const n = spec.n ?? 1;
    const uuid = spec.uuid ?? randomUUID();
    const name = `.retire-${uuid}`;
    const parent = path.join(this.runnerRoot, name), clone = path.join(parent, "clone");
    const attemptId = formatAttemptId(new Date(Date.UTC(2026, 10, 5, 10, 10, n)), 1, "0123456789abcdef");
    const clonePath = path.join(this.runnerRoot, "slug", `issue-4263.attempt-${attemptId}`);
    const runId = spec.runId ?? `run-${n}`;
    let parentIno = "1", cloneIno = "1";
    if (spec.withParent !== false) {
      fs.mkdirSync(path.join(clone, "src", "deep"), { recursive: true });
      fs.writeFileSync(path.join(clone, "src", "deep", "f.txt"), "f".repeat(5000));
      fs.writeFileSync(path.join(clone, "top.txt"), "t");
      fs.chmodSync(parent, 0o700);
      parentIno = String(fs.lstatSync(parent, { bigint: true }).ino);
      cloneIno = String(fs.lstatSync(clone, { bigint: true }).ino);
    }
    const binding: RetireBinding = { v: 1, class: spec.cls ?? "owner_terminal", attemptId, runId, branch: BRANCH, clonePath,
      phase: spec.phase ?? "settled", parentIno, cloneIno, pod: "" };
    const raw = retireBindingValue(binding);
    assert.ok(parseRetireBinding(uuid, raw).ok);
    this.setConfig(retireBindingKey(uuid), raw);
    const ledger = spec.ledger ?? "retired";
    if (ledger !== "none") this.setConfig(`uzi-attempts.${BRANCH}.entry`, JSON.stringify({ attemptId, runId, clonePath, state: ledger }), true);
    return { uuid, name, parent, clone, clonePath, attemptId, runId, binding, raw };
  }

  bindingRaws(uuid: string): string[] {
    try { return this.git("config", "--local", "--get-all", retireBindingKey(uuid)).split("\n"); } catch { return []; }
  }
  phaseOf(uuid: string): string {
    const raws = this.bindingRaws(uuid);
    if (raws.length === 0) return "absent";
    const parsed = raws.length === 1 ? parseRetireBinding(uuid, raws[0]!) : undefined;
    return parsed?.ok ? parsed.binding.phase : "uncertain";
  }

  async run(): Promise<NonNullable<Awaited<ReturnType<typeof runPodEpochShadow>>>> {
    return (await runPodEpochShadow(this.options))!;
  }

  async cleanup(): Promise<void> {
    try { fs.closeSync(this.exFd); } catch { /* closed */ }
    await restoreTreeWritability(this.fx.dataDir).catch(() => undefined);
    this.fx.cleanup();
  }
}

let w: World;
beforeEach(async () => { w = new World(); await w.init(); });
afterEach(async () => { resetResidueQuarantineForTests(); delete process.env.UZI_UID_SPLIT; await w.cleanup(); });

const exists = (p: string): boolean => { try { fs.lstatSync(p); return true; } catch { return false; } };
const kept = (r: Awaited<ReturnType<World["run"]>>, name: string) => r.retire_report.parents.find(p => p.name === name);
/** Nothing changed: the parent is intact, its binding still reads `raw`, and no helper ran. */
function untouched(s: Seed, raw = s.raw): void {
  assert.ok(exists(path.join(s.clone, "top.txt")), "clone content intact");
  assert.deepEqual(w.bindingRaws(s.uuid), [raw], "binding unchanged");
}

describe("activation switch parser", () => {
  it("unset, empty and 1 are on; 0 is off; anything else refuses startup", () => {
    assert.equal(readRetireScratchReclaimActivation({}), true);
    assert.equal(readRetireScratchReclaimActivation({ UZI_RETIRE_SCRATCH_RECLAIM: "" }), true);
    assert.equal(readRetireScratchReclaimActivation({ UZI_RETIRE_SCRATCH_RECLAIM: "1" }), true);
    assert.equal(readRetireScratchReclaimActivation({ UZI_RETIRE_SCRATCH_RECLAIM: "0" }), false);
    for (const bad of ["true", "2", " 1", "0 ", "yes", "off"])
      assert.throws(() => readRetireScratchReclaimActivation({ UZI_RETIRE_SCRATCH_RECLAIM: bad }), /must be unset, empty, 0 or 1/, bad);
  });

  it("production deps carry activation, the reservation and the runner root, never the test sentinel or seams", () => {
    const calls: unknown[][] = [];
    const deps = buildPodEpochRetireDeps({ authority: {} as never, runner: { withSourceReservation: async (...a: unknown[]) => { calls.push(a); return "r"; } } as never,
      git: {} as never, dockerHost: undefined, dataDir: "/data", activation: true });
    assert.deepEqual(Object.keys(deps).sort(), ["activation", "authority", "git", "quiesce", "reserve", "runnerRoot"]);
    assert.equal(deps.runnerRoot, "/data/runner");
    assert.equal(buildPodEpochRetireDeps({ authority: {} as never, runner: {} as never, git: {} as never, dockerHost: undefined, dataDir: "/data" }).activation, false);
    return deps.reserve("run-x", async () => 1).then(r => { assert.equal(r, "r"); assert.equal(calls.length, 1); });
  });

  it("nothing under agent/src supplies the retire gate sentinel", async () => {
    const src = fileURLToPath(new URL("../src/", import.meta.url));
    const files = (await fsp.readdir(src, { recursive: true })).filter(f => /\.(ts|mts|cts|js|mjs|cjs)$/.test(f));
    const referencing: string[] = [];
    for (const f of files) if (/retireTestGate|retire-gate|ENABLE_RETIRE_FOR_TEST/.test(await fsp.readFile(path.join(src, f), "utf8"))) referencing.push(f);
    assert.deepEqual(referencing, ["pod-epoch-reclaim.ts"]);
  });
});

describe("reclaim of settled parents", PROC, () => {
  for (const cls of ["owner_terminal", "verified_successor"] as const) {
    it(`reclaims a settled ${cls} parent: helper empties it, the worker removes it, the binding is unset`, async () => {
      const s = w.seed({ cls });
      const result = await w.run();
      assert.equal(result.verdict, "eligible");
      assert.equal(result.retire_scratch.state, "ran", JSON.stringify(result.retire_scratch));
      assert.equal(result.retire_scratch.reclaimed, 1);
      assert.equal(result.retire_scratch.kept, 0);
      assert.equal(result.retire_scratch.bytes_state, "measured");
      assert.ok(result.retire_scratch.bytes > 0);
      assert.ok(!exists(s.parent), "parent gone");
      assert.deepEqual(w.bindingRaws(s.uuid), [], "binding unset");
      assert.equal(w.spawns.length, 1);
      assert.equal(w.pending, 0);
      assert.equal(w.handovers, 1);
      assert.deepEqual(kept(result, s.name), { name: s.name, outcome: "reclaimed", reason: "reclaimed", bytes: result.retire_scratch.bytes, bytesState: "measured" });
      assert.ok(w.steps.includes("removing-written") && w.steps.includes("unset-done"));
      // the settled value was replaced by removing before the helper ran
      assert.ok(w.steps.indexOf("removing-written") < w.steps.indexOf("unset"));
    });
  }

  it("reports per-phase times", async () => {
    w.seed();
    const result = await w.run();
    for (const k of ["scratch_phase_ms", "attempt_phase_ms", "retire_scratch_phase_ms", "evidence_wait_ms", "pass_ms"] as const)
      assert.equal(typeof result[k], "number", k);
  });

  it("with the switch off nothing is read, removed or unset and the phase says disabled", async () => {
    const s = w.seed();
    w.gate = false; w.activation = false;
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "not_run");
    assert.equal(result.retire_scratch.reason, "disabled");
    assert.deepEqual(w.reserveCalls, []);
    assert.equal(w.spawns.length, 0);
    untouched(s);
    assert.equal(result.retire_report.parents.length, 0);
  });

  it("without retire dependencies the phase is not_run", async () => {
    const s = w.seed();
    delete w.options.retire;
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "not_run");
    untouched(s);
  });

  it("a partial shadow verdict (incomplete inventory) runs no retire phase and the parents stay", async () => {
    const s = w.seed();
    w.discoverComplete = false;
    const result = await w.run();
    assert.equal(result.verdict, "partial");
    assert.equal(result.retire_scratch.state, "not_run");
    assert.deepEqual(w.reserveCalls, []);
    untouched(s);
  });

  it("does not run when the pass has no native reclaim scope", async () => {
    const s = w.seed();
    delete (w.options.ownership as { beginReclaim?: unknown }).beginReclaim;
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "not_run");
    untouched(s);
  });

  it("an unsettled runner delete anywhere in the process is fatal and keeps EX", async () => {
    const s = w.seed();
    (w.cache as unknown as { unsettledDeletes: Map<string, number> }).unsettledDeletes.set("/x", 1);
    await assert.rejects(w.run(), PodEpochSettlementError);
    assert.equal(w.handovers, 0);
    untouched(s);
  });
});

describe("keep reasons", PROC, () => {
  const cases: Array<[string, string, (s: Seed) => void, SeedSpec?]> = [
    ["conflict (duplicate binding key)", "conflict", s => w.setConfig(retireBindingKey(s.uuid), s.raw, true)],
    ["malformed binding", "malformed_binding", s => w.setConfig(retireBindingKey(s.uuid), "not json")],
    ["intent phase", "bound_intent", () => undefined, { phase: "intent" }],
    ["removing phase", "interrupted_removal", () => undefined, { phase: "removing" }],
    ["owner_terminal_covered class", "class_ineligible", () => undefined, { cls: "owner_terminal_covered" }],
    ["completion class", "class_ineligible", () => undefined, { cls: "completion" }],
    ["other class", "class_ineligible", () => undefined, { cls: "other" }],
    ["no ledger entry (an old compaction dropped it)", "ledger_missing", () => undefined, { ledger: "none" }],
    ["attempt id re-seeded as live", "ledger_mismatch", s => w.setConfig(`uzi-attempts.${BRANCH}.entry`,
      JSON.stringify({ attemptId: s.attemptId, runId: s.runId, clonePath: s.clonePath, state: "live" }), true)],
    ["ledger names another run", "ledger_mismatch", s => w.setConfig(`uzi-attempts.${BRANCH}.entry`,
      JSON.stringify({ attemptId: s.attemptId, runId: "other-run", clonePath: s.clonePath, state: "retired" }), true)],
    ["a journal names the parent", "protected_by_retained_source", s => w.setConfig("uzi-recovery.agent/other.clone",
      JSON.stringify({ runId: "x", clonePath: path.join(s.parent, "clone") }))],
    ["a journal names the binding's clone path", "protected_by_retained_source", s => w.setConfig("uzi-recovery.agent/other.clone",
      JSON.stringify({ runId: "x", clonePath: s.clonePath }))],
    ["a stopped marker for the run", "custody", s => w.setConfig(`uzi-stopped.${s.runId}.entry`, "{}")],
    ["the run holds a custody record", "custody", s => {
      const dir = path.join(w.fx.dataDir, "recovery", s.runId);
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(path.join(dir, "g1.json"), "{\"generation\":1}");
    }],
    ["a slot at the clone path is active", "source_use_held", s => { w.slots.set(s.clonePath, { state: "active", pending: null }); }],
    ["a slot has a pending footprint", "source_use_held", s => { w.slots.set(s.clonePath, { state: "ineligible", pending: {} }); }],
    ["another sink holds the clone", "source_use_held", s => { w.held.add(path.join(s.parent, "clone")); }],
    ["the canonical clone is present again", "canonical_present", s => fs.mkdirSync(s.clonePath)],
    ["the parent is newer than the predecessor", "newer_than_predecessor", () => { w.fence = () => ({ t0Ms: Date.now() - 3_600_000 }); }],
    ["a process holds the parent", "held", s => { w.scan = names => ({ held: new Set(names.filter(n => n === s.name)) }); }],
    ["a bind source lies inside the parent", "mount_changed", s => { w.binds = () => ({ state: "read", sources: [path.join(s.parent, "clone")] }); }],
    ["an extra entry beside clone", "unexpected_entries", s => fs.writeFileSync(path.join(s.parent, "other"), "")],
    ["the clone was moved away", "unexpected_entries", s => fs.renameSync(s.clone, path.join(w.runnerRoot, "moved"))],
    ["the clone inode differs from the binding", "identity_changed", s => { fs.renameSync(s.clone, path.join(w.runnerRoot, "moved")); fs.mkdirSync(s.clone); }],
    ["the parent is a symlink", "identity_changed", s => {
      const target = path.join(w.runnerRoot, "target");
      fs.renameSync(s.parent, target);
      fs.symlinkSync(target, s.parent);
    }],
    ["the parent inode differs from the binding", "identity_changed", s => {
      const moved = path.join(w.runnerRoot, "moved");
      fs.renameSync(s.parent, moved);
      fs.mkdirSync(s.parent, { mode: 0o700 });
      fs.renameSync(path.join(moved, "clone"), path.join(s.parent, "clone"));
    }],
    ["the run is not terminal", "custody", () => { w.runStatus = "running"; }],
    ["the run still has an open hold", "custody", () => { w.openHolds = 1; }],
  ];
  for (const [label, reason, mutate, spec] of cases) {
    it(`keeps everything: ${label} -> ${reason}`, async () => {
      const s = w.seed(spec);
      mutate(s);
      const result = await w.run();
      assert.equal(result.retire_scratch.reclaimed, 0, label);
      assert.equal(result.retire_scratch.kept_by_reason[reason], 1, `${label}: ${JSON.stringify(result.retire_scratch.kept_by_reason)}`);
      assert.equal(kept(result, s.name)?.reason, reason, label);
      assert.equal(kept(result, s.name)?.outcome, "kept", label);
      assert.equal(w.spawns.length, 0, `${label}: no helper`);
      assert.equal(w.pending, 0);
      // The binding is exactly what it was (a refusal before the `removing` write keeps `settled`).
      if (!/conflict|malformed/.test(label)) assert.deepEqual(w.bindingRaws(s.uuid), [s.raw], label);
      assert.ok(exists(s.parent), `${label}: the parent is still there`);
      assert.equal(result.verdict, "eligible", "a keep never alters the shadow verdict");
    });
  }

  it("a legacy unbound parent is not examined and stays", async () => {
    const parent = path.join(w.runnerRoot, `.retire-${randomUUID()}`);
    fs.mkdirSync(path.join(parent, "clone"), { recursive: true });
    const result = await w.run();
    assert.equal(result.retire_scratch.unbound, 1);
    assert.equal(result.retire_scratch.kept, 0);
    assert.deepEqual(result.retire_scratch.kept_by_reason, {});
    assert.equal(result.retire_scratch.examined, 0);
    assert.ok(exists(parent));
    assert.equal(w.spawns.length, 0);
  });

  it("a refusal after the removing write keeps removing (a not-live reservation between the write and the spawn)", async () => {
    const s = w.seed();
    w.stepHook = step => { if (step === "removing-written") w.eligible = false; };
    const result = await w.run();
    assert.equal(w.spawns.length, 0);
    assert.equal(kept(result, s.name)?.reason, "interrupted_removal");
    assert.equal(w.phaseOf(s.uuid), "removing");
    assert.ok(exists(path.join(s.clone, "top.txt")), "nothing was emptied");
  });
});

describe("whole-phase refusals keep everything", PROC, () => {
  const refusals: Array<[string, string, () => void]> = [
    ["pod-entry fence invalid", "pod_entry_invalid", () => { w.fence = () => ({ refused: "pod_entry_invalid" }); }],
    ["mounts unknown", "mount_unknown", () => { w.mounts = () => "mount_unknown"; }],
    ["a mount under the runner root", "mount_unexpected", () => { w.mounts = () => "mount_unexpected"; }],
    ["unsupported filesystem", "fs_unsupported", () => { w.statfs = () => "fs_unsupported"; }],
    ["bind inventory unknown", "bind_unknown", () => { w.binds = () => ({ state: "unknown" }); }],
    ["a bind source above the root", "bind_ancestor", () => { w.binds = () => ({ state: "read", sources: [path.dirname(w.runnerRoot)] }); }],
    ["not idle", "not_idle", () => { w.quiesce = async () => false; }],
    ["unreadable process scan", "self_descriptor_unreadable", () => { w.scan = () => ({ refused: "self_descriptor_unreadable" }); }],
  ];
  for (const [label, reason, mutate] of refusals) {
    it(`${label} -> ${reason}`, async () => {
      const s = w.seed();
      mutate();
      const result = await w.run();
      assert.equal(result.retire_scratch.state, "refused", label);
      assert.equal(result.retire_scratch.reason, reason, label);
      assert.equal(result.retire_scratch.reclaimed, 0);
      assert.deepEqual(w.reserveCalls, []);
      assert.equal(w.spawns.length, 0);
      untouched(s);
      assert.equal(result.verdict, "eligible");
    });
  }

  it("an unreadable binding set (over the cap) refuses the phase", async () => {
    const s = w.seed();
    for (let i = 0; i < 70; i++) w.setConfig(retireBindingKey(randomUUID()), s.raw);
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "evidence_unknown");
    untouched(s);
  });

  it("an undecodable restore holder refuses the phase (it may protect everything)", async () => {
    const s = w.seed();
    w.setConfig(`uzi-restore.${BRANCH}.journal`, "{\"v\":1}");
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "protected_unknown");
    untouched(s);
  });

  it("an undecodable record in a protected family refuses the phase", async () => {
    const s = w.seed();
    w.setConfig("uzi-recovery.agent/z.clone", "not json and not a path");
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "evidence_unknown");
    untouched(s);
  });

  it("a runner root without the sticky bit refuses the phase", async () => {
    const s = w.seed();
    fs.chmodSync(w.runnerRoot, 0o700);
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "root_identity");
    untouched(s);
  });

  it("a split-inactive worker refuses the phase", async () => {
    const s = w.seed();
    (w.options.retire!.seams as { split?: boolean }).split = false;
    const result = await w.run();
    assert.equal(result.retire_scratch.reason, "split_inactive");
    untouched(s);
  });
});

describe("arrivals between the nomination and the locked re-check", PROC, () => {
  const arrivals: Array<[string, string, (s: Seed) => void]> = [
    ["a journal naming the parent", "protected_by_retained_source", s => w.setConfig("uzi-recovery.agent/late.clone", JSON.stringify({ runId: "x", clonePath: s.parent }))],
    ["a custody record", "custody", s => {
      const dir = path.join(w.fx.dataDir, "recovery-settlement", s.runId);
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(path.join(dir, "r1.json"), "{\"predecessorGeneration\":1}");
    }],
    ["a source-use slot", "source_use_held", s => { w.slots.set(s.clonePath, { state: "active", pending: null }); }],
    ["a residue quarantine", "custody", () => { latchResidueQuarantine({ source: "test" } as never); }],
    ["a canonical clone", "canonical_present", s => fs.mkdirSync(s.clonePath)],
    ["a running process", "held", s => { w.scan = names => ({ held: new Set(names.filter(n => n === s.name)) }); }],
  ];
  for (const [label, reason, arrive] of arrivals) {
    it(`${label} is kept`, async () => {
      const s = w.seed();
      let armed = false;
      w.beforeB = () => { if (!armed) { armed = true; arrive(s); } };
      const result = await w.run();
      assert.equal(kept(result, s.name)?.reason, reason, label);
      assert.equal(w.spawns.length, 0);
      untouched(s);
    });
  }

  it("a reservation that is no longer eligible before the spawn has no effect", async () => {
    const s = w.seed();
    w.beforeB = () => { w.eligible = false; };
    const result = await w.run();
    assert.equal(kept(result, s.name)?.reason, "epoch_not_eligible");
    assert.equal(w.spawns.length, 0);
    untouched(s);
  });

  it("a lost authority transaction before the spawn has no effect", async () => {
    const s = w.seed();
    w.txLost = true;
    const result = await w.run();
    assert.equal(w.spawns.length, 0);
    assert.notEqual(kept(result, s.name)?.outcome, "reclaimed");
    assert.ok(exists(path.join(s.clone, "top.txt")));
    // A refusal before the removing write keeps the settled phase.
    assert.equal(w.phaseOf(s.uuid), "settled");
  });

  it("a refused reservation (the run is busy) keeps the parent", async () => {
    const s = w.seed();
    w.options.retire!.reserve = (async () => undefined) as never;
    const result = await w.run();
    assert.equal(kept(result, s.name)?.reason, "epoch_not_eligible");
    untouched(s);
  });

  it("an unreadable ownership response keeps the parent as custody", async () => {
    const s = w.seed();
    w.options.client.getRunOwnership = (async () => { throw new Error("api down"); }) as never;
    const result = await w.run();
    assert.equal(kept(result, s.name)?.reason, "custody");
    untouched(s);
  });
});

describe("budget", PROC, () => {
  it("a phase that cannot be admitted (under 30 s left) defers everything and changes nothing", async () => {
    const s = w.seed();
    let calls = 0;
    // The pass starts at clock 1_000_000 and hard-stops 600 s later: leave 20 s for the phase.
    w.fence = () => { calls++; return FUTURE_FENCE(); };
    const start = w.clock;
    const open = w.options.openDaemonGuard!;
    w.options.openDaemonGuard = async (...a) => { w.clock = start + 580_000; return open(...a); };
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    assert.equal(calls, 0, "nothing was read");
    assert.deepEqual(w.reserveCalls, []);
    assert.equal(w.spawns.length, 0);
    untouched(s);
    assert.equal(result.retire_report.reason, "budget_deferred");
    // Deferral never alters the shadow result.
    assert.equal(result.verdict, "eligible");
    assert.equal(result.reason, "shadow_complete");
    assert.equal(result.bytes_state, "measured", "the shadow measurement is untouched");
    assert.deepEqual(result.partial_causes, {});
  });

  it("when the window closes before a parent's first effect that parent and the later ones are deferred untouched", async () => {
    const first = w.seed({ n: 1 }), second = w.seed({ n: 2 }), third = w.seed({ n: 3 });
    const order = [first, second, third].sort((a, b) => (a.name < b.name ? -1 : 1));
    // The helper of the first sorted parent runs "for 110 s": the window (120 s) then has 10 s left.
    w.onChild = () => { w.clock += 110_000; };
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 1);
    assert.equal(result.retire_scratch.deferred, 2);
    assert.equal(result.retire_scratch.kept, 0, "a deferral is not a keep");
    assert.deepEqual(result.retire_scratch.kept_by_reason, {});
    assert.equal(result.retire_scratch.state, "stopped");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    assert.ok(!exists(order[0]!.parent));
    for (const s of order.slice(1)) {
      untouched(s);
      assert.equal(kept(result, s.name)?.reason, "budget_deferred");
    }
    assert.equal(w.spawns.length, 1);
    assert.equal(result.verdict, "eligible", "deferral never alters the shadow verdict");
    assert.equal(result.reason, "shadow_complete");
  });

  it("a helper that ends over budget (exit 8) leaves residue, keeps removing and stops new parents", async () => {
    const a = w.seed({ n: 1 }), b = w.seed({ n: 2 });
    const order = [a, b].sort((x, y) => (x.name < y.name ? -1 : 1));
    w.outcome = { exit: 8 };
    const result = await w.run();
    assert.equal(result.retire_scratch.residue, 1);
    assert.equal(result.retire_scratch.reclaimed, 0);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(w.spawns.length, 1);
    assert.equal(w.phaseOf(order[0]!.uuid), "removing", "the binding stays removing");
    assert.equal(kept(result, order[0]!.name)?.reason, "interrupted_removal");
    assert.equal(w.phaseOf(order[1]!.uuid), "settled");
    untouched(order[1]!);
    assert.equal(result.retire_scratch.state, "stopped");
    assert.equal(w.pending, 0);
    assert.equal(w.handovers, 1, "a joined residue is not fatal");
  });

  it("a helper that fails otherwise is residue too, and a later pass never retries it", async () => {
    const s = w.seed();
    w.outcome = { exit: 4 };
    const first = await w.run();
    assert.equal(first.retire_scratch.residue, 1);
    assert.equal(w.phaseOf(s.uuid), "removing");
    w.outcome = "real";
    const second = await w.run();
    assert.equal(kept(second, s.name)?.reason, "interrupted_removal");
    assert.equal(w.spawns.length, 1, "a removing binding is never retried");
    assert.equal(w.phaseOf(s.uuid), "removing");
  });

  it("at most 64 bound parents are examined, the rest deferred", async () => {
    // The binding cap is 64 per bare, so a second bare carries the overflow.
    const second = makeFixture();
    try {
      const bare2 = await w.cache.ensureClone(second.originPath);
      fs.chmodSync(path.dirname(bare2), 0o700);
      fs.chmodSync(bare2, 0o700);
      for (let i = 0; i < 40; i++) w.seed({ n: 1 + (i % 50), withParent: false, ledger: "none" });
      const first = w.bare;
      w.bare = bare2;
      for (let i = 0; i < 26; i++) w.seed({ n: 1 + (i % 50), withParent: false, ledger: "none" });
      w.bare = first;
      await runCapped();
    } finally { second.cleanup(); }
  });

  async function runCapped(): Promise<void> {
    const result = await w.run();
    assert.equal(result.retire_scratch.examined, 64);
    assert.equal(result.retire_scratch.deferred, 2);
  }

  it("an unjoined helper makes the pass fatal and keeps EX", async () => {
    const s = w.seed();
    w.outcome = "hang";
    w.helperDeadlineMs = 6500;
    await assert.rejects(w.run(), PodEpochSettlementError);
    assert.equal(w.handovers, 0, "EX is kept");
    assert.equal(w.phaseOf(s.uuid), "removing");
    assert.ok(w.stopped, "the scope is stopped");
  });
});

describe("a whole-phase refusal inside the lock halts the loop", PROC, () => {
  const pair = (): [Seed, Seed] => {
    const sorted = [w.seed({ n: 1 }), w.seed({ n: 2 })].sort((x, y) => (x.name < y.name ? -1 : 1));
    return [sorted[0]!, sorted[1]!];
  };
  /** True exactly once, the first time it is asked after the first parent's locked section began. */
  const armOnce = (): (() => boolean) => {
    let armed = false, fired = false;
    w.beforeB = () => { armed = true; };
    return () => { if (!armed || fired) return false; fired = true; return true; };
  };
  const assertHalted = async (reason: string): Promise<void> => {
    const [first, second] = pair2;
    const result = await w.run();
    assert.equal(result.retire_scratch.state, "stopped", JSON.stringify(result.retire_scratch));
    assert.equal(result.retire_scratch.reason, reason);
    assert.equal(result.retire_scratch.reclaimed, 0);
    assert.equal(result.retire_scratch.kept, 1);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(w.spawns.length, 0, "no helper ran for any parent");
    assert.equal(w.pending, 0);
    untouched(first); untouched(second);
    assert.equal(result.verdict, "eligible");
  };
  let pair2: [Seed, Seed];

  it("a parent-level idle check that fails once stops the phase before the next parent", async () => {
    pair2 = pair();
    const fire = armOnce();
    w.quiesce = async () => !fire();
    await assertHalted("not_idle");
  });

  it("a process-scan refusal for one parent stops the phase before the next parent", async () => {
    pair2 = pair();
    const fire = armOnce();
    w.scan = names => (names.length === 1 && fire() ? { refused: "self_descriptor_unreadable" } : { held: new Set() });
    await assertHalted("self_descriptor_unreadable");
  });

  it("a daemon guard that cannot refresh its inventory stops the phase before the next parent", async () => {
    pair2 = pair();
    const fire = armOnce();
    const open = w.options.openDaemonGuard!;
    w.options.openDaemonGuard = async (...a) => {
      const guard = await open(...a);
      Object.defineProperty(guard, "refreshInventory", { get: () => (fire() ? undefined : async () => undefined) });
      return guard;
    };
    await assertHalted("daemon_changed");
  });

  it("a mount table that cannot be re-read for one parent stops the phase before the next parent", async () => {
    pair2 = pair();
    const fire = armOnce();
    w.mounts = () => (fire() ? "mount_unknown" : "ok");
    await assertHalted("mount_unknown");
  });

  it("an unreadable root mount id for one parent stops the phase with mount_unknown", async () => {
    pair2 = pair();
    (w.options.retire!.seams as { rootMntId?: () => string | undefined }).rootMntId = () => undefined;
    await assertHalted("mount_unknown");
  });

  it("a bind inventory that cannot be read inside the lock stops the phase before the next parent", async () => {
    pair2 = pair();
    const fire = armOnce();
    w.binds = () => (fire() ? { state: "unknown" } : { state: "read", sources: [] });
    await assertHalted("bind_unknown");
  });

  it("a bind inventory refused inside the lock stops the phase before the next parent", async () => {
    pair2 = pair();
    const fire = armOnce();
    w.binds = () => (fire() ? { state: "read", sources: [path.dirname(w.runnerRoot)] } : { state: "read", sources: [] });
    await assertHalted("bind_ancestor");
  });
});

describe("protection arriving in another store between the nomination and the locked section", PROC, () => {
  it("a record in another bare naming the parent is seen by the re-read inside S and keeps the parent", async () => {
    const s = w.seed();
    const second = makeFixture();
    try {
      const bare2 = await w.cache.ensureClone(second.originPath);
      fs.chmodSync(path.dirname(bare2), 0o700);
      fs.chmodSync(bare2, 0o700);
      w.beforeB = () => {
        const first = w.bare;
        w.bare = bare2;
        try { w.setConfig("uzi-recovery.agent/late.clone", JSON.stringify({ runId: "x", clonePath: s.parent })); } finally { w.bare = first; }
      };
      const result = await w.run();
      assert.equal(kept(result, s.name)?.reason, "protected_by_retained_source");
      assert.equal(w.spawns.length, 0);
      untouched(s);
    } finally { second.cleanup(); }
  });

  // The custody stores (not the bares' config) are read only by the locked examine, so a record there naming only the
  // binding's clone path (never the parent) is invisible to the nomination and to the re-read of the bares inside S.
  const storeRecord = (name: string, body: unknown): void => {
    const dir = path.join(w.fx.dataDir, "recovery");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, name), JSON.stringify(body));
  };

  it("a recovery-store record arriving after the nomination that names only the clone path keeps the parent", async () => {
    const s = w.seed();
    w.beforeB = () => { storeRecord("late.json", { runId: "x", clonePath: s.clonePath }); };
    const result = await w.run();
    assert.equal(kept(result, s.name)?.reason, "protected_by_retained_source");
    assert.equal(w.spawns.length, 0);
    assert.equal(w.phaseOf(s.uuid), "settled");
    untouched(s);
  });

});

describe("budget inside the locked section", PROC, () => {
  it("a recovery under an exhausted window is deferred and the binding stays", async () => {
    const s = w.seed({ withParent: false });
    w.beforeB = () => { w.clock += 110_000; };
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(result.retire_scratch.kept, 0);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  it("with 6 to 20 s left at the pre-helper check no helper is spawned and the parent is deferred", async () => {
    const s = w.seed();
    w.beforeB = () => { w.clock += 105_000; };
    const result = await w.run();
    assert.equal(w.spawns.length, 0);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(result.retire_scratch.kept, 0);
    assert.deepEqual(result.retire_scratch.kept_by_reason, {});
    untouched(s);
  });

  /** Replaces the measurement and records the entry cap every helper launch was given. */
  const fakeMeasure = (measure: { bytes: number; bytesState: "measured" | "partial" | "unknown"; entries: number }): number[] => {
    const git = w.options.retire!.git;
    const caps: number[] = [];
    const real = git.discardRetireScratchParent;
    git.discardRetireScratchParent = input => { caps.push(input.maxEntries); return real(input); };
    git.measureRetireScratchParent = async () => measure;
    return caps;
  };

  it("an incomplete measurement charges the whole entry budget: later parents are deferred, no cap exceeds what was left", async () => {
    const seeds = [w.seed({ n: 1 }), w.seed({ n: 2 }), w.seed({ n: 3 })].sort((x, y) => (x.name < y.name ? -1 : 1));
    const caps = fakeMeasure({ bytes: 0, bytesState: "partial", entries: 200_000 });
    const result = await w.run();
    assert.deepEqual(caps, [2_000_000]);
    assert.equal(result.retire_scratch.reclaimed, 1);
    assert.equal(result.retire_scratch.deferred, 2);
    assert.equal(result.retire_scratch.kept, 0);
    assert.equal(result.retire_scratch.state, "stopped");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    for (const s of seeds.slice(1)) { untouched(s); assert.equal(kept(result, s.name)?.reason, "budget_deferred"); }
  });

  it("a complete measurement charges what the walk saw: the next helper is capped to what remains", async () => {
    const seeds = [w.seed({ n: 1 }), w.seed({ n: 2 }), w.seed({ n: 3 })].sort((x, y) => (x.name < y.name ? -1 : 1));
    const caps = fakeMeasure({ bytes: 10, bytesState: "measured", entries: 1_500_000 });
    const result = await w.run();
    assert.deepEqual(caps, [1_500_001, 499_999]);
    assert.equal(caps.reduce((a, b) => a + b, 0) <= 2_000_000, true);
    assert.equal(result.retire_scratch.reclaimed, 2);
    assert.equal(result.retire_scratch.deferred, 1);
    untouched(seeds[2]!);
  });

  it("the measurement runs before the budget check: a measure that uses the window defers the parent without a helper", async () => {
    const s = w.seed();
    w.options.retire!.git.measureRetireScratchParent = async () => { w.clock += 105_000; return { bytes: 1, bytesState: "measured", entries: 3 }; };
    const result = await w.run();
    assert.equal(w.spawns.length, 0);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(result.retire_scratch.kept, 0);
    untouched(s);
  });


  it("a rejection before the removing write is a refusal that keeps settled", async () => {
    const s = w.seed();
    w.stepHook = step => { if (step === "removing-write") throw new Error("injected before the write"); };
    const result = await w.run();
    assert.equal(w.spawns.length, 0);
    assert.equal(kept(result, s.name)?.reason, "epoch_not_eligible");
    assert.equal(w.phaseOf(s.uuid), "settled");
    untouched(s);
  });
});

describe("alias-aware protection at the runner root itself", () => {
  const root = "/data/runner";
  const uuid = randomUUID();
  const name = `.retire-${uuid}`;
  const attemptId = formatAttemptId(new Date(Date.UTC(2026, 10, 5, 10, 10, 1)), 1, "0123456789abcdef");
  const clonePath = `${root}/slug/issue-4263.attempt-${attemptId}`;
  const binding: RetireBinding = { v: 1, class: "owner_terminal", attemptId, runId: "run-1", branch: BRANCH, clonePath,
    phase: "settled", parentIno: "1", cloneIno: "1", pod: "" };
  const raw = retireBindingValue(binding);
  const evidenceWith = (protectedPaths: string[]): RetireScratchBareEvidence => ({
    barePath: "/bare", bindings: [{ key: retireBindingKey(uuid), uuid, raw, parsed: binding }],
    ledger: [{ branch: BRANCH, raw: JSON.stringify({ attemptId, runId: "run-1", clonePath, state: "retired" }) }],
    protectedPaths, restoreBranches: [], recovery: [], stoppedRuns: [],
  });
  const shared = "/data/runner-shared";

  it("protectedLeaves sees a record written through the shared alias", () => {
    assert.deepEqual([...protectedLeaves([`${shared}/${name}/clone`], root, [name])], [name]);
    assert.deepEqual([...protectedLeaves([`${shared}/.retire-${randomUUID()}/clone`], root, [name])], []);
  });

  it("a record naming the parent through the alias keeps the binding", () => {
    assert.deepEqual(evaluateRetireBinding({ evidence: evidenceWith([`${shared}/${name}/clone`]), uuid, runnerRoot: root, expectRaw: raw }),
      { kind: "keep", reason: "protected_by_retained_source" });
  });

  it("a record naming the clone path through the alias keeps the binding", () => {
    assert.deepEqual(evaluateRetireBinding({ evidence: evidenceWith([`${shared}/slug/issue-4263.attempt-${attemptId}/sub`]), uuid, runnerRoot: root, expectRaw: raw }),
      { kind: "keep", reason: "protected_by_retained_source" });
    assert.equal(evaluateRetireBinding({ evidence: evidenceWith([`${shared}/other`]), uuid, runnerRoot: root, expectRaw: raw }).kind, "ready");
  });

  // Non-canonical spellings of the alias must not slip past the alias mapping.
  const spellings = [`/data//runner-shared`, `/data/./runner-shared`, `/data/x/../runner-shared`, `//data/runner-shared`];
  for (const base of spellings) {
    it(`a clone path recorded as ${base}/... is protected and unresolved for recovery`, () => {
      const recorded = [`${base}/slug/issue-4263.attempt-${attemptId}/sub`];
      assert.deepEqual(evaluateRetireBinding({ evidence: evidenceWith(recorded), uuid, runnerRoot: root, expectRaw: raw }),
        { kind: "keep", reason: "protected_by_retained_source" });
      assert.equal(evaluateRetireRecovery({ evidence: evidenceWith(recorded), uuid, runnerRoot: root, expectRaw: raw, canonical: { present: false } }), "unresolved");
    });
  }

  it("the locked stores check is alias-aware for the parent and the clone path", () => {
    assert.equal(storesNameRetireParent([`${shared}/${name}/clone`], root, uuid, clonePath), true);
    assert.equal(storesNameRetireParent([`${shared}/slug/issue-4263.attempt-${attemptId}`], root, uuid, clonePath), true);
    assert.equal(storesNameRetireParent([`${shared}/other`], root, uuid, clonePath), false);
    // The compared path may itself be spelled through the alias while the record is canonical.
    assert.equal(storesNameRetireParent([`${root}/slug/issue-4263.attempt-${attemptId}`], root, uuid, `${shared}/slug/issue-4263.attempt-${attemptId}`), true);
  });

  it("recovery case 1 does not unset when the clone path is named through the alias", () => {
    const canonical = { present: false };
    assert.equal(evaluateRetireRecovery({ evidence: evidenceWith([]), uuid, runnerRoot: root, expectRaw: raw, canonical }), "case1");
    assert.equal(evaluateRetireRecovery({ evidence: evidenceWith([`${shared}/slug/issue-4263.attempt-${attemptId}`]), uuid, runnerRoot: root, expectRaw: raw, canonical }), "unresolved");
  });
});

describe("recovery of a gone parent", PROC, () => {
  it("case 1: a settled binding whose parent is gone and whose ledger says retired is unset after the root sync", async () => {
    const s = w.seed({ withParent: false });
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 1);
    assert.deepEqual(w.bindingRaws(s.uuid), []);
    assert.equal(w.spawns.length, 0);
    assert.ok(w.steps.indexOf("recover-root-sync") < w.steps.indexOf("unset"));
    assert.ok(!w.steps.includes("recover-clone-dir-sync"));
  });

  it("case 1 also resolves a removing binding whose parent vanished (nobody names the clone)", async () => {
    const s = w.seed({ withParent: false, phase: "removing" });
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 1);
    assert.deepEqual(w.bindingRaws(s.uuid), []);
  });

  it("case 2: an intent binding, parent gone, the canonical clone present and still journaled, syncs both directories then unsets", async () => {
    const s = w.seed({ withParent: false, phase: "intent", ledger: "none" });
    fs.mkdirSync(s.clonePath);
    const ino = String(fs.lstatSync(s.clonePath, { bigint: true }).ino);
    const intent: RetireBinding = { ...s.binding, cloneIno: ino };
    w.setConfig(retireBindingKey(s.uuid), retireBindingValue(intent));
    w.setConfig(`uzi-attempts.${BRANCH}.entry`, JSON.stringify({ attemptId: s.attemptId, runId: s.runId, clonePath: s.clonePath, state: "live" }), true);
    w.setConfig(`uzi-recovery.${BRANCH}.clone`, JSON.stringify({ runId: s.runId, clonePath: s.clonePath, attemptId: s.attemptId }));
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 1, JSON.stringify(result.retire_scratch));
    assert.deepEqual(w.bindingRaws(s.uuid), []);
    assert.ok(w.steps.indexOf("recover-clone-dir-sync") < w.steps.indexOf("recover-root-sync"));
    assert.ok(exists(s.clonePath), "the canonical clone is untouched");
  });

  for (const step of ["recover-root-sync", "unset"] as const) {
    it(`a crash at ${step} leaves the binding and a later pass finishes the job`, async () => {
      const s = w.seed({ withParent: false });
      let thrown = false;
      w.stepHook = st => { if (st === step && !thrown) { thrown = true; throw new Error(`injected ${step}`); } };
      const first = await w.run();
      assert.equal(first.retire_scratch.recovered, 0);
      assert.equal(first.retire_scratch.kept_by_reason.metadata_unresolved, 1);
      assert.deepEqual(w.bindingRaws(s.uuid), [s.raw], "still bound");
      const second = await w.run();
      assert.equal(second.retire_scratch.recovered, 1);
      assert.deepEqual(w.bindingRaws(s.uuid), []);
    });
  }

  it("the completion window (removing, parent gone, the journal still names the clone) is unresolved and not unset", async () => {
    const s = w.seed({ withParent: false, phase: "removing" });
    w.setConfig(`uzi-recovery.${BRANCH}.clone`, JSON.stringify({ runId: s.runId, clonePath: s.clonePath, attemptId: s.attemptId }));
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.equal(result.retire_scratch.kept_by_reason.metadata_unresolved, 1);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  it("a removing binding whose clone was renamed back is never unset", async () => {
    const s = w.seed({ withParent: false, phase: "removing" });
    fs.mkdirSync(s.clonePath);
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  it("a gone parent whose ledger lost the retired entry is unresolved", async () => {
    const s = w.seed({ withParent: false, ledger: "none" });
    const result = await w.run();
    assert.equal(result.retire_scratch.kept_by_reason.metadata_unresolved, 1);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  it("a gone parent is not recovered when the switch is off", async () => {
    const s = w.seed({ withParent: false });
    w.gate = false; w.activation = false;
    await w.run();
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });
});

describe("the startup inspector reports the pass", PROC, () => {
  const rowsOf = () => w.logs.filter(l => l.message === "runner retire scratch outcome").map(l => l.fields);
  const totalOf = () => w.logs.filter(l => l.message === "runner retire scratch total").map(l => l.fields);
  const inspect = async (reclaim?: { enabled: boolean; report?: RetireScratchPassReport }) => {
    w.logs.length = 0;
    await w.cache.inspectRetireScratchAtStartup(Date.now() + 60_000, { outcomes: true, ...(reclaim ? { reclaim } : {}) });
  };

  it("emits reclaimed rows and counts reclaimed parents and bytes in the total", async () => {
    const gone = w.seed({ n: 1 });
    const stays = w.seed({ n: 2, cls: "completion" });
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 1);
    await inspect({ enabled: true, report: result.retire_report });
    const rows = rowsOf();
    assert.equal(rows.length, 2);
    const reclaimedRow = rows.find(r => r.outcome === "reclaimed")!;
    assert.equal(reclaimedRow.name, gone.name);
    assert.equal(reclaimedRow.reason, "reclaimed");
    assert.equal(reclaimedRow.bytes, result.retire_scratch.bytes);
    const keptRow = rows.find(r => r.outcome === "kept")!;
    assert.equal(keptRow.name, stays.name);
    assert.equal(keptRow.reason, "class_ineligible");
    const [total] = totalOf();
    assert.equal(total!.reclaimed, 1);
    assert.equal(total!.reclaimed_bytes, result.retire_scratch.bytes);
    assert.equal(total!.kept, 1);
    assert.deepEqual(total!.kept_by_reason, { class_ineligible: 1 });
  });

  it("binding-aware reasons without a pass: intent, removing, malformed, settled with the switch off or no epoch", async () => {
    const intent = w.seed({ n: 1, phase: "intent" }), removing = w.seed({ n: 2, phase: "removing" });
    const settled = w.seed({ n: 3 }), junk = w.seed({ n: 4 });
    w.setConfig(retireBindingKey(junk.uuid), "not json");
    const unbound = path.join(w.runnerRoot, `.retire-${randomUUID()}`);
    fs.mkdirSync(path.join(unbound, "clone"), { recursive: true });
    const reasons = async (reclaim?: { enabled: boolean; report?: RetireScratchPassReport }) => {
      await inspect(reclaim);
      return Object.fromEntries(rowsOf().map(r => [r.name as string, r.reason]));
    };
    const off = await reasons({ enabled: false });
    assert.equal(off[intent.name], "bound_intent");
    assert.equal(off[removing.name], "interrupted_removal");
    assert.equal(off[junk.name], "malformed_binding");
    assert.equal(off[settled.name], "disabled");
    assert.equal(off[path.basename(unbound)], "no_disposition_evidence");
    const noEpoch = await reasons({ enabled: true });
    assert.equal(noEpoch[settled.name], "epoch_not_eligible");
    const deferred = await reasons({ enabled: true, report: { state: "refused", reason: "budget_deferred", parents: [] } });
    assert.equal(deferred[settled.name], "budget_deferred");
    assert.equal(totalOf()[0]!.removing_bindings, 1);
  });

  it("with outcomes off only the count-only warning is written", async () => {
    w.seed();
    w.logs.length = 0;
    await w.cache.inspectRetireScratchAtStartup(Date.now() + 60_000, { outcomes: false, reclaim: { enabled: true } });
    assert.deepEqual(w.logs.map(l => l.message), ["runner retire scratch retained"]);
  });
});

describe("a real retire-produced binding", PROC, () => {
  it("reclaims the settled owner_terminal parent a simulated split retirement left behind", async (t: TestContext) => {
    const owner = randomUUID();
    const rename = fsp.rename.bind(fsp);
    t.mock.method(fsp, "rename", async (...args: Parameters<typeof fsp.rename>) => {
      if (String(args[1]).startsWith(path.join(w.fx.dataDir, "runner-quarantine"))) throw Object.assign(new Error("cross device"), { code: "EXDEV" });
      return rename(...args);
    });
    const attemptId = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
    const split = w.cache, bare = w.bare;
    const clone = await split.createOrAttachRunnerClone(bare, 4263, noProofReseed, owner, false, undefined,
      { attemptId, isLive: () => false, beforeSeed: async () => {} });
    await split.markRecoveryCapture(bare, clone.path, BRANCH, owner, attemptId);
    fs.writeFileSync(path.join(clone.path, "owner.txt"), "owner bytes\n");
    await split.retireRunnerClone(bare, clone.path, BRANCH, owner, { discard: true, attemptId, dispositionClass: "owner_terminal" });
    fs.chmodSync(w.runnerRoot, 0o1700);
    const parents = fs.readdirSync(w.runnerRoot).filter(n => n.startsWith(".retire-"));
    assert.equal(parents.length, 1);
    const uuid = parents[0]!.slice(".retire-".length);
    const raw = execFileSync("git", ["-C", bare, "config", "--local", "--get", retireBindingKey(uuid)], { encoding: "utf8", env: ENV }).trim();
    const decoded = parseRetireBinding(uuid, raw);
    assert.ok(decoded.ok && decoded.binding.phase === "settled" && decoded.binding.class === "owner_terminal");
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 1, JSON.stringify(result.retire_scratch));
    assert.ok(!exists(path.join(w.runnerRoot, parents[0]!)));
    assert.deepEqual(w.bindingRaws(uuid), [], "the binding is unset");
  });
});


describe("pass-wide protection of the binding's original clone path", PROC, () => {
  it("a record in ANOTHER bare naming the clone path keeps the parent although the owning bare names nothing", async () => {
    const s = w.seed();
    const second = makeFixture();
    try {
      const bare2 = await w.cache.ensureClone(second.originPath);
      fs.chmodSync(path.dirname(bare2), 0o700);
      fs.chmodSync(bare2, 0o700);
      const first = w.bare;
      w.bare = bare2;
      try { w.setConfig("uzi-recovery.agent/other.clone", JSON.stringify({ runId: "x", clonePath: s.clonePath })); } finally { w.bare = first; }
      const result = await w.run();
      assert.equal(kept(result, s.name)?.reason, "protected_by_retained_source");
      assert.equal(result.retire_scratch.reclaimed, 0);
      assert.equal(w.spawns.length, 0);
      untouched(s);
    } finally { second.cleanup(); }
  });

  it("the binding and the attempt ledger of the candidate do not protect it (they name the clone path by design)", async () => {
    const s = w.seed();
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 1, JSON.stringify(result.retire_scratch));
    assert.ok(!exists(s.parent));
  });
});

describe("a non-canonical ledger value naming the attempt outranks an earlier canonical retired entry", PROC, () => {
  const variants: Array<[string, (s: Seed) => string]> = [
    ["reordered keys", s => JSON.stringify({ runId: s.runId, attemptId: s.attemptId, clonePath: s.clonePath, state: "live" })],
    ["whitespace", s => JSON.stringify({ attemptId: s.attemptId, runId: s.runId, clonePath: s.clonePath, state: "live" }, null, 1)],
    ["an unknown field", s => JSON.stringify({ attemptId: s.attemptId, runId: s.runId, clonePath: s.clonePath, state: "live", extra: 1 })],
    // The attempt id (and the clone path embedding it) spelled with JSON unicode escapes: it parses to the same id but
    // never appears literally in the raw value.
    ["JSON-escaped attemptId and clonePath", s => JSON.stringify({ attemptId: s.attemptId, runId: s.runId, clonePath: s.clonePath, state: "live" }).split(s.attemptId).join(s.attemptId.replace(/T/g, "\\u0054"))],
  ];
  for (const [label, value] of variants) {
    it(`a later live entry with ${label} keeps the settled parent`, async () => {
      const s = w.seed();
      w.setConfig(`uzi-attempts.${BRANCH}.entry`, value(s), true);
      const result = await w.run();
      assert.equal(result.retire_scratch.kept_by_reason.ledger_mismatch, 1, JSON.stringify(result.retire_scratch.kept_by_reason));
      assert.equal(result.retire_scratch.reclaimed, 0);
      assert.equal(w.spawns.length, 0);
      untouched(s);
    });

    it(`a later live entry with ${label} leaves the binding of a gone parent unrecovered`, async () => {
      const s = w.seed({ withParent: false });
      w.setConfig(`uzi-attempts.${BRANCH}.entry`, value(s), true);
      const result = await w.run();
      assert.equal(result.retire_scratch.recovered, 0);
      assert.equal(result.retire_scratch.kept_by_reason.metadata_unresolved, 1);
      assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
    });
  }
});

describe("the phase window bounds the pre-effect reads", PROC, () => {
  it("evidence, protected-store and bare-lock deadlines sit inside the phase window, and a read that spends the window defers the parent", async () => {
    const s = w.seed();
    const git = w.options.retire!.git;
    const seen: Record<string, number> = {};
    const evidenceRead = git.readRetireScratchEvidence, protectedRead = git.collectEpochScratchProtectedPaths, bareLock = git.withRetireScratchBare;
    git.readRetireScratchEvidence = async o => { seen.evidence = o.deadline - Date.now(); const r = await evidenceRead(o); w.clock += 125_000; return r; };
    git.collectEpochScratchProtectedPaths = o => { seen.protected = o.deadline - Date.now(); return protectedRead(o); };
    git.withRetireScratchBare = ((b: string, o: { deadline: number }, fn: never) => { seen.lock = o.deadline - Date.now(); return bareLock(b, o as never, fn); }) as never;
    const result = await w.run();
    // The window is 120 s (plus the 5 s settlement reserve on the lock); the pass itself would allow ~595 s.
    assert.ok(seen.evidence! <= 120_500, `evidence deadline ${seen.evidence}`);
    assert.ok(seen.protected! <= 120_500, `protected deadline ${seen.protected}`);
    // The read used the window: the whole phase is deferred (nothing was nominated, nothing spawned).
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    assert.equal(w.spawns.length, 0);
    assert.deepEqual(w.reserveCalls, []);
    untouched(s);
  });

  it("the bare lock of a parent that is examined is bounded by the window plus the settlement reserve", async () => {
    const s = w.seed();
    const git = w.options.retire!.git;
    let lock = Infinity;
    const bareLock = git.withRetireScratchBare;
    git.withRetireScratchBare = ((b: string, o: { deadline: number }, fn: never) => { lock = o.deadline - Date.now(); return bareLock(b, o as never, fn); }) as never;
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 1, JSON.stringify(result.retire_scratch));
    assert.ok(lock <= 125_500, `lock deadline ${lock}`);
    assert.ok(!exists(s.parent));
  });
});

// A restart after an interrupted REAL retire (the crash seam of retireBindingStep) followed by the verified epoch phase:
// only a fully settled prefix is ever reclaimed; no interrupted prefix is ever deleted.
describe("restart after an interrupted retire, then the verified epoch phase", PROC, () => {
  const ATT = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
  const exdev = (t: TestContext): void => {
    const rename = fsp.rename.bind(fsp);
    t.mock.method(fsp, "rename", async (...args: Parameters<typeof fsp.rename>) => {
      if (String(args[1]).startsWith(path.join(w.fx.dataDir, "runner-quarantine"))) throw Object.assign(new Error("cross device"), { code: "EXDEV" });
      return rename(...args);
    });
  };
  interface Produced { uuid: string | undefined; parent: string | undefined; clonePath: string; raw: string | undefined }
  async function produce(t: TestContext, opts: { split: boolean; fault?: RetireBindingStep; partialRm?: boolean }): Promise<Produced> {
    exdev(t);
    const fault = (s: string): void => { if (s === opts.fault) throw new Error(`injected ${s}`); };
    const cache = opts.split ? w.cache : new GitCache(w.fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ retentionDelete: { split: false }, retireBindingStep: async (s: RetireBindingStep) => { fault(s); } }));
    if (opts.split) w.stepHook = s => fault(s);
    if (opts.partialRm) {
      const rm = fsp.rm.bind(fsp);
      t.mock.method(fsp, "rm", async (...args: Parameters<typeof fsp.rm>) => {
        const target = String(args[0]);
        if (path.basename(target).startsWith(".retire-")) { await rm(path.join(target, "clone", "owner.txt")); throw Object.assign(new Error("partial"), { code: "EIO" }); }
        return rm(...args);
      });
    }
    const owner = randomUUID();
    const clone = await cache.createOrAttachRunnerClone(w.bare, 4263, noProofReseed, owner, false, undefined,
      { attemptId: ATT, isLive: () => false, beforeSeed: async () => {} });
    await cache.markRecoveryCapture(w.bare, clone.path, BRANCH, owner, ATT);
    fs.writeFileSync(path.join(clone.path, "owner.txt"), "owner bytes\n");
    try { await cache.retireRunnerClone(w.bare, clone.path, BRANCH, owner, { discard: true, attemptId: ATT, dispositionClass: "owner_terminal" }); } catch { /* the injected fault */ }
    w.stepHook = undefined;
    fs.chmodSync(w.runnerRoot, 0o1700);
    let listing = "";
    try { listing = w.git("config", "--local", "--null", "--get-regexp", "^uzi-retire-scratch\\."); } catch { /* none */ }
    const item = listing.split("\0").filter(Boolean)[0];
    const uuid = item === undefined ? undefined : /^uzi-retire-scratch\.([0-9a-f-]{36})\.binding/.exec(item)?.[1];
    return { uuid, parent: uuid === undefined ? undefined : path.join(w.runnerRoot, `.retire-${uuid}`), clonePath: clone.path,
      raw: uuid === undefined ? undefined : w.bindingRaws(uuid)[0] };
  }
  const untouchedAfter = (p: Produced): void => {
    assert.deepEqual(w.bindingRaws(p.uuid!), [p.raw], "binding unchanged");
    assert.equal(w.spawns.length, 0, "no helper");
    assert.ok(exists(path.join(p.parent!, "clone")), "the interrupted parent keeps its clone");
  };

  const split = true;
  // A prefix before the settled write: the binding says intent and the parent (if any) is never touched.
  for (const fault of ["renamed", "move-verified", "journal-cleared", "ledger-appended", "settle-write"] as const) {
    it(`split, interrupted at ${fault}: kept bound_intent, nothing deleted`, async (t) => {
      const p = await produce(t, { split, fault });
      assert.equal(w.phaseOf(p.uuid!), "intent");
      assert.ok(p.parent && exists(p.parent), "the scratch parent holds the clone");
      const result = await w.run();
      assert.equal(result.retire_scratch.kept_by_reason.bound_intent, 1, JSON.stringify(result.retire_scratch));
      assert.equal(result.retire_scratch.reclaimed + result.retire_scratch.recovered, 0);
      untouchedAfter(p);
    });
  }

  it("split, intent written then rolled back with the canonical clone present: the metadata-only case 2 unset, the clone untouched", async (t) => {
    const p = await produce(t, { split, fault: "intent-written" });
    assert.equal(w.phaseOf(p.uuid!), "intent");
    assert.ok(!exists(p.parent!), "the failed retire removed its parent");
    assert.ok(exists(path.join(p.clonePath, "owner.txt")));
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 1, JSON.stringify(result.retire_scratch));
    assert.deepEqual(w.bindingRaws(p.uuid!), []);
    assert.ok(exists(path.join(p.clonePath, "owner.txt")), "the canonical clone is untouched");
    assert.equal(w.spawns.length, 0);
  });

  for (const fault of [undefined, "settled"] as const) {
    it(`split, ${fault ? "fault after the settled write" : "uninterrupted"}: the settled prefix is reclaimed`, async (t) => {
      const p = await produce(t, { split, ...(fault ? { fault } : {}) });
      assert.equal(w.phaseOf(p.uuid!), "settled");
      const result = await w.run();
      assert.equal(result.retire_scratch.reclaimed, 1, JSON.stringify(result.retire_scratch));
      assert.ok(!exists(p.parent!));
      assert.deepEqual(w.bindingRaws(p.uuid!), []);
      assert.equal(w.spawns.length, 1);
    });
  }

  it("split, settled and the parent already gone (an earlier reclaim died before its unset): metadata-only unset", async (t) => {
    const p = await produce(t, { split });
    await fsp.rm(p.parent!, { recursive: true });
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 1, JSON.stringify(result.retire_scratch));
    assert.deepEqual(w.bindingRaws(p.uuid!), []);
    assert.equal(w.spawns.length, 0);
  });

  for (const step of ["recover-clone-dir-sync", "recover-root-sync"] as const) {
    it(`case 2 recovery that crashes at ${step} keeps the binding; the next pass finishes it`, async (t) => {
      const p = await produce(t, { split, fault: "intent-written" });
      let thrown = false;
      w.stepHook = s => { if (s === step && !thrown) { thrown = true; throw new Error(`injected ${step}`); } };
      const first = await w.run();
      assert.equal(first.retire_scratch.recovered, 0);
      assert.equal(first.retire_scratch.kept_by_reason.metadata_unresolved, 1);
      assert.deepEqual(w.bindingRaws(p.uuid!), [p.raw]);
      const second = await w.run();
      assert.equal(second.retire_scratch.recovered, 1);
      assert.deepEqual(w.bindingRaws(p.uuid!), []);
      assert.ok(exists(path.join(p.clonePath, "owner.txt")));
    });
  }

  // Single uid: the retire itself removes in process, writing `removing` first.
  it("single uid, interrupted at removing-write: intent, kept bound_intent", async (t) => {
    const p = await produce(t, { split: false, fault: "removing-write" });
    assert.equal(w.phaseOf(p.uuid!), "intent");
    const result = await w.run();
    assert.equal(result.retire_scratch.kept_by_reason.bound_intent, 1, JSON.stringify(result.retire_scratch));
    untouchedAfter(p);
  });

  for (const fault of ["removing-sync", "removing-written"] as const) {
    it(`single uid, interrupted at ${fault}: removing with the parent present is kept interrupted_removal`, async (t) => {
      const p = await produce(t, { split: false, fault });
      assert.equal(w.phaseOf(p.uuid!), "removing");
      const result = await w.run();
      assert.equal(result.retire_scratch.kept_by_reason.interrupted_removal, 1, JSON.stringify(result.retire_scratch));
      untouchedAfter(p);
    });
  }

  it("single uid, a partial rm (one file gone): removing, kept interrupted_removal, the rest is never deleted by the phase", async (t) => {
    const p = await produce(t, { split: false, partialRm: true });
    assert.equal(w.phaseOf(p.uuid!), "removing");
    assert.ok(!exists(path.join(p.parent!, "clone", "owner.txt")));
    const result = await w.run();
    assert.equal(result.retire_scratch.kept_by_reason.interrupted_removal, 1, JSON.stringify(result.retire_scratch));
    assert.equal(result.retire_scratch.reclaimed, 0);
    untouchedAfter(p);
  });

  for (const fault of ["removed", "root-synced-after-rm", "unset"] as const) {
    it(`single uid, interrupted at ${fault} (parent gone, ledger retired, journal cleared): metadata-only unset`, async (t) => {
      const p = await produce(t, { split: false, fault });
      assert.equal(w.phaseOf(p.uuid!), "removing");
      assert.ok(!exists(p.parent!));
      const result = await w.run();
      assert.equal(result.retire_scratch.recovered, 1, JSON.stringify(result.retire_scratch));
      assert.deepEqual(w.bindingRaws(p.uuid!), []);
      assert.equal(w.spawns.length, 0);
    });
  }

  it("single uid, interrupted after the unset: no binding is left and the phase has nothing to examine", async (t) => {
    const p = await produce(t, { split: false, fault: "unset-sync" });
    assert.equal(p.uuid, undefined);
    const result = await w.run();
    assert.equal(result.retire_scratch.examined, 0);
    assert.equal(w.spawns.length, 0);
  });
});

// Owner review item 2: the 120 s phase bound also holds for the initial and per-candidate quiescence proofs and the authority
// read. Each step gets a deadline 5 s before the window ends and a signal that fires there; the step is JOINED (never raced),
// and one that cannot finish inside the window leaves its candidate `budget_deferred` before any effect. The fake clock only
// moves the window (ctx.now()); the deadlines are on the Date.now() domain, so the tests leave 6.5 s of window (1.5 s of real time).
describe("the pre-effect steps are bounded by the phase window less the settlement reserve", PROC, () => {
  const RESERVE = 5_000, LEFT = 6_500, STEP = { timeout: 30_000 };
  type Bound = { signal: AbortSignal; deadline: number };
  const probe = { gaveUp: false, inflight: 0 };
  beforeEach(() => { probe.gaveUp = false; probe.inflight = 0; });

  /** Rejects when the signal fires; gives up on its own after 8 s so a missing bound fails the test instead of hanging it. */
  const hang = (signal: AbortSignal | undefined): Promise<never> => new Promise((_resolve, reject) => {
    probe.inflight++;
    const timer = setTimeout(() => { probe.gaveUp = true; probe.inflight--; reject(new Error("never cancelled")); }, 8_000);
    const stop = (): void => { clearTimeout(timer); probe.inflight--; reject(signal?.reason ?? new Error("aborted")); };
    if (signal?.aborted) stop(); else signal?.addEventListener("abort", stop, { once: true });
  });
  /** Leaves LEFT ms of the 120 s window; returns the real time of that moment. */
  const spend = (): number => { w.clock += 120_000 - LEFT; return Date.now(); };
  function settledClean(): void {
    assert.equal(probe.gaveUp, false, "the step was never cancelled");
    assert.equal(probe.inflight, 0, "no pending work");
    assert.equal(w.pending, 0);
    assert.equal(w.stopped, false);
    assert.equal(w.handovers, 1);
  }
  function withinWindow(bound: Bound | undefined, calledAt: number, t0: number): void {
    assert.ok(bound, "the step received a bound");
    assert.ok(bound.deadline > calledAt, "the bound is in the future when the step starts");
    assert.ok(bound.deadline - calledAt <= LEFT - RESERVE, `forwarded deadline ${bound.deadline - calledAt} ms ahead`);
    const elapsed = Date.now() - t0;
    assert.ok(elapsed <= LEFT, `phase elapsed ${elapsed}`);
    assert.ok(elapsed < LEFT - RESERVE + 1_500, `the step settled ${elapsed} ms in, outside the reserve`);
  }

  it("a delayed initial quiescence proof is cancelled at the step deadline and the whole phase is deferred", STEP, async () => {
    const s = w.seed();
    let t0 = 0, calledAt = 0, seen: Bound | undefined;
    w.fence = () => { t0 = spend(); return FUTURE_FENCE(); };
    w.quiesce = async bound => {
      seen = bound; calledAt = Date.now();
      try { await hang(bound?.signal); } catch { return false; }
      return true;
    };
    const result = await w.run();
    withinWindow(seen, calledAt, t0);
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    assert.deepEqual(w.reserveCalls, []);
    assert.equal(w.spawns.length, 0);
    untouched(s);
    settledClean();
  });

  it("a proof that cannot start inside the window less the reserve is never started", STEP, async () => {
    const s = w.seed();
    let calls = 0;
    w.fence = () => { w.clock += 120_000 - (RESERVE + 500); return FUTURE_FENCE(); };
    w.quiesce = async () => { calls++; return true; };
    const result = await w.run();
    assert.equal(calls, 0);
    assert.equal(result.retire_scratch.state, "refused");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    assert.deepEqual(w.reserveCalls, []);
    untouched(s);
    settledClean();
  });

  it("a delayed authority read is cancelled at the step deadline and the parent is deferred before any effect", STEP, async () => {
    const s = w.seed();
    let t0 = 0, calledAt = 0, seen: Bound | undefined;
    w.beforeB = () => { t0 = spend(); };
    (w.options.client as { getRunOwnership: unknown }).getRunOwnership = async (_id: string, o: Bound) => {
      seen = o; calledAt = Date.now();
      await hang(o.signal);
    };
    const result = await w.run();
    withinWindow(seen, calledAt, t0);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(result.retire_scratch.kept, 0);
    assert.deepEqual(result.retire_scratch.kept_by_reason, {});
    assert.equal(result.retire_scratch.state, "stopped");
    assert.equal(result.retire_scratch.reason, "budget_deferred");
    assert.equal(kept(result, s.name)?.reason, "budget_deferred");
    assert.equal(w.spawns.length, 0);
    assert.equal(w.phaseOf(s.uuid), "settled");
    untouched(s);
    settledClean();
  });

  it("a delayed per-candidate quiescence proof is cancelled at the step deadline: deferred, no removing write, no helper", STEP, async () => {
    const s = w.seed();
    let t0 = 0, calledAt = 0, seen: Bound | undefined, calls = 0;
    w.beforeB = () => { t0 = spend(); };
    w.quiesce = async bound => {
      if (++calls === 1) return true;
      seen = bound; calledAt = Date.now();
      try { await hang(bound?.signal); } catch { return false; }
      return true;
    };
    const result = await w.run();
    assert.equal(calls, 2);
    withinWindow(seen, calledAt, t0);
    assert.equal(result.retire_scratch.deferred, 1);
    assert.equal(result.retire_scratch.kept, 0);
    assert.deepEqual(result.retire_scratch.kept_by_reason, {});
    assert.equal(result.retire_scratch.state, "stopped");
    assert.equal(result.retire_scratch.reason, "budget_deferred", "a cancelled proof is a deferral, not not_idle");
    assert.equal(kept(result, s.name)?.reason, "budget_deferred");
    assert.equal(w.spawns.length, 0);
    assert.equal(w.phaseOf(s.uuid), "settled", "removing is only written in beforeEffect");
    untouched(s);
    settledClean();
  });

  it("a real daemon socket that never answers, aborted at the step deadline, settles inside the reserve with the socket closed", STEP, async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "retire-docker-"));
    const sock = path.join(dir, "d.sock");
    let accepted = 0, closed = 0;
    const sockets = new Set<net.Socket>();
    const server = net.createServer(c => { accepted++; sockets.add(c); c.resume(); c.on("error", () => undefined); c.on("close", () => { closed++; }); });
    await new Promise<void>(resolve => server.listen(sock, resolve));
    try {
      const window = 1_500;
      const signal = AbortSignal.any([new AbortController().signal, AbortSignal.timeout(window)]);
      const started = Date.now();
      assert.equal(await dockerRunningContainers(`unix://${sock}`, signal, window), "unknown");
      const elapsed = Date.now() - started;
      assert.ok(elapsed < window + 250, `settled ${elapsed} ms against a ${window} ms step deadline`);
      for (let i = 0; i < 50 && closed < 1; i++) await new Promise(r => setTimeout(r, 10));
      assert.equal(accepted, 1);
      assert.equal(closed, 1, "the socket was closed, not abandoned");
      // Nothing left to spend: no request at all.
      const idleStart = Date.now();
      assert.equal(await dockerRunningContainers(`unix://${sock}`, new AbortController().signal, 0), "unknown");
      assert.ok(Date.now() - idleStart < 500, "answered without waiting on the daemon");
      await new Promise(r => setTimeout(r, 50));
      assert.equal(accepted, 1, "a step with no time left sends no request");
    } finally {
      for (const c of sockets) c.destroy();
      await new Promise<void>(resolve => server.close(() => resolve()));
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("a real API whose body stalls after the headers, cancelled at the step deadline, defers the parent inside the reserve", STEP, async () => {
    const s = w.seed();
    let accepted = 0, closed = 0;
    const server = http.createServer((_req, res) => {
      res.writeHead(200, { "content-type": "application/json", "content-length": "1000" });
      res.write("{\"status\":\"compl");
    });
    server.on("connection", c => { accepted++; c.on("error", () => undefined); c.on("close", () => { closed++; }); });
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    try {
      const client = new WorkerClient(`http://127.0.0.1:${(server.address() as { port: number }).port}`, "token", "0.1.0-test", nullLogger());
      w.options.client = { listRecoveryHolds: client.listRecoveryHolds.bind(client), getRunOwnership: client.getRunOwnership.bind(client) } as never;
      let t0 = 0;
      w.beforeB = () => { t0 = spend(); };
      const result = await w.run();
      const elapsed = Date.now() - t0;
      assert.ok(elapsed < LEFT - RESERVE + 1_500, `the read settled ${elapsed} ms in, outside the reserve`);
      assert.equal(result.retire_scratch.deferred, 1);
      assert.equal(result.retire_scratch.kept, 0);
      assert.equal(kept(result, s.name)?.reason, "budget_deferred");
      assert.equal(w.spawns.length, 0);
      untouched(s);
      for (let i = 0; i < 50 && closed < accepted; i++) await new Promise(r => setTimeout(r, 10));
      assert.ok(accepted >= 1);
      assert.equal(closed, accepted, "every connection was closed after the cancel");
      settledClean();
    } finally {
      server.closeAllConnections();
      await new Promise<void>(resolve => server.close(() => resolve()));
    }
  });
});

describe("cross-bare attempt identity conflicts", PROC, () => {
  const otherBare = (over: Partial<RetireScratchBareEvidence>): RetireScratchBareEvidence => ({
    barePath: path.join(w.fx.dataDir, "repos", "another.git"),
    bindings: [], protectedPaths: [], restoreBranches: [], recovery: [], stoppedRuns: [], ledger: [], ...over,
  });
  /** Wraps the evidence read so another bare is added from the `from`-th call on (1 = every call). */
  const addBare = (extra: RetireScratchBareEvidence, from = 1): void => {
    const git = w.options.retire!.git;
    const read = git.readRetireScratchEvidence;
    let calls = 0;
    git.readRetireScratchEvidence = async opts => {
      const result = await read(opts);
      calls++;
      if (result.state !== "read" || calls < from) return result;
      return { ...result, bares: [...result.bares, extra] };
    };
  };
  const ledgerRaw = (attemptId: string) => JSON.stringify({ attemptId, runId: "other-run", clonePath: path.join(w.runnerRoot, "other", "attempt"), state: "retired" });
  const ledgerOf = (...raws: string[]) => otherBare({ ledger: raws.map(raw => ({ branch: "agent/other", raw })) });

  it("another bare's conflicting attempt identity must keep the settled parent", async () => {
    const s = w.seed();
    addBare(ledgerOf(ledgerRaw(s.attemptId)));
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 0, "conflicting identity across bares must not grant deletion authority");
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    assert.equal(exists(s.parent), true);
    untouched(s);
  });

  it("control: another bare naming a different well-formed attempt does not stop the reclaim", async () => {
    const s = w.seed();
    const other = formatAttemptId(new Date(Date.UTC(2026, 10, 5, 10, 10, 59)), 1, "0123456789abcdef");
    addBare(ledgerOf(ledgerRaw(other)));
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 1);
    assert.equal(exists(s.parent), false);
  });

  it("another bare with an undecodable binding entry keeps the parent as conflict", async () => {
    const s = w.seed();
    addBare(otherBare({ bindings: [{ key: "uzi-retire-scratch.x.binding", raw: "" }] }));
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 0);
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    untouched(s);
  });

  for (const raw of ["{}", "null", '{"attemptId":42}']) {
    it(`another bare's unidentified ledger value ${raw} keeps the parent`, async () => {
      const s = w.seed();
      addBare(ledgerOf(raw));
      const result = await w.run();
      assert.equal(result.retire_scratch.reclaimed, 0);
      assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
      untouched(s);
    });
  }

  it("a conflicting bare that appears only at the deletion-time re-read keeps the parent", async () => {
    const s = w.seed();
    addBare(ledgerOf(ledgerRaw(s.attemptId)), 2);
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 0);
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    untouched(s);
  });

  it("a gone parent is not recovered while another bare conflicts, and its binding is untouched", async () => {
    const s = w.seed({ withParent: false });
    addBare(ledgerOf(ledgerRaw(s.attemptId)));
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  it("a gone parent whose conflicting bare appears only at the re-read is not recovered", async () => {
    const s = w.seed({ withParent: false });
    addBare(ledgerOf(ledgerRaw(s.attemptId)), 2);
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  /** Replaces the result of the `n`-th evidence read only (1 = the nomination read, 2 = the re-read inside the reservation). */
  type ReadResult = Awaited<ReturnType<ReturnType<typeof w.splitGit>["readRetireScratchEvidence"]>>;
  const onRead = (n: number, change: (r: ReadResult) => ReadResult): void => {
    const git = w.options.retire!.git;
    const read = git.readRetireScratchEvidence;
    let calls = 0;
    git.readRetireScratchEvidence = async opts => { const r = await read(opts); return ++calls === n ? change(r) : r; };
  };
  const withBare = (extra: RetireScratchBareEvidence) => (r: ReadResult): ReadResult =>
    r.state === "read" ? { ...r, bares: [...r.bares, extra] } : r;

  it("a conflicting bare seen only at nomination keeps a present parent as conflict before any reservation", async () => {
    const s = w.seed();
    onRead(1, withBare(ledgerOf(ledgerRaw(s.attemptId))));
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 0);
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    assert.deepEqual(w.reserveCalls, []);
    assert.equal(w.spawns.length, 0);
    assert.equal(exists(s.parent), true);
    untouched(s);
  });

  it("a conflicting bare seen only at nomination keeps a gone parent as conflict, binding untouched, no reservation", async () => {
    const s = w.seed({ withParent: false });
    onRead(1, withBare(ledgerOf(ledgerRaw(s.attemptId))));
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    assert.deepEqual(w.reserveCalls, []);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  it("a gone parent whose uuid is bound in two bares is not recovered and is kept as conflict", async () => {
    const s = w.seed({ withParent: false });
    // A different attempt id keeps the cross-bare identity check quiet, so only the two-owner count can refuse it.
    const otherRaw = retireBindingValue({ ...s.binding, attemptId: formatAttemptId(new Date(Date.UTC(2026, 10, 5, 10, 10, 59)), 1, "0123456789abcdef") });
    const parsed = parseRetireBinding(s.uuid, otherRaw);
    assert.ok(parsed.ok);
    addBare(otherBare({ bindings: [{ key: retireBindingKey(s.uuid), uuid: s.uuid, raw: otherRaw, parsed: parsed.binding }] }));
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    assert.deepEqual(w.reserveCalls, []);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  const unreadable = (): ReadResult => ({ state: "unknown", reason: "unreadable" } as unknown as ReadResult);

  it("an unknown re-read inside the reservation keeps a present parent as conflict", async () => {
    const s = w.seed();
    onRead(2, unreadable);
    const result = await w.run();
    assert.equal(result.retire_scratch.reclaimed, 0);
    assert.equal(result.retire_scratch.kept_by_reason.conflict, 1);
    untouched(s);
  });

  it("an unknown re-read inside the reservation leaves a gone parent's binding as metadata_unresolved", async () => {
    const s = w.seed({ withParent: false });
    onRead(2, unreadable);
    const result = await w.run();
    assert.equal(result.retire_scratch.recovered, 0);
    assert.equal(result.retire_scratch.kept_by_reason.metadata_unresolved, 1);
    assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
  });

  for (const withParent of [true, false]) {
    it(`an unknown re-read that spends the phase window defers a ${withParent ? "present" : "gone"} parent as budget_deferred`, async () => {
      const s = w.seed({ withParent });
      onRead(2, () => { w.clock += 125_000; return unreadable(); });
      const result = await w.run();
      assert.equal(result.retire_scratch.deferred, 1);
      assert.equal(result.retire_scratch.kept, 0);
      assert.equal(result.retire_scratch.reclaimed + result.retire_scratch.recovered, 0);
      assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
    });
  }
});

// Compaction of either bare must not erase the evidence the cross-bare refusal reads (a real second bare, not a faked read).
describe("ledger compaction keeps the cross-bare conflict the reclaim refuses on", PROC, () => {
  const compactOf = (b: string): Promise<void> =>
    (w.cache as unknown as { compactAttemptLedger(b: string, br: string): Promise<void> }).compactAttemptLedger(b, BRANCH);
  const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;
  /** A second real bare holding an UNBOUND canonical value for the seeded attempt, with another run id and a gone clone path. */
  const otherBareHolding = (attemptId: string): { bare: string; value: string } => {
    const bare = path.join(w.fx.dataDir, "repos", "another.git");
    execFileSync("git", ["init", "--bare", "-q", bare], { env: ENV });
    fs.chmodSync(bare, 0o700);
    const value = JSON.stringify({ attemptId, runId: "run-another", clonePath: path.join(w.runnerRoot, "slug", "another-gone-attempt"), state: "live" });
    execFileSync("git", ["-C", bare, "config", "--local", "--add", LEDGER_KEY, value], { env: ENV });
    return { bare, value };
  };
  const valuesOf = (bare: string): string[] => execFileSync("git", ["-C", bare, "config", "--local", "--get-all", LEDGER_KEY], { encoding: "utf8", env: ENV }).trim().split("\n");

  for (const order of [["seed", "other"], ["other", "seed"]] as const) {
    it(`a present parent stays kept as conflict after compacting ${order.join(" then ")}`, async () => {
      const s = w.seed();
      const other = otherBareHolding(s.attemptId);
      const seeded = valuesOf(w.bare);
      for (const which of order) await compactOf(which === "seed" ? w.bare : other.bare);
      assert.deepEqual(valuesOf(other.bare), [other.value], "the other bare's unbound value survives verbatim");
      assert.deepEqual(valuesOf(w.bare), seeded, "the seeded bare's retired entry survives");
      const result = await w.run();
      assert.equal(result.retire_scratch.reclaimed, 0);
      assert.equal(result.retire_scratch.kept_by_reason.conflict, 1, JSON.stringify(result.retire_scratch.kept_by_reason));
      assert.equal(exists(s.parent), true);
      untouched(s);
    });

    it(`a gone parent is not recovered and its binding is untouched after compacting ${order.join(" then ")}`, async () => {
      const s = w.seed({ withParent: false });
      const other = otherBareHolding(s.attemptId);
      const seeded = valuesOf(w.bare);
      for (const which of order) await compactOf(which === "seed" ? w.bare : other.bare);
      assert.deepEqual(valuesOf(other.bare), [other.value]);
      assert.deepEqual(valuesOf(w.bare), seeded);
      const result = await w.run();
      assert.equal(result.retire_scratch.recovered, 0);
      // The cross-bare check precedes the gone-parent recovery check, so the reason is the same `conflict`.
      assert.equal(result.retire_scratch.kept_by_reason.conflict, 1, JSON.stringify(result.retire_scratch.kept_by_reason));
      assert.deepEqual(w.bindingRaws(s.uuid), [s.raw]);
    });
  }
});
