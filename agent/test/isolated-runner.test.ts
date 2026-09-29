// PRD #1906 M4: the IsolatedRunner (fail-closed preflight, lifecycle, no git) and the
// worker's routing and isolated_fetch_v1 advertisement.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { IsolatedContext } from "../src/isolated-executor.js";
import { IsolatedRunner, type IsolatedRunnerOptions } from "../src/isolated-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ChatClaimResponse, ClaimResponse, StateRequest } from "../src/protocol.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { RunRunner } from "../src/runner.js";
import { ISOLATED_FETCH_CAPABILITY, Worker } from "../src/worker.js";
import { nullLogger, recordingLogger } from "./helpers.js";

const CRED = "iso-cred-" + "test-only-abcdef";

let dataDir: string;
let caFile: string;

beforeEach(() => {
  dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-isolated-runner-"));
  caFile = path.join(dataDir, "fetcher-ca.pem");
  fs.writeFileSync(caFile, "not parsed by these tests\n");
});
afterEach(() => fs.rmSync(dataDir, { recursive: true, force: true }));

function isolatedClaim(over: Partial<ClaimResponse> = {}): ClaimResponse {
  return {
    run_id: "iso-1",
    kind: "issue",
    issue_iid: null,
    issue_title: "Compare datasheets",
    issue_description: "Read the vendor datasheets.",
    repo: {},
    secrets: { forge_pat: "", anthropic_oauth_token: "oauth-dummy" },
    last_seq: 0,
    claim_generation: 3,
    agents: [],
    isolated_fetch: { credential: CRED, profile: "vendor-docs", hosts: ["docs.example.com"] },
    ...over,
  } as unknown as ClaimResponse;
}

function recordingClient(): { client: WorkerClient; states: StateRequest[] } {
  const states: StateRequest[] = [];
  const client = {
    reportState: async (_runId: string, body: StateRequest) => {
      states.push(body);
      return { applied: true };
    },
    postMessages: async () => ({}),
  } as unknown as WorkerClient;
  return { client, states };
}

const noCancel = (): IsolatedRunnerOptions["makeSource"] => () => ({ start() {}, stop: async () => {} });

function runner(client: WorkerClient, over: Partial<IsolatedRunnerOptions> & { ran?: IsolatedContext[] } = {}): IsolatedRunner {
  const ran = over.ran ?? [];
  return new IsolatedRunner(client, nullLogger(), {
    dataDir,
    fetcherUrl: "https://uzi-fetcher.test:8443",
    fetcherCaFile: caFile,
    batchMs: 5,
    makeSource: noCancel(),
    // Pinned: the default reads process.env.UZI_UID_SPLIT, which would make this file
    // depend on the environment it runs in. The split case is its own test below.
    splitActive: false,
    executor: {
      run: async (ctx) => {
        ran.push(ctx);
        assert.ok(fs.statSync(ctx.workspace).isDirectory(), "the workspace exists while the session runs");
      },
    },
    ...over,
  });
}

describe("IsolatedRunner: requires its executor", () => {
  it("refuses construction without an executor (no fallback path guard without the worker-credential deny set)", () => {
    const { client } = recordingClient();
    const opts = { dataDir, fetcherUrl: "https://uzi-fetcher.test:8443", fetcherCaFile: caFile, batchMs: 5 };
    // @ts-expect-error executor is required
    assert.throws(() => new IsolatedRunner(client, nullLogger(), opts), /executor/);
  });
});

describe("IsolatedRunner: fail closed before anything starts", () => {
  const cases: Array<[string, Partial<ClaimResponse>, Partial<IsolatedRunnerOptions>, RegExp]> = [
    [
      "a Codex block (codex + isolated)",
      { secrets: { forge_pat: "", anthropic_oauth_token: "oauth-dummy", codex: { auth_mode: "api_key" } } as unknown as ClaimResponse["secrets"] },
      {},
      /Codex harness/,
    ],
    ["a forge credential", { secrets: { forge_pat: "glpat-x", anthropic_oauth_token: "oauth-dummy" } }, {}, /forge credential/],
    ["no fetcher URL", {}, { fetcherUrl: undefined }, /UZI_FETCHER_URL/],
    ["no fetcher CA", {}, { fetcherCaFile: undefined }, /UZI_FETCHER_CA_FILE/],
    ["an empty credential", { isolated_fetch: { credential: "", profile: "p", hosts: [] } }, {}, /malformed isolated_fetch/],
    ["no Anthropic token", { secrets: { forge_pat: "" } }, {}, /no Anthropic OAuth token/],
  ];
  for (const [name, claimOver, optsOver, want] of cases) {
    it(`refuses ${name}: reports failed, runs no session, creates no workspace`, async () => {
      const { client, states } = recordingClient();
      const ran: IsolatedContext[] = [];
      const activeRuns = new ActiveRunRegistry();
      await runner(client, { ran, activeRuns, ...optsOver }).execute(isolatedClaim(claimOver));
      assert.equal(ran.length, 0, "no session was started");
      assert.equal(states.length, 1, "exactly one report");
      assert.equal(states[0]!.status, "failed", "and it is the failure (never `running`)");
      assert.match(states[0]!.failure_reason ?? "", want);
      assert.equal(states[0]!.claim_generation, 3);
      assert.ok(!fs.existsSync(path.join(dataDir, "isolated")), "no per-run directory was made");
      assert.equal(activeRuns.size, 0, "the run is no longer listed as active");
    });
  }

  it("fails closed when the CA file cannot be read", async () => {
    const { client, states } = recordingClient();
    const ran: IsolatedContext[] = [];
    await runner(client, { ran, fetcherCaFile: path.join(dataDir, "missing.pem") }).execute(isolatedClaim());
    assert.equal(ran.length, 0);
    assert.deepEqual(states.map((s) => s.status), ["failed"]);
    assert.match(states[0]!.failure_reason ?? "", /fetcher CA bundle/);
  });
});

describe("IsolatedRunner: lifecycle", () => {
  it("reports running, runs the session in a fresh per-run workspace, completes, and cleans up", async () => {
    const { client, states } = recordingClient();
    const ran: IsolatedContext[] = [];
    const activeRuns = new ActiveRunRegistry();
    let listedDuringRun = false;
    const r = runner(client, {
      activeRuns,
      executor: {
        run: async (ctx) => {
          ran.push(ctx);
          listedDuringRun = activeRuns.has("iso-1");
        },
      },
    });
    await r.execute(isolatedClaim());
    assert.deepEqual(states.map((s) => s.status), ["running", "completed"]);
    assert.ok(states.every((s) => s.claim_generation === 3));
    assert.equal(ran.length, 1);
    const ctx = ran[0]!;
    assert.equal(ctx.workspace, path.join(dataDir, "isolated", "iso-1", "workspace"));
    assert.ok(ctx.homeDir.startsWith(path.join(dataDir, "isolated", "iso-1")));
    assert.match(ctx.prompt, /Compare datasheets/);
    assert.match(ctx.prompt, /docs\.example\.com/);
    assert.ok(!ctx.prompt.includes(CRED), "the credential is not in the prompt");
    assert.ok(listedDuringRun, "the run is in the active-run registry while it runs");
    assert.equal(activeRuns.size, 0);
    assert.ok(!fs.existsSync(path.join(dataDir, "isolated", "iso-1")), "the per-run dir is removed");
  });

  it("a session failure reports failed with the reason", async () => {
    const { client, states } = recordingClient();
    const r = runner(client, { executor: { run: async () => Promise.reject(new Error("isolated run refused: extra tool")) } });
    await r.execute(isolatedClaim());
    assert.deepEqual(states.map((s) => s.status), ["running", "failed"]);
    assert.match(states[1]!.failure_reason ?? "", /extra tool/);
  });

  it("the prompt always opens with the literal \"Research task: \", so a leading / is never a slash command", async () => {
    for (const over of [
      { issue_title: "/compact", issue_description: "/clear everything" },
      { issue_title: "", issue_description: "/model opus" },
      { issue_title: null, issue_description: null },
      { issue_title: " /resume", issue_description: "" },
    ] as Array<Partial<ClaimResponse>>) {
      const { client } = recordingClient();
      const ran: IsolatedContext[] = [];
      await runner(client, { ran }).execute(isolatedClaim(over));
      assert.equal(ran.length, 1);
      const prompt = ran[0]!.prompt;
      assert.ok(prompt.startsWith("Research task: "), JSON.stringify(over));
      assert.ok(!prompt.startsWith("/"), JSON.stringify(over));
    }
  });

  it("the run's signal is aborted once the session ends, so an in-flight fetch never outlives the run", async () => {
    const { client } = recordingClient();
    const ran: IsolatedContext[] = [];
    await runner(client, { ran }).execute(isolatedClaim());
    assert.equal(ran[0]!.signal.aborted, true);
  });

  it("the isolated runner has no git collaborator: its module imports neither git nor the RunRunner", () => {
    const src = fs.readFileSync(path.join(import.meta.dirname, "..", "src", "isolated-runner.ts"), "utf8");
    const imports = [...src.matchAll(/from "(\.\/[^"]+)"/g)].map((m) => m[1]);
    for (const banned of ["./git.js", "./runner.js", "./provision-run.js"]) {
      assert.ok(!imports.includes(banned), `isolated-runner.ts must not import ${banned}`);
    }
  });
});

describe("IsolatedRunner: steering, claim loss and shutdown", () => {
  /** A client whose GET /inputs answers `inputs` once (consume-on-read, no receipts). */
  function steeringClient(inputs: Array<{ kind: string; body?: string }>, stateAck: (b: StateRequest) => object = () => ({ applied: true })) {
    const states: StateRequest[] = [];
    const posted: unknown[] = [];
    let served = false;
    const client = {
      reportState: async (_runId: string, body: StateRequest) => {
        states.push(body);
        return stateAck(body);
      },
      postMessages: async (_runId: string, messages: unknown[]) => void posted.push(...messages),
      getInputs: async () => {
        if (served) return { inputs: [] };
        served = true;
        return { inputs: inputs.map((i, n) => ({ id: n + 1, kind: i.kind, body: i.body ?? null })) };
      },
    } as unknown as WorkerClient;
    return { client, states, posted };
  }

  /** An executor that runs until its signal aborts, then fails the way the real one does. */
  const untilAborted = { run: (ctx: IsolatedContext) => new Promise<void>((_, reject) => {
    const fail = (): void => reject(new Error("isolated run cancelled"));
    if (ctx.signal.aborted) fail();
    else ctx.signal.addEventListener("abort", fail, { once: true });
  }) };

  it("a cancel input aborts the session and the run is reported failed", async () => {
    const { client, states } = steeringClient([{ kind: "cancel" }]);
    await runner(client, { makeSource: undefined, pollMs: 1, executor: untilAborted }).execute(isolatedClaim());
    assert.deepEqual(states.map((s) => s.status), ["running", "failed"]);
    assert.match(states[1]!.failure_reason ?? "", /cancelled/);
  });

  it("a follow_up is refused visibly (a research run takes no steering), and the run carries on", async () => {
    const { client, states, posted } = steeringClient([{ kind: "follow_up", body: "also compare prices" }]);
    const r = runner(client, {
      makeSource: undefined,
      pollMs: 1,
      executor: { run: async () => { for (let i = 0; i < 100 && !JSON.stringify(posted).includes("follow-up"); i++) await tick(5); } },
    });
    await r.execute(isolatedClaim());
    assert.deepEqual(states.map((s) => s.status), ["running", "completed"]);
    const text = JSON.stringify(posted);
    assert.match(text, /follow-up was not applied/);
    assert.ok(!text.includes("also compare prices"), "the ignored text is not echoed back as if it were acted on");
  });

  it("a stale-claim ack at the running report abandons the claim: no session, no further report, no workspace", async () => {
    const { client, states } = steeringClient([], (b) => (b.status === "running" ? { applied: false, staleClaim: true } : { applied: true }));
    const ran: IsolatedContext[] = [];
    const activeRuns = new ActiveRunRegistry();
    await runner(client, { ran, activeRuns }).execute(isolatedClaim());
    assert.equal(ran.length, 0);
    assert.deepEqual(states.map((s) => s.status), ["running"]);
    assert.equal(activeRuns.size, 0);
    assert.ok(!fs.existsSync(path.join(dataDir, "isolated", "iso-1")));
  });

  for (const outcome of ["completes", "fails"] as const) {
    it(`a lost claim reports no terminal state when the session ${outcome}`, async () => {
      const { client, states } = steeringClient([]);
      const r = runner(client, {
        makeSource: () => ({ start() {}, stop: async () => {}, claimLost: () => true }),
        executor: { run: async () => { if (outcome === "fails") throw new Error("isolated run cancelled"); } },
      });
      await r.execute(isolatedClaim());
      assert.deepEqual(states.map((s) => s.status), ["running"]);
      assert.ok(!fs.existsSync(path.join(dataDir, "isolated", "iso-1")), "the per-run dir is still removed");
    });
  }

  it("shutdown() aborts an in-flight session and reports it failed", async () => {
    const { client, states } = steeringClient([]);
    const r = runner(client, { executor: untilAborted });
    const done = r.execute(isolatedClaim());
    for (let i = 0; i < 200 && states.length === 0; i++) await tick();
    r.shutdown();
    await done;
    assert.deepEqual(states.map((s) => s.status), ["running", "failed"]);
    assert.match(states[1]!.failure_reason ?? "", /worker shut down/);
  });

  it("a claim that arrives after shutdown() starts no session", async () => {
    const { client, states } = steeringClient([]);
    const ran: IsolatedContext[] = [];
    const r = runner(client, { ran });
    r.shutdown();
    await r.execute(isolatedClaim());
    assert.equal(ran.length, 0);
    assert.equal(states.at(-1)?.status, "failed");
    assert.match(states.at(-1)?.failure_reason ?? "", /worker shut down/);
  });

  it("fails closed under the uid split (the 0700 per-run dirs would be the worker's alone)", async () => {
    const { client, states } = recordingClient();
    const ran: IsolatedContext[] = [];
    await runner(client, { ran, splitActive: true }).execute(isolatedClaim());
    assert.equal(ran.length, 0);
    assert.deepEqual(states.map((s) => s.status), ["failed"]);
    assert.match(states[0]!.failure_reason ?? "", /UZI_UID_SPLIT/);
    assert.ok(!fs.existsSync(path.join(dataDir, "isolated")));
  });
});

// --- worker routing and advertisement -----------------------------------------------

const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));
const okPreflight = () => ({ ok: true as const, missing: [] });
const noResumeRecoveries = { resumePendingRecoveries: async () => {} };

function fakeConfig(over: Partial<Config> = {}): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dockerWiring: {},
    ...over,
  } as unknown as Config;
}

async function routeOne(claim: ClaimResponse, wire: "fake" | "none" = "fake"): Promise<{ routed: string[]; states: StateRequest[] }> {
  const controller = new AbortController();
  const routed: string[] = [];
  const states: StateRequest[] = [];
  let gave = false;
  const client = {
    register: async () => ({}),
    heartbeat: async () => {},
    claimRun: async (): Promise<ClaimResponse | null> => {
      if (gave) return null;
      gave = true;
      return claim;
    },
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
    reportState: async (_id: string, body: StateRequest) => {
      states.push(body);
      routed.push(`report:${body.status}`);
      return { applied: true };
    },
  } as unknown as WorkerClient;
  const runRunner = {
    ...noResumeRecoveries,
    execute: async () => {
      routed.push("runner");
    },
  } as unknown as RunRunner;
  const judge = { execute: async () => void routed.push("judge") } as unknown as JudgeRunner;
  const review = { execute: async () => void routed.push("review") } as unknown as ReviewRunner;
  const isolatedRunner =
    wire === "fake" ? ({ execute: async () => void routed.push("isolated") } as unknown as IsolatedRunner) : undefined;
  const worker = new Worker(
    fakeConfig(), client, runRunner, {} as unknown as ChatRunner, judge, review, recordingLogger().logger, okPreflight,
    undefined, undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    isolatedRunner,
  );
  const done = worker.run(controller.signal);
  for (let i = 0; i < 500 && routed.length === 0; i++) await tick();
  controller.abort();
  await done;
  return { routed, states };
}

describe("Worker: isolated claim dispatch (PRD #1906 M4)", () => {
  it("routes an isolated_fetch claim to the IsolatedRunner first, even with a review target or judge kind", async () => {
    for (const over of [{}, { review_target_run_id: "t-1", kind: "task" }, { kind: "judge" }] as Array<Partial<ClaimResponse>>) {
      const { routed } = await routeOne(isolatedClaim(over));
      assert.deepEqual(routed, ["isolated"], JSON.stringify(over));
    }
  });

  it("with no IsolatedRunner wired, an isolated claim fails closed and never reaches the RunRunner", async () => {
    const { routed, states } = await routeOne(isolatedClaim(), "none");
    assert.ok(!routed.includes("runner"));
    assert.equal(states[0]?.status, "failed");
  });

  it("a claim without isolated_fetch still goes to the RunRunner", async () => {
    const { routed } = await routeOne(isolatedClaim({ isolated_fetch: undefined }));
    assert.deepEqual(routed, ["runner"]);
  });
});

describe("Worker: isolated_fetch_v1 advertisement", () => {
  async function advertised(config: Config): Promise<string[] | undefined> {
    const controller = new AbortController();
    let captured: string[] | undefined;
    const client = {
      register: async (_n: string, _t: string, _m: number, _c?: string[], protocolCapabilities?: string[]) => {
        captured = protocolCapabilities;
        return {};
      },
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;
    const worker = new Worker(
      config, client, { ...noResumeRecoveries, execute: async () => {} } as unknown as RunRunner, {} as unknown as ChatRunner,
      {} as unknown as JudgeRunner, {} as unknown as ReviewRunner, recordingLogger().logger, okPreflight,
    );
    const done = worker.run(controller.signal);
    for (let i = 0; i < 300 && captured === undefined; i++) await tick();
    controller.abort();
    await done;
    return captured;
  }

  let savedSplit: string | undefined;
  beforeEach(() => {
    savedSplit = process.env.UZI_UID_SPLIT;
    delete process.env.UZI_UID_SPLIT;
  });
  afterEach(() => {
    if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT;
    else process.env.UZI_UID_SPLIT = savedSplit;
  });

  it("is not advertised under the UZI_UID_SPLIT uid split, whose preflight refuses every isolated claim", async () => {
    process.env.UZI_UID_SPLIT = "1";
    const caps = await advertised(fakeConfig({ fetcherUrl: "https://f:8443", fetcherCaFile: "/run/ca.pem" }));
    assert.ok(caps && !caps.includes("isolated_fetch_v1"), JSON.stringify(caps));
  });

  it("is advertised only when both UZI_FETCHER_URL and UZI_FETCHER_CA_FILE are configured", async () => {
    assert.equal(ISOLATED_FETCH_CAPABILITY, "isolated_fetch_v1");
    const both = await advertised(fakeConfig({ fetcherUrl: "https://f:8443", fetcherCaFile: "/run/ca.pem" }));
    assert.ok(both?.includes("isolated_fetch_v1"));
    for (const partial of [{}, { fetcherUrl: "https://f:8443" }, { fetcherCaFile: "/run/ca.pem" }]) {
      const caps = await advertised(fakeConfig(partial));
      assert.ok(caps && !caps.includes("isolated_fetch_v1"), JSON.stringify(partial));
    }
  });
});
