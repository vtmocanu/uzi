import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SecretSet } from "../src/redact.js";

const REDACTED = "***REDACTED***";
// Assembled at runtime so no provider-token-shaped literal sits in source.
const FIRST = "sk-ant-" + "oat01-first-secret-value";
const SECOND = "sk-ant-" + "oat01-second-secret-value";

describe("SecretSet", () => {
  it("scrubs the initial secrets in text and payloads", () => {
    const set = new SecretSet([FIRST, undefined, null]);
    assert.equal(set.text(`a ${FIRST} b`), `a ${REDACTED} b`);
    assert.deepEqual(set.payload({ k: [`x ${FIRST}`], n: 1 }), { k: [`x ${REDACTED}`], n: 1 });
  });

  it("scrubs a secret added after the redactors were handed out", () => {
    const set = new SecretSet([FIRST]);
    const { text, payload } = set;
    assert.equal(text(`v ${SECOND}`), `v ${SECOND}`);
    set.add(SECOND);
    assert.equal(text(`v ${SECOND} and ${FIRST}`), `v ${REDACTED} and ${REDACTED}`);
    assert.deepEqual(payload({ nested: { s: SECOND } }), { nested: { s: REDACTED } });
  });

  it("starts empty and still redacts after the first add", () => {
    const set = new SecretSet([]);
    const payload = { s: FIRST };
    assert.equal(set.payload(payload), payload, "identity when there is nothing to scrub");
    set.add(FIRST);
    assert.deepEqual(set.payload(payload), { s: REDACTED });
  });

  it("add is idempotent", () => {
    const set = new SecretSet([FIRST]);
    set.add(FIRST);
    set.add(FIRST);
    assert.deepEqual(set.values(), [FIRST]);
    assert.equal(set.text(FIRST), REDACTED);
  });

  it("keeps the short-secret floor for the initial list and for add", () => {
    const set = new SecretSet(["short", "1234567"]);
    set.add("abcdefg");
    assert.equal(set.text("short 1234567 abcdefg"), "short 1234567 abcdefg");
    assert.deepEqual(set.values(), []);
    set.add("12345678");
    assert.equal(set.text("x 12345678 y 1234567"), `x ${REDACTED} y 1234567`);
  });

  it("scrubs an added secret split by invisible characters", () => {
    const set = new SecretSet([]);
    set.add(SECOND);
    const half = SECOND.length >> 1;
    for (const sep of ["​", "­", "\r", "\u001b[31m"]) {
      const split = SECOND.slice(0, half) + sep + SECOND.slice(half);
      const out = set.text(`a ${split} b`);
      assert.equal(out, `a ${REDACTED} b`);
      assert.ok(!out.includes(SECOND.slice(0, half)));
    }
  });
});
