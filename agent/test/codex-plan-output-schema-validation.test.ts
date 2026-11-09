import { it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { CrossCheckMalformedError, PLAN_VERDICT_SCHEMA } from "../src/codex/cross-check.js";
import { replaySchemaFixture, VERDICT, type SchemaFixture } from "./codex-plan-output-schema-helper.js";
import { check } from "./json-schema-subset.js";

// Labelled derivatives of the M1 tape; the historical capture stays unchanged.
function withFinal(text: string): SchemaFixture {
 const f: SchemaFixture = JSON.parse(readFileSync(new URL("./fixtures/codex-plan-output-schema-0.164.0.json", import.meta.url), "utf8"));
 f.verdict = text;
 for (const event of f.events) {
  const p = event.frame.params;
  if (p?.item?.type === "agentMessage") p.item.text = text;
  for (const item of p?.turn?.items ?? []) if (item.type === "agentMessage") item.text = text;
  if (event.frame.method === "item/agentMessage/delta") p!.delta = text;
 }
 // The captured provider completed without deltas. Add the matching streamed form
 // to each derivative so rejection cannot be explained by a stream mismatch.
 const index = f.events.findIndex(e => e.frame.method === "item/completed" && e.frame.params?.item?.type === "agentMessage");
 const p = f.events[index]!.frame.params!;
 f.events.splice(index, 0, { direction: "in", frame: { method: "item/agentMessage/delta",
  params: { threadId: p.threadId, turnId: p.turnId, itemId: p.item.id, delta: text } } });
 return f;
}

const finding = { file: "anchor.ts", severity: "warning", summary: "Finding", rationale: "界".repeat(600) };
const summaryValue = { verdict: "approve", summary: "😀".repeat(1100), items: [] };
const individualValue = { verdict: "revise", summary: "Checked", items: [{ ...finding, rationale: "界".repeat(700) }] };
const aggregateValue = { verdict: "revise", summary: "Checked", items: Array.from({ length: 20 }, () => ({ ...finding })) };
const variants = [
 { name: "fenced", text: "```json\n" + VERDICT + "\n```", subreason: "invalid_json", fence: true },
 { name: "prose wrapped", text: "Result: " + VERDICT, subreason: "invalid_json", fence: false },
 { name: "empty", text: "", subreason: "invalid_json", fence: false },
 { name: "whitespace before fence", text: " \n```json\n" + VERDICT + "\n```", subreason: "invalid_json", fence: false },
 { name: "multibyte summary byte cap", text: JSON.stringify(summaryValue), subreason: "verdict_schema", fence: false, value: summaryValue },
 { name: "individual finding byte cap", text: JSON.stringify(individualValue), subreason: "findings_cap", fence: false, value: individualValue },
 { name: "aggregate finding byte cap", text: JSON.stringify(aggregateValue), subreason: "findings_cap", fence: false, value: aggregateValue },
];

for (const v of variants) {
 it("M2 fail-closed captured replay: " + v.name, async () => {
  if (v.value) assert.equal(check(PLAN_VERDICT_SCHEMA, v.value), true, "schema-valid before byte-cap rejection");
  if (v.name === "multibyte summary byte cap") assert.ok(Buffer.byteLength(summaryValue.summary) > 4096);
  if (v.name === "individual finding byte cap") assert.ok(Buffer.byteLength(JSON.stringify(individualValue.items[0])) > 2048);
  if (v.name === "aggregate finding byte cap") {
   for (const item of aggregateValue.items) assert.ok(Buffer.byteLength(JSON.stringify(item)) <= 2048);
   assert.ok(aggregateValue.items.reduce((sum, item) => sum + Buffer.byteLength(JSON.stringify(item)), 0) > 32 * 1024);
   assert.ok(Buffer.byteLength(v.text) < 64 * 1024);
  }
  await assert.rejects(replaySchemaFixture(withFinal(v.text)), (error: unknown) => {
   assert.ok(error instanceof CrossCheckMalformedError);
   assert.equal(error.subreason, v.subreason);
   assert.equal(error.message, v.subreason === "invalid_json" ? "cross-check invalid verdict JSON"
    : v.subreason === "verdict_schema" ? "cross-check invalid verdict schema" : "cross-check findings exceed cap");
   return true;
  });
 });
 it("M2 final text shape captured replay: " + v.name, async () => {
  await assert.rejects(replaySchemaFixture(withFinal(v.text)), (error: unknown) => {
   assert.ok(error instanceof CrossCheckMalformedError);
   assert.deepEqual(error.finalTextShape, {
    final_text_bytes: Buffer.byteLength(v.text, "utf8"), final_text_empty: v.text.length === 0,
    final_text_starts_with_code_fence: v.fence,
   });
   assert.equal(JSON.stringify(error).includes("Synthetic anchor checked"), false);
   return true;
  });
 });
}
