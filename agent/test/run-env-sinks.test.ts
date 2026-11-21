import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { makeMemoryToolHandlers } from "../src/memory-tools.js";
import { makeForgeToolHandlers } from "../src/forge-tools.js";
import { makeFindingsToolHandlers } from "../src/findings-tools.js";
import { redactPlanSummary } from "../src/summary-runner.js";
import { makeTextRedactor } from "../src/redact.js";
import type { WorkerClient } from "../src/client.js";
import { fakeGitlab, git, installHarness, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

installHarness();

// PRD 4416 M2 (W2b): sinks that carry agent-authored or model-authored text out of the worker redact it with
// the run's redactor, so a registered run-variable secret never reaches the outbound payload. The memory,
// MR-thread reply and finding sinks already redacted (pin tests here); the plan summary, the planning diff
// and the code cross-check repository instructions are redacted by this change. Dummy values only.

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

describe("sinks redacted by this change", () => {
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
  it("both executors post the redacted intent summary", () => {
    const src = (rel: string): string => fs.readFileSync(path.join(new URL("../src/", import.meta.url).pathname, rel), "utf8");
    for (const rel of ["sdk-executor.ts", path.join("codex", "codex-executor.ts")]) {
      assert.match(src(rel), /postIntentSummary\(ctx\.runId, ctx\.redactText\?\.\(summary\) \?\? summary\)/, rel);
    }
  });

  const src = (rel: string): string => fs.readFileSync(path.join(new URL("../src/", import.meta.url).pathname, rel), "utf8");

  it("both executors post the redacted plan summary and plan text", () => {
    for (const rel of ["sdk-executor.ts", path.join("codex", "codex-executor.ts")]) {
      const text = src(rel);
      assert.match(text, /const safe = redactPlanSummary\(result, ctx\.redactText\);\s*await [\w.!]*client[!]?\.postPlanSummary\(ctx\.runId, \{\s*\.\.\.safe, plan_md: \(ctx\.redactText\?\.\(planMd\) \?\? planMd\)/, rel);
    }
  });

  it("the runner redacts the code cross-check repository instructions and passes the redactor to the planning diff", () => {
    const text = src("runner.ts");
    assert.match(text, /return "text" in read \? flight\.redactText\(read\.text\) : "";/);
    assert.equal((text.match(/this\.captureCheckedPlanningDiff\(.*runLog, flight\.redactText\)/g) ?? []).length, 2);
  });
});
