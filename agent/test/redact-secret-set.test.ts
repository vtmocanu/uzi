import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { PER_SECRET_MAX, SecretSet, makeTextRedactor } from "../src/redact.js";
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

// Mirrors api/internal/termsafe SanitizeTTY: drop Cc (except \t, \n) and Cf.
const displayAsTTY = (s: string) => s.replace(/[\p{Cc}\p{Cf}]/gu, (c) => (c === "\t" || c === "\n" ? c : ""));
// True when any window of `min` characters of the secret is still readable in the displayed output.
const leaksFragment = (out: string, secret: string, min = 8): boolean => {
  const shown = displayAsTTY(out);
  for (let i = 0; i + min <= secret.length; i++) if (shown.includes(secret.slice(i, i + min))) return true;
  return false;
};

// The live run redactor gathers every span (verbatim and across invisible characters) in the original
// string and replaces the union once (#4688). Each case below left half the secret visible when the
// verbatim span was replaced first and the invisible pass ran on the redacted output.
describe("SecretSet redacts overlapping occurrences as one union", () => {
  const block = ["run", "owned", "Secret", "9f3k2"].join("-");
  const secret = block + block;
  const zwsp = "​";
  const fillers = Array.from({ length: PER_SECRET_MAX + 1 }, (_, i) => `filler-secret-${String(i).padStart(3, "0")}-dummy`);
  const cases: Array<[string, string, string]> = [
    ["a verbatim occurrence overlapping an invisible-separated one", `before ${secret}${zwsp}${block} after`, `before ${REDACTED} after`],
    ["self-overlapping invisible-separated occurrences", `before ${block}${zwsp}${block}${zwsp}${block} after`, `before ${REDACTED} after`],
  ];
  const variants: Array<[string, string[]]> = [
    ["per-secret path", [secret]],
    ["indexed path (more than PER_SECRET_MAX secrets)", [...fillers, secret]],
  ];

  for (const [path, secrets] of variants) {
    for (const [name, input, expected] of cases) {
      it(`${path}, text: ${name}`, () => {
        const out = new SecretSet(secrets).text(input);
        assert.equal(out, expected);
        assert.ok(!leaksFragment(out, secret), JSON.stringify(out));
      });
      it(`${path}, payload: ${name}`, () => {
        const out = new SecretSet(secrets).payload({ k: [input] }) as { k: string[] };
        assert.deepEqual(out, { k: [expected] });
        assert.ok(!leaksFragment(out.k[0]!, secret), JSON.stringify(out));
      });
    }

    it(`${path}: a self-overlapping verbatim run is redacted whole`, () => {
      const set = new SecretSet(path.startsWith("per") ? ["abababab"] : [...fillers, "abababab"]);
      const out = set.text("xx ababababab yy");
      assert.equal(out, `xx ${REDACTED} yy`);
      assert.ok(!leaksFragment(out, "abababab"), JSON.stringify(out));
      assert.deepEqual(set.payload({ k: "ababababab" }), { k: REDACTED });
    });
  }

  it("occurrences that only touch stay separate markers", () => {
    assert.equal(new SecretSet(["abcdefgh"]).text("abcdefghabcdefgh"), REDACTED + REDACTED);
  });
});

describe("makeTextRedactor over more than PER_SECRET_MAX secrets redacts overlapping occurrences as one union", () => {
  const block = ["run", "owned", "Secret", "9f3k2"].join("-");
  const secret = block + block;
  const zwsp = "​";
  const fillers = Array.from({ length: PER_SECRET_MAX + 1 }, (_, i) => `filler-secret-${String(i).padStart(3, "0")}-dummy`);
  const scrub = makeTextRedactor([...fillers, secret]);

  it("a verbatim occurrence overlapping an invisible-separated one leaves no fragment", () => {
    const out = scrub(`before ${secret}${zwsp}${block} after`);
    assert.equal(out, `before ${REDACTED} after`);
    assert.ok(!leaksFragment(out, secret), JSON.stringify(out));
  });

  it("self-overlapping invisible-separated occurrences leave no fragment", () => {
    const out = scrub(`before ${block}${zwsp}${block}${zwsp}${block} after`);
    assert.equal(out, `before ${REDACTED} after`);
    assert.ok(!leaksFragment(out, secret), JSON.stringify(out));
  });

  it("a self-overlapping verbatim run is redacted whole", () => {
    const out = makeTextRedactor([...fillers, "abababab"])("xx ababababab yy");
    assert.equal(out, `xx ${REDACTED} yy`);
  });
});

describe("SecretSet over a run's secret variable forms", () => {
  const ENTROPY = Array.from({ length: 80 }, (_, i) => createHash("sha256").update(`entropy-${i}`).digest("base64"));
  const STRUCTURAL_LINES = ["apiVersion: v1", "kind: Config", "current-context: example", "    server: https://kubernetes.example.com:6443", "clusters:", "- name: example"];
  const PADDING = " ".repeat(8);
  const value = [...STRUCTURAL_LINES, PADDING, ...ENTROPY, "}"].join("\n");
  const claim = runEnvClaimForms([
    { var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "SVC_JSON", value, secret: true, description: "" },
  ], []);

  it("the claim registers more forms than the per-secret path serves", () => {
    assert.ok(claim.redaction.length > PER_SECRET_MAX, `${claim.redaction.length} forms`);
  });

  it("masks a Unicode-space padding line (NBSP, U+3000) of a multi-line secret in text and payloads", () => {
    for (const ch of [" ", "　"]) {
      const pad = ch.repeat(8);
      const c = runEnvClaimForms([
        { var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "SVC_PAD", value: [...STRUCTURAL_LINES, pad, ...ENTROPY.slice(0, 5)].join("\n"), secret: true, description: "" },
      ], []);
      const set = new SecretSet(c.redaction);
      const out = set.text(`a\n${pad}\nb`);
      assert.ok(!out.includes(pad), "the padding line does not survive");
      assert.equal(out, `a\n${REDACTED}\nb`);
      assert.deepEqual(set.payload({ k: pad }), { k: REDACTED });
    }
  });

  it("leaves ASCII space/tab-only padding unmasked and masks every other line of 8+ characters, in text and payloads", () => {
    const set = new SecretSet(claim.redaction);
    assert.equal(set.text(`a\n${PADDING}\nb`), `a\n${PADDING}\nb`);
    assert.deepEqual(set.payload({ k: PADDING }), { k: PADDING });
    for (const line of STRUCTURAL_LINES) {
      assert.equal(set.text(`a\n${line}\nb`), `a\n${REDACTED}\nb`);
      assert.deepEqual(set.payload({ k: line }), { k: REDACTED });
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
