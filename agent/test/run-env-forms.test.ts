import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import crypto from "node:crypto";
import { createLogger } from "../src/log.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";
import { runEnvRedactionForms, runEnvScanNeedles } from "../src/run-env-forms.js";
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

const BEGIN = ["-----BEGIN ", "DUMMY KEY", "-----"].join("");
const END = ["-----END ", "DUMMY KEY", "-----"].join("");
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
  const redact = makeTextRedactor(runEnvRedactionForms([secretVar("SVC_PASSWORD", VALUE)], []));
  const needles = runEnvScanNeedles([secretVar("SVC_PASSWORD", VALUE)], []);

  for (const [label, form] of Object.entries(encodings)) {
    it(label, () => {
      const text = `before ${form} after`;
      assert.ok(!redact(text).includes(form), "redacted");
      assert.deepEqual(textHitVariables(text, needles), ["SVC_PASSWORD"], "found");
    });
  }

  it("the three base64url alignments of a value whose base64 uses + and /", () => {
    const value = "pw-???>>>-~~~~???>>>-zz";
    const redact = makeTextRedactor(runEnvRedactionForms([secretVar("SVC_PASSWORD", value)], []));
    for (let n = 0; n < 3; n++) {
      const embedded = Buffer.from(`${"x".repeat(n)}${value}trailing`);
      assert.ok(/[+/]/.test(embedded.toString("base64")) && /[-_]/.test(embedded.toString("base64url")), "the fixture exercises both alphabets");
      const text = `t=${embedded.toString("base64url")};`;
      assert.ok(runEnvRedactionForms([secretVar("SVC_PASSWORD", value)], []).some((f) => text.includes(f)), `alignment ${n}`);
      assert.ok(redact(text).includes("***REDACTED***"), `redacted at alignment ${n}`);
    }
  });
});

describe("a multi-line secret in a unified diff", () => {
  const needles = runEnvScanNeedles([secretVar("SIGNING_KEY", PEM)], []);
  const redact = makeTextRedactor(runEnvRedactionForms([secretVar("SIGNING_KEY", PEM)], []));
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
    const withEol = runEnvScanNeedles([secretVar("SIGNING_KEY", `${PEM}\n`)], []);
    assert.deepEqual(textHitVariables(asPatch(lines), withEol), ["SIGNING_KEY"]);
  });

  it("a different key in the patch is not a hit", () => {
    assert.deepEqual(textHitVariables(asPatch(OTHER_PEM.split("\n")), needles), []);
  });

  it("a constant line of a JSON credential is not a hit, while the whole credential is", () => {
    const credential = ["{", '  "type": "service_account",', '  "project_id": "dummy-project-0001",', '  "private_key_id": "0123456789abcdef"', "}"].join("\n");
    const sa = runEnvScanNeedles([secretVar("GCP_CREDENTIALS", credential)], []);
    const publicLine = asPatch(['  "type": "service_account",', '  "client_email": "a@b.example"']);
    assert.deepEqual(textHitVariables(publicLine, sa), []);
    assert.deepEqual(textHitVariables(asPatch(credential.split("\n")), sa), ["GCP_CREDENTIALS"]);
  });

  it("registers each line for redaction but never the PEM armor, and never a line as a needle", () => {
    const forms = runEnvRedactionForms([secretVar("SIGNING_KEY", PEM)], []);
    for (const line of bodyLines("one")) assert.ok(forms.includes(line), "a body line is registered");
    assert.ok(!forms.includes(BEGIN) && !forms.includes(END), "armor is not registered");
    for (const needle of needles) for (const line of bodyLines("one")) assert.ok(!needle.forms.includes(line), "no per-line needle");
  });

  it("masks every body line wherever it appears alone, and the armor stays", () => {
    const out = redact(`${BEGIN}\n${bodyLines("one").map((l) => `> ${l}`).join("\n")}\n${END}\nand one line: ${bodyLines("one")[2]!}`);
    for (const line of bodyLines("one")) assert.ok(!out.includes(line), "body line masked");
    assert.ok(out.includes(BEGIN) && out.includes(END));
  });

  it("masks the secret in a preserved patch", () => {
    const out = redact(asPatch(lines));
    for (const line of bodyLines("one")) assert.ok(!out.includes(line));
  });

  it("masks the secret in a feed payload (a tool result)", () => {
    const payloadRedact = makeRedactor(runEnvRedactionForms([secretVar("SIGNING_KEY", PEM)], []));
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
  const value = (seed: string, bytes: number, lineLen: number): string => {
    let text = "";
    for (let i = 0; text.length < bytes; i++) text += crypto.createHash("sha512").update(`${seed}-${i}`).digest("base64").replace(/=+$/, "");
    text = text.slice(0, bytes);
    if (lineLen === 0) return text;
    return text.match(new RegExp(`.{1,${lineLen}}`, "g"))!.join("\n");
  };
  // The most forms a claim can register: every value of the claim split into the shortest registrable lines
  // (8 characters and a line break: 9 bytes per line, one form per line) plus the whole-value forms.
  const MAX_FORMS = Math.ceil((limits.max_run_env_total_bytes + limits.max_redact_total_bytes) / 9) + 10 * (limits.max_run_env_entries + limits.max_redact_entries);

  for (const [label, lineLen] of [["single-line", 0], ["64-column", 64], ["9-column", 9]] as const) {
    it(`${label} values: forms stay under the bound and a 1 MiB text redacts in under 5 s`, () => {
      const live = Array.from({ length: limits.max_run_env_entries }, (_, i) =>
        secretVar(`LIVE_${i}`, value(`live-${i}`, Math.floor(limits.max_run_env_total_bytes / limits.max_run_env_entries), i % 5 === 0 ? 0 : lineLen)));
      const old = Array.from({ length: limits.max_redact_entries }, (_, i) =>
        retired(`OLD_${i}`, value(`old-${i}`, Math.floor(limits.max_redact_total_bytes / limits.max_redact_entries), i % 5 === 0 ? 0 : lineLen)));
      const forms = runEnvRedactionForms(live, old);
      assert.ok(forms.length < MAX_FORMS, `${forms.length} forms`);
      const needleForms = runEnvScanNeedles(live, old).reduce((n, x) => n + x.forms.length, 0);
      assert.ok(needleForms < 16 * (limits.max_run_env_entries + limits.max_redact_entries), `${needleForms} needle forms`);

      const planted = old[7]!.value;
      const text = `${"ordinary output line with no secret 0123456789\n".repeat(Math.ceil((1024 * 1024) / 46))}${planted.split("\n")[0]}\n`;
      const started = performance.now();
      const out = makeTextRedactor(forms)(text);
      const elapsed = performance.now() - started;
      assert.ok(elapsed < 5000, `redacting took ${elapsed.toFixed(0)} ms`);
      assert.ok(!out.includes(planted.split("\n")[0]!), "the planted form is masked");

      const log = createLogger("error");
      for (const f of forms) log.addSecret(f);
      const line = JSON.stringify({ msg: "x".repeat(200), planted: planted.split("\n")[0] });
      const logStarted = performance.now();
      const scrubbed = log.redactDiagnostic!(line);
      const logElapsed = performance.now() - logStarted;
      assert.ok(logElapsed < 5000, `scrubbing one log line took ${logElapsed.toFixed(0)} ms`);
      assert.ok(!scrubbed.includes(planted.split("\n")[0]!));
    });
  }

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
