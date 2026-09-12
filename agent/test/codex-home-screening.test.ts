// PRD #1287 C3 / D6 — the failing-old / passing-fixed regression for the approved provider-HOME
// screening repair.
//
// The repair (agent/src/codex/codex-executor.ts, the `shared.screenPolicy` literal) carries the
// trusted provider-HOME prefix `homeRoot/codex-data/` into the `screenPolicy.extraSecretPaths`
// threaded to EVERY phase broker (root + child) of EVERY epoch. Before the repair that literal
// passed only `{ dockerWired: false }`, so a literal shell read of a provider-owned credential
// file reached the command launcher and relied on OS containment (D6: "absence of CODEX_HOME
// from the command environment does not protect a known absolute path").
//
// This regression drives the REAL production `CodexExecutor` — NOT a hand-built broker with a
// hand-passed `extraSecretPaths` (that would pass both before and after the repair, vacuously).
// It mirrors the injected-fakes composition of e2e/codex-m3b/lifecycle.test.ts Block A and
// agent/test/codex-executor.test.ts: a real executor over an in-memory scripted transport, with
// only the launcher / command / fileop OS seams injected. The executor builds its own
// `screenPolicy` at the wiring site under test, so the assertion exercises the repaired
// construction, not a test-authored policy.
//
// The executor is constructed with a KNOWN `homeRoot` so the test can name the literal absolute
// path to the epoch-0 provider HOME's `auth.json` (`homeRoot/codex-data/epoch-0/codex/auth.json`,
// per startProviderEpoch's ownedDataRoot/codexHome derivation). The screener guards the STATIC
// parent `homeRoot/codex-data/`, so the concrete epoch-0 credential path is caught even though the
// epoch index is chosen inside the executor.
//
// Calibration (failing-old / passing-fixed) is the SHELL leg: with the repair the `cat <authPath>`
// command is DENIED before the spawn seam (spawn counter stays 0); revert the repair and the
// command reaches the spawn seam (counter 1, reply success). A harmless in-worktree command still
// reaches its effect either way, proving the policy is not over-broad.
//
// NOTE on `$CODEX_HOME`: this file deliberately does NOT assert a `$CODEX_HOME/auth.json` shell
// denial. The screener's tokenizer does not variable-expand `$CODEX_HOME`, and CODEX_HOME is
// absent from the scrubbed command-identity env, so that unexpanded string discloses no secret and
// is already harmless — it is not what the repair is for. The repair closes the LITERAL resolved
// absolute path, which is exactly what these tests drive.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { PassThrough } from "node:stream";

import {
  CodexExecutor,
  type CodexExecutorDeps,
} from "../src/codex/codex-executor.js";
import { selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import type { CodexLaunchRootResult, CodexProviderConfig } from "../src/codex/codex-harness.js";
import type { RegisteredRoot } from "../src/codex/registry.js";
import type { FileopHelperHandle } from "../src/codex/fileop-client.js";
import type { FileopRequest } from "../src/codex/broker.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import type { RunContext, EmittedMessage } from "../src/executor.js";
import type { Logger } from "../src/log.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../src/codex/launcher.js";

// The executor's homeRoot is KNOWN so the test can name the literal epoch-0 provider-HOME path.
const HOME_ROOT = "/data/agent-home/run-1";
// The STATIC parent the repaired screenPolicy guards (homeRoot/codex-data/), and the concrete
// epoch-0 credential file UNDER it (startProviderEpoch: ownedDataRoot = homeRoot/codex-data/epoch-N,
// codexHome = ownedDataRoot/codex).
const CODEX_DATA_DIR = path.join(HOME_ROOT, "codex-data");
const EPOCH0_AUTH_PATH = path.join(CODEX_DATA_DIR, "epoch-0", "codex", "auth.json");

const WORKSPACE = "/work/repo";
const FRESH_TOKEN = "fresh-codex-access-token-XXXXXXXX";

const provider: CodexProviderConfig = {
  name: "openai",
  baseUrl: "http://127.0.0.1:9/v1",
  envKey: "OPENAI_API_KEY",
  model: "gpt-6-astra",
};

const noopLog: Logger = {
  debug() {},
  info() {},
  warn() {},
  error() {},
  addSecret() {},
  removeSecret() {},
  child() {
    return noopLog;
  },
};

function rec(v: unknown): Record<string, unknown> {
  return (v ?? {}) as Record<string, unknown>;
}

// ─── a minimal scriptable in-memory transport (mirrors the proven Block A shape) ────
interface ResponderCtx {
  transport: FakeTransport;
  method: string;
  params: unknown;
  threadStartCount: number;
  turnStartCount: number;
}
type Responder = (c: ResponderCtx) => unknown;

class FakeTransport implements CodexTransport {
  requests: { method: string; params: unknown; opts?: { signal?: AbortSignal } }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
  notifies: { method: string; params: unknown }[] = [];
  closes = 0;
  threadStartCount = 0;
  turnStartCount = 0;

  private readonly queue: CodexNotification[] = [];
  private ended = false;
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
  private consumed = false;
  private closedFlag = false;

  constructor(private readonly responder: Responder) {}

  push(note: CodexNotification): this {
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: note, done: false });
    } else {
      this.queue.push(note);
    }
    return this;
  }

  end(): this {
    this.ended = true;
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: undefined, done: true });
    }
    return this;
  }

  request<T = unknown>(method: string, params?: unknown, opts?: { signal?: AbortSignal }): Promise<T> {
    this.requests.push({ method, params, opts });
    if (method === "initialize") {
      return Promise.resolve({ userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" } as T);
    }
    if (method === "account/login/start") {
      return Promise.resolve({ type: rec(params).type } as T);
    }
    if (method === "thread/start") this.threadStartCount += 1;
    if (method === "turn/start") this.turnStartCount += 1;
    const c: ResponderCtx = {
      transport: this,
      method,
      params,
      threadStartCount: this.threadStartCount,
      turnStartCount: this.turnStartCount,
    };
    try {
      return Promise.resolve(this.responder(c) as T);
    } catch (err) {
      return Promise.reject(err instanceof Error ? err : new Error(String(err)));
    }
  }

  notify(method: string, params?: unknown): void {
    this.notifies.push({ method, params });
  }

  installServerRequestInterceptor(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void {
    void interceptor;
    return () => undefined;
  }

  respond(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    if (this.closedFlag) throw new Error("codex transport is closed");
    this.responses.push({ requestId, response });
  }

  notifications(): AsyncIterableIterator<CodexNotification> {
    if (this.consumed) throw new Error("notifications() is single-consumer");
    this.consumed = true;
    const next = (): Promise<IteratorResult<CodexNotification>> =>
      new Promise((resolve) => {
        const q = this.queue.shift();
        if (q !== undefined) {
          resolve({ value: q, done: false });
          return;
        }
        if (this.ended) {
          resolve({ value: undefined, done: true });
          return;
        }
        this.waiter = resolve;
      });
    return {
      next,
      return: () => Promise.resolve({ value: undefined, done: true }),
      [Symbol.asyncIterator]() {
        return this;
      },
    };
  }

  close(): Promise<void> {
    this.closes += 1;
    this.closedFlag = true;
    return Promise.resolve();
  }
}

function defaultResponder(c: ResponderCtx): unknown {
  if (c.method === "thread/start") return { thread: { id: "th-1" } };
  if (c.method === "thread/resume") return { thread: { id: "resumed-1" } };
  if (c.method === "turn/start") return { turn: { id: "tn-1" } };
  return {};
}

// ─── notification builders ──────────────────────────────────────────────────────
function threadStarted(threadId = "th-1"): CodexNotification {
  return { kind: "thread_started", method: "thread/started", threadId, params: { thread: { id: threadId } } };
}
function turnCompleted(status = "completed", threadId = "th-1", turnId = "tn-1"): CodexNotification {
  return { kind: "turn_completed", method: "turn/completed", threadId, turnId, status, params: { threadId, turn: { id: turnId, status } } };
}
function toolCall(requestId: number, tool: string, args: unknown, callId: string): CodexNotification {
  return { kind: "activity", method: "item/tool/call", requestId, params: { threadId: "th-1", turnId: "tn-1", callId, tool, arguments: args } };
}
function signalDone(): CodexNotification {
  return toolCall(700, "signal_done", {}, "c-done");
}

function bindingOf(codex: Record<string, unknown>): CodexBinding {
  const selection = selectCodexBinding({ codex });
  if (selection.kind !== "codex") throw new Error("expected a codex selection");
  return selection.binding;
}
const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok",
  capability: "run-cap",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
};

interface FakeClient {
  releaseCodex(runId: string, req: { capability: string }): Promise<{ access_token: string }>;
  refreshCodex(runId: string, req: { capability: string; operation_id: string; observed_generation: number }): Promise<{ access_token: string; generation: number; chatgpt_account_id: string; outcome: string }>;
}
function fakeClient(): FakeClient {
  let gen = 4;
  return {
    async releaseCodex() {
      return { access_token: FRESH_TOKEN };
    },
    async refreshCodex() {
      return { access_token: `${FRESH_TOKEN}-refreshed`, generation: gen++, chatgpt_account_id: "verified-account", outcome: "advanced" };
    },
  };
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emitted: EmittedMessage[] } {
  const emitted: EmittedMessage[] = [];
  const ctx: RunContext = {
    runId: "run-1",
    issueIid: 42,
    issueTitle: "do a thing",
    issueDescription: "the description",
    worktreePath: WORKSPACE,
    branch: "agent/issue-42",
    emit: (m) => emitted.push(m),
    planApproved: true,
    approvedPlan: "the approved plan",
    agents: [],
    ...overrides,
  };
  return { ctx, emitted };
}

function trackedRoot(): RegisteredRoot {
  return {
    kind: "provider",
    reap: async () => ({ ok: true }),
    dispose: async () => undefined,
  };
}

interface Rig {
  transport: FakeTransport;
  client: FakeClient;
  /** Every argv the command spawn seam observed — the negative-effect oracle for the shell leg. */
  spawnCommandCalls: { argv: readonly string[]; opts: { cwd?: string } }[];
  /** Every fileop request the fileop client observed — the negative-effect oracle for the file leg. */
  fileopOps: FileopRequest[];
  deps: CodexExecutorDeps;
}

function makeRig(responder: Responder): Rig {
  const transport = new FakeTransport(responder);
  const client = fakeClient();
  const root = trackedRoot();
  const spawnCommandCalls: Rig["spawnCommandCalls"] = [];
  const fileopOps: FileopRequest[] = [];
  const fileopHandle: FileopHelperHandle = {
    client: {
      op: async (request) => {
        fileopOps.push(request);
        // An allowed in-worktree op reaches this and succeeds; a screened-out op never gets here.
        return { ok: true, size: 0, data: "" };
      },
    },
    dispose: async () => undefined,
  };
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (): Promise<CodexLaunchRootResult> => ({ root, transport, supervisorPid: 1234 }),
    spawnCommand: async (argv, cmdOpts) => {
      spawnCommandCalls.push({ argv, opts: cmdOpts });
      return { code: 0, stdout: "ok", stderr: "" };
    },
    launchEffectRoot: async (_spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const stderr = new PassThrough();
      return {
        started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true, nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
        supervisorPid: 200,
        transport: { stdin, stdout, stderr },
        snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
        waitChild: async () => ({ event: "child_exit", code: 0 }),
        dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
        failed: undefined,
        whenFailed: new Promise<Error>(() => undefined),
      };
    },
    wireFileop: () => fileopHandle,
    sessionStore: {
      adopt: async () => ({ files: 0 }),
      inspect: async () => "absent",
      remove: async () => undefined,
      persist: async () => ({ files: 0, bytes: 0 }),
    },
    idleMs: 5000,
    wallMs: 5000,
    boundaryDeadlineMs: 200,
    childTurnDeadlineMs: 5000,
    commandTmpdir: "/run/runner-tmp",
  };
  return { transport, client, spawnCommandCalls, fileopOps, deps };
}

function makeExecutor(rig: Rig): CodexExecutor {
  // The KNOWN homeRoot is what makes EPOCH0_AUTH_PATH nameable; the executor builds its screenPolicy
  // from `this.homeRoot` at the wiring site under test. The fake client implements only the
  // release/refresh credential-bridge seam this rig reaches (cast per the executor's WorkerClient
  // contract, exactly as the sibling Block A rigs do).
  return new CodexExecutor(noopLog, HOME_ROOT, { binding: bindingOf(SUBSCRIPTION), client: rig.client as never, provider }, rig.deps);
}

async function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), ms);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

function replySuccess(rig: Rig, requestId: number): boolean | undefined {
  const response = rig.transport.responses.find((r) => r.requestId === requestId)?.response;
  const result = rec(rec(response).result);
  return result.success as boolean | undefined;
}

// ================================================================================
describe("PRD #1287 D6: provider-HOME screening through the REAL CodexExecutor screenPolicy", () => {
  it("shell leg (calibration): a literal `cat <homeRoot>/codex-data/epoch-0/codex/auth.json` is DENIED before the command spawn seam (counter stays 0), while a harmless in-worktree command still reaches its effect", async () => {
    const rig = makeRig(defaultResponder);
    const exec = makeExecutor(rig);

    // One implement turn: the forbidden credential read, then a harmless allowed command, then done.
    rig.transport
      .push(threadStarted())
      .push(toolCall(41, "Bash", { command: `cat ${EPOCH0_AUTH_PATH}` }, "c-forbidden"))
      .push(toolCall(42, "Bash", { command: "echo hello" }, "c-allowed"))
      .push(signalDone())
      .push(turnCompleted("completed"))
      .end();

    const { ctx } = makeCtx();
    const result = await withTimeout(exec.run(ctx), 5000, "shell-leg run");
    assert.equal(result.branch, "agent/issue-42", "the run resolved through the implement turn");

    // NEGATIVE-EFFECT ORACLE: the forbidden credential read NEVER reached the command spawn seam.
    // With the repair reverted this fails: the argv appears and the counter is 2.
    const spawnedForbidden = rig.spawnCommandCalls.some((s) => s.argv.some((a) => a.includes(EPOCH0_AUTH_PATH)));
    assert.equal(spawnedForbidden, false, "the credential read never reached the command spawn seam");
    assert.equal(replySuccess(rig, 41), false, "the forbidden Bash callback was DENIED");

    // POSITIVE CONTROL: the harmless in-worktree command still reached its effect (policy not over-broad).
    const spawnedAllowed = rig.spawnCommandCalls.filter((s) => s.argv.some((a) => a.includes("echo hello")));
    assert.equal(spawnedAllowed.length, 1, "the harmless command reached the command spawn seam exactly once");
    assert.equal(replySuccess(rig, 42), true, "the harmless in-worktree command callback succeeded");

    // Exactly one command reached the seam in total: the allowed one, never the forbidden one.
    assert.equal(rig.spawnCommandCalls.length, 1, "exactly one (allowed) command spawn occurred");
  });

  it("file leg: the resolved Read AND Write path form of the same auth.json is denied by the file screen with NO fileop effect, while an allowed in-worktree read reaches the fileop client", async () => {
    const rig = makeRig(defaultResponder);
    const exec = makeExecutor(rig);

    rig.transport
      .push(threadStarted())
      // Read (uzi_read → canonical Read) and Write (uzi_apply_patch → apply_patch) of the credential path.
      .push(toolCall(51, "uzi_read", { path: EPOCH0_AUTH_PATH }, "c-read-forbidden"))
      .push(toolCall(52, "uzi_apply_patch", { path: EPOCH0_AUTH_PATH, content: "poison" }, "c-write-forbidden"))
      // An allowed in-worktree read as the positive control (reaches the fileop client).
      .push(toolCall(53, "uzi_read", { path: "notes.txt" }, "c-read-allowed"))
      .push(signalDone())
      .push(turnCompleted("completed"))
      .end();

    const { ctx } = makeCtx();
    await withTimeout(exec.run(ctx), 5000, "file-leg run");

    // NEGATIVE-EFFECT ORACLE: no fileop op ever ran against the credential path (denied before the
    // effect — by the codex-data/ secret prefix under the repair, and by the outside-worktree jail
    // regardless, since the provider HOME sits outside /work/repo: defense in depth).
    const opsOnAuth = rig.fileopOps.filter((o) => (o.path ?? "").includes("auth.json") || (o.path ?? "").includes("codex-data"));
    assert.equal(opsOnAuth.length, 0, "no fileop op reached the credential path");
    assert.equal(replySuccess(rig, 51), false, "the forbidden Read path form was denied");
    assert.equal(replySuccess(rig, 52), false, "the forbidden Write path form was denied");

    // POSITIVE CONTROL: the allowed in-worktree read reached the fileop client (allowed workspace access preserved).
    const allowedReads = rig.fileopOps.filter((o) => o.op === "read" && o.path === "notes.txt");
    assert.equal(allowedReads.length, 1, "the allowed in-worktree read reached the fileop client exactly once");
    assert.equal(replySuccess(rig, 53), true, "the allowed in-worktree read succeeded");
  });
});
