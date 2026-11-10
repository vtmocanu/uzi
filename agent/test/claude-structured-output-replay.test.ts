// Replays the REAL Claude Agent SDK frames recorded in fixtures/claude-structured-output-0.3.316.json
// (see claude-structured-output.probe.ts) through the production ClaudeCrossCheck: the real built
// options, the real composed PreToolUse hooks, the real init check and tool budget. This file imports
// only symbols that exist without the structured-output carrier support, so it can be run against an
// older tree to see which runtime failure the carrier causes there.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { HookInput, HookJSONOutput, Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";
import { ClaudeCrossCheck } from "../src/claude-cross-check.js";
import { CODE_VERDICT_SCHEMA, CrossCheckMalformedError, PLAN_VERDICT_SCHEMA, validateCodeVerdict, validateVerdict } from "../src/codex/cross-check.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { check } from "./json-schema-subset.js";

interface Session { stage: "plan" | "code"; threw: string | null; frames: Array<Record<string, unknown>>; hook_inputs: Array<Record<string, unknown>> }
const fixture = JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "fixtures", "claude-structured-output-0.3.316.json"), "utf8")) as
  { sessions: Record<string, Session> };

const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
fs.mkdirSync(scratch, { recursive: true });

const session = (name: string): Session => fixture.sessions[name] as Session;
const initOf = (s: Session): Record<string, unknown> => s.frames.find(f => f["type"] === "system" && f["subtype"] === "init") as Record<string, unknown>;
// The carrier name as the real SDK reported it: the init tool that is not one of the checker's own.
const CARRIER = ((): string => {
  const extra = (initOf(session("plan_retry"))["tools"] as string[]).filter(t => !["Read", "Grep", "Glob"].includes(t));
  assert.equal(extra.length, 1);
  return extra[0] as string;
})();

function claim(stage: "plan" | "code"): ClaimResponse {
  const base = {
    kind: "cross_check", run_id: "child", claim_generation: 7, last_seq: 0, agents: [],
    issue_iid: null, issue_title: "Issue", issue_description: "Issue DATA",
    repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo", clone_url: "https://example.test/repo.git", default_branch: "main" },
    config: { default_model: "sonnet", default_effort: "high" },
    secrets: { forge_pat: "", anthropic_oauth_token: "fixture-token" },
  };
  const cross = stage === "code"
    ? { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64), base_commit: "a".repeat(40), head_commit: "b".repeat(40),
      plan_md: "Approved plan DATA", guidance_snapshot: "", milestones: [], required_capabilities: [], required_tools: [], size_class: "small",
      code_context: { task: "context DATA" }, code_diff: "diff DATA", deadline_at: "2099-01-01T00:00:00Z" }
    : { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest", deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan",
      milestones: [], required_capabilities: [], required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff" };
  return { ...base, cross_check: cross } as unknown as ClaimResponse;
}

async function withCheckout<T>(fn: (checkout: string, home: string) => Promise<T>): Promise<T> {
  const root = fs.mkdtempSync(path.join(scratch, "so-replay-"));
  try {
    fs.mkdirSync(path.join(root, "checkout"), { recursive: true });
    const checkout = fs.realpathSync(path.join(root, "checkout"));
    const home = path.join(root, "home");
    fs.mkdirSync(home, { recursive: true });
    fs.writeFileSync(path.join(checkout, "a.ts"), "export const answer = 42;\n");
    return await fn(checkout, home);
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
}

function fill<T>(v: T, checkout: string, home: string): T {
  const esc = (p: string): string => JSON.stringify(p).slice(1, -1);
  return JSON.parse(JSON.stringify(v).split("<checkout>").join(esc(checkout)).split("<home>").join(esc(home))) as T;
}

/** Runs the real composed PreToolUse hooks (SDK matcher semantics, every matching hook, any deny wins). */
async function composedDecision(options: SdkOptions, recorded: Record<string, unknown>, checkout: string): Promise<string | undefined> {
  const name = String(recorded["tool_name"]);
  for (const matcher of options.hooks!.PreToolUse!) {
    if (matcher.matcher && !new RegExp(`^(?:${matcher.matcher})$`).test(name)) continue;
    for (const fn of matcher.hooks) {
      const out = await fn({ ...recorded, cwd: checkout, session_id: "s", transcript_path: "/unused" } as unknown as HookInput, undefined,
        { signal: new AbortController().signal }) as HookJSONOutput;
      if ("hookSpecificOutput" in out && out.hookSpecificOutput && "permissionDecision" in out.hookSpecificOutput &&
        out.hookSpecificOutput.permissionDecision === "deny") return out.hookSpecificOutput.permissionDecisionReason ?? "deny";
    }
  }
  return undefined;
}

type Outcome = { text: string; kind?: undefined; outputFormat: string } | { text?: undefined; kind: string; err: unknown; outputFormat: string };

async function replay(name: string): Promise<Outcome> {
  const s = session(name);
  const schema = s.stage === "code" ? CODE_VERDICT_SCHEMA : PLAN_VERDICT_SCHEMA;
  return withCheckout(async (checkout, home) => {
    let outputFormat = "not seen";
    const query = ((p: { options: SdkOptions }) => (async function* () {
      outputFormat = JSON.stringify(p.options.outputFormat) === JSON.stringify({ type: "json_schema", schema }) ? "ok" : `MISMATCH ${JSON.stringify(p.options.outputFormat)?.slice(0, 80)}`;
      const hooks = new Map(fill(s.hook_inputs, checkout, home).map(h => [String(h["tool_use_id"]), h] as const));
      for (const frame of fill(s.frames, checkout, home)) {
        if (frame["type"] === "assistant") {
          const content = (frame["message"] as { content: Array<Record<string, unknown>> }).content;
          for (const block of content) {
            if (block["type"] !== "tool_use") continue;
            const recorded = hooks.get(String(block["id"]));
            assert.ok(recorded, `no recorded hook input for ${String(block["id"])}`);
            const denied = await composedDecision(p.options, recorded, checkout);
            if (denied !== undefined) throw new Error(`carrier refused: ${String(block["name"])}: ${denied}`);
          }
        }
        yield frame;
      }
    })()) as unknown as SdkQueryFn;
    const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], queryFn: query });
    try {
      return { text: await checker.run(claim(s.stage), checkout, home, new AbortController().signal, async () => {}), outputFormat };
    } catch (err) {
      const sub = err instanceof CrossCheckMalformedError ? err.subreason : undefined;
      const e = err as Error;
      return { kind: `${e.constructor.name}/${sub ?? e.message}`, err, outputFormat };
    }
  });
}

const expectOk = (o: Outcome): string => {
  assert.equal(o.kind, undefined, `failure kind: ${o.kind}; outputFormat: ${o.outputFormat}`);
  assert.equal(o.outputFormat, "ok", "outputFormat was not the stage's json_schema");
  return o.text as string;
};

describe("real recorded SDK frames replayed through the production Claude checker", () => {
  it("plan_retry: a rejected then a valid StructuredOutput call returns the validated verdict", async () => {
    assert.deepEqual(session("plan_retry").hook_inputs.map(h => h["tool_name"]), ["Read", CARRIER, CARRIER]);
    const text = expectOk(await replay("plan_retry"));
    const success = session("plan_retry").frames.find(f => f["type"] === "result") as Record<string, unknown>;
    assert.equal(text, JSON.stringify(success["structured_output"]));
    assert.doesNotThrow(() => validateVerdict(text));
    assert.deepEqual(Object.keys(JSON.parse(text) as object).sort(), ["items", "summary", "verdict"]);
  });

  it("code_retry: a rejected then a valid StructuredOutput call returns the validated findings", async () => {
    const text = expectOk(await replay("code_retry"));
    const success = session("code_retry").frames.find(f => f["type"] === "result") as Record<string, unknown>;
    assert.equal(text, JSON.stringify(success["structured_output"]));
    assert.doesNotThrow(() => validateCodeVerdict(text));
  });

  it("exhausted: the SDK's retry exhaustion is a malformed verdict with a fixed message", async () => {
    const o = await replay("exhausted");
    assert.equal(o.outputFormat, "ok", `outputFormat was not the stage's json_schema; failure kind: ${o.kind}`);
    assert.equal(o.kind, "CrossCheckMalformedError/structured_output_retries", `failure kind: ${o.kind}`);
    const err = o.err as Error;
    assert.ok(!/Output does not match|attempts|required property/.test(err.message), err.message);
  });
});

// The schema is a hint to the model; the validators stay authoritative over what the SDK returns.
function initOnlyResult(stage: "plan" | "code", structured: unknown): Promise<string> {
  const init = initOf(session("plan_retry"));
  const frames = [init, { type: "result", subtype: "success", is_error: false, num_turns: 1, result: "ignored", structured_output: structured }];
  const query = (() => (async function* () { yield* frames; })()) as unknown as SdkQueryFn;
  return new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], queryFn: query })
    .run(claim(stage), "/checkout", "/home", new AbortController().signal, async () => {});
}

const planItem = { file: "a.ts", severity: "info", summary: "s", rationale: "r" };
const finding = { id: "F1", severity: "minor", path: "a.ts", line: 1, title: "t", detail: "d" };
const planRejected: Array<[string, unknown]> = [
  ["summary of 4096 multibyte characters", { verdict: "approve", summary: "é".repeat(4096), items: [] }],
  ["one item over 2 KiB", { verdict: "revise", summary: "x", items: [{ ...planItem, summary: "s".repeat(1000), rationale: "r".repeat(1100) }] }],
  ["items over 32 KiB in total", { verdict: "revise", summary: "x", items: Array.from({ length: 20 }, () => ({ ...planItem, rationale: "r".repeat(1650) })) }],
];
const codeRejected: Array<[string, unknown]> = [
  ["duplicate finding ids", { findings: [finding, finding] }],
  ["findings over 32 KiB", { findings: Array.from({ length: 20 }, (_, i) => ({ ...finding, id: `F${i}`, detail: "a".repeat(1800) })) }],
];
const planAccepted: Array<[string, unknown]> = [
  ["approve", { verdict: "approve", summary: "ok", items: [] }],
  ["revise", { verdict: "revise", summary: "x", items: [{ ...planItem, severity: "warning" }] }],
];
const codeAccepted: Array<[string, unknown]> = [
  ["findings without coverage", { findings: [finding] }],
  ["valid coverage", { findings: [], coverage: [{ path: "a.ts", status: "examined", reason: "read" }] }],
  ["null coverage", { findings: [finding], coverage: null }],
  ["coverage entries with an extra key", { findings: [], coverage: [{ path: "a.ts", status: "excluded", reason: "r", extra: 1 }] }],
];

describe("schema-accepted but validator-rejected structured output", () => {
  for (const [name, v] of planRejected) it(`plan rejects: ${name}`, async () => {
    assert.equal(check(PLAN_VERDICT_SCHEMA, v), true, "case must satisfy the plan schema");
    await assert.rejects(initOnlyResult("plan", v), CrossCheckMalformedError);
  });
  for (const [name, v] of codeRejected) it(`code rejects: ${name}`, async () => {
    assert.equal(check(CODE_VERDICT_SCHEMA, v), true, "case must satisfy the code schema");
    await assert.rejects(initOnlyResult("code", v), CrossCheckMalformedError);
  });
});

describe("validator-accepted structured output is returned verbatim", () => {
  for (const [name, v] of planAccepted) it(`plan returns: ${name}`, async () => {
    assert.equal(await initOnlyResult("plan", v), JSON.stringify(v));
  });
  for (const [name, v] of codeAccepted) it(`code returns: ${name}`, async () => {
    assert.equal(await initOnlyResult("code", v), JSON.stringify(v));
  });
});
