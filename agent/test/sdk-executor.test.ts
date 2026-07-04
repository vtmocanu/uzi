import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage, HookInput } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { AgentTemplate } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// The SDK executor is exercised only up to — never across — the network
// boundary: `queryFn` is faked, so guardrails, sparse env, session/resume, the
// message stream, and the watchdogs are all provable with dummy credentials and
// NO live Anthropic session (testing-credentials policy).

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

const coder: AgentTemplate = {
  name: "coder",
  description: "writes code",
  prompt_body: "You implement.",
  tools: ["Read", "Edit", "Write", "Bash"],
};
const reviewer: AgentTemplate = {
  name: "reviewer",
  description: "reviews",
  prompt_body: "You review.",
  tools: ["Read", "Grep"],
};
const lead: AgentTemplate = {
  name: "lead",
  description: "leads",
  prompt_body: "LEAD SYSTEM PROMPT",
  model: "fable",
};

interface Captured {
  options?: SdkOptions;
  promptText?: string;
}

type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);

/** A fake `query`: captures the options/prompt, then replays a scripted stream. */
function fakeQuery(script: Script): { queryFn: SdkQueryFn; captured: Captured } {
  const captured: Captured = {};
  const queryFn: SdkQueryFn = (params) => {
    captured.options = params.options;
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        captured.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      if (typeof script === "function") {
        yield* script(params.options.abortController!.signal) as AsyncIterable<SDKMessage>;
      } else {
        for (const m of script) yield m;
      }
    })();
  };
  return { queryFn, captured };
}

/** Async iterable that yields nothing and returns only once aborted. */
function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        signal.addEventListener("abort", () => resolve(), { once: true });
      });
    },
  };
}

function resultSuccess(sessionId: string): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

let homeDir: string;
let saved: Record<string, string | undefined>;

function makeCtx(overrides: Partial<RunContext> = {}): {
  ctx: RunContext;
  emits: EmittedMessage[];
  sessionIds: string[];
} {
  const emits: EmittedMessage[] = [];
  const sessionIds: string[] = [];
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: "/tmp/does-not-need-to-exist",
    branch: "agent/issue-5",
    emit: (m) => emits.push(m),
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: (s) => sessionIds.push(s),
    ...overrides,
  };
  return { ctx, emits, sessionIds };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sdkhome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
});

afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("SdkExecutor guardrail options", () => {
  it("locks down permissions, isolation, subagents, and reports the branch", async () => {
    const { queryFn, captured } = fakeQuery([resultSuccess("sess-1")]);
    const { ctx, emits, sessionIds } = makeCtx({ agents: [lead, coder, reviewer] });

    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.deepStrictEqual(result, { branch: "agent/issue-5" });

    const o = captured.options!;
    assert.strictEqual(o.permissionMode, "bypassPermissions");
    assert.strictEqual(o.allowDangerouslySkipPermissions, true);
    assert.deepStrictEqual(o.settingSources, []);
    assert.strictEqual(o.includePartialMessages, false);
    assert.strictEqual(o.cwd, "/tmp/does-not-need-to-exist");
    assert.strictEqual(o.resume, undefined); // no session to resume

    // Lead template drives the system prompt + model; it is NOT a subagent.
    // systemPrompt keeps the claude_code preset and appends the lead body.
    assert.ok(o.systemPrompt && typeof o.systemPrompt === "object" && !Array.isArray(o.systemPrompt));
    const sp = o.systemPrompt as { type: string; preset: string; append: string };
    assert.strictEqual(sp.type, "preset");
    assert.strictEqual(sp.preset, "claude_code");
    assert.match(sp.append, /^LEAD SYSTEM PROMPT\n\n/);
    assert.strictEqual(o.model, "fable");
    // Detached-spawn hook is wired so a watchdog trip can group-kill the tree.
    assert.strictEqual(typeof o.spawnClaudeCodeProcess, "function");
    assert.deepStrictEqual(Object.keys(o.agents ?? {}).sort(), ["coder", "reviewer"]);
    for (const name of ["coder", "reviewer"]) {
      assert.deepStrictEqual(o.agents?.[name]?.disallowedTools, ["Agent"]);
    }

    // PreToolUse hook is wired, scoped to Bash.
    const matcher = o.hooks?.PreToolUse?.[0];
    assert.strictEqual(matcher?.matcher, "Bash");
    assert.strictEqual(matcher?.hooks.length, 1);

    // session id surfaced once; success status + start status streamed.
    assert.deepStrictEqual(sessionIds, ["sess-1"]);
    assert.ok(emits.some((m) => m.kind === "status" && m.payload["event"] === "result"));
  });

  it("hands the SDK a sparse env with no worker secrets", async () => {
    const { queryFn, captured } = fakeQuery([resultSuccess("s")]);
    const { ctx } = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);

    const env = captured.options!.env!;
    assert.deepStrictEqual(
      new Set(Object.keys(env)),
      new Set(["CLAUDE_CODE_OAUTH_TOKEN", "HOME", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]),
    );
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, OAUTH);
    assert.strictEqual(env.HOME, homeDir);
    const serialized = JSON.stringify(env);
    assert.ok(!serialized.includes(FAKE_PAT), "PAT must not reach the SDK env");
    assert.ok(!serialized.includes(FAKE_JOIN_TOKEN), "join token must not reach the SDK env");
  });

  it("wires a PreToolUse hook that denies a protected push", async () => {
    const { queryFn, captured } = fakeQuery([resultSuccess("s")]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx);

    const hook = captured.options!.hooks!.PreToolUse![0]!.hooks[0]!;
    const input: HookInput = {
      session_id: "s",
      transcript_path: "/t",
      cwd: "/w",
      hook_event_name: "PreToolUse",
      tool_name: "Bash",
      tool_input: { command: "git push origin main" },
      tool_use_id: "tu",
    } as HookInput;
    const out = await hook(input, "tu", { signal: new AbortController().signal });
    assert.strictEqual(
      (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
      "deny",
    );
  });

  it("passes the claim's session id as the SDK resume target", async () => {
    const { queryFn, captured } = fakeQuery([resultSuccess("s")]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ sessionId: "prev-session" }).ctx);
    assert.strictEqual(captured.options!.resume, "prev-session");
  });
});

describe("SdkExecutor streaming + attribution", () => {
  it("maps assistant and subagent frames onto the run stream", async () => {
    const script: SDKMessage[] = [
      { type: "assistant", session_id: "s2", message: { content: [{ type: "text", text: "planning" }] } } as unknown as SDKMessage,
      { type: "assistant", subagent_type: "reviewer", message: { content: [{ type: "text", text: "LGTM" }] } } as unknown as SDKMessage,
      resultSuccess("s2"),
    ];
    const { queryFn } = fakeQuery(script);
    const { ctx, emits } = makeCtx({ agents: [coder, reviewer] });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);

    assert.ok(emits.some((m) => m.kind === "text" && m.agent === "lead" && m.payload["text"] === "planning"));
    assert.ok(emits.some((m) => m.kind === "text" && m.agent === "reviewer" && m.payload["text"] === "LGTM"));
  });
});

describe("SdkExecutor failure + watchdog paths", () => {
  it("fails fast when no OAuth token is present", async () => {
    const { queryFn } = fakeQuery([resultSuccess("s")]);
    const { ctx } = makeCtx({ oauthToken: undefined });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx),
      /no Anthropic OAuth token/,
    );
  });

  it("fails when the agent run ends in an error result", async () => {
    const script: SDKMessage[] = [
      { type: "result", subtype: "error_max_turns", is_error: true, errors: ["cap"], session_id: "s" } as unknown as SDKMessage,
    ];
    const { queryFn } = fakeQuery(script);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx),
      /agent run failed: error_max_turns/,
    );
  });

  it("trips the idle watchdog on a silent agent and aborts the SDK", async () => {
    const { queryFn, captured } = fakeQuery((signal) => hangUntilAbort(signal));
    const { ctx } = makeCtx({ config: { idle_timeout_seconds: 0.03, run_timeout_seconds: 100 } });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx),
      /idle timeout/,
    );
    // abort() is the asserted stop: the abortController handed to the SDK fired.
    assert.strictEqual(captured.options!.abortController!.signal.aborted, true);
  });

  it("trips the wall-clock watchdog even while the agent is active and aborts", async () => {
    // Yield a message (resets idle) then hang; a short wall-clock still fires.
    const script = (signal: AbortSignal): AsyncIterable<unknown> => ({
      async *[Symbol.asyncIterator]() {
        yield { type: "assistant", session_id: "s", message: { content: [{ type: "text", text: "working" }] } };
        yield* hangUntilAbort(signal);
      },
    });
    const { queryFn, captured } = fakeQuery(script);
    const { ctx } = makeCtx({ config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.03 } });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx),
      /wall-clock timeout/,
    );
    assert.strictEqual(captured.options!.abortController!.signal.aborted, true);
  });

  it("cancels via an external abort signal", async () => {
    const controller = new AbortController();
    controller.abort();
    const { queryFn } = fakeQuery((signal) => hangUntilAbort(signal));
    const { ctx } = makeCtx({ signal: controller.signal });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx),
      /run cancelled/,
    );
  });
});
