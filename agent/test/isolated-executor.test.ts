// PRD #1906 M4: the isolated executor's fail-closed confinement. Every test here runs with
// a fake queryFn (no live session); the fake stands in for the CLI and, where a test needs
// it, calls the configured PreToolUse hooks exactly as the CLI would before a tool runs.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { HookInput, HookJSONOutput, Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";

import {
  buildIsolatedSdkOptions,
  checkIsolatedInit,
  isolateSdkEnv,
  IsolatedExecutor,
  ISOLATED_TOOLS,
  type IsolatedContext,
} from "../src/isolated-executor.js";
import { buildFetchToolsServer } from "../src/fetch-tools.js";
import { buildSdkEnv } from "../src/sdk-env.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage } from "../src/executor.js";
import { nullLogger } from "./helpers.js";

const OAUTH = "dummy-oauth-token-isolated-0000";
const NONESSENTIAL = "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC";

/** The REAL init frame the pinned SDK emitted for these options (see its _provenance). */
function realInit(): Record<string, unknown> {
  const p = path.join(import.meta.dirname, "fixtures", "isolated-init-frame.json");
  return JSON.parse(fs.readFileSync(p, "utf8")) as Record<string, unknown>;
}

function assistantText(text: string): SDKMessage {
  return { type: "assistant", session_id: "s", message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function resultSuccess(): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "s" } as unknown as SDKMessage;
}

/** Run every PreToolUse hook whose matcher covers `tool`, as the CLI does; first deny wins. */
async function simulateToolUse(options: SdkOptions, tool: string, toolInput: Record<string, unknown>): Promise<"allow" | "deny"> {
  for (const m of options.hooks?.PreToolUse ?? []) {
    if (m.matcher !== undefined && !new RegExp(`^(?:${m.matcher})$`).test(tool)) continue;
    for (const hook of m.hooks) {
      const input = { hook_event_name: "PreToolUse", tool_name: tool, tool_input: toolInput, session_id: "s", transcript_path: "", cwd: options.cwd } as unknown as HookInput;
      const out = (await hook(input, undefined, { signal: new AbortController().signal })) as HookJSONOutput;
      const d = (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision;
      if (d === "deny") return "deny";
    }
  }
  return "allow";
}

type Step = SDKMessage | ((options: SdkOptions) => Promise<void>);

/** A fake query that plays `steps`; a function step is a CLI-side action (e.g. a tool call). */
function fakeQuery(steps: Step[]): { queryFn: SdkQueryFn; seen: { options?: SdkOptions } } {
  const seen: { options?: SdkOptions } = {};
  const queryFn: SdkQueryFn = (params) => {
    seen.options = params.options;
    return (async function* () {
      for await (const _ of params.prompt) break;
      for (const s of steps) {
        if (params.options.abortController?.signal.aborted) return;
        if (typeof s === "function") await s(params.options);
        else yield s;
      }
    })() as ReturnType<SdkQueryFn>;
  };
  return { queryFn, seen };
}

let dir: string;
let workspace: string;

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-isolated-exec-"));
  workspace = path.join(dir, "workspace");
  fs.mkdirSync(workspace);
  fs.mkdirSync(path.join(dir, "home"));
});
afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

function ctx(emitted: EmittedMessage[] = []): IsolatedContext {
  const fetch = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace, log: nullLogger() });
  return {
    runId: "run-1",
    oauthToken: OAUTH,
    workspace,
    homeDir: path.join(dir, "home"),
    prompt: "research",
    fetchServer: fetch.server,
    emit: (m) => emitted.push(m),
    signal: new AbortController().signal,
    maxTurns: 5,
    timeoutMs: 30_000,
  };
}

function options(gate = { passed: false }): SdkOptions {
  const fetch = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace, log: nullLogger() });
  return buildIsolatedSdkOptions({
    env: isolateSdkEnv(buildSdkEnv(OAUTH, path.join(dir, "home"))),
    cwd: workspace,
    log: nullLogger(),
    secretPaths: ["/run/secrets/worker_token"],
    fetchServer: fetch.server,
    gate,
    maxTurns: 5,
  });
}

describe("isolated executor: the init check (fail closed on the EFFECTIVE tool set)", () => {
  it("the fixed tool set is exactly the named list", () => {
    assert.deepEqual([...ISOLATED_TOOLS], ["Read", "Write", "Edit", "Grep", "Glob", "mcp__uzi_fetch__fetch_url"]);
  });

  it("accepts the real init frame the pinned SDK emits for these options", () => {
    assert.equal(checkIsolatedInit(realInit()), undefined);
  });

  it("refuses an extra tool, a missing tool, an extra MCP server, a plugin, a stray agent or skill", () => {
    const cases: Array<[string, (f: Record<string, unknown>) => void, RegExp]> = [
      ["extra tool", (f) => (f.tools as string[]).push("WebSearch"), /extra: WebSearch/],
      ["missing tool", (f) => (f.tools = (f.tools as string[]).filter((t) => t !== "Read")), /missing: Read/],
      ["extra mcp", (f) => (f.mcp_servers as unknown[]).push({ name: "forge", status: "connected" }), /MCP servers/],
      ["plugin", (f) => (f.plugins = [{ name: "p", path: "/x" }]), /plugin/],
      ["agent", (f) => (f.agents as string[]).push("repo-agent"), /unexpected agents: repo-agent/],
      ["skill", (f) => (f.skills as string[]).push("pdf"), /unexpected skills: pdf/],
      ["malformed tools", (f) => (f.tools = "Read"), /malformed tool list/],
    ];
    for (const [name, mutate, want] of cases) {
      const f = realInit();
      mutate(f);
      assert.match(checkIsolatedInit(f) ?? "", want, name);
    }
  });

  it("treats absent plugins/skills/agents as empty", () => {
    const f = realInit();
    delete f.plugins;
    delete f.skills;
    delete f.agents;
    assert.equal(checkIsolatedInit(f), undefined);
  });

  it("an extra tool in the init frame aborts the run before any later frame is acted on", async () => {
    const init = realInit();
    (init.tools as string[]).push("Bash");
    const emitted: EmittedMessage[] = [];
    const { queryFn } = fakeQuery([init as unknown as SDKMessage, assistantText("should never be emitted"), resultSuccess()]);
    const ex = new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true });
    await assert.rejects(ex.run(ctx(emitted)), /isolated run refused: effective tool set differs.*extra: Bash/);
    assert.ok(!JSON.stringify(emitted).includes("should never be emitted"), "no frame after the failed init was emitted");
  });

  it("the happy path: a matching init, then output, then success", async () => {
    const emitted: EmittedMessage[] = [];
    const { queryFn } = fakeQuery([realInit() as unknown as SDKMessage, assistantText("done"), resultSuccess()]);
    await new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true }).run(ctx(emitted));
    assert.ok(JSON.stringify(emitted).includes("done"));
  });

  it("no init frame at all fails the run", async () => {
    const { queryFn } = fakeQuery([resultSuccess()]);
    await assert.rejects(new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true }).run(ctx()), /no init frame/);
  });

  it("model output before the init frame fails the run", async () => {
    const { queryFn } = fakeQuery([assistantText("early"), realInit() as unknown as SDKMessage, resultSuccess()]);
    await assert.rejects(new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true }).run(ctx()), /before its tool set was verified/);
  });
});

describe("isolated executor: the tool gate (matcher-less PreToolUse hook + init latch)", () => {
  it("a Bash tool_use before init is denied, and so is an allowed tool before init", async () => {
    const decisions: string[] = [];
    const { queryFn } = fakeQuery([
      async (o) => void decisions.push(`bash-pre:${await simulateToolUse(o, "Bash", { command: "curl https://x" })}`),
      async (o) => void decisions.push(`read-pre:${await simulateToolUse(o, "Read", { file_path: path.join(workspace, "a") })}`),
      realInit() as unknown as SDKMessage,
      async (o) => void decisions.push(`read-post:${await simulateToolUse(o, "Read", { file_path: path.join(workspace, "a") })}`),
      async (o) => void decisions.push(`bash-post:${await simulateToolUse(o, "Bash", { command: "ls" })}`),
      async (o) => void decisions.push(`websearch-post:${await simulateToolUse(o, "WebSearch", { query: "x" })}`),
      async (o) => void decisions.push(`fetch-post:${await simulateToolUse(o, "mcp__uzi_fetch__fetch_url", { url: "https://a" })}`),
      resultSuccess(),
    ]);
    await new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true }).run(ctx());
    assert.deepEqual(decisions, [
      "bash-pre:deny",
      "read-pre:deny",
      "read-post:allow",
      "bash-post:deny",
      "websearch-post:deny",
      "fetch-post:allow",
    ]);
  });

  it("with no init at all, every tool call (Bash and Read alike) is denied", async () => {
    const decisions: string[] = [];
    const { queryFn } = fakeQuery([
      async (o) => void decisions.push(await simulateToolUse(o, "Bash", { command: "id" })),
      async (o) => void decisions.push(await simulateToolUse(o, "Read", { file_path: path.join(workspace, "a") })),
      resultSuccess(),
    ]);
    await assert.rejects(new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true }).run(ctx()), /no init frame/);
    assert.deepEqual(decisions, ["deny", "deny"]);
  });

  it("an unknown tool name is denied even after init passed", async () => {
    const o = options({ passed: true });
    assert.equal(await simulateToolUse(o, "SomeFutureWebTool", {}), "deny");
    assert.equal(await simulateToolUse(o, "Agent", { prompt: "x" }), "deny");
  });
});

describe("isolated executor: SDK options and the path guard", () => {
  it("restricts tools, disallows nested/web/shell tools, loads nothing from disk, and wires only uzi_fetch", () => {
    const o = options();
    assert.deepEqual(o.tools, [...ISOLATED_TOOLS]);
    assert.deepEqual(o.settingSources, []);
    assert.deepEqual(Object.keys(o.mcpServers ?? {}), ["uzi_fetch"]);
    assert.equal(o.strictMcpConfig, true);
    assert.deepEqual(o.plugins, []);
    for (const t of ["Agent", "Task", "Bash", "WebFetch", "WebSearch", "Skill"]) {
      assert.ok(o.disallowedTools?.includes(t), `${t} is disallowed`);
    }
    assert.equal(o.hooks?.PreToolUse?.[0]?.matcher, undefined, "the tool gate is matcher-less");
  });

  it("the path guard denies /run/uzi-secrets/x and paths outside the workspace, allows the workspace", async () => {
    const o = options({ passed: true });
    fs.writeFileSync(path.join(workspace, "notes.md"), "x");
    assert.equal(await simulateToolUse(o, "Read", { file_path: "/run/uzi-secrets/x" }), "deny");
    assert.equal(await simulateToolUse(o, "Read", { file_path: path.join(dir, "home", "x") }), "deny");
    assert.equal(await simulateToolUse(o, "Write", { file_path: "/tmp/elsewhere.txt", content: "x" }), "deny");
    assert.equal(await simulateToolUse(o, "Grep", { pattern: "x", path: "/etc" }), "deny");
    assert.equal(await simulateToolUse(o, "Read", { file_path: path.join(workspace, "notes.md") }), "allow");
  });
});

describe("isolated executor: fetched sources are read-only to the model", () => {
  const SHA = "a".repeat(64);

  function withSource(): SdkOptions {
    const o = options({ passed: true });
    fs.mkdirSync(path.join(workspace, "sources"));
    fs.writeFileSync(path.join(workspace, "sources", SHA), "fetched bytes");
    return o;
  }

  it("denies Write/Edit on sources/<sha256> in every path form", async () => {
    const o = withSource();
    fs.mkdirSync(path.join(workspace, "notes"));
    fs.symlinkSync(path.join(workspace, "sources"), path.join(workspace, "srclink"));
    const forms = [
      `sources/${SHA}`,
      `./sources/${SHA}`,
      `notes/../sources/${SHA}`,
      path.join(workspace, "sources", SHA),
      `${workspace}/./notes/../sources/${SHA}`,
      `srclink/${SHA}`,
      "sources/new-file.md",
      "sources",
    ];
    for (const tool of ["Write", "Edit"]) {
      for (const p of forms) {
        const input = tool === "Write" ? { file_path: p, content: "x" } : { file_path: p, old_string: "fetched", new_string: "forged" };
        assert.equal(await simulateToolUse(o, tool, input), "deny", `${tool} ${p}`);
      }
    }
  });

  it("the sources hook also covers MultiEdit and NotebookEdit (not in the tool set, so denied twice over)", async () => {
    const o = withSource();
    const writeOnly = o.hooks?.PreToolUse?.find(
      (m) => m.matcher !== undefined && new RegExp(`^(?:${m.matcher})$`).test("MultiEdit") && !new RegExp(`^(?:${m.matcher})$`).test("Read"),
    );
    assert.ok(writeOnly, "a write-only PreToolUse matcher exists");
    const cases: Array<[string, Record<string, unknown>]> = [
      ["MultiEdit", { file_path: `sources/${SHA}`, edits: [] }],
      ["NotebookEdit", { notebook_path: `sources/${SHA}`, new_source: "x" }],
    ];
    for (const [tool, input] of cases) {
      const hookInput = { hook_event_name: "PreToolUse", tool_name: tool, tool_input: input, session_id: "s", transcript_path: "", cwd: workspace } as unknown as HookInput;
      const out = (await writeOnly.hooks[0]!(hookInput, undefined, { signal: new AbortController().signal })) as {
        hookSpecificOutput?: { permissionDecision?: string };
      };
      assert.equal(out.hookSpecificOutput?.permissionDecision, "deny", tool);
    }
  });

  it("still allows Read/Grep/Glob of sources/ and writes elsewhere in the workspace", async () => {
    const o = withSource();
    assert.equal(await simulateToolUse(o, "Read", { file_path: `sources/${SHA}` }), "allow");
    assert.equal(await simulateToolUse(o, "Grep", { pattern: "x", path: "sources" }), "allow");
    assert.equal(await simulateToolUse(o, "Glob", { pattern: "*", path: path.join(workspace, "sources") }), "allow");
    assert.equal(await simulateToolUse(o, "Write", { file_path: "findings.md", content: "x" }), "allow");
    assert.equal(await simulateToolUse(o, "Write", { file_path: "sources-notes.md", content: "x" }), "allow");
  });
});

describe("isolated executor: the run secrets dir entry is load-bearing", () => {
  it("denies it even when the workspace root contains it, while the rest of the root stays readable", async () => {
    const fetch = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace: "/run", log: nullLogger() });
    const o = buildIsolatedSdkOptions({
      env: isolateSdkEnv(buildSdkEnv(OAUTH, path.join(dir, "home"))),
      cwd: "/run",
      log: nullLogger(),
      secretPaths: [],
      fetchServer: fetch.server,
      gate: { passed: true },
      maxTurns: 5,
    });
    // Containment alone would allow both: only the secret-dir entry tells them apart.
    const secretDir = ["", "run", "uzi-secrets"].join("/");
    assert.equal(await simulateToolUse(o, "Read", { file_path: "/run/some-other-file" }), "allow");
    assert.equal(await simulateToolUse(o, "Read", { file_path: `${secretDir}/fetch-token` }), "deny");
    assert.equal(await simulateToolUse(o, "Grep", { pattern: "x", path: "uzi-secrets" }), "deny");
  });
});

describe("isolated executor: env scope", () => {
  it("a normal run's env (buildSdkEnv, what SdkExecutor and ChatExecutor use) has no nonessential-traffic key", () => {
    const env = buildSdkEnv(OAUTH, "/h");
    assert.ok(!(NONESSENTIAL in env));
  });

  it("the isolated env sets it to 1 even when the input env tries to override it", () => {
    const base = buildSdkEnv(OAUTH, "/h", { [NONESSENTIAL]: "0" });
    assert.equal(base[NONESSENTIAL], "0", "precondition: the base env carries the override");
    assert.equal(isolateSdkEnv(base)[NONESSENTIAL], "1");
  });

  it("the executor hands the SDK child that env, and never the fetch credential", async () => {
    const { queryFn, seen } = fakeQuery([realInit() as unknown as SDKMessage, resultSuccess()]);
    await new IsolatedExecutor(nullLogger(), { queryFn, kill: () => true }).run(ctx());
    const env = seen.options?.env as Record<string, string | undefined>;
    assert.equal(env[NONESSENTIAL], "1");
    assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, OAUTH);
    assert.ok(!Object.values(env).includes("cred"), "the fetch credential is not in the SDK env");
  });
});
