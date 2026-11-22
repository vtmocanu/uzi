// Independent registered-redactor proof for native Codex; no provider claim executes.
import { it } from "node:test";
import assert from "node:assert/strict";
import { ModelPassCredentialError, runReadOnlyModelPass, type ReadOnlyModelPassOpts } from "../src/model-pass.js";
import { LimitReachedError } from "../src/limit.js";
import { createLogger } from "../src/log.js";
import { providerKeyForms } from "../src/redact.js";
import type { AdviceRequest, AdviceUsageSnapshot } from "../src/harness.js";

const key = ["gateway", 'Q"z', "\\", "canary"].join("");
const safe = "***REDACTED***";
function opts(run: (request: AdviceRequest) => Promise<unknown>, registered = true): ReadOnlyModelPassOpts {
  const log = createLogger("error");
  if (registered) for (const form of providerKeyForms(key)) log.addSecret(form);
  return {
    systemPrompt: "s", prompt: "p", homeRoot: ".", homePrefix: "unused", label: "summary",
    timeoutMs: 5000, log, denyReason: "deny", queryFn: (() => { throw new Error("unused"); }) as never,
    codex: { runId: "native", binding: { authMode: "api_key", accessToken: "native", capability: "cap" } as never,
      buildHarness: (async () => ({ kind: "codex", run })) as never },
  };
}
it("native registered redactor scrubs returned JSON before parsing and merges usage without stripping pricing", async () => {
  let usage: AdviceUsageSnapshot | undefined;
  const entry = (n: number) => ({ inputTokens: n, outputTokens: n, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: n });
  const options = opts(async request => {
    request.usageObserver?.({ [key]: entry(2), [safe]: entry(3) });
    return { text: JSON.stringify({ summary: key }), end: { kind: "terminal", terminal: { outcome: "success" } } };
  });
  options.onUsage = u => { usage = u; };
  assert.deepEqual(JSON.parse(await runReadOnlyModelPass(options)), { summary: safe });
  assert.deepEqual(usage, { [safe]: entry(5) });
});
it("native registered redactor sanitizes nested causes while preserving error class", async () => {
  const err = new TypeError("clean", { cause: { [key]: key } });
  let caught: unknown;
  try { await runReadOnlyModelPass(opts(async () => { throw err; })); } catch (e) { caught = e; }
  assert.ok(caught instanceof TypeError);
  assert.deepEqual(caught.cause, { [safe]: safe });
});
it("native registered redactor scrubs hidden and inherited non-Error messages", async () => {
  for (const form of providerKeyForms(key)) {
    for (const diagnostic of [
      Object.defineProperty({}, "message", { value: form }),
      Object.create(Object.defineProperty({}, "message", { value: form })),
    ]) {
      await assert.rejects(runReadOnlyModelPass(opts(async () => { throw diagnostic; })), caught => {
        assert.equal((caught as { message: string }).message, safe);
        return true;
      });
    }
  }
});
it("native no-matching-key errors retain identity", async () => {
  const err = new RangeError("clean");
  await assert.rejects(runReadOnlyModelPass(opts(async () => { throw err; }, false)), e => e === err);
});

it("native refresh classification precedes scrubbing and keeps the typed reason and cause", async () => {
  const options = opts(async () => { throw new Error("unused"); });
  options.codex = { ...options.codex!, buildHarness: (async (params: { onRefreshDenied(): void }) => {
    params.onRefreshDenied();
    return { kind: "codex", run: async () => { throw new Error(key, { cause: { nested: key } }); } };
  }) as never };
  await assert.rejects(runReadOnlyModelPass(options), e => {
    assert.ok(e instanceof ModelPassCredentialError);
    assert.equal(e.reason, "refresh_denied");
    assert.equal(e.message, safe);
    assert.ok(e.cause instanceof Error);
    assert.deepEqual(e.cause.cause, { nested: safe });
    return true;
  });
});
it("native callback warnings pass sanitized diagnostics to the logger", async () => {
  const options = opts(async request => {
    request.usageObserver?.({ clean: { inputTokens: 1, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } });
    return { text: "ok" };
  });
  let warning: unknown;
  options.log.warn = (message, fields) => { warning = { message, fields }; };
  options.onUsage = () => { throw new Error(key); };
  assert.equal(await runReadOnlyModelPass(options), "ok");
  assert.deepEqual(warning, { message: "summary usage callback failed", fields: { error: safe } });
});

it("native registered forms are scrubbed independently", async () => {
  for (const form of providerKeyForms(key)) {
    assert.equal(await runReadOnlyModelPass(opts(async () => ({ text: `prefix ${form} suffix` }))), `prefix ${safe} suffix`);
  }
});
it("native typed limits keep structured fields after scrubbing", async () => {
  const err = new LimitReachedError({ resetsAtMs: 12345, rateLimitType: "five_hour", detail: key });
  await assert.rejects(runReadOnlyModelPass(opts(async () => { throw err; })), e => {
    assert.ok(e instanceof LimitReachedError);
    assert.equal(e.resetsAtMs, 12345);
    assert.equal(e.rateLimitType, "five_hour");
    assert.equal(e.message, `usage limit reached: ${safe}`);
    return true;
  });
});
it("native nested arrays do not multiply diagnostic accessor reads", async () => {
  let reads = 0;
  let nested: unknown = Object.defineProperty({}, "value", {
    enumerable: true, get() { reads++; return key; },
  });
  for (let depth = 0; depth < 8; depth++) nested = [nested];
  const err = new Error("clean", { cause: nested });
  await assert.rejects(runReadOnlyModelPass(opts(async () => { throw err; })), caught => {
    assert.ok(caught instanceof Error);
    assert.ok(reads <= 2, `nested arrays must not amplify reads: ${reads}`);
    for (const form of providerKeyForms(key)) assert.equal(JSON.stringify(caught.cause).includes(form), false);
    return true;
  });
});
it("native diagnostic member restoration keeps redactor work proportional to fields", async () => {
  const fields = 96;
  const cause = Object.fromEntries(Array.from({ length: fields }, (_, index) => [`field-${index}`, index]));
  const options = opts(async () => { throw new Error(key, { cause }); });
  const scrub = options.log.redactDiagnostic!;
  let calls = 0;
  options.log.redactDiagnostic = function (text) { calls++; return scrub.call(this, text); };
  await assert.rejects(runReadOnlyModelPass(options), caught => {
    assert.ok(caught instanceof Error);
    assert.deepEqual(caught.cause, cause);
    assert.equal(caught.message, safe);
    assert.ok(calls < fields * 20, `diagnostic restoration must not amplify redaction work: ${calls}`);
    return true;
  });
});
it("native logger without an optional scrubber preserves existing text and usage", async () => {
  const snapshot = { clean: { inputTokens: 2, outputTokens: 3, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.25 } };
  const options = opts(async request => {
    request.usageObserver?.(snapshot);
    return { text: key };
  }, false);
  options.log = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child() { return this; } };
  let usage: AdviceUsageSnapshot | undefined;
  options.onUsage = u => { usage = u; };
  assert.equal(await runReadOnlyModelPass(options), key);
  assert.equal(usage, snapshot);
});
