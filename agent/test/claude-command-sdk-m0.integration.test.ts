import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import type { AddressInfo } from "node:net";
import { createSdkMcpServer, query, tool } from "@anthropic-ai/claude-agent-sdk";
import type { AgentDefinition, HookCallback, HookInput, HookJSONOutput, Options } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import { assembleAgents, planTurnSubagents } from "../src/agents.js";
import { buildSdkOptions, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { GUARDED_COMMAND_SERVER, GUARDED_COMMAND_TOOL, shapeGuardedGrants } from "../src/claude-command/grants.js";
import type { AgentTemplate } from "../src/protocol.js";
import type { MessagesRequest } from "./fake-anthropic-api.js";

// Issue #2471 M0 (STOP gate): the one proof no unit test can give. The REAL bundled Claude CLI,
// driven through production buildSdkOptions({guardedCommand}) and production shapeGuardedGrants,
// must route a model `Bash` tool_use to an in-process MCP tool on the lead and on subagents,
// never execute native Bash, fire PreToolUse for the aliased call with a toolUseId that matches
// the handler's `_meta`, settle on abort, and honour the per-server timeout knob. The MCP handler
// is an INERT sentinel: it records and returns text, it never runs a command. The model is a
// scripted local fake of the Anthropic Messages API (nothing leaves 127.0.0.1). Every observation
// is logged as an `M0RESULT <json>` line.

const DRIVE_BUDGET_MS = 50_000;
const BUILTINS_DIR = path.join(import.meta.dirname, "..", "..", "api", "internal", "agenttmpl", "builtins");

function result(item: string, data: Record<string, unknown>): void {
  console.log(`M0RESULT ${JSON.stringify({ item, ...data })}`);
}

// ---- a multi-tool_use fake of the Messages API (the shared fake emits one call per turn) ----

type Turn =
  | { kind: "text"; text: string }
  | { kind: "tools"; calls: Array<{ name: string; input: Record<string, unknown> }> };

class MultiToolFake {
  private readonly server: http.Server;
  readonly bodies: Array<{ at: number; body: MessagesRequest }> = [];
  readonly issuedIds: string[] = [];
  constructor(private readonly respond: (body: MessagesRequest) => Turn) {
    this.server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => this.handle(req, res, Buffer.concat(chunks).toString("utf8")));
    });
  }
  async listen(): Promise<string> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    this.server.unref();
    return `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }
  async close(): Promise<void> {
    this.server.closeAllConnections();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }
  private handle(req: http.IncomingMessage, res: http.ServerResponse, raw: string): void {
    const p = (req.url ?? "").split("?")[0] ?? "";
    const json = (status: number, body: unknown): void => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.method === "POST" && p === "/v1/messages/count_tokens") return json(200, { input_tokens: 100 });
    if (req.method !== "POST" || p !== "/v1/messages") return json(404, { type: "error", error: { type: "not_found_error", message: "not found" } });
    let body: MessagesRequest;
    try {
      body = JSON.parse(raw) as MessagesRequest;
    } catch {
      return json(400, { type: "error", error: { type: "invalid_request_error", message: "bad json" } });
    }
    this.bodies.push({ at: Date.now(), body });
    const turn = this.respond(body);
    const seq = this.bodies.length;
    type Block = { type: "text"; text: string } | { type: "tool_use"; id: string; name: string; input: Record<string, unknown> };
    const content: Block[] =
      turn.kind === "text"
        ? [{ type: "text", text: turn.text }]
        : turn.calls.map((c, i) => {
            const id = `toolu_${seq}_${i}`;
            this.issuedIds.push(id);
            return { type: "tool_use", id, name: c.name, input: c.input };
          });
    const stopReason = turn.kind === "text" ? "end_turn" : "tool_use";
    const message = {
      id: `msg_${seq}`,
      type: "message",
      role: "assistant",
      model: body.model ?? "claude-sonnet-4-5",
      stop_reason: null as string | null,
      stop_sequence: null,
      usage: { input_tokens: 100, output_tokens: 1 },
    };
    if (!body.stream) return json(200, { ...message, content, stop_reason: stopReason, usage: { input_tokens: 100, output_tokens: 20 } });
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    const send = (event: string, data: unknown): void => {
      res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
    };
    send("message_start", { type: "message_start", message: { ...message, content: [] } });
    content.forEach((block, index) => {
      if (block.type === "text") {
        send("content_block_start", { type: "content_block_start", index, content_block: { type: "text", text: "" } });
        send("content_block_delta", { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } });
      } else {
        send("content_block_start", { type: "content_block_start", index, content_block: { type: "tool_use", id: block.id, name: block.name, input: {} } });
        send("content_block_delta", { type: "content_block_delta", index, delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) } });
      }
      send("content_block_stop", { type: "content_block_stop", index });
    });
    send("message_delta", { type: "message_delta", delta: { stop_reason: stopReason, stop_sequence: null }, usage: { output_tokens: 20 } });
    send("message_stop", { type: "message_stop" });
    res.end();
  }
}

// ---- request-content helpers (the script routes on content, never on call order) ----

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
function stepOf(body: MessagesRequest, token: string): { step: number; lastResult: string } {
  let promptIdx = -1;
  body.messages.forEach((m, i) => {
    if (m.role === "user" && !hasToolResult(m.content) && textOf(m.content).includes(token)) promptIdx = i;
  });
  const rest = body.messages.slice(promptIdx + 1);
  const step = rest.filter((m) => m.role === "assistant" && Array.isArray(m.content) && m.content.some((b: { type?: string }) => b.type === "tool_use")).length;
  const last = body.messages[body.messages.length - 1];
  return { step, lastResult: last && last.role === "user" && hasToolResult(last.content) ? textOf(last.content) : "" };
}

function builtinTemplates(): AgentTemplate[] {
  return fs
    .readdirSync(BUILTINS_DIR)
    .filter((f) => f.endsWith(".md"))
    .sort()
    .map((f) => {
      const raw = fs.readFileSync(path.join(BUILTINS_DIR, f), "utf8");
      const m = /^---\n([\s\S]*?)\n---\n([\s\S]*)$/.exec(raw);
      assert.ok(m, `${f} has frontmatter`);
      const fm = new Map<string, string>();
      for (const line of m[1]!.split("\n")) {
        const i = line.indexOf(":");
        if (i > 0) fm.set(line.slice(0, i).trim(), line.slice(i + 1).trim());
      }
      const tools = fm.get("tools");
      return {
        name: fm.get("name") ?? f,
        description: fm.get("description") ?? "",
        prompt_body: m[2]!,
        tools: tools ? tools.split(",").map((t) => t.trim()).filter(Boolean) : null,
        model: null,
      } as AgentTemplate;
    });
}

// ---- the scenario driver ----

interface HandlerCall {
  at: number;
  args: Record<string, unknown>;
  toolUseId: unknown;
  extraKeys: string[];
  metaKeys: string[];
  hasSignal: boolean;
}
interface HookRecord {
  /** Which registered matcher fired: the production "Bash" one or the "mcp__uzi_cmd__bash" one. */
  tag: "bash-matcher" | "mcp-matcher";
  at: number;
  tool_name: string;
  tool_use_id: string;
  agent_id: unknown;
  command: unknown;
  denied: boolean;
}
interface FakeObservation {
  at: number;
  role: "lead" | "sub";
  step: number;
  lastResult: string;
}
interface ScenarioResult {
  name: string;
  t0: number;
  calls: HandlerCall[];
  hooks: HookRecord[];
  observed: FakeObservation[];
  offered: { lead?: string[]; sub?: string[] };
  nativeRan: boolean;
  issuedIds: string[];
  requestCount: () => number;
  queryError?: string;
  queryEndAt?: number;
  initVersion?: string;
  extra: Record<string, unknown>;
}

interface Scenario {
  name: string;
  /** Agents for options.agents, already shaped by production shapeGuardedGrants. */
  agents?: Record<string, AgentDefinition>;
  serverTimeout: number;
  /** The scripted model. `ctx.native` is the marker path a native Bash would create. */
  script: (ctx: { role: "lead" | "sub"; step: number; native: string }) => Turn;
  /** The sentinel handler's behaviour; it must never run the command. */
  behave: (call: HandlerCall, r: ScenarioResult, extra: unknown) => Promise<string>;
  /** Abort the query once this resolves (turn-abort case). */
  abortAfterHandler?: boolean;
  /** Control scenario: no guardedCommand, no sentinel server, native Bash runs (calibrates the marker). */
  control?: boolean;
}

const DENY_MARK = "DENYME";
const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

async function runScenario(sc: Scenario, roots: string[]): Promise<ScenarioResult> {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m0-"));
  roots.push(root);
  const home = path.join(root, "home");
  const worktree = path.join(root, "worktree");
  const tmp = path.join(root, "tmp");
  for (const d of [home, worktree, tmp]) fs.mkdirSync(d, { recursive: true });
  const native = path.join(root, "NATIVE_RAN");
  const r: ScenarioResult = {
    name: sc.name,
    t0: Date.now(),
    calls: [],
    hooks: [],
    observed: [],
    offered: {},
    nativeRan: false,
    issuedIds: [],
    requestCount: () => 0,
    extra: {},
  };
  const token = `SCENARIO:${sc.name}`;
  const fake = new MultiToolFake((body) => {
    const tools = (body.tools ?? []).map((t) => t.name);
    if (tools.length === 0) return { kind: "text", text: "ok" };
    const firstText = textOf(body.messages[0]?.content);
    const subToken = /SUBTOKEN:\S+/.exec(firstText)?.[0];
    const role = subToken ? "sub" : "lead";
    r.offered[role] ??= tools;
    const { step, lastResult } = stepOf(body, subToken ?? token);
    r.observed.push({ at: Date.now(), role, step, lastResult });
    return sc.script({ role, step, native });
  });
  const baseUrl = await fake.listen();
  r.requestCount = () => fake.bodies.length;

  let handlerCalled: () => void = () => {};
  const handlerSeen = new Promise<void>((res) => (handlerCalled = res));
  const server = createSdkMcpServer({
    name: GUARDED_COMMAND_SERVER,
    alwaysLoad: true,
    timeout: sc.serverTimeout,
    tools: [
      tool(
        "bash",
        "Run a shell command (inert M0 sentinel: records the call and returns text).",
        {
          command: z.string(),
          timeout: z.number().optional(),
          description: z.string().optional(),
          run_in_background: z.boolean().optional(),
        },
        async (args, extra) => {
          const ex = (extra ?? {}) as { _meta?: Record<string, unknown>; signal?: unknown };
          const call: HandlerCall = {
            at: Date.now(),
            args: args as Record<string, unknown>,
            toolUseId: ex._meta?.["claudecode/toolUseId"],
            extraKeys: Object.keys(ex),
            metaKeys: Object.keys(ex._meta ?? {}),
            hasSignal: ex.signal !== undefined,
          };
          r.calls.push(call);
          handlerCalled();
          const text = await sc.behave(call, r, extra);
          return { content: [{ type: "text" as const, text }] };
        },
      ),
    ],
  });

  const makeHook = (tag: HookRecord["tag"]): HookCallback => async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse") return {};
    const ti = (input.tool_input ?? {}) as { command?: unknown };
    const denied = typeof ti.command === "string" && ti.command.includes(DENY_MARK);
    r.hooks.push({
      tag,
      at: Date.now(),
      tool_name: input.tool_name,
      tool_use_id: input.tool_use_id,
      agent_id: (input as { agent_id?: unknown }).agent_id,
      command: ti.command,
      denied,
    });
    // Only the mcp-matched hook may deny, so item 3 proves that hook's deny (not the
    // Bash-matched one's) is what blocks the handler.
    if (denied && tag === "mcp-matcher") {
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: "m0 deny" } };
    }
    return {};
  };

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
    agents: sc.agents ?? {},
    mcpServers: sc.control ? {} : { [GUARDED_COMMAND_SERVER]: server },
    // The two production matcher strings, registered separately so each firing is attributable.
    preToolUse: [
      { matcher: "Bash", hooks: [makeHook("bash-matcher")] },
      { matcher: GUARDED_COMMAND_TOOL, hooks: [makeHook("mcp-matcher")] },
    ],
    model: "claude-sonnet-4-5",
    ...(sc.control ? {} : { guardedCommand: true as const }),
  };
  const controller = new AbortController();
  const options: Options = { ...buildSdkOptions(cfg), abortController: controller };
  // The skills plugin needs a materialized plugin dir; irrelevant to routing, so leave it out.
  delete options.plugins;
  const budget = setTimeout(() => controller.abort(), DRIVE_BUDGET_MS);
  if (sc.abortAfterHandler) {
    void handlerSeen.then(async () => {
      await sleep(500);
      r.extra["abortRequestedAt"] = Date.now();
      controller.abort();
    });
  }
  try {
    for await (const m of query({ prompt: token, options })) {
      if (m.type === "system" && m.subtype === "init") r.initVersion = (m as { claude_code_version?: string }).claude_code_version;
    }
  } catch (err) {
    r.queryError = String(err);
  } finally {
    clearTimeout(budget);
  }
  r.queryEndAt = Date.now();
  if (sc.abortAfterHandler) await sleep(1500);
  r.nativeRan = fs.existsSync(native);
  r.issuedIds = [...fake.issuedIds];
  await fake.close();
  return r;
}

// ---- scenarios ----

const okBehave = async (call: HandlerCall): Promise<string> => `SENTINEL-OK ${JSON.stringify(call.args.command)}`;

function leadSubAgentCall(subType: string, name: string): Turn {
  return {
    kind: "tools",
    calls: [{ name: "Agent", input: { description: "m0 sub", prompt: `SUBTOKEN:${name} run your commands`, subagent_type: subType, run_in_background: false } }],
  };
}

describe("M0 real-CLI proof: Bash routes to the guarded sentinel (issue #2471)", () => {
  const roots: string[] = [];
  const out = new Map<string, ScenarioResult>();

  const { subagents } = assembleAgents(builtinTemplates().filter((t) => t.name !== "lead"));
  const shapedRoster = shapeGuardedGrants(subagents).agents;
  const planRoster = shapeGuardedGrants(planTurnSubagents(subagents).subagents).agents;
  const pick = (roster: Record<string, AgentDefinition>, ...names: string[]): Record<string, AgentDefinition> =>
    Object.fromEntries(names.map((n) => [n, roster[n]!]));

  const subScript =
    (subType: string, name: string, withDeny: boolean) =>
    ({ role, step, native }: { role: "lead" | "sub"; step: number; native: string }): Turn => {
      if (role === "lead") return step === 0 ? leadSubAgentCall(subType, name) : { kind: "text", text: "lead done" };
      if (step === 0) return { kind: "tools", calls: [{ name: "Bash", input: { command: `touch '${native}'; echo SUB-A`, description: "sub a" } }] };
      if (step === 1 && withDeny) return { kind: "tools", calls: [{ name: "Bash", input: { command: `echo ${DENY_MARK}-SUB`, description: "sub deny" } }] };
      return { kind: "text", text: "sub done" };
    };

  const scenarios: Scenario[] = [
    {
      name: "LEAD",
      serverTimeout: 120_000,
      behave: okBehave,
      script: ({ step, native }) => {
        if (step === 0) {
          return {
            kind: "tools",
            calls: [
              { name: "Bash", input: { command: `touch '${native}'; echo LEAD-A`, description: "lead a" } },
              { name: "Bash", input: { command: "echo LEAD-B", description: "lead b", run_in_background: true } },
            ],
          };
        }
        if (step === 1) return { kind: "tools", calls: [{ name: "Bash", input: { command: `echo ${DENY_MARK}-LEAD`, description: "lead deny" } }] };
        return { kind: "text", text: "lead done" };
      },
    },
    { name: "INHERIT", agents: pick(shapedRoster, "coder"), serverTimeout: 120_000, behave: okBehave, script: subScript("coder", "INHERIT", false) },
    { name: "EXPLICIT", agents: pick(shapedRoster, "reviewer"), serverTimeout: 120_000, behave: okBehave, script: subScript("reviewer", "EXPLICIT", true) },
    { name: "PLAN", agents: pick(planRoster, "architect"), serverTimeout: 120_000, behave: okBehave, script: subScript("architect", "PLAN", false) },
    {
      name: "ABORT",
      serverTimeout: 120_000,
      abortAfterHandler: true,
      // Any request after the aborted call (step >= 1) tries to run another command; it must never execute.
      script: ({ step }) => ({ kind: "tools", calls: [{ name: "Bash", input: { command: step === 0 ? "sleep 1000" : "echo POSTABORT" } }] }),
      behave: async (_call, r, extra) => {
        const sig = (extra as { signal?: AbortSignal } | undefined)?.signal;
        sig?.addEventListener("abort", () => {
          r.extra["extraSignalAbortedAt"] = Date.now();
        });
        return new Promise<string>(() => {});
      },
    },
    {
      name: "CONTROL",
      control: true,
      serverTimeout: 120_000,
      behave: okBehave,
      script: ({ step, native }) =>
        step === 0 ? { kind: "tools", calls: [{ name: "Bash", input: { command: `touch '${native}'; echo CONTROL-A`, description: "control" } }] } : { kind: "text", text: "control done" },
    },
    {
      name: "TIMEOUT_CUT",
      serverTimeout: 3000,
      script: ({ step }) => (step === 0 ? { kind: "tools", calls: [{ name: "Bash", input: { command: "slow" } }] } : { kind: "text", text: "after" }),
      behave: async (_call, r) => {
        await sleep(5000);
        r.extra["handlerCompletedAt"] = Date.now();
        return "LATE-RESULT-MARKER";
      },
    },
    {
      name: "TIMEOUT_LONG",
      serverTimeout: 3_660_000,
      script: ({ step }) => (step === 0 ? { kind: "tools", calls: [{ name: "Bash", input: { command: "slow" } }] } : { kind: "text", text: "after" }),
      behave: async (_call, r) => {
        await sleep(15_000);
        r.extra["handlerCompletedAt"] = Date.now();
        return "LONG-RESULT-MARKER";
      },
    },
    {
      name: "CJK",
      serverTimeout: 120_000,
      script: ({ step }) => (step === 0 ? { kind: "tools", calls: [{ name: "Bash", input: { command: "big" } }] } : { kind: "text", text: "after" }),
      behave: async () => "漢".repeat(30_000),
    },
  ];

  before(async () => {
    const settled = await Promise.allSettled(scenarios.map((sc) => runScenario(sc, roots)));
    settled.forEach((s, i) => {
      if (s.status === "fulfilled") out.set(scenarios[i]!.name, s.value);
      else console.log(`M0RESULT ${JSON.stringify({ item: "driver-failure", scenario: scenarios[i]!.name, error: String(s.reason) })}`);
    });
  });

  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  const get = (name: string): ScenarioResult => {
    const r = out.get(name);
    assert.ok(r, `scenario ${name} failed to run`);
    return r;
  };
  const diag = (r: ScenarioResult): string =>
    `\nscenario ${r.name}: queryError=${r.queryError} calls=${JSON.stringify(r.calls.map((c) => c.args))} hooks=${JSON.stringify(r.hooks)} observed=${JSON.stringify(r.observed.map((o) => [o.role, o.step, o.lastResult.slice(0, 120)]))}`;
  const nonDenied = (r: ScenarioResult): HookRecord[] => r.hooks.filter((h) => !h.denied && h.tag === "mcp-matcher");
  const mcpHooks = (r: ScenarioResult): HookRecord[] => r.hooks.filter((h) => h.tag === "mcp-matcher");
  const bashTagHooks = (r: ScenarioResult): HookRecord[] => r.hooks.filter((h) => h.tag === "bash-matcher");

  it("item 1: a model Bash tool_use reaches the sentinel on the lead, an inherit-all subagent and an explicit-grant subagent", { timeout: 120_000 }, () => {
    const lead = get("LEAD");
    const inherit = get("INHERIT");
    const explicit = get("EXPLICIT");
    result("1-cli", { sdkInit: lead.initVersion });
    // A bump of either pin invalidates this proof: re-run M0 for the new SDK before changing the pins.
    const sdkPkg = JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "..", "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"), "utf8")) as { version: string };
    assert.equal(sdkPkg.version, "0.3.292", "installed SDK package version changed: re-run M0 for the new SDK");
    assert.equal(lead.initVersion, "2.1.292", "bundled CLI version changed: re-run M0 for the new SDK");
    for (const r of [lead, inherit, explicit]) {
      result("1-offered", {
        scenario: r.name,
        leadOffered: r.offered.lead,
        subOffered: r.offered.sub,
        nativeBashOfferedToLead: r.offered.lead?.includes("Bash"),
        nativeBashOfferedToSub: r.offered.sub?.includes("Bash"),
      });
    }
    assert.ok(lead.calls.some((c) => String(c.args.command).includes("LEAD-A")), `lead Bash call never reached the sentinel${diag(lead)}`);
    assert.ok(inherit.calls.some((c) => String(c.args.command).includes("SUB-A")), `inherit-all subagent Bash never reached the sentinel${diag(inherit)}`);
    assert.ok(explicit.calls.some((c) => String(c.args.command).includes("SUB-A")), `explicit-grant subagent Bash never reached the sentinel${diag(explicit)}`);
    assert.ok(lead.offered.lead?.includes(GUARDED_COMMAND_TOOL), `mapped tool not offered to the lead: ${lead.offered.lead}`);
    assert.ok(inherit.offered.sub?.includes(GUARDED_COMMAND_TOOL), `mapped tool not offered to the inherit-all subagent: ${inherit.offered.sub}`);
    assert.ok(explicit.offered.sub?.includes(GUARDED_COMMAND_TOOL), `mapped tool not offered to the explicit subagent: ${explicit.offered.sub}`);
    for (const [who, tools] of [["lead", lead.offered.lead], ["inherit-all subagent", inherit.offered.sub], ["explicit subagent", explicit.offered.sub]] as const) {
      assert.ok(tools, `${who} made no request`);
      for (const forbidden of ["Bash", "Monitor"]) assert.ok(!tools.includes(forbidden), `${forbidden} offered to the ${who}: ${tools.join(",")}`);
    }
  });

  it("item 2: native Bash never executes (the marker file stays absent)", { timeout: 120_000 }, () => {
    const names = ["LEAD", "INHERIT", "EXPLICIT", "PLAN"];
    result("2", { ...Object.fromEntries(names.map((n) => [n, { nativeRan: get(n).nativeRan }])), CONTROL: { nativeRan: get("CONTROL").nativeRan, handlerCalls: get("CONTROL").calls.length } });
    // Calibration: with the guard off, the same scripted native Bash creates the marker, so its absence below means something.
    assert.equal(get("CONTROL").nativeRan, true, `control: native Bash did not create the marker, so marker absence proves nothing${diag(get("CONTROL"))}`);
    assert.equal(get("CONTROL").calls.length, 0, "control: the sentinel handler was reached without routing");
    for (const n of names) assert.equal(get(n).nativeRan, false, `native Bash created the marker in ${n}${diag(get(n))}`);
  });

  it("item 3: the mcp__uzi_cmd__bash-matched PreToolUse fires on the lead and subagent paths and a deny blocks the handler", { timeout: 120_000 }, () => {
    const lead = get("LEAD");
    const explicit = get("EXPLICIT");
    const perId = (r: ScenarioResult, tag: HookRecord["tag"]): Record<string, number> => {
      const counts: Record<string, number> = {};
      for (const h of r.hooks.filter((x) => x.tag === tag)) counts[h.tool_use_id] = (counts[h.tool_use_id] ?? 0) + 1;
      return counts;
    };
    result("3", {
      leadMcpMatcherToolNames: mcpHooks(lead).map((h) => h.tool_name),
      subMcpMatcherToolNames: mcpHooks(explicit).map((h) => h.tool_name),
      leadBashMatcherFired: bashTagHooks(lead).length > 0,
      subBashMatcherFired: bashTagHooks(explicit).length > 0,
      leadBashMatcherToolNames: bashTagHooks(lead).map((h) => h.tool_name),
      subBashMatcherToolNames: bashTagHooks(explicit).map((h) => h.tool_name),
      leadMcpRecordsPerToolUseId: perId(lead, "mcp-matcher"),
      leadBashRecordsPerToolUseId: perId(lead, "bash-matcher"),
      subMcpRecordsPerToolUseId: perId(explicit, "mcp-matcher"),
      subBashRecordsPerToolUseId: perId(explicit, "bash-matcher"),
      leadDeniedHandlerCalls: lead.calls.filter((c) => String(c.args.command).includes(DENY_MARK)).length,
      subDeniedHandlerCalls: explicit.calls.filter((c) => String(c.args.command).includes(DENY_MARK)).length,
      leadDenyResult: lead.observed.filter((o) => o.step === 2).map((o) => o.lastResult.slice(0, 160)),
      subDenyResult: explicit.observed.filter((o) => o.role === "sub" && o.step === 2).map((o) => o.lastResult.slice(0, 160)),
    });
    assert.ok(nonDenied(lead).some((h) => h.agent_id === undefined && String(h.command).includes("LEAD-A")), `no lead mcp-matcher hook record${diag(lead)}`);
    assert.ok(nonDenied(explicit).some((h) => typeof h.agent_id === "string" && String(h.command).includes("SUB-A")), `no subagent mcp-matcher hook record${diag(explicit)}`);
    for (const h of [...mcpHooks(lead), ...mcpHooks(explicit)]) assert.equal(h.tool_name, GUARDED_COMMAND_TOOL);
    // Pinned invariant: the Bash matcher also fires for a routed call, but reports the MAPPED
    // tool name, so a hook keyed on tool_name === "Bash" (buildPreToolUseHook) screens nothing.
    // Production must screen the mapped name (guarded hook and handler).
    for (const r of [lead, explicit]) {
      assert.ok(bashTagHooks(r).length > 0, `the Bash-matched hook never fired in ${r.name}${diag(r)}`);
      for (const h of bashTagHooks(r)) assert.equal(h.tool_name, GUARDED_COMMAND_TOOL, `Bash-matched hook saw ${String(h.tool_name)}`);
    }
    for (const r of [lead, explicit]) {
      assert.ok(mcpHooks(r).some((h) => h.denied), `the deny command never reached the mcp-matched hook in ${r.name}${diag(r)}`);
      assert.equal(r.calls.filter((c) => String(c.args.command).includes(DENY_MARK)).length, 0, `a denied call reached the handler in ${r.name}${diag(r)}`);
    }
  });

  it("item 4: handler toolUseId equals the hook tool_use_id for parallel lead calls and for a subagent call, with agent_id recorded", { timeout: 120_000 }, () => {
    const lead = get("LEAD");
    const explicit = get("EXPLICIT");
    const leadCalls = lead.calls.filter((c) => String(c.args.command).includes("LEAD-A") || String(c.args.command).includes("LEAD-B"));
    assert.equal(leadCalls.length, 2, `expected two parallel lead calls${diag(lead)}`);
    const ids = leadCalls.map((c) => c.toolUseId);
    result("4", {
      leadToolUseIds: ids,
      leadHookIds: nonDenied(lead).map((h) => h.tool_use_id),
      fakeIssuedIds: lead.issuedIds,
      handlerExtraKeys: leadCalls[0]!.extraKeys,
      handlerMetaKeys: leadCalls[0]!.metaKeys,
      subToolUseId: explicit.calls.find((c) => String(c.args.command).includes("SUB-A"))?.toolUseId,
      subHookAgentId: nonDenied(explicit).map((h) => h.agent_id),
    });
    for (const id of ids) assert.equal(typeof id, "string", `handler _meta has no string toolUseId${diag(lead)}`);
    for (const r of [lead, explicit]) {
      for (const c of r.calls) assert.ok(r.issuedIds.includes(c.toolUseId as string), `handler toolUseId ${String(c.toolUseId)} was not issued by the fake in ${r.name}${diag(r)}`);
    }
    assert.notEqual(ids[0], ids[1], "parallel calls share a toolUseId");
    for (const c of leadCalls) {
      const h = mcpHooks(lead).filter((x) => x.tool_use_id === c.toolUseId);
      assert.equal(h.length, 1, `no unique hook record for ${String(c.toolUseId)}${diag(lead)}`);
      assert.equal(h[0]!.command, c.args.command, "hook and handler disagree on the command");
      assert.equal(h[0]!.agent_id, undefined, "lead hook record carries an agent_id");
    }
    const subCall = explicit.calls.find((c) => String(c.args.command).includes("SUB-A"));
    assert.ok(subCall, `no subagent handler call${diag(explicit)}`);
    const subHooks = mcpHooks(explicit).filter((x) => x.tool_use_id === subCall.toolUseId);
    assert.equal(subHooks.length, 1, `no unique hook record for the subagent call${diag(explicit)}`);
    assert.equal(typeof subHooks[0]!.agent_id, "string", "subagent hook record has no agent_id");
  });

  it("item 5: the plan-turn roster reaches the CLI with mapped names only", { timeout: 120_000 }, () => {
    const plan = get("PLAN");
    const tools = plan.offered.sub;
    result("5", { planSubOffered: tools, planRosterArchitectTools: planRoster["architect"]?.tools });
    assert.ok(tools, `the plan-turn subagent never made a request${diag(plan)}`);
    assert.ok(tools.includes(GUARDED_COMMAND_TOOL), `mapped tool missing: ${tools}`);
    for (const forbidden of ["Bash", "Edit", "Write"]) assert.ok(!tools.includes(forbidden), `${forbidden} offered to the plan-turn subagent: ${tools}`);
    assert.ok(plan.calls.some((c) => String(c.args.command).includes("SUB-A")), `plan-turn subagent Bash never reached the sentinel${diag(plan)}`);
  });

  it("item 6: aborting the query with the handler pending settles it promptly and nothing is retried", { timeout: 120_000 }, () => {
    const r = get("ABORT");
    const requestedAt = r.extra["abortRequestedAt"] as number | undefined;
    assert.ok(requestedAt, `the handler was never called, so abort was never requested${diag(r)}`);
    const settleMs = (r.queryEndAt ?? 0) - requestedAt;
    result("6", {
      settleMs,
      queryError: r.queryError,
      handlerCalls: r.calls.length,
      extraHasSignal: r.calls[0]?.hasSignal,
      extraSignalAbortedAfterMs: r.extra["extraSignalAbortedAt"] ? (r.extra["extraSignalAbortedAt"] as number) - requestedAt : null,
      fakeRequests: r.requestCount(),
      postAbortIssuedIds: r.issuedIds.length - 1,
      postAbortHookRecords: r.hooks.filter((h) => String(h.command).includes("POSTABORT")).length,
      postAbortHandlerCalls: r.calls.filter((c) => String(c.args.command).includes("POSTABORT")).length,
      // After the abort the CLI keeps querying the model until torn down: the first follow-up
      // carries an interruption tool_result, later ones carry rejections; none runs a tool.
      followUpCount: r.observed.filter((o) => o.step >= 1).length,
      followUps: r.observed.filter((o) => o.step >= 1).slice(0, 3).map((o) => ({ afterAbortMs: o.at - requestedAt, result: o.lastResult.slice(0, 120) })),
    });
    assert.ok(settleMs < 10_000, `the query took ${settleMs} ms to settle after abort${diag(r)}`);
    assert.equal(r.calls.length, 1, `the tool call was retried${diag(r)}`);
    assert.equal(r.calls[0]?.hasSignal, true, `the handler extra carried no signal${diag(r)}`);
    assert.ok(r.extra["extraSignalAbortedAt"] !== undefined, `the handler extra.signal never aborted after the query abort${diag(r)}`);
    // The scripted follow-up carries a POSTABORT Bash call: it must never reach a hook or the handler.
    assert.ok(![...r.hooks, ...r.calls].some((x) => JSON.stringify("command" in x ? x.command : x.args).includes("POSTABORT")), `a POSTABORT command reached a hook or the handler${diag(r)}`);
    // The scripted model keeps answering follow-ups with a POSTABORT tool_use, so the CLI may keep
    // requesting until its process is torn down. Observed: the first follow-up carries the
    // interruption result and every later one carries a rejection of the new tool_use; none carries a
    // real handler result, and (asserted above) no POSTABORT command reached a hook or the handler.
    const followUps = r.observed.filter((o) => o.step >= 1);
    assert.ok(followUps.length >= 1, `no follow-up request was observed${diag(r)}`);
    assert.match(followUps[0]!.lastResult, /interrupted before a result was received/, `first follow-up carried a non-interruption result${diag(r)}`);
    for (const f of followUps.slice(1)) assert.match(f.lastResult, /doesn't want to take this action/, `a later follow-up carried a non-rejection result${diag(r)}`);
    for (const f of followUps) assert.ok(!f.lastResult.includes("SENTINEL-OK"), `a follow-up carried a real handler result${diag(r)}`);
  });

  it("item 7: the per-server timeout governs - 3000 ms cuts a 5 s handler, a max-plus-grace timeout lets a 15 s handler finish", { timeout: 120_000 }, () => {
    const cut = get("TIMEOUT_CUT");
    const long = get("TIMEOUT_LONG");
    const cutCall = cut.calls[0];
    const cutObs = cut.observed.find((o) => o.step === 1);
    const longCall = long.calls[0];
    const longObs = long.observed.find((o) => o.step === 1);
    result("7", {
      cutResultAtModelAfterMs: cutObs && cutCall ? cutObs.at - cutCall.at : null,
      cutResultText: cutObs?.lastResult.slice(0, 300),
      cutHandlerCompleted: cut.extra["handlerCompletedAt"] !== undefined,
      longResultAtModelAfterMs: longObs && longCall ? longObs.at - longCall.at : null,
      longResultText: longObs?.lastResult.slice(0, 300),
      longHandlerCompleted: long.extra["handlerCompletedAt"] !== undefined,
    });
    assert.ok(cutCall && cutObs, `timeout-cut: model never got a result${diag(cut)}`);
    const cutMs = cutObs.at - cutCall.at;
    assert.ok(cutMs >= 2500 && cutMs < 4800, `timeout-cut: result reached the model after ${cutMs} ms, expected ~3000${diag(cut)}`);
    assert.ok(!cutObs.lastResult.includes("LATE-RESULT-MARKER"), "timeout-cut: the late handler result reached the model");
    assert.ok(longCall && longObs, `long: model never got a result${diag(long)}`);
    assert.ok(longObs.lastResult.includes("LONG-RESULT-MARKER"), `long: handler result did not reach the model${diag(long)}`);
    assert.ok(longObs.at - longCall.at >= 14_500, "long: result arrived before the handler finished");
  });

  it("item 8: run_in_background true in the model's Bash input reaches the handler args", { timeout: 120_000 }, () => {
    const lead = get("LEAD");
    const bg = lead.calls.find((c) => String(c.args.command).includes("LEAD-B"));
    result("8", { args: bg?.args });
    assert.ok(bg, `the LEAD-B call never reached the handler${diag(lead)}`);
    assert.equal(bg.args.run_in_background, true);
    const plain = lead.calls.find((c) => String(c.args.command).includes("LEAD-A"));
    assert.ok(plain && plain.args.run_in_background !== true);
  });

  it("item 9 (informational): a 30000-character CJK handler result is recorded as delivered inline or truncated", { timeout: 120_000 }, () => {
    const r = get("CJK");
    const obs = r.observed.find((o) => o.step === 1);
    const text = obs?.lastResult ?? "";
    result("9", {
      resultChars: text.length,
      cjkChars: [...text].filter((c) => c === "漢").length,
      mentionsSavedTo: /saved to|Full output/i.test(text),
      head: text.slice(0, 200),
    });
    assert.ok(obs, `the CJK result never reached the model${diag(r)}`);
  });
});
