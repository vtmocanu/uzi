import { it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { writeFile, stat, rm } from "node:fs/promises";
import path from "node:path";
import { captureSchemaFixture } from "./codex-plan-output-schema.probe.js";
import { assertFixture, replaySchemaFixture, VERDICT, type SchemaFixture } from "./codex-plan-output-schema-helper.js";

const fixturePath = new URL("./fixtures/codex-plan-output-schema-0.164.0.json", import.meta.url);
const load = (): SchemaFixture => JSON.parse(readFileSync(fixturePath, "utf8"));

it("probe import exposes capture without executing it", () => {
 assert.equal(typeof captureSchemaFixture, "function");
});

it("setup failure removes owned scratch before any Codex launch", async () => {
 let owned: string | undefined;
 try {
  await assert.rejects(captureSchemaFixture(undefined, async base => {
   owned = base;
   await writeFile(path.join(base, "checkout"), "synthetic setup blocker");
  }), { code: "EEXIST" });
  assert.ok(owned);
  await assert.rejects(stat(owned), { code: "ENOENT" });
 } finally {
  if (owned) await rm(owned, { recursive: true, force: true });
 }
});

// Missing capture is deliberately a failure, never a fabricated tape or silent skip.
it("captured strict-schema turn replays through CodexCrossCheck with a usable Read", async () => {
 const fixture = load();
 assertFixture(fixture);
 const result = await replaySchemaFixture(fixture);
 assert.equal(result.text, VERDICT);
 assert.equal(result.reads, 1);
 assert.equal(result.cleanup, true);
 assert.deepEqual(result.disposed, ["provider", "fileop"]);
});

async function rejectsDamagedReplay(fixture: SchemaFixture, expectedMessage: string): Promise<void> {
 await assert.rejects(replaySchemaFixture(fixture), (error: Error) => {
  assert.equal(error.message, expectedMessage, "must reach the damaged replay seam");
  return true;
 });
}

for (const target of ["item/tool/call", "turn/completed"] as const) {
 it("rejects capture missing " + target, async () => {
  const f = load();
  f.events = f.events.filter(e => e.frame.method !== target);
  assert.throws(() => assertFixture(f));
  await rejectsDamagedReplay(f, "cross-check ended without a result or terminal turn");
 });
 it("rejects capture altering " + target, async () => {
  const f = load();
  const e = f.events.find(e => e.frame.method === target)!;
  if (target === "item/tool/call") e.frame.params!.arguments.path = "missing.ts";
  else e.frame.params!.turn.status = "failed";
  assert.throws(() => assertFixture(f));
  await rejectsDamagedReplay(f, target === "item/tool/call"
   ? "codex transport write stream error" : "cross-check model error: terminal turn unsuccessful");
 });
}
