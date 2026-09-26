import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";

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

describe("makeTextRedactor", () => {
  it("scrubs secret substrings from a bare string (e.g. a failure_reason)", () => {
    const redact = makeTextRedactor([TOKEN, PAT]);
    assert.strictEqual(
      redact(`fatal: auth failed with ${PAT} and ${TOKEN}`),
      `fatal: auth failed with ${REDACTED} and ${REDACTED}`,
    );
  });

  it("is an identity when no usable secret is supplied", () => {
    const redact = makeTextRedactor([undefined, null, "", "short"]);
    assert.strictEqual(redact(`keeps ${TOKEN}`), `keeps ${TOKEN}`);
  });
});

// The worker redacts run-owned secrets by value, but the CLI/TUI render payloads through
// termsafe.SanitizeTTY, which DELETES control (Cc) and format (Cf) characters. A secret split by
// one of them (zero-width space, soft hyphen, \r, \b, a bidi mark) escaped exact matching, was
// stored split, and re-joined on screen. Only the worker knows these exact values, so the worker
// must match across those invisible separators.
describe("redaction across invisible separators", () => {
  // An arbitrary synthetic run-owned secret (not provider-shaped), built at runtime.
  const secret = ["run", "owned", "Secret", "9f3k2"].join("-");
  // Mirrors api/internal/termsafe SanitizeTTY: drop Cc (except \t, \n) and Cf.
  const displayAsTTY = (s: string) => s.replace(/[\p{Cc}\p{Cf}]/gu, (c) => (c === "\t" || c === "\n" ? c : ""));
  const split = (sep: string) => secret.slice(0, 5) + sep + secret.slice(5, 11) + sep + secret.slice(11);
  const separators: Array<[string, string]> = [
    ["zero-width space", "​"],
    ["soft hyphen", "­"],
    ["carriage return", "\r"],
    ["backspace", "\b"],
    ["right-to-left mark", "‏"],
    ["escape sequence", "\u001b[0m"],
  ];

  for (const [name, sep] of separators) {
    it(`text redactor: a secret split by ${name} never re-forms on display`, () => {
      const scrub = makeTextRedactor([secret]);
      const stored = scrub(`before ${split(sep)} after`);
      assert.ok(!displayAsTTY(stored).includes(secret), `displayed: ${JSON.stringify(displayAsTTY(stored))}`);
      assert.match(stored, /\*\*\*REDACTED\*\*\*/);
      assert.ok(stored.startsWith("before ") && stored.endsWith(" after"), "surrounding text is kept");
    });
  }

  it("payload redactor: nested split secrets are redacted before persistence", () => {
    const redact = makeRedactor([secret]);
    const out = redact({ tool_result: { stdout: `token=${split("​")}`, lines: [split("­")] } });
    const shown = displayAsTTY(JSON.stringify(out));
    assert.ok(!shown.includes(secret), shown);
  });

  it("does not over-redact: invisible characters between unrelated text are untouched", () => {
    const scrub = makeTextRedactor([secret]);
    const text = "a​b soft­hyphen";
    assert.equal(scrub(text), text);
  });

  it("a separator inside a secret shorter than the floor is still ignored", () => {
    const scrub = makeTextRedactor(["short1"]);
    assert.equal(scrub("sho​rt1"), "sho​rt1");
  });
});

describe("redaction across invisible separators: the shared fixture", () => {
  type Case = { name: string; rejoins_on_tty: boolean; input: string; redacted: string };
  const fixture = JSON.parse(
    readFileSync(new URL("../../fixtures/split-secret-redaction/cases.json", import.meta.url), "utf8"),
  ) as { secret: string; cases: Case[] };
  const scrub = makeTextRedactor([fixture.secret]);
  for (const c of fixture.cases) {
    it(`fixture: ${c.name}`, () => assert.equal(scrub(c.input), c.redacted));
  }
});
