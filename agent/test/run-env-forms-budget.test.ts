import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import crypto from "node:crypto";
import { RUN_ENV_FORM_CHARS_MAX, runEnvClaimForms } from "../src/run-env-forms.js";
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
  "short lines (no line forms, many joins)": (n) => "abcdefgh" + fill(["\n"], n - 8),
  "alternating text and break": (n) => fill(["a\n"], n),
  "lone CR and LF": (n) => fill(["a\r", "b\n"], n),
  "spaces and html-significant characters": (n) => fill([" ", "<", ">", "&", "/", '"', "\\"], n),
  "high-variety alphanumerics": (n) => hashed("variety", n),
  "controls with a space": (n) => " " + fill(range(1, 31, [9, 10, 13]), n - 1),
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
      it(`${name}: a ${n}-byte value's forms stay within ${"form_chars_per_secret_byte"} per byte`, () => {
        const value = gen(n);
        assert.equal(bytes(value), n, "the generator produces the requested size");
        const claim = runEnvClaimForms([secretVar("ADVERSARIAL", value)], undefined);
        assert.equal(claim.overBudget, false);
        assert.ok(claim.formChars <= PER_BYTE * n, `${claim.formChars} units for ${n} bytes (bound ${PER_BYTE * n})`);
      });
    }
  }

  it("a claim at the write-time budget of worst-case values is not over budget", () => {
    // Four distinct values (a prefix keeps their forms apart) totalling exactly the cap, built from the
    // generator that costs the most units per byte.
    let costliest = "";
    let worst = 0;
    for (const [name, gen] of Object.entries(GENERATORS)) {
      const ratio = runEnvClaimForms([secretVar("PROBE", gen(4096))], undefined).formChars / 4096;
      if (ratio > worst) [costliest, worst] = [name, ratio];
    }
    console.log(`costliest generator: ${costliest} at ${worst.toFixed(1)} units per byte (bound ${PER_BYTE})`);
    const sizes = [MAX_VALUE_BYTES, MAX_VALUE_BYTES, MAX_VALUE_BYTES, MAX_CLAIM_SECRET_BYTES - 3 * MAX_VALUE_BYTES];
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
