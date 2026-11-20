// Issue #4505: a provider-bound advice pass never lets the gateway key out through its returned text,
// its usage snapshots, a thrown value or its own log lines, even when the worker log has not
// registered the key. Vendor passes are unchanged. Keys are assembled at runtime from fragments.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";

import { runReadOnlyModelPass, redactingLogger, type ReadOnlyModelPassOpts } from "../src/model-pass.js";
import { SummaryRunner } from "../src/summary-runner.js";
import { errorClass } from "../src/summary-runner.js";
import { ReviewRunner } from "../src/review-runner.js";
import { buildDeliveryContext, type DeliveryContext } from "../src/pr-description-context.js";
import { makeTextRedactor } from "../src/redact.js";
import type { AdviceUsageSnapshot } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { Logger } from "../src/log.js";
import type { GitCache } from "../src/git.js";
import type { WorkerClient } from "../src/client.js";
import type { ClaimProviderBinding, ClaimResponse, OutgoingMessage, StateRequest, TaskReviewRequest } from "../src/protocol.js";
import { testOwnedPassHomeRoot } from "./teardown-fixtures.js";

const HOME_ROOT = testOwnedPassHomeRoot();
const REDACTED = "***REDACTED***";

// An identifier-shaped key (valid as an Error name) and one carrying a quote and a backslash.
const ID_KEY = ["gwk", "Zq8", "Lm3Xr", "T7vNp2", "Hd9"].join("");
const ESC_KEY = ["gwk", 'Q"z', "\\", "Lm3Xr", "T7v", "Hd9Kp"].join("");
const NUM_KEY = ["1234", "5678", "9012", "3456"].join("");
const KEYS = [ID_KEY, ESC_KEY] as const;

const forms = (key: string): string[] => [key, JSON.stringify(key).slice(1, -1)];
/** Whether any form of `key` occurs in the string or in the JSON serialization of the value. */
function carries(value: unknown, key: string): boolean {
  const flat = typeof value === "string" ? value : JSON.stringify(value) ?? "";
  return forms(key).some((f) => flat.includes(f));
}

function binding(key: string): ClaimProviderBinding {
  return {
    provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
    auth_style: "bearer", credential: key, model_map: { sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } },
  };
}

interface Line { level: string; message: string; fields?: Record<string, unknown> }
/** A capture logger that has NOT registered any key: it records exactly what it is handed. */
function capture(): { log: Logger; lines: Line[]; secretCalls: string[] } {
  const lines: Line[] = [];
  const secretCalls: string[] = [];
  const mk = (level: string) => (message: string, fields?: Record<string, unknown>): void => void lines.push({ level, message, fields });
  const log: Logger = {
    debug: mk("debug"), info: mk("info"), warn: mk("warn"), error: mk("error"),
    addSecret: (s) => void secretCalls.push(`add:${s}`),
    removeSecret: (s) => void secretCalls.push(`remove:${s}`),
    child: () => log,
  };
  return { log, lines, secretCalls };
}

const assistant = (text: string, extra: Record<string, unknown> = {}, usage?: Record<string, unknown>) => ({
  type: "assistant",
  message: { id: "m1", model: "corp-haiku", role: "assistant", content: [{ type: "text", text }], ...(usage ? { usage } : {}), ...extra },
});
const OK = { type: "result", subtype: "success", is_error: false };
const frames = (...all: unknown[]): SdkQueryFn => (async function* () { for (const f of all) yield f; }) as unknown as SdkQueryFn;
// eslint-disable-next-line require-yield
const throwing = (value: unknown): SdkQueryFn => (async function* () { throw value; }) as unknown as SdkQueryFn;

function passOpts(over: Partial<ReadOnlyModelPassOpts> & { queryFn: SdkQueryFn }): ReadOnlyModelPassOpts {
  return {
    systemPrompt: "s", prompt: "p", homeRoot: HOME_ROOT, homePrefix: "uzi-summary-", label: "summary",
    timeoutMs: 5_000, graceMs: 50, denyReason: "r", model: "haiku", log: capture().log, ...over,
  };
}

async function caught(run: () => Promise<unknown>): Promise<unknown> {
  try {
    await run();
  } catch (e) {
    return e;
  }
  assert.fail("expected the pass to throw");
}

// ---------------------------------------------------------------------------------------------
// Returned text
// ---------------------------------------------------------------------------------------------
for (const key of KEYS) {
  describe(`provider pass output (${key === ID_KEY ? "identifier" : "quote and backslash"} key)`, () => {
    const provider = binding(key);

    it("redacts the returned text, raw and JSON-escaped; a vendor pass returns it unchanged", async () => {
      const text = `echo ${key} and ${JSON.stringify(key).slice(1, -1)}`;
      const out = await runReadOnlyModelPass(passOpts({ provider, queryFn: frames(assistant(text), OK) }));
      assert.equal(out, `echo ${REDACTED} and ${REDACTED}`);
      const vendor = await runReadOnlyModelPass(passOpts({ token: "tok", queryFn: frames(assistant(text), OK) }));
      assert.equal(vendor, text);
    });

    it("never registers or evicts a secret on the caller's logger", async () => {
      const { log, secretCalls } = capture();
      await runReadOnlyModelPass(passOpts({ provider, log, queryFn: frames(assistant("x"), OK) }));
      assert.deepEqual(secretCalls, []);
    });

    // -------------------------------------------------------------------------------------
    // Usage snapshots
    // -------------------------------------------------------------------------------------
    it("(g1) redacts model ids in the delivered snapshot and sums ids that collide", async () => {
      const calls: AdviceUsageSnapshot[] = [];
      const entry = (i: number, o: number, r: number, c: number) => ({ inputTokens: i, outputTokens: o, cacheReadInputTokens: r, cacheCreationInputTokens: c });
      const modelUsage = {
        [key]: entry(10, 2, 1, 0),
        [REDACTED]: entry(5, 1, 0, 4),
        [`gw/${key}`]: entry(1, 1, 1, 1),
        "corp-clean": entry(2, 2, 2, 2),
      };
      await runReadOnlyModelPass(passOpts({
        provider, onUsage: (u) => void calls.push(u),
        queryFn: frames(assistant("ok"), { ...OK, modelUsage }),
      }));
      assert.equal(calls.length, 1, "delivered exactly once");
      assert.equal(carries(calls[0], key), false);
      assert.deepEqual(calls[0], {
        [REDACTED]: entry(15, 3, 1, 4),
        [`gw/${REDACTED}`]: entry(1, 1, 1, 1),
        "corp-clean": entry(2, 2, 2, 2),
      });
      // The vendor control keeps the key-bearing id as observed.
      const vendor: AdviceUsageSnapshot[] = [];
      await runReadOnlyModelPass(passOpts({
        token: "tok", onUsage: (u) => void vendor.push(u), queryFn: frames(assistant("ok"), { ...OK, modelUsage }),
      }));
      assert.ok(key in vendor[0]!, "the vendor snapshot is untouched");
    });

    it("(g2) redacts tier, speed and geo markers kept from assistant usage, keeping the token totals", async () => {
      const usage = { input_tokens: 7, output_tokens: 3, service_tier: `tier-${key}`, speed: `fast-${key}`, inference_geo: `geo-${key}` };
      const calls: AdviceUsageSnapshot[] = [];
      await runReadOnlyModelPass(passOpts({
        provider, onUsage: (u) => void calls.push(u), queryFn: frames(assistant("ok", {}, usage), OK),
      }));
      assert.equal(calls.length, 1);
      assert.deepEqual(calls[0], {
        "corp-haiku": {
          inputTokens: 7, outputTokens: 3, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
          service_tier: `tier-${REDACTED}`, speed: `fast-${REDACTED}`, inference_geo: `geo-${REDACTED}`,
        },
      });
      // Positive observation: a vendor pass over the same frames carries the markers verbatim.
      const vendor: AdviceUsageSnapshot[] = [];
      await runReadOnlyModelPass(passOpts({
        token: "tok", onUsage: (u) => void vendor.push(u), queryFn: frames(assistant("ok", {}, usage), OK),
      }));
      assert.equal(vendor[0]!["corp-haiku"]!.service_tier, `tier-${key}`);
      assert.equal(vendor[0]!["corp-haiku"]!.speed, `fast-${key}`);
      assert.equal(vendor[0]!["corp-haiku"]!.inference_geo, `geo-${key}`);
    });

    // -------------------------------------------------------------------------------------
    // Internal logs
    // -------------------------------------------------------------------------------------
    it("(h) a throwing usage callback is warned about without the key and delivered once", async () => {
      for (const withProvider of [true, false]) {
        const { log, lines } = capture();
        let delivered = 0;
        const out = await runReadOnlyModelPass(passOpts({
          ...(withProvider ? { provider } : { token: "tok" }), log,
          onUsage: () => {
            delivered++;
            throw new Error(`callback failed for ${key}`);
          },
          queryFn: frames(assistant("fine", {}, { input_tokens: 1, output_tokens: 1 }), OK),
        }));
        assert.equal(out, "fine");
        assert.equal(delivered, 1);
        const warn = lines.filter((l) => l.message === "summary usage callback failed");
        assert.equal(warn.length, 1, "the warn is positively captured");
        if (withProvider) {
          assert.equal(carries(lines, key), false);
          assert.equal(warn[0]!.fields?.error, `callback failed for ${REDACTED}`);
        } else {
          assert.equal(warn[0]!.fields?.error, `callback failed for ${key}`, "vendor control: logged as before");
        }
      }
    });

    it("(i) a failed HOME cleanup is warned about without the key in the message or any field", async () => {
      for (const withProvider of [true, false]) {
        const { log, lines } = capture();
        const out = await runReadOnlyModelPass(passOpts({
          ...(withProvider ? { provider } : { token: "tok" }), log,
          homePrefix: `uzi-summary-${key}-`,
          teardownTestDeps: {
            removeTreePinned: async (parent, name) => {
              await fs.rm(path.join(parent, name), { recursive: true, force: true });
              throw Object.assign(new Error(`cannot remove under ${key}`), { path: `/x/${key}` });
            },
          },
          queryFn: frames(assistant("fine"), OK),
        }));
        assert.equal(out, "fine", "cleanup stays best effort");
        const warn = lines.filter((l) => l.message === "summary HOME cleanup failed");
        assert.equal(warn.length, 1, "the warn is positively captured");
        if (withProvider) {
          assert.equal(carries(lines, key), false);
          assert.equal(warn[0]!.fields?.error, `cannot remove under ${REDACTED}`);
          assert.ok(String(warn[0]!.fields?.home_dir).includes(REDACTED), "the home_dir field carried the key and is redacted");
        } else {
          assert.equal(warn[0]!.fields?.error, `cannot remove under ${key}`, "vendor control: logged as before");
          assert.ok(String(warn[0]!.fields?.home_dir).includes(key));
        }
      }
    });
  });
}

describe("provider pass key encodings", () => {
  // A key with a slash, an ampersand, angle brackets and a space.
  const key = ["gwk", "a/b", "&c", "<d>", " e", "Zq8Lm3Xr"].join("");
  const provider = binding(key);
  const json = JSON.stringify(key).slice(1, -1);
  const variants: Record<string, string> = {
    "JSON with escaped slash": json.replaceAll("/", "\\/"),
    "HTML-safe lowercase hex": json.replace(/[<>&]/g, (c) => `\\u00${c.charCodeAt(0).toString(16)}`),
    "HTML-safe uppercase hex": json.replace(/[<>&]/g, (c) => `\\u00${c.charCodeAt(0).toString(16).toUpperCase()}`),
    "percent-encoded": encodeURIComponent(key),
    "percent-encoded lowercase hex": encodeURIComponent(key).replace(/%[0-9A-F]{2}/g, (m) => m.toLowerCase()),
    "percent-encoded with plus for space": encodeURIComponent(key).replaceAll("%20", "+"),
  };
  for (const [name, variant] of Object.entries(variants)) {
    it(`redacts the ${name} form completely`, async () => {
      assert.notEqual(variant, key, "precondition: the variant differs from the raw key");
      const echoed = `gateway said [${variant}] end`;
      const stub = frames(assistant(echoed), OK);
      assert.ok(echoed.includes(variant), "the variant is present in the stub output");
      const out = await runReadOnlyModelPass(passOpts({ provider, queryFn: stub }));
      assert.equal(out, `gateway said [${REDACTED}] end`);
      assert.equal(out.includes(variant), false);
    });
  }

  it("redacts a variant echoed in a thrown error", async () => {
    const variant = variants["JSON with escaped slash"]!;
    const thrown = (await caught(() => runReadOnlyModelPass(passOpts({ provider, queryFn: throwing(new Error(`bad ${variant}`)) })))) as Error;
    assert.equal(thrown.message, `bad ${REDACTED}`);
  });
});

it("provider usage preserves metadata when redacted model ids collide", async () => {
  const calls: AdviceUsageSnapshot[] = [];
  await runReadOnlyModelPass(passOpts({
    provider: binding(ID_KEY), onUsage: usage => { calls.push(usage); },
    queryFn: frames(
      assistant("first", { id: "metadata-first", model: ID_KEY }, { input_tokens: 10, output_tokens: 2, service_tier: "first-tier" }),
      assistant("second", { id: "metadata-second", model: REDACTED }, { input_tokens: 5, output_tokens: 3, service_tier: "second-tier" }),
      OK,
    ),
  }));
  assert.equal(calls.length, 1);
  assert.equal(carries(calls[0], ID_KEY), false);
  const merged = calls[0]![REDACTED] as unknown as Record<string, unknown>;
  assert.equal(merged.inputTokens, 15);
  assert.equal(merged.outputTokens, 5);
  assert.equal(merged.service_tier, "first-tier");
  assert.equal(merged["service_tier#2"], "second-tier");
});

describe("redactingLogger", () => {
  it("scrubs the message and nested fields, including through child()", () => {
    const key = ID_KEY;
    const sink: Line[] = [];
    const base: Logger = {
      debug: () => {}, info: () => {}, error: () => {},
      warn: (message, fields) => void sink.push({ level: "warn", message, fields }),
      addSecret: () => {}, removeSecret: () => {},
      child: (fields) => {
        const inner: Logger = { ...base, warn: (message, f) => void sink.push({ level: "warn", message, fields: { ...fields, ...f } }), child: () => inner };
        return inner;
      },
    };
    const log = redactingLogger(base, makeTextRedactor([key]));
    log.child({ ctx: `c-${key}` }).warn(`oops ${key}`, { nested: { k: key, list: [`l-${key}`] } });
    assert.equal(sink.length, 1);
    assert.equal(carries(sink, key), false);
    assert.equal(sink[0]!.message, `oops ${REDACTED}`);
    assert.deepEqual(sink[0]!.fields, { ctx: `c-${REDACTED}`, nested: { k: REDACTED, list: [`l-${REDACTED}`] } });
  });
});

// ---------------------------------------------------------------------------------------------
// Thrown values: redacted, with the caller-visible classification kept
// ---------------------------------------------------------------------------------------------
describe("provider pass thrown values", () => {
  const key = ID_KEY;
  const provider = binding(key);
  const run = (value: unknown, p: ClaimProviderBinding = provider) => caught(() => runReadOnlyModelPass(passOpts({ provider: p, queryFn: throwing(value) })));

  it("rewrites a writable Error in place and rethrows the same object", async () => {
    const err = Object.assign(new RangeError(`boom ${key}`), { reason: `reason ${key}`, code: 42 });
    void err.stack; // materialize the lazily built stack so its redaction is actually exercised
    assert.equal(carries(err.stack ?? "", key), true, "precondition: the stack carries the key");
    const thrown = await run(err);
    assert.equal(thrown, err);
    assert.ok(thrown instanceof RangeError);
    const e = thrown as Error & { reason: string; code: number };
    assert.equal(e.message, `boom ${REDACTED}`);
    assert.equal(carries(e.stack ?? "", key), false);
    assert.equal(e.reason, `reason ${REDACTED}`);
    assert.equal(e.code, 42);
    assert.equal(errorClass(e), "RangeError");
  });

  it("redacts a key-bearing name, which classifies as the generic Error", async () => {
    const err = new Error("boom");
    err.name = key;
    assert.equal(errorClass(err), key, "precondition: the key is a trusted-looking class before the pass");
    const thrown = (await run(err)) as Error;
    assert.equal(carries(thrown.name, key), false);
    assert.equal(errorClass(thrown), "Error");
  });

  it("leaves a key-free Error untouched, name included", async () => {
    const err = new TypeError("no secret here");
    const thrown = await run(err);
    assert.equal(thrown, err);
    assert.equal(errorClass(thrown), "TypeError");
  });

  it("replaces a frozen TypeError with a redacted TypeError that keeps its class", async () => {
    const err = Object.freeze(Object.assign(new TypeError(`frozen ${key}`), { reason: `r ${key}` }));
    const thrown = (await run(err)) as TypeError & { reason: string };
    assert.notEqual(thrown, err);
    assert.ok(thrown instanceof TypeError);
    assert.equal(errorClass(thrown), "TypeError");
    assert.equal(thrown.message, `frozen ${REDACTED}`);
    assert.equal(thrown.reason, `r ${REDACTED}`);
    assert.equal(carries(thrown.stack ?? "", key), false);
  });

  it("replaces an Error whose message is an accessor without a setter", async () => {
    class GetterError extends Error {
      override get message(): string { return `getter ${key}`; }
    }
    const err = new GetterError();
    const thrown = (await run(err)) as Error;
    assert.notEqual(thrown, err);
    assert.ok(thrown instanceof GetterError);
    assert.ok(thrown instanceof Error);
    assert.equal(thrown.message, `getter ${REDACTED}`);
    assert.equal(carries(thrown.stack ?? "", key), false);
  });

  it("(key-free controls) rethrows non-Error values of every type with the same identity", async () => {
    const values: unknown[] = ["plain", 42, 5n, Symbol("plain"), () => "plain", { message: "plain" }, null];
    for (const v of values) assert.equal(await run(v), v, `${typeof v} keeps its identity`);
  });

  it("substitutes a key-bearing string, number, bigint, symbol, function and object by type", async () => {
    const str = await run(`s ${key}`);
    assert.equal(str, `s ${REDACTED}`);

    const numKey = binding(NUM_KEY);
    const num = await run(Number(NUM_KEY), numKey);
    assert.equal(typeof num, "number");
    assert.ok(Number.isNaN(num as number));
    const big = await run(BigInt(NUM_KEY), numKey);
    assert.equal(big, 0n);

    const sym = (await run(Symbol(`sym ${key}`))) as symbol;
    assert.equal(typeof sym, "symbol");
    assert.equal(sym.description, `sym ${REDACTED}`);

    const fn = Object.assign(function () {}, { toString: () => `source ${key}` });
    Object.defineProperty(fn, "name", { value: `fn${key}` });
    const subFn = (await run(fn)) as (() => void) & { name: string };
    assert.equal(typeof subFn, "function");
    assert.equal(carries(subFn.name, key), false);
    assert.equal(carries(String(subFn), key), false);
    assert.equal(String(subFn), `source ${REDACTED}`);

    const obj = { message: `m ${key}`, reason: `r ${key}`, toString: () => `obj ${key}` };
    const subObj = (await run(obj)) as Record<string, unknown>;
    assert.equal(typeof subObj, "object");
    assert.ok(Object.isFrozen(subObj));
    assert.equal(subObj["message"], `m ${REDACTED}`);
    assert.equal(subObj["reason"], `r ${REDACTED}`);
    assert.equal(String(subObj), `obj ${REDACTED}`);
    assert.equal(carries(subObj, key), false);
  });
});

// ---------------------------------------------------------------------------------------------
// Runner-level behaviour through the real SummaryRunner / ReviewRunner
// ---------------------------------------------------------------------------------------------
const base = { token: "", model: "haiku", issueTitle: "t", issueBody: "b", prdText: "p" };

function summaryRunner(log: Logger, queryFn: SdkQueryFn): SummaryRunner {
  return new SummaryRunner(log, { queryFn, homeRoot: HOME_ROOT, modelTimeoutMs: 5_000 });
}

async function deliveryContext(): Promise<DeliveryContext> {
  return buildDeliveryContext({
    git: {
      sizeMergeBase: async () => "b".repeat(40),
      diffNumstatZ: async () => "1\t0\tsrc/x.ts\0",
      checkAttrZ: async (_b, _h, paths) => new Map(paths.map((p) => [p, {}])),
      readBare: async (_b, args) => ({ text: args[0] === "log" ? "feat: add x\0" : "diff --git a/src/x.ts b/src/x.ts\n+x\n", truncated: false }),
    },
    barePath: "/bare",
    targetBranch: "main",
    headSha: "a".repeat(40),
    deadlineMs: Date.now() + 60_000,
    redact: makeTextRedactor([]),
    issueTitle: "Add x",
    issueBody: "Please add x.",
    prSummary: { what: "Adds x" },
  });
}

const deliveryClaim = (key: string) => ({ run_id: "run-1", summary_model: "haiku", secrets: { provider: binding(key) } });

for (const key of KEYS) {
  describe(`SummaryRunner provider passes (${key === ID_KEY ? "identifier" : "quote and backslash"} key)`, () => {
    const provider = binding(key);

    it("(c) a delivery summary that echoes the key comes back key-free", async () => {
      const echoed = { summary: `Adds ${key}.`, changes: [`changed ${JSON.stringify(key).slice(1, -1)}`], scope_notes: [], review_pointers: [`see ${key}`] };
      const { log } = capture();
      const out = await summaryRunner(log, frames(assistant(JSON.stringify(echoed)), OK)).generateDeliverySummary({
        claim: deliveryClaim(key), context: await deliveryContext(), deadlineMs: Date.now() + 60_000,
      });
      assert.ok(out, "the summary parsed");
      assert.equal(carries(out, key), false);
      assert.ok(out.summary.includes(REDACTED), "the echoed key was observed and redacted");
    });

    it("(d) intent and plan summaries that echo the key come back key-free", async () => {
      const { log } = capture();
      const intent = await summaryRunner(log, frames(assistant(`Implements ${key} support.`), OK)).generateIntentSummary({ ...base, provider });
      assert.ok(intent);
      assert.equal(carries(intent, key), false);
      assert.ok(intent.includes(REDACTED));
      const planText = JSON.stringify({ summary: `Plan for ${key}`, deltas: [{ kind: "added", text: `adds ${key}` }] });
      const plan = await summaryRunner(log, frames(assistant(planText), OK)).generatePlanSummary({ ...base, provider, planMd: "# plan" });
      assert.ok(plan);
      assert.equal(carries(plan, key), false);
      assert.ok(plan.summary.includes(REDACTED));
    });

    it("(e) a key-bearing thrown Error returns null and the generation-failed log is key-free", async () => {
      const { log, lines } = capture();
      const runner = summaryRunner(log, throwing(new Error(`gateway rejected ${key}`)));
      assert.equal(await runner.generateIntentSummary({ ...base, provider }), null);
      assert.equal(await runner.generatePlanSummary({ ...base, provider, planMd: "# plan" }), null);
      for (const message of ["intent summary generation failed", "plan summary generation failed"]) {
        const warn = lines.filter((l) => l.message === message);
        assert.equal(warn.length, 1, `${message} was captured`);
        assert.equal(warn[0]!.fields?.error, `gateway rejected ${REDACTED}`);
      }
      assert.equal(carries(lines, key), false);
    });
  });
}

describe("SummaryRunner delivery error class", () => {
  it("(f) an Error named after the key is skipped with a key-free error_class", async () => {
    const err = new Error("boom");
    err.name = ID_KEY;
    const { log, lines } = capture();
    const out = await summaryRunner(log, throwing(err)).generateDeliverySummary({
      claim: deliveryClaim(ID_KEY), context: await deliveryContext(), deadlineMs: Date.now() + 60_000,
    });
    assert.equal(out, null);
    const skipped = lines.filter((l) => l.message === "delivery summary skipped");
    assert.equal(skipped.length, 1);
    assert.equal(skipped[0]!.fields?.reason, "the model pass failed");
    assert.equal(skipped[0]!.fields?.error_class, "Error");
    assert.equal(carries(lines, ID_KEY), false);
  });

  it("control: a key-free delivery throw keeps its class", async () => {
    const { log, lines } = capture();
    const out = await summaryRunner(log, throwing(new RangeError("boom"))).generateDeliverySummary({
      claim: deliveryClaim(ID_KEY), context: await deliveryContext(), deadlineMs: Date.now() + 60_000,
    });
    assert.equal(out, null);
    assert.equal(lines.find((l) => l.message === "delivery summary skipped")?.fields?.error_class, "RangeError");
  });
});

// A fake worker client and git layer, enough for one review claim.
function fakeClient() {
  const calls: { review?: TaskReviewRequest; usage: OutgoingMessage[][]; states: StateRequest[] } = { usage: [], states: [] };
  const client = {
    reportState: async (_id: string, body: StateRequest) => {
      calls.states.push(body);
      return { applied: true, status: body.status };
    },
    postMessages: async (_id: string, messages: OutgoingMessage[]) => void calls.usage.push(messages),
    postTaskReview: async (_id: string, review: TaskReviewRequest) => void (calls.review = review),
  } as unknown as WorkerClient;
  return { client, calls };
}

const fakeGit = {
  ensureClone: async () => "/bare/repo.git",
  reviewDiff: async () => "diff --git a/poller.ts b/poller.ts\n@@ -1 +1 @@\n-old\n+new\n",
  removeRunnerClone: async () => {},
  defaultBranchName: async () => "main",
} as unknown as GitCache;

function reviewClaim(key: string): ClaimResponse {
  return {
    run_id: "review-1", kind: "task", review_target_run_id: "target-1", issue_iid: null, issue_title: "Review: tidy the poller",
    issue_description: "", branch: "uzi/task/target-1", base_branch: "develop",
    repo: { id: "r1", url: "u", clone_url: "/origin", default_branch: "main" },
    secrets: { forge_pat: "pat", anthropic_oauth_token: "", provider: binding(key) },
    last_seq: 0, agents: [],
  } as unknown as ClaimResponse;
}

for (const key of KEYS) {
  describe(`ReviewRunner provider review (${key === ID_KEY ? "identifier" : "quote and backslash"} key)`, () => {
    it("(a) posts a completed review whose summary and findings are key-free", async () => {
      const text = JSON.stringify({
        summary: `Echoed ${key}`,
        findings: [{ file: "poller.ts", symbol: `s${key}`, line: 3, severity: "error", summary: `leaks ${key}`, rationale: `because ${key}` }],
      });
      const { client, calls } = fakeClient();
      const { log } = capture();
      await new ReviewRunner(client, fakeGit, log, { homeRoot: HOME_ROOT, queryFn: frames(assistant(text), OK) }).execute(reviewClaim(key));
      assert.equal(calls.review?.status, "complete", "a provider claim with no Anthropic token posts a completed review");
      assert.equal(carries(calls.review, key), false);
      assert.ok(calls.review!.summary.includes(REDACTED));
      assert.equal(calls.review!.findings.length, 1);
    });

    it("(b) a model throw carrying the key posts a key-free fallback summary", async () => {
      const { client, calls } = fakeClient();
      const { log, lines } = capture();
      await new ReviewRunner(client, fakeGit, log, { homeRoot: HOME_ROOT, queryFn: throwing(new Error(`gateway said ${key}`)) }).execute(reviewClaim(key));
      assert.equal(calls.review?.status, "failed");
      assert.ok(calls.review!.summary.includes(`gateway said ${REDACTED}`));
      assert.equal(carries(calls.review, key), false);
      assert.equal(lines.filter((l) => l.message === "review model call failed; posting failed review").length, 1);
      assert.equal(carries(lines, key), false);
    });

    it("(g1) the posted usage has redacted ids with the collided totals summed", async () => {
      const entry = (i: number, o: number) => ({ inputTokens: i, outputTokens: o, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 });
      const modelUsage = { [key]: entry(10, 2), [REDACTED]: entry(5, 1) };
      const { client, calls } = fakeClient();
      const { log } = capture();
      const text = JSON.stringify({ summary: "fine", findings: [] });
      await new ReviewRunner(client, fakeGit, log, { homeRoot: HOME_ROOT, queryFn: frames(assistant(text), { ...OK, modelUsage }) }).execute(reviewClaim(key));
      assert.equal(calls.review?.status, "complete");
      assert.equal(calls.usage.length, 1);
      assert.equal(carries(calls.usage, key), false);
      assert.deepEqual(calls.usage[0]![0]!.payload["modelUsage"], { [REDACTED]: entry(15, 3) });
    });
  });
}

describe("ReviewRunner provider claim without an Anthropic token", () => {
  it("posts a completed review for a key-free reply", async () => {
    const { client, calls } = fakeClient();
    const text = JSON.stringify({ summary: "All good.", findings: [] });
    await new ReviewRunner(client, fakeGit, capture().log, { homeRoot: HOME_ROOT, queryFn: frames(assistant(text), OK) }).execute(reviewClaim(ID_KEY));
    assert.deepEqual(calls.review, { status: "complete", summary: "All good.", findings: [] });
  });
});
