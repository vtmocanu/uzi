// PRD #1287 C1 — the REAL-binary P startup / allowed-callback smoke (D7 layer P).
//
// This drives the PINNED real Codex app-server directly (no setpriv, no Go supervisor) through
// the PRODUCTION launch primitive (launchCodexRoot) + the PRODUCTION loopback config builder
// (buildCodexLoopbackTestConfigToml, selected via LauncherDeps.appServerAuthOpenAIBaseUrlForTest
// + useAppServerAuth) + the PRODUCTION transport + the PRODUCTION app-server auth owner, and
// routes ONE allowed uzi_bash callback through a REAL CodexCallbackBroker. Only the launcher OS
// seams are injected (spawnSupervisor, makeRunnerTrees, removeRunnerTree) — exactly the D7
// "launcher/setpriv, fileop and command OS effects injected through existing production seam
// types". Those injections prove PROTOCOL admission + outcome, never OS isolation (that is O).
//
// The injected spawnSupervisor spawns the real `codex app-server` directly and synthesizes the
// Go supervisor's evidence channel (fd3 control / fd4 evidence) so launchCodexRoot's fail-closed
// started/dispose contract still runs; the OS posture in that synthetic `started` frame is NOT
// proven here (it is O-layer, maintainer-owned). Everything else is the real production path.
//
// MEASURED startup+turn wall time on this worker: ~1.0s (image-baked binary; measured 1017ms on
// 2026-09-12). The suite deadline is set well above that at 90s; the case is bounded to ONE turn.

import { before, test } from "node:test";
import assert from "node:assert/strict";
import { spawn as nodeSpawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";

import { deadline } from "../codex-m0/harness.mjs";
import {
  FakeProvider,
  bashArgCanary,
  countToolCallbacks,
  dummyCredential,
  scriptedAllowedBashResponder,
  toolCallbackTexts,
  SMOKE_BASH_CALL_ID,
} from "./fake-provider.js";
import { resolveCodexBin } from "./provision.js";
import { recordEvidence } from "./evidence.js";
import { CODEX_STARTUP_SMOKE_TITLE } from "./titles.js";
import {
  loadPackagedLauncher,
  loadPackagedBroker,
  loadPackagedConfig,
  loadPackagedTransport,
  loadPackagedAppServerAuth,
  loadPackagedDynamicTools,
  loadPackagedRegistry,
  type LauncherModule,
  type BrokerModule,
  type ConfigModule,
  type TransportModule,
  type AppServerAuthModule,
  type DynamicToolsModule,
  type RegistryModule,
} from "./packaged-modules.js";

import type {
  CodexLaunchSpec,
  CodexRootHandle,
  LauncherDeps,
  MakeRunnerTrees,
  RemoveRunnerTree,
  SpawnSupervisor,
  SupervisorProcess,
} from "../../agent/src/codex/launcher.js";
import type { RunGrants } from "../../agent/src/codex/broker.js";
import type { CodexTransport } from "../../agent/src/codex/transport.js";

/** Bounded suite deadline for startup + turn (measured ~1.0s on this worker; ample headroom). */
const SUITE_DEADLINE_MS = 90_000;

// ─── the production modules, loaded once before the case (host source tree / image /app/src) ──
let launchCodexRoot: LauncherModule["launchCodexRoot"];
let SUPERVISOR_BIN: LauncherModule["SUPERVISOR_BIN"];
let PROVIDER_CHILD_ARGV: LauncherModule["PROVIDER_CHILD_ARGV"];
let CODEX_BIN: LauncherModule["CODEX_BIN"];
let CodexCallbackBroker: BrokerModule["CodexCallbackBroker"];
let CODEX_M3B_LOOPBACK_PROVIDER_NAME: ConfigModule["CODEX_M3B_LOOPBACK_PROVIDER_NAME"];
let createCodexTransport: TransportModule["createCodexTransport"];
let createCodexAppServerAuth: AppServerAuthModule["createCodexAppServerAuth"];
let buildCodexDynamicTools: DynamicToolsModule["buildCodexDynamicTools"];
let ExecutionRegistry: RegistryModule["ExecutionRegistry"];
let newLocalExecutionEpoch: RegistryModule["newLocalExecutionEpoch"];

before(async () => {
  const launcher = await loadPackagedLauncher();
  launchCodexRoot = launcher.launchCodexRoot;
  SUPERVISOR_BIN = launcher.SUPERVISOR_BIN;
  PROVIDER_CHILD_ARGV = launcher.PROVIDER_CHILD_ARGV;
  CODEX_BIN = launcher.CODEX_BIN;
  CodexCallbackBroker = (await loadPackagedBroker()).CodexCallbackBroker;
  CODEX_M3B_LOOPBACK_PROVIDER_NAME = (await loadPackagedConfig()).CODEX_M3B_LOOPBACK_PROVIDER_NAME;
  createCodexTransport = (await loadPackagedTransport()).createCodexTransport;
  createCodexAppServerAuth = (await loadPackagedAppServerAuth()).createCodexAppServerAuth;
  buildCodexDynamicTools = (await loadPackagedDynamicTools()).buildCodexDynamicTools;
  const registry = await loadPackagedRegistry();
  ExecutionRegistry = registry.ExecutionRegistry;
  newLocalExecutionEpoch = registry.newLocalExecutionEpoch;
});

/**
 * A fake spawnSupervisor that spawns the REAL `codex app-server` directly and synthesizes the
 * Go supervisor's fd3 control / fd4 evidence so launchCodexRoot's started/dispose contract runs.
 * No setpriv, no supervisor binary, no uid switch — the OS posture in `started` is fabricated
 * (P-layer smoke; OS isolation is O-layer). Disposal really SIGKILLs the codex child.
 */
function makeDirectCodexSpawn(codexBin: string, expectUid: number): SpawnSupervisor {
  return (_command, _args, options): SupervisorProcess => {
    const child: ChildProcess = nodeSpawn(codexBin, ["app-server"], {
      cwd: options.cwd,
      env: options.env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    const control = new PassThrough(); // launcher WRITES control frames here (fd3)
    const evidence = new PassThrough(); // launcher READS evidence frames here (fd4)
    const emitter = new EventEmitter();
    let disposing = false;
    let exited = false;

    const writeEvidence = (frame: Record<string, unknown>): void => {
      evidence.write(`${JSON.stringify(frame)}\n`);
    };

    // Emit the synthetic `started` posture the launcher validates. supervisorPid MUST equal the
    // spawned process pid; the OS booleans are fabricated (not proven here).
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
      if (!disposing) {
        // A spontaneous codex exit (crash) before dispose is a launch failure.
        emitter.emit("exit", code, signal);
      }
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
            // The launcher requires a clean supervisor exit (0) after a drained report.
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

/** Plain runner-tree provisioning (no setpriv/chown): the worker owns the trees it creates. */
const fakeMakeRunnerTrees: MakeRunnerTrees = (request) => {
  mkdirSync(request.root, { recursive: true, mode: 0o700 });
  for (const dir of request.dirs) mkdirSync(dir, { recursive: true, mode: 0o700 });
  if (request.sharedSessionRead !== undefined) {
    mkdirSync(request.sharedSessionRead.sessionDir, { recursive: true, mode: 0o700 });
  }
  for (const file of request.files) writeFileSync(file.path, file.content, { mode: file.mode });
};

/** Plain runner-tree removal (no setpriv). */
const fakeRemoveRunnerTree: RemoveRunnerTree = (request) => {
  rmSync(request.root, { recursive: true, force: true });
};

test(CODEX_STARTUP_SMOKE_TITLE, async (t) => {
  const startedAt = Date.now();

  // 1. Resolve the real pinned binary (image-baked on this worker; else a rootless cache install).
  const resolved = resolveCodexBin();
  assert.equal(resolved.codexBin, CODEX_BIN, "the resolved binary is the launcher's pinned path");

  const runnerUid = process.getuid?.() ?? 10001;
  const model = "gpt-6-astra";
  const credential = dummyCredential();
  const bashArg = bashArgCanary();

  // 2. Loopback fake provider on an EPHEMERAL port; scripts ONE allowed uzi_bash call then finish.
  const provider = await FakeProvider.start({ credential, respond: scriptedAllowedBashResponder(bashArg) });

  // Isolated fresh trees (canonical, disjoint root/cwd) so parallel runs never collide.
  const base = realpathSync(mkdtempSync(path.join(tmpdir(), "codex-m4-smoke-")));
  const ownedDataRoot = path.join(base, "root");
  const cwd = path.join(base, "cwd");
  mkdirSync(cwd, { recursive: true, mode: 0o700 });

  // 3. A REAL CodexCallbackBroker with an injected spawnCommand spy + a fileop seam (unused by Bash).
  const spawnCalls: { argv: readonly string[]; cwd?: string }[] = [];
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const grants: RunGrants = {
    role: "coder",
    phase: "implement",
    allowedTools: new Set(["Bash"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
  };
  const broker = new CodexCallbackBroker({
    registry,
    spawnCommand: async (argv, opts) => {
      spawnCalls.push({ argv, cwd: opts.cwd });
      return { code: 0, stdout: `${bashArg}\n`, stderr: "" };
    },
    fileop: { op: async () => ({ ok: true }) },
    worktreePath: cwd,
    grants,
    delegate: async () => ({ ok: false, code: "child_denied", message: "delegation is not exercised by the smoke" }),
    allowedRoles: new Set<string>(),
    screenPolicy: {},
  });

  let handle: CodexRootHandle | undefined;
  let transport: CodexTransport | undefined;
  t.after(async () => {
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
  });

  // 4. Launch the REAL app-server through the PRODUCTION launcher + loopback config builder.
  const spec: CodexLaunchSpec = {
    kind: "provider",
    provider: { name: "openai", baseUrl: provider.baseUrl, envKey: "FAKE_PROVIDER_API_KEY" },
    model,
    codexBin: resolved.codexBin,
    supervisorBin: SUPERVISOR_BIN,
    childArgv: [...PROVIDER_CHILD_ARGV],
    cwd,
    ownedDataRoot,
    useAppServerAuth: true,
    authMode: "api_key",
  };
  const deps: LauncherDeps = {
    env: { UZI_UID_SPLIT: "1" }, // this worker is single-uid; the smoke injects the profile gate input
    resolveRunnerUid: () => runnerUid,
    makeRunnerTrees: fakeMakeRunnerTrees,
    removeRunnerTree: fakeRemoveRunnerTree,
    spawnSupervisor: makeDirectCodexSpawn(resolved.codexBin, runnerUid),
    appServerAuthOpenAIBaseUrlForTest: provider.baseUrl, // → buildCodexLoopbackTestConfigToml
    deadlines: { started: 60_000 }, // the 258 MB pinned binary can be slow to cold-start
  };
  handle = await launchCodexRoot(spec, deps);
  assert.ok(handle.transport.stdin && handle.transport.stdout, "the app-server transport is available");

  // 5. Production transport + production app-server auth handshake (initialize/login).
  transport = createCodexTransport({ inbound: handle.transport.stdout, outbound: handle.transport.stdin });
  const auth = createCodexAppServerAuth({ mode: "api_key", apiKey: credential });
  await auth.authenticate(transport);

  // 6. Consume the notification stream: route item/tool/call to the broker, track turn/completed.
  const brokerCalls: string[] = [];
  let threadId: string | undefined;
  let turnId: string | undefined;
  let turnStatus: string | undefined;
  const notes = transport.notifications();
  const consume = (async (): Promise<void> => {
    for await (const note of notes) {
      if (
        note.kind === "turn_completed"
        && note.threadId === threadId
        && note.turnId === turnId
      ) {
        turnStatus = note.status;
        return;
      }
      if (note.kind === "activity" && note.requestId !== undefined && note.method === "item/tool/call") {
        const p = (note.params ?? {}) as Record<string, unknown>;
        const rt = { threadId: String(p.threadId), turnId: String(p.turnId), callId: String(p.callId) };
        brokerCalls.push(String(p.tool));
        const result = await broker.handleToolCall(rt, p.tool, p.arguments, "root");
        const text = result.ok ? JSON.stringify(result.output) : result.message;
        transport!.respond(note.requestId, { result: { success: result.ok, contentItems: [{ type: "inputText", text }] } });
      }
    }
  })();
  // A background rejection during setup must not become an unhandled rejection.
  void consume.catch(() => undefined);

  // 7. thread/start + turn/start against the loopback provider, advertising the real dynamic tools.
  const threadRes = await transport.request<{ thread?: { id?: string } }>("thread/start", {
    model,
    modelProvider: CODEX_M3B_LOOPBACK_PROVIDER_NAME,
    cwd,
    approvalPolicy: "never",
    approvalsReviewer: "user",
    sandbox: "danger-full-access",
    ephemeral: false,
    environments: [],
    dynamicTools: buildCodexDynamicTools(grants),
    config: { project_doc_max_bytes: 0, projects: { [cwd]: { trust_level: "untrusted" } } },
    developerInstructions: "M4 P smoke developer instructions",
  });
  threadId = threadRes.thread?.id;
  assert.ok(threadId, "thread/start returned a thread id");

  const turnRes = await transport.request<{ turn?: { id?: string } }>("turn/start", {
    threadId,
    input: [{ type: "text", text: "M4 smoke: run the one allowed shell command" }],
    environments: [],
    model,
  });
  turnId = turnRes.turn?.id;
  assert.ok(turnId, "turn/start returned a turn id");

  // 8. Await the bounded turn completion.
  await deadline(consume, "codex turn completion", 60_000);

  const elapsed = Date.now() - startedAt;
  console.log(`[codex-m4 P smoke] source=${resolved.source} startup+turn=${elapsed}ms`);
  t.diagnostic(`codex-m4 P smoke startup+turn=${elapsed}ms (source=${resolved.source})`);

  // 9. Assertions: (a) the tool result was delivered back, (b) the turn completed, (c) the
  //    spawnCommand seam ran for the allowed command (reachability through the REAL broker).
  assert.equal(turnStatus, "completed", "the turn completed successfully");
  assert.deepEqual(brokerCalls, ["uzi_bash"], "exactly one allowed uzi_bash callback reached the broker");
  assert.equal(spawnCalls.length, 1, "the broker's spawnCommand seam ran once (reachability)");
  assert.deepEqual(
    spawnCalls[0]?.argv,
    ["/bin/sh", "-c", `echo ${bashArg}`],
    "the broker spawned the exact allowed argv",
  );
  assert.ok(countToolCallbacks(provider.requests) >= 1, "the app-server fed a tool result back to the provider");
  assert.ok(
    toolCallbackTexts(provider.requests, SMOKE_BASH_CALL_ID).length >= 1,
    "the uzi_bash function_call_output was delivered back to the model",
  );
  assert.deepEqual(provider.errors, [], "the fake provider recorded no errors");
  assert.ok(elapsed < SUITE_DEADLINE_MS, `startup+turn ${elapsed}ms is under the ${SUITE_DEADLINE_MS}ms bound`);

  recordEvidence(CODEX_STARTUP_SMOKE_TITLE, "pass");
});
