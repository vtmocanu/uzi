import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { Readable } from "node:stream";
import { verifyClosureFrames, RecoveryClosureLimitError } from "../src/recovery-closure.js";

const content = Buffer.from("public history");
const oid = createHash("sha1").update(`blob ${content.length}\0`).update(content).digest("hex");
const object = { oid, type: "blob", size: content.length };
const frame = Buffer.concat([Buffer.from(`${oid} blob ${content.length}\n`), content, Buffer.from("\n")]);
test("raw batch frames accept every header/content split and charge actual decoded bytes", async () => {
  for (let split = 1; split < frame.length; split++) {
    const budget = { delivered: 0, limit: content.length };
    await verifyClosureFrames(Readable.from([frame.subarray(0, split), frame.subarray(split)]), [object], budget);
    assert.equal(budget.delivered, content.length);
  }
});
test("delivered-byte overflow is charged across roots and never refunded", async () => {
  const budget = { delivered: 0, limit: content.length + 1 };
  await verifyClosureFrames(Readable.from([frame]), [object], budget);
  await assert.rejects(verifyClosureFrames(Readable.from([frame]), [object], budget), RecoveryClosureLimitError);
  assert.equal(budget.delivered, content.length);
});
for (const [name, bytes] of [
  ["truncated header", frame.subarray(0, 20)],
  ["truncated content", frame.subarray(0, frame.length - 3)],
  ["missing separator", frame.subarray(0, frame.length - 1)],
  ["extra output", Buffer.concat([frame, Buffer.from("extra")])],
  ["bad separator", Buffer.concat([frame.subarray(0, -1), Buffer.from("x")])],
  ["hash mismatch", Buffer.concat([frame.subarray(0, frame.length - content.length - 1), Buffer.alloc(content.length), Buffer.from("\n")])],
  ["malformed header", Buffer.from("bad\n")],
  ["oversized header", Buffer.from("a".repeat(101) + "\n")],
  ["size mismatch", Buffer.from(`${oid} blob 0\n\n`)],
  ["type mismatch", Buffer.from(`${oid} tree ${content.length}\n`)],
] as const) test(name + " fails closed", async () => {
  await assert.rejects(verifyClosureFrames(Readable.from([bytes]), [object], { delivered: 0, limit: 100 }));
});
test("nonzero producer error and already interrupted verification cannot pass", async () => {
  async function* failed() { yield frame; throw new Error("producer exited 1"); }
  await assert.rejects(verifyClosureFrames(Readable.from(failed()), [object], { delivered: 0, limit: 100 }), /exited 1/);
  const budget = { delivered: 7, limit: 100 };
  await assert.rejects(verifyClosureFrames(Readable.from([frame]), [object], budget, AbortSignal.abort(new Error("interrupted"))), /interrupted/);
  assert.equal(budget.delivered, 7);
});
