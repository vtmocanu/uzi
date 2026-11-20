import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import {
  RUN_ENV_MAX_LIVE,
  RUN_ENV_MAX_LIVE_BYTES,
  RUN_ENV_MAX_REDACT,
  RUN_ENV_MAX_REDACT_BYTES,
  RUN_ENV_MAX_VALUE_BYTES,
  RUN_ENV_MIN_SECRET_LEN,
  RUN_ENV_V1,
  RunEnvClaimError,
  decodeRunEnvRedactResponse,
  decodeRunEnvSecrets,
} from "../src/run-env-claim.js";
import { makeTextRedactor } from "../src/redact.js";

const fixture = (name: string): any =>
  JSON.parse(fs.readFileSync(new URL(`../../fixtures/claims/${name}`, import.meta.url), "utf8"));

// Distinct v4-shaped ids.
const id = (n: number) => `00000000-0000-4000-8000-${n.toString(16).padStart(12, "0")}`;
const live = (n: number, over: Record<string, unknown> = {}) => ({
  var_id: id(n), scope: "repo", rev: 1, name: `VAR_${n}`, value: "plain", secret: false, description: "d", ...over,
});
const red = (n: number, over: Record<string, unknown> = {}) => ({
  var_id: id(1000 + n), scope: "user", rev: 1, name: `OLD_${n}`, value: "retired-secret-value", ...over,
});
const rejectsLive = (e: unknown, re = /./) => assert.throws(() => decodeRunEnvSecrets({ run_env: [e] }), (x) => x instanceof RunEnvClaimError && re.test(x.message));
const rejectsRedact = (e: unknown, re = /./) => assert.throws(() => decodeRunEnvSecrets({ run_env_redact: [e] }), (x) => x instanceof RunEnvClaimError && re.test(x.message));

describe("run_env_v1 constants", () => {
  it("names the capability", () => assert.equal(RUN_ENV_V1, "run_env_v1"));
  it("RUN_ENV_MIN_SECRET_LEN is the floor redact.ts masks at", () => {
    const below = "x".repeat(RUN_ENV_MIN_SECRET_LEN - 1);
    const at = "y".repeat(RUN_ENV_MIN_SECRET_LEN);
    const r = makeTextRedactor([below, at]);
    assert.equal(r(`${below} ${at}`).includes(below), true, "a secret below the floor is not masked");
    assert.equal(r(`${below} ${at}`).includes(at), false, "a secret at the floor is masked");
  });
});

describe("fixtures", () => {
  it("run-env-claim.json decodes", () => {
    const raw = fixture("run-env-claim.json");
    const d = decodeRunEnvSecrets(raw);
    assert.deepEqual(d.runEnv, raw.run_env);
    assert.deepEqual(d.runEnvRedact, raw.run_env_redact);
    assert.equal(d.runEnv.length, 2);
    assert.equal(d.runEnvRedact.length, 1);
  });
  it("run-env-redact-response.json decodes", () => {
    const raw = fixture("run-env-redact-response.json");
    const d = decodeRunEnvRedactResponse(raw);
    assert.equal(d.protected, true);
    assert.deepEqual(d.versions, raw.versions);
  });
});

describe("decodeRunEnvSecrets", () => {
  it("absent fields decode to empty arrays", () => {
    assert.deepEqual(decodeRunEnvSecrets({ forge_pat: "x" }), { runEnv: [], runEnvRedact: [] });
  });
  it("rejects a non-object and non-array lists", () => {
    for (const bad of [null, [], "x", 1]) assert.throws(() => decodeRunEnvSecrets(bad), RunEnvClaimError);
    for (const bad of [{}, "x", null, 1]) {
      assert.throws(() => decodeRunEnvSecrets({ run_env: bad }), RunEnvClaimError);
      assert.throws(() => decodeRunEnvSecrets({ run_env_redact: bad }), RunEnvClaimError);
    }
  });
  it("rejects a non-object entry", () => {
    for (const bad of [null, "x", 1, []]) rejectsLive(bad);
  });

  for (const [what, over] of [
    ["var_id not a string", { var_id: 5 }],
    ["var_id not a UUID", { var_id: "not-a-uuid" }],
    ["scope not repo|user", { scope: "org" }],
    ["scope not a string", { scope: 1 }],
    ["rev zero", { rev: 0 }],
    ["rev negative", { rev: -1 }],
    ["rev fractional", { rev: 1.5 }],
    ["rev a string", { rev: "1" }],
    ["name lower-case", { name: "lower" }],
    ["name starts with a digit", { name: "1A" }],
    ["name 129 chars", { name: "A".repeat(129) }],
    ["name not a string", { name: 3 }],
    ["value not a string", { value: 3 }],
    ["value holds NUL", { value: "a\u0000b" }],
    ["value over the byte limit", { value: "x".repeat(RUN_ENV_MAX_VALUE_BYTES + 1) }],
    ["value a lone surrogate", { value: "a\ud800b" }],
    ["secret not a boolean", { secret: "true" }],
    ["description not a string", { description: null }],
    ["secret one short of the floor", { secret: true, value: "x".repeat(RUN_ENV_MIN_SECRET_LEN - 1) }],
  ] as Array<[string, Record<string, unknown>]>) {
    it(`live entry: ${what}`, () => rejectsLive(live(1, over)));
  }
  for (const key of ["var_id", "scope", "rev", "name", "value", "secret", "description"]) {
    it(`live entry: missing ${key}`, () => {
      const e: Record<string, unknown> = live(1);
      delete e[key];
      rejectsLive(e, new RegExp(key));
    });
  }
  for (const [what, over] of [
    ["var_id not a UUID", { var_id: "x" }],
    ["scope invalid", { scope: "x" }],
    ["rev zero", { rev: 0 }],
    ["name invalid", { name: "bad name" }],
    ["value holds NUL", { value: "retired\u0000secret" }],
    ["value over the byte limit", { value: "x".repeat(RUN_ENV_MAX_VALUE_BYTES + 1) }],
    ["value one short of the floor", { value: "x".repeat(RUN_ENV_MIN_SECRET_LEN - 1) }],
  ] as Array<[string, Record<string, unknown>]>) {
    it(`redact entry: ${what}`, () => rejectsRedact(red(1, over)));
  }

  it("accepts the boundaries of value size, secret floor and plain empty values", () => {
    const d = decodeRunEnvSecrets({
      run_env: [
        live(1, { value: "" }),
        live(2, { value: "é".repeat(RUN_ENV_MAX_VALUE_BYTES / 2) }),
        live(3, { secret: true, value: "x".repeat(RUN_ENV_MIN_SECRET_LEN) }),
        live(4, { value: "short" }),
      ],
      run_env_redact: [red(1, { value: "x".repeat(RUN_ENV_MIN_SECRET_LEN) })],
    });
    assert.equal(d.runEnv.length, 4);
  });
  it("counts the secret floor in UTF-16 code units as redact.ts does", () => {
    // Four astral characters are 8 code units, so they meet the floor; three do not.
    assert.equal(decodeRunEnvSecrets({ run_env: [live(1, { secret: true, value: "\u{1F600}".repeat(4) })] }).runEnv.length, 1);
    rejectsLive(live(1, { secret: true, value: "\u{1F600}".repeat(3) }));
  });
  it("measures the value limit in UTF-8 bytes", () => {
    decodeRunEnvSecrets({ run_env: [live(1, { value: "é".repeat(RUN_ENV_MAX_VALUE_BYTES / 2) })] });
    rejectsLive(live(1, { value: "é".repeat(RUN_ENV_MAX_VALUE_BYTES / 2 + 1) }));
  });

  it("live count: 50 accepted, 51 rejected", () => {
    const mk = (n: number) => Array.from({ length: n }, (_, i) => live(i + 1));
    assert.equal(decodeRunEnvSecrets({ run_env: mk(RUN_ENV_MAX_LIVE) }).runEnv.length, RUN_ENV_MAX_LIVE);
    assert.throws(() => decodeRunEnvSecrets({ run_env: mk(RUN_ENV_MAX_LIVE + 1) }), RunEnvClaimError);
  });
  it("live bytes: exactly 262144 accepted, one more rejected", () => {
    const full = RUN_ENV_MAX_VALUE_BYTES;
    const four = [1, 2, 3, 4].map((n) => live(n, { value: "x".repeat(full) }));
    assert.equal(RUN_ENV_MAX_LIVE_BYTES, 4 * full);
    decodeRunEnvSecrets({ run_env: four });
    assert.throws(() => decodeRunEnvSecrets({ run_env: [...four, live(5, { value: "x" })] }), RunEnvClaimError);
  });
  it("redact count: 200 accepted, 201 rejected", () => {
    const mk = (n: number) => Array.from({ length: n }, (_, i) => red(i + 1));
    assert.equal(decodeRunEnvSecrets({ run_env_redact: mk(RUN_ENV_MAX_REDACT) }).runEnvRedact.length, RUN_ENV_MAX_REDACT);
    assert.throws(() => decodeRunEnvSecrets({ run_env_redact: mk(RUN_ENV_MAX_REDACT + 1) }), RunEnvClaimError);
  });
  it("redact bytes: exactly 2 MiB accepted, one more rejected", () => {
    const full = RUN_ENV_MAX_VALUE_BYTES;
    const n = RUN_ENV_MAX_REDACT_BYTES / full; // 32
    const mk = (extra: number) => [
      ...Array.from({ length: n }, (_, i) => red(i + 1, { value: "x".repeat(full) })),
      ...(extra ? [red(n + 1, { value: "x".repeat(extra < RUN_ENV_MIN_SECRET_LEN ? RUN_ENV_MIN_SECRET_LEN : extra) })] : []),
    ];
    decodeRunEnvSecrets({ run_env_redact: mk(0) });
    assert.throws(() => decodeRunEnvSecrets({ run_env_redact: mk(RUN_ENV_MIN_SECRET_LEN) }), RunEnvClaimError);
  });
  it("rejects a duplicate (var_id, rev) within a list and across live and redact", () => {
    assert.throws(() => decodeRunEnvSecrets({ run_env: [live(1), live(1, { name: "OTHER" })] }), /duplicate/);
    assert.throws(() => decodeRunEnvSecrets({ run_env_redact: [red(1), red(1)] }), /duplicate/);
    assert.throws(
      () => decodeRunEnvSecrets({ run_env: [live(1)], run_env_redact: [red(1, { var_id: id(1) })] }),
      /duplicate/,
    );
    // same var_id, different rev is fine
    decodeRunEnvSecrets({ run_env: [live(1, { rev: 2 })], run_env_redact: [red(1, { var_id: id(1), rev: 1 })] });
  });
  it("error messages never carry the value", () => {
    const secret = "sentinel-value-that-must-not-leak";
    try {
      decodeRunEnvSecrets({ run_env: [live(1, { name: "bad name", value: secret })] });
      assert.fail("expected a throw");
    } catch (e) {
      assert.ok(e instanceof RunEnvClaimError);
      assert.equal(e.message.includes(secret), false);
    }
  });
});

describe("decodeRunEnvRedactResponse", () => {
  it("protected true with and without versions", () => {
    assert.deepEqual(decodeRunEnvRedactResponse({ protected: true }), { protected: true, versions: [] });
    assert.equal(decodeRunEnvRedactResponse({ protected: true, versions: [red(1)] }).versions.length, 1);
  });
  it("protected false needs no or empty versions", () => {
    assert.deepEqual(decodeRunEnvRedactResponse({ protected: false }), { protected: false, versions: [] });
    assert.deepEqual(decodeRunEnvRedactResponse({ protected: false, versions: [] }), { protected: false, versions: [] });
    assert.throws(() => decodeRunEnvRedactResponse({ protected: false, versions: [red(1)] }), RunEnvClaimError);
  });
  it("rejects a non-boolean protected, a non-object body and malformed versions", () => {
    for (const bad of [{}, { protected: "true" }, { protected: 1 }, null, [], "x"]) {
      assert.throws(() => decodeRunEnvRedactResponse(bad), RunEnvClaimError);
    }
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: {} }), RunEnvClaimError);
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [red(1, { value: "short" })] }), RunEnvClaimError);
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [red(1), red(1)] }), /duplicate/);
  });
  it("versions count: 200 accepted, 201 rejected", () => {
    const mk = (n: number) => Array.from({ length: n }, (_, i) => red(i + 1));
    decodeRunEnvRedactResponse({ protected: true, versions: mk(RUN_ENV_MAX_REDACT) });
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: mk(RUN_ENV_MAX_REDACT + 1) }), RunEnvClaimError);
  });
});
