import { NO_SCAN_NEEDLES } from "./run-env-test-support.js";
import { afterEach, describe, it, type TestContext } from "node:test";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { performance } from "node:perf_hooks";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { GitCache } from "../src/git.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { WorkerClient } from "../src/client.js";
import {
  TERMINAL_PRESERVATION_ENABLED,
  terminalPreservationActive,
} from "../src/terminal-preservation.js";
import { FakeRecoveryClient, FakeRecoveryGit } from "./codex-reap-fixture.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import * as h from "./runner-harness.js";
import { fixture as codexReapFixture } from "./codex-reap-fixture.js";
import type { TerminalPreservationTestHooks } from "../src/terminal-preservation.js";
import { Outbox } from "../src/outbox.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, StateRequest } from "../src/protocol.js";

const SRC_DIR = path.resolve(import.meta.dirname, "..", "src");

const roots: string[] = [];
afterEach(() => {
  for (const r of roots.splice(0)) fs.rmSync(r, { recursive: true, force: true });
});

function srcFiles(): string[] {
  return fs.readdirSync(SRC_DIR, { recursive: true, encoding: "utf8" })
    .filter(f => f.endsWith(".ts"))
    .map(f => path.join(SRC_DIR, f));
}

// A client that claims every feature, including recovery_terminal_preservation_v1.
function allFeaturesClient(): WorkerClient {
  return { hasFeature: (_f: string) => true } as unknown as WorkerClient;
}

function withEnv<T>(env: Record<string, string>, fn: () => T): T {
  const saved: Record<string, string | undefined> = {};
  for (const k of Object.keys(env)) saved[k] = process.env[k];
  Object.assign(process.env, env);
  try {
    return fn();
  } finally {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
}

const HOSTILE_ENV = {
  UZI_EPOCH_RECLAIM_DELETION: "1",
  UZI_EPOCH_SCRATCH_DELETION: "1",
  UZI_TERMINAL_PRESERVATION: "1",
  UZI_TERMINAL_PRESERVATION_ENABLED: "1",
  UZI_TERMINAL_PRESERVATION_V1: "1",
};

describe("terminal preservation gate (#4007 slice B, production-dark)", () => {
  it("the constant is false and only a test override turns the predicate on", () => {
    assert.equal(TERMINAL_PRESERVATION_ENABLED, false);
    assert.equal(terminalPreservationActive(undefined), false);
    assert.equal(terminalPreservationActive({}), true);
    assert.equal(terminalPreservationActive({ label: "x" }), true);
  });

  it("environment and server features cannot enable RunRunner or RecoveryCoordinator", () => {
    const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-tp-gate-"));
    roots.push(dataDir);
    const git = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
    withEnv(HOSTILE_ENV, () => {
      const factories: ExecutorFactory[] = [
        () => ({ executor: { run: async () => { throw new Error("unused"); } } }) as unknown as ReturnType<ExecutorFactory>,
        ((_runId: string, _codex?: unknown) => ({ executor: { run: async () => { throw new Error("unused"); } } })) as unknown as ExecutorFactory,
      ];
      for (const factory of factories) {
        const runner = new RunRunner(allFeaturesClient(), git, factory, nullLogger(), 20, "tok");
        assert.equal(runner.terminalPreservationEnabled(), false);
        const withOverride = new RunRunner(allFeaturesClient(), git, factory, nullLogger(), 20, "tok", {
          terminalPreservationTestOverride: {},
        });
        assert.equal(withOverride.terminalPreservationEnabled(), true);
        // The runner's own coordinator shares the gate in both directions.
        const coordOf = (r: RunRunner) => (r as unknown as { recovery: RecoveryCoordinator }).recovery;
        assert.equal(coordOf(runner).terminalPreservationEnabled(), false);
        assert.equal(coordOf(withOverride).terminalPreservationEnabled(), true);
      }
      const coord = new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
        client: new FakeRecoveryClient(),
        git: new FakeRecoveryGit(),
        log: nullLogger(),
        recoveryRoot: git.recoveryRoot,
        workerToken: "gate-test-worker-join-token-0123456789",
      });
      assert.equal(coord.terminalPreservationEnabled(), false);
      const on = new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
        client: new FakeRecoveryClient(),
        git: new FakeRecoveryGit(),
        log: nullLogger(),
        recoveryRoot: git.recoveryRoot,
        terminalPreservationTestOverride: {},
      });
      assert.equal(on.terminalPreservationEnabled(), true);
    });
  });

  it("source text: no production path can set the gate", () => {
    const main = fs.readFileSync(path.join(SRC_DIR, "main.ts"), "utf8");
    const worker = fs.readFileSync(path.join(SRC_DIR, "worker.ts"), "utf8");
    for (const [name, text] of [["main.ts", main], ["worker.ts", worker]] as const) {
      assert.ok(!text.includes("terminalPreservationTestOverride"), `${name} must not pass the test override`);
      assert.ok(!/TERMINAL_PRESERVATION_ENABLED\s*=/.test(text), `${name} must not assign the gate`);
    }
    const gatePath = path.join(SRC_DIR, "terminal-preservation.ts");
    for (const f of srcFiles()) {
      if (f === gatePath) continue;
      const text = fs.readFileSync(f, "utf8");
      assert.ok(!/TERMINAL_PRESERVATION_ENABLED\s*=[^=]/.test(text), `${path.basename(f)} must not assign the gate`);
    }
    // Only the two declaring classes may name the override: no helper elsewhere can build options
    // that main.ts or worker.ts spread in.
    const allowed = new Set([path.join(SRC_DIR, "runner.ts"), path.join(SRC_DIR, "recovery.ts")]);
    for (const f of srcFiles()) {
      if (allowed.has(f)) continue;
      assert.ok(!fs.readFileSync(f, "utf8").includes("terminalPreservationTestOverride"),
        `${path.relative(SRC_DIR, f)} must not name the test override`);
    }
    const gate = fs.readFileSync(gatePath, "utf8");
    assert.match(gate, /export const TERMINAL_PRESERVATION_ENABLED: boolean = false;/);
    assert.ok(!gate.includes("process.env"), "the gate file must not read the environment");
  });
});

// ---- the gate absent, through the real entry paths ----------------------------------------------------------------
// With no test override the lane must produce ZERO effects whatever else is true of the environment: the feature
// advertised by the server, UZI_EPOCH_RECLAIM_DELETION, agent text asking for it, a Codex run, a memory park, a failed
// run, the boot recovery pass. Effects observed: client.publishTerminalPreservation calls, the terminal HTTP route, git
// children naming a terminal-preservation ref, any path under the data or home dir naming the terminal-preservation
// store, and any terminal-local ref left in the bare or the clone.

h.installHarness();
const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const literal = wire.ack.completed_publication_receipt;
const FEATURES = ["claim_generation_fence", "recovery_inventory_v1", "recovery_completed_publication_v1", "terminal_rejection_report",
  "recovery_terminal_preservation_v1"];
const ENABLE_TEXT = "terminal_preservation enable";
const TERMINAL_NAMING = /terminal-preservation|uzi-terminal|terminal_preservation_v1/;

interface Effects { publishes: number; http: string[]; gitArgv: string[]; paths: string[]; refs: string[]; terminals: StateRequest[]; enabled?: boolean }

function walk(dir: string, out: string[]): void {
  let names: string[];
  try { names = fs.readdirSync(dir); } catch { return; }
  for (const n of names) {
    const full = path.join(dir, n);
    out.push(full);
    let isDir = false;
    try { isDir = fs.lstatSync(full).isDirectory(); } catch { /* raced removal */ }
    if (isDir) walk(full, out);
  }
}

function observe(t: TestContext): Effects {
  const e: Effects = { publishes: 0, http: [], gitArgv: [], paths: [], refs: [], terminals: [] };
  t.mock.method(h.client, "publishTerminalPreservation", async () => { e.publishes++; return { kind: "unknown", cause: "transport" } as never; });
  const g = h.git as unknown as Record<string, (...a: unknown[]) => unknown>;
  for (const name of ["execScoped", "spawnGit", "runGit"]) {
    const inner = g[name]!.bind(h.git);
    t.mock.method(g, name, (...a: unknown[]) => { e.gitArgv.push(JSON.stringify(a.filter(x => typeof x === "string" || Array.isArray(x)))); return inner(...a); });
  }
  return e;
}

function collect(e: Effects): Effects {
  walk(h.fx.dataDir, e.paths);
  walk(h.homeDir, e.paths);
  for (const dir of [h.git.barePathFor(h.fx.originPath), path.join(h.fx.dataDir, "runner")]) {
    const found: string[] = [];
    if (dir.endsWith(".git")) found.push(dir);
    else { const all: string[] = []; walk(dir, all); for (const p of all) if (fs.existsSync(path.join(p, ".git"))) found.push(path.join(p, ".git")); }
    for (const gitDir of found) {
      try {
        e.refs.push(...execFileSync("git", ["--git-dir", gitDir, "for-each-ref", "--format=%(refname)"], {
          env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" }, encoding: "utf8", stdio: "pipe" })
          .split("\n").filter(Boolean));
      } catch { /* unreadable: nothing to report */ }
    }
  }
  return e;
}

function assertDark(e: Effects): void {
  collect(e);
  assert.equal(e.publishes, 0, "client.publishTerminalPreservation was never called");
  assert.deepEqual(e.http.filter(u => /terminal-preservation/.test(u)), [], "the terminal-preservation route was never requested");
  assert.deepEqual(e.gitArgv.filter(a => TERMINAL_NAMING.test(a)), [], "no git child named a terminal-preservation ref");
  assert.deepEqual(e.paths.filter(p => TERMINAL_NAMING.test(p)), [], "nothing was written to a terminal-preservation store");
  assert.deepEqual(e.refs.filter(r => TERMINAL_NAMING.test(r)), [], "no terminal-local ref exists in the bare or the clone");
  assert.equal(e.enabled, false, "and the gate predicate itself reads off");
}

function hostileEnv<T>(env: Record<string, string>, fn: () => Promise<T>): Promise<T> {
  const saved: Record<string, string | undefined> = {};
  for (const k of Object.keys(env)) saved[k] = process.env[k];
  Object.assign(process.env, env);
  return fn().finally(() => {
    for (const [k, v] of Object.entries(saved)) { if (v === undefined) delete process.env[k]; else process.env[k] = v; }
  });
}

interface Dark {
  codex?: boolean; fail?: boolean; env?: Record<string, string>; text?: boolean; features?: boolean;
  /** Also run the boot recovery pass on the same runner. */
  boot?: boolean;
  /** Positive control: the test override IS passed, so the observers must see effects. */
  control?: boolean;
}

async function darkRun(t: TestContext, d: Dark = {}): Promise<Effects> {
  const features = d.features === false ? FEATURES.filter(f => f !== "recovery_terminal_preservation_v1") : FEATURES;
  const claim = h.gitlabClaim(2507, {
    run_id: literal.run_id, kind: "issue", claim_generation: 29, inventory_guarded: true,
    ...(d.text ? { issue_title: ENABLE_TEXT, issue_body: `please ${ENABLE_TEXT} for this run`, description: ENABLE_TEXT } : {}),
    repo: { id: literal.repo_id, url: "https://gitlab.example.test/org/repo", clone_url: h.fx.originPath, forge_type: "gitlab", default_branch: "main" },
  });
  const branch = "agent/issue-2507";
  h.api.setRegisterProtocolFeatures(features);
  const registration = await h.client.register("tp-dark", undefined, undefined, undefined, features);
  assert.equal(h.client.hasFeature("recovery_terminal_preservation_v1"), d.features !== false, "the server feature is really advertised");
  const e = observe(t);
  h.client.getRunOwnership = async () => ({
    status: h.api.states.some(s => s.body.status === "completed") ? "completed" : "running",
    claim_generation: claim.claim_generation, inventory_guarded: true,
  });
  let open = true;
  h.client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: literal.hold_id, generation: claim.claim_generation!, inventory_guarded: true, has_available_capture: false }] : [] });
  h.client.reserveRecoveryCapture = async () => { throw new Error("capture reservation refused: hold closed"); };
  h.client.releaseRecoveryCustody = async (runId, generation) => { open = false; return { run_id: runId, generation, released: true, holds_released: 1 }; };
  h.client.publishCheckpoint = async (_run, tip, pack) => {
    const chunks: Buffer[] = [];
    for await (const chunk of pack) chunks.push(Buffer.from(chunk));
    execFileSync("git", ["-C", h.fx.originPath, "index-pack", "--stdin", "--fix-thin"], { input: Buffer.concat(chunks) });
    execFileSync("git", ["-C", h.fx.originPath, "update-ref", `refs/uzi-checkpoints/${branch}`, tip]);
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  const safetyFx = d.codex ? codexReapFixture([], () => false) : undefined;
  if (safetyFx) t.after(() => fs.rmSync(safetyFx.root, { recursive: true, force: true }));
  const outbox = new Outbox({ root: path.join(h.fx.dataDir, "outbox"), log: nullLogger(), runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86_400_000 });
  await outbox.init();
  const forge = h.fakeGitlab();
  let finalHead = "";
  const hooks: TerminalPreservationTestHooks | undefined = d.control ? {
    workerId: registration.worker_id!, attemptId: "at1",
    deps: {
      universe: async () => ({ kind: "proved", sources: [], roots: [], universeDigest: "u1" }),
      capture: async onPhase => {
        await onPhase("capturing", { cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1" });
        await onPhase("captured", { cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1" });
        return { kind: "ok", base: finalHead, extraRoots: [], manifest: {
          run_id: claim.run_id, generation: claim.claim_generation!, worker_id: registration.worker_id!,
          roots: [{ source_id: "s1", attempt_id: "at1", commit: finalHead, tree: "b".repeat(40) }], abandoned: [] } };
      },
      aggregate: async () => ({ tip: finalHead }),
      prepare: async () => ({ pack: Buffer.alloc(0), range: { tipSha: finalHead, excludeSha: "c".repeat(40) }, digests: { scan: "s", transport: "t" } }),
      coverage: () => ({ ok: true }),
      recheck: async () => undefined,
      send: (req, signal) => h.client.publishTerminalPreservation(claim.run_id, req, signal),
    },
  } : undefined;
  const r = h.runner({ ...(safetyFx ? { safety: safetyFx.safety } : {}), run: async (ctx: RunContext) => {
    const clone = ctx.worktreePath;
    const commit = (name: string): string => {
      fs.writeFileSync(path.join(clone, name), name + "\n");
      const cmd = (...a: string[]) => execFileSync("git", ["-C", clone, ...a], { encoding: "utf8" }).trim();
      cmd("add", name);
      cmd("-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", name);
      return cmd("rev-parse", "HEAD");
    };
    commit("checkpoint.txt");
    await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
    finalHead = commit("post-checkpoint.txt");
    forge.pr.head = finalHead;
    if (d.fail) throw new Error("agent exploded");
    return { branch: ctx.branch, summary: d.text ? ENABLE_TEXT : undefined };
  } } as never, forge.gitlab, "tp-dark-key", {
    outbox, checkpointIntervalMs: 1, checkpointTickIntervalMs: 0, ...(hooks ? { terminalPreservationTestOverride: hooks } : {}),
  });
  const originalFetch = globalThis.fetch;
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.origin === new URL(h.baseUrl).origin) e.http.push(url.pathname);
    if (url.origin === new URL(h.baseUrl).origin && url.pathname.endsWith("/terminal-rejection-custody")) {
      return Response.json({ run_id: claim.run_id, worker_id: registration.worker_id, generation: claim.claim_generation,
        exact_holds: [{ id: literal.hold_id, state: open ? "open" : "released" }], sibling_holds: [], exact_count: 1, sibling_count: 0,
        complete: true, exact_complete: true, sibling_complete: true, outcome: open ? "retained" : "settled" });
    }
    const response = await originalFetch(input, init);
    if (url.origin !== new URL(h.baseUrl).origin || url.pathname !== `/api/worker/runs/${claim.run_id}/state`) return response;
    const body = JSON.parse(String(init?.body)) as StateRequest;
    if (body.status === "completed") {
      e.terminals.push(body);
      const ack = { ...wire.ack, run: { ...wire.ack.run, id: claim.run_id, worker_id: registration.worker_id } };
      if (body.completion_final_head) ack.completed_publication_receipt = {
        ...literal, run_id: claim.run_id, worker_id: registration.worker_id, generation: claim.claim_generation,
        repo_id: claim.repo!.id, branch, final_head: finalHead, observed_branch_head: finalHead, mr_iid: body.mr_iid };
      else delete ack.completed_publication_receipt;
      return Response.json(ack);
    }
    if (body.status === "failed") e.terminals.push(body);
    return response;
  });
  const go = async () => {
    await r.execute(claim);
    if (d.boot) await (r as unknown as { recovery: { resumePending(): Promise<void> } }).recovery.resumePending();
  };
  await (d.env ? hostileEnv(d.env, go) : go());
  e.enabled = r.terminalPreservationEnabled();
  assert.equal(e.terminals.length, 1, "the run reached its real terminal report");
  assert.equal(e.terminals[0]!.status, d.fail ? "failed" : "completed");
  return e;
}

describe("terminal preservation gate absent: zero effects through the real entry paths", { concurrency: false }, () => {
  it("control: the observers do see the effects when the test override turns the lane on", async t => {
    const e = collect(await darkRun(t, { control: true, features: true }));
    assert.equal(e.publishes, 1, "client.publishTerminalPreservation is observed");
    assert.ok(e.paths.some(p => /terminal-preservation/.test(p)), "the store observer sees the lane's write");
    assert.throws(() => assertDark(e), /client.publishTerminalPreservation was never called/);
  });
  it("a regular completed run", async t => { assertDark(await darkRun(t)); });
  it("a regular failed run", async t => { assertDark(await darkRun(t, { fail: true })); });
  it("a Codex completed run", async t => { assertDark(await darkRun(t, { codex: true })); });
  it("a Codex failed run", async t => { assertDark(await darkRun(t, { codex: true, fail: true })); });
  it("UZI_EPOCH_RECLAIM_DELETION=1 and the other hostile environment", async t => {
    assertDark(await darkRun(t, { env: HOSTILE_ENV }));
  });
  it("a server that advertises recovery_terminal_preservation_v1, with the boot recovery pass", async t => {
    assertDark(await darkRun(t, { features: true, boot: true }));
  });
  it("agent input and plan text asking for terminal_preservation enable", async t => { assertDark(await darkRun(t, { text: true })); });
  it("everything at once: hostile environment, feature, text, Codex and the boot pass", async t => {
    assertDark(await darkRun(t, { codex: true, env: HOSTILE_ENV, text: true, features: true, boot: true }));
  });

  it("a memory-pressure park with the feature advertised", async t => {
    const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
    h.api.memoryRegisterNonce = "tp-dark-memory";
    h.api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence", "recovery_terminal_preservation_v1"]);
    await h.client.register("tp-dark-memory", "base", undefined, undefined, undefined, undefined, undefined);
    assert.equal(h.client.hasFeature("recovery_terminal_preservation_v1"), true);
    const claim: ClaimResponse = { ...h.gitlabClaim(2396), claim_generation: 7,
      memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 2 };
    h.api.setOwnershipStatus(claim.run_id, "running", 7);
    let used = 2;
    const accepted = new Map<string, number>();
    h.client.reserveMemoryIntervention = async request => {
      accepted.set(request.intervention_id, ++used);
      return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
    };
    h.client.reportMemoryInterventionOutcome = async request => {
      const spent = accepted.get(request.intervention_id)!;
      return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
        allowance: { limit: 2, used: spent, remaining: 2 - spent } };
    };
    let sampleId = 0;
    const memory = new WorkerMemoryRuntime({ enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
      sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 }, h.client, nullLogger(), {
      reader: { sample: () => {
        const time = performance.now();
        return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time, currentBytes: 0, limitBytes: 1000,
          stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
          pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
      } },
    });
    const lifetime = await acquireWorkerLifetime(fs.realpathSync(h.fx.dataDir), testLockOptions());
    const authority = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    fs.writeFileSync(h.homeDir + "/session-proof", "original session");
    h.git.worktreeStatus = async () => [];
    const e = observe(t);
    const executor: Executor = {
      run: async ctx => {
        assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
        throw new Error("fixture stops original model execution");
      },
    };
    const r = h.runnerWith(() => ({ executor, homeDir: h.homeDir }), h.fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: memory, codexBoundaryDeadlineMs: 120_000, authority });
    try { await hostileEnv(HOSTILE_ENV, () => r.execute(claim)); } finally { await memory.stop(); }
    const parks = h.api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure");
    assert.equal(parks.length, 1, "the memory park really ran");
    e.enabled = r.terminalPreservationEnabled();
    assertDark(e);
  });
});
