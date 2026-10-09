// PRD #2460: the Claude plan checker. The confinement is asserted on the built SDK options and the
// composed hook list (no live session), the verdict and model-rejection rules through an injected
// query function, and the CrossCheckRunner dispatch through its public execute().
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { HookCallbackMatcher, HookInput, HookJSONOutput, Options as SdkOptions, SDKAssistantMessageError } from "@anthropic-ai/claude-agent-sdk";
import { buildClaudeCrossCheckOptions, ClaudeCrossCheck, stubClaudeCrossCheckQueryFn } from "../src/claude-cross-check.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { CrossCheckMalformedError } from "../src/codex/cross-check.js";
import { CrossCheckCheckerUnavailableError } from "../src/codex/model-rejection.js";
import { buildIsolatedPreToolUse, type InitGate, type IsolationSurface } from "../src/isolated-executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { GitCache } from "../src/git.js";
import type { Logger } from "../src/log.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const TOKEN = "fixture-anthropic-token-abc123";
const APPROVE = JSON.stringify({ verdict: "approve", summary: "Anchors checked", items: [] });
const REVISE = JSON.stringify({ verdict: "revise", summary: "Tighten scope", items: [
  { file: "a.ts", severity: "warning", summary: "Too broad", rationale: "Read a.ts" }] });
const fixtures = JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "fixtures", "claude-api-error-frames.json"), "utf8")) as
  { api_retry: Record<string, unknown>; assistant_error: Record<string, unknown>; result_error: Record<string, unknown> };

function claudeClaim(overrides: Partial<ClaimResponse> = {}, cross: Partial<NonNullable<ClaimResponse["cross_check"]>> = {}): ClaimResponse {
  return {
    kind: "cross_check", run_id: "check", issue_iid: null, issue_title: "Issue",
    repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/owner/repo", clone_url: "https://example.test/owner/repo.git", default_branch: "main" },
    last_seq: 0, agents: [], issue_description: "Untrusted issue", claim_generation: 7,
    config: { default_model: "sonnet", default_effort: "high" },
    secrets: { forge_pat: "", anthropic_oauth_token: TOKEN },
    cross_check: { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest",
      deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan", milestones: [], required_capabilities: [],
      required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff", ...cross },
    ...overrides,
  } as ClaimResponse;
}

const initFrame = (over: Record<string, unknown> = {}) => ({
  type: "system", subtype: "init", session_id: "s", model: "sonnet", tools: ["Read", "Grep", "Glob"],
  mcp_servers: [], plugins: [], agents: ["claude", "general-purpose", "statusline-setup"], skills: ["doctor"], ...over,
});
const textFrame = (text: string) => ({ type: "assistant", parent_tool_use_id: null, message: { role: "assistant", content: [{ type: "text", text }] } });
const toolFrame = () => ({ type: "assistant", parent_tool_use_id: null, message: { role: "assistant", content: [{ type: "tool_use", id: "t", name: "Read", input: {} }] } });
const resultFrame = (result: string, over: Record<string, unknown> = {}) => ({ type: "result", subtype: "success", is_error: false, num_turns: 1, result, ...over });
// The SDK's typed error classification (sdk.d.ts SDKAssistantMessageError) on a real recorded frame shape.
const errorFrame = (error: SDKAssistantMessageError, over: Record<string, unknown> = {}) => ({ ...fixtures.assistant_error, error, ...over });

function queryOf(frames: unknown[], seen?: { options?: SdkOptions; prompts?: string[] }): SdkQueryFn {
  return ((params: { prompt: AsyncIterable<unknown>; options: SdkOptions }) => {
    if (seen) seen.options = params.options;
    return (async function* () { for (const f of frames) yield f; })();
  }) as unknown as SdkQueryFn;
}

function rig(frames: unknown[], opts: { claim?: ClaimResponse; kill?: (pid: number | undefined) => boolean } = {}) {
  const secrets = new Set<string>();
  const added: string[] = [];
  const usage: Record<string, unknown>[] = [];
  const log = { ...nullLogger(), addSecret: (s: string) => { added.push(s); secrets.add(s); }, removeSecret: (s: string) => { secrets.delete(s); }, child() { return log; } } as Logger;
  const seen: { options?: SdkOptions } = {};
  const checker = new ClaudeCrossCheck(log, { secretPaths: ["/run/secrets/"], queryFn: queryOf(frames, seen), kill: opts.kill });
  const controller = new AbortController();
  return { checker, secrets, added, usage, seen, controller,
    run: () => checker.run(opts.claim ?? claudeClaim(), "/checkout", "/home", controller.signal, async (p) => { usage.push(p); }) };
}

function scratch(): { root: string; checkout: string; home: string } {
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "claude-checker-"));
  const checkout = path.join(root, "checkout");
  const home = path.join(root, "home");
  fs.mkdirSync(checkout); fs.mkdirSync(home);
  fs.writeFileSync(path.join(checkout, "a.ts"), "export const a = 1;\n");
  return { root, checkout, home };
}

/** Run the composed PreToolUse list the way the SDK does: every matching matcher, first deny wins. */
async function decide(options: SdkOptions, input: Record<string, unknown>): Promise<"allow" | string> {
  const tool = String(input["tool_name"]);
  for (const m of options.hooks!.PreToolUse as HookCallbackMatcher[]) {
    if (m.matcher !== undefined && !new RegExp(`^(?:${m.matcher})$`).test(tool)) continue;
    for (const hook of m.hooks) {
      const out = await hook({ hook_event_name: "PreToolUse", tool_use_id: "u", session_id: "s", transcript_path: "/x", cwd: String(input["cwd"] ?? ""), ...input } as HookInput,
        undefined, { signal: new AbortController().signal }) as HookJSONOutput;
      const specific = (out as { hookSpecificOutput?: { permissionDecision?: string; permissionDecisionReason?: string } }).hookSpecificOutput;
      if (specific?.permissionDecision === "deny") return specific.permissionDecisionReason ?? "deny";
    }
  }
  return "allow";
}

describe("buildClaudeCrossCheckOptions: the checker's confinement", () => {
  const build = (s: ReturnType<typeof scratch>, gate: InitGate = { passed: true }) => buildClaudeCrossCheckOptions({
    cwd: s.checkout, homeDir: s.home, oauthToken: TOKEN, log: nullLogger(), secretPaths: ["/run/secrets/"], gate, brief: "brief",
  });

  it("is read-only by construction: tools, denials, no settings, MCP, plugins, skills or agents", () => {
    const s = scratch();
    try {
      const o = build(s);
      assert.deepEqual(o.tools, ["Read", "Grep", "Glob"]);
      assert.equal(o.allowedTools, undefined, "allowedTools would not restrict under bypassPermissions");
      for (const t of ["Bash", "BashOutput", "KillShell", "Monitor", "WebFetch", "WebSearch", "Agent", "Task", "Skill", "NotebookEdit", "MultiEdit", "TodoWrite", "ToolSearch"]) {
        assert.ok(o.disallowedTools!.includes(t), `${t} is disallowed`);
      }
      for (const t of ["Read", "Grep", "Glob"]) assert.ok(!o.disallowedTools!.includes(t));
      assert.deepEqual(o.settingSources, []);
      assert.equal(o.strictMcpConfig, true);
      assert.deepEqual(o.mcpServers, {});
      assert.deepEqual(o.plugins, []);
      assert.deepEqual(o.skills, []);
      assert.deepEqual(o.agents, {});
      assert.equal(o.permissionMode, "bypassPermissions");
      assert.equal(o.cwd, s.checkout);
      assert.ok(Number.isInteger(o.maxTurns) && o.maxTurns! > 0 && o.maxTurns! <= 100, "a bounded turn budget");
      assert.equal(typeof o.spawnClaudeCodeProcess, "function", "the runner-uid detached spawn");
      assert.deepEqual((o.settings as { enabledPlugins: Record<string, boolean> }).enabledPlugins, { "agents-md@builtin": false });
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("hands the SDK child a full-replacement env with only the Anthropic token and the private HOME", () => {
    const s = scratch();
    try {
      const env = build(s).env as Record<string, string | undefined>;
      assert.equal(env["CLAUDE_CODE_OAUTH_TOKEN"], TOKEN);
      assert.equal(env["HOME"], s.home);
      assert.equal(env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"], "1");
      assert.equal(env["ANTHROPIC_API_KEY"], undefined);
      assert.equal(env["UZI_WORKER_TOKEN"], undefined);
      assert.ok(!Object.keys(env).some((k) => /FORGE|CODEX|WORKER/i.test(k)));
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("composes exactly ONE path guard, then the pattern guard, with no second guard to undo a denial", () => {
    const s = scratch();
    try {
      const matchers = build(s).hooks!.PreToolUse as HookCallbackMatcher[];
      assert.equal(matchers.filter((m) => m.matcher === "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep").length, 1, "one path guard");
      assert.equal(matchers[0]!.matcher, undefined, "the matcher-less tool gate runs first");
      assert.equal(matchers.at(-1)!.matcher, "Glob|Grep", "the checker's own pattern guard is appended last");
      assert.equal(matchers.length, 4, "gate, path guard, sources write guard, pattern guard");
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("denies every tool until the init frame is verified, and any tool outside Read/Grep/Glob always", async () => {
    const s = scratch();
    try {
      const closed = build(s, { passed: false });
      assert.match(await decide(closed, { tool_name: "Read", tool_input: { file_path: path.join(s.checkout, "a.ts") }, cwd: s.checkout }), /not been verified/);
      const open = build(s);
      assert.equal(await decide(open, { tool_name: "Read", tool_input: { file_path: path.join(s.checkout, "a.ts") }, cwd: s.checkout }), "allow");
      for (const tool of ["Bash", "Write", "Edit", "WebFetch", "Agent", "mcp__x__y"]) {
        assert.match(await decide(open, { tool_name: tool, tool_input: { file_path: path.join(s.checkout, "a.ts"), command: "id" }, cwd: s.checkout }), /not in the isolated run's fixed tool set/, tool);
      }
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("refuses a Read outside the checkout, of a worker secret path and of the run secrets dir (acceptance 3)", async () => {
    const s = scratch();
    try {
      const o = build(s);
      const read = (p: string) => decide(o, { tool_name: "Read", tool_input: { file_path: p }, cwd: s.checkout });
      assert.equal(await read(path.join(s.checkout, "a.ts")), "allow");
      assert.notEqual(await read("/etc/passwd"), "allow");
      assert.notEqual(await read(path.join(s.root, "outside.txt")), "allow", "a sibling of the checkout");
      assert.notEqual(await read("../outside.txt"), "allow");
      assert.notEqual(await read("/run/secrets/worker_token"), "allow", "the worker credential prefix");
      assert.notEqual(await read("/run/uzi-secrets/anything"), "allow", "the run secrets dir");
      fs.symlinkSync("/etc", path.join(s.checkout, "link"));
      assert.notEqual(await read(path.join(s.checkout, "link", "passwd")), "allow", "a symlink out of the checkout");
      const grep = (p: string) => decide(o, { tool_name: "Grep", tool_input: { pattern: "x", path: p }, cwd: s.checkout });
      assert.notEqual(await grep("/etc"), "allow");
      assert.notEqual(await decide(o, { tool_name: "Glob", tool_input: { pattern: "*", path: "/etc" }, cwd: s.checkout }), "allow");
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("lets Read open only this session's own authenticated SDK spill file under the SDK HOME", async () => {
    const s = scratch();
    try {
      const o = build(s);
      const slug = "-checkout";
      const session = "11111111-2222-3333-4444-555555555555";
      const projects = path.join(s.home, ".claude", "projects");
      const spillDir = path.join(projects, slug, session, "tool-results");
      fs.mkdirSync(spillDir, { recursive: true });
      const spill = path.join(spillDir, "out.txt");
      fs.writeFileSync(spill, "big output");
      const transcript = path.join(projects, slug, `${session}.jsonl`);
      const read = (file: string, extra: Record<string, unknown> = {}, tool = "Read") =>
        decide(o, { tool_name: tool, tool_input: { file_path: file }, cwd: s.checkout, session_id: session, transcript_path: transcript, ...extra });
      assert.equal(await read(spill), "allow", "the checker's own spill file is readable");
      assert.notEqual(await read(spill, { session_id: "99999999-2222-3333-4444-555555555555" }), "allow", "another session's spill dir");
      assert.notEqual(await read(spill, { transcript_path: "/etc/passwd" }), "allow", "an unauthenticated transcript path");
      assert.notEqual(await read(path.join(projects, slug, `${session}.jsonl`)), "allow", "the transcript itself is not a spill file");
      assert.notEqual(await read(path.join(s.home, ".claude", ".credentials.json")), "allow", "other files under the SDK HOME");
      assert.notEqual(await read(spill, {}, "Grep"), "allow", "the allowance is Read-only");
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("keeps the shared helper's default: without sdkHomeDir the same spill file is NOT readable", async () => {
    const s = scratch();
    try {
      const session = "11111111-2222-3333-4444-555555555555";
      const projects = path.join(s.home, ".claude", "projects");
      const spillDir = path.join(projects, "-checkout", session, "tool-results");
      fs.mkdirSync(spillDir, { recursive: true });
      fs.writeFileSync(path.join(spillDir, "out.txt"), "big output");
      const surface: IsolationSurface = { tools: ["Read", "Grep", "Glob"], mcpServers: [], plugins: [], skills: [] };
      const matchers = (extra: { sdkHomeDir?: string }) => ({ hooks: { PreToolUse: buildIsolatedPreToolUse({
        surface, gate: { passed: true }, cwd: s.checkout, log: nullLogger(), secretPaths: ["/srv/secret-dir/"], ...extra }) } }) as SdkOptions;
      const input = { tool_name: "Read", tool_input: { file_path: path.join(spillDir, "out.txt") }, cwd: s.checkout, session_id: session,
        transcript_path: path.join(projects, "-checkout", `${session}.jsonl`) };
      assert.notEqual(await decide(matchers({}), input), "allow");
      assert.equal(await decide(matchers({ sdkHomeDir: s.home }), input), "allow");
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });

  it("denies an absolute, home-relative or '..' Glob pattern and Grep glob, but allows a relative one", async () => {
    const s = scratch();
    try {
      const o = build(s);
      const glob = (pattern: string) => decide(o, { tool_name: "Glob", tool_input: { pattern }, cwd: s.checkout });
      const grepGlob = (g: string) => decide(o, { tool_name: "Grep", tool_input: { pattern: "x", glob: g }, cwd: s.checkout });
      for (const ok of ["**/*.ts", "src/**", "*.{ts,tsx}", "a..b/*.ts", "docs/..hidden", "src/{a,b}/{c,{d,e}}/*.md", "{x}.ts", "{a,b}{c,d}{e,f}{g,h}{i,j}{k,l}{m,n}{o,p}", "{x}".repeat(32), `{${Array.from({ length: 256 }, (_, i) => i).join(",")}}`]) assert.equal(await glob(ok), "allow", ok);
      for (const bad of ["/etc/*", "../*", "a/../../b", "~/.ssh/*", "{a,/etc/*}", "{x,../y}", "..", "\\etc\\*", "{a/,}../etc/*", ".{.,}/*", "{.,}./*", "src/{.,}./x", "{a,.}./*", ".{x,}./*", "{.,x}{y,.}/*", "{a,.}{.,b}/etc/*", "{x,{.,}.}/*", "{a,b", "a}b", "{a,b}{c,d}{e,f}{g,h}{i,j}{k,l}{m,n}{o,p}{q,r}", "{.,x\\{}{.,y\\}}/*", "a\\b", "{x}".repeat(33), "a".repeat(1025), `{${Array.from({ length: 257 }, (_, i) => i).join(",")}}`]) {
        assert.match(await glob(bad), /relative to the checkout/, bad);
      }
      for (const bad of ["*.ts /etc/**", "*.ts ../x", "*.ts\t~/.ssh/*", "*.ts\n/etc/**", "*.ts,../x"]) assert.match(await grepGlob(bad), /relative to the checkout/, JSON.stringify(bad));
      assert.equal(await grepGlob("*.ts *.md"), "allow");
      assert.equal(await grepGlob("*.ts"), "allow");
      assert.match(await grepGlob("/etc/**"), /relative to the checkout/);
      assert.match(await grepGlob("../**"), /relative to the checkout/);
    } finally { fs.rmSync(s.root, { recursive: true, force: true }); }
  });
});

describe("ClaudeCrossCheck.run", () => {
  it("returns the verdict text, posts the init and result usage frames and releases the token secret", async () => {
    const r = rig([initFrame(), toolFrame(), textFrame("thinking"), resultFrame(APPROVE, { modelUsage: { sonnet: { inputTokens: 1 } } })]);
    assert.equal(await r.run(), APPROVE);
    assert.deepEqual(r.usage.map((u) => u["event"]), ["init", "result"]);
    assert.equal(r.usage[0]!["harness"], "claude");
    assert.equal(r.usage[1]!["is_error"], false);
    assert.deepEqual(r.added, [TOKEN]);
    assert.equal(r.secrets.size, 0, "the token is no longer registered once the check ends");
    assert.equal(r.seen.options!.model, "sonnet");
    assert.equal(r.seen.options!.effort, "high");
  });

  it("passes the nonce-fenced prompt and a brief naming Read, Grep and Glob", async () => {
    const r = rig([initFrame(), resultFrame(APPROVE)]);
    let prompt = "";
    const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], queryFn: ((p: { prompt: AsyncIterable<unknown> }) => {
      return (async function* () {
        for await (const f of p.prompt) prompt += JSON.stringify(f);
        for (const f of [initFrame(), resultFrame(APPROVE)]) yield f;
      })();
    }) as unknown as SdkQueryFn });
    await checker.run(claudeClaim(), "/checkout", "/home", r.controller.signal, async () => {});
    assert.match(prompt, /candidate_[a-f0-9]{32}/);
    assert.match(prompt, /issue_body_[a-f0-9]{32}/);
    let brief = "";
    const probe = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], queryFn: ((p: { options: SdkOptions }) => {
      brief = JSON.stringify(p.options.systemPrompt);
      return (async function* () { yield initFrame(); yield resultFrame(APPROVE); })();
    }) as unknown as SdkQueryFn });
    await probe.run(claudeClaim(), "/checkout", "/home", r.controller.signal, async () => {});
    assert.match(brief, /Read, Grep and Glob/);
    assert.doesNotMatch(brief, /Read and Search/);
  });

  describe("confinement latch", () => {
    for (const [name, init] of [
      ["an extra tool", initFrame({ tools: ["Read", "Grep", "Glob", "Bash"] })],
      ["a missing tool", initFrame({ tools: ["Read", "Grep"] })],
      ["an MCP server", initFrame({ mcp_servers: [{ name: "x", status: "connected" }] })],
      ["a plugin", initFrame({ plugins: [{ name: "p" }] })],
      ["an unexpected agent", initFrame({ agents: ["claude", "evil"] })],
      ["an unexpected skill", initFrame({ skills: ["doctor", "evil"] })],
    ] as const) {
      it(`fails with a confinement error on an init frame with ${name}`, async () => {
        const r = rig([init, textFrame("never consumed"), resultFrame(APPROVE)]);
        await assert.rejects(r.run(), (err: Error) => /confinement/.test(err.message) && !(err instanceof CrossCheckCheckerUnavailableError));
        assert.deepEqual(r.usage.map((u) => u["event"]).filter((e) => e === "result").length, 0, "no verdict usage for a refused session");
        assert.equal(r.secrets.size, 0);
      });
    }

    it("refuses model output that arrives before a passing init frame", async () => {
      await assert.rejects(rig([textFrame("early"), initFrame(), resultFrame(APPROVE)]).run(), /confinement/);
      await assert.rejects(rig([resultFrame(APPROVE)]).run(), /confinement.*no init frame/, "a result is not a substitute for a verified init");
      await assert.rejects(rig([]).run(), /confinement.*no init frame/);
    });
  });

  describe("verdict validation", () => {
    for (const [name, text] of [
      ["prose around the JSON", `Here you go: ${APPROVE}`],
      ["a fenced JSON", "```json\n" + APPROVE + "\n```"],
      ["an unknown verdict", JSON.stringify({ verdict: "maybe", summary: "x", items: [] })],
      ["an extra key", JSON.stringify({ verdict: "approve", summary: "x", items: [], extra: 1 })],
      ["a bad finding severity", JSON.stringify({ verdict: "revise", summary: "x", items: [{ file: "a", severity: "bad", summary: "s", rationale: "r" }] })],
      ["more than twenty findings", JSON.stringify({ verdict: "revise", summary: "x", items: Array.from({ length: 21 }, () => ({ file: "a", severity: "info", summary: "s", rationale: "r" })) })],
      ["an oversized text", JSON.stringify({ verdict: "approve", summary: "x".repeat(70 * 1024), items: [] })],
      ["an empty text", ""],
    ] as const) {
      it(`raises CrossCheckMalformedError for ${name}`, async () => {
        await assert.rejects(rig([initFrame(), resultFrame(text)]).run(), CrossCheckMalformedError);
      });
    }

    it("validates only the final result text, not intermediate assistant text", async () => {
      assert.equal(await rig([initFrame(), textFrame("not json at all"), textFrame("{broken"), resultFrame(REVISE)]).run(), REVISE);
    });

    it("treats an error result as a model error, not a verdict or an unavailable checker", async () => {
      const err = await rig([initFrame(), resultFrame(APPROVE, { subtype: "error_max_turns", is_error: true })]).run().catch((e: unknown) => e);
      assert.ok(err instanceof Error && !(err instanceof CrossCheckMalformedError) && !(err instanceof CrossCheckCheckerUnavailableError));
    });
  });

  describe("credentials", () => {
    it("raises CrossCheckCheckerUnavailableError when the claim has no Anthropic token", async () => {
      const claim = claudeClaim({ secrets: { forge_pat: "" } as ClaimResponse["secrets"] });
      await assert.rejects(rig([initFrame(), resultFrame(APPROVE)], { claim }).run(), CrossCheckCheckerUnavailableError);
      const blank = claudeClaim({ secrets: { forge_pat: "", anthropic_oauth_token: "  " } as ClaimResponse["secrets"] });
      await assert.rejects(rig([initFrame(), resultFrame(APPROVE)], { claim: blank }).run(), CrossCheckCheckerUnavailableError);
    });

    it("refuses a claim that also carries a Codex credential, before any session", async () => {
      const claim = claudeClaim({ secrets: { forge_pat: "", anthropic_oauth_token: TOKEN,
        codex: { auth_mode: "api_key", access_token: "t", capability: "c" } } as ClaimResponse["secrets"] });
      const r = rig([initFrame(), resultFrame(APPROVE)], { claim });
      await assert.rejects(r.run(), /refuses a Codex credential/);
      assert.equal(r.seen.options, undefined, "no query started");
      assert.deepEqual(r.added, [], "the token was never registered");
    });

    it("raises CrossCheckCheckerUnavailableError for a pinned effort the SDK cannot take", async () => {
      const claim = claudeClaim({ config: { default_model: "sonnet", default_effort: "extreme" as never } }, { effort_source: "pin" });
      await assert.rejects(rig([initFrame(), resultFrame(APPROVE)], { claim }).run(), CrossCheckCheckerUnavailableError);
    });
  });

  describe("pinned-model rejection (the SDK's typed model_not_found classification)", () => {
    const pinned = claudeClaim({}, { model_source: "pin" });
    const unpinned = claudeClaim({}, { model_source: "worker default" });
    const resultErr = { ...fixtures.result_error, api_error_status: 404 };

    it("maps model_not_found on a PINNED model, before any model content, to checker unavailable", async () => {
      const r = rig([initFrame(), fixtures.api_retry, errorFrame("model_not_found"), resultErr], { claim: pinned });
      await assert.rejects(r.run(), CrossCheckCheckerUnavailableError);
      assert.equal(r.secrets.size, 0);
    });

    it("keeps model_error for the same frame on a worker-default (unpinned) model", async () => {
      for (const claim of [unpinned, claudeClaim()]) {
        const err = await rig([initFrame(), errorFrame("model_not_found"), resultErr], { claim }).run().catch((e: unknown) => e);
        assert.ok(err instanceof Error && !(err instanceof CrossCheckCheckerUnavailableError), "model_error, not unavailable");
      }
    });

    it("keeps model_error for every other typed error value, even when pinned", async () => {
      for (const error of ["authentication_failed", "oauth_org_not_allowed", "account_on_hold", "verification_required", "billing_error", "rate_limit",
        "overloaded", "invalid_request", "server_error", "unknown", "max_output_tokens", "cloud_credential_error"] as const satisfies readonly SDKAssistantMessageError[]) {
        const err = await rig([initFrame(), errorFrame(error), resultErr], { claim: pinned }).run().catch((e: unknown) => e);
        assert.ok(err instanceof Error && !(err instanceof CrossCheckCheckerUnavailableError), error);
      }
    });

    it("never classifies from an HTTP status plus model-name prose without the typed classification", async () => {
      const prose = { ...fixtures.assistant_error, error: undefined,
        message: { ...(fixtures.assistant_error["message"] as object), content: [{ type: "text", text: "API Error: 404 model: definitely-not-a-model not found" }] } };
      for (const status of [400, 404]) {
        const err = await rig([initFrame(), prose, { ...fixtures.result_error, api_error_status: status, result: "API Error: model not found" }], { claim: pinned }).run().catch((e: unknown) => e);
        assert.ok(err instanceof Error && !(err instanceof CrossCheckCheckerUnavailableError), String(status));
      }
      const unclassified = errorFrame("invalid_request", { message: { ...(fixtures.assistant_error["message"] as object), content: [{ type: "text", text: "model_not_found: the model does not exist" }] } });
      const err = await rig([initFrame(), unclassified, resultErr], { claim: pinned }).run().catch((e: unknown) => e);
      assert.ok(err instanceof Error && !(err instanceof CrossCheckCheckerUnavailableError), "prose naming model_not_found under another typed error");
    });

    it("does not map model_not_found seen after real assistant content, or on a subagent frame, or as a retry notice", async () => {
      const after = await rig([initFrame(), textFrame("I started"), errorFrame("model_not_found"), resultErr], { claim: pinned }).run().catch((e: unknown) => e);
      assert.ok(after instanceof Error && !(after instanceof CrossCheckCheckerUnavailableError), "after content");
      assert.equal(await rig([initFrame(), errorFrame("model_not_found", { parent_tool_use_id: "toolu_1" }), resultFrame(APPROVE)], { claim: pinned }).run(),
        APPROVE, "a subagent frame is not the lead's classification");
      const retry = { ...fixtures.api_retry, error: "model_not_found" };
      assert.equal(await rig([initFrame(), retry, resultFrame(APPROVE)], { claim: pinned }).run(), APPROVE, "an api_retry notice is not terminal");
    });
  });

  describe("lifecycle", () => {
    it("kills the spawned process group and balances the secret on a cancelled check", async () => {
      const killed: (number | undefined)[] = [];
      const controller = new AbortController();
      const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], kill: (pid) => { killed.push(pid); return true; },
        spawn: () => ({ pid: 4242 }),
        queryFn: ((p: { options: SdkOptions }) => (async function* () {
          p.options.spawnClaudeCodeProcess!({ command: "claude", args: [], env: {}, signal: new AbortController().signal } as never);
          yield initFrame();
          controller.abort(new Error("cross-check timed out"));
          yield textFrame("late");
          yield resultFrame(APPROVE);
        })()) as unknown as SdkQueryFn });
      await assert.rejects(checker.run(claudeClaim(), "/checkout", "/home", controller.signal, async () => {}));
      assert.deepEqual(killed, [4242]);
    });

    it("runs the canned stub query to a passing APPROVE (UZI_EXECUTOR=stub)", async () => {
      const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], queryFn: stubClaudeCrossCheckQueryFn });
      const text = await checker.run(claudeClaim(), "/checkout", "/home", new AbortController().signal, async () => {});
      assert.equal(JSON.parse(text).verdict, "approve");
    });
  });
});

describe("CrossCheckRunner dispatch on the claim's credential family (PRD #2460)", () => {
  async function exercise(claim: ClaimResponse, deps: { model?: boolean; claudeText?: string | Error } = {}) {
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "runner-claude-"));
    const decisions: any[] = [];
    const states: any[] = [];
    const calls = { codex: 0, claude: 0 };
    const client = {
      reportState: async (_id: string, body: unknown) => { states.push(body); return { applied: true }; },
      getInputs: async () => ({ inputs: [] }),
      postMessages: async () => undefined,
      reportCrossCheckVerdict: async (_id: string, _g: number, v: unknown) => { decisions.push(v); },
    } as unknown as WorkerClient;
    const git = { ensureClone: async () => "/bare", runnerCloneAtCommit: async () => "/checkout", removeRunnerClone: async () => {} } as unknown as GitCache;
    try {
      await new CrossCheckRunner(client, git, nullLogger(), { homeRoot: root, pollMs: 1, modelTimeoutMs: 1000,
        model: { run: async () => { calls.codex++; return APPROVE; } },
        claudeModel: { run: async () => {
          calls.claude++;
          if (deps.claudeText instanceof Error) throw deps.claudeText;
          return deps.claudeText ?? APPROVE;
        } } }).execute(claim);
    } finally { fs.rmSync(root, { recursive: true, force: true }); }
    return { decisions, states, calls };
  }
  const codexSecrets = { auth_mode: "api_key", access_token: "claim-token", capability: "cap" };

  it("runs the Claude checker for an Anthropic-only claim and delivers APPROVE and REVISE", async () => {
    const approve = await exercise(claudeClaim());
    assert.deepEqual(approve.calls, { codex: 0, claude: 1 });
    assert.equal(approve.decisions.length, 1);
    assert.equal(approve.decisions[0].verdict, "approve");
    assert.equal(approve.states.at(-1).status, "completed");
    const revise = await exercise(claudeClaim(), { claudeText: REVISE });
    assert.equal(revise.decisions[0].verdict, "revise");
    assert.equal(revise.decisions[0].items.length, 1);
    assert.equal(revise.states.at(-1).status, "completed");
  });

  it("settles malformed, unavailable and confinement failures with their reason classes", async () => {
    const malformed = await exercise(claudeClaim(), { claudeText: "prefix " + APPROVE });
    assert.equal(malformed.decisions.at(-1).verdict, "failed");
    assert.equal(malformed.decisions.at(-1).reason_class, "malformed");
    const typed = await exercise(claudeClaim(), { claudeText: new CrossCheckMalformedError("cross-check invalid verdict schema") });
    assert.equal(typed.decisions.at(-1).reason_class, "malformed");
    const unavailable = await exercise(claudeClaim(), { claudeText: new CrossCheckCheckerUnavailableError() });
    assert.equal(unavailable.decisions.at(-1).reason_class, "checker_unavailable");
    assert.equal(unavailable.states.at(-1).failure_reason, "plan cross-check: checker unavailable");
    const confinement = await exercise(claudeClaim(), { claudeText: new Error("cross-check confinement refused: effective tool set differs") });
    assert.equal(confinement.decisions.at(-1).reason_class, "confinement_failed");
    assert.equal(confinement.states.at(-1).status, "failed");
  });

  it("with the real Claude checker, a missing token settles checker unavailable and a confinement refusal settles confinement_failed", async () => {
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "runner-claude-real-"));
    try {
      for (const [claim, frames, reason] of [
        [claudeClaim({ secrets: { forge_pat: "" } as ClaimResponse["secrets"] }), [initFrame(), resultFrame(APPROVE)], "invalid"],
        [claudeClaim(), [initFrame({ tools: ["Read", "Bash"] }), resultFrame(APPROVE)], "confinement_failed"],
        [claudeClaim(), [initFrame(), resultFrame(APPROVE)], "approve"],
      ] as const) {
        const decisions: any[] = [];
        const client = { reportState: async () => ({ applied: true }), getInputs: async () => ({ inputs: [] }), postMessages: async () => undefined,
          reportCrossCheckVerdict: async (_i: string, _g: number, v: unknown) => { decisions.push(v); } } as unknown as WorkerClient;
        const git = { ensureClone: async () => "/bare", runnerCloneAtCommit: async () => "/checkout", removeRunnerClone: async () => {} } as unknown as GitCache;
        await new CrossCheckRunner(client, git, nullLogger(), { homeRoot: root, pollMs: 1, modelTimeoutMs: 1000, claudeQueryFn: queryOf([...frames]) }).execute(claim);
        if (reason === "invalid") {
          // A claim with neither family is refused before any model call.
          assert.equal(decisions.at(-1).verdict, "failed");
        } else assert.equal(decisions.at(-1).reason_class, reason);
      }
    } finally { fs.rmSync(root, { recursive: true, force: true }); }
  });

  it("refuses a claim carrying both families, and a claim carrying neither, without running any checker", async () => {
    const both = await exercise(claudeClaim({ secrets: { forge_pat: "", anthropic_oauth_token: TOKEN, codex: codexSecrets } as ClaimResponse["secrets"] }));
    assert.deepEqual(both.calls, { codex: 0, claude: 0 });
    assert.equal(both.decisions.at(-1).verdict, "failed");
    const neither = await exercise(claudeClaim({ secrets: { forge_pat: "" } as ClaimResponse["secrets"] }));
    assert.deepEqual(neither.calls, { codex: 0, claude: 0 });
    assert.equal(neither.decisions.at(-1).verdict, "failed");
  });

  it("keeps the Codex path for a Codex-only claim, and refuses a Codex claim that also holds an Anthropic token", async () => {
    const codex = await exercise(claudeClaim({ secrets: { forge_pat: "", codex: codexSecrets } as ClaimResponse["secrets"] }));
    assert.deepEqual(codex.calls, { codex: 1, claude: 0 });
    assert.equal(codex.decisions[0].verdict, "approve");
    const leaked = await exercise(claudeClaim({ secrets: { forge_pat: "", anthropic_oauth_token: TOKEN, codex: codexSecrets } as ClaimResponse["secrets"] }));
    assert.deepEqual(leaked.calls, { codex: 0, claude: 0 });
  });
});
