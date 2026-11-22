import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import crypto from "node:crypto";
import { createLogger } from "../src/log.js";
import { makeRedactor, makeTextRedactor, providerKeyForms } from "../src/redact.js";
import { PEM_STANDARD_LABELS, RUN_ENV_FORM_CHARS_MAX, multiLineSecretVariables, runEnvClaimForms } from "../src/run-env-forms.js";
import { textHitVariables } from "../src/run-env-scan.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";
import { fakeGitlab, git, installHarness, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

installHarness();

// PRD 4416 M2: the registered forms of a secret variable, the diff-shaped scan needles of a multi-line value
// and the cost of redacting with the largest claim the api can send. Every value is an obvious dummy; armor
// lines are joined at runtime.

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const secretVar = (name: string, value: string): ClaimRunEnvVar =>
  ({ var_id: UUID, scope: "repo", rev: 1, name, value, secret: true, description: "" });
const retired = (name: string, value: string): ClaimRunEnvRedact => ({ var_id: UUID, scope: "repo", rev: 1, name, value });

const marker = (kind: "BEGIN" | "END", label: string): string => ["-----", kind, " ", label, "-----"].join("");
const BEGIN = marker("BEGIN", "PRIVATE KEY");
const END = marker("END", "PRIVATE KEY");
const bodyLines = (seed: string): string[] =>
  Array.from({ length: 5 }, (_, i) => crypto.createHash("sha256").update(`${seed}-${i}`).digest("base64").replace(/=+$/, "").padEnd(44, "x"));
const pem = (seed: string): string => [BEGIN, ...bodyLines(seed), END].join("\n");
const PEM = pem("one");
const OTHER_PEM = pem("two");

const asPatch = (lines: string[], eol = "\n"): string =>
  ["diff --git a/k.pem b/k.pem", "new file mode 100644", "--- /dev/null", "+++ b/k.pem", `@@ -0,0 +1,${lines.length} @@`,
    ...lines.map((l) => `+${l}`)].join(eol) + eol;

describe("whole-value encodings are redacted and found", () => {
  const VALUE = "p<a>&s s/w+o=rd-9Z";
  const encodings: Record<string, string> = {
    raw: VALUE,
    json: JSON.stringify(VALUE).slice(1, -1),
    "json with escaped slash": JSON.stringify(VALUE).slice(1, -1).replaceAll("/", "\\/"),
    "html-safe lower hex": JSON.stringify(VALUE).slice(1, -1).replace(/[<>&]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`),
    "html-safe upper hex": JSON.stringify(VALUE).slice(1, -1).replace(/[<>&]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0").toUpperCase()}`),
    "percent upper": encodeURIComponent(VALUE),
    "percent lower": encodeURIComponent(VALUE).replace(/%[0-9A-F]{2}/g, (m) => m.toLowerCase()),
    "percent with plus for space": encodeURIComponent(VALUE).replaceAll("%20", "+"),
  };
  const redact = makeTextRedactor(runEnvClaimForms([secretVar("SVC_PASSWORD", VALUE)], []).redaction);
  const needles = runEnvClaimForms([secretVar("SVC_PASSWORD", VALUE)], []).needles;

  for (const [label, form] of Object.entries(encodings)) {
    it(label, () => {
      const text = `before ${form} after`;
      assert.ok(!redact(text).includes(form), "redacted");
      assert.deepEqual(textHitVariables(text, needles), ["SVC_PASSWORD"], "found");
    });
  }

  it("the three base64url alignments of a value whose base64 uses + and /", () => {
    const value = "pw-???>>>-~~~~???>>>-zz";
    const redact = makeTextRedactor(runEnvClaimForms([secretVar("SVC_PASSWORD", value)], []).redaction);
    for (let n = 0; n < 3; n++) {
      const embedded = Buffer.from(`${"x".repeat(n)}${value}trailing`);
      assert.ok(/[+/]/.test(embedded.toString("base64")) && /[-_]/.test(embedded.toString("base64url")), "the fixture exercises both alphabets");
      const text = `t=${embedded.toString("base64url")};`;
      assert.ok(runEnvClaimForms([secretVar("SVC_PASSWORD", value)], []).redaction.some((f) => text.includes(f)), `alignment ${n}`);
      assert.ok(redact(text).includes("***REDACTED***"), `redacted at alignment ${n}`);
    }
  });
});

describe("a variant of the value shorter than the redaction floor registers no form", () => {
  it("an 8-character value whose line break-stripped variant has 7 characters derives forms from the whole value only", () => {
    const short = "a b&c=d";
    const forms = runEnvClaimForms([secretVar("SHORT_VALUE", `${short}\n`)], []).redaction;
    const seven = [...providerKeyForms(short), Buffer.from(short).toString("base64").slice(0, 9)].filter((f) => f.length >= 8);
    assert.ok(seven.length > 0, "the fixture has encoded forms of the 7-character variant that reach the floor");
    for (const f of seven) assert.ok(!forms.includes(f), `a form of the 7-character variant is registered: ${f}`);
    assert.ok(forms.includes(`${short}\n`), "the whole value is registered");
    assert.ok(forms.includes(encodeURIComponent(`${short}\n`)), "its own percent form is registered");
    const scan = runEnvClaimForms([secretVar("SHORT_VALUE", `${short}\n`)], []).needles;
    for (const f of seven) assert.ok(!scan[0]!.forms.includes(f), `a form of the 7-character variant is a needle: ${f}`);
  });
});

describe("which lines of a multi-line value are registered alone", () => {
  const redactOf = (value: string) => makeTextRedactor(runEnvClaimForms([secretVar("BACKUP_CODES", value)], []).redaction);
  const FILLER = ["Qm9kdXMtZml4dHVyZS1hLQ", "c2Vjb25kLWZpeHR1cmUtYg"] as [string, string];

  for (const code of ["a1b2c3d4", "x9y8z7w6"]) {
    it(`the line ${code} is masked on its own`, () => {
      const redact = redactOf([FILLER[0], code, FILLER[1]].join("\n"));
      assert.ok(!redact(`log: ${code} end`).includes(code));
    });
  }

  for (const code of ["84629137", "5083619274"]) {
    it(`the digits-only line ${code} is masked on its own`, () => {
      const redact = redactOf([FILLER[0], code, FILLER[1]].join("\n"));
      assert.ok(!redact(`log: ${code} end`).includes(code));
    });
  }

  for (const [label, line] of [["a digits-only line of repeated digits", "11223344"], ["a mixed line with one digit", "abcdefg1"], ["a punctuation rule", "========"], ["one repeated character", "AAAAAAAA"], ["a lowercase-only line", "abcdefgh"]] as const) {
    it(`${label} is registered alone`, () => {
      const redact = redactOf([FILLER[0], line, FILLER[1]].join("\n"));
      assert.ok(!redact(`text ${line} end`).includes(line));
      assert.ok(!redact(`text ${FILLER[0]} end`).includes(FILLER[0]), "its other lines are too");
    });
  }

  for (const [label, line] of [["spaces", " ".repeat(8)], ["tabs", "\t".repeat(8)], ["spaces and tabs", "\t \t \t \t "]] as const) {
    it(`a line of only ${label} is not registered alone`, () => {
      const redact = redactOf([FILLER[0], line, FILLER[1]].join("\n"));
      assert.equal(redact(`text ${line} end`), `text ${line} end`);
      assert.ok(!redact(`text ${FILLER[0]} end`).includes(FILLER[0]), "its other lines still are");
    });
  }

  const unicodeSpaces: [string, string][] = [["U+00A0", " "], ["U+3000", "　"],
    ...Array.from({ length: 11 }, (_, i): [string, string] => [`U+${(0x2000 + i).toString(16).toUpperCase()}`, String.fromCharCode(0x2000 + i)])];
  for (const [label, ch] of unicodeSpaces) {
    it(`a line of 8 ${label} is a normal line form and is masked alone`, () => {
      const line = ch.repeat(8);
      const claim = runEnvClaimForms([secretVar("BACKUP_CODES", [FILLER[0], line, FILLER[1]].join("\n"))], []);
      assert.ok(claim.lineForms[0]!.forms.includes(line), "a line form");
      assert.ok(!makeTextRedactor(claim.redaction)(`text ${line} end`).includes(line), "masked");
    });
  }
});

describe("a multi-line secret in a unified diff", () => {
  const needles = runEnvClaimForms([secretVar("SIGNING_KEY", PEM)], []).needles;
  const redact = makeTextRedactor(runEnvClaimForms([secretVar("SIGNING_KEY", PEM)], []).redaction);
  const lines = PEM.split("\n");

  it("is found when added as lines", () => {
    assert.deepEqual(textHitVariables(asPatch(lines), needles), ["SIGNING_KEY"]);
  });

  it("is found when removed or kept as context lines", () => {
    const prefixed = (p: string): string => `@@ -1,${lines.length} +1 @@\n${lines.map((l) => `${p}${l}`).join("\n")}\n`;
    assert.deepEqual(textHitVariables(prefixed("-"), needles), ["SIGNING_KEY"]);
    assert.deepEqual(textHitVariables(prefixed(" "), needles), ["SIGNING_KEY"]);
  });

  it("is found when its last line is the last added line before the no-newline marker", () => {
    const patch = `${asPatch(lines).slice(0, -1)}\n\\ No newline at end of file\n`;
    assert.ok(patch.includes(`+${END}\n\\ No newline at end of file`));
    assert.deepEqual(textHitVariables(patch, needles), ["SIGNING_KEY"]);
  });

  it("is found in a CRLF patch", () => {
    assert.deepEqual(textHitVariables(asPatch(lines, "\r\n"), needles), ["SIGNING_KEY"]);
  });

  it("is found for a value that ends in a line break", () => {
    const withEol = runEnvClaimForms([secretVar("SIGNING_KEY", `${PEM}\n`)], []).needles;
    assert.deepEqual(textHitVariables(asPatch(lines), withEol), ["SIGNING_KEY"]);
  });

  it("a different key in the patch is not a hit", () => {
    assert.deepEqual(textHitVariables(asPatch(OTHER_PEM.split("\n")), needles), []);
  });

  it("a constant line of a JSON credential is masked alone but is never a scan needle, while the whole credential is a hit", () => {
    const credential = ["{", '  "type": "service_account",', '  "project_id": "dummy-project-0001",', '  "private_key_id": "0123456789abcdef"', "}"].join("\n");
    const claim = runEnvClaimForms([secretVar("GCP_CREDENTIALS", credential)], []);
    const sa = claim.needles;
    const constant = '  "type": "service_account",';
    const publicLine = asPatch([constant, '  "client_email": "a@b.example"']);
    assert.deepEqual(textHitVariables(publicLine, sa), []);
    assert.ok(claim.lineForms[0]!.forms.includes(constant), "the constant line is a line form");
    assert.ok(!makeTextRedactor(claim.redaction)(`x ${constant} y`).includes(constant), "and is masked");
    assert.deepEqual(textHitVariables(asPatch(credential.split("\n")), sa), ["GCP_CREDENTIALS"]);
  });

  it("registers each line for redaction but never the PEM armor, and never a line as a needle", () => {
    const forms = runEnvClaimForms([secretVar("SIGNING_KEY", PEM)], []).redaction;
    for (const line of bodyLines("one")) assert.ok(forms.includes(line), "a body line is registered");
    assert.ok(!forms.includes(BEGIN) && !forms.includes(END), "armor is not registered");
    for (const needle of needles) for (const line of bodyLines("one")) assert.ok(!needle.forms.includes(line), "no per-line needle");
  });

  it("masks every body line wherever it appears alone, and the armor stays", () => {
    const out = redact(`${BEGIN}\n${bodyLines("one").map((l) => `> ${l}`).join("\n")}\n${END}\nand one line: ${bodyLines("one")[2]!}`);
    for (const line of bodyLines("one")) assert.ok(!out.includes(line), "body line masked");
    assert.ok(out.includes(BEGIN) && out.includes(END));
  });

  it("the redactor masks each line of a multi-line secret inside unified-diff text", () => {
    const out = redact(asPatch(lines));
    for (const line of bodyLines("one")) assert.ok(!out.includes(line));
  });

  it("masks the secret in a feed payload (a tool result)", () => {
    const payloadRedact = makeRedactor(runEnvClaimForms([secretVar("SIGNING_KEY", PEM)], []).redaction);
    const out = payloadRedact({ kind: "tool_result", content: [{ type: "text", text: `cat k.pem\n${PEM}` }], note: JSON.stringify(PEM) });
    const text = JSON.stringify(out);
    for (const line of bodyLines("one")) assert.ok(!text.includes(line));
  });

  it("masks the secret in the planning diff", async () => {
    const run = runnerWith(() => ({ executor: { run: async () => ({ branch: "b" }) } }), fakeGitlab().gitlab) as unknown as {
      captureCheckedPlanningDiff(clone: string, base: string, bare: string | undefined, signal: AbortSignal,
        log: ReturnType<typeof nullLogger>, redact?: (s: string) => string): Promise<{ diff: string }>;
    };
    const originalCapture = git.capturePlanningDiff;
    const originalScan = git.scanPatchForSecrets;
    git.capturePlanningDiff = async () => Buffer.from(asPatch(lines));
    git.scanPatchForSecrets = async () => ({ trusted: true, findings: [] });
    try {
      const got = await run.captureCheckedPlanningDiff("/nonexistent/clone", "a".repeat(40), "/nonexistent/bare", new AbortController().signal, nullLogger(), redact);
      for (const line of bodyLines("one")) assert.ok(!got.diff.includes(line));
    } finally {
      git.capturePlanningDiff = originalCapture;
      git.scanPatchForSecrets = originalScan;
    }
  });
});

describe("the largest claim redacts in bounded time", () => {
  const limits = JSON.parse(fs.readFileSync(new URL("../../fixtures/run-env/claim-limits.json", import.meta.url), "utf8")) as {
    max_run_env_entries: number; max_run_env_total_bytes: number; max_redact_entries: number; max_redact_total_bytes: number;
  };
  const value = (seed: string, bytes: number, lineLen: number, eol = "\n", trailing = false): string => {
    let text = "";
    for (let i = 0; text.length < bytes; i++) text += crypto.createHash("sha512").update(`${seed}-${i}`).digest("base64").replace(/=+$/, "");
    text = text.slice(0, bytes);
    const body = lineLen === 0 ? text : text.match(new RegExp(`.{1,${lineLen}}`, "g"))!.join(eol);
    return trailing ? body + eol : body;
  };
  const firstLine = (v: string): string => v.split(/\r?\n/)[0]!;
  // The most forms a claim can register, per entry and per line.
  // Per entry: a value has at most 4 variants (as given, trailing breaks stripped, breaks normalized to LF, and
  // that stripped), and each variant has at most 8 key forms (raw, JSON, JSON with escaped slashes, HTML-safe in
  // two hex cases, percent in two hex cases, percent with plus for a space) and 6 base64 forms (3 alignments, in
  // two alphabets): 4 * (8 + 6) = 56. The scan needles add the diff-shaped forms of a multi-line value: 3 prefixes
  // with 2 line breaks = 6, so 62 per entry.
  // Per line: a line is registered raw and JSON-escaped (2 forms), and the shortest registrable line is 8
  // characters and a line break (9 bytes), so a value of B bytes has at most B / 9 lines.
  const ENTRIES = limits.max_run_env_entries + limits.max_redact_entries;
  const TOTAL_BYTES = limits.max_run_env_total_bytes + limits.max_redact_total_bytes;
  const MAX_FORMS = 2 * Math.ceil(TOTAL_BYTES / 9) + 56 * ENTRIES;
  const MAX_NEEDLE_FORMS = 62 * ENTRIES;

  const cases: Array<[string, number, string, boolean]> = [
    ["single-line", 0, "\n", false],
    ["64-column", 64, "\n", false],
    ["9-column", 9, "\n", false],
    ["64-column CRLF with a trailing CRLF", 64, "\r\n", true],
    ["9-column CRLF with a trailing CRLF", 9, "\r\n", true],
  ];
  for (const [label, lineLen, eol, trailing] of cases) {
    it(`${label} values: forms stay under the bound and a 1 MiB text redacts in under 5 s`, () => {
      let live = Array.from({ length: limits.max_run_env_entries }, (_, i) =>
        secretVar(`LIVE_${i}`, value(`live-${i}`, Math.floor(limits.max_run_env_total_bytes / limits.max_run_env_entries), i % 5 === 0 ? 0 : lineLen, eol, trailing)));
      let old = Array.from({ length: limits.max_redact_entries }, (_, i) =>
        retired(`OLD_${i}`, value(`old-${i}`, Math.floor(limits.max_redact_total_bytes / limits.max_redact_entries), i % 5 === 0 ? 0 : lineLen, eol, trailing)));
      let claim = runEnvClaimForms(live, old);
      if (lineLen !== 0) {
        // Multi-line values at the full store bounds pass the form budget: the claim fails closed, and the
        // timing below runs on a claim a sixth of that size.
        assert.ok(claim.overBudget, "the largest multi-line claim is over budget");
        live = live.slice(0, 8);
        old = old.slice(0, 50);
        claim = runEnvClaimForms(live, old);
      }
      assert.ok(!claim.overBudget, "the claim measured is within budget");
      const forms = claim.redaction;
      assert.ok(forms.length < MAX_FORMS, `${forms.length} forms, bound ${MAX_FORMS}`);
      const needleForms = claim.needles.reduce((n, x) => n + x.forms.length, 0);
      assert.ok(needleForms < MAX_NEEDLE_FORMS, `${needleForms} needle forms, bound ${MAX_NEEDLE_FORMS}`);

      const planted = firstLine(old[7]!.value);
      const text = `${"ordinary output line with no secret 0123456789\n".repeat(Math.ceil((1024 * 1024) / 46))}${planted}\n`;
      const started = performance.now();
      const out = makeTextRedactor(forms)(text);
      const elapsed = performance.now() - started;
      assert.ok(elapsed < 5000, `redacting took ${elapsed.toFixed(0)} ms`);
      assert.ok(!out.includes(planted), "the planted form is masked");

      const log = createLogger("error");
      for (const f of forms) log.addSecret(f);
      const line = JSON.stringify({ msg: "x".repeat(200), planted });
      const logStarted = performance.now();
      const scrubbed = log.redactDiagnostic!(line);
      const logElapsed = performance.now() - logStarted;
      assert.ok(logElapsed < 5000, `scrubbing one log line took ${logElapsed.toFixed(0)} ms`);
      assert.ok(!scrubbed.includes(planted));
    });
  }

  it("dense PEM armor text against many PEM-valued secrets that share the armor key redacts in under 5 s", () => {
    // Every PEM value begins with the same 8-unit key ("-----BEG"), so one table bucket holds every value's
    // whole-value forms; the text repeats armor lines, so that bucket is probed at the start of every BEGIN line.
    const count = limits.max_run_env_entries;
    const pems = Array.from({ length: count }, (_, i) => secretVar(`PEM_${i}`, pem(`dense-${i}`)));
    const forms = runEnvClaimForms(pems, []).redaction;
    const armor = `${BEGIN}\n${END}\n`;
    const text = armor.repeat(Math.ceil((1024 * 1024) / armor.length));
    const started = performance.now();
    const out = makeTextRedactor(forms)(text);
    const elapsed = performance.now() - started;
    assert.ok(elapsed < 5000, `redacting dense armor took ${elapsed.toFixed(0)} ms`);
    assert.equal(out, text, "armor is public text and stays");
    const planted = `${text}${pem("dense-3")}\n`;
    const plantedOut = makeTextRedactor(forms)(planted);
    for (const line of bodyLines("dense-3")) assert.ok(!plantedOut.includes(line), "a planted body line is masked");
  });

  it("an indented YAML/JSON secret (thousands of forms sharing the 8-space key) redacts, logs and scans in under 5 s", () => {
    // Every line of 8+ characters is a form, so a deeply indented file puts its lines into one 8-space bucket.
    // The claim holds as many distinct lines as the claim's secret-byte bound (116,959 bytes) allows: the
    // shortest registrable lines, a few at deeper indents and with key shapes.
    const SECRET_BYTES_MAX = 116_959;
    const ALNUM = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";
    const offset = crypto.createHash("sha256").update("indented-offset").digest().readUInt32BE(0);
    // A permutation of the tokens of each length, keyed by a hash-derived offset: 2 characters first, then 3.
    const tokenAt = (n: number, salt: number): string => {
      const len = n < 62 * 62 ? 2 : 3;
      const space = 62 ** len;
      const idx = (((n - (len === 2 ? 0 : 62 * 62)) * 40503 + offset + salt) % space + space) % space;
      let out = "";
      for (let k = 0, v = idx; k < len; k++, v = Math.floor(v / 62)) out += ALNUM[v % 62];
      return out;
    };
    const shape = (n: number, tok: string): string => {
      switch (n % 10) {
        case 7: return `${" ".repeat(10)}${tok}`;
        case 8: return `${" ".repeat(12)}- ${tok}`;
        case 9: return `${" ".repeat(8)}k:${tok}`;
        default: return `${" ".repeat(8)}${tok}`;
      }
    };
    const SEPARATOR = "=".repeat(64);
    const lineOf = (n: number, salt: number): string => shape(n, tokenAt(n, salt));
    const lines: string[] = [SEPARATOR];
    for (let n = 0, bytes = SEPARATOR.length + 1; ; n++) {
      const line = lineOf(n, 0);
      if (bytes + line.length + 1 > SECRET_BYTES_MAX) break;
      lines.push(line);
      bytes += line.length + 1;
    }
    const ENTRY_COUNT = 4;
    const perEntry = Math.ceil(lines.length / ENTRY_COUNT);
    const entries = Array.from({ length: ENTRY_COUNT }, (_, e) =>
      secretVar(`INDENTED_${e}`, lines.slice(e * perEntry, (e + 1) * perEntry).join("\n")));
    const claim = runEnvClaimForms(entries, []);
    assert.ok(!claim.overBudget, "the claim is within the form budget");
    const eightSpaces = claim.redaction.filter((f) => f.startsWith(" ".repeat(8))).length;
    assert.ok(eightSpaces >= 9_000, `${eightSpaces} forms start with 8 spaces`);
    console.log(`indented claim: ${claim.redaction.length} forms, ${eightSpaces} starting with 8 spaces`);

    const body: string[] = [];
    for (let n = 0; n < lines.length; n++) body.push(lineOf(n, 1_000_003));
    const shaped = body.join("\n") + "\n";
    const text = shaped.repeat(Math.ceil((1024 * 1024) / shaped.length)).slice(0, 1024 * 1024 - 150_000) + `\n${"=".repeat(150_000)}\n`;
    const planted = lines[5]!;
    const full = `${text}${planted}\n`;

    const before = process.memoryUsage().heapUsed;
    const redact = makeTextRedactor(claim.redaction);
    const log = createLogger("error");
    for (const f of claim.redaction) log.addSecret(f);
    const heapGrowthMiB = (process.memoryUsage().heapUsed - before) / (1024 * 1024);
    console.log(`heap growth building the indexes: ${heapGrowthMiB.toFixed(1)} MiB (informational)`);

    let started = performance.now();
    const out = redact(full);
    const redactMs = performance.now() - started;
    assert.ok(redactMs < 5000, `makeTextRedactor took ${redactMs.toFixed(0)} ms`);
    assert.ok(out.endsWith("***REDACTED***\n"), "the planted last line is masked");

    started = performance.now();
    const scrubbed = log.redactDiagnostic!(full);
    const logMs = performance.now() - started;
    assert.ok(logMs < 5000, `redactDiagnostic took ${logMs.toFixed(0)} ms`);
    assert.ok(scrubbed.endsWith("***REDACTED***\n"), "the logger masks the planted last line");

    started = performance.now();
    const hits = textHitVariables(full, claim.lineForms);
    const scanMs = performance.now() - started;
    assert.ok(scanMs < 5000, `textHitVariables took ${scanMs.toFixed(0)} ms`);
    assert.ok(hits.includes("INDENTED_0"), "the planted line is found");
    console.log(`1 MiB: redact ${redactMs.toFixed(0)} ms, logger ${logMs.toFixed(0)} ms, scan ${scanMs.toFixed(0)} ms`);
  });

  it("the indexed redactor agrees with the per-secret redactor on a mixed text", () => {
    const secrets = Array.from({ length: 80 }, (_, i) => `secret-number-${String(i).padStart(3, "0")}-value`);
    const split = makeTextRedactor(secrets.slice(0, 5));
    const indexed = makeTextRedactor(secrets);
    const text = `a ${secrets[2]!} b ${secrets[3]!}${secrets[4]!} c ${secrets[2]!.slice(0, -3)} d`;
    assert.equal(indexed(text), split(text));
    assert.equal(indexed(`x ${secrets[40]} y`), "x ***REDACTED*** y");
    assert.equal(indexed(`x sec​ret-number-041-value y`), "x ***REDACTED*** y", "split by an invisible character");
  });
});

// Every line of a multi-line value of at least 8 characters is registered alone, except PEM armor and
// whitespace-only lines. Armor is joined at runtime; every secret-looking value is derived from a hash or joined
// from parts.
const hex = (seed: string, n = 40): string => crypto.createHash("sha256").update(seed).digest("hex").slice(0, n);
const b64 = (seed: string, n = 40): string => crypto.createHash("sha256").update(seed).digest("base64").replace(/[+/=]/g, "Q").slice(0, n);

/** Builds a claim from `lines`, then checks each excluded line is left alone and each registered line is masked, in
 *  redacted text and in the per-name line forms. */
function assertLineClasses(lines: string[], excluded: string[], registered: string[]): void {
  const claim = runEnvClaimForms([secretVar("SVC_FILE", lines.join("\n"))], []);
  const redact = makeTextRedactor(claim.redaction);
  const forms = claim.lineForms.find((n) => n.name === "SVC_FILE")?.forms ?? [];
  for (const line of excluded) {
    const text = `before\n${line}\nafter`;
    assert.equal(redact(text), text, `excluded: ${line}`);
    assert.ok(!forms.includes(line), `not a line form: ${line}`);
  }
  for (const line of registered) {
    const text = `before\n${line}\nafter`;
    assert.notEqual(redact(text), text, `registered: ${line}`);
    assert.ok(forms.includes(line), `a line form: ${line}`);
  }
}

describe("every line of 8+ characters of a multi-line value is registered, except standard PEM markers and ASCII space/tab-only lines", () => {
  const ENTROPY = bodyLines("one");

  it("lowercase-only, single-case one-digit, legacy PEM header, leading-key, URL host and rule lines are registered; armor and whitespace-only are not", () => {
    const value = [BEGIN, " ".repeat(8), "aaaaaaaaaaaaaaaa", "abcdefghij", "abcdefg1", "ABCDEFG1", "Proc-Type: 4,ENCRYPTED", "========",
      `"client_email": "${ENTROPY[0]}",`, "    server: https://kubernetes.example.com:6443", "11223344", ENTROPY[1]!, END];
    assertLineClasses(value, [BEGIN, value[1]!, END], value.slice(2, -1));
    const claim = runEnvClaimForms([secretVar("SVC_FILE", value.join("\n"))], []);
    const forms = claim.lineForms[0]!.forms;
    assert.ok(forms.includes(JSON.stringify(ENTROPY[1]).slice(1, -1)), "the JSON-escaped form is kept");
    assert.ok(!makeTextRedactor(claim.redaction)(`x ${JSON.stringify(ENTROPY[1]).slice(1, -1)} y`).includes(ENTROPY[1]!.slice(0, 30)));
  });

  it("a line shorter than 8 characters is not registered", () => {
    assertLineClasses(["abcdefg", ENTROPY[0]!], ["abcdefg"], [ENTROPY[0]!]);
  });

  it("an 8-space line and a tab-and-space line are excluded; an 8-NBSP line is not", () => {
    assertLineClasses([" ".repeat(8), "\t \t \t \t ", ENTROPY[0]!], [" ".repeat(8), "\t \t \t \t "], [ENTROPY[0]!]);
    assertLineClasses([" ".repeat(8), ENTROPY[0]!], [], [" ".repeat(8), ENTROPY[0]!]);
  });

  it("a 256 KiB line builds its forms in linear time", () => {
    const long = hex("long-line", 64).repeat(4096);
    const started = performance.now();
    const claim = runEnvClaimForms([secretVar("LONG_HEX", `${long}\n${ENTROPY[0]}\n`)], []);
    const ms = performance.now() - started;
    assert.ok(claim.lineForms[0]!.forms.includes(long), "the long line is a line form");
    assert.ok(ms < 1500, `forms built in ${ms.toFixed(0)} ms`);
  });

  it("a kubeconfig masks every line of 8+ characters", () => {
    const lines = ["apiVersion: v1", "kind: Config", "current-context: example", "clusters:", "- name: example", "    server: https://kubernetes.example.com:6443",
      `    certificate-authority-data: ${b64("ca", 60)}`, `    client-key-data: ${b64("key", 60)}`, `    token: ${b64("tok", 32)}`];
    assertLineClasses(lines, [], lines);
  });

  it("an AWS credentials file masks every line of 8+ characters, section headers included", () => {
    const lines = ["[default]", "region = us-east-1", `aws_access_key_id = ${["AK", "IA", "7Q2M", "X9D4", "K1LZ", "8P3R"].join("")}`, `aws_secret_access_key = ${b64("aws-secret")}`];
    assertLineClasses(lines, [], lines);
  });

  it("a Google service account masks its constant members, key id and key body", () => {
    const lines = ["{", '"type": "service_account",', '"auth_uri": "https://accounts.google.com/o/oauth2/auth",', '"token_uri": "https://oauth2.googleapis.com/token",',
      '"universe_domain": "googleapis.com"', `"private_key_id": "${hex("gsa-id")}",`, ...bodyLines("gsa"), "}"];
    assertLineClasses(lines, [], lines.filter((l) => l.length >= 8));
  });

  it("a padded last base64 line without + or / is masked", () => {
    const last = `${b64("pad", 41)}==`;
    assert.ok(!/[+/]/.test(last) && last.endsWith("=="));
    assertLineClasses([BEGIN, ...bodyLines("pad"), last, END], [BEGIN, END], [last]);
  });

  it("PEM armor lines of the standard labels are never registered", () => {
    assertLineClasses([BEGIN, ENTROPY[0]!, END], [BEGIN, END], [ENTROPY[0]!]);
  });

  it("the BEGIN and END marker of every listed standard label is exempt", () => {
    assert.equal(PEM_STANDARD_LABELS.length, 18);
    const markers = PEM_STANDARD_LABELS.flatMap((label) => [marker("BEGIN", label), marker("END", label)]);
    assertLineClasses([...markers, ENTROPY[0]!], markers, [ENTROPY[0]!]);
  });

  it("an unknown label, a mismatched kind and a lower-case label are normal line forms", () => {
    const lines = [marker("BEGIN", "SECRET STUFF"), marker("END", "SECRET STUFF"), marker("BEGIN", "private key"), "-----BEGIN PRIVATE KEY----", "-----BEGIN-----"];
    assertLineClasses([...lines, ENTROPY[0]!], [], [...lines, ENTROPY[0]!]);
  });

  it("a payload-bearing label (a secret carried in the label itself) is a normal line form", () => {
    // The old broad rule (`-----(BEGIN|END) [^-]+-----`) exempted these.
    const lines = [marker("BEGIN", b64("label-payload", 40)), marker("END", `PRIVATE KEY ${b64("label-tail", 24)}`)];
    assertLineClasses([...lines, ENTROPY[0]!], [], [...lines, ENTROPY[0]!]);
  });

  it("a marker with a leading space, trailing content or a trailing space is a normal line form", () => {
    const lines = [` ${BEGIN}`, `${BEGIN}abc`, `${BEGIN} x`, `${END} `, `\t${END}`, `${BEGIN} `];
    assertLineClasses([...lines, ENTROPY[0]!], [], [...lines, ENTROPY[0]!]);
  });

  it("a claim with no multi-line value has no line forms", () => {
    assert.deepEqual(runEnvClaimForms([secretVar("ONE_LINE", "single-line-dummy-value")], []).lineForms, []);
  });
});

describe("the form budget", () => {
  const big = (i: number): string => Array.from({ length: 800 }, (_, n) =>
    crypto.createHash("sha256").update(`budget-${i}-${n}`).digest("base64")).join("\r\n") + "\r\n";

  it("an over-budget claim reports it, stops building near the cap and registers only raw values", () => {
    const old = Array.from({ length: 300 }, (_, i) => retired(`OLD_${i % 7}`, big(i)));
    const claim = runEnvClaimForms([], old);
    assert.equal(claim.overBudget, true);
    assert.ok(claim.formChars > RUN_ENV_FORM_CHARS_MAX, "the cap was passed");
    assert.ok(claim.formChars < RUN_ENV_FORM_CHARS_MAX + 4_000_000, `stopped near the cap: ${claim.formChars}`);
    assert.deepEqual(claim.needles, []);
    assert.deepEqual(claim.lineForms, []);
    assert.ok(claim.redaction.includes(old[0]!.value), "the raw value is registered");
    assert.ok(claim.redaction.length <= 4 * old.length, "and no encoded form, only each value's variants");
  });

  it("a claim under the cap is complete", () => {
    const claim = runEnvClaimForms([secretVar("ONE", big(1))], []);
    assert.equal(claim.overBudget, false);
    assert.ok(claim.formChars > 0 && claim.formChars < RUN_ENV_FORM_CHARS_MAX);
    assert.ok(claim.needles.length === 1 && claim.lineForms.length === 1);
  });
});

describe("multiLineSecretVariables", () => {
  const live = (name: string, value: string, secret = true): ClaimRunEnvVar => ({ ...secretVar(name, value), secret });
  it("lists the distinct names of multi-line secrets with no length floor", () => {
    const names = multiLineSecretVariables(
      [
        live("PLAIN_NOTE", "first line of a note\nsecond line", false),
        live("CR_ONLY", "one\rtwo"),
        live("TINY_MULTI", "a\nb"),
        live("DUP", "x\ny"),
        live("SINGLE", "single-line-dummy-value"),
      ],
      [retired("DUP", "p\nq"), retired("RETIRED_FILE", "r\ns"), retired("RETIRED_ONE", "single-line-dummy-value")],
    );
    assert.deepEqual([...names].sort(), ["CR_ONLY", "DUP", "RETIRED_FILE", "TINY_MULTI"]);
  });
  it("is empty with no claim", () => {
    assert.deepEqual(multiLineSecretVariables(undefined, undefined), []);
  });
});
