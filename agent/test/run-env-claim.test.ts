import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import {
  RUN_ENV_CAPABILITY,
  RUN_ENV_MAX_DESCRIPTION_BYTES,
  RUN_ENV_MAX_REDACT_ENTRIES,
  RUN_ENV_MAX_REV,
  RUN_ENV_MAX_REDACT_TOTAL_BYTES,
  RUN_ENV_MAX_TOTAL_VALUE_BYTES,
  RUN_ENV_MAX_VALUE_BYTES,
  RUN_ENV_MAX_VARS,
  decodeRunEnvClaim,
  decodeRunEnvRedactResponse,
} from "../src/run-env-claim.js";
import { MIN_SECRET_LEN } from "../src/redact.js";

const read = (name: string): unknown =>
  JSON.parse(fs.readFileSync(new URL(`../../fixtures/claims/${name}`, import.meta.url), "utf8"));
const claimFixture = read("run-env-claim.json") as { run_env: Record<string, unknown>[]; run_env_redact: Record<string, unknown>[] };
const redactFixture = read("run-env-redact-response.json") as { protected: boolean; versions: Record<string, unknown>[] };

const UUID = (n: number): string => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const plain = (n: number, value = "v"): Record<string, unknown> => ({
  var_id: UUID(n), scope: "repo", rev: 1, name: `NAME_${n}`, value, secret: false, description: "",
});
const redact = (n: number, value: string): Record<string, unknown> => ({
  var_id: UUID(n), scope: "user", rev: 1, name: "NAME", value,
});
const decode = (run_env: unknown, run_env_redact?: unknown) => decodeRunEnvClaim({ run_env, run_env_redact });

const limits = JSON.parse(fs.readFileSync(new URL("../../fixtures/run-env/claim-limits.json", import.meta.url), "utf8")) as Record<string, number>;

describe("run-env claim limits equal the shared fixture", () => {
  it("constants match fixtures/run-env/claim-limits.json (the api side is pinned to it against its store limits)", () => {
    assert.equal(RUN_ENV_MAX_VARS, limits.max_run_env_entries);
    assert.equal(RUN_ENV_MAX_VALUE_BYTES, limits.max_value_bytes);
    assert.equal(RUN_ENV_MAX_TOTAL_VALUE_BYTES, limits.max_run_env_total_bytes);
    assert.equal(RUN_ENV_MAX_REDACT_ENTRIES, limits.max_redact_entries);
    assert.equal(RUN_ENV_MAX_REDACT_TOTAL_BYTES, limits.max_redact_total_bytes);
    assert.equal(MIN_SECRET_LEN, limits.min_secret_len);
  });

  it("a claim with more redact entries than the retired budget alone still decodes", () => {
    // 201 legal entries: 200 retired versions plus a disabled secret, as the api can send.
    assert.doesNotThrow(() => decode([], Array.from({ length: 201 }, (_, i) => redact(i + 1, "longenough"))));
  });
});

describe("run-env claim fixtures", () => {
  it("capability name", () => assert.equal(RUN_ENV_CAPABILITY, "run_env_v1"));

  it("the claim fixture decodes and round-trips", () => {
    const d = decodeRunEnvClaim(claimFixture);
    assert.deepEqual({ run_env: d.runEnv, run_env_redact: d.runEnvRedact }, claimFixture);
  });

  it("the redact-response fixture decodes and round-trips", () => {
    assert.deepEqual(decodeRunEnvRedactResponse(redactFixture), redactFixture);
  });

  it("absent fields decode to empty lists", () => {
    assert.deepEqual(decodeRunEnvClaim({}), { runEnv: [], runEnvRedact: [] });
  });

  it("protected:false without versions decodes", () => {
    assert.deepEqual(decodeRunEnvRedactResponse({ protected: false }), { protected: false });
  });
});

describe("run-env claim limits fail closed", () => {
  it("more than the variable cap", () => {
    assert.doesNotThrow(() => decode(Array.from({ length: RUN_ENV_MAX_VARS }, (_, i) => plain(i + 1))));
    assert.throws(() => decode(Array.from({ length: RUN_ENV_MAX_VARS + 1 }, (_, i) => plain(i + 1))), /run_env: at most 50/);
  });

  it("value over the per-value cap (counted in UTF-8 bytes)", () => {
    assert.doesNotThrow(() => decode([plain(1, "a".repeat(RUN_ENV_MAX_VALUE_BYTES))]));
    assert.throws(() => decode([plain(1, "a".repeat(RUN_ENV_MAX_VALUE_BYTES + 1))]), /value: at most 65536 bytes/);
    // 21846 three-byte characters are 65538 bytes but only 21846 characters.
    assert.throws(() => decode([plain(1, "€".repeat(21846))]), /value: at most 65536 bytes/);
  });

  it("total run_env value bytes over the cap", () => {
    const per = RUN_ENV_MAX_VALUE_BYTES;
    const n = RUN_ENV_MAX_TOTAL_VALUE_BYTES / per;
    assert.doesNotThrow(() => decode(Array.from({ length: n }, (_, i) => plain(i + 1, "a".repeat(per)))));
    assert.throws(() => decode([...Array.from({ length: n }, (_, i) => plain(i + 1, "a".repeat(per))), plain(n + 1, "a")]), /values total/);
  });

  it("more than the redact entry cap", () => {
    const mk = (count: number) => Array.from({ length: count }, (_, i) => redact(i + 1, "longenough"));
    assert.doesNotThrow(() => decode([], mk(RUN_ENV_MAX_REDACT_ENTRIES)));
    assert.throws(() => decode([], mk(RUN_ENV_MAX_REDACT_ENTRIES + 1)), new RegExp(`run_env_redact: at most ${RUN_ENV_MAX_REDACT_ENTRIES}`));
  });

  it("total redact value bytes over the cap", () => {
    const per = RUN_ENV_MAX_VALUE_BYTES;
    const n = RUN_ENV_MAX_REDACT_TOTAL_BYTES / per; // 40 full-size values
    const full = Array.from({ length: n }, (_, i) => redact(i + 1, "a".repeat(per)));
    assert.doesNotThrow(() => decode([], full));
    assert.throws(() => decode([], [...full, redact(n + 1, "longenough")]), /values total/);
  });

  it("a secret under the redaction floor (run_env and redact)", () => {
    assert.throws(() => decode([{ ...plain(1, "short"), secret: true }]), /a secret must be at least 8 characters/);
    assert.doesNotThrow(() => decode([plain(1, "short")]));
    assert.throws(() => decode([], [redact(1, "short")]), /a secret must be at least 8 characters/);
    assert.doesNotThrow(() => decode([{ ...plain(1, "12345678"), secret: true }]));
  });

  it("a malformed name", () => {
    for (const name of ["lower", "1ABC", "A-B", "", "A".repeat(129)]) {
      assert.throws(() => decode([{ ...plain(1), name }]), /name: must match/);
    }
  });

  it("a scope outside repo and user", () => {
    assert.throws(() => decode([{ ...plain(1), scope: "org" }]), /scope: must be repo or user/);
  });

  it("rev that is not a positive integer", () => {
    for (const rev of [0, -1, 1.5, "1", null]) {
      assert.throws(() => decode([{ ...plain(1), rev }]), /rev: must be a positive integer/);
    }
  });

  it("rev above the api's int32 range", () => {
    assert.doesNotThrow(() => decode([{ ...plain(1), rev: RUN_ENV_MAX_REV }]));
    assert.throws(() => decode([{ ...plain(1), rev: RUN_ENV_MAX_REV + 1 }]), /rev: must be a positive integer/);
    assert.throws(() => decode([], [{ ...redact(1, "longenough"), rev: RUN_ENV_MAX_REV + 1 }]), /rev: must be a positive integer/);
  });

  it("lone surrogates in a value or description", () => {
    assert.throws(() => decode([plain(1, "a\ud800b")]), /value: must be valid Unicode/);
    assert.throws(() => decode([plain(1, "a\udc00")]), /value: must be valid Unicode/);
    assert.throws(() => decode([], [redact(1, "longenough\ud800")]), /value: must be valid Unicode/);
    assert.throws(() => decode([{ ...plain(1), description: "x\ud800" }]), /description: must be valid Unicode/);
    assert.doesNotThrow(() => decode([plain(1, "pair \ud83d\ude00")]));
  });

  it("var_id that is not a UUID", () => {
    assert.throws(() => decode([{ ...plain(1), var_id: "not-a-uuid" }]), /var_id: must be a UUID/);
  });

  it("description over its cap or not a string", () => {
    assert.doesNotThrow(() => decode([{ ...plain(1), description: "d".repeat(RUN_ENV_MAX_DESCRIPTION_BYTES) }]));
    assert.throws(() => decode([{ ...plain(1), description: "d".repeat(RUN_ENV_MAX_DESCRIPTION_BYTES + 1) }]), /description: at most 1024 bytes/);
    assert.throws(() => decode([{ ...plain(1), description: 5 }]), /description: must be a string/);
  });

  it("NUL in a value", () => {
    assert.throws(() => decode([plain(1, "a\u0000b")]), /value: must not contain NUL/);
    assert.throws(() => decode([], [redact(1, "longenough\u0000")]), /value: must not contain NUL/);
  });

  it("duplicate (var_id, rev)", () => {
    assert.throws(() => decode([plain(1), plain(1)]), /duplicate/);
    assert.throws(() => decode([], [redact(1, "longenough"), redact(1, "otherlonger")]), /duplicate/);
    // two versions of one variable may be in run_env_redact, never twice in run_env.
    assert.doesNotThrow(() => decode([], [redact(1, "longenough"), { ...redact(1, "otherlonger"), rev: 2 }]));
  });

  it("duplicate variable names in run_env", () => {
    assert.throws(() => decode([plain(1), { ...plain(2), name: "NAME_1" }]), /run_env\[1\]\.name: duplicate name/);
    assert.doesNotThrow(() => decode([plain(1), { ...plain(2), name: "OTHER" }]));
    // redact entries are versions, not variables: a name may repeat there.
    assert.doesNotThrow(() => decode([], [redact(1, "longenough"), redact(2, "otherlonger")]));
  });

  it("a (var_id, rev) in both run_env and run_env_redact", () => {
    const secret = { ...plain(1, "longenough"), secret: true };
    assert.throws(() => decode([secret], [redact(1, "longenough")].map((r) => ({ ...r, scope: "repo" }))), /run_env_redact\[0\]: .*already in run_env/);
    assert.doesNotThrow(() => decode([secret], [{ ...redact(1, "olderversion"), scope: "repo", rev: 2 }]));
  });

  it("exact field types", () => {
    assert.throws(() => decode({}), /run_env: must be an array/);
    assert.throws(() => decode([null]), /must be an object/);
    assert.throws(() => decode([{ ...plain(1), secret: "false" }]), /secret: must be a boolean/);
    assert.throws(() => decode([{ ...plain(1), value: 5 }]), /value: must be a string/);
    assert.throws(() => decodeRunEnvClaim("x"), /secrets: must be an object/);
  });

  it("an error never carries the value", () => {
    assert.throws(
      () => decode([{ ...plain(1, "topsecretvalue"), name: "bad-name" }]),
      (e: Error) => !e.message.includes("topsecretvalue"),
    );
  });
});

describe("run-env redact response rules", () => {
  it("protected:false must carry no versions", () => {
    assert.throws(() => decodeRunEnvRedactResponse({ protected: false, versions: [redact(1, "longenough")] }), /empty when protected is false/);
  });

  it("protected:true with no versions fails closed", () => {
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true }), /non-empty when protected is true/);
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [] }), /non-empty when protected is true/);
  });

  it("applies the per-entry rules", () => {
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [redact(1, "short")] }), /at least 8 characters/);
    assert.throws(() => decodeRunEnvRedactResponse({ protected: true, versions: [redact(1, "longenough"), redact(1, "longenough")] }), /duplicate/);
    assert.throws(() => decodeRunEnvRedactResponse({ protected: "yes" }), /protected: must be a boolean/);
  });
});
