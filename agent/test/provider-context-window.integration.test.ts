import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";
import { buildSdkEnv, PROVIDER_FALLBACK_CONTEXT_WINDOW } from "../src/sdk-env.js";

// PRD #4320 M2 (agent half): does CLAUDE_CODE_MAX_CONTEXT_TOKENS / CLAUDE_CODE_AUTO_COMPACT_WINDOW
// change the REAL bundled CLI's auto-compaction point for a model name it does not recognize
// (a gateway name such as `corp-sonnet`)? The CLI runs in provider-like mode against a loopback
// Messages API whose reported `usage.input_tokens` is a pure function of the request (perMessage
// tokens per message in the history), so the point at which the CLI issues its summarization
// request is a function of the history length alone.
//
// Two different things are measured and kept apart:
//  - the RUNTIME default: what the CLI does with NO window override at all. Characterized at SDK
//    0.3.325 / CLI 2.1.325 (2026-11-22): it assumes 1M tokens for an unrecognized id and compacts near
//    1M. At 0.3.319 the same case compacted near 200k, so this one is a dependency-induced change.
//  - the uzi PRODUCTION policy: buildSdkEnv always sets CLAUDE_CODE_MAX_CONTEXT_TOKENS for a provider
//    credential, to the configured window or PROVIDER_FALLBACK_CONTEXT_WINDOW (200k) when none is
//    configured. For a model id the CLI does not recognize that is what keeps an unconfigured provider
//    compacting early; it is uzi's choice, not the CLI's. For ids the CLI recognizes (claude-sonnet-4-5,
//    us.anthropic.claude-sonnet-4-5) the measured CLI ignores both the fallback and an explicit window:
//    see the mapped-name cases below.
// The compaction point itself is chosen by the CLI and moves by a turn or two between runs, so every
// assertion is a MEASURED BAND: its lower bound is at least one reported-token step (2 * perMessage,
// the increment between consecutive lead calls) below the observed first compaction and its upper bound
// is the configured or runtime window (or at least one step above the observation), never an exact
// threshold. Findings: the PRD Decision Log in prds/4320-custom-providers-core-anthropic.md.

const DRIVE_BUDGET_MS = 90_000;
const PER_MESSAGE = 5_000; // default reported input tokens per history message
const MAX_TOOL_TURNS = 60; // default cap on lead tool turns
const SUBAGENT_TOKEN = "SUBAGENT-TASK-9d41";
const LEAD_MODEL = "corp-sonnet";
const SUB_MODEL = "corp-haiku";
// Carried in the subagent's system prompt, so a request is attributed to the subagent by identity
// and not by model name (the small/fast haiku slot also serves side calls for the lead).
const SUB_IDENTITY = "SUBAGENT-IDENTITY-7b3f";
// Lower edge of the 200k policy band: one 10k step below the 170k observed at perMessage 5_000.
const BASELINE_FLOOR = 160_000;
// The CLI's own window for a model id it does not recognize, measured with no override at all
// (SDK 0.3.325 / CLI 2.1.325). Not uzi policy: see UZI_FALLBACK_WINDOW.
const RUNTIME_DEFAULT_WINDOW = 1_000_000;
// Reported-token step per lead tool turn is 2 * perMessage; the runtime-default cases use 10_000 so
// the step (20k) is smaller than the ~33k buffer the CLI keeps below the window.
const RUNTIME_PER_MESSAGE = 10_000;
// What buildSdkEnv sets for a provider credential with no configured window (uzi policy).
const UZI_FALLBACK_WINDOW = PROVIDER_FALLBACK_CONTEXT_WINDOW;

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
  system?: string | Array<{ text?: string }>;
  stream?: boolean;
  tools?: Array<{ name: string }>;
  messages: Array<{ role: string; content: unknown }>;
}

/** One recorded /v1/messages call, reduced to what the assertions need. */
interface Call {
  model: string;
  agent: "lead" | "sub" | "side";
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
  constructor(private readonly opts: { withSubagent: boolean; subTurns: number; perMessage: number; maxToolTurns: number }) {
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
    const systemText = typeof body.system === "string" ? body.system : (body.system ?? []).map((b) => b.text ?? "").join("\n");
    const firstText = body.messages[0] ? textOf(body.messages[0].content) : "";
    const isSub = systemText.includes(SUB_IDENTITY) || firstText.includes(SUBAGENT_TOKEN);
    const compaction = /detailed summary of the conversation|summary of the conversation so far|CRITICAL: Respond with TEXT ONLY/i.test(lastText);
    const hasBash = tools.includes("Bash");
    const reported = this.opts.perMessage * body.messages.length;
    let turn: { text: string } | { tool: string; input: Record<string, unknown> };
    if (compaction) {
      turn = { text: "<analysis>a</analysis><summary>Work so far: ran many echo commands.</summary>" };
    } else if (!hasBash) {
      turn = { text: "ok" }; // side calls (titles etc.), attributed to neither agent
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
        (leadCompacted && this.leadTurnsSinceCompaction >= 2) || toolTurns >= this.opts.maxToolTurns
          ? { text: "lead done" }
          : { tool: "Bash", input: { command: "echo lead", description: "lead step" } };
    }
    this.calls.push({ model: body.model ?? "", agent: isSub ? "sub" : compaction || hasBash ? "lead" : "side", compaction, messages: body.messages.length, reported });
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

  async function run(
    contextEnv: Record<string, string>,
    withSubagent: boolean,
    subTurns = 0,
    gwOpts: { perMessage?: number; maxToolTurns?: number } = {},
  ): Promise<Outcome> {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-ctxwin-it-"));
    roots.push(root);
    const home = path.join(root, "home");
    const worktree = path.join(root, "worktree");
    const tmp = path.join(root, "tmp");
    for (const d of [path.join(home, ".claude"), worktree, tmp]) fs.mkdirSync(d, { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "settings.json"), "{}\n");
    const gw = new Gateway({ withSubagent, subTurns, perMessage: gwOpts.perMessage ?? PER_MESSAGE, maxToolTurns: gwOpts.maxToolTurns ?? MAX_TOOL_TURNS });
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
        ? { agents: { helper: { description: "runs a few shell steps", prompt: `Do exactly what the task says. ${SUB_IDENTITY}`, tools: ["Bash"], model: "haiku" as const } } }
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
      console.log(`DBG ${JSON.stringify(contextEnv)} sub=${withSubagent} pm=${gwOpts.perMessage ?? PER_MESSAGE} :: ${seq} :: stderr=${JSON.stringify(stderr.slice(0, 500))}`);
    }
    return { gw, stderr, sysNotes };
  }

  const trace = (o: Outcome): string => o.gw.calls.map((c) => `${c.agent}/${c.model}/${c.compaction ? "COMPACT" : "turn"}/${c.reported}`).join(" ");
  const firstCompaction = (o: Outcome, agent: "lead" | "sub", what: string): number => {
    const n = o.gw.tokensAtFirstCompaction(agent);
    assert.ok(n !== undefined, `${what}: ${agent} never compacted\ncalls: ${trace(o)}`);
    return n;
  };
  /** The first compaction is within [lo, hi] (exclusive lo, inclusive hi), with the calls shown on failure. */
  const within = (n: number, lo: number, hi: number, what: string, o: Outcome): void => {
    assert.ok(n > lo && n <= hi, `${what}: first compaction at ${n}, expected above ${lo} and at most ${hi}\ncalls: ${trace(o)}`);
  };

  // Characterized at SDK 0.3.325 / CLI 2.1.325 (2026-11-22). History: at 0.3.319 an unrecognized id
  // assumed 200k and compacted at ~170k; the target (measured at 0.3.325 / CLI 2.1.325; the CLI changelog dates the change to 2.1.323) assumes 1M. Observed first compaction (last reported
  // input_tokens before the summarization request) at perMessage 10_000: 970k to 980k.
  const RUNTIME_BAND_LO = RUNTIME_DEFAULT_WINDOW - 50_000; // >= one 20k step below the lowest observation
  it("runtime default (no override): a lead on an unrecognized model compacts near 1M and warns unrecognized_model", { timeout: 120_000 }, async () => {
    const o = await run({}, false, 0, { perMessage: RUNTIME_PER_MESSAGE });
    within(firstCompaction(o, "lead", "runtime default"), RUNTIME_BAND_LO, RUNTIME_DEFAULT_WINDOW, "runtime default", o);
    assert.match(o.stderr, /unrecognized_model\] \{"model":"corp-sonnet"/);
  });

  it("runtime default (no override): lead and subagent on different unrecognized models both compact near 1M", { timeout: 120_000 }, async () => {
    const o = await run({}, true, 60, { perMessage: RUNTIME_PER_MESSAGE });
    within(firstCompaction(o, "lead", "lead"), RUNTIME_BAND_LO, RUNTIME_DEFAULT_WINDOW, "lead", o);
    within(firstCompaction(o, "sub", "sub"), RUNTIME_BAND_LO, RUNTIME_DEFAULT_WINDOW, "sub", o);
    assert.match(o.stderr, /unrecognized_model\] \{"model":"corp-haiku"/);
  });

  // The context env for a mapped sonnet name, taken from the production buildSdkEnv. With a window it
  // is that window; without one it is the uzi fallback (PROVIDER_FALLBACK_CONTEXT_WINDOW).
  const prodEnv = (sonnet: string, window?: number): Record<string, string> => {
    const prod = buildSdkEnv(
      {
        kind: "provider",
        baseUrl: "http://127.0.0.1:1",
        authStyle: "bearer",
        key: "k",
        modelMap: { sonnet: { model: sonnet, ...(window === undefined ? {} : { context_window: window }) }, haiku: { model: SUB_MODEL } },
      },
      "/unused-home",
    );
    assert.equal(prod.CLAUDE_CODE_MAX_CONTEXT_TOKENS, String(window ?? PROVIDER_FALLBACK_CONTEXT_WINDOW));
    return { ANTHROPIC_DEFAULT_SONNET_MODEL: sonnet, CLAUDE_CODE_MAX_CONTEXT_TOKENS: prod.CLAUDE_CODE_MAX_CONTEXT_TOKENS! };
  };
  const maxLeadTokens = (o: Outcome): number => Math.max(0, ...o.gw.calls.filter((c) => c.agent === "lead" && !c.compaction).map((c) => c.reported));

  // Observed 170k for both lead and subagent at perMessage 5_000 (step 10k): inside (160k, 200k].
  it("uzi production policy (buildSdkEnv, no configured window, unknown name): the lead compacts within the 200k fallback", { timeout: 120_000 }, async () => {
    const o = await run(prodEnv(LEAD_MODEL), false);
    within(firstCompaction(o, "lead", "production no-window"), BASELINE_FLOOR, UZI_FALLBACK_WINDOW, "production no-window", o);
  });

  it("uzi production policy (buildSdkEnv, no configured window, unknown names): lead and subagent both compact within the 200k fallback", { timeout: 120_000 }, async () => {
    const o = await run(prodEnv(LEAD_MODEL), true, 40);
    within(firstCompaction(o, "lead", "lead"), BASELINE_FLOOR, UZI_FALLBACK_WINDOW, "lead", o);
    within(firstCompaction(o, "sub", "sub"), BASELINE_FLOOR, UZI_FALLBACK_WINDOW, "sub", o);
  });

  // Explicit overrides. Observed at perMessage 5_000 (step 10k): MAX=50000 -> 20k; MAX=400000 -> 370k;
  // AUTO_COMPACT=120000 -> 90k (105k seen at 0.3.319); AUTO_COMPACT=50000 -> 70k (floored to 100000).
  it("explicit CLAUDE_CODE_MAX_CONTEXT_TOKENS=50000 moves the compaction point down to ~window minus the buffer", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_MAX_CONTEXT_TOKENS: "50000" }, false);
    within(firstCompaction(o, "lead", "MAX_CONTEXT_TOKENS=50000"), 10_000, 50_000, "MAX_CONTEXT_TOKENS=50000", o);
  });

  it("explicit CLAUDE_CODE_MAX_CONTEXT_TOKENS=400000, below the runtime default, lowers the compaction point", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_MAX_CONTEXT_TOKENS: "400000" }, false);
    within(firstCompaction(o, "lead", "MAX_CONTEXT_TOKENS=400000"), 360_000, 400_000, "MAX_CONTEXT_TOKENS=400000", o);
  });

  it("explicit CLAUDE_CODE_AUTO_COMPACT_WINDOW=120000 moves the compaction point to ~window minus the buffer", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_AUTO_COMPACT_WINDOW: "120000" }, false);
    const n = firstCompaction(o, "lead", "AUTO_COMPACT_WINDOW=120000");
    assert.ok(n < BASELINE_FLOOR, `not below the policy band: ${n}\ncalls: ${trace(o)}`);
    within(n, 70_000, 120_000, "AUTO_COMPACT_WINDOW=120000", o);
  });

  it("explicit CLAUDE_CODE_AUTO_COMPACT_WINDOW below 100000 is floored (50000 behaves like 100000)", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_AUTO_COMPACT_WINDOW: "50000" }, false);
    // Unfloored, a 50000 window would compact near 17k; the floor keeps it above 50k and within 100k.
    within(firstCompaction(o, "lead", "AUTO_COMPACT_WINDOW=50000"), 50_000, 100_000, "AUTO_COMPACT_WINDOW=50000", o);
  });

  it("lead + subagent: one process-wide CLAUDE_CODE_MAX_CONTEXT_TOKENS applies to both models", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_MAX_CONTEXT_TOKENS: "50000" }, true, 40);
    for (const agent of ["lead", "sub"] as const) {
      const n = firstCompaction(o, agent, "MAX_CONTEXT_TOKENS=50000");
      assert.ok(n < BASELINE_FLOOR, `${agent} not below the policy band: ${n}\ncalls: ${trace(o)}`);
      within(n, 0, 50_000, agent, o);
    }
  });

  it("lead + subagent: one process-wide CLAUDE_CODE_AUTO_COMPACT_WINDOW applies to both models", { timeout: 120_000 }, async () => {
    const o = await run({ CLAUDE_CODE_AUTO_COMPACT_WINDOW: "120000" }, true, 40);
    // The exact trigger varies by a turn or two between runs (90k and 105k seen for the lead).
    for (const agent of ["lead", "sub"] as const) {
      const n = firstCompaction(o, agent, "AUTO_COMPACT_WINDOW=120000");
      assert.ok(n < BASELINE_FLOOR, `${agent} not below the policy band: ${n}\ncalls: ${trace(o)}`);
      within(n, 0, 120_000, agent, o);
    }
  });

  // Mapped names with a 50000 window from buildSdkEnv, observed at perMessage 5_000 (step 10k):
  //  - claude-corp-custom and corp-claude-x: enforced, first compaction at 20k. (claude-corp-custom was
  //    NOT enforced at 0.3.319, where it compacted near the 200k default.)
  //  - claude-sonnet-4-5 and us.anthropic.claude-sonnet-4-5: not enforced. The CLI recognizes them (no
  //    unrecognized_model warning) and never compacted up to the 60-turn cap (605k reported).
  // These record what was seen, not why; a CLI bump that changes either side reddens them and the
  // docs/CLI help/form warning must be revisited.
  const WINDOW = 50_000;
  for (const name of ["claude-corp-custom", "corp-claude-x"]) {
    it(`mapped name ${name} with a 50000 window from buildSdkEnv: the window is enforced`, { timeout: 120_000 }, async () => {
      const o = await run(prodEnv(name, WINDOW), false);
      assert.ok(o.gw.calls.some((c) => c.model === name), `the name reached the gateway\ncalls: ${trace(o)}`);
      within(firstCompaction(o, "lead", name), 10_000, WINDOW, name, o);
    });
  }

  for (const name of ["claude-sonnet-4-5", "us.anthropic.claude-sonnet-4-5"]) {
    it(`mapped name ${name} with a 50000 window from buildSdkEnv: the window is not honoured`, { timeout: 120_000 }, async () => {
      const o = await run(prodEnv(name, WINDOW), false);
      assert.ok(o.gw.calls.some((c) => c.model === name), `the name reached the gateway\ncalls: ${trace(o)}`);
      const max = maxLeadTokens(o);
      assert.ok(max > 500_000, `the lead only reached ${max} reported tokens, expected above 500000 (window ${WINDOW})\ncalls: ${trace(o)}`);
      const n = o.gw.tokensAtFirstCompaction("lead");
      assert.ok(n === undefined || n > WINDOW, `compaction at or below the configured window (first compaction at ${n})\ncalls: ${trace(o)}`);
    });

    it(`mapped name ${name} with no configured window from buildSdkEnv: the 200k fallback is not honoured`, { timeout: 120_000 }, async () => {
      const o = await run(prodEnv(name), false);
      assert.ok(o.gw.calls.some((c) => c.model === name), `the name reached the gateway\ncalls: ${trace(o)}`);
      const max = maxLeadTokens(o);
      assert.ok(max > 500_000, `the lead only reached ${max} reported tokens, expected above 500000 (fallback ${UZI_FALLBACK_WINDOW})\ncalls: ${trace(o)}`);
      const n = o.gw.tokensAtFirstCompaction("lead");
      assert.ok(n === undefined || n > UZI_FALLBACK_WINDOW, `compaction at or below the fallback (first compaction at ${n})\ncalls: ${trace(o)}`);
    });
  }
});
