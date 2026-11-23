import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { PauseNowSignal } from "../src/steering.js";
import type { ClaimCodexProviderBinding } from "../src/protocol.js";
import { decodeProviderBinding } from "../src/provider-binding.js";
import { createCodexProviderExecutionAuthority, selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import { CodexAdviceCredentialBridge, buildRunLaneReconcile } from "../src/codex/codex-executor.js";

function provider() {
  const decoded = decodeProviderBinding({ provider: {
    provider_id: "provider-1", revision: 2, engine: "codex", protocol: "openai_responses",
    base_url: "https://gateway.example/v1", auth_style: "bearer",
    credential: "provider-fixture-key", model_map: { default: { model: "gateway-model", context_window: 32768 } },
  } });
  assert.equal(decoded?.engine, "codex");
  return decoded as ClaimCodexProviderBinding;
}
function vendor(subscription = false): CodexBinding {
  const selection = selectCodexBinding({ codex: {
    auth_mode: subscription ? "subscription" : "api_key", access_token: "vendor-key", capability: "vendor-cap",
    ...(subscription ? { generation: 3, chatgpt_account_id: "account-1", chatgpt_plan_type: null } : {}),
  } });
  assert.equal(selection.kind, "codex");
  if (selection.kind !== "codex") throw new Error("expected codex");
  return selection.binding;
}
const request = { boundary: "checkpoint" as const, deadlineMs: 1000 };
const signal = new AbortController().signal;

describe("Codex provider execution authority bridge contract", () => {
  it("validates a distinct authority and deeply freezes an independent snapshot", () => {
    const input = provider();
    const authority = createCodexProviderExecutionAuthority(input);
    input.credential = "replacement-fixture";
    input.base_url = "https://changed.example/v1";
    input.model_map.default.model = "changed";
    assert.equal(authority.authMode, "provider");
    assert.equal(authority.providerBinding.credential, "provider-fixture-key");
    assert.equal(authority.providerBinding.base_url, "https://gateway.example/v1");
    assert.equal(authority.providerBinding.model_map.default.model, "gateway-model");
    assert.ok(Object.isFrozen(authority));
    assert.ok(Object.isFrozen(authority.providerBinding));
    assert.ok(Object.isFrozen(authority.providerBinding.model_map));
    assert.ok(Object.isFrozen(authority.providerBinding.model_map.default));
    assert.throws(() => Object.assign(authority.providerBinding, { credential: "changed" }), TypeError);
    assert.throws(() => Object.assign(authority.providerBinding.model_map.default, { model: "changed" }), TypeError);
    assert.equal("capability" in authority, false);
    assert.equal("accessToken" in authority, false);
    assert.throws(() => createCodexProviderExecutionAuthority({ ...provider(), credential: "" }));
    // @ts-expect-error Provider execution authority cannot widen vendor-only consumers.
    const unsupported: CodexBinding = authority;
    void unsupported;
    // @ts-expect-error The provider authority brand prevents plain object construction.
    const forged: typeof authority = { authMode: "provider", providerBinding: input };
    void forged;
  });

  it("executes provider release and every fresh reconcile without vendor or vault admission", async () => {
    const calls: string[] = [];
    const client = {
      releaseCodex: async (): Promise<never> => { calls.push("release"); throw new Error("vendor release"); },
      refreshCodex: async (): Promise<never> => { calls.push("refresh"); throw new Error("vendor refresh"); },
      admitCodexOperation: async (): Promise<never> => { calls.push("admission"); throw new Error("vendor admission"); },
    };
    const input = provider();
    const authority = createCodexProviderExecutionAuthority(input);
    const bridge = new CodexAdviceCredentialBridge("run", client, authority);
    input.credential = "mutated-source-key";
    assert.equal(bridge.authMode, "provider");
    assert.equal(bridge.chatgptAccountId, undefined);
    assert.equal(await bridge.release(), "provider-fixture-key");
    assert.equal(await bridge.release(), "provider-fixture-key");
    await assert.rejects(bridge.refresh(), /not permitted/);
    assert.throws(() => bridge.buildSubscriptionRefreshBridge(() => {}), /not permitted/);
    const tokens: string[] = [];
    let fences = 0;
    const reconcile = buildRunLaneReconcile("run", client, authority, t => tokens.push(t), undefined,
      () => assert.fail("provider requires no vault"), undefined, () => { fences++; });
    assert.deepEqual(await reconcile(request, signal), { kind: "ready" });
    assert.deepEqual(await reconcile(request, signal), { kind: "ready" });
    assert.deepEqual(tokens, ["provider-fixture-key", "provider-fixture-key"]);
    assert.ok(fences >= 2);
    assert.deepEqual(calls, []);
    const aborted = AbortSignal.abort();
    await assert.rejects(bridge.release(aborted));
    assert.equal((await reconcile(request, aborted)).kind, "blocked");
    assert.equal((await reconcile({ ...request, deadlineMs: 0 }, signal)).kind, "blocked");
    assert.equal(tokens.length, 2);
    const fenced = buildRunLaneReconcile("run", client, authority, () => assert.fail("fenced"), undefined,
      undefined, undefined, () => { throw new Error("retired"); });
    await assert.rejects(fenced(request, signal), /retired/);
    assert.deepEqual(calls, []);
  });

  it("provider soft admission accepts two typed pause lifecycles without credential exchange", async () => {
    const client = {
      releaseCodex: async (): Promise<never> => assert.fail("provider must not release"),
      refreshCodex: async (): Promise<never> => assert.fail("provider must not refresh"),
    };
    const authority = createCodexProviderExecutionAuthority(provider());
    for (const obstacle of ["none", "captured hard cancel", "actual hard cancel", "boundary cancel", "deadline", "retired"] as const) {
      const captured = new AbortController(), actual = new AbortController(), soft = new AbortController();
      captured.abort(obstacle === "captured hard cancel" ? new Error("hard stop") : new PauseNowSignal());
      actual.abort(obstacle === "actual hard cancel" ? new Error("hard stop") : new PauseNowSignal());
      soft.abort();
      const stop = { signal: soft.signal, lifecycleSignal: captured.signal,
        cooperativeLifecycleReason: (value: unknown) => value instanceof PauseNowSignal };
      const reconcile = buildRunLaneReconcile("run", client, authority,
        () => assert.fail("stopped provider must not register a token"), undefined,
        () => assert.fail("provider requires no vault"), actual.signal,
        () => { if (obstacle === "retired") throw new Error("retired"); });
      const result = reconcile({ ...request, deadlineMs: obstacle === "deadline" ? 0 : 1000,
        checkpointStop: stop }, obstacle === "boundary cancel" ? AbortSignal.abort() : signal);
      if (obstacle === "none") await assert.rejects(result, CheckpointSoftDeadlineError);
      else if (obstacle === "retired") await assert.rejects(result, /retired/);
      else assert.equal((await result).kind, "blocked", obstacle);
    }
  });

  it("preserves vendor capability fencing, release and subscription refresh", async () => {
    const calls: string[] = [];
    const client = {
      releaseCodex: async (_run: string, body: { capability: string }) => {
        assert.equal(body.capability, "vendor-cap"); calls.push("release");
        return { auth_mode: "api_key" as const, access_token: "released-key" };
      },
      refreshCodex: async (_run: string, body: { capability: string; observed_generation: number }) => {
        assert.equal(body.capability, "vendor-cap"); assert.equal(body.observed_generation, 3);
        calls.push("refresh");
        return { auth_mode: "subscription" as const, access_token: "refreshed-key", generation: 4, chatgpt_account_id: "account-1", chatgpt_plan_type: null };
      },
    };
    const bridge = new CodexAdviceCredentialBridge("run", client as never, vendor());
    assert.equal(await bridge.release(), "released-key");
    await assert.rejects(bridge.refresh(), /not permitted/);
    const sub = new CodexAdviceCredentialBridge("run", client as never, vendor(true));
    assert.equal(await sub.refresh(), "refreshed-key");
    const tokens: string[] = [];
    assert.deepEqual(await buildRunLaneReconcile("run", client as never, vendor(), t => tokens.push(t))(request, signal), { kind: "ready" });
    assert.deepEqual(await buildRunLaneReconcile("run", client as never, vendor(true), t => tokens.push(t))(request, signal), { kind: "ready" });
    assert.deepEqual(tokens, ["released-key", "refreshed-key"]);
    assert.deepEqual(calls, ["release", "refresh", "release", "refresh"]);
    const denied = { ...client, releaseCodex: async () => { throw new Error("capability revoked"); } };
    await assert.rejects(new CodexAdviceCredentialBridge("run", denied as never, vendor()).release(), /capability revoked/);
    assert.equal((await buildRunLaneReconcile("run", denied as never, vendor(), () => assert.fail("denied"))(request, signal)).kind, "blocked");
  });
});
