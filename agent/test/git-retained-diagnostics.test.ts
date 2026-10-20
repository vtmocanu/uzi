import { it } from "node:test";
import assert from "node:assert/strict";
import { GitCache, formatRetainedRecoveryDiagnostic } from "../src/git.js";
import { makeTextRedactor } from "../src/redact.js";
import { nullLogger } from "./helpers.js";

const secret = "glpat-" + "abcdefghijklmnopqrst";
const redact = makeTextRedactor([secret]);
async function failure(stderr: string, arg = "origin"): Promise<Error> {
  const git = new GitCache("/unused-diagnostic-fixture", nullLogger());
  (git as any).execScoped = async () => { throw Object.assign(new Error("exec failed"), { stderr }); };
  try { await (git as any).runGit("/source", ["fetch", arg]); }
  catch (error) { return error as Error; }
  throw new Error("fixture did not fail");
}

it("actual runGit preserves its public message and carries no enumerable raw metadata", async () => {
  const error = await failure("  fatal: Permission denied\n");
  assert.equal(error.message, "git fetch origin failed: fatal: Permission denied");
  assert.deepEqual(Object.keys(error), []);
  assert.equal(JSON.stringify(error), "{}");
  const diagnostic = formatRetainedRecoveryDiagnostic(error, redact);
  assert.equal(diagnostic.diagnostic_stage, "git");
  assert.equal(diagnostic.diagnostic_code, "git_failed");
  assert.equal(diagnostic.cause, "fatal: Permission denied");
  assert.match(diagnostic.context!, /fetch origin/);
});

it("long argv and stderr paths preserve cause prefix and Permission denied suffix within all budgets", async () => {
  const diagnostic = formatRetainedRecoveryDiagnostic(
    await failure("fatal: cannot open /" + "p".repeat(5000) + ": Permission denied", "a".repeat(5000)), redact);
  assert.match(diagnostic.cause, /^fatal: cannot open/);
  assert.match(diagnostic.cause, /\.\.\..*Permission denied$/);
  assert.ok(diagnostic.cause.length <= 1024);
  assert.ok(diagnostic.context!.length <= 256);
  assert.match(diagnostic.context!, /\.\.\./);
  assert.ok(Object.values(diagnostic).join("").length +
    "retained recovery iteration failed; reserved budget retained".length <= 1536);
});

for (const separator of ["\n", "\u2028", "\u2029", "\u0000", "\u202e"]) {
  it("redacts whole input before sanitization and truncation with separator " + JSON.stringify(separator), async () => {
    const split = secret.slice(0, 10) + separator + secret.slice(10);
    const diagnostic = formatRetainedRecoveryDiagnostic(
      await failure("fatal: " + split + " /" + "x".repeat(3000) + " " + split + ": Permission denied", split), redact);
    assert.match(diagnostic.cause, /REDACTED/);
    assert.match(diagnostic.context!, /REDACTED/);
    assert.ok(!JSON.stringify(diagnostic).includes("abcdefgh"));
    assert.doesNotMatch(JSON.stringify(diagnostic), /ijklmnopqrst/);
  });
}

it("omits oversized unsplit input wholly before redaction, including UTF8 byte overflow", async () => {
  for (const input of ["x".repeat(65537), "é".repeat(32769), " ".repeat(65537) + secret]) {
    let calls = 0;
    const diagnostic = formatRetainedRecoveryDiagnostic(await failure(input), () => { calls++; throw new Error("must not redact"); });
    assert.equal(calls, 0);
    assert.equal(diagnostic.diagnostic_code, "input_oversized");
    assert.equal(diagnostic.cause, "[omitted: redaction input limit]");
    assert.equal(diagnostic.context, undefined);
  }
});

it("sanitizes controls and bidi and never cuts an astral character in half", async () => {
  const diagnostic = formatRetainedRecoveryDiagnostic(
    await failure("fatal:\u0000\u202e" + "😀".repeat(1000) + "\nPermission denied"), redact);
  assert.doesNotMatch(diagnostic.cause, /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u);
  assert.equal(Buffer.from(diagnostic.cause).toString("utf8"), diagnostic.cause);
  assert.match(diagnostic.cause, /Permission denied$/);
  assert.ok(diagnostic.cause.length <= 1024);
});

it("generic errors are redacted and bounded; helper and formatter failures use fixed codes only", () => {
  const generic = formatRetainedRecoveryDiagnostic(new Error(secret + "x".repeat(4000)), redact);
  assert.match(generic.cause, /REDACTED/);
  assert.ok(generic.cause.length <= 1024);
  const helper = formatRetainedRecoveryDiagnostic(new Error("retained_readiness_cleanup"), () => { throw new Error(secret); });
  assert.equal(helper.diagnostic_code, "retained_readiness_cleanup");
  assert.equal(helper.cause, "");
  const failed = formatRetainedRecoveryDiagnostic(new Error(secret), () => { throw new Error(secret); });
  assert.equal(failed.diagnostic_code, "formatter_failed");
  assert.equal(failed.cause, "");
  assert.ok(!JSON.stringify(failed).includes(secret));
});
