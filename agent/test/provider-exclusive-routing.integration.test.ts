import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo, Socket } from "node:net";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";
import { buildSdkEnv, type SdkCredential } from "../src/sdk-env.js";
import { JudgeRunner } from "../src/judge-runner.js";
import type { WorkerClient } from "../src/client.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { ClaimResponse, ReviewRequest, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// PRD #4320 module 7 (activation-blocking): does the REAL bundled CLI, run with the provider-mode
// env uzi builds, send every request (main agent, Task subagent, haiku auxiliary calls) ONLY to the
// bound base URL? Redirect handling is NOT activation-blocking: the pinned CLI follows a gateway 307,
// an accepted risk (ADR-4320) whose observed behaviour the redirect tests below pin.
//
// The SDK `env` REPLACES the child environment (sdk-env.ts), so the CLI's environment is built from
// {...buildSdkEnv(providerCredential, home), HTTPS_PROXY, HTTP_PROXY, NO_PROXY} (the SDK may add its own
// entrypoint variables on top; the worker's process environment does not leak in). The proxy variables are
// TEST-ONLY: they point at a loopback recorder that records and refuses every CONNECT and absolute-URI
// request, so a request the CLI sends anywhere except the (NO_PROXY-exempt) fake gateway is recorded.
// CALIBRATION comes first: a binding to https://foreign.invalid must be recorded at the recorder,
// else routing cannot be observed and the whole suite fails (routing UNVERIFIED), never passes.
// CLAUDE_CODE_MAX_RETRIES=1 is likewise test-only: it bounds the CLI's own retry of a refused call.

const DRIVE_BUDGET_MS = 60_000;
const GATEWAY_PREFIX = "/gw/anthropic";
const SUB_IDENTITY = "SUBAGENT-IDENTITY-routing-4d2a";
const SUBAGENT_TOKEN = "SUBAGENT-TASK-routing-91c3";
// Assembled at runtime: no provider-token-shaped literal in source.
const KEY = ["gw", "key", "do-not-scan", "9999", "abcdefgh"].join("-");

interface Seen {
  method: string;
  url: string;
  headers: http.IncomingHttpHeaders;
  body: string;
}

/** A loopback server that records every request and answers with `respond`. */
class RecordingServer {
  readonly seen: Seen[] = [];
  protected readonly server: http.Server;
  constructor(respond: (req: http.IncomingMessage, res: http.ServerResponse, seen: Seen) => void) {
    this.server = http.createServer((req, res) => {
      req.socket.on("error", () => {});
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        const seen: Seen = { method: req.method ?? "", url: req.url ?? "", headers: req.headers, body: Buffer.concat(chunks).toString("utf8") };
        this.seen.push(seen);
        respond(req, res, seen);
      });
    });
  }
  async listen(): Promise<{ port: number; origin: string }> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    this.server.unref();
    const port = (this.server.address() as AddressInfo).port;
    return { port, origin: `http://127.0.0.1:${port}` };
  }
  async close(): Promise<void> {
    this.server.closeAllConnections();
    await new Promise<void>((resolve, reject) => this.server.close((e) => (e ? reject(e) : resolve())));
  }
}

/** The forward-proxy recorder: CONNECT and absolute-URI requests are recorded and refused (403). */
class Recorder extends RecordingServer {
  readonly connects: string[] = [];
  port = 0;
  constructor() {
    super((_req, res) => {
      res.writeHead(403, { "content-type": "text/plain" });
      res.end("refused by the routing recorder");
    });
    this.server.on("connect", (req: http.IncomingMessage, socket: Socket) => {
      this.connects.push(req.url ?? "");
      socket.on("error", () => {}); // the CLI may reset the refused tunnel; that is expected
      socket.end("HTTP/1.1 403 Forbidden\r\nconnection: close\r\n\r\n");
    });
  }
  /** Everything the recorder was asked to carry: CONNECT targets and absolute-URI request lines. */
  get targets(): string[] {
    return [...this.connects, ...this.seen.map((s) => s.url)];
  }
}

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.map((b: { type?: string; text?: string; content?: unknown }) => (b.type === "tool_result" ? textOf(b.content) : typeof b.text === "string" ? b.text : "")).join("\n");
}

interface MessagesBody {
  model?: string;
  system?: string | Array<{ text?: string }>;
  stream?: boolean;
  tools?: Array<{ name: string }>;
  messages: Array<{ role: string; content: unknown }>;
}

interface GatewayCall { path: string; model: string; agent: "lead" | "sub" | "side"; authorization?: string; xApiKey?: string }

type Redirect = { to: string; status: number } | undefined;

/** A Messages gateway under GATEWAY_PREFIX: scripted lead -> subagent -> done, optional 307 redirect. */
class Gateway extends RecordingServer {
  readonly calls: GatewayCall[] = [];
  private spawned = false;
  constructor(private readonly redirect: () => Redirect = () => undefined) {
    super((req, res, seen) => this.handle(req, res, seen));
  }
  private handle(req: http.IncomingMessage, res: http.ServerResponse, seen: Seen): void {
    const p = (req.url ?? "").split("?")[0] ?? "";
    const json = (status: number, body: unknown): void => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    const redirect = this.redirect();
    if (redirect && req.method === "POST" && p.startsWith(`${GATEWAY_PREFIX}/v1/messages`)) {
      res.writeHead(redirect.status, { location: redirect.to });
      res.end();
      return;
    }
    if (req.method === "POST" && p === `${GATEWAY_PREFIX}/v1/messages/count_tokens`) return json(200, { input_tokens: 100 });
    if (req.method !== "POST" || p !== `${GATEWAY_PREFIX}/v1/messages`) return json(404, { type: "error", error: { type: "not_found_error", message: "nf" } });
    const body = JSON.parse(seen.body) as MessagesBody;
    const tools = (body.tools ?? []).map((t) => t.name);
    const systemText = typeof body.system === "string" ? body.system : (body.system ?? []).map((b) => b.text ?? "").join("\n");
    const firstText = body.messages[0] ? textOf(body.messages[0].content) : "";
    const isSub = systemText.includes(SUB_IDENTITY) || firstText.includes(SUBAGENT_TOKEN);
    const hasBash = tools.includes("Bash");
    const agentTool = tools.includes("Agent") ? "Agent" : "Task";
    let turn: { text: string } | { tool: string; input: Record<string, unknown> };
    if (!hasBash) turn = { text: "ok" }; // side calls (titles etc.), the small/fast slot
    else if (isSub) turn = { text: "subagent done" };
    else if (!this.spawned && tools.includes(agentTool)) {
      this.spawned = true;
      turn = { tool: agentTool, input: { description: "sub", prompt: `${SUBAGENT_TOKEN}: do the steps`, subagent_type: "helper" } };
    } else turn = { text: "lead done" };
    this.calls.push({
      path: p, model: body.model ?? "", agent: isSub ? "sub" : hasBash ? "lead" : "side",
      authorization: req.headers.authorization, xApiKey: typeof req.headers["x-api-key"] === "string" ? req.headers["x-api-key"] : undefined,
    });
    const content = "text" in turn
      ? [{ type: "text", text: turn.text }]
      : [{ type: "tool_use", id: `toolu_${this.calls.length}`, name: turn.tool, input: turn.input }];
    const stopReason = "text" in turn ? "end_turn" : "tool_use";
    const usage = { input_tokens: 100, output_tokens: 20 };
    const message = { id: `msg_${this.calls.length}`, type: "message", role: "assistant", model: body.model ?? "x", stop_reason: null as string | null, stop_sequence: null, usage };
    if (!body.stream) return json(200, { ...message, content, stop_reason: stopReason });
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    const send = (event: string, data: unknown): void => { res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`); };
    send("message_start", { type: "message_start", message: { ...message, content: [] } });
    content.forEach((block, index) => {
      if (block.type === "text") {
        send("content_block_start", { type: "content_block_start", index, content_block: { type: "text", text: "" } });
        send("content_block_delta", { type: "content_block_delta", index, delta: { type: "text_delta", text: (block as { text: string }).text } });
      } else {
        const tu = block as { id: string; name: string; input: unknown };
        send("content_block_start", { type: "content_block_start", index, content_block: { type: "tool_use", id: tu.id, name: tu.name, input: {} } });
        send("content_block_delta", { type: "content_block_delta", index, delta: { type: "input_json_delta", partial_json: JSON.stringify(tu.input) } });
      }
      send("content_block_stop", { type: "content_block_stop", index });
    });
    send("message_delta", { type: "message_delta", delta: { stop_reason: stopReason, stop_sequence: null }, usage });
    send("message_stop", { type: "message_stop" });
    res.end();
  }
}

function credential(baseUrl: string, authStyle: "bearer" | "x_api_key"): SdkCredential {
  return { kind: "provider", baseUrl, authStyle, key: KEY, modelMap: { sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } } };
}

describe("exclusive routing of the real CLI in provider mode (PRD #4320 module 7)", () => {
  const roots: string[] = [];
  after(() => { for (const r of roots) fs.rmSync(r, { recursive: true, force: true }); });

  /** Drive the real CLI once under the provider env; resolves with the SDK env it was given and its stderr. */
  async function drive(opts: {
    cred: SdkCredential;
    proxy: Recorder;
    noProxy: string;
    withSubagent: boolean;
    boundedRetries: boolean;
    budgetMs?: number;
    prompt?: string;
  }): Promise<{ env: Record<string, string | undefined>; stderr: string; failed: boolean }> {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-routing-it-"));
    roots.push(root);
    const home = path.join(root, "home");
    const worktree = path.join(root, "worktree");
    const tmp = path.join(root, "tmp");
    for (const d of [path.join(home, ".claude"), worktree, tmp]) fs.mkdirSync(d, { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "settings.json"), "{}\n");
    const recorderUrl = `http://127.0.0.1:${opts.proxy.port}`;
    const env = {
      ...buildSdkEnv(opts.cred, home),
      TMPDIR: tmp,
      HTTPS_PROXY: recorderUrl,
      HTTP_PROXY: recorderUrl,
      https_proxy: recorderUrl,
      http_proxy: recorderUrl,
      NO_PROXY: opts.noProxy,
      no_proxy: opts.noProxy,
      ...(opts.boundedRetries ? { CLAUDE_CODE_MAX_RETRIES: "1" } : {}),
    } as Record<string, string | undefined>;
    let stderr = "";
    let failed = false;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), opts.budgetMs ?? DRIVE_BUDGET_MS);
    const options: Options = {
      cwd: worktree,
      env,
      model: "sonnet",
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      settingSources: [],
      stderr: (d: string) => { stderr += d; },
      abortController: controller,
      ...(opts.withSubagent
        ? { agents: { helper: { description: "does one step", prompt: `Do exactly what the task says. ${SUB_IDENTITY}`, tools: ["Bash"], model: "haiku" as const } } }
        : {}),
    };
    try {
      for await (const m of query({ prompt: opts.prompt ?? "run the steps", options })) {
        const mm = m as { type: string; is_error?: boolean };
        if (mm.type === "result" && mm.is_error) failed = true;
      }
    } catch {
      failed = true;
    } finally {
      clearTimeout(timer);
    }
    return { env, stderr, failed };
  }

  async function startRecorder(): Promise<Recorder> {
    const rec = new Recorder();
    rec.port = (await rec.listen()).port;
    return rec;
  }

  it("CALIBRATION: a binding to https://foreign.invalid is recorded at the proxy recorder (else routing is unverified)", { timeout: 120_000 }, async () => {
    const rec = await startRecorder();
    try {
      await drive({ cred: credential("https://foreign.invalid", "bearer"), proxy: rec, noProxy: "127.0.0.1:1", withSubagent: false, boundedRetries: true, budgetMs: 40_000 });
      assert.ok(
        rec.targets.some((t) => t.includes("foreign.invalid")),
        `ROUTING UNVERIFIED: the CLI's request to a foreign destination did not reach the recorder (targets: ${JSON.stringify(rec.targets)})`,
      );
    } finally {
      await rec.close();
    }
  });

  for (const authStyle of ["bearer", "x_api_key"] as const) {
    it(`${authStyle}: main agent, a Task subagent and the auxiliary calls reach only the bound gateway`, { timeout: 120_000 }, async () => {
      const rec = await startRecorder();
      const gw = new Gateway();
      const { port, origin } = await gw.listen();
      try {
        const base = `${origin}${GATEWAY_PREFIX}`;
        const { env, failed, stderr } = await drive({ cred: credential(base, authStyle), proxy: rec, noProxy: `127.0.0.1:${port}`, withSubagent: true, boundedRetries: false });
        assert.ok(!failed, `the drive failed: ${stderr.slice(0, 800)}`);
        // The child env: no OAuth token anywhere, exactly one auth variable.
        assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, undefined);
        assert.equal(env.ANTHROPIC_BASE_URL, base);
        // The recorder (the only other reachable destination) saw nothing.
        assert.deepStrictEqual(rec.targets, [], `a request left the gateway: ${JSON.stringify(rec.targets)}`);
        // Everything the gateway saw is under the bound base URL, and every model is a mapped one.
        const paths = [...new Set(gw.calls.map((c) => c.path))];
        const allPaths = [...new Set(gw.seen.map((s) => s.url.split("?")[0]))];
        for (const p of allPaths) assert.ok(p!.startsWith(`${GATEWAY_PREFIX}/`), `a request left the base URL path: ${p}`);
        assert.ok(paths.includes(`${GATEWAY_PREFIX}/v1/messages`), "the lead reached /v1/messages under the base URL");
        const models = new Set(gw.calls.map((c) => c.model));
        for (const m of models) assert.ok(m === "corp-sonnet" || m === "corp-haiku", `an unmapped model reached the gateway: ${m}`);
        assert.ok(models.has("corp-sonnet"), "the lead ran on the mapped sonnet model");
        assert.ok(models.has("corp-haiku"), "a haiku-slot call (subagent or auxiliary) ran on the mapped haiku model");
        assert.ok(gw.calls.some((c) => c.agent === "sub"), "the Task subagent reached the gateway");
        // The key rides in exactly the configured auth header.
        for (const c of gw.calls) {
          if (authStyle === "bearer") {
            assert.equal(c.authorization, `Bearer ${KEY}`);
            assert.equal(c.xApiKey, undefined);
          } else {
            assert.equal(c.xApiKey, KEY);
            assert.equal(c.authorization, undefined);
          }
        }
      } finally {
        await gw.close();
        await rec.close();
      }
    });
  }

  // ── Provider judge settlement against the REAL pinned CLI (PRD #4322 M1a, D-H) ─────────────────
  // The judge settles a gateway limit from the typed `api_error_status` of the raw result frame. This
  // drives JudgeRunner.execute end to end through the real CLI against a gateway that answers every
  // /v1/messages with 429, then 529 (the CLI's own retries bounded to 1 by the query wrapper, test-only),
  // and a status-less control where the gateway drops the connection.
  async function judgeAgainst(respond: (req: http.IncomingMessage, res: http.ServerResponse) => void) {
    const gw = new RecordingServer((req, res) => respond(req, res));
    const { origin } = await gw.listen();
    const states: StateRequest[] = [];
    const reviews: ReviewRequest[] = [];
    const client = {
      getTrace: async () => ({ target: { id: "t", kind: "issue", status: "completed", issue_title: "x", issue_description: "", branch: null,
        mr_iid: null, failure_reason: null, fix_verdict: null, plan_md: "", iteration_count: 1, repo_agents: null, stop_kind: null }, messages: [], inputs: [] }),
      postReview: async (_id: string, review: ReviewRequest) => { reviews.push(review); },
      reportState: async (_id: string, body: StateRequest) => { states.push(body); return { applied: true, status: body.status } as never; },
      postMessages: async () => {},
    } as unknown as WorkerClient;
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-judge-it-"));
    roots.push(root);
    // The real CLI creates its socket and per-uid runtime directories under TMPDIR; point it inside
    // root so they are removed with it (the shard's TMPDIR leak guard fails otherwise).
    const tmp = path.join(root, "tmp");
    fs.mkdirSync(tmp, { recursive: true });
    const frames: unknown[] = [];
    const queryFn = ((params: { prompt: AsyncIterable<unknown>; options: Options }) => {
      const env = { ...params.options.env, CLAUDE_CODE_MAX_RETRIES: "1", TMPDIR: tmp };
      return (async function* () {
        for await (const m of query({ prompt: params.prompt as never, options: { ...params.options, env } })) { frames.push(m); yield m; }
      })();
    }) as unknown as SdkQueryFn;
    const runner = new JudgeRunner(client, nullLogger(), { queryFn, homeRoot: root, modelTimeoutMs: 100_000 });
    const claim = {
      run_id: "judge-it", kind: "judge", target_run_id: "t", claim_generation: 2, issue_iid: null, issue_title: "j", issue_description: "",
      repo: {} as never, last_seq: 0, agents: [], judge_model: "sonnet", secrets: { forge_pat: "", anthropic_oauth_token: "", provider: {
        provider_id: "p", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: `${origin}${GATEWAY_PREFIX}`, auth_style: "bearer",
        credential: KEY, model_map: { sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } } } },
    } as unknown as ClaimResponse;
    try { await runner.execute(claim); } finally { await gw.close(); }
    const result = frames.find((f) => (f as { type?: string }).type === "result") as Record<string, unknown> | undefined;
    return { states, reviews, result, seen: gw.seen };
  }

  for (const status of [429, 529]) {
    it(`a provider judge against a gateway answering ${status}: failed/provider_gateway_limit, no review, never completed`, { timeout: 180_000 }, async () => {
      const r = await judgeAgainst((_req, res) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify({ type: "error", error: { type: status === 429 ? "rate_limit_error" : "overloaded_error", message: "slow down" } }));
      });
      assert.ok(r.seen.some((s) => s.url.startsWith(`${GATEWAY_PREFIX}/v1/messages`)), "the judge's model call reached the gateway");
      assert.equal(r.result?.api_error_status, status, `the real CLI surfaces api_error_status ${status} (result: ${JSON.stringify(r.result)?.slice(0, 400)})`);
      assert.equal(r.reviews.length, 0);
      assert.equal(r.states.some((s) => s.status === "completed"), false);
      const last = r.states.at(-1)!;
      assert.equal(last.status, "failed");
      assert.equal(last.rate_limit_type, "provider_gateway_limit");
      assert.equal(last.limit_resets_at, undefined);
    });
  }

  it("status-less control: a gateway that drops the connection ends the provider judge failed, no review, never completed", { timeout: 180_000 }, async () => {
    const r = await judgeAgainst((req) => { req.socket.destroy(); });
    assert.ok(r.seen.length > 0, "the judge's model call reached the gateway");
    assert.equal(r.reviews.length, 0);
    assert.equal(r.states.some((s) => s.status === "completed"), false);
    const last = r.states.at(-1)!;
    assert.equal(last.status, "failed");
    assert.equal(last.rate_limit_type, undefined);
    assert.ok(!(last.failure_reason ?? "").includes(KEY));
  });

  // ── Redirects ───────────────────────────────────────────────────────────────────────────────
  // ACCEPTED RISK (ADR-4320, 2026-11-19, #4505): the CLI, characterized at SDK 0.3.319 / CLI 2.1.319, FOLLOWS a
  // gateway 307 for POST /v1/messages, re-sending the request body to the Location the gateway names:
  // to a same-origin sibling path with the auth header, and to a second loopback origin (bearer: the
  // Authorization header is dropped by the fetch cross-origin rule; x_api_key: `x-api-key` IS forwarded).
  // The owner accepted this and claude_provider_v1 is advertised. These tests PIN that observed behaviour.
  // The relay (#4496) is the future mitigation. An SDK/CLI bump that changes redirect handling turns
  // these red and forces a re-look at the accepted risk.
  const REDIRECT_SENTINEL = "uzi-redirect-sentinel-7f3a91";
  const REDIRECT_SENTINEL_PROMPT = `run the steps ${REDIRECT_SENTINEL}`;

  async function redirectRun(authStyle: "bearer" | "x_api_key", target: "same-origin sibling path" | "second loopback server") {
    const rec = await startRecorder();
    const other = new RecordingServer((_req, res) => { res.writeHead(500); res.end("redirect target"); });
    const otherAddr = await other.listen();
    let location = "";
    const gw = new Gateway(() => ({ status: 307, to: location }));
    const { port, origin } = await gw.listen();
    location = target === "same-origin sibling path" ? "/other-tenant/v1/messages" : `${otherAddr.origin}/v1/messages`;
    try {
      await drive({
        cred: credential(`${origin}${GATEWAY_PREFIX}`, authStyle), proxy: rec,
        noProxy: `127.0.0.1:${port},127.0.0.1:${otherAddr.port}`, withSubagent: false, boundedRetries: true, budgetMs: 40_000, prompt: REDIRECT_SENTINEL_PROMPT,
      });
      const line = (s: Seen): string => `${s.method} ${s.url}`;
      return {
        gatewayLines: gw.seen.map(line),
        gatewaySibling: gw.seen.filter((s) => s.url.startsWith("/other-tenant")),
        otherSeen: other.seen,
        recorderTargets: rec.targets,
      };
    } finally {
      await gw.close();
      await other.close();
      await rec.close();
    }
  }

  for (const authStyle of ["bearer", "x_api_key"] as const) {
    for (const target of ["same-origin sibling path", "second loopback server"] as const) {
      it(`ACCEPTED RISK ${authStyle}: a 307 to a ${target} is followed with the request body (key per auth style)`, { timeout: 120_000 }, async () => {
        const r = await redirectRun(authStyle, target);
        assert.ok(r.gatewayLines.some((l) => l.startsWith("POST /gw/anthropic/v1/messages")), "the gateway issued the redirect");
        const hop = target === "same-origin sibling path" ? r.gatewaySibling[0] : r.otherSeen[0];
        assert.ok(hop, "the redirect is followed (observed)");
        assert.equal(hop.method, "POST");
        assert.ok(hop.body.includes(REDIRECT_SENTINEL), "the request body is re-sent to the redirect target");
        assert.deepEqual(r.recorderTargets, [], "no request left through the proxy recorder");
        const auth = hop.headers.authorization;
        const xKey = hop.headers["x-api-key"];
        if (authStyle === "bearer") assert.equal(xKey, undefined, "bearer: no x-api-key header is sent");
        if (target === "same-origin sibling path") {
          if (authStyle === "bearer") assert.equal(auth, `Bearer ${KEY}`, "the key travels to the same-origin sibling");
          else assert.equal(xKey, KEY, "the key travels to the same-origin sibling");
        } else if (authStyle === "bearer") {
          assert.equal(auth, undefined, "bearer: the fetch cross-origin rule drops Authorization");
        } else {
          assert.equal(xKey, KEY, "x_api_key: the key is forwarded to the second origin");
        }
      });
    }
  }
});
