import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { HookInput, HookJSONOutput, Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";
import { buildClaudeCrossCheckOptions, checkerSurface, ClaudeCrossCheck, CodeToolBudget } from "../src/claude-cross-check.js";
import { CODE_VERDICT_SCHEMA, PLAN_VERDICT_SCHEMA, validateCodeVerdict, validateVerdict } from "../src/codex/cross-check.js";
import {
 buildIsolatedPreToolUse, checkIsolatedInit, ISOLATED_TOOLS, STRUCTURED_OUTPUT_CARRIER,
 type InitGate, type IsolationSurface,
} from "../src/isolated-executor.js";
import { laneJobSurface } from "../src/job-runner.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

import { check, isRecord, SUPPORTED_KEYWORDS } from "./json-schema-subset.js";

function keywordsUsed(schema: unknown, out: Set<string> = new Set()): Set<string> {
 if (Array.isArray(schema)) for (const v of schema) keywordsUsed(v, out);
 else if (isRecord(schema)) {
  for (const [key, child] of Object.entries(schema)) {
   out.add(key);
   // "properties" maps property NAMES to schemas; the names are not keywords.
   if (key === "properties" && isRecord(child)) for (const sub of Object.values(child)) keywordsUsed(sub, out);
   else if (key !== "enum" && key !== "required") keywordsUsed(child, out);
  }
 }
 return out;
}

const planItem = { file: "a.ts", severity: "info", summary: "s", rationale: "r" };
const codeFinding = { id: "F1", severity: "minor", path: "a.ts", line: 1, title: "t", detail: "d" };

describe("verdict schemas and the in-test checker", () => {
 it("use only keywords the checker supports", () => {
  for (const schema of [PLAN_VERDICT_SCHEMA, CODE_VERDICT_SCHEMA]) {
   const unsupported = [...keywordsUsed(schema)].filter((k) => !SUPPORTED_KEYWORDS.has(k));
   assert.deepEqual(unsupported, []);
  }
 });

 it("survive a JSON round trip unchanged", () => {
  for (const schema of [PLAN_VERDICT_SCHEMA, CODE_VERDICT_SCHEMA]) {
   assert.deepEqual(JSON.parse(JSON.stringify(schema)), schema);
  }
 });

 const planAccepted: Array<[string, unknown]> = [
  ["approve with no items", { verdict: "approve", summary: "ok", items: [] }],
  ["revise with info item", { verdict: "revise", summary: "x", items: [planItem] }],
  ["revise with warning item", { verdict: "revise", summary: "x", items: [{ ...planItem, severity: "warning" }] }],
  ["block with error item", { verdict: "block", summary: "x", items: [{ ...planItem, severity: "error" }] }],
  ["empty strings", { verdict: "approve", summary: "", items: [{ file: "", severity: "info", summary: "", rationale: "" }] }],
  ["summary of exactly 4096 bytes", { verdict: "approve", summary: "x".repeat(4096), items: [] }],
  ["item with a 1900 character string field", { verdict: "revise", summary: "x", items: [{ ...planItem, rationale: "r".repeat(1900) }] }],
  ["twenty items",{ verdict: "revise", summary: "x", items: Array.from({ length: 20 }, () => planItem) }],
 ];
 for (const [name, v] of planAccepted) {
  it(`plan verdict accepted by validator and schema: ${name}`, () => {
   assert.doesNotThrow(() => validateVerdict(JSON.stringify(v)));
   assert.equal(check(PLAN_VERDICT_SCHEMA, v), true);
  });
 }

 const codeAccepted: Array<[string, unknown]> = [
  ["no findings", { findings: [] }],
  ["minor finding", { findings: [codeFinding] }],
  ["major finding at line 0", { findings: [{ ...codeFinding, severity: "major", line: 0 }] }],
  ["critical finding at max line", { findings: [{ ...codeFinding, severity: "critical", line: 2147483647 }] }],
  ["id of 64 characters", { findings: [{ ...codeFinding, id: "a_-".repeat(21) + "b" }] }],
  ["detail of 1900 characters", { findings: [{ ...codeFinding, detail: "d".repeat(1900) }] }],
  ["valid coverage",{ findings: [], coverage: [{ path: "a.ts", status: "examined", reason: "read" }] }],
  ["null coverage", { findings: [codeFinding], coverage: null }],
  ["coverage entries with an extra key", { findings: [], coverage: [{ path: "a.ts", status: "excluded", reason: "r", extra: 1 }] }],
  ["coverage as an object", { findings: [], coverage: { path: "a.ts" } }],
 ];
 for (const [name, v] of codeAccepted) {
  it(`code verdict accepted by validator and schema: ${name}`, () => {
   assert.doesNotThrow(() => validateCodeVerdict(JSON.stringify(v)));
   assert.equal(check(CODE_VERDICT_SCHEMA, v), true);
  });
 }

 const planRejected: Array<[string, unknown]> = [
  ["extra root key", { verdict: "approve", summary: "ok", items: [], extra: 1 }],
  ["bad verdict enum", { verdict: "maybe", summary: "ok", items: [] }],
  ["missing items", { verdict: "approve", summary: "ok" }],
  ["bad severity enum", { verdict: "revise", summary: "x", items: [{ ...planItem, severity: "fatal" }] }],
  ["extra item key", { verdict: "revise", summary: "x", items: [{ ...planItem, extra: 1 }] }],
  ["too many items", { verdict: "revise", summary: "x", items: Array.from({ length: 21 }, () => planItem) }],
  ["summary too long", { verdict: "approve", summary: "x".repeat(4097), items: [] }],
  ["items not an array", { verdict: "approve", summary: "x", items: "none" }],
 ];
 for (const [name, v] of planRejected) {
  it(`plan schema rejects: ${name}`, () => {
   assert.equal(check(PLAN_VERDICT_SCHEMA, v), false);
  });
 }

 const codeRejected: Array<[string, unknown]> = [
  ["findings not an array", { findings: "none" }],
  ["extra root key", { findings: [], extra: 1 }],
  ["bad id", { findings: [{ ...codeFinding, id: "bad id" }] }],
  ["bad severity", { findings: [{ ...codeFinding, severity: "blocker" }] }],
  ["negative line", { findings: [{ ...codeFinding, line: -1 }] }],
  ["fractional line", { findings: [{ ...codeFinding, line: 1.5 }] }],
  ["line above cap", { findings: [{ ...codeFinding, line: 2147483648 }] }],
  ["missing detail", { findings: [{ id: "F1", severity: "minor", path: "a", line: 1, title: "t" }] }],
 ];
 for (const [name, v] of codeRejected) {
  it(`code schema rejects: ${name}`, () => {
   assert.equal(check(CODE_VERDICT_SCHEMA, v), false);
  });
 }

 it("counts maxLength in code points", () => {
  const emoji = "\u{1F600}";
  assert.equal(check({ type: "string", maxLength: 2 }, emoji + emoji), true);
  assert.equal(check({ type: "string", maxLength: 2 }, emoji.repeat(3)), false);
 });
});

// ---- The carrier in the gate, the init check, the budget and the built options ----

const fixtureInit = ((): Record<string, unknown> => {
 const parsed = JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "fixtures", "claude-structured-output-0.3.316.json"), "utf8")) as
  { sessions: Record<string, { frames: Array<Record<string, unknown>> }> };
 return parsed.sessions["plan_retry"]!.frames.find((f) => f["type"] === "system" && f["subtype"] === "init")!;
})();
const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
fs.mkdirSync(scratch, { recursive: true });

const NAMES = ["Bash", "Write", "Edit", "WebFetch", "StructuredOutputX", "StructuredOutput ", "structuredoutput", "mcp__x__StructuredOutput", " StructuredOutput"];

type Matcher = NonNullable<NonNullable<SdkOptions["hooks"]>["PreToolUse"]>[number];

/** Runs a composed hook list for one tool call (SDK matcher semantics); returns the first deny reason. */
async function decide(matchers: readonly Matcher[], name: string, id = "t", input: unknown = { file_path: "a.ts" }, cwd = "/checkout"): Promise<string> {
 for (const m of matchers) {
  if (m.matcher && !new RegExp(`^(?:${m.matcher})$`).test(name)) continue;
  for (const fn of m.hooks) {
   const out = await fn({ hook_event_name: "PreToolUse", tool_use_id: id, tool_name: name, tool_input: input, cwd, session_id: "s", transcript_path: "/unused" } as HookInput,
    undefined, { signal: new AbortController().signal }) as HookJSONOutput;
   if ("hookSpecificOutput" in out && out.hookSpecificOutput && "permissionDecision" in out.hookSpecificOutput && out.hookSpecificOutput.permissionDecision === "deny") {
    return out.hookSpecificOutput.permissionDecisionReason ?? "deny";
   }
  }
 }
 return "allow";
}

const plainSurface: IsolationSurface = { tools: ["Read", "Grep", "Glob"], mcpServers: [], plugins: [], skills: [] };
const researchLike: IsolationSurface = { tools: ISOLATED_TOOLS, mcpServers: ["uzi_fetch"], plugins: [], skills: [] };
const carrierSurface: IsolationSurface = { ...plainSurface, structuredOutputCarrier: STRUCTURED_OUTPUT_CARRIER };

function isolated(surface: IsolationSurface, gate: InitGate): Matcher[] {
 return buildIsolatedPreToolUse({ surface, gate, cwd: "/checkout", log: nullLogger(), secretPaths: ["/worker-denied/"] });
}
function built(gate: InitGate, outputSchema?: Record<string, unknown>): SdkOptions {
 return buildClaudeCrossCheckOptions({
  cwd: "/checkout", homeDir: "/home", oauthToken: "fixture-value", log: nullLogger(), secretPaths: ["/worker-denied/"], gate, brief: "brief",
  ...(outputSchema ? { outputSchema } : {}),
 });
}

describe("the StructuredOutput carrier in the tool gate", () => {
 it("is the SDK's recorded tool name", () => {
  assert.equal(STRUCTURED_OUTPUT_CARRIER, (fixtureInit["tools"] as string[]).find((t) => !["Read", "Grep", "Glob"].includes(t)));
 });

 const sources: Array<[string, (gate: InitGate) => readonly Matcher[]]> = [
  ["a carrier surface", (g) => isolated(carrierSurface, g)],
  ["the builder with the plan schema", (g) => built(g, PLAN_VERDICT_SCHEMA).hooks!.PreToolUse!],
  ["the builder with the code schema", (g) => built(g, CODE_VERDICT_SCHEMA).hooks!.PreToolUse!],
 ];
 for (const [label, make] of sources) {
  it(`${label}: allows exactly the carrier after init and denies it before`, async () => {
   const gate: InitGate = { passed: false };
   const hooks = make(gate);
   assert.match(await decide(hooks, STRUCTURED_OUTPUT_CARRIER), /not been verified/);
   gate.passed = true;
   assert.equal(await decide(hooks, STRUCTURED_OUTPUT_CARRIER, "t", {}), "allow");
   for (const name of NAMES) assert.notEqual(await decide(hooks, name, "t", { command: "true", file_path: "a.ts", url: "https://x.test" }), "allow", name);
  });
 }

 const without: Array<[string, (gate: InitGate) => readonly Matcher[]]> = [
  ["a plain checker surface", (g) => isolated(plainSurface, g)],
  ["the builder without an outputSchema", (g) => built(g).hooks!.PreToolUse!],
  ["a research-like surface", (g) => isolated(researchLike, g)],
  ["the lane job surface", (g) => isolated(laneJobSurface([]), g)],
 ];
 for (const [label, make] of without) {
  it(`${label}: denies the carrier even after init`, async () => {
   const gate: InitGate = { passed: true };
   assert.match(await decide(make(gate), STRUCTURED_OUTPUT_CARRIER, "t", {}), /not in the isolated run's fixed tool set/);
  });
 }
});

describe("the StructuredOutput carrier in the init check", () => {
 const init = (tools: string[]): Record<string, unknown> => ({ ...fixtureInit, tools });
 it("accepts the recorded real init frame with a carrier surface", () => {
  assert.equal(checkIsolatedInit(fixtureInit, carrierSurface), undefined);
  assert.equal(checkIsolatedInit(fixtureInit, checkerSurface(true)), undefined);
 });
 it("accepts the surface tools with or without the carrier", () => {
  assert.equal(checkIsolatedInit(init(["Read", "Grep", "Glob"]), carrierSurface), undefined);
  assert.equal(checkIsolatedInit(init(["Read", "Grep", "Glob", "StructuredOutput"]), carrierSurface), undefined);
 });
 it("refuses any other extra tool, a duplicated carrier and a missing surface tool", () => {
  for (const tools of [
   ["Read", "Grep", "Glob", "Bash"], ["Read", "Grep", "Glob", "StructuredOutputX"], ["Read", "Grep", "Glob", "StructuredOutput", "Bash"],
   ["Read", "Grep", "Glob", "StructuredOutput", "StructuredOutput"], ["Read", "Grep", "StructuredOutput"],
  ]) assert.match(checkIsolatedInit(init(tools), carrierSurface) ?? "", /effective tool set differs/, tools.join(","));
 });
 it("refuses the carrier on a plain surface", () => {
  assert.match(checkIsolatedInit(fixtureInit, plainSurface) ?? "", /extra: StructuredOutput/);
  assert.match(checkIsolatedInit(fixtureInit, checkerSurface(false)) ?? "", /extra: StructuredOutput/);
 });
});

describe("the built options with and without an outputSchema", () => {
 it("set outputFormat only when a schema is passed, leaving tools, disallowedTools and settingSources alone", () => {
  const plain = built({ passed: false });
  const planOpts = built({ passed: false }, PLAN_VERDICT_SCHEMA);
  const codeOpts = built({ passed: false }, CODE_VERDICT_SCHEMA);
  assert.equal("outputFormat" in plain, false);
  assert.deepEqual(planOpts.outputFormat, { type: "json_schema", schema: PLAN_VERDICT_SCHEMA });
  assert.deepEqual(codeOpts.outputFormat, { type: "json_schema", schema: CODE_VERDICT_SCHEMA });
  for (const o of [planOpts, codeOpts]) {
   assert.deepEqual(o.tools, plain.tools);
   assert.deepEqual(o.tools, ["Read", "Grep", "Glob"]);
   assert.deepEqual(o.disallowedTools, plain.disallowedTools);
   assert.deepEqual(o.settingSources, []);
  }
 });
 it("do not set the SDK's structured output retry cap in the child env", () => {
  for (const o of [built({ passed: false }), built({ passed: false }, PLAN_VERDICT_SCHEMA)]) {
   assert.equal(Object.hasOwn(o.env ?? {}, "MAX_STRUCTURED_OUTPUT_RETRIES"), false);
  }
 });
});

describe("the StructuredOutput carrier and the code tool budget", () => {
 function codeClaim(): ClaimResponse {
  return { kind: "cross_check", run_id: "child", claim_generation: 7, last_seq: 0, agents: [], issue_iid: null, issue_title: "Issue", issue_description: "Issue DATA",
   repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo", clone_url: "https://example.test/repo.git", default_branch: "main" },
   config: { default_model: "sonnet", default_effort: "high" }, secrets: { forge_pat: "", anthropic_oauth_token: "fixture-value" },
   cross_check: { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64), base_commit: "a".repeat(40), head_commit: "b".repeat(40),
    plan_md: "Approved plan DATA", guidance_snapshot: "", milestones: [], required_capabilities: [], required_tools: [], size_class: "small",
    code_context: { task: "context DATA" }, code_diff: "diff DATA", deadline_at: "2099-01-01T00:00:00Z" } } as unknown as ClaimResponse;
 }
 const assistant = (id: string, name: string, input: unknown) => ({ type: "assistant", parent_tool_use_id: null, message: { content: [{ type: "tool_use", id, name, input }] } });
 const success = { type: "result", subtype: "success", is_error: false, num_turns: 1, result: "ignored", structured_output: { findings: [] } };

 async function runWith(frames: (o: SdkOptions) => AsyncIterable<unknown>): Promise<string> {
  const root = fs.mkdtempSync(path.join(scratch, "so-budget-"));
  fs.writeFileSync(path.join(root, "anchor.ts"), "anchor");
  try {
   const query = ((p: { options: SdkOptions }) => frames(p.options)) as unknown as SdkQueryFn;
   return await new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/worker-denied/"], queryFn: query })
    .run(codeClaim(), root, root, new AbortController().signal, async () => {});
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
 }

 // Reads interleaved with carrier calls: the carrier is never counted, so 200 Reads still pass and the 201st fails.
 for (const [reads, ok] of [[200, true], [201, false]] as const) {
  it(`${reads} distinct Read calls interleaved with carrier calls ${ok ? "succeed" : "fail with the tool budget"}`, async () => {
   const run = runWith((o) => (async function* () {
    yield fixtureInit;
    const hooks = o.hooks!.PreToolUse!;
    for (let i = 1; i <= reads; i++) {
     const readInput = { file_path: "anchor.ts" };
     const readDecision = await decide(hooks, "Read", "read_" + i, readInput, o.cwd);
     if (readDecision !== "allow") break;
     yield assistant("read_" + i, "Read", readInput);
     assert.equal(await decide(hooks, STRUCTURED_OUTPUT_CARRIER, "so_" + i, { findings: "bad" }, o.cwd), "allow");
     yield assistant("so_" + i, STRUCTURED_OUTPUT_CARRIER, { findings: "bad" });
    }
    yield success;
   })());
   if (ok) assert.equal(await run, '{"findings":[]}');
   else await assert.rejects(run, /tool budget/);
  });
 }

 it("an over-budget Read is denied and an exceeded budget still denies the carrier", async () => {
  let carrierAfter = "";
  await assert.rejects(runWith((o) => (async function* () {
   yield fixtureInit;
   const hooks = o.hooks!.PreToolUse!;
   for (let i = 1; i <= 200; i++) assert.equal(await decide(hooks, "Read", "r" + i, { file_path: "anchor.ts" }, o.cwd), "allow");
   assert.match(await decide(hooks, "Read", "r201", { file_path: "anchor.ts" }, o.cwd), /tool budget/);
   carrierAfter = await decide(hooks, STRUCTURED_OUTPUT_CARRIER, "so", { findings: [] }, o.cwd);
   yield success;
  })()), /tool budget/);
  assert.match(carrierAfter, /tool budget/);
 });

 it("a tool_use that only resembles the carrier is an unallowed tool", async () => {
  for (const name of ["StructuredOutputX", "structuredoutput", "Bash"]) {
   await assert.rejects(runWith(() => (async function* () { yield fixtureInit; yield assistant("x", name, {}); yield success; })()), /unallowed tool/, name);
  }
 });

 it("the carrier tool_use is neither counted nor treated as a repeat", async () => {
  const text = await runWith(() => (async function* () {
   yield fixtureInit;
   yield assistant("same", STRUCTURED_OUTPUT_CARRIER, {});
   yield assistant("same", STRUCTURED_OUTPUT_CARRIER, {});
   yield success;
  })());
  assert.equal(text, '{"findings":[]}');
 });
 it("a budget with no carrier configured treats a StructuredOutput tool_use as unallowed and counts its hook call", async () => {
  const frame = { type: "assistant", message: { content: [{ type: "tool_use", id: "so-1", name: STRUCTURED_OUTPUT_CARRIER, input: {} }] } };
  assert.throws(() => new CodeToolBudget(new AbortController()).observe(frame), /unallowed tool/);
  // The configured budget skips the same frame.
  new CodeToolBudget(new AbortController(), STRUCTURED_OUTPUT_CARRIER).observe(frame);
  // Unconfigured, the hook admits the carrier only as an ordinary counted call: after 200 ids it is denied.
  const plain = new CodeToolBudget(new AbortController());
  const input = (id: string, tool_name: string) => ({ hook_event_name: "PreToolUse", tool_use_id: id, tool_name, tool_input: {},
   session_id: "s", transcript_path: "/unused", cwd: "/checkout" }) as HookInput;
  for (let i = 0; i < 200; i++) assert.deepEqual(await plain.hook(input(`r${i}`, "Read")), {});
  assert.match(JSON.stringify(await plain.hook(input("so-2", STRUCTURED_OUTPUT_CARRIER))), /tool budget exceeded/);
 });
});
