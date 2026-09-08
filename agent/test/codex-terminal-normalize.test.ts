import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  normalizeCodexStatus,
  normalizeCodexTerminalErrors,
  normalizeCodexUsage,
} from "../src/codex/terminal-normalize.js";

// PRD #1171 (M3, milestone 3) — the pure provider-terminal normalizers. These bound and
// redact untrusted Codex terminal fields (status / turn.error / turn.usage) for BOTH the
// run and advice decoders, so the two lanes cannot diverge. Every "secret-shaped" fixture
// is assembled at RUNTIME so no secret literal sits in the source.

describe("normalizeCodexStatus", () => {
  it("maps the exact status 'completed' to success + subtype 'completed'", () => {
    assert.deepEqual(normalizeCodexStatus("completed"), { subtype: "completed", outcome: "success" });
  });

  it("maps an allowlisted failure status to failed but keeps the subtype token", () => {
    for (const s of ["failed", "cancelled", "interrupted", "incomplete", "error", "timeout"]) {
      assert.deepEqual(normalizeCodexStatus(s), { subtype: s, outcome: "failed" });
    }
  });

  it("collapses an ARBITRARY provider status to failed + subtype 'unknown' (never echoes it)", () => {
    const arbitrary = ["provider", "made", "this", "up"].join("-");
    const r = normalizeCodexStatus(arbitrary);
    assert.equal(r.outcome, "failed");
    assert.equal(r.subtype, "unknown");
    assert.ok(!r.subtype.includes(arbitrary), "the arbitrary status is never echoed as the subtype");
  });

  it("collapses an OVERSIZE provider status to 'unknown' rather than echoing a huge string", () => {
    const oversize = "z".repeat(100_000);
    const r = normalizeCodexStatus(oversize);
    assert.equal(r.subtype, "unknown");
    assert.equal(r.outcome, "failed");
  });

  it("treats an absent status as failed + 'unknown'", () => {
    assert.deepEqual(normalizeCodexStatus(undefined), { subtype: "unknown", outcome: "failed" });
  });
});

describe("normalizeCodexTerminalErrors", () => {
  it("is empty on success", () => {
    assert.deepEqual(normalizeCodexTerminalErrors("completed", "success"), []);
  });

  it("is a single fixed diagnostic derived only from the closed subtype on failure", () => {
    assert.deepEqual(normalizeCodexTerminalErrors("failed", "failed"), ["codex turn ended with status: failed"]);
    assert.deepEqual(normalizeCodexTerminalErrors("unknown", "failed"), ["codex turn ended with status: unknown"]);
  });

  it("never contains a planted secret substring (it reads no raw provider text)", () => {
    // The function signature takes ONLY the closed subtype/outcome, never the raw error —
    // this asserts the contract: a secret can only leak if a caller feeds it as `subtype`,
    // which the caller (decodeTerminal) never does. Prove the fixed shape carries no secret.
    const secret = ["sk", "leak", "SHOULDNOTAPPEAR9999"].join("-");
    const errors = normalizeCodexTerminalErrors("unknown", "failed");
    assert.ok(!errors.join(" ").includes(secret));
  });
});

describe("normalizeCodexUsage", () => {
  it("returns undefined when rawUsage is not a plain object", () => {
    assert.equal(normalizeCodexUsage(undefined, "turn"), undefined);
    assert.equal(normalizeCodexUsage(null, "turn"), undefined);
    assert.equal(normalizeCodexUsage("string", "turn"), undefined);
    assert.equal(normalizeCodexUsage(42, "turn"), undefined);
    assert.equal(normalizeCodexUsage([1, 2, 3], "call"), undefined);
  });

  it("keeps finite numeric fields >= 0 and drops a secret STRING field and any nested object", () => {
    const secret = ["sk", "usage", "MUSTNOTLEAK1234567890"].join("-");
    const raw = {
      input_tokens: 5,
      output_tokens: 0,
      token: secret, // string → dropped
      breakdown: { a: 1, blob: "y".repeat(2048) }, // nested object → dropped
      list: [1, 2, 3], // array → dropped
      flag: true, // boolean → dropped
      negative: -1, // negative → dropped
      nan: Number.NaN, // NaN → dropped
      inf: Number.POSITIVE_INFINITY, // Infinity → dropped
    };
    const usage = normalizeCodexUsage(raw, "turn");
    assert.ok(usage);
    assert.equal(usage.basis, "turn");
    assert.deepEqual(usage.tokens, {});
    assert.deepEqual(usage.wire, { usage: { input_tokens: 5, output_tokens: 0 } });

    const json = JSON.stringify(usage);
    assert.ok(!json.includes(secret), "no secret string leaked");
    assert.ok(!json.includes("blob"), "no nested object retained");
    assert.ok(!json.includes("breakdown"), "the nested object key is dropped");
  });

  it("NEVER returns the raw object (builds a fresh bounded subset)", () => {
    const raw: Record<string, unknown> = { input_tokens: 9 };
    const usage = normalizeCodexUsage(raw, "call");
    assert.ok(usage);
    assert.ok(usage.wire);
    assert.notEqual(usage.wire.usage, raw, "the wire usage is a fresh object, not the raw reference");
    assert.deepEqual(usage.wire.usage, { input_tokens: 9 });
  });

  it("caps the number of retained numeric keys", () => {
    const raw: Record<string, number> = {};
    for (let i = 0; i < 64; i++) raw[`k${i}`] = i;
    const usage = normalizeCodexUsage(raw, "turn");
    assert.ok(usage);
    assert.ok(usage.wire);
    const kept = Object.keys(usage.wire.usage as Record<string, unknown>);
    assert.ok(kept.length < 64, "the retained keys are capped below the raw count");
    assert.ok(kept.length <= 32, "the retained key count honours the cap");
  });
});
