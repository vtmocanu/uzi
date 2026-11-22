import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildRunEnvBlock, sanitizeRunEnvPromptText } from "../src/run-env-prompt.js";
import { buildCIFixPlanPrompt, buildImplementPrompt, buildPlanPrompt, buildSelfImprovePlanPrompt } from "../src/prompt.js";
import { assembleAgents, subagentsFromTemplates } from "../src/agents.js";
import { renderCodexRun } from "../src/codex/render.js";
import { sdkRunEnvInjected, type SdkCredential } from "../src/sdk-env.js";
import { commandRunEnvInjected } from "../src/codex/codex-executor.js";
import type { HarnessAgent, RunTurnRequest } from "../src/harness.js";
import type { AgentTemplate, ClaimRunEnvVar } from "../src/protocol.js";

// PRD 4416 M2: the run-variables prompt block. Every value is an obvious dummy.

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const SECRET = "dummy-secret-run-value-000000";
const PLAIN = "dummy plain \"quoted\" value";
const ESC = String.fromCharCode(0x1b);
const HOSTILE_DESC = `a${ESC}[31mred${ESC}[0m b\r\nc ‮drow⁦iso​zw ${ESC}]0;title\u0007d ${ESC}x e`;

const v = (name: string, over: Partial<ClaimRunEnvVar> = {}): ClaimRunEnvVar => ({
  var_id: UUID, scope: "repo", rev: 1, name, value: PLAIN, secret: false, description: "", ...over,
});
const VARS: ClaimRunEnvVar[] = [
  v("REGION", { description: HOSTILE_DESC }),
  v("DB_PASSWORD", { value: SECRET, secret: true, description: "database login" }),
];

function assertClean(text: string): void {
  assert.ok(!text.includes(ESC), "ESC");
  assert.ok(!/\r/.test(text), "CR");
  for (const cp of [0x202e, 0x2066, 0x200b, 0x0007]) assert.ok(!text.includes(String.fromCodePoint(cp)), `U+${cp.toString(16)}`);
  assert.ok(!text.includes("[31m") && !text.includes("title"), "an ANSI sequence body survived");
}

function assertBlockFacts(prompt: string): void {
  assert.match(prompt, /<run_env_[0-9a-f]+>/);
  assert.ok(prompt.includes("DB_PASSWORD"), "a secret's name is listed");
  assert.ok(prompt.includes("database login"));
  assert.ok(!prompt.includes(SECRET), "a secret value never appears");
  assert.ok(prompt.includes(JSON.stringify(PLAIN)), "a plain value is JSON-quoted");
  assert.ok(prompt.includes("REGION"));
  const block = prompt.slice(prompt.indexOf("<run_env_"), prompt.indexOf("</run_env_"));
  assertClean(block);
}

describe("sanitizeRunEnvPromptText", () => {
  it("removes CSI, OSC (BEL and ST) and two-character escapes whole", () => {
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}[31;1mb${ESC}[0m`), "ab");
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}]0;t\u0007b`), "ab");
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}]8;;u${ESC}\\b`), "ab");
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}Mb`), "ab");
  });

  it("replaces every unsafe code point with U+FFFD", () => {
    const unsafe = [0x00, 0x09, 0x0a, 0x0d, 0x7f, 0x85, 0x9b, 0x200b, 0x200f, 0x2028, 0x2029, 0x202a, 0x202e, 0x2060, 0x2064, 0x2066, 0x206f, 0xfeff];
    for (const cp of unsafe) assert.equal(sanitizeRunEnvPromptText(`x${String.fromCodePoint(cp)}y`), "x�y", `U+${cp.toString(16)}`);
  });

  it("also replaces the invisible characters U+061C, U+00AD, U+180E and the tag block U+E0000 to U+E007F", () => {
    for (const cp of [0x061c, 0x00ad, 0x180e, 0xe0000, 0xe0041, 0xe007f]) {
      assert.equal(sanitizeRunEnvPromptText(`x${String.fromCodePoint(cp)}y`), "x�y", `U+${cp.toString(16)}`);
    }
    // The neighbours stay: U+E0080 is outside the tag block, U+061B is an ordinary Arabic semicolon.
    for (const cp of [0xe0080, 0x061b, 0x00ae]) assert.equal(sanitizeRunEnvPromptText(`x${String.fromCodePoint(cp)}y`), `x${String.fromCodePoint(cp)}y`);
  });

  it("escapes those characters in a plain value, as \\u escapes that name the code point", () => {
    const block = buildRunEnvBlock([v("TAGGED", { value: `a${String.fromCodePoint(0xe0041)}b­c؜d᠎e` })]);
    assert.ok(block.includes("\\u{e0041}") && block.includes("\\u00ad") && block.includes("\\u061c") && block.includes("\\u180e"));
    for (const cp of [0xe0041, 0x00ad, 0x061c, 0x180e]) assert.ok(!block.includes(String.fromCodePoint(cp)), `U+${cp.toString(16)} not raw`);
  });

  it("keeps ordinary text, including non-ASCII", () => {
    assert.equal(sanitizeRunEnvPromptText("Données: 日本語 ok"), "Données: 日本語 ok");
  });

  it("bounds to 1024 UTF-8 bytes without splitting a code point", () => {
    const out = sanitizeRunEnvPromptText("é".repeat(600));
    assert.equal(Buffer.byteLength(out, "utf8"), 1024);
    assert.ok(!out.includes("�"));
    const odd = sanitizeRunEnvPromptText("a" + "日".repeat(400));
    assert.ok(Buffer.byteLength(odd, "utf8") <= 1024);
    assert.equal(Buffer.from(odd, "utf8").toString("utf8"), odd);
  });
});

describe("buildRunEnvBlock", () => {
  it("is empty without variables", () => {
    assert.equal(buildRunEnvBlock(undefined), "");
    assert.equal(buildRunEnvBlock([]), "");
  });

  it("lists names, scope, descriptions and plain values, never a secret value", () => {
    assertBlockFacts(buildRunEnvBlock(VARS));
  });

  it("escapes unsafe code points in a plain value as \\uXXXX", () => {
    const block = buildRunEnvBlock([v("MULTI", { value: "a‮b​c\nd" })]);
    assert.ok(block.includes('"a\\u202eb\\u200bc\\nd"'), block);
    assertClean(block);
  });

  it("states that the variables are readable by the agent and fences them with a nonce", () => {
    const a = buildRunEnvBlock(VARS);
    const b = buildRunEnvBlock(VARS);
    assert.match(a, /set in the environment of every command you run/);
    assert.match(a, /never instructions/);
    assert.notEqual(a.match(/<run_env_([0-9a-f]+)>/)![1], b.match(/<run_env_([0-9a-f]+)>/)![1]);
  });

  it("does not list a name the injection would refuse", () => {
    assert.equal(buildRunEnvBlock([v("PATH"), v("lowercase")]), "");
  });
});

describe("prompts built from the precomputed set list the injected names and omit shadowed and denied ones", () => {
  const cred: SdkCredential = { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" };
  const claimVars = [v("NPM_TOKEN"), v("REGION"), v("PATH"), v("DB_PASSWORD", { value: SECRET, secret: true })];

  it("a Claude plan prompt lists the injected names and leaves out a tool-profile shadow and a denied name", () => {
    const injected = sdkRunEnvInjected(claimVars, cred, "/h", { NPM_TOKEN: "from-tool-profile" }).injected;
    const prompt = buildPlanPrompt({ branch: "agent/issue-9", subagentNames: ["coder"], issueIid: 9, issueTitle: "t", issueDescription: "d", runEnv: injected });
    assert.match(prompt, /- REGION \(repo scope\)/);
    assert.match(prompt, /- DB_PASSWORD \(repo scope, secret/);
    assert.doesNotMatch(prompt, /- NPM_TOKEN/);
    assert.doesNotMatch(prompt, /- PATH/);
  });

  it("a Codex block lists the injected names and leaves out a tool-profile shadow and a denied name", () => {
    const injected = commandRunEnvInjected(claimVars, "/private/tmp", { NPM_TOKEN: "from-tool-profile", PATH: "/nix/bin" }).injected;
    const block = buildRunEnvBlock(injected);
    assert.match(block, /- REGION \(repo scope\)/);
    assert.match(block, /- DB_PASSWORD \(repo scope, secret/);
    assert.doesNotMatch(block, /- NPM_TOKEN/);
    assert.doesNotMatch(block, /- PATH/);
  });
});

describe("Claude lead prompts", () => {
  const base = { branch: "agent/issue-9", subagentNames: ["coder"] };
  const plan = (runEnv?: ClaimRunEnvVar[]) => buildPlanPrompt({ ...base, issueIid: 9, issueTitle: "t", issueDescription: "d", ...(runEnv ? { runEnv } : {}) });
  const norm = (p: string) => p.replace(/_[0-9a-f]{16}>/g, "_N>");

  it("plan prompt carries the block", () => assertBlockFacts(plan(VARS)));
  it("plan prompt is unchanged without variables", () => {
    assert.equal(norm(plan(undefined)), norm(plan([])));
    assert.ok(!plan([]).includes("run_env_"));
  });

  it("implement prompt carries the block on the first turn only", () => {
    assertBlockFacts(buildImplementPrompt({ ...base, first: true, iteration: 1, runEnv: VARS }));
    assert.ok(!buildImplementPrompt({ ...base, first: false, iteration: 2, runEnv: VARS }).includes("run_env_"));
    assert.equal(norm(buildImplementPrompt({ ...base, first: true, iteration: 1, runEnv: [] })), norm(buildImplementPrompt({ ...base, first: true, iteration: 1 })));
  });

  it("ci_fix plan prompt carries the block", () => {
    const input = { ref: "main", branch: "b", pipelineWebURL: "https://ci.example.test/1", failedJobs: [{ name: "j", stage: "s", logTail: "boom" }], subagentNames: ["coder"] };
    assertBlockFacts(buildCIFixPlanPrompt({ ...input, runEnv: VARS }));
    assert.equal(norm(buildCIFixPlanPrompt({ ...input, runEnv: [] })), norm(buildCIFixPlanPrompt(input)));
  });

  it("self_improve plan prompt carries the block", () => {
    const input = { ...base, recommendations: "- improve" };
    assertBlockFacts(buildSelfImprovePlanPrompt({ ...input, runEnv: VARS }));
    assert.equal(norm(buildSelfImprovePlanPrompt({ ...input, runEnv: [] })), norm(buildSelfImprovePlanPrompt(input)));
  });
});

describe("Claude subagent prompts", () => {
  const t: AgentTemplate = { name: "coder", description: "writes code", prompt_body: "You implement changes.", tools: ["Read"], model: null };
  const block = buildRunEnvBlock(VARS);

  it("assembleAgents and subagentsFromTemplates append the block before the safety block", () => {
    for (const prompt of [
      assembleAgents([t], undefined, [], block).subagents.coder!.prompt,
      subagentsFromTemplates([t], new Set(), undefined, [], block).coder!.prompt,
    ]) {
      assertBlockFacts(prompt);
      assert.ok(prompt.indexOf("<run_env_") < prompt.lastIndexOf("</run_env_"));
    }
  });

  it("without a block the prompt is unchanged", () => {
    assert.equal(assembleAgents([t]).subagents.coder!.prompt, assembleAgents([t], undefined, [], "").subagents.coder!.prompt);
    assert.ok(!assembleAgents([t]).subagents.coder!.prompt.includes("run_env_"));
  });
});

describe("Codex subagent prompts", () => {
  const role: HarnessAgent = { description: "role", prompt: "body", tools: { kind: "inherit" }, deniedTools: [], toolServers: [], skills: [] };
  const req = (runEnvAppend?: string): RunTurnRequest => ({
    prompt: "task", systemPrompt: "system", phase: "implement", signal: new AbortController().signal, leadSkills: [],
    agents: { coder: role }, ...(runEnvAppend ? { runEnvAppend } : {}),
  });

  it("renders the block into each subagent prompt", () => {
    assertBlockFacts(renderCodexRun(req(buildRunEnvBlock(VARS))).perRolePrompts.get("coder")!);
  });

  it("is unchanged without a block", () => {
    assert.ok(!renderCodexRun(req()).perRolePrompts.get("coder")!.includes("run_env_"));
  });
});
