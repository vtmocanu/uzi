import { describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { rootCertificates } from "node:tls";
import { PassThrough, Writable } from "node:stream";
import { CodexCrossCheck, type CrossCheckModelDeps } from "../src/codex/cross-check.js";
import { codexCustomProviderName } from "../src/codex/config.js";
import { captureExtraCa } from "../src/codex/extra-ca.js";
import { ProviderModelUnmappedError } from "../src/provider-binding.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { createLogger, type Logger } from "../src/log.js";
import { providerKeyForms } from "../src/redact.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { GitCache } from "../src/git.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { CodexEffectLaunchSpec, CodexLaunchSpec, CodexRootHandle } from "../src/codex/launcher.js";
import { nullLogger } from "./helpers.js";

// Issue #4574 M2: a provider-bound Codex cross-check (plan and code) on the frozen binding. The gateway
// key is assembled at runtime (no provider-token-shaped literal in source); it is made of characters
// that are also legal in a mapped model id so a test can plant it in a model field.
const KEY = ["gw", "fixture", "key-9f3a1c7e"].join("/");
const FORMS = providerKeyForms(KEY);
const MASK = "***REDACTED***";
const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
await fs.mkdir(scratch, { recursive: true });

const ASTRA = "gpt-6-astra";
const MAP = { default: { model: "gw/default-model" }, [ASTRA]: { model: "gw/astra-target", context_window: 32768 } };

type Map_ = Record<string, { model: string; context_window?: number }>;
const binding = (map: Map_ = MAP, over: Record<string, unknown> = {}) => ({
  provider_id: "gw-1", revision: 2, engine: "codex", protocol: "openai_responses",
  base_url: "https://gw.example.test/v1", auth_style: "bearer", credential: KEY, model_map: map, ...over,
});

function claimOf(stage: "plan" | "code", secrets: Record<string, unknown>, model: string | null = ASTRA): ClaimResponse {
  const base = {
    kind: "cross_check", run_id: "child", claim_generation: 7, last_seq: 0, agents: [], issue_iid: null,
    issue_title: "Issue", issue_description: "Issue DATA",
    repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo",
      clone_url: "https://example.test/repo.git", default_branch: "main" },
    config: { ...(model !== null ? { default_model: model } : {}), default_effort: "high" },
    secrets: { forge_pat: "", ...secrets },
  };
  const cross_check = stage === "code"
    ? { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64),
      base_commit: "a".repeat(40), head_commit: "b".repeat(40), plan_md: "Approved plan DATA",
      guidance_snapshot: "", milestones: [], required_capabilities: [], required_tools: [], size_class: "small",
      code_context: { task: "context DATA" }, code_diff: "diff DATA", deadline_at: "2099-01-01T00:00:00Z" }
    : { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest",
      deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan", milestones: [], required_capabilities: [],
      required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff" };
  return { ...base, cross_check } as unknown as ClaimResponse;
}
const providerClaim = (stage: "plan" | "code" = "plan", map: Map_ = MAP, model: string | null = ASTRA): ClaimResponse =>
  claimOf(stage, { provider: binding(map) }, model);
const vendorClaim = (model = ASTRA): ClaimResponse =>
  claimOf("plan", { codex: { auth_mode: "api_key", access_token: "vendor-claim-token-1234", capability: "vendor-capability-1234" } }, model);

const planText = JSON.stringify({ verdict: "approve", summary: "Anchors checked", items: [] });
const codeText = JSON.stringify({ findings: [] });

type Frame = { id?: number | string; method?: string; params?: Record<string, any> };
type Emit = (frame: unknown) => void;

/** A ref-counting logger fake: the shape of the real registry, observable by the test. */
function countingLogger(): { log: Logger; counts: Map<string, number> } {
  const counts = new Map<string, number>();
  const log = { ...nullLogger(),
    addSecret: (s: string) => { counts.set(s, (counts.get(s) ?? 0) + 1); },
    removeSecret: (s: string) => { const n = counts.get(s); if (n === undefined) return; if (n <= 1) counts.delete(s); else counts.set(s, n - 1); },
  } as unknown as Logger;
  return { log, counts };
}

/** CodexCrossCheck over two injected NDJSON roots (the shape of codex-cross-check.test.ts's rig). */
function rig(claim: ClaimResponse, opts: { log?: Logger; notes?: (emit: Emit) => void; extraCa?: ReturnType<typeof captureExtraCa> } = {}) {
  const requests: Frame[] = [];
  const specs: (CodexLaunchSpec | CodexEffectLaunchSpec)[] = [];
  const usage: Record<string, unknown>[] = [];
  const clientCalls: string[] = [];
  const roots = (onFrame: (frame: Frame, emit: Emit) => void): CodexRootHandle => {
    const stdout = new PassThrough();
    const emit: Emit = (frame) => stdout.write(JSON.stringify(frame) + "\n");
    let buffer = "";
    const stdin = new Writable({ write(chunk, _enc, done) {
      buffer += chunk.toString();
      let nl: number;
      while ((nl = buffer.indexOf("\n")) >= 0) { const line = buffer.slice(0, nl); buffer = buffer.slice(nl + 1); onFrame(JSON.parse(line), emit); }
      done();
    } });
    return { transport: { stdin, stdout, stderr: null }, whenFailed: new Promise<Error>(() => {}), failed: undefined,
      dispose: async () => { stdout.end(); return { clean: true }; } } as unknown as CodexRootHandle;
  };
  const terminal = (emit: Emit, text = claim.cross_check?.stage === "code" ? codeText : planText): void => {
    emit({ method: "item/completed", params: { threadId: "thread", turnId: "turn", item: { type: "agentMessage", id: "assistant", text } } });
    emit({ method: "turn/completed", params: { threadId: "thread", turn: { id: "turn", status: "completed" } } });
  };
  const provider = roots((frame, emit) => {
    requests.push(frame);
    if (frame.method === "initialize") emit({ id: frame.id, result: { userAgent: "fake", codexHome: "/owned", platformFamily: "unix", platformOs: "linux" } });
    else if (frame.method === "account/login/start") emit({ id: frame.id, result: { type: "apiKey" } });
    else if (frame.method === "thread/start") emit({ id: frame.id, result: { thread: { id: "thread" } } });
    else if (frame.method === "turn/start") {
      emit({ id: frame.id, result: { turn: { id: "turn" } } });
      queueMicrotask(() => (opts.notes ?? terminal)(emit));
    }
  });
  const fileop = roots((frame, emit) => {
    const op = frame as unknown as { id: number; op: string };
    emit({ id: op.id, ok: op.op === "stat" });
  });
  const deps: CrossCheckModelDeps = {
    prepareHome: async () => {},
    launchFileop: async (spec) => { specs.push(spec); return fileop; },
    launchProvider: async (spec) => { specs.push(spec); return provider; },
    ...(opts.extraCa ? { codexExtraCa: opts.extraCa } : {}),
  };
  // Any property read or call on the worker client is a vendor operation: a provider checker makes none.
  const client = claim.secrets.codex
    ? { releaseCodex: async () => ({ auth_mode: "api_key", access_token: "released-token-1234" }) } as unknown as WorkerClient
    : new Proxy({}, { get: (_t, prop) => { clientCalls.push(String(prop)); return undefined; } }) as unknown as WorkerClient;
  const checker = new CodexCrossCheck(client, opts.log ?? nullLogger(), deps);
  const launchedProvider = (): CodexLaunchSpec => specs.find(s => "useAppServerAuth" in s && s.useAppServerAuth === true) as CodexLaunchSpec;
  return { checker, requests, specs, usage, clientCalls, terminal, launchedProvider,
    run: () => checker.run(claim, "/checkout", "/owned", new AbortController().signal, async (p) => { usage.push(p); }, () => {}) };
}

describe("provider-bound Codex checker on the frozen binding", () => {
  for (const stage of ["plan", "code"] as const) {
    it(`${stage}: the mapped target reaches every RPC, the key is the only credential, and no vendor operation occurs`, async () => {
      const { log, counts } = countingLogger();
      const r = rig(providerClaim(stage), { log });
      assert.equal(await r.run(), stage === "code" ? codeText : planText);
      const spec = r.launchedProvider();
      assert.equal(spec.authMode, "provider");
      assert.equal(spec.model, ASTRA, "the launch carries the map slot, not the target");
      assert.equal(spec.crossCheckReadOnly, true);
      assert.equal(spec.useAppServerAuth, true);
      assert.equal(spec.provider, undefined, "no vendor provider block beside a provider binding");
      assert.equal(spec.providerBinding?.provider_id, "gw-1");
      assert.equal(spec.codexExtraCa, undefined, "no private CA unless one is configured");
      const login = r.requests.find(f => f.method === "account/login/start");
      assert.deepEqual(login?.params, { type: "apiKey", apiKey: KEY }, "the frozen key is delivered over the login RPC");
      const thread = r.requests.find(f => f.method === "thread/start")!.params!;
      const turn = r.requests.find(f => f.method === "turn/start")!.params!;
      assert.equal(thread.model, "gw/astra-target");
      assert.equal(turn.model, "gw/astra-target");
      assert.equal(thread.modelProvider, codexCustomProviderName({ provider_id: "gw-1", revision: 2 }));
      assert.equal(thread.config.model_context_window, 32768);
      assert.deepEqual(r.usage[0], { event: "init", model: "gw/astra-target", harness: "codex" });
      assert.deepEqual(r.clientCalls, [], "no vendor release, refresh or admission operation");
      assert.equal(counts.size, 0, "every form the checker registered is evicted");
    });
  }

  it("registers every key form while the checker runs", async () => {
    const { log, counts } = countingLogger();
    let during: string[] = [];
    const r = rig(providerClaim(), { log, notes: (emit) => { during = [...counts.keys()]; r.terminal(emit); } });
    await r.run();
    for (const form of FORMS) assert.ok(during.includes(form), "registered during the run: " + form);
  });

  it("an unmapped model uses the default slot; an unmapped explicit pin is a typed refusal before any launch", async () => {
    const r = rig(providerClaim("plan", MAP, "gpt-6-luna"));
    await r.run();
    assert.equal(r.launchedProvider().model, "default");
    assert.equal(r.requests.find(f => f.method === "thread/start")!.params!.model, "gw/default-model");
    const pinned = providerClaim("plan", MAP, "gpt-6-luna");
    pinned.cross_check!.model_source = "pin";
    const { log, counts } = countingLogger();
    const p = rig(pinned, { log });
    await assert.rejects(p.run(), (e: unknown) => e instanceof ProviderModelUnmappedError);
    assert.equal(p.specs.length, 0, "nothing launched");
    assert.equal(counts.size, 0);
  });

  it("a model-less provider claim resolves through the default slot", async () => {
    const r = rig(providerClaim("plan", MAP, null));
    await r.run();
    assert.equal(r.requests.find(f => f.method === "turn/start")!.params!.model, "gw/default-model");
  });

  describe("usage pricing", () => {
    // The accountant keys on the model sent; gpt-6-astra is non-promotional and table-priced.
    const SAME = { default: { model: ASTRA }, [ASTRA]: { model: ASTRA } };
    const bucket = { inputTokens: 1000, cachedInputTokens: 600, cacheWriteInputTokens: 100, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 1200 };
    const usageNotes = (r: { terminal: (e: Emit) => void }) => (emit: Emit) => {
      emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn", tokenUsage: { total: bucket, last: bucket } } });
      r.terminal(emit);
    };
    async function resultUsage(claim: ClaimResponse): Promise<Record<string, unknown>> {
      const r = rig(claim, { notes: (emit) => usageNotes(r)(emit) });
      await r.run();
      const result = r.usage.at(-1)!;
      assert.equal(result.event, "result");
      assert.equal(result.is_error, false);
      return (result.modelUsage as Record<string, Record<string, unknown>>)[ASTRA]!;
    }

    it("a vendor api_key checker is metered; a provider checker reports the same tokens as unreported with no cost", async () => {
      const vendor = await resultUsage(vendorClaim(ASTRA));
      const provider = await resultUsage(providerClaim("plan", SAME, ASTRA));
      assert.equal(vendor.costStatus, "metered");
      assert.equal(typeof vendor.costUSD, "number");
      assert.ok((vendor.costUSD as number) > 0);
      assert.deepEqual({ ...provider }, { inputTokens: 300, cacheReadInputTokens: 600, cacheCreationInputTokens: 100,
        outputTokens: 200, reasoningOutputTokens: 50, costStatus: "unreported" }, "tokens are reported, cost is unreported");
      assert.equal("costUSD" in provider, false, "no dollar figure on a provider run");
      const { costStatus: _v, costUSD: _c, ...vendorTokens } = vendor;
      const { costStatus: _p, ...providerTokens } = provider;
      assert.deepEqual(providerTokens, vendorTokens, "identical token counts either way");
    });
  });

  describe("private CA bundle", () => {
    async function ca() {
      const file = path.join(await fs.mkdtemp(path.join(scratch, "cc-ca-")), "ca.pem");
      await fs.writeFile(file, rootCertificates[0]! + "\n");
      try { return captureExtraCa(file)!; } finally { await fs.rm(path.dirname(file), { recursive: true, force: true }); }
    }
    it("the provider launch carries the configured bundle; without one it is absent", async () => {
      const bundle = await ca();
      const withCa = rig(providerClaim(), { extraCa: bundle });
      await withCa.run();
      assert.equal(withCa.launchedProvider().codexExtraCa, bundle);
      const without = rig(providerClaim());
      await without.run();
      assert.equal("codexExtraCa" in without.launchedProvider(), false);
    });
    it("a vendor checker never carries the bundle", async () => {
      const bundle = await ca();
      const vendor = rig(vendorClaim(), { extraCa: bundle });
      await vendor.checker.run(vendorClaim(), "/checkout", "/owned", new AbortController().signal, async () => {}).catch(() => {});
      assert.equal(vendor.specs.some(s => "codexExtraCa" in s), false);
    });
  });
});

// ── CrossCheckRunner: registration order, scrubbers and the outer-log canaries ─────────────────────

type Harness = ReturnType<typeof harness>;
async function harness(opts: {
  reportState?: (...a: unknown[]) => Promise<unknown>;
  verdictPost?: (v: unknown, nth: number) => Promise<void>;
  codeVerdictPost?: (v: unknown, nth: number) => Promise<void>;
} = {}) {
  const root = await fs.mkdtemp(path.join(scratch, "cross-check-provider-"));
  const posted: unknown[] = [];
  const verdicts: unknown[] = [];
  let verdictCalls = 0;
  const client = {
    reportState: opts.reportState ?? (async () => ({ applied: true })),
    getInputs: async () => ({ inputs: [] }),
    postMessages: async (...args: unknown[]) => { posted.push(args); },
    reportCrossCheckVerdict: async (_id: string, _gen: number, v: unknown) => {
      verdictCalls++;
      if (opts.verdictPost) await opts.verdictPost(v, verdictCalls);
      verdicts.push(v);
    },
    reportCodeCrossCheckVerdict: async (_id: string, _gen: number, v: unknown) => {
      verdictCalls++;
      if (opts.codeVerdictPost) await opts.codeVerdictPost(v, verdictCalls);
      verdicts.push(v);
    },
    getCodeSnapshotCleanup: async () => { throw new Error("unused"); },
  } as unknown as WorkerClient;
  const git = {
    ensureClone: async () => "/bare", barePathFor: () => "/bare", existingBarePathFor: async () => "/bare",
    runnerCloneAtCommit: async () => root, readBare: async () => ({ text: "diff", truncated: false }),
    removeRunnerClone: async () => {},
  } as unknown as GitCache;
  return { root, posted, verdicts, client, git, cleanup: () => fs.rm(root, { recursive: true, force: true }) };
}
const runnerOf = (h: Harness extends Promise<infer T> ? T : never, log: Logger, model: Pick<CodexCrossCheck, "run">) =>
  new CrossCheckRunner(h.client, h.git, log, { homeRoot: h.root, pollMs: 1, batchMs: 5, modelTimeoutMs: 2000, model,
    claudeModel: { run: async () => { assert.fail("wrong family"); } } });

function strings(value: unknown, out: string[] = []): string[] {
  if (typeof value === "string") out.push(value);
  else if (Array.isArray(value)) for (const v of value) strings(v, out);
  else if (value !== null && typeof value === "object") for (const [k, v] of Object.entries(value)) { out.push(k); strings(v, out); }
  return out;
}
const leaks = (value: unknown): string[] => strings(value).filter(s => FORMS.some(f => s.includes(f)));

/** Capture the real logger's stdout and stderr for the duration of `work`. */
async function captured(work: () => Promise<void>): Promise<string> {
  const chunks: string[] = [];
  const sink = (chunk: unknown): boolean => { chunks.push(String(chunk)); return true; };
  const err = mock.method(process.stderr, "write", sink as never);
  const out = mock.method(process.stdout, "write", sink as never);
  try { await work(); } finally { err.mock.restore(); out.mock.restore(); }
  return chunks.join("");
}

describe("CrossCheckRunner on a provider-bound Codex claim", () => {
  const keyEcho = (stage: "plan" | "code"): string => stage === "code"
    ? JSON.stringify({ findings: [{ id: "F_1", severity: "major", path: "anchor.ts", line: 1, title: `raw ${KEY}`,
      detail: `json ${FORMS[1]} pct ${FORMS.at(-1)}` }] })
    : JSON.stringify({ verdict: "approve", summary: `raw ${KEY}`, items: [
      { file: "a.ts", severity: "info", summary: `json ${FORMS[1]}`, rationale: `pct ${FORMS.at(-1)} slash ${FORMS[2]}` }] });

  for (const stage of ["plan", "code"] as const) {
    it(`${stage}: a verdict whose parsed fields echo the key is delivered with every key form masked`, async () => {
      const h = await harness();
      const text = keyEcho(stage);
      // Positive control: the model's own output really carries each form, so a clean body is not vacuous.
      assert.ok(leaks(JSON.parse(text)).length > 0, "control: the raw verdict carries the key");
      try {
        await runnerOf(h, nullLogger(), { run: async (_c, _co, _h, _s, _postUsage, confirm) => { confirm?.(); return text; } })
          .execute(providerClaim(stage));
        assert.equal(h.verdicts.length, 1);
        assert.deepEqual(leaks(h.verdicts[0]), [], "the delivered body carries no key form");
        assert.ok(strings(h.verdicts[0]).some(s => s.includes(MASK)), "the text is kept with the key masked");
      } finally { await h.cleanup(); }
    });
  }

  it("a key form in the usage init frame's model field is masked in the delivered frame", async () => {
    const h = await harness();
    try {
      await runnerOf(h, nullLogger(), { run: async (_c, _co, _h, _s, postUsage) => {
        await postUsage({ event: "init", model: `team/${KEY}`, harness: "codex" });
        await postUsage({ event: "result", subtype: "cross_check", is_error: false, modelUsage: { [`team/${KEY}`]: { inputTokens: 1 } } });
        return planText;
      } }).execute(providerClaim("plan"));
      assert.equal(h.verdicts.length, 1, "the check completed");
      assert.ok(h.posted.length > 0, "frames were delivered");
      assert.deepEqual(leaks(h.posted), [], "no delivered frame carries a key form");
      assert.ok(strings(h.posted).some(s => s.includes(MASK)), "the frame is kept with the key masked");
    } finally { await h.cleanup(); }
  });

  it("the key is evicted once per holder: a registration another lane holds survives", async () => {
    const { log, counts } = countingLogger();
    for (const f of FORMS) log.addSecret(f); // a chat or judge run holding the same key
    const h = await harness();
    try {
      await runnerOf(h, log, { run: async () => planText }).execute(providerClaim("plan"));
      assert.equal(h.verdicts.length, 1);
      for (const f of FORMS) assert.equal(counts.get(f), 1, "only this check's own registration was removed: " + f);
      // The startup-failure path evicts too.
      const failing = await harness({ reportState: async () => { throw new Error("boom"); } });
      try {
        await runnerOf(failing, log, { run: async () => planText }).execute(providerClaim("plan"));
      } finally { await failing.cleanup(); }
      for (const f of FORMS) assert.equal(counts.get(f), 1, "a failed start evicts its own registration only: " + f);
    } finally { await h.cleanup(); }
  });

  it("a Claude-engine provider binding still refuses the lane: reported checker_unavailable, never run", async () => {
    const h = await harness();
    let ran = false;
    const claude = { provider_id: "c-1", revision: 1, engine: "claude", protocol: "anthropic_messages",
      base_url: "https://gw.example.test/anthropic", auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } } };
    try {
      await runnerOf(h, nullLogger(), { run: async () => { ran = true; return planText; } })
        .execute(claimOf("plan", { anthropic_oauth_token: "", provider: claude }, "sonnet"));
      assert.equal(ran, false);
      // The refusal precedes the telemetry batcher and the activity recorder: nothing was streamed.
      assert.deepEqual(h.posted, [], "refused before any checker telemetry started");
      assert.equal(h.verdicts.length, 1);
      assert.deepEqual(h.verdicts[0], { verdict: "failed", reason_class: "checker_unavailable",
        summary: "The cross-check did not complete.", items: [] });
    } finally { await h.cleanup(); }
  });

  // The two canaries use the REAL logger: a stub logger's addSecret is a no-op, so only the real
  // registry can show whether a log record leaks.
  it("canary (startup): the key is registered before the first await, so a failed start cannot log it", async () => {
    const delivery = new Error(`gateway said no for ${FORMS.join(" | ")}`);
    const h = await harness({
      reportState: async () => { throw new Error("connection refused"); },
      verdictPost: async () => { throw delivery; },
    });
    const log = createLogger("debug");
    try {
      const output = await captured(() => runnerOf(h, log, { run: async () => { assert.fail("must not run"); } }).execute(providerClaim("plan")));
      const record = output.split("\n").find(l => l.includes("cross-check failed verdict delivery failed"));
      assert.ok(record, "the failed-verdict delivery record is emitted");
      assert.deepEqual(FORMS.filter(f => output.includes(f)), [], "no log line carries a key form");
      // Positive control: with nothing registered the same record WOULD carry the key.
      const control = await captured(async () => { createLogger("debug").warn("cross-check failed verdict delivery failed", { error: delivery.message }); });
      assert.ok(FORMS.some(f => control.includes(f)), "control: an unregistered logger leaks");
    } finally { await h.cleanup(); }
  });

  it("canary (post-model): the outer registration still scrubs after the checker's own eviction", async () => {
    const delivery = new Error(`gateway said no for ${FORMS.join(" | ")}`);
    const h = await harness({
      // The success-path POST fails after the model finished, then the failed-verdict POST fails too.
      verdictPost: async () => { throw delivery; },
    });
    const log = createLogger("debug");
    const { checker } = rig(providerClaim("plan"), { log });
    try {
      const output = await captured(() => runnerOf(h, log, checker).execute(providerClaim("plan")));
      assert.ok(output.includes("cross-check failed verdict delivery failed"), "the failed-verdict delivery record is emitted");
      assert.deepEqual(FORMS.filter(f => output.includes(f)), [], "no log line carries a key form after the inner eviction");
    } finally { await h.cleanup(); }
  });
});

it("a RequestError 409 stale-claim on the initial report returns silently (the canary's non-409 requirement)", async () => {
  // Pins why the startup canary must reject with a plain error: a stale claim never reaches the catch.
  const h = await harness({ reportState: async () => { throw new RequestError("POST", "/x", 409, JSON.stringify({ disposition: "stale_claim" })); } });
  try {
    await runnerOf(h, nullLogger(), { run: async () => planText }).execute(providerClaim("plan"));
    assert.equal(h.verdicts.length, 0);
  } finally { await h.cleanup(); }
});
