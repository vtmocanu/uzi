// Protocol-only composition adapted from M4 harness-p. Synthetic supervisor posture; no OS claim.
import { ownChild } from "./ownership.js";
import { ownedDeadline } from "./timers.js";
import { EventEmitter } from "node:events";
import { spawn as nodeSpawn, type ChildProcess } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync, opendirSync, lstatSync, openSync, closeSync, readSync, writeSync, fstatSync, constants } from "node:fs";
import { readFileSync } from "node:fs";
import { probeCodexRuntime } from "../../agent/src/codex/codex-runtime-probe.js";
import type { CodexNotification } from "../../agent/src/codex/transport.js";
import path from "node:path";
import { PassThrough } from "node:stream";

import { deadline, message } from "../codex-m0/harness.mjs";
import { Budget, FakeProvider, type ResponseItem, type ResponsesBody } from "./fake-provider.js";
import { resolveCodexBin, parseLock, resolveArch, assertBinaryVersion } from "../codex-m4/provision.js";
import { buildProviderLaunchPlan } from "../codex-m4/provider-launch-plan.js";

import {
  loadPackagedLauncher,
  loadPackagedBroker,
  loadPackagedConfig,
  loadPackagedTransport,
  loadPackagedAppServerAuth,
  loadPackagedDynamicTools,
  loadPackagedRegistry,
} from "../codex-m4/packaged-modules.js";

import type {
  LauncherDeps,
  MakeRunnerTrees,
  RemoveRunnerTree,
  SpawnSupervisor,
  SupervisorProcess,
} from "../../agent/src/codex/launcher.js";
import type {
  CallbackResult,
  CodexCallbackBrokerOptions,
  DelegateSeam,
  FileopClient,
  RunGrants,
  ScreenPolicy,
  SpawnCommandSeam,
} from "../../agent/src/codex/broker.js";

/** Bounded suite deadline for a single startup + turn (measured ~0.4-1.0s on this worker). */
export const P_SUITE_DEADLINE_MS = 30_000;

/** The production modules the P driver composes, resolved once in a test's `before()`. */
export interface ProtocolModules {
  launchCodexRoot: (typeof import("../../agent/src/codex/launcher.js"))["launchCodexRoot"];
  SUPERVISOR_BIN: string;
  PROVIDER_CHILD_ARGV: readonly string[];
  CODEX_BIN: string;
  CodexCallbackBroker: (typeof import("../../agent/src/codex/broker.js"))["CodexCallbackBroker"];
  CODEX_M3B_LOOPBACK_PROVIDER_NAME: string;
  createCodexTransport: (typeof import("../../agent/src/codex/transport.js"))["createCodexTransport"];
  createCodexAppServerAuth: (typeof import("../../agent/src/codex/appserver-auth.js"))["createCodexAppServerAuth"];
  buildCodexDynamicTools: (typeof import("../../agent/src/codex/dynamic-tools.js"))["buildCodexDynamicTools"];
  ExecutionRegistry: (typeof import("../../agent/src/codex/registry.js"))["ExecutionRegistry"];
  newLocalExecutionEpoch: (typeof import("../../agent/src/codex/registry.js"))["newLocalExecutionEpoch"];
}

/** Load the production modules the P driver composes. Call in a `before()`. */
export async function loadProtocolModules(): Promise<ProtocolModules> {
  const launcher = await loadPackagedLauncher();
  const broker = await loadPackagedBroker();
  const config = await loadPackagedConfig();
  const transport = await loadPackagedTransport();
  const appServerAuth = await loadPackagedAppServerAuth();
  const dynamicTools = await loadPackagedDynamicTools();
  const registry = await loadPackagedRegistry();
  return {
    launchCodexRoot: launcher.launchCodexRoot,
    SUPERVISOR_BIN: launcher.SUPERVISOR_BIN,
    PROVIDER_CHILD_ARGV: launcher.PROVIDER_CHILD_ARGV,
    CODEX_BIN: launcher.CODEX_BIN,
    CodexCallbackBroker: broker.CodexCallbackBroker,
    CODEX_M3B_LOOPBACK_PROVIDER_NAME: config.CODEX_M3B_LOOPBACK_PROVIDER_NAME,
    createCodexTransport: transport.createCodexTransport,
    createCodexAppServerAuth: appServerAuth.createCodexAppServerAuth,
    buildCodexDynamicTools: dynamicTools.buildCodexDynamicTools,
    ExecutionRegistry: registry.ExecutionRegistry,
    newLocalExecutionEpoch: registry.newLocalExecutionEpoch,
  };
}

/** A single native/worker tool callback the broker saw, with its origin. */
export interface ObservedCallback {
  readonly tool: string;
  readonly callId: string;
  readonly result: CallbackResult;
}

/** Everything one driven turn observed. Reachability + negative-effect oracles read from here. */
export interface ProtocolObservation {
  /** Every model-selected callback the broker admitted/authorized, in order. */
  readonly callbacks: readonly ObservedCallback[];
  readonly callbackAttempted: boolean;
  /** The advertised model-visible tool schema the app-server offered (the `tools` array on the
   *  FIRST provider request). Native authority reaching the model would appear HERE. */
  readonly advertisedTools: readonly Record<string, unknown>[];
  /** The scrubbed env the real app-server was spawned under (the sparse replaced env). */
  readonly spawnEnv: NodeJS.ProcessEnv;
  /** The terminal turn status (`completed` / `failed` / …), or undefined if it never settled. */
  readonly turnStatus: string | undefined;
  /** Every `/v1/responses` request body the fake provider received. */
  readonly providerRequests: readonly ResponsesBody[];
  /** Any error the fake provider recorded (an auth mismatch, oversize, etc.). */
  readonly providerErrors: readonly string[];
  /** The measured startup+turn wall time in ms (bounded by {@link P_SUITE_DEADLINE_MS}). */
  readonly elapsedMs: number;
  /** How the pinned binary was resolved (image-baked / cache-install). */
  readonly binSource: string;
  readonly binaryEvidence: { path: string; version: string; archiveDigest: string; receipt: string; integrity: true };
  readonly notes: readonly CodexNotification[];
  readonly threadId: string;
}

/** Shared inbound callback observer; count before the advice rejection can hide an attempt. */
export function observeCallbackRequest(note: CodexNotification, lifecycle: ProtocolTurnOptions["lifecycle"],
  observer: { attempts: number }, respond: (id: string | number, payload: { error: { code: number; message: string } }) => void): boolean {
  if (note.kind !== "activity" || note.requestId === undefined || note.method !== "item/tool/call") return false;
  if (++observer.attempts > 16) throw new Error("callback-budget");
  if (lifecycle !== "advice") return false;
  respond(note.requestId, { error: { code: -32601, message: "codex advice is tool-less" } });
  return true;
}

/** Inputs to one driven turn. Only the policy-relevant knobs; the launch plumbing is fixed. */
export interface ProtocolTurnOptions {
  /** The immutable grants the REAL broker binds every callback to (the model-visible subset is
   *  advertised via buildCodexDynamicTools). */
  readonly grants: RunGrants;
  /** The screen policy threaded to the broker (extraSecretPaths / dockerWired). */
  readonly screenPolicy?: ScreenPolicy;
  /** The known delegation roles; unset denies every delegation as an unknown role. */
  readonly allowedRoles?: ReadonlySet<string>;
  /** The synchronous child-thread delegation seam; defaults to a deny stub (delegation not driven). */
  readonly delegate?: DelegateSeam;
  /** The exact bearer credential the app-server presents to the fake provider. */
  readonly credential: string;
  readonly provider: FakeProvider;
  readonly resumeOverride?: FakeProvider;
  readonly onResumeProbe?: () => void;
  readonly session?: { seed?: string; resumeId?: string; save?: string };

  readonly fixture?: { table: (table: string) => string; env: NodeJS.ProcessEnv };
  readonly metadataObservation?: (value: { modelContextWindowOwn: boolean; modelContextWindowNullableInteger: boolean }) => void;
  readonly warningObservation?: () => void;
  readonly diagnostic?: (code: "config-rejected" | "tls-refused") => void;
  readonly signal?: AbortSignal;
  readonly configObservation?: (value: ReturnType<typeof configProjection>) => void;
  readonly captureActual?: (env: NodeJS.ProcessEnv) => void;
  readonly capture?: (env: NodeJS.ProcessEnv) => void;
  /** The scripted fake-provider responder (fixed items, never a model choice). */

  /** The command-identity spawn seam the broker calls for an allowed shell effect (test-owned). */
  readonly spawnCommand: SpawnCommandSeam;
  /** The openat2 fileop client the broker calls for an allowed file effect (test-owned). */
  readonly fileop: FileopClient;
  /** Trusted parent env presented to the launcher before it constructs the fully replaced child env. */
  readonly launcherEnv?: NodeJS.ProcessEnv;
  /** The model id (default gpt-6-astra, a contract model). */
  readonly model?: string;
  /** Trusted reasoning effort for the real turn/start wire proof. */
  readonly effort?: "medium";
  /** Optional per-callback origin (default "root"). */
  readonly origin?: "root" | "child" | "unknown";
  /** PRD #1533: thread `codeModeHost` onto the launch spec so the REAL `config.ts` loopback
   *  builder emits `code_mode_host = <value>`. `true` enables the real code-mode execution host
   *  (a nested `exec` cell's worker callback then reaches the broker); `false`/absent keeps it
   *  disabled (the same exec cell fails with "code-mode host is disabled" and no callback fires). */
  readonly codeModeHost?: boolean;
  readonly allowCallbacks?: boolean;
  readonly adversarialAdvertisement?: boolean;
  /** #1566: worker-created child, saved-root resume, or isolated advice protocol posture. */
  readonly lifecycle?: "start" | "resume" | "child" | "advice";
  /** How long to wait for the single turn to complete (default 60s). */
  readonly turnDeadlineMs?: number;
}

/**
 * Build a fake spawnSupervisor that spawns the REAL `codex app-server` directly and synthesizes
 * the Go supervisor's fd3 control / fd4 evidence so launchCodexRoot's started/dispose contract
 * runs. No setpriv/uid switch — the OS posture in `started` is fabricated (P-layer; OS isolation
 * is O-layer). The spawned env is captured into `envSink` (the sparse replaced env isolation
 * oracle). Disposal really SIGKILLs the codex child.
 */
function makeDirectCodexSpawn(codexBin: string, expectUid: number, envSink: { env?: NodeJS.ProcessEnv }, opts: ProtocolTurnOptions, ownedChildren: Array<() => Promise<void>>, saveHome: (home: string) => void): SpawnSupervisor {
  return (_command, _args, options): SupervisorProcess => {
    envSink.env = options.env;
    opts.capture?.(options.env);
    if (opts.session?.save) saveHome(options.env.CODEX_HOME!);

    if (opts.fixture) {
      const configPath = path.join(options.env.CODEX_HOME!, "config.toml");
      const config = readFileSync(configPath, "utf8");
      const split = config.indexOf("[model_providers.");
      if (split < 0) throw new Error("fixture-config");
      const table = opts.fixture.table(config.slice(split));
      validateFixtureTable(table);
      if (table.split("\n")[0] !== config.slice(split).split("\n")[0]) throw new Error("fixture-config");
      validateFixtureEnv(opts.fixture.env);
      writeFileSync(configPath, config.slice(0, split) + table, { mode: 0o600 });
    }
    const projection = configProjection(readFileSync(path.join(options.env.CODEX_HOME!, "config.toml"), "utf8"), options.cwd!, opts.provider.baseUrl);
    opts.configObservation?.(projection);
    if (!projection.untrusted || !projection.nativeConfigDisabled || !projection.auxiliaryConfigDisabled || !projection.destinationMatched || !projection.zeroRequestRetries || !projection.zeroStreamRetries || !projection.websocketsDisabled) throw new Error("config-policy");
    opts.signal?.throwIfAborted();
    opts.captureActual?.({ ...options.env, ...opts.fixture?.env });
    const child: ChildProcess = nodeSpawn(codexBin, ["app-server"], {
      cwd: options.cwd,
      env: { ...options.env, ...opts.fixture?.env },
      stdio: ["pipe", "pipe", "pipe"],
    });
    ownedChildren.push(ownChild(child));
    const abort = () => { child.kill("SIGKILL"); };
    opts.signal?.addEventListener("abort", abort, { once: true });
    child.once("close", () => opts.signal?.removeEventListener("abort", abort));
    let diagnosticText = "";
    child.stderr?.on("data", (chunk: Buffer) => {
      diagnosticText = (diagnosticText + chunk.toString("utf8")).slice(-65536);
      if (unknownModelWarning(diagnosticText)) opts.warningObservation?.();
      if (/unknown variant .unsupported.|unsupported.*wire_api/i.test(diagnosticText)) opts.diagnostic?.("config-rejected");
      if (/certificate verify failed|invalid peer certificate|UnknownIssuer|certificate.*unknown issuer/i.test(diagnosticText)) opts.diagnostic?.("tls-refused");
    });
    const clearCaseTimer = ownedDeadline(() => { child.kill("SIGKILL"); });
    child.once("close", clearCaseTimer);
    const control = new PassThrough();
    const evidence = new PassThrough();
    const emitter = new EventEmitter();
    let disposing = false;
    let exited = false;
    const ownedPid = child.pid;
    let killDelivered = false;
    let disposalFinished = false;
    let realClose: [number | null, NodeJS.Signals | null] | undefined;
    const forwardClose = (): void => {
      if (realClose && disposalFinished) {
        emitter.emit("close", ...realClose);
        realClose = undefined;
      }
    };
    child.once("close", (code, signal) => {
      realClose = [code, signal];
      forwardClose();
    });

    const writeEvidence = (frame: Record<string, unknown>): void => {
      evidence.write(`${JSON.stringify(frame)}\n`);
    };

    queueMicrotask(() => {
      writeEvidence({
        event: "started",
        supervisorPid: child.pid,
        childPid: ownedPid,
        subreaper: true,
        nondumpable: true,
        uid: expectUid,
        liveCapsZero: true,
        capBoundingSet: "0xc0",
        noNewPrivs: true,
      });
    });

    child.once("exit", (code, signal) => {
      exited = true;
      if (!disposing) emitter.emit("exit", code, signal);
    });
    child.once("error", (err) => emitter.emit("error", err));

    let controlBuf = "";
    control.on("data", (chunk: Buffer) => {
      controlBuf += chunk.toString("utf8");
      let nl = controlBuf.indexOf("\n");
      while (nl !== -1) {
        const line = controlBuf.slice(0, nl);
        controlBuf = controlBuf.slice(nl + 1);
        let frame: Record<string, unknown>;
        try {
          frame = JSON.parse(line) as Record<string, unknown>;
        } catch {
          nl = controlBuf.indexOf("\n");
          continue;
        }
        if (frame.op === "snapshot") {
          writeEvidence({ event: "snapshot", id: frame.id, processes: [] });
        } else if (frame.op === "dispose") {
          disposing = true;
          const finish = (): void => {
            // Only the exact owned child's observed exit can complete this report.
            writeEvidence({
              event: "dispose", id: frame.id, state: "drained", authority: "ECHILD+__WALL",
              killed: killDelivered ? [ownedPid] : [],
              reaped: exited && ownedPid !== undefined ? [ownedPid] : [],
            });
            evidence.end();
            disposalFinished = true;
            // Synthetic supervisor success is separate from the real child's exit/signal.
            emitter.emit("exit", 0, null);
            // PassThrough data parsing is synchronous: the final response is consumed first.
            forwardClose();
          };
          if (exited) finish();
          else {
            child.once("exit", finish);
            try {
              killDelivered = child.kill("SIGKILL");
              if (!killDelivered) emitter.emit("error", new Error("owned child SIGKILL was not delivered"));
            } catch (error) {
              emitter.emit("error", error);
            }
          }
        }
        nl = controlBuf.indexOf("\n");
      }
    });

    const proc: SupervisorProcess = {
      pid: child.pid,
      stdin: child.stdin,
      stdout: child.stdout,
      stderr: child.stderr,
      stdio: [child.stdin, child.stdout, child.stderr, control, evidence],
      once(event: string, listener: (...args: never[]) => void): unknown {
        emitter.once(event, listener as (...a: unknown[]) => void);
        return proc;
      },
      on(event: string, listener: (...args: never[]) => void): unknown {
        emitter.on(event, listener as (...a: unknown[]) => void);
        return proc;
      },
    };
    return proc;
  };
}

const fakeMakeRunnerTrees: MakeRunnerTrees = (request) => {
  mkdirSync(request.root, { recursive: true, mode: 0o700 });
  for (const dir of request.dirs) mkdirSync(dir, { recursive: true, mode: 0o700 });
  if (request.sharedSessionRead !== undefined) {
    mkdirSync(request.sharedSessionRead.sessionDir, { recursive: true, mode: 0o700 });
  }
  if (request.sessionSeedDir) {
    const home = request.sharedSessionRead?.codexHome ?? path.dirname(request.files.find(f => f.path.endsWith("/config.toml"))!.path);
    copySyntheticSessions(request.sessionSeedDir, path.join(home, "sessions"));
  }
  for (const file of request.files) writeFileSync(file.path, file.content, { mode: file.mode });
};
const fakeRemoveRunnerTree: RemoveRunnerTree = (request) => {
  rmSync(request.root, { recursive: true, force: true });
};

/** The `tools` array the app-server advertised to the model on the first request (else []). */
function advertisedToolsOf(requests: readonly ResponsesBody[]): Record<string, unknown>[] {
  for (const body of requests) {
    const tools = (body as { tools?: unknown }).tools;
    if (Array.isArray(tools)) return tools.filter((t): t is Record<string, unknown> => t !== null && typeof t === "object");
  }
  return [];
}

/**
 * Drive ONE real-app-server turn under a test's grants/screenPolicy, routing every model-selected
 * callback through a REAL broker, and return the observations. Owns its temp trees, the fake
 * provider, the launched process and the transport — every one is torn down before it returns.
 */
export async function runProtocolTurn(mods: ProtocolModules, opts: ProtocolTurnOptions): Promise<ProtocolObservation> {
  const ownedChildren: Array<() => Promise<void>> = [];
  const startedAt = Date.now();
  const preflight = await probeCodexRuntime({ prefix: "/opt/uzi-codex", env: {} });
  if (!preflight.capable) throw new Error("prerequisite");
  const resolved = resolveCodexBin();
  if (resolved.source !== "image-baked") throw new Error("prerequisite");
  const lock = parseLock(readFileSync(path.resolve(__dirname, "../../agent/codex/codex-package.lock"), "utf8"));
  const arch = resolveArch(process.arch);
  const prefix = path.dirname(resolved.prefix);
  const integrity = await probeCodexRuntime({ prefix, expectedVersion: lock.version, arch, expectedLockDigest: lock.sha256[arch] });
  if (!integrity.capable) throw new Error(`Codex integrity prerequisite failed: ${integrity.reason}`);
  assertBinaryVersion(resolved.codexBin, lock.version);
  const binaryEvidence = { path: resolved.codexBin, version: `codex-cli ${lock.version}`, archiveDigest: lock.sha256[arch],
    receipt: path.join(prefix, `${lock.version}.receipt.json`), integrity: true as const };
  const runnerUid = process.getuid?.() ?? 10001;
  const model = opts.model ?? "gpt-6-astra";
  const origin = opts.origin ?? "root";

  let provider: FakeProvider | undefined;
  let base: string | undefined;
  let handle: Awaited<ReturnType<ProtocolModules["launchCodexRoot"]>> | undefined;
  let transport: ReturnType<ProtocolModules["createCodexTransport"]> | undefined;
  let sessionHome: string | undefined;
  return withProtocolCleanup(async () => {
    let priming = opts.lifecycle === "resume";
    provider = opts.provider;
    const scratch = path.resolve(__dirname, "../../.uzi/scratch");
    mkdirSync(scratch, { recursive: true });
    base = realpathSync(mkdtempSync(path.join(scratch, "codex-m4-p-")));
    const ownedDataRoot = path.join(base, "root");
    const cwd = path.join(base, "cwd");
    mkdirSync(cwd, { recursive: true, mode: 0o700 });

    const registry = new mods.ExecutionRegistry(mods.newLocalExecutionEpoch(1));
    const brokerOpts: CodexCallbackBrokerOptions = {
      registry,
      spawnCommand: opts.spawnCommand,
      fileop: opts.fileop,
      worktreePath: cwd,
      grants: opts.grants,
      delegate: opts.delegate
        ?? (async () => ({ ok: false, code: "child_denied", message: "delegation is not exercised by this P case" })),
      allowedRoles: opts.allowedRoles ?? new Set<string>(),
      screenPolicy: opts.screenPolicy ?? {},
    };
    const broker = new mods.CodexCallbackBroker(brokerOpts);

    const envSink: { env?: NodeJS.ProcessEnv } = {};
    const callbacks: ObservedCallback[] = [];
    const callbackObserver = { attempts: 0 };
    const rawNotes: CodexNotification[] = [];
    const notificationBudget = new Budget(4 * 1024 * 1024, 16384);
    const launchPlan = buildProviderLaunchPlan(mods, {
      executableBin: resolved.codexBin,
      provider: { name: "openai", baseUrl: provider.baseUrl, envKey: "FAKE_PROVIDER_API_KEY" },
      model,
      cwd,
      ownedDataRoot,
      ...(opts.codeModeHost !== undefined ? { codeModeHost: opts.codeModeHost } : {}),
    });
    if (opts.session?.seed) copySyntheticSessions(opts.session.seed, path.join(ownedDataRoot + ".session-seed", "sessions"));
    const deps: LauncherDeps = {
      env: opts.launcherEnv ?? { UZI_UID_SPLIT: "1" },
      resolveRunnerUid: () => runnerUid,
      makeRunnerTrees: fakeMakeRunnerTrees,
      removeRunnerTree: fakeRemoveRunnerTree,
      spawnSupervisor: makeDirectCodexSpawn(launchPlan.executableBin, runnerUid, envSink, opts, ownedChildren, home => { sessionHome = home; }),
      appServerAuthOpenAIBaseUrlForTest: provider.baseUrl,
      deadlines: { started: 5_000 },
    };
    handle = await mods.launchCodexRoot({ ...launchPlan.spec, ...(opts.session?.seed ? { seedSession: true } : {}) }, deps);
    const inbound = handle.transport.stdout;
    const outbound = handle.transport.stdin;
    if (inbound === null || outbound === null) throw new Error("the app-server transport streams are unavailable");
    transport = mods.createCodexTransport({ inbound, outbound });
    const closeTransport = () => { transport!.close(); };
    opts.signal?.addEventListener("abort", closeTransport, { once: true });
    if (opts.signal?.aborted) closeTransport();
    const auth = mods.createCodexAppServerAuth({ mode: "api_key", apiKey: opts.credential });
    await auth.authenticate(transport);
    if (opts.metadataObservation) {
      const response = await transport.request<{ config?: Record<string, unknown> }>("config/read", {});
      const config = response.config ?? {};
      const value = config.model_context_window;
      opts.metadataObservation({ modelContextWindowOwn: Object.hasOwn(config, "model_context_window"),
        modelContextWindowNullableInteger: value === null || typeof value === "number" && Number.isSafeInteger(value) });
    }

    let threadId: string | undefined;
    let turnId: string | undefined;
    let turnStatus: string | undefined;
    let primeFinished!: () => void;
    const primeCompletion = new Promise<void>(resolve => { primeFinished = resolve; });
    const notes = transport.notifications();
    const consume = (async (): Promise<void> => {
      for await (const note of notes) {
        notificationBudget.take(Buffer.byteLength(JSON.stringify(note)));
        if (/certificate verify failed|invalid peer certificate|UnknownIssuer|certificate.*unknown issuer/i.test(JSON.stringify(note))) opts.diagnostic?.("tls-refused");
        if (unknownModelWarning(JSON.stringify(note))) opts.warningObservation?.();
        // M1 consumes notifications without retaining transcripts.
        if (rawNotes.length > 0) throw new Error("unexpected-retention");
        if (note.kind === "turn_completed" && note.threadId === threadId && note.turnId === turnId) {
          if (priming) { primeFinished(); continue; }
          turnStatus = note.status;
          return;
        }
        if (observeCallbackRequest(note, opts.lifecycle, callbackObserver, (id, payload) => transport!.respond(id, payload))) continue;
        if (note.kind === "activity" && note.requestId !== undefined && note.method === "item/tool/call") {
          const p = (note.params ?? {}) as Record<string, unknown>;
          const rt = { threadId: String(p.threadId), turnId: String(p.turnId), callId: String(p.callId) };
          if (!opts.allowCallbacks) throw new Error("unexpected-callback");
          if (callbacks.length >= 16) throw new Error("callback-budget");
          const result = await broker.handleToolCall(rt, p.tool, p.arguments, origin);
          callbacks.push({ tool: String(p.tool), callId: rt.callId, result });
          transport!.respond(note.requestId, { result: { success: result.ok,
            contentItems: [{ type: "inputText", text: result.ok ? JSON.stringify(result.output) : result.message }] } });
        } else if (note.kind === "activity" && note.requestId !== undefined) {
          transport!.respond(note.requestId, { error: { code: -32601, message: opts.lifecycle === "advice" ? "codex advice is tool-less" : "unsupported request" } });
        }
      }
    })();
    void consume.catch(() => undefined);

    const threadParams = {
      model,
      modelProvider: mods.CODEX_M3B_LOOPBACK_PROVIDER_NAME,
      cwd,
      approvalPolicy: "never",
      approvalsReviewer: "user",
      sandbox: "danger-full-access",
      ephemeral: false,
      environments: [],
      dynamicTools: opts.lifecycle === "advice" ? [] : mods.buildCodexDynamicTools(opts.adversarialAdvertisement ? { ...opts.grants, allowedTools: new Set(["Bash"]) } : opts.grants),
      config: { project_doc_max_bytes: 0, projects: { [cwd]: { trust_level: "untrusted" } } },
      developerInstructions: "M4 C3 P developer instructions",
    };
    if (opts.lifecycle === "child") {
      // A separate worker-created thread, never Codex's upstream spawn_agent.
      await transport.request("thread/start", threadParams);
    }
    if (opts.session?.resumeId) opts.onResumeProbe?.();
    const threadRes = opts.session?.resumeId
      ? await transport.request<{ thread?: { id?: string } }>("thread/resume", { ...threadParams, threadId: opts.session.resumeId })
      : await transport.request<{ thread?: { id?: string } }>("thread/start", threadParams);
    threadId = threadRes.thread?.id;
    if (threadId === undefined) throw new Error("thread/start returned no thread id");
    if (opts.session?.resumeId && threadId !== opts.session.resumeId) throw new Error("resume identity mismatch");

    if (opts.lifecycle === "resume") {
      const prime = await transport.request<{ turn?: { id?: string } }>("turn/start", {
        threadId, model, environments: [], input: [{ type: "text", text: "save root before resume" }],
      });
      turnId = prime.turn?.id;
      await deadline(primeCompletion, "saved root completion", opts.turnDeadlineMs ?? 12_000);
      priming = false;
      if (opts.resumeOverride) await provider.close();
      opts.onResumeProbe?.();
      const resumed = await transport.request<{ thread?: { id?: string } }>("thread/resume", {
        threadId, model, modelProvider: mods.CODEX_M3B_LOOPBACK_PROVIDER_NAME, cwd,
        approvalPolicy: "never", config: { ...threadParams.config, ...(opts.resumeOverride ? { ["model_providers." + mods.CODEX_M3B_LOOPBACK_PROVIDER_NAME + ".base_url"]: opts.resumeOverride.baseUrl } : {}) }, developerInstructions: threadParams.developerInstructions,
      });
      if (resumed.thread?.id !== threadId) throw new Error("resume did not retain the saved root");
    }

    const turnRes = await transport.request<{ turn?: { id?: string } }>("turn/start", {
      threadId,
      input: [{ type: "text", text: "M4 C3 P: run the scripted turn" }],
      environments: [],
      model,
      ...(opts.effort === undefined ? {} : { effort: opts.effort }),
    });
    turnId = turnRes.turn?.id;
    if (turnId === undefined) throw new Error("turn/start returned no turn id");

    await deadline(consume, "codex turn completion", opts.turnDeadlineMs ?? 12_000);

    if (opts.session?.save) {
      if (!sessionHome) throw new Error("session home missing");
      copySyntheticSessions(path.join(sessionHome, "sessions"), opts.session.save);
    }
    return {
      threadId,
      callbacks,
      callbackAttempted: callbackObserver.attempts > 0,
      advertisedTools: advertisedToolsOf(provider.requests),
      spawnEnv: envSink.env ?? {},
      turnStatus,
      providerRequests: [...provider.requests],
      providerErrors: [...provider.errors],
      elapsedMs: Date.now() - startedAt,
      binSource: resolved.source,
      binaryEvidence,
      notes: rawNotes,
    };
  }, async () => {
    const results = await Promise.allSettled([
      Promise.resolve().then(() => transport?.close()),
      Promise.resolve().then(async () => { if (handle && !(await handle.dispose()).clean) throw new Error("cleanup-failure"); }),
    ]);
    results.push(...await Promise.allSettled(ownedChildren.map(settle => settle())));
    results.push(...await Promise.allSettled([Promise.resolve().then(() => { if (base) rmSync(base, { recursive: true, force: true }); })]));
    const failures = results.flatMap(r => r.status === "rejected" ? [r.reason] : []);
    if (failures.length) throw new AggregateError(failures, "cleanup-failure");
  });
}

export async function withProtocolCleanup<T>(run: () => Promise<T>, cleanup: () => Promise<void>): Promise<T> {
  let value: T | undefined;
  let failure: { error: unknown } | undefined;
  try { value = await run(); } catch (error) { failure = { error }; }
  try { await cleanup(); } catch (error) {
    if (!failure) throw error;
    throw new AggregateError([failure.error, error], failure.error instanceof Error ? failure.error.message : "protocol-failure", { cause: failure.error });
  }
  if (failure) throw failure.error;
  return value as T;
}

export function validateFixtureEnv(env: NodeJS.ProcessEnv): void {
  if (Object.keys(env).some(key => !["SSL_CERT_FILE", "CODEX_CA_CERTIFICATE", "SPIKE_KEY"].includes(key))
      || Object.values(env).some(value => typeof value !== "string")) throw new Error("fixture-env");
}
export function validateFixtureTable(table: string): void {
  // A transform may alter only one provider table, never append configuration sections.
  if (!/^\[model_providers\.(?:[A-Za-z0-9_-]+|"[A-Za-z0-9_-]+")\]\r?\n/.test(table)
      || (table.match(/^[\t ]*\[/gm) ?? []).length !== 1) throw new Error("fixture-config");
  const urls = [...table.matchAll(/^base_url = "([^"]+)"$/gm)];
  if (urls.length !== 1) throw new Error("fixture-url");
  const url = new URL(urls[0][1]);
  if (!["http:", "https:"].includes(url.protocol) || !["127.0.0.1", "127.0.0.2"].includes(url.hostname)
      || url.username || url.password || url.search || url.hash || url.pathname !== "/v1" || !url.port)
    throw new Error("fixture-url");
}

// Copies only fixture-owned synthetic sessions. Symlinks, oversized trees and excess depth fail closed.
export function copySyntheticSessions(source: string, target: string): void {
  let files = 0, bytes = 0;
  const visit = (from: string, to: string, depth: number): void => {
    if (depth > 8) throw new Error("session-copy-limit");
    const stat = lstatSync(from);
    if (stat.isSymbolicLink()) throw new Error("session-copy-symlink");
    if (stat.isDirectory()) {
      if (++files > 128) throw new Error("session-copy-limit");
      mkdirSync(to, { recursive: true, mode: 0o700 });
      const directory = opendirSync(from, { bufferSize: 1 });
      try {
        let entry;
        while ((entry = directory.readSync()) !== null) {
          if (files >= 128) throw new Error("session-copy-limit");
          visit(path.join(from, entry.name), path.join(to, entry.name), depth + 1);
        }
      } finally { directory.closeSync(); }
    } else {
      if (!stat.isFile() || ++files > 128 || (bytes += stat.size) > 4 * 1024 * 1024) throw new Error("session-copy-limit");
      // Charge actual reads too: a growing owned source cannot bypass the byte cap.
      bytes -= stat.size;
      const input = openSync(from, constants.O_RDONLY | constants.O_NOFOLLOW);
      let output: number | undefined;
      try {
        if (!fstatSync(input).isFile()) throw new Error("session-copy-limit");
        output = openSync(to, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL, 0o600);
        const buffer = Buffer.alloc(65536);
        let length;
        while ((length = readSync(input, buffer, 0, buffer.length, null)) !== 0) {
          bytes += length;
          if (bytes > 4 * 1024 * 1024) throw new Error("session-copy-limit");
          let written = 0;
          while (written < length) written += writeSync(output, buffer, written, length - written);
        }
      } finally { closeSync(input); if (output !== undefined) closeSync(output); }
    }
  };
  visit(source, target, 0);
}

export function configProjection(config: string, cwd: string, baseUrl: string) {
  const has = (line: string) => config.split("\n").includes(line);
  return {
    untrusted: config.includes("[projects." + JSON.stringify(cwd) + "]\ntrust_level = \"untrusted\""),
    nativeConfigDisabled: ["apps", "plugins", "shell_tool", "view_image", "sleep_tool", "apply_patch_freeform", "shell_snapshot", "shell_snapshot_v2", "code_mode", "code_mode_only", "code_mode_prewarm", "unified_exec", "hooks", "multi_agent", "multi_agent_v2"].every(k => has(k + " = false")) && has('web_search = "disabled"'),
    auxiliaryConfigDisabled: has("check_for_update_on_startup = false") && config.includes("[analytics]\nenabled = false") && config.includes("[feedback]\nenabled = false"),
    destinationMatched: has("base_url = " + JSON.stringify(baseUrl)) || has("base_url = " + JSON.stringify(baseUrl.replace("http:", "https:"))),
    zeroRequestRetries: has("request_max_retries = 0"),
    zeroStreamRetries: has("stream_max_retries = 0"),
    websocketsDisabled: has("supports_websockets = false"),
  };
}

// Only pinned static warning templates count; arbitrary model text is never emitted.
export function unknownModelWarning(text: string): boolean {
  return text.includes("Model metadata for `spike-unknown-model` not found. Defaulting to fallback metadata; this can degrade performance and cause issues.")
    || text.includes("Unknown model spike-unknown-model is used. This will use fallback model metadata.");
}
