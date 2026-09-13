// PRD #1287 C4 (D7 layer U) — Codex advice-ceiling conformance, driving the REAL
// CodexAdviceHarness (render.ts renderCodexAdvice runtime ceiling + disposeOnce cleanup + the
// api_key/subscription appserver-auth lane) with an in-memory transport and a fake
// LaunchAdviceRootSeam. There is NO broker/registry/run-workspace in this lane — the tool-less
// ceiling by construction (D5). Each case pairs a POSITIVE control (a clean pass / a detectable
// refresh) with an independent NEGATIVE-effect oracle (nothing launched / dispose-count===1 /
// zero refresh on the wire / an error response with no token), never denial text alone (D4).
//
// Distinct angle from agent/test/codex-advice-harness.test.ts: that file covers the `agents`
// ceiling key and a slow-LAUNCH late dispose. Here we cover the OTHER three ceiling keys
// (tools/toolServers/cwd) at the render level AND end-to-end, an abort-DURING-in-flight-dispose
// race, and the api_key advice lane (the existing advice tests use subscription).

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  loadAdviceModules,
  makeAdviceHarness,
  makeAdviceRequest,
  noThrowPolicy,
  FakeTransport,
  authResponder,
  threadStarted,
  agentMessage,
  turnCompleted,
  refreshRequest,
  type AdviceModules,
} from "./harness-advice.js";
import { defer } from "./harness-lifecycle.js";
import { recordEvidence } from "./evidence.js";
import type { AdviceRequest } from "../../agent/src/harness.js";
import type { CodexAdviceHarnessOptions } from "../../agent/src/codex/codex-advice-harness.js";
import {
  CODEX_U_ADVICE_CEILING_TITLE,
  CODEX_U_ADVICE_DISPOSE_RACE_TITLE,
  CODEX_U_ADVICE_APIKEY_ZERO_REFRESH_TITLE,
  CODEX_U_ADVICE_APIKEY_NO_FALLBACK_TITLE,
} from "./titles-c4.js";

let mods: AdviceModules;
before(async () => {
  mods = await loadAdviceModules();
});

describe("codex U advice ceiling (real CodexAdviceHarness)", () => {
  it(CODEX_U_ADVICE_CEILING_TITLE, async () => {
    const { renderCodexAdvice } = mods.render;

    // (a) renderCodexAdvice ENFORCES the ceiling at runtime: a request carrying ANY run-tool
    // surface throws. The existing unit test covers `agents`; cover the other three keys here.
    for (const key of ["tools", "toolServers", "cwd"] as const) {
      const bad = { ...makeAdviceRequest(), [key]: {} } as unknown as AdviceRequest;
      assert.throws(() => renderCodexAdvice(bad), /advice ceiling/, `renderCodexAdvice rejects "${key}"`);
    }

    // (b) the harness enforces the SAME ceiling end-to-end: a cwd-carrying request rejects and
    // NOTHING is launched, so no isolated root (and thus no shell/file/network/delegation/
    // credential surface) is ever created.
    const bits = makeAdviceHarness(mods);
    const withCwd = { ...makeAdviceRequest(), cwd: "/isolated/advice/work" } as unknown as AdviceRequest;
    await assert.rejects(bits.harness.run(withCwd, noThrowPolicy), /advice ceiling/);
    assert.equal(bits.launchSpecs.length, 0, "a ceiling-violating request launches no isolated root");

    // (c) type-level: the advice options expose NO run-tool authority (no handler registry /
    // registry / run workspace / cwd / delegate). Compile-time guardrail distinct from the
    // existing broker/agents/registry/workspace/tools check.
    type NoHandlers = "toolHandlers" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoRegistry = "registry" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoWorktree = "worktreePath" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoCwd = "cwd" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoDelegate = "delegate" extends keyof CodexAdviceHarnessOptions ? never : true;
    const checks: [NoHandlers, NoRegistry, NoWorktree, NoCwd, NoDelegate] = [true, true, true, true, true];
    assert.deepEqual(checks, [true, true, true, true, true]);

    // POSITIVE control: a clean advice pass (no forbidden keys) returns its accumulated text —
    // the ceiling does not break legitimate advice.
    const ok = makeAdviceHarness(mods);
    ok.transport
      .push(threadStarted())
      .push(agentMessage("hello "))
      .push(agentMessage("world"))
      .push(turnCompleted("completed"))
      .end();
    const result = await ok.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(result.text, "hello world");
    assert.equal(ok.disposeCalls(), 1);

    recordEvidence(CODEX_U_ADVICE_CEILING_TITLE, "pass");
  });

  it(CODEX_U_ADVICE_DISPOSE_RACE_TITLE, async () => {
    // POSITIVE control: a clean pass disposes the isolated HOME exactly once.
    const clean = makeAdviceHarness(mods);
    clean.transport.push(threadStarted()).push(agentMessage("ok")).push(turnCompleted("completed")).end();
    const cleanResult = await clean.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(cleanResult.text, "ok");
    assert.equal(clean.disposeCalls(), 1, "a clean pass disposes the HOME exactly once");

    // NEGATIVE oracle: an external abort fired WHILE the disposer is already in flight must not
    // cause a SECOND disposal. disposeOnce collapses the finally + late-work owners to EXACTLY one.
    let markDisposeStarted!: () => void;
    const disposeStarted = new Promise<void>((r) => { markDisposeStarted = r; });
    const gate = defer<void>();
    const controller = new AbortController();
    const bits = makeAdviceHarness(mods, {
      dispose: async () => {
        markDisposeStarted();
        await gate.promise; // hold disposal in flight
      },
    });
    bits.transport.push(threadStarted()).push(agentMessage("kept")).push(turnCompleted("completed")).end();

    const run = bits.harness.run(makeAdviceRequest({ signal: controller.signal }), noThrowPolicy);
    await disposeStarted; // disposal is now in flight (gated)
    assert.equal(bits.disposeCalls(), 1, "the disposer started exactly once");
    controller.abort(); // cancellation WHILE disposal is already in flight
    await new Promise((r) => setImmediate(r));
    gate.resolve(); // let the gated disposer finish
    const result = await run;
    assert.equal(result.text, "kept", "the primary result stands");
    assert.equal(bits.disposeCalls(), 1, "the abort during in-flight disposal did NOT cause a second dispose");

    recordEvidence(CODEX_U_ADVICE_DISPOSE_RACE_TITLE, "pass");
  });

  it(CODEX_U_ADVICE_APIKEY_ZERO_REFRESH_TITLE, async () => {
    // POSITIVE control (the refresh seam is real and detectable): a SUBSCRIPTION pass whose
    // stream carries a refresh request DOES call the bridge exactly once — proving the spy can
    // detect a refresh, so a later count of 0 is meaningful.
    let bridgeCalls = 0;
    const sub = mods.appServerAuth.createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "init-access", accountId: "acct-1" },
      bridge: {
        refresh: async () => {
          bridgeCalls += 1;
          return { accessToken: "next-access", accountId: "acct-1" };
        },
      },
    });
    const subTransport = new FakeTransport(authResponder);
    const subBits = makeAdviceHarness(mods, { transport: subTransport, appServerAuth: sub });
    subTransport
      .push(threadStarted())
      .push(refreshRequest(44))
      .push(agentMessage("verdict"))
      .push(turnCompleted("completed"))
      .end();
    const subResult = await subBits.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(subResult.text, "verdict");
    assert.equal(bridgeCalls, 1, "the subscription refresh bridge IS called (the spy detects a refresh)");
    assert.ok(
      subTransport.responses.some((r) => {
        const resp = r.response as { result?: { accessToken?: unknown } };
        return resp.result?.accessToken === "next-access";
      }),
      "the subscription refresh delivered a token (positive control reachable)",
    );

    // NEGATIVE oracle: a clean api_key advice pass performs ZERO refresh. The api_key config
    // carries no bridge (structurally no refresh authority), logs in with type apiKey (no
    // subscription fallback), and its semantics (accumulated text + turn-basis usage) are
    // preserved. A synthetic non-secret credential is assembled at runtime.
    const apiKey = ["synthetic", "codex-m4", "apikey", "value"].join("-");
    const api = mods.appServerAuth.createCodexAppServerAuth({ mode: "api_key", apiKey });
    const apiTransport = new FakeTransport(authResponder);
    const apiBits = makeAdviceHarness(mods, { transport: apiTransport, appServerAuth: api });
    apiTransport
      .push(threadStarted())
      .push(agentMessage("clean verdict"))
      .push(turnCompleted("completed", { total_tokens: 5 }))
      .end();
    const apiResult = await apiBits.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(apiResult.text, "clean verdict", "api_key advice semantics are preserved");
    assert.deepEqual(apiResult.usage, { basis: "turn", tokens: {}, wire: { usage: { total_tokens: 5 } } });

    const methods = apiTransport.requests.map((r) => r.method);
    assert.deepEqual(methods, ["initialize", "account/login/start", "thread/start", "turn/start"]);
    const login = apiTransport.requests.find((r) => r.method === "account/login/start");
    assert.equal((login!.params as { type?: unknown }).type, "apiKey", "api_key logs in with type apiKey");
    assert.ok(!methods.includes("account/chatgptAuthTokens/refresh"), "ZERO refresh request on the wire");
    assert.equal(apiTransport.responses.length, 0, "api_key advice mints no refresh response at all");

    recordEvidence(CODEX_U_ADVICE_APIKEY_ZERO_REFRESH_TITLE, "pass");
  });

  it(CODEX_U_ADVICE_APIKEY_NO_FALLBACK_TITLE, async () => {
    // A subscription-refresh server request arriving DURING an api_key advice pass is refused
    // fail-closed: the api_key session responds with an ERROR (no token) and never falls back to
    // re-presenting the api key as a refreshed credential. POSITIVE control: the api_key session
    // authenticates (initialize + login) first, so the refusal is a real deny, not a broken setup.
    const apiKey = ["synthetic", "codex-m4", "nofallback", "value"].join("-");
    const api = mods.appServerAuth.createCodexAppServerAuth({ mode: "api_key", apiKey });
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "turn/start") {
        // Emit the hostile refresh through the installed auth pump after login is established.
        queueMicrotask(() => {
          transport.emitServerRequest(refreshRequest(51));
        });
      }
      return authResponder(method, params);
    });
    const bits = makeAdviceHarness(mods, { transport, appServerAuth: api });
    // No terminal: the run can only end via the fail-closed refusal (which closes the transport).
    transport.push(threadStarted()).push(agentMessage("should-not-return"));

    await assert.rejects(bits.harness.run(makeAdviceRequest({ timeoutMs: 2000 }), noThrowPolicy));

    // POSITIVE control: authentication reached login (the refusal is a real deny).
    const authMethods = transport.requests.map((r) => r.method);
    assert.ok(authMethods.includes("initialize") && authMethods.includes("account/login/start"), "the api_key session authenticated");

    // NEGATIVE oracle: the refresh was answered with an ERROR and NO token was minted anywhere.
    const refreshResp = transport.responses.find((r) => r.requestId === 51);
    assert.ok(refreshResp, "the api_key session responded to the refusal");
    const resp = refreshResp!.response as { error?: { code?: number }; result?: unknown };
    assert.ok(resp.error, "the refresh was refused with an ERROR (fail-closed)");
    assert.equal(resp.result, undefined, "NO token result was minted (no api_key fallback)");
    assert.ok(
      !JSON.stringify(transport.responses).includes("accessToken"),
      "no response anywhere carries a minted access token",
    );
    assert.equal(bits.disposeCalls(), 1, "the isolated HOME is still disposed after the fail-closed refusal");

    recordEvidence(CODEX_U_ADVICE_APIKEY_NO_FALLBACK_TITLE, "pass");
  });
});
