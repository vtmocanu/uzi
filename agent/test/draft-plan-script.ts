import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import { CodexHarness } from "../src/codex/codex-harness.js";
import { CodexCallbackBroker } from "../src/codex/broker.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { ClaudeHarness } from "../src/claude-harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { RunHarness, RunTurnRequest } from "../src/harness.js";
import { nullLogger } from "./helpers.js";

export const request: RunTurnRequest = {
  prompt: "Plan", systemPrompt: "lead", signal: new AbortController().signal,
  phase: "plan", agents: {}, leadSkills: [],
};

export function claudeScript(frames: unknown[], failure?: Error): RunHarness {
  const queryFn = (() => (async function* () {
    for (const frame of frames) yield frame;
    if (failure) throw failure;
  })()) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn, spawn: () => ({ pid: undefined }), kill: () => true, log: nullLogger(),
    contextUsageTimeoutMs: 100, spawnedPids: new Set<number>(),
    homeDir: "/tmp/draft-test-home-absent",
  });
  harness.prepareTurn({
    cwd: "/tmp/draft-test", env: {}, skillsPluginPath: "/tmp/draft-plugin",
    skills: [], systemPrompt: "lead", agents: {}, mcpServers: {}, preToolUse: [],
  }, { pid: undefined });
  return harness;
}

export function capture(plan_md: unknown, markers = {}): unknown {
  return { type: "assistant", ...markers, message: { content: [
    { type: "tool_use", id: "draft-call", name: "mcp__uzi__save_draft_plan", input: { plan_md } },
  ] } };
}

/** Finite script: each notification is consumed once; an injected throw ends the iterator. */
export function codexScript(notes: CodexNotification[], failure?: Error, scrubProjected?: (s: string) => string) {
  let harness: CodexHarness;
  const replies: unknown[] = [];
  const replied = new Set<string | number>();
  let reaps = 0;
  const transport: CodexTransport = {
    async request<T>(method: string): Promise<T> {
      return (method === "thread/start" ? { thread: { id: "th-1" } } :
        method === "turn/start" ? { turn: { id: "tn-1" } } : {}) as T;
    },
    notify() {},
    respond(id, result) { replies.push(result); replied.add(id); },
    async *notifications() {
      for (const note of notes) {
        yield note;
        // Bound each callback wait: finish child projection before the next scripted note.
        if (note.kind === "activity" && note.requestId !== undefined) {
          const deadline = Date.now() + 2000;
          while (!replied.has(note.requestId)) {
            if (Date.now() >= deadline) throw new Error("scripted callback did not settle");
            await new Promise<void>((resolve) => setImmediate(resolve));
          }
        }
      }
      if (failure) throw failure;
    },
    async close() {},
  };
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const broker = new CodexCallbackBroker({
    registry, worktreePath: "/tmp/draft-test",
    grants: { role: "lead", phase: "plan", isRoot: true,
      allowedTools: new Set(["save_draft_plan", "submit_plan", "spawn_agent"]), allowedSkills: new Set() },
    spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
    fileop: { op: async () => ({ ok: false, code: "E_IO" }) },
    delegate: async ({ parent, role }) => {
      const thread = `child-${parent.callId}`;
      harness.registerChildSink(thread, { push() {} });
      harness.bindChildDispatch(thread, parent, role);
      harness.emitChildFrame(thread, [
        { kind: "tool", phase: "started", id: "validation", name: "Read", input: { file_path: "WORK.txt" } },
        { kind: "tool", phase: "finished", id: "validation", name: "Read", output: "bounded feature" },
        { kind: "text", text: `${role}: approved` },
      ]);
      harness.unregisterChildSink(thread);
      return { ok: true, output: { text: `${role}: approved` } };
    }, allowedRoles: new Set(["reviewer", "tester"]),
  });
  harness = new CodexHarness({
    registry, broker, workspace: "/tmp/draft-test", homeDir: "/tmp/draft-codex",
    provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "test" },
    log: nullLogger(), sessionInspect: async () => "unknown", scrubProjected,
    launchRoot: async () => ({ transport, supervisorPid: 4321,
      root: { kind: "provider", reap: async () => { reaps++; return { ok: true }; }, dispose: async () => {} } }),
  });
  return { harness, replies, broker, registry, get reaps() { return reaps; } };
}

export function toolNote(tool: string, args: unknown, callId = "draft-call"): CodexNotification {
  return { kind: "activity", method: "item/tool/call", requestId: callId,
    params: { threadId: "th-1", turnId: "tn-1", callId, tool, arguments: args, namespace: null } };
}

