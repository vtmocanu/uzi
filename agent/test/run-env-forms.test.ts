import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { makeRedactor, makeTextRedactor, MIN_SECRET_LEN } from "../src/redact.js";
import { registeredForms, runEnvSecretForms } from "../src/run-env-forms.js";
import { runEnvVar } from "./run-env-catalogue.js";

// PRD #4416 M2, "Registered forms". Secrets are assembled at runtime; no token-shaped literal.
const SECRET = ["s3cr3t", "pass", "w0rd", "x9"].join("-");
const REDACTED = "***REDACTED***";

function redactorFor(value: string): (s: string) => string {
  return makeTextRedactor(registeredForms(value));
}

describe("registeredForms", () => {
  it("registers the value itself", () => {
    assert.ok(registeredForms(SECRET).includes(SECRET));
  });

  it("registers the JSON-escaped form only when it differs", () => {
    assert.deepEqual(registeredForms(SECRET).filter((f) => f !== SECRET && !/^[A-Za-z0-9+/]+$/.test(f)), []);
    const tricky = `pa"ss\\wo\nrd-12345`;
    const escaped = JSON.stringify(tricky).slice(1, -1);
    assert.notEqual(escaped, tricky);
    assert.ok(registeredForms(tricky).includes(escaped));
    // Scrubbing after JSON.stringify (what log.ts does) now finds it.
    assert.equal(redactorFor(tricky)(JSON.stringify({ line: tricky })), `{"line":"${REDACTED}"}`);
  });

  for (const k of [0, 1, 2]) {
    it(`catches the secret inside a base64 stream at alignment ${k}`, () => {
      const stream = Buffer.concat([Buffer.alloc(k, 0x41), Buffer.from(SECRET), Buffer.from("-tail-bytes")]).toString("base64");
      const out = redactorFor(SECRET)(`prefix ${stream} suffix`);
      assert.ok(out.includes(REDACTED), `alignment ${k} not caught: ${out}`);
      // Nothing of the secret's own base64 interior survives.
      const inner = Buffer.from(SECRET).toString("base64").slice(4, -4);
      assert.ok(!out.includes(inner), "the interior of the secret's encoding survived");
    });
  }

  it("catches a docker login auth entry (base64 of user:SECRET)", () => {
    const auth = Buffer.from(`registry-user:${SECRET}`).toString("base64");
    const config = JSON.stringify({ auths: { "registry.example.test": { auth } } });
    const out = redactorFor(SECRET)(config);
    assert.ok(out.includes(REDACTED), out);
    assert.ok(!out.includes(auth));
  });

  it("catches an HTTP Basic header", () => {
    const header = `Authorization: Basic ${Buffer.from(`svc:${SECRET}`).toString("base64")}`;
    assert.ok(redactorFor(SECRET)(header).includes(REDACTED));
  });

  it("drops forms shorter than the floor and never emits a form under it", () => {
    assert.deepEqual(registeredForms("short"), []);
    for (const f of registeredForms("12345678")) assert.ok(f.length >= MIN_SECRET_LEN);
    // An 8-character value has base64 forms shorter than 8 in the unaligned alignments: they are dropped.
    assert.ok(registeredForms("abcdefgh").every((f) => f.length >= MIN_SECRET_LEN));
  });

  it("does not match unrelated text", () => {
    const unrelated = "the quick brown fox jumps over the lazy dog 0123456789";
    assert.equal(redactorFor(SECRET)(unrelated), unrelated);
  });
});

describe("runEnvSecretForms", () => {
  it("registers secrets and redact entries, never plain values", () => {
    const plain = "https://stub.example.test:8443/base";
    const forms = runEnvSecretForms({
      run_env: [runEnvVar("SERVICE_URL", { value: plain, secret: false }), runEnvVar("SERVICE_KEY", { value: SECRET, secret: true })],
      run_env_redact: [{ var_id: "7a2e3d4b-1c5f-4d6e-9b8c-2f3a4b5c6d7e", scope: "repo", rev: 1, name: "SERVICE_KEY", value: "retired-secret-value" }],
    });
    assert.ok(forms.includes(SECRET));
    assert.ok(forms.includes("retired-secret-value"));
    assert.ok(!forms.includes(plain), "a plain value is not redacted");
    const text = makeRedactor(forms)({ line: `url=${plain} key=${SECRET} old=retired-secret-value` });
    assert.equal(text.line, `url=${plain} key=${REDACTED} old=${REDACTED}`);
  });

  it("is empty for a claim without variables", () => {
    assert.deepEqual(runEnvSecretForms({}), []);
  });
});
