import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { claimRunEnvSecretForms, runEnvSecretForms } from "../src/run-env-forms.js";
import { SecretSet } from "../src/redact.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";

// Fixtures are assembled at runtime so no token-shaped literal lives in source.
const SECRET = ["s3cr3t", "Pa55", "w0rd", "xyz"].join("-");

function caught(forms: string[], haystack: string): boolean {
  return forms.some((f) => haystack.includes(f));
}
const b64 = (s: string): string => Buffer.from(s, "utf8").toString("base64");

describe("runEnvSecretForms", () => {
  it("registers the value itself", () => {
    assert.ok(runEnvSecretForms([SECRET]).includes(SECRET));
  });

  it("registers the JSON-escaped form when it differs, and only then", () => {
    const hostile = 'ab"cd\\ef\ngh12345';
    const forms = runEnvSecretForms([hostile]);
    assert.ok(forms.includes(JSON.stringify(hostile).slice(1, -1)));
    assert.equal(runEnvSecretForms([SECRET]).filter((f) => f === JSON.stringify(SECRET).slice(1, -1)).length, 1);
  });

  it("catches the secret inside any base64 alignment of a longer stream", () => {
    const forms = runEnvSecretForms([SECRET]);
    for (const [pre, post] of [["", ""], ["x", ""], ["xx", ""], ["", "y"], ["user:", ""], ["xx", "yy"], ["abc", "z"], ["a", "yy"]]) {
      assert.ok(caught(forms, b64(`${pre}${SECRET}${post}`)), `missed alignment ${JSON.stringify([pre, post])}`);
    }
  });

  it("catches a docker config auth entry and an HTTP Basic header", () => {
    const forms = runEnvSecretForms([SECRET]);
    const auth = b64(`registry-user:${SECRET}`);
    assert.ok(caught(forms, `{"auths":{"r.example":{"auth":"${auth}"}}}`));
    assert.ok(caught(forms, `Authorization: Basic ${b64(`ci:${SECRET}`)}`));
  });

  it("works through the redactor end to end", () => {
    const set = new SecretSet(runEnvSecretForms([SECRET]));
    const out = set.text(`Authorization: Basic ${b64(`ci:${SECRET}`)} and ${SECRET}`);
    assert.equal(out.includes(SECRET), false);
    assert.equal(out.includes(b64(`ci:${SECRET}`).slice(4, -4)), false);
  });

  it("drops forms shorter than the masking floor and dedupes", () => {
    assert.deepEqual(runEnvSecretForms(["short"]), []);
    const forms = runEnvSecretForms([SECRET, SECRET]);
    assert.equal(new Set(forms).size, forms.length);
    for (const f of forms) assert.ok(f.length >= 8);
  });
});

describe("claimRunEnvSecretForms", () => {
  const live = (name: string, value: string, secret: boolean): ClaimRunEnvVar => ({
    var_id: "11111111-1111-4111-8111-111111111111", scope: "repo", rev: 1, name, value, secret, description: "",
  });
  const redact = (value: string): ClaimRunEnvRedact => ({
    var_id: "22222222-2222-4222-8222-222222222222", scope: "user", rev: 2, name: "OLD", value,
  });

  it("registers secret entries and redact-only entries, never a plain value", () => {
    const plain = "plain-visible-value-1234";
    const old = "retired-secret-value-5678";
    const forms = claimRunEnvSecretForms({
      run_env: [live("A_SECRET", SECRET, true), live("A_PLAIN", plain, false)],
      run_env_redact: [redact(old)],
    });
    assert.ok(forms.includes(SECRET));
    assert.ok(forms.includes(old));
    assert.equal(caught(forms, plain), false);
    assert.equal(caught(forms, b64(`x:${plain}`)), false);
  });

  it("is empty for a claim without variables", () => {
    assert.deepEqual(claimRunEnvSecretForms({}), []);
  });
});
