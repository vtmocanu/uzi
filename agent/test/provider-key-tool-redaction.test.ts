import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { makeForgeToolHandlers } from "../src/forge-tools.js";
import { makeFindingsToolHandlers } from "../src/findings-tools.js";
import { makeMemoryToolHandlers } from "../src/memory-tools.js";
import { makeUziToolHandlers } from "../src/uzi-tools.js";
import { buildCodexToolHandlers } from "../src/codex/codex-executor.js";
import { memoryToolNames } from "../src/memory-tools.js";
import { reportIncidentalIssueToolName } from "../src/findings-tools.js";
import { makeTextRedactor, providerKeyForms } from "../src/redact.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// A prefixless provider key: nothing the API's pattern scrub could recognise. Assembled
// from fragments so no token-shaped literal sits in source.
const KEY = ["gw", "Zk3/", "p9Q+", "x7 Lm", "2vR&", "t8"].join("");
const forms = providerKeyForms(KEY);
const redactor = makeTextRedactor(forms);

// The raw key plus every encoded form (JSON `\/`, percent, ...).
const leaky = (label: string): string => `${label}: ${forms.join(" | ")}`;
const assertNoKey = (s: string): void => {
  for (const f of forms) assert.ok(!s.includes(f), `form leaked: ${f}`);
};

describe("provider-bound tool handlers redact agent-authored text before the POST", () => {
  it("forge reply_mr_thread body", async () => {
    const sent: string[] = [];
    const client = { async replyMRThread(_r: string, _id: string, body: string) { sent.push(body); return { replied: true }; } } as unknown as WorkerClient;
    const h = makeForgeToolHandlers({ client, runId: "r", log: nullLogger(), redactText: redactor });
    await h.replyMrThread({ reply_id: "d1", body: leaky("reply") });
    assert.equal(sent.length, 1);
    assertNoKey(sent[0]!);
    assert.match(sent[0]!, /^reply: /);
  });

  it("findings title/description/location/labels", async () => {
    const sent: Array<Record<string, unknown>> = [];
    const client = { async reportFinding(_r: string, body: Record<string, unknown>) { sent.push(body); return "f1"; } } as unknown as WorkerClient;
    const h = makeFindingsToolHandlers({ client, runId: "r", emit: () => {}, log: nullLogger(), redactText: redactor });
    await h.reportIncidentalIssue({
      title: leaky("t"), description: leaky("d"), location: leaky("l"), labels: [leaky("x")], confidence: "high",
    });
    assert.equal(sent.length, 1);
    for (const k of ["title", "description", "location"]) assertNoKey(String(sent[0]![k]));
    for (const l of sent[0]!.labels as string[]) assertNoKey(l);
  });

  it("memory title/body/evidence", async () => {
    const sent: Array<{ title: string; body: string; evidence?: string }> = [];
    const client = {
      async saveMemory(_r: string, b: { title: string; body: string; evidence?: string }) {
        sent.push(b);
        return { id: "m1", title: b.title, body: b.body, created_at: "2026-07-19T00:00:00Z" };
      },
    } as unknown as WorkerClient;
    const h = makeMemoryToolHandlers({ client, runId: "r", log: nullLogger(), redactText: redactor });
    await h.saveMemory({ title: leaky("t"), body: leaky("b"), evidence: leaky("e") });
    assert.equal(sent.length, 1);
    assertNoKey(sent[0]!.title);
    assertNoKey(sent[0]!.body);
    assertNoKey(sent[0]!.evidence ?? "");
  });

  // Redact-before-clamp: a key form that straddles the field cap must not leave a prefix of itself
  // (clamping first would cut the form in half, and the redactor would then no longer recognise it).
  // A prefix of length >= 8 surviving means the cut happened before the redaction.
  it("findings: a key form straddling the title and location caps leaves no >=8 char prefix; confidence, labels and the card are redacted", async () => {
    const sent: Array<Record<string, unknown>> = [];
    const emitted: Array<{ kind: string; payload: Record<string, unknown> }> = [];
    const client = { async reportFinding(_r: string, body: Record<string, unknown>) { sent.push(body); return "f1"; } } as unknown as WorkerClient;
    const h = makeFindingsToolHandlers({ client, runId: "r", emit: (m) => emitted.push(m as never), log: nullLogger(), redactText: redactor });
    const straddle = (cap: number): string => "a".repeat(cap - 6) + KEY + " tail";
    await h.reportIncidentalIssue({
      title: straddle(255), description: leaky("d"), location: straddle(512),
      labels: [leaky("x"), "plain"], confidence: KEY,
    });
    assert.equal(sent.length, 1);
    // Any surviving prefix of length >= 8 contains the form's own first 8 characters.
    const hasPrefix = (s: string): boolean => forms.some((f) => f.length >= 8 && s.includes(f.slice(0, 8)));
    for (const k of ["title", "location", "description", "confidence"]) {
      const v = String(sent[0]![k] ?? "");
      assertNoKey(v);
      assert.ok(!hasPrefix(v), `${k}: a >=8 char key prefix survived`);
    }
    const labels = sent[0]!.labels as string[];
    assert.equal(labels.length, 2, "both labels are kept");
    for (const l of labels) assertNoKey(l);
    assert.ok(labels.includes("plain"), "plain label unchanged");
    assert.ok(String(sent[0]!.title).startsWith("aaa"), "evidence of the surrounding text is still present");
    assert.equal(emitted.length, 1);
    assert.equal(emitted[0]!.kind, "finding");
    assertNoKey(JSON.stringify(emitted[0]!.payload));
    assert.ok(!hasPrefix(JSON.stringify(emitted[0]!.payload)), "the emitted card carries no key prefix");
  });

  it("codex handler map: forge reply, finding and memory are redacted before the POST", async () => {
    const sent = { reply: [] as string[], finding: [] as Array<Record<string, unknown>>, memory: [] as Array<Record<string, unknown>> };
    const client = {
      async replyMRThread(_r: string, _id: string, body: string) { sent.reply.push(body); return { replied: true }; },
      async reportFinding(_r: string, body: Record<string, unknown>) { sent.finding.push(body); return "f1"; },
      async saveMemory(_r: string, b: Record<string, unknown>) { sent.memory.push(b); return { id: "m1", title: String(b.title), body: String(b.body), created_at: "2026-07-19T00:00:00Z" }; },
    } as unknown as WorkerClient;
    const emitted: unknown[] = [];
    const map = buildCodexToolHandlers({ client, runId: "r", log: nullLogger(), emit: (m) => emitted.push(m), redactText: redactor, skills: [] });
    await map.get("mcp__forge__reply_mr_thread")!({ reply_id: "d1", body: leaky("reply") });
    await map.get(reportIncidentalIssueToolName())!({ title: leaky("t"), description: leaky("d"), location: leaky("l"), labels: [leaky("x")] });
    await map.get(memoryToolNames()[0]!)!({ title: leaky("t"), body: leaky("b"), evidence: leaky("e") });
    assert.equal(sent.reply.length, 1);
    assertNoKey(sent.reply[0]!);
    assert.equal(sent.finding.length, 1);
    assertNoKey(JSON.stringify(sent.finding[0]));
    assertNoKey(JSON.stringify(emitted));
    assert.equal(sent.memory.length, 1);
    assertNoKey(JSON.stringify(sent.memory[0]));
  });

  it("chat propose_issue: title, description and labels are redacted before createProposal", async () => {
    const sent: Array<Record<string, unknown>> = [];
    const client = {
      async createProposal(runId: string, body: Record<string, unknown>) {
        sent.push(body);
        return { id: "p1", run_id: runId, title: String(body.title), description: String(body.description), labels: body.labels as string[], status: "pending", created_at: "2026-07-19T00:00:00Z" };
      },
    } as unknown as WorkerClient;
    const h = makeUziToolHandlers({ client, runId: "chat", emit: () => {}, log: nullLogger(), redactText: redactor });
    await h.proposeIssue({ repo_path: "g/p", title: leaky("t"), description: leaky("d"), labels: [leaky("x")] });
    assert.equal(sent.length, 1);
    assertNoKey(JSON.stringify(sent[0]));
    assert.match(String(sent[0]!.title), /^t: /);
  });

  it("absent redactor is identity", async () => {
    const sent: string[] = [];
    const client = { async replyMRThread(_r: string, _id: string, body: string) { sent.push(body); return { replied: true }; } } as unknown as WorkerClient;
    const h = makeForgeToolHandlers({ client, runId: "r", log: nullLogger() });
    await h.replyMrThread({ reply_id: "d1", body: "plain body " + KEY });
    assert.equal(sent[0], "plain body " + KEY);
  });
});
