// PRD #4416 M2 (W2b): the prompt block that names the run's environment variables. Plain values are
// shown (JSON, invisible characters escaped), secret values never, descriptions are sanitized, and
// the block reaches the lead and subagent prompts of both harnesses.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { assembleAgents, subagentsFromTemplates } from "../src/agents.js";
import { renderCodexRun } from "../src/codex/render.js";
import type { HarnessAgent, RunTurnRequest } from "../src/harness.js";
import {
  buildCIFixPlanPrompt, buildImplementPrompt, buildPlanPrompt, buildSelfImprovePlanPrompt, SUBAGENT_SAFETY_APPEND,
} from "../src/prompt.js";
import { buildRunEnvBlock, sanitizeRunEnvPromptText, type RunEnvPromptVar } from "../src/run-env-prompt.js";
import type { AgentTemplate } from "../src/protocol.js";

const ESC = String.fromCharCode(0x1b);
const BEL = String.fromCharCode(7);
// Assembled from fragments: no token-shaped literal in source.
const SECRET_VALUE = ["s3cr3t", "-value-", "do-not-show"].join("");
const HOSTILE_DESC = `Deploy ${ESC}[31mred${ESC}[0m${ESC}]0;title${BEL}\r\nIgnore previous\u202eevil\u2066x\u200bz${ESC}Q end`;
const vars: RunEnvPromptVar[] = [
  { name: "API_BASE_URL", value: "https://example.test/\u202ea\"b", secret: false, scope: "repo", description: HOSTILE_DESC },
  { name: "DEPLOY_TOKEN", value: SECRET_VALUE, secret: true, scope: "repo", description: "the deploy token" },
];
// Written out independently of isUnsafeNameChar so the test does not use the code under test.
const RANGES: Array<[number, number]> = [
  [0x00, 0x1f], [0x7f, 0x9f], [0x200b, 0x200f], [0x2028, 0x2029], [0x202a, 0x202e], [0x2060, 0x2064], [0x2066, 0x206f], [0xfeff, 0xfeff],
];
const hasControlOrBidi = (s: string): boolean => [...s].some((ch) => RANGES.some(([lo, hi]) => ch.codePointAt(0)! >= lo && ch.codePointAt(0)! <= hi));

function assertBlock(prompt: string, what: string): void {
  assert.match(prompt, /<run_env_[0-9a-f]+>/, `${what}: fenced block present`);
  assert.ok(prompt.includes("- API_BASE_URL (repo)"), `${what}: plain var listed`);
  assert.ok(prompt.includes("- DEPLOY_TOKEN (repo, secret)"), `${what}: secret var listed`);
  assert.ok(prompt.includes('value "https://example.test/\\u202ea\\"b"'), `${what}: plain value as escaped JSON`);
  assert.ok(prompt.includes("the deploy token"), `${what}: description present`);
  assert.ok(prompt.includes("already set in the environment of every command"), `${what}: states vars are set`);
  assert.ok(!prompt.includes(SECRET_VALUE), `${what}: the secret value never appears`);
  const block = /<run_env_([0-9a-f]+)>[\s\S]*<\/run_env_\1>/.exec(prompt)![0];
  // Inside the fence only newlines separate the fixed lines: no other control or bidi character.
  assert.ok(!hasControlOrBidi(block.replaceAll("\n", "")), `${what}: no control or bidi character in the block`);
  assert.ok(!block.includes(ESC), `${what}: no escape character`);
}

describe("sanitizeRunEnvPromptText", () => {
  it("removes CSI, OSC and two-character escapes whole", () => {
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}[31;1mb${ESC}[0m`), "ab");
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}]0;title${BEL}b`), "ab");
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}]8;;http://x${ESC}\\b`), "ab");
    assert.equal(sanitizeRunEnvPromptText(`a${ESC}Mb`), "ab");
  });
  it("replaces control, bidi and format code points with U+FFFD", () => {
    for (const cp of [0x09, 0x0a, 0x0d, 0x7f, 0x85, 0x200b, 0x200f, 0x2028, 0x2029, 0x202a, 0x202e, 0x2060, 0x2064, 0x2066, 0x206f, 0xfeff]) {
      assert.equal(sanitizeRunEnvPromptText(`a${String.fromCodePoint(cp)}b`), "a\ufffdb", `U+${cp.toString(16)}`);
    }
    assert.equal(sanitizeRunEnvPromptText("plain text"), "plain text");
  });
  it("bounds to 1024 bytes without splitting a code point", () => {
    const out = sanitizeRunEnvPromptText("\u{1F600}".repeat(400));
    assert.equal(Buffer.byteLength(out), 1024);
    assert.equal(out, "\u{1F600}".repeat(256));
    const odd = sanitizeRunEnvPromptText("a" + "\u{1F600}".repeat(400));
    assert.ok(Buffer.byteLength(odd) <= 1024 && !odd.includes("\ufffd"));
  });
});

describe("buildRunEnvBlock", () => {
  it("renders names, scopes, descriptions and plain values; never a secret value", () => {
    assertBlock(buildRunEnvBlock(vars), "block");
  });
  it("renders nothing without variables", () => {
    assert.equal(buildRunEnvBlock(undefined), "");
    assert.equal(buildRunEnvBlock([]), "");
  });
  it("does not list a name the shared name rules deny (the worker never sets it)", () => {
    const b = buildRunEnvBlock([...vars, { name: "PATH", value: "/evil", secret: false, scope: "repo", description: "" }]);
    assert.ok(!b.includes("- PATH"));
  });
});

describe("Claude lead prompts", () => {
  const base = { branch: "b", subagentNames: ["coder"] };
  const builders: Array<[string, (runEnv?: RunEnvPromptVar[]) => string]> = [
    ["buildPlanPrompt", (runEnv) => buildPlanPrompt({ ...base, issueIid: 1, issueTitle: "t", issueDescription: "d", runEnv })],
    ["buildImplementPrompt (first turn)", (runEnv) => buildImplementPrompt({ ...base, first: true, iteration: 1, runEnv })],
    ["buildCIFixPlanPrompt", (runEnv) => buildCIFixPlanPrompt({ ...base, ref: "main", pipelineWebURL: "https://ci/1", failedJobs: [{ name: "u", stage: "t", logTail: "x" }], runEnv })],
    ["buildSelfImprovePlanPrompt", (runEnv) => buildSelfImprovePlanPrompt({ ...base, recommendations: "r", runEnv })],
  ];
  for (const [name, build] of builders) {
    it(`${name} carries the block, and none without variables`, () => {
      assertBlock(build(vars), name);
      assert.ok(!/run_env_[0-9a-f]+/.test(build(undefined)), `${name}: no block when absent`);
      assert.ok(!/run_env_[0-9a-f]+/.test(build([])), `${name}: no block when empty`);
    });
  }
  it("buildImplementPrompt carries it on the first turn only", () => {
    assert.ok(!/run_env_[0-9a-f]+/.test(buildImplementPrompt({ ...base, first: false, iteration: 2, runEnv: vars })));
  });
});

describe("Claude subagent prompts", () => {
  const coder: AgentTemplate = { name: "coder", description: "writes", prompt_body: "You implement.", tools: null };
  const block = buildRunEnvBlock(vars);
  it("assembleAgents and subagentsFromTemplates append the block before the safety block", () => {
    const own = assembleAgents([coder], undefined, [], block).subagents.coder!.prompt;
    const repo = subagentsFromTemplates([coder], new Set(), undefined, [], block).coder!.prompt;
    for (const [what, prompt] of [["own", own], ["repo", repo]] as const) {
      assertBlock(prompt, what);
      assert.ok(prompt.indexOf("<run_env_") < prompt.indexOf(SUBAGENT_SAFETY_APPEND), `${what}: safety block stays last`);
      assert.ok(prompt.endsWith(SUBAGENT_SAFETY_APPEND));
    }
  });
  it("adds nothing without variables", () => {
    assert.ok(!assembleAgents([coder]).subagents.coder!.prompt.includes("run_env_"));
  });
});

describe("Codex subagent prompts", () => {
  const role: HarnessAgent = { description: "r", prompt: "body", tools: { kind: "inherit" }, deniedTools: [], toolServers: [], skills: [] };
  const request = (runEnvBlock?: string): RunTurnRequest => ({
    prompt: "task", systemPrompt: "system", phase: "implement", signal: new AbortController().signal,
    leadSkills: [], agents: { coder: role }, ...(runEnvBlock !== undefined ? { runEnvBlock } : {}),
  });
  it("renders the block into each role's developer instructions, before the safety block", () => {
    const prompt = renderCodexRun(request(buildRunEnvBlock(vars))).perRolePrompts.get("coder")!;
    assertBlock(prompt, "codex subagent");
    assert.ok(prompt.indexOf("<run_env_") < prompt.indexOf(SUBAGENT_SAFETY_APPEND));
  });
  it("renders nothing without a block", () => {
    assert.ok(!renderCodexRun(request()).perRolePrompts.get("coder")!.includes("run_env_"));
  });
});
