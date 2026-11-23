import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { ClaimResponse } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
import type { RunContext } from "../src/executor.js";
import { fakeGitlab, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// Engine-aware routing for a provider-bound Codex claim (secrets.provider with engine "codex" and NO
// secrets.codex). Sites that mean "this is a Codex run" must follow isCodexBoundClaim, not vendor
// credential presence, so a provider Codex claim is handled like its vendor twin.

const providerSecrets = (engine: "codex" | "claude"): ClaimResponse["secrets"] => ({
  forge_pat: "fixture-forge", forge_username: "bot",
  provider: {
    provider_id: `${engine}-engine-sites`, revision: 1, engine,
    protocol: engine === "codex" ? "openai_responses" : "anthropic_messages",
    base_url: "https://gateway.example.test/v1", auth_style: "bearer",
    credential: ["pk", "-engine-sites-", "0123456789abcdef"].join(""),
    model_map: engine === "codex" ? { default: { model: "corp-model" } } : { haiku: { model: "corp-model" } },
  },
}) as ClaimResponse["secrets"];

const vendorSecrets = (): ClaimResponse["secrets"] => ({
  forge_pat: "fixture-forge", forge_username: "bot", anthropic_oauth_token: "fixture-oauth",
  codex: { auth_mode: "subscription", access_token: "fixture-access", capability: "fixture-capability",
    generation: 3, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null },
}) as ClaimResponse["secrets"];

const claudeSecrets = (): ClaimResponse["secrets"] => ({
  forge_pat: "fixture-forge", forge_username: "bot", anthropic_oauth_token: "fixture-oauth",
}) as ClaimResponse["secrets"];

describe("repo-agent roster flavour follows the claim's engine", () => {
  const cases: Array<[string, () => ClaimResponse["secrets"], string]> = [
    ["vendor Codex", vendorSecrets, "codex"],
    ["provider-bound Codex", () => providerSecrets("codex"), "codex"],
    ["provider-bound Claude", () => providerSecrets("claude"), "claude"],
    ["plain Claude", claudeSecrets, "claude"],
  ];
  let iid = 4330;
  for (const [name, secrets, expected] of cases) {
    it(`${name} parses the roster as ${expected}`, async t => {
      const { gitlab } = fakeGitlab();
      const r = runner({ run: async (ctx: RunContext) => ({ branch: ctx.branch }) }, gitlab);
      const flavours: unknown[] = [];
      const parse = (r as unknown as { parseRepoAgents: (...a: unknown[]) => Promise<unknown> }).parseRepoAgents.bind(r);
      t.mock.method(r, "parseRepoAgents" as never, (async (...args: unknown[]) => {
        flavours.push(args[3]);
        return parse(...args);
      }) as never);
      await r.execute(gitlabClaim(iid++, { secrets: secrets() }));
      assert.deepEqual(flavours, [expected]);
    });
  }
});

describe("credential-free owner cancel follows the claim's engine", () => {
  const instance = (recoveryEnabled: boolean): RunRunner => {
    const r = Object.create(RunRunner.prototype) as RunRunner;
    Object.assign(r, { recovery: { enabled: recoveryEnabled } });
    return r;
  };
  const flight = (cancelled: boolean) => ({
    worktreePath: "/tmp/worktree", barePath: "/tmp/bare", steering: { isCancelled: () => cancelled },
  }) as unknown as Parameters<RunRunner["isCredentialFreeOwnerCancel"]>[1];
  const claim = (secrets: ClaimResponse["secrets"]): ClaimResponse => gitlabClaim(4340, { secrets });

  it("a provider-bound Codex owner cancel takes the credential-free preserve path", () => {
    assert.equal(instance(true)["isCredentialFreeOwnerCancel"](claim(providerSecrets("codex")), flight(true)), true);
  });
  it("a vendor Codex owner cancel still takes it", () => {
    assert.equal(instance(true)["isCredentialFreeOwnerCancel"](claim(vendorSecrets()), flight(true)), true);
  });
  it("Claude claims (plain or provider-bound) never take it", () => {
    for (const secrets of [claudeSecrets(), providerSecrets("claude")])
      assert.equal(instance(true)["isCredentialFreeOwnerCancel"](claim(secrets), flight(true)), false);
  });
  it("requires recovery enabled and an actual cancel", () => {
    assert.equal(instance(false)["isCredentialFreeOwnerCancel"](claim(providerSecrets("codex")), flight(true)), false);
    assert.equal(instance(true)["isCredentialFreeOwnerCancel"](claim(providerSecrets("codex")), flight(false)), false);
  });
});
