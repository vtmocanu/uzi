import { after, afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { HookCallbackMatcher, HookInput, HookJSONOutput, Options } from "@anthropic-ai/claude-agent-sdk";
import { buildPathGuardHook } from "../src/guardrails.js";
import { nullLogger } from "./helpers.js";
import { FakeAnthropicApi, type MessagesRequest, type ScriptedTurn } from "./fake-anthropic-api.js";

// Issue #2332 AC6 — the one proof no unit test can give: the REAL bundled Claude CLI
// spills an oversized Bash output to `<HOME>/.claude/projects/<P>/<session_id>/tool-results/`,
// and our REAL PreToolUse path guard, fed the hook input the CLI really sends, allows a
// bounded Read of that spill file (fresh run, resumed run, subagent) while still denying
// a Read of the SDK HOME's settings.json. The model is a scripted local fake of the
// Anthropic Messages API, so nothing leaves 127.0.0.1 and the guard never parses prose.

// Per-drive abort budget: two drives (resume) must finish inside the 120s test cap, so the abort
// (which stops the CLI child) fires before the test timeout.
const DRIVE_BUDGET_MS = 50_000;
const MARKER = "SPILL-MARKER-7c1e9d";
const MARKER_LINE = 1400;
const SUBAGENT_TOKEN = "SUBAGENT-TASK-5b2f";
// ~40 chars a line x 1500 lines = ~60KB, past the CLI's 30000-char inline limit. The marker sits on
// line 1400, far beyond the inline preview, so seeing it proves the spill file itself was read.
const BASH_COMMAND =
  `awk 'BEGIN { for (i = 1; i <= 1500; i++) { if (i == ${MARKER_LINE}) print "${MARKER}"; ` +
  `else printf "line %04d xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\\n", i } }'`;

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((b: { type?: string; text?: string; content?: unknown }) =>
      b.type === "tool_result" ? textOf(b.content) : typeof b.text === "string" ? b.text : "",
    )
    .join("\n");
}

function hasToolResult(content: unknown): boolean {
  return Array.isArray(content) && content.some((b: { type?: string }) => b.type === "tool_result");
}

/** The conversation's step: assistant tool calls since the prompt message, and the last tool result. */
function stepOf(body: MessagesRequest, isPrompt: (text: string) => boolean): { step: number; lastResult: string; prompt: string } {
  let promptIdx = -1;
  body.messages.forEach((m, i) => {
    if (m.role === "user" && !hasToolResult(m.content) && isPrompt(textOf(m.content))) promptIdx = i;
  });
  const after = body.messages.slice(promptIdx + 1);
  const step = after.filter((m) => m.role === "assistant" && Array.isArray(m.content) && m.content.some((b: { type?: string }) => b.type === "tool_use")).length;
  const last = body.messages[body.messages.length - 1];
  return {
    step,
    lastResult: last && last.role === "user" && hasToolResult(last.content) ? textOf(last.content) : "",
    prompt: promptIdx >= 0 ? textOf(body.messages[promptIdx]?.content) : "",
  };
}

interface Observation {
  scenario: string;
  step: number;
  result: string;
}

describe("a Claude run Reads its own SDK spill file through the real CLI (issue #2332)", () => {
  const roots: string[] = [];
  const observed: Observation[] = [];
  let fake: FakeAnthropicApi;
  let baseUrl: string;
  let home: string;
  let worktree: string;
  let tmp: string;
  const hookRecords: Array<{ input: HookInput; decision: HookJSONOutput }> = [];

  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  /** The scripted model. Routes on request content: side calls (no tools) get trivial text. */
  function script(body: MessagesRequest): ScriptedTurn {
    const toolNames = (body.tools ?? []).map((t) => t.name);
    if (!toolNames.includes("Bash")) return { kind: "text", text: "ok" };
    const isSubagent = textOf(body.messages[0]?.content).includes(SUBAGENT_TOKEN);
    const spillPath = (r: string): string => /saved to: (\S+\.txt)/.exec(r)?.[1] ?? "MISSING-SPILL-PATH";
    // A bounded Read around the marker so reading the spill does not itself spill again.
    const boundedRead = (r: string): ScriptedTurn => ({
      kind: "tool_use",
      name: "Read",
      input: { file_path: spillPath(r), offset: MARKER_LINE - 5, limit: 10 },
    });
    if (isSubagent) {
      const { step, lastResult } = stepOf(body, (t) => t.includes(SUBAGENT_TOKEN));
      observed.push({ scenario: "subagent-inner", step, result: lastResult });
      if (step === 0) return { kind: "tool_use", name: "Bash", input: { command: BASH_COMMAND, description: "print a lot" } };
      if (step === 1) return boundedRead(lastResult);
      return { kind: "text", text: "subagent done" };
    }
    const { step, lastResult, prompt } = stepOf(body, (t) => t.includes("SCENARIO:"));
    const scenario = /SCENARIO:(\S+)/.exec(prompt)?.[1] ?? "unknown";
    observed.push({ scenario, step, result: lastResult });
    if (scenario.startsWith("SUBAGENT")) {
      if (step === 0) {
        const agentTool = toolNames.includes("Agent") ? "Agent" : "Task";
        return {
          kind: "tool_use",
          name: agentTool,
          input: { description: "spill reader", prompt: `${SUBAGENT_TOKEN}: run the big command, then read the spill`, subagent_type: "helper" },
        };
      }
      return { kind: "text", text: "main done" };
    }
    if (step === 0) return { kind: "tool_use", name: "Bash", input: { command: BASH_COMMAND, description: "print a lot" } };
    if (step === 1) return boundedRead(lastResult);
    if (step === 2) return { kind: "tool_use", name: "Read", input: { file_path: path.join(home, ".claude", "settings.json") } };
    return { kind: "text", text: "all done" };
  }

  beforeEach(async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-spill-it-"));
    roots.push(root);
    home = path.join(root, "home");
    worktree = path.join(root, "worktree");
    fs.mkdirSync(path.join(home, ".claude"), { recursive: true });
    tmp = path.join(root, "tmp");
    fs.mkdirSync(worktree, { recursive: true });
    fs.mkdirSync(tmp, { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "settings.json"), "{}\n");
    observed.length = 0;
    hookRecords.length = 0;
    fake = new FakeAnthropicApi(script);
    baseUrl = await fake.listen();
  });

  afterEach(async () => {
    await fake.close();
  });

  function options(extra: Partial<Options> = {}): Options {
    const guard = buildPathGuardHook(worktree, nullLogger(), [], { sdkHomeDir: home });
    const recording = async (input: HookInput): Promise<HookJSONOutput> => {
      const decision = await guard(input);
      hookRecords.push({ input, decision });
      return decision;
    };
    const hooks: Partial<Record<"PreToolUse", HookCallbackMatcher[]>> = {
      PreToolUse: [{ matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep", hooks: [recording] }],
    };
    return {
      cwd: worktree,
      // Minimal env: no inherited credentials. The key is a placeholder the fake accepts.
      env: {
        PATH: process.env.PATH ?? "",
        HOME: home,
        // Keeps the CLI's /tmp/claude-<uid>/... task dirs inside the root the test removes.
        TMPDIR: tmp,
        ANTHROPIC_BASE_URL: baseUrl,
        ANTHROPIC_API_KEY: ["dummy", "key"].join("-"),
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
        DISABLE_TELEMETRY: "1",
        DISABLE_AUTOUPDATER: "1",
      },
      model: "claude-sonnet-4-5",
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      settingSources: [],
      hooks,
      ...extra,
    };
  }

  /** Run one query to completion; returns the session id from the init message. */
  async function drive(prompt: string, extra: Partial<Options> = {}): Promise<string> {
    let sid = "";
    const deadline = AbortSignal.timeout(DRIVE_BUDGET_MS);
    const controller = new AbortController();
    deadline.addEventListener("abort", () => controller.abort());
    try {
      for await (const m of query({ prompt, options: { ...options(extra), abortController: controller } })) {
        if (m.type === "system" && m.subtype === "init") sid = m.session_id;
      }
    } catch (err) {
      assert.fail(`query failed: ${String(err)}\nfake API requests:\n${fake.requestLog()}`);
    }
    assert.ok(sid, `no init message with a session id\nfake API requests:\n${fake.requestLog()}`);
    return sid;
  }

  const diag = (): string => `\nfake API requests:\n${fake.requestLog()}\nobserved: ${JSON.stringify(observed.map((o) => [o.scenario, o.step, o.result.slice(0, 160)]))}`;

  function spillFiles(sid: string): string[] {
    const projects = path.join(home, ".claude", "projects");
    if (!fs.existsSync(projects)) return [];
    return fs
      .readdirSync(projects)
      .map((slug) => path.join(projects, slug, sid, "tool-results"))
      .filter((d) => fs.existsSync(d))
      .flatMap((d) => fs.readdirSync(d).map((f) => path.join(d, f)));
  }

  const filePathOf = (input: HookInput): unknown => (input as { tool_input?: { file_path?: unknown } }).tool_input?.file_path;
  const readsOf = (sid: string) => hookRecords.filter((r) => r.input.hook_event_name === "PreToolUse" && r.input.tool_name === "Read" && r.input.session_id === sid);
  const isSpill = (p: unknown): boolean => typeof p === "string" && p.includes(`${path.sep}tool-results${path.sep}`);
  const spillReads = (sid: string) => readsOf(sid).filter((r) => isSpill(filePathOf(r.input)));
  const denied = (d: HookJSONOutput): boolean => (d as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision === "deny";
  const resultAt = (scenario: string, step: number): string => observed.filter((o) => o.scenario === scenario && o.step === step).map((o) => o.result).join("\n");

  function assertSpillAllowed(sid: string, scenario: string, readStep: number): void {
    const files = spillFiles(sid);
    assert.ok(files.length > 0, `no spill file under <HOME>/.claude/projects/*/${sid}/tool-results/${diag()}`);
    const reads = spillReads(sid);
    assert.ok(reads.length > 0, `the hook never saw a Read of the spill file with session_id ${sid}${diag()}`);
    assert.ok(reads.every((r) => !denied(r.decision)), `the spill Read was denied: ${JSON.stringify(reads.map((r) => r.decision))}${diag()}`);
    const result = resultAt(scenario, readStep);
    assert.ok(result.includes(MARKER), `the spill Read result lacks the marker${diag()}`);
    assert.ok(!result.includes("denied by guardrail"), `the spill Read result was a denial${diag()}`);
    // The Read must be bounded so reading the spill does not itself spill (and point at another file).
    for (const r of reads) {
      const ti = (r.input as { tool_input?: { offset?: unknown; limit?: unknown } }).tool_input;
      assert.equal(typeof ti?.offset, "number", `the spill Read has no numeric offset: ${JSON.stringify(ti)}${diag()}`);
      assert.equal(typeof ti?.limit, "number", `the spill Read has no numeric limit: ${JSON.stringify(ti)}${diag()}`);
      assert.ok((ti?.limit as number) <= 20, `the spill Read limit is not small: ${JSON.stringify(ti)}${diag()}`);
    }
    assert.ok(!/Full output saved to|saved to:/.test(result), `the spill Read result itself spilled${diag()}`);
  }

  it("fresh run: a bounded Read of the spill is allowed, settings.json is still denied", { timeout: 120_000 }, async () => {
    const sid = await drive("SCENARIO:FRESH");
    assertSpillAllowed(sid, "FRESH", 2);
    const settings = readsOf(sid).filter((r) => String(filePathOf(r.input)).endsWith("settings.json"));
    assert.equal(settings.length, 1, `expected one settings.json Read${diag()}`);
    assert.ok(denied(settings[0]!.decision), `settings.json Read was not denied${diag()}`);
    assert.ok(resultAt("FRESH", 3).includes("denied by guardrail"), `the settings.json tool_result is not the guardrail denial${diag()}`);
  });

  it("resumed run: the same session id keeps the spill allowed", { timeout: 120_000 }, async () => {
    const first = await drive("SCENARIO:FRESH");
    const before = new Set(spillFiles(first));
    assert.ok(before.size > 0, `the fresh run left no spill file${diag()}`);
    hookRecords.length = 0;
    const second = await drive("SCENARIO:RESUMED", { resume: first });
    assert.equal(second, first, `resume changed the session id${diag()}`);
    assertSpillAllowed(second, "RESUMED", 2);
    const fresh = spillFiles(second).filter((f) => !before.has(f));
    assert.ok(fresh.length > 0, `the resumed run wrote no new spill file${diag()}`);
    const dirs = new Set([...before, ...fresh].map((f) => path.dirname(f)));
    assert.equal(dirs.size, 1, `spill files are not all in one <sid>/tool-results dir: ${[...dirs].join(", ")}`);
    const resumedReads = spillReads(second).filter((r) => fresh.includes(String(filePathOf(r.input))));
    assert.ok(resumedReads.length > 0, `the resumed run never Read its new spill file${diag()}`);
    assert.ok(resumedReads.every((r) => !denied(r.decision)), `the resumed spill Read was denied${diag()}`);
  });

  it("subagent: its spill lands in the root session's tool-results and is readable", { timeout: 120_000 }, async () => {
    const sid = await drive("SCENARIO:SUBAGENT", {
      agents: { helper: { description: "reads its own big output", prompt: "Do exactly what the task says.", tools: ["Bash", "Read"] } },
    });
    assertSpillAllowed(sid, "subagent-inner", 2);
    const reads = spillReads(sid);
    assert.ok(
      reads.some((r) => (r.input as { agent_id?: string }).agent_id),
      `no spill Read carried an agent_id (the subagent's)${diag()}`,
    );
  });
});
