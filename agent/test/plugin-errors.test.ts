// issue #1888: the fail-closed decode and bounded, redacted rendering of the SDK init frame's
// `plugin_errors`.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { describePluginErrors, parsePluginErrors } from "../src/plugin-errors.js";

// A secret-shaped value assembled at runtime so no scanner sees a complete literal.
const SECRET = "sk-" + "fake" + "-" + "Z".repeat(40);
const redact = (s: string): string => s.split(SECRET).join("[REDACTED]");

/** True when the text holds a C0/C1 control or a bidi control code point. */
function hasUnsafe(text: string): boolean {
  for (const ch of text) {
    const c = ch.codePointAt(0) ?? 0;
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x200e || c === 0x200f) return true;
    if ((c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069)) return true;
  }
  return false;
}

describe("parsePluginErrors", () => {
  it("returns undefined only for absent, null or an empty array", () => {
    assert.equal(parsePluginErrors(undefined), undefined);
    assert.equal(parsePluginErrors(null), undefined);
    assert.equal(parsePluginErrors([]), undefined);
  });

  it("keeps one entry per element for a malformed-only array, with placeholders", () => {
    const out = parsePluginErrors([null, 42, {}, { plugin: 7, message: {} }]);
    assert.deepEqual(out, [
      { plugin: "(unnamed plugin)", type: "malformed-entry", message: "(malformed plugin error entry)" },
      { plugin: "(unnamed plugin)", type: "malformed-entry", message: "(malformed plugin error entry)" },
      { plugin: "(unnamed plugin)", type: "malformed-entry", message: "(no usable error detail)" },
      { plugin: "(unnamed plugin)", type: "malformed-entry", message: "(no usable error detail)" },
    ]);
  });

  it("keeps the valid string fields of a mixed entry and drops a non-string path", () => {
    const out = parsePluginErrors([
      { plugin: "uzi-skills", type: 3, message: "boom", path: 9 },
      { plugin: "x", type: "path-not-found", message: "gone", path: "/a/b" },
    ]);
    assert.deepEqual(out, [
      { plugin: "uzi-skills", type: "malformed-entry", message: "boom" },
      { plugin: "x", type: "path-not-found", message: "gone", path: "/a/b" },
    ]);
  });

  it("turns any other present non-array value into one generic entry", () => {
    for (const raw of [{ plugin: "p" }, "boom", 0, 1, false, true]) {
      assert.deepEqual(parsePluginErrors(raw), [
        { plugin: "(unnamed plugin)", type: "malformed-entry", message: "(malformed plugin error entry)" },
      ]);
    }
  });
});

describe("describePluginErrors", () => {
  it("renders plugin (type): message [path: ...] and passes an unknown type through", () => {
    assert.equal(
      describePluginErrors([{ plugin: "uzi-skills", type: "brand-new-kind", message: "bad", path: "/p" }]),
      "uzi-skills (brand-new-kind): bad [path: /p]",
    );
  });

  it("strips control and bidi characters", () => {
    const out = describePluginErrors([
      { plugin: "a\u001b[31m", type: "t\u202e", message: "line1\nline2\u0007", path: "/x\u2066" },
    ]);
    assert.ok(!hasUnsafe(out), out);
    assert.ok(out.includes("line1?line2?"), out);
  });

  it("redacts a secret straddling the message cap before bounding (no prefix survives)", () => {
    // The secret starts at code point 190 of the message, so a cap-then-redact would keep a
    // 10-character prefix of it.
    const message = "m".repeat(190) + SECRET + " tail";
    const out = describePluginErrors([{ plugin: "p", type: "t", message }], redact);
    assert.ok(!out.includes(SECRET.slice(0, 6)), out);
    assert.ok(out.includes("[REDACTED]"), out);
    // Without redaction the prefix does survive, so the check above is meaningful.
    assert.ok(describePluginErrors([{ plugin: "p", type: "t", message }]).includes(SECRET.slice(0, 6)));
  });

  it("bounds a 10k message and the whole string to 360 characters", () => {
    const out = describePluginErrors([
      { plugin: "p".repeat(10_000), type: "t".repeat(10_000), message: "m".repeat(10_000), path: "x".repeat(10_000) },
    ]);
    assert.ok(out.length <= 360, `${out.length}`);
    assert.ok(!out.includes("m".repeat(204)));
  });

  it("shows the first three entries then `and N more`, still within 360", () => {
    const many = Array.from({ length: 5 }, (_, i) => ({ plugin: `p${i}`, type: "t", message: "m" }));
    assert.equal(describePluginErrors(many), "p0 (t): m; p1 (t): m; p2 (t): m; and 2 more");
    const big = Array.from({ length: 9 }, (_, i) => ({ plugin: `p${i}`, type: "t", message: "m".repeat(500) }));
    const out = describePluginErrors(big);
    assert.ok(out.length <= 360, `${out.length}`);
    assert.ok(out.endsWith("; and 6 more"), out);
  });
});
