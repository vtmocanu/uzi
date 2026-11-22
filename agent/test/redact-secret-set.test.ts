import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { PER_SECRET_MAX, SecretSet } from "../src/redact.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";

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

describe("SecretSet over a run's secret variable forms", () => {
  const ENTROPY = Array.from({ length: 80 }, (_, i) => createHash("sha256").update(`entropy-${i}`).digest("base64"));
  const value = ["{", '  "type": "service_account",', "Proc-Type: 4,ENCRYPTED", " ".repeat(8), ...ENTROPY, "}"].join("\n");
  const claim = runEnvClaimForms([
    { var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "SVC_JSON", value, secret: true, description: "" },
  ], []);

  it("the claim registers more forms than the per-secret path serves", () => {
    assert.ok(claim.redaction.length > PER_SECRET_MAX, `${claim.redaction.length} forms`);
  });

  it("leaves public lines and padding unmasked and masks an entropy line, in text and payloads", () => {
    const set = new SecretSet(claim.redaction);
    for (const line of ['  "type": "service_account",', "Proc-Type: 4,ENCRYPTED", " ".repeat(8)]) {
      assert.equal(set.text(`a\n${line}\nb`), `a\n${line}\nb`);
      assert.deepEqual(set.payload({ k: line }), { k: line });
    }
    assert.equal(set.text(`a ${ENTROPY[1]} b`), `a ${REDACTED} b`);
    assert.deepEqual(set.payload({ k: [ENTROPY[2]] }), { k: [REDACTED] });
  });

  it("a form added after the redactors were handed out is masked on the indexed path", () => {
    const set = new SecretSet(claim.redaction);
    const late = "late-added-form-0123456789";
    assert.equal(set.text(late), late);
    set.add(late);
    assert.equal(set.text(`x ${late} y`), `x ${REDACTED} y`);
    assert.equal(set.text(`x ${ENTROPY[0]} y`), `x ${REDACTED} y`);
  });

  it("an invisible separator inside a form added late is still matched on the indexed path", () => {
    const set = new SecretSet(claim.redaction);
    set.text("warm the index");
    const late = "split-by-invisible-0123456789";
    set.add(late);
    assert.equal(set.text(`x ${late.slice(0, 8)}​${late.slice(8)} y`), `x ${REDACTED} y`);
  });
});
