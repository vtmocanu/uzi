import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import type { CodexNotification, CodexTransport } from "../../agent/src/codex/transport.js";
import { loadPackagedAppServerAuth, loadPackagedConfig, loadPackagedTransport } from "./packaged-modules.js";

/** Preserve an explicit runtime-default null separately from an absent effort observation. */
export function phaseSettings(notes: readonly CodexNotification[], threadId: string, turnId: string) {
  let duringTurn = false;
  const observations: Array<{ phase: string; model: string; effortPresent: boolean; effort: string | null }> = [];
  for (const note of notes) {
    if (note.kind === "turn_started" && note.threadId === threadId && note.turnId === turnId) duringTurn = true;
    if (note.method !== "thread/settings/updated") continue;
    const params = note.params as { threadId?: string; threadSettings?: Record<string, unknown> };
    if (params.threadId !== threadId || !params.threadSettings) continue;
    const settings = params.threadSettings;
    const effortPresent = Object.hasOwn(settings, "effort");
    observations.push({ phase: duringTurn ? "turn" : "before-turn",
      model: typeof settings.model === "string" ? settings.model : "unreported", effortPresent,
      effort: settings.effort === null ? null : typeof settings.effort === "string" ? settings.effort : "absent-or-invalid" });
  }
  return observations;
}

/** Only scalar assignments emitted by the production builder; unknown syntax refuses launch. */
function configArguments(text: string, provider?: string): string[] {
  let section = "";
  const argv: string[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    if (line.startsWith("[") && line.endsWith("]")) { section = line.slice(1, -1); continue; }
    const field = /^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+)$/.exec(line);
    if (!field) throw new Error("unsupported production config syntax in phase probe");
    const value = !section && field[1] === "model_provider" && provider ? JSON.stringify(provider) : field[2];
    argv.push("-c", `${section ? section + "." : ""}${field[1]}=${value}`);
  }
  return argv;
}

/** Probe/test composition only: two process roles share HOME, never model-selected tools. */
export class PhaseCodex {
  private closing?: Promise<void>;
  private constructor(readonly child: ChildProcess, readonly transport: CodexTransport,
    private readonly notes: AsyncIterator<CodexNotification>, readonly cwd: string, readonly provider?: string) {}
  static async open(bin: string, home: string, cwd: string, credential?: string, provider?: string, model = "gpt-6-astra"): Promise<PhaseCodex> {
    const mods = { ...await loadPackagedTransport(), ...await loadPackagedAppServerAuth() };
    const config = await loadPackagedConfig();
    const overrides = configArguments(config.buildCodexProductionConfigToml({ model, projectPath: cwd,
      authMode: credential === undefined ? "subscription" : "api_key" }), provider);
    const child = spawn(bin, [...overrides, "app-server"], { cwd,
      env: { HOME: home, CODEX_HOME: home, PATH: process.env.PATH }, stdio: ["pipe", "pipe", "pipe"] });
    child.stderr?.resume(); // Never forward provider/account diagnostics to a report.
    const transport = mods.createCodexTransport({ inbound: child.stdout!, outbound: child.stdin! });
    const driver = new PhaseCodex(child, transport, transport.notifications(), cwd, provider);
    try {
      const signal = AbortSignal.timeout(20_000);
      if (credential !== undefined) await mods.createCodexAppServerAuth({ mode: "api_key", apiKey: credential }).authenticate(transport, signal);
      else {
        await transport.request("initialize", { clientInfo: { name: "uzi-phase-probe", version: "1" }, capabilities: { experimentalApi: true } }, { signal });
        transport.notify("initialized");
        const result = await transport.request<{ account?: { type?: string } }>("account/read", { refreshToken: false }, { signal });
        if (result.account?.type !== "chatgpt") throw new Error("probe requires the selected CODEX_HOME ChatGPT login");
      }
      return driver;
    } catch (error) { await driver.close(); throw error; }
  }
  async thread(model: string, threadId?: string): Promise<{ thread: { id: string }; model: string; reasoningEffort: unknown }> {
    return this.transport.request(threadId ? "thread/resume" : "thread/start", {
      model, ...(this.provider ? { modelProvider: this.provider } : {}), cwd: this.cwd,
      approvalPolicy: "never", environments: [], dynamicTools: [],
      config: { project_doc_max_bytes: 0, agents: { enabled: false }, features: { multi_agent_v2: false, goals: false },
        projects: { [this.cwd]: { trust_level: "untrusted" } } },
      developerInstructions: "Bounded phase continuity probe. Do not use tools. Reply with only the requested nonce.",
      ...(threadId ? { threadId, excludeTurns: true } : { ephemeral: false }),
    }, { signal: AbortSignal.timeout(20_000) });
  }
  async turn(threadId: string, model: string | undefined, prompt: string, effort?: string | null): Promise<{ turnId: string; notes: CodexNotification[]; text: string }> {
    const reply = await this.transport.request<{ turn: { id: string } }>("turn/start", {
      threadId, ...(model !== undefined ? { model } : {}), environments: [], input: [{ type: "text", text: prompt }],
      ...(effort !== undefined ? { effort } : {}),
    }, { signal: AbortSignal.timeout(20_000) });
    const notes: CodexNotification[] = [];
    let reasoningBytes = 0, text = "", completedText = "";
    const timer = setTimeout(() => this.child.kill("SIGKILL"), 30_000);
    try {
      for (;;) {
        const next = await this.notes.next();
        if (next.done) throw new Error("phase app-server stopped before turn completion");
        const note = next.value;
        const delta = (note.params as Record<string, unknown> | undefined)?.delta;
        if (typeof delta === "string") {
          if (note.method === "item/agentMessage/delta") text += delta;
          else reasoningBytes += Buffer.byteLength(delta);
        }
        const item = (note.params as { item?: { type?: string; text?: string } } | undefined)?.item;
        if (note.method === "item/completed" && item?.type === "agentMessage" && typeof item.text === "string") completedText += item.text;
        const outputBytes = reasoningBytes + Math.max(Buffer.byteLength(text), Buffer.byteLength(completedText));
        if (outputBytes > 2_048) {
          await this.transport.request("turn/interrupt", { threadId, turnId: reply.turn.id }, { signal: AbortSignal.timeout(5_000) });
          throw new Error("probe exceeded its 2 KiB streamed output cap");
        }
        notes.push(note);
        if (notes.length > 512) throw new Error("phase notification budget exceeded");
        if (note.kind === "activity" && note.requestId !== undefined) throw new Error("probe refuses runtime tool requests");
        if (note.kind === "turn_completed" && note.turnId === reply.turn.id) {
          if (note.status !== "completed") {
            const detail = (note.params as { turn?: { error?: { message?: unknown } } }).turn?.error?.message;
            throw new Error(typeof detail === "string" ? detail : `phase turn failed: ${note.status}`);
          }
          return { turnId: reply.turn.id, notes, text: completedText || text };
        }
      }
    } finally { clearTimeout(timer); }
  }
  close(): Promise<void> { return this.closing ??= this.closeOnce(); }
  private async closeOnce(): Promise<void> {
    const closed = this.child.exitCode === null && this.child.signalCode === null ? once(this.child, "close") : undefined;
    await this.transport.close();
    if (closed) { this.child.kill("SIGKILL"); await closed; }
  }
}
