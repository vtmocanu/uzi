import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";
import type { Readable } from "node:stream";
import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";

import { RequestError, type WorkerClient } from "../src/client.js";
import { buildJobResultServer, JobRunner } from "../src/job-runner.js";
import type { Logger } from "../src/log.js";
import { Outbox } from "../src/outbox.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { PROVIDER_LANE_UNSUPPORTED } from "../src/provider-binding.js";
import type { ClaimResponse, JobFileUploadMeta, JobResultRequest, OutgoingMessage, StateRequest } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// PRD #4322 M1b: the Claude provider-bound JOB on the worker. Every case drives the real
// JobRunner.execute (or the real result tool) with a scripted queryFn (the SDK seam) and a fake api
// client, and asserts only what the api would observe: the posted states, messages, result, uploads
// and the captured log output. Gateway keys are assembled at runtime: no provider-token-shaped
// literal in source.

const LINUX_ONLY = process.platform !== "linux" ? "needs Linux procfs output-ancestor pinning" : false;
const GEN = 7;
/** A key with characters every encoding form rewrites. */
const KEY = ["gw", "key/do-not-scan", "7777<abcdefgh&z"].join("-");
/** A lowercase key that is also a legal result status (<= 32 chars, ^[a-z][a-z0-9_-]*$). */
const STATUS_KEY = ["zq9k", "jw3x", "v7mn", "p2rt", "b8cd", "e4fg", "h5ij", "k6lm"].join("");

const roots: string[] = [];
afterEach(async () => {
  while (roots.length) await fsp.rm(roots.pop()!, { recursive: true, force: true });
});
async function tmpJobsRoot(): Promise<string> {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-jobprov-"));
  roots.push(d);
  return path.join(d, "jobs");
}

function binding(key: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    provider_id: "p-1", revision: 3, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/anthropic",
    auth_style: "bearer", credential: key,
    model_map: { haiku: { model: "corp-haiku" }, sonnet: { model: "corp-sonnet" } }, ...over,
  };
}

function providerJobClaim(key = KEY, over: Partial<ClaimResponse> = {}, bindingOver: Record<string, unknown> = {}): ClaimResponse {
  return {
    run_id: randomUUID(), kind: "job", issue_iid: null, issue_title: "", issue_description: "", repo: undefined,
    secrets: { forge_pat: "", anthropic_oauth_token: "", provider: binding(key, bindingOver) },
    last_seq: 0, agents: [], claim_generation: GEN, budget_wall_seconds: 30,
    job: { type: "research", title: "Summarise the doc", prompt: "Summarise the input.", inputs: [{ name: "doc.md", content: "the document body" }] },
    config: { default_model: "sonnet" },
    ...over,
  } as unknown as ClaimResponse;
}

interface Calls {
  order: string[];
  states: StateRequest[];
  messages: OutgoingMessage[];
  results: JobResultRequest[];
  uploads: { meta: JobFileUploadMeta; bytes: Buffer }[];
}

function fakeClient(opts: { reportState?: (body: StateRequest) => unknown } = {}): { client: WorkerClient; calls: Calls } {
  const calls: Calls = { order: [], states: [], messages: [], results: [], uploads: [] };
  const client = {
    reportState: async (_id: string, body: StateRequest) => {
      calls.order.push(`state:${body.status}`);
      calls.states.push(body);
      return opts.reportState ? opts.reportState(body) : { applied: true, status: body.status };
    },
    postMessages: async (_id: string, messages: OutgoingMessage[]) => { calls.messages.push(...messages); },
    getInputs: async () => ({ inputs: [], receipts: false }),
    ackInputs: async () => ({ active: true, inputs: [] }),
    applyInputs: async () => ({ active: true, inputs: [] }),
    postJobResult: async (_id: string, body: JobResultRequest) => { calls.order.push("result"); calls.results.push(body); },
    uploadJobFile: async (_id: string, meta: JobFileUploadMeta, body: Buffer | (() => Readable | Promise<Readable>)) => {
      const parts: Buffer[] = [];
      for await (const c of Buffer.isBuffer(body) ? [body] : await body()) parts.push(Buffer.from(c as Uint8Array));
      calls.uploads.push({ meta, bytes: Buffer.concat(parts) });
      return { status: 201, file: { id: "f", display_name: meta.display_name, storage_name: "s", content_type: "text/plain", byte_size: meta.size, sha256: meta.sha256, state: "attached", expires_at: null } };
    },
    hasFeature: () => false,
  } as unknown as WorkerClient;
  return { client, calls };
}

type Handler = (args: unknown, extra: unknown) => Promise<{ isError?: boolean; content: Array<{ text?: string }> }>;
async function callSubmit(options: SdkOptions, args: unknown): Promise<{ isError?: boolean; content: Array<{ text?: string }> }> {
  const servers = options.mcpServers as unknown as Record<string, { instance: { _registeredTools: Record<string, { handler: Handler }> } }>;
  return servers.job!.instance._registeredTools["submit_job_result"]!.handler(args, {});
}

const INIT_OK = { type: "system", subtype: "init", tools: ["Read", "Write", "Glob", "Grep", "mcp__job__submit_job_result"] };
const RESULT_OK = { type: "result", subtype: "success", is_error: false, result: "done", usage: { input_tokens: 11, output_tokens: 7 }, modelUsage: {} };
const GOOD = { status: "completed", report_md: "# Report", findings: [] as unknown[] };

function scripted(script: (ctx: { options: SdkOptions }) => AsyncGenerator<unknown>, seen: { options?: SdkOptions; calls: number } = { calls: 0 }): SdkQueryFn {
  return (({ options }: { options: SdkOptions }) => {
    seen.options = options;
    seen.calls++;
    return script({ options });
  }) as unknown as SdkQueryFn;
}

function runner(client: WorkerClient, jobsRoot: string, queryFn: SdkQueryFn, log: Logger = nullLogger(), extra: Record<string, unknown> = {}): JobRunner {
  return new JobRunner(client, log, { queryFn, jobsRoot, batchMs: 5, cancelPollMs: 10, outputRetryDelaysMs: [1, 1], ...extra });
}

const everywhere = (calls: Calls, lines: unknown[] = []): string => JSON.stringify([calls.states, calls.messages, calls.results, calls.uploads.map((u) => u.meta), lines]);
function assertNoKey(key: string, text: string, what: string): void {
  // The text is JSON-stringified, which doubles a form's backslashes: check both spellings.
  for (const f of providerKeyForms(key)) {
    assert.ok(!text.includes(f), `${what}: a form of the key reached the api or a log: ${f}`);
    assert.ok(!text.includes(JSON.stringify(f).slice(1, -1)), `${what}: a JSON-escaped form of the key reached the api or a log: ${f}`);
  }
}

it("assertNoKey catches a JSON-escaped echo of every key form (negative control)", () => {
  const escaped = providerKeyForms(KEY).filter((f) => JSON.stringify(f).slice(1, -1) !== f);
  assert.ok(escaped.length > 0, "at least one form carries a backslash");
  for (const f of escaped) assert.throws(() => assertNoKey(KEY, JSON.stringify([f]), "control"), /JSON-escaped form/);
});

describe("provider job: SDK options (A5 worker side)", () => {
  it("runs on the gateway: provider env, no vendor token, the mapped model, the same closed tool surface", async () => {
    const { client, calls } = fakeClient();
    const seen: { options?: SdkOptions; calls: number } = { calls: 0 };
    const qf = scripted(async function* ({ options }) {
      yield INIT_OK;
      assert.equal((await callSubmit(options, GOOD)).isError, undefined);
      yield RESULT_OK;
    }, seen);
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim());
    const env = seen.options!.env as Record<string, string | undefined>;
    assert.equal(env.ANTHROPIC_BASE_URL, "https://gw.example.test/anthropic");
    assert.equal(env.ANTHROPIC_AUTH_TOKEN, KEY);
    assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, undefined);
    assert.equal(seen.options!.model, "sonnet");
    assert.deepEqual(seen.options!.tools, ["Read", "Write", "Glob", "Grep", "mcp__job__submit_job_result"]);
    assert.equal(calls.states.at(-1)!.status, "completed");
  });

  it("a mapped target model string passes through unrewritten; an unset or inherit model is the sonnet slot", async () => {
    for (const [configured, expected] of [["corp-sonnet", "corp-sonnet"], [undefined, "sonnet"], ["inherit", "sonnet"]] as const) {
      const { client } = fakeClient();
      const seen: { options?: SdkOptions; calls: number } = { calls: 0 };
      const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield RESULT_OK; }, seen);
      await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(KEY, { config: configured === undefined ? {} : { default_model: configured } } as never));
      assert.equal(seen.options!.model, expected, String(configured));
    }
  });

  describe("the model alias and the gateway target are separate facts", () => {
    const MODEL_ENV = { opus: "ANTHROPIC_DEFAULT_OPUS_MODEL", sonnet: "ANTHROPIC_DEFAULT_SONNET_MODEL", haiku: "ANTHROPIC_DEFAULT_HAIKU_MODEL" } as const;
    async function resolved(modelMap: Record<string, unknown>, config: Record<string, unknown>): Promise<{ model: unknown; env: Record<string, string | undefined> }> {
      const { client } = fakeClient();
      const seen: { options?: SdkOptions; calls: number } = { calls: 0 };
      const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield RESULT_OK; }, seen);
      await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(KEY, { config } as never, { model_map: modelMap }));
      assert.equal(seen.calls, 1, "the session ran");
      return { model: seen.options!.model, env: seen.options!.env as Record<string, string | undefined> };
    }
    const MAP = { opus: { model: "corp-opus" }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } };

    it("an alias is the model; the map's target for it is the gateway target", async () => {
      const r = await resolved(MAP, { default_model: "opus" });
      assert.equal(r.model, "opus");
      assert.equal(r.env[MODEL_ENV.opus], "corp-opus");
      assert.equal(r.env[MODEL_ENV.sonnet], "corp-sonnet");
    });

    it("a sonnet slot mapped to the literal target inherit is passed through unrewritten when no model is configured", async () => {
      const r = await resolved({ ...MAP, sonnet: { model: "inherit" } }, {});
      assert.equal(r.model, "sonnet");
      assert.equal(r.env[MODEL_ENV.sonnet], "inherit");
    });

    it("the server-canonical sonnet model keeps its own target next to an opus slot mapped to inherit", async () => {
      const r = await resolved({ ...MAP, opus: { model: "inherit" }, sonnet: { model: "x" } }, { default_model: "sonnet" });
      assert.equal(r.model, "sonnet");
      assert.equal(r.env[MODEL_ENV.sonnet], "x");
      assert.equal(r.env[MODEL_ENV.opus], "inherit");
    });

    it("a blank model is the sonnet slot", async () => {
      const r = await resolved(MAP, { default_model: "   " });
      assert.equal(r.model, "sonnet");
      assert.equal(r.env[MODEL_ENV.sonnet], "corp-sonnet");
    });

    it("an explicit alias other than sonnet selects that alias and its target", async () => {
      const r = await resolved(MAP, { default_model: "haiku" });
      assert.equal(r.model, "haiku");
      assert.equal(r.env[MODEL_ENV.haiku], "corp-haiku");
    });

    it("an explicit mapped target ID is the model as written, with the same target in its slot", async () => {
      const r = await resolved(MAP, { default_model: "corp-haiku" });
      assert.equal(r.model, "corp-haiku");
      assert.equal(r.env[MODEL_ENV.haiku], "corp-haiku");
      assert.equal(r.env[MODEL_ENV.sonnet], "corp-sonnet");
    });
  });

  it("an unmapped model fails the job with the constant reason before any workspace or model call", async () => {
    const { client, calls } = fakeClient();
    const seen: { options?: SdkOptions; calls: number } = { calls: 0 };
    const qf = scripted(async function* () { yield INIT_OK; }, seen);
    const jobsRoot = await tmpJobsRoot();
    await runner(client, jobsRoot, qf).execute(providerJobClaim(KEY, { config: { default_model: "opus" } } as never));
    assert.equal(seen.calls, 0);
    assert.deepEqual(calls.states.map((s) => [s.status, s.failure_reason]), [["failed", "provider_model_unmapped"]]);
    await assert.rejects(fsp.stat(jobsRoot), { code: "ENOENT" }, "no workspace was created");
  });
});

describe("provider job: lane refusals (constant reason, before the first report)", () => {
  const isolated = { profile: "p", hosts: ["a.example.test"], credential: "fetch-credential-do-not-scan-0001", fetcher: "x" };
  for (const [name, claim] of [
    ["a site-list job (isolated_fetch next to the provider)", providerJobClaim(KEY, { isolated_fetch: isolated } as never)],
    ["a non-Claude engine binding", providerJobClaim(KEY, {}, { engine: "codex", protocol: "openai_responses", model_map: { default: { model: "m" } } })],
  ] as const) {
    it(`refuses ${name}`, async () => {
      const { client, calls } = fakeClient();
      const seen = { calls: 0 };
      const qf = scripted(async function* () { yield INIT_OK; }, seen as never);
      await runner(client, await tmpJobsRoot(), qf).execute(claim);
      assert.equal((seen as { calls: number }).calls, 0);
      assert.deepEqual(calls.states.map((s) => [s.status, s.failure_reason]), [["failed", PROVIDER_LANE_UNSUPPORTED]]);
    });
  }
});

describe("provider job: a late terminal replay", () => {
  it("a failed late replay of a provider job terminal logs no key, whatever the error response body echoes", async () => {
    const root = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-jobprov-replay-"));
    roots.push(root);
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e6, retentionMs: 86_400_000 });
    await outbox.init();
    const { client } = fakeClient({
      reportState: (b) => {
        if (b.status === "failed" || b.status === "completed") throw new Error("connect ECONNRESET: api unreachable");
        return { applied: true, status: b.status };
      },
    });
    // A refused lane fails the job before any workspace (so this runs on every platform) and journals the terminal.
    const qf = scripted(async function* () { yield INIT_OK; });
    const claim = providerJobClaim(KEY, { isolated_fetch: { profile: "p", hosts: ["a.example.test"], credential: "fetch-credential-do-not-scan-0001", fetcher: "x" } } as never);
    await runner(client, path.join(root, "jobs"), qf, nullLogger(), { outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 }).execute(claim);
    const mine = outbox.listPendingTerminals().find((p) => p.run_id === claim.run_id && p.claim_generation === GEN);
    assert.ok(mine, "the job's terminal is journaled and pending");
    // The late replay runs on the plain worker logger (no per-run redactor): the error response echoes
    // every form of the bound key.
    const echo = providerKeyForms(KEY).join(" | ");
    const replay = recordingLogger();
    const deps = makeTerminalOutboxDeps(outbox, client, { gapFillMax: 100, terminalMaxBytes: 1 << 20, log: replay.logger })!;
    await resolvePendingTerminal(deps, {
      runId: mine.run_id, claimGeneration: mine.claim_generation,
      send: async () => { throw new RequestError("POST", "/state", 502, `gateway said ${echo}`); },
    });
    const lines = JSON.stringify(replay.lines);
    assert.ok(replay.lines.length > 0, "the failed replay was logged");
    assertNoKey(KEY, lines, "replay log");
    assert.ok(lines.includes("POST /state returned 502"), "the bounded diagnostic names the request and status");
  });
});

describe("provider job: key registration for the whole claim", () => {
  function registryLogger(events: string[]): { logger: Logger; added: string[]; removed: string[] } {
    const added: string[] = [];
    const removed: string[] = [];
    const logger = nullLogger();
    logger.addSecret = (s: string) => { added.push(s); events.push("add"); };
    logger.removeSecret = (s: string) => { removed.push(s); };
    return { logger, added, removed };
  }

  it("registers every key form before the first report and removes exactly those on every exit", async () => {
    const events: string[] = [];
    const reg = registryLogger(events);
    const { client } = fakeClient({ reportState: (b) => { events.push(`state:${b.status}`); return { applied: true, status: b.status }; } });
    const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield RESULT_OK; });
    await runner(client, await tmpJobsRoot(), qf, reg.logger).execute(providerJobClaim());
    const forms = providerKeyForms(KEY);
    assert.deepEqual([...reg.added].sort(), [...forms].sort());
    assert.deepEqual([...reg.removed].sort(), [...forms].sort());
    const firstReport = events.findIndex((e) => e.startsWith("state:"));
    assert.equal(events.slice(0, firstReport).filter((e) => e === "add").length, forms.length, "every form was registered before the first report");
  });

  it("a refused claim still registers and removes the forms (the refusal text is scrubbed too)", async () => {
    const reg = registryLogger([]);
    const { client } = fakeClient();
    const claim = providerJobClaim(KEY, {}, { engine: "codex", protocol: "openai_responses", model_map: { default: { model: "m" } } });
    await runner(client, await tmpJobsRoot(), scripted(async function* () { yield INIT_OK; }), reg.logger).execute(claim);
    assert.equal(reg.added.length, providerKeyForms(KEY).length);
    assert.deepEqual([...reg.removed].sort(), [...reg.added].sort());
  });
});

describe("provider job: a key echoed by the gateway never reaches the api or the logs", { skip: LINUX_ONLY }, () => {
  it("scrubs messages, tool input, result frame, report, findings, failure text and logs", async () => {
    const { client, calls } = fakeClient();
    const rec = recordingLogger();
    const echoed = `echo ${KEY} and ${JSON.stringify(KEY).slice(1, -1)} and ${encodeURIComponent(KEY)}`;
    const qf = scripted(async function* ({ options }) {
      yield INIT_OK;
      yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: echoed }, { type: "tool_use", id: "t1", name: "Read", input: { file_path: `inputs/${KEY}` } }] } };
      yield { type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "t1", content: echoed, is_error: true }] } };
      const res = await callSubmit(options, {
        status: "completed", report_md: `report ${KEY}`,
        findings: [
          { severity: "info", message_md: `finding ${KEY}`, url: `https://example.test/${encodeURIComponent(KEY)}` },
          { severity: "warning", message_md: "second", file: `f/${KEY}`, line: 1 },
        ],
      });
      assert.equal(res.isError, undefined);
      yield { ...RESULT_OK, result: echoed };
    });
    await runner(client, await tmpJobsRoot(), qf, rec.logger).execute(providerJobClaim());
    assert.equal(calls.states.at(-1)!.status, "completed", JSON.stringify(calls.states.at(-1)));
    assertNoKey(KEY, everywhere(calls, rec.lines), "completed job");
    assert.ok(everywhere(calls).includes("***REDACTED***"), "the scrub ran");
    assert.match(calls.results[0]!.report_md, /^report \*\*\*REDACTED\*\*\*$/);
  });

  it("scrubs a thrown session error carried into failure_reason, and the terminal outbox replay", async () => {
    const root = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-jobprov-ob-"));
    roots.push(root);
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e6, retentionMs: 86_400_000 });
    await outbox.init();
    // The api is unreachable for the terminal report: the send throws, so the journal stays pending.
    const { client, calls } = fakeClient({
      reportState: (b) => {
        if (b.status === "failed" || b.status === "completed") throw new Error("connect ECONNRESET: api unreachable");
        return { applied: true, status: b.status };
      },
    });
    const qf = scripted(async function* () {
      yield INIT_OK;
      throw new Error(`gateway said ${KEY} is bad`);
    });
    const claim = providerJobClaim();
    const jobLog = recordingLogger();
    await runner(client, path.join(root, "jobs"), qf, jobLog.logger, { outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 }).execute(claim);
    // The terminal report itself was attempted (and refused), so every observation is of the journal.
    const attempted = calls.states.at(-1)!;
    assert.equal(attempted.status, "failed");
    assert.match(attempted.failure_reason ?? "", /the job session failed: gateway said \*\*\*REDACTED\*\*\* is bad/);
    assertNoKey(KEY, everywhere(calls, jobLog.lines), "attempted terminal");
    // The journal is still pending: the replay below is the only thing that can deliver it.
    const pending = outbox.listPendingTerminals();
    assert.ok(pending.length >= 1, "a retryable terminal delivery failure leaves a pending terminal record");
    const mine = pending.find((p) => p.run_id === claim.run_id && p.claim_generation === GEN);
    assert.ok(mine, "the job's own terminal is the pending one");
    const journal = await outbox.readTerminalJournal(mine.run_id, mine.claim_generation);
    assert.ok(journal, "the journal is readable");
    assertNoKey(KEY, JSON.stringify(journal), "terminal journal");
    // Replay it now, long after the claim's key registration ended, through the real resolve path.
    const sent: StateRequest[] = [];
    const deps = makeTerminalOutboxDeps(outbox, client, { gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() })!;
    await resolvePendingTerminal(deps, {
      runId: mine.run_id, claimGeneration: mine.claim_generation,
      send: async (body) => { sent.push(body); return { applied: true, status: body.status } as never; },
    });
    assert.equal(sent.length, 1, "the replay sent the journaled terminal exactly once");
    assert.equal(sent[0]!.status, "failed");
    assert.match(sent[0]!.failure_reason ?? "", /gateway said \*\*\*REDACTED\*\*\* is bad/);
    assertNoKey(KEY, JSON.stringify(sent), "replayed terminal");
    assert.deepEqual(outbox.listPendingTerminals().filter((p) => p.run_id === claim.run_id), [], "the replay retired the journal");
  });

  const PRICED = {
    ...RESULT_OK, total_cost_usd: 1.25,
    usage: { input_tokens: 123, output_tokens: 45, cache_read_input_tokens: 678, cache_creation_input_tokens: 91 },
    modelUsage: {
      "corp-sonnet": { inputTokens: 123, outputTokens: 45, cacheReadInputTokens: 678, cacheCreationInputTokens: 91, webSearchRequests: 3, costUSD: 0.5 },
      "corp-haiku": { inputTokens: 8, outputTokens: 6, cacheReadInputTokens: 5, cacheCreationInputTokens: 4, webSearchRequests: 2, costUSD: 0.25 },
    },
  };
  const usageOf = (calls: Calls): Record<string, unknown> => {
    const found = calls.messages.filter((m) => "usage" in (m.payload as Record<string, unknown>));
    assert.equal(found.length, 1, "exactly one emitted message carries the usage");
    return found[0]!.payload as Record<string, unknown>;
  };
  function assertPricedCounters(payload: Record<string, unknown>): void {
    assert.ok(!Object.hasOwn(payload, "total_cost_usd"), "top-level cost dropped");
    const usage = payload["usage"] as Record<string, unknown>;
    assert.equal(usage["input_tokens"], 123);
    assert.equal(usage["output_tokens"], 45);
    assert.equal(usage["cache_read_input_tokens"], 678);
    assert.equal(usage["cache_creation_input_tokens"], 91);
    const mu = payload["modelUsage"] as Record<string, Record<string, unknown>>;
    assert.deepEqual(mu["corp-sonnet"], { inputTokens: 123, outputTokens: 45, cacheReadInputTokens: 678, cacheCreationInputTokens: 91, webSearchRequests: 3 });
    assert.deepEqual(mu["corp-haiku"], { inputTokens: 8, outputTokens: 6, cacheReadInputTokens: 5, cacheCreationInputTokens: 4, webSearchRequests: 2 });
    for (const entry of Object.values(mu)) assert.ok(!Object.hasOwn(entry, "costUSD"), "per-model cost dropped");
  }

  it("strips cost from the usage frame and keeps every token counter at its exact field", async () => {
    const { client, calls } = fakeClient();
    const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield PRICED; });
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim());
    assertPricedCounters(usageOf(calls));
  });

  for (const counterKey of ["cacheReadInputTokens", "cache_read_input_tokens", "cacheCreationInputTokens"]) {
    it(`a key equal to the counter name ${counterKey} cannot rename the field and zero the count`, async () => {
      const { client, calls } = fakeClient();
      const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield PRICED; });
      await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(counterKey));
      assertPricedCounters(usageOf(calls));
      assert.equal(calls.states.at(-1)!.status, "completed");
    });
  }

  it("a numeric usage member named with the key is never restored under its raw name", async () => {
    const { client, calls } = fakeClient();
    const frame = {
      ...PRICED,
      usage: { ...PRICED.usage, ["x" + KEY]: 7 },
      modelUsage: { ...PRICED.modelUsage, "corp-sonnet": { ...PRICED.modelUsage["corp-sonnet"], ["y" + KEY]: 9 } },
    };
    const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield frame; });
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim());
    assertPricedCounters(usageOf(calls));
    assertNoKey(KEY, everywhere(calls), "usage member names");
  });

  it("two model names that scrub to the same name keep the summed entry", async () => {
    const { client, calls } = fakeClient();
    const frame = {
      ...RESULT_OK,
      usage: { input_tokens: 3, output_tokens: 4 },
      // The raw key and its JSON `\/`-escaped form are both key forms, so both names scrub to "a-<marker>".
      modelUsage: { ["a-" + KEY]: { inputTokens: 1, outputTokens: 2 }, ["a-" + KEY.replace("/", "\\/")]: { inputTokens: 10, outputTokens: 20 } },
    };
    const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield frame; });
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim());
    const mu = usageOf(calls)["modelUsage"] as Record<string, Record<string, unknown>>;
    const totals = Object.values(mu).reduce((acc, e) => acc + Number(e["inputTokens"] ?? 0), 0);
    assert.equal(totals, 11, "no model's input tokens are lost");
    assertNoKey(KEY, everywhere(calls), "model names");
  });
});

describe("provider job: gateway limit settlement reads the raw frame", () => {
  const errorResult = (extra: Record<string, unknown>): Record<string, unknown> => ({ type: "result", subtype: "success", is_error: true, result: "gateway error", ...extra });
  async function settle(frames: unknown[], key = KEY): Promise<StateRequest[]> {
    const { client, calls } = fakeClient();
    const qf = scripted(async function* () { for (const f of frames) yield f; });
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(key));
    return calls.states;
  }

  for (const status of [429, 529]) {
    it(`api_error_status ${status} fails the job as the provider gateway limit: no vendor limit type, never limit_wait`, async () => {
      const states = await settle([INIT_OK, errorResult({ api_error_status: status })]);
      const last = states.at(-1)!;
      assert.equal(last.status, "failed");
      assert.equal(last.rate_limit_type, "provider_gateway_limit");
      assert.equal(last.limit_resets_at, undefined);
      assert.ok(!states.some((s) => (s.status as string) === "limit_wait"));
    });
  }

  it("a key equal to the status member name cannot turn a limit into a plain error", async () => {
    const states = await settle([INIT_OK, errorResult({ api_error_status: 429 })], "api_error_status");
    assert.equal(states.at(-1)!.rate_limit_type, "provider_gateway_limit");
  });

  it("a key equal to a result member value cannot hide an error result", async () => {
    // The key equals the error flag's member name, so only the raw frame still says is_error; the
    // reason distinguishes the error-result settlement from the no-result path.
    const states = await settle([INIT_OK, { type: "result", subtype: "success", is_error: true }], "is_error");
    assert.equal(states.at(-1)!.status, "failed");
    assert.equal(states.at(-1)!.failure_reason, "the job session ended with an error result");
    assert.equal(states.at(-1)!.rate_limit_type, undefined);
  });

  it("a proxied vendor-looking blocking limit never classifies as a vendor limit", async () => {
    const states = await settle([
      INIT_OK,
      { type: "rate_limit_event", rate_limit_info: { status: "rejected", resetsAt: Math.floor(Date.now() / 1000) + 3600, rateLimitType: "five_hour" } },
      errorResult({ api_error_status: 500, result: "You've hit your limit" }),
    ]);
    const last = states.at(-1)!;
    assert.equal(last.status, "failed");
    assert.equal(last.rate_limit_type, undefined);
    assert.equal(last.limit_resets_at, undefined);
    assert.equal(last.failure_reason, "the job session ended with an error result");
  });
});

describe("provider job: the tool policy decides on the original frame and reports scrubbed text", () => {
  it("a key equal to an allowed tool name does not trip the policy", async () => {
    const { client, calls } = fakeClient();
    const allowed = "mcp__job__submit_job_result";
    const qf = scripted(async function* ({ options }) { yield INIT_OK; await callSubmit(options, GOOD); yield RESULT_OK; });
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(allowed));
    assert.equal(calls.states.at(-1)!.status, "completed");
  });

  it("a disallowed tool named with the key is reported without the key, wherever the 200-character cut falls", async () => {
    for (let pad = 100; pad <= 170; pad += 3) {
      const { client, calls } = fakeClient();
      const rec = recordingLogger();
      const tool = "x".repeat(pad) + KEY;
      const qf = scripted(async function* () { yield { type: "system", subtype: "init", tools: [...INIT_OK.tools, tool] }; });
      await runner(client, await tmpJobsRoot(), qf, rec.logger).execute(providerJobClaim());
      const last = calls.states.at(-1)!;
      assert.equal(last.status, "failed");
      assert.match(last.failure_reason ?? "", /^the job session exposed tools outside the job policy: /);
      const text = JSON.stringify([calls.states, calls.messages, rec.lines]);
      assert.ok(!text.includes("gw-ke"), `pad ${pad}: no key prefix survives the cut`);
      assertNoKey(KEY, text, `pad ${pad}`);
    }
  });
});

describe("provider job: submit_job_result with key forms present", { skip: LINUX_ONLY }, () => {
  async function toolEnv(key: string, files: Record<string, string> = {}): Promise<{ work: string; submit: ReturnType<typeof buildJobResultServer>["submit"]; store: Parameters<typeof buildJobResultServer>[0] }> {
    const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-jobprov-tool-"));
    roots.push(d);
    const work = path.join(d, "work");
    await fsp.mkdir(path.join(work, "outputs"), { recursive: true });
    await fsp.mkdir(path.join(work, "sources"));
    for (const [rel, content] of Object.entries(files)) {
      await fsp.mkdir(path.dirname(path.join(work, rel)), { recursive: true });
      await fsp.writeFile(path.join(work, rel), content);
    }
    const { makeTextRedactor } = await import("../src/redact.js");
    const store: Parameters<typeof buildJobResultServer>[0] = {};
    const { submit } = buildJobResultServer(store, { workDir: work, secretPaths: [], redactText: makeTextRedactor(providerKeyForms(key)) });
    return { work, submit, store };
  }
  const textOf = (r: { content: Array<{ text: string }> }): string => r.content.map((c) => c.text).join("\n");

  it("a 32-character lowercase key as status is refused with a constant message and nothing is stored", async () => {
    const { submit, store } = await toolEnv(STATUS_KEY);
    assert.equal(STATUS_KEY.length, 32);
    const res = submit({ ...GOOD, status: STATUS_KEY });
    assert.equal(res.isError, true);
    assert.ok(!textOf(res).includes(STATUS_KEY));
    assert.equal(store.result, undefined);
    assert.equal(submit({ ...GOOD, status: "completed" }).isError, undefined, "a clean status is accepted");
  });

  it("a job whose only submission carried a key status fails with the constant no-result reason", async () => {
    const { client, calls } = fakeClient();
    const qf = scripted(async function* ({ options }) {
      yield INIT_OK;
      assert.equal((await callSubmit(options, { ...GOOD, status: STATUS_KEY })).isError, true);
      yield RESULT_OK;
    });
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(STATUS_KEY));
    const last = calls.states.at(-1)!;
    assert.equal(last.status, "failed");
    assert.equal(last.failure_reason, "the job ended without submitting a result (submit_job_result was never accepted)");
    assert.deepEqual(calls.results, []);
    assertNoKey(STATUS_KEY, everywhere(calls), "status key");
  });

  it("scrubs report and finding text before validation and storage", async () => {
    const { submit, store } = await toolEnv(KEY);
    const res = submit({ status: "completed", report_md: `r ${KEY}`, findings: [{ severity: "info", message_md: `m ${KEY}`, file: `f/${KEY}`, line: 2 }] });
    assert.equal(res.isError, undefined);
    assertNoKey(KEY, JSON.stringify(store.result), "stored result");
  });

  const OK = ["abcdefghijklmnop", ":"] as const; // key "abcdefghijklmnop:" (runtime-assembled)
  it("(b1) a missing-file diagnostic that renders the key is scrubbed, also when the cut would fall inside it", async () => {
    const key = OK.join("");
    for (let n = 60; n <= 130; n += 1) {
      const { submit, store } = await toolEnv(key);
      const pads = Array.from({ length: 9 }, (_, i) => `outputs/${"p".repeat(n)}${i}`);
      const res = submit({ ...GOOD, output_files: [...pads, "outputs/abcdefghijklmnop"] });
      assert.equal(res.isError, true);
      const text = textOf(res);
      assert.ok(!text.includes("abcdefg"), `n ${n}: no key text or prefix in the tool response`);
      assert.equal(store.result, undefined);
    }
    const { submit } = await toolEnv(key);
    const res = submit({ ...GOOD, output_files: ["outputs/abcdefghijklmnop"] });
    assert.ok(textOf(res).includes("***REDACTED***"), "the key position is marked");
  });

  it("a key spanning the diagnostic label and the start of the path is scrubbed from the final text", async () => {
    const key = ["output_files", ": ", "outputs/missing"].join("");
    assert.ok(key.length >= 16);
    const { submit, store } = await toolEnv(key);
    const res = submit({ ...GOOD, output_files: ["outputs/missing.txt"] });
    assert.equal(res.isError, true);
    assertNoKey(key, textOf(res), "tool response");
    assert.ok(!textOf(res).includes("output_files: outputs/missing"), "the reassembled key is gone");
    assert.equal(store.result, undefined);
  });

  it("(b2) a duplicate-name message that renders the key is scrubbed", async () => {
    const key = ["(abcdefghijklmnop", ")"].join("");
    const { submit, store } = await toolEnv(key, { "outputs/a/abcdefghijklmnop": "1", "outputs/b/abcdefghijklmnop": "2" });
    const res = submit({ ...GOOD, output_files: ["outputs/a/abcdefghijklmnop", "outputs/b/abcdefghijklmnop"] });
    assert.equal(res.isError, true);
    assert.match(textOf(res), /same file name/);
    assert.ok(!textOf(res).includes("abcdefghijklmnop"));
    assert.equal(store.result, undefined);
  });

  it("(a) uppercase and lowercase spellings of a key-bearing basename are both withheld, never reported as a duplicate", async () => {
    const key = "abcdefghijklmnop";
    const { submit, store } = await toolEnv(key);
    const res = submit({ ...GOOD, output_files: ["outputs/a/ABCDEFGHIJKLMNOP", "outputs/b/abcdefghijklmnop"] });
    assert.equal(res.isError, undefined, textOf(res));
    assert.equal(store.keyedOutputs?.length, 2);
    assert.deepEqual(store.outputFiles, []);
  });

  it("(c)(d) distinct key-bearing names are each withheld; the same name is reported once at upload", async () => {
    const key = "abcdefghijklmnop";
    const { submit, store } = await toolEnv(key, { "outputs/clean.txt": "ok" });
    const res = submit({ ...GOOD, output_files: ["outputs/abcdefghijklmnop-a.txt", "outputs/abcdefghijklmnop-b.txt", "outputs/clean.txt"] });
    assert.equal(res.isError, undefined, textOf(res));
    assert.deepEqual(store.keyedOutputs, ["outputs/abcdefghijklmnop-a.txt", "outputs/abcdefghijklmnop-b.txt"]);
    assert.deepEqual(store.outputFiles, ["outputs/clean.txt"]);
  });

  it("(e) the cap is checked on the raw list: 49 clean + 2 keyed entries is refused and nothing is stored", async () => {
    const key = "abcdefghijklmnop";
    const { submit, store } = await toolEnv(key);
    const list = [...Array.from({ length: 49 }, (_, i) => `outputs/f${i}.txt`), "outputs/abcdefghijklmnop-1", "outputs/abcdefghijklmnop-2"];
    const res = submit({ ...GOOD, output_files: list });
    assert.equal(res.isError, true);
    assert.match(textOf(res), /at most 50/);
    assert.equal(store.result, undefined);
  });

  it("(f) an omitted or null output_files is accepted", async () => {
    for (const files of [undefined, null]) {
      const { submit, store } = await toolEnv(KEY);
      const res = submit({ ...GOOD, ...(files === undefined ? {} : { output_files: files }) });
      assert.equal(res.isError, undefined);
      assert.deepEqual(store.outputFiles, []);
    }
  });

  it("(g) a non-string or empty entry gets the existing per-index error before any string helper runs", async () => {
    const { submit, store } = await toolEnv(KEY);
    for (const [entry, index] of [[7, 1], [{ toString: () => KEY }, 1], ["", 1], [null, 1]] as const) {
      const res = submit({ ...GOOD, output_files: ["outputs/a.txt", entry] });
      assert.equal(res.isError, true);
      assert.match(textOf(res), new RegExp(`output_files\\[${index}\\] must be a non-empty string`));
      assert.equal(store.result, undefined);
    }
    assert.equal(submit({ ...GOOD, output_files: "outputs/a.txt" }).isError, true, "a non-array is refused");
  });
});

describe("provider job: output publication end to end", { skip: LINUX_ONLY }, () => {
  function session(files: Record<string, string | Buffer>, outputs: string[]): SdkQueryFn {
    return scripted(async function* ({ options }) {
      for (const [rel, content] of Object.entries(files)) {
        await fsp.mkdir(path.dirname(path.join(options.cwd!, rel)), { recursive: true });
        await fsp.writeFile(path.join(options.cwd!, rel), content);
      }
      yield INIT_OK;
      const res = await callSubmit(options, { ...GOOD, output_files: outputs });
      assert.equal(res.isError, undefined, JSON.stringify(res));
      yield RESULT_OK;
    });
  }

  it("a file whose bytes carry the key makes zero upload attempts and is reported worker_unreadable; the clean sibling uploads and the job completes", async () => {
    const { client, calls } = fakeClient();
    const split = Buffer.concat([Buffer.alloc(65536 - 5, "a"), Buffer.from(KEY), Buffer.alloc(10, "b")]);
    const qf = session({ "outputs/clean.txt": "clean sibling", "outputs/leak.txt": `x ${KEY} y`, "outputs/split.bin": split }, ["outputs/leak.txt", "outputs/clean.txt", "outputs/split.bin"]);
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim());
    assert.deepEqual(calls.uploads.map((u) => u.meta.display_name), ["clean.txt"]);
    assert.deepEqual(calls.uploads[0]!.bytes, Buffer.from("clean sibling"));
    assert.equal(calls.states.at(-1)!.status, "completed");
    assert.deepEqual(calls.results[0]!.refused_outputs, [
      { display_name: "leak.txt", reason: "worker_unreadable" },
      { display_name: "split.bin", reason: "worker_unreadable" },
    ]);
    assertNoKey(KEY, everywhere(calls), "published");
  });

  it("an output whose own name carries the key is withheld under its scrubbed name and never opened or uploaded", async () => {
    const { client, calls } = fakeClient();
    const qf = session({ [`outputs/${STATUS_KEY}.txt`]: "named by the key", "outputs/clean.txt": "ok" }, [`outputs/${STATUS_KEY}.txt`, "outputs/clean.txt"]);
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(STATUS_KEY));
    assert.deepEqual(calls.uploads.map((u) => u.meta.display_name), ["clean.txt"]);
    assert.deepEqual(calls.results[0]!.refused_outputs, [{ display_name: "***REDACTED***.txt", reason: "worker_unreadable" }]);
    assert.equal(calls.states.at(-1)!.status, "completed");
    assertNoKey(STATUS_KEY, everywhere(calls), "published");
  });

  it("a name withheld for an uppercase spelling of the key is reported under a placeholder, never its own spelling", async () => {
    const { client, calls } = fakeClient();
    const upper = STATUS_KEY.toUpperCase();
    // The upper-case path is listed twice: one file still yields one refusal.
    const qf = session({ [`outputs/${upper}.txt`]: "named by the key", [`outputs/${STATUS_KEY}.md`]: "x", "outputs/clean.txt": "ok" }, [`outputs/${upper}.txt`, `outputs/${upper}.txt`, `outputs/${STATUS_KEY}.md`, "outputs/clean.txt"]);
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(STATUS_KEY));
    assert.deepEqual(calls.uploads.map((u) => u.meta.display_name), ["clean.txt"]);
    assert.deepEqual(calls.results[0]!.refused_outputs, [
      { display_name: "withheld-output-1", reason: "worker_unreadable" },
      { display_name: "***REDACTED***.md", reason: "worker_unreadable" },
    ]);
    assert.equal(calls.states.at(-1)!.status, "completed");
    const text = everywhere(calls);
    assert.ok(!text.toLowerCase().includes(STATUS_KEY), "no case variant of the key is published");
  });

  it("a key straddling the display-name cut is scrubbed before the cut, so no prefix of it is reported", async () => {
    const { client, calls } = fakeClient();
    // 80 filler characters then the key then ".txt": the 100-code-point display cut falls inside the key.
    const name = `${"a".repeat(80)}${STATUS_KEY}.txt`;
    const qf = session({ [`outputs/${name}`]: "named by the key", "outputs/clean.txt": "ok" }, [`outputs/${name}`, "outputs/clean.txt"]);
    await runner(client, await tmpJobsRoot(), qf).execute(providerJobClaim(STATUS_KEY));
    assert.deepEqual(calls.uploads.map((u) => u.meta.display_name), ["clean.txt"]);
    assert.equal(calls.results[0]!.refused_outputs?.length, 1);
    assert.equal(calls.results[0]!.refused_outputs![0]!.reason, "worker_unreadable");
    const text = everywhere(calls);
    assert.ok(!text.includes(STATUS_KEY.slice(0, 8)), "no prefix of the key was posted");
    assert.equal(calls.states.at(-1)!.status, "completed");
  });

  it("a vendor job is untouched: no capture, same-handle upload, key-free claim carries no provider", async () => {
    const { client, calls } = fakeClient();
    const claim = { ...providerJobClaim(), secrets: { forge_pat: "", anthropic_oauth_token: "sk-fixture-model-credential" } } as unknown as ClaimResponse;
    const qf = session({ "outputs/a.txt": "vendor bytes" }, ["outputs/a.txt"]);
    await runner(client, await tmpJobsRoot(), qf).execute(claim);
    assert.deepEqual(calls.uploads.map((u) => u.bytes.toString()), ["vendor bytes"]);
    assert.equal(calls.states.at(-1)!.status, "completed");
  });
});
