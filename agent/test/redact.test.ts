import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { makeRedactor, makeTextRedactor, providerKeyForms } from "../src/redact.js";

const TOKEN = "dummy-oauth-token-do-not-scan-0000";
const PAT = "dummy-forge-pat-do-not-scan-1111";
const REDACTED = "***REDACTED***";
// Mirrors api/internal/termsafe SanitizeTTY: drop Cc (except \t, \n) and Cf.
const displayAsTTY = (s: string) => s.replace(/[\p{Cc}\p{Cf}]/gu, (c) => (c === "\t" || c === "\n" ? c : ""));

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

  it("redacts two secrets that overlap in the text in full, whichever is listed first", () => {
    const text = "x alphaalpha1234betabetabeta y";
    assert.strictEqual(makeTextRedactor(["alphaalpha1234", "1234betabetabeta"])(text), `x ${REDACTED} y`);
    assert.strictEqual(makeTextRedactor(["1234betabetabeta", "alphaalpha1234"])(text), `x ${REDACTED} y`);
    assert.strictEqual(makeTextRedactor(["alphaalpha1234", "1234betabetabeta"])("a alphaalpha1234 b 1234betabetabeta c"), `a ${REDACTED} b ${REDACTED} c`);
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

describe("makeTextRedactor over more than 64 secrets", () => {
  const fillers = Array.from({ length: 70 }, (_, i) => `filler-secret-${String(i).padStart(3, "0")}-dummy`);
  const first = "aaaaaaaa-1";
  const second = "-1bbbbbbbbbbbbbbbb";
  const zwsp = "​";

  it("redacts two distinct secrets that overlap in plain text, in full", () => {
    const scrub = makeTextRedactor([...fillers, first, second]);
    assert.equal(scrub("x aaaaaaaa-1bbbbbbbbbbbbbbbb y"), `x ${REDACTED} y`);
  });

  it("redacts overlapping secrets split by an invisible character exactly as the per-secret path does", () => {
    const text = `x aaaaaaaa-${zwsp}1bbbbbbbbbbbbbbbb y`;
    const many = makeTextRedactor([...fillers, first, second]);
    const few = makeTextRedactor([first, second]);
    assert.equal(many(text), few(text));
    assert.ok(!many(text).includes("bbbbbbbb"), many(text));
  });

  it("matches a secret that starts inside another match", () => {
    const scrub = makeTextRedactor([...fillers, "abcdefgh-ijkl", "ijkl-mnopqrstu"]);
    assert.equal(scrub("<abcdefgh-ijkl-mnopqrstu>"), `<${REDACTED}>`);
  });

  it("redacts a large text with a bounded cost", () => {
    const scrub = makeTextRedactor([...fillers, first]);
    const text = `${"lorem ipsum dolor ".repeat(200000)}${first}`;
    const started = Date.now();
    const out = scrub(text);
    const ms = Date.now() - started;
    assert.ok(out.endsWith(REDACTED));
    assert.ok(ms < 5000, `took ${ms}ms`);
  });
});

// A secret built from a repeating block X (S = XX) overlaps itself, and a verbatim occurrence can
// overlap an invisible-separated one. Every span is found in the original text and the union is
// replaced once, so no block of the secret survives (#4688).
describe("overlapping occurrences redact as one union", () => {
  const block = ["run", "owned", "Secret", "9f3k2"].join("-");
  const secret = block + block;
  const zwsp = "\u200b";

  it("a verbatim occurrence overlapping an invisible-separated one leaves no block", () => {
    const stored = makeTextRedactor([secret])(`before ${secret}${zwsp}${block} after`);
    assert.equal(stored, `before ${REDACTED} after`);
    assert.ok(!displayAsTTY(stored).includes(block), JSON.stringify(stored));
  });

  it("self-overlapping invisible-separated occurrences leave no block", () => {
    const stored = makeTextRedactor([secret])(`before ${block}${zwsp}${block}${zwsp}${block} after`);
    assert.equal(stored, `before ${REDACTED} after`);
    assert.ok(!displayAsTTY(stored).includes(block), JSON.stringify(stored));
  });

  it("a self-overlapping verbatim run is redacted whole", () => {
    assert.equal(makeTextRedactor(["aaaaaaaa"])("a".repeat(20)), REDACTED);
  });

  it("occurrences that only touch stay separate markers", () => {
    assert.equal(makeTextRedactor(["abcdefgh"])("abcdefghabcdefgh"), REDACTED + REDACTED);
  });

  it("many disjoint invisible-pass matches do not overflow the argument limit", () => {
    // Spans are collected into one array, never spread as call arguments (a spread of ~300k
    // spans threw RangeError before any replacement ran).
    const count = 300_000;
    const stored = makeTextRedactor(["abcdefgh"])("\u200b" + "abcdefgh ".repeat(count));
    assert.equal(stored, "\u200b" + `${REDACTED} `.repeat(count));
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

describe("providerKeyForms", () => {
  const KEY = ["gw", "key", "do-not-scan", "ab/cd", "efgh"].join("-");

  it("lists raw, JSON-slash and percent forms, deduplicated, longest first", () => {
    const forms = providerKeyForms(KEY);
    assert.ok(forms.includes(KEY));
    assert.ok(forms.includes(KEY.replaceAll("/", "\\/")));
    assert.ok(forms.includes(encodeURIComponent(KEY)));
    assert.ok(forms.includes(encodeURIComponent(KEY).replace(/%[0-9A-F]{2}/g, (m) => m.toLowerCase())));
    assert.equal(new Set(forms).size, forms.length, "no duplicates");
    for (let i = 1; i < forms.length; i++) assert.ok(forms[i - 1]!.length >= forms[i]!.length, "longest first");
  });

  it("collapses to one form for a key every encoding leaves untouched", () => {
    assert.deepEqual(providerKeyForms("plainkey12345678"), ["plainkey12345678"]);
  });
});

describe("makeTextRedactor with a long periodic secret", () => {
  // A 70 000 unit secret whose 8-unit prefix recurs every 9 units: every shift of one real occurrence is a
  // prefix candidate, but only the shifts that leave a border of the secret can be occurrences.
  const long = ["Q9Z7W4X2", "-"].join("").repeat(8000).slice(0, 70_000);

  it("redacts the one real occurrence inside plain text without withholding", () => {
    const text = `${"a".repeat(20_000)}${long}${"b".repeat(50_000)}`;
    const out = makeTextRedactor([long])(text);
    assert.equal(out, `${"a".repeat(20_000)}${REDACTED}${"b".repeat(50_000)}`);
  });

  it("redacts two overlapping occurrences sharing a border as one union and keeps a partial one", () => {
    const secret = "abcdeabc"; // border "abc"
    assert.equal(makeTextRedactor([secret])("<abcdeabcdeabc>"), `<${REDACTED}>`);
    assert.equal(makeTextRedactor([secret])("<abcdeab>"), "<abcdeab>");
    assert.equal(makeTextRedactor([secret])("<abcdeabcdeab>"), `<${REDACTED}deab>`);
  });

  it("redacts every overlapping occurrence of a periodic secret and no partial one", () => {
    const secret = "abababababababab"; // 16 units, period 2
    const run = "ab".repeat(12); // five overlapping occurrences at shifts 0, 2, 4, 6, 8
    assert.equal(makeTextRedactor([secret])(`<${run}>`), `<${REDACTED}>`);
    const partial = "ab".repeat(7); // 14 units: shorter than the secret, so not an occurrence
    assert.equal(makeTextRedactor([secret])(`<${partial}>`), `<${partial}>`);
  });
});
