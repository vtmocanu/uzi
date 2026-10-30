// PRD #3244: the worker's lead-config resolver and reporter. The request matrix is the shared
// fixture fixtures/lead-config/contract.json (`request_cases`); the reporter is driven with a fake
// client and injected timers, so nothing here waits on a real clock.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import type { LeadConfigPostOutcome } from "../src/client.js";
import {
  LEAD_CONFIG_ENTER_WAIT_MS,
  LeadConfigReporter,
  leadConfigActive,
  resolveLeadRequest,
  type LeadConfigClient,
  type LeadConfigTimers,
  type LeadRequest,
  type LeadRequestInput,
} from "../src/lead-config-report.js";
import type { ClaimConfig, LeadConfigReportBody } from "../src/protocol.js";
import { CodexExecutor } from "../src/codex/codex-executor.js";
import { selectCodexBinding } from "../src/codex/select.js";
import type { CodexProviderConfig } from "../src/codex/codex-harness.js";
import type { RunContext } from "../src/executor.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// ── the shared request matrix ────────────────────────────────────────────────

interface RequestCase {
  id: string;
  harness: "claude" | "codex";
  phase: "plan" | "implement";
  config: ClaimConfig;
  templateModel?: string;
  providerDefaultModel?: string;
  expect: LeadRequest;
}

const contract = JSON.parse(
  readFileSync(join(import.meta.dirname, "..", "..", "fixtures", "lead-config", "contract.json"), "utf8"),
) as { request_cases: RequestCase[] };
const LEAD_REQUEST_CASES: RequestCase[] = contract.request_cases;

function codexExecutorFor(providerModel: string): CodexExecutor {
  const selection = selectCodexBinding({
    codex: { auth_mode: "subscription", access_token: "t", capability: "c", generation: 1, chatgpt_account_id: "a", chatgpt_plan_type: null },
  });
  if (selection.kind !== "codex") throw new Error("expected a codex selection");
  const provider: CodexProviderConfig = {
    name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: providerModel,
  };
  return new CodexExecutor(nullLogger(), "/tmp/lead-config-home", { binding: selection.binding, client: {} as never, provider },
    { installDeps: async () => ({ results: [], truncated: false }) });
}

describe("lead request matrix (fixtures/lead-config/contract.json request_cases)", () => {
  it("resolves every case, and the executors send what is resolved", () => {
    const visited = new Set<string>();
    for (const c of LEAD_REQUEST_CASES) {
      const input: LeadRequestInput = {
        harness: c.harness, phase: c.phase, config: c.config,
        ...(c.templateModel !== undefined ? { templateModel: c.templateModel } : {}),
        ...(c.providerDefaultModel !== undefined ? { providerDefaultModel: c.providerDefaultModel } : {}),
      };
      assert.deepEqual(resolveLeadRequest(input), c.expect, `resolveLeadRequest ${c.id}`);
      if (c.harness === "claude") {
        // What the SdkExecutor sends for each claude case (model and effort in the SDK query
        // options) is asserted by driving the real executor in sdk-executor.test.ts, which
        // iterates this same request_cases list.
      } else {
        const exec = codexExecutorFor(c.providerDefaultModel!);
        const ctx = { runId: "r", config: c.config, agents: [] } as unknown as RunContext;
        const selection = c.phase === "plan" ? undefined : { source: "own" as const, exclusions: [] };
        const request = (exec as unknown as {
          buildRunRequest(...a: unknown[]): { model: string; effort?: string };
        }).buildRunRequest(ctx, c.phase, "p", undefined, new AbortController().signal, selection);
        assert.equal(request.model, c.expect.model, `codex sent model ${c.id}`);
        assert.equal(request.effort, c.expect.effort, `codex sent effort ${c.id}`);
      }
      visited.add(c.id);
    }
    assert.deepEqual([...visited].sort(), LEAD_REQUEST_CASES.map((c) => c.id).sort());
    assert.ok(visited.size >= 10, "the matrix is loaded, not empty");
  });

  it("a blank claim model falls back to the template (|| not ??), and nothing resolves to no model", () => {
    const base = { harness: "claude" as const, phase: "implement" as const };
    assert.equal(resolveLeadRequest({ ...base, config: { default_model: "" }, templateModel: "fable" }).model, "fable");
    assert.equal(resolveLeadRequest({ ...base, config: { default_model: "" } }).model, undefined);
  });

  it("activation follows default_effort_source only", () => {
    assert.equal(leadConfigActive(undefined), false);
    assert.equal(leadConfigActive(null), false);
    assert.equal(leadConfigActive({ default_effort: "medium" }), false);
    assert.equal(leadConfigActive({ default_effort_source: "builtin" }), true);
  });
});

// ── reporter ─────────────────────────────────────────────────────────────────

const REQ: LeadRequest = { model: "opus", modelSource: "user_default", effort: "medium", effortMode: "explicit", effortSource: "builtin" };
const REQ_B: LeadRequest = { model: "sonnet", modelSource: "template", effortMode: "runtime_default", effortSource: "builtin" };

const OK: LeadConfigPostOutcome = { kind: "ok" };
const http = (status: number, error?: string): LeadConfigPostOutcome => ({ kind: "http", status, ...(error ? { error } : {}) });
const TRANSPORT: LeadConfigPostOutcome = { kind: "transport", error: new Error("econnreset") };

class FakeClient implements LeadConfigClient {
  readonly posts: LeadConfigReportBody[] = [];
  readonly script: LeadConfigPostOutcome[] = [];
  /** When set, every call blocks on it before answering. */
  gate: Promise<void> | undefined;
  throwOnCall = false;
  async recordLeadConfig(_runId: string, body: LeadConfigReportBody): Promise<LeadConfigPostOutcome> {
    this.posts.push(body);
    if (this.throwOnCall) throw new Error("client exploded");
    if (this.gate) await this.gate;
    return this.script.shift() ?? OK;
  }
}

class FakeTimers implements LeadConfigTimers {
  readonly pending = new Map<number, { fn: () => void; ms: number }>();
  private next = 1;
  setTimeout(fn: () => void, ms: number): unknown {
    const id = this.next++;
    this.pending.set(id, { fn, ms });
    return id;
  }
  clearTimeout(handle: unknown): void {
    this.pending.delete(handle as number);
  }
  fireAll(): void {
    for (const [id, t] of Array.from(this.pending)) {
      this.pending.delete(id);
      t.fn();
    }
  }
}

const flush = async (): Promise<void> => {
  for (let i = 0; i < 20; i++) await new Promise<void>((r) => setImmediate(r));
};

function rig(opts: { active?: boolean } = {}) {
  const client = new FakeClient();
  const timers = new FakeTimers();
  const log = recordingLogger();
  const reporter = new LeadConfigReporter({
    client, runId: "run-1", claimGeneration: 3, active: opts.active ?? true, logger: log.logger, timers,
  });
  return { client, timers, log, reporter };
}

const rowsOf = (client: FakeClient): string[] => client.posts.map((p) => `${p.phase}#${p.phase_entry}/${p.seq}${p.truncated ? "T" : ""}`);

describe("LeadConfigReporter", () => {
  it("posts one requested-side row on the first call, with the observed side sent as explicit nulls", async () => {
    const { client, reporter } = rig();
    const entry = await reporter.enterPhase("plan", REQ);
    assert.deepEqual(entry, { phase: "plan", entry: 1 });
    assert.deepEqual(client.posts, [{
      claim_generation: 3, phase: "plan", phase_entry: 1, seq: 1, truncated: false,
      requested_model: "opus", requested_model_source: "user_default",
      requested_effort: "medium", requested_effort_mode: "explicit", requested_effort_source: "builtin",
      observed_model: null, observed_model_evidence: null, observed_effort: null, observed_effort_evidence: null,
    }]);
  });

  it("sends null model and effort for an SDK-default model and a runtime-default effort", async () => {
    const { client, reporter } = rig();
    await reporter.enterPhase("implement", { modelSource: "builtin", effortMode: "runtime_default", effortSource: "builtin" });
    const body = client.posts[0]!;
    assert.equal(body.requested_model, null);
    assert.equal(body.requested_effort, null);
    assert.equal(body.requested_effort_mode, "runtime_default");
  });

  it("is inactive without the activation signal: numbers entries, posts nothing", async () => {
    const { client, reporter } = rig({ active: false });
    assert.deepEqual(await reporter.enterPhase("plan", REQ), { phase: "plan", entry: 1 });
    assert.deepEqual(await reporter.enterPhase("implement", REQ), { phase: "implement", entry: 1 });
    await flush();
    assert.equal(client.posts.length, 0);
    assert.equal(reporter.enabled, false);
  });

  it("starts a new entry only when the phase changes", async () => {
    const { client, reporter } = rig();
    assert.deepEqual(await reporter.enterPhase("plan", REQ), { phase: "plan", entry: 1 });
    assert.deepEqual(await reporter.enterPhase("plan", REQ), { phase: "plan", entry: 1 }); // a revise round: same entry
    assert.deepEqual(await reporter.enterPhase("implement", REQ), { phase: "implement", entry: 1 });
    assert.deepEqual(await reporter.enterPhase("implement", REQ), { phase: "implement", entry: 1 }); // a re-driven turn
    assert.deepEqual(await reporter.enterPhase("plan", REQ_B), { phase: "plan", entry: 2 }); // a re-plan
    await flush();
    assert.deepEqual(rowsOf(client), ["plan#1/1", "implement#1/1", "plan#2/2"]);
    assert.equal(client.posts[2]!.requested_model, "sonnet");
  });

  it("posts an observation row repeating the requested side, deduped against the entry's last row", async () => {
    const { client, reporter } = rig();
    const e = await reporter.enterPhase("implement", REQ);
    const obs = { model: "opus", modelEvidence: "claude_init", effort: "medium", effortEvidence: "claude_init" };
    reporter.observe(e, obs);
    reporter.observe(e, { ...obs }); // identical: nothing
    await flush();
    assert.deepEqual(rowsOf(client), ["implement#1/1", "implement#1/2"]);
    const row = client.posts[1]!;
    assert.equal(row.requested_model, "opus");
    assert.equal(row.observed_model, "opus");
    assert.equal(row.observed_effort_evidence, "claude_init");
  });

  it("caps an entry at three observation rows and keeps later entries working", async () => {
    const { client, reporter } = rig();
    const e1 = await reporter.enterPhase("plan", REQ);
    for (const m of ["a", "b", "c", "d"]) {
      reporter.observe(e1, { model: m, modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    }
    await reporter.enterPhase("implement", REQ);
    const e2 = await reporter.enterPhase("plan", REQ);
    reporter.observe(e2, { model: "z", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    await flush();
    assert.deepEqual(rowsOf(client), ["plan#1/1", "plan#1/2", "plan#1/3", "plan#1/4", "implement#1/1", "plan#2/5", "plan#2/6"]);
    assert.ok(client.posts.every((p) => !p.truncated));
  });

  it("drops an observation from a superseded entry, keeps a late one for the still-latest entry", async () => {
    const { client, reporter, log } = rig();
    const plan1 = await reporter.enterPhase("plan", REQ);
    const impl1 = await reporter.enterPhase("implement", REQ);
    // Late plan observation after implement began: still the latest plan entry, so it attaches.
    reporter.observe(plan1, { model: "opus", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    const plan2 = await reporter.enterPhase("plan", REQ);
    // Now plan1 is superseded.
    reporter.observe(plan1, { model: "other", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    reporter.observe(plan2, { model: "p2", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    reporter.observe(impl1, { model: "i1", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    await flush();
    assert.deepEqual(rowsOf(client), ["plan#1/1", "implement#1/1", "plan#1/2", "plan#2/3", "plan#2/4", "implement#1/2"]);
    assert.ok(JSON.stringify(log.lines).includes("superseded"));
  });

  it("allows eight entries per phase and posts a truncation row only when a ninth begins", async () => {
    const { client, reporter } = rig();
    for (let i = 1; i <= 8; i++) {
      await reporter.enterPhase("plan", REQ);
      await reporter.enterPhase("implement", REQ);
    }
    await flush();
    assert.equal(client.posts.filter((p) => p.phase === "plan").length, 8);
    assert.ok(client.posts.every((p) => !p.truncated), "eight entries need no truncation row");

    const ninth = await reporter.enterPhase("plan", REQ_B);
    assert.equal(ninth.entry, 9);
    await flush();
    const last = client.posts.at(-1)!;
    assert.equal(last.truncated, true);
    assert.equal(last.seq, 32);
    assert.equal(last.phase, "plan");
    assert.equal(last.phase_entry, 8, "carries the latest recorded entry, within the server's bound");
    assert.equal(last.requested_model, "opus", "latest recorded values, not the unrecorded ninth");

    // The phase is stopped: nothing more for plan, implement is unaffected.
    const before = client.posts.length;
    reporter.observe(ninth, { model: "x", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    await reporter.enterPhase("implement", REQ);
    await reporter.enterPhase("plan", REQ);
    await flush();
    assert.equal(client.posts.length, before + 1, "only the ninth implement entry's own truncation row");
  });

  it("posts the truncation row at seq 32 when the next row would need it", async () => {
    const { client, reporter } = rig();
    let n = 0;
    const distinct = () => ({ model: `m${n++}`, modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    for (let entry = 1; entry <= 8; entry++) {
      const e = await reporter.enterPhase("plan", REQ);
      await reporter.enterPhase("implement", REQ);
      for (let k = 0; k < 3; k++) reporter.observe(e, distinct());
    }
    await flush();
    const plan = client.posts.filter((p) => p.phase === "plan");
    // 8 entry rows + 3 observations per entry would be 32 rows; row 32 becomes the truncation row.
    assert.equal(plan.length, 32);
    assert.deepEqual(plan.map((p) => p.seq), Array.from({ length: 32 }, (_, i) => i + 1));
    assert.equal(plan.at(-1)!.truncated, true);
    assert.equal(plan.filter((p) => p.truncated).length, 1);
    assert.equal(plan.at(-1)!.observed_model, "m23", "carries the latest known values");
  });

  it("bounds the pending queue at four, dropping the oldest and logging it", async () => {
    const { client, reporter, log, timers } = rig();
    let release!: () => void;
    client.gate = new Promise<void>((r) => { release = r; });
    const calls: Promise<unknown>[] = [];
    for (let i = 0; i < 3; i++) {
      calls.push(reporter.enterPhase("plan", REQ_B));
      calls.push(reporter.enterPhase("implement", REQ));
    }
    // r1 is in flight; r2..r6 queue five rows against a bound of four, so r2 is dropped.
    release();
    timers.fireAll();
    await Promise.all(calls);
    await flush();
    assert.deepEqual(rowsOf(client), ["plan#1/1", "plan#2/2", "implement#2/2", "plan#3/3", "implement#3/3"]);
    assert.ok(JSON.stringify(log.lines).includes("dropped the oldest"));
  });

  for (const [name, outcome] of [
    ["400", http(400, "invalid_lead_config")],
    ["400 claim_generation_required", http(400, "claim_generation_required")],
    ["404", http(404)],
    ["409 claim_not_current", http(409, "claim_not_current")],
  ] as const) {
    it(`disables for the rest of the claim on ${name}`, async () => {
      const { client, reporter } = rig();
      client.script.push(outcome);
      await reporter.enterPhase("plan", REQ);
      await flush();
      assert.equal(reporter.enabled, false);
      await reporter.enterPhase("implement", REQ);
      await flush();
      assert.equal(client.posts.length, 1);
    });
  }

  it("logs a key conflict at error level and disables", async () => {
    const { client, reporter, log } = rig();
    client.script.push(http(409, "lead_config_key_conflict"));
    await reporter.enterPhase("plan", REQ);
    await flush();
    assert.equal(reporter.enabled, false);
    assert.ok((log.lines as Array<{ level: string; msg: string }>).some((l) => l.level === "error" && l.msg.includes("key conflict")));
  });

  it("does not disable on a 5xx or an unknown 409; the row is dropped and later rows still go", async () => {
    const { client, reporter } = rig();
    client.script.push(http(503), http(409, "something_else"));
    await reporter.enterPhase("plan", REQ);
    await reporter.enterPhase("implement", REQ);
    await reporter.enterPhase("plan", REQ);
    await flush();
    assert.equal(reporter.enabled, true);
    assert.equal(client.posts.length, 3, "no retry on an http refusal");
  });

  it("drains the queue when a refusal disables reporting", async () => {
    const { client, reporter, timers } = rig();
    let release!: () => void;
    client.gate = new Promise<void>((r) => { release = r; });
    client.script.push(http(404));
    const calls = [reporter.enterPhase("plan", REQ), reporter.enterPhase("implement", REQ), reporter.enterPhase("plan", REQ)];
    release();
    timers.fireAll();
    await Promise.all(calls);
    await flush();
    assert.equal(client.posts.length, 1);
  });

  it("retries once on a transport error with the same body, then gives up on that row only", async () => {
    const { client, reporter } = rig();
    client.script.push(TRANSPORT, OK, TRANSPORT, TRANSPORT);
    await reporter.enterPhase("plan", REQ);
    await flush();
    assert.equal(client.posts.length, 2);
    assert.deepEqual(client.posts[0], client.posts[1]);
    await reporter.enterPhase("implement", REQ);
    await flush();
    assert.equal(client.posts.length, 4, "two attempts for the second row, then it is dropped");
    assert.equal(reporter.enabled, true);
    await reporter.enterPhase("plan", REQ);
    await flush();
    assert.equal(client.posts.length, 5, "later rows are still delivered");
  });

  it("never throws, even when the client throws", async () => {
    const { client, reporter } = rig();
    client.throwOnCall = true;
    const e = await reporter.enterPhase("plan", REQ);
    reporter.observe(e, { model: "x", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" });
    await flush();
    assert.equal(e.entry, 1);
  });

  it("bounds the enterPhase await at 3 s and lets the post continue in the background", async () => {
    const { client, reporter, timers } = rig();
    let release!: () => void;
    client.gate = new Promise<void>((r) => { release = r; });
    let resolved = false;
    const p = reporter.enterPhase("plan", REQ).then((e) => { resolved = true; return e; });
    await flush();
    assert.equal(resolved, false, "waits for the row while the post is outstanding");
    assert.deepEqual([...timers.pending.values()].map((t) => t.ms), [LEAD_CONFIG_ENTER_WAIT_MS]);
    timers.fireAll();
    assert.deepEqual(await p, { phase: "plan", entry: 1 });
    assert.equal(client.posts.length, 1, "the post was started and is still outstanding");
    release();
    await flush();
  });

  it("returns at once, with the timer cleared, when the row lands in time", async () => {
    const { reporter, timers } = rig();
    await reporter.enterPhase("plan", REQ);
    assert.equal(timers.pending.size, 0);
  });
});
