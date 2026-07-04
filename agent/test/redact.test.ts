import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { makeRedactor } from "../src/redact.js";

const TOKEN = "dummy-oauth-token-do-not-scan-0000";
const PAT = "dummy-forge-pat-do-not-scan-1111";
const REDACTED = "***REDACTED***";

describe("makeRedactor", () => {
  it("scrubs secret substrings from strings, arrays, and nested objects", () => {
    const redact = makeRedactor([TOKEN, PAT]);
    const out = redact({
      text: `here is ${TOKEN} inline`,
      content: [{ type: "tool_result", output: `leaked ${PAT}` }],
      nested: { deeper: { value: TOKEN } },
      untouched: 42,
    });
    assert.strictEqual(out.text, `here is ${REDACTED} inline`);
    assert.deepStrictEqual(out.content, [{ type: "tool_result", output: `leaked ${REDACTED}` }]);
    assert.deepStrictEqual(out.nested, { deeper: { value: REDACTED } });
    assert.strictEqual(out.untouched, 42);
  });

  it("is an identity when no usable secret is supplied", () => {
    const payload = { a: TOKEN };
    const same = makeRedactor([undefined, null, ""]);
    assert.strictEqual(same(payload), payload); // same reference — no work done
  });

  it("ignores secrets shorter than 8 chars (would corrupt unrelated output)", () => {
    const redact = makeRedactor(["short"]);
    assert.deepStrictEqual(redact({ v: "short and sweet" }), { v: "short and sweet" });
  });
});
