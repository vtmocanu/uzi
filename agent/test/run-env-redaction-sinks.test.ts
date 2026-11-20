// PRD #4416 M2 (W2b): a registered run-env secret, in any of the forms the worker registers (raw,
// JSON-escaped, inside a base64 stream), must not leave the worker through any sink that carries
// agent- or model-authored text. One sentinel test per sink; the sinks that already redacted before
// this unit are pinned as well, at the cheapest seam that still shows the redactor is applied.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { MessageBatcher } from "../src/batcher.js";
import { RequestError, type PlanCrossCheckCandidate, type WorkerClient } from "../src/client.js";
import { checkCode } from "../src/code-cross-check-gate.js";
import { readCIEvidence } from "../src/ci-evidence-tools.js";
import { makeForgeToolHandlers } from "../src/forge-tools.js";
import { makeFindingsToolHandlers } from "../src/findings-tools.js";
import { makeMemoryToolHandlers } from "../src/memory-tools.js";
import { sanitizeNowText } from "../src/now-summary.js";
import { checkPlan } from "../src/plan-cross-check-gate.js";
import { boundedPrSummaryJson } from "../src/publication-intent.js";
import { redactNonClosingReason, SecretSet } from "../src/redact.js";
import { claimRunEnvSecretForms } from "../src/run-env-forms.js";
import { SemanticPlanningGate } from "../src/semantic-planning-gate.js";
import { planSummaryPostBody } from "../src/summary-runner.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { boundedTransport, proposal } from "./semantic-coverage-fixture.js";
import { semanticServer } from "./semantic-server-fixture.js";
import { nullLogger } from "./helpers.js";

// Assembled from fragments (no token-shaped literal); the quote and backslash make the JSON form differ.
const SECRET = ["sentinel", '"q', "\\b", "-run-env-", "value-7"].join("");
const JSON_FORM = JSON.stringify(SECRET).slice(1, -1);
const B64_STREAM = Buffer.from(`deploy:${SECRET}`, "utf8").toString("base64");
const VARIANTS = [SECRET, JSON_FORM, B64_STREAM];
const MARK = "***REDACTED***";

const secrets = new SecretSet(claimRunEnvSecretForms({
  run_env: [{ var_id: "00000000-0000-4000-8000-000000000001", scope: "repo", rev: 1, name: "DEPLOY_TOKEN", value: SECRET, secret: true, description: "" }],
  run_env_redact: [],
}));
const redactText = secrets.text;

/** The text under test: every form of the sentinel, so one pass proves all three are masked. */
const leaky = (label: string): string => `${label}: ${VARIANTS.join(" | ")}`;
function assertMasked(s: string, what: string): void {
  for (const v of VARIANTS) assert.ok(!s.includes(v), `${what}: a form of the secret survived`);
  assert.ok(s.includes(MARK), `${what}: no redaction marker`);
}
const read = (rel: string): string => fs.readFileSync(new URL(`../src/${rel}`, import.meta.url), "utf8");

describe("the sentinel itself is masked in every registered form", () => {
  it("raw, JSON-escaped and base64-stream spellings", () => {
    assert.notEqual(JSON_FORM, SECRET);
    assertMasked(redactText(leaky("x")), "redactText");
  });
});

describe("sinks redacted by this unit", () => {
  it("memory save: title, body, evidence", async () => {
    const sent: Array<{ title: string; body: string; evidence?: string }> = [];
    const client = { async saveMemory(_r: string, b: { title: string; body: string; evidence?: string }) {
      sent.push(b); return { id: "m1", title: b.title, body: b.body, created_at: "2026-11-20T00:00:00Z" };
    } } as unknown as WorkerClient;
    const h = makeMemoryToolHandlers({ client, runId: "r", log: nullLogger(), redactText });
    await h.saveMemory({ title: leaky("t"), body: leaky("b"), evidence: leaky("e") });
    assert.equal(sent.length, 1);
    assertMasked(sent[0]!.title, "title"); assertMasked(sent[0]!.body, "body"); assertMasked(sent[0]!.evidence ?? "", "evidence");
  });

  it("MR-thread reply", async () => {
    const sent: string[] = [];
    const client = { async replyMRThread(_r: string, _id: string, body: string) { sent.push(body); return { replied: true }; } } as unknown as WorkerClient;
    await makeForgeToolHandlers({ client, runId: "r", log: nullLogger(), redactText }).replyMrThread({ reply_id: "d1", body: leaky("reply") });
    assert.equal(sent.length, 1);
    assertMasked(sent[0]!, "reply body");
  });

  it("finding title and description", async () => {
    const sent: Array<Record<string, unknown>> = [];
    const client = { async reportFinding(_r: string, body: Record<string, unknown>) { sent.push(body); return "f1"; } } as unknown as WorkerClient;
    await makeFindingsToolHandlers({ client, runId: "r", emit: () => {}, log: nullLogger(), redactText })
      .reportIncidentalIssue({ title: leaky("t"), description: leaky("d"), location: "a.ts:1", confidence: "high" });
    assert.equal(sent.length, 1);
    assertMasked(String(sent[0]!.title), "title"); assertMasked(String(sent[0]!.description), "description");
  });

  it("CI evidence excerpt", async () => {
    const prepared = { metadata: "", read: async () => ({ text: leaky("log"), truncated: false, omitted_bytes: 0 }) };
    const reply = await readCIEvidence(prepared as never, { manifest_id: "00000000-0000-4000-8000-000000000002" }, redactText);
    assert.ok(!reply.isError);
    assertMasked(reply.content[0]!.text, "evidence reply");
    // The JSON-escaped form is a different string once the reply is serialised: check the parsed text too.
    assertMasked((JSON.parse(reply.content[0]!.text) as { text: string }).text, "evidence text");
  });

  it("plan summary POST body: summary, deltas and the plan guard", () => {
    const body = planSummaryPostBody({ summary: leaky("s"), deltas: [{ kind: "added", text: leaky("d") }] }, leaky("plan"), redactText);
    assertMasked(body.summary, "summary"); assertMasked(body.deltas[0]!.text, "delta"); assertMasked(body.plan_md, "plan_md");
  });

  it("both executors post the plan summary through planSummaryPostBody", () => {
    for (const f of ["sdk-executor.ts", "codex/codex-executor.ts"]) {
      assert.match(read(f), /postPlanSummary\(ctx\.runId, planSummaryPostBody\(result, planMd, ctx\.redactText\)\)/, f);
    }
  });

  it("semantic coverage report is redacted before it is published", async () => {
    const client = {} as WorkerClient;
    const events: string[] = [];
    const server = semanticServer(client, events);
    const published: unknown[] = [];
    const inner = client.publishSemanticCoverage.bind(client);
    client.publishSemanticCoverage = async (id, gen, request) => { published.push(request.report); return inner(id, gen, request); };
    const leakyReport = () => {
      const r = server.report();
      return { ...r, requirements: r.requirements.map(q => ({ ...q, explanation: leaky("why") })) };
    };
    const transport = await boundedTransport("claude", leakyReport, events);
    const gate = new SemanticPlanningGate(client, "run", 1, new AbortController().signal, transport, redactText);
    const structural: PlanCrossCheckCandidate = { plan_md: "Implement A", milestones: [{ id: "m1", title: "A" }],
      required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40), planning_diff: "" };
    const sources = [{ id: "issue:1", kind: "issue", provenance: "server:issue:1", completeness: "complete",
      bytes: Buffer.from("Implement A").toString("base64") }];
    const c = await gate.compile(structural, proposal(), sources);
    await gate.review(c, structural.plan_md, sources);
    assert.equal(published.length, 1);
    assertMasked(JSON.stringify(published[0]).replaceAll("\\\\", "\\"), "published report");
  });

  it("semantic candidate: the runner redacts the plan, milestones, proposal and diff before compile", () => {
    // Wiring pin (driving gatePlan needs a full run): the structural candidate is built from the
    // already-redacted plan and the redacted planning diff, so compile()'s plan check holds.
    const src = read("runner.ts");
    assert.match(src, /const planMd = flight\.redactText\(rawPlanMd\);/);
    assert.match(src, /planning_diff: flight\.redactText\(semanticDiff\.diff\),\n\s*\}, semanticSubmission/);
    assert.match(src, /this\.semanticCoverageTestTransport, flight\.redactText\)/);
  });

  it("plan cross-check candidate: plan, milestones and planning diff", async () => {
    const submitted: PlanCrossCheckCandidate[] = [];
    const reservation = { prepare: async () => ({ prepared: true, tail: 0 }), markSubmitted() {}, cancel() {}, release: () => true,
      releaseAppliedGate: () => true };
    const batcher = { reserveCandidateTransport: () => reservation,
      usage: { drainConfirmed: async () => true, hasUnconfirmedLoss: false, inactive: false } };
    const client = { submitPlanCrossCheck: async (_r: string, _g: number, c: PlanCrossCheckCandidate) => {
      submitted.push(c); throw new RequestError("POST", "/cross-checks", 400, "{}");
    } } as unknown as WorkerClient;
    const candidate: PlanCrossCheckCandidate = { plan_md: leaky("plan"), milestones: [{ id: "m1", title: leaky("m") }],
      required_capabilities: [], required_tools: [], size_class: "s", base_commit: "b".repeat(40), planning_diff: leaky("diff") };
    await checkPlan({ client, runId: "run", generation: 1, candidate, batcher: batcher as never, signal: new AbortController().signal,
      timing: { pollMs: 0, backoffMs: [0, 0] }, redactText });
    assert.equal(submitted.length, 1);
    assertMasked(submitted[0]!.plan_md, "plan_md"); assertMasked(submitted[0]!.planning_diff, "planning_diff");
    assertMasked(JSON.stringify(submitted[0]!.milestones).replaceAll("\\\\", "\\"), "milestones");
    assert.match(read("runner.ts"), /planning_diff: flight\.redactText\(captured\.diff\) \}, batcher, redactText: flight\.redactText,/);
  });

  it("code cross-check candidate: repository instruction text", async () => {
    const submits: unknown[] = [];
    const noRow = { stage: "code", result: "no_row", repo_instructions_enabled: true };
    const client = {
      codeCrossCheckStatus: async () => noRow,
      submitCodeCrossCheck: async (_i: string, _g: number, _s: unknown, _sig: unknown, instructions: unknown) => {
        submits.push(instructions); throw new RequestError("POST", "/cross-checks", 500, "x");
      },
    };
    await checkCode({ client: client as never, runId: "run", generation: 1, signal: new AbortController().signal, requestMs: 1000,
      snapshot: async () => ({ head_commit: "a".repeat(40), base_commit: "b".repeat(40) }),
      readInstructions: async () => leaky("instructions"), redactText }).catch(() => undefined);
    assert.equal(submits.length, 1);
    assertMasked((submits[0] as { repo_instructions_text: string }).repo_instructions_text, "instructions");
    assert.match(read("runner.ts"), /codeGate \?\?= new CodeCrossCheckGate\(\{\n\s*redactText: flight\.redactText,/);
  });
});

describe("sinks that already redacted, pinned", () => {
  it("run-message batcher (covers questions, answers and the Codex stream payloads)", async () => {
    const sent: OutgoingMessage[] = [];
    const client = { async postMessages(_r: string, m: OutgoingMessage[]) { sent.push(...m); } } as unknown as WorkerClient;
    const b = new MessageBatcher(client, "run-1", 0, 500, nullLogger(), secrets.payload, secrets.text);
    b.emit({ kind: "question", agent: "lead", payload: { questions: [leaky("q")] } });
    b.emit({ kind: "answer", agent: "lead", payload: { answers: [leaky("a")] } });
    b.emit({ kind: "tool_result", agent: "lead", payload: { tool_use_id: "t", content: leaky("out"), is_error: false } });
    await b.close();
    assert.equal(sent.length, 3);
    for (const m of sent) assertMasked(JSON.stringify(m.payload).replaceAll("\\\\", "\\"), m.kind);
  });

  it("MR body: the bounded PR summary", () => {
    assertMasked(boundedPrSummaryJson({ headline: leaky("h") }, redactText).replaceAll("\\\\", "\\"), "pr summary");
  });

  it("park (non-closing) reason", () => {
    assertMasked(redactNonClosingReason(redactText, leaky("reason")), "non-closing reason");
  });

  it("now summary text", () => {
    assertMasked(sanitizeNowText(leaky("now"), redactText), "now summary");
  });

  it("wiring pins: MR title, decisions memo, failure reason and the Codex stream scrub use the run's redactor", () => {
    const runner = read("runner.ts");
    assert.match(runner, /sliceWellFormed\(redactText\(mrTitle\(claim, result\.scopeCapped, completionScope\)\), 1024\)/);
    assert.match(runner, /clampUtf8Bytes\(flight\.redactText\(memo\), DECISIONS_MEMO_MAX_BYTES\)/);
    assert.match(runner, /sanitizeForLog\(redactText\(rawReason\), Number\.POSITIVE_INFINITY\)/);
    assert.match(read("codex/codex-executor.ts"), /scrubProjected: \(s: string\): string => claimRedact\(/);
  });
});
