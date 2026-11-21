import { test } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher } from "../src/batcher.js";
import { mapSdkMessage } from "../src/sdk-messages.js";
import { stripProviderCost } from "../src/provider-binding.js";
import { makeRedactor, providerKeyForms, SecretSet } from "../src/redact.js";
import { redactStrings } from "../src/publication-intent.js";
import { redactingLogger } from "../src/model-pass.js";
import { nullLogger } from "./helpers.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import type { Logger } from "../src/log.js";

const KEY = ["dict", "Opaque", "19abc", "72DEF"].join("");
const ENCODED_KEY = ["opaque/", "quote\"", "slash\\", "&<9>", " token"].join("");
const REDACTED = "***REDACTED***";
const encodedForms = providerKeyForms(ENCODED_KEY);

async function captureFeed(messages: ReturnType<typeof mapSdkMessage>) {
  const captured: OutgoingMessage[] = [];
  const client = { async postMessages(_id: string, out: OutgoingMessage[]) { captured.push(...out); } } as unknown as WorkerClient;
  const secrets = new SecretSet(providerKeyForms(KEY));
  const batcher = new MessageBatcher(client, "synthetic-member-regression", 0, 0, nullLogger(), secrets.payload, secrets.text);
  for (const message of messages) batcher.emit(stripProviderCost(message));
  await batcher.close();
  return captured;
}

test("provider tool-input member names are redacted before postMessages", async () => {
  const messages = mapSdkMessage({
    type: "assistant", message: { role: "assistant", content: [{
      type: "tool_use", id: "synthetic-call", name: "Read", input: { file_path: "example.txt", [KEY]: "ordinary-value" },
    }] },
  });
  messages.push({ kind: "text", agent: "lead", payload: { text: KEY } });
  const captured = await captureFeed(messages);
  assert.equal(captured.length, 2, "SDK projection and production batcher both delivered");
  assert.equal(captured[1]!.payload.text, REDACTED, "value-redaction positive control");
  assert.deepEqual(captured[0]!.payload.input, { file_path: "example.txt", [REDACTED]: "ordinary-value" });
  assert.ok(!JSON.stringify(captured).includes(KEY));
});

test("provider terminal modelUsage collisions preserve summed accounting on the run feed", async () => {
  const captured = await captureFeed(mapSdkMessage({
    type: "result", subtype: "success", is_error: false, result: "ordinary summary", num_turns: 1,
    modelUsage: {
      [KEY]: { inputTokens: 10, outputTokens: 2, costUSD: 0 },
      [REDACTED]: { inputTokens: 4, outputTokens: 3, costUSD: 0 },
    },
  }));
  const frame = captured.find(message => message.payload.modelUsage !== undefined);
  assert.ok(frame, "the production result projection delivers modelUsage");
  assert.deepEqual(frame.payload.modelUsage, { [REDACTED]: { inputTokens: 14, outputTokens: 5 } });
  assert.ok(!JSON.stringify(captured).includes(KEY));
});

const walkers = [
  { name: "makeRedactor", walk: (value: Record<string, unknown>) => makeRedactor(encodedForms)(value) },
  { name: "SecretSet.payload", walk: (value: Record<string, unknown>) => new SecretSet(encodedForms).payload(value) },
  { name: "redactStrings", walk: (value: Record<string, unknown>) => redactStrings(value, new SecretSet(encodedForms).text) },
];

for (const { name, walk } of walkers) {
  test(`${name}: raw and encoded member names and values are scrubbed`, () => {
    for (const form of encodedForms) {
      const input = { stable: [{ [`slot:${form}`]: `value:${form}` }], ordinary: "unchanged" };
      assert.deepEqual(walk(input), { stable: [{ [`slot:${REDACTED}`]: `value:${REDACTED}` }], ordinary: "unchanged" });
      assert.equal(Object.keys(input.stable[0]!)[0], `slot:${form}`, "input is not mutated");
    }
  });

  test(`${name}: colliding records merge recursively and sum finite numbers`, () => {
    const input = {
      [ENCODED_KEY]: { inputTokens: 10, nested: { calls: 2, label: "first" } },
      [REDACTED]: { inputTokens: 5, nested: { calls: 3, label: "second" }, outputTokens: 4 },
    };
    assert.deepEqual(walk(input), {
      [REDACTED]: { inputTokens: 15, nested: { calls: 5, label: "first", "label#2": "second" }, outputTokens: 4 },
    });
  });

  test(`${name}: incompatible collisions keep every value without stealing a real suffixed name`, () => {
    const secondForm = encodedForms.find(form => form !== ENCODED_KEY)!;
    assert.ok(secondForm);
    const input = {
      [ENCODED_KEY]: "first",
      [secondForm]: ["second"],
      [REDACTED]: true,
      [`${REDACTED}#2`]: "real field",
      unchanged: 42,
    };
    const output = walk(input);
    assert.deepEqual(output, {
      [REDACTED]: "first",
      [`${REDACTED}#3`]: ["second"],
      [`${REDACTED}#4`]: true,
      [`${REDACTED}#2`]: "real field",
      unchanged: 42,
    });
    assert.deepEqual(Object.keys(output), [REDACTED, `${REDACTED}#3`, `${REDACTED}#4`, `${REDACTED}#2`, "unchanged"]);
  });

  test(`${name}: ordinary and vendor-named fields keep their serialized bytes and order`, () => {
    const input = {
      kind: "tool_use", agent: "lead", input: { file_path: "example.txt", ordinary: 3 },
      modelUsage: { "vendor-model": { inputTokens: 10, outputTokens: 2 } },
      fields: [null, false, "ordinary"],
    };
    assert.equal(JSON.stringify(walk(input)), JSON.stringify(input));
  });

  test(`${name}: non-finite operands and overflowing sums are preserved under safe names`, () => {
    const nonfinite = walk({ [ENCODED_KEY]: Number.NaN, [REDACTED]: 2 });
    assert.ok(Number.isNaN(nonfinite[REDACTED]));
    assert.equal(nonfinite[`${REDACTED}#2`], 2);
    const overflow = walk({ [ENCODED_KEY]: Number.MAX_VALUE, [REDACTED]: Number.MAX_VALUE });
    assert.deepEqual(overflow, { [REDACTED]: Number.MAX_VALUE, [`${REDACTED}#2`]: Number.MAX_VALUE });
  });

  test(`${name}: class-instance collisions retain separate enumerable copies`, () => {
    const output = walk({ [ENCODED_KEY]: new Date(0), [REDACTED]: new Date(1) });
    assert.deepEqual(output, { [REDACTED]: {}, [`${REDACTED}#2`]: {} });
  });

  test(`${name}: prototype-looking names remain ordinary own data`, () => {
    const input = JSON.parse('{"__proto__":{"ordinary":true},"constructor":"field","toString":"data"}') as Record<string, unknown>;
    const output = walk(input);
    assert.equal(Object.getPrototypeOf(output), Object.prototype);
    assert.equal(Object.hasOwn(output, "__proto__"), true);
    assert.equal(JSON.stringify(output), JSON.stringify(input));
  });
}

test("vendor SecretSet without a provider key does not rewrite that key in member names", () => {
  const secrets = new SecretSet(["synthetic-vendor-credential-0192"]);
  const input = { [KEY]: { inputTokens: 10 }, ordinary: "unchanged" };
  assert.equal(JSON.stringify(secrets.payload(input)), JSON.stringify(input));
});

test("bounded logger: arbitrary diagnostic member names use the same collision policy", () => {
  const captured: Array<{ message: string; fields?: Record<string, unknown> }> = [];
  const logger: Logger = { ...nullLogger(), info: (message, fields) => { captured.push({ message, fields }); } };
  redactingLogger(logger, new SecretSet(encodedForms).text).info("ordinary message", {
    [ENCODED_KEY]: { calls: 2 }, [REDACTED]: { calls: 3 }, nested: { [encodedForms[0]!]: "ordinary" },
  });
  assert.deepEqual(captured, [{ message: "ordinary message", fields: {
    [REDACTED]: { calls: 5 }, nested: { [REDACTED]: "ordinary" },
  } }]);
});

test("bounded logger: existing depth and class-instance boundaries remain unchanged", () => {
  const captured: Array<Record<string, unknown> | undefined> = [];
  const logger: Logger = { ...nullLogger(), info: (_message, fields) => { captured.push(fields); } };
  const boundary = { marker: "not traversed beyond the diagnostic depth limit" };
  let deep: Record<string, unknown> = boundary;
  for (let i = 0; i < 5; i++) deep = { child: deep };
  const instance = new Date(0);
  redactingLogger(logger, new SecretSet(encodedForms).text).info("ordinary", { deep, instance });
  let found = captured[0]!.deep as Record<string, unknown>;
  for (let i = 0; i < 5; i++) found = found.child as Record<string, unknown>;
  assert.equal(found, boundary);
  assert.equal(captured[0]!.instance, instance);
});
