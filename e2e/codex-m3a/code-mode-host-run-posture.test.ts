// PRD #1533 (D7 layer O) — RUNTIME real-supervisor validation of the run-provider-root code-mode
// execution host, through the ACTUAL packaged launcher (`launchCodexRoot`) + the REAL `config.ts`
// loopback config builder (`buildCodexLoopbackTestConfigToml`, selected via
// `useAppServerAuth` + `appServerAuthOpenAIBaseUrlForTest`).
//
// Unlike control A (`lifecycle.test.ts`), which drives the supervisor DIRECTLY with a hand-rolled
// TEST-ONLY code-mode config, this launches the REAL production launch primitive that M1
// (commit 901bfbda) wired `CodexLaunchSpec.codeModeHost` into, and proves the O-layer posture the
// P layer cannot: the real Go supervisor's true process list + `handle.dispose()` reap authority.
//
//   * codeModeHost:true  (RUN posture)   → a real `codex-code-mode-host` process appears below the
//     supervisor with a SEPARATE pgid (it escapes the app-server's process group) while a worker
//     callback is in flight, and `handle.dispose()` reaps BOTH the app-server AND the host clean
//     (`state:"drained"`, `authority:"ECHILD+__WALL"`).
//   * codeModeHost:false (advice posture)→ NO `codex-code-mode-host` process ever appears below the
//     supervisor, and dispose reaps ONLY the app-server. This is the reviewer-required proof that
//     the advice/stock root launches no code-mode host.
//
// POSTURE / where this runs: the launcher is the real one, so it requires the A1 uid-split PROFILE
// GATE (`UZI_UID_SPLIT=1`, injected via `deps.env`). It then spawns the supervisor through
// `runnerCommand`, which decides setpriv-vs-direct from the GLOBAL `process.env`:
//   * in this single-uid worker (global `UZI_UID_SPLIT` unset, the process already IS the `runner`
//     uid) the supervisor spawns DIRECTLY as the current uid — the posture this file was verified
//     under (measured 2026-09-21: started posture valid, host present with a separate pgid, both
//     reaped ECHILD+__WALL);
//   * inside the built worker image (global `UZI_UID_SPLIT=1`, the process is the `worker` uid) it
//     spawns via setpriv to the distinct `runner` uid — the same posture control A / control B use.
// `resolveRunnerUid` and the base/cwd provisioning branch on the same global flag so both postures
// are exercised faithfully.
//
// Module resolution mirrors production-launcher.test.ts + packaged-modules.ts: the source-tree
// imports are TYPE-ONLY (erased at runtime), and the runtime production code is dynamically imported
// from `<cwd>/src` — `agent/src` (the M1 worktree) when run via `dir: agent`, and `/app/src` (the
// image-baked tree) in the container where run-lifecycle.sh runs from `/app` — with an optional
// `M3A_CODE_MODE_SRC` override. It SKIPS only when the Linux supervisor + pinned codex are absent
// (e.g. a macOS host). NO hook-trust bypass, NO live credentials.

import assert from "node:assert/strict";
import { existsSync, mkdirSync, rmSync } from "node:fs";
import nodePath from "node:path";
import { before, test } from "node:test";
import { pathToFileURL } from "node:url";
import type { Readable, Writable } from "node:stream";

import { message } from "../codex-m0/harness.mjs";
import { FakeProvider, dummyCredential, type ResponseItem, type ResponsesBody } from "./fake-provider.js";
import { asRunnerSync } from "./supervisor-driver.js";

import type { CodexLaunchSpec, CodexRootHandle } from "../../agent/src/codex/launcher.js";
import type { CodexTransport } from "../../agent/src/codex/transport.js";

type LauncherModule = typeof import("../../agent/src/codex/launcher.js");
type ConfigModule = typeof import("../../agent/src/codex/config.js");
type TransportModule = typeof import("../../agent/src/codex/transport.js");
type AppServerAuthModule = typeof import("../../agent/src/codex/appserver-auth.js");
type RunnerUidModule = typeof import("../../agent/src/runner-uid.js");

// Local literals (env-overridable), independent of the async-loaded launcher constants, used for
// the module-eval skip guard AND the spec — they must equal the launcher's own pinned values so
// its trusted-construction contract accepts the spec. Mirrors production-launcher.test.ts.
const SUPERVISOR_BIN = process.env.M3A_SUPERVISOR_BIN ?? "/usr/local/bin/uzi-codex-supervisor";
const CODEX_BIN = process.env.M3A_CODEX_BIN ?? "/opt/uzi-codex/0.153.2/bin/codex";
const PROVIDER_CHILD_ARGV = ["app-server"] as const;
const DATA_BASE = process.env.M3A_DATA_BASE ?? "/data/runner";
const MODEL = "gpt-6-astra";
const EXEC_CALL_ID = "o-cm-exec";

// Skip only where the real supervisor cannot run at all (non-Linux, or the pinned binaries absent).
const SKIP: string | false =
  process.platform === "linux" && existsSync(SUPERVISOR_BIN) && existsSync(CODEX_BIN)
    ? false
    : "O-layer real-supervisor run posture needs the Linux supervisor + pinned codex binaries (run inside the worker image)";

// Runtime production modules, loaded once before the cases (worktree `agent/src` here, image
// `/app/src` in the container). Left undefined when the suite skips.
let launchCodexRoot: LauncherModule["launchCodexRoot"];
let LOOPBACK_PROVIDER_NAME: string;
let createCodexTransport: TransportModule["createCodexTransport"];
let createCodexAppServerAuth: AppServerAuthModule["createCodexAppServerAuth"];
let RUNNER_UID: number;
let uidSplitActive: RunnerUidModule["uidSplitActive"];

before(async () => {
  if (SKIP !== false) return;
  // Match the m4 convention (packaged-modules.ts): default to `<cwd>/src` — `agent/src` (the M1
  // worktree) when run via `dir: agent`, and `/app/src` (the image-baked tree) in the container
  // where run-lifecycle.sh runs from `/app`. `M3A_CODE_MODE_SRC` overrides for a bespoke tree.
  const override = process.env.M3A_CODE_MODE_SRC;
  const base = override && override.trim().length > 0 ? override : nodePath.resolve(process.cwd(), "src");
  const load = async <T>(rel: string): Promise<T> => await import(pathToFileURL(`${base}/${rel}`).href) as T;
  launchCodexRoot = (await load<LauncherModule>("codex/launcher.ts")).launchCodexRoot;
  LOOPBACK_PROVIDER_NAME = (await load<ConfigModule>("codex/config.ts")).CODEX_M3B_LOOPBACK_PROVIDER_NAME;
  createCodexTransport = (await load<TransportModule>("codex/transport.ts")).createCodexTransport;
  createCodexAppServerAuth = (await load<AppServerAuthModule>("codex/appserver-auth.ts")).createCodexAppServerAuth;
  const runnerUid = await load<RunnerUidModule>("runner-uid.ts");
  RUNNER_UID = runnerUid.RUNNER_UID;
  uidSplitActive = runnerUid.uidSplitActive;
});

/** The supervisor runs as `runner` under the split, else directly as the current (single-uid) uid;
 *  expect-uid must match, so derive it from the same GLOBAL flag `runnerCommand` reads. */
function runnerUidForPosture(): number {
  return uidSplitActive() ? RUNNER_UID : (process.getuid?.() ?? RUNNER_UID);
}

/** Create the runner-owned base/cwd. Under the split the launcher creates its 0700 trees as
 *  `runner`, so base/cwd must be runner-owned (setpriv, mirrors production-launcher.test.ts); in
 *  the single-uid worker the process already IS the runner uid, so a plain mkdir owns them. */
function makeBaseDirs(base: string, cwd: string): void {
  if (uidSplitActive()) {
    const mk = asRunnerSync("/bin/sh", ["-c", 'umask 022; mkdir -p "$1" "$2"', "sh", base, cwd]);
    assert.equal(mk.status, 0, `runner base/cwd mkdir failed: ${mk.stderr}`);
  } else {
    mkdirSync(base, { recursive: true, mode: 0o755 });
    mkdirSync(cwd, { recursive: true, mode: 0o700 });
  }
}

function removeBaseDirs(base: string): void {
  if (uidSplitActive()) asRunnerSync("/bin/rm", ["-rf", base]);
  else rmSync(base, { recursive: true, force: true });
}

/** One code-mode `exec` cell (custom_tool_call name="exec"): the ONLY execution surface the
 *  intended model is offered, its script body invoking the nested worker tool. */
function execCell(input: string): ResponseItem {
  return { type: "custom_tool_call", call_id: EXEC_CALL_ID, name: "exec", input } as ResponseItem;
}

/** The scripted responder: the exec cell first, then a finish message once its output returns. */
function execThenFinish(): (body: ResponsesBody) => ResponseItem[] {
  return (body: ResponsesBody): ResponseItem[] => {
    const input = Array.isArray(body.input) ? body.input : [];
    const sawExecOut = input.some((it) => it !== null && typeof it === "object"
      && (it as Record<string, unknown>).type === "custom_tool_call_output"
      && (it as Record<string, unknown>).call_id === EXEC_CALL_ID);
    return sawExecOut
      ? [message("o run posture finished") as ResponseItem]
      : [execCell('text(await tools.uzi_bash({ command: "echo o" }));')];
  };
}

interface Row { readonly pid: number; readonly ppid: number; readonly pgid: number; readonly comm: string }

/** A minimal app-server JSONL RPC client over the launcher transport (fds 0/1/2). It answers ONE
 *  item/tool/call (the nested uzi_bash callback), holding it open via `hold` so the code-mode host
 *  stays alive for a snapshot, and tracks the terminal turn status. */
class RootRpc {
  callbackStarted = false;
  turnStatus: string | undefined;
  private threadId: string | undefined;
  private turnId: string | undefined;

  constructor(
    private readonly transport: CodexTransport,
    private readonly hold: Promise<void>,
  ) {
    void this.pump();
  }

  private async pump(): Promise<void> {
    for await (const note of this.transport.notifications()) {
      if (note.kind === "turn_completed" && note.threadId === this.threadId && note.turnId === this.turnId) {
        this.turnStatus = note.status;
        continue;
      }
      if (note.kind === "activity" && note.requestId !== undefined && note.method === "item/tool/call") {
        this.callbackStarted = true;
        await this.hold; // keep the host alive for the snapshot
        this.transport.respond(note.requestId, {
          result: { success: true, contentItems: [{ type: "inputText", text: '{"code":0,"stdout":"o\\n","stderr":""}' }] },
        });
      }
    }
  }

  async initialize(credential: string): Promise<void> {
    const auth = createCodexAppServerAuth({ mode: "api_key", apiKey: credential });
    await auth.authenticate(this.transport);
  }

  async startThreadAndTurn(cwd: string): Promise<void> {
    const threadRes = await this.transport.request<{ thread?: { id?: string } }>("thread/start", {
      model: MODEL, modelProvider: LOOPBACK_PROVIDER_NAME, cwd,
      approvalPolicy: "never", approvalsReviewer: "user", sandbox: "danger-full-access",
      ephemeral: false, environments: [],
      dynamicTools: [{
        type: "function", name: "uzi_bash", description: "run one worker-screened shell command",
        inputSchema: { type: "object", properties: { command: { type: "string" } }, required: ["command"], additionalProperties: false },
      }],
      config: { project_doc_max_bytes: 0, projects: { [cwd]: { trust_level: "untrusted" } } },
      developerInstructions: "m3a O run-posture",
    });
    this.threadId = threadRes.thread?.id;
    assert.ok(this.threadId, "thread/start returned a thread id");
    const turnRes = await this.transport.request<{ turn?: { id?: string } }>("turn/start", {
      threadId: this.threadId, input: [{ type: "text", text: "run the one allowed shell command" }],
      environments: [], model: MODEL,
    });
    this.turnId = turnRes.turn?.id;
    assert.ok(this.turnId, "turn/start returned a turn id");
  }
}

async function waitFor(predicate: () => boolean, label: string, ms = 60_000): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (predicate()) return;
    await new Promise((r) => setTimeout(r, 40));
  }
  throw new Error(`${label} timed out`);
}

test("production launcher run posture: code_mode_host=true launches a real code-mode host below the supervisor with a separate pgid and reaps it clean", { skip: SKIP }, async (t) => {
  const credential = dummyCredential();
  const provider = await FakeProvider.start({
    credential,
    respond: (body: ResponsesBody): ResponseItem[] => {
      assert.equal(body.model, MODEL, "the intended model reaches the loopback provider");
      return execThenFinish()(body);
    },
  });

  // A worker-writable base under /data/runner (worker-owned 2775): the launcher creates the
  // runner-owned 0700 trees below it. Cleanup removes the whole base in the after hook.
  const realBase = `${DATA_BASE}/m3a-cmh-on-${process.pid}-${Math.random().toString(16).slice(2, 8)}`;
  const ownedDataRoot = `${realBase}/run`;
  const cwd = `${realBase}/cwd`;
  makeBaseDirs(realBase, cwd);

  let holdRelease!: () => void;
  const hold = new Promise<void>((r) => { holdRelease = r; });
  let handle: CodexRootHandle | undefined;
  let transport: CodexTransport | undefined;
  t.after(async () => {
    holdRelease();
    try { await transport?.close(); } catch { /* best effort */ }
    try { if (handle && !handle.failed) await handle.dispose(); } catch { /* asserted in body */ }
    await provider.close();
    removeBaseDirs(realBase);
  });

  const spec: CodexLaunchSpec = {
    kind: "provider",
    provider: { name: "openai", baseUrl: provider.baseUrl, envKey: "FAKE_PROVIDER_API_KEY" },
    model: MODEL, codexBin: CODEX_BIN, supervisorBin: SUPERVISOR_BIN, childArgv: [...PROVIDER_CHILD_ARGV],
    cwd, ownedDataRoot, useAppServerAuth: true, authMode: "api_key", codeModeHost: true,
  };
  handle = await launchCodexRoot(spec, {
    env: { UZI_UID_SPLIT: "1" },
    resolveRunnerUid: runnerUidForPosture,
    appServerAuthOpenAIBaseUrlForTest: provider.baseUrl,
    deadlines: { started: 60_000 },
  });

  // Real hardened supervisor posture.
  assert.equal(handle.started.subreaper, true);
  assert.equal(handle.started.nondumpable, true);
  assert.equal(handle.started.liveCapsZero, true);
  assert.ok(handle.started.capBoundingSet === "0x0" || handle.started.capBoundingSet === "0xc0");
  assert.equal(handle.started.noNewPrivs, true);

  transport = createCodexTransport({ inbound: handle.transport.stdout as Readable, outbound: handle.transport.stdin as Writable });
  const rpc = new RootRpc(transport, hold);
  await rpc.initialize(credential);
  await rpc.startThreadAndTurn(cwd);

  // Hold at the in-flight worker callback so the code-mode host is alive, then snapshot.
  await waitFor(() => rpc.callbackStarted, "nested worker callback in flight");
  const snap = await handle.snapshot(20_000);
  const processes = snap.processes as readonly Row[];
  const app = processes.find((p) => p.pid === handle!.started.childPid);
  const host = processes.find((p) => p.comm.includes("code-mode"));
  t.diagnostic(JSON.stringify({ posture: "on", processes }));
  assert.ok(app, `the real app-server is below the supervisor (${JSON.stringify(processes)})`);
  assert.ok(host, `a real code-mode host is below the supervisor (${JSON.stringify(processes)})`);
  assert.notEqual(host.pgid, app.pgid, "the code-mode host escapes the app-server process group (separate pgid)");

  // Release the callback, let the turn complete, then dispose and prove BOTH are reaped clean.
  holdRelease();
  await waitFor(() => rpc.turnStatus !== undefined, "turn completion");
  assert.equal(rpc.turnStatus, "completed", "the turn completed through the host callback");

  const outcome = await handle.dispose(10_000);
  assert.equal(outcome.clean, true, `run root disposed clean: ${outcome.clean ? "" : outcome.reason}`);
  assert.equal(outcome.event?.state, "drained");
  assert.equal(outcome.event?.authority, "ECHILD+__WALL");
  assert.ok(outcome.event?.reaped?.includes(app.pid), "the app-server was reaped by its own supervisor");
  assert.ok(outcome.event?.reaped?.includes(host.pid), "the code-mode host was reaped by its own supervisor");
  assert.deepEqual(provider.errors, [], "provider errors");
  t.diagnostic(JSON.stringify({ disposal: outcome.event }));
});

test("production launcher advice posture: code_mode_host=false launches NO code-mode host below the supervisor and reaps only the app-server", { skip: SKIP }, async (t) => {
  const credential = dummyCredential();
  const provider = await FakeProvider.start({ credential, respond: execThenFinish() });

  const realBase = `${DATA_BASE}/m3a-cmh-off-${process.pid}-${Math.random().toString(16).slice(2, 8)}`;
  const ownedDataRoot = `${realBase}/run`;
  const cwd = `${realBase}/cwd`;
  makeBaseDirs(realBase, cwd);

  // No callback ever fires when the host is disabled, so nothing needs holding.
  const hold = Promise.resolve();
  let handle: CodexRootHandle | undefined;
  let transport: CodexTransport | undefined;
  t.after(async () => {
    try { await transport?.close(); } catch { /* best effort */ }
    try { if (handle && !handle.failed) await handle.dispose(); } catch { /* asserted in body */ }
    await provider.close();
    removeBaseDirs(realBase);
  });

  const spec: CodexLaunchSpec = {
    kind: "provider",
    provider: { name: "openai", baseUrl: provider.baseUrl, envKey: "FAKE_PROVIDER_API_KEY" },
    model: MODEL, codexBin: CODEX_BIN, supervisorBin: SUPERVISOR_BIN, childArgv: [...PROVIDER_CHILD_ARGV],
    cwd, ownedDataRoot, useAppServerAuth: true, authMode: "api_key", codeModeHost: false,
  };
  handle = await launchCodexRoot(spec, {
    env: { UZI_UID_SPLIT: "1" },
    resolveRunnerUid: runnerUidForPosture,
    appServerAuthOpenAIBaseUrlForTest: provider.baseUrl,
    deadlines: { started: 60_000 },
  });

  transport = createCodexTransport({ inbound: handle.transport.stdout as Readable, outbound: handle.transport.stdin as Writable });
  const rpc = new RootRpc(transport, hold);
  await rpc.initialize(credential);
  await rpc.startThreadAndTurn(cwd);

  // The exec cell fails "code-mode host is disabled" and the turn completes with NO callback.
  await waitFor(() => rpc.turnStatus !== undefined, "turn completion");
  assert.equal(rpc.turnStatus, "completed", "the turn completes cleanly with the host disabled");
  assert.equal(rpc.callbackStarted, false, "no worker callback fired with the host disabled");

  const snap = await handle.snapshot(20_000);
  const processes = snap.processes as readonly Row[];
  const app = processes.find((p) => p.pid === handle!.started.childPid);
  const host = processes.find((p) => p.comm.includes("code-mode"));
  t.diagnostic(JSON.stringify({ posture: "off", processes }));
  assert.ok(app, `the real app-server is below the supervisor (${JSON.stringify(processes)})`);
  assert.equal(host, undefined, `NO code-mode host under the advice-posture stock config (${JSON.stringify(processes)})`);

  const outcome = await handle.dispose(10_000);
  assert.equal(outcome.clean, true, `advice root disposed clean: ${outcome.clean ? "" : outcome.reason}`);
  assert.equal(outcome.event?.state, "drained");
  assert.equal(outcome.event?.authority, "ECHILD+__WALL");
  assert.ok(outcome.event?.reaped?.includes(app.pid), "the app-server was reaped by its own supervisor");
  assert.equal(
    outcome.event?.reaped?.some((pid) => pid !== app.pid),
    false,
    `dispose reaped ONLY the app-server, no code-mode host (${JSON.stringify(outcome.event?.reaped)})`,
  );
  assert.deepEqual(provider.errors, [], "provider errors");
  t.diagnostic(JSON.stringify({ disposal: outcome.event }));
});
