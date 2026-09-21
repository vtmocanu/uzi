import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  formatCodexClassification,
  normalizeCodexErrorInfo,
  normalizeCodexStatus,
  normalizeCodexTerminalErrors,
  normalizeCodexUsage,
  pickCodexClassification,
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

  it("appends the classification suffix on failure when info is given", () => {
    assert.deepEqual(normalizeCodexTerminalErrors("failed", "failed", { classification: "unauthorized" }), [
      "codex turn ended with status: failed (unauthorized)",
    ]);
  });

  it("appends the classification AND http status when info carries one", () => {
    assert.deepEqual(
      normalizeCodexTerminalErrors("failed", "failed", { classification: "httpConnectionFailed", httpStatus: 503 }),
      ["codex turn ended with status: failed (httpConnectionFailed; http 503)"],
    );
  });

  it("is byte-identical to the 2-arg form when info is absent (backward-compatible)", () => {
    assert.deepEqual(normalizeCodexTerminalErrors("failed", "failed"), ["codex turn ended with status: failed"]);
  });

  it("is still empty on success even when info is given", () => {
    assert.deepEqual(normalizeCodexTerminalErrors("completed", "success", { classification: "unauthorized" }), []);
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

describe("normalizeCodexErrorInfo", () => {
  it("resolves an exact lower-camel scalar tag to its display + category", () => {
    assert.deepEqual(normalizeCodexErrorInfo("unauthorized"), {
      classification: "unauthorized",
      category: "authentication",
    });
  });

  it("resolves the other scalar categories correctly", () => {
    assert.deepEqual(normalizeCodexErrorInfo("internalServerError"), {
      classification: "internalServerError",
      category: "transport",
    });
    assert.deepEqual(normalizeCodexErrorInfo("badRequest"), { classification: "badRequest", category: "unknown" });
    assert.deepEqual(normalizeCodexErrorInfo("sandboxError"), { classification: "sandboxError", category: "unknown" });
  });

  it("resolves an exact tagged variant with a bounded http status", () => {
    assert.deepEqual(normalizeCodexErrorInfo({ httpConnectionFailed: { httpStatusCode: 503 } }), {
      classification: "httpConnectionFailed",
      category: "transport",
      httpStatus: 503,
    });
    assert.deepEqual(normalizeCodexErrorInfo({ responseStreamConnectionFailed: { httpStatusCode: 502 } }), {
      classification: "responseStreamConnectionFailed",
      category: "transport",
      httpStatus: 502,
    });
    assert.deepEqual(normalizeCodexErrorInfo({ responseStreamDisconnected: { httpStatusCode: 504 } }), {
      classification: "responseStreamDisconnected",
      category: "transport",
      httpStatus: 504,
    });
    assert.deepEqual(normalizeCodexErrorInfo({ responseTooManyFailedAttempts: { httpStatusCode: 429 } }), {
      classification: "responseTooManyFailedAttempts",
      category: "transport",
      httpStatus: 429,
    });
  });

  it("ignores the activeTurnNotSteerable payload entirely (no http status extracted)", () => {
    // Payload carries a well-formed httpStatusCode, yet this tagged variant is NOT an
    // `http` entry — so the status MUST still be dropped. This pins the no-extraction
    // behavior against a mutation that flips activeTurnNotSteerable to `http: true`.
    const result = normalizeCodexErrorInfo({ activeTurnNotSteerable: { turnKind: "regular", httpStatusCode: 503 } });
    assert.deepEqual(result, { classification: "activeTurnNotSteerable", category: "unknown" });
    assert.equal(result?.httpStatus, undefined);
  });

  it("collapses a shape MISMATCH to unknown (tagged tag as a bare string)", () => {
    assert.deepEqual(normalizeCodexErrorInfo("httpConnectionFailed"), {
      classification: "unknown",
      category: "unknown",
    });
  });

  it("collapses a shape MISMATCH to unknown (scalar tag given as an object)", () => {
    assert.deepEqual(normalizeCodexErrorInfo({ unauthorized: {} }), {
      classification: "unknown",
      category: "unknown",
    });
  });

  it("is casing-sensitive: PascalCase does NOT match (a casing regression must fail this)", () => {
    assert.deepEqual(normalizeCodexErrorInfo("Unauthorized"), { classification: "unknown", category: "unknown" });
  });

  it("drops an out-of-range or non-integer httpStatusCode but keeps the classification", () => {
    for (const bad of [503.7, Number.NaN, 42, 700]) {
      const result = normalizeCodexErrorInfo({ httpConnectionFailed: { httpStatusCode: bad } });
      assert.equal(result?.classification, "httpConnectionFailed");
      assert.equal(result?.category, "transport");
      assert.equal(result?.httpStatus, undefined, `httpStatusCode ${String(bad)} must be dropped`);
    }
  });

  it("keeps the boundary statuses 100 and 599", () => {
    assert.equal(normalizeCodexErrorInfo({ httpConnectionFailed: { httpStatusCode: 100 } })?.httpStatus, 100);
    assert.equal(normalizeCodexErrorInfo({ httpConnectionFailed: { httpStatusCode: 599 } })?.httpStatus, 599);
  });

  it("collapses an unknown tag to unknown", () => {
    assert.deepEqual(normalizeCodexErrorInfo("totallyMadeUp"), { classification: "unknown", category: "unknown" });
  });

  it("returns undefined for null / undefined", () => {
    assert.equal(normalizeCodexErrorInfo(null), undefined);
    assert.equal(normalizeCodexErrorInfo(undefined), undefined);
  });

  it("collapses every other shape to unknown", () => {
    for (const shape of [{}, { a: 1, b: 2 }, [], 5, true]) {
      assert.deepEqual(normalizeCodexErrorInfo(shape), { classification: "unknown", category: "unknown" });
    }
  });

  it("NEVER leaks a secret-shaped value in any of the three positions it can appear", () => {
    const secret = ["sk", "leak", "SHOULDNOTAPPEAR9999"].join("-");
    // (1) as a bare-string codexErrorInfo
    const asBareString = normalizeCodexErrorInfo(secret);
    // (2) as a one-key tag
    const asTag = normalizeCodexErrorInfo({ [secret]: { httpStatusCode: 503 } });
    // (3) as the value under a recognized tag's payload
    const inPayload = normalizeCodexErrorInfo({ httpConnectionFailed: { httpStatusCode: 503, detail: secret } });
    for (const result of [asBareString, asTag, inPayload]) {
      assert.ok(!JSON.stringify(result).includes(secret), "the secret substring never appears in the output");
    }
    assert.deepEqual(asBareString, { classification: "unknown", category: "unknown" });
    assert.deepEqual(asTag, { classification: "unknown", category: "unknown" });
    // The recognized tag still resolves; only the range-gated status is read, the secret detail is ignored.
    assert.deepEqual(inPayload, { classification: "httpConnectionFailed", category: "transport", httpStatus: 503 });
  });
});

describe("formatCodexClassification", () => {
  it("renders just the classification when no http status is present", () => {
    assert.equal(formatCodexClassification({ classification: "unauthorized" }), "unauthorized");
  });

  it("renders classification + http status when present", () => {
    assert.equal(
      formatCodexClassification({ classification: "httpConnectionFailed", httpStatus: 503 }),
      "httpConnectionFailed; http 503",
    );
  });
});

describe("pickCodexClassification", () => {
  const recognized = { classification: "unauthorized", category: "authentication" } as const;
  const unknown = { classification: "unknown", category: "unknown" } as const;
  const terminalRecognized = { classification: "usageLimitExceeded", category: "rate_limit" } as const;

  it("prefers a RECOGNIZED notification over a recognized terminal", () => {
    assert.deepEqual(pickCodexClassification(recognized, terminalRecognized), recognized);
  });

  it("prefers the RECOGNIZED terminal when the notification collapsed to 'unknown' (precedence guard)", () => {
    // A malformed notification's "unknown" must NOT mask a valid terminal turn.error.
    assert.deepEqual(pickCodexClassification(unknown, terminalRecognized), terminalRecognized);
  });

  it("returns an 'unknown' when BOTH are unknown (neither recognized)", () => {
    const other = { classification: "unknown", category: "transport" } as const;
    assert.deepEqual(pickCodexClassification(unknown, other), unknown);
  });

  it("returns the notification when the terminal is undefined", () => {
    assert.deepEqual(pickCodexClassification(recognized, undefined), recognized);
    // Even an 'unknown' notification is surfaced when there is no terminal fallback.
    assert.deepEqual(pickCodexClassification(unknown, undefined), unknown);
  });

  it("returns undefined when BOTH are undefined", () => {
    assert.equal(pickCodexClassification(undefined, undefined), undefined);
  });

  it("returns the terminal when the notification is undefined", () => {
    assert.deepEqual(pickCodexClassification(undefined, terminalRecognized), terminalRecognized);
  });
});
