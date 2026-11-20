import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { makeForgeToolHandlers } from "../src/forge-tools.js";
import { makeFindingsToolHandlers } from "../src/findings-tools.js";
import { makeMemoryToolHandlers } from "../src/memory-tools.js";
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

  it("vendor control: no redactor leaves text unchanged", async () => {
    const sent: string[] = [];
    const client = { async replyMRThread(_r: string, _id: string, body: string) { sent.push(body); return { replied: true }; } } as unknown as WorkerClient;
    const h = makeForgeToolHandlers({ client, runId: "r", log: nullLogger() });
    await h.replyMrThread({ reply_id: "d1", body: "plain body " + KEY });
    assert.equal(sent[0], "plain body " + KEY);
  });
});
