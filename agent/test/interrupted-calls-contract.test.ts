import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { decodeInterruptedCalls, interruptedRegistration } from "../src/interrupted-calls.js";

const worker = "11111111-1111-4111-8111-111111111111";
const run = "22222222-2222-4222-8222-222222222222";
const identity = interruptedRegistration(worker, "registration-nonce", 1);
assert.ok(identity);

// Independent expectations pin names and nullability; the input is recorded by
// TestInterruptedCallsWireContract using the production Go DTO, never a TS fake.
const empty = {
  format: 1, run_id: run, worker_id: worker, register_nonce: "registration-nonce",
  claim_generation: 3, source_generation: null, inspected_count: 0,
  history_incomplete: true, unavailable: false, truncated: false, entries: [],
};
const full = {
  format: 1, run_id: run, worker_id: worker, register_nonce: "registration-nonce",
  claim_generation: 3, source_generation: 2, inspected_count: 4,
  history_incomplete: true, unavailable: false, truncated: true,
  entries: [
    { seq: 10, source_generation: 2, agent: "lead", agent_instance: "worker-display",
      agent_label: null, tool_name: "Bash", command: "git status", display_omitted: false },
    { seq: 8, source_generation: 2, agent: "lead", agent_instance: null,
      agent_label: "Implementation", tool_name: "Bash", command: "git diff --stat", display_omitted: true },
  ],
};

function fixture(name: string): unknown {
  return JSON.parse(readFileSync(new URL(`../../fixtures/interrupted-calls/${name}`, import.meta.url), "utf8"));
}

for (const [name, expected] of [
  ["response.empty.json", empty],
  ["response.full.json", full],
] as const) {
  test(`interrupted calls Go wire contract: ${name}`, () => {
    const raw = fixture(name);
    assert.deepEqual(raw, expected, "recorded field names, nulls and values");
    const decoded = decodeInterruptedCalls(raw, run, 3, identity);
    assert.ok(decoded, "production decoder must accept the Go response");
    assert.deepEqual(decoded, expected);
  });
}

test("interrupted calls recorded response stays bound to run, claim and registration", () => {
  const raw = fixture("response.full.json");
  assert.equal(decodeInterruptedCalls(raw, worker, 3, identity), undefined, "different run");
  assert.equal(decodeInterruptedCalls(raw, run, 4, identity), undefined, "different generation");
  const otherWorker = interruptedRegistration(run, identity.nonce, 1);
  const otherNonce = interruptedRegistration(worker, "other-registration", 1);
  assert.ok(otherWorker);
  assert.ok(otherNonce);
  assert.equal(decodeInterruptedCalls(raw, run, 3, otherWorker), undefined, "different worker");
  assert.equal(decodeInterruptedCalls(raw, run, 3, otherNonce), undefined, "different nonce");
});

test("interrupted calls decoder rejects renamed fields and lost nullability in recorded JSON", () => {
  // Every mutation starts from the real recorded response and keeps the key count.
  for (const key of Object.keys(full)) {
    const raw = fixture("response.full.json") as Record<string, unknown>;
    raw[`renamed_${key}`] = raw[key];
    delete raw[key];
    assert.equal(decodeInterruptedCalls(raw, run, 3, identity), undefined, `envelope ${key}`);
  }
  for (const key of Object.keys(full.entries[0]!)) {
    const raw = fixture("response.full.json") as { entries: Record<string, unknown>[] };
    const entry = raw.entries[0]!;
    entry[`renamed_${key}`] = entry[key];
    delete entry[key];
    assert.equal(decodeInterruptedCalls(raw, run, 3, identity), undefined, `entry ${key}`);
  }
  for (const key of ["source_generation", "entries"]) {
    const raw = fixture("response.empty.json") as Record<string, unknown>;
    raw[key] = key === "entries" ? null : [];
    assert.equal(decodeInterruptedCalls(raw, run, 3, identity), undefined, key);
  }
  for (const [index, key] of [[0, "agent_label"], [1, "agent_instance"]] as const) {
    const raw = fixture("response.full.json") as { entries: Record<string, unknown>[] };
    delete raw.entries[index]![key];
    assert.equal(decodeInterruptedCalls(raw, run, 3, identity), undefined, `missing nullable ${key}`);
  }
});
