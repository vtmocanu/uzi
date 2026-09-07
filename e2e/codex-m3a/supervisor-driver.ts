// PRD #1156 M3a — a driver for the PRODUCTION Go supervisor + real Codex app-server,
// used by the real-code-mode-host lifecycle suite (control A).
//
// This deliberately does NOT go through `launchCodexRoot`/`config.ts` (that ships the
// HARDENED stock config with `code_mode_host=false`, which correctly does not launch a
// host). Instead it drives the SAME production supervisor binary the launcher drives —
// `uzi-codex-supervisor --expect-uid <N> -- <codex> app-server` spawned as the runner
// uid — with a TEST-ONLY CODEX_HOME whose config ENABLES the code-mode host, so the real
// `codex-code-mode-host` process actually launches below the supervisor. It speaks the
// app-server JSONL RPC over the supervisor's stdio 0/1/2, the supervisor control frames
// over fd 3, and reads supervisor evidence over fd 4 — exactly the wire contract in
// `agent/codex/supervisor/doc.go`. NO `--dangerously-bypass-hook-trust`, NO per-thread
// `config:{bypass_hook_trust:true}`, NO live credentials.
//
// It reproduces the BEHAVIOUR of the frozen `e2e/codex-m0/supervisor.test.mjs`
// `SupervisorProbe` WITHOUT importing it (that fixture writes `bypass_hook_trust`); only
// the frozen PURE protocol helpers `poll`/`deadline` are reused.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { EventEmitter } from "node:events";

import { deadline, poll } from "../codex-m0/harness.mjs";

const RPC_DEADLINE_MS = 20_000;
const EVIDENCE_DEADLINE_MS = 8_000;

/** The setpriv argv prefix mirroring the production `runner-uid.ts` `setprivRunnerArgs()`
 *  (reuid/regid runner, init-groups, clear inheritable + ambient caps). Reproduced here
 *  rather than imported so the driver stays a self-contained test artifact. */
function setprivRunnerArgs(): string[] {
  return ["--reuid", "runner", "--regid", "runner", "--init-groups",
    "--bounding-set", "-all", "--inh-caps", "-all", "--ambient-caps", "-all", "--"];
}
const SETPRIV = "/bin/setpriv";

/** Run a command AS the runner uid (setpriv), synchronously. */
export function asRunnerSync(command: string, args: string[], input?: string): { status: number | null; stdout: string; stderr: string } {
  const r = spawnSync(SETPRIV, [...setprivRunnerArgs(), command, ...args], { input, encoding: "utf8" });
  return { status: r.status, stdout: String(r.stdout ?? ""), stderr: String(r.stderr ?? "") };
}

// ── Production supervisor evidence shapes (agent/codex/supervisor/evidence.go) ──────
export interface StartedEvidence {
  readonly event: "started";
  readonly supervisorPid: number;
  readonly childPid: number;
  readonly subreaper: boolean;
  readonly dumpable: boolean;
  readonly uid: number;
  readonly capsZero: boolean;
  readonly noNewPrivs: boolean;
}
export interface ProcRow { readonly pid: number; readonly ppid: number; readonly pgid: number; readonly comm: string }
export interface SnapshotEvidence { readonly event: "snapshot"; readonly id: number; readonly processes: ProcRow[] }
export interface DisposeEvidence {
  readonly event: "dispose";
  readonly id: number;
  readonly state: "drained" | "unconfirmed";
  readonly authority?: string;
  readonly reason?: string;
  readonly killed?: number[];
  readonly reaped?: number[];
  readonly children?: number[];
}

/** A dynamic-tool (item/tool/call) request the app-server issues over stdio RPC. */
export interface DynamicRequest {
  readonly id: number;
  readonly method: string;
  readonly params: { readonly tool: string; readonly threadId: string; readonly turnId: string; readonly namespace: unknown; readonly callId?: string; readonly arguments?: Record<string, unknown> };
}
/** The reply body sent back for an item/tool/call (the frozen dynamic-tool contract). */
export type DynamicReply = { result: { success: boolean; contentItems: { type: string; text: string }[] } };

export interface SupervisorRootOptions {
  readonly supervisorBin: string;
  readonly codexBin: string;
  readonly childArgv: readonly string[];
  readonly ownedDataRoot: string;
  readonly cwd: string;
  readonly expectUid: number;
  /** The TEST-ONLY config.toml text (written 0600 into the fresh CODEX_HOME). */
  readonly configToml: string;
  /** The fully-replaced env (HOME/CODEX_HOME/XDG/TMPDIR/PATH + provider credential). */
  readonly env: Record<string, string>;
  /** Answers an item/tool/call callback. */
  readonly dynamicTool?: (request: DynamicRequest, root: SupervisorRoot) => Promise<DynamicReply>;
}

interface Pending { resolve: (value: unknown) => void; reject: (error: Error) => void }

/** The seven fresh per-launch trees, created runner-owned 0700 (owner-only). */
export const OWNED_TREE_NAMES = ["home", "codex", "xdg-config", "xdg-cache", "xdg-data", "xdg-state", "tmp"] as const;

export class SupervisorRoot {
  readonly supervisorMessages: (StartedEvidence | SnapshotEvidence | DisposeEvidence | Record<string, unknown>)[] = [];
  readonly messages: Record<string, unknown>[] = [];
  readonly dynamicCalls: DynamicRequest[] = [];
  readonly errors: string[] = [];
  started!: StartedEvidence;
  private child!: ChildProcess;
  private readonly pending = new Map<number, Pending>();
  private readonly events = new EventEmitter();
  private nextId = 1;
  private exited = false;
  private drained = false;
  private closing = false;
  private stderrBuf = "";

  private constructor(private readonly options: SupervisorRootOptions) {}

  static async start(options: SupervisorRootOptions): Promise<SupervisorRoot> {
    const root = new SupervisorRoot(options);
    await root.provisionTrees();
    await root.spawnSupervisor();
    return root;
  }

  /** Create the runner-owned trees (0700) + cwd (worker-traversable) and write config. */
  private async provisionTrees(): Promise<void> {
    // ROOT + cwd are worker-traversable so the worker can chdir there to spawn the
    // supervisor; the owned trees are 0700 (only the runner-uid child reaches them).
    const mk1 = asRunnerSync("/bin/sh", ["-c", 'umask 022; mkdir -p "$1" "$2"', "sh", this.options.ownedDataRoot, this.options.cwd]);
    if (mk1.status !== 0) throw new Error(`runner mkdir ROOT/cwd failed (${String(mk1.status)}): ${mk1.stderr}`);
    const trees = OWNED_TREE_NAMES.map((name) => `${this.options.ownedDataRoot}/${name}`);
    const mk2 = asRunnerSync("/bin/sh", ["-c", 'umask 077; mkdir -p "$@"', "sh", ...trees]);
    if (mk2.status !== 0) throw new Error(`runner mkdir trees failed (${String(mk2.status)}): ${mk2.stderr}`);
    const w = asRunnerSync("/bin/sh", ["-c", 'umask 077; cat > "$1"; chmod 600 "$1"', "sh", `${this.options.ownedDataRoot}/codex/config.toml`], this.options.configToml);
    if (w.status !== 0) throw new Error(`runner write config failed (${String(w.status)}): ${w.stderr}`);
  }

  private async spawnSupervisor(): Promise<void> {
    const argv = [...setprivRunnerArgs(), this.options.supervisorBin,
      "--expect-uid", String(this.options.expectUid), "--", this.options.codexBin, ...this.options.childArgv];
    this.child = spawn(SETPRIV, argv, {
      cwd: this.options.cwd,
      env: this.options.env,
      stdio: ["pipe", "pipe", "pipe", "pipe", "pipe"],
    });
    this.child.once("exit", () => {
      this.exited = true;
      for (const p of this.pending.values()) p.reject(new Error("supervisor/app-server exited"));
      this.pending.clear();
      this.events.emit("change");
    });
    this.child.stderr?.on("data", (chunk: Buffer) => { this.stderrBuf = (this.stderrBuf + chunk.toString()).slice(-MAX_STDERR); });

    // fd 4 — supervisor evidence.
    createInterface({ input: this.child.stdio[4] as NodeJS.ReadableStream }).on("line", (line) => {
      try { this.supervisorMessages.push(JSON.parse(line)); } catch (error) { this.errors.push(String(error)); }
      this.events.emit("change");
    });
    // fds 0/1/2 — app-server transport (stdout is JSONL RPC).
    createInterface({ input: this.child.stdout as NodeJS.ReadableStream }).on("line", (line) => this.onRpcLine(line));

    const started = await poll(
      () => this.supervisorMessages.find((m) => (m as Record<string, unknown>).event === "started") as StartedEvidence | undefined,
      "supervisor started evidence", EVIDENCE_DEADLINE_MS,
    );
    // Re-validate EVERY posture boolean the launcher validates (symmetric independent check).
    if (started.subreaper !== true || started.dumpable !== true || started.capsZero !== true
      || started.noNewPrivs !== true || started.uid !== this.options.expectUid) {
      throw new Error(`unsafe start posture: ${JSON.stringify(started)}`);
    }
    this.started = started;
  }

  private onRpcLine(line: string): void {
    let value: Record<string, unknown>;
    try { value = JSON.parse(line) as Record<string, unknown>; } catch { return; }
    this.messages.push(value);
    if (typeof value.method === "string" && value.id !== undefined) {
      if (value.method === "item/tool/call" && this.options.dynamicTool) {
        const request = value as unknown as DynamicRequest;
        this.dynamicCalls.push(request);
        this.events.emit("change");
        void this.answerDynamic(request);
        return;
      }
      if (!value.method.endsWith("/requestApproval")) {
        this.send({ id: value.id, error: { code: -32601, message: "unsupported m3a client method" } });
        return;
      }
      // approvalPolicy:"never" means the app-server should not ask; a stray ask is declined.
      this.send({ id: value.id, result: { decision: "decline" } });
      return;
    }
    if (value.id !== undefined) {
      const p = this.pending.get(value.id as number);
      if (!p) return;
      this.pending.delete(value.id as number);
      if (value.error) p.reject(new Error(JSON.stringify(value.error)));
      else p.resolve(value.result);
    }
    this.events.emit("change");
  }

  private async answerDynamic(request: DynamicRequest): Promise<void> {
    try {
      const reply = await deadline(this.options.dynamicTool!(request, this), "dynamic callback", RPC_DEADLINE_MS);
      this.send({ id: request.id, ...reply });
    } catch (error) {
      this.errors.push(error instanceof Error ? error.message : String(error));
    }
  }

  private send(value: Record<string, unknown>): void {
    const stdin = this.child.stdin;
    // A late RPC reply during teardown must not write to an already-closed transport.
    if (!stdin || stdin.destroyed || stdin.writableEnded) return;
    stdin.write(`${JSON.stringify(value)}\n`);
  }

  private async rpc(method: string, params: Record<string, unknown>): Promise<Record<string, unknown>> {
    const id = this.nextId++;
    const response = new Promise<unknown>((resolve, reject) => this.pending.set(id, { resolve, reject }));
    this.send({ id, method, params });
    return await deadline(response, method, RPC_DEADLINE_MS) as Record<string, unknown>;
  }

  /** Wait until `predicate` is truthy (change-driven, bounded). */
  async until<T>(predicate: () => T, label: string, milliseconds = RPC_DEADLINE_MS): Promise<NonNullable<T>> {
    let listener!: () => void;
    try {
      return await deadline(new Promise<NonNullable<T>>((resolve, reject) => {
        listener = () => {
          try {
            const value = predicate();
            if (value) resolve(value as NonNullable<T>);
            else if (this.exited) reject(new Error(`${label}: supervisor exited; ${this.stderrBuf}`));
          } catch (error) { reject(error instanceof Error ? error : new Error(String(error))); }
        };
        this.events.on("change", listener);
        listener();
      }), label, milliseconds);
    } finally { this.events.off("change", listener); }
  }

  async initialize(): Promise<void> {
    await this.rpc("initialize", { clientInfo: { name: "uzi-m3a", version: "0.0.0" }, capabilities: { experimentalApi: true } });
    this.send({ method: "initialized" });
  }

  async threadStart(params: { model: string; environments: unknown[]; dynamicTools?: unknown[]; approvalPolicy?: string }): Promise<string> {
    const result = await this.rpc("thread/start", {
      model: params.model,
      modelProvider: PROVIDER_NAME,
      cwd: this.options.cwd,
      approvalPolicy: params.approvalPolicy ?? "never",
      approvalsReviewer: "user",
      sandbox: "danger-full-access",
      ephemeral: true,
      // NO config:{bypass_hook_trust:true} — the code-mode host + dynamic-callback path
      // needs no hook-trust bypass (proven by the m3a spike).
      environments: params.environments,
      ...(params.dynamicTools ? { dynamicTools: params.dynamicTools } : {}),
    });
    return (result.thread as { id: string }).id;
  }

  async turnStart(threadId: string, text = "m3a deterministic fixture"): Promise<string> {
    const result = await this.rpc("turn/start", { threadId, input: [{ type: "text", text }] });
    return (result.turn as { id: string }).id;
  }

  /** Interrupt an in-flight turn (the revoke/interrupt/dispose lifecycle control). */
  async turnInterrupt(threadId: string, turnId: string): Promise<void> {
    await this.rpc("turn/interrupt", { threadId, turnId });
  }

  /** A supervisor control frame (fd 3) whose evidence (fd 4) is awaited by id. */
  private async supervisorCommand(op: "snapshot" | "dispose", extra: Record<string, unknown> = {}): Promise<SnapshotEvidence | DisposeEvidence> {
    if (this.exited) throw new Error(`supervisor exited; ${op} unavailable`);
    const id = this.nextId++;
    const control = this.child.stdio[3] as (NodeJS.WritableStream & { destroyed?: boolean }) | null;
    if (!control || control.destroyed) throw new Error(`supervisor control channel unavailable; ${op} unconfirmed`);
    control.write(`${JSON.stringify({ op, id, ...extra })}\n`);
    return await this.until(
      () => this.supervisorMessages.find((m) => (m as Record<string, unknown>).event === op && (m as Record<string, unknown>).id === id) as SnapshotEvidence | DisposeEvidence | undefined,
      `supervisor ${op}`, EVIDENCE_DEADLINE_MS,
    );
  }

  async snapshot(): Promise<SnapshotEvidence> {
    return await this.supervisorCommand("snapshot") as SnapshotEvidence;
  }

  async dispose(timeoutMs = 2000): Promise<DisposeEvidence> {
    const result = await this.supervisorCommand("dispose", { timeoutMs }) as DisposeEvidence;
    if (result.state === "drained") {
      const exit = await deadline(new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) => {
        if (this.exited) { resolve({ code: this.child.exitCode, signal: this.child.signalCode }); return; }
        this.child.once("exit", (code, signal) => resolve({ code, signal }));
      }), "supervisor exit", EVIDENCE_DEADLINE_MS);
      if (exit.code !== 0) throw new Error(`drained but supervisor exited non-zero: code=${String(exit.code)} signal=${String(exit.signal)}`);
      this.drained = true;
    }
    return result;
  }

  async close(): Promise<void> {
    if (this.closing) return;
    this.closing = true;
    try {
      if (this.child && !this.exited && !this.drained) {
        (this.child.stdio[3] as NodeJS.WritableStream).end?.();
        try { await this.dispose(); } catch { /* best effort */ }
      }
    } finally {
      if (this.child && !this.exited) { try { this.child.kill("SIGKILL"); } catch { /* ignore */ } }
      asRunnerSync("/bin/rm", ["-rf", this.options.ownedDataRoot]);
    }
  }
}

const MAX_STDERR = 256 * 1024;
/** The single provider table key both the config and thread/start reference. */
export const PROVIDER_NAME = "m3aprov";

/**
 * TEST-ONLY code-mode-host config.toml (drives the supervisor DIRECTLY; NEVER
 * `config.ts`). It is the frozen M0 code-mode config MINUS the fixture's hook bypass:
 * `hooks = false`, no `bypass_hook_trust`. Native authority is off (agents disabled,
 * multi_agent off, environments enforced per-thread), the canonical project is
 * untrusted with `project_doc_max_bytes = 0`, and the code-mode host is ENABLED.
 */
export function codeModeHostConfigToml(opts: { model: string; baseUrl: string; envKey: string; projectPath: string }): string {
  const q = (value: string): string => JSON.stringify(value);
  return [
    `model = ${q(opts.model)}`,
    `model_provider = ${q(PROVIDER_NAME)}`,
    `project_doc_max_bytes = 0`,
    `check_for_update_on_startup = false`,
    `web_search = "disabled"`,
    ``,
    `[analytics]`,
    `enabled = false`,
    ``,
    `[feedback]`,
    `enabled = false`,
    ``,
    `[agents]`,
    `enabled = false`,
    ``,
    `[features]`,
    `apps = false`,
    `plugins = false`,
    `shell_snapshot = false`,
    `shell_snapshot_v2 = false`,
    `code_mode = true`,
    `code_mode_only = true`,
    `code_mode_host = true`,
    `code_mode_prewarm = false`,
    `remote_models = false`,
    `unified_exec = false`,
    `hooks = false`,
    `multi_agent = false`,
    `multi_agent_v2 = false`,
    `enable_request_compression = false`,
    ``,
    `[projects.${q(opts.projectPath)}]`,
    `trust_level = "untrusted"`,
    ``,
    `[model_providers.${PROVIDER_NAME}]`,
    `name = ${q(PROVIDER_NAME)}`,
    `base_url = ${q(opts.baseUrl)}`,
    `env_key = ${q(opts.envKey)}`,
    `wire_api = "responses"`,
    `supports_websockets = false`,
    `requires_openai_auth = false`,
    `request_max_retries = 0`,
    `stream_max_retries = 0`,
    `stream_idle_timeout_ms = 15000`,
    ``,
  ].join("\n");
}
