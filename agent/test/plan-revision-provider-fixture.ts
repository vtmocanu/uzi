// Fake only the provider boundary: both planning drivers and RunRunner stay real.
import { PassThrough } from "node:stream";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { CodexExecutor, type CodexExecutorDeps } from "../src/codex/codex-executor.js";
import { selectCodexBinding } from "../src/codex/select.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import { client, homeDir } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

export type Frame = { plan: string } | { ask: true } | { failure: true };
export interface Start { prompt: string; resume?: string; threadId?: string }
export function providerFixture(harness: "claude" | "codex", script: (n: number) => Frame, sessionless = false) {
  const starts: Start[] = [];
  const credential = { auth_mode: "api_key" as const, access_token: "fixture-key", capability: "fixture-cap" };
  const frame = () => script(starts.length);
  if (harness === "claude") {
    const queryFn: SdkQueryFn = params => (async function* () {
      let prompt = "";
      for await (const p of params.prompt) prompt += JSON.stringify((p as { message: { content: unknown } }).message.content);
      starts.push({ prompt, resume: params.options.resume });
      const f = frame();
      const session_id = sessionless ? undefined : `session-${starts.length}`;
      if ("failure" in f) {
        yield { type: "result", subtype: "success", is_error: true, api_error_status: 429,
          terminal_reason: "api_error", result: "API Error: 429", session_id } as SDKMessage;
      } else {
        yield { type: "assistant", session_id, message: { content: [{ type: "tool_use", id: `tool-${starts.length}`,
          name: "ask" in f ? "mcp__uzi__ask_user" : "mcp__uzi__submit_plan",
          input: "ask" in f ? { questions: [{ question: "Which target?", header: "Target" }] } : { plan_md: f.plan } }] } } as unknown as SDKMessage;
        yield { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id } as SDKMessage;
      }
    })();
    return { starts, credential, executor: new SdkExecutor(nullLogger(), homeDir, {
      queryFn, emptyTurnBackoffBaseMs: 1, emptyTurnMaxRetries: 1,
      installDeps: async () => ({ results: [], truncated: false }),
    }) };
  }
  const transport = new RevisionTransport(starts, frame);
  const selected = selectCodexBinding({ codex: credential });
  if (selected.kind !== "codex") throw new Error("fixture credential selection failed");
  client.releaseCodex = async () => ({ auth_mode: "api_key", access_token: "fixture-key" });
  const deps: CodexExecutorDeps = {
    transientRetryMax: 1, transientBackoffBaseMs: 1,
    idleMs: 10000, wallMs: 20000, boundaryDeadlineMs: 1000,
    installDeps: async () => ({ results: [], truncated: false }),
    launchProviderRoot: async () => ({ transport, supervisorPid: 1234,
      root: { kind: "provider", reap: async () => ({ ok: true }), dispose: async () => {} } }),
    rootStartTime: () => 1,
    spawnCommand: async () => ({ code: 0, stdout: JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok", tmp: "ok" }), stderr: "" }),
    wireFileop: () => ({ client: { op: async () => ({ ok: true }) }, dispose: async () => {} }),
    launchEffectRoot: async () => ({
      started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 200,
      transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      waitChild: async () => ({ event: "child_exit", code: 0 }),
      dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
      failed: undefined, whenFailed: new Promise<Error>(() => {}),
    }),
    sessionStore: { adopt: async () => ({ files: 0 }), inspect: async () => "absent",
      remove: async () => {}, persist: async () => ({ files: 0, bytes: 0 }) },
  };
  return { starts, credential, executor: new CodexExecutor(nullLogger(), homeDir, {
    binding: selected.binding, client, commandSandbox: "off",
    provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
  }, deps) };
}

class RevisionTransport implements CodexTransport {
  private queue: CodexNotification[] = [];
  private waiter?: (value: IteratorResult<CodexNotification>) => void;
  constructor(private readonly starts: Start[], private readonly frame: () => Frame) {}
  private push(value: CodexNotification): void {
    if (this.waiter) { const resolve = this.waiter; this.waiter = undefined; resolve({ value, done: false }); }
    else this.queue.push(value);
  }
  request<T>(method: string, params?: unknown): Promise<T> {
    const p = params as { type?: string; threadId?: string; input?: { text?: string }[] };
    let result: unknown = {};
    if (method === "initialize") result = { userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" };
    if (method === "account/login/start") result = { type: p.type };
    if (method === "thread/start" || method === "thread/resume") result = { thread: { id: "th-revision" } };
    if (method === "turn/start") {
      this.starts.push({ prompt: p.input?.[0]?.text ?? "", threadId: p.threadId });
      const n = this.starts.length;
      if (n === 1) this.push({ kind: "thread_started", method: "thread/started", threadId: "th-revision", params: { thread: { id: "th-revision" } } });
      const f = this.frame();
      const status = "failure" in f ? "failed" : "completed";
      if (!("failure" in f)) this.push({ kind: "activity", method: "item/tool/call", requestId: n,
        params: { threadId: "th-revision", turnId: "tn-revision", callId: `call-${n}`,
          tool: "ask" in f ? "ask_user" : "submit_plan",
          arguments: "ask" in f ? { questions: [{ question: "Which target?", header: "Target" }] } : { plan_md: f.plan } } });
      this.push({ kind: "turn_completed", method: "turn/completed", threadId: "th-revision", turnId: "tn-revision", status,
        params: { threadId: "th-revision", turn: { id: "tn-revision", status,
          ...(status === "failed" ? { error: { codexErrorInfo: { httpConnectionFailed: { httpStatusCode: 429 } } } } : {}) } } });
      result = { turn: { id: "tn-revision" } };
    }
    return Promise.resolve(result as T);
  }
  installServerRequestInterceptor(): () => void { return () => {}; }
  notify(): void {}
  respond(): void {}
  notifications(): AsyncIterableIterator<CodexNotification> {
    return {
      next: () => this.queue.length ? Promise.resolve({ value: this.queue.shift()!, done: false })
        : new Promise(resolve => { this.waiter = resolve; }),
      [Symbol.asyncIterator]() { return this; },
    };
  }
  close(): Promise<void> {
    this.waiter?.({ value: undefined, done: true });
    this.waiter = undefined;
    return Promise.resolve();
  }
}
