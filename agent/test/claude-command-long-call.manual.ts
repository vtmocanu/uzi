// Issue #2471 M0 off-CI long-call proof (not a *.test.ts, so `npm test` never runs it).
//
//   UZI_M0_LONG_CALL_MS=20000 node --import tsx test/claude-command-long-call.manual.ts
//
// One lead `Bash` call is routed (production buildSdkOptions({guardedCommand})) to an INERT
// sentinel MCP handler that sleeps UZI_M0_LONG_CALL_MS while emitting nothing, then returns a
// marker. The server timeout is 3660000 ms (the 3600000 ms command maximum plus transport
// headroom). Prints one JSON object; exits 0 only if the handler's result reached the model
// after at least the configured duration.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createSdkMcpServer, query, tool } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import { buildSdkOptions, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { GUARDED_COMMAND_SERVER } from "../src/claude-command/grants.js";
import { FakeAnthropicApi, type MessagesRequest, type ScriptedTurn } from "./fake-anthropic-api.js";

const SERVER_TIMEOUT_MS = 3_660_000;
const MARKER = "LONG-CALL-RESULT-MARKER";

const rawDuration = process.env.UZI_M0_LONG_CALL_MS;
const durationMs = Number(rawDuration);
if (!rawDuration || !Number.isInteger(durationMs) || durationMs < 0) {
  console.error("UZI_M0_LONG_CALL_MS is required and must be a non-negative integer (milliseconds)");
  process.exit(2);
}

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((b: { type?: string; text?: string; content?: unknown }) =>
      b.type === "tool_result" ? textOf(b.content) : typeof b.text === "string" ? b.text : "",
    )
    .join("\n");
}

const sdkVersion = (
  JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "..", "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"), "utf8")) as {
    version: string;
  }
).version;

const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m0-long-"));
const [home, worktree, tmp] = ["home", "worktree", "tmp"].map((d) => path.join(root, d)) as [string, string, string];
for (const d of [home, worktree, tmp]) fs.mkdirSync(d, { recursive: true });

let toolResultText: string | undefined;
let toolResultAt: number | undefined;
const fake = new FakeAnthropicApi((body: MessagesRequest): ScriptedTurn => {
  if ((body.tools ?? []).length === 0) return { kind: "text", text: "ok" };
  const last = body.messages[body.messages.length - 1];
  const isResult = !!last && last.role === "user" && Array.isArray(last.content) && last.content.some((b: { type?: string }) => b.type === "tool_result");
  if (isResult) {
    toolResultText ??= textOf(last.content);
    toolResultAt ??= Date.now();
    return { kind: "text", text: "done" };
  }
  return { kind: "tool_use", name: "Bash", input: { command: "long-running-check", description: "long call" } };
});
const baseUrl = await fake.listen();

let handlerStartedAt: number | undefined;
let handlerEndedAt: number | undefined;
const server = createSdkMcpServer({
  name: GUARDED_COMMAND_SERVER,
  alwaysLoad: true,
  timeout: SERVER_TIMEOUT_MS,
  tools: [
    tool(
      "bash",
      "Run a shell command (inert M0 sentinel: sleeps, emits nothing, returns a marker).",
      { command: z.string(), timeout: z.number().optional(), description: z.string().optional(), run_in_background: z.boolean().optional() },
      async () => {
        handlerStartedAt = Date.now();
        await new Promise<void>((r) => setTimeout(r, durationMs));
        handlerEndedAt = Date.now();
        return { content: [{ type: "text" as const, text: MARKER }] };
      },
    ),
  ],
});

const cfg: ClaudeTurnConfig = {
  cwd: worktree,
  env: {
    PATH: process.env.PATH ?? "",
    HOME: home,
    TMPDIR: tmp,
    ANTHROPIC_BASE_URL: baseUrl,
    ANTHROPIC_API_KEY: ["dummy", "key"].join("-"),
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
    DISABLE_TELEMETRY: "1",
    DISABLE_AUTOUPDATER: "1",
  },
  skillsPluginPath: tmp,
  skills: [],
  systemPrompt: "You are a scripted test agent.",
  agents: {},
  mcpServers: { [GUARDED_COMMAND_SERVER]: server },
  preToolUse: [],
  model: "claude-sonnet-4-5",
  guardedCommand: true,
};
const controller = new AbortController();
const options: Options = { ...buildSdkOptions(cfg), abortController: controller };
delete options.plugins;

const startedAt = Date.now();
const budgetMs = durationMs + 120_000;
const budget = setTimeout(() => controller.abort(), budgetMs);
let cliVersion: string | undefined;
let queryOutcome = "completed";
try {
  for await (const m of query({ prompt: "LONG-CALL scenario", options })) {
    if (m.type === "system" && m.subtype === "init") cliVersion = (m as { claude_code_version?: string }).claude_code_version;
  }
} catch (err) {
  queryOutcome = `error: ${String(err)}`;
} finally {
  clearTimeout(budget);
}
const endedAt = Date.now();
await fake.close();
fs.rmSync(root, { recursive: true, force: true });

const handlerCompleted = handlerEndedAt !== undefined;
const resultReachedModel = toolResultText?.includes(MARKER) === true;
const elapsedToModelMs = toolResultAt !== undefined && handlerStartedAt !== undefined ? toolResultAt - handlerStartedAt : null;
const ok = resultReachedModel && elapsedToModelMs !== null && elapsedToModelMs >= durationMs;
console.log(
  JSON.stringify(
    {
      sdkVersion,
      cliVersion: cliVersion ?? null,
      config: { serverTimeoutMs: SERVER_TIMEOUT_MS, durationMs, queryBudgetMs: budgetMs },
      startedAt: new Date(startedAt).toISOString(),
      endedAt: new Date(endedAt).toISOString(),
      totalMs: endedAt - startedAt,
      handlerCompleted,
      handlerMs: handlerStartedAt !== undefined && handlerEndedAt !== undefined ? handlerEndedAt - handlerStartedAt : null,
      resultReachedModel,
      elapsedHandlerStartToModelMs: elapsedToModelMs,
      errorToolResultText: resultReachedModel ? null : (toolResultText ?? null),
      queryOutcome,
      ok,
    },
    null,
    2,
  ),
);
process.exit(ok ? 0 : 1);
