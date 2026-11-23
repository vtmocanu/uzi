import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { query, type SDKUserMessage, type Options } from "@anthropic-ai/claude-agent-sdk";
import { buildSdkOptions } from "../src/claude-harness.js";

export const CLAUDE_PHASE_SDK = "0.3.325";
export const CLAUDE_PHASE_CLI = "2.1.325";

/** Test/probe driver only. Nothing from the contributor's HOME or repository is loaded. */
export class PhaseClaude {
  readonly root = fs.mkdtempSync(path.join(os.tmpdir(), "phase-claude-"));
  readonly home = path.join(this.root, "home");
  readonly cwd = path.join(this.root, "cwd");
  readonly plugin = path.join(this.root, "plugin");
  readonly events: string[] = [];
  readonly timeline: Array<{ event: string; atMs: number }> = [];
  private readonly started = performance.now();
  private readonly children = new Set<ChildProcess>();
  constructor(readonly providerEnv: Record<string, string>) {
    for (const dir of [this.home, this.cwd, this.plugin]) fs.mkdirSync(dir);
    fs.mkdirSync(path.join(this.plugin, ".claude-plugin"));
    fs.writeFileSync(path.join(this.plugin, ".claude-plugin", "plugin.json"), JSON.stringify({ name: "phase-proof", version: "1.0.0" }));
  }
  mark(event: string): void {
    this.events.push(event);
    this.timeline.push({ event, atMs: performance.now() - this.started });
  }
  async turn(input: {
    prompt: string | AsyncIterable<SDKUserMessage>; model?: string; effort?: Options["effort"];
    resume?: string; onInit?: () => void; onReady?: (settings: unknown) => void; abortOnInit?: boolean;
  }): Promise<{ session: string; model: string; cli: string; effort: unknown; usage: unknown; text: string; aborted: boolean; pid: number; thinkingBlocks: number; failure: string | undefined }> {
    const installed = JSON.parse(fs.readFileSync(new URL("../node_modules/@anthropic-ai/claude-agent-sdk/package.json", import.meta.url), "utf8"));
    if (installed.version !== CLAUDE_PHASE_SDK) throw new Error("phase proof SDK pin mismatch");
    const abort = new AbortController();
    const timer = setTimeout(() => abort.abort(), 40_000);
    const options = buildSdkOptions({
      cwd: this.cwd, env: { PATH: process.env.PATH ?? "", HOME: this.home, TMPDIR: this.root,
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1", DISABLE_AUTOUPDATER: "1",
        CLAUDE_CODE_MAX_OUTPUT_TOKENS: "256", ...this.providerEnv },
      skillsPluginPath: this.plugin, skills: [], agents: {}, mcpServers: {},
      systemPrompt: "This is a bounded conversation continuity probe. Use no tools. Reply briefly.",
      preToolUse: [{ hooks: [async () => ({ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: "probe has no tool authority" } })] }],
      ...(input.model !== undefined ? { model: input.model } : {}),
      ...(input.effort !== undefined ? { effort: input.effort } : {}),
    });
    options.tools = [];
    options.maxTurns = 1;
    options.abortController = abort;
    if (input.resume !== undefined) options.resume = input.resume;
    let pid = 0;
    options.spawnClaudeCodeProcess = args => {
      // SDK process defaults must not smuggle a contributor credential into the offline fake.
      const env = { ...options.env, CLAUDE_CODE_ENTRYPOINT: args.env.CLAUDE_CODE_ENTRYPOINT ?? "sdk-ts", IS_SANDBOX: args.env.IS_SANDBOX };
      const child = spawn(args.command, args.args, { cwd: args.cwd, env, stdio: ["pipe", "pipe", "pipe"] });
      pid = child.pid ?? 0;
      this.children.add(child);
      child.once("close", () => this.children.delete(child));
      return child;
    };
    const result = { session: "", model: "", cli: "", effort: undefined as unknown, usage: undefined as unknown, text: "", aborted: false, pid: 0, thinkingBlocks: 0, failure: undefined as string | undefined };
    const q = query({ prompt: input.prompt, options });
    // The production harness also feature-detects this pinned control method: it is not in Query's public type.
    const control = q as unknown as { getSettings?: () => Promise<{ applied?: { effort?: unknown } }> };
    const settings = () => typeof control.getSettings === "function"
      ? control.getSettings.call(q) : Promise.reject(new Error("get_settings unsupported"));
    const ready = input.onReady ? settings().then(applied => input.onReady!(applied)).catch(() => undefined) : undefined;
    try {
      for await (const frame of q) {
        if (frame.type === "assistant") result.thinkingBlocks += frame.message.content.filter(b => b.type === "thinking" || b.type === "redacted_thinking").length;
        if (frame.type === "system" && frame.subtype === "init") {
          this.mark("init");
          result.session = frame.session_id;
          result.model = frame.model;
          result.cli = frame.claude_code_version;
          if (result.cli !== CLAUDE_PHASE_CLI) throw new Error("phase proof bundled CLI pin mismatch");
          input.onInit?.();
          if (input.abortOnInit) { result.aborted = true; abort.abort(); continue; }
          try { result.effort = (await settings()).applied?.effort; } catch { /* Unknown is evidence, not an assumed default. */ }
        }
        if (frame.type === "result") {
          result.usage = frame.modelUsage;
          if (frame.subtype === "success") result.text = frame.result;
          else result.failure = `phase query ${frame.subtype}: ${frame.errors.join("; ")}`;
        }
      }
      return { ...result, pid };
    } catch (error) {
      if (!result.aborted) throw error;
      return { ...result, pid };
    } finally {
      clearTimeout(timer);
      abort.abort();
      q.close();
      await ready;
      await this.stopChildren();
    }
  }
  private async stopChildren(): Promise<void> {
    await Promise.all([...this.children].map(async child => {
      if (child.exitCode !== null || child.signalCode !== null) return;
      const closed = once(child, "close");
      child.kill("SIGKILL");
      await closed;
    }));
  }
  async close(): Promise<void> {
    await this.stopChildren();
    fs.rmSync(this.root, { recursive: true, force: true });
  }
}
