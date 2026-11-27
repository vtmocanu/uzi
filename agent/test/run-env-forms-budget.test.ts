import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import crypto from "node:crypto";
import { RUN_ENV_FORM_CHARS_MAX, runEnvClaimForms } from "../src/run-env-forms.js";
import { providerKeyForms } from "../src/redact.js";
import type { ClaimRunEnvVar } from "../src/protocol.js";

// PRD 4416: the api refuses a variable write that would push a repo's secret value bytes past
// MaxClaimSecretBytes (floor(form_chars_max / form_chars_per_secret_byte)), so a store filled to its own
// limits never makes a claim whose forms pass RUN_ENV_FORM_CHARS_MAX. These tests pin the cap to the shared
// fixture and prove the per-byte bound against the real form builder with adversarial values. Every value is
// an obvious dummy, derived from a counter or a hash.

const fx = JSON.parse(fs.readFileSync(new URL("../../fixtures/run-env/form-budget.json", import.meta.url), "utf8")) as {
  form_chars_max: number;
  form_chars_per_secret_byte: number;
};
const PER_BYTE = fx.form_chars_per_secret_byte;
const MAX_VALUE_BYTES = 65536;
const MAX_CLAIM_SECRET_BYTES = Math.floor(fx.form_chars_max / PER_BYTE);

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const secretVar = (name: string, value: string): ClaimRunEnvVar =>
  ({ var_id: UUID, scope: "repo", rev: 1, name, value, secret: true, description: "" });
const bytes = (s: string): number => Buffer.byteLength(s, "utf8");

/** A string of exactly `n` UTF-8 bytes built by cycling `unit`s (each unit whole), then padded with `pad`. */
function fill(units: readonly string[], n: number, pad = "a"): string {
  let out = "";
  let size = 0;
  for (let i = 0; ; i++) {
    const u = units[i % units.length]!;
    const b = bytes(u);
    if (size + b > n) break;
    out += u;
    size += b;
  }
  return out + pad.repeat(n - size);
}

const range = (from: number, to: number, skip: readonly number[] = []): string[] =>
  Array.from({ length: to - from + 1 }, (_, i) => from + i).filter((c) => !skip.includes(c)).map((c) => String.fromCharCode(c));

const hashed = (seed: string, n: number): string => {
  let text = "";
  for (let i = 0; text.length < n; i++) text += crypto.createHash("sha512").update(`${seed}-${i}`).digest("base64").replace(/=+$/, "");
  return text.slice(0, n);
};

type Gen = (n: number) => string;
const GENERATORS: Record<string, Gen> = {
  // One-byte controls: a six-unit \u00XX escape in JSON and the HTML-safe forms, three units percent-encoded.
  "C0 controls without breaks": (n) => fill(range(1, 31, [9, 10, 13]), n),
  "all C0 controls": (n) => fill(range(1, 31), n),
  "four-byte characters": (n) => fill(["\u{1F600}"], n),
  "three-byte characters": (n) => fill([" ", "€"], n),
  "CRLF lines with trailing breaks": (n) => fill(["abcdefghij\r\n"], n - 4, "z") + "\r\n\r\n\r\n\r\n".slice(0, 4),
  "LF lines": (n) => fill(["abcdefghij\n"], n),
  "one 8-character line then breaks (one line form, many joins)": (n) => "abcdefgh" + fill(["\n"], n - 8),
  // Lines the old token rule never registered: every line of 8+ characters now is a line form.
  "control-padded lowercase-only lines": (n) => fill(["abcdefgh\u0001\u0002\u0003\u0004\u0005\u0006\u0007\u000b\n"], n),
  "control-padded lowercase-only lines, CRLF with trailing breaks": (n) =>
    fill(["abcdefgh" + range(1, 8).join("") + "\r\n"], n - 4, "\u0001") + "\r\n\r\n",
  "lowercase-only 8-character lines": (n) => fill(["abcdefgh\n"], n),
  "digit-only 8-character lines": (n) => fill(["11223344\n"], n),
  "punctuation-only 8-character lines": (n) => fill(["========\r\n"], n),
  "alternating text and break": (n) => fill(["a\n"], n),
  "lone CR and LF": (n) => fill(["a\r", "b\n"], n),
  "spaces and html-significant characters": (n) => fill([" ", "<", ">", "&", "/", '"', "\\"], n),
  "high-variety alphanumerics": (n) => hashed("variety", n),
  "controls with a space": (n) => " " + fill(range(1, 31, [9, 10, 13]), n - 1),
  // Lines that each register as a line form, padded with controls that JSON-escape to
  // six units per byte, and ending in breaks so all four value variants exist.
  "control-padded token lines with trailing breaks": (n) =>
    fill(["aB3xY7kQ" + range(1, 8).join("") + "\r\n"], n - 4, "\u0001") + "\r\n\r\n",
  "control-padded token lines, LF": (n) => fill(["aB3xY7kQ\u0001\u0002\u0003\u0004\u0005\u0006\u0007\u000b\n"], n),
};

describe("the form budget shared with the api", () => {
  it("RUN_ENV_FORM_CHARS_MAX is the fixture's form_chars_max", () => {
    assert.equal(RUN_ENV_FORM_CHARS_MAX, fx.form_chars_max);
  });

  it("the fixture's per-byte bound is a positive integer and the cap fits the store's limits", () => {
    assert.ok(Number.isInteger(PER_BYTE) && PER_BYTE > 0);
    assert.ok(MAX_CLAIM_SECRET_BYTES > MAX_VALUE_BYTES, "a claim holds more than one maximal value");
    assert.ok(PER_BYTE * MAX_CLAIM_SECRET_BYTES <= fx.form_chars_max);
  });

  for (const [name, gen] of Object.entries(GENERATORS)) {
    for (const n of [8, 100, 4096, MAX_VALUE_BYTES]) {
      it(`${name}: a ${n}-byte value's forms stay within the fixture's per-byte bound`, () => {
        const value = gen(n);
        assert.equal(bytes(value), n, "the generator produces the requested size");
        const claim = runEnvClaimForms([secretVar("ADVERSARIAL", value)], undefined);
        assert.equal(claim.overBudget, false);
        assert.ok(claim.formChars <= PER_BYTE * n, `${claim.formChars} units for ${n} bytes (bound ${PER_BYTE * n})`);
      });
    }
  }

  // Drift guard for the derivation behind the per-byte bound (see form_budget.go): sampling values cannot
  // notice a form type the worker gains later, so these pin the form COUNTS and per-family lengths the
  // derivation multiplies. A new form raises a count or a length bound here and fails before the bound is wrong.
  describe("the form counts the per-byte derivation assumes", () => {
    const distinctForms = (c: ReturnType<typeof runEnvClaimForms>): Set<string> =>
      new Set([...c.redaction, ...c.needles.flatMap((n) => n.forms)]);
    // Every provider form differs from the others: "/" splits json from json-with-\/, "<" splits json from the
    // HTML-safe forms, hex digits a-f split their two cases, a control character splits raw from json, a space adds
    // the "+" form.
    // The \u00ff and \u00fb pairs put the base64 characters "+" and "/" into every alignment, which ASCII text
    // never does, so each URL-safe form differs from its standard one.
    const WIDE = "\u00ff\u00fb".repeat(8);
    const everyFormDistinct = (tail: string): string => `a/< \u0001${WIDE}${tail}`;

    it("providerKeyForms yields at most 8 forms: raw, 4 JSON-family, 3 percent-family", () => {
      assert.equal(providerKeyForms(everyFormDistinct("bcdefgh")).length, 8);
      for (const [name, gen] of Object.entries(GENERATORS)) {
        assert.ok(providerKeyForms(gen(4096)).length <= 8, name);
      }
    });

    it("providerKeyForms forms stay within the per-family byte bounds", () => {
      const n = 4096;
      const inputs = [...Object.values(GENERATORS).map((gen) => gen(n)), everyFormDistinct("x".repeat(n - 5))];
      for (const value of inputs) {
        const b = bytes(value);
        const lens = providerKeyForms(value).map((f) => f.length);
        assert.ok(Math.max(...lens) <= 6 * b, "a form passes 6 units per byte");
        assert.ok(lens.filter((l) => l > 3 * b).length <= 4, "more than 4 forms pass 3 units per byte (JSON family)");
        assert.ok(lens.filter((l) => l > b).length <= 7, "more than 7 forms pass 1 unit per byte (JSON and percent families)");
        assert.ok(lens.reduce((a, c) => a + c, 0) <= (1 + 4 * 6 + 3 * 3) * b, "the provider forms total more than 34 units per byte");
      }
    });

    it("a single-line value has at most 14 distinct whole-value forms: 8 provider and 6 base64", () => {
      let widest = 0;
      for (let i = 0; i < 200; i++) {
        const value = everyFormDistinct(hashed(`drift-${i}`, 100 + i));
        const claim = runEnvClaimForms([secretVar("ONE", value)], undefined);
        assert.equal(claim.lineForms.length, 0);
        assert.equal(distinctForms(claim).size, claim.redaction.length, "a single-line value has no needle-only form");
        assert.ok(claim.redaction.length <= 14, `${claim.redaction.length} distinct forms for sample ${i}`);
        widest = Math.max(widest, claim.redaction.length);
      }
      assert.equal(widest, 14, "the construction reaches the analytic maximum, so the guard is live");
    });

    it("a multi-line value has at most 4 variants, 2 line forms per line and 6 diff-shaped forms", () => {
      const lineCount = 3;
      const lines = Array.from({ length: lineCount }, (_, i) => `${WIDE}${hashed(`line-${i}`, 40)} <"${hashed(`tail-${i}`, 20)}`);
      const value = `${lines.join("\r\n")}\r\n`;
      const claim = runEnvClaimForms([secretVar("MULTI", value)], undefined);
      const lineFormCount = claim.lineForms.flatMap((n) => n.forms).length;
      assert.equal(lineFormCount, 2 * lineCount, "raw and JSON-escaped form per line");
      const redaction = new Set(claim.redaction);
      const diffOnly = [...distinctForms(claim)].filter((f) => !redaction.has(f));
      assert.equal(diffOnly.length, 6, "LF and CRLF joins for each of +, -, and space");
      const wholeValue = redaction.size - lineFormCount;
      assert.equal(wholeValue, 4 * 14, "4 value variants of 14 whole-value forms each");
      assert.equal(distinctForms(claim).size, 4 * 14 + 2 * lineCount + 6);
    });

    // The logger indexes each registered secret and, when it differs, its JSON-escaped copy, and formChars counts that
    // copy too. A control byte is 1 byte and 1 unit: its JSON-family forms are 6 units and re-escape to 7 (the
    // backslash doubles), its raw copy is 6; percent and base64 forms hold no escapable character.
    const escapedCopy = (f: string): string => JSON.stringify(f).slice(1, -1);

    it("escaped copies of a single-line value's forms add at most 6 + 4 x 7 units per byte, only for raw and JSON-family forms", () => {
      const n = 4096;
      const inputs = [...Object.values(GENERATORS).map((gen) => gen(n)), everyFormDistinct("x".repeat(n - 5)), "\u0001".repeat(n)];
      for (const value of inputs) {
        if (/[\r\n]/.test(value)) continue;
        const b = bytes(value);
        const claim = runEnvClaimForms([secretVar("ONE", value)], undefined);
        const provider = new Set(providerKeyForms(value));
        let extra = 0;
        let differing = 0;
        for (const f of claim.redaction) {
          const copy = escapedCopy(f);
          if (copy === f) continue;
          assert.ok(f === value || provider.has(f), "only the raw value and a provider form re-escape");
          assert.ok(copy.length <= (f === value ? 6 : 7) * b, `a ${copy.length}-unit copy of a ${f.length}-unit form`);
          extra += copy.length;
          differing++;
        }
        assert.ok(differing <= 5, `${differing} forms re-escape`);
        assert.ok(extra <= (6 + 4 * 7) * b, `${extra} units of copies for ${b} bytes`);
      }
    });

    it("escaped copies of a multi-line value's line forms add at most 6 + 7 units per byte", () => {
      for (const name of ["control-padded token lines with trailing breaks", "control-padded token lines, LF", "control-padded lowercase-only lines"]) {
        const value = GENERATORS[name]!(4096);
        const claim = runEnvClaimForms([secretVar("MULTI", value)], undefined);
        let extra = 0;
        for (const f of claim.lineForms.flatMap((l) => l.forms)) {
          const copy = escapedCopy(f);
          if (copy !== f) extra += copy.length;
        }
        assert.ok(extra > 0, `${name} registers line forms that re-escape`);
        assert.ok(extra <= (6 + 7) * bytes(value), `${extra} units of line-form copies`);
      }
    });

    it("counts a form's escaped copy once, and only when it differs", () => {
      // 4096 x U+0001: raw 4096, JSON-family (all four forms the same string) 6 x 4096, percent 3 x 4096 (both cases the
      // same string), base64 5461 + 5460 + 5461 (the URL-safe alignments equal the standard ones). The raw value's copy
      // is that JSON string, already counted; the JSON string's copy is 7 x 4096 new units.
      const n = 4096;
      const claim = runEnvClaimForms([secretVar("CTRL", "\u0001".repeat(n))], undefined);
      assert.equal(claim.formChars, n + 6 * n + 3 * n + (5461 + 5460 + 5461) + 7 * n);
    });

    it("counts no escaped copy for a value that JSON leaves unchanged", () => {
      // 4096 hex digits: raw 4096, the four JSON forms and the two percent forms equal it, base64 3 alignments.
      const n = 4096;
      const value = "ab12".repeat(n / 4);
      const claim = runEnvClaimForms([secretVar("PLAIN", value)], undefined);
      assert.equal(claim.redaction.filter((f) => f.length === n).length, 1);
      assert.equal(claim.formChars, claim.redaction.reduce((a, f) => a + f.length, 0));
    });

    it("every diff-shaped form stays within 3 units per byte", () => {
      for (const n of [100, 4096]) {
        for (const gen of [GENERATORS["alternating text and break"]!, GENERATORS["CRLF lines with trailing breaks"]!, GENERATORS["LF lines"]!, GENERATORS["lone CR and LF"]!]) {
          const claim = runEnvClaimForms([secretVar("DIFF", gen(n))], undefined);
          const redaction = new Set(claim.redaction);
          const diffOnly = [...distinctForms(claim)].filter((f) => !redaction.has(f));
          assert.ok(diffOnly.length <= 6);
          for (const f of diffOnly) assert.ok(f.length <= 3 * n, `a ${f.length}-unit diff form for ${n} bytes`);
        }
      }
    });
  });

  it("a claim at the write-time budget of worst-case values is not over budget", () => {
    // Distinct values (a prefix keeps their forms apart) of the largest size and the remainder, totalling exactly the
    // cap, built from the generator that costs the most units per byte.
    let costliest = "";
    let worst = 0;
    for (const [name, gen] of Object.entries(GENERATORS)) {
      const ratio = runEnvClaimForms([secretVar("PROBE", gen(4096))], undefined).formChars / 4096;
      if (ratio > worst) [costliest, worst] = [name, ratio];
    }
    assert.ok(worst <= PER_BYTE, `${costliest} costs ${worst.toFixed(1)} units per byte (bound ${PER_BYTE})`);
    const sizes = [MAX_VALUE_BYTES, MAX_CLAIM_SECRET_BYTES - MAX_VALUE_BYTES];
    assert.equal(sizes.reduce((a, b) => a + b, 0), MAX_CLAIM_SECRET_BYTES);
    const gen = GENERATORS[costliest]!;
    const vars = sizes.map((n, i) => {
      const prefix = `p${i}`.padEnd(2, "p");
      return secretVar(`WORST_${i}`, prefix + gen(n - 2));
    });
    for (const [i, v] of vars.entries()) assert.equal(bytes(v.value), sizes[i]);
    const claim = runEnvClaimForms(vars, undefined);
    assert.equal(claim.overBudget, false, `${claim.formChars} units against ${RUN_ENV_FORM_CHARS_MAX}`);
    assert.ok(claim.formChars <= fx.form_chars_max);
  });
});
