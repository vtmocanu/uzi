import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import {
  RUN_ENV_CAPABILITY,
  RUN_ENV_MAX_DESCRIPTION_BYTES,
  RUN_ENV_MAX_REDACT_ENTRIES,
  RUN_ENV_MAX_REDACT_TOTAL_BYTES,
  RUN_ENV_MAX_TOTAL_VALUE_BYTES,
  RUN_ENV_MAX_VALUE_BYTES,
  RUN_ENV_MAX_VARS,
  RunEnvClaimError,
  decodeRunEnv,
  decodeRunEnvRedactResponse,
} from "../src/run-env-claim.js";

// PRD 4416 M2 wire contract. The api side decodes and re-marshals the same fixtures in
// api/internal/workersvc/claim_run_env_seam_test.go.
const readFixture = (name: string): Record<string, unknown> =>
  JSON.parse(fs.readFileSync(new URL(`../../fixtures/claims/${name}`, import.meta.url), "utf8")) as Record<string, unknown>;

const ID = "6f1d2c3a-0b4e-4c5d-8a7b-1e2f3a4b5c6d";
const ID2 = "7a2e3d4b-1c5f-4d6e-9b8c-2f3a4b5c6d7e";

function plain(over: Record<string, unknown> = {}): Record<string, unknown> {
  return { var_id: ID, scope: "repo", rev: 1, name: "A_NAME", value: "v", secret: false, description: "d", ...over };
}
function redact(over: Record<string, unknown> = {}): Record<string, unknown> {
  return { var_id: ID2, scope: "user", rev: 1, name: "A_NAME", value: "x".repeat(8), ...over };
}
function uuidN(n: number): string {
  return `00000000-0000-4000-8000-${n.toString(16).padStart(12, "0")}`;
}
function breaches(secrets: unknown, fragment: string): void {
  assert.throws(
    () => decodeRunEnv(secrets),
    (e: unknown) => e instanceof RunEnvClaimError && e.message.includes(fragment),
    `expected RunEnvClaimError containing "${fragment}"`,
  );
}

describe("run-env claim contract", () => {
  it("advertises the run_env_v1 capability name the api knows", () => {
    assert.equal(RUN_ENV_CAPABILITY, "run_env_v1");
  });

  it("decodes fixtures/claims/run-env-claim.json", () => {
    const d = decodeRunEnv(readFixture("run-env-claim.json"));
    assert.equal(d.runEnv.length, 2);
    assert.equal(d.runEnvRedact.length, 2);
    assert.deepEqual(d.runEnv.map((v) => v.secret), [false, true]);
    assert.equal(d.runEnv[0]?.name, "SERVICE_URL");
    assert.equal(d.runEnvRedact[1]?.scope, "user");
  });

  it("decodes fixtures/claims/run-env-redact-response.json", () => {
    const r = decodeRunEnvRedactResponse(readFixture("run-env-redact-response.json"));
    assert.equal(r.protected, true);
    assert.equal(r.versions.length, 3);
    assert.equal(r.versions[0]?.name, "SERVICE_PASSPHRASE");
  });

  it("absent fields decode to empty arrays", () => {
    assert.deepEqual(decodeRunEnv({}), { runEnv: [], runEnvRedact: [] });
    assert.deepEqual(decodeRunEnv(undefined), { runEnv: [], runEnvRedact: [] });
    assert.deepEqual(decodeRunEnv({ forge_pat: "p" }), { runEnv: [], runEnvRedact: [] });
  });

  it("an unprotected maintenance answer carries no versions", () => {
    assert.deepEqual(decodeRunEnvRedactResponse({ protected: false, versions: [] }), { protected: false, versions: [] });
    assert.deepEqual(decodeRunEnvRedactResponse({ protected: false }), { protected: false, versions: [] });
  });
});

describe("decodeRunEnv fails closed", () => {
  it("non-object secrets", () => breaches("nope", "secrets must be an object"));
  it("non-array run_env", () => breaches({ run_env: {} }, "run_env must be an array"));
  it("non-array run_env_redact", () => breaches({ run_env_redact: "x" }, "run_env_redact must be an array"));
  it("non-object entry", () => breaches({ run_env: ["x"] }, "must be an object"));

  for (const [field, bad] of [
    ["var_id", 5],
    ["name", 5],
    ["value", 5],
    ["description", 5],
  ] as const) {
    it(`wrong type for ${field}`, () => breaches({ run_env: [plain({ [field]: bad })] }, field));
  }
  it("wrong type for secret", () => breaches({ run_env: [plain({ secret: "yes" })] }, "secret must be a boolean"));
  it("wrong type for rev", () => breaches({ run_env: [plain({ rev: "1" })] }, "rev"));
  it("wrong type in a redact entry", () => breaches({ run_env_redact: [redact({ value: 7 })] }, "value must be a string"));

  it("invalid uuid", () => breaches({ run_env: [plain({ var_id: "not-a-uuid" })] }, "var_id must be a uuid"));
  for (const scope of ["org", "", "REPO", 1]) {
    it(`scope ${JSON.stringify(scope)}`, () => breaches({ run_env: [plain({ scope })] }, "scope"));
  }
  for (const rev of [0, -1, 1.5, Number.NaN, 2 ** 60]) {
    it(`rev ${String(rev)}`, () => breaches({ run_env: [plain({ rev })] }, "rev must be a positive integer"));
  }
  for (const name of ["", "lower", "1ABC", "A-B", "A".repeat(129), "NAME\n"]) {
    it(`name ${JSON.stringify(name.length > 20 ? `${name.length} chars` : name)}`, () => breaches({ run_env: [plain({ name })] }, "name must match"));
  }

  it("more than 50 run_env entries", () => {
    const ok = Array.from({ length: RUN_ENV_MAX_VARS }, (_, i) => plain({ var_id: uuidN(i + 1) }));
    assert.equal(decodeRunEnv({ run_env: ok }).runEnv.length, RUN_ENV_MAX_VARS);
    breaches({ run_env: [...ok, plain({ var_id: uuidN(99) })] }, "more than 50 entries");
  });

  it("a value over 65536 bytes (multi-byte counted in bytes)", () => {
    const edge = "é".repeat(RUN_ENV_MAX_VALUE_BYTES / 2);
    assert.equal(decodeRunEnv({ run_env: [plain({ value: edge })] }).runEnv.length, 1);
    breaches({ run_env: [plain({ value: `${edge}a` })] }, "value exceeds 65536 bytes");
  });

  it("total run_env value bytes over 262144", () => {
    const full = "a".repeat(RUN_ENV_MAX_VALUE_BYTES);
    const four = Array.from({ length: 4 }, (_, i) => plain({ var_id: uuidN(i + 1), value: full }));
    assert.equal(RUN_ENV_MAX_TOTAL_VALUE_BYTES, 4 * RUN_ENV_MAX_VALUE_BYTES);
    assert.equal(decodeRunEnv({ run_env: four }).runEnv.length, 4);
    breaches({ run_env: [...four, plain({ var_id: uuidN(9), value: "b" })] }, "run_env values exceed 262144 bytes in total");
  });

  it("more than 200 redact entries", () => {
    const ok = Array.from({ length: RUN_ENV_MAX_REDACT_ENTRIES }, (_, i) => redact({ var_id: uuidN(i + 1) }));
    assert.equal(decodeRunEnv({ run_env_redact: ok }).runEnvRedact.length, RUN_ENV_MAX_REDACT_ENTRIES);
    breaches({ run_env_redact: [...ok, redact({ var_id: uuidN(999) })] }, "more than 200 entries");
  });

  it("a redact value over 65536 bytes", () => {
    breaches({ run_env_redact: [redact({ value: "a".repeat(RUN_ENV_MAX_VALUE_BYTES + 1) })] }, "value exceeds 65536 bytes");
  });

  it("total redact bytes over 2097152", () => {
    const full = "a".repeat(RUN_ENV_MAX_VALUE_BYTES);
    const per = RUN_ENV_MAX_REDACT_TOTAL_BYTES / RUN_ENV_MAX_VALUE_BYTES;
    const ok = Array.from({ length: per }, (_, i) => redact({ var_id: uuidN(i + 1), value: full }));
    assert.equal(decodeRunEnv({ run_env_redact: ok }).runEnvRedact.length, per);
    breaches({ run_env_redact: [...ok, redact({ var_id: uuidN(999) })] }, "values exceed 2097152 bytes in total");
  });

  it("a secret under 8 characters, in run_env and in run_env_redact", () => {
    breaches({ run_env: [plain({ secret: true, value: "short" })] }, "at least 8 characters");
    breaches({ run_env_redact: [redact({ value: "short" })] }, "at least 8 characters");
    assert.equal(decodeRunEnv({ run_env: [plain({ secret: true, value: "12345678" })] }).runEnv.length, 1);
  });

  it("a plain value may be short or empty", () => {
    assert.equal(decodeRunEnv({ run_env: [plain({ value: "" })] }).runEnv.length, 1);
  });

  it("a description over 1024 bytes", () => {
    assert.equal(decodeRunEnv({ run_env: [plain({ description: "d".repeat(RUN_ENV_MAX_DESCRIPTION_BYTES) })] }).runEnv.length, 1);
    breaches({ run_env: [plain({ description: "d".repeat(RUN_ENV_MAX_DESCRIPTION_BYTES + 1) })] }, "description exceeds 1024 bytes");
  });

  it("duplicate (var_id, rev) within run_env, within run_env_redact and across them", () => {
    breaches({ run_env: [plain(), plain({ name: "OTHER" })] }, "duplicate (var_id, rev)");
    breaches({ run_env_redact: [redact(), redact()] }, "duplicate (var_id, rev)");
    breaches({ run_env: [plain({ var_id: ID2, rev: 1 })], run_env_redact: [redact()] }, "duplicate (var_id, rev)");
    assert.equal(decodeRunEnv({ run_env: [plain(), plain({ rev: 2 })] }).runEnv.length, 2);
  });
});

describe("decodeRunEnvRedactResponse fails closed", () => {
  it("non-object body", () => assert.throws(() => decodeRunEnvRedactResponse([]), RunEnvClaimError));
  it("missing protected flag", () => assert.throws(() => decodeRunEnvRedactResponse({ versions: [] }), RunEnvClaimError));
  it("protected=false with versions", () => {
    assert.throws(() => decodeRunEnvRedactResponse({ protected: false, versions: [redact()] }), RunEnvClaimError);
  });
  it("a short secret version", () => {
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [redact({ value: "short" })] }), RunEnvClaimError);
  });
  it("duplicate (var_id, rev)", () => {
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [redact(), redact()] }), RunEnvClaimError);
  });
  it("more than 200 versions", () => {
    const many = Array.from({ length: RUN_ENV_MAX_REDACT_ENTRIES + 1 }, (_, i) => redact({ var_id: uuidN(i + 1) }));
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: many }), RunEnvClaimError);
  });
});
