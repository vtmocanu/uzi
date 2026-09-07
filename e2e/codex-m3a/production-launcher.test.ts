// PRD #1156 M3a — Control B: production-config app-server lifecycle (code-mode host
// DISABLED), through the ACTUAL packaged launcher + config builder.
//
// Unlike control A (which drives the supervisor directly with a TEST-ONLY code-mode
// config), this exercises the REAL `launchCodexRoot` + `config.ts` stock hardened config:
// it launches the real Codex app-server below the production supervisor, runs a trivial
// credential-free turn against the localhost fake provider, and asserts the run is
// disposed clean AND that NO `codex-code-mode-host` process ever appears — the shipped
// stock config correctly disables the host. This is the production launcher + config
// builder under test, not a reconstructed equivalent. No hook-trust bypass, no live creds.

import assert from "node:assert/strict";
import { createInterface } from "node:readline";
import { before, test } from "node:test";
import type { Readable, Writable } from "node:stream";

import { message } from "../codex-m0/harness.mjs";
import type { CodexLaunchSpec, CodexRootHandle } from "../../agent/src/codex/launcher.js";
import { FakeProvider, dummyCredential, type ResponseItem, type ResponsesBody } from "./fake-provider.js";
import { loadPackagedLauncher, type LauncherModule } from "./packaged-modules.js";
import { asRunnerSync } from "./supervisor-driver.js";

let launchCodexRoot: LauncherModule["launchCodexRoot"];
before(async () => { ({ launchCodexRoot } = await loadPackagedLauncher()); });

const SUPERVISOR_BIN = process.env.M3A_SUPERVISOR_BIN ?? "/usr/local/bin/uzi-codex-supervisor";
const CODEX_BIN = process.env.M3A_CODEX_BIN ?? "/opt/uzi-codex/0.153.2/bin/codex";
// A worker-writable base: /data/runner is worker-owned 2775 (entrypoint), so the worker
// can mkdtemp directly under it; the launcher creates the runner-owned 0700 trees below.
const DATA_BASE = process.env.M3A_DATA_BASE ?? "/data/runner";
const PROVIDER_NAME = "m3aprovB";
const ENV_KEY = "FAKE_PROVIDER_API_KEY";

/** A minimal app-server JSONL RPC client over the launcher's transport (fds 0/1/2). It
 *  speaks ONLY the client half the production run would; it never writes config or a
 *  hook-trust bypass. */
class AppServerRpc {
  readonly messages: Record<string, unknown>[] = [];
  private nextId = 1;
  private readonly pending = new Map<number, { resolve: (v: unknown) => void; reject: (e: Error) => void }>();
  constructor(private readonly stdin: Writable, stdout: Readable) {
    createInterface({ input: stdout }).on("line", (line) => this.onLine(line));
  }
  private onLine(line: string): void {
    let v: Record<string, unknown>;
    try { v = JSON.parse(line) as Record<string, unknown>; } catch { return; }
    this.messages.push(v);
    if (typeof v.method === "string" && v.id !== undefined) {
      // No dynamic tools / no approvals expected under the stock config; be defensive.
      if (v.method.endsWith("/requestApproval")) this.send({ id: v.id, result: { decision: "decline" } });
      else this.send({ id: v.id, error: { code: -32601, message: "unsupported m3a client method" } });
      return;
    }
    if (v.id !== undefined) {
      const p = this.pending.get(v.id as number);
      if (!p) return;
      this.pending.delete(v.id as number);
      if (v.error) p.reject(new Error(JSON.stringify(v.error)));
      else p.resolve(v.result);
    }
  }
  private send(v: Record<string, unknown>): void {
    if (!this.stdin.destroyed && !this.stdin.writableEnded) this.stdin.write(`${JSON.stringify(v)}\n`);
  }
  private rpc(method: string, params: Record<string, unknown>): Promise<Record<string, unknown>> {
    const id = this.nextId++;
    return new Promise<Record<string, unknown>>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`${method} timed out`)), 20_000);
      this.pending.set(id, { resolve: (r) => { clearTimeout(timer); resolve(r as Record<string, unknown>); }, reject: (e) => { clearTimeout(timer); reject(e); } });
      this.send({ id, method, params });
    });
  }
  async initialize(): Promise<void> {
    await this.rpc("initialize", { clientInfo: { name: "uzi-m3a-B", version: "0.0.0" }, capabilities: { experimentalApi: true } });
    this.send({ method: "initialized" });
  }
  async threadStart(model: string): Promise<string> {
    const r = await this.rpc("thread/start", {
      model, modelProvider: PROVIDER_NAME, approvalPolicy: "never", approvalsReviewer: "user",
      sandbox: "danger-full-access", ephemeral: true, environments: [],
    });
    return (r.thread as { id: string }).id;
  }
  async turn(threadId: string): Promise<Record<string, unknown>> {
    const r = await this.rpc("turn/start", { threadId, input: [{ type: "text", text: "m3a stock turn" }] });
    const turnId = (r.turn as { id: string }).id;
    // Wait for the turn/completed notification.
    const end = Date.now() + 20_000;
    while (Date.now() < end) {
      const done = this.messages.find((m) => m.method === "turn/completed"
        && (m.params as { threadId?: string })?.threadId === threadId
        && ((m.params as { turn?: { id?: string } })?.turn)?.id === turnId);
      if (done) return (done.params as { turn: Record<string, unknown> }).turn;
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    throw new Error("turn/completed timed out");
  }
}

test("production launcher: real app-server runs a stock credential-free turn, no code-mode host, disposed clean", async (t) => {
  const model = "gpt-6-astra";
  const credential = dummyCredential();
  const base = `${DATA_BASE}/m3a-B-${process.pid}-${Math.random().toString(16).slice(2, 8)}`;
  const ownedDataRoot = `${base}/run`;
  const cwd = `${base}/cwd`;
  // base + cwd are created RUNNER-owned 0755 (like control A): the worker (a runner-group
  // member) can traverse them to spawn the supervisor, and the runner owns them so the
  // launcher can create the 0700 owned trees under base/run. Cleanup is a runner-uid rm.
  const mk = asRunnerSync("/bin/sh", ["-c", 'umask 022; mkdir -p "$1" "$2"', "sh", base, cwd]);
  assert.equal(mk.status, 0, `base/cwd mkdir: ${mk.stderr}`);

  const provider = await FakeProvider.start({
    credential,
    respond: (body: ResponsesBody): ResponseItem[] => {
      assert.equal(body.model, model, "stock turn reaches the fake provider with the intended model");
      return [message() as ResponseItem];
    },
  });

  let handle: CodexRootHandle | undefined;
  t.after(async () => {
    try { if (handle && !handle.failed) await handle.dispose(); } catch { /* asserted in-body */ }
    await provider.close();
    asRunnerSync("/bin/rm", ["-rf", base]);
  });

  const spec: CodexLaunchSpec = {
    ownedDataRoot,
    provider: { name: PROVIDER_NAME, baseUrl: provider.baseUrl, envKey: ENV_KEY, credentialValue: credential },
    model, codexBin: CODEX_BIN, supervisorBin: SUPERVISOR_BIN, kind: "provider", childArgv: ["app-server"], cwd,
  };
  handle = await launchCodexRoot(spec);

  // The production supervisor posture must be the hardened one.
  assert.equal(handle.started.subreaper, true);
  assert.equal(handle.started.nondumpable, true);
  assert.equal(handle.started.liveCapsZero, true);
  assert.ok(handle.started.capBoundingSet === "0x0" || handle.started.capBoundingSet === "0xc0");
  assert.equal(handle.started.noNewPrivs, true);
  assert.equal(handle.started.uid, Number(process.env.M3A_RUNNER_UID ?? "10002"));

  const rpc = new AppServerRpc(handle.transport.stdin!, handle.transport.stdout!);
  await rpc.initialize();
  const threadId = await rpc.threadStart(model);
  const turn = await rpc.turn(threadId);
  assert.equal(turn.status, "completed", "stock credential-free turn completes");

  // The stock config disables code_mode_host: NO code-mode host process appears below the
  // supervisor, before OR after the turn. (Control A proves it DOES appear when enabled.)
  const snap = await handle.snapshot();
  const host = snap.processes.find((p) => p.comm.includes("code-mode"));
  assert.equal(host, undefined, `no code-mode host under the stock launcher (${JSON.stringify(snap.processes)})`);
  assert.ok(snap.processes.some((p) => p.pid === handle!.started.childPid), "the real app-server IS below the supervisor");

  const outcome = await handle.dispose();
  assert.equal(outcome.clean, true, `stock root disposed clean: ${outcome.clean ? "" : outcome.reason}`);
  assert.equal(outcome.event?.state, "drained");
  assert.equal(outcome.event?.authority, "ECHILD+__WALL");
  assert.deepEqual(provider.errors, [], "provider errors");
  t.diagnostic(JSON.stringify({ processes: snap.processes, disposal: outcome.event }));
});
