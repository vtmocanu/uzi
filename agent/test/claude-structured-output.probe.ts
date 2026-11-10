// Outside the gate's *.test.ts glob: a manual, bounded, offline recording of the real Claude Agent
// SDK frames for the structured-output carrier (the StructuredOutput tool). The model turns are
// scripted by a loopback Anthropic Messages stub, so no Anthropic credential is needed.
//   node --import tsx test/claude-structured-output.probe.ts capture <absolute CLI path> [out path]
//   node --import tsx test/claude-structured-output.probe.ts verify <absolute CLI path>
// `verify` runs the PRODUCTION ClaudeCrossCheck (builder, hooks, init check, budget, result handling)
// against the same scripted stub and the real SDK; it writes nothing.
// Run from agent/. Use the CLI that matches the pinned SDK, and repeat it when either is bumped.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import type { AddressInfo } from "node:net";
import path from "node:path";
import { buildClaudeCrossCheckOptions, ClaudeCrossCheck } from "../src/claude-cross-check.js";
import { codeCrossCheckBrief, crossCheckBrief, CODE_VERDICT_SCHEMA, CrossCheckMalformedError, PLAN_VERDICT_SCHEMA, validateCodeVerdict, validateVerdict } from "../src/codex/cross-check.js";
import type { ClaimResponse } from "../src/protocol.js";
import { checkIsolatedInit } from "../src/isolated-executor.js";
import { promptStream } from "../src/sdk-messages.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

type Block = { type: "text"; text: string } | { type: "tool_use"; id: string; name: string; input: unknown };
type Turn = { blocks: Block[]; stop: "tool_use" | "end_turn" };
type Stage = "plan" | "code";
interface SessionSpec { name: string; stage: Stage; turns: (checkout: string) => Turn[]; exhaust?: boolean }
type Json = Record<string, unknown>;
type Auth = "oauth" | "apikey";

const MODEL = "claude-test";
const SURFACE_TOOLS = ["Read", "Grep", "Glob"];

const read = (checkout: string): Turn => ({
  blocks: [{ type: "tool_use", id: "toolu_read_1", name: "Read", input: { file_path: path.join(checkout, "a.ts") } }], stop: "tool_use" });
const glob = (checkout: string): Turn => ({
  blocks: [{ type: "tool_use", id: "toolu_glob_1", name: "Glob", input: { pattern: "*.ts", path: checkout } }], stop: "tool_use" });
const text = (t: string): Turn => ({ blocks: [{ type: "text", text: t }], stop: "end_turn" });
const carrier = (id: string, input: unknown): Turn => ({ blocks: [{ type: "tool_use", id, name: "StructuredOutput", input }], stop: "tool_use" });

const SESSIONS: SessionSpec[] = [
  { name: "plan_retry", stage: "plan", turns: (c) => [
    read(c), text("Verdict: approve"),
    carrier("toolu_so_invalid_1", { verdict: "maybe" }),
    carrier("toolu_so_valid_1", { verdict: "approve", summary: "The plan matches the checked files.",
      items: [{ file: "a.ts", severity: "info", summary: "Anchor confirmed", rationale: "Read a.ts" }] }),
  ] },
  { name: "code_retry", stage: "code", turns: (c) => [
    glob(c), text("No defects found."),
    carrier("toolu_so_invalid_1", { findings: "none" }),
    carrier("toolu_so_valid_1", { findings: [{ id: "F1", severity: "minor", path: "a.ts", line: 1, title: "Nit", detail: "Cosmetic only" }],
      coverage: [{ path: "a.ts", status: "examined", reason: "read" }] }),
  ] },
  { name: "exhausted", stage: "plan", exhaust: true, turns: (c) => [read(c), text("Verdict: approve")] },
];

function sse(res: http.ServerResponse, n: number, model: string, turn: Turn): void {
  res.writeHead(200, { "content-type": "text/event-stream" });
  const w = (ev: string, d: unknown): void => { res.write(`event: ${ev}\ndata: ${JSON.stringify(d)}\n\n`); };
  w("message_start", { type: "message_start", message: { id: `msg_stub_${n}`, type: "message", role: "assistant", model, content: [], stop_reason: null, stop_sequence: null, usage: { input_tokens: 10, output_tokens: 1 } } });
  turn.blocks.forEach((b, i) => {
    if (b.type === "text") {
      w("content_block_start", { type: "content_block_start", index: i, content_block: { type: "text", text: "" } });
      w("content_block_delta", { type: "content_block_delta", index: i, delta: { type: "text_delta", text: b.text } });
    } else {
      w("content_block_start", { type: "content_block_start", index: i, content_block: { type: "tool_use", id: b.id, name: b.name, input: {} } });
      w("content_block_delta", { type: "content_block_delta", index: i, delta: { type: "input_json_delta", partial_json: JSON.stringify(b.input) } });
    }
    w("content_block_stop", { type: "content_block_stop", index: i });
  });
  w("message_delta", { type: "message_delta", delta: { stop_reason: turn.stop, stop_sequence: null }, usage: { output_tokens: 5 } });
  w("message_stop", { type: "message_stop" });
  res.end();
}

/** A loopback Messages stub. Only requests for the scripted model consume a scripted turn. */
async function startStub(script: (n: number) => Turn): Promise<{ url: string; requests: () => number; close: () => Promise<void> }> {
  let n = 0;
  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (c: Buffer) => { body += c.toString("utf8"); });
    req.on("end", () => {
      let model = "";
      try { model = String((JSON.parse(body) as Json).model ?? ""); } catch { /* not JSON */ }
      if (!(req.url ?? "").startsWith("/v1/messages") || (req.url ?? "").includes("count_tokens")) { res.writeHead(404); res.end("{}"); return; }
      if (model !== MODEL) { sse(res, 0, model, text("ok")); return; }
      n++;
      sse(res, n, model, script(n));
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const port = (server.address() as AddressInfo).port;
  return { url: `http://127.0.0.1:${port}`, requests: () => n, close: () => new Promise<void>((r) => { server.close(() => r()); server.closeAllConnections(); }) };
}

/** Rewrites host-specific and generated values; everything else stays as the SDK emitted it. */
/** The CLI names per-project directories by replacing every non-alphanumeric character with a dash. */
const dashed = (p: string): string => p.replace(/[^a-zA-Z0-9]/g, "-");

function normalizer(replacements: Array<[string, string]>): (v: unknown, key?: string) => unknown {
  const withDashed = replacements.flatMap(([from, to]): Array<[string, string]> => [[from, to], [dashed(from), to]]);
  const ordered = withDashed.filter(([from]) => from).sort((a, b) => b[0].length - a[0].length);
  const uuid = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g;
  const iso = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g;
  const walk = (v: unknown, key?: string): unknown => {
    if (typeof v === "string") {
      if (key === "transcript_path") return "<transcript>";
      if (key === "messaging_socket_path") return "<socket>";
      let s = v;
      for (const [from, to] of ordered) s = s.split(from).join(to);
      // Git worktrees share the original repository's auto-memory directory, not repoRoot's.
      if (key === "auto") s = s.replace(/^<home>\/\.claude\/projects\/[^/]+\/memory\/$/, "<home>/.claude/projects/<repo>/memory/");
      return s.replace(uuid, "<uuid>").replace(iso, "<timestamp>");
    }
    if (typeof v === "number" && key && (/^(duration|ttft|time_to_request|first_content_frame)/.test(key) || key.endsWith("_ms"))) return 0;
    if (Array.isArray(v)) return v.map((x) => walk(x));
    if (v && typeof v === "object") return Object.fromEntries(Object.entries(v as Json).map(([k, x]) => [k, walk(x, k)]));
    return v;
  };
  return walk;
}

interface Captured { stage: Stage; threw: string | null; frames: unknown[]; hook_inputs: unknown[]; auth: Auth; version: string; spawn: string }

async function runSession(spec: SessionSpec, cli: string, root: string, sdk: { query: SdkQueryFn }, auth: Auth): Promise<Captured> {
  const checkout = path.join(root, spec.name, "checkout");
  const home = path.join(root, spec.name, `home-${auth}`);
  await fs.mkdir(checkout, { recursive: true });
  await fs.mkdir(home, { recursive: true });
  await fs.writeFile(path.join(checkout, "a.ts"), "export const answer = 42;\n");
  const turns = spec.turns(checkout);
  const stub = await startStub((n) => {
    if (n <= turns.length) return turns[n - 1] as Turn;
    return spec.exhaust ? carrier(`toolu_so_invalid_${n - turns.length}`, { verdict: "maybe" }) : text("done");
  });
  const gate = { passed: false };
  const hookInputs: unknown[] = [];
  const schema = spec.stage === "plan" ? PLAN_VERDICT_SCHEMA : CODE_VERDICT_SCHEMA;
  try {
    // The UNCHANGED production builder, with its production arguments.
    const options = buildClaudeCrossCheckOptions({
      cwd: checkout, homeDir: home, oauthToken: "dummy-offline-credential", log: nullLogger(),
      secretPaths: [path.join(checkout, "worker-secret")], gate,
      brief: spec.stage === "plan" ? crossCheckBrief("Read, Grep and Glob") : codeCrossCheckBrief("Read, Grep and Glob"),
      maxTurns: 20,
      model: MODEL,
      ...(spec.stage === "code" ? { toolBudgetHook: async () => ({}) } : {}),
    });
    (options as unknown as Json)["outputFormat"] = { type: "json_schema", schema };

    // Replace ONLY the matcher-less isolated tool gate; every other hook position is untouched.
    const pre = options.hooks?.PreToolUse;
    assert.ok(pre && pre.length >= 4, "unexpected PreToolUse layout");
    const before = pre.map((m) => m.matcher ?? null);
    const original = pre[0];
    assert.ok(original && original.matcher === undefined && original.hooks.length === 1, "PreToolUse[0] is not the matcher-less gate");
    const gateHook = original.hooks[0] as (...a: unknown[]) => Promise<unknown>;
    pre[0] = { hooks: [(async (...args: unknown[]) => {
      const input = args[0] as Json;
      hookInputs.push({ hook_event_name: input["hook_event_name"], tool_name: input["tool_name"], tool_input: input["tool_input"], tool_use_id: input["tool_use_id"] });
      assert.ok(gate.passed, "a tool call arrived before the init frame was checked");
      if (input["tool_name"] === "StructuredOutput") return {};
      return gateHook(...args);
    }) as never] };
    assert.deepEqual(pre.map((m) => m.matcher ?? null), before, "hook matchers changed");

    // Env: the production replacement env, changed only by the stub URL, auth and (already) HOME.
    const env: Record<string, string | undefined> = { ...options.env, ANTHROPIC_BASE_URL: stub.url };
    assert.equal(env["HOME"], home);
    if (auth === "apikey") { delete env["CLAUDE_CODE_OAUTH_TOKEN"]; env["ANTHROPIC_API_KEY"] = "dummy-offline-key"; }
    options.env = env;
    options.pathToClaudeCodeExecutable = cli;
    assert.equal(options.model, MODEL, "the builder did not apply its model argument");
    const abort = new AbortController();
    options.abortController = abort;
    const timer = setTimeout(() => abort.abort(), 60000);

    const frames: unknown[] = [];
    let threw: string | null = null;
    let version = "";
    try {
      for await (const msg of sdk.query({ prompt: promptStream("Check the claim."), options })) {
        const frame = msg as unknown as Json;
        if (frame["type"] === "system" && frame["subtype"] === "init") {
          const tools = frame["tools"] as string[];
          assert.deepEqual([...tools].sort(), [...SURFACE_TOOLS, "StructuredOutput"].sort(), "init tool set is not Read, Grep, Glob + StructuredOutput");
          const withoutCarrier = { ...frame, tools: tools.filter((t) => t !== "StructuredOutput") };
          assert.equal(checkIsolatedInit(withoutCarrier, { tools: SURFACE_TOOLS, mcpServers: [], plugins: [], skills: [] }), undefined);
          gate.passed = true;
          version = String(frame["claude_code_version"]);
        }
        frames.push(frame);
        if (frame["type"] === "result") break;
      }
    } catch (e) { threw = e instanceof Error ? e.constructor.name : typeof e; } finally { clearTimeout(timer); abort.abort(); }
    assert.ok(stub.requests() > 0 || auth === "oauth", "stub saw no scripted request");
    return { stage: spec.stage, threw, frames, hook_inputs: hookInputs, auth, version, spawn: "spawnDetached (production default)" };
  } finally { await stub.close(); }
}

async function capture(cli: string, outPath: string): Promise<void> {
  const sdk = await import("@anthropic-ai/claude-agent-sdk") as unknown as { query: SdkQueryFn };
  const repoRoot = path.resolve(import.meta.dirname, "../..");
  const sdkVersion = (JSON.parse(await fs.readFile(path.resolve(import.meta.dirname, "../node_modules/@anthropic-ai/claude-agent-sdk/package.json"), "utf8")) as { version: string }).version;
  assert.equal(sdkVersion, "0.3.316", "the fixture file name pins SDK 0.3.316; rename it when the SDK is bumped");
  const scratch = path.join(repoRoot, ".uzi/scratch");
  await fs.mkdir(scratch, { recursive: true });
  const root = await fs.mkdtemp(path.join(scratch, "so-probe-"));
  try {
    const sessions: Record<string, unknown> = {};
    const auths = new Set<Auth>();
    let version = "";
    for (const spec of SESSIONS) {
      // OAuth first; a CLI that will not use it against a custom base URL makes no stub request.
      let result = await runSession(spec, cli, root, sdk, "oauth").catch((e: unknown) => {
        if (e instanceof assert.AssertionError) throw e;
        return { error: e };
      });
      if ("error" in result || result.frames.length === 0 || (result.frames.some((f) => (f as Json)["type"] === "result" && (f as Json)["is_error"] === true) && result.hook_inputs.length === 0)) {
        result = await runSession(spec, cli, root, sdk, "apikey");
      }
      const done = result as Captured;
      assert.equal(done.threw, null, `session ${spec.name} threw ${String(done.threw)}; refusing to write the fixture`);
      auths.add(done.auth);
      version = done.version;
      const checkout = path.join(root, spec.name, "checkout");
      const home = path.join(root, spec.name, `home-${done.auth}`);
      const norm = normalizer([
        [await fs.realpath(checkout), "<checkout>"], [checkout, "<checkout>"],
        [await fs.realpath(home), "<home>"], [home, "<home>"],
        [root, "<scratch>"],
        [repoRoot, "<repo>"],
        [process.env["HOME"] ?? "", "<home-dir>"],
      ]);
      sessions[spec.name] = { stage: done.stage, threw: done.threw, frames: norm(done.frames), hook_inputs: norm(done.hook_inputs) };
    }
    const authNote = auths.size === 1 && auths.has("oauth")
      ? "auth: the production CLAUDE_CODE_OAUTH_TOKEN placeholder was accepted against the stub"
      : "auth: CLAUDE_CODE_OAUTH_TOKEN removed and ANTHROPIC_API_KEY set to a dummy because the CLI would not use the OAuth placeholder against a custom base URL";
    const fixture = {
      _provenance: `real frames from @anthropic-ai/claude-agent-sdk ${sdkVersion} / native CLI ${version}, recorded ${new Date().toISOString().slice(0, 10)} by test/claude-structured-output.probe.ts capture; model turns scripted by a loopback Anthropic Messages stub because no Anthropic credential is available in the worker; production buildClaudeCrossCheckOptions (model passed through its own argument; the code session also passes an allow-all recording toolBudgetHook) with probe-local outputFormat (PLAN/CODE_VERDICT_SCHEMA), probe-local abortController with a 60 s timeout, probe-local replacement of only the matcher-less tool gate (allows exactly StructuredOutput, delegates the rest), probe-local init latch opening after a carrier-aware init check, env changed only by ANTHROPIC_BASE_URL (+ auth used) and HOME, pathToClaudeCodeExecutable set; ${authNote}; spawnClaudeCodeProcess left as built; ids/timestamps/paths normalized; repeat with: node --import tsx test/claude-structured-output.probe.ts capture <absolute CLI path>`,
      sessions,
    };
    const out = JSON.stringify(fixture, null, 2) + "\n";
    const forbiddens = [repoRoot, root, process.env["HOME"] ?? "\u0000", "sk-ant-", "cc-socks", "/tmp/", "data-runner", path.basename(repoRoot)];
    for (const p of [repoRoot, root, process.env["HOME"] ?? "\u0000"]) forbiddens.push(dashed(p));
    for (const forbidden of forbiddens) {
      assert.ok(!out.includes(forbidden), `fixture still contains a host-specific or token-shaped value: ${forbidden}`);
    }
    // Any absolute-path-looking string left outside a placeholder (a placeholder precedes its path as "<x>/").
    const strings: string[] = [];
    const collect = (v: unknown): void => {
      if (typeof v === "string") strings.push(v);
      else if (Array.isArray(v)) v.forEach(collect);
      else if (v && typeof v === "object") Object.values(v as Json).forEach(collect);
    };
    collect(sessions);
    const absPath = /(?<![\w<>.:/-])\/(?:[\w.-]+\/)+[\w.-]*/;
    for (const s of strings) {
      const hit = absPath.exec(s);
      assert.ok(!hit, `fixture still contains an absolute path outside the placeholders: ${hit?.[0]}`);
    }
    await fs.mkdir(path.dirname(outPath), { recursive: true });
    await fs.writeFile(outPath, out);
    console.log(`wrote ${outPath} (${[...auths].join(",")}, CLI ${version})`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
}

function verifyClaim(stage: Stage): ClaimResponse {
  const base = {
    kind: "cross_check", run_id: "probe", claim_generation: 1, last_seq: 0, agents: [], issue_iid: null, issue_title: "Issue", issue_description: "Issue DATA",
    repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo", clone_url: "https://example.test/repo.git", default_branch: "main" },
    config: { default_model: MODEL, default_effort: "high" },
    secrets: { forge_pat: "", anthropic_oauth_token: "dummy-offline-credential" },
  };
  const cross = stage === "code"
    ? { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64), base_commit: "a".repeat(40), head_commit: "b".repeat(40),
      plan_md: "Approved plan DATA", guidance_snapshot: "", milestones: [], required_capabilities: [], required_tools: [], size_class: "small",
      code_context: { task: "context DATA" }, code_diff: "diff DATA", deadline_at: "2099-01-01T00:00:00Z" }
    : { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest", deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan",
      milestones: [], required_capabilities: [], required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff" };
  return { ...base, cross_check: cross } as unknown as ClaimResponse;
}

async function verify(cli: string): Promise<void> {
  const sdk = await import("@anthropic-ai/claude-agent-sdk") as unknown as { query: SdkQueryFn };
  const repoRoot = path.resolve(import.meta.dirname, "../..");
  const scratch = path.join(repoRoot, ".uzi/scratch");
  await fs.mkdir(scratch, { recursive: true });
  const root = await fs.mkdtemp(path.join(scratch, "so-verify-"));
  try {
    for (const spec of SESSIONS) {
      const checkout = await fs.realpath(await fs.mkdir(path.join(root, spec.name, "checkout"), { recursive: true }).then(() => path.join(root, spec.name, "checkout")));
      const home = path.join(root, spec.name, "home");
      await fs.mkdir(home, { recursive: true });
      await fs.writeFile(path.join(checkout, "a.ts"), "export const answer = 42;\n");
      const turns = spec.turns(checkout);
      const stub = await startStub((n) => {
        if (n <= turns.length) return turns[n - 1] as Turn;
        return spec.exhaust ? carrier(`toolu_so_invalid_${n - turns.length}`, { verdict: "maybe" }) : text("done");
      });
      try {
        // Only the stub URL and the CLI path change; every other option is the production builder's.
        const queryFn = ((p: Parameters<SdkQueryFn>[0]) => sdk.query({ ...p, options: {
          ...p.options, env: { ...p.options.env, ANTHROPIC_BASE_URL: stub.url }, pathToClaudeCodeExecutable: cli,
        } })) as SdkQueryFn;
        const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: [path.join(checkout, "worker-secret")], queryFn });
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), 90000);
        let outcome: string;
        try {
          const verdict = await checker.run(verifyClaim(spec.stage), checkout, home, controller.signal, async () => {});
          assert.notEqual(spec.exhaust, true, `${spec.name} must reject`);
          if (spec.stage === "code") validateCodeVerdict(verdict); else validateVerdict(verdict);
          outcome = `verdict ${verdict}`;
        } catch (err) {
          if (err instanceof assert.AssertionError) throw err;
          assert.ok(spec.exhaust === true, `${spec.name} rejected unexpectedly: ${String(err)}`);
          assert.ok(err instanceof CrossCheckMalformedError && err.subreason === "structured_output_retries", `${spec.name} rejected with ${String(err)}`);
          outcome = `rejected CrossCheckMalformedError/${err.subreason} (${err.message})`;
        } finally { clearTimeout(timer); }
        console.log(`${spec.name}: ${outcome}; stub requests ${stub.requests()}`);
      } finally { await stub.close(); }
    }
    console.log("verify ok");
  } finally { await fs.rm(root, { recursive: true, force: true }); }
}

const [mode, cli, out] = process.argv.slice(2);
if (mode !== "capture" && mode !== "verify") throw new Error("usage: claude-structured-output.probe.ts capture|verify <absolute CLI path> [out path]");
if (!cli || !path.isAbsolute(cli)) throw new Error("an explicit absolute native CLI path is required");
if (mode === "verify") await verify(cli);
else await capture(cli, out ? path.resolve(out) : path.resolve(import.meta.dirname, "fixtures/claude-structured-output-0.3.316.json"));
