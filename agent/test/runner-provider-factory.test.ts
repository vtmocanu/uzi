import { it } from "node:test";
import assert from "node:assert/strict";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { inspect } from "node:util";
import { ProviderLaneUnsupportedError } from "../src/provider-binding.js";
import { createLogger } from "../src/log.js";
import { providerKeyForms } from "../src/redact.js";
import { fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const key = ["factory/", "opaque<&", "fixture+key"].join("");
const forms = providerKeyForms(key);
const provider: ClaimProviderBinding = Object.freeze({
  provider_id: "factory-fixture", revision: 1, engine: "claude", protocol: "anthropic_messages",
  base_url: "https://gateway.example.test", auth_style: "bearer", credential: key,
  model_map: { haiku: { model: "corp-haiku" } },
});

it("registers claim forms before factory diagnostics and detaches errors before eviction", async (t) => {
  const output: string[] = [];
  t.mock.method(process.stderr, "write", (line: string) => { output.push(line); return true; });
  const { gitlab } = fakeGitlab();
  const log = createLogger();
  const child = log.child({ lane: "construction" });
  const claim = gitlabClaim(45321, { secrets: { forge_pat: "fixture-forge-pat", provider } });
  let seen = false;
  const runner = runnerWith((_id, codex, addr) => {
    seen = true;
    assert.equal(codex, undefined);
    assert.equal(addr?.provider, provider);
    assert.ok(Object.isFrozen(addr?.provider));
    for (const form of forms) {
      assert.ok(!child.redactDiagnostic!(form).includes(form));
    }
    child.error("factory refused", { diagnostic: forms.join(" "), members: { echoed: forms } });
    throw new Error(forms.join(" "), { cause: new Error(key) });
  }, gitlab, undefined, log);
  await assert.rejects(runner.execute(claim), error => {
    assert.ok(error instanceof Error);
    assert.equal(error.cause, undefined);
    for (const form of forms) assert.ok(!error.message.includes(form));
    assert.ok(error.message.length <= 200);
    return true;
  });
  assert.ok(seen);
  assert.equal(output.length, 1);
  for (const form of forms) assert.ok(!output.join("").includes(form));
  for (const form of forms) assert.equal(child.redactDiagnostic!(form), form, "factory failure balances registration");
});

for (const sibling of [false, true]) {
  for (const diagnostic of ["message", "cause"] as const) {
    it(`vendor construction ${diagnostic} is detached after eviction, sibling=${sibling}`, async (t) => {
      const output: string[] = [];
      t.mock.method(process.stderr, "write", (line: string) => { output.push(line); return true; });
      const { gitlab } = fakeGitlab();
      const log = createLogger();
      const codex = {
        auth_mode: "subscription" as const, access_token: "fixture-factory-access",
        capability: "fixture-factory-capability", generation: 3,
        chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
      };
      const canaries = [codex.access_token, codex.capability];
      if (sibling) for (const value of canaries) log.addSecret(value);
      const original = Object.assign(new Error(diagnostic === "message" ? canaries.join(" ") : "construction refused",
        { cause: new Error(canaries.join(" ")) }), { unsafe: canaries });
      const runner = runnerWith(function (_id, received, addr) {
        assert.equal(arguments.length, 2, "vendor argument shape stays unchanged");
        assert.equal(received, codex);
        assert.equal(addr, undefined);
        log.error("construction refused", { error: original });
        throw original;
      }, gitlab, undefined, log);
      let rejected: unknown;
      await assert.rejects(runner.execute(gitlabClaim(45324, {
        secrets: { forge_pat: "fixture-forge-pat", codex },
      })), error => {
        rejected = error;
        assert.ok(error instanceof Error);
        assert.notEqual(error, original);
        assert.equal(error.cause, undefined);
        assert.deepEqual(Object.keys(error), []);
        return true;
      });
      for (const value of canaries) {
        assert.equal(log.redactDiagnostic!(value).includes(value), !sibling);
      }
      if (sibling) for (const value of canaries) log.removeSecret(value);
      log.error("after eviction", { error: rejected });
      for (const value of canaries) {
        assert.equal(log.redactDiagnostic!(value), value, "balanced after sibling eviction");
        assert.ok(!inspect(rejected).includes(value));
        assert.ok(!output.join("").includes(value));
      }
    });
  }
}

for (const engine of ["provider", "vendor"] as const) {
  it(`a throwing construction message getter cannot escape ${engine} canaries`, async () => {
    const { gitlab } = fakeGitlab();
    const log = createLogger("error");
    const codex = { auth_mode: "api_key" as const, access_token: key, capability: "fixture-capability" };
    const original = new Error("construction fault");
    Object.defineProperty(original, "message", { get: () => { throw new Error(key); } });
    const runner = runnerWith(() => { throw original; }, gitlab, undefined, log);
    const secrets = engine === "provider"
      ? { forge_pat: "fixture-forge-pat", provider }
      : { forge_pat: "fixture-forge-pat", codex };
    await assert.rejects(runner.execute(gitlabClaim(45327, { secrets })), error => {
      assert.ok(error instanceof Error);
      assert.ok(!inspect(error).includes(key));
      assert.equal(error.cause, undefined);
      return true;
    });
    assert.equal(log.redactDiagnostic!(key), key);
  });
}

it("a factory without bound credential canaries preserves its original error", async () => {
  const { gitlab } = fakeGitlab();
  const original = new Error("ordinary constructor failure", { cause: new Error("detail") });
  const runner = runnerWith(() => { throw original; }, gitlab);
  await assert.rejects(runner.execute(gitlabClaim(45326)), error => error === original);
});

it("construction preserves only the stable typed provider refusal", async () => {
  const { gitlab } = fakeGitlab();
  const original = Object.assign(new ProviderLaneUnsupportedError(), { unsafe: key, cause: new Error(key) });
  const runner = runnerWith(() => { throw original; }, gitlab);
  await assert.rejects(runner.execute(gitlabClaim(45325, { secrets: { forge_pat: "fixture-forge-pat", provider } })), error => {
    assert.ok(error instanceof ProviderLaneUnsupportedError);
    assert.notEqual(error, original);
    assert.equal(error.reason, original.reason);
    assert.equal(error.cause, undefined);
    assert.ok(!inspect(error).includes(key));
    return true;
  });
});

it("successful execution balances the hoisted registration", async () => {
  const { gitlab } = fakeGitlab();
  const log = createLogger("error");
  const runner = runnerWith((_id, _codex, addr) => {
    assert.equal(addr?.provider, provider);
    assert.ok(!log.redactDiagnostic!(key).includes(key));
    return { executor: { run: async ctx => ({ branch: ctx.branch, reportOnly: true, summary: "ok" }) } };
  }, gitlab, undefined, log);
  await runner.execute(gitlabClaim(45323, { secrets: { forge_pat: "fixture-forge-pat", provider } }));
  for (const form of forms) assert.equal(log.redactDiagnostic!(form), form);
});

it("factory failure leaves an identical sibling registration intact", async () => {
  const { gitlab } = fakeGitlab();
  const log = createLogger();
  for (const form of forms) log.addSecret(form);
  const runner = runnerWith(() => { throw new Error(key); }, gitlab, undefined, log);
  await assert.rejects(runner.execute(gitlabClaim(45322, { secrets: { forge_pat: "fixture-forge-pat", provider } })));
  for (const form of forms) assert.ok(!log.redactDiagnostic!(form).includes(form));
  for (const form of forms) log.removeSecret(form);
  assert.equal(log.redactDiagnostic!(key), key);
});
