import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";

// PRD #4320 M2 (agent half): does CLAUDE_CODE_MAX_CONTEXT_TOKENS / CLAUDE_CODE_AUTO_COMPACT_WINDOW
// change the REAL bundled CLI's auto-compaction point for a model name it does not recognize
// (a gateway name such as `corp-sonnet`)? The CLI runs in provider-like mode against a loopback
// Messages API whose reported `usage.input_tokens` is a pure function of the request (PER_MESSAGE
// tokens per message in the history), so the point at which the CLI issues its summarization
// request is deterministic. Findings: .uzi/scratch/context-window-findings.md.

const DRIVE_BUDGET_MS = 90_000;
const PER_MESSAGE = 5_000; // reported input tokens per history message
const MAX_TOOL_TURNS = 60;
const SUBAGENT_TOKEN = "SUBAGENT-TASK-9d41";
const LEAD_MODEL = "corp-sonnet";
const SUB_MODEL = "corp-haiku";

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((b: { type?: string; text?: string; content?: unknown }) =>
      b.type === "tool_result" ? textOf(b.content) : typeof b.text === "string" ? b.text : "",
    )
    .join("\n");
}

interface Req {
  model?: string;
  stream?: boolean;
  tools?: Array<{ name: string }>;
  messages: Array<{ role: string; content: unknown }>;
}

/** One recorded /v1/messages call, reduced to what the assertions need. */
interface Call {
  model: string;
  agent: "lead" | "sub";
  compaction: boolean;
  messages: number;
  reported: number; // usage.input_tokens we returned
}

/** A loopback Messages API with a configurable usage figure and a lead/subagent/compaction router. */
class Gateway {
  readonly calls: Call[] = [];
  private readonly server: http.Server;
  private leadTurnsSinceCompaction = 0;
  private subTurns = 0;
  private subSinceCompaction = 0;
  private spawned = false;
  constructor(private readonly opts: { withSubagent: boolean; subTurns: number }) {
    this.server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => this.handle(req, res, Buffer.concat(chunks).toString("utf8")));
    });
  }
  async listen(): Promise<string> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    this.server.unref();
    return `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }
  async close(): Promise<void> {
    this.server.closeAllConnections();
    await new Promise<void>((resolve, reject) => this.server.close((e) => (e ? reject(e) : resolve())));
  }
  get compactions(): Call[] {
    return this.calls.filter((c) => c.compaction);
  }
  /** Reported tokens of the last non-compaction call before the first compaction, per agent. */
  tokensAtFirstCompaction(agent: "lead" | "sub"): number | undefined {
    const idx = this.calls.findIndex((c) => c.compaction && c.agent === agent);
    if (idx < 0) return undefined;
    for (let i = idx - 1; i >= 0; i--) if (this.calls[i]!.agent === agent && !this.calls[i]!.compaction) return this.calls[i]!.reported;
    return undefined;
  }
  private handle(req: http.IncomingMessage, res: http.ServerResponse, raw: string): void {
    const p = (req.url ?? "").split("?")[0];
    const json = (status: number, body: unknown): void => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.method === "POST" && p === "/v1/messages/count_tokens") return json(200, { input_tokens: 100 });
    if (req.method !== "POST" || p !== "/v1/messages") return json(404, { type: "error", error: { type: "not_found_error", message: "nf" } });
    const body = JSON.parse(raw) as Req;
    const tools = (body.tools ?? []).map((t) => t.name);
    const last = body.messages[body.messages.length - 1];
    const lastText = last ? textOf(last.content) : "";
    const isSub = body.model === SUB_MODEL;
    const compaction = /detailed summary of the conversation|summary of the conversation so far|CRITICAL: Respond with TEXT ONLY/i.test(lastText);
    const hasBash = tools.includes("Bash");
    const reported = PER_MESSAGE * body.messages.length;
    let turn: { text: string } | { tool: string; input: Record<string, unknown> };
    if (compaction) {
      turn = { text: "<analysis>a</analysis><summary>Work so far: ran many echo commands.</summary>" };
    } else if (!hasBash) {
      turn = { text: "ok" }; // side calls (titles etc.)
    } else if (isSub) {
      this.subTurns++;
      const subCompacted = this.calls.some((c) => c.agent === "sub" && c.compaction);
      if (subCompacted) this.subSinceCompaction++;
      turn =
        this.subTurns <= this.opts.subTurns && !(subCompacted && this.subSinceCompaction >= 2)
          ? { tool: "Bash", input: { command: "echo sub", description: "sub step" } }
          : { text: "subagent done" };
    } else if (this.opts.withSubagent && !this.spawned) {
      this.spawned = true;
      const agentTool = tools.includes("Agent") ? "Agent" : "Task";
      turn = { tool: agentTool, input: { description: "sub", prompt: `${SUBAGENT_TOKEN}: do the steps`, subagent_type: "helper" } };
    } else {
      const leadCompacted = this.calls.some((c) => c.agent === "lead" && c.compaction);
      if (leadCompacted) this.leadTurnsSinceCompaction++;
      const toolTurns = body.messages.filter((m) => m.role === "assistant" && Array.isArray(m.content) && m.content.some((b: { type?: string }) => b.type === "tool_use")).length;
      turn =
        (leadCompacted && this.leadTurnsSinceCompaction >= 2) || toolTurns >= MAX_TOOL_TURNS
          ? { text: "lead done" }
          : { tool: "Bash", input: { command: "echo lead", description: "lead step" } };
    }
    this.calls.push({ model: body.model ?? "", agent: isSub ? "sub" : "lead", compaction, messages: body.messages.length, reported });
    type Block = { type: "text"; text: string } | { type: "tool_use"; id: string; name: string; input: Record<string, unknown> };
    const content: Block[] =
      "text" in turn
        ? [{ type: "text", text: turn.text }]
        : [{ type: "tool_use", id: `toolu_${this.calls.length}`, name: turn.tool, input: turn.input }];
    const stopReason = "text" in turn ? "end_turn" : "tool_use";
    const usage = { input_tokens: reported, output_tokens: 20 };
    const message = { id: `msg_${this.calls.length}`, type: "message", role: "assistant", model: body.model ?? "x", stop_reason: null as string | null, stop_sequence: null, usage };
    if (!body.stream) return json(200, { ...message, content, stop_reason: stopReason });
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
    send("message_delta", { type: "message_delta", delta: { stop_reason: stopReason, stop_sequence: null }, usage });
    send("message_stop", { type: "message_stop" });
    res.end();
  }
}

interface Outcome {
  gw: Gateway;
  stderr: string;
  sysNotes: string[];
}

describe("context-window env keys vs a model name the CLI does not recognize (PRD #4320 M2)", () => {
  const roots: string[] = [];
  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  async function run(contextEnv: Record<string, string>, withSubagent: boolean, subTurns = 0): Promise<Outcome> {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-ctxwin-it-"));
    roots.push(root);
    const home = path.join(root, "home");
    const worktree = path.join(root, "worktree");
    const tmp = path.join(root, "tmp");
    for (const d of [path.join(home, ".claude"), worktree, tmp]) fs.mkdirSync(d, { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "settings.json"), "{}\n");
    const gw = new Gateway({ withSubagent, subTurns });
    const baseUrl = await gw.listen();
    let stderr = "";
    const sysNotes: string[] = [];
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), DRIVE_BUDGET_MS);
    const options: Options = {
      cwd: worktree,
      // Provider-like mode: base URL + bearer token, no OAuth token, gateway model names via the alias map.
      env: {
        PATH: process.env.PATH ?? "",
        HOME: home,
        TMPDIR: tmp,
        ANTHROPIC_BASE_URL: baseUrl,
        ANTHROPIC_AUTH_TOKEN: ["dummy", "token"].join("-"),
        ANTHROPIC_DEFAULT_SONNET_MODEL: LEAD_MODEL,
        ANTHROPIC_DEFAULT_HAIKU_MODEL: SUB_MODEL,
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
        DISABLE_TELEMETRY: "1",
        DISABLE_AUTOUPDATER: "1",
        ...contextEnv,
      },
      model: "sonnet",
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      settingSources: [],
      stderr: (d: string) => {
        stderr += d;
      },
      abortController: controller,
      ...(withSubagent
        ? { agents: { helper: { description: "runs a few shell steps", prompt: "Do exactly what the task says.", tools: ["Bash"], model: "haiku" as const } } }
        : {}),
    };
    try {
      for await (const m of query({ prompt: "run the steps", options })) {
        const mm = m as { type: string; subtype?: string };
        if (mm.type === "system" && mm.subtype !== "init") sysNotes.push(JSON.stringify(m).slice(0, 300));
      }
    } catch (err) {
      assert.fail(`query failed: ${String(err)}\ncalls: ${JSON.stringify(gw.calls)}\nstderr: ${stderr.slice(0, 1500)}`);
    } finally {
      clearTimeout(timer);
      await gw.close();
    }
    if (process.env.UZI_CTXWIN_DEBUG) {
      const seq = gw.calls.map((c) => `${c.agent}/${c.model}/${c.compaction ? "COMPACT" : "turn"}/${c.reported}`).join(" ");
      console.log(`DBG ${JSON.stringify(contextEnv)} sub=${withSubagent} :: ${seq} :: stderr=${JSON.stringify(stderr.slice(0, 500))}`);
    }
    return { gw, stderr, sysNotes };
  }

  const between = (n: number | undefined, lo: number, hi: number, what: string, o: Outcome): void => {
    assert.ok(n !== undefined && n >= lo && n <= hi, `${what}: first compaction at ${n}, expected ${lo}..${hi}\ncalls: ${o.gw.calls.map((c) => `${c.agent}/${c.model}/${c.compaction ? "COMPACT" : "turn"}/${c.reported}`).join(" ")}`);
  };

  // Observed at the pinned SDK 0.3.319 (CLI 2.1.319): the unknown-model default window is 200k and the
  // CLI compacts about 33k below the window, i.e. when the last reported input_tokens is ~170k.
  it("single model, no context env: assumes a 200k window and warns unrecognized_model", { timeout: 120_000 }, async () => {
    const o = await run({}, false);
    between(o.gw.tokensAtFirstCompaction("lead"), 160_001, 199_999, "baseline", o);
    assert.match(o.stderr, /unrecognized_model\] \{"model":"corp-sonnet"/);
  });

  it("single model: CLAUDE_CODE_MAX_CONTEXT_TOKENS=50000 moves the compaction point down to ~window minus the buffer", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_MAX_CONTEXT_TOKENS: "50000" }, false);
    between(o.gw.tokensAtFirstCompaction("lead"), 20_000, 30_000, "MAX_CONTEXT_TOKENS=50000", o);
  });

  it("single model: CLAUDE_CODE_MAX_CONTEXT_TOKENS above the default raises the compaction point", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_MAX_CONTEXT_TOKENS: "400000" }, false);
    between(o.gw.tokensAtFirstCompaction("lead"), 300_000, 399_999, "MAX_CONTEXT_TOKENS=400000", o);
  });

  it("single model: CLAUDE_CODE_AUTO_COMPACT_WINDOW=120000 moves the compaction point to ~window minus the buffer", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_AUTO_COMPACT_WINDOW: "120000" }, false);
    between(o.gw.tokensAtFirstCompaction("lead"), 80_000, 100_000, "AUTO_COMPACT_WINDOW=120000", o);
  });

  it("single model: CLAUDE_CODE_AUTO_COMPACT_WINDOW below 100000 is floored (50000 behaves like 100000)", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_AUTO_COMPACT_WINDOW: "50000" }, false);
    between(o.gw.tokensAtFirstCompaction("lead"), 60_001, 80_000, "AUTO_COMPACT_WINDOW=50000", o);
  });

  it("lead + subagent on different unrecognized models: no context env, both assume the same 200k default", { timeout: 120_000 }, async () => {
    const o = await run({}, true, 40);
    between(o.gw.tokensAtFirstCompaction("lead"), 160_001, 199_999, "lead", o);
    between(o.gw.tokensAtFirstCompaction("sub"), 160_001, 199_999, "sub", o);
    assert.match(o.stderr, /unrecognized_model\] \{"model":"corp-haiku"/);
  });

  it("lead + subagent: one process-wide CLAUDE_CODE_MAX_CONTEXT_TOKENS applies to both models", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_MAX_CONTEXT_TOKENS: "50000" }, true, 40);
    between(o.gw.tokensAtFirstCompaction("lead"), 20_000, 30_000, "lead", o);
    between(o.gw.tokensAtFirstCompaction("sub"), 20_000, 30_000, "sub", o);
  });

  it("lead + subagent: one process-wide CLAUDE_CODE_AUTO_COMPACT_WINDOW applies to both models", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_AUTO_COMPACT_WINDOW: "120000" }, true, 40);
    // The lead's history also carries the subagent hand-off, so its exact trigger varies by one turn
    // between runs (90k, 105k seen); both stay far below the 170k baseline and above the 50k floor case.
    between(o.gw.tokensAtFirstCompaction("lead"), 80_000, 120_000, "lead", o);
    between(o.gw.tokensAtFirstCompaction("sub"), 80_000, 120_000, "sub", o);
  });
});
