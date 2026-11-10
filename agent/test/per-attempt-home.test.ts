import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import { readdirSync, lstatSync, readFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { WorkerClient } from "../src/client.js";
import { CodexExecutor } from "../src/codex/codex-executor.js";
import { DiskGovernor, type CacheCapConfig } from "../src/cache-cap.js";
import { StubExecutor, type EmittedMessage, type RunContext } from "../src/executor.js";
import { buildRunExecutor, makeRunExecutorFactory, type BuildRunExecutorDeps } from "../src/main.js";
import type { ClaimCodexSecrets } from "../src/protocol.js";
import { provisionRunTools, REASON_PROVISION_FAILED, type ProvisionRunDeps } from "../src/provision-run.js";
import type { ProvisionInput, ProvisionResult } from "../src/provision.js";
import { restoreTreeWritability } from "../src/rmtree.js";
import { SdkExecutor } from "../src/sdk-executor.js";
import { runDirKey } from "../src/util.js";
import { nullLogger } from "./helpers.js";

// issue #3644 plan 3.12 (item 11): a restore's successor S gets its own HOME and provision dir, `<runId>.g<N>`, so it never writes
// the restored predecessor P's `agent-home/<runId>` or `provision/<runId>`. Default (no generation) is today's exact paths.

const RUN = randomUUID();
const CODEX: ClaimCodexSecrets = {
  auth_mode: "subscription", access_token: "fixture-codex-access-token-DO-NOT-USE", capability: "fixture-codex-capability-DO-NOT-USE",
  generation: 3, chatgpt_account_id: "fixture-account-DO-NOT-USE", chatgpt_plan_type: null,
};
const ROOT = path.join(os.tmpdir(), "uzi-per-attempt-home-test");
const deps = (over: Partial<BuildRunExecutorDeps> = {}): BuildRunExecutorDeps => ({
  log: nullLogger(),
  client: new WorkerClient("http://127.0.0.1:1", "fixture-worker-join-token", "0.1.0-test", nullLogger(), { httpTimeoutMs: 1_000 }),
  sdkHomeRoot: ROOT, executorKind: "sdk", stubPlanGate: false, workerTokenFile: undefined, dockerWiring: {},
  codexCommandSandbox: "required", codexSandboxDegraded: false, ...over,
});
/** The executors keep their HOME and provision key privately; reading them is the only way to see where a launch would write. */
const peek = (e: unknown, key: string): unknown => (e as Record<string, unknown>)[key];

describe("the executor factory takes a per-attempt address", () => {
  it("default: both harnesses keep agent-home/<runId> and no provision key", () => {
    const sdk = buildRunExecutor(RUN, undefined, deps());
    assert.ok(sdk.executor instanceof SdkExecutor);
    assert.equal(sdk.homeDir, path.join(ROOT, RUN));
    assert.equal(peek(sdk.executor, "homeDir"), path.join(ROOT, RUN));
    assert.equal(peek(sdk.executor, "dirKey"), undefined);
    const codex = buildRunExecutor(RUN, CODEX, deps());
    assert.ok(codex.executor instanceof CodexExecutor);
    assert.equal(codex.homeDir, path.join(ROOT, RUN));
    assert.equal(peek(codex.executor, "homeRoot"), path.join(ROOT, RUN));
    assert.equal((peek(codex.executor, "opts") as { dirKey?: string }).dirKey, undefined);
  });

  it("with a generation: the Claude and Codex executors take agent-home/<runId>.g<N> and provision under <runId>.g<N>", () => {
    const sdk = buildRunExecutor(RUN, undefined, deps(), { generation: 2 });
    assert.ok(sdk.executor instanceof SdkExecutor);
    assert.equal(sdk.homeDir, path.join(ROOT, `${RUN}.g2`));
    assert.equal(peek(sdk.executor, "homeDir"), path.join(ROOT, `${RUN}.g2`));
    assert.equal(peek(sdk.executor, "dirKey"), `${RUN}.g2`);
    assert.equal(peek(sdk.executor, "provisionHomeDir"), ROOT, "the shared provisioning HOME stays the shared root");
    const codex = buildRunExecutor(RUN, CODEX, deps(), { generation: 3 });
    assert.ok(codex.executor instanceof CodexExecutor);
    assert.equal(codex.homeDir, path.join(ROOT, `${RUN}.g3`));
    assert.equal(peek(codex.executor, "homeRoot"), path.join(ROOT, `${RUN}.g3`));
    assert.equal((peek(codex.executor, "opts") as { dirKey?: string }).dirKey, `${RUN}.g3`);
  });

  it("the stub keeps its shared root HOME whatever the address (the provision key comes from RunContext.dirKey)", () => {
    const stub = buildRunExecutor(RUN, undefined, deps({ executorKind: "stub" }), { generation: 2 });
    assert.ok(stub.executor instanceof StubExecutor);
    assert.equal(stub.homeDir, undefined);
    assert.equal((peek(stub.executor, "opts") as { homeDir?: string }).homeDir, ROOT);
  });

  it("makeRunExecutorFactory (the seam the worker's closure calls and the runner's restore decision will call) forwards the address", () => {
    const make = makeRunExecutorFactory(deps());
    assert.equal(make(RUN).homeDir, path.join(ROOT, RUN));
    assert.equal(make(RUN, undefined, {}).homeDir, path.join(ROOT, RUN));
    assert.equal(make(RUN, undefined, { generation: 2 }).homeDir, path.join(ROOT, `${RUN}.g2`));
    assert.equal(make(RUN, CODEX, { generation: 4 }).homeDir, path.join(ROOT, runDirKey(RUN, 4)));
  });
});

describe("provisioning touches only provision/<dirKey>", () => {
  let worktree: string;
  let provisionRoot: string;
  let homeDir: string;
  beforeEach(async () => {
    worktree = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-pa-wt-"));
    provisionRoot = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-pa-prov-"));
    homeDir = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-pa-home-"));
  });
  afterEach(async () => {
    await fs.rm(worktree, { recursive: true, force: true });
    await restoreTreeWritability(provisionRoot);
    await fs.rm(provisionRoot, { recursive: true, force: true });
    await fs.rm(homeDir, { recursive: true, force: true });
  });

  /** names, bytes, modes and mtimes of a tree. */
  function digest(dir: string): string {
    const h = createHash("sha256");
    const walk = (d: string): void => {
      for (const name of readdirSync(d).sort()) {
        const p = path.join(d, name);
        const st = lstatSync(p);
        h.update(`${path.relative(dir, p)}\0${st.mode}\0${st.mtimeMs}\0${st.size}\0`);
        if (st.isDirectory()) walk(p); else if (st.isFile()) h.update(readFileSync(p));
      }
    };
    walk(dir);
    return h.digest("hex");
  }
  const ctx = (extra: Record<string, unknown> = {}): { ctx: RunContext; emits: EmittedMessage[] } => {
    const emits: EmittedMessage[] = [];
    return {
      emits,
      ctx: { runId: RUN, worktreePath: worktree, config: { tool_packages: ["kubectl@1.31"], repo_devbox_opt_in: true }, emit: (m: EmittedMessage) => emits.push(m), ...extra } as unknown as RunContext,
    };
  };
  /** A provision that materialises its run dir like the real one and fails (after writing) when asked. */
  const fake = (failWhen: (packages: string[]) => boolean): { fn: ProvisionRunDeps["provision"]; dirs: string[] } => {
    const dirs: string[] = [];
    const fn = (async (input: ProvisionInput): Promise<ProvisionResult> => {
      dirs.push(input.runDir);
      await fs.mkdir(input.runDir, { recursive: true });
      await fs.writeFile(path.join(input.runDir, "devbox.json"), JSON.stringify(input.packages));
      if (failWhen(input.packages)) throw new Error("devbox install failed");
      return { toolEnv: { PATH: "/tier1/bin" } };
    }) as unknown as ProvisionRunDeps["provision"];
    return { fn, dirs };
  };
  const seedPredecessor = async (): Promise<string> => {
    const p = path.join(provisionRoot, RUN);
    await fs.mkdir(path.join(p, ".devbox"), { recursive: true });
    await fs.writeFile(path.join(p, "devbox.json"), "predecessor's manifest", { mode: 0o640 });
    await fs.writeFile(path.join(p, ".devbox", "lock"), "predecessor's lock");
    return p;
  };
  const wt = async (packages: string[]): Promise<void> => { await fs.writeFile(path.join(worktree, "devbox.json"), JSON.stringify({ packages })); };

  it("success under ctx.dirKey writes <runId>.g2 only and returns it; P's dir is byte for byte unchanged", async () => {
    const p = await seedPredecessor();
    const before = digest(p);
    const f = fake(() => false);
    const h = ctx({ dirKey: `${RUN}.g2` });
    const r = await provisionRunTools(h.ctx, { provisionRoot, homeDir, log: nullLogger(), provision: f.fn });
    assert.equal(r.provisionDir, path.join(provisionRoot, `${RUN}.g2`));
    assert.deepEqual(f.dirs, [path.join(provisionRoot, `${RUN}.g2`)]);
    assert.equal(digest(p), before);
  });

  it("a failure and the tier-1 retry both stay under <runId>.g2: only that dir is rewritten and removed, P's digest is unchanged", async () => {
    await wt(["ruby@3.3"]);
    const p = await seedPredecessor();
    const before = digest(p);
    const f = fake((packages) => packages.includes("ruby@3.3"));
    const h = ctx({ dirKey: `${RUN}.g2` });
    const r = await provisionRunTools(h.ctx, { provisionRoot, homeDir, log: nullLogger(), provision: f.fn });
    assert.deepEqual(f.dirs, [path.join(provisionRoot, `${RUN}.g2`), path.join(provisionRoot, `${RUN}.g2`)], "the merged attempt and the retry");
    assert.equal(r.provisionDir, path.join(provisionRoot, `${RUN}.g2`));
    assert.deepEqual((await fs.readdir(provisionRoot)).sort(), [RUN, `${RUN}.g2`].sort());
    assert.deepEqual(JSON.parse(await fs.readFile(path.join(provisionRoot, `${RUN}.g2`, "devbox.json"), "utf8")), ["kubectl@1.31"], "the retry rewrote only S's manifest");
    assert.equal(digest(p), before, "the failure path removed nothing of P's");
  });

  it("a fatal failure removes S's dir and leaves P's; deps.dirKey wins over ctx.dirKey", async () => {
    const p = await seedPredecessor();
    const before = digest(p);
    const f = fake(() => true);
    const h = ctx({ dirKey: `${RUN}.g9`, config: { tool_packages: ["kubectl@1.31"] } });
    await assert.rejects(provisionRunTools(h.ctx, { provisionRoot, homeDir, log: nullLogger(), provision: f.fn, dirKey: `${RUN}.g2` }), (e: Error) => e.message.startsWith(REASON_PROVISION_FAILED));
    assert.deepEqual(f.dirs, [path.join(provisionRoot, `${RUN}.g2`)]);
    assert.deepEqual(await fs.readdir(provisionRoot), [RUN], "S's dir is gone, P's is the only one left");
    assert.equal(digest(p), before);
  });

  it("without a key it is today's exact <runId> path", async () => {
    const f = fake(() => false);
    const r = await provisionRunTools(ctx().ctx, { provisionRoot, homeDir, log: nullLogger(), provision: f.fn });
    assert.equal(r.provisionDir, path.join(provisionRoot, RUN));
  });

  it("a key that is not a directory name of this run is refused before anything is created", async () => {
    const f = fake(() => false);
    for (const dirKey of [`${randomUUID()}.g2`, `${RUN}.g0`, `${RUN}/../x`, "..", `${RUN}.g2/x`]) {
      await assert.rejects(provisionRunTools(ctx({ dirKey }).ctx, { provisionRoot, homeDir, log: nullLogger(), provision: f.fn }), /invalid provision directory key/, dirKey);
    }
    assert.deepEqual(f.dirs, []);
    assert.deepEqual(await fs.readdir(provisionRoot), []);
  });
});

describe("the disk governor and a late-bound HOME", () => {
  const CONFIG: CacheCapConfig = { capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03 };
  const GIB = 1024 ** 3;
  function governor() {
    const measured: string[] = [];
    const trimmed: string[] = [];
    const gov = new DiskGovernor({
      config: CONFIG, log: nullLogger(), volumeTotalBytes: () => 25 * GIB, thresholdOf: () => 0.9, reclaim: async () => {},
      measure: async (home) => { measured.push(home); return { cacheBytes: 10 * GIB, entries: 1, truncated: false }; },
      trim: async (home) => { trimmed.push(home); return { estimatedBytesAfter: 0, goBuildUnits: 0, goBuildBytes: 0, npmBuckets: 0, npmContents: 0, npmBytes: 0, moduleCacheRemoved: false, kept: [] } as never; },
      now: () => 1_000_000,
    });
    return { gov, measured, trimmed };
  }

  it("a function HOME is read at every use (register does not snapshot it), and an unbound run is skipped by the boundary", async () => {
    const { gov, measured, trimmed } = governor();
    let bound: string | undefined;
    gov.register("run-1", { home: () => bound, requestStop: () => {}, reclaimInPlace: async () => undefined });
    assert.equal(await gov.boundary("run-1", async () => false), "continue");
    assert.deepEqual(measured, [], "an unbound run is not measured");
    assert.deepEqual(trimmed, []);
    bound = "/data/agent-home/run-1.g2";
    await gov.boundary("run-1", async () => false);
    assert.equal(measured[0], "/data/agent-home/run-1.g2", "the HOME bound after registration is the one measured");
    assert.equal(trimmed[0], "/data/agent-home/run-1.g2", "soft trim runs only after binding, on the bound HOME");
  });

  it("the hard layer skips an unbound run and acts on the bound one", async () => {
    const { gov, measured } = governor();
    const stopped: string[] = [];
    gov.register("unbound", { home: () => undefined, requestStop: () => stopped.push("unbound"), reclaimInPlace: async () => undefined });
    gov.register("bound", { home: () => "/data/agent-home/bound", requestStop: () => stopped.push("bound"), reclaimInPlace: async () => undefined });
    for (const id of ["unbound", "bound"]) { gov.enterRun(id); }
    gov.observe(0.95, 1_000_000);
    await new Promise((r) => setTimeout(r, 20));
    assert.ok(!measured.some((h) => h === undefined), "no measurement of an unbound HOME");
    assert.ok(measured.length > 0 && measured.every((h) => h === "/data/agent-home/bound"), "only the bound HOME is measured");
    assert.ok(!stopped.includes("unbound"));
  });

  it("a plain string HOME is a permanently bound HOME", async () => {
    const { gov, measured } = governor();
    gov.register("run-2", { home: "/data/agent-home/run-2", requestStop: () => {}, reclaimInPlace: async () => undefined });
    await gov.boundary("run-2", async () => false);
    assert.equal(measured[0], "/data/agent-home/run-2");
  });
});
