import { it } from "node:test";
import assert from "node:assert/strict";
import { GitCache, formatRetainedRecoveryDiagnostic, annotateRetainedSuccessorError } from "../src/git.js";
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

it("M1 untagged formatting preserves prototype observations", () => {
  let prototypeReads = 0;
  const error = new Proxy(new Error("plain untagged failure"), {
    getPrototypeOf() { return ++prototypeReads <= 2 ? Error.prototype : null; },
  });
  assert.deepEqual(formatRetainedRecoveryDiagnostic(error, s => s), {
    diagnostic_stage: "recovery", diagnostic_code: "recovery_failed", cause: "plain untagged failure",
  });
  assert.equal(prototypeReads, 2, "only the original formatter checks the untagged value");
});

// Candidate-only annotation API tables; portable lifecycle assertions live in the fixture tests.
const successorReasons = [
  "wrapper_tip_subject", "wrapper_source_subject", "wrapper_leaf_subject", "wrapper_commit_read",
  "wrapper_walk_bound", "wrapper_tip_bound", "wrapper_coverage_leaf_bound", "wrapper_lookalike_bound",
  "wrapper_aggregate_bound", "wrapper_leaf_bound", "wrapper_lookalike", "wrapper_source_missing",
  "wrapper_aggregate_missing", "wrapper_fingerprint_mismatch", "wrapper_source_oid", "wrapper_source_tree",
  "wrapper_tree", "wrapper_author", "wrapper_committer", "wrapper_source_leaf", "wrapper_fingerprint",
  "wrapper_pin_read", "wrapper_pin", "wrapper_lineage_marker", "wrapper_lineage_membership",
  "wrapper_marker_parent", "wrapper_lineage_ancestry", "wrapper_source_parent",
  "final_head", "final_tree", "final_marker_restoration", "final_dirty_diff", "final_ancestry",
  "attempt_invalid", "iteration_before", "episode_pin_changed", "identity_used", "seed_failed",
  "iteration_after", "post_adoption_check",
];
for (const reason of successorReasons) {
  it("M1 fixed successor allowlist: " + reason, () => {
    const error = new TypeError("retained_readiness_cleanup " + secret);
    const own = Object.getOwnPropertyDescriptors(error);
    assert.equal(annotateRetainedSuccessorError(error, reason), error);
    assert.deepEqual(Object.getOwnPropertyDescriptors(error), own);
    assert.equal(error.name, "TypeError");
    assert.deepEqual(Object.keys(error), []);
    assert.equal(JSON.stringify(error), "{}");
    assert.deepEqual(formatRetainedRecoveryDiagnostic(error, () => { throw new Error("must not redact"); }), {
      diagnostic_stage: "successor", diagnostic_code: "retained_successor_" + reason, cause: "",
    });
  });
}

it("M1 tags precede real transport metadata, throwing message getters and hostile redaction inputs", async () => {
  for (const message of [secret, secret.slice(0, 10) + "\n" + secret.slice(10), "x".repeat(65537)]) {
    const error = await failure(message);
    annotateRetainedSuccessorError(error, "seed_failed");
    Object.defineProperty(error, "message", { get() { throw new Error("message must not be read"); } });
    assert.deepEqual(formatRetainedRecoveryDiagnostic(error, () => { throw new Error("redactor must not run"); }), {
      diagnostic_stage: "successor", diagnostic_code: "retained_successor_seed_failed", cause: "",
    });
  }
});

it("M1 rejects unknown tags and untrusted successor prefixes, without altering untagged output", () => {
  for (const reason of ["unknown", "retained_successor_seed_failed", "seed_failed ", "__proto__", "constructor"]) {
    const error = Object.assign(new Error("retained_successor_" + reason), { diagnostic_code: "retained_successor_final_head" });
    const before = formatRetainedRecoveryDiagnostic(error, s => s);
    annotateRetainedSuccessorError(error, reason);
    assert.deepEqual(formatRetainedRecoveryDiagnostic(error, s => s), before);
    assert.deepEqual(before, { diagnostic_stage: "recovery", diagnostic_code: "recovery_failed", cause: error.message.trim() });
  }
  for (const value of ["retained_successor_seed_failed", null, 1, { message: "retained_successor_final_head" }]) {
    const before = formatRetainedRecoveryDiagnostic(value, s => s);
    assert.equal(annotateRetainedSuccessorError(value, "seed_failed"), value);
    assert.deepEqual(formatRetainedRecoveryDiagnostic(value, s => s), before);
  }
  assert.deepEqual(formatRetainedRecoveryDiagnostic(new Error("retained_readiness_cleanup"), s => s), {
    diagnostic_stage: "readiness", diagnostic_code: "retained_readiness_cleanup", cause: "",
  });
  assert.deepEqual(formatRetainedRecoveryDiagnostic(new Error("plain"), s => s), {
    diagnostic_stage: "recovery", diagnostic_code: "recovery_failed", cause: "plain",
  });
});
