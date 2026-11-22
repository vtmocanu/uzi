import { describe, it } from "node:test";
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { makeMemoryToolHandlers } from "../src/memory-tools.js";
import { makeForgeToolHandlers } from "../src/forge-tools.js";
import { makeFindingsToolHandlers } from "../src/findings-tools.js";
import { redactPlanSummary } from "../src/summary-runner.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";
import { createLogger } from "../src/log.js";
import { MessageBatcher } from "../src/batcher.js";
import { sanitizeNowText, trimToolFrame } from "../src/now-summary.js";
import type { Executor } from "../src/executor.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar, OutgoingMessage } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { ScratchPublicationError } from "../src/git.js";
import { api, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runner, runnerWith, simulateCommittedWork } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

installHarness();

// PRD 4416 M2: sinks that carry agent-authored or model-authored text out of the worker redact it with the
// run's redactor, so a registered run-variable secret never reaches the outbound payload. Dummy values only.

const SECRET = "dummy-secret-run-value-000000";
const redactText = makeTextRedactor([SECRET]);

function leaked(payload: unknown): boolean {
  return JSON.stringify(payload).includes(SECRET);
}

describe("already-redacted sinks (pins)", () => {
  it("save_memory", async () => {
    const sent: unknown[] = [];
    const client = { async saveMemory(_id: string, body: unknown) { sent.push(body); return { id: "m1", title: "t", body: "b", created_at: "x" }; } } as unknown as WorkerClient;
    const h = makeMemoryToolHandlers({ client, runId: "r1", log: nullLogger(), redactText });
    await h.saveMemory({ title: `title ${SECRET}`, body: `body ${SECRET}`, evidence: `ev ${SECRET}` });
    assert.equal(sent.length, 1);
    assert.ok(!leaked(sent));
  });

  it("reply_mr_thread", async () => {
    const sent: string[] = [];
    const client = { async replyMRThread(_id: string, _reply: string, body: string) { sent.push(body); return { replied: true }; } } as unknown as WorkerClient;
    const h = makeForgeToolHandlers({ client, runId: "r1", log: nullLogger(), redactText });
    await h.replyMrThread({ reply_id: "t1", body: `thanks ${SECRET}` });
    assert.equal(sent.length, 1);
    assert.ok(!leaked(sent));
  });

  it("report_incidental_issue (payload and card)", async () => {
    const sent: unknown[] = [];
    const emitted: unknown[] = [];
    const client = { async reportFinding(_id: string, body: unknown) { sent.push(body); return "f1"; } } as unknown as WorkerClient;
    const h = makeFindingsToolHandlers({ client, runId: "r1", emit: (m) => emitted.push(m), log: nullLogger(), redactText });
    await h.reportIncidentalIssue({ title: `t ${SECRET}`, description: `d ${SECRET}`, location: `l ${SECRET}`, labels: [`x ${SECRET}`] });
    assert.equal(sent.length, 1);
    assert.ok(!leaked(sent) && !leaked(emitted));
  });
});

describe("plan summary and planning diff sinks", () => {
  it("the plan summary and its deltas", () => {
    const out = redactPlanSummary({ summary: `s ${SECRET}`, deltas: [{ kind: "added", text: `d ${SECRET}` }, { kind: "dropped", text: "clean" }] }, redactText);
    assert.ok(!leaked(out));
    assert.deepEqual(out.deltas.map((d) => d.kind), ["added", "dropped"]);
    assert.equal(out.deltas[1]!.text, "clean");
  });

  it("the plan summary is unchanged without a redactor", () => {
    const input = { summary: "s", deltas: [] };
    assert.equal(redactPlanSummary(input, undefined), input);
  });

  it("the plan-cross-check and semantic planning diff", async () => {
    const run = runnerWith(() => ({ executor: { run: async () => ({ branch: "b" }) } }), fakeGitlab().gitlab) as unknown as {
      captureCheckedPlanningDiff(clone: string, base: string, bare: string | undefined, signal: AbortSignal,
        log: ReturnType<typeof nullLogger>, redact?: (s: string) => string): Promise<{ diff: string }>;
    };
    const diff = `diff --git a/x b/x\n+++ b/x\n@@ -0,0 +1 @@\n+token=${SECRET}\n`;
    const originalCapture = git.capturePlanningDiff;
    const originalScan = git.scanPatchForSecrets;
    git.capturePlanningDiff = async () => Buffer.from(diff);
    git.scanPatchForSecrets = async () => ({ trusted: true, findings: [] });
    try {
      const got = await run.captureCheckedPlanningDiff("/nonexistent/clone", "a".repeat(40), "/nonexistent/bare", new AbortController().signal, nullLogger(), redactText);
      assert.ok(!leaked(got), "the redacted diff is what leaves the worker");
      assert.ok(got.diff.startsWith("diff --git a/x b/x\n+++ b/x\n@@ -0,0 +1 @@\n+token="));
      const raw = await run.captureCheckedPlanningDiff("/nonexistent/clone", "a".repeat(40), "/nonexistent/bare", new AbortController().signal, nullLogger());
      assert.equal(raw.diff, diff, "no redactor leaves the diff byte-identical");
    } finally {
      git.capturePlanningDiff = originalCapture;
      git.scanPatchForSecrets = originalScan;
    }
  });
});

describe("the planning diff cap holds after redaction", () => {
  type Capture = (clone: string, base: string, bare: string | undefined, signal: AbortSignal,
    log: ReturnType<typeof nullLogger>, redact?: (s: string) => string) => Promise<{ diff: string } | { refusal: string; diagnostic: string }>;
  async function capture(diff: string, redact: (s: string) => string): Promise<Awaited<ReturnType<Capture>>> {
    const run = runnerWith(() => ({ executor: { run: async () => ({ branch: "b" }) } }), fakeGitlab().gitlab) as unknown as { captureCheckedPlanningDiff: Capture };
    const originalCapture = git.capturePlanningDiff;
    const originalScan = git.scanPatchForSecrets;
    git.capturePlanningDiff = async () => Buffer.from(diff);
    git.scanPatchForSecrets = async () => ({ trusted: true, findings: [] });
    try {
      return await run.captureCheckedPlanningDiff("/nonexistent/clone", "a".repeat(40), "/nonexistent/bare", new AbortController().signal, nullLogger(), redact);
    } finally {
      git.capturePlanningDiff = originalCapture;
      git.scanPatchForSecrets = originalScan;
    }
  }

  it("a diff under the cap that redaction grows past it is refused with the same typed refusal as an oversized capture", async () => {
    // An 8-byte value becomes a 14-byte placeholder: 40000 of them with a separator are 360 KB raw, 600 KB redacted.
    const grow = makeTextRedactor(["abcd1234"]);
    const diff = "abcd1234 ".repeat(40_000);
    assert.ok(Buffer.byteLength(diff) < 512 * 1024 && Buffer.byteLength(grow(diff)) > 512 * 1024, "the fixture straddles the cap");
    assert.deepEqual(await capture(diff, grow), { refusal: "diff_too_large", diagnostic: "output_cap" });
    assert.ok("diff" in await capture(diff.slice(0, 1000), grow), "a small diff is unaffected");
  });
});

describe("source pins: the executors and the runner route these payloads through the redactor", () => {
  it("both executors post the redacted intent summary (call-site pin)", () => {
    const src = (rel: string): string => fs.readFileSync(path.join(new URL("../src/", import.meta.url).pathname, rel), "utf8");
    for (const rel of ["sdk-executor.ts", path.join("codex", "codex-executor.ts")]) {
      assert.match(src(rel), /postIntentSummary\(ctx\.runId, ctx\.redactText\?\.\(summary\) \?\? summary\)/, rel);
    }
  });

  const src = (rel: string): string => fs.readFileSync(path.join(new URL("../src/", import.meta.url).pathname, rel), "utf8");

  it("both executors post the redacted plan summary and plan text (call-site pin)", () => {
    for (const rel of ["sdk-executor.ts", path.join("codex", "codex-executor.ts")]) {
      const text = src(rel);
      assert.match(text, /const safe = redactPlanSummary\(result, ctx\.redactText\);\s*await [\w.!]*client[!]?\.postPlanSummary\(ctx\.runId, \{\s*\.\.\.safe, plan_md: \(ctx\.redactText\?\.\(planMd\) \?\? planMd\)/, rel);
    }
  });

  it("the runner redacts the code cross-check repository instructions and passes the redactor to the planning diff (call-site pin)", () => {
    const text = src("runner.ts");
    assert.match(text, /return "text" in read \? flight\.redactText\(read\.text\) : "";/);
    assert.equal((text.match(/this\.captureCheckedPlanningDiff\(.*runLog, flight\.redactText\)/g) ?? []).length, 2);
  });
});

const src = (rel: string): string => fs.readFileSync(path.join(new URL("../src/", import.meta.url).pathname, rel), "utf8");
const secretVar = (name: string, value: string): ClaimRunEnvVar =>
  ({ var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name, value, secret: true, description: "" });
const claimSecrets = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", run_env: [secretVar("DB_PASSWORD", SECRET)] });

describe("questions, now summary, failure reasons and the merge request", () => {
  it("a question reaches the api through the batcher's payload redactor", async () => {
    const sent: OutgoingMessage[] = [];
    const client = { async postMessages(_id: string, messages: OutgoingMessage[]) { sent.push(...messages); } } as unknown as WorkerClient;
    const forms = runEnvClaimForms([secretVar("DB_PASSWORD", SECRET)], []).redaction;
    const batcher = new MessageBatcher(client, "run-q", 0, 0, nullLogger(), makeRedactor(forms), makeTextRedactor(forms));
    batcher.emit({ kind: "question", agent: "lead", payload: { question_id: "q1", questions: [{ header: "h", question: `is ${SECRET} right?`, options: [{ label: `use ${SECRET}` }] }] } });
    await batcher.close();
    assert.equal(sent.length, 1);
    assert.ok(!leaked(sent));
  });

  it("the now summary scrubs the frame fields and the note text", () => {
    const frame = trimToolFrame({ kind: "tool_use", agent: "lead", payload: { name: "Bash", input: { description: `deploy ${SECRET}` } } } as never, redactText);
    assert.ok(frame !== undefined && !leaked(frame));
    assert.ok(!leaked(sanitizeNowText(`working on ${SECRET}`, redactText)));
  });

  it("a now summary note the runner posts carries no value from the model's text", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(46104, { secrets: claimSecrets() });
    api.nowSummary.set(claim.run_id, true);
    simulateCommittedWork();
    const HAIKU = "claude-haiku-4-5-20251001";
    // The value starts inside the 120 rune note cap, so a cut that ran before the redaction would leave a prefix of it.
    const NOW_FILLER = "filler ".repeat(14);
    const queryFn = (() => (async function* () {
      yield { type: "assistant", message: { id: "m1", model: HAIKU, role: "assistant", content: [{ type: "text", text: `${NOW_FILLER}${SECRET} for the api gate` }], usage: { input_tokens: 2, output_tokens: 2 } } };
      yield { type: "result", subtype: "success", is_error: false, modelUsage: { [HAIKU]: { inputTokens: 2, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0001 } } };
    })()) as unknown as SdkQueryFn;
    const notes = () => api.messages(claim.run_id).filter((m) => m.kind === "progress_note");
    const exec: Executor = {
      async run(ctx) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "Write the docs" }] as never);
        const until = Date.now() + 4000;
        while (notes().length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "task gate:api", description: "Run the api gate" } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        return { branch: ctx.branch };
      },
    };
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { nowSummary: { homeRoot: homeDir, queryFn } }).execute(claim);
    assert.ok(notes().length > 0, "the summary pass posted a note");
    assert.ok(!leaked(api.messages(claim.run_id)), "no outbound message carries the value");
    const noteText = JSON.stringify(notes());
    for (let i = 0; i + 8 <= SECRET.length; i++)
      assert.ok(!noteText.includes(SECRET.slice(i, i + 8)), `no note carries the fragment at ${i}`);
  });

  it("a failed run reports a failure reason without the value", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(46101, { secrets: claimSecrets() });
    const exec: Executor = { run: async () => { throw new Error(`deploy failed with ${SECRET}`); } };
    await runner(exec, gitlab).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed.length > 0 && failed.every((s) => typeof s.body.failure_reason === "string" && s.body.failure_reason.length > 0));
    assert.ok(!leaked(failed.map((s) => s.body)));
  });

  it("the merge request title and body carry no value from the issue", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(46102, { secrets: claimSecrets(), issue_title: `rotate ${SECRET}` });
    simulateCommittedWork();
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, agentSelection: { source: "own", agents: ["coder", "reviewer"] } }) };
    await runner(exec, gitlab).execute(claim);
    assert.equal(calls.length > 0, true, "a merge request was opened: " + JSON.stringify(api.states.filter((s) => s.runId === claim.run_id).map((s) => [s.body.status, s.body.failure_reason])));
    assert.match(calls[0]!.body ?? "", /rotate /, "the title is still built from the issue");
    for (const call of calls) assert.ok(!(call.body ?? "").includes(SECRET), "no outbound forge body carries the value");
  });

  it("a scratch publication failure reports a stable reason without the value in its detail", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(46105, { secrets: claimSecrets() });
    const detail = `remote rejected ${SECRET}`;
    const exec: Executor = { run: async () => { throw new ScratchPublicationError("candidate history", undefined, { kind: "exec_failed", step: "object_walk", detail, rawDetail: detail }); } };
    await runner(exec, gitlab).execute(claim);
    const reasons = api.states.filter((s) => s.runId === claim.run_id && typeof s.body.failure_reason === "string").map((s) => s.body.failure_reason as string);
    assert.ok(reasons.some((r) => r.startsWith("scratch_publication_refused:")), JSON.stringify(reasons));
    assert.ok(!leaked(api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body)));
    assert.ok(!leaked(api.messages(claim.run_id)));
  });

  it("the terminal journal and the state report choke points call redactFailureReason with the run's redactor (call-site pin)", () => {
    const text = src("runner.ts");
    assert.match(text, /body = redactFailureReason\(flight\.redactText, body\);/);
    assert.match(text, /body = redactFailureReason\(redactText, body\);/);
    assert.match(text, /const reason = sanitizeForLog\(redactText\(rawReason\), Number\.POSITIVE_INFINITY\);/);
  });

  it("the local scan blocked reasons are composed with the run's redactor (call-site pin)", () => {
    const text = src("runner.ts");
    assert.match(text, /composeLocalScanBlockedReason\(findings, \{ redact: redactText/);
    assert.match(text, /composeLocalScanBlockedReason\(scan\.findings, \{ redact: redactText/);
  });

  it("the decisions memo the client receives is redacted", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(46103, { secrets: claimSecrets() });
    simulateCommittedWork();
    api.decisionsMemo.get = { status: 200, body: { enabled: true, memo: null } };
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, agentSelection: { source: "own", agents: ["coder", "reviewer"] }, decisionsMemo: `kept ${SECRET} end` }) };
    await runner(exec, gitlab).execute(claim);
    assert.equal(api.decisionsMemoPosts.length, 1, "the memo was saved");
    const saved = api.decisionsMemoPosts[0]!.body.body ?? "";
    assert.ok(saved.startsWith("kept ") && saved.endsWith(" end"), saved);
    assert.ok(!leaked(saved));
  });

  it("a non-closing reason declared by the executor reaches the merge request without the value", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(46106, { secrets: claimSecrets() });
    simulateCommittedWork();
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, agentSelection: { source: "own", agents: ["coder", "reviewer"] }, nonClosing: { reason: `follow-up needed for ${SECRET}` } }) };
    await runner(exec, gitlab).execute(claim);
    assert.ok(calls.length > 0, "a merge request was opened");
    assert.ok(calls.some((c) => (c.body ?? "").includes("follow-up needed for ")), "the reason is still rendered");
    for (const call of calls) assert.ok(!(call.body ?? "").includes(SECRET), "no outbound forge body carries the value");
  });

  it("the completion attempt and checkpoint non-closing reasons are redacted where they are sent (call-site pin)", () => {
    const text = src("runner.ts");
    assert.match(text, /redactNonClosingReason\(flight\.redactText, effectiveDeclared\.reason\)/);
    assert.match(text, /redactNonClosingReason\(flight\.redactText, nonClosing\.reason\)/);
  });

  it("the merge request body is built from redacted inputs (call-site pin)", () => {
    const text = src("runner.ts");
    assert.match(text, /lead: result\.prSummary \? redactStrings\(result\.prSummary, redactText\) : result\.prSummary,/);
    const ctx = src("pr-description-context.ts");
    for (const input of ["Title: ${input.issueTitle}", "input.prdText", "planRaw", "renderPrevious(input.previous)", "claimsRaw"]) {
      assert.ok(ctx.includes(`part(redact, ${input.startsWith("Title") ? "`" + input : input}`), input);
    }
  });
});

// Regression 3a: a line of a multi-line secret that the old token rule left public (lowercase-only, or one case
// with a single digit) is masked wherever it appears alone, for a live run_env secret and a retained
// run_env_redact version, in a feed payload, in the merge request title and body, and in a log line.
describe("a line fragment of a multi-line secret is masked in every sink", () => {
  const LOWER = "zqxvbnmkl";
  const ONE_DIGIT = "pwrtyhgf4";
  // Two long structural lines, derived from a hash so no literal is encoded text.
  const OTHER_LINES = ["first", "second"].map((n) => crypto.createHash("sha256").update(`sinks-${n}`).digest("base64url").slice(0, 22));
  const MULTI = [OTHER_LINES[0]!, LOWER, ONE_DIGIT, OTHER_LINES[1]!].join("\n");
  const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
  const shapes: Array<[string, () => { run_env?: ClaimRunEnvVar[]; run_env_redact?: ClaimRunEnvRedact[] }]> = [
    ["a live run_env secret", () => ({ run_env: [secretVar("SVC_FILE", MULTI)] })],
    ["a retained run_env_redact version", () => ({ run_env_redact: [{ var_id: UUID, scope: "repo", rev: 1, name: "OLD_FILE", value: MULTI }] })],
  ];
  let mrCount = 0;
  const fragments: Array<[string, string]> = [["a lowercase-only line", LOWER], ["a single-case line with one digit", ONE_DIGIT]];

  for (const [shape, vars] of shapes) {
    for (const [label, fragment] of fragments) {
      const forms = (): string[] => runEnvClaimForms(vars().run_env, vars().run_env_redact).redaction;

      it(`${shape}, ${label}: masked in a feed payload`, async () => {
        const { gitlab } = fakeGitlab();
        const claim = gitlabClaim(46120 + mrCount++, { secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", ...vars() } });
        const exec: Executor = {
          async run(ctx) {
            ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: `echo ${fragment}`, description: `show ${fragment}` } } });
            return { branch: ctx.branch };
          },
        };
        await runner(exec, gitlab).execute(claim);
        const feed = api.messages(claim.run_id);
        assert.ok(feed.some((m) => m.kind === "tool_use"), "the tool_use message reached the feed");
        assert.ok(!JSON.stringify(feed).includes(fragment), "no feed message carries the fragment");
      });

      it(`${shape}, ${label}: masked in the merge request title and body`, async () => {
        const { gitlab, calls } = fakeGitlab();
        const claim = gitlabClaim(46110 + mrCount++, {
          secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", ...vars() },
          issue_title: `rotate ${fragment}`,
        });
        simulateCommittedWork();
        const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, agentSelection: { source: "own", agents: ["coder", "reviewer"] } }) };
        await runner(exec, gitlab).execute(claim);
        assert.ok(calls.length > 0, "a merge request was opened");
        assert.match(calls[0]!.body ?? "", /rotate /, "the title is still built from the issue");
        for (const call of calls) assert.ok(!(call.body ?? "").includes(fragment), "no outbound forge body carries the fragment");
      });

      it(`${shape}, ${label}: masked in a log line`, () => {
        const log = createLogger("error");
        for (const f of forms()) log.addSecret(f);
        const scrubbed = log.redactDiagnostic!(JSON.stringify({ msg: `saw ${fragment} here` }));
        assert.ok(!scrubbed.includes(fragment));
      });
    }
  }
});
