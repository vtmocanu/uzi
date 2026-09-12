// PRD #1287 C3 — the shared P-layer (real app-server) driver, extracted from the C1
// startup-smoke (startup-smoke.test.ts) so the C3 policy/native-bypass/isolation P cases
// compose the SAME production path without copying the launch plumbing.
//
// It drives the PINNED real `codex app-server` through the PRODUCTION launch primitive
// (launchCodexRoot) + the PRODUCTION loopback config builder (buildCodexLoopbackTestConfigToml,
// selected via LauncherDeps.appServerAuthOpenAIBaseUrlForTest + useAppServerAuth) + the
// PRODUCTION transport + the PRODUCTION app-server auth owner, and routes every model-selected
// callback through a REAL CodexCallbackBroker. Only the launcher OS seams are injected
// (spawnSupervisor, makeRunnerTrees, removeRunnerTree) — exactly D7's "launcher/setpriv, fileop
// and command OS effects injected through existing production seam types". Those injections prove
// PROTOCOL admission + outcome, never OS isolation (that is O-layer, maintainer-owned).
//
// A test constructs the grants + screenPolicy + spawn/fileop seams it wants to observe, hands a
// scripted fake-provider responder, and reads back the observations: every callback the broker
// saw, its per-call result, the advertised tool schema the app-server offered the model, the env
// the app-server was spawned under (the sparse replaced env), and the provider-visible requests.

import { EventEmitter } from "node:events";
import { spawn as nodeSpawn, type ChildProcess } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";

import { deadline } from "../codex-m0/harness.mjs";
import { FakeProvider, type ResponseItem, type ResponsesBody } from "./fake-provider.js";
import { resolveCodexBin } from "./provision.js";
import {
  loadPackagedLauncher,
  loadPackagedBroker,
  loadPackagedConfig,
  loadPackagedTransport,
  loadPackagedAppServerAuth,
  loadPackagedDynamicTools,
  loadPackagedRegistry,
} from "./packaged-modules.js";

import type {
  CodexLaunchSpec,
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
export const P_SUITE_DEADLINE_MS = 90_000;

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
  /** The scripted fake-provider responder (fixed items, never a model choice). */
  readonly respond: (body: ResponsesBody, provider: FakeProvider) => ResponseItem[] | Promise<ResponseItem[]>;
  /** The command-identity spawn seam the broker calls for an allowed shell effect (test-owned). */
  readonly spawnCommand: SpawnCommandSeam;
  /** The openat2 fileop client the broker calls for an allowed file effect (test-owned). */
  readonly fileop: FileopClient;
  /** The model id (default gpt-6-astra, a contract model). */
  readonly model?: string;
  /** Optional per-callback origin (default "root"). */
  readonly origin?: "root" | "child" | "unknown";
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
function makeDirectCodexSpawn(codexBin: string, expectUid: number, envSink: { env?: NodeJS.ProcessEnv }): SpawnSupervisor {
  return (_command, _args, options): SupervisorProcess => {
    envSink.env = options.env;
    const child: ChildProcess = nodeSpawn(codexBin, ["app-server"], {
      cwd: options.cwd,
      env: options.env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    const control = new PassThrough();
    const evidence = new PassThrough();
    const emitter = new EventEmitter();
    let disposing = false;
    let exited = false;

    const writeEvidence = (frame: Record<string, unknown>): void => {
      evidence.write(`${JSON.stringify(frame)}\n`);
    };

    queueMicrotask(() => {
      writeEvidence({
        event: "started",
        supervisorPid: child.pid,
        childPid: child.pid,
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
            writeEvidence({ event: "dispose", id: frame.id, state: "drained", authority: "ECHILD+__WALL" });
            emitter.emit("exit", 0, null);
          };
          if (exited) finish();
          else {
            child.once("exit", finish);
            child.kill("SIGKILL");
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
  const startedAt = Date.now();
  const resolved = resolveCodexBin();
  const runnerUid = process.getuid?.() ?? 10001;
  const model = opts.model ?? "gpt-6-astra";
  const origin = opts.origin ?? "root";

  const provider = await FakeProvider.start({ credential: opts.credential, respond: opts.respond });
  const base = realpathSync(mkdtempSync(path.join(tmpdir(), "codex-m4-p-")));
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
  let handle: Awaited<ReturnType<ProtocolModules["launchCodexRoot"]>> | undefined;
  let transport: ReturnType<ProtocolModules["createCodexTransport"]> | undefined;

  try {
    const spec: CodexLaunchSpec = {
      kind: "provider",
      provider: { name: "openai", baseUrl: provider.baseUrl, envKey: "FAKE_PROVIDER_API_KEY" },
      model,
      codexBin: resolved.codexBin,
      supervisorBin: mods.SUPERVISOR_BIN,
      childArgv: [...mods.PROVIDER_CHILD_ARGV],
      cwd,
      ownedDataRoot,
      useAppServerAuth: true,
      authMode: "api_key",
    };
    const deps: LauncherDeps = {
      env: { UZI_UID_SPLIT: "1" },
      resolveRunnerUid: () => runnerUid,
      makeRunnerTrees: fakeMakeRunnerTrees,
      removeRunnerTree: fakeRemoveRunnerTree,
      spawnSupervisor: makeDirectCodexSpawn(resolved.codexBin, runnerUid, envSink),
      appServerAuthOpenAIBaseUrlForTest: provider.baseUrl,
      deadlines: { started: 60_000 },
    };
    handle = await mods.launchCodexRoot(spec, deps);
    const inbound = handle.transport.stdout;
    const outbound = handle.transport.stdin;
    if (inbound === null || outbound === null) throw new Error("the app-server transport streams are unavailable");
    transport = mods.createCodexTransport({ inbound, outbound });
    const auth = mods.createCodexAppServerAuth({ mode: "api_key", apiKey: opts.credential });
    await auth.authenticate(transport);

    let threadId: string | undefined;
    let turnId: string | undefined;
    let turnStatus: string | undefined;
    const notes = transport.notifications();
    const consume = (async (): Promise<void> => {
      for await (const note of notes) {
        if (note.kind === "turn_completed" && note.threadId === threadId && note.turnId === turnId) {
          turnStatus = note.status;
          return;
        }
        if (note.kind === "activity" && note.requestId !== undefined && note.method === "item/tool/call") {
          const p = (note.params ?? {}) as Record<string, unknown>;
          const rt = { threadId: String(p.threadId), turnId: String(p.turnId), callId: String(p.callId) };
          const result = await broker.handleToolCall(rt, p.tool, p.arguments, origin);
          callbacks.push({ tool: String(p.tool), callId: rt.callId, result });
          const text = result.ok ? JSON.stringify(result.output) : result.message;
          transport!.respond(note.requestId, { result: { success: result.ok, contentItems: [{ type: "inputText", text }] } });
        }
      }
    })();
    void consume.catch(() => undefined);

    const threadRes = await transport.request<{ thread?: { id?: string } }>("thread/start", {
      model,
      modelProvider: mods.CODEX_M3B_LOOPBACK_PROVIDER_NAME,
      cwd,
      approvalPolicy: "never",
      approvalsReviewer: "user",
      sandbox: "danger-full-access",
      ephemeral: false,
      environments: [],
      dynamicTools: mods.buildCodexDynamicTools(opts.grants),
      config: { project_doc_max_bytes: 0, projects: { [cwd]: { trust_level: "untrusted" } } },
      developerInstructions: "M4 C3 P developer instructions",
    });
    threadId = threadRes.thread?.id;
    if (threadId === undefined) throw new Error("thread/start returned no thread id");

    const turnRes = await transport.request<{ turn?: { id?: string } }>("turn/start", {
      threadId,
      input: [{ type: "text", text: "M4 C3 P: run the scripted turn" }],
      environments: [],
      model,
    });
    turnId = turnRes.turn?.id;
    if (turnId === undefined) throw new Error("turn/start returned no turn id");

    await deadline(consume, "codex turn completion", opts.turnDeadlineMs ?? 60_000);

    return {
      callbacks,
      advertisedTools: advertisedToolsOf(provider.requests),
      spawnEnv: envSink.env ?? {},
      turnStatus,
      providerRequests: [...provider.requests],
      providerErrors: [...provider.errors],
      elapsedMs: Date.now() - startedAt,
      binSource: resolved.source,
    };
  } finally {
    try {
      await transport?.close();
    } catch {
      /* best effort */
    }
    try {
      if (handle !== undefined) await handle.dispose();
    } catch {
      /* best effort — the shim SIGKILLs the codex child on dispose */
    }
    try {
      await provider.close();
    } catch {
      /* best effort */
    }
    rmSync(base, { recursive: true, force: true });
  }
}
